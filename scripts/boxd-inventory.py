#!/usr/bin/env python3
"""Independent HTTPS inventory proof for the Boxd live smoke (no vendor CLI)."""
import json
import os
import sys
import time
import urllib.parse
import urllib.request

LIMIT = 4 * 1024 * 1024


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise ValueError("redirect refused")


def inventory():
    raw = os.environ.get("CRABBOX_BOXD_API_URL") or "https://app.boxd.sh"
    url = urllib.parse.urlsplit(raw)
    if (url.scheme != "https" or not url.hostname or url.username is not None
            or url.password is not None or url.path not in ("", "/")
            or "?" in raw or "#" in raw):
        raise ValueError("invalid HTTPS origin")
    origin = urllib.parse.urlunsplit(("https", url.netloc, "", "", ""))
    token = os.environ.get("CRABBOX_BOXD_TOKEN") or os.environ.get("BOXD_TOKEN")
    if not token or token.strip().startswith("bxd_") or len(token) > 16384 or any(ord(c) < 33 or ord(c) > 126 for c in token):
        raise ValueError("an interactive session token is required; API keys are not supported")
    opener = urllib.request.build_opener(NoRedirect())

    def request(route):
        req = urllib.request.Request(origin + route, headers={
            "Accept": "application/json", "Authorization": "Bearer " + token})
        with opener.open(req, timeout=30) as response:
            data = response.read(LIMIT + 1)
        if len(data) > LIMIT:
            raise ValueError("response too large")
        return json.loads(data)

    user = request("/api/v1/whoami")["user_id"]
    if not isinstance(user, str) or not user:
        raise ValueError("missing authenticated identity")
    org = os.environ.get("CRABBOX_BOXD_ORG", "")
    rows = request("/api/v1/vms" + ("?" + urllib.parse.urlencode({"org": org}) if org else ""))
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
    old_ids = {vm["id"] for vm in before["machines"]}
    # Account and org must stay fixed. This never deletes resources or trusts
    # Crabbox's own list as evidence of provider-side absence.
    for attempt in range(6):
        after = inventory()
        if any(before[k] != after[k] for k in ("origin", "org", "user_id")):
            raise ValueError("inventory scope changed")
        if not old_ids.issubset({vm["id"] for vm in after["machines"]}):
            raise ValueError("preexisting machine disappeared during smoke")
        residue = [vm for vm in after["machines"] if vm["id"] not in old_ids
                   and vm.get("name", "").startswith("crabbox-")]
        if not residue:
            print("boxd independent HTTPS inventory: zero residue")
            return
        if attempt < 5:
            time.sleep(2)
    raise ValueError("inventory has remaining machines")


if __name__ == "__main__":
    try:
        main()
    except Exception:
        # urllib errors may contain tokens, redirects or arbitrary response text.
        print("boxd HTTPS inventory proof failed; verify origin, interactive BOXD_TOKEN session (not an API key), account and remaining machines in the console", file=sys.stderr)
        sys.exit(1)
