#!/usr/bin/env python3
"""Scenario-label regression tests; no Go build, lease or network required."""

import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
import contextlib
import importlib.util
import io
import json
from unittest import mock


SCRIPT = Path(os.environ.get("CRABBOX_BENCH_SCRIPT", Path(__file__).with_name("benchmark-sync.py"))).resolve()


class Admission(unittest.TestCase):
    """Evidence checks use synthetic executables, never Git commits or Go builds."""

    @classmethod
    def setUpClass(cls):
        spec = importlib.util.spec_from_file_location("sync_benchmark", SCRIPT)
        cls.benchmark = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(cls.benchmark)

    @staticmethod
    def line(phase="fingerprint-full", ns="123000", files=1, extra=""):
        return (f"BenchmarkSyncEfficiency/{phase}-4 1 {ns} ns/op "
                f"0 git/op 0 inblock/op 0 read-bytes/op 0 hashes/op {files} files "
                f"8 candidate-bytes 0 dirty-bytes{extra}\n")

    def scenario(self, a, b=None, phase="fingerprint-full", extra=(), exit_a=0, executable_b=True):
        with tempfile.TemporaryDirectory() as temp:
            temp = Path(temp).resolve()
            root = temp / "fixture"
            (root / ".git").mkdir(parents=True)
            (root / ".git/crabbox-benchmark").touch()
            source = root / "src/pkg0000/file000000.txt"
            source.parent.mkdir(parents=True)
            source.write_bytes(b"fixture\n")
            binaries = []
            for label, output, code in [("a", a, exit_a), ("b", a if b is None else b, 0)]:
                binary = temp / label
                binary.write_text("#!/usr/bin/env python3\nimport sys\nsys.stdout.write(" + repr(output) +
                                  ")\nraise SystemExit(" + str(code) + ")\n")
                binary.chmod(0o700 if label != "b" or executable_b else 0o600)
                binaries.append(binary)
            artifact = temp / "result.json"
            argv = [str(SCRIPT), str(root), "--baseline-test-binary", str(binaries[0]),
                    "--test-binary", str(binaries[1]), "--phase", phase, "--output", str(artifact),
                    "--timeout", "2", "--scenario-timeout", "20", *extra]
            env = {key: os.environ[key] for key in ["PATH", "HOME", "TMPDIR", "LANG"] if key in os.environ}
            stderr = io.StringIO()
            code = 0
            with mock.patch.object(self.benchmark, "git", return_value=b"fixture\n") as git, \
                    mock.patch.object(sys, "argv", argv), mock.patch.dict(os.environ, env, clear=True), \
                    contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(stderr):
                try:
                    self.benchmark.main()
                except SystemExit as exc:
                    code = exc.code or 0
            if git.called:
                git.assert_called_once_with(root, "show", "HEAD:src/pkg0000/file000000.txt")
            self.assertEqual(source.read_bytes(), b"fixture\n")
            def reject_nonfinite(value):
                raise ValueError("nonfinite JSON number: " + value)
            record = json.loads(artifact.read_text(), parse_constant=reject_nonfinite) if artifact.exists() else None
            return code, record, stderr.getvalue()

    def test_common_selectors(self):
        for selector, expected in [("fingerprint-full", {"fingerprint-full"}),
                                   ("manifest|fingerprint|plan", {"manifest", "fingerprint", "plan"}),
                                   (".*", self.benchmark.PHASE_NAMES)]:
            with self.subTest(selector=selector):
                self.assertEqual(self.benchmark.expected_phase_names(selector, []), expected)
        for explicit in [[""], [" manifest"], ["manifest", "manifest"]]:
            with self.subTest(explicit=explicit), self.assertRaises(ValueError):
                self.benchmark.expected_phase_names(".*", explicit)

    def test_custom_go_regex_keeps_its_syntax(self):
        selector = r"\Qfingerprint-full\E"
        code, record, _ = self.scenario(self.line(), phase=selector,
                                        extra=("--expected-phase", "fingerprint-full"))
        self.assertEqual(code, 0)
        self.assertIn("(" + selector + ")$", record["samples"][0]["command"][2])
        code, record, error = self.scenario(self.line(), phase=selector)
        self.assertEqual(code, 2)
        self.assertIsNone(record)
        self.assertIn("--expected-phase", error)

    def test_valid_pair_matrix_and_zero_counters(self):
        code, record, _ = self.scenario(self.line())
        self.assertEqual(code, 0)
        self.assertEqual(record["status"], "complete")
        self.assertEqual([s["label"] for s in record["samples"]], ["A", "B", "A", "B", "A", "B"])
        self.assertTrue(all(s["validation"]["valid"] for s in record["samples"]))
        self.assertEqual(record["summary"]["fingerprint-full"]["pairDeltaPercent"], [0, 0, 0])
        self.assertEqual(record["samples"][0]["phases"]["fingerprint-full"]["hashes/op"], 0)

    def test_full_phase_selection(self):
        output = "".join(self.line(name) for name in sorted(self.benchmark.PHASE_NAMES))
        code, record, _ = self.scenario(output, phase=".*")
        self.assertEqual(code, 0)
        self.assertEqual(set(record["summary"]), self.benchmark.PHASE_NAMES)

    def test_invalid_success_output_is_preserved_and_rejected(self):
        outputs = ["", "PASS\n", self.line("manifest"), self.line(ns="NaN"),
                   self.line(ns="-1"), self.line() + self.line(), self.line(ns="0")]
        for output in outputs:
            with self.subTest(output=output):
                code, record, error = self.scenario(output)
                self.assertEqual(code, 1)
                self.assertEqual(record["status"], "failed")
                self.assertNotIn("summary", record)
                self.assertEqual(len(record["samples"]), 1)
                self.assertEqual(record["samples"][0]["stdout"], output)
                self.assertFalse(record["samples"][0]["validation"]["valid"])
                self.assertIn("benchmark evidence rejected", error)

    def test_partial_pair_failure_keeps_completed_sample(self):
        code, record, _ = self.scenario(self.line(), self.line("manifest"))
        self.assertEqual(code, 1)
        self.assertEqual(len(record["samples"]), 2)
        self.assertTrue(record["samples"][0]["validation"]["valid"])
        self.assertFalse(record["samples"][1]["validation"]["valid"])
        self.assertNotIn("summary", record)

    def test_fixture_counters_must_match_between_samples_and_phases(self):
        code, record, _ = self.scenario(self.line(), self.line(files=2))
        self.assertEqual(code, 1)
        self.assertEqual(len(record["samples"]), 2)
        self.assertIn("fixture counters differ", record["validationErrors"][0])
        code, record, _ = self.scenario(self.line("manifest") + self.line(files=2),
                                        phase="manifest|fingerprint-full")
        self.assertEqual(code, 1)
        self.assertEqual(len(record["samples"]), 1)

    def test_subprocess_failure_is_not_passing_evidence(self):
        code, record, _ = self.scenario(self.line(), exit_a=3)
        self.assertEqual(code, 1)
        self.assertEqual(record["samples"][0]["returncode"], 3)
        self.assertFalse(record["samples"][0]["validation"]["valid"])
        self.assertNotIn("summary", record)

    def test_launch_failure_preserves_the_completed_sample(self):
        code, record, _ = self.scenario(self.line(), executable_b=False)
        self.assertEqual(code, 1)
        self.assertEqual(len(record["samples"]), 2)
        self.assertTrue(record["samples"][0]["validation"]["valid"])
        self.assertIn("launchError", record["samples"][1])
        self.assertIsNone(record["samples"][1]["returncode"])
        self.assertIsNone(record["samples"][1]["wallSeconds"])
        self.assertEqual(record["status"], "failed")
        self.assertNotIn("summary", record)

    def test_parser_rejects_invalid_counters_and_rows(self):
        valid = self.line()
        outputs = [valid.replace("0 git/op", f"{value} git/op") for value in ["NaN", "Infinity", "-1"]]
        outputs += [valid.replace("0 git/op ", ""), valid.replace("1 123000", "0 123000"),
                    valid.replace("1 123000", "x 123000"), valid.replace(" ns/op", " ms/op"),
                    self.line(extra=" 0 files"), self.line(extra=" 1 seconds"), self.line(extra=" 0 ns/op"),
                    self.line(extra=" unmatched"), self.line(ns="1e309"), self.line(ns="1e-320")]
        for output in outputs:
            with self.subTest(output=output), self.assertRaises(ValueError):
                self.benchmark.phase_metrics(output)

    def test_header_and_name_without_worker_suffix(self):
        output = "BenchmarkSyncEfficiency/fingerprint-full\n" + self.line().replace("fingerprint-full-4", "fingerprint-full")
        phases = self.benchmark.phase_metrics(output)
        self.assertEqual(set(phases), {"fingerprint-full"})

    def test_counter_extensions_are_validated_without_being_required(self):
        phases = self.benchmark.phase_metrics(self.line(extra=" 0 B/op 0 allocs/op"))
        self.assertEqual(phases["fingerprint-full"]["allocs/op"], 0)
        with self.assertRaises(ValueError):
            self.benchmark.phase_metrics(self.line(extra=" NaN B/op"))

    def test_incomplete_phase_set_is_rejected(self):
        code, record, _ = self.scenario(self.line(), phase="manifest|fingerprint-full")
        self.assertEqual(code, 1)
        self.assertIn("missing: manifest", record["validationErrors"][0])

    def test_timeout_inputs_must_be_finite(self):
        for flag, value in [("--timeout", "nan"), ("--timeout", "inf"), ("--scenario-timeout", "0")]:
            with self.subTest(flag=flag, value=value):
                code, record, error = self.scenario(self.line(), extra=(flag, value))
                self.assertEqual(code, 2)
                self.assertIsNone(record)
                self.assertIn("finite and positive", error)

    def test_scenario_deadline_preserves_partial_evidence(self):
        run = self.benchmark.run_bounded
        with mock.patch.object(self.benchmark.time, "monotonic", return_value=0) as clock:
            def finish_sample(*args, **kwargs):
                result = run(*args, **kwargs)
                clock.return_value = 2
                return result
            with mock.patch.object(self.benchmark, "run_bounded", side_effect=finish_sample):
                code, record, _ = self.scenario(self.line(), extra=("--scenario-timeout", "1"))
        self.assertEqual(code, 1)
        self.assertTrue(record["scenarioTimedOut"])
        self.assertEqual(len(record["samples"]), 1)
        self.assertNotIn("summary", record)

    def test_nonfinite_comparison_and_incomplete_pairs_are_rejected(self):
        samples = [{"label": label, "phases": {"p": {"seconds": seconds}}}
                   for _ in range(3) for label, seconds in [("A", 1e-310), ("B", 1e299)]]
        with self.assertRaises(ValueError):
            self.benchmark.paired_summary(samples, {"p"})
        with self.assertRaises(ValueError):
            self.benchmark.paired_summary(samples[:2], {"p"})


class Scenarios(unittest.TestCase):
    def fixture(self, contents):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)
        (root / "docs").mkdir()
        for i, content in enumerate(contents):
            (root / "docs" / f"file{i}.md").write_bytes(content)
        self.git(root, "init", "-q")
        self.git(root, "add", ".")
        self.git(root, "-c", "user.name=Benchmark", "-c", "user.email=benchmark@example.com",
                 "-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + os.devnull,
                 "commit", "-qm", "fixture")
        (root / ".git" / "crabbox-benchmark").touch()
        return root

    def git(self, root, *args):
        return subprocess.check_output(["git", "-C", str(root), *args], stderr=subprocess.STDOUT)

    def scenario(self, root, changes):
        return subprocess.run([sys.executable, str(SCRIPT), str(root), "--changes", str(changes)],
                              text=True, capture_output=True, timeout=20)

    def test_rejects_insufficient_candidates(self):
        root = self.fixture([b"one\n"])
        result = self.scenario(root, 100)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("only 1 eligible", result.stderr)
        self.assertEqual(self.git(root, "diff", "--name-only"), b"")

    def test_guarantees_same_size_content_change(self):
        for content in [b"changed already\n", b"x"]:
            with self.subTest(content=content):
                root = self.fixture([content])
                result = self.scenario(root, 1)
                self.assertEqual(result.returncode, 0, result.stderr)
                actual = (root / "docs/file0.md").read_bytes()
                self.assertEqual(len(actual), len(content))
                self.assertNotEqual(actual, content)
                self.assertEqual(self.git(root, "diff", "--name-only"), b"docs/file0.md\n")

    def test_rejects_empty_file_edit(self):
        root = self.fixture([b""])
        self.assertNotEqual(self.scenario(root, 1).returncode, 0)


if __name__ == "__main__":
    unittest.main()
