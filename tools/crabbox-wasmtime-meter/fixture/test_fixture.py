import copy
import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from types import SimpleNamespace
from unittest import mock

import qualify
import supervise


class ConsumerContract(unittest.TestCase):
    def test_deadline_reports_progress_and_reaps_without_replacing_failure(self):
        stdout = b'child output'
        for exit_code, guest_started, broken_diagnostics in [(None, True, False), (17, False, False), (None, True, True)]:
            stderr = (b'meter: guest-started\n' if guest_started else b'') + b'x' * 4096 + b'\xff tail\n'
            child = mock.Mock(pid=12345)
            child.stdout.fileno.return_value = 100
            child.stderr.fileno.return_value = 101
            child.poll.return_value = exit_code
            selector = mock.Mock()
            keys = {}
            selector.register.side_effect = lambda stream, _events, data: keys.update(
                {data: SimpleNamespace(fileobj=stream, data=data)})
            selector.get_map.return_value = keys
            selector.select.side_effect = lambda **_kwargs: [(key, 0) for key in keys.values()]
            captured = io.StringIO()
            sink = mock.Mock() if broken_diagnostics else captured
            if broken_diagnostics:
                sink.write.side_effect = OSError('diagnostic sink unavailable')
            with self.subTest(exit_code=exit_code, guest_started=guest_started, broken=broken_diagnostics), \
                    tempfile.TemporaryDirectory() as name:
                directory = Path(name) / 'trial'
                directory.mkdir()
                with mock.patch.object(supervise.os, 'chown'), \
                        mock.patch.object(supervise.os, 'open', return_value=42), \
                        mock.patch.object(supervise.os, 'close'), \
                        mock.patch.object(supervise.os, 'read', side_effect=lambda fd, _size: {100: stdout, 101: stderr}[fd]), \
                        mock.patch.object(supervise.os, 'killpg') as kill, \
                        mock.patch.object(supervise.subprocess, 'Popen', return_value=child), \
                        mock.patch.object(supervise.selectors, 'DefaultSelector', return_value=selector), \
                        mock.patch.object(supervise, 'time', SimpleNamespace(monotonic=mock.Mock(side_effect=[0, 0, 120]))), \
                        mock.patch.object(supervise, 'meter_processes', return_value=[]) as processes, \
                        contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(sink):
                    with self.assertRaisesRegex(RuntimeError, '^fixture child deadline exceeded$'):
                        supervise.invoke(directory, Path(name), Path(name) / 'unused.json', 1000,
                                         'probe-identity', '0' * 64, '1')
                kill.assert_called_once_with(child.pid, supervise.signal.SIGKILL)
                child.wait.assert_called_once_with(timeout=5)
                processes.assert_called_once_with()
                selector.close.assert_called_once_with()
                self.assertFalse((directory / 'out' / 'evidence.json').exists())
                if not broken_diagnostics:
                    diagnostic = json.loads(captured.getvalue().removeprefix('fixture failure: '))
                    self.assertEqual(diagnostic['phase'], 'child-supervision')
                    self.assertTrue(diagnostic['spawned'])
                    self.assertEqual(diagnostic['guestStarted'], guest_started)
                    self.assertEqual(diagnostic['childState'], 'live' if exit_code is None else 'exited')
                    self.assertEqual(diagnostic['exitCode'], exit_code)
                    self.assertEqual((diagnostic['stdoutBytes'], diagnostic['stderrBytes']), (len(stdout), len(stderr)))
                    self.assertLessEqual(len(diagnostic['stderrTail'].encode()), 2048)
                    self.assertTrue(diagnostic['stderrTail'].endswith('\ufffd tail\n'))

    def evidence(self):
        meter = dict(mechanism='wasmtime-fuel', engine='wasmtime', engineVersion='50.0.1',
                     engineRevision='c307ec81de02852d815e9849f7c89d955c03c30d',
                     configuration='crabbox-wasmtime-fuel/v1', wasiConfiguration='crabbox-deterministic-wasi/v1',
                     deterministic=True, initialFuel=qualify.INITIAL, remainingFuel=qualify.INITIAL - 12)
        projection = dict(argv=qualify.ARGV, configuration=meter['configuration'], engine='wasmtime',
                          engineRevision=meter['engineRevision'], engineVersion=meter['engineVersion'],
                          metric='fuel', target='wasip1/wasm', toolchain='go1.27.3', unit='fuel',
                          wasiConfiguration=meter['wasiConfiguration'], workload=qualify.WORKLOAD)
        meter['comparisonKey'] = 'sha256:' + qualify.sha(json.dumps(projection, sort_keys=True, separators=(',', ':')).encode())
        return dict(schemaVersion=1, generatedAt='2026-01-01T00:00:00Z', provider='local-container',
                    workload=dict(name=qualify.WORKLOAD, moduleSha256='a' * 64, toolchain='go1.27.3', target='wasip1/wasm', argv=qualify.ARGV),
                    meter=meter, results=[dict(metric='fuel', unit='fuel', measured=12, budget=12, exceeded=False)],
                    exceeded=False, guestOutput=dict(stdoutSha256=qualify.sha(b'PASS\n'), stderrSha256=qualify.sha(b'')))

    def check(self, value):
        return qualify.validate(json.dumps(value).encode(), 'a' * 64, 12)

    def test_real_semantic_gate_rejects_inconsistent_evidence(self):
        mutations = [
            lambda v: v['results'][0].update(measured=13),
            lambda v: v['results'][0].update(measured=True),
            lambda v: v.update(exceeded=True),
            lambda v: v['meter'].update(deterministic=False),
            lambda v: v['meter'].update(comparisonKey='sha256:' + '0' * 64),
            lambda v: v['meter'].update(engineRevision='0' * 40),
            lambda v: v['workload'].update(moduleSha256='0' * 64),
            lambda v: v.update(generatedAt='not a timestamp'),
            lambda v: v['guestOutput'].update(stdoutSha256=qualify.sha(b'no tests ran')),
        ]
        self.check(self.evidence())
        for mutate in mutations:
            value = self.evidence()
            mutate(value)
            with self.subTest(mutate=mutate), self.assertRaises((AssertionError, ValueError)):
                self.check(value)

    def test_protocol_rejects_trailing_duplicate_and_nonfinite_json(self):
        for data in (b'{} {}', b'{"x":1,"x":2}', b'{"x":NaN}', b'not-json', b' ' * 65537):
            with self.subTest(data=data[:40]), self.assertRaises(ValueError):
                qualify.strict_json(data)

    def test_v1_additive_fields_are_ignored(self):
        value = self.evidence()
        value['future'] = 'optional'
        value['workload']['future'] = 'optional'
        value['meter']['future'] = 'optional'
        value['results'][0]['future'] = 'optional'
        self.check(value)

    def test_mount_gate_refuses_extra_writable_and_unexpected_contents(self):
        with tempfile.TemporaryDirectory() as name:
            directory = Path(name)
            (directory / 'bootstrap.sh').write_text('public bootstrap script\n')
            value = dict(HostConfig=dict(Privileged=False, NetworkMode='bridge', Memory=4 * 1024**3),
                         Mounts=[dict(Type='bind', Destination='/tmp/crabbox-bootstrap', RW=False, Source=name)],
                         Config=dict(Env=['CRABBOX_AUTHORIZED_KEY=public-fixture-key']))
            qualify.audit_mounts(value)
            for mutate in (lambda v: v['Mounts'].append(dict(v['Mounts'][0])),
                           lambda v: v['Mounts'][0].update(RW=True),
                           lambda v: v['HostConfig'].update(Privileged=True),
                           lambda v: v['HostConfig'].update(CapAdd=['SYS_ADMIN'])):
                changed = copy.deepcopy(value)
                mutate(changed)
                with self.assertRaises(AssertionError):
                    qualify.audit_mounts(changed)
            (directory / 'unexpected').write_text('not permitted')
            with self.assertRaises(AssertionError):
                qualify.audit_mounts(value)

    def test_cross_host_gate_requires_counts_and_bound_build_identity(self):
        with tempfile.TemporaryDirectory() as name:
            paths, builds, reports = [], [], []
            for architecture, host in [('amd64', 'x86_64'), ('arm64', 'aarch64')]:
                directory = Path(name) / architecture
                directory.mkdir()
                paths.append(directory / 'qualification.json')
                reports.append(dict(architecture=architecture, result='passed',
                                    cleanup=dict(stopExit=0, containerAbsent=True), counts=[12] * 20,
                                    source='a' * 40, sourceTree='b' * 40, inputs={'fixture': 'identical'},
                                    modules={'baseline': 'c' * 64}, comparisonKey='sha256:' + 'd' * 64,
                                    mutationCount=13, lockSha256='e' * 64, helperSha256=host))
                builds.append(dict(source='a' * 40, lockSha256='e' * 64,
                                   engineSource='c307ec81de02852d815e9849f7c89d955c03c30d',
                                   rustc=f'rustc 1.97.0 (fixture)\nhost: {host}-unknown-linux-gnu\n',
                                   helperSha256=host, engineFeatures=['cranelift'], cargo='cargo 1.97.0'))

            def compare():
                for path, report, build in zip(paths, reports, builds):
                    path.write_text(json.dumps(report))
                    path.with_name('build.json').write_text(json.dumps(build))
                with contextlib.redirect_stdout(io.StringIO()):
                    qualify.compare(paths)

            compare()
            for group, key, changed in [(reports, 'counts', [13] * 20),
                                        (reports, 'counts', [12] * 19),
                                        (reports, 'source', 'f' * 40),
                                        (reports, 'cleanup', dict(stopExit=1, containerAbsent=True)),
                                        (builds, 'lockSha256', 'f' * 64),
                                        (builds, 'helperSha256', 'not-the-built-helper'),
                                        (builds, 'rustc', 'rustc 1.96.0 (fixture)'),
                                        (builds, 'engineFeatures', ['threads'])]:
                original = group[1][key]
                group[1][key] = changed
                with self.subTest(key=key, changed=changed), self.assertRaises(AssertionError):
                    compare()
                group[1][key] = original


if __name__ == '__main__':
    unittest.main()
