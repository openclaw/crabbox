#!/usr/bin/env python3
"""JSON-on-stdout wrapper around cwsandbox. crabbox embeds this and shells
out: cwsandbox CLI 0.23.0 lacks `run`/`stop`, which only live in the SDK."""
from __future__ import annotations
import argparse, json, os, sys, threading

# Importing wandb.sandbox registers a W&B-specific auth mode against cwsandbox
# (via cwsandbox.set_auth_mode) that injects an `x-wandb-api-key` metadata
# header sourced from the W&B user API key. The cwsandbox gateway rejects
# W&B-format keys when sent as a bare Bearer token, so this side-effect
# import is what makes WANDB_API_KEY actually authenticate.
from wandb import sandbox as _wandb_auth_side_effect  # noqa: F401


def die(message, code=1):
    sys.stderr.write(json.dumps({"error": message}) + "\n")
    sys.stderr.flush()
    sys.exit(code)


def require_api_key():
    key = os.environ.get("WANDB_API_KEY", "").strip()
    if not key:
        die("WANDB_API_KEY not set", code=2)


def parse_env(values):
    env = {}
    for raw in values or []:
        if "=" not in raw:
            die("--env entries must be KEY=VALUE")
        key, value = raw.split("=", 1)
        if not key.strip():
            die("--env entries must have a non-empty key")
        env[key.strip()] = value
    return env


def sandbox_to_dict(sb):
    try:
        status = str(sb.status or "")
    except Exception:
        status = ""
    try:
        started = sb.started_at.isoformat() if sb.started_at else ""
    except Exception:
        started = ""
    return {"id": sb.sandbox_id, "status": status, "created_at": started}


def cmd_version(_args):
    import cwsandbox
    print(json.dumps({"cwsandbox": cwsandbox.__version__}))


def cmd_acquire(args):
    require_api_key()
    import cwsandbox
    kwargs = {"container_image": args.image, "tags": [t for t in (args.tag or []) if t]}
    if args.max_lifetime is not None:
        kwargs["max_lifetime_seconds"] = float(args.max_lifetime)
    env = parse_env(args.env)
    if env:
        kwargs["environment_variables"] = env
    sb = cwsandbox.Sandbox.run(**kwargs)
    sb.wait()
    print(json.dumps(sandbox_to_dict(sb)))


def _pump(stream, sink):
    try:
        for chunk in stream:
            if chunk is None:
                continue
            if isinstance(chunk, bytes):
                chunk = chunk.decode("utf-8", errors="replace")
            sink.write(chunk)
            sink.flush()
    except Exception:
        pass


def cmd_exec(args):
    require_api_key()
    import cwsandbox
    sb = cwsandbox.Sandbox.from_id(args.id).result()
    exec_kwargs = {}
    if args.cwd:
        exec_kwargs["cwd"] = args.cwd
    if args.timeout is not None:
        exec_kwargs["timeout_seconds"] = float(args.timeout)
    proc = sb.exec(list(args.command), **exec_kwargs)
    t_out = threading.Thread(target=_pump, args=(proc.stdout, sys.stdout), daemon=True)
    t_err = threading.Thread(target=_pump, args=(proc.stderr, sys.stderr), daemon=True)
    t_out.start()
    t_err.start()
    rc = proc.wait()
    t_out.join(timeout=2)
    t_err.join(timeout=2)
    sys.stdout.flush()
    sys.stderr.flush()
    sys.exit(int(rc or 0))


def cmd_stop(args):
    require_api_key()
    import cwsandbox
    try:
        sb = cwsandbox.Sandbox.from_id(args.id).result()
    except cwsandbox.SandboxNotFoundError:
        if args.missing_ok:
            return
        raise
    sb.stop(graceful_shutdown_seconds=float(args.graceful_seconds), missing_ok=bool(args.missing_ok)).result()


def cmd_list(args):
    require_api_key()
    import cwsandbox
    kwargs = {"tags": [t for t in (args.tag or []) if t]}
    status = (args.status or "").strip().lower()
    if status and status != "all":
        kwargs["status"] = status.upper()
    if status == "all":
        kwargs["include_stopped"] = True
    sandboxes = cwsandbox.Sandbox.list(**kwargs).result()
    print(json.dumps({"sandboxes": [sandbox_to_dict(sb) for sb in sandboxes]}))


def cmd_status(args):
    require_api_key()
    import cwsandbox
    sb = cwsandbox.Sandbox.from_id(args.id).result()
    print(json.dumps(sandbox_to_dict(sb)))


def build_parser():
    parser = argparse.ArgumentParser(prog="wandb_sandbox", description=__doc__)
    sub = parser.add_subparsers(dest="cmd", required=True)
    sub.add_parser("version").set_defaults(func=cmd_version)

    p = sub.add_parser("acquire")
    p.add_argument("--image", required=True)
    p.add_argument("--max-lifetime", type=int, default=1800)
    p.add_argument("--tag", action="append", default=[])
    p.add_argument("--env", action="append", default=[])
    p.set_defaults(func=cmd_acquire)

    p = sub.add_parser("exec")
    p.add_argument("--id", required=True)
    p.add_argument("--cwd", default="")
    p.add_argument("--timeout", type=int, default=None)
    p.add_argument("command", nargs=argparse.REMAINDER)
    p.set_defaults(func=cmd_exec)

    p = sub.add_parser("stop")
    p.add_argument("--id", required=True)
    p.add_argument("--graceful-seconds", type=int, default=10)
    p.add_argument("--missing-ok", action="store_true")
    p.set_defaults(func=cmd_stop)

    p = sub.add_parser("list")
    p.add_argument("--tag", action="append", default=[])
    p.add_argument("--status", default="")
    p.set_defaults(func=cmd_list)

    p = sub.add_parser("status")
    p.add_argument("--id", required=True)
    p.set_defaults(func=cmd_status)

    return parser


def main(argv=None):
    args = build_parser().parse_args(argv)
    if args.cmd == "exec":
        cmd = list(args.command or [])
        if cmd and cmd[0] == "--":
            cmd = cmd[1:]
        args.command = cmd
        if not args.command:
            die("exec requires a command after --")
    try:
        args.func(args)
    except SystemExit:
        raise
    except ImportError as exc:
        die("cwsandbox SDK not installed: %s" % exc, code=3)
    except Exception as exc:
        die("%s: %s" % (type(exc).__name__, exc))


if __name__ == "__main__":
    main()
