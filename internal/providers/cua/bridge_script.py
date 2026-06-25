import importlib
import asyncio
import base64
import inspect
import json
import os
import re
import shlex
import sys
import traceback
import uuid


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


def image_for_config(mod, cfg):
    image_cls = getattr(mod, "Image", None)
    if image_cls is None:
        image_cls = importlib.import_module("cua_sandbox").Image
    raw = str(cfg.get("image") or "ubuntu:24.04").strip()
    kind = str(cfg.get("kind") or "container").strip().lower() or "container"
    if raw.startswith("ubuntu:"):
        return image_cls.linux("ubuntu", raw.split(":", 1)[1] or "24.04", kind=kind)
    if raw == "ubuntu":
        return image_cls.linux("ubuntu", "24.04", kind=kind)
    if hasattr(image_cls, "from_registry"):
        img = image_cls.from_registry(raw)
        if getattr(img, "kind", None) is None:
            img = image_cls.from_dict({**img.to_dict(), "kind": kind})
        return img
    return image_cls.linux("ubuntu", "24.04", kind=kind)


def sandbox_kwargs(cfg, create_func=None):
    kwargs = {"local": False}
    if cfg.get("region"):
        kwargs["region"] = str(cfg.get("region"))
    if int(cfg.get("vcpus") or 0) > 0:
        kwargs["cpu"] = int(cfg.get("vcpus"))
    if int(cfg.get("memoryMb") or 0) > 0:
        kwargs["memory_mb"] = int(cfg.get("memoryMb"))
    if int(cfg.get("diskGb") or 0) > 0:
        kwargs["disk_gb"] = int(cfg.get("diskGb"))
    parameters = {}
    if create_func is not None:
        try:
            parameters = inspect.signature(create_func).parameters
        except (TypeError, ValueError):
            parameters = {}
    if int(cfg.get("startupTimeoutSecs") or 0) > 0 and "time_to_start" in parameters:
        kwargs["time_to_start"] = int(cfg.get("startupTimeoutSecs"))
    return kwargs


def command_text(argv):
    values = [str(v) for v in (argv or [])]
    if not values:
        return ""
    return shlex.join(values)


def valid_env_name(name):
    return re.match(r"^[A-Za-z_][A-Za-z0-9_]*$", name or "") is not None


def env_file_content(env):
    return "".join(f"export {k}={shlex.quote(v)}\n" for k, v in sorted(env.items())).encode("utf-8")


def exec_script(command, workdir="", env_file_path=""):
    script = ""
    if env_file_path:
        script += "env_file=" + shlex.quote(env_file_path) + "; trap 'rm -f \"$env_file\"' EXIT; . \"$env_file\" && "
    if workdir:
        script += "cd " + shlex.quote(workdir) + " && "
    return script + command


async def cleanup_env_file(sb, path):
    remove = getattr(getattr(sb, "files", None), "remove", None)
    if callable(remove):
        await remove(path)
        return
    await sb.shell.run("rm -f " + shlex.quote(path), timeout=10)


def error_response(exc, klass="environment_blocked"):
    code = exc.__class__.__name__
    message = str(exc)
    lower = message.lower()
    if "not found" in lower or "404" in lower:
        klass = "not_found"
    elif "quota" in lower or "capacity" in lower or "rate limit" in lower or "429" in lower:
        klass = "quota_blocked"
    elif "unauthorized" in lower or "forbidden" in lower or "api key" in lower:
        klass = "environment_blocked"
    return {"ok": False, "class": klass, "error": {"code": code, "message": message, "class": klass}}


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
    async def _run():
        cls = sandbox_class(mod)
        if cls is None or not hasattr(cls, "list"):
            return {"ok": False, "class": "environment_blocked", "error": {"code": "sdk_missing_list", "message": "CUA SDK Sandbox.list is unavailable", "class": "environment_blocked"}}
        values = await cls.list()
        return {"ok": True, "sandboxes": [summarize_sandbox(v) for v in (values or [])]}

    return asyncio.run(_run())


def info(req, mod):
    async def _run():
        cls = sandbox_class(mod)
        sid = str(req.get("sandboxId") or "").strip()
        if not sid:
            return {"ok": False, "class": "validation_failed", "error": {"code": "missing_sandbox_id", "message": "sandboxId is required", "class": "validation_failed"}}
        if cls is None:
            return {"ok": False, "class": "environment_blocked", "error": {"code": "sdk_missing_sandbox", "message": "CUA SDK Sandbox is unavailable", "class": "environment_blocked"}}
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
        return {"ok": True, "sandbox": summarize_sandbox(value)}

    return asyncio.run(_run())


def create(req, mod):
    async def _run():
        cls = sandbox_class(mod)
        if cls is None or not hasattr(cls, "create"):
            return {"ok": False, "class": "environment_blocked", "error": {"code": "sdk_missing_create", "message": "CUA SDK Sandbox.create is unavailable", "class": "environment_blocked"}}
        cfg = req.get("config") or {}
        name = str((req.get("sandbox") or {}).get("name") or "").strip()
        if not name:
            return {"ok": False, "class": "validation_failed", "error": {"code": "missing_sandbox_name", "message": "sandbox.name is required", "class": "validation_failed"}}
        try:
            sb = await cls.create(image_for_config(mod, cfg), name=name, **sandbox_kwargs(cfg, cls.create))
            try:
                return {"ok": True, "sandbox": summarize_sandbox({"id": name, "name": getattr(sb, "name", name), "status": "running", "metadata": (req.get("sandbox") or {}).get("metadata") or {}})}
            finally:
                await sb.disconnect()
        except Exception as exc:
            return error_response(exc)

    return asyncio.run(_run())


def delete(req, mod):
    async def _run():
        cls = sandbox_class(mod)
        sid = str(req.get("sandboxId") or "").strip()
        if not sid:
            return {"ok": False, "class": "validation_failed", "error": {"code": "missing_sandbox_id", "message": "sandboxId is required", "class": "validation_failed"}}
        if cls is None or not hasattr(cls, "delete"):
            return {"ok": False, "class": "environment_blocked", "error": {"code": "sdk_missing_delete", "message": "CUA SDK Sandbox.delete is unavailable", "class": "environment_blocked"}}
        try:
            await cls.delete(sid)
            return {"ok": True, "sandbox": summarize_sandbox({"id": sid, "name": sid, "status": "deleted"})}
        except Exception as exc:
            return error_response(exc)

    return asyncio.run(_run())


def upload_bytes(req, mod):
    async def _run():
        cls = sandbox_class(mod)
        sid = str(req.get("sandboxId") or "").strip()
        files = req.get("files") or []
        if not sid or not files:
            return {"ok": False, "class": "validation_failed", "error": {"code": "missing_upload_input", "message": "sandboxId and files are required", "class": "validation_failed"}}
        sb = None
        try:
            sb = await cls.connect(sid)
            for item in files:
                path = str(item.get("path") or "").strip()
                content = base64.b64decode(str(item.get("contentBase64") or ""))
                await sb.files.write_bytes(path, content)
            return {"ok": True, "sandbox": summarize_sandbox({"id": sid, "name": sid, "status": "running"})}
        except Exception as exc:
            return error_response(exc)
        finally:
            if sb is not None:
                await sb.disconnect()

    return asyncio.run(_run())


def exec_command(req, mod):
    async def _run():
        cls = sandbox_class(mod)
        sid = str(req.get("sandboxId") or "").strip()
        command = command_text(req.get("command") or [])
        if not sid or not command:
            return {"ok": False, "class": "validation_failed", "error": {"code": "missing_exec_input", "message": "sandboxId and command are required", "class": "validation_failed"}}
        timeout = int(req.get("timeout") or (req.get("config") or {}).get("execTimeoutSecs") or 600)
        workdir = str(req.get("workdir") or "").strip()
        env = {str(k): str(v) for k, v in (req.get("env") or {}).items()}
        invalid_env = sorted(k for k in env if not valid_env_name(k))
        if invalid_env:
            return {"ok": False, "class": "validation_failed", "error": {"code": "invalid_env_name", "message": "invalid environment variable name: " + ", ".join(invalid_env), "class": "validation_failed"}}
        sb = None
        try:
            sb = await cls.connect(sid)
            env_file_path = ""
            if env:
                env_file_path = f"/tmp/crabbox-cua-env-{uuid.uuid4().hex}.sh"
                await sb.files.write_bytes(env_file_path, env_file_content(env))
            script = exec_script(command, workdir, env_file_path)
            try:
                result = await sb.shell.run(script, timeout=timeout)
            finally:
                if env_file_path:
                    await cleanup_env_file(sb, env_file_path)
            return {"ok": True, "stdout": result.stdout, "stderr": result.stderr, "exitCode": int(result.returncode), "sandbox": summarize_sandbox({"id": sid, "name": sid, "status": "running"})}
        except Exception as exc:
            return error_response(exc)
        finally:
            if sb is not None:
                await sb.disconnect()

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
            emit(create(req, mod))
        if action == "delete":
            emit(delete(req, mod))
        if action == "upload_bytes":
            emit(upload_bytes(req, mod))
        if action == "exec":
            emit(exec_command(req, mod))
        emit({"ok": False, "class": "validation_failed", "error": {"code": "unknown_action", "message": f"unknown CUA bridge action {action}", "class": "validation_failed"}})
    except SystemExit:
        raise
    except Exception as exc:
        emit({"ok": False, "class": "environment_blocked", "error": {"code": exc.__class__.__name__, "message": str(exc), "class": "environment_blocked"}, "stderr": traceback.format_exc()}, 0)


if __name__ == "__main__":
    main()
