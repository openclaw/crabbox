import importlib.util
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


class ShardTests(unittest.TestCase):
    def test_partition_includes_every_test_once(self):
        names = [f"TestCase{i}" for i in range(101)] + ["FuzzInput", "Example", "Example_run"]
        listing = "\n".join(names + ["BenchmarkIgnored", "ok package 0.1s"])
        groups = [shard.select_tests(listing, i, 4) for i in range(4)]
        self.assertEqual(sorted(sum(groups, [])), sorted(names))
        self.assertEqual({len(g) for g in groups}, {26})
        for group in groups:
            pattern = re.compile("^(" + "|".join(group) + ")$")
            self.assertEqual([n for n in names if pattern.fullmatch(n)], [n for n in names if n in group])

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
            "-run", "^(FuzzInput|TestB)$",
        ])


if __name__ == "__main__":
    unittest.main()
