#!/usr/bin/env node

import crypto from "node:crypto";
import fs from "node:fs";

function fail(message) {
  throw new Error(message);
}

if (process.argv.length !== 5) {
  fail("usage: extract-release-vmd.mjs <helper> <provenance> <output>");
}

const [, , helperFile, provenanceFile, outputFile] = process.argv;
const provenance = JSON.parse(fs.readFileSync(provenanceFile, "utf8"));
const helperRecords = (provenance.payloads ?? [])
  .flatMap((payload) => payload.binaries ?? [])
  .filter((binary) => binary.name === "crabbox-apple-vm-helper");
if (helperRecords.length !== 1) {
  fail("provenance must contain exactly one Apple VM helper record");
}

const embedded = helperRecords[0].embeddedVmd;
if (
  !embedded ||
  !Number.isSafeInteger(embedded.size) ||
  embedded.size <= 0 ||
  !/^[0-9a-f]{64}$/.test(embedded.sha256 ?? "")
) {
  fail("embedded VMD provenance must contain an exact positive size and SHA-256");
}

const helper = fs.readFileSync(helperFile);
if (embedded.size > helper.length) {
  fail("embedded VMD is larger than its containing helper");
}

// Official VMD payloads are thin arm64 Mach-O executables. Search only Mach-O
// headers, then require the provenance-sized byte range to have the one exact
// protected digest. This reads the Go-embedded blob without executing helper
// code or trusting a candidate-provided export command.
const arm64MachO = Buffer.from([0xcf, 0xfa, 0xed, 0xfe, 0x0c, 0x00, 0x00, 0x01]);
const matches = [];
for (let offset = helper.indexOf(arm64MachO); offset !== -1; offset = helper.indexOf(arm64MachO, offset + 1)) {
  const end = offset + embedded.size;
  if (end > helper.length) continue;
  const candidate = helper.subarray(offset, end);
  const digest = crypto.createHash("sha256").update(candidate).digest("hex");
  if (digest === embedded.sha256) matches.push(Buffer.from(candidate));
}
if (matches.length !== 1) {
  fail(`expected exactly one provenance-matched embedded VMD, found ${matches.length}`);
}

fs.writeFileSync(outputFile, matches[0], { flag: "wx", mode: 0o700 });
