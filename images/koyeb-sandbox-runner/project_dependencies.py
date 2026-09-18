"""Explicit npm lockfile reconstruction, with every local deviation retained.

No arbitrary ignore list, lifecycle script, private registry or credential reuse.
Capture proves the baseline using the worker's existing cache. Restore recreates
that baseline from public integrity-pinned packages before applying user deltas.
"""
import base64
import hashlib
import json
import os
import pathlib
import posixpath
import shutil
import stat
import subprocess
import tempfile
import urllib.parse

POLICY = "npm-lockfile-v1"
MAX_INVENTORY_BYTES = 4 * 1024 * 1024 * 1024
MAX_INVENTORY_ENTRIES = 250000
DENIED = {".git", "browser-profile", "worker-browser", "google-chrome", "chromium", ".mozilla"}


class DependencyError(ValueError):
    def __init__(self, code):
        self.code = code
        super().__init__(code)


def encode(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True).encode()


def digest(value):
    return hashlib.sha256(value).hexdigest()


def path_or_ancestor_in(path, paths):
    candidate = path
    while True:
        if candidate in paths:
            return True
        parent = posixpath.dirname(candidate)
        if parent == candidate or not parent:
            return False
        candidate = parent


def valid_link_target(path, target):
    if not isinstance(target, str) or not target or target.strip() != target or len(target) > 4096 or target.startswith("/") or "\\" in target or any(ord(c) < 32 for c in target):
        return False
    resolved = posixpath.normpath(posixpath.join(posixpath.dirname(path), target))
    return resolved != ".." and not resolved.startswith("../") and not any(part in DENIED for part in resolved.split("/"))


def validate_symlink_graph(entries):
    """Resolve each segment through the actual graph, before applying any '..'.

    Lexical normalization alone is unsafe: dir/up -> .. followed by
    dir/up/../outside escapes the root despite a harmless normalized spelling.
    Missing targets may remain dangling; no link may escape, cycle, or exceed
    Linux's 40-link traversal bound. No filesystem link is followed here.
    """
    links = {path: entry["target"] for path, entry in entries.items() if entry["type"] == "symlink"}
    for path, target in links.items():
        expansions = 0

        def resolve(parts, tokens, active):
            nonlocal expansions
            parts = list(parts)
            for token in tokens:
                if token in ("", "."):
                    continue
                if token == "..":
                    if not parts:
                        raise DependencyError("checkpoint_symlink_escape")
                    parts.pop()
                    continue
                if token in DENIED:
                    raise DependencyError("checkpoint_symlink_escape")
                candidate = "/".join(parts + [token])
                if candidate in links:
                    expansions += 1
                    if candidate in active or expansions > 40:
                        raise DependencyError("checkpoint_symlink_cycle")
                    linked = links[candidate]
                    if not valid_link_target(candidate, linked):
                        raise DependencyError("checkpoint_symlink_escape")
                    parts = resolve(parts, linked.split("/"), active | {candidate})
                else:
                    parts.append(token)
            return parts

        if not valid_link_target(path, target):
            raise DependencyError("checkpoint_symlink_escape")
        resolve(path.split("/")[:-1], target.split("/"), {path})


def inventory(root):
    """Stream file digests, never allocate dependency bytes into the checkpoint."""
    result = {}
    total = 0
    fd = os.open(root, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)

    def walk(directory, parent="node_modules"):
        nonlocal total
        mode = stat.S_IMODE(os.fstat(directory).st_mode) & 0o777
        result[parent] = {"type": "directory", "mode": mode}
        for name in sorted(os.listdir(directory)):
            if name in DENIED or any(ord(c) < 32 for c in name) or "\\" in name:
                raise DependencyError("checkpoint_dependency_unsafe")
            path = parent + "/" + name
            before = os.stat(name, dir_fd=directory, follow_symlinks=False)
            mode = stat.S_IMODE(before.st_mode) & 0o777
            if stat.S_ISDIR(before.st_mode):
                child = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=directory)
                try:
                    walk(child, path)
                finally:
                    os.close(child)
            elif stat.S_ISREG(before.st_mode):
                total += before.st_size
                if total > MAX_INVENTORY_BYTES:
                    raise DependencyError("checkpoint_dependency_inventory_limit")
                file = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=directory)
                try:
                    opened = os.fstat(file)
                    if not stat.S_ISREG(opened.st_mode) or (opened.st_dev, opened.st_ino) != (before.st_dev, before.st_ino):
                        raise DependencyError("checkpoint_project_changed")
                    sha = hashlib.sha256()
                    count = 0
                    while True:
                        data = os.read(file, 1024 * 1024)
                        if not data:
                            break
                        count += len(data)
                        if count > before.st_size:
                            raise DependencyError("checkpoint_project_changed")
                        sha.update(data)
                    after = os.fstat(file)
                    if (before.st_mtime_ns, before.st_ctime_ns, before.st_size) != (after.st_mtime_ns, after.st_ctime_ns, count):
                        raise DependencyError("checkpoint_project_changed")
                    result[path] = {"type": "file", "mode": mode, "size": count, "sha256": sha.hexdigest()}
                finally:
                    os.close(file)
            elif stat.S_ISLNK(before.st_mode):
                target = os.readlink(name, dir_fd=directory)
                if not valid_link_target(path, target):
                    raise DependencyError("checkpoint_dependency_unsafe")
                result[path] = {"type": "symlink", "mode": mode, "target": target}
            else:
                raise DependencyError("checkpoint_dependency_unsafe")
            if len(result) > MAX_INVENTORY_ENTRIES:
                raise DependencyError("checkpoint_dependency_inventory_limit")
    try:
        walk(fd)
    finally:
        os.close(fd)
    validate_symlink_graph(result)
    return result, total


def read_inputs(root):
    inputs = {}
    for name in ("package.json", "package-lock.json"):
        file = os.open(root / name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        try:
            info = os.fstat(file)
            if not stat.S_ISREG(info.st_mode) or info.st_size > 8 * 1024 * 1024:
                raise DependencyError("checkpoint_dependency_recipe_unsupported")
            with os.fdopen(os.dup(file), "rb") as stream:
                inputs[name] = stream.read(8 * 1024 * 1024 + 1)
        finally:
            os.close(file)
    lock = json.loads(inputs["package-lock.json"])
    if lock.get("lockfileVersion") not in (2, 3) or not isinstance(lock.get("packages"), dict):
        raise DependencyError("checkpoint_dependency_recipe_unsupported")
    for path, package in lock["packages"].items():
        if path == "":
            continue
        url = urllib.parse.urlsplit(package.get("resolved", ""))
        integrity = package.get("integrity", "")
        if not path.startswith("node_modules/") or package.get("link") or url.scheme != "https" or url.netloc != "registry.npmjs.org" or not integrity.startswith(("sha512-", "sha256-")):
            raise DependencyError("checkpoint_dependency_recipe_unsupported")
    return inputs


def npm_environment(cache=None):
    return {"PATH": "/usr/local/bin:/usr/bin:/bin", "HOME": str(pathlib.Path.home()),
            "npm_config_cache": str(cache or pathlib.Path.home() / ".npm"),
            "npm_config_userconfig": "/dev/null", "npm_config_globalconfig": "/nonexistent/crabbox-checkpoint-global.npmrc",
            "npm_config_registry": "https://registry.npmjs.org/", "npm_config_ignore_scripts": "true"}


def rebuild(root, inputs, offline, expected_version=None, cache=None):
    env = npm_environment(cache)
    try:
        version = subprocess.run(["npm", "--version"], env=env, check=True, capture_output=True, timeout=10).stdout.decode().strip()
        if expected_version and version != expected_version:
            raise DependencyError("checkpoint_dependency_runtime_mismatch")
        for name, data in inputs.items():
            (root / name).write_bytes(data)
        result = subprocess.run(["npm", "ci", "--ignore-scripts", "--no-audit", "--no-fund", "--offline" if offline else "--prefer-offline"],
                                cwd=root, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=120)
        if result.returncode:
            raise DependencyError("checkpoint_dependency_rebuild_unavailable")
        return version
    except (OSError, subprocess.SubprocessError):
        raise DependencyError("checkpoint_dependency_rebuild_unavailable") from None


def selected_inputs(root):
    if not (root / ".git").exists():
        return set()
    commands = [["git", "-c", "core.fsmonitor=false", "-C", str(root), "ls-files", "--cached", "-z", "--", "node_modules"]]
    include = root / ".worktreeinclude"
    if include.exists():
        if include.is_symlink() or not include.is_file():
            raise DependencyError("checkpoint_dependency_input_policy_unsafe")
        commands.append(["git", "-c", "core.fsmonitor=false", "-C", str(root), "ls-files", "--others", "--ignored", "--exclude-from=.worktreeinclude", "-z", "--", "node_modules"])
    paths = set()
    for command in commands:
        try:
            result = subprocess.run(command, check=True, capture_output=True, timeout=20, env={"PATH": "/usr/local/bin:/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM": "1", "HOME": "/nonexistent"})
            if len(result.stdout) > 64 * 1024 * 1024:
                raise DependencyError("checkpoint_dependency_inventory_limit")
            paths.update(value.decode() for value in result.stdout.split(b"\0") if value)
        except (OSError, UnicodeError, subprocess.SubprocessError):
            raise DependencyError("checkpoint_dependency_input_policy_unsafe") from None
    return paths


def capture_dependencies(root, policy, cache=None):
    if policy == "none" or not (root / "node_modules").exists():
        return None
    if policy != POLICY:
        raise DependencyError("checkpoint_dependency_policy_unsupported")
    try:
        inputs = read_inputs(root)
    except (OSError, ValueError, TypeError, AttributeError) as error:
        if isinstance(error, DependencyError):
            raise
        raise DependencyError("checkpoint_dependency_recipe_unsupported") from None
    with tempfile.TemporaryDirectory(prefix=".crabbox-dependency-proof-", dir=root.parent) as directory:
        scratch = pathlib.Path(directory)
        version = rebuild(scratch, inputs, offline=True, cache=cache)
        baseline, baseline_bytes = inventory(scratch / "node_modules")
    current, _ = inventory(root / "node_modules")
    preserved = selected_inputs(root)
    omitted = {path for path, entry in current.items() if baseline.get(path) == entry and path not in preserved}
    # A directory may be omitted only when all descendants match reproducible data.
    for path in sorted(current, key=lambda p: (p.count("/"), p), reverse=True):
        if path not in omitted:
            parent = posixpath.dirname(path)
            while parent and parent != ".":
                omitted.discard(parent)
                parent = posixpath.dirname(parent)
    skip = sorted(path for path in omitted if posixpath.dirname(path) not in omitted)
    removed = sorted(path for path in baseline if path not in current or baseline[path]["type"] != current[path]["type"])
    removed_set = set(removed)
    removed = [path for path in removed if not path_or_ancestor_in(posixpath.dirname(path), removed_set)]
    recipe = {"policy": POLICY, "path": "node_modules", "recipe": "npm-ci-ignore-scripts/v1", "npmVersion": version,
              "inputs": {name: digest(data) for name, data in inputs.items()}, "treeSHA256": digest(encode(baseline)),
              "entries": len(baseline), "bytes": baseline_bytes, "omittedPaths": skip, "removedPaths": removed,
              "symlinks": {path: entry["target"] for path, entry in baseline.items() if entry["type"] == "symlink"}}
    return {"recipe": recipe, "current": current, "inputs": inputs}


def assert_unchanged(root, proof):
    if not proof:
        return
    current, _ = inventory(root / "node_modules")
    if current != proof["current"] or read_inputs(root) != proof["inputs"]:
        raise DependencyError("checkpoint_project_changed")


def restore_dependencies(stage, recipes, entries, cache=None):
    if not recipes:
        return {}
    if len(recipes) != 1:
        raise DependencyError("checkpoint_dependency_policy_unsupported")
    recipe = recipes[0]
    if recipe.get("policy") != POLICY or recipe.get("path") != "node_modules" or recipe.get("recipe") != "npm-ci-ignore-scripts/v1":
        raise DependencyError("checkpoint_dependency_policy_unsupported")
    files = {entry["path"]: entry for entry in entries if entry["type"] == "file"}
    try:
        inputs = {name: base64.b64decode(files[name]["data"], validate=True) for name in ("package.json", "package-lock.json")}
    except (KeyError, ValueError):
        raise DependencyError("checkpoint_dependency_recipe_corrupt") from None
    if {name: digest(data) for name, data in inputs.items()} != recipe["inputs"]:
        raise DependencyError("checkpoint_dependency_recipe_corrupt")
    with tempfile.TemporaryDirectory(prefix=".crabbox-dependency-restore-", dir=stage.parent) as directory:
        scratch = pathlib.Path(directory)
        # Validate the recorded public, integrity-pinned recipe before allowing npm network access.
        for name, data in inputs.items():
            (scratch / name).write_bytes(data)
        read_inputs(scratch)
        rebuild(scratch, inputs, offline=False, expected_version=recipe["npmVersion"], cache=cache)
        baseline, total = inventory(scratch / "node_modules")
        if digest(encode(baseline)) != recipe["treeSHA256"] or len(baseline) != recipe["entries"] or total != recipe["bytes"]:
            raise DependencyError("checkpoint_dependency_rebuild_mismatch")
        if recipe.get("symlinks") != {path: entry["target"] for path, entry in baseline.items() if entry["type"] == "symlink"}:
            raise DependencyError("checkpoint_dependency_recipe_corrupt")
        if not isinstance(recipe.get("removedPaths"), list) or any(path == "node_modules" or path not in baseline for path in recipe["removedPaths"]):
            raise DependencyError("checkpoint_dependency_recipe_corrupt")
        os.replace(scratch / "node_modules", stage / "node_modules")
    for path in recipe["removedPaths"]:
        if not isinstance(path, str) or not path.startswith("node_modules/") or posixpath.normpath(path) != path or "\\" in path:
            raise DependencyError("checkpoint_dependency_recipe_corrupt")
        target = stage / path
        if target.is_symlink() or target.is_file():
            target.unlink()
        elif target.is_dir():
            shutil.rmtree(target)
    removed = set(recipe["removedPaths"])
    return {path: entry for path, entry in baseline.items()
            if not path_or_ancestor_in(path, removed)}
