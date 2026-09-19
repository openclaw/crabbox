#!/usr/bin/env python3
"""Independent HTTPS inventory proof for the Boxd live smoke (no vendor CLI).

Authenticates with the same bxd_ API key the provider uses, but exchanges and
reads over the HTTPS console REST API — a different transport than the
provider's gRPC path, so the residue check does not trust the code under test.
"""
import json
import os
import sys
import time
import urllib.parse
import urllib.request

LIMIT = 4 * 1024 * 1024

# One key exchange per process: the endpoint is rate-limited per source IP,
# and the verify loop below re-reads inventory many times.
_token_cache = {}


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise ValueError("redirect refused")


def api_key():
    key = os.environ.get("CRABBOX_BOXD_API_KEY") or os.environ.get("BOXD_API_KEY")
    if not key or not key.startswith("bxd_") or len(key) != 47 or not key[4:].isalnum():
        raise ValueError("set CRABBOX_BOXD_API_KEY or BOXD_API_KEY to a bxd_ API key")
    return key


def inventory():
    raw = os.environ.get("CRABBOX_BOXD_API_URL") or "https://app.boxd.sh"
    url = urllib.parse.urlsplit(raw)
    if (url.scheme != "https" or not url.hostname or url.username is not None
            or url.password is not None or url.path not in ("", "/")
            or "?" in raw or "#" in raw):
        raise ValueError("invalid HTTPS origin")
    origin = urllib.parse.urlunsplit(("https", url.netloc, "", "", ""))
    opener = urllib.request.build_opener(NoRedirect())

    def request(route, body=None, headers=None):
        data = None
        send = {"Accept": "application/json"}
        if body is not None:
            data = json.dumps(body).encode("utf8")
            send["Content-Type"] = "application/json"
        send.update(headers or {})
        req = urllib.request.Request(origin + route, data=data, headers=send)
        with opener.open(req, timeout=30) as response:
            payload = response.read(LIMIT + 1)
        if len(payload) > LIMIT:
            raise ValueError("response too large")
        return json.loads(payload)

    token = _token_cache.get(origin)
    if not token:
        token = request("/api/v1/auth/token", body={"api_key": api_key()})["token"]
        if not isinstance(token, str) or not token or token.startswith("bxd_"):
            raise ValueError("invalid exchange response")
        _token_cache[origin] = token
    auth = {"Authorization": "Bearer " + token}
    user = request("/api/v1/whoami", headers=auth)["user_id"]
    if not isinstance(user, str) or not user:
        raise ValueError("missing authenticated identity")
    # A bxd_ key is fenced to exactly one org on the vendor side, and the
    # console's org parameter resolves ids only (a name is refused), so reads
    # rely on the key's own fencing. The configured org still participates in
    # the scope-change check below.
    org = os.environ.get("CRABBOX_BOXD_ORG", "")
    rows = request("/api/v1/vms", headers=auth)
    if not isinstance(rows, list):
        raise ValueError("inventory is not an array")
    ids = set()
    for vm in rows:
        if not isinstance(vm.get("id"), str) or not vm["id"] or vm["id"] in ids:
            raise ValueError("invalid machine identity")
        ids.add(vm["id"])
    return {"origin": origin, "org": org, "user_id": user, "machines": rows}


def main():
    if len(sys.argv) not in (2, 3) or sys.argv[1] not in ("snapshot", "verify"):
        raise ValueError("usage")
    if sys.argv[1] == "snapshot":
        print(json.dumps(inventory()))
        return
    with open(sys.argv[2], encoding="utf8") as file:
        before = json.load(file)
    # Destroyed tombstones in the baseline are expected to purge from the
    # listing mid-smoke; only live preexisting machines must survive.
    old_ids = {vm["id"] for vm in before["machines"] if vm.get("status") != "destroyed"}
    old_tombstones = {vm["id"] for vm in before["machines"]} - old_ids
    # Account and org must stay fixed. This never deletes resources or trusts
    # Crabbox's own list as evidence of provider-side absence. The console
    # listing can lag the authoritative destroy by tens of seconds, so real
    # residue is only reported after it persists across the whole window.
    attempts = int(os.environ.get("CRABBOX_BOXD_VERIFY_ATTEMPTS", "20"))
    delay = float(os.environ.get("CRABBOX_BOXD_VERIFY_DELAY", "3"))
    for attempt in range(attempts):
        try:
            after = inventory()
        except Exception:
            # The exchange endpoint rate-limits per source IP and follows a
            # burst of provider exchanges; a transient read failure retries
            # within the window and only the final attempt is fatal.
            if attempt == attempts - 1:
                raise
            time.sleep(delay)
            continue
        if any(before[k] != after[k] for k in ("origin", "org", "user_id")):
            raise ValueError("inventory scope changed")
        if not old_ids.issubset({vm["id"] for vm in after["machines"]}):
            raise ValueError("preexisting machine disappeared during smoke")
        # Boxd keeps a readable "destroyed" tombstone in inventory for a while
        # after destruction; a tombstone is not a live resource and not residue.
        residue = [vm for vm in after["machines"] if vm["id"] not in old_ids
                   and vm["id"] not in old_tombstones
                   and vm.get("name", "").startswith("crabbox-")
                   and vm.get("status") != "destroyed"]
        if not residue:
            print("boxd independent HTTPS inventory: zero residue")
            return
        if attempt < attempts - 1:
            time.sleep(delay)
    raise ValueError("inventory has remaining machines")


if __name__ == "__main__":
    try:
        main()
    except Exception:
        # urllib errors may contain keys, redirects or arbitrary response text.
        print("boxd HTTPS inventory proof failed; verify origin, bxd_ API key, account and remaining machines in the console", file=sys.stderr)
        sys.exit(1)
