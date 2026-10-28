#!/usr/bin/env python3
"""Disposable local-only borrower proof. Never uses the configured broker or hosts.

Requires Docker, a native CLI at bin/crabbox and a Linux CLI at
bin/crabbox-host-linux (matching the Docker engine architecture), plus a built
worker/dist-node/server.mjs. All containers, networks, accounts, and coordinator
state are owned by this invocation. No cloud capacity or real credentials.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
import json
import os
from pathlib import Path
import secrets
import pty
import select
import shutil
import subprocess
import tempfile
import time
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parents[1]


def command(args, **kwargs):
    return subprocess.run([str(a) for a in args], check=True, text=True, capture_output=True, **kwargs).stdout.strip()


def eventually(fn, seconds=60):
    end = time.monotonic() + seconds
    last = None
    while time.monotonic() < end:
        try:
            value = fn()
            if value:
                return value
        except (OSError, ValueError, subprocess.CalledProcessError) as e:
            last = type(e).__name__
        time.sleep(0.5)
    raise RuntimeError(f"condition did not settle ({last})")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--evidence", required=True, type=Path)
    parser.add_argument("--upgrade-worker", type=Path, help="built latest-stable worker tree for persisted registration upgrade proof")
    opts = parser.parse_args()
    opts.evidence.mkdir(parents=True, exist_ok=True)
    prefix = "cbx-host-proof-" + secrets.token_hex(4)
    containers = []
    clients = []
    image = prefix + ":target"
    evidence = {"authority": {}, "checks": [], "scope": "isolated local Docker coordinator and Linux host; synthetic principals"}
    def record(check):
        evidence["checks"].append(check)
        print("PASS: " + check, flush=True)

    with tempfile.TemporaryDirectory(prefix=prefix) as temp:
        work = Path(temp)
        os.chmod(work, 0o700)
        admin, borrower = secrets.token_hex(32), secrets.token_hex(32)
        envfile = work / "coordinator.env"
        envfile.write_text(f"CRABBOX_ADMIN_TOKEN={admin}\nCRABBOX_SHARED_TOKEN={borrower}\nCRABBOX_SHARED_OWNER=borrower@example.com\nCRABBOX_DEFAULT_ORG=example-org\nDATABASE_URL=postgresql://postgres@db/postgres\nPORT=8080\nCRABBOX_PORTABLE_POOLS_ENABLED=true\n")
        envfile.chmod(0o600)
        agentenv = work / "agent.env"
        agentenv.write_text(f"CRABBOX_COORDINATOR=http://coordinator:8080\nCRABBOX_COORDINATOR_TOKEN={admin}\nCRABBOX_OWNER=operator@example.com\nCRABBOX_CONFIG=/tmp/host-empty.yaml\n")
        agentenv.chmod(0o600)
        cli = ROOT / "bin/crabbox"
        targetcli = ROOT / "bin/crabbox-host-linux"
        if not cli.is_file() or not targetcli.is_file():
            raise RuntimeError("build native and Linux CLIs first")
        command(["docker", "network", "create", prefix])
        try:
            db = prefix + "-db"
            command(["docker", "run", "-d", "--name", db, "--network", prefix, "--network-alias", "db", "-e", "POSTGRES_HOST_AUTH_METHOD=trust", "postgres:17-bookworm"])
            containers.append(db)
            eventually(lambda: command(["docker", "exec", db, "pg_isready", "-U", "postgres"]))
            coordinator = prefix + "-coordinator"
            def start_coordinator(worker_tree):
                command(["docker", "run", "-d", "--name", coordinator, "--network", prefix, "--network-alias", "coordinator", "--env-file", envfile,
                         "-p", "127.0.0.1::8080", "-v", f"{worker_tree}:/worker:ro", "-w", "/worker", "node:24.22.1-bookworm", "node", "dist-node/server.mjs"])
            start_coordinator(opts.upgrade_worker or ROOT / "worker")
            containers.append(coordinator)
            port = command(["docker", "port", coordinator, "8080/tcp"]).split(":")[-1]
            url = "http://127.0.0.1:" + port

            def api(path, body=None, token=borrower, method=None, owner=None):
                headers = {"Authorization": "Bearer " + token, "Content-Type": "application/json"}
                if owner:
                    headers["X-Crabbox-Owner"] = owner
                data = None if body is None else json.dumps(body).encode()
                request = urllib.request.Request(url + path, data=data, method=method, headers=headers)
                try:
                    with urllib.request.urlopen(request, timeout=45) as response:
                        return response.status, json.load(response)
                except urllib.error.HTTPError as e:
                    return e.code, json.load(e)

            eventually(lambda: api("/v1/health")[0] == 200)
            record("isolated coordinator health")
            if opts.upgrade_worker:
                registration = {"provider": "ssh", "target": "linux", "host": "existing.example.invalid", "sshUser": "alice", "workRoot": "/work/existing", "ttlSeconds": 3600, "idleTimeoutSeconds": 1800}
                legacy_id = "cbx_eeeeeeeeeeee"
                status, saved = api("/v1/leases/" + legacy_id + "/registration", registration, method="PUT")
                assert status in (200, 201), (status, saved)
                saved = saved["lease"]
                command(["docker", "rm", "-f", coordinator])
                start_coordinator(ROOT / "worker")
                port = command(["docker", "port", coordinator, "8080/tcp"]).split(":")[-1]
                url = "http://127.0.0.1:" + port
                eventually(lambda: api("/v1/health")[0] == 200)
                status, loaded = api("/v1/leases/" + legacy_id)
                assert status == 200, (status, loaded)
                for field in ("id", "owner", "org", "provider", "target", "host", "sshUser", "workRoot", "createdAt", "registeredAt"):
                    assert loaded["lease"][field] == saved[field], field
                status, refreshed = api("/v1/leases/" + legacy_id + "/registration", registration, method="PUT")
                assert status in (200, 201) and refreshed["lease"]["createdAt"] == saved["createdAt"], (status, refreshed)
                record("latest-stable PostgreSQL registration survives upgrade and refresh with identity preserved")
                evidence["upgrade"] = {"scope": "persisted direct-SSH registration; PostgreSQL reused across stable/head server replacement", "loaded": True, "refreshed": True}
            status, _ = api("/v1/ssh-hosts", token="synthetic-invalid")
            assert status in (401, 403), status
            record("unauthorized principal denied")
            build = work / "build"
            build.mkdir()
            shutil.copy2(targetcli, build / "crabbox")
            (build / "Dockerfile").write_text("FROM node:24.22.1-bookworm\nRUN apt-get update && apt-get install -y --no-install-recommends bubblewrap rsync openssh-server procps passwd util-linux && rm -rf /var/lib/apt/lists/*\nCOPY crabbox /usr/local/bin/crabbox\nRUN printf '{}\\n' > /tmp/host-empty.yaml\nCMD [\"sleep\",\"infinity\"]\n")
            command(["docker", "build", "-q", "-t", image, build])
            target = prefix + "-target"
            command(["docker", "run", "-d", "--name", target, "--network", prefix, "--env-file", agentenv,
                     "--init", "--privileged", image])
            containers.append(target)
            command(["docker", "exec", target, "sed", "-i", "s/^USERGROUPS_ENAB.*/USERGROUPS_ENAB no/", "/etc/login.defs"])
            enrolled = json.loads(command(["docker", "exec", target, "crabbox", "host", "enroll", "--name", "disposable-linux", "--template", "proof-v1", "--capabilities", "test"]))
            host = enrolled["host"]["id"]
            status, _ = api("/v1/ssh-hosts/" + host, {"enabled": True}, admin, "PATCH", "operator@example.com")
            assert status == 200, status
            command(["docker", "exec", "-d", target, "sh", "-c", "exec crabbox host serve > /tmp/agent.log 2>&1"])
            eventually(lambda: api("/v1/ssh-hosts")[1]["hosts"][0]["online"])
            repo = work / "repo"
            command(["git", "clone", "--quiet", "--depth=1", "https://github.com/githubtraining/hellogitworld.git", repo], timeout=60)
            (repo / "hello.txt").write_text("borrower-sync-proof\n")
            command(["git", "-C", repo, "add", "hello.txt"])
            config = work / "empty.yaml"
            config.write_text("{}\n")
            cli_env = {"PATH": os.environ["PATH"], "HOME": str(work / "home"), "XDG_CONFIG_HOME": str(work / "config"), "XDG_CACHE_HOME": str(work / "cache"),
                       "CRABBOX_CONFIG": str(config), "CRABBOX_COORDINATOR": url, "CRABBOX_COORDINATOR_TOKEN": borrower, "CRABBOX_OWNER": "borrower@example.com"}
            for p in ("home", "config", "cache"):
                (work / p).mkdir()
            (work / "home/Library/Caches").mkdir(parents=True)
            architecture = command(["docker", "exec", target, "uname", "-m"])
            architecture = "arm64" if architecture == "aarch64" else "amd64"
            output = command([cli, "run", "--debug", "--provider", "ssh-host", "--arch", architecture, "--keep", "--ttl", "10m", "--idle-timeout", "2m", "--shell", "cat hello.txt; printf managed-host-exec-ok"], env=cli_env, cwd=repo, timeout=150)
            assert "borrower-sync-proof" in output and "managed-host-exec-ok" in output, output
            record("borrower CLI sync and execution over outbound SSH relay")
            hosts = api("/v1/ssh-hosts")[1]["hosts"]
            assert hosts[0]["state"] == "allocated"
            leases = api("/v1/leases")[1]["leases"]
            lease = next(l for l in leases if l.get("provider") == "ssh-host" and l["state"] == "active")
            assert "sshHostAllocation" not in lease
            # Use the shipped interactive command with an actual local terminal.
            master, slave = pty.openpty()
            interactive = subprocess.Popen([str(cli), "connect", "--provider", "ssh-host", "--id", lease["id"]], env=cli_env, cwd=repo, stdin=slave, stdout=slave, stderr=slave)
            clients.append(interactive)
            os.close(slave)
            os.write(master, b"test -t 0 && test -t 1 && printf 'interactive-%s\\n' ok; exit\n")
            transcript = b""
            end = time.monotonic() + 60
            while time.monotonic() < end:
                if select.select([master], [], [], 0.2)[0]:
                    try:
                        chunk = os.read(master, 65536)
                    except OSError:
                        break
                    if not chunk:
                        break
                    transcript += chunk
                if interactive.poll() is not None:
                    break
            os.close(master)
            assert interactive.wait(timeout=5) == 0 and b"interactive-ok" in transcript, transcript[-2000:]
            record("shipped CLI connect opens an isolated interactive PTY shell")
            evidence["authority"]["allowedExecution"] = "borrower-sync-proof / managed-host-exec-ok / interactive-ok"
            # Even a privileged foreign identity must not acquire the owner's relay.
            code, _ = api("/v1/serve/leases/" + lease["id"] + "/relay", {}, token=admin, owner="foreign@example.com")
            assert code in (403, 404), code
            evidence["authority"]["foreignIdentityRelayHTTP"] = code
            retained_output = work / "retained-output"
            retained_error = work / "retained-error"
            with retained_output.open("w") as out, retained_error.open("w") as err:
                retained = subprocess.Popen([str(cli), "connect", "--provider", "ssh-host", "--id", lease["id"]], env=cli_env, cwd=repo, stdin=subprocess.PIPE, stdout=out, stderr=err, text=True)
            clients.append(retained)
            retained.stdin.write("printf 'retained-%s\\n' ready\n")
            retained.stdin.flush()
            eventually(lambda: "retained-ready" in retained_output.read_text())
            command([cli, "stop", "--provider", "ssh-host", "--id", lease["id"]], env=cli_env, cwd=repo, timeout=90)
            eventually(lambda: api("/v1/ssh-hosts")[1]["hosts"][0]["state"] == "available")
            assert retained.wait(timeout=30) != 0, "revocation did not terminate the retained session"
            try:
                retained.stdin.write("echo forbidden-after-revocation\n")
                retained.stdin.flush()
            except (BrokenPipeError, OSError):
                pass
            try:
                retained.stdin.close()
            except BrokenPipeError:
                pass
            assert "forbidden-after-revocation" not in retained_output.read_text()
            code, _ = api("/v1/serve/leases/" + lease["id"] + "/relay", {})
            assert code in (403, 404, 409), code
            evidence["authority"]["revokedNewRelayHTTP"] = code
            evidence["authority"]["retainedConnection"] = "retained-ready before revoke; disconnected after revoke; no post-revocation output"
            record("revocation disconnects a retained shell and rejects a new relay")
            users = command(["docker", "exec", target, "getent", "passwd"])
            assert "cbh" + lease["id"][4:] not in users
            record("return confirms account/process/workspace cleanup before availability")
            # Exercise the normal portable pool path, including fresh borrower keys.
            command([cli, "warmup", "--provider", "ssh-host", "--arch", architecture, "--ttl", "20m", "--idle-timeout", "10m"], env=cli_env, cwd=repo, timeout=150)
            lease = next(l for l in api("/v1/leases")[1]["leases"] if l.get("provider") == "ssh-host" and l["state"] == "active")
            identity = work / "pool-identity.json"
            identity.write_text(command([cli, "pool", "identity", "builders", "--id", lease["id"], "--cache-compatibility", "proof-v1"], env=cli_env, cwd=repo) + "\n")
            command([cli, "pool", "register", "builders", "--id", lease["id"], "--identity-file", identity, "--compatibility-key", "proof", "--access"], env=cli_env, cwd=repo, timeout=90)
            endpoint_keys = []
            for _ in range(2):
                output = command([cli, "run", "--provider", "ssh-host", "--pool", "builders", "--pool-access", "--pool-identity-file", identity, "--pool-compatibility-key", "proof", "--pool-duration", "2m", "--no-hydrate", "--shell", 'set -eu; test ! -e "$HOME/previous-borrower"; touch "$HOME/previous-borrower"; cat hello.txt; echo pooled-host-exec-ok'], env=cli_env, cwd=repo, timeout=150)
                assert "pooled-host-exec-ok" in output and "borrower-sync-proof" in output, output
                endpoint_keys.append(next(l for l in api("/v1/leases")[1]["leases"] if l["id"] == lease["id"])["sshHostKey"])
            assert len(set(endpoint_keys)) == 2, "endpoint identity did not rotate"
            record("portable pool enrollment, two borrower generations, sync/run/return and cleanup-before-reuse")
            command([cli, "stop", "--provider", "ssh-host", "--id", lease["id"]], env=cli_env, cwd=repo, timeout=90)
            eventually(lambda: api("/v1/ssh-hosts")[1]["hosts"][0]["state"] == "available")

            # Use independently named synthetic principals against the real HTTP
            # listener and PostgreSQL transaction owner, not an in-memory fixture.
            proofkey = work / "proof-key"
            command(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", proofkey])
            publickey = (work / "proof-key.pub").read_text().strip()
            def acquire(i, ttl=600):
                body = {"provider": "ssh-host", "leaseID": f"cbx_{i:012x}", "sshPublicKey": publickey, "target": "linux", "architecture": architecture, "ttlSeconds": ttl, "idleTimeoutSeconds": ttl}
                return api("/v1/leases", body, admin, owner=f"borrower-{i}@example.com")
            with ThreadPoolExecutor(max_workers=8) as executor:
                responses = list(executor.map(acquire, range(1, 9)))
            winners = [(i+1, response) for i, response in enumerate(responses) if response[0] == 202]
            assert len(winners) == 1 and all(code in (202, 409) for code, _ in responses), [(code, body.get("error")) for code, body in responses]
            winner, (_, response) = winners[0]
            lease = response["lease"]
            assert acquire(winner)[1]["lease"]["id"] == lease["id"]
            command(["docker", "restart", coordinator])
            port = command(["docker", "port", coordinator, "8080/tcp"]).split(":")[-1]
            url = "http://127.0.0.1:" + port
            eventually(lambda: api("/v1/health")[0] == 200)
            assert acquire(winner)[1]["lease"]["id"] == lease["id"]
            assert acquire(20)[0] == 409
            record("eight simultaneous borrowers have one winner; same-intent retry and coordinator restart preserve custody")
            eventually(lambda: next(l for l in api("/v1/leases", token=admin)[1]["leases"] if l["id"] == lease["id"])["state"] == "active")
            # The shared bearer is a different, authenticated NON-admin borrower
            # from the allocation winner; check the real HTTP/relay admission.
            code, _ = api("/v1/serve/leases/" + lease["id"] + "/relay", {})
            assert code in (403, 404), code
            evidence["authority"]["foreignNonAdminRelayHTTP"] = code
            record("authenticated non-admin foreign borrower cannot mint a host execution relay")
            # Break only the owned disposable account identity. Cleanup must
            # quarantine rather than delete an account it no longer recognizes.
            user = "cbh" + lease["id"][4:]
            marker = command(["docker", "exec", target, "getent", "passwd", user]).split(":")[4]
            command(["docker", "exec", target, "usermod", "--comment", "foreign-account", user])
            code, _ = api("/v1/leases/" + lease["id"] + "/release", {}, token=admin, owner=f"borrower-{winner}@example.com")
            assert code in (200, 202), code
            eventually(lambda: api("/v1/ssh-hosts")[1]["hosts"][0]["state"] == "quarantined")
            assert acquire(21)[0] == 409
            command(["docker", "exec", target, "getent", "passwd", user])
            record("failed cleanup quarantines host and refuses reallocation without deleting a foreign account")
            command(["docker", "exec", target, "usermod", "--comment", marker, user])
            eventually(lambda: api("/v1/ssh-hosts")[1]["hosts"][0]["state"] == "available")

            status, response = acquire(30, 60)
            assert status == 202, (status, response)
            expired = response["lease"]["id"]
            eventually(lambda: api("/v1/ssh-hosts")[1]["hosts"][0]["state"] == "available", seconds=100)
            assert "cbh" + expired[4:] not in command(["docker", "exec", target, "getent", "passwd"])
            record("unattended lease expires and account/workspace are removed before reuse")
            for i, signal in enumerate(("KILL", "TERM"), 31):
                status, response = acquire(i)
                assert status == 202, (status, response)
                interrupted = response["lease"]["id"]
                eventually(lambda: next(l for l in api("/v1/leases", token=admin)[1]["leases"] if l["id"] == interrupted)["state"] == "active")
                command(["docker", "exec", target, "pkill", "-" + signal, "-f", "^crabbox host serve$"])
                eventually(lambda: subprocess.run(["docker", "exec", target, "pgrep", "-f", "^crabbox host serve$"], capture_output=True).returncode == 1)
                command(["docker", "exec", "-d", target, "sh", "-c", "exec crabbox host serve >> /tmp/agent.log 2>&1"])
                eventually(lambda: api("/v1/ssh-hosts")[1]["hosts"][0]["state"] == "available")
                assert next(l for l in api("/v1/leases", token=admin)[1]["leases"] if l["id"] == interrupted)["state"] == "failed"
            record("host agent crash and graceful restart settle retained accounts and fail interrupted leases before reuse")
            status, _ = api("/v1/ssh-hosts/" + host, {"draining": True}, admin, "PATCH", "operator@example.com")
            assert status == 200 and acquire(33)[0] == 409
            status, _ = api("/v1/ssh-hosts/" + host, token=admin, method="DELETE", owner="operator@example.com")
            assert status == 200
            record("drain blocks placement; clean inventory removal leaves physical target running")
            assert command(["docker", "inspect", "--format", "{{.State.Running}}", target]) == "true"
            evidence["status"] = "passed"
        except Exception as error:
            evidence["status"] = "failed"
            evidence["failure"] = type(error).__name__
            evidence["failureMessage"] = str(error)
            if isinstance(error, (subprocess.CalledProcessError, subprocess.TimeoutExpired)):
                # Synthetic-only diagnostics; never persist the child environment.
                diagnostic = error.stderr or ""
                if isinstance(diagnostic, bytes):
                    diagnostic = diagnostic.decode(errors="replace")
                evidence["diagnostic"] = diagnostic[-10000:]
            for name in containers:
                if name.endswith("-target"):
                    try:
                        evidence["agentLog"] = command(["docker", "exec", name, "cat", "/tmp/agent.log"])[-6000:]
                    except subprocess.CalledProcessError:
                        pass
                if name.endswith("-coordinator"):
                    result = subprocess.run(["docker", "logs", "--tail", "30", name], text=True, capture_output=True)
                    evidence["coordinatorLog"] = (result.stdout + result.stderr)[-6000:]
            raise
        finally:
            for client in clients:
                if client.poll() is None:
                    client.kill()
                client.wait()
            for name in reversed(containers):
                subprocess.run(["docker", "rm", "-f", name], capture_output=True)
            subprocess.run(["docker", "network", "rm", prefix], capture_output=True)
            subprocess.run(["docker", "image", "rm", image], capture_output=True)
            (opts.evidence / "result.json").write_text(json.dumps(evidence, indent=2) + "\n")
    print(json.dumps(evidence, indent=2))


if __name__ == "__main__":
    main()
