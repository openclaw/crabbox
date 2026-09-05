#!/usr/bin/env python3
"""Run one bounded, report-only golangci-lint unused-analysis cell."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import signal
import subprocess
import sys
import tempfile
import time
from typing import Any, BinaryIO, Sequence


SCHEMA_VERSION = 1
LIMITATIONS = [
    "U1000 findings are review leads, not reachability proof or deletion authority.",
    "Constant groups, interface-indirect uses, and anonymous fields are conservatively retained.",
    "Smoke and localcontainer build tags are omitted.",
    "The audit does not exercise Docker, race instrumentation, or CGO.",
    "Darwin vmdembed and vmdrelease modes require genuine payloads and are omitted.",
    "Only linux, darwin, and windows targets on amd64 and arm64 are covered.",
    "Targets are type-loaded on a Linux amd64 host; no native execution is claimed.",
]
LOADER_ERROR_RE = re.compile(
    r"(?:typecheck|could not load export data|failed to load|build constraints exclude all Go files)",
    re.IGNORECASE,
)
SAFE_ENV_KEYS = (
    "COMSPEC",
    "GOROOT",
    "HTTPS_PROXY",
    "HTTP_PROXY",
    "LANG",
    "LC_ALL",
    "NO_PROXY",
    "PATH",
    "PATHEXT",
    "RUNNER_TEMP",
    "SSL_CERT_DIR",
    "SSL_CERT_FILE",
    "SYSTEMROOT",
    "TEMP",
    "TMP",
    "TMPDIR",
    "TZ",
    "WINDIR",
)


class AuditError(RuntimeError):
    """An evidence failure that makes the cell incomplete."""


class CommandResult:
    def __init__(self, returncode: int | None, timed_out: bool, duration: float):
        self.returncode = returncode
        self.timed_out = timed_out
        self.duration = duration


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def atomic_json(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(
        json.dumps(payload, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    temporary.replace(path)


def run_capture(
    argv: Sequence[str],
    *,
    cwd: Path,
    env: dict[str, str],
    stdout: BinaryIO,
    stderr: BinaryIO,
    timeout_seconds: float,
) -> CommandResult:
    if timeout_seconds <= 0:
        return CommandResult(None, True, 0.0)

    popen_kwargs: dict[str, Any] = {}
    if os.name == "nt":
        popen_kwargs["creationflags"] = subprocess.CREATE_NEW_PROCESS_GROUP
    else:
        popen_kwargs["start_new_session"] = True

    started = time.monotonic()
    process = subprocess.Popen(
        list(argv),
        cwd=cwd,
        env=env,
        stdin=subprocess.DEVNULL,
        stdout=stdout,
        stderr=stderr,
        **popen_kwargs,
    )
    try:
        returncode = process.wait(timeout=timeout_seconds)
        return CommandResult(returncode, False, time.monotonic() - started)
    except subprocess.TimeoutExpired:
        if os.name == "nt":
            process.send_signal(signal.CTRL_BREAK_EVENT)
        else:
            os.killpg(process.pid, signal.SIGTERM)
        try:
            process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            if os.name == "nt":
                process.kill()
            else:
                os.killpg(process.pid, signal.SIGKILL)
            process.wait(timeout=10)
        return CommandResult(process.returncode, True, time.monotonic() - started)


def require_remaining(deadline: float) -> float:
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise AuditError("job_budget_exhausted")
    return remaining


def git_value(root: Path, expression: str, *, deadline: float) -> str:
    result = subprocess.run(
        ["git", "-C", str(root), "rev-parse", expression],
        check=True,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        timeout=min(10, require_remaining(deadline)),
    )
    return result.stdout.strip()


def verify_checkout(
    root: Path, expected_commit: str, *, deadline: float
) -> dict[str, str]:
    actual_commit = git_value(root, "HEAD", deadline=deadline)
    if actual_commit != expected_commit:
        raise AuditError("checkout_identity_mismatch")
    return {
        "commit": actual_commit,
        "tree": git_value(root, "HEAD^{tree}", deadline=deadline),
    }


def isolated_host_env(private_root: Path) -> dict[str, str]:
    env = {key: os.environ[key] for key in SAFE_ENV_KEYS if key in os.environ}
    home = private_root / "home"
    cache = private_root / "golangci-cache"
    config = private_root / "config"
    gopath = private_root / "go"
    for directory in (home, cache, config, gopath):
        directory.mkdir(parents=True, exist_ok=True)
    env.update(
        {
            "GOLANGCI_LINT_CACHE": str(cache.resolve()),
            "GOLANGCI_LINT_TELEMETRY": "off",
            "GOTELEMETRY": "off",
            "GOPATH": str(gopath.resolve()),
            "HOME": str(home.resolve()),
            "XDG_CONFIG_HOME": str(config.resolve()),
        }
    )
    return env


def target_env(host_env: dict[str, str], goos: str, goarch: str) -> dict[str, str]:
    env = host_env.copy()
    env.update(
        {
            "CGO_ENABLED": "0",
            "GOARCH": goarch,
            "GOFLAGS": "-mod=readonly -trimpath",
            "GOOS": goos,
            "GOTOOLCHAIN": "local",
            "GOWORK": "off",
        }
    )
    return env


def parse_version_document(path: Path, expected_version: str) -> dict[str, str]:
    try:
        document = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as error:
        raise AuditError("tool_version_malformed") from error
    if not isinstance(document, dict):
        raise AuditError("tool_version_malformed")

    normalized = {str(key).lower(): value for key, value in document.items()}
    version = normalized.get("version")
    if not isinstance(version, str) or version.lstrip("v") != expected_version.lstrip("v"):
        raise AuditError("tool_version_mismatch")
    identity = {"version": version.lstrip("v")}
    go_version = normalized.get("goversion")
    if isinstance(go_version, str) and re.fullmatch(r"go\d+\.\d+(?:\.\d+)?", go_version):
        identity["builtWithGo"] = go_version
    if identity.get("builtWithGo") != "go1.26.2":
        raise AuditError("tool_go_version_mismatch")
    return identity


def verify_host_tool(
    tool: Path,
    expected_version: str,
    *,
    cwd: Path,
    env: dict[str, str],
    private_root: Path,
    timeout_seconds: float,
) -> dict[str, str]:
    stdout_path = private_root / "tool-version.json"
    stderr_path = private_root / "tool-version.stderr"
    with stdout_path.open("wb") as stdout, stderr_path.open("wb") as stderr:
        result = run_capture(
            [str(tool), "version", "--json"],
            cwd=cwd,
            env=env,
            stdout=stdout,
            stderr=stderr,
            timeout_seconds=min(timeout_seconds, 30),
        )
    if result.timed_out:
        raise AuditError("tool_version_timeout")
    if result.returncode != 0:
        raise AuditError("tool_version_failed")
    return parse_version_document(stdout_path, expected_version)


def verify_host_go(
    *,
    cwd: Path,
    env: dict[str, str],
    private_root: Path,
    timeout_seconds: float,
) -> str:
    stdout_path = private_root / "go-version.txt"
    stderr_path = private_root / "go-version.stderr"
    with stdout_path.open("wb") as stdout, stderr_path.open("wb") as stderr:
        result = run_capture(
            ["go", "version"],
            cwd=cwd,
            env=env,
            stdout=stdout,
            stderr=stderr,
            timeout_seconds=min(timeout_seconds, 30),
        )
    if result.timed_out:
        raise AuditError("go_version_timeout")
    if result.returncode != 0:
        raise AuditError("go_version_failed")
    match = re.search(r"\b(go\d+\.\d+\.\d+)\b", stdout_path.read_text(encoding="utf-8"))
    if not match or match.group(1) != "go1.26.5":
        raise AuditError("go_version_mismatch")
    return match.group(1)


def prepare_execution_environment(
    tool: Path,
    expected_tool_version: str,
    *,
    cwd: Path,
    private_root: Path,
    deadline: float,
) -> tuple[dict[str, str], dict[str, str], str]:
    host_env = isolated_host_env(private_root)
    tool_identity = verify_host_tool(
        tool,
        expected_tool_version,
        cwd=cwd,
        env=host_env,
        private_root=private_root,
        timeout_seconds=require_remaining(deadline),
    )
    go_version = verify_host_go(
        cwd=cwd,
        env=host_env,
        private_root=private_root,
        timeout_seconds=require_remaining(deadline),
    )
    return host_env, tool_identity, go_version


def compute_audit_deadline(
    *,
    job_started_epoch: int,
    job_deadline_epoch: int,
    upload_reserve_seconds: int,
    budget_overhead_seconds: int,
    process_deadline_seconds: int,
    wall_time: float,
    monotonic_time: float,
) -> tuple[float, int, bool]:
    if job_started_epoch <= 0 or job_deadline_epoch - job_started_epoch != 35 * 60:
        raise AuditError("job_budget_invalid")
    if wall_time + 5 < job_started_epoch:
        raise AuditError("job_budget_invalid")
    job_available = (
        job_deadline_epoch
        - upload_reserve_seconds
        - budget_overhead_seconds
        - wall_time
    )
    effective_seconds = min(float(process_deadline_seconds), job_available)
    if effective_seconds <= 0:
        raise AuditError("job_budget_exhausted")
    return (
        monotonic_time + effective_seconds,
        max(0, int(effective_seconds)),
        job_available <= process_deadline_seconds,
    )


def decode_json_stream(path: Path) -> list[dict[str, Any]]:
    try:
        raw = path.read_text(encoding="utf-8")
    except (OSError, UnicodeDecodeError) as error:
        raise AuditError("go_list_malformed") from error
    decoder = json.JSONDecoder()
    offset = 0
    documents: list[dict[str, Any]] = []
    while True:
        while offset < len(raw) and raw[offset].isspace():
            offset += 1
        if offset >= len(raw):
            break
        try:
            value, offset = decoder.raw_decode(raw, offset)
        except json.JSONDecodeError as error:
            raise AuditError("go_list_malformed") from error
        if not isinstance(value, dict):
            raise AuditError("go_list_malformed")
        documents.append(value)
    if not documents:
        raise AuditError("go_list_missing")
    return documents


def repo_relative_path(filename: str, *, module_root: Path, source_root: Path) -> str:
    candidate = Path(filename)
    if not candidate.is_absolute():
        candidate = module_root / candidate
    try:
        relative = candidate.resolve(strict=False).relative_to(source_root.resolve())
    except ValueError as error:
        raise AuditError("path_outside_source") from error
    posix = PurePosixPath(*relative.parts).as_posix()
    if posix in ("", ".") or posix.startswith("../"):
        raise AuditError("path_outside_source")
    return posix


def summarize_packages(
    documents: list[dict[str, Any]],
    *,
    module_root: Path,
    source_root: Path,
    include_tests: bool,
) -> tuple[list[str], list[str]]:
    files: set[str] = set()
    main_packages: set[str] = set()
    module_directory = module_root.resolve()
    packages: list[tuple[dict[str, Any], Path, dict[str, list[str]]]] = []

    for package in documents:
        if package.get("Error") or package.get("DepsErrors"):
            raise AuditError("go_list_loader_error")
        directory_value = package.get("Dir")
        if not isinstance(directory_value, str):
            continue
        directory = Path(directory_value).resolve()
        try:
            directory.relative_to(module_directory)
        except ValueError:
            continue

        for_test = package.get("ForTest", "")
        if not isinstance(for_test, str):
            raise AuditError("go_list_malformed")
        file_lists: dict[str, list[str]] = {}
        for field in ("GoFiles", "CgoFiles", "TestGoFiles", "XTestGoFiles"):
            values = package.get(field, [])
            if not isinstance(values, list) or any(
                not isinstance(value, str) or not value for value in values
            ):
                raise AuditError("go_list_malformed")
            file_lists[field] = values
        packages.append((package, directory, file_lists))

    synthetic_test_mains: set[int] = set()
    for index, (package, directory, file_lists) in enumerate(packages):
        import_path = package.get("ImportPath")
        if (
            package.get("Name") != "main"
            or package.get("ForTest", "")
            or not isinstance(import_path, str)
            or not import_path.endswith(".test")
            or len(file_lists["GoFiles"]) != 1
        ):
            continue
        base_import = import_path[: -len(".test")]
        if not base_import:
            continue
        for partner_index, (partner, partner_directory, partner_files) in enumerate(
            packages
        ):
            if partner_index == index:
                continue
            if (
                partner.get("ImportPath") == base_import
                and partner_directory == directory
                and not partner.get("ForTest", "")
                and (partner_files["TestGoFiles"] or partner_files["XTestGoFiles"])
            ):
                synthetic_test_mains.add(index)
                break

    for index, (package, directory, file_lists) in enumerate(packages):
        if index in synthetic_test_mains:
            continue
        fields = ["GoFiles", "CgoFiles"]
        if include_tests:
            fields.extend(("TestGoFiles", "XTestGoFiles"))
        for field in fields:
            for value in file_lists[field]:
                files.add(
                    repo_relative_path(
                        str(directory / value),
                        module_root=module_root,
                        source_root=source_root,
                    )
                )
        import_path = package.get("ImportPath")
        if (
            package.get("Name") == "main"
            and not package.get("ForTest")
            and isinstance(import_path, str)
        ):
            main_packages.add(import_path)
    if not files:
        raise AuditError("go_list_no_eligible_files")
    return sorted(files), sorted(main_packages)


def sanitize_issue(
    raw_issue: dict[str, Any], *, module_root: Path, source_root: Path
) -> dict[str, Any]:
    linter = raw_issue.get("FromLinter", raw_issue.get("fromLinter"))
    text = raw_issue.get("Text", raw_issue.get("text"))
    severity = raw_issue.get("Severity", raw_issue.get("severity", ""))
    position = raw_issue.get("Pos", raw_issue.get("pos"))
    if not isinstance(linter, str) or not isinstance(text, str) or not isinstance(position, dict):
        raise AuditError("linter_json_malformed")
    if linter != "unused":
        raise AuditError("unexpected_linter")
    filename = position.get("Filename", position.get("filename"))
    line = position.get("Line", position.get("line"))
    column = position.get("Column", position.get("column"))
    if (
        not isinstance(filename, str)
        or not isinstance(line, int)
        or isinstance(line, bool)
        or line < 1
        or not isinstance(column, int)
        or isinstance(column, bool)
        or column < 0
        or not isinstance(severity, str)
    ):
        raise AuditError("linter_json_malformed")
    return {
        "linter": linter,
        "message": text,
        "path": repo_relative_path(
            filename, module_root=module_root, source_root=source_root
        ),
        "line": line,
        "column": column,
        "severity": severity,
    }


def parse_linter_report(
    path: Path, *, module_root: Path, source_root: Path
) -> list[dict[str, Any]]:
    try:
        document = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as error:
        raise AuditError("linter_json_malformed") from error
    if not isinstance(document, dict):
        raise AuditError("linter_json_malformed")
    if set(document) != {"Issues", "Report"}:
        if "Issues" not in document:
            raise AuditError("linter_json_missing_issues")
        if "Report" not in document:
            raise AuditError("linter_json_missing_report")
        raise AuditError("linter_json_malformed")
    issues = document["Issues"]
    report = document["Report"]
    if not isinstance(issues, list):
        raise AuditError("linter_json_malformed")
    if not isinstance(report, dict) or not set(report).issubset(
        {"Warnings", "Linters", "Error"}
    ):
        raise AuditError("linter_json_malformed")
    report_error = report.get("Error", "")
    if not isinstance(report_error, str):
        raise AuditError("linter_json_malformed")
    if report_error.strip():
        raise AuditError("linter_report_error")
    warnings = report.get("Warnings", [])
    if not isinstance(warnings, list):
        raise AuditError("linter_json_malformed")
    for warning in warnings:
        if (
            not isinstance(warning, dict)
            or not set(warning).issubset({"Tag", "Text"})
            or not isinstance(warning.get("Text"), str)
            or ("Tag" in warning and not isinstance(warning["Tag"], str))
        ):
            raise AuditError("linter_json_malformed")
    linters = report.get("Linters", [])
    if not isinstance(linters, list):
        raise AuditError("linter_json_malformed")
    for linter in linters:
        if (
            not isinstance(linter, dict)
            or not set(linter).issubset({"Name", "Enabled"})
            or not isinstance(linter.get("Name"), str)
            or ("Enabled" in linter and not isinstance(linter["Enabled"], bool))
        ):
            raise AuditError("linter_json_malformed")
    sanitized = []
    for issue in issues:
        if not isinstance(issue, dict):
            raise AuditError("linter_json_malformed")
        sanitized.append(
            sanitize_issue(issue, module_root=module_root, source_root=source_root)
        )
    return sorted(
        sanitized,
        key=lambda issue: (
            issue["path"],
            issue["line"],
            issue["column"],
            issue["message"],
        ),
    )


def stderr_has_loader_error(path: Path) -> bool:
    try:
        text = path.read_text(encoding="utf-8", errors="replace")
    except OSError:
        return True
    return bool(LOADER_ERROR_RE.search(text))


def build_report(args: argparse.Namespace) -> tuple[dict[str, Any], int]:
    source_root = Path(args.source_root).resolve()
    harness_root = Path(args.harness_root).resolve()
    module_root = (source_root / args.module).resolve()
    config = Path(args.config).resolve()
    output_dir = Path(args.output_dir).resolve()
    output_dir.mkdir(parents=True, exist_ok=True)
    private_parent = Path(args.private_root).resolve()
    private_parent.mkdir(parents=True, exist_ok=True)

    reasons: list[str] = []
    effective_budget_seconds = 0
    job_budget_limited = False
    deadline = time.monotonic()
    try:
        deadline, effective_budget_seconds, job_budget_limited = compute_audit_deadline(
            job_started_epoch=args.job_started_epoch,
            job_deadline_epoch=args.job_deadline_epoch,
            upload_reserve_seconds=args.upload_reserve_seconds,
            budget_overhead_seconds=args.budget_overhead_seconds,
            process_deadline_seconds=args.process_deadline_seconds,
            wall_time=time.time(),
            monotonic_time=deadline,
        )
    except AuditError as error:
        reasons.append(str(error))
    findings: list[dict[str, Any]] | None = None
    eligible_files: list[str] = []
    main_packages: list[str] = []
    list_result = CommandResult(None, False, 0.0)
    lint_result = CommandResult(None, False, 0.0)
    linter_loader_error = False
    tool_identity: dict[str, str] = {"version": args.tool_version}
    go_version = ""
    source_identity = {"commit": args.source_sha, "tree": ""}
    harness_identity = {"commit": args.harness_sha, "tree": ""}

    with tempfile.TemporaryDirectory(prefix="unused-audit-", dir=private_parent) as private:
        private_root = Path(private)
        try:
            if reasons:
                raise AuditError(reasons[0])
            source_identity = verify_checkout(
                source_root, args.source_sha, deadline=deadline
            )
            harness_identity = verify_checkout(
                harness_root, args.harness_sha, deadline=deadline
            )
            if not module_root.is_dir() or not (module_root / "go.mod").is_file():
                raise AuditError("module_root_invalid")
            if not config.is_file():
                raise AuditError("config_missing")
            tool = Path(args.tool).resolve()
            if not tool.is_file() or not os.access(tool, os.X_OK):
                raise AuditError("tool_missing")

            host_env, tool_identity, go_version = prepare_execution_environment(
                tool,
                args.tool_version,
                cwd=module_root,
                private_root=private_root,
                deadline=deadline,
            )
            tool_identity["binarySha256"] = sha256_file(tool)
            env = target_env(host_env, args.goos, args.goarch)

            list_stdout = private_root / "go-list.json"
            list_stderr = private_root / "go-list.stderr"
            list_argv = ["go", "list", "-json"]
            if args.tests:
                list_argv.append("-test")
            list_argv.append("./...")
            with list_stdout.open("wb") as stdout, list_stderr.open("wb") as stderr:
                list_result = run_capture(
                    list_argv,
                    cwd=module_root,
                    env=env,
                    stdout=stdout,
                    stderr=stderr,
                    timeout_seconds=max(0, deadline - time.monotonic()),
                )
            if list_result.timed_out:
                reasons.append("go_list_timeout")
                if job_budget_limited:
                    reasons.append("job_budget_exhausted")
            elif list_result.returncode != 0:
                reasons.append("go_list_failed")
            else:
                try:
                    eligible_files, main_packages = summarize_packages(
                        decode_json_stream(list_stdout),
                        module_root=module_root,
                        source_root=source_root,
                        include_tests=args.tests,
                    )
                except AuditError as error:
                    reasons.append(str(error))

            raw_json = private_root / "golangci-unused.json"
            raw_text = private_root / "golangci-unused.txt"
            lint_stderr = private_root / "golangci-unused.stderr"
            lint_argv = [
                str(Path(args.tool).resolve()),
                "run",
                "-c",
                str(config),
                "--enable-only=unused",
                f"--tests={str(args.tests).lower()}",
                "--issues-exit-code=0",
                f"--output.json.path={raw_json}",
                f"--output.text.path={raw_text}",
                "--color=never",
                "./...",
            ]
            with (private_root / "lint.stdout").open("wb") as stdout, lint_stderr.open(
                "wb"
            ) as stderr:
                lint_result = run_capture(
                    lint_argv,
                    cwd=module_root,
                    env=env,
                    stdout=stdout,
                    stderr=stderr,
                    timeout_seconds=max(0, deadline - time.monotonic()),
                )
            if lint_result.timed_out:
                reasons.append("linter_timeout")
                if job_budget_limited:
                    reasons.append("job_budget_exhausted")
            elif lint_result.returncode != 0:
                reasons.append("linter_nonzero_exit")
            linter_loader_error = stderr_has_loader_error(lint_stderr)
            if linter_loader_error:
                reasons.append("linter_loader_error")
            if not lint_result.timed_out and lint_result.returncode == 0:
                try:
                    findings = parse_linter_report(
                        raw_json, module_root=module_root, source_root=source_root
                    )
                except AuditError as error:
                    reasons.append(str(error))
        except (AuditError, OSError, subprocess.SubprocessError) as error:
            reasons.append(str(error) if isinstance(error, AuditError) else "harness_failure")

    reasons = sorted(set(reasons))
    complete = not reasons
    report = {
        "schemaVersion": SCHEMA_VERSION,
        "status": "complete" if complete else "incomplete",
        "completeness": {"complete": complete, "reasons": reasons},
        "identity": {
            "source": source_identity,
            "harness": harness_identity,
            "tool": {
                "name": "golangci-lint",
                **tool_identity,
                "archiveSha256": args.tool_archive_sha256,
                "unusedAnalyzer": "honnef.co/go/tools/unused v0.7.0",
                "languageGateCompatible": tool_identity.get("builtWithGo") == "go1.26.2",
            },
            "config": {"sha256": sha256_file(config) if config.is_file() else ""},
        },
        "cell": {
            "module": args.module,
            "target": {"goos": args.goos, "goarch": args.goarch},
            "tests": args.tests,
            "buildTags": [],
            "cgoEnabled": "0",
            "goVersion": go_version,
        },
        "execution": {
            "goListExitStatus": list_result.returncode,
            "goListTimedOut": list_result.timed_out,
            "linterExitStatus": lint_result.returncode,
            "linterTimedOut": lint_result.timed_out,
            "linterLoaderError": linter_loader_error,
            "jobStartedEpoch": args.job_started_epoch,
            "jobDeadlineEpoch": args.job_deadline_epoch,
            "effectiveAuditBudgetSeconds": effective_budget_seconds,
            "budgetOverheadSeconds": args.budget_overhead_seconds,
            "processDeadlineSeconds": args.process_deadline_seconds,
            "uploadReserveSeconds": args.upload_reserve_seconds,
        },
        "coverage": {
            "eligibleSourceFiles": eligible_files,
            "mainPackages": main_packages,
        },
        "findingCount": len(findings) if complete and findings is not None else None,
        "findings": findings if findings is not None else [],
        "limitations": LIMITATIONS,
    }
    return report, 0 if complete else 1


def render_text(report: dict[str, Any]) -> str:
    cell = report["cell"]
    target = cell["target"]
    lines = [
        f"status: {report['status']}",
        f"source: {report['identity']['source']['commit']}",
        f"harness: {report['identity']['harness']['commit']}",
        f"module: {cell['module']}",
        f"target: {target['goos']}/{target['goarch']}",
        f"tests: {str(cell['tests']).lower()}",
        f"finding-count: {report['findingCount'] if report['findingCount'] is not None else 'unknown'}",
    ]
    if report["completeness"]["reasons"]:
        lines.append("incomplete-reasons: " + ", ".join(report["completeness"]["reasons"]))
    for issue in report["findings"]:
        lines.append(
            f"{issue['path']}:{issue['line']}:{issue['column']}: "
            f"{issue['message']} [{issue['linter']}]"
        )
    lines.extend(["", "coverage limitations:"])
    lines.extend(f"- {item}" for item in report["limitations"])
    return "\n".join(lines) + "\n"


def parse_args(argv: Sequence[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--source-root", required=True)
    parser.add_argument("--harness-root", required=True)
    parser.add_argument("--module", required=True)
    parser.add_argument("--goos", choices=("linux", "darwin", "windows"), required=True)
    parser.add_argument("--goarch", choices=("amd64", "arm64"), required=True)
    parser.add_argument("--tests", choices=("true", "false"), required=True)
    parser.add_argument("--tool", required=True)
    parser.add_argument("--tool-version", required=True)
    parser.add_argument("--tool-archive-sha256", required=True)
    parser.add_argument("--config", required=True)
    parser.add_argument("--output-dir", required=True)
    parser.add_argument("--private-root", required=True)
    parser.add_argument("--source-sha", required=True)
    parser.add_argument("--harness-sha", required=True)
    parser.add_argument("--job-started-epoch", type=int, required=True)
    parser.add_argument("--job-deadline-epoch", type=int, required=True)
    parser.add_argument("--process-deadline-seconds", type=int, default=1560)
    parser.add_argument("--upload-reserve-seconds", type=int, default=300)
    parser.add_argument("--budget-overhead-seconds", type=int, default=60)
    parsed = parser.parse_args(argv)
    parsed.tests = parsed.tests == "true"
    if (
        parsed.process_deadline_seconds < 60
        or parsed.upload_reserve_seconds < 300
        or parsed.budget_overhead_seconds < 30
    ):
        parser.error("unsafe timeout budget")
    return parsed


def main(argv: Sequence[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    report, exit_code = build_report(args)
    output_dir = Path(args.output_dir)
    module_label = "root" if args.module == "." else args.module.replace("/", "-")
    stem = f"unused-{module_label}-{args.goos}-{args.goarch}-tests-{str(args.tests).lower()}"
    atomic_json(output_dir / f"{stem}.json", report)
    (output_dir / f"{stem}.txt").write_text(render_text(report), encoding="utf-8")
    return exit_code


if __name__ == "__main__":
    raise SystemExit(main())
