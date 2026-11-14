#!/usr/bin/env python3
"""One first ownership walk of one explicitly identified disposable EBS copy.

Run only inside an owned private mount namespace and transient resource-bounded
systemd unit. Inputs are supplied by the protected qualification executor.
"""
import argparse
import grp
import hashlib
import json
import os
from pathlib import Path
import pwd
import signal
import stat
import subprocess
import tempfile
import time


def exact_device(volume_id, sys_block=Path('/sys/block')):
    if not volume_id.startswith('vol-') or not volume_id[4:] or any(c not in '0123456789abcdef' for c in volume_id[4:]):
        raise ValueError('invalid exact EBS volume ID')
    matches = []
    for path in sys_block.glob('nvme*n*'):
        serial = path / 'device/serial'
        if serial.exists() and serial.read_text().strip() == volume_id.replace('-', ''):
            matches.append(path.name)
    if len(matches) != 1:
        raise RuntimeError('exact EBS NVMe serial is absent or ambiguous')
    return matches[0]


def recipe_body(recipe, digest):
    raw = recipe.read_bytes()
    if hashlib.sha256(raw).hexdigest() != digest:
        raise RuntimeError('serial recipe digest mismatch')
    text = raw.decode()
    marker = "<<'CRABBOX_OWNERSHIP'\n"
    if text.count(marker) != 1 or not text.endswith('CRABBOX_OWNERSHIP\n'):
        raise RuntimeError('unsupported canonical recipe')
    return text.split(marker)[1].removesuffix('CRABBOX_OWNERSHIP\n')


def safe_root(mount, absolute):
    if not absolute.startswith('/') or any(p in ('.', '..') for p in absolute.split('/')):
        raise ValueError('saved root must be a canonical absolute path')
    # Only ancestor metadata; never a preflight tree walk. No other guest writer
    # can reach this private mount. The routine retains its own no-follow policy.
    components = [p for p in absolute.split('/') if p]
    if not components:
        raise ValueError('whole-filesystem ownership repair is outside scope')
    fd = os.open(mount, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        for part in components[:-1]:
            child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            os.close(fd)
            fd = child
    finally:
        os.close(fd)
    return str(mount / absolute.lstrip('/'))


def verify_layout(roots, uid, gid):
    result = {'visited': 0, 'mismatched': 0, 'files': 0, 'directories': 0,
              'symlinks': 0, 'maxDepth': 0, 'maxFanout': 0}
    for root in roots:
        stack = []
        parent, name = os.path.split(root)
        parent_fd = os.open(parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        def visit(name, parent_fd, depth):
            info = os.stat(name, dir_fd=parent_fd, follow_symlinks=False)
            result['visited'] += 1
            result['mismatched'] += int((info.st_uid, info.st_gid) != (uid, gid))
            result['maxDepth'] = max(result['maxDepth'], depth)
            if stat.S_ISDIR(info.st_mode):
                result['directories'] += 1
                fd = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent_fd)
                try:
                    iterator = os.scandir(fd)
                except BaseException:
                    os.close(fd)
                    raise
                stack.append([fd, iterator, depth, 0])
            elif stat.S_ISLNK(info.st_mode):
                result['symlinks'] += 1
            else:
                result['files'] += 1
        try:
            visit(name, parent_fd, 0)
            while stack:
                frame = stack[-1]
                entry = next(frame[1], None)
                if entry is None:
                    result['maxFanout'] = max(result['maxFanout'], frame[3])
                    frame[1].close()
                    os.close(frame[0])
                    stack.pop()
                else:
                    frame[3] += 1
                    visit(entry.name, frame[0], frame[2] + 1)
        finally:
            for fd, iterator, _, _ in reversed(stack):
                iterator.close()
                os.close(fd)
            os.close(parent_fd)
    if result['mismatched']:
        raise RuntimeError('post-walk ownership verification failed: ' + json.dumps(result))
    return result


def observe(body, user, roots, out):
    command = ['/usr/bin/python3', '-I', '-', 'crabbox-ownership-repair-v1', user, *roots]
    child = None
    last_io = None
    started = time.monotonic()
    try:
        with (out / 'ownership.stdout').open('wb') as stdout, (out / 'ownership.stderr').open('wb') as stderr:
            child = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=stdout, stderr=stderr,
                                     start_new_session=True)
            child.stdin.write(body.encode())
            child.stdin.close()
            while True:
                try:
                    last_io = Path(f'/proc/{child.pid}/io').read_text()
                except FileNotFoundError:
                    pass
                waited, status, usage = os.wait4(child.pid, os.WNOHANG)
                if waited:
                    child.returncode = os.waitstatus_to_exitcode(status)
                    break
                if time.monotonic() - started >= 2700:
                    raise TimeoutError('45-minute first-walk bound exceeded; do not repeat warm')
                time.sleep(0.1)
        return {'exit': child.returncode, 'elapsedSeconds': time.monotonic() - started,
                'cpuUserSeconds': usage.ru_utime, 'cpuSystemSeconds': usage.ru_stime,
                'maxRSSKiB': usage.ru_maxrss, 'inputBlocks': usage.ru_inblock,
                'outputBlocks': usage.ru_oublock, 'lastProcIO': last_io,
                'procIOLimitation': 'last sampled value; final wait4 input/output blocks are complete',
                'summary': (out / 'ownership.stdout').read_text(),
                'stderr': (out / 'ownership.stderr').read_text()}
    finally:
        if child is not None and child.returncode is None:
            try:
                os.killpg(child.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            finally:
                child.wait()


def consume_once(receipts, volume_id, digest):
    # Open the root-owned receipt directory without following substitutions.
    receipts.mkdir(mode=0o700, exist_ok=True)
    directory = os.open(receipts, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        info = os.fstat(directory)
        if info.st_uid != os.geteuid() or info.st_mode & 0o022:
            raise RuntimeError('unsafe first-walk receipt directory')
        fd = os.open(volume_id + '.started', os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                     0o600, dir_fd=directory)
        try:
            os.write(fd, (digest + '\n').encode())
            os.fsync(fd)
        finally:
            os.close(fd)
        os.fsync(directory)
    finally:
        os.close(directory)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--volume', required=True)
    parser.add_argument('--saved-work-root', required=True)
    parser.add_argument('--recipe', type=Path, required=True)
    parser.add_argument('--recipe-sha256', required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    if os.geteuid() != 0 or os.readlink('/proc/self/ns/mnt') == os.readlink('/proc/1/ns/mnt'):
        raise RuntimeError('root in a private mount namespace is required')
    for service in ('udisks2.service', 'autofs.service'):
        if subprocess.run(['systemctl', 'is-active', '--quiet', service]).returncode == 0:
            raise RuntimeError('automounter present; first-walk admission refused')
    args.output.mkdir(mode=0o700, parents=True, exist_ok=False)
    body = recipe_body(args.recipe, args.recipe_sha256)
    device = exact_device(args.volume)
    partition = Path('/dev') / (device + 'p1')
    if not partition.is_block_device():
        raise RuntimeError('initial capability requires partition 1 of the exact EBS device')
    device_number = partition.stat().st_rdev
    mounted = subprocess.check_output(['findmnt', '-rn', '-o', 'MAJ:MIN'], text=True).splitlines()
    if f'{os.major(device_number)}:{os.minor(device_number)}' in mounted:
        raise RuntimeError('copy already mounted before first-walk admission')
    # Stock-root receipt survives executor/SSH retries and is never on the copy.
    # Once consumed, a failed or timed-out observation cannot be replayed warm.
    receipts = Path('/var/lib/crabbox/first-walk')
    consume_once(receipts, args.volume, args.recipe_sha256)
    account = pwd.getpwnam('crabbox')
    gid = grp.getgrnam('crabbox').gr_gid
    result = {'volumeID': args.volume, 'device': device, 'partition': str(partition),
              'uid': account.pw_uid, 'gid': gid, 'recipeSha256': args.recipe_sha256,
              'limitation': 'mount/replay and ancestor metadata may be warm; attached data omits original root-boot contention'}
    mount = Path(tempfile.mkdtemp(prefix='crabbox-first-walk-'))
    mounted_copy = False
    try:
        mount_start = time.monotonic()
        subprocess.run(['mount', '-o', 'rw,noatime,nosuid,nodev,noexec', '--source', str(partition), '--target', str(mount)], check=True)
        mounted_copy = True
        result['mountElapsedSeconds'] = time.monotonic() - mount_start
        result['mountInfo'] = Path('/proc/self/mountinfo').read_text()
        kernel_log = subprocess.check_output(['dmesg', '--time-format=iso'], text=True)
        result['mountJournalMessages'] = '\n'.join(line for line in kernel_log.splitlines() if device in line)[-65536:]
        result['diskstatsBefore'] = Path('/proc/diskstats').read_text()
        roots = [safe_root(mount, args.saved_work_root), safe_root(mount, '/var/cache/crabbox')]
        result['ownership'] = observe(body, 'crabbox', roots, args.output)
        result['diskstatsAfter'] = Path('/proc/diskstats').read_text()
        # A failed/timed-out first call remains failed; never replay it warm.
        if result['ownership']['exit'] != 0:
            raise RuntimeError('first ownership call failed')
        result['verification'] = verify_layout(roots, account.pw_uid, gid)
        result['completed'] = True
    finally:
        result['unmounted'] = not mounted_copy
        if mounted_copy:
            unmount = subprocess.run(['umount', '--', str(mount)])
            result['unmounted'] = unmount.returncode == 0
        if result['unmounted']:
            mount.rmdir()
        (args.output / 'result.json').write_text(json.dumps(result, indent=2) + '\n')
        print('CRABBOX_FIRST_WALK_RESULT=' + json.dumps(result), flush=True)
        if not result['unmounted']:
            raise RuntimeError('copy unmount failed; authority termination must settle guest')


if __name__ == '__main__':
    signal.signal(signal.SIGTERM, lambda *_: (_ for _ in ()).throw(KeyboardInterrupt()))
    main()
