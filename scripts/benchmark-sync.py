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
import signal
import statistics
import hashlib
import math
import sys


PHASE_NAMES = {"manifest", "fingerprint", "fingerprint-full", "snapshot-copy", "snapshot-local", "plan"}
REQUIRED_COUNTERS = {"git/op", "inblock/op", "read-bytes/op", "hashes/op", "files", "candidate-bytes", "dirty-bytes"}
FIXTURE_COUNTERS = ("files", "candidate-bytes", "dirty-bytes")


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
    git(root, "config", "gc.auto", "0")
    git(root, "config", "maintenance.auto", "false")
    git(root, "add", ".")
    git(root, "-c", "user.name=Sync Benchmark", "-c", "user.email=benchmark@example.com",
        "-c", "commit.gpgsign=false", "commit", "-qm", "benchmark fixture")


def run_bounded(command, timeout, **kwargs):
    """Kill the complete measurement process group at its deadline; keep output."""
    started = time.monotonic()
    process = subprocess.Popen(command, text=True, stdout=subprocess.PIPE,
                               stderr=subprocess.PIPE, start_new_session=True, **kwargs)
    timed_out = False
    try:
        stdout, stderr = process.communicate(timeout=timeout)
    except subprocess.TimeoutExpired:
        timed_out = True
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        stdout, stderr = process.communicate()
    return {"returncode": 124 if timed_out else process.returncode,
            "timedOut": timed_out, "timeoutSeconds": timeout,
            "wallSeconds": time.monotonic() - started,
            "stdout": stdout, "stderr": stderr}


def phase_metrics(output):
    phases = {}
    for line in output.splitlines():
        if not line.startswith("BenchmarkSyncEfficiency/"):
            continue
        fields = line.split()
        if len(fields) == 1:  # Go may print a benchmark heading before its result.
            continue
        if len(fields) < 4 or fields[3] != "ns/op" or (len(fields) - 4) % 2:
            raise ValueError("malformed benchmark result: " + line)
        name = fields[0].removeprefix("BenchmarkSyncEfficiency/")
        phase, separator, workers = name.rpartition("-")
        if separator and workers.isdecimal():
            name = phase
        if name in phases:
            raise ValueError("duplicate benchmark phase: " + name)
        if int(fields[1]) <= 0:
            raise ValueError("benchmark iteration count must be positive")
        seconds = float(fields[2]) / 1e9
        if not math.isfinite(seconds) or seconds <= 0:
            raise ValueError("benchmark time must be finite and positive: " + fields[2])
        metrics = {"seconds": seconds}
        for i in range(4, len(fields) - 1, 2):
            unit = fields[i + 1]
            if unit in metrics or unit == "ns/op":
                raise ValueError("duplicate benchmark metric: " + unit)
            value = float(fields[i])
            if not math.isfinite(value) or value < 0:
                raise ValueError("benchmark counter must be finite and nonnegative: " + unit)
            metrics[unit] = value
        missing = REQUIRED_COUNTERS - metrics.keys()
        if missing:
            raise ValueError("missing benchmark counters: " + ", ".join(sorted(missing)))
        phases[name] = metrics
    return phases


def expected_phase_names(selector, explicit):
    if explicit:
        if any(not name or name.strip() != name for name in explicit) or len(set(explicit)) != len(explicit):
            raise ValueError("expected phases must be nonempty, unique names")
        return set(explicit)
    if selector == ".*":
        return PHASE_NAMES.copy()
    names = selector.split("|")
    if all(name in PHASE_NAMES for name in names):
        return set(names)
    raise ValueError("custom Go phase regex requires one --expected-phase per expected result")


def validate_phases(phases, expected, fixture_counts):
    if set(phases) != expected:
        missing, unexpected = expected - phases.keys(), phases.keys() - expected
        raise ValueError("benchmark phase coverage mismatch; missing: " + ", ".join(sorted(missing)) +
                         "; unexpected: " + ", ".join(sorted(unexpected)))
    for name, metrics in phases.items():
        counts = tuple(metrics[key] for key in FIXTURE_COUNTERS)
        if fixture_counts is None:
            fixture_counts = counts
        elif counts != fixture_counts:
            raise ValueError("fixture counters differ between phases or samples: " + name)
    return fixture_counts


def paired_summary(samples, expected):
    summary = {}
    for phase in sorted(expected):
        a = [s["phases"][phase]["seconds"] for s in samples if s["label"] == "A"]
        b = [s["phases"][phase]["seconds"] for s in samples if s["label"] == "B"]
        if len(a) != 3 or len(b) != 3:
            raise ValueError("paired summary requires three complete A/B pairs")
        delta = [y - x for x, y in zip(a, b)]
        percent = [100 * (y - x) / x for x, y in zip(a, b)]
        if not all(math.isfinite(value) for value in delta + percent):
            raise ValueError("paired comparison is not finite: " + phase)
        summary[phase] = {"medianA": statistics.median(a), "medianB": statistics.median(b),
                          "pairDeltaSeconds": delta, "pairDeltaPercent": percent}
    return summary


def write_benchmark_record(record, output):
    if output:
        output.write_text(json.dumps(record, indent=2, allow_nan=False) + "\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("root", type=Path, help="disposable fixture, never a working checkout")
    parser.add_argument("--generate", choices=["small", "medium", "large"])
    parser.add_argument("--generate-from", type=Path, help="archive HEAD of a realistic repository into a new fixture")
    parser.add_argument("--changes", type=int, choices=[0, 1, 100], default=0)
    parser.add_argument("--test-binary", type=Path)
    parser.add_argument("--baseline-test-binary", type=Path, help="run three interleaved baseline/candidate pairs")
    parser.add_argument("--timeout", type=float, default=480, help="hard per-sample seconds")
    parser.add_argument("--scenario-timeout", type=float, default=1800, help="hard aggregate sample budget in seconds")
    parser.add_argument("--phase", default="manifest|fingerprint|plan")
    parser.add_argument("--expected-phase", action="append", default=[],
                        help="repeat exact expected phase names for a custom Go --phase regex")
    parser.add_argument("--output", type=Path)
    parser.add_argument("--crabbox", type=Path, help="also time a full run against an already prepared target")
    parser.add_argument("--config", type=Path, help="explicit trusted config for the SSH target")
    parser.add_argument("--run-arg", action="append", default=[], help="repeat with --run-arg=--flag or --run-arg=value")
    args = parser.parse_args()
    if not all(math.isfinite(value) and value > 0 for value in [args.timeout, args.scenario_timeout]):
        parser.error("timeouts must be finite and positive")
    if (args.baseline_test_binary or args.expected_phase) and not args.test_binary:
        parser.error("--baseline-test-binary and --expected-phase require --test-binary")
    if args.test_binary:
        try:
            expected = expected_phase_names(args.phase, args.expected_phase)
        except ValueError as exc:
            parser.error(str(exc))
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
        git(root, "config", "gc.auto", "0")
        git(root, "config", "maintenance.auto", "false")
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
    if len(paths) < args.changes:
        parser.error(f"requested {args.changes} changes but fixture has only {len(paths)} eligible files")
    # Each requested scenario starts from exactly the committed content.
    for path in paths[:100]:
        content = git(root, "show", "HEAD:" + path.relative_to(root).as_posix())
        if path.read_bytes() != content:
            path.write_bytes(content)
    for path in paths[:args.changes]:
        content = path.read_bytes()
        if not content:
            parser.error(f"cannot make a same-size content edit to empty file {path.relative_to(root)}")
        path.write_bytes(bytes([content[0] ^ 1]) + content[1:])
    if args.test_binary:
        env = dict(os.environ, CRABBOX_BENCH_REPO=str(root), GOMAXPROCS="4")
        sequence = [("B", args.test_binary)]
        if args.baseline_test_binary:
            sequence = [(label, binary) for _ in range(3) for label, binary in
                        [("A", args.baseline_test_binary), ("B", args.test_binary)]]
        record = {"root": str(root), "changes": args.changes, "samples": [], "status": "running",
                  "expectedPhases": sorted(expected), "validationErrors": [],
                  "scenarioTimeoutSeconds": args.scenario_timeout, "binaries": {}}
        fixture_counts = None
        for label, binary in sequence:
            if label not in record["binaries"]:
                with binary.open("rb") as executable:
                    digest = hashlib.file_digest(executable, "sha256").hexdigest()
                record["binaries"][label] = {"path": str(binary.resolve()), "sha256": digest}
        deadline = time.monotonic() + args.scenario_timeout
        for index, (label, binary) in enumerate(sequence):
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                record["scenarioTimedOut"] = True
                record["validationErrors"].append("scenario deadline exhausted before the next sample")
                record["status"] = "failed"
                break
            command = [str(binary.resolve()), "-test.run=^$", "-test.bench=^BenchmarkSyncEfficiency/(" + args.phase + ")$",
                       "-test.benchtime=1x", "-test.count=1", "-test.timeout=8m"]
            print(f"sample {index + 1}/{len(sequence)} {label}, timeout={min(args.timeout, remaining):.1f}s", flush=True)
            with tempfile.TemporaryDirectory(prefix="crabbox-bench-staging-") as staging:
                env["TMPDIR"] = staging
                try:
                    result = run_bounded(command, min(args.timeout, remaining), env=env)
                except OSError as exc:
                    result = {"returncode": None, "timedOut": False, "timeoutSeconds": min(args.timeout, remaining),
                              "wallSeconds": None, "stdout": "", "stderr": "", "launchError": str(exc)}
            result.update({"label": label, "command": command, "phases": {}})
            errors = []
            if result["returncode"] != 0 or result["timedOut"]:
                errors.append("benchmark subprocess failed or timed out")
            if "launchError" in result:
                errors.append(result["launchError"])
            try:
                result["phases"] = phase_metrics(result["stdout"])
                fixture_counts = validate_phases(result["phases"], expected, fixture_counts)
            except ValueError as exc:
                errors.append(str(exc))
            result["validation"] = {"valid": not errors, "errors": errors}
            record["samples"].append(result)
            elapsed = f"{result['wallSeconds']:.3f}s" if result["wallSeconds"] is not None else "not started"
            print(f"sample {label} exit={result['returncode']} wall={elapsed}", flush=True)
            if errors:
                record["status"] = "failed"
                record["validationErrors"].extend(errors)
            write_benchmark_record(record, args.output)
            if errors:
                break
        if record["status"] != "failed":
            record["status"] = "complete"
            if args.baseline_test_binary:
                try:
                    record["summary"] = paired_summary(record["samples"], expected)
                except ValueError as exc:
                    record["status"] = "failed"
                    record["validationErrors"].append(str(exc))
        write_benchmark_record(record, args.output)
        if "summary" in record:
            print(json.dumps(record["summary"], allow_nan=False), flush=True)
        if record["status"] == "failed":
            print("benchmark evidence rejected: " + "; ".join(record["validationErrors"]), file=sys.stderr)
            raise SystemExit(1)
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
            result = run_bounded(command, args.timeout, cwd=root, env=env)
            elapsed = result["wallSeconds"]
            events = calls.read_text().splitlines()
        timings = []
        for line in result["stderr"].splitlines():
            if line.startswith('{"provider":'):
                timings.append(json.loads(line))
        record = {"root": str(root), "changes": args.changes, "command": command, "wallSeconds": elapsed,
                  "processes": {name: events.count(name) for name in ["git", "ssh", "rsync"]},
                  "returncode": result["returncode"], "timedOut": result["timedOut"], "timeoutSeconds": args.timeout, "timing": timings,
                  "stdout": result["stdout"], "stderr": result["stderr"]}
        print(json.dumps({key: value for key, value in record.items() if key not in ["stdout", "stderr"]}))
        if args.output:
            args.output.write_text(json.dumps(record, indent=2) + "\n")
        raise SystemExit(result["returncode"])


if __name__ == "__main__":
    main()
