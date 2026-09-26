#!/usr/bin/env python3
"""Scenario-label regression tests; no Go build, lease or network required."""

import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(os.environ.get("CRABBOX_BENCH_SCRIPT", Path(__file__).with_name("benchmark-sync.py"))).resolve()


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
