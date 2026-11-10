# Resolve the original user:same-named-group contract to numeric IDs once.
# Keep traversal and updates anchored to directory descriptors; never follow symlinks.
# The fixed argv label lets read-only observers distinguish this child from other Python.
PATH=/usr/sbin:/usr/bin:/sbin:/bin python3 -I - crabbox-ownership-repair-v1 {{user}} {{roots}} <<'CRABBOX_OWNERSHIP'
import grp
import os
import pwd
import resource
import stat
import sys
import time

uid = pwd.getpwnam(sys.argv[2]).pw_uid
gid = grp.getgrnam(sys.argv[2]).gr_gid
# systemd normally starts services with a 1024 soft limit. This child uses
# descriptors instead of following mutable ancestor paths; allow its inherited
# hard limit without changing the bootstrap shell or any system-wide limit.
soft_limit, hard_limit = resource.getrlimit(resource.RLIMIT_NOFILE)
if soft_limit != hard_limit:
    resource.setrlimit(resource.RLIMIT_NOFILE, (hard_limit, hard_limit))

visited = mismatched = changed = 0

def repair(name, directory_fd):
    global visited, mismatched, changed
    info = os.stat(name, dir_fd=directory_fd, follow_symlinks=False)
    visited += 1
    if info.st_uid != uid or info.st_gid != gid:
        mismatched += 1
        os.chown(name, uid, gid, dir_fd=directory_fd, follow_symlinks=False)
        changed += 1
    return stat.S_ISDIR(info.st_mode)

def descend(name, parent_fd):
    directory_fd = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent_fd)
    try:
        return directory_fd, os.scandir(directory_fd)
    except BaseException:
        os.close(directory_fd)
        raise

started = time.monotonic()
state = "failed"
print("crabbox ownership-repair state=started", flush=True)
try:
    for root in sys.argv[3:]:
        parent, name = os.path.split(root)
        parent_fd = os.open(parent or ".", os.O_RDONLY | os.O_DIRECTORY)
        stack = []
        try:
            if repair(name or ".", parent_fd):
                stack.append(descend(name or ".", parent_fd))
            # An explicit stack also supports trees deeper than Python's recursion limit.
            while stack:
                directory_fd, iterator = stack[-1]
                entry = next(iterator, None)
                if entry is None:
                    iterator.close()
                    os.close(directory_fd)
                    stack.pop()
                elif repair(entry.name, directory_fd):
                    stack.append(descend(entry.name, directory_fd))
        finally:
            for directory_fd, iterator in reversed(stack):
                iterator.close()
                os.close(directory_fd)
            os.close(parent_fd)
    state = "complete"
finally:
    # Counts come from this walk, without another inventory or persistent readiness state.
    print(f"crabbox ownership-repair state={state} visited={visited} mismatched={mismatched} changed={changed} elapsed_seconds={time.monotonic() - started:.3f}", flush=True)
CRABBOX_OWNERSHIP
