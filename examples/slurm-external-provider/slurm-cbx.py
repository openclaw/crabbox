#!/usr/bin/env python3
"""Reference Crabbox external-provider adapter for Slurm.

This adapter is intentionally conservative example code. It owns Slurm job
submission and cancellation, then returns a normal SSH endpoint to Crabbox once
the scheduled allocation has started a site runner.
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import stat
import subprocess
import sys
import time
from pathlib import Path
from typing import Any, Dict, List, Optional

PROTOCOL_VERSION = 1
DEFAULT_READY_CHECK = (
    "command -v bash && command -v python3 && command -v git && "
    "command -v rsync && command -v tar"
)
TERMINAL_STATES = {
    "BOOT_FAIL",
    "CANCELLED",
    "COMPLETED",
    "DEADLINE",
    "FAILED",
    "NODE_FAIL",
    "OUT_OF_MEMORY",
    "PREEMPTED",
    "REVOKED",
    "SPECIAL_EXIT",
    "TIMEOUT",
}


class AdapterError(Exception):
    pass


def main() -> int:
    parser = argparse.ArgumentParser(description="Crabbox external provider for Slurm")
    parser.add_argument("--state-dir", default="~/.crabbox/slurm")
    parser.add_argument("--runner-script", default="")
    parser.add_argument("--poll-interval", type=float, default=5.0)
    args = parser.parse_args()

    try:
        request = json.load(sys.stdin)
        adapter = SlurmCrabboxAdapter(args)
        response = adapter.handle(request)
        json.dump(response, sys.stdout, separators=(",", ":"))
        sys.stdout.write("\n")
        return 0
    except AdapterError as exc:
        print(str(exc), file=sys.stderr)
        json.dump({"error": str(exc)}, sys.stdout, separators=(",", ":"))
        sys.stdout.write("\n")
        return 1
    except Exception as exc:  # pragma: no cover - defensive boundary for protocol callers.
        print(f"unexpected adapter failure: {exc}", file=sys.stderr)
        json.dump({"error": f"unexpected adapter failure: {exc}"}, sys.stdout, separators=(",", ":"))
        sys.stdout.write("\n")
        return 1


class SlurmCrabboxAdapter:
    def __init__(self, args: argparse.Namespace) -> None:
        self.state_dir = Path(args.state_dir).expanduser().resolve()
        self.runner_script = Path(args.runner_script).expanduser().resolve() if args.runner_script else None
        self.poll_interval = args.poll_interval

    def handle(self, request: Dict[str, Any]) -> Dict[str, Any]:
        operation = str(request.get("operation") or "")
        config = request.get("config") or {}
        if not isinstance(config, dict):
            raise AdapterError("request config must be an object")

        if operation == "doctor":
            return self.doctor(config)
        if operation == "acquire":
            return {"protocolVersion": PROTOCOL_VERSION, "lease": self.acquire(request, config)}
        if operation == "resolve":
            return {"protocolVersion": PROTOCOL_VERSION, "lease": self.resolve(request, config)}
        if operation == "list":
            return {"protocolVersion": PROTOCOL_VERSION, "leases": self.list_leases(request, config)}
        if operation == "release":
            self.release(request, config)
            return {"protocolVersion": PROTOCOL_VERSION, "message": "released Slurm lease"}
        if operation == "touch":
            return {"protocolVersion": PROTOCOL_VERSION, "lease": self.touch(request, config)}
        if operation == "cleanup":
            return self.cleanup(request, config)
        raise AdapterError(f"unsupported operation: {operation}")

    def doctor(self, config: Dict[str, Any]) -> Dict[str, Any]:
        missing = [name for name in ("sbatch", "squeue", "scancel") if shutil.which(name) is None]
        if missing:
            raise AdapterError(f"missing Slurm command(s): {', '.join(missing)}")
        runner = self.runner(config)
        if not runner.exists():
            raise AdapterError(f"runner script does not exist: {runner}")
        self.ensure_private_dir(self.state_dir)
        return {
            "protocolVersion": PROTOCOL_VERSION,
            "message": f"Slurm external provider ready; state={self.state_dir} runner={runner}",
        }

    def acquire(self, request: Dict[str, Any], config: Dict[str, Any]) -> Dict[str, Any]:
        desired = request.get("desired") or {}
        lease_id = required_string(desired, "leaseId")
        slug = required_string(desired, "slug")
        name = required_string(desired, "name")
        job_dir = self.job_dir(lease_id)
        self.ensure_private_dir(job_dir)

        existing = self.load_state(lease_id)
        if existing and existing.get("jobId"):
            return self.wait_for_endpoint(existing, config, keep=bool(request.get("keep")))

        key_path = self.ensure_ssh_key(job_dir, lease_id, config)
        public_key_path = Path(str(key_path) + ".pub")
        endpoint_path = job_dir / "endpoint.json"
        runner = self.runner(config)
        if not runner.exists():
            raise AdapterError(f"runner script does not exist: {runner}")

        state = {
            "leaseId": lease_id,
            "slug": slug,
            "name": name,
            "status": "submitting",
            "createdAt": now(),
            "updatedAt": now(),
            "keyPath": str(key_path),
            "publicKeyPath": str(public_key_path),
            "endpointPath": str(endpoint_path),
            "runnerScript": str(runner),
            "config": public_config(config),
        }
        self.save_state(lease_id, state)

        command = self.sbatch_command(config, name, job_dir, runner)
        env = os.environ.copy()
        env.update(
            {
                "CBX_LEASE_ID": lease_id,
                "CBX_SLUG": slug,
                "CBX_NAME": name,
                "CBX_STATE_DIR": str(self.state_dir),
                "CBX_WORK_ROOT": str(config_string(config, "runnerWorkRoot", "")),
                "CBX_SSH_PUBLIC_KEY_FILE": str(public_key_path),
                "CBX_SSH_USER": config_string(config, "sshUser", ""),
                "CBX_READY_CHECK": config_string(config, "readyCheck", DEFAULT_READY_CHECK),
            }
        )
        if not env["CBX_WORK_ROOT"]:
            env["CBX_WORK_ROOT"] = str(Path("~/crabbox-slurm-work").expanduser())

        result = run(command, env=env)
        raw_job_id = result.stdout.strip().splitlines()[-1].strip()
        if not raw_job_id:
            raise AdapterError("sbatch returned no job id")
        job_id = raw_job_id.split(";", 1)[0]
        state.update(
            {
                "jobId": job_id,
                "rawJobId": raw_job_id,
                "cloudId": f"slurm/job/{raw_job_id}",
                "status": "pending",
                "updatedAt": now(),
            }
        )
        self.save_state(lease_id, state)

        try:
            return self.wait_for_endpoint(state, config, keep=bool(request.get("keep")))
        except Exception:
            if not request.get("keep"):
                self.cancel_job(job_id)
                self.remove_job_dir(lease_id)
            raise

    def resolve(self, request: Dict[str, Any], config: Dict[str, Any]) -> Dict[str, Any]:
        state = self.find_state(request)
        if not state:
            raise AdapterError(f"could not resolve Slurm lease {request.get('id')!r}")
        self.validate_expected(request, state)
        if request.get("releaseOnly"):
            return self.lease_from_state(state, config, require_endpoint=False)
        return self.wait_for_endpoint(state, config, keep=True)

    def list_leases(self, request: Dict[str, Any], config: Dict[str, Any]) -> List[Dict[str, Any]]:
        include_all = bool(request.get("all"))
        leases: List[Dict[str, Any]] = []
        for state_path in sorted((self.state_dir / "jobs").glob("*/state.json")):
            state = read_json(state_path)
            refreshed = self.refresh_state(state)
            if not include_all and str(refreshed.get("status") or "").lower() in {"released", "missing"}:
                continue
            if not include_all and scheduler_state(refreshed) in TERMINAL_STATES:
                continue
            leases.append(self.lease_from_state(refreshed, config, require_endpoint=False))
        return leases

    def release(self, request: Dict[str, Any], config: Dict[str, Any]) -> None:
        state = self.find_state(request)
        if not state:
            return
        self.validate_expected(request, state)
        job_id = str(state.get("jobId") or "")
        if job_id:
            self.cancel_job(job_id)
        self.remove_job_dir(str(state.get("leaseId")))

    def touch(self, request: Dict[str, Any], config: Dict[str, Any]) -> Dict[str, Any]:
        state = self.find_state(request)
        if not state:
            raise AdapterError("touch could not resolve lease")
        state = self.refresh_state(state)
        state["updatedAt"] = now()
        self.save_state(str(state["leaseId"]), state)
        return self.lease_from_state(state, config, require_endpoint=False)

    def cleanup(self, request: Dict[str, Any], config: Dict[str, Any]) -> Dict[str, Any]:
        dry_run = bool(request.get("dryRun"))
        removed: List[str] = []
        for state_path in sorted((self.state_dir / "jobs").glob("*/state.json")):
            state = self.refresh_state(read_json(state_path))
            lease_id = str(state.get("leaseId") or state_path.parent.name)
            if scheduler_state(state) in TERMINAL_STATES or str(state.get("status")) == "missing":
                removed.append(lease_id)
                if not dry_run:
                    self.remove_job_dir(lease_id)
        action = "would remove" if dry_run else "removed"
        return {"protocolVersion": PROTOCOL_VERSION, "message": f"{action} {len(removed)} Slurm lease state directories"}

    def wait_for_endpoint(self, state: Dict[str, Any], config: Dict[str, Any], keep: bool) -> Dict[str, Any]:
        timeout = int(config.get("acquireTimeoutSeconds") or 1800)
        deadline = time.time() + timeout
        lease_id = str(state["leaseId"])
        endpoint_path = Path(str(state["endpointPath"]))
        while time.time() < deadline:
            if endpoint_path.exists():
                endpoint = read_json(endpoint_path)
                state["endpoint"] = endpoint
                state["status"] = "ready"
                state["updatedAt"] = now()
                self.save_state(lease_id, state)
                return self.lease_from_state(state, config, require_endpoint=True)
            state = self.refresh_state(state)
            current = scheduler_state(state)
            if current in TERMINAL_STATES:
                raise AdapterError(f"Slurm job {state.get('jobId')} ended before publishing SSH endpoint: {current}")
            time.sleep(self.poll_interval)
        if not keep:
            self.cancel_job(str(state.get("jobId") or ""))
        raise AdapterError(f"timed out waiting for Slurm job {state.get('jobId')} to publish SSH endpoint")

    def lease_from_state(self, state: Dict[str, Any], config: Dict[str, Any], require_endpoint: bool) -> Dict[str, Any]:
        endpoint = state.get("endpoint")
        endpoint_path = Path(str(state.get("endpointPath") or ""))
        if endpoint is None and endpoint_path.exists():
            endpoint = read_json(endpoint_path)
            state["endpoint"] = endpoint
        if require_endpoint and not isinstance(endpoint, dict):
            raise AdapterError(f"Slurm lease {state.get('leaseId')} has no endpoint")
        if not isinstance(endpoint, dict):
            endpoint = {}

        host = str(endpoint.get("host") or "")
        port = str(endpoint.get("port") or "22")
        user = str(endpoint.get("user") or config_string(config, "sshUser", "") or os.environ.get("USER") or "")
        key = str(endpoint.get("key") or config_string(config, "sshPrivateKey", "") or state.get("keyPath") or "")
        ready_check = str(endpoint.get("readyCheck") or config_string(config, "readyCheck", DEFAULT_READY_CHECK))
        ssh: Dict[str, Any] = {}
        if host and user:
            ssh = {
                "user": user,
                "host": host,
                "port": port,
                "key": key,
                "readyCheck": ready_check,
            }
            proxy = self.proxy_command(config, state, host, port, user, endpoint)
            if proxy:
                ssh["proxyCommand"] = proxy
                ssh["sshConfigProxy"] = True

        labels = {
            "slug": str(state["slug"]),
            "state": str(state.get("status") or "pending"),
            "slurmJobId": str(state.get("rawJobId") or state.get("jobId") or ""),
        }
        return {
            "leaseId": str(state["leaseId"]),
            "slug": str(state["slug"]),
            "name": str(state["name"]),
            "cloudId": str(state.get("cloudId") or f"slurm/job/{state.get('rawJobId') or state.get('jobId')}"),
            "status": str(state.get("status") or "pending"),
            "serverType": self.server_type(state, config),
            "labels": labels,
            "ssh": ssh,
        }

    def proxy_command(
        self,
        config: Dict[str, Any],
        state: Dict[str, Any],
        host: str,
        port: str,
        user: str,
        endpoint: Dict[str, Any],
    ) -> str:
        explicit = str(endpoint.get("proxyCommand") or config_string(config, "proxyCommand", ""))
        login_host = str(endpoint.get("loginHost") or config_string(config, "loginHost", ""))
        login_user = str(endpoint.get("loginUser") or config_string(config, "loginUser", ""))
        if explicit:
            return render_template(
                explicit,
                state,
                host=host,
                port=port,
                user=user,
                login_host=login_host,
                login_user=login_user,
            )
        if config_string(config, "sshMode", "direct") == "proxy-through-login":
            if not login_host:
                raise AdapterError("sshMode=proxy-through-login requires loginHost")
            login = f"{login_user}@{login_host}" if login_user else login_host
            return f"ssh -W %h:%p {login}"
        return ""

    def sbatch_command(self, config: Dict[str, Any], name: str, job_dir: Path, runner: Path) -> List[str]:
        command = ["sbatch", "--parsable", "--job-name", name, "--output", str(job_dir / "slurm-%j.out")]
        mapping = [
            ("account", "--account"),
            ("partition", "--partition"),
            ("qos", "--qos"),
            ("cpus", "--cpus-per-task"),
            ("mem", "--mem"),
            ("timeLimit", "--time"),
            ("gres", "--gres"),
            ("nodes", "--nodes"),
            ("constraint", "--constraint"),
            ("reservation", "--reservation"),
        ]
        for key, flag in mapping:
            value = config.get(key)
            if value not in (None, ""):
                command.extend([flag, str(value)])
        extra = config.get("extraSbatchArgs") or []
        if not isinstance(extra, list) or not all(isinstance(item, str) and item for item in extra):
            raise AdapterError("extraSbatchArgs must be a string array")
        command.extend(extra)
        command.extend(["--export", "ALL,CBX_LEASE_ID,CBX_SLUG,CBX_NAME,CBX_STATE_DIR,CBX_WORK_ROOT,CBX_SSH_PUBLIC_KEY_FILE,CBX_SSH_USER,CBX_READY_CHECK"])
        command.append(str(runner))
        return command

    def refresh_state(self, state: Dict[str, Any]) -> Dict[str, Any]:
        job_id = str(state.get("jobId") or "")
        if not job_id:
            return state
        status = self.query_job_state(job_id)
        if status:
            state["schedulerState"] = status
            if status in TERMINAL_STATES:
                state["status"] = status.lower()
            elif status == "RUNNING" and str(state.get("status")) != "ready":
                state["status"] = "running"
            elif status == "PENDING":
                state["status"] = "pending"
        else:
            state["status"] = "missing"
        state["updatedAt"] = now()
        self.save_state(str(state["leaseId"]), state)
        return state

    def query_job_state(self, job_id: str) -> str:
        result = subprocess.run(
            ["squeue", "-h", "-j", job_id, "-o", "%T"],
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            check=False,
        )
        for line in result.stdout.splitlines():
            value = line.strip().upper()
            if value:
                return value
        if shutil.which("sacct"):
            result = subprocess.run(
                ["sacct", "-n", "-j", job_id, "--format", "State", "--parsable2"],
                text=True,
                stdout=subprocess.PIPE,
                stderr=subprocess.DEVNULL,
                check=False,
            )
            for line in result.stdout.splitlines():
                value = line.split("|", 1)[0].strip().split()[0].upper()
                if value:
                    return value
        return ""

    def cancel_job(self, job_id: str) -> None:
        if not job_id:
            return
        result = subprocess.run(
            ["scancel", job_id],
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
        if result.returncode == 0:
            return
        status = self.query_job_state(job_id)
        if status and status not in TERMINAL_STATES:
            detail = result.stderr.strip() or result.stdout.strip() or f"exit {result.returncode}"
            raise AdapterError(f"scancel {job_id} failed while job is still {status}: {detail}")

    def ensure_ssh_key(self, job_dir: Path, lease_id: str, config: Dict[str, Any]) -> Path:
        configured = config_string(config, "sshPrivateKey", "")
        if configured:
            path = Path(configured).expanduser().resolve()
            if not path.exists():
                raise AdapterError(f"configured sshPrivateKey does not exist: {path}")
            if not Path(str(path) + ".pub").exists():
                raise AdapterError(f"configured sshPrivateKey public key does not exist: {path}.pub")
            return path
        key_path = job_dir / "id_ed25519"
        if not key_path.exists():
            run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(key_path), "-C", f"crabbox-{lease_id}"])
            key_path.chmod(stat.S_IRUSR | stat.S_IWUSR)
        return key_path

    def runner(self, config: Dict[str, Any]) -> Path:
        configured = config_string(config, "runnerScript", "")
        if configured:
            return Path(configured).expanduser().resolve()
        if self.runner_script:
            return self.runner_script
        return Path(__file__).with_name("runner-unprivileged-sshd.sh")

    def find_state(self, request: Dict[str, Any]) -> Optional[Dict[str, Any]]:
        lease = request.get("lease") or {}
        identifiers = [
            request.get("id"),
            lease.get("leaseId") if isinstance(lease, dict) else None,
            lease.get("slug") if isinstance(lease, dict) else None,
            lease.get("name") if isinstance(lease, dict) else None,
            lease.get("cloudId") if isinstance(lease, dict) else None,
        ]
        expected = request.get("expected") or {}
        if isinstance(expected, dict):
            identifiers.extend([expected.get("leaseId"), expected.get("attemptLeaseId"), expected.get("slug"), expected.get("cloudId")])
        wanted = {str(value) for value in identifiers if value}
        for state_path in sorted((self.state_dir / "jobs").glob("*/state.json")):
            state = read_json(state_path)
            values = {
                str(state.get("leaseId") or ""),
                str(state.get("slug") or ""),
                str(state.get("name") or ""),
                str(state.get("cloudId") or ""),
                f"slurm/job/{state.get('rawJobId') or state.get('jobId')}",
                str(state.get("jobId") or ""),
                str(state.get("rawJobId") or ""),
            }
            if wanted & values:
                return state
        return None

    def validate_expected(self, request: Dict[str, Any], state: Dict[str, Any]) -> None:
        expected = request.get("expected") or {}
        if not isinstance(expected, dict) or not expected:
            return
        checks = {
            "leaseId": str(state.get("leaseId") or ""),
            "attemptLeaseId": str(state.get("leaseId") or ""),
            "slug": str(state.get("slug") or ""),
            "cloudId": str(state.get("cloudId") or ""),
        }
        for key, actual in checks.items():
            want = str(expected.get(key) or "")
            if want and want != actual:
                raise AdapterError(f"expected {key}={want} does not match Slurm lease {actual}")

    def server_type(self, state: Dict[str, Any], config: Dict[str, Any]) -> str:
        parts = ["slurm"]
        for key in ("partition", "account", "qos", "cpus", "mem", "timeLimit", "gres"):
            value = config.get(key)
            if value not in (None, ""):
                parts.append(f"{key}={value}")
        scheduler = state.get("schedulerState")
        if scheduler:
            parts.append(f"state={scheduler}")
        return " ".join(parts)

    def ensure_private_dir(self, path: Path) -> None:
        path.mkdir(parents=True, exist_ok=True)
        try:
            path.chmod(stat.S_IRWXU)
        except OSError:
            pass
        jobs = self.state_dir / "jobs"
        jobs.mkdir(parents=True, exist_ok=True)
        try:
            jobs.chmod(stat.S_IRWXU)
        except OSError:
            pass

    def job_dir(self, lease_id: str) -> Path:
        return self.state_dir / "jobs" / lease_id

    def state_path(self, lease_id: str) -> Path:
        return self.job_dir(lease_id) / "state.json"

    def load_state(self, lease_id: str) -> Optional[Dict[str, Any]]:
        path = self.state_path(lease_id)
        if not path.exists():
            return None
        return read_json(path)

    def save_state(self, lease_id: str, state: Dict[str, Any]) -> None:
        job_dir = self.job_dir(lease_id)
        self.ensure_private_dir(job_dir)
        path = self.state_path(lease_id)
        tmp = path.with_suffix(".json.tmp")
        with tmp.open("w", encoding="utf-8") as handle:
            json.dump(state, handle, indent=2, sort_keys=True)
            handle.write("\n")
        os.replace(tmp, path)

    def remove_job_dir(self, lease_id: str) -> None:
        job_dir = self.job_dir(lease_id)
        if job_dir.exists():
            shutil.rmtree(job_dir)


def run(command: List[str], env: Optional[Dict[str, str]] = None) -> subprocess.CompletedProcess:
    result = subprocess.run(command, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env, check=False)
    if result.returncode != 0:
        stderr = result.stderr.strip()
        stdout = result.stdout.strip()
        detail = stderr or stdout or f"exit {result.returncode}"
        raise AdapterError(f"command failed: {' '.join(command)}: {detail}")
    return result


def read_json(path: Path) -> Dict[str, Any]:
    with path.open("r", encoding="utf-8") as handle:
        value = json.load(handle)
    if not isinstance(value, dict):
        raise AdapterError(f"expected JSON object in {path}")
    return value


def required_string(data: Dict[str, Any], key: str) -> str:
    value = str(data.get(key) or "").strip()
    if not value:
        raise AdapterError(f"missing required desired.{key}")
    return value


def config_string(config: Dict[str, Any], key: str, default: str) -> str:
    value = config.get(key)
    if value in (None, ""):
        return default
    return str(value)


def scheduler_state(state: Dict[str, Any]) -> str:
    return str(state.get("schedulerState") or "").upper()


def public_config(config: Dict[str, Any]) -> Dict[str, Any]:
    redacted: Dict[str, Any] = {}
    for key, value in config.items():
        lower = key.lower()
        if "token" in lower or "secret" in lower or "password" in lower:
            redacted[key] = "<redacted>"
        else:
            redacted[key] = value
    return redacted


def render_template(
    template: str,
    state: Dict[str, Any],
    *,
    host: str,
    port: str,
    user: str,
    login_host: str,
    login_user: str,
) -> str:
    values = {
        "host": host,
        "port": port,
        "user": user,
        "loginHost": login_host,
        "loginUser": login_user,
        "leaseId": str(state.get("leaseId") or ""),
        "slug": str(state.get("slug") or ""),
        "name": str(state.get("name") or ""),
        "jobId": str(state.get("rawJobId") or state.get("jobId") or ""),
    }
    result = template
    for key, value in values.items():
        result = result.replace("{" + key + "}", value)
    return result


def now() -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


if __name__ == "__main__":
    raise SystemExit(main())
