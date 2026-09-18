import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { readFile } from "node:fs/promises";
import test from "node:test";

test("persistent runner children leave the operation lock to the bootstrap parent", async () => {
  const bootstrap = await readFile(
    new URL("../images/koyeb-sandbox-runner/bootstrap.sh", import.meta.url),
    "utf8",
  );
  const startOwned = bootstrap.match(/^start_owned\(\) \{\n[\s\S]*?^\}/m)?.[0];
  assert.ok(startOwned, "bootstrap must define its persistent child launcher");

  // Execute the production launcher with real processes and flock(2). Python
  // acquires FD9 so this boundary also runs on macOS without util-linux flock.
  // Only Linux has /proc for the launcher's additional cmdline ownership check.
  const result = spawnSync("python3", ["-c", String.raw`
import fcntl
import os
from pathlib import Path
import select
import signal
import subprocess
import sys
import tempfile

start_owned = sys.stdin.read()
lock_and_exec = """
import fcntl, os, sys
fd = os.open(sys.argv[1], os.O_WRONLY | os.O_CREAT, 0o600)
os.dup2(fd, 9, inheritable=True)
if fd != 9:
    os.close(fd)
fcntl.flock(9, fcntl.LOCK_EX)
os.execv('/bin/bash', ['bash', '-c', sys.argv[2], 'bootstrap-lock-test', *sys.argv[3:]])
"""
shell = """
set -euo pipefail
state_root="$1"
runtime_root="$1"
fail() { printf '%s\\n' "$*" >&2; exit 2; }
""" + start_owned + """
start_owned worker "$3" "$2" -c 'import os; os.setsid(); os.execv("/bin/sleep", ["sleep", "60"])'
printf 'ready\\n'
read -r _
"""

with tempfile.TemporaryDirectory(prefix='crabbox-runner-lock-') as directory:
    lock_file = str(Path(directory) / 'operation.lock')
    pid_file = Path(directory) / 'worker.pid'
    expected = 'sleep 60' if sys.platform.startswith('linux') else ''
    parent = subprocess.Popen(
        [sys.executable, '-c', lock_and_exec, lock_file, shell, directory, sys.executable, expected],
        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        env={'PATH': '/usr/local/bin:/usr/bin:/bin'},
    )
    worker_pid = None

    def lock_available():
        with open(lock_file, 'a') as contender:
            try:
                fcntl.flock(contender, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                return False
            return True

    try:
        readable, _, _ = select.select([parent.stdout], [], [], 5)
        assert readable, 'bootstrap launcher did not become ready'
        assert parent.stdout.readline() == b'ready\n', 'bootstrap launcher failed before readiness'
        worker_pid = int(pid_file.read_text().strip())
        os.kill(worker_pid, 0)
        assert not lock_available(), 'child launch released the parent operation lock'
        print('parent retains exclusive operation lock while child runs', flush=True)

        parent.stdin.write(b'exit\n')
        parent.stdin.close()
        parent.wait(timeout=5)
        assert parent.returncode == 0, parent.stderr.read().decode()
        os.kill(worker_pid, 0)
        assert lock_available(), 'persistent child retained FD9: bootstrap replay cannot acquire the operation lock'
        print('replay lock boundary acquires after parent exits while child stays alive', flush=True)
        assert lock_available(), 'teardown cannot acquire the operation lock'
        print('teardown lock boundary independently acquires while child stays alive', flush=True)
    finally:
        if parent.poll() is None:
            parent.terminate()
            try:
                parent.wait(timeout=2)
            except subprocess.TimeoutExpired:
                parent.kill()
                parent.wait(timeout=2)
        if worker_pid is None and pid_file.exists():
            worker_pid = int(pid_file.read_text().strip())
        if worker_pid is not None:
            try:
                os.kill(worker_pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
`], {
    input: startOwned,
    encoding: "utf8",
    timeout: 15_000,
    env: { PATH: process.env.PATH },
  });
  assert.ifError(result.error);
  assert.equal(result.status, 0, `${result.stdout}\n${result.stderr}`);
});
