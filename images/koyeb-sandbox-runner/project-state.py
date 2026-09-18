#!/usr/bin/env python3
"""Fenced, bounded project filesystem recovery. No process or browser profile state."""
import base64
import ctypes
import errno
import fcntl
import hashlib
import json
import os
import pathlib
import re
import shutil
import stat
import sys
import tempfile
from project_dependencies import (DependencyError, assert_unchanged, capture_dependencies,
                                  valid_link_target, validate_symlink_graph, restore_dependencies)

SCHEMA = "crabbox-project-files/v1"
MAX_BYTES = 32 * 1024 * 1024
MAX_ENTRIES = 20000
PROFILE_PARTS = {"browser-profile", "worker-browser", "google-chrome", "chromium", ".mozilla"}


def digest(value):
    return hashlib.sha256(value).hexdigest()


def encode(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True).encode()


def safe_path(value):
    if not isinstance(value, str) or not value or len(value) > 4096 or value.strip() != value or any(ord(c) < 32 for c in value) or "\\" in value:
        raise ValueError("invalid project path")
    parts = value.split("/")
    if any(part in ("", ".", "..") or part in PROFILE_PARTS for part in parts):
        raise ValueError("invalid project path")
    if ".git" in parts:
        raise ValueError("repository metadata is not filesystem recovery data")
    return parts


def project_root(root, allowed_root):
    if not isinstance(root, str) or not isinstance(allowed_root, str):
        raise ValueError("invalid project root")
    path = pathlib.Path(root)
    prefix = pathlib.Path(allowed_root)
    if not path.is_absolute() or not prefix.is_absolute() or str(path) != root:
        raise ValueError("invalid project root")
    if path == prefix or prefix not in path.parents or any(part in PROFILE_PARTS for part in path.parts):
        raise ValueError("project root is outside its authorized parent")
    if str(path.resolve(strict=True)) != root or not path.is_dir():
        raise ValueError("project root must be a real directory")
    return path


def snapshot(root, excluded=frozenset(), dependency_entries=None, forbid_git=False):
    entries = []
    total = 0
    root_fd = os.open(root, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)

    def walk(directory, parent=""):
        nonlocal total
        for name in sorted(os.listdir(directory)):
            if name == ".git":
                if forbid_git:
                    raise ValueError("repository state cannot enter a clean runner home")
                continue
            relative = f"{parent}/{name}" if parent else name
            if relative in excluded:
                continue
            safe_path(relative)
            before = os.stat(name, dir_fd=directory, follow_symlinks=False)
            mode = stat.S_IMODE(before.st_mode) & 0o777
            if stat.S_ISDIR(before.st_mode):
                child = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=directory)
                try:
                    entries.append({"path": relative, "type": "directory", "mode": mode})
                    walk(child, relative)
                finally:
                    os.close(child)
            elif stat.S_ISREG(before.st_mode):
                if before.st_size > MAX_BYTES - total:
                    raise DependencyError("checkpoint_unpreserved_content_limit")
                fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=directory)
                try:
                    opened = os.fstat(fd)
                    if not stat.S_ISREG(opened.st_mode) or (opened.st_dev, opened.st_ino) != (before.st_dev, before.st_ino):
                        raise ValueError("project changed during checkpoint")
                    with os.fdopen(os.dup(fd), "rb") as stream:
                        data = stream.read(MAX_BYTES - total + 1)
                    after = os.fstat(fd)
                    if (before.st_mtime_ns, before.st_ctime_ns, before.st_size) != (after.st_mtime_ns, after.st_ctime_ns, after.st_size):
                        raise ValueError("project changed during checkpoint")
                finally:
                    os.close(fd)
                total += len(data)
                if total > MAX_BYTES:
                    raise DependencyError("checkpoint_unpreserved_content_limit")
                entries.append({"path": relative, "type": "file", "mode": mode,
                                "data": base64.b64encode(data).decode(), "sha256": digest(data)})
            elif stat.S_ISLNK(before.st_mode):
                target = os.readlink(name, dir_fd=directory)
                if not valid_link_target(relative, target):
                    raise ValueError("project symlink leaves checkpoint scope")
                entries.append({"path": relative, "type": "symlink", "mode": mode, "target": target})
            else:
                raise ValueError("project contains unsupported filesystem state")
            if len(entries) > MAX_ENTRIES:
                raise ValueError("project checkpoint exceeds entry limit")
    try:
        walk(root_fd)
    finally:
        os.close(root_fd)
    validate_symlink_graph({**(dependency_entries or {}), **{entry["path"]: entry for entry in entries}})
    return {"schema": SCHEMA, "entries": entries, "bytes": total, "gitMetadata": "excluded", "processState": "not-captured"}


def capture(root, dependency_policy="none", cache=None):
    proof = capture_dependencies(root, dependency_policy, cache)
    excluded = set(proof["recipe"]["omittedPaths"]) if proof else set()
    dependency_entries = proof["current"] if proof else None
    first = snapshot(root, excluded, dependency_entries)
    if encode(first) != encode(snapshot(root, excluded, dependency_entries)):
        raise ValueError("project changed during checkpoint")
    assert_unchanged(root, proof)
    first["exclusions"] = [proof["recipe"]] if proof else []
    payload = encode(first)
    return {"schema": SCHEMA, "sha256": digest(payload), "bytes": first["bytes"],
            "content": base64.b64encode(payload).decode(), "consistency": "stable-tree"}


def restore(root, content, expected_sha256, cache=None):
    payload = base64.b64decode(content, validate=True)
    if len(payload) > MAX_BYTES * 2 + MAX_ENTRIES * 512 or digest(payload) != expected_sha256:
        raise ValueError("project checkpoint digest mismatch")
    document = json.loads(payload)
    if document.get("schema") != SCHEMA or document.get("processState") != "not-captured" or document.get("gitMetadata") != "excluded":
        raise ValueError("invalid project checkpoint schema")
    entries = document.get("entries")
    if not isinstance(entries, list) or len(entries) > MAX_ENTRIES:
        raise ValueError("invalid project checkpoint entries")
    seen = set()
    directories = {""}
    total = 0
    validated = []
    for entry in entries:
        parts = safe_path(entry.get("path"))
        path = "/".join(parts)
        parent = "/".join(parts[:-1])
        if path in seen or parent not in directories:
            raise ValueError("invalid project checkpoint tree")
        seen.add(path)
        mode = entry.get("mode")
        if not isinstance(mode, int) or isinstance(mode, bool) or not 0 <= mode <= 0o777:
            raise ValueError("invalid project checkpoint permissions")
        kind = entry.get("type")
        data = None
        if kind == "directory":
            directories.add(path)
        elif kind == "file":
            data = base64.b64decode(entry.get("data", ""), validate=True)
            total += len(data)
            if total > MAX_BYTES or digest(data) != entry.get("sha256"):
                raise ValueError("invalid project checkpoint file")
        elif kind == "symlink":
            target = entry.get("target")
            if not valid_link_target(path, target):
                raise ValueError("invalid project checkpoint symlink")
        else:
            raise ValueError("invalid project checkpoint entry type")
        validated.append((entry, data))
    if total != document.get("bytes"):
        raise ValueError("invalid project checkpoint byte count")
    stage = pathlib.Path(tempfile.mkdtemp(prefix=".crabbox-restore-", dir=root.parent))
    try:
        dependency_entries = restore_dependencies(stage, document.get("exclusions", []), entries, cache)
        validate_symlink_graph({**dependency_entries, **{entry["path"]: entry for entry in entries}})
        for entry, data in validated:
            path = stage / entry["path"]
            if entry["type"] == "directory":
                path.mkdir(mode=0o700, exist_ok=True)
            elif entry["type"] == "file":
                if path.is_symlink() or path.is_file():
                    path.unlink()
                with path.open("xb") as stream:
                    stream.write(data)
                    stream.flush()
                    os.fsync(stream.fileno())
                path.chmod(entry["mode"])
            else:
                if path.is_symlink() or path.is_file():
                    path.unlink()
                path.symlink_to(entry["target"])
        # Preserve the fresh checkout's Git metadata, never another worker's absolute worktree link.
        git = root / ".git"
        if git.exists():
            if git.is_symlink() or not (git.is_dir() or git.is_file()):
                raise ValueError("restore requires independent repository metadata")
            if git.is_dir():
                shutil.copytree(git, stage / ".git", symlinks=True)
            else:
                shutil.copy2(git, stage / ".git", follow_symlinks=False)
        for entry, _ in reversed(validated):
            if entry["type"] == "directory":
                fd = os.open(stage / entry["path"], os.O_RDONLY | os.O_DIRECTORY)
                try:
                    os.fsync(fd)
                finally:
                    os.close(fd)
                (stage / entry["path"]).chmod(entry["mode"])
        stage.chmod(stat.S_IMODE(root.stat().st_mode))
        fd = os.open(stage, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
        if list(root.iterdir()):
            # Linux's atomic exchange leaves either the complete previous or complete restored tree.
            libc = ctypes.CDLL(None, use_errno=True)
            exchange = getattr(libc, "renameat2", None)
            if exchange is None or exchange(-100, os.fsencode(stage), -100, os.fsencode(root), 2) != 0:
                raise OSError(ctypes.get_errno() or errno.ENOTSUP, "atomic project restore unavailable")
        else:
            os.replace(stage, root)
        fd = os.open(root.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
    finally:
        if stage.exists():
            shutil.rmtree(stage)
    return {"schema": SCHEMA, "sha256": expected_sha256, "bytes": total, "recovery": "filesystem-only"}


def pool_baseline(state_root, user_home):
    path = pathlib.Path(state_root, "pool-baseline.json")
    if path.exists():
        return  # Never bless post-bootstrap edits after a restart.
    content = encode({"homeSHA256": digest(encode(snapshot(pathlib.Path(user_home), forbid_git=True)))})
    fd = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "wb") as output:
        output.write(content)
        output.flush()
        os.fsync(output.fileno())


def pool_clean(state_root, work_root, user_home, claim=None):
    state = pathlib.Path(state_root)
    if not state.is_dir() or state.is_symlink():
        raise ValueError("invalid runner state")
    lock = os.open(state / "pool.lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        marker = state / "pool-consumed.json"
        if marker.exists():
            # Replayed execution may observe completion, but can never make the runner clean again.
            previous = json.loads(marker.read_text())
            if claim and previous.get("claim") == claim:
                return {"schema": "crabbox-clean-runner/v1", "state": "claimed"}
            raise ValueError("runner has already been consumed")
        workspace = pathlib.Path(work_root)
        if workspace.is_symlink() or not workspace.is_dir() or list(workspace.iterdir()):
            raise ValueError("pool requires an empty project workspace")
        for relative in (".cache/crabbox/browser-profile", ".cache/crabbox/worker-browser", ".config/google-chrome", ".config/chromium", ".mozilla"):
            path = pathlib.Path(user_home, relative)
            if path.exists() or path.is_symlink():
                raise ValueError("browser profiles cannot enter the ready pool")
        baseline = json.loads((state / "pool-baseline.json").read_text())
        if digest(encode(snapshot(pathlib.Path(user_home), forbid_git=True))) != baseline.get("homeSHA256"):
            raise ValueError("runner home changed after clean bootstrap")
        if claim:
            if not isinstance(claim, str) or not re.fullmatch(r"[a-f0-9]{64}", claim):
                raise ValueError("invalid pool claim")
            fd = os.open(marker, os.O_CREAT | os.O_EXCL | os.O_WRONLY | os.O_NOFOLLOW, 0o600)
            with os.fdopen(fd, "w") as output:
                json.dump({"claim": claim}, output)
                output.flush()
                os.fsync(output.fileno())
            directory = os.open(state, os.O_RDONLY | os.O_DIRECTORY)
            try:
                os.fsync(directory)
            finally:
                os.close(directory)
        return {"schema": "crabbox-clean-runner/v1", "state": "claimed" if claim else "clean"}
    finally:
        os.close(lock)


def main():
    if sys.argv[1:] == ["--record-clean-baseline"]:
        if os.geteuid() != 0:
            raise ValueError("clean baseline requires bootstrap authority")
        pool_baseline("/var/lib/crabbox-koyeb", "/home/crabbox")
        return
    path = os.environ.pop("CRABBOX_PROJECT_STATE_REQUEST_PATH")
    if not re.fullmatch(r"/var/lib/crabbox-koyeb/project-request-[a-f0-9-]{36}\.json", path):
        raise ValueError("invalid project state request")
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or info.st_size > MAX_BYTES * 4:
            raise ValueError("invalid project state request")
        with os.fdopen(os.dup(fd), "r") as stream:
            request = json.load(stream)
    finally:
        os.close(fd)
        os.unlink(path)
    os.environ.pop("SANDBOX_SECRET", None)
    os.environ.pop("KOYEB_API_TOKEN", None)
    state = pathlib.Path("/var/lib/crabbox-koyeb")
    if request.get("leaseID") != (state / "lease-id").read_text().strip():
        raise ValueError("runner lease identity mismatch")
    action = request.get("action")
    if action in ("pool-check", "pool-claim"):
        result = pool_clean(state, "/workspace/crabbox", "/home/crabbox", request.get("claim") if action == "pool-claim" else None)
    elif action in ("capture", "restore"):
        import pwd
        user = pwd.getpwnam("crabbox")
        os.setgroups([])
        os.setgid(user.pw_gid)
        os.setuid(user.pw_uid)
        os.environ["HOME"] = user.pw_dir
        root = project_root(request.get("root"), request.get("allowedRoot"))
        result = capture(root, request.get("dependencyPolicy", "none")) if action == "capture" else restore(root, request["content"], request["sha256"])
    else:
        raise ValueError("invalid project state action")
    print(json.dumps(result, separators=(",", ":")))


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        # Never print project paths, content, credentials or raw exception messages.
        print(json.dumps({"error": error.code if isinstance(error, DependencyError) else "checkpoint_project_state_failed"}), file=sys.stderr)
        sys.exit(2)
