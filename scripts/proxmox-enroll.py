#!/usr/bin/python3
"""Prepared-template QGA stdin helper. No issuer credential or enrolled state.

Install root-owned and non-writable by the lease user at
/usr/local/libexec/crabbox-enroll. Never enable request-body/stdin logging.
"""
import ipaddress
import json
import os
from pathlib import Path
import re
import subprocess
import sys

TAILSCALE = "/usr/bin/tailscale"
METADATA = Path("/var/lib/crabbox")


def enroll(stream):
    if os.geteuid() != 0:
        raise ValueError("root required")
    raw = stream.read(8193)
    if len(raw) > 8192:
        raise ValueError("input too large")
    data = json.loads(raw)
    key, hostname, tags = data["authKey"], data["hostname"], data["tags"]
    if not isinstance(key, str) or not re.fullmatch(r"tskey-auth-[A-Za-z0-9_-]{1,4000}", key):
        raise ValueError("key format")
    if not isinstance(hostname, str) or not re.fullmatch(r"[a-z0-9][a-z0-9-]{0,62}", hostname):
        raise ValueError("hostname format")
    if not isinstance(tags, list) or not tags or len(tags) > 32 or any(
        not isinstance(tag, str) or not re.fullmatch(r"tag:[a-zA-Z0-9][a-zA-Z0-9-]*", tag)
        for tag in tags
    ):
        raise ValueError("tag format")
    before = subprocess.run(
        [TAILSCALE, "status", "--json"], stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL, timeout=10, check=False,
        env={"PATH": "/usr/sbin:/usr/bin:/sbin:/bin"},
    )
    state = json.loads(before.stdout)
    if state.get("BackendState") not in ("NeedsLogin", "NoState") or (state.get("Self") or {}).get("ID"):
        raise ValueError("template is not clean")
    # No auth key in argv, environment, seed, files or subprocess output.
    # /dev/stdin is consumed by tailscale, not interpreted by a shell.
    subprocess.run(
        [TAILSCALE, "up", "--auth-key=file:/dev/stdin", "--hostname=" + hostname,
         "--advertise-tags=" + ",".join(tags), "--accept-dns=true",
         "--accept-routes=false", "--ssh=false", "--timeout=60s"],
        input=key.encode(), stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        env={"PATH": "/usr/sbin:/usr/bin:/sbin:/bin"}, timeout=70, check=True,
    )
    result = subprocess.run(
        [TAILSCALE, "status", "--json"], stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL, timeout=10, check=True,
        env={"PATH": "/usr/sbin:/usr/bin:/sbin:/bin"},
    )
    status = json.loads(result.stdout)
    if status.get("BackendState") != "Running":
        raise ValueError("not joined")
    device_id = status.get("Self", {}).get("ID")
    if not isinstance(device_id, str) or not device_id.strip() or any(char.isspace() for char in device_id) or "\x00" in device_id:
        raise ValueError("no device identity")
    addresses = [ipaddress.ip_address(value) for value in status.get("Self", {}).get("TailscaleIPs", [])]
    if not any(ip.version == 4 and ip in ipaddress.ip_network("100.64.0.0/10") for ip in addresses):
        raise ValueError("no Tailnet IPv4")
    # Reuse Crabbox's common SSH metadata reader; QGA remains the authoritative
    # live address selector. Only individual non-secret fields are saved.
    if METADATA.is_symlink():
        raise ValueError("metadata symlink")
    METADATA.mkdir(mode=0o755, parents=True, exist_ok=True)
    directory = os.open(METADATA, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        info = os.fstat(directory)
        if info.st_uid != 0 or info.st_mode & 0o022:
            raise ValueError("metadata permissions")
        # QGA may run with umask 077. These non-secret receipts must be readable
        # by the lease SSH user; change only the checked, non-symlink inode.
        os.fchmod(directory, 0o755)
    finally:
        os.close(directory)
    values = {
        "tailscale-ipv4": sorted(str(ip) for ip in addresses if ip.version == 4 and ip in ipaddress.ip_network("100.64.0.0/10"))[0],
        "tailscale-hostname": hostname,
        "tailscale-device-id": device_id,
    }
    for name, value in values.items():
        # Do not follow an image-provided or lease-user-controlled symlink.
        descriptor = os.open(METADATA / name, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o644)
        with os.fdopen(descriptor, "w") as output:
            os.fchmod(output.fileno(), 0o644)
            output.write(value + "\n")


def main():
    try:
        enroll(sys.stdin)
    except Exception:
        # QGA may retain output; never print an exception, input or subprocess data.
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
