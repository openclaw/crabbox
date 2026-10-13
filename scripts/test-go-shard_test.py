import importlib.util
import contextlib
import io
import json
import pathlib
import re
import subprocess
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location(
    "shard", pathlib.Path(__file__).with_name("test-go-shard.py")
)
shard = importlib.util.module_from_spec(spec)
spec.loader.exec_module(shard)


def owners(names, count):
    listing = "\n".join(names)
    return {name: i for i in range(count) for name in shard.select_tests(listing, i, count)}


@patch.dict(shard.SLOW_TESTS, clear=True)
class ShardTests(unittest.TestCase):
    def test_partition_includes_every_test_once(self):
        names = [f"TestCase{i}" for i in range(101)] + ["FuzzInput", "Example", "Example_run"]
        listing = "\n".join(names + ["BenchmarkIgnored", "ok package 0.1s"])
        for count in (3, 4):
            groups = [shard.select_tests(listing, i, count) for i in range(count)]
            self.assertEqual(sorted(sum(groups, [])), sorted(names))
            self.assertTrue(all(groups))
            for group in groups:
                pattern = re.compile("^(" + "|".join(group) + ")$")
                self.assertEqual([n for n in names if pattern.fullmatch(n)], [n for n in names if n in group])

    def test_adding_tests_moves_no_existing_test(self):
        names = [f"TestCase{i}" for i in range(200)]
        for count in (3, 4):
            before = owners(names, count)
            after = owners(names + ["TestAAAFirst", "TestCase99b", "TestZZZLast"], count)
            self.assertEqual({name: after[name] for name in names}, before)

    def test_slow_tests_are_spread_by_weight(self):
        weights = {"TestA": 100, "TestB": 90, "TestC": 50, "TestD": 40, "TestE": 10}
        with patch.dict(shard.SLOW_TESTS, weights):
            self.assertEqual(owners(list(weights), 2), {"TestA": 0, "TestB": 1, "TestC": 1, "TestD": 0, "TestE": 0})

    def test_missing_slow_test_fails(self):
        with patch.dict(shard.SLOW_TESTS, {"TestRenamed": 60}):
            with self.assertRaisesRegex(ValueError, "TestRenamed"):
                shard.select_tests("TestA\nTestB", 0, 2)

    def test_bad_or_empty_selection_fails(self):
        for index, count in [(-1, 4), (4, 4), (0, 0), (0, 4)]:
            with self.assertRaises(ValueError):
                shard.select_tests("TestOnly", index, count)

    @patch.object(shard.subprocess, "run")
    def test_discovery_failure_is_not_hidden(self, run):
        run.return_value = subprocess.CompletedProcess([], 7, "", "")
        with patch("sys.argv", ["test-go-shard.py", "0", "4"]):
            self.assertEqual(shard.main(), 7)
        self.assertEqual(run.call_count, 1)

    @patch.object(shard.subprocess, "run")
    def test_runs_selected_tests_with_race_and_propagates_failure(self, run):
        run.side_effect = [
            subprocess.CompletedProcess([], 0, "TestA\nTestB\nExample\nFuzzInput\n", ""),
            subprocess.CompletedProcess([], 1),
        ]
        with patch("sys.argv", ["test-go-shard.py", "1", "2"]):
            self.assertEqual(shard.main(), 1)
        self.assertEqual(run.call_args.args[0], [
            "go", "test", "-race", "-count=1", "-timeout=10m", "./internal/cli",
            "-run", "^(Example|FuzzInput|TestB)$",
        ])

    @patch.object(shard, "coverage_test", return_value=7)
    @patch.object(shard.subprocess, "run")
    def test_coverage_is_explicit_and_propagates_failure(self, run, coverage):
        run.return_value = subprocess.CompletedProcess([], 0, "TestA\nTestB\nExample\nFuzzInput\n", "")
        with patch("sys.argv", ["test-go-shard.py", "1", "3", "--coverprofile", "cli-1.out"]):
            self.assertEqual(shard.main(), 7)
        self.assertEqual(run.call_args.args[0], [
            "go", "test", "-count=1", "-timeout=20m", "-covermode=atomic", "./internal/cli", "-list", ".",
        ])
        self.assertEqual(coverage.call_args.args[0], [
            "go", "test", "-count=1", "-timeout=20m", "-covermode=atomic", "./internal/cli",
            "-run", "^(FuzzInput|TestA)$", "-coverprofile=cli-1.out",
        ])

    @patch.object(shard.subprocess, "Popen")
    def test_timings_are_bounded_and_do_not_sum_subtests(self, popen):
        child = popen.return_value.__enter__.return_value
        events = [{"Action": "pass", "Test": f"Test{i}", "Elapsed": i} for i in range(20)]
        events += [{"Action": "pass", "Test": "Test19/child", "Elapsed": 999},
                   {"Action": "pass", "Package": "fixture", "Elapsed": 1000},
                   {"Action": "output", "Output": "diagnostic\n"}]
        child.stdout = [json.dumps(event) + "\n" for event in events]
        child.wait.return_value = 0
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            self.assertEqual(shard.coverage_test(["go", "test"]), 0)
        lines = output.getvalue().splitlines()
        self.assertEqual(lines[0], "diagnostic")
        self.assertEqual(lines[2:], [f"  {i:.3f} Test{i}" for i in reversed(range(10, 20))])
        self.assertEqual(popen.call_args.args[0], ["go", "test", "-json"])

    @patch.object(shard.subprocess, "Popen")
    def test_bad_json_or_failed_test_cannot_succeed(self, popen):
        child = popen.return_value.__enter__.return_value
        for result, lines, expected in [(9, [], 9), (0, ["not JSON\n"], 1)]:
            child.stdout, child.wait.return_value = lines, result
            with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(shard.coverage_test(["go", "test"]), expected)


if __name__ == "__main__":
    unittest.main()
