#!/usr/bin/env python3
"""Run one exhaustive, disjoint shard of the CLI's top-level Go tests."""

import argparse
import re
import subprocess
import sys


def select_tests(listing, shard, count):
    if count < 1 or not 0 <= shard < count:
        raise ValueError("shard must be in [0, count)")
    # Include fuzz seed corpora and examples, just as an ordinary go test does.
    names = sorted(set(re.findall(r"^(?:Test|Example|Fuzz)\w*$", listing, re.M)))
    if len(names) < count:
        raise ValueError("test discovery returned fewer tests than shards")
    return names[shard::count]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("shard", type=int)
    parser.add_argument("count", type=int)
    args = parser.parse_args()
    if args.count < 1 or not 0 <= args.shard < args.count:
        parser.error("shard must be in [0, count)")
    package = "./internal/cli"
    common = ["go", "test", "-race", "-count=1", "-timeout=10m", package]
    listing = subprocess.run(common + ["-list", "."], text=True, capture_output=True)
    if listing.returncode:
        sys.stdout.write(listing.stdout)
        sys.stderr.write(listing.stderr)
        return listing.returncode
    names = select_tests(listing.stdout, args.shard, args.count)
    print(f"CLI shard {args.shard + 1}/{args.count}: {len(names)} tests", flush=True)
    return subprocess.run(common + ["-run", "^(" + "|".join(names) + ")$"]).returncode


if __name__ == "__main__":
    sys.exit(main())
