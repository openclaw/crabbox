#!/usr/bin/env node

import fs from "node:fs";
import path from "node:path";
import { extractRunnerBundle } from "./runner-release-bundle.mjs";

if (process.argv.length !== 5)
  throw new Error(
    "usage: extract-release-runners.mjs <cli-binary> <provenance> <output-directory>",
  );
const [, , binaryFile, provenanceFile, outputDirectory] = process.argv;
const provenance = JSON.parse(fs.readFileSync(provenanceFile, "utf8"));
if (
  provenance.schemaVersion !== 2 ||
  provenance.runnerBundle?.buildId !== provenance.source?.commit
) {
  throw new Error("runner extraction requires source-bound provenance schema 2");
}
const info = fs.lstatSync(binaryFile);
if (!info.isFile() || info.isSymbolicLink() || info.size > 512 * 1024 * 1024)
  throw new Error("invalid containing CLI binary");
const bundle = extractRunnerBundle(fs.readFileSync(binaryFile), provenance.runnerBundle);
for (const [index, member] of bundle.members.entries()) {
  const recorded = provenance.runnerBundle.members?.[index];
  if (recorded?.name !== member.name) throw new Error("runner provenance member name mismatch");
  for (const [key, value] of Object.entries(member.metadata)) {
    if (recorded[key] !== value)
      throw new Error(`runner provenance member mismatch: ${member.name}/${key}`);
  }
}
fs.mkdirSync(outputDirectory, { mode: 0o700 });
for (const member of bundle.members) {
  fs.writeFileSync(path.join(outputDirectory, member.name), member.bytes, {
    flag: "wx",
    mode: 0o700,
  });
}
