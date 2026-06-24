import importlib
import json
import os
import sys
import traceback


def emit(value, code=0):
    sys.stdout.write(json.dumps(value, separators=(",", ":")))
    sys.stdout.flush()
    raise SystemExit(code)


def check(status, name, message="", klass="", details=None):
    item = {"status": status, "check": name}
    if message:
        item["message"] = message
    if klass:
        item["class"] = klass
    if details:
        item["details"] = details
    return item


def import_sdk(preferred, fallback):
    errors = []
    for path in [preferred, fallback]:
        path = str(path or "").strip()
        if not path:
            continue
        try:
            mod = importlib.import_module(path)
            return mod, path, ""
        except Exception as exc:
            errors.append(f"{path}: {exc.__class__.__name__}: {exc}")
    return None, "", "; ".join(errors)


def version_tuple():
    return (sys.version_info.major, sys.version_info.minor, sys.version_info.micro)


def python_supported(import_path):
    major, minor, _ = version_tuple()
    if import_path == "cua_sandbox":
        return (major, minor) >= (3, 11), "3.11+"
    return (major, minor) >= (3, 12), "3.12+"


def sdk_version(mod):
    return str(getattr(mod, "__version__", "") or "")


def sdk_configure(mod, cfg):
    configure = getattr(mod, "configure", None)
    if callable(configure):
        base_url = cfg.get("apiUrl") or os.environ.get("CUA_BASE_URL") or ""
        if base_url:
            try:
                configure(base_url=base_url)
            except TypeError:
                configure(api_url=base_url)


def sandbox_class(mod):
    sandbox = getattr(mod, "Sandbox", None)
    if sandbox is None:
        sandbox_mod = getattr(mod, "sandbox", None)
        sandbox = getattr(sandbox_mod, "Sandbox", None)
    return sandbox


def summarize_sandbox(value):
    if isinstance(value, dict):
        data = value
    else:
        data = {}
        for key in ["id", "sandbox_id", "name", "status", "state", "metadata", "tags"]:
            if hasattr(value, key):
                data[key] = getattr(value, key)
    return {
        "id": str(data.get("id") or data.get("sandbox_id") or data.get("name") or ""),
        "name": str(data.get("name") or data.get("id") or data.get("sandbox_id") or ""),
        "status": str(data.get("status") or data.get("state") or ""),
        "state": str(data.get("state") or data.get("status") or ""),
        "metadata": data.get("metadata") or data.get("tags") or {},
    }


def auth_state():
    if os.environ.get("CUA_API_KEY"):
        return "env"
    return "credential_store_or_missing"


def doctor(req, mod, import_path, import_error):
    cfg = req.get("config") or {}
    checks = []
    pyver = ".".join(str(v) for v in version_tuple())
    details = {
        "mutation": "false",
        "sdkPackage": str(cfg.get("sdkPackage") or os.environ.get("CRABBOX_CUA_SDK_PACKAGE") or ""),
    }
    if not mod:
        checks.append(check("failed", "sdk", f"CUA SDK import failed: {import_error}", "environment_blocked"))
        return {
            "ok": False,
            "class": "environment_blocked",
            "doctor": {"pythonVersion": pyver, "auth": auth_state(), "baseUrl": str(cfg.get("apiUrl") or ""), "checks": checks, "details": details},
        }
    supported, required = python_supported(import_path)
    if supported:
        checks.append(check("ok", "python", f"python={pyver} required={required}", details={"required": required}))
    else:
        checks.append(check("failed", "python", f"python={pyver} required={required}", "environment_blocked", {"required": required}))
    checks.append(check("ok", "sdk", f"import={import_path}", details={"import": import_path, "version": sdk_version(mod)}))
    auth = auth_state()
    if auth == "env":
        checks.append(check("ok", "auth", "auth=env mutation=false", details={"source": "env"}))
    else:
        checks.append(check("failed", "auth", "auth=missing_or_credential_store_unverified mutation=false", "environment_blocked", {"source": "credential_store_or_missing"}))
    if cfg.get("apiUrl"):
        checks.append(check("ok", "api_url", "api_url=configured mutation=false", details={"baseUrl": str(cfg.get("apiUrl"))}))
    else:
        checks.append(check("ok", "api_url", "api_url=sdk_default mutation=false"))
    ok = all(item["status"] != "failed" for item in checks)
    return {
        "ok": ok,
        "class": "" if ok else "environment_blocked",
        "doctor": {
            "pythonVersion": pyver,
            "importPath": import_path,
            "sdkVersion": sdk_version(mod),
            "auth": auth,
            "baseUrl": str(cfg.get("apiUrl") or os.environ.get("CUA_BASE_URL") or ""),
            "checks": checks,
            "details": details,
        },
    }


def list_sandboxes(mod):
    cls = sandbox_class(mod)
    if cls is None or not hasattr(cls, "list"):
        return {"ok": False, "class": "environment_blocked", "error": {"code": "sdk_missing_list", "message": "CUA SDK Sandbox.list is unavailable", "class": "environment_blocked"}}
    values = cls.list()
    return {"ok": True, "sandboxes": [summarize_sandbox(v) for v in (values or [])]}


def info(req, mod):
    cls = sandbox_class(mod)
    sid = str(req.get("sandboxId") or "").strip()
    if not sid:
        return {"ok": False, "class": "validation_failed", "error": {"code": "missing_sandbox_id", "message": "sandboxId is required", "class": "validation_failed"}}
    if cls is None:
        return {"ok": False, "class": "environment_blocked", "error": {"code": "sdk_missing_sandbox", "message": "CUA SDK Sandbox is unavailable", "class": "environment_blocked"}}
    if hasattr(cls, "get_info"):
        value = cls.get_info(sid)
    elif hasattr(cls, "connect"):
        value = cls.connect(sid).get_info()
    else:
        return {"ok": False, "class": "environment_blocked", "error": {"code": "sdk_missing_info", "message": "CUA SDK info operation is unavailable", "class": "environment_blocked"}}
    return {"ok": True, "sandbox": summarize_sandbox(value)}


def unsupported(action):
    return {"ok": False, "class": "diagnostic_only", "error": {"code": "action_deferred", "message": f"CUA bridge action {action} is deferred to lifecycle implementation", "class": "diagnostic_only"}}


def main():
    try:
        req = json.load(sys.stdin)
        cfg = req.get("config") or {}
        preferred = cfg.get("sdkImport") or os.environ.get("CRABBOX_CUA_SDK_IMPORT") or "cua"
        fallback = cfg.get("fallbackImport") or os.environ.get("CRABBOX_CUA_SDK_FALLBACK_IMPORT") or "cua_sandbox"
        mod, import_path, import_error = import_sdk(preferred, fallback)
        if mod is not None:
            sdk_configure(mod, cfg)
        action = str(req.get("action") or "").strip()
        if action == "doctor":
            emit(doctor(req, mod, import_path, import_error))
        if mod is None:
            emit({"ok": False, "class": "environment_blocked", "error": {"code": "sdk_import_failed", "message": import_error, "class": "environment_blocked"}})
        if action == "list":
            emit(list_sandboxes(mod))
        if action == "info":
            emit(info(req, mod))
        if action in {"create", "delete", "upload_bytes", "exec"}:
            emit(unsupported(action))
        emit({"ok": False, "class": "validation_failed", "error": {"code": "unknown_action", "message": f"unknown CUA bridge action {action}", "class": "validation_failed"}})
    except SystemExit:
        raise
    except Exception as exc:
        emit({"ok": False, "class": "environment_blocked", "error": {"code": exc.__class__.__name__, "message": str(exc), "class": "environment_blocked"}, "stderr": traceback.format_exc()}, 0)


if __name__ == "__main__":
    main()
