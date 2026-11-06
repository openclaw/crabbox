#!/usr/bin/env python3
"""Opt-in proof against an already deployed, isolated candidate runner.

The caller owns credentials, budget, deployment and provider cleanup observation.
This script creates one kept lease and always attempts exact-handle stop.
"""
import argparse
import hashlib
import json
import re
import shutil
import subprocess
import sys
import tarfile
import time
import zipfile
from pathlib import Path
from urllib.parse import urlsplit


def digest(data):
    return {"bytes": len(data), "sha256": hashlib.sha256(data).hexdigest()}


def verify(stdout, destination, mode):
    records = [line[len("ARTIFACT_PROOF "):] for line in stdout.splitlines()
               if line.startswith("ARTIFACT_PROOF ")]
    if len(records) != 1:
        raise ValueError("expected exactly one remote hash record")
    remote = json.loads(records[0])
    if remote["mode"] != mode or remote["exitCode"] != (23 if mode == "failure" else 0):
        raise ValueError("wrong remote test identity")
    if set(remote["files"]) != {"screen.png", "trace.zip", "result.json"}:
        raise ValueError("unexpected artifact set")
    for name, expected in remote["files"].items():
        if digest((destination / name).read_bytes()) != expected:
            raise ValueError("download hash mismatch: " + name)
    png = (destination / "screen.png").read_bytes()
    if not png.startswith(b"\x89PNG\r\n\x1a\n") or len(png) <= 65536:
        raise ValueError("missing large binary PNG")
    with zipfile.ZipFile(destination / "trace.zip") as trace:
        if trace.testzip() or not any(n.endswith(".trace") for n in trace.namelist()):
            raise ValueError("not an intact Playwright trace")
    result = json.loads((destination / "result.json").read_text())
    if result["mode"] != mode or result["exitCode"] != remote["exitCode"]:
        raise ValueError("downloaded result disagrees with remote command")
    return remote


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--crabbox", type=Path, required=True)
    parser.add_argument("--runner-url", required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--execute", action="store_true")
    args = parser.parse_args()
    parsed = urlsplit(args.runner_url)
    if parsed.scheme != "https" or not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment or parsed.path not in ("", "/"):
        parser.error("runner URL must be an HTTPS origin without credentials")
    binary = args.crabbox.resolve(strict=True)
    output = args.output.resolve()
    plan = {"runner": args.runner_url, "leaseCount": 1, "type": "standard-2",
            "ttl": "45m", "cases": ["fresh success", "reused deliberate exit 23"],
            "deploymentByThisHarness": False, "executed": False}
    if not args.execute:
        print(json.dumps(plan, indent=2))
        return 0
    output.mkdir(parents=True, exist_ok=False)
    fixture = output / "fixture"
    fixture.mkdir()
    shutil.copyfile(Path(__file__).with_name("live-browser.mjs"), fixture / "browser.mjs")
    for source, target in (("browser-package.json", "package.json"),
                           ("browser-package-lock.json", "package-lock.json")):
        shutil.copyfile(Path(__file__).with_name(source), fixture / target)
    (fixture / ".gitignore").write_text('node_modules/\nreports/\n.crabbox/\n')
    subprocess.run(["git", "init", "-q", str(fixture)], check=True)
    subprocess.run(["git", "-C", str(fixture), "add", "."], check=True)
    handle = output / "lease.json"
    receipt = dict(plan, executed=True, crabbox=digest(binary.read_bytes()), cases=[])
    deadline = time.monotonic() + 2700

    def run(label, arguments, timeout):
        started = time.monotonic()
        result = subprocess.run([str(binary)] + arguments, cwd=fixture,
                                capture_output=True, text=True, timeout=timeout)
        (output / (label + ".stdout.txt")).write_text(result.stdout)
        (output / (label + ".stderr.txt")).write_text(result.stderr)
        receipt[label] = {"exitCode": result.returncode, "seconds": time.monotonic() - started}
        return result

    def lease_id():
        value = json.loads(handle.read_text())
        lease = value.get("leaseId", "")
        if value.get("provider") != "cloudflare" or not re.fullmatch(r"cbx_[a-zA-Z0-9]+", lease):
            raise ValueError("invalid exact Cloudflare lease handle; manual cleanup required")
        return lease

    problem = None
    try:
        for mode, expected in (("success", 0), ("failure", 23)):
            destination = output / mode
            destination.mkdir()
            archives_before = set(fixture.glob(".crabbox/runs/*/delegated-artifacts.tgz"))
            arguments = ["run", "--provider", "cloudflare", "--cloudflare-url", args.runner_url,
                         "--cloudflare-workdir", "/workspace/artifact-proof", "--ttl", "45m",
                         "--timing-json", "--artifact-glob", "reports/**"]
            for name in ("screen.png", "trace.zip", "result.json"):
                arguments += ["--download", "reports/" + name + "=" + str(destination / name),
                              "--require-artifact", "reports/" + name]
            command = "node browser.mjs " + mode
            if mode == "success":
                arguments += ["--type", "standard-2", "--keep", "--lease-output", str(handle)]
                command = "npm ci --ignore-scripts --no-audit --no-fund && npx --no-install playwright install --with-deps chromium && " + command
            else:
                arguments += ["--id", lease_id(), "--no-sync"]
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError("45-minute total proof budget expired")
            result = run(mode, arguments + ["--shell", "--", command], remaining)
            if result.returncode != expected:
                raise ValueError(f"{mode}: expected exit {expected}, got {result.returncode}")
            record = verify(result.stdout, destination, mode)
            archives = set(fixture.glob(".crabbox/runs/*/delegated-artifacts.tgz")) - archives_before
            if len(archives) != 1:
                raise ValueError("expected one newly published artifact archive")
            archive = archives.pop()
            with tarfile.open(archive, "r:gz") as bundle:
                if set(bundle.getnames()) != {"reports/" + n for n in record["files"]}:
                    raise ValueError("unexpected archive members")
                for name in record["files"]:
                    if bundle.extractfile("reports/" + name).read() != (destination / name).read_bytes():
                        raise ValueError("archive and explicit download disagree")
            shutil.copyfile(archive, destination / "delegated-artifacts.tgz")
            receipt["cases"].append(record)
    except Exception as error:
        problem = type(error).__name__ + ": " + str(error)
    finally:
        if handle.exists():
            try:
                lease = lease_id()
                stopped = run("stop", ["stop", "--provider", "cloudflare", lease], 120)
                if stopped.returncode:
                    raise ValueError("exact-handle stop failed")
                receipt["leaseId"] = lease
                receipt["cleanup"] = "stop command succeeded; provider terminal/absence observation still required"
            except Exception as error:
                receipt["cleanupError"] = type(error).__name__ + ": " + str(error)
                problem = problem or receipt["cleanupError"]
        else:
            receipt["cleanup"] = "no handle; confirm no allocation or reconcile with provider before retry"
            problem = problem or "no lease handle"
        receipt["result"] = "FAILED" if problem else "ARTIFACT_CASES_PASSED_CLEANUP_OBSERVATION_PENDING"
        receipt["error"] = problem
        (output / "receipt.json").write_text(json.dumps(receipt, indent=2) + "\n")
    print(json.dumps({"result": receipt["result"], "receipt": str(output / "receipt.json")}))
    return 1 if problem else 0


if __name__ == "__main__":
    sys.exit(main())
