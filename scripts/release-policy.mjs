#!/usr/bin/env node

import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

// Read literal policy data, never source a historical shell script. The caller
// establishes that the recorded commit belongs to protected release history.
export function releaseToolchainPolicy(repository, commit) {
  const root = path.resolve(repository);
  let source;
  if (commit === undefined) {
    source = fs.readFileSync(path.join(root, "scripts/release-config.sh"), "utf8");
  } else {
    if (!/^[0-9a-f]{40}$/.test(commit)) throw new Error("release policy requires a full commit SHA");
    source = execFileSync("git", ["--no-replace-objects", "--no-lazy-fetch", "-C", root,
      "cat-file", "blob", `${commit}:scripts/release-config.sh`], { encoding: "utf8", maxBuffer: 1024 * 1024 });
  }
  function literal(name, pattern) {
    const lines = source.split(/\r?\n/).filter((line) => line.startsWith(`${name}=`));
    if (lines.length !== 1) throw new Error(`release policy must define ${name} exactly once`);
    const value = lines[0].slice(name.length + 1);
    if (!pattern.test(value)) throw new Error(`release policy ${name} must be a literal version`);
    return value;
  }
  return Object.freeze({
    go: literal("CRABBOX_RELEASE_GO_VERSION", /^go\d+\.\d+\.\d+$/),
    goreleaser: literal("CRABBOX_RELEASE_GORELEASER_VERSION", /^\d+\.\d+\.\d+$/),
  });
}

if (process.argv[1] && pathToFileURL(path.resolve(process.argv[1])).href === import.meta.url) {
  try {
    const [root, commit, format] = process.argv.slice(2);
    if (!root || !commit || process.argv.length > 5 || (format !== undefined && format !== "--go-version")) {
      throw new Error("usage: release-policy.mjs <repository> <verifier-commit> [--go-version]");
    }
    const policy = releaseToolchainPolicy(root, commit);
    process.stdout.write((format ? policy.go : JSON.stringify(policy)) + "\n");
  } catch (error) {
    process.stderr.write(`${error.message}\n`);
    process.exitCode = 1;
  }
}
