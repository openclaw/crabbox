import copy
import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest

import qualify


class ConsumerContract(unittest.TestCase):
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
