"""Fixed public workload preparation, before snapshot capture; never a timing run.

There are no operator-selected URLs, archives, commands, versions or snapshots.
Run only in the disposable source guest under its ordinary numeric owner. The
qualification owner controls the enclosing cgroup, capture and final settlement.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import pwd
import signal
import stat
import subprocess
import tarfile
import tempfile
import time
import urllib.request

INPUTS = (
    ("source.tgz", "https://api.github.com/repositories/1103012935/tarball/8436c62514241a0afdef84bc2abb658dbdf57d40", 164272143,
     "ebaaa5f50f6b3ba54f825eaed7c602838e0a5eea936aee9b5212085591084dda"),
    ("node.tar.xz", "https://nodejs.org/dist/v24.22.1/node-v24.22.1-linux-x64.tar.xz", 32026916,
     "6c172a9ab6718f7b801fb5a7bd486af2ee5e32758086a04b84fcce2aa77095e6"),
    ("pnpm.tgz", "https://registry.npmjs.org/@pnpm/exe.linux-x64/-/exe.linux-x64-12.4.0.tgz", 21013550,
     "bcbd17511de1718a262be1acc42066559bbce8fcfb1adb584d1f5e8ad70fe218"),
)
SOURCE_COMMIT = "8436c62514241a0afdef84bc2abb658dbdf57d40"
LOCK_SHA256 = "0c50a34a1f4f78e7fa5da6340878f54da7f6b9f36a0c04c3f67c61ab8fbb7d06"
MAX_BYTES = 12 * 1024 ** 3
MAX_ENTRIES = 600_000
MAX_SECONDS = 900


def inventory(roots, *, max_bytes=MAX_BYTES, max_entries=MAX_ENTRIES):
    """Conservative block accounting includes hardlink aliases, without following links."""
    result = {"entries": 0, "allocatedBytes": 0}
    pending = list(roots)
    while pending:
        item = pending.pop()
        try:
            info = os.lstat(item)
        except FileNotFoundError:
            continue  # Installer can delete its own temporary entries.
        result["entries"] += 1
        result["allocatedBytes"] += info.st_blocks * 512
        if result["entries"] > max_entries or result["allocatedBytes"] > max_bytes:
            raise RuntimeError("workload preparation resource limit")
        if stat.S_ISDIR(info.st_mode):
            try:
                with os.scandir(item) as entries:
                    pending.extend(Path(entry.path) for entry in entries)
            except FileNotFoundError:
                pass
    return result


def download(spec, destination, deadline):
    name, url, size, expected = spec
    digest = hashlib.sha256()
    total = 0
    request = urllib.request.Request(url, headers={"User-Agent": "crabbox-fixed-qualification"})
    with urllib.request.urlopen(request, timeout=30) as response, destination.open("xb") as output:
        while True:
            if time.monotonic() >= deadline:
                raise RuntimeError("workload preparation deadline")
            chunk = response.read(1024 * 1024)
            if not chunk:
                break
            total += len(chunk)
            if total > size:
                raise RuntimeError("fixed input exceeded expected size")
            digest.update(chunk)
            output.write(chunk)
    if total != size or digest.hexdigest() != expected:
        raise RuntimeError("fixed input identity mismatch: " + name)


def extract(archive, destination, *, max_bytes=1024 ** 3, max_entries=MAX_ENTRIES):
    """Remove one archive wrapper and apply Python's data filter before each write."""
    total = 0
    count = 0
    prefix = None
    seen = set()
    with tarfile.open(archive) as source:
        for item in source:
            parts = PurePosixPath(item.name).parts
            if not parts or item.name.startswith("/") or ".." in parts:
                raise RuntimeError("noncanonical fixed archive path")
            if prefix is None:
                prefix = parts[0]
            if parts[0] != prefix:
                raise RuntimeError("fixed archive has more than one wrapper")
            parts = parts[1:]
            if not parts:
                if not item.isdir():
                    raise RuntimeError("fixed archive wrapper is not a directory")
                continue
            name = "/".join(parts)
            if name in seen or ".git" in parts:
                raise RuntimeError("duplicate or excluded fixed archive path")
            seen.add(name)
            if not (item.isfile() or item.isdir() or item.issym()):
                raise RuntimeError("fixed archive contains a special file or hardlink")
            count += 1
            total += item.size
            if count > max_entries or total > max_bytes:
                raise RuntimeError("fixed archive extraction limit")
            # Ignore archive ownership and metadata; the process is the target user.
            member = item.replace(name=name, uid=None, gid=None, uname=None, gname=None)
            source.extract(member, destination, filter="data")
    if count == 0:
        raise RuntimeError("empty fixed archive")


def install_command(pnpm, store):
    return [str(pnpm), "install", "--frozen-lockfile", "--ignore-scripts", "--ignore-pnpmfile",
            "--pm-on-fail=ignore", "--registry=https://registry.npmjs.org",
            "--store-dir=" + str(store), "--network-concurrency=8", "--child-concurrency=2"]


def monitored_install(command, cwd, env, roots, log, deadline):
    child = None
    try:
        with log.open("xb") as output:
            child = subprocess.Popen(command, cwd=cwd, env=env, start_new_session=True,
                                     stdin=subprocess.DEVNULL, stdout=output, stderr=output)
            while child.poll() is None:
                if time.monotonic() >= deadline:
                    raise RuntimeError("workload preparation deadline")
                inventory(roots)
                try:
                    child.wait(timeout=min(5, max(0.01, deadline - time.monotonic())))
                except subprocess.TimeoutExpired:
                    pass
            if child.returncode != 0:
                raise RuntimeError("fixed locked no-script install failed")
    finally:
        if child is not None:
            # The unreaped leader anchors this process group against PID reuse.
            # After leader exit, the enclosing systemd control group owns any
            # remaining descendants; never signal a potentially reused PGID.
            if child.poll() is None:
                try:
                    os.killpg(child.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
            child.wait()


def prepare(work_root):
    account = pwd.getpwnam("crabbox")
    if os.getuid() != account.pw_uid or os.getgid() != account.pw_gid:
        raise RuntimeError("preparation must run as the guest's ordinary owner")
    cache = Path("/var/cache/crabbox")
    if work_root.resolve() != work_root or cache.resolve() != cache:
        raise RuntimeError("work and cache roots must be canonical")
    if work_root.stat().st_dev != cache.stat().st_dev:
        raise RuntimeError("capture requires both roots on one filesystem")
    start = time.monotonic()
    deadline = start + MAX_SECONDS
    wrapper = work_root / "first-walk-workload"
    wrapper.mkdir()
    repository = wrapper / "repository"
    repository.mkdir()
    cache_wrapper = cache / "first-walk"
    cache_wrapper.mkdir()
    store = cache_wrapper / "store"
    # All download/tool/config/log files disappear before capture, including on error.
    with tempfile.TemporaryDirectory(prefix="crabbox-fixed-workload-") as directory:
        tools = Path(directory)
        for spec in INPUTS:
            download(spec, tools / spec[0], deadline)
        node = tools / "node"
        pnpm = tools / "pnpm"
        node.mkdir()
        pnpm.mkdir()
        extract(tools / "source.tgz", repository)
        extract(tools / "node.tar.xz", node)
        extract(tools / "pnpm.tgz", pnpm)
        if hashlib.sha256((repository / "pnpm-lock.yaml").read_bytes()).hexdigest() != LOCK_SHA256:
            raise RuntimeError("fixed lock identity mismatch")
        home = tools / "home"
        home.mkdir()
        env = {"PATH": str(node / "bin") + ":/usr/bin:/bin", "HOME": str(home),
               "XDG_CONFIG_HOME": str(tools / "config"), "CI": "true"}
        monitored_install(install_command(pnpm / "pnpm", store), repository, env,
                          [work_root, cache, tools], tools / "install.log", deadline)
        if not store.is_dir() or not any(store.iterdir()):
            raise RuntimeError("fixed package store is unpopulated")
        counts = inventory([work_root, cache])
    os.sync()
    return {"completed": True, "profile": "fixed-node-monorepo-v1", "sourceCommit": SOURCE_COMMIT,
            "sourceArchiveSha256": INPUTS[0][3], "lockSha256": LOCK_SHA256,
            "nodeSha256": INPUTS[1][3], "pnpmSha256": INPUTS[2][3],
            "scriptsDisabled": True, "hooksDisabled": True, "sameFilesystem": True,
            "cachePopulated": True, "inventory": counts, "elapsedSeconds": time.monotonic() - start,
            "meaning": "source preparation only; not first-walk timing"}


def cancelled(_number, _frame):
    raise RuntimeError("workload preparation cancelled")


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--work-root", required=True, type=Path)
    args = parser.parse_args()
    signal.signal(signal.SIGTERM, cancelled)
    signal.signal(signal.SIGINT, cancelled)
    signal.signal(signal.SIGALRM, cancelled)
    signal.alarm(MAX_SECONDS)
    print("CRABBOX_WORKLOAD_PREPARED=" + json.dumps(prepare(args.work_root), sort_keys=True))
