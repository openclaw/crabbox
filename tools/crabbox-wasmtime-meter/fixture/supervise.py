#!/usr/bin/python3
"""Owned Linux lease fixture. No Docker control or host credential enters here."""

import hashlib
import json
import math
import os
from pathlib import Path
import re
import resource
import selectors
import shutil
import signal
import stat
import subprocess
import sys
import tempfile
import time

UID = 60001
OUTPUT_LIMIT = 262144
EVIDENCE_LIMIT = 65536


def meter_processes():
    found = []
    for path in Path('/proc').glob('[0-9]*/status'):
        try:
            rows = dict(line.split(':', 1) for line in path.read_text().splitlines() if ':' in line)
            if str(UID) in rows['Uid'].split():
                found.append(int(path.parent.name))
        except FileNotFoundError:
            continue
    return found


def limits():
    resource.setrlimit(resource.RLIMIT_NPROC, (64, 64))
    resource.setrlimit(resource.RLIMIT_FSIZE, (1048576, 1048576))
    resource.setrlimit(resource.RLIMIT_NOFILE, (128, 128))
    resource.setrlimit(resource.RLIMIT_CORE, (0, 0))


def interrupted(signum, _frame):
    raise RuntimeError(f'fixture interrupted by signal {signum}')


def regular_bytes(path, limit):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd, 'rb') as source:
        info = os.fstat(source.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_uid != UID:
            raise RuntimeError('unexpected evidence identity')
        value = source.read(limit + 1)
    if len(value) > limit:
        raise RuntimeError('evidence exceeds limit')
    return value


def waited_child_cpu():
    try:
        usage = resource.getrusage(resource.RUSAGE_CHILDREN)
        values = (usage.ru_utime, usage.ru_stime)
        if all(type(value) in (int, float) and math.isfinite(value) and value >= 0 for value in values):
            return values
    except Exception:
        pass
    return None


def waited_child_cpu_delta(before):
    after = waited_child_cpu()
    if before is not None and after is not None:
        delta = tuple(end - start for start, end in zip(before, after))
        if all(math.isfinite(value) and value >= 0 for value in delta):
            return dict(user=delta[0], system=delta[1])
    return None


def main():
    if os.geteuid() != 0:
        raise RuntimeError('fixture supervisor requires the lease bootstrap account')
    if sys.argv[1:] == ['check-cleanup']:
        deadline = time.monotonic() + 5
        while meter_processes() and time.monotonic() < deadline:
            time.sleep(0.1)
        if meter_processes():
            raise RuntimeError('meter process survived its invocation')
        if any(Path('/run/meter-fixture').iterdir()):
            raise RuntimeError('trial scratch survived its invocation')
        print('fixture-cleanup-ok', flush=True)
        return 0
    if len(sys.argv) != 5:
        raise RuntimeError('expected trial, module/probe, expected digest and budget')
    trial, selection, expected, budget = sys.argv[1:]
    if not re.fullmatch(r'[a-z0-9-]{1,60}', trial):
        raise RuntimeError('invalid trial identifier')
    if not budget.isascii() or not budget.isdecimal() or int(budget) > 9007199254740991:
        raise RuntimeError('invalid budget')
    work = Path.cwd().resolve()
    if not work.is_relative_to('/work/crabbox') or meter_processes():
        raise RuntimeError('unexpected work root or previous meter process')
    artifacts = work / '.artifacts'
    artifacts.mkdir(mode=0o700, exist_ok=True)
    controller = int(os.environ['SUDO_UID'])
    artifact_info = artifacts.lstat()
    if not stat.S_ISDIR(artifact_info.st_mode) or artifact_info.st_uid not in (0, controller):
        raise RuntimeError('unexpected artifact directory identity')
    os.chown(artifacts, controller, -1, follow_symlinks=False)
    destination = artifacts / f'{trial}.json'
    if destination.exists() or destination.is_symlink():
        raise RuntimeError('refusing stale artifact destination')
    directory = Path(tempfile.mkdtemp(prefix=f'{trial}-', dir='/run/meter-fixture'))
    try:
        return invoke(directory, work, destination, controller, selection, expected, budget)
    finally:
        shutil.rmtree(directory)


def invoke(directory, work, destination, controller, selection, expected, budget):
    directory.chmod(0o711)
    output = directory / 'out'
    output.mkdir(mode=0o700)
    os.chown(output, UID, UID)
    evidence = output / 'evidence.json'
    bad_environment = selection == 'probe-bad-environment'
    probe = selection.startswith('probe-')
    if probe:
        child_arguments = ['--probe', 'identity' if bad_environment else selection.removeprefix('probe-')]
    else:
        module_name = {'cancel': 'infinite', 'bad-helper-digest': 'baseline'}.get(selection, selection)
        if module_name not in ('baseline', 'mutation', 'trap', 'exit', 'infinite'):
            raise RuntimeError('unknown module')
        data = (work / 'inputs' / f'{module_name}.wasm').read_bytes()
        if len(data) > 16777216 or hashlib.sha256(data).hexdigest() != expected:
            raise RuntimeError('staged module provenance mismatch')
        module = directory / 'module.wasm'
        module.write_bytes(data)
        module.chmod(0o444)
        helper_digest = '0' * 64 if selection == 'bad-helper-digest' else expected
        child_arguments = ['--', '--module', str(module), '--module-sha256', helper_digest,
                           '--output', str(evidence), '--workload', 'example-org/workflow-action-pin-tests/v2',
                           '--provider', 'local-container', '--budget', budget]
    command = ['/usr/bin/setpriv', '--reuid=60001', '--regid=60001', '--clear-groups',
               '--inh-caps=-all', '--ambient-caps=-all', '--bounding-set=-all', '--no-new-privs',
               '/usr/local/bin/fixture-child', *child_arguments]
    seeded = os.open('/root/controller-sentinel', os.O_RDONLY)
    if seeded != 42:
        os.dup2(seeded, 42, inheritable=True)
        os.close(seeded)
    child = None
    diagnostic = None
    streams = selectors.DefaultSelector()
    buffers = [bytearray(), bytearray()]
    try:
        cpu_before = waited_child_cpu()
        child = subprocess.Popen(command, env={'UNEXPECTED': 'fixture'} if bad_environment else {}, stdin=subprocess.DEVNULL,
                                 stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                 pass_fds=(42,), start_new_session=True, preexec_fn=limits)
        os.close(42)
        print('fixture-child-started', flush=True)
        streams.register(child.stdout, selectors.EVENT_READ, 0)
        streams.register(child.stderr, selectors.EVENT_READ, 1)
        deadline = time.monotonic() + (2 if selection == 'infinite' else 120)
        guest_announced = False
        while streams.get_map():
            if time.monotonic() >= deadline:
                raise RuntimeError('fixture child deadline exceeded')
            for key, _event in streams.select(timeout=0.1):
                block = os.read(key.fileobj.fileno(), 65536)
                if not block:
                    streams.unregister(key.fileobj)
                    continue
                buffers[key.data].extend(block)
                if selection == 'cancel' and not guest_announced and b'meter: guest-started\n' in buffers[1]:
                    print('fixture-guest-started', flush=True)
                    guest_announced = True
                if len(buffers[key.data]) > OUTPUT_LIMIT:
                    raise RuntimeError('fixture child output limit exceeded')
        code = child.wait(timeout=5)
        if code not in (0, 3):
            if evidence.exists():
                raise RuntimeError('failed child left evidence')
            sys.stderr.buffer.write(buffers[1])
            return code if 0 < code < 128 else 125
        if probe:
            if bytes(buffers[0]).strip() != f'probe-ok:{selection.removeprefix("probe-")}'.encode():
                raise RuntimeError('probe did not produce its exact success marker')
            print(bytes(buffers[0]).decode(), end='')
            return 0
        data = regular_bytes(evidence, EVIDENCE_LIMIT)
        # Publication remains data-only. The external consumer independently
        # validates provenance, canonical-key arithmetic and schema semantics.
        json.loads(data)
        fd = os.open(destination, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, 'wb') as target:
            target.write(data)
            target.flush()
            os.fsync(target.fileno())
            os.fchown(target.fileno(), controller, -1)
        return code
    except Exception:
        try:
            exit_code = child.poll() if child is not None else None
            tail = bytes(buffers[1]).decode(errors='replace').encode()[-2048:].decode(errors='ignore')
            diagnostic = dict(phase='child-supervision', spawned=child is not None,
                              guestStarted=b'meter: guest-started\n' in buffers[1],
                              childState='not-spawned' if child is None else 'live' if exit_code is None else 'exited',
                              exitCode=exit_code, stdoutBytes=len(buffers[0]), stderrBytes=len(buffers[1]),
                              stderrTail=tail, waitedChildCpuSeconds=None,
                              cpuScope='waited child and descendants waited by their parents')
        except Exception:
            pass  # Diagnostics must not replace the original failure or skip reaping.
        raise
    finally:
        try:
            try:
                os.close(42)
            except OSError:
                pass
            streams.close()
            if child is not None:
                try:
                    os.killpg(child.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                child.wait(timeout=5)
                if diagnostic is not None:
                    # This invocation starts only this child. RUSAGE_CHILDREN also
                    # includes descendants whose intermediate parents waited for them.
                    try:
                        diagnostic['waitedChildCpuSeconds'] = waited_child_cpu_delta(cpu_before)
                    except Exception:
                        pass  # Accounting must not interrupt the remaining cleanup.
            if meter_processes():
                raise RuntimeError('meter process survived reaping')
        finally:
            if diagnostic is not None:
                try:
                    print('fixture failure: ' + json.dumps(diagnostic, sort_keys=True), file=sys.stderr, flush=True)
                except Exception:
                    pass  # A broken sink must not replace the failure or interrupt cleanup.


if __name__ == '__main__':
    for number in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
        signal.signal(number, interrupted)
    try:
        sys.exit(main())
    except Exception as error:
        print(f'fixture: {error}', file=sys.stderr)
        sys.exit(125)
