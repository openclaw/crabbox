#!/usr/bin/env node

import fs from "node:fs";
import path from "node:path";
import zlib from "node:zlib";
import {
  isRunnerBuildID,
  maxRunnerBytes,
  runnerSHA256,
  runnerTargets,
  unpackRunnerBundle,
} from "./runner-release-bundle.mjs";

const [directory, buildId, output] = process.argv.slice(2);
if (process.argv.length !== 5 || !isRunnerBuildID(buildId)) {
  throw new Error(
    "usage: pack-release-runners.mjs <raw-runner-directory> <source-build-id> <output>",
  );
}

const entries = [];
const payloads = [];
let offset = 0;
for (const { os, arch, name } of runnerTargets) {
  const file = path.join(directory, name);
  const stat = fs.lstatSync(file);
  if (!stat.isFile() || stat.isSymbolicLink() || stat.size < 1 || stat.size > maxRunnerBytes) {
    throw new Error(`invalid raw runner component: ${name}`);
  }
  const raw = fs.readFileSync(file);
  const packed = zlib.gzipSync(raw, { level: 9 });
  if (packed.length > maxRunnerBytes) throw new Error(`compressed runner too large: ${name}`);
  entries.push({
    os,
    arch,
    sha256: runnerSHA256(raw),
    size: raw.length,
    packedSha256: runnerSHA256(packed),
    packedSize: packed.length,
    offset,
  });
  payloads.push(packed);
  offset += packed.length;
}
const header = Buffer.from(JSON.stringify({ version: 1, buildId, entries }));
const prefix = Buffer.alloc(12);
prefix.write("CBXRPK01", 0, "ascii");
prefix.writeUInt32BE(header.length, 8);
const bundle = Buffer.concat([prefix, header, ...payloads]);
unpackRunnerBundle(bundle, buildId);
fs.writeFileSync(output, bundle, { flag: "wx", mode: 0o600 });
