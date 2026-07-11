#!/usr/bin/env node

import { execFileSync } from "node:child_process";

const [binary, expectedPath, expectedCommit, expectedGoos, expectedGoarch, expectedGoVersion] =
  process.argv.slice(2);

if (
  !binary ||
  !expectedPath ||
  !/^[0-9a-f]{40}$/.test(expectedCommit ?? "") ||
  !expectedGoos ||
  !expectedGoarch ||
  !/^go[0-9]+\.[0-9]+(?:\.[0-9]+)?$/.test(expectedGoVersion ?? "")
) {
  process.stderr.write(
    "usage: verify-go-release-binary.mjs <binary> <package> <commit> <goos> <goarch> <go-version>\n",
  );
  process.exit(2);
}

let info;
try {
  info = JSON.parse(execFileSync("go", ["version", "-m", "-json", binary], { encoding: "utf8" }));
} catch (error) {
  throw new Error(`cannot read Go build info from ${binary}: ${error.message}`);
}

const settings = new Map((info.Settings ?? []).map(({ Key, Value }) => [Key, Value]));
const expected = new Map([
  ["-trimpath", "true"],
  ["CGO_ENABLED", "0"],
  ["GOOS", expectedGoos],
  ["GOARCH", expectedGoarch],
  ["vcs.revision", expectedCommit],
  ["vcs.modified", "false"],
]);

if (info.Path !== expectedPath) {
  throw new Error(`${binary} package path ${JSON.stringify(info.Path)} does not equal ${expectedPath}`);
}
if (info.GoVersion !== expectedGoVersion) {
  throw new Error(`${binary} Go version ${JSON.stringify(info.GoVersion)} does not equal ${expectedGoVersion}`);
}
for (const [key, value] of expected) {
  if (settings.get(key) !== value) {
    throw new Error(
      `${binary} build setting ${key}=${JSON.stringify(settings.get(key))} does not equal ${value}`,
    );
  }
}

