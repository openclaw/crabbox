#!/usr/bin/env python3
"""Disposable, local Docker proof for static SSH power custody (no cloud access)."""
import argparse
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import tempfile
import time

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--binary', required=True)
args = parser.parse_args()
binary = str(Path(args.binary).resolve())
docker = shutil.which('docker')
if not docker:
    raise SystemExit('Docker is required')
work = Path(tempfile.mkdtemp(prefix='cbx-ssh-power-proof-'))
name = 'cbx-ssh-power-' + str(os.getpid())
image = name + ':proof'
children = []
logs = []


def run(argv, **kwargs):
    result = subprocess.run(argv, text=True, stdout=subprocess.PIPE,
                            stderr=subprocess.STDOUT, **kwargs)
    if result.returncode:
        raise RuntimeError(f'{argv[0]} {argv[1]} exit={result.returncode}: {result.stdout[-5000:]}')
    return result.stdout.strip()


def wait_for(predicate, description, seconds=45):
    end = time.monotonic() + seconds
    while time.monotonic() < end:
        if predicate():
            return
        time.sleep(.1)
    raise RuntimeError('timed out: ' + description)


def running(container=name):
    return run([docker, 'inspect', '--format', '{{.State.Running}}', container]) == 'true'


def calls():
    path = work / 'hooks.log'
    return path.read_text().splitlines() if path.exists() else []


def custody(lease):
    files = list((work / 'state').rglob('ssh-power/*.json'))
    for path in files:
        ref = json.loads(path.read_text())['references'].get(lease)
        if ref:
            return ref['state']
    return None


def launch(lease, command):
    output = work / (lease + '.log')
    log = output.open('w')
    logs.append(log)
    child_env = dict(env, CRABBOX_STATIC_ID=lease)
    process = subprocess.Popen([binary, 'run', '--provider', 'ssh', '--no-sync',
                                '--record-local', '--static-work-root', '/work/' + lease,
                                '--', '/bin/sh', '-c', command],
                               env=child_env, stdout=log, stderr=subprocess.STDOUT)
    children.append(process)
    return process, output


def finished(process, output, success=True):
    try:
        code = process.wait(timeout=90)
    except subprocess.TimeoutExpired as exc:
        raise RuntimeError('run timed out: ' + output.read_text()[-6000:]) from exc
    if (code == 0) != success:
        raise RuntimeError(f'run exit={code}: {output.read_text()[-6000:]}')
    return code


def stop(lease, force=False):
    argv = [binary, 'stop', '--provider', 'ssh', '--id', lease]
    if force:
        argv += ['--force', '--static-power-acknowledge-stop']
    return run(argv, env=env)


try:
    run([docker, 'info', '--format', '{{.ServerVersion}}'])
    run(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', str(work / 'key')])
    (work / 'authorized_keys').write_bytes((work / 'key.pub').read_bytes())
    (work / 'Dockerfile').write_text('''FROM alpine:3.22
RUN apk add --no-cache openssh-server bash git rsync tar coreutils procps python3 util-linux && ssh-keygen -A && passwd -d root && mkdir -p /root/.ssh
COPY authorized_keys /root/.ssh/authorized_keys
RUN chmod 700 /root/.ssh && chmod 600 /root/.ssh/authorized_keys
CMD ["/usr/sbin/sshd", "-D", "-e", "-o", "PasswordAuthentication=no", "-o", "PermitRootLogin=prohibit-password"]
''')
    (work / '.dockerignore').write_text('*\n!Dockerfile\n!authorized_keys\n')
    run([docker, 'build', '--quiet', '-t', image, str(work)])
    run([docker, 'create', '--name', name, '-p', '127.0.0.1:2222:22', image])
    # Hook paths/argv stay constant; control files inject failures without
    # changing the durable authority contract. No secrets enter argv or logs.
    hook = work / 'hook'
    hook.write_text('''#!/bin/sh
set -eu
printf '%s\\n' "$1" >> "$PROOF_ROOT/hooks.log"
if [ "$1" = start ] && [ -e "$PROOF_ROOT/pause" ]; then
  /bin/sh -c 'sleep 120 & echo $! > "$PROOF_ROOT/grandchild"; wait' &
  wait
fi
exec "$PROOF_DOCKER" "$1" "$PROOF_CONTAINER"
''')
    hook.chmod(0o700)
    config = work / 'config.json'
    config.write_text(json.dumps({'provider': 'ssh', 'target': 'linux',
        'ssh': {'key': str(work / 'key'), 'fallbackPorts': []},
        'static': {'host': '127.0.0.1', 'user': 'root', 'port': '2222',
                   'workRoot': '/work', 'power': {'dedicated': True, 'hostID': name},
                   'startCommand': [str(hook), 'start'],
                   'stopCommand': [str(hook), 'stop']}}))
    env = {key: value for key, value in os.environ.items() if not key.startswith('CRABBOX_')}
    env.update(CRABBOX_CONFIG=str(config), XDG_CONFIG_HOME=str(work / 'config'),
               XDG_STATE_HOME=str(work / 'state'), PROOF_ROOT=str(work),
               PROOF_DOCKER=docker, PROOF_CONTAINER=name)
    assert not running()
    process, output = launch('static_single', 'echo powered-command-ok')
    finished(process, output)
    assert 'powered-command-ok' in output.read_text() and not running()
    assert calls() == ['start', 'stop']
    print('single: stopped -> command=powered-command-ok -> stopped; hooks=start,stop', flush=True)

    for first in ['a', 'b']:
        before = len(calls())
        processes = {}
        for suffix in ['a', 'b']:
            lease = 'static_overlap_' + suffix
            processes[suffix] = launch(lease, 'touch /tmp/ready-' + suffix + '; while [ ! -e /tmp/release-' + suffix + ' ]; do sleep .1; done')
            wait_for(lambda: custody(lease) == 'active', lease + ' active')
        wait_for(lambda: subprocess.run([docker, 'exec', name, 'test', '-e', '/tmp/ready-b'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0, 'both workloads ready')
        assert calls()[before:] == ['start']
        run([docker, 'exec', name, 'touch', '/tmp/release-' + first])
        finished(*processes[first])
        assert running() and calls()[before:] == ['start']
        other = 'b' if first == 'a' else 'a'
        run([docker, 'exec', name, 'touch', '/tmp/release-' + other])
        finished(*processes[other])
        assert not running() and calls()[before:] == ['start', 'stop']
        print(f'overlap release={first},{other}: start=1; first-release=running; final-release=stopped; stop=1', flush=True)
        # Reset only the fixture's workload markers for the reverse order.
        run([docker, 'start', name])
        run([docker, 'exec', name, 'rm', '-f', '/tmp/ready-a', '/tmp/ready-b', '/tmp/release-a', '/tmp/release-b'])
        run([docker, 'stop', name])

    (work / 'pause').touch()
    process, output = launch('static_interrupted', 'echo must-not-run')
    wait_for(lambda: (work / 'grandchild').exists(), 'start hook grandchild')
    pid = int((work / 'grandchild').read_text())
    process.send_signal(signal.SIGINT)
    code = finished(process, output, success=False)
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        pass
    else:
        raise RuntimeError('start hook grandchild survived cancellation')
    assert custody('static_interrupted') == 'pending-stop'
    (work / 'pause').unlink()
    stop('static_interrupted', force=True)
    assert custody('static_interrupted') is None and not running()
    print(f'interrupt: exit={code}; grandchild=gone; state=pending-stop; forced-acknowledged-retry=cleared', flush=True)

    process, output = launch('static_retry', 'touch /tmp/retry-ready; while [ ! -e /tmp/retry-release ]; do sleep .1; done')
    wait_for(lambda: subprocess.run([docker, 'exec', name, 'test', '-e', '/tmp/retry-ready'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0, 'retry workload ready')
    run([docker, 'rename', name, name + '-renamed'])
    run([docker, 'exec', name + '-renamed', 'touch', '/tmp/retry-release'])
    finished(process, output, success=False)
    assert custody('static_retry') == 'pending-stop' and running(name + '-renamed')
    run([docker, 'rename', name + '-renamed', name])
    stop('static_retry')
    assert custody('static_retry') is None and not running()
    print('stop-failure: renamed-container=running; state=pending-stop; restored-name+ordinary-stop=stopped; custody=cleared', flush=True)
finally:
    for process in children:
        if process.poll() is None:
            process.send_signal(signal.SIGINT)
            try:
                process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
    for log in logs:
        log.close()
    for container in [name, name + '-renamed']:
        subprocess.run([docker, 'rm', '-f', container], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    subprocess.run([docker, 'image', 'rm', image], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    shutil.rmtree(work)
    print('cleanup: fixture container, image, generated key and local state removed', flush=True)
