import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import zlib from "node:zlib";
import {
  extractRunnerBundle,
  runnerSHA256,
  runnerTargets,
  unpackRunnerBundle,
} from "./runner-release-bundle.mjs";

const sourceCommit = "b".repeat(40);
const root = path.resolve(import.meta.dirname, "..");

function fixture(t, sourceCommit = "b".repeat(40)) {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-runner-bundle-"));
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }));
  const raw = path.join(directory, "raw");
  fs.mkdirSync(raw);
  for (const { name } of runnerTargets)
    fs.writeFileSync(path.join(raw, name), `fixture:${name}`, { mode: 0o700 });
  const bundleFile = path.join(directory, "runners.bin");
  execFileSync(process.execPath, [
    path.join(root, "scripts/pack-release-runners.mjs"),
    raw,
    sourceCommit,
    bundleFile,
  ]);
  const bytes = fs.readFileSync(bundleFile);
  const unpacked = unpackRunnerBundle(bytes, sourceCommit);
  const provenance = {
    schemaVersion: 2,
    source: { commit: sourceCommit },
    runnerBundle: {
      buildId: sourceCommit,
      sha256: unpacked.sha256,
      size: unpacked.size,
      members: unpacked.members.map(({ name, metadata }) => ({ name, ...metadata })),
    },
  };
  return { directory, bytes, provenance };
}

test("runner packer and reader accept both supported build ID widths", (t) => {
  for (const width of [40, 64]) {
    const id = "c".repeat(width);
    const { bytes } = fixture(t, id);
    const unpacked = unpackRunnerBundle(bytes, id);
    assert.equal(unpacked.manifest.buildId, id);
    assert.equal(unpacked.members.length, 6);
    assert.throws(() => unpackRunnerBundle(bytes, "c".repeat(width - 1)), /identity/);
    assert.throws(() => unpackRunnerBundle(bytes, "g".repeat(width)), /identity/);
  }
});

test("protected runner extraction reads candidate bytes without executing candidate code", (t) => {
  const { directory, bytes, provenance } = fixture(t);
  const marker = path.join(directory, "executed");
  const binary = path.join(directory, "candidate");
  fs.writeFileSync(
    binary,
    Buffer.concat([
      Buffer.from(`#!/bin/sh\ntouch '${marker}'\nexit 0\n`),
      bytes,
      Buffer.from("trailer"),
    ]),
    { mode: 0o700 },
  );
  const proof = path.join(directory, "provenance.json");
  fs.writeFileSync(proof, JSON.stringify(provenance));
  const output = path.join(directory, "extracted");
  execFileSync(process.execPath, [
    path.join(root, "scripts/extract-release-runners.mjs"),
    binary,
    proof,
    output,
  ]);
  assert.equal(fs.existsSync(marker), false);
  assert.deepEqual(fs.readdirSync(output).sort(), runnerTargets.map(({ name }) => name).sort());
  for (const { name } of runnerTargets)
    assert.equal(fs.readFileSync(path.join(output, name), "utf8"), `fixture:${name}`);
});

test("runner extraction rejects duplicate bundles and provenance substitution", (t) => {
  const { directory, bytes, provenance } = fixture(t);
  assert.throws(
    () => extractRunnerBundle(Buffer.concat([bytes, bytes]), provenance.runnerBundle),
    /found 2/,
  );
  assert.throws(
    () => extractRunnerBundle(bytes, { ...provenance.runnerBundle, sha256: "0".repeat(64) }),
    /found 0/,
  );
  const binary = path.join(directory, "candidate");
  fs.writeFileSync(binary, bytes);
  const proof = path.join(directory, "provenance.json");
  provenance.runnerBundle.members[0].sha256 = "0".repeat(64);
  fs.writeFileSync(proof, JSON.stringify(provenance));
  const result = spawnSync(process.execPath, [
    path.join(root, "scripts/extract-release-runners.mjs"),
    binary,
    proof,
    path.join(directory, "out"),
  ]);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr.toString(), /provenance member mismatch/);
});

test("an appended approved bundle cannot conceal a different operational bundle", (t) => {
  const { directory, bytes, provenance } = fixture(t);
  const raw = path.join(directory, "raw");
  fs.appendFileSync(path.join(raw, runnerTargets[0].name), "substituted executable");
  const other = path.join(directory, "other.bin");
  execFileSync(process.execPath, [
    path.join(root, "scripts/pack-release-runners.mjs"),
    raw,
    sourceCommit,
    other,
  ]);
  const candidate = Buffer.concat([fs.readFileSync(other), Buffer.from("binary padding"), bytes]);
  assert.throws(
    () => extractRunnerBundle(candidate, provenance.runnerBundle),
    /exactly one valid runner bundle, found 2/,
  );
});

test("bundle parser rejects duplicate metadata and hidden gzip trailers", (t) => {
  const { bytes } = fixture(t);
  const headerLength = bytes.readUInt32BE(8);
  const manifest = JSON.parse(bytes.subarray(12, 12 + headerLength));
  const payload = bytes.subarray(12 + headerLength);
  const encode = (header, body) => {
    const prefix = Buffer.alloc(12);
    prefix.write("CBXRPK01");
    prefix.writeUInt32BE(header.length, 8);
    return Buffer.concat([prefix, header, body]);
  };
  const duplicated = Buffer.from(
    JSON.stringify(manifest).replace('"version":1', '"version":1,"version":1'),
  );
  assert.throws(
    () => unpackRunnerBundle(encode(duplicated, payload), sourceCommit),
    /not canonical/,
  );
  const last = manifest.entries.at(-1);
  const emptyStream = zlib.gzipSync(Buffer.alloc(0));
  const extended = Buffer.concat([
    payload.subarray(last.offset, last.offset + last.packedSize),
    emptyStream,
  ]);
  last.packedSize = extended.length;
  last.packedSha256 = runnerSHA256(extended);
  const trailing = encode(
    Buffer.from(JSON.stringify(manifest)),
    Buffer.concat([payload.subarray(0, last.offset), extended]),
  );
  assert.throws(() => unpackRunnerBundle(trailing, sourceCommit), /gzip has trailing data/);
});

test("static release gate verifies nested runners before candidate execution", () => {
  const verifier = fs.readFileSync(path.join(root, "scripts/verify-release.sh"), "utf8");
  assert.ok(
    verifier.indexOf("scripts/extract-release-runners.mjs") <
      verifier.indexOf('if [[ "$VERIFY_MODE" == static ]]'),
  );
  assert.ok(
    verifier.indexOf('"$CRABBOX_RELEASE_RUNNER_IDENTIFIER" arm64') <
      verifier.indexOf('if [[ "$VERIFY_MODE" == static ]]'),
  );
  const packager = fs.readFileSync(path.join(root, "scripts/package-release.sh"), "utf8");
  assert.doesNotMatch(packager, /go run|goreleaser release|runner-export/);
  assert.ok(packager.indexOf("notary_runner_arm64=") < packager.indexOf("-tags=runnerembed"));
  assert.match(packager, /env -i[\s\S]*-tags=runnerembed/);
  assert.match(packager, /\$ROOT\/scripts\/pack-release-runners\.mjs/);
});
