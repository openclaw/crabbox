#!/usr/bin/env bash
set -euo pipefail

# Snapshots do not own the caller's Go cache. Only the credential-free
# producer marks a private cache for retirement after each serialized build.
[[ -n "${CRABBOX_RELEASE_CACHE_OWNER+x}" ]] || exit 0

node <<'NODE'
const fs = require("node:fs");
const path = require("node:path");
const marker = process.env.CRABBOX_RELEASE_CACHE_OWNER;
const cache = process.env.GOCACHE;
try {
  if (!marker || !path.isAbsolute(marker) || path.basename(marker) !== ".gocache-owner") throw new Error();
  const root = path.dirname(marker);
  const rootStat = fs.lstatSync(root);
  const markerStat = fs.lstatSync(marker);
  if (
    root !== fs.realpathSync(root) ||
    !path.basename(root).startsWith("crabbox-release-build.") ||
    !rootStat.isDirectory() || rootStat.uid !== process.getuid() || (rootStat.mode & 0o077) !== 0 ||
    !markerStat.isFile() || markerStat.uid !== process.getuid() || (markerStat.mode & 0o077) !== 0 ||
    cache !== path.join(root, "gocache") ||
    fs.readFileSync(marker, "utf8") !== `${cache}\n`
  ) throw new Error();
  const cacheStat = fs.lstatSync(cache);
  if (!cacheStat.isDirectory() || cacheStat.uid !== process.getuid()) throw new Error();
} catch {
  console.error("release cache cleanup refused invalid private-cache ownership");
  process.exit(1);
}
NODE

go clean -cache
echo "Cleared completed release build cache"
