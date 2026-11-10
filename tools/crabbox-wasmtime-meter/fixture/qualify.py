#!/usr/bin/env python3
"""Source-built qualification through the real Crabbox SSH/artifact consumer."""

import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import selectors
import shutil
import signal
import subprocess
import sys
import tempfile
import time

ROOT = Path(__file__).resolve().parents[3]
HELPER = ROOT / 'tools/crabbox-wasmtime-meter'
INITIAL = 9007199254740991
WORKLOAD = 'example-org/workflow-action-pin-tests/v2'
# Temporary calibration ceiling; a v1 count does not establish a v2 budget.
CALIBRATION_BUDGET = 1_000_000_000
ARGV = ['workflow-action-pins.test',
        '-test.run=^(TestCheckWorkflowNodeAcceptsImmutableUsesSyntaxes|TestCheckWorkflowNodeIgnoresUnrelatedUsesKeys)$',
        '-test.count=1', '-test.shuffle=off']


def sha(data):
    return hashlib.sha256(data).hexdigest()


def command(args, *, cwd=ROOT, env=None, expected=0, timeout=300):
    result = subprocess.run([str(x) for x in args], cwd=cwd, env=env, capture_output=True, timeout=timeout)
    if result.returncode != expected:
        raise RuntimeError(f'{args[0]} exited {result.returncode}, expected {expected}: '
                           + result.stderr[-8192:].decode(errors='replace'))
    return result.stdout.decode()


def strict_json(data):
    if len(data) > 65536:
        raise ValueError('evidence size limit exceeded')
    def pairs(entries):
        result = {}
        for key, value in entries:
            if key in result:
                raise ValueError('duplicate JSON field')
            result[key] = value
        return result
    def invalid(_value):
        raise ValueError('non-JSON number')
    return json.loads(data, object_pairs_hook=pairs, parse_constant=invalid)


def validate(data, module_digest, budget, *, oracle=True):
    value = strict_json(data)
    assert type(value['schemaVersion']) is int and value['schemaVersion'] == 1
    assert value['provider'] == 'local-container'
    assert re.fullmatch(r'\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|[+-]\d\d:\d\d)', value['generatedAt'])
    assert datetime.datetime.fromisoformat(value['generatedAt'].replace('Z', '+00:00')).tzinfo is not None
    workload, meter, results = value['workload'], value['meter'], value['results']
    expected_workload = dict(name=WORKLOAD, moduleSha256=module_digest, toolchain='go1.27.3', target='wasip1/wasm', argv=ARGV)
    assert {key: workload[key] for key in expected_workload} == expected_workload
    assert meter['mechanism'] == 'wasmtime-fuel' and meter['engine'] == 'wasmtime'
    assert meter['engineVersion'] == '50.0.1'
    assert meter['engineRevision'] == 'c307ec81de02852d815e9849f7c89d955c03c30d'
    assert meter['configuration'] == 'crabbox-wasmtime-fuel/v2'
    assert meter['wasiConfiguration'] == 'crabbox-deterministic-wasi/v1'
    assert meter['deterministic'] is True and type(meter['initialFuel']) is int and meter['initialFuel'] == INITIAL
    assert type(meter['remainingFuel']) is int and 0 <= meter['remainingFuel'] <= INITIAL
    assert len(results) == 1
    row = results[0]
    assert row['metric'] == row['unit'] == 'fuel'
    assert type(row['budget']) is int and row['budget'] == budget
    assert type(row['measured']) is int and row['measured'] == INITIAL - meter['remainingFuel']
    assert row['exceeded'] is (row['measured'] > budget) and value['exceeded'] is row['exceeded']
    projection = dict(argv=ARGV, configuration=meter['configuration'], engine='wasmtime',
                      engineRevision=meter['engineRevision'], engineVersion=meter['engineVersion'],
                      metric='fuel', target='wasip1/wasm', toolchain='go1.27.3', unit='fuel',
                      wasiConfiguration=meter['wasiConfiguration'], workload=WORKLOAD)
    canonical = json.dumps(projection, sort_keys=True, ensure_ascii=True, separators=(',', ':')).encode()
    assert meter['comparisonKey'] == 'sha256:' + sha(canonical)
    if oracle:
        assert value['guestOutput'] == dict(stdoutSha256=sha(b'PASS\n'), stderrSha256=sha(b''))
    return value


def section(number, payload):
    def leb(value):
        result = bytearray()
        while True:
            part, value = value & 127, value >> 7
            result.append(part | (128 if value else 0))
            if not value:
                return bytes(result)
    return bytes([number]) + leb(len(payload)) + payload


def probe_module(body, *, exit_code=None):
    # Minimal binary fixtures: no external compiler, package or alternate runtime.
    header = b'\0asm\x01\0\0\0'
    if exit_code is None:
        return header + section(1, b'\x01\x60\x00\x00') + section(3, b'\x01\x00') + section(7, b'\x01\x06_start\x00\x00') + section(10, bytes([1, len(body) + 2, 0]) + body + b'\x0b')
    imports = b'\x01\x16wasi_snapshot_preview1\x09proc_exit\x00\x00'
    return (header + section(1, b'\x02\x60\x01\x7f\x00\x60\x00\x00') + section(2, imports)
            + section(3, b'\x01\x01') + section(5, b'\x01\x00\x01')
            + section(7, b'\x02\x06_start\x00\x01\x06memory\x02\x00')
            + section(10, b'\x01\x06\x00\x41' + bytes([exit_code]) + b'\x10\x00\x0b'))


def build_inputs(destination):
    destination.mkdir(parents=True, exist_ok=False)
    version = command(['go', 'version']).strip()
    assert version.startswith('go version go1.27.3 ')
    source = ROOT / 'scripts/workflow_action_pins_test.go'
    original = source.read_bytes()
    manifest = dict(source=command(['git', 'rev-parse', 'HEAD']).strip(),
                    sourceTree=command(['git', 'rev-parse', 'HEAD^{tree}']).strip(),
                    go=version, target='wasip1/wasm', argv=ARGV, sourceTestSha256=sha(original))
    env = dict(os.environ, GOOS='wasip1', GOARCH='wasm', GOTOOLCHAIN='go1.27.3', CGO_ENABLED='0')
    command(['go', 'test', '-c', '-trimpath', '-o', destination / 'baseline.wasm', './scripts'], env=env, timeout=600)
    marker = b'func TestCheckWorkflowNodeAcceptsImmutableUsesSyntaxes(t *testing.T) {\n'
    assert original.count(marker) == 1
    addition = b'\tfor i := 1; i <= 256; i++ { if len(strings.Repeat("x", i)) != i { t.Fatal("mutation oracle") } }\n'
    mutated = original.replace(marker, marker + addition)
    try:
        source.write_bytes(mutated)
        command(['go', 'test', '-c', '-trimpath', '-o', destination / 'mutation.wasm', './scripts'], env=env, timeout=600)
    finally:
        source.write_bytes(original)
    manifest['mutationSourceSha256'] = sha(mutated)
    for name, data in {'trap': probe_module(b'\x00'), 'exit': probe_module(b'', exit_code=7),
                       'infinite': probe_module(b'\x03\x40\x0c\x00\x0b')}.items():
        (destination / f'{name}.wasm').write_bytes(data)
    manifest['modules'] = {p.stem: sha(p.read_bytes()) for p in sorted(destination.glob('*.wasm'))}
    assert manifest['modules']['baseline'] != manifest['modules']['mutation']
    (destination / 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')


def audit_mounts(info):
    assert info['HostConfig']['Privileged'] is False
    assert not info['HostConfig'].get('CapAdd')
    assert info['HostConfig']['NetworkMode'] == 'bridge'
    assert info['HostConfig']['Memory'] == 4 * 1024**3
    mounts = info['Mounts']
    assert len(mounts) == 1
    mount = mounts[0]
    assert mount['Type'] == 'bind' and mount['Destination'] == '/tmp/crabbox-bootstrap' and mount['RW'] is False
    directory = Path(mount['Source'])
    assert directory.is_dir() and not directory.is_symlink()
    assert [p.name for p in directory.iterdir()] == ['bootstrap.sh']
    script = directory / 'bootstrap.sh'
    assert script.is_file() and not script.is_symlink() and script.stat().st_size <= 262144
    assert 'CRABBOX_AUTHORIZED_KEY=' in '\n'.join(info['Config']['Env'])
    assert not any('PRIVATE KEY' in text for text in [script.read_text(), *info['Config']['Env']])
    return dict(destination=mount['Destination'], readOnly=True, files={'bootstrap.sh': sha(script.read_bytes())})


def qualify(args):
    inputs = Path(args.inputs).resolve()
    manifest = json.loads((inputs / 'manifest.json').read_text())
    assert manifest['source'] == command(['git', 'rev-parse', 'HEAD']).strip()
    assert manifest['sourceTree'] == command(['git', 'rev-parse', 'HEAD^{tree}']).strip()
    for name, expected in manifest['modules'].items():
        assert sha((inputs / f'{name}.wasm').read_bytes()) == expected
    architecture = {'x86_64': 'amd64', 'aarch64': 'arm64'}[platform.machine()]
    assert architecture == args.architecture
    output = Path(args.output).resolve()
    output.mkdir(parents=True, exist_ok=False)
    root = Path(tempfile.mkdtemp(prefix='crabbox-meter-consumer-'))
    project = root / 'project'
    project.mkdir()
    shutil.copytree(inputs, project / 'inputs')
    shutil.copy2(ROOT / 'docs/schemas/perf-evidence-v1.schema.json', project / 'schema.json')
    command(['git', 'init', '--quiet'], cwd=project)
    command(['git', 'add', 'inputs', 'schema.json'], cwd=project)
    command(['git', '-c', 'user.name=Fixture', '-c', 'user.email=fixture@example.invalid',
             '-c', 'commit.gpgsign=false', 'commit', '--quiet', '-m', 'fixture inputs'], cwd=project)
    for name in ('state', 'cache', 'config', 'docker-config'):
        (root / name).mkdir(mode=0o700)
    env = {key: os.environ[key] for key in ('PATH', 'TMPDIR', 'LANG') if key in os.environ}
    env.update(XDG_STATE_HOME=str(root / 'state'), XDG_CACHE_HOME=str(root / 'cache'),
               XDG_CONFIG_HOME=str(root / 'config'), DOCKER_CONFIG=str(root / 'docker-config'))
    identity = ':'.join([manifest['source'], architecture, os.environ['GITHUB_RUN_ID'], os.environ['GITHUB_RUN_ATTEMPT']])
    lease = 'cbx_' + sha(identity.encode())[:12]
    cli = Path(args.crabbox).resolve()
    provider_flags = ['--provider', 'docker', '--local-container-image', args.image,
                      '--local-container-user', 'crabbox', '--local-container-memory', '4g',
                      '--local-container-cpus', '2', '--local-container-docker-socket=false']
    common = [cli, 'run', *provider_flags, '--id', lease, '--keep']
    records = []
    report = dict(source=manifest['source'], sourceTree=manifest['sourceTree'], architecture=architecture,
                  modules=manifest['modules'], inputs=manifest, kernel=platform.release(),
                  runnerImage=os.environ.get('ImageVersion'), cases=records, counts=[],
                  lockSha256=sha((HELPER / 'Cargo.lock').read_bytes()))
    container = None
    # Never enter the cleanup scope if an unrelated resource already owns this ID.
    assert not command(['docker', 'ps', '-aq', '--filter', f'label=lease={lease}']).strip()
    try:
        command([cli, 'warmup', *provider_flags, '--lease-id', lease, '--keep'], cwd=project, env=env, timeout=300)
        command([*common, '--', 'true'], cwd=project, env=env, timeout=300)
        # Preserve remote evidence between invocations; a new sync must not
        # erase a failed command's stale artifact before the absence assertion.
        common.append('--no-sync')
        ids = command(['docker', 'ps', '-q', '--filter', f'label=lease={lease}']).split()
        assert len(ids) == 1
        container = ids[0]
        info = json.loads(command(['docker', 'inspect', container]))[0]
        report['bootstrap'] = audit_mounts(info)
        image = json.loads(command(['docker', 'image', 'inspect', info['Image']]))[0]
        assert image['Architecture'] == architecture
        report['imageId'] = image['Id']
        report['helperSha256'] = command(['docker', 'exec', container, 'sha256sum', '/usr/local/bin/crabbox-wasmtime-meter']).split()[0]
        def trial(name, selection, budget=CALIBRATION_BUDGET, expected=0, digest_override=None):
            success, failure = output / f'{name}.success.json', output / f'{name}.failure.json'
            remote = f'.artifacts/{name}.json'
            flags = [] if selection.startswith('probe-') or expected not in (0, 3) else [
                '--artifact-glob', remote, '--require-artifact', remote,
                '--require-artifact-schema', remote + '=schema.json',
                '--download', remote + '=' + str(success),
                '--download-on-failure', remote + '=' + str(failure)]
            digest = digest_override or manifest['modules'].get(selection, '0' * 64)
            result = command([*common, *flags, '--', 'sudo', '-n', '/usr/local/bin/meter-supervise',
                              name, selection, digest, str(budget)], cwd=project, env=env, expected=expected, timeout=180)
            command([*common, '--', 'sudo', '-n', '/usr/local/bin/meter-supervise', 'check-cleanup'], cwd=project, env=env)
            records.append(dict(name=name, exit=expected, outputSha256=sha(result.encode()), reaped=True))
            if flags:
                chosen, absent = (success, failure) if expected == 0 else (failure, success)
                assert chosen.is_file() and not absent.exists()
                value = validate(chosen.read_bytes(), digest, budget)
                chosen.unlink()  # retain one compact report, not forty raw duplicates
                return value
            assert not success.exists() and not failure.exists()
            command([*common, '--', 'test', '!', '-e', remote], cwd=project, env=env)
            return None
        for index in range(20):
            value = trial(f'repeat-{index:02}', 'baseline')
            report['counts'].append(value['results'][0]['measured'])
            report['comparisonKey'] = value['meter']['comparisonKey']
            if index == 0:
                report['firstEvidence'] = value
        assert len(set(report['counts'])) == 1 and report['counts'][0] > 0
        count = report['counts'][0]
        assert trial('equal', 'baseline', count)['exceeded'] is False
        assert trial('below', 'baseline', count - 1, 3)['exceeded'] is True
        mutation = trial('mutation', 'mutation')
        assert mutation['results'][0]['measured'] > count
        assert mutation['meter']['comparisonKey'] == report['comparisonKey']
        report['mutationCount'] = mutation['results'][0]['measured']
        for selection in ('sockets', 'identity', 'resource'):
            trial('probe-' + selection, 'probe-' + selection)
        trial('trap', 'trap', expected=2)
        trial('guest-exit', 'exit', expected=2)
        trial('deadline', 'infinite', expected=125)
        trial('output-limit', 'probe-flood', expected=125)
        trial('file-limit', 'probe-file-limit', expected=125)
        trial('privilege-escalation', 'probe-sudo', expected=1)
        trial('bad-environment', 'probe-bad-environment', expected=125)
        trial('provenance', 'baseline', expected=125, digest_override='0' * 64)
        trial('helper-provenance', 'bad-helper-digest', expected=2, digest_override=manifest['modules']['baseline'])
        # Cancel the actual Crabbox process after the remote child exists; then
        # inspect the same retained lease, without retrying that invocation.
        cancel = subprocess.Popen([str(x) for x in [*common, '--', 'sudo', '-n', '/usr/local/bin/meter-supervise',
            'cancel', 'cancel', manifest['modules']['infinite'], '1']], cwd=project, env=env, stdout=subprocess.PIPE,
            stderr=subprocess.PIPE, text=True, start_new_session=True)
        ready = False
        poll = selectors.DefaultSelector()
        poll.register(cancel.stdout, selectors.EVENT_READ)
        cancel_output = bytearray()
        limit = time.monotonic() + 60
        try:
            while time.monotonic() < limit and cancel.poll() is None:
                for _key, _event in poll.select(timeout=0.2):
                    cancel_output.extend(os.read(cancel.stdout.fileno(), 4096))
                    assert len(cancel_output) <= 262144, 'cancellation output limit exceeded'
                    if b'fixture-guest-started' in cancel_output:
                        ready = True
                        break
                if ready:
                    break
            assert ready, 'remote child never became ready for cancellation'
            cancel.send_signal(signal.SIGINT)
            _stdout, _stderr = cancel.communicate(timeout=30)
            assert cancel.returncode != 0
        finally:
            poll.close()
            if cancel.poll() is None:
                os.killpg(cancel.pid, signal.SIGKILL)
                cancel.wait(timeout=5)
        command([*common, '--', 'sudo', '-n', '/usr/local/bin/meter-supervise', 'check-cleanup'], cwd=project, env=env)
        command([*common, '--', 'test', '!', '-e', '.artifacts/cancel.json'], cwd=project, env=env)
        records.append(dict(name='cancel', exit=cancel.returncode, reaped=True))
        report['result'] = 'passed'
    except Exception as error:
        report['result'] = 'failed'
        report['failure'] = str(error)[-8192:]
        raise
    finally:
        stop = subprocess.run([str(cli), 'stop', '--provider', 'docker', lease], cwd=project, env=env, capture_output=True, timeout=120)
        remaining = command(['docker', 'ps', '-aq', '--filter', f'label=lease={lease}']).strip()
        report['cleanup'] = dict(stopExit=stop.returncode, containerAbsent=not remaining)
        (output / 'qualification.json').write_text(json.dumps(report, indent=2) + '\n')
        if stop.returncode != 0 or remaining:
            raise RuntimeError('owned fixture cleanup failed')
        shutil.rmtree(root)


def compare(paths):
    values = [json.loads(Path(p).read_text()) for p in paths]
    assert {v['architecture'] for v in values} == {'amd64', 'arm64'} and len(values) == 2
    for value in values:
        assert value['result'] == 'passed' and value['cleanup'] == dict(stopExit=0, containerAbsent=True)
        assert len(value['counts']) >= 20 and len(set(value['counts'])) == 1
    builds = [json.loads(Path(p).with_name('build.json').read_text()) for p in paths]
    for value, build in zip(values, builds):
        assert build['source'] == value['source']
        assert build['lockSha256'] == value['lockSha256']
        assert build['engineSource'] == 'c307ec81de02852d815e9849f7c89d955c03c30d'
        assert build['rustc'].startswith('rustc 1.97.0 ')
        assert build['helperSha256'] == value['helperSha256']
        assert 'host: ' + {'amd64': 'x86_64', 'arm64': 'aarch64'}[value['architecture']] + '-unknown-linux-gnu' in build['rustc']
    assert builds[0]['engineFeatures'] == builds[1]['engineFeatures']
    assert builds[0]['cargo'] == builds[1]['cargo']
    for key in ('source', 'sourceTree', 'inputs', 'modules', 'lockSha256', 'comparisonKey', 'counts', 'mutationCount'):
        assert values[0][key] == values[1][key], f'cross-host mismatch: {key}'
    print(json.dumps(dict(source=values[0]['source'], count=values[0]['counts'][0],
                         repetitionsPerArchitecture=20, exactCrossHostEquality=True)))


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    sub = parser.add_subparsers(dest='mode', required=True)
    build = sub.add_parser('build-inputs')
    build.add_argument('destination', type=Path)
    run = sub.add_parser('run')
    for name in ('inputs', 'output', 'architecture', 'image', 'crabbox'):
        run.add_argument('--' + name, required=True)
    pair = sub.add_parser('compare')
    pair.add_argument('reports', nargs=2)
    args = parser.parse_args()
    if args.mode == 'build-inputs':
        build_inputs(args.destination)
    elif args.mode == 'run':
        qualify(args)
    else:
        compare(args.reports)
