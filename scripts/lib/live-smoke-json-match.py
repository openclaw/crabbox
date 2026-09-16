#!/usr/bin/env python3
import json
import os
import sys

PROFILES = {"standard", "runpod", "lambda", "nebius", "vast"}
OPERATIONS = {"contains", "absent", "probe", "empty"}
DIRECT_KEYS = ("slug", "name", "id", "leaseId")
def has_slug(value, slug, profile):
    if isinstance(value, dict):
        labels = value.get("labels")
        if profile in ("lambda", "vast"):
            labels = labels or value.get("tags")
        aliases = ("slug", "crabbox_slug") if profile == "nebius" else ("slug", "crabbox.slug") if profile in ("lambda", "vast") else ("slug",)
        if isinstance(labels, dict) and any(labels.get(alias) == slug for alias in aliases):
            return True
        if profile != "runpod" and any(value.get(key) == slug for key in DIRECT_KEYS):
            return True
        if profile == "vast" and f"|{slug}|" in str(value.get("label", "")):
            return True
        return any(has_slug(child, slug, profile) for child in value.values())
    if isinstance(value, list):
        return any(has_slug(child, slug, profile) for child in value)
    return False
def main():
    if len(sys.argv) != 3 or sys.argv[1] not in PROFILES or sys.argv[2] not in OPERATIONS:
        print("usage: live-smoke-json-match.py PROFILE OPERATION", file=sys.stderr)
        return 2
    profile, operation = sys.argv[1:3]
    try:
        payload = json.load(sys.stdin)
    except Exception as exc:
        print(f"invalid JSON: {exc}", file=sys.stderr)
        return 2 if operation == "probe" else 1
    matched = payload == [] if operation == "empty" else has_slug(payload, os.environ["CRABBOX_SMOKE_SLUG"], profile)
    if operation == "probe":
        return 0 if matched else 1
    if matched == (operation == "absent"):
        print(os.environ["CRABBOX_SMOKE_FAILURE"], file=sys.stderr)
        return 1
    return 0
if __name__ == "__main__":
    sys.exit(main())
