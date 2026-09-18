#!/usr/bin/env python3
"""Real filesystem and offline npm tests; no cloud credentials or live capacity."""
import base64
import concurrent.futures
import hashlib
import importlib.util
import io
import json
import os
import pathlib
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

HERE = pathlib.Path(os.environ.get("CRABBOX_PROJECT_STATE_TEST_MODULE_DIR", pathlib.Path(__file__).resolve().parent))
sys.path.insert(0, str(HERE))
import project_dependencies as dependencies

spec = importlib.util.spec_from_file_location("project_state", HERE / "project-state.py")
state = importlib.util.module_from_spec(spec)
spec.loader.exec_module(state)


class ProjectStateTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="crabbox-project-test-")
        self.addCleanup(self.temporary.cleanup)
        self.directory = pathlib.Path(self.temporary.name)
        self.root = self.directory / "project"
        self.root.mkdir()

    def test_uncommitted_ignored_files_and_confined_parent_symlink_round_trip(self):
        (self.root / "src").mkdir()
        (self.root / "src" / "bin").mkdir()
        (self.root / "src" / "tool.js").write_text("uncommitted source\n")
        (self.root / "src" / "bin" / "tool").symlink_to("../tool.js")
        (self.root / ".gitignore").write_text(".env.local\n")
        (self.root / ".env.local").write_text("synthetic user work\n")
        (self.root / ".git").mkdir()
        (self.root / ".git" / "index").write_text("not recovered from the old worker")
        captured = state.capture(self.root)
        document = json.loads(base64.b64decode(captured["content"]))
        self.assertFalse(any(e["path"].startswith(".git/") for e in document["entries"]))
        destination = self.directory / "replacement"
        destination.mkdir()
        result = state.restore(destination, captured["content"], captured["sha256"])
        self.assertEqual(result["recovery"], "filesystem-only")
        self.assertEqual((destination / "src/bin/tool").read_text(), "uncommitted source\n")
        self.assertEqual((destination / ".env.local").read_text(), "synthetic user work\n")

    def test_escaping_links_profiles_and_unsupported_files_fail(self):
        (self.root / "escape").symlink_to("../outside")
        with self.assertRaisesRegex(ValueError, "scope"):
            state.capture(self.root)
        (self.root / "escape").unlink()
        (self.root / "browser-profile").mkdir()
        with self.assertRaises(ValueError):
            state.capture(self.root)
        (self.root / "browser-profile").rmdir()
        os.mkfifo(self.root / "pipe")
        with self.assertRaises(ValueError):
            state.capture(self.root)

    def test_corrupt_content_never_replaces_destination(self):
        (self.root / "work").write_text("preserve me")
        original = (self.root / "work").read_bytes()
        with self.assertRaises(ValueError):
            state.restore(self.root, base64.b64encode(b"{}").decode(), "a" * 64)
        self.assertEqual((self.root / "work").read_bytes(), original)

    def test_chained_link_escape_and_cycle_hold_capture_and_restore(self):
        (self.root / "dir").mkdir()
        (self.root / "dir/up").symlink_to("..")
        (self.root / "escape").symlink_to("dir/up/../outside")
        with self.assertRaisesRegex(dependencies.DependencyError, "symlink_escape"):
            state.capture(self.root)
        entries = [{"path": "dir", "type": "directory", "mode": 0o755},
                   {"path": "dir/up", "type": "symlink", "mode": 0o777, "target": ".."},
                   {"path": "escape", "type": "symlink", "mode": 0o777, "target": "dir/up/../outside"}]
        for target, code in [("dir/up/../outside", "symlink_escape"), ("escape", "symlink_cycle")]:
            entries[-1]["target"] = target
            payload = state.encode({"schema": state.SCHEMA, "entries": entries, "bytes": 0,
                                    "gitMetadata": "excluded", "processState": "not-captured", "exclusions": []})
            destination = self.directory / "replacement"
            destination.mkdir(exist_ok=True)
            (destination / "keep").write_text("keep user data")
            with self.assertRaisesRegex(dependencies.DependencyError, code):
                state.restore(destination, base64.b64encode(payload).decode(), state.digest(payload))
            self.assertEqual((destination / "keep").read_text(), "keep user data")
        (self.root / "escape").unlink()
        (self.root / "a").symlink_to("b")
        (self.root / "b").symlink_to("a")
        with self.assertRaisesRegex(dependencies.DependencyError, "symlink_cycle"):
            state.capture(self.root)

    def test_safe_link_graph_expands_parent_segments_and_allows_repeated_links(self):
        (self.root / "dir").mkdir()
        (self.root / "dir/up").symlink_to("..")
        (self.root / "file").write_text("safe target")
        (self.root / "safe").symlink_to("dir/up/dir/up/file")
        captured = state.capture(self.root)
        destination = self.directory / "replacement"
        destination.mkdir()
        state.restore(destination, captured["content"], captured["sha256"])
        self.assertEqual((destination / "safe").read_text(), "safe target")

    @unittest.skipUnless(sys.platform == "linux", "atomic nonempty directory exchange is a Linux runner boundary")
    def test_linux_atomic_restore_preserves_replacement_git_metadata(self):
        (self.root / "work").write_text("checkpoint")
        saved = state.capture(self.root)
        (self.root / "work").write_text("replacement")
        (self.root / ".git").write_text("gitdir: /native/replacement/worktree\n")
        state.restore(self.root, saved["content"], saved["sha256"])
        self.assertEqual((self.root / "work").read_text(), "checkpoint")
        self.assertEqual((self.root / ".git").read_text(), "gitdir: /native/replacement/worktree\n")

    def test_pool_home_workspace_and_browser_contamination_denied(self):
        state_root, home = self.directory / "state", self.directory / "home"
        state_root.mkdir()
        home.mkdir()
        (home / ".bashrc").write_text("baseline")
        state.pool_baseline(state_root, home)
        self.assertEqual(state.pool_clean(state_root, self.root, home)["state"], "clean")
        (self.root / "sentinel").write_text("earlier project")
        with self.assertRaises(ValueError):
            state.pool_clean(state_root, self.root, home)
        (self.root / "sentinel").unlink()
        (home / ".bashrc").write_text("dirty shell hook")
        state.pool_baseline(state_root, home)
        with self.assertRaises(ValueError):
            state.pool_clean(state_root, self.root, home)
        (home / ".bashrc").write_text("baseline")
        (home / ".config/chromium").mkdir(parents=True)
        with self.assertRaises(ValueError):
            state.pool_clean(state_root, self.root, home)

    def test_clean_claim_is_single_use_and_same_token_replays(self):
        state_root, home = self.directory / "state", self.directory / "home"
        state_root.mkdir()
        home.mkdir()
        state.pool_baseline(state_root, home)

        def claim(token):
            try:
                state.pool_clean(state_root, self.root, home, token)
                return token
            except (ValueError, OSError):
                return None
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as executor:
            results = list(executor.map(claim, ["a" * 64, "b" * 64]))
        winner = next(value for value in results if value)
        self.assertEqual(sum(value is not None for value in results), 1)
        self.assertEqual(state.pool_clean(state_root, self.root, home, winner)["state"], "claimed")
        with self.assertRaises(ValueError):
            state.pool_clean(state_root, self.root, home)

    def test_removed_path_lookup_scales_with_path_depth(self):
        class CountingSet(set):
            def __init__(self, values):
                super().__init__(values)
                self.lookups = 0

            def __contains__(self, value):
                self.lookups += 1
                return super().__contains__(value)

        removed = CountingSet(f"node_modules/package-{index}" for index in range(250000))
        self.assertTrue(
            dependencies.path_or_ancestor_in(
                "node_modules/package-249999/lib/index.js",
                removed,
            )
        )
        self.assertLessEqual(removed.lookups, 3)


@unittest.skipUnless(shutil.which("npm"), "npm must be installed for the real dependency reconstruction probe")
class DependencyTests(unittest.TestCase):
    def setUp(self):
        ProjectStateTests.setUp(self)
        self.cache = self.directory / "npm-cache"
        original = dependencies.npm_environment
        npm_bin = pathlib.Path(shutil.which("npm")).parent
        node_bin = pathlib.Path(shutil.which("node")).parent
        self.environment = lambda cache=None: {**original(cache), "PATH": str(node_bin) + ":" + str(npm_bin) + ":/usr/local/bin:/usr/bin:/bin"}
        self.patch = patch.object(dependencies, "npm_environment", self.environment)
        self.patch.start()
        self.addCleanup(self.patch.stop)
        package = self.directory / "example.tgz"
        with tarfile.open(package, "w:gz") as archive:
            contents = {"package.json": json.dumps({"name": "example", "version": "1.0.0", "bin": {"example": "cli.js"}}).encode(),
                        "cli.js": b"#!/usr/bin/env node\nconsole.log('fixture');\n", "payload.bin": hashlib.shake_256(b"large dependency fixture").digest(33 * 1024 * 1024), "removable.txt": b"generated"}
            for name, data in contents.items():
                info = tarfile.TarInfo("package/" + name)
                info.size, info.mode = len(data), 0o755 if name == "cli.js" else 0o644
                archive.addfile(info, io.BytesIO(data))
        integrity = "sha512-" + base64.b64encode(hashlib.sha512(package.read_bytes()).digest()).decode()
        (self.root / "package.json").write_text(json.dumps({"name": "fixture", "version": "1.0.0", "dependencies": {"example": "1.0.0"}}))
        (self.root / "package-lock.json").write_text(json.dumps({"name": "fixture", "version": "1.0.0", "lockfileVersion": 3,
            "packages": {"": {"name": "fixture", "version": "1.0.0", "dependencies": {"example": "1.0.0"}},
                         "node_modules/example": {"version": "1.0.0", "resolved": "https://registry.npmjs.org/example/-/example-1.0.0.tgz", "integrity": integrity, "bin": {"example": "cli.js"}}}}))
        env = self.environment(self.cache)
        cached = subprocess.run(["npm", "cache", "add", str(package), "--ignore-scripts"], cwd=self.directory, env=env, capture_output=True, text=True)
        self.assertEqual(cached.returncode, 0, cached.stderr)
        dependencies.rebuild(self.root, dependencies.read_inputs(self.root), offline=True, cache=self.cache)

    def test_real_npm_large_dependencies_user_edits_and_internal_bin_link_recover(self):
        (self.root / "app.js").write_text("uncommitted app work")
        (self.root / "node_modules/example/cli.js").write_text("user patched dependency")
        (self.root / "node_modules/example/notes.txt").write_text("untracked user notes")
        (self.root / "node_modules/example/removable.txt").unlink()
        captured = state.capture(self.root, "npm-lockfile-v1", self.cache)
        document = json.loads(base64.b64decode(captured["content"]))
        self.assertLess(captured["bytes"], 10000)
        recipe = document["exclusions"][0]
        self.assertGreater(recipe["bytes"], 32 * 1024 * 1024)
        self.assertIn("node_modules/example/payload.bin", recipe["omittedPaths"])
        destination = self.directory / "replacement"
        destination.mkdir()
        state.restore(destination, captured["content"], captured["sha256"], self.cache)
        self.assertEqual((destination / "app.js").read_text(), "uncommitted app work")
        self.assertEqual((destination / "node_modules/.bin/example").read_text(), "user patched dependency")
        self.assertEqual((destination / "node_modules/example/notes.txt").read_text(), "untracked user notes")
        self.assertEqual((destination / "node_modules/example/payload.bin").stat().st_size, 33 * 1024 * 1024)
        self.assertFalse((destination / "node_modules/example/removable.txt").exists())
        # The dependency inventory alone cannot see the external-to-node_modules
        # link. Capture must validate the complete saved + reproducible graph.
        (self.root / "dir").mkdir()
        (self.root / "dir/up").symlink_to("..")
        (self.root / "node_modules/.bin/escape").symlink_to("../../dir/up/../outside")
        with self.assertRaisesRegex(dependencies.DependencyError, "symlink_escape"):
            state.capture(self.root, "npm-lockfile-v1", self.cache)

    def test_tracked_or_changed_large_dependency_is_not_silently_excluded(self):
        subprocess.run(["git", "init", "--quiet", str(self.root)], check=True)
        subprocess.run(["git", "-C", str(self.root), "add", "-f", "node_modules/example/payload.bin"], check=True)
        with self.assertRaisesRegex(dependencies.DependencyError, "unpreserved_content_limit"):
            state.capture(self.root, "npm-lockfile-v1", self.cache)
        shutil.rmtree(self.root / ".git")
        with (self.root / "node_modules/example/payload.bin").open("r+b") as output:
            output.write(b"user edit")
        with self.assertRaisesRegex(dependencies.DependencyError, "unpreserved_content_limit"):
            state.capture(self.root, "npm-lockfile-v1", self.cache)

    def test_missing_rebuild_cache_and_unsupported_recipe_hold(self):
        shutil.rmtree(self.cache)
        with self.assertRaisesRegex(dependencies.DependencyError, "rebuild_unavailable"):
            state.capture(self.root, "npm-lockfile-v1", self.cache)
        lock = json.loads((self.root / "package-lock.json").read_text())
        lock["packages"]["node_modules/example"]["resolved"] = "https://private.invalid/with-credentials"
        (self.root / "package-lock.json").write_text(json.dumps(lock))
        with self.assertRaisesRegex(dependencies.DependencyError, "recipe_unsupported"):
            state.capture(self.root, "npm-lockfile-v1", self.cache)


if __name__ == "__main__":
    unittest.main()
