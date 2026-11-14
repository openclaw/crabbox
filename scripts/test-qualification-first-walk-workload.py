"""Tests for fixed public preparation; no cloud calls, downloads or ownership runs."""
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tarfile
import tempfile
import time
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('workload', Path(__file__).with_name('qualification-first-walk-workload.py'))
w = importlib.util.module_from_spec(spec)
spec.loader.exec_module(w)


class WorkloadTests(unittest.TestCase):
    def archive(self, path, rows):
        with tarfile.open(path, 'w') as t:
            for name, kind, payload in rows:
                m = tarfile.TarInfo(name)
                m.type = kind
                if kind == tarfile.REGTYPE:
                    data = payload.encode(); m.size = len(data)
                    t.addfile(m, io.BytesIO(data))
                else:
                    m.linkname = payload; t.addfile(m)

    def test_fixed_inputs_and_no_repository_execution(self):
        self.assertEqual(len(w.INPUTS), 3)
        for name, url, size, digest in w.INPUTS:
            self.assertTrue(url.startswith('https://'))
            self.assertGreater(size, 0); self.assertEqual(len(digest), 64)
        command = w.install_command(Path('/tools/pnpm'), Path('/cache/store'))
        for flag in ['--ignore-scripts', '--ignore-pnpmfile', '--frozen-lockfile', '--pm-on-fail=ignore']:
            self.assertIn(flag, command)
        self.assertNotIn('run', command)

    def test_archive_is_data_and_strips_only_one_wrapper(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d); target = p/'target'; target.mkdir()
            self.archive(p/'a.tar', [('wrapper/a', tarfile.REGTYPE, 'inert'), ('wrapper/link', tarfile.SYMTYPE, 'a')])
            w.extract(p/'a.tar', target)
            self.assertEqual((target/'a').read_text(), 'inert')
            self.assertEqual(os.readlink(target/'link'), 'a')

    def test_archive_escape_links_specials_duplicates_and_limits_fail(self):
        cases = [
            [('wrapper/../../escape', tarfile.REGTYPE, 'x')],
            [('/wrapper/absolute', tarfile.REGTYPE, 'x')],
            [('wrapper/a', tarfile.SYMTYPE, '../../escape'), ('wrapper/a/payload', tarfile.REGTYPE, 'x')],
            [('wrapper/a', tarfile.LNKTYPE, 'wrapper/b')],
            [('wrapper/a', tarfile.FIFOTYPE, '')],
            [('wrapper/.git/config', tarfile.REGTYPE, 'x')],
            [('wrapper/a', tarfile.REGTYPE, 'x'), ('other/b', tarfile.REGTYPE, 'x')],
            [('wrapper/a', tarfile.REGTYPE, 'x'), ('wrapper/a', tarfile.REGTYPE, 'x')],
        ]
        for rows in cases:
            with self.subTest(rows=rows), tempfile.TemporaryDirectory() as d:
                p=Path(d); target=p/'target'; target.mkdir(); self.archive(p/'a.tar', rows)
                with self.assertRaises((RuntimeError, tarfile.FilterError)):
                    w.extract(p/'a.tar', target)
                self.assertFalse((p/'escape').exists())
        with tempfile.TemporaryDirectory() as d:
            p=Path(d); target=p/'target'; target.mkdir()
            self.archive(p/'a.tar', [('w/a', tarfile.REGTYPE, 'xx')])
            with self.assertRaisesRegex(RuntimeError, 'limit'): w.extract(p/'a.tar', target, max_bytes=1)
            with self.assertRaisesRegex(RuntimeError, 'limit'): w.extract(p/'a.tar', target, max_entries=0)

    def test_inventory_never_follows_symlinks_and_enforces_limits(self):
        with tempfile.TemporaryDirectory() as d:
            p=Path(d); (p/'f').write_text('x'); (p/'loop').symlink_to(p)
            self.assertEqual(w.inventory([p])['entries'], 3)
            with self.assertRaisesRegex(RuntimeError, 'limit'): w.inventory([p], max_entries=2)
            with self.assertRaisesRegex(RuntimeError, 'limit'): w.inventory([p], max_bytes=0)

    def test_download_requires_exact_size_digest_and_deadline(self):
        b=b'inert'; good=('test', 'https://example.invalid/test', len(b), hashlib.sha256(b).hexdigest())
        for spec, data, deadline, accepted in [
            (good, b, time.monotonic()+10, True), (good, b+'x'.encode(), time.monotonic()+10, False),
            (good, b[:-1], time.monotonic()+10, False),
            ((good[0],good[1],len(b),'0'*64), b, time.monotonic()+10, False),
            (good, b, time.monotonic()-1, False),
        ]:
            with self.subTest(spec=spec,data=data), tempfile.TemporaryDirectory() as d, patch.object(w.urllib.request,'urlopen',return_value=io.BytesIO(data)):
                if accepted:
                    w.download(spec,Path(d)/'data',deadline)
                    self.assertEqual((Path(d)/'data').read_bytes(),b)
                else:
                    with self.assertRaises(RuntimeError):w.download(spec,Path(d)/'data',deadline)

    def test_install_process_failure_and_deadline_are_settled(self):
        with tempfile.TemporaryDirectory() as d:
            p=Path(d)
            with self.assertRaisesRegex(RuntimeError, 'install failed'):
                w.monitored_install([sys.executable,'-c','raise SystemExit(7)'],p,dict(os.environ),[p],p/'failure.log',time.monotonic()+5)
            with self.assertRaisesRegex(RuntimeError, 'deadline'):
                w.monitored_install([sys.executable,'-c','import time; time.sleep(30)'],p,dict(os.environ),[p],p/'timeout.log',time.monotonic()+0.1)

    @unittest.skipUnless(Path('/inputs/pnpm-native.tgz').is_file(), 'requires pinned disposable Linux inputs')
    def test_actual_pnpm_disables_hooks_and_scripts_with_positive_control(self):
        with tempfile.TemporaryDirectory() as d:
            p=Path(d); node=p/'node'; node.mkdir(); pnpm=p/'pnpm'; pnpm.mkdir()
            w.extract(Path('/inputs/node.tar.xz'),node)
            w.extract(Path('/inputs/pnpm-native.tgz'),pnpm)
            project=p/'project';project.mkdir();home=p/'home';home.mkdir()
            script = """node -e "require('fs').writeFileSync('script-fired','yes')" """
            (project/'package.json').write_text(json.dumps({'name':'inert-qualification-fixture','private':True,'packageManager':'pnpm@0.0.0','scripts':{'preinstall': script}}))
            (project/'.pnpmfile.cjs').write_text("require('fs').writeFileSync('hook-fired','yes'); module.exports={hooks:{}};\n")
            env={'PATH':str(node/'bin')+':/usr/bin:/bin','HOME':str(home),'CI':'true'}
            args=w.install_command(pnpm/'pnpm',p/'store')
            for command in [[a for a in args if a!='--frozen-lockfile'],args]:
                result=subprocess.run(command,cwd=project,env=env,capture_output=True,text=True,timeout=20)
                self.assertEqual(result.returncode,0,result.stderr)
                self.assertFalse((project/'script-fired').exists())
                self.assertFalse((project/'hook-fired').exists())
            # A mismatched repository pin must not replace the verified executable.
            # The strict control proves pnpm reads that same mismatch; no download
            # or alternate package manager is needed for either observation.
            strict=[a.replace('--pm-on-fail=ignore','--pm-on-fail=error') for a in args]
            result=subprocess.run(strict,cwd=project,env=env,capture_output=True,text=True,timeout=20)
            self.assertNotEqual(result.returncode,0)
            self.assertIn('0.0.0',result.stdout+result.stderr)
            self.assertFalse((project/'script-fired').exists())
            self.assertFalse((project/'hook-fired').exists())
            # The controls prove these same inert markers can actually execute.
            command=[a.replace('--ignore-scripts','--ignore-scripts=false').replace('--ignore-pnpmfile','--ignore-pnpmfile=false').replace('--frozen-lockfile','--no-frozen-lockfile') for a in args]
            result=subprocess.run(command,cwd=project,env=env,capture_output=True,text=True,timeout=20)
            self.assertEqual(result.returncode,0,result.stderr)
            self.assertTrue((project/'script-fired').is_file())
            self.assertTrue((project/'hook-fired').is_file())

    def test_preparation_rejects_different_numeric_owner_before_download(self):
        from types import SimpleNamespace
        with patch.object(w.pwd,'getpwnam',return_value=SimpleNamespace(pw_uid=os.getuid()+1,pw_gid=os.getgid())),patch.object(w,'download') as network:
            with self.assertRaisesRegex(RuntimeError, 'ordinary owner'):w.prepare(Path('/does-not-exist'))
            network.assert_not_called()


if __name__ == '__main__':
    unittest.main()
