#!/usr/bin/env node

import { execFileSync } from "node:child_process";

const [binary, expectedPath, expectedCommit, expectedGoos, expectedGoarch, expectedGoVersion, runtimeLayout] =
  process.argv.slice(2);

if (
  !binary ||
  !expectedPath ||
  !/^[0-9a-f]{40}$/.test(expectedCommit ?? "") ||
  !expectedGoos ||
  !expectedGoarch ||
  process.argv.length > 9 ||
  (runtimeLayout !== undefined && (runtimeLayout !== "filesystem" || expectedPath !== "github.com/openclaw/crabbox/cmd/crabbox-runtime")) ||
  !/^go[0-9]+\.[0-9]+(?:\.[0-9]+)?$/.test(expectedGoVersion ?? "")
) {
  process.stderr.write(
    "usage: verify-go-release-binary.mjs <binary> <package> <commit> <goos> <goarch> <go-version> [filesystem]\n",
  );
  process.exit(2);
}

let info;
try {
  info = JSON.parse(execFileSync("go", ["version", "-m", "-json", binary], {
    encoding: "utf8",
    // Reading build info must not select or download the checkout's toolchain.
    env: { ...process.env, GOTOOLCHAIN: "local" },
  }));
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
if (expectedPath === "github.com/openclaw/crabbox/cmd/crabbox-runtime") {
  const operatingSystems = runtimeLayout === "filesystem" ? ["darwin", "linux", "windows"] : ["linux"];
  if (!operatingSystems.includes(expectedGoos) || !["amd64", "arm64"].includes(expectedGoarch)) {
    throw new Error("remote runtime target does not match the selected release layout");
  }
  expected.set(expectedGoarch === "amd64" ? "GOAMD64" : "GOARM64", expectedGoarch === "amd64" ? "v1" : "v8.0");
}

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
