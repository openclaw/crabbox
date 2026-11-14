#!/usr/bin/env python3
"""Filesystem-only helper tests; no AWS, mount, root or allocation."""
import importlib.util
import hashlib
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock

script = Path(__file__).with_name('qualification-first-walk-guest.py')
spec = importlib.util.spec_from_file_location('first_walk', script)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class FirstWalkHelperTests(unittest.TestCase):
    def test_once_receipt_survives_retry_and_rejects_substitution(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp) / 'receipts'
            module.consume_once(directory, 'vol-1234', 'a' * 64)
            with self.assertRaises(FileExistsError):
                module.consume_once(directory, 'vol-1234', 'a' * 64)
            alias = Path(tmp) / 'alias'
            alias.symlink_to(directory)
            with self.assertRaises(OSError):
                module.consume_once(alias, 'vol-5678', 'a' * 64)

    @unittest.skipUnless(Path('/usr/bin/python3').exists(), 'requires POSIX Python')
    def test_real_child_exit_and_failure_settlement(self):
        with tempfile.TemporaryDirectory() as tmp:
            result = module.observe("print('state=complete')", 'unused', [], Path(tmp))
            self.assertEqual(result['exit'], 0)
            self.assertIn('state=complete', result['summary'])
            # Force an observation failure after spawn, then verify the real
            # child was killed and reaped; no filesystem mutation is involved.
            children = []
            original = module.subprocess.Popen
            def capture(*args, **kwargs):
                child = original(*args, **kwargs)
                children.append(child)
                return child
            with mock.patch.object(module.subprocess, 'Popen', capture), mock.patch.object(module.time, 'sleep', side_effect=RuntimeError('cancel')):
                with self.assertRaisesRegex(RuntimeError, 'cancel'):
                    module.observe('import time; time.sleep(30)', 'unused', [], Path(tmp))
            self.assertIsNotNone(children[0].returncode)
            with self.assertRaises(ChildProcessError):
                os.waitpid(children[0].pid, os.WNOHANG)

    def test_device_requires_exact_unique_volume_serial_not_uuid_or_label(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            def add(name, serial):
                p = root / name / 'device'
                p.mkdir(parents=True)
                (p / 'serial').write_text(serial)
            add('nvme0n1', 'vol11111111')
            add('nvme1n1', 'vol22222222')
            self.assertEqual(module.exact_device('vol-22222222', root), 'nvme1n1')
            add('nvme2n1', 'vol22222222')
            with self.assertRaisesRegex(RuntimeError, 'ambiguous'):
                module.exact_device('vol-22222222', root)
            with self.assertRaises(ValueError):
                module.exact_device('../original', root)

    def test_exact_recipe_body_and_digest(self):
        recipe = script.parent / 'fixtures/first-walk-reviewed-recipe.sh'
        digest = hashlib.sha256(recipe.read_bytes()).hexdigest()
        self.assertIn('follow_symlinks=False', module.recipe_body(recipe, digest))
        with self.assertRaisesRegex(RuntimeError, 'digest'):
            module.recipe_body(recipe, '0' * 64)

    def test_saved_root_rejects_escape_and_symlink_ancestors(self):
        with tempfile.TemporaryDirectory() as tmp:
            mount = Path(tmp)
            (mount / 'work').mkdir()
            (mount / 'alias').symlink_to(mount / 'work')
            self.assertEqual(module.safe_root(mount, '/work/tree'), str(mount / 'work/tree'))
            for root in ['/', '../work', '/work/../escape']:
                with self.assertRaises(ValueError):
                    module.safe_root(mount, root)
            with self.assertRaises(OSError):
                module.safe_root(mount, '/alias/tree')

    def test_one_post_walk_pass_counts_layout_and_does_not_follow_links(self):
        with tempfile.TemporaryDirectory() as tmp:
            p = Path(tmp)
            (p / 'sub').mkdir()
            (p / 'sub/file').write_text('fixture')
            (p / 'cycle').symlink_to(p)
            (p / 'dangling').symlink_to('absent')
            result = module.verify_layout([str(p)], os.getuid(), os.getgid())
            self.assertEqual(result, {'visited': 5, 'mismatched': 0, 'files': 1,
                'directories': 2, 'symlinks': 2, 'maxDepth': 2, 'maxFanout': 3})
            with self.assertRaisesRegex(RuntimeError, 'ownership'):
                module.verify_layout([str(p)], os.getuid() + 1, os.getgid())


if __name__ == '__main__':
    unittest.main()
