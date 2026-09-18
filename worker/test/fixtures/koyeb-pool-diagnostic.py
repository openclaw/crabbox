"""Test-container-only observer: fixed categories, no helper result changes."""
import errno
import hashlib
import hmac
import json
import os
import stat
import sys

HELPER = "/usr/local/libexec/crabbox-koyeb-sandbox/project-state.py"
EXPECTED_HELPER_SHA256 = "25af5e99345ff1d6f2e45ce51c646804c8f09f2fd1d29ebdf3f5b723ba4226f3"
OUTPUT = "/tmp/crabbox-pool-diagnostic/events.jsonl"
FUNCTIONS = {"main", "pool_baseline", "pool_clean", "snapshot", "walk", "safe_path"}
REASONS = {
    "invalid runner state": "invalid_state",
    "runner has already been consumed": "already_consumed",
    "pool requires an empty project workspace": "workspace_not_empty",
    "browser profiles cannot enter the ready pool": "browser_profile_present",
    "runner home changed after clean bootstrap": "home_changed",
    "invalid pool claim": "invalid_claim",
    "invalid project state request": "invalid_request",
    "runner lease identity mismatch": "lease_mismatch",
    "clean baseline requires bootstrap authority": "invalid_baseline_authority",
    "repository state cannot enter a clean runner home": "home_repository_present",
    "project changed during checkpoint": "home_changed_during_snapshot",
    "project symlink leaves checkpoint scope": "home_symlink_invalid",
    "project contains unsupported filesystem state": "home_unsupported_entry",
    "project checkpoint exceeds entry limit": "home_entry_limit",
    "invalid project path": "home_path_invalid",
}
EXCEPTIONS = {"ValueError": "value_error", "OSError": "os_error", "PermissionError": "permission_error",
              "FileNotFoundError": "not_found", "FileExistsError": "already_exists",
              "BlockingIOError": "lock_blocked", "NotADirectoryError": "not_directory",
              "IsADirectoryError": "is_directory", "JSONDecodeError": "invalid_json",
              "DependencyError": "dependency_error", "KeyError": "missing_key", "TypeError": "type_error"}
ERRNOS = {"EACCES", "EPERM", "ENOENT", "EEXIST", "EAGAIN", "EWOULDBLOCK", "EIO", "ENOSPC",
          "EROFS", "ELOOP", "ENOTDIR", "EISDIR", "EINTR", "EINVAL", "EBADF", "ENOTSUP"}
operation = "baseline" if sys.argv[1:] == ["--record-clean-baseline"] else "request"
emitted = 0
MAX_PROJECTION = 8 * 1024 * 1024
MAX_ENTRIES = 20000
MAX_GROUPS = 8
PATH_CLASSES = {"home_cache", "home_config", "home_local_share", "home_local_state",
                "home_top_level", "home_other", "none"}
EMPTY_SHA256 = hashlib.sha256(b"").hexdigest()
snapshot_result = None
snapshot_owner = None
pending_projection = None
baseline_failed = False


class ProjectionUnavailable(Exception):
    pass


def require_projection(ok, reason="projection_invalid"):
    if not ok:
        raise ProjectionUnavailable(reason)


def encoded(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True).encode()


def keyed(key, domain, value):
    return hmac.new(key, domain.encode() + b"\0" + encoded(value), hashlib.sha256).hexdigest()


def private_directory():
    fd = os.open(os.path.dirname(OUTPUT), os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        info = os.fstat(fd)
        # activate() is root-only. Matching euid also permits credential-free
        # synthetic trace tests without weakening the installed activation gate.
        require_projection(info.st_uid == os.geteuid() and stat.S_IMODE(info.st_mode) == 0o700)
        return fd
    except Exception:
        os.close(fd)
        raise


def private_info(fd, limit):
    info = os.fstat(fd)
    require_projection(stat.S_ISREG(info.st_mode) and info.st_uid == os.geteuid()
                       and stat.S_IMODE(info.st_mode) == 0o600 and info.st_nlink == 1)
    require_projection(info.st_size <= limit, "projection_overflow")
    return info


def read_private(directory, name, limit):
    try:
        fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=directory)
    except FileNotFoundError:
        raise ProjectionUnavailable("projection_missing") from None
    try:
        before = private_info(fd, limit)
        with os.fdopen(os.dup(fd), "rb") as source:
            data = source.read(limit + 1)
        require_projection(len(data) <= limit, "projection_overflow")
        after = private_info(fd, limit)
        require_projection((before.st_size, before.st_mtime_ns, before.st_ctime_ns)
                           == (after.st_size, after.st_mtime_ns, after.st_ctime_ns))
        return data
    finally:
        os.close(fd)


def write_private_once(directory, name, data, limit):
    require_projection(len(data) <= limit, "projection_overflow")
    try:
        fd = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                     0o600, dir_fd=directory)
    except FileExistsError:
        raise ProjectionUnavailable("projection_exists") from None
    try:
        private_info(fd, limit)
        with os.fdopen(os.dup(fd), "wb") as output:
            output.write(data)
            output.flush()
            os.fsync(output.fileno())
    finally:
        os.close(fd)
    # A partial/failed write is never repaired or overwritten on replay.


def projection_key(directory, create=False):
    try:
        key = read_private(directory, "home-key", 32)
    except ProjectionUnavailable as error:
        if not create or error.args != ("projection_missing",):
            raise
        key = os.urandom(32)
        write_private_once(directory, "home-key", key, 32)
    require_projection(len(key) == 32)
    return key


def path_class(path):
    for prefix, category in ((".cache", "home_cache"), (".config", "home_config"),
                             (".local/share", "home_local_share"), (".local/state", "home_local_state")):
        if path == prefix or path.startswith(prefix + "/"):
            return category
    return "home_other" if "/" in path else "home_top_level"


def hex_digest(value):
    return isinstance(value, str) and len(value) == 64 and all(c in "0123456789abcdef" for c in value)


def project_snapshot(document, key, baseline, reference):
    require_projection(hex_digest(baseline))
    require_projection(isinstance(document, dict) and set(document) ==
                       {"schema", "entries", "bytes", "gitMetadata", "processState"})
    entries = document["entries"]
    require_projection(isinstance(entries, list))
    require_projection(len(entries) <= MAX_ENTRIES, "projection_overflow")
    rows = []
    seen = set()
    for entry in entries:
        require_projection(isinstance(entry, dict))
        path, kind, mode = entry.get("path"), entry.get("type"), entry.get("mode")
        require_projection(isinstance(path, str) and 0 < len(path) <= 4096
                           and kind in ("directory", "file", "symlink")
                           and type(mode) is int and 0 <= mode <= 0o777)
        fields = {"path", "type", "mode"} | ({"data", "sha256"} if kind == "file" else
                                            {"target"} if kind == "symlink" else set())
        require_projection(set(entry) == fields)
        if kind == "file":
            require_projection(hex_digest(entry["sha256"]) and isinstance(entry["data"], str))
        if kind == "symlink":
            require_projection(isinstance(entry["target"], str) and len(entry["target"]) <= 4096)
        identity = keyed(key, "path", path)
        require_projection(identity not in seen)
        seen.add(identity)
        # The pinned snapshot computes sha256 from the same bytes it encodes in
        # data. Reuse that digest in memory; never reopen or rehash HOME content.
        rows.append([identity, path_class(path), keyed(key, "type", kind), keyed(key, "mode", mode),
                     keyed(key, "file_content", entry.get("sha256")),
                     keyed(key, "symlink_target", entry.get("target"))])
    result = {"v": 1, "s": EXPECTED_HELPER_SHA256, "i": keyed(key, "instance", None),
              "b": keyed(key, "baseline", baseline), "r": reference, "e": rows,
              "m": keyed(key, "top_level", {k: v for k, v in document.items() if k != "entries"})}
    result["a"] = keyed(key, "projection", result)
    require_projection(len(encoded(result)) <= MAX_PROJECTION, "projection_overflow")
    return result


def read_projection(directory, reference, key, baseline):
    document = json.loads(read_private(directory, "home-" + reference + ".json", MAX_PROJECTION))
    require_projection(isinstance(document, dict) and set(document) == {"v", "s", "i", "b", "r", "e", "m", "a"})
    require_projection(type(document["v"]) is int and document["v"] == 1
                       and document["s"] == EXPECTED_HELPER_SHA256 and document["r"] == reference)
    require_projection(all(hex_digest(document[k]) for k in ("i", "b", "m", "a")))
    require_projection(hmac.compare_digest(document["i"], keyed(key, "instance", None)))
    require_projection(hmac.compare_digest(document["a"], keyed(key, "projection",
                       {k: v for k, v in document.items() if k != "a"})))
    require_projection(hmac.compare_digest(document["b"], keyed(key, "baseline", baseline)),
                       "baseline_binding_changed")
    rows = document["e"]
    require_projection(isinstance(rows, list))
    require_projection(len(rows) <= MAX_ENTRIES, "projection_overflow")
    seen = set()
    for row in rows:
        require_projection(isinstance(row, list) and len(row) == 6)
        require_projection(isinstance(row[1], str) and row[1] in PATH_CLASSES - {"none"}
                           and all(hex_digest(row[k]) for k in (0, 2, 3, 4, 5)))
        require_projection(row[0] not in seen)
        seen.add(row[0])
    return document


def emit_reference(reference, reason):
    write_record({"event": "runner_home_reference", "operation": operation,
                  "reference": reference, "reason": reason})


def emit_home(reference, reason, category="none", field="none", count=0):
    bucket = "none" if count == 0 else "one" if count == 1 else "two_to_eight" if count <= 8 else "nine_or_more"
    write_record({"event": "runner_home_comparison", "operation": operation, "reference": reference,
                  "reason": reason, "pathClass": category, "field": field, "count": bucket})


def emit_home_entry(reference, previous, current, document):
    """Describe one added top-level entry without another HOME read."""
    if operation not in ("pool_check", "pool_claim"):
        return
    old = {row[0] for row in previous["e"]}
    added = [(row, entry) for row, entry in zip(current["e"], document["entries"])
             if row[0] not in old]
    if len(added) != 1:
        return
    _, entry = added[0]
    path, kind = entry["path"], entry["type"]
    if "/" in path:
        return
    entry_class = ({".local": "local_root", ".Xauthority": "x_authority_candidate",
                    ".ICEauthority": "ice_authority_candidate", "Desktop": "desktop_candidate",
                    ".dbus": "dbus_candidate"}.get(path, "other_top_level"))
    payload = ("empty" if kind == "file" and entry["data"] == ""
               and entry["sha256"] == EMPTY_SHA256 else
               "nonempty" if kind == "file" else "not_applicable")
    try:
        write_record({"schema": "crabbox-home-entry/v1", "event": "runner_home_entry",
                      "operation": operation, "reference": reference,
                      "field": "entry_added", "pathClass": "home_top_level", "count": "one",
                      "entryClass": entry_class, "kind": kind, "payload": payload})
    except Exception:
        # Supplemental evidence must never replace the native exception or the
        # aggregate comparison stream.
        pass


def compare_projection(previous, current, document, reference):
    old = {row[0]: row for row in previous["e"]}
    new = {row[0]: row for row in current["e"]}
    groups = {}

    def changed(category, field):
        pair = (category, field)
        groups[pair] = groups.get(pair, 0) + 1

    for identity in old.keys() - new.keys():
        changed(old[identity][1], "entry_removed")
    for identity in new.keys() - old.keys():
        changed(new[identity][1], "entry_added")
    for identity in old.keys() & new.keys():
        require_projection(old[identity][1] == new[identity][1])
        for index, field in ((2, "type"), (3, "mode"), (4, "file_content"), (5, "symlink_target")):
            if old[identity][index] != new[identity][index]:
                changed(new[identity][1], field)
    if old.keys() == new.keys() and [row[0] for row in previous["e"]] != [row[0] for row in current["e"]]:
        changed("none", "order")
    if previous["m"] != current["m"]:
        changed("none", "top_level")
    if not groups:
        emit_home(reference, "unclassified_snapshot_mismatch")
    for (category, field), count in sorted(groups.items())[:MAX_GROUPS]:
        emit_home(reference, "changed", category, field, count)
    if len(groups) > MAX_GROUPS:
        emit_home(reference, "groups_omitted", count=len(groups) - MAX_GROUPS)
    emit_home_entry(reference, previous, current, document)


def finish_projection():
    global pending_projection, snapshot_result, snapshot_owner
    pending, pending_projection = pending_projection, None
    snapshot_result, snapshot_owner = None, None
    if pending is None:
        return
    action, document, baseline = pending
    if action in ("bootstrap", "last_clean") and document is None:
        emit_reference(action, "snapshot_not_captured")
        return
    references = ("bootstrap", "last_clean") if action == "compare" else (action,)
    for reference in references:
        directory = None
        try:
            require_projection(document is not None, "projection_missing")
            directory = private_directory()
            key = projection_key(directory, create=action == "bootstrap")
            current = project_snapshot(document, key, baseline, reference)
            if action == "compare":
                compare_projection(read_projection(directory, reference, key, baseline), current, document, reference)
            else:
                write_private_once(directory, "home-" + reference + ".json", encoded(current), MAX_PROJECTION)
                emit_reference(reference, "projection_saved")
        except ProjectionUnavailable as error:
            emit_home(reference, error.args[0])
        except (ValueError, TypeError, KeyError):
            emit_home(reference, "projection_invalid")
        except Exception:
            emit_home(reference, "projection_error")
        finally:
            if directory is not None:
                os.close(directory)


def emit(stage, reason, exception="none", error_number=None):
    code = errno.errorcode.get(error_number) if isinstance(error_number, int) else None
    write_record({"operation": operation, "stage": stage, "reason": reason, "exception": exception,
                  "errno": code if code in ERRNOS else "none" if error_number is None else "other_errno"})


def write_record(record):
    global emitted
    try:
        if emitted >= 32:
            return
        emitted += 1
        fd = os.open(OUTPUT, os.O_WRONLY | os.O_APPEND | os.O_CREAT | os.O_NOFOLLOW, 0o600)
        try:
            os.write(fd, (json.dumps(record, separators=(",", ":")) + "\n").encode())
        finally:
            os.close(fd)
    except Exception:
        pass  # An observation failure must never replace the helper's result.


def stage_for(function, line):
    # Line mapping is enabled only for EXPECTED_HELPER_SHA256. No source paths,
    # exception text, home inventory/content hashes or request data are emitted.
    if function == "pool_clean":
        for upper, stage in ((253, "state_directory"), (254, "lock_open"), (256, "lock_acquire"),
                             (263, "consumed_marker"), (266, "workspace_empty"), (270, "browser_absent"),
                             (271, "baseline_read"), (272, "home_snapshot"), (273, "home_integrity"),
                             (276, "claim_format"), (280, "claim_marker_write"), (281, "claim_marker_sync"),
                             (286, "claim_directory_sync"), (287, "claim_complete"), (289, "lock_close")):
            if line <= upper:
                return stage
        return "pool_dispatch"
    if function == "pool_baseline":
        return "baseline_capture"
    if function in ("snapshot", "walk", "safe_path"):
        return "home_snapshot"
    for upper, stage in ((297, "baseline_request"), (300, "request_path"), (307, "request_read"),
                         (310, "request_remove"), (315, "lease_identity")):
        if line <= upper:
            return stage
    return "pool_dispatch"


def trace(frame, event, arg):
    global operation, snapshot_result, snapshot_owner, pending_projection, baseline_failed
    try:
        function = frame.f_code.co_name
        if frame.f_code.co_filename != HELPER or function not in FUNCTIONS:
            return None
        frame.f_trace_lines = False
        if event == "call":
            if function == "main":
                snapshot_result, snapshot_owner, pending_projection = None, None, None
                baseline_failed = False
            if function == "pool_baseline":
                operation = "baseline"
                emit("baseline_capture", "entered")
            elif function == "pool_clean":
                operation = "pool_claim" if frame.f_locals.get("claim") is not None else "pool_check"
                emit("pool_dispatch", "entered")
        elif event == "return":
            if function == "snapshot" and isinstance(arg, dict):
                caller = frame.f_back
                if caller and caller.f_code.co_filename == HELPER and caller.f_code.co_name in ("pool_baseline", "pool_clean"):
                    snapshot_result, snapshot_owner = arg, id(caller)
            elif function == "pool_baseline" and not baseline_failed:
                baseline = json.loads(frame.f_locals.get("content", b"null"))
                pending_projection = ("bootstrap", snapshot_result if snapshot_owner == id(frame) else None,
                                      baseline.get("homeSHA256") if isinstance(baseline, dict) else None)
            elif function == "pool_clean" and isinstance(arg, dict):
                state = arg.get("state")
                if state in ("clean", "claimed"):
                    emit("claim_complete", state)
                if state == "clean":
                    baseline = frame.f_locals.get("baseline")
                    pending_projection = ("last_clean", snapshot_result if snapshot_owner == id(frame) else None,
                                          baseline.get("homeSHA256") if isinstance(baseline, dict) else None)
            elif function == "main":
                # Native success/error records precede comparison. pool_clean's
                # finally has already closed its application lock at this point.
                finish_projection()
        elif event == "exception":
            if function == "main":
                request = frame.f_locals.get("request")
                action = request.get("action") if isinstance(request, dict) else None
                if action in ("capture", "restore"):
                    return None
                if action in ("pool-check", "pool-claim"):
                    operation = "pool_check" if action == "pool-check" else "pool_claim"
            elif operation == "request":
                return trace  # Do not observe project capture/restore internals.
            error = arg[1]
            if isinstance(error, (StopIteration, GeneratorExit)):
                return trace
            if function == "pool_baseline":
                baseline_failed = True
            reason = "unclassified_error"
            if type(error) is ValueError and error.args and isinstance(error.args[0], str):
                reason = REASONS.get(error.args[0], reason)
            elif type(error).__name__ == "DependencyError" and getattr(error, "code", None) == "checkpoint_unpreserved_content_limit":
                reason = "unpreserved_content_limit"
            emit(stage_for(function, frame.f_lineno), reason,
                 EXCEPTIONS.get(type(error).__name__, "other_exception"), getattr(error, "errno", None))
            if function == "pool_clean" and reason == "home_changed":
                baseline = frame.f_locals.get("baseline")
                pending_projection = ("compare", snapshot_result if snapshot_owner == id(frame) else None,
                                      baseline.get("homeSHA256") if isinstance(baseline, dict) else None)
        return trace
    except Exception:
        emit("observer", "observer_error")
        return trace


def activate():
    if sys.argv[0] != HELPER or os.geteuid() != 0:
        return
    try:
        with open(HELPER, "rb") as source:
            matches = hashlib.sha256(source.read()).hexdigest() == EXPECTED_HELPER_SHA256
        if not matches:
            emit("observer", "source_mismatch")
            return
        emit("observer", "ready")
        sys.settrace(trace)
    except Exception:
        emit("observer", "observer_error")


activate()
