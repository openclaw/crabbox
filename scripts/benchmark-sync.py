#!/usr/bin/env python3
"""Reproducible sync fixtures and phase measurements (macOS/Linux, Python 3)."""

import argparse
import json
import os
from pathlib import Path
import random
import subprocess
import tarfile
import io
import shutil
import shlex
import tempfile
import time
import fcntl


def git(root, *args):
    return subprocess.check_output(["git", "-C", str(root), *args])


def generate(root, size):
    root.mkdir(parents=True, exist_ok=False)
    count, length, binaries = {
        "small": (1000, 4096, []),
        "medium": (20000, 12288, [50]),
        "large": (100000, 6144, [50, 100, 200]),
    }[size]
    (root / ".gitignore").write_text("node_modules/\n")
    for i in range(count):
        path = root / "src" / f"pkg{i // 100:04}" / f"file{i:06}.txt"
        path.parent.mkdir(parents=True, exist_ok=True)
        line = f"synthetic source {i:06}\n".encode()
        path.write_bytes((line * (length // len(line) + 1))[:length])
    rng = random.Random(17)
    for i, mib in enumerate(binaries):
        with (root / f"asset{i}.bin").open("wb") as output:
            for _ in range(mib):
                output.write(rng.randbytes(1024 * 1024))
    for i in range(count // 10):
        path = root / "node_modules" / f"pkg{i // 10:04}" / f"output{i}.js"
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(b"ignored build output\n" * 512)
    git(root, "init", "-q")
    git(root, "add", ".")
    git(root, "-c", "user.name=Sync Benchmark", "-c", "user.email=benchmark@example.com",
        "-c", "commit.gpgsign=false", "commit", "-qm", "benchmark fixture")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("root", type=Path, help="disposable fixture, never a working checkout")
    parser.add_argument("--generate", choices=["small", "medium", "large"])
    parser.add_argument("--generate-from", type=Path, help="archive HEAD of a realistic repository into a new fixture")
    parser.add_argument("--changes", type=int, choices=[0, 1, 100], default=0)
    parser.add_argument("--test-binary", type=Path)
    parser.add_argument("--phase", default="manifest|fingerprint|plan")
    parser.add_argument("--output", type=Path)
    parser.add_argument("--crabbox", type=Path, help="also time a full run against an already prepared target")
    parser.add_argument("--config", type=Path, help="explicit trusted config for the SSH target")
    parser.add_argument("--run-arg", action="append", default=[], help="repeat with --run-arg=--flag or --run-arg=value")
    args = parser.parse_args()
    root = args.root.resolve()
    if args.generate:
        generate(root, args.generate)
        (root / ".git" / "crabbox-benchmark").write_text("disposable\n")
    if args.generate_from:
        root.mkdir(parents=True, exist_ok=False)
        archive = git(args.generate_from, "archive", "HEAD")
        with tarfile.open(fileobj=io.BytesIO(archive)) as source:
            source.extractall(root, filter="data")
        git(root, "init", "-q")
        git(root, "add", ".")
        git(root, "-c", "user.name=Sync Benchmark", "-c", "user.email=benchmark@example.com",
            "-c", "commit.gpgsign=false", "commit", "-qm", "benchmark fixture")
        (root / ".git" / "crabbox-benchmark").write_text("disposable\n")
    if not (root / ".git" / "crabbox-benchmark").is_file():
        parser.error("fixture must carry .git/crabbox-benchmark; use --generate")
    fixture_lock = (root / ".git" / "crabbox-benchmark.lock").open("w")
    fcntl.flock(fixture_lock, fcntl.LOCK_EX)
    paths = sorted((root / "src").glob("pkg*/file*.txt"))
    if not paths:
        paths = [root / name.decode() for name in git(root, "ls-files", "-z").split(b"\0")
                 if name.startswith(b"docs/") and name.endswith(b".md") and
                 (root / name.decode()).is_file() and not (root / name.decode()).is_symlink()][:100]
    # Each requested scenario starts from exactly the committed content.
    for path in paths[:100]:
        content = git(root, "show", "HEAD:" + path.relative_to(root).as_posix())
        if path.read_bytes() != content:
            path.write_bytes(content)
    for path in paths[:args.changes]:
        content = path.read_bytes()
        path.write_bytes(b"changed " + content[8:])
    if args.test_binary:
        env = dict(os.environ, CRABBOX_BENCH_REPO=str(root), GOMAXPROCS="4")
        command = [str(args.test_binary.resolve()), "-test.run=^$", "-test.bench=^BenchmarkSyncEfficiency/(" + args.phase + ")$",
                   "-test.benchtime=1x", "-test.count=1", "-test.timeout=8m"]
        with tempfile.TemporaryDirectory(prefix="crabbox-bench-staging-") as staging:
            env["TMPDIR"] = staging
            result = subprocess.run(command, env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=500)
        record = {"root": str(root), "changes": args.changes, "command": command,
                  "returncode": result.returncode, "output": result.stdout}
        print(result.stdout, end="")
        if args.output:
            args.output.write_text(json.dumps(record, indent=2) + "\n")
        if result.returncode:
            raise SystemExit(result.returncode)
    if args.crabbox:
        env = dict(os.environ)
        if args.config:
            env["CRABBOX_CONFIG"] = str(args.config.resolve())
        with tempfile.TemporaryDirectory(prefix="crabbox-bench-tools-") as tools:
            calls = Path(tools) / "calls"
            calls.touch()
            for name in ["git", "ssh", "rsync"]:
                executable = shutil.which(name)
                if not executable:
                    parser.error(f"missing {name}")
                wrapper = Path(tools) / name
                wrapper.write_text("#!/bin/sh\nprintf '%s\\n' " + shlex.quote(name) + " >> " + shlex.quote(str(calls)) +
                                   "\nexec " + shlex.quote(executable) + ' "$@"\n')
                wrapper.chmod(0o700)
            env["PATH"] = tools + os.pathsep + env["PATH"]
            command = [str(args.crabbox.resolve()), "run", "--no-hydrate", "--keep", "--timing-json", *args.run_arg, "--", "true"]
            started = time.monotonic()
            result = subprocess.run(command, cwd=root, env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=480)
            elapsed = time.monotonic() - started
            events = calls.read_text().splitlines()
        timings = []
        for line in result.stderr.splitlines():
            if line.startswith('{"provider":'):
                timings.append(json.loads(line))
        record = {"root": str(root), "changes": args.changes, "command": command, "wallSeconds": elapsed,
                  "processes": {name: events.count(name) for name in ["git", "ssh", "rsync"]},
                  "returncode": result.returncode, "timing": timings,
                  "stdout": result.stdout, "stderr": result.stderr}
        print(json.dumps({key: value for key, value in record.items() if key not in ["stdout", "stderr"]}))
        if args.output:
            args.output.write_text(json.dumps(record, indent=2) + "\n")
        raise SystemExit(result.returncode)


if __name__ == "__main__":
    main()
