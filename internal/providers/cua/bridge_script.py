import importlib
import asyncio
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
        return (3, 11) <= (major, minor) < (3, 14), ">=3.11,<3.14"
    return (3, 12) <= (major, minor) < (3, 14), ">=3.12,<3.14"


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
        for key in ["id", "sandbox_id", "name", "status", "state", "metadata", "tags", "created_at", "source", "os_type"]:
            if hasattr(value, key):
                data[key] = getattr(value, key)
    metadata = dict(data.get("metadata") or data.get("tags") or {})
    for source, target in [("created_at", "createdAt"), ("source", "source"), ("os_type", "osType")]:
        if data.get(source) is not None:
            metadata[target] = str(data.get(source))
    return {
        "id": str(data.get("id") or data.get("sandbox_id") or data.get("name") or ""),
        "name": str(data.get("name") or data.get("id") or data.get("sandbox_id") or ""),
        "status": str(data.get("status") or data.get("state") or ""),
        "state": str(data.get("state") or data.get("status") or ""),
        "osType": str(data.get("os_type") or metadata.get("osType") or ""),
        "metadata": metadata,
    }


def exception_http_status(exc):
    values = [getattr(exc, "status_code", None), getattr(exc, "status", None)]
    response = getattr(exc, "response", None)
    if response is not None:
        values.extend([getattr(response, "status_code", None), getattr(response, "status", None)])
    for value in values:
        try:
            if value is not None:
                return int(value)
        except (TypeError, ValueError):
            continue
    return None


def error_response(exc, klass="environment_blocked"):
    code = exc.__class__.__name__
    message = str(exc)
    lower = message.lower()
    normalized_code = code.lower()
    if exception_http_status(exc) == 404 or normalized_code in {"notfound", "notfounderror", "sandboxnotfounderror"}:
        klass = "not_found"
    elif "quota" in lower or "capacity" in lower or "rate limit" in lower or "429" in lower:
        klass = "quota_blocked"
    elif "unauthorized" in lower or "forbidden" in lower or "api key" in lower:
        klass = "environment_blocked"
    return {"ok": False, "class": klass, "error": {"code": code, "message": message, "class": klass}}


def auth_state():
    if os.environ.get("CUA_API_KEY"):
        return "env"
    return "missing"


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
        inventory = list_sandboxes(mod)
        if inventory.get("ok"):
            count = len(inventory.get("sandboxes") or [])
            checks.append(check("ok", "auth", "auth=verified_read_only mutation=false", details={"source": "env"}))
            checks.append(check("ok", "inventory", f"inventory=readable count={count} mutation=false", details={"count": str(count)}))
        else:
            error = inventory.get("error") or {}
            message = str(error.get("message") or "CUA inventory probe failed")
            klass = str(error.get("class") or inventory.get("class") or "environment_blocked")
            checks.append(check("failed", "auth", f"read-only inventory probe failed: {message}", klass, {"source": "env"}))
    else:
        checks.append(check("failed", "auth", "auth=missing mutation=false", "environment_blocked", {"source": "missing"}))
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
    async def _run():
        cls = sandbox_class(mod)
        if cls is None or not hasattr(cls, "list"):
            return {"ok": False, "class": "environment_blocked", "error": {"code": "sdk_missing_list", "message": "CUA SDK Sandbox.list is unavailable", "class": "environment_blocked"}}
        try:
            values = await cls.list()
            return {"ok": True, "sandboxes": [summarize_sandbox(v) for v in (values or [])]}
        except Exception as exc:
            return error_response(exc)

    return asyncio.run(_run())


def info(req, mod):
    async def _run():
        cls = sandbox_class(mod)
        sid = str(req.get("sandboxId") or "").strip()
        if not sid:
            return {"ok": False, "class": "validation_failed", "error": {"code": "missing_sandbox_id", "message": "sandboxId is required", "class": "validation_failed"}}
        if cls is None:
            return {"ok": False, "class": "environment_blocked", "error": {"code": "sdk_missing_sandbox", "message": "CUA SDK Sandbox is unavailable", "class": "environment_blocked"}}
        try:
            if hasattr(cls, "get_info"):
                value = await cls.get_info(sid)
            elif hasattr(cls, "connect"):
                sb = await cls.connect(sid)
                try:
                    value = {"id": sid, "name": getattr(sb, "name", sid), "status": "running", "state": "running"}
                finally:
                    await sb.disconnect()
            else:
                return {"ok": False, "class": "environment_blocked", "error": {"code": "sdk_missing_info", "message": "CUA SDK info operation is unavailable", "class": "environment_blocked"}}
        except Exception as exc:
            return error_response(exc)
        return {"ok": True, "sandbox": summarize_sandbox(value)}

    return asyncio.run(_run())


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
        if action == "create":
            emit({"ok": False, "class": "unsupported", "error": {"code": "provisioning_disabled", "message": "CUA provisioning is disabled until create supports an idempotency key or client-assigned identity echoed by create/list/get; tracking: https://github.com/openclaw/crabbox/issues/381", "class": "unsupported"}})
        if action == "delete":
            emit({"ok": False, "class": "unsupported", "error": {"code": "deletion_disabled", "message": "CUA deletion is disabled until it can atomically target an immutable sandbox identity; tracking: https://github.com/openclaw/crabbox/issues/381", "class": "unsupported"}})
        emit({"ok": False, "class": "validation_failed", "error": {"code": "unknown_action", "message": f"unknown CUA bridge action {action}", "class": "validation_failed"}})
    except SystemExit:
        raise
    except Exception as exc:
        emit({"ok": False, "class": "environment_blocked", "error": {"code": exc.__class__.__name__, "message": str(exc), "class": "environment_blocked"}, "stderr": traceback.format_exc()}, 0)


if __name__ == "__main__":
    main()
