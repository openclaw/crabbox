import assert from "node:assert/strict";
import crypto from "node:crypto";
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

const root = path.resolve(import.meta.dirname, "..");
const read = (file) => fs.readFileSync(path.join(root, file), "utf8");

function writeProvenance(file, bytes) {
  fs.writeFileSync(
    file,
    `${JSON.stringify({
      payloads: [
        {
          binaries: [
            {
              name: "crabbox-apple-vm-helper",
              embeddedVmd: {
                size: bytes.length,
                sha256: crypto.createHash("sha256").update(bytes).digest("hex"),
              },
            },
          ],
        },
      ],
    })}\n`,
  );
}

test("protected extractor finds one exact embedded arm64 Mach-O without executing it", () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-vmd-extract-"));
  try {
    const vmd = Buffer.concat([
      Buffer.from([0xcf, 0xfa, 0xed, 0xfe, 0x0c, 0x00, 0x00, 0x01]),
      crypto.randomBytes(128),
    ]);
    const helper = path.join(directory, "helper");
    const provenance = path.join(directory, "provenance.json");
    const output = path.join(directory, "vmd");
    fs.writeFileSync(helper, Buffer.concat([Buffer.from("helper-prefix"), vmd, Buffer.from("suffix")]));
    writeProvenance(provenance, vmd);

    const result = spawnSync(
      "node",
      [path.join(root, "scripts/extract-release-vmd.mjs"), helper, provenance, output],
      { encoding: "utf8" },
    );
    assert.equal(result.status, 0, result.stderr);
    assert.deepEqual(fs.readFileSync(output), vmd);
    assert.equal(fs.statSync(output).mode & 0o777, 0o700);
  } finally {
    fs.rmSync(directory, { recursive: true, force: true });
  }
});

test("protected extractor rejects ambiguous embedded payload bytes", () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-vmd-ambiguous-"));
  try {
    const vmd = Buffer.concat([
      Buffer.from([0xcf, 0xfa, 0xed, 0xfe, 0x0c, 0x00, 0x00, 0x01]),
      crypto.randomBytes(64),
    ]);
    const helper = path.join(directory, "helper");
    const provenance = path.join(directory, "provenance.json");
    const output = path.join(directory, "vmd");
    fs.writeFileSync(helper, Buffer.concat([vmd, Buffer.from("gap"), vmd]));
    writeProvenance(provenance, vmd);

    const result = spawnSync(
      "node",
      [path.join(root, "scripts/extract-release-vmd.mjs"), helper, provenance, output],
      { encoding: "utf8" },
    );
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /expected exactly one provenance-matched embedded VMD, found 2/);
    assert.equal(fs.existsSync(output), false);
  } finally {
    fs.rmSync(directory, { recursive: true, force: true });
  }
});

test("workflow freezes static proof before dependent clean candidate execution", () => {
  const workflow = read(".github/workflows/release-assets.yml");
  const staticStart = workflow.indexOf("  verify-native:");
  const executeStart = workflow.indexOf("  execute-native:");
  assert.ok(staticStart >= 0 && executeStart > staticStart);
  const staticJob = workflow.slice(staticStart, executeStart);
  const executeJob = workflow.slice(executeStart);

  assert.match(staticJob, /CRABBOX_VERIFY_MODE=static/);
  assert.match(staticJob, /Freeze exact static proof before candidate execution/);
  assert.match(staticJob, /actions\/upload-artifact@/);
  assert.doesNotMatch(staticJob, /CRABBOX_VERIFY_MODE=execute/);

  assert.match(executeJob, /needs: \[guard, download-draft, verify-native\]/);
  assert.match(executeJob, /Download immutable opaque input/);
  assert.match(executeJob, /exec env -i[\s\S]*CRABBOX_VERIFY_MODE=execute/);
  assert.match(executeJob, /permissions:\n\s+contents: read/);
  assert.doesNotMatch(executeJob, /actions\/upload-artifact@|verified-assets\.json|GH_TOKEN:|contents: write/);
});

test("release-capable token is isolated from checkout, verification, and candidate jobs", () => {
  const workflow = read(".github/workflows/release-assets.yml");
  const guardStart = workflow.indexOf("  guard:");
  const downloadStart = workflow.indexOf("  download-draft:");
  const verifyStart = workflow.indexOf("  verify-native:");
  const executeStart = workflow.indexOf("  execute-native:");
  assert.ok(guardStart >= 0 && downloadStart > guardStart && verifyStart > downloadStart && executeStart > verifyStart);
  const guardJob = workflow.slice(guardStart, downloadStart);
  const downloadJob = workflow.slice(downloadStart, verifyStart);
  const verifierJobs = workflow.slice(verifyStart);

  assert.match(guardJob, /GH_TOKEN: \$\{\{ secrets\.CRABBOX_RULESET_READ_TOKEN \}\}/);
  assert.equal((guardJob.match(/secrets\./g) ?? []).length, 1);
  assert.match(downloadJob, /permissions:\n\s+contents: write/);
  assert.match(downloadJob, /GH_TOKEN: \$\{\{ github\.token \}\}/);
  assert.match(downloadJob, /Download exact numeric release without executing its bytes/);
  assert.doesNotMatch(downloadJob, /actions\/checkout@|verify-release\.sh|CRABBOX_VERIFY_MODE/);
  assert.doesNotMatch(verifierJobs, /contents: write|GH_TOKEN:|secrets\./);
  assert.equal((workflow.match(/contents: write/g) ?? []).length, 1);
});

test("static verifier extracts VMD bytes before its candidate execution boundary", () => {
  const verifier = read("scripts/verify-release.sh");
  const extractor = verifier.indexOf("scripts/extract-release-vmd.mjs");
  const staticExit = verifier.indexOf('if [[ "$VERIFY_MODE" == static ]]');
  const helperExecution = verifier.indexOf("crabbox-apple-vm-helper\" vmd-info");
  assert.ok(extractor >= 0 && staticExit > extractor && helperExecution > staticExit);
  assert.doesNotMatch(verifier, /vmd-export/);
});
