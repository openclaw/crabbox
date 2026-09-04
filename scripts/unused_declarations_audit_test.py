#!/usr/bin/env python3

from __future__ import annotations

from contextlib import redirect_stderr
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import time
import unittest
from unittest import mock


SCRIPT = Path(__file__).with_name("unused_declarations_audit.py")
SPEC = importlib.util.spec_from_file_location("unused_declarations_audit", SCRIPT)
assert SPEC and SPEC.loader
audit = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(audit)


class UnusedDeclarationsAuditTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        self.source = self.root / "source"
        self.module = self.source / "worker" / "module"
        self.module.mkdir(parents=True)
        (self.module / "main.go").write_text("package main\n", encoding="utf-8")

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def test_valid_report_is_sanitized(self) -> None:
        report = self.root / "report.json"
        report.write_text(
            json.dumps(
                {
                    "Issues": [
                        {
                            "FromLinter": "unused",
                            "Text": "func unusedThing is unused",
                            "Severity": "",
                            "SourceLines": ["private source body"],
                            "SuggestedFixes": [{"TextEdits": ["private replacement"]}],
                            "Pos": {
                                "Filename": str(self.module / "main.go"),
                                "Line": 7,
                                "Column": 2,
                            },
                        }
                    ],
                    "Report": {
                        "Linters": [{"Name": "unused", "Enabled": True}],
                        "Warnings": [],
                    },
                }
            ),
            encoding="utf-8",
        )
        issues = audit.parse_linter_report(
            report, module_root=self.module, source_root=self.source
        )
        self.assertEqual(
            issues,
            [
                {
                    "linter": "unused",
                    "message": "func unusedThing is unused",
                    "path": "worker/module/main.go",
                    "line": 7,
                    "column": 2,
                    "severity": "",
                }
            ],
        )
        serialized = json.dumps(issues)
        self.assertNotIn("private source body", serialized)
        self.assertNotIn("replacement", serialized)

    def test_malformed_report_is_incomplete(self) -> None:
        report = self.root / "report.json"
        report.write_text("{", encoding="utf-8")
        with self.assertRaisesRegex(audit.AuditError, "linter_json_malformed"):
            audit.parse_linter_report(
                report, module_root=self.module, source_root=self.source
            )

    def test_missing_report_is_incomplete(self) -> None:
        report = self.root / "report.json"
        report.write_text('{"Issues": []}', encoding="utf-8")
        with self.assertRaisesRegex(audit.AuditError, "linter_json_missing_report"):
            audit.parse_linter_report(
                report, module_root=self.module, source_root=self.source
            )

    def test_malformed_report_object_is_incomplete(self) -> None:
        report = self.root / "report.json"
        report.write_text('{"Issues": [], "Report": []}', encoding="utf-8")
        with self.assertRaisesRegex(audit.AuditError, "linter_json_malformed"):
            audit.parse_linter_report(
                report, module_root=self.module, source_root=self.source
            )

    def test_null_issue_list_is_incomplete(self) -> None:
        report = self.root / "report.json"
        report.write_text('{"Issues": null, "Report": {}}', encoding="utf-8")
        with self.assertRaisesRegex(audit.AuditError, "linter_json_malformed"):
            audit.parse_linter_report(
                report, module_root=self.module, source_root=self.source
            )

    def test_report_error_is_fixed_and_does_not_leak_secret(self) -> None:
        report = self.root / "report.json"
        report.write_text(
            json.dumps(
                {
                    "Issues": [],
                    "Report": {
                        "Error": "loader exposed secret-token and /private/source.go"
                    },
                }
            ),
            encoding="utf-8",
        )
        with self.assertRaises(audit.AuditError) as raised:
            audit.parse_linter_report(
                report, module_root=self.module, source_root=self.source
            )
        self.assertEqual(str(raised.exception), "linter_report_error")
        self.assertNotIn("secret-token", str(raised.exception))
        self.assertNotIn("/private/source.go", str(raised.exception))

    def test_loader_error_is_incomplete(self) -> None:
        with self.assertRaisesRegex(audit.AuditError, "go_list_loader_error"):
            audit.summarize_packages(
                [{"Dir": str(self.module), "Error": {"Err": "type failure"}}],
                module_root=self.module,
                source_root=self.source,
                include_tests=False,
            )

    def test_eligible_files_follow_tests_mode(self) -> None:
        (self.module / "main_test.go").write_text("package main\n", encoding="utf-8")
        package = {
            "Dir": str(self.module),
            "ImportPath": "example.test/module",
            "Name": "main",
            "GoFiles": ["main.go"],
            "TestGoFiles": ["main_test.go"],
        }
        without_tests, main_packages = audit.summarize_packages(
            [package],
            module_root=self.module,
            source_root=self.source,
            include_tests=False,
        )
        with_tests, _ = audit.summarize_packages(
            [package],
            module_root=self.module,
            source_root=self.source,
            include_tests=True,
        )
        self.assertEqual(without_tests, ["worker/module/main.go"])
        self.assertEqual(
            with_tests,
            ["worker/module/main.go", "worker/module/main_test.go"],
        )
        self.assertEqual(main_packages, ["example.test/module"])

    def test_path_outside_source_is_rejected(self) -> None:
        with self.assertRaisesRegex(audit.AuditError, "path_outside_source"):
            audit.repo_relative_path(
                str(self.root / "private.go"),
                module_root=self.module,
                source_root=self.source,
            )

    def test_timeout_budget_reaps_process(self) -> None:
        stdout = io.BytesIO()
        stderr = io.BytesIO()
        with tempfile.TemporaryFile() as out, tempfile.TemporaryFile() as err:
            result = audit.run_capture(
                [
                    "python3",
                    "-c",
                    "import time; time.sleep(30)",
                ],
                cwd=self.root,
                env={},
                stdout=out,
                stderr=err,
                timeout_seconds=0.05,
            )
        self.assertTrue(result.timed_out)
        self.assertLess(result.duration, 12)
        self.assertEqual(stdout.getvalue(), b"")
        self.assertEqual(stderr.getvalue(), b"")

    def test_host_verification_precedes_target_environment(self) -> None:
        events: list[tuple[str, bool]] = []

        def fake_tool(*args, **kwargs):
            events.append(("verify", "GOOS" in kwargs["env"]))
            return {"version": "2.12.2", "builtWithGo": "go1.26.2"}

        def fake_go(*args, **kwargs):
            events.append(("go", "GOOS" in kwargs["env"]))
            return "go1.26.5"

        with (
            mock.patch.object(
                audit, "isolated_host_env", return_value={"HOME": str(self.root)}
            ),
            mock.patch.object(audit, "verify_host_tool", side_effect=fake_tool),
            mock.patch.object(audit, "verify_host_go", side_effect=fake_go),
        ):
            host, _, _ = audit.prepare_execution_environment(
                Path("/tool"),
                "2.12.2",
                cwd=self.module,
                private_root=self.root,
                deadline=time.monotonic() + 10,
            )
            target = audit.target_env(host, "windows", "arm64")
            events.append(("target", "GOOS" in target))
        self.assertEqual(events, [("verify", False), ("go", False), ("target", True)])

    def test_child_environment_drops_unrelated_secrets(self) -> None:
        with mock.patch.dict(
            audit.os.environ,
            {"PATH": "/bin", "CRABBOX_PROVIDER_TOKEN": "secret"},
            clear=True,
        ):
            env = audit.isolated_host_env(self.root)
        self.assertEqual(env["PATH"], "/bin")
        self.assertNotIn("CRABBOX_PROVIDER_TOKEN", env)

    def test_identity_requires_exact_commit(self) -> None:
        with mock.patch.object(audit, "git_value", return_value="wrong"):
            with self.assertRaisesRegex(audit.AuditError, "checkout_identity_mismatch"):
                audit.verify_checkout(self.source, "expected", deadline=100)

    def test_absolute_job_budget_caps_process_window(self) -> None:
        deadline, effective, job_limited = audit.compute_audit_deadline(
            job_started_epoch=1000,
            job_deadline_epoch=3100,
            upload_reserve_seconds=300,
            budget_overhead_seconds=60,
            process_deadline_seconds=1560,
            wall_time=1400,
            monotonic_time=50,
        )
        self.assertEqual(deadline, 1390)
        self.assertEqual(effective, 1340)
        self.assertTrue(job_limited)

    def test_early_job_keeps_26_minute_process_window(self) -> None:
        deadline, effective, job_limited = audit.compute_audit_deadline(
            job_started_epoch=1000,
            job_deadline_epoch=3100,
            upload_reserve_seconds=300,
            budget_overhead_seconds=60,
            process_deadline_seconds=1560,
            wall_time=1001,
            monotonic_time=50,
        )
        self.assertEqual(deadline, 1610)
        self.assertEqual(effective, 1560)
        self.assertFalse(job_limited)

    def test_exhausted_job_budget_is_incomplete(self) -> None:
        with self.assertRaisesRegex(audit.AuditError, "job_budget_exhausted"):
            audit.compute_audit_deadline(
                job_started_epoch=1000,
                job_deadline_epoch=3100,
                upload_reserve_seconds=300,
                budget_overhead_seconds=60,
                process_deadline_seconds=1560,
                wall_time=2741,
                monotonic_time=50,
            )

    def test_deadline_policy_requires_five_minute_upload_reserve(self) -> None:
        base = [
            "--source-root",
            str(self.source),
            "--harness-root",
            str(self.source),
            "--module",
            "worker/module",
            "--goos",
            "linux",
            "--goarch",
            "amd64",
            "--tests",
            "false",
            "--tool",
            "/tool",
            "--tool-version",
            "2.12.2",
            "--tool-archive-sha256",
            "a" * 64,
            "--config",
            "/config",
            "--output-dir",
            "/output",
            "--private-root",
            "/private",
            "--source-sha",
            "a" * 40,
            "--harness-sha",
            "b" * 40,
            "--job-started-epoch",
            "1000",
            "--job-deadline-epoch",
            "3100",
            "--upload-reserve-seconds",
            "299",
        ]
        with redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            audit.parse_args(base)


if __name__ == "__main__":
    unittest.main()
