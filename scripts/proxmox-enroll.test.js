import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { test } from 'node:test';

// Execute the actual helper with a mocked tailscale process, not a reconstructed
// command. No VM, tailnet credentials, root privilege or system files are used.
test('Proxmox enrollment helper keeps keys on stdin and fails closed', () => {
  const result = spawnSync('python3', ['-c', String.raw`
import importlib.util, io, json, os, pathlib, subprocess, tempfile
from unittest.mock import patch
spec = importlib.util.spec_from_file_location('enroll', 'scripts/proxmox-enroll.py')
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
key = 'tskey-auth-not-a-real-enrollment-key'
request = {'authKey': key, 'hostname': 'cbx-example', 'tags': ['tag:worker']}
# Reproduce the QGA execution mask; only this isolated Python fixture is affected.
os.umask(0o077)
for problem in ('', 'private-existing', 'not-root', 'dirty-template', 'join-failure', 'not-running', 'lan-ip', 'symlink', 'receipt-symlink', 'missing-id', 'empty-id'):
    with tempfile.TemporaryDirectory() as directory:
        m.METADATA = pathlib.Path(directory) / 'metadata'
        success = problem in ('', 'private-existing')
        if problem == 'symlink':
            m.METADATA.symlink_to(directory, target_is_directory=True)
        if problem in ('private-existing', 'receipt-symlink'):
            m.METADATA.mkdir(mode=0o700)
        if problem == 'private-existing':
            for name in ('tailscale-ipv4', 'tailscale-hostname', 'tailscale-device-id'):
                (m.METADATA / name).write_text('old receipt')
        if problem == 'receipt-symlink':
            external = pathlib.Path(directory) / 'external'
            external.write_text('do not modify')
            (m.METADATA / 'tailscale-ipv4').symlink_to(external)
        calls = []
        def run(argv, **kwargs):
            calls.append((argv, kwargs))
            assert not any(key in str(arg) for arg in argv)
            assert key not in str(kwargs.get('env', {}))
            assert kwargs['stderr'] == subprocess.DEVNULL
            if argv[1] == 'up':
                assert kwargs['input'] == key.encode()
                assert kwargs['stdout'] == subprocess.DEVNULL
                assert '--auth-key=file:/dev/stdin' in argv
                assert '--ssh=false' in argv
                assert '--accept-dns=true' in argv
                if problem == 'join-failure':
                    raise subprocess.CalledProcessError(1, argv, output=key)
                return subprocess.CompletedProcess(argv, 0)
            before = len(calls) == 1
            state = {'BackendState': 'NeedsLogin', 'Self': None}
            if not before or problem == 'dirty-template':
                state = {'BackendState': 'Running', 'Self': {'ID': 'nFixtureDevice', 'TailscaleIPs': ['100.100.1.2']}}
            if not before and problem == 'not-running':
                state['BackendState'] = 'NeedsLogin'
            if not before and problem == 'lan-ip':
                state['Self']['TailscaleIPs'] = ['10.77.1.32']
            if not before and problem == 'missing-id':
                del state['Self']['ID']
            if not before and problem == 'empty-id':
                state['Self']['ID'] = ''
            return subprocess.CompletedProcess(argv, 0, stdout=json.dumps(state).encode())
        # Test fixtures run without root. Production requires root ownership;
        # substitute only the fixture's stat owner, retaining real file I/O.
        real_fstat = os.fstat
        def root_fstat(descriptor):
            values = list(real_fstat(descriptor))
            values[4] = 0
            return os.stat_result(values)
        with patch.object(m.os, 'fstat', root_fstat), patch.object(m.os, 'geteuid', return_value=1000 if problem == 'not-root' else 0), patch.object(m.subprocess, 'run', side_effect=run):
            failed = False
            try:
                m.enroll(io.StringIO(json.dumps(request)))
            except Exception:
                if success:
                    raise
                failed = True
            assert failed != success, problem
        for path in pathlib.Path(directory).rglob('*'):
            if path.is_file() and not path.is_symlink():
                assert key not in path.read_text()
        if success:
            assert (m.METADATA / 'tailscale-ipv4').read_text() == '100.100.1.2\n'
            assert (m.METADATA / 'tailscale-device-id').read_text() == 'nFixtureDevice\n'
            assert m.METADATA.stat().st_mode & 0o777 == 0o755
            for name in ('tailscale-ipv4', 'tailscale-hostname', 'tailscale-device-id'):
                assert (m.METADATA / name).stat().st_mode & 0o777 == 0o644
        if problem == 'receipt-symlink':
            assert external.read_text() == 'do not modify'
            assert external.stat().st_mode & 0o777 == 0o600
        if problem == 'symlink':
            assert pathlib.Path(directory).stat().st_mode & 0o777 == 0o700
        if problem == 'dirty-template':
            assert len(calls) == 1
for request in ({'authKey': key, 'hostname': '--bad', 'tags': ['tag:worker']}, {'authKey': key, 'hostname': 'safe', 'tags': []}, {'authKey': 'invalid', 'hostname': 'safe', 'tags': ['tag:worker']}):
    with patch.object(m.os, 'geteuid', return_value=0), patch.object(m.subprocess, 'run') as run:
        try:
            m.enroll(io.StringIO(json.dumps(request)))
            raise AssertionError('invalid input admitted')
        except ValueError:
            pass
        run.assert_not_called()
with patch.object(m, 'enroll', side_effect=RuntimeError(key)), patch.object(m.sys, 'stdout', io.StringIO()) as out, patch.object(m.sys, 'stderr', io.StringIO()) as err:
    assert m.main() == 1
    assert out.getvalue() == err.getvalue() == ''
`], { encoding: 'utf8' });
  assert.equal(result.status, 0, result.stderr || result.stdout);
});
