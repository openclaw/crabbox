import importlib.util
import contextlib
import collections
import io
import json
import os
import pathlib
import re
import shutil
import subprocess
import tempfile
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

    def test_empty_shard_fails_even_when_discovery_has_enough_tests(self):
        names = [f"TestCase{i}" for i in range(20) if shard.hashed_shard(f"TestCase{i}", 2) == 0]
        with self.assertRaisesRegex(ValueError, "empty shard"):
            shard.select_tests("\n".join(names), 1, 2)

    @patch.object(shard.subprocess, "run")
    def test_discovery_failure_is_not_hidden(self, run):
        run.return_value = subprocess.CompletedProcess([], 7, "", "")
        with patch("sys.argv", ["test-go-shard.py", "0", "4"]):
            self.assertEqual(shard.main(), 7)
        self.assertEqual(run.call_count, 1)

    @patch.object(shard, "go_test", return_value=1)
    @patch.object(shard.subprocess, "run")
    def test_runs_selected_tests_with_race_and_propagates_failure(self, run, go_test):
        run.return_value = subprocess.CompletedProcess([], 0, "TestA\nTestB\nExample\nFuzzInput\n", "")
        with patch("sys.argv", ["test-go-shard.py", "1", "2"]):
            self.assertEqual(shard.main(), 1)
        self.assertEqual(go_test.call_args.args[0], [
            "go", "test", "-race", "-count=1", "-timeout=10m", "./internal/cli",
            "-run", "^(Example|FuzzInput|TestB)$",
        ])

    @patch.object(shard, "go_test", return_value=7)
    @patch.object(shard.subprocess, "run")
    def test_coverage_is_explicit_and_propagates_failure(self, run, go_test):
        run.return_value = subprocess.CompletedProcess([], 0, "TestA\nTestB\nExample\nFuzzInput\n", "")
        with patch("sys.argv", ["test-go-shard.py", "1", "3", "--coverprofile", "cli-1.out"]):
            self.assertEqual(shard.main(), 7)
        self.assertEqual(run.call_args.args[0], [
            "go", "test", "-count=1", "-timeout=20m", "-covermode=atomic", "./internal/cli", "-list", ".",
        ])
        self.assertEqual(go_test.call_args.args[0], [
            "go", "test", "-count=1", "-timeout=20m", "-covermode=atomic", "./internal/cli",
            "-run", "^(FuzzInput|TestA)$", "-coverprofile=cli-1.out",
        ])

    @patch.object(shard, "go_test", return_value=5)
    @patch.object(shard.subprocess, "run")
    def test_normal_is_explicit_and_uses_the_same_selection(self, run, go_test):
        run.return_value = subprocess.CompletedProcess([], 0, "TestA\nTestB\nExample\nFuzzInput\n", "")
        with patch("sys.argv", ["test-go-shard.py", "1", "2", "--normal"]):
            self.assertEqual(shard.main(), 5)
        common = ["go", "test", "-count=1", "-timeout=20m", "./internal/cli"]
        self.assertEqual(run.call_args.args[0], common + ["-list", "."])
        self.assertEqual(go_test.call_args.args[0], common + ["-run", "^(Example|FuzzInput|TestB)$"])

    @patch.object(shard.subprocess, "run")
    def test_conflicting_or_empty_modes_fail_before_discovery(self, run):
        for flags in [["--normal", "--coverprofile", "out"], ["--coverprofile", ""]]:
            with patch("sys.argv", ["test-go-shard.py", "0", "4"] + flags), contextlib.redirect_stderr(io.StringIO()):
                with self.assertRaises(SystemExit) as error:
                    shard.main()
                self.assertEqual(error.exception.code, 2)
        run.assert_not_called()

    @unittest.skipUnless(shutil.which("go"), "Go is not installed")
    def test_native_normal_partition_matches_all_modules(self):
        # Standard-library-only source exercises real Go discovery and -run,
        # including subtests, fuzz seeds, examples and a nested CLI package.
        with tempfile.TemporaryDirectory(prefix="crabbox-normal-partition-") as temporary:
            base = pathlib.Path(temporary)
            root = base / "repo with spaces"
            (root / "scripts").mkdir(parents=True)
            runner = root / "scripts/test-go-modules.sh"
            shutil.copyfile(pathlib.Path(__file__).with_name("test-go-modules.sh"), runner)
            (root / "go.mod").write_text("module example.test/fixture\n\ngo 1.26.0\n")
            preamble = '''import ("fmt"; "os"; "testing")
func record(name string) {
    file, err := os.OpenFile(os.Getenv("TEST_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
    if err != nil { panic(err) }
    defer file.Close()
    if _, err := fmt.Fprintln(file, name); err != nil { panic(err) }
}
'''
            names = [f"TestCase{i}" for i in range(16)]
            cli = root / "internal/cli"
            cli.mkdir(parents=True)
            (cli / "cli_test.go").write_text("package cli\n" + preamble + "\n".join(
                f'func {name}(t *testing.T) {{ record(t.Name()) }}' for name in names
            ) + '''
func TestPrefix(t *testing.T) { record(t.Name()); t.Run("child", func(t *testing.T) { record(t.Name()) }) }
func TestPrefixLong(t *testing.T) { record(t.Name()) }
func FuzzInput(f *testing.F) { f.Add("seed"); f.Fuzz(func(t *testing.T, value string) { record(t.Name()) }) }
func Example_demo() { record("Example_demo"); fmt.Println("ok")
    // Output: ok
}
func BenchmarkExcluded(b *testing.B) { record(b.Name()) }
''')
            for directory, name in [(root / "core", "TestCore"), (root / "nested module/internal/cli", "TestNestedCLI")]:
                directory.mkdir(parents=True)
                (directory / "fixture_test.go").write_text("package fixture\n" + preamble +
                    f'func {name}(t *testing.T) {{ record(t.Name()) }}\n')
            (root / "nested module/go.mod").write_text("module example.test/nested\n\ngo 1.26.0\n")
            env = {
                "PATH": os.environ.get("PATH", ""), "HOME": str(base / "home"),
                "GOCACHE": str(base / "cache"), "GOPATH": str(base / "gopath"),
                "GOENV": "off", "GOTOOLCHAIN": "local", "GOPROXY": "off",
                "GOSUMDB": "off", "GOWORK": "off", "GOTELEMETRY": "off",
                "CGO_ENABLED": "0",
            }
            records = []
            for label, flags in [("baseline", []), ("partition", ["--exclude-root-cli"])]:
                log = base / (label + ".log")
                env["TEST_LOG"] = str(log)
                result = subprocess.run(["/bin/bash", str(runner)] + flags, cwd=root, env=env,
                                        text=True, capture_output=True, timeout=120)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                if label == "partition":
                    with contextlib.ExitStack() as cleanup, patch.dict(os.environ, env, clear=True), contextlib.redirect_stdout(io.StringIO()):
                        cleanup.callback(os.chdir, os.getcwd())
                        os.chdir(root)
                        for index in range(4):
                            with patch("sys.argv", ["test-go-shard.py", str(index), "4", "--normal"]):
                                self.assertEqual(shard.main(), 0)
                records.append(collections.Counter(log.read_text().splitlines()))
            expected = collections.Counter(names + ["TestPrefix", "TestPrefix/child", "TestPrefixLong",
                "FuzzInput/seed#0", "Example_demo", "TestCore", "TestNestedCLI"])
            self.assertEqual(records[0], expected)
            self.assertEqual(records[1], expected)

    def stream(self, popen, events, result=0):
        child = popen.return_value.__enter__.return_value
        child.stdout = [json.dumps(event) + "\n" for event in events]
        child.wait.return_value = result
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            code = shard.go_test(["go", "test"])
        self.assertEqual(popen.call_args.args[0], ["go", "test", "-json"])
        return code, output.getvalue().splitlines()

    @patch.object(shard.subprocess, "Popen")
    def test_only_failed_or_unfinished_test_output_is_printed(self, popen):
        def out(test, text):
            return {"Action": "output", "Test": test, "Output": text + "\n"}
        code, lines = self.stream(popen, [
            {"Action": "output", "Output": "build note\n"},
            out("TestPass", "=== RUN   TestPass"), out("TestPass/child", "passing log"),
            {"Action": "pass", "Test": "TestPass/child"}, {"Action": "pass", "Test": "TestPass"},
            out("TestSkip", "skip reason"), {"Action": "skip", "Test": "TestSkip"},
            out("TestFail/child", "child broke"), {"Action": "fail", "Test": "TestFail/child"},
            out("TestFail", "--- FAIL: TestFail"), {"Action": "fail", "Test": "TestFail"},
            out("TestHang", "before hang"), out("TestPass", "panic: test timed out"),
            {"Action": "output", "Output": "FAIL\tpackage\n"},
        ], result=1)
        self.assertEqual(code, 1)
        self.assertEqual(lines[:6], [
            "build note", "child broke", "--- FAIL: TestFail", "FAIL\tpackage",
            "before hang", "panic: test timed out",
        ])

    @patch.object(shard.subprocess, "Popen")
    def test_timings_list_slow_tests_and_do_not_sum_subtests(self, popen):
        events = [{"Action": "pass", "Test": f"Test{i}", "Elapsed": i} for i in range(20)]
        events += [{"Action": "fail", "Test": "TestFailed", "Elapsed": 30},
                   {"Action": "skip", "Test": "TestSkipped", "Elapsed": 40},
                   {"Action": "pass", "Test": "Test19/child", "Elapsed": 999},
                   {"Action": "pass", "Package": "fixture", "Elapsed": 1000}]
        with patch.object(shard, "SLOW_SECONDS", 15), patch.dict(shard.SLOW_TESTS, {"Test16": 1, "Test2": 1}):
            self.assertEqual(self.stream(popen, events)[1][1:], [
                "  30.000 TestFailed (unlisted)", "  19.000 Test19 (unlisted)",
                "  18.000 Test18 (unlisted)", "  17.000 Test17 (unlisted)", "  16.000 Test16",
                "  15.000 Test15 (unlisted)", "  2.000 Test2",
            ])

    @patch.object(shard.subprocess, "Popen")
    def test_bad_json_or_failed_test_cannot_succeed(self, popen):
        child = popen.return_value.__enter__.return_value
        for result, lines, expected in [(9, [], 9), (0, ["not JSON\n"], 1)]:
            child.stdout, child.wait.return_value = lines, result
            with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(shard.go_test(["go", "test"]), expected)

if __name__ == "__main__":
    unittest.main()
