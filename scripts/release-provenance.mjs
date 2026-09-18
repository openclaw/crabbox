#!/usr/bin/env node

import crypto from "node:crypto";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { releaseToolchainPolicy } from "./release-policy.mjs";

const REPOSITORY = "openclaw/crabbox";
const TEAM_ID = "FWJYW4S8P8";
const AUTHORITY = `Developer ID Application: OpenClaw Foundation (${TEAM_ID})`;
const RUNTIME_ID = "org.openclaw.crabbox.runtime";
const CLI_ID = "org.openclaw.crabbox";
const HELPER_ID = "org.openclaw.crabbox.apple-vm-helper";
const VMD_ID = "org.openclaw.crabbox.apple-vm-vmd";
const CURRENT_TOOLCHAIN = releaseToolchainPolicy(path.resolve(import.meta.dirname, ".."));
const CANDIDATE_MANIFEST = ".components/candidate-manifest.json";
const VMD_COMPONENT = ".components/crabbox-apple-vm-vmd";
const VMD_ENTITLEMENTS_SHA256 = crypto
  .createHash("sha256")
  .update(fs.readFileSync(new URL("../internal/applevmhelper/vmd-entitlements.plist", import.meta.url)))
  .digest("hex");
const RELEASE_CONFIG_SHA256 = crypto
  .createHash("sha256")
  .update(fs.readFileSync(new URL("../.goreleaser.yaml", import.meta.url)))
  .digest("hex");

function parseArgs(argv) {
  const command = argv.shift();
  const args = {};
  while (argv.length > 0) {
    const key = argv.shift();
    if (!key?.startsWith("--") || argv.length === 0) throw new Error(`invalid argument: ${key}`);
    args[key.slice(2)] = argv.shift();
  }
  return { command, args };
}

function sha256(file) {
  return crypto.createHash("sha256").update(fs.readFileSync(file)).digest("hex");
}

function fileMode(stat) {
  return (stat.mode & 0o7777).toString(8).padStart(4, "0");
}

function candidateInput(directory, relativePath, kind) {
  const file = path.join(directory, ...relativePath.split("/"));
  const stat = fs.lstatSync(file);
  if (!stat.isFile() || stat.isSymbolicLink() || stat.size <= 0) {
    throw new Error(`candidate input must be a nonempty regular file: ${relativePath}`);
  }
  const mode = fileMode(stat);
  if (kind === "embedded-vmd" && (Number.parseInt(mode, 8) & 0o111) === 0) {
    throw new Error("candidate Apple VM daemon must be executable");
  }
  return { path: relativePath, kind, size: stat.size, mode, sha256: sha256(file) };
}

function candidateInputs(directory, version) {
  return [
    ...expectedArchives(version).map((name) => candidateInput(directory, name, "archive")),
    candidateInput(directory, VMD_COMPONENT, "embedded-vmd"),
  ];
}

function expectedArchives(version) {
  return [
    `crabbox_${version}_darwin_amd64.tar.gz`,
    `crabbox_${version}_darwin_arm64.tar.gz`,
    `crabbox_${version}_linux_amd64.tar.gz`,
    `crabbox_${version}_linux_arm64.tar.gz`,
    `crabbox_${version}_windows_amd64.zip`,
    `crabbox_${version}_windows_arm64.zip`,
  ];
}

function assertSha(name, value) {
  if (!/^[0-9a-f]{40}$/.test(value ?? "")) throw new Error(`${name} must be a full lowercase SHA-1`);
}

function releaseNotes(notesFile) {
  const bytes = fs.readFileSync(notesFile);
  if (bytes.length === 0) throw new Error("release notes are empty");
  return { bytes: bytes.length, sha256: crypto.createHash("sha256").update(bytes).digest("hex") };
}

function runtimePackExpected(args, actual = false) {
  const requested = args["runtime-pack"];
  if (requested !== undefined && !["true", "false", "filesystem"].includes(requested)) {
    throw new Error("--runtime-pack must be true, false or filesystem");
  }
  const expected = requested === "filesystem" ? "filesystem" : requested === "true";
  if ((requested !== undefined && expected !== actual) ||
      (actual === "filesystem" && requested !== "filesystem")) {
    throw new Error("runtime pack layout does not match the frozen source capability");
  }
  return actual;
}

function runtimeMode(schemaVersion) {
  return schemaVersion === 3 ? "filesystem" : schemaVersion === 2;
}

function releaseSchema(runtimePack) {
  return runtimePack === "filesystem" ? 3 : runtimePack ? 2 : 1;
}

function filesystemBuildId(value) {
  if (typeof value !== "string" || !/^[0-9a-f]{64}$/.test(value)) {
    throw new Error("filesystem build ID must be a full lowercase SHA-256");
  }
  return value;
}

// Reports are produced afresh by protected runtime-artifacts extraction in the
// caller's private staging directory. Their archive identity prevents mixing
// reports between payloads; they are not a substitute for that extraction gate.
function runtimePackReport(directory, reports, name, platform, arch, buildId) {
  if (!reports) throw new Error("runtime pack layout requires --runtime-reports from protected extraction");
  const file = path.join(reports, `${name}.json`);
  const stat = fs.lstatSync(file);
  if (!stat.isFile() || stat.isSymbolicLink() || stat.size > 65536) {
    throw new Error("runtime extraction report must be a bounded regular file");
  }
  const report = JSON.parse(fs.readFileSync(file, "utf8"));
  assertExactKeys(report, ["name", "size", "sha256", "os", "arch", "runtimePack"], "runtime extraction report");
  const archive = path.join(directory, name);
  if (report.name !== name || report.os !== platform || report.arch !== arch ||
      report.size !== fs.statSync(archive).size || report.sha256 !== sha256(archive)) {
    throw new Error(`runtime extraction report does not match archive: ${name}`);
  }
  const pack = report.runtimePack;
  assertExactKeys(pack, [buildId ? "schemaVersion" : "protocolVersion", "controllerSha256", "manifest", "artifacts"], "runtime pack");
  if ((buildId ? pack.schemaVersion !== 2 : pack.protocolVersion !== "CBX-REMOTE-1") || !/^[0-9a-f]{64}$/.test(pack.controllerSha256 ?? "")) {
    throw new Error("runtime pack protocol or controller identity is invalid");
  }
  const assertFile = (entry, expectedPath, maximum, label, extra = []) => {
    assertExactKeys(entry, ["path", "size", "sha256", ...extra], label);
    if (entry.path !== expectedPath || !Number.isSafeInteger(entry.size) || entry.size <= 0 ||
        entry.size > maximum || !/^[0-9a-f]{64}$/.test(entry.sha256 ?? "")) {
      throw new Error(`${label} identity is invalid`);
    }
  };
  assertFile(pack.manifest, "crabbox-runtime/manifest.json", 65536, "runtime manifest");
  const targets = buildId
    ? ["darwin", "linux", "windows"].flatMap((os) => ["amd64", "arm64"].map((arch) => ({ os, arch })))
    : ["amd64", "arm64"].map((arch) => ({ os: "linux", arch }));
  if (!Array.isArray(pack.artifacts) || pack.artifacts.length !== targets.length) {
    throw new Error("runtime artifact inventory is not exact");
  }
  for (const [index, target] of targets.entries()) {
    const entry = pack.artifacts[index];
    const suffix = target.os === "windows" ? ".exe" : "";
    assertFile(entry, `crabbox-runtime/${target.os}-${target.arch}${suffix}`, 64 * 1024 * 1024,
      "runtime artifact", ["os", "arch", ...(buildId ? ["capabilities"] : [])]);
    if (entry.os !== target.os || entry.arch !== target.arch) throw new Error("runtime artifact target is invalid");
    if (buildId) {
      const capabilities = [{ name: "filesystem", protocolVersion: "1", buildId }];
      if (target.os === "linux") capabilities.push({ name: "supervisor", protocolVersion: "CBX-REMOTE-1" });
      if (!Array.isArray(entry.capabilities) || entry.capabilities.length !== capabilities.length) {
        throw new Error("runtime capability inventory is not exact");
      }
      for (const [position, expected] of capabilities.entries()) {
        const actual = entry.capabilities[position];
        assertExactKeys(actual, Object.keys(expected), "runtime capability");
        if (Object.entries(expected).some(([key, value]) => actual[key] !== value)) {
          throw new Error("runtime capability identity is invalid");
        }
      }
    }
  }
  return pack;
}

function runtimeSignatures(payloads, notaryIds) {
  return ["amd64", "arm64"].map((arch) => {
    const name = `crabbox-runtime/darwin-${arch}`;
    const artifact = payloads[0]?.runtimePack?.artifacts.find((entry) => entry.path === name);
    if (!artifact || !payloads.every((payload) => payload.runtimePack.artifacts.some(
      (entry) => entry.path === name && entry.sha256 === artifact.sha256 && entry.size === artifact.size))) {
      throw new Error("Darwin runtime companion identity differs between archives");
    }
    const notarizationSubmissionId = notaryIds[`runtime-${arch}`];
    if (!isNotaryId(notarizationSubmissionId)) throw new Error("invalid runtime notarization submission ID");
    return { name, arch, sha256: artifact.sha256, size: artifact.size,
      identifier: RUNTIME_ID, teamId: TEAM_ID, hardenedRuntime: true,
      timestamp: true, notarized: true, notarizationSubmissionId };
  });
}

function payloadFor(directory, name, version, embeddedVmd, notaryIds = {}, runtimeReports, buildId) {
  const match = new RegExp(
    `^crabbox_${version.replaceAll(".", "\\.")}_(darwin|linux|windows)_(amd64|arm64)\\.(tar\\.gz|zip)$`,
  ).exec(name);
  if (!match) throw new Error(`unexpected archive name ${name}`);
  const [, platform, arch, format] = match;
  const binaries = [
    {
      name: platform === "windows" ? "crabbox.exe" : "crabbox",
      package: "github.com/openclaw/crabbox/cmd/crabbox",
      ...(platform === "darwin"
        ? {
            identifier: CLI_ID,
            teamId: TEAM_ID,
            hardenedRuntime: true,
            timestamp: true,
            notarized: true,
            notarizationSubmissionId: notaryIds[`cli-${arch}`],
          }
        : {}),
    },
  ];
  if (platform === "darwin" && arch === "arm64") {
    binaries.push({
      name: "crabbox-apple-vm-helper",
      package: "github.com/openclaw/crabbox/cmd/crabbox-apple-vm-helper",
      identifier: HELPER_ID,
      teamId: TEAM_ID,
      hardenedRuntime: true,
      timestamp: true,
      notarized: true,
      notarizationSubmissionId: notaryIds["helper-arm64"],
      embeddedVmd,
    });
  }
  const file = path.join(directory, name);
  return { name, sha256: sha256(file), size: fs.statSync(file).size, platform, arch, format, binaries,
    ...(runtimeReports ? { runtimePack: runtimePackReport(directory, runtimeReports, name, platform, arch, buildId) } : {}),
  };
}

function exactJson(value) {
  return `${JSON.stringify(value, null, 2)}\n`;
}

function isNotaryId(value) {
  return /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(
    value ?? "",
  );
}

function assertExactKeys(value, expected, label) {
  if (
    !value ||
    typeof value !== "object" ||
    Array.isArray(value) ||
    JSON.stringify(Object.keys(value).sort()) !== JSON.stringify([...expected].sort())
  ) {
    throw new Error(`${label} fields do not match the exact provenance schema`);
  }
}

function assertCandidateInventory(directory, version, manifestPresent) {
  const top = fs.readdirSync(directory, { withFileTypes: true });
  const topNames = top.map((entry) => entry.name).sort();
  const expectedTop = [...expectedArchives(version), ".components"].sort();
  if (JSON.stringify(topNames) !== JSON.stringify(expectedTop)) {
    throw new Error("candidate top-level inventory is not exact");
  }
  for (const entry of top) {
    if (entry.name === ".components" ? !entry.isDirectory() : !entry.isFile()) {
      throw new Error(`candidate input has unexpected file type: ${entry.name}`);
    }
  }
  const componentsDir = path.join(directory, ".components");
  const components = fs.readdirSync(componentsDir, { withFileTypes: true });
  const componentNames = components.map((entry) => entry.name).sort();
  const expectedComponents = manifestPresent
    ? [path.basename(CANDIDATE_MANIFEST), path.basename(VMD_COMPONENT)].sort()
    : [path.basename(VMD_COMPONENT)];
  if (JSON.stringify(componentNames) !== JSON.stringify(expectedComponents)) {
    throw new Error("candidate private-component inventory is not exact");
  }
  if (components.some((entry) => !entry.isFile())) {
    throw new Error("candidate private components must be regular files");
  }
}

function assertProducer(value, expectedConfigSha256 = RELEASE_CONFIG_SHA256, policy = CURRENT_TOOLCHAIN) {
  assertExactKeys(
    value,
    [
      "arch",
      "go",
      "goreleaser",
      "platform",
      "releaseConfigSha256",
      "swift",
      "xcodeBuild",
      "xcodeVersion",
    ],
    "candidate producer",
  );
  if (
    typeof value.platform !== "string" ||
    !/^\d+(?:\.\d+){1,2}$/.test(value.platform) ||
    value.arch !== "arm64" ||
    value.go !== policy.go ||
    value.goreleaser !== policy.goreleaser ||
    typeof value.swift !== "string" ||
    !/^Apple Swift version [^\r\n]+$/.test(value.swift) ||
    typeof value.xcodeVersion !== "string" ||
    !/^\d+(?:\.\d+){0,2}$/.test(value.xcodeVersion) ||
    typeof value.xcodeBuild !== "string" ||
    !/^[A-Za-z0-9.]+$/.test(value.xcodeBuild) ||
    value.releaseConfigSha256 !== expectedConfigSha256
  ) {
    throw new Error("candidate producer does not match the pinned toolchain contract");
  }
}

function assertPackager(value, policy = CURRENT_TOOLCHAIN) {
  assertExactKeys(
    value,
    ["arch", "go", "platform", "xcodeBuild", "xcodeVersion"],
    "release packager",
  );
  if (
    typeof value.platform !== "string" ||
    !/^\d+(?:\.\d+){1,2}$/.test(value.platform) ||
    value.arch !== "arm64" ||
    value.go !== policy.go ||
    typeof value.xcodeVersion !== "string" ||
    !/^\d+(?:\.\d+){0,2}$/.test(value.xcodeVersion) ||
    typeof value.xcodeBuild !== "string" ||
    !/^[A-Za-z0-9.]+$/.test(value.xcodeBuild)
  ) {
    throw new Error("release packager does not match the pinned toolchain contract");
  }
}

function assertCandidateInputRecords(value, directory, version) {
  if (!Array.isArray(value) || value.length !== 7) {
    throw new Error("candidate input inventory must contain exactly seven files");
  }
  for (const entry of value) {
    assertExactKeys(entry, ["kind", "mode", "path", "sha256", "size"], "candidate input");
    if (
      !["archive", "embedded-vmd"].includes(entry.kind) ||
      !/^[0-7]{4}$/.test(entry.mode ?? "") ||
      !/^[0-9a-f]{64}$/.test(entry.sha256 ?? "") ||
      !Number.isSafeInteger(entry.size) ||
      entry.size <= 0
    ) {
      throw new Error("candidate input metadata is invalid");
    }
  }
  const actual = candidateInputs(directory, version);
  if (JSON.stringify(value) !== JSON.stringify(actual)) {
    throw new Error("candidate input bytes, sizes, or modes do not match their manifest");
  }
}

function assertRecordedCandidateInputs(value, version) {
  const expectedPaths = [...expectedArchives(version), VMD_COMPONENT];
  if (!Array.isArray(value) || value.length !== expectedPaths.length) {
    throw new Error("recorded candidate input inventory is not exact");
  }
  for (let index = 0; index < value.length; index += 1) {
    const entry = value[index];
    assertExactKeys(entry, ["kind", "mode", "path", "sha256", "size"], "candidate input");
    const expectedKind = index === expectedPaths.length - 1 ? "embedded-vmd" : "archive";
    if (
      entry.path !== expectedPaths[index] ||
      entry.kind !== expectedKind ||
      !/^[0-7]{4}$/.test(entry.mode ?? "") ||
      !/^[0-9a-f]{64}$/.test(entry.sha256 ?? "") ||
      !Number.isSafeInteger(entry.size) ||
      entry.size <= 0 ||
      (expectedKind === "embedded-vmd" && (Number.parseInt(entry.mode, 8) & 0o111) === 0)
    ) {
      throw new Error("recorded candidate input metadata is invalid");
    }
  }
}

function assertFinalProducer(value, releaseIdentity, version, runtimePack) {
  assertExactKeys(
    value,
    [
      "arch",
      "go",
      "goreleaser",
      "inputs",
      "manifestSha256",
      "platform",
      "releaseConfigSha256",
      "swift",
      "xcodeBuild",
      "xcodeVersion",
      ...(runtimePack ? ["runtimePack"] : []),
      ...(runtimePack === "filesystem" ? ["filesystemBuildId"] : []),
    ],
    "release producer",
  );
  const { inputs, manifestSha256, runtimePack: recordedRuntimePack, filesystemBuildId: buildId, ...toolchain } = value;
  if (runtimePack === "filesystem") filesystemBuildId(buildId);
  if (runtimePack && recordedRuntimePack !== true) throw new Error("producer runtime capability is missing");
  // Reverification uses the original protected producer policy, not today's
  // working-tree config. Outer release gates establish this commit's ancestry.
  assertSha("provenance verifier commit", releaseIdentity.verifierCommit);
  const producerConfig = execFileSync("git", [
    "--no-replace-objects", "--no-lazy-fetch", "-C", path.resolve(import.meta.dirname, ".."),
    "cat-file", "blob", `${releaseIdentity.verifierCommit}:.goreleaser.yaml`,
  ], { maxBuffer: 1024 * 1024 });
  const recordedPolicy = releaseToolchainPolicy(path.resolve(import.meta.dirname, ".."), releaseIdentity.verifierCommit);
  assertProducer(toolchain, crypto.createHash("sha256").update(producerConfig).digest("hex"), recordedPolicy);
  assertRecordedCandidateInputs(inputs, version);
  if (!/^[0-9a-f]{64}$/.test(manifestSha256 ?? "")) {
    throw new Error("candidate manifest digest is invalid");
  }
  const originalManifest = {
    schemaVersion: releaseSchema(runtimePack),
    repository: REPOSITORY,
    tag: releaseIdentity.tag,
    tagObject: releaseIdentity.tagObject,
    sourceCommit: releaseIdentity.sourceCommit,
    verifierCommit: releaseIdentity.verifierCommit,
    producer: toolchain,
    inputs,
    ...(runtimePack ? { runtimePack: true } : {}),
    ...(runtimePack === "filesystem" ? { filesystemBuildId: buildId } : {}),
  };
  const actualManifestSha256 = crypto.createHash("sha256").update(exactJson(originalManifest)).digest("hex");
  if (manifestSha256 !== actualManifestSha256) {
    throw new Error("candidate manifest digest does not bind the recorded producer handoff");
  }
  return recordedPolicy;
}

function validateCandidateManifest(value, directory, args) {
  const runtimePack = runtimePackExpected(args, runtimeMode(value.schemaVersion));
  assertExactKeys(
    value,
    [
      "inputs",
      "producer",
      "repository",
      "schemaVersion",
      "sourceCommit",
      "tag",
      "tagObject",
      "verifierCommit",
      ...(runtimePack ? ["runtimePack"] : []),
      ...(runtimePack === "filesystem" ? ["filesystemBuildId"] : []),
    ],
    "candidate manifest",
  );
  const version = args.tag?.slice(1);
  if (
    value.schemaVersion !== releaseSchema(runtimePack) ||
    (runtimePack && value.runtimePack !== true) ||
    value.repository !== REPOSITORY ||
    value.tag !== args.tag ||
    value.tagObject !== args["tag-object"] ||
    value.sourceCommit !== args["source-commit"] ||
    value.verifierCommit !== args["verifier-commit"]
  ) {
    throw new Error("candidate manifest does not match the pinned release identity");
  }
  if (runtimePack === "filesystem") {
    filesystemBuildId(value.filesystemBuildId);
    if (args["filesystem-build-id"] !== undefined && value.filesystemBuildId !== filesystemBuildId(args["filesystem-build-id"])) {
      throw new Error("candidate filesystem build ID does not match protected source");
    }
  }
  assertProducer(value.producer);
  assertCandidateInputRecords(value.inputs, directory, version);
  return value;
}

function candidateRequired(args) {
  for (const required of ["dir", "tag", "tag-object", "source-commit", "verifier-commit"]) {
    if (!args[required]) throw new Error(`missing --${required}`);
  }
  if (!/^v[0-9]+\.[0-9]+\.[0-9]+$/.test(args.tag)) throw new Error("invalid stable release tag");
  assertSha("tag object", args["tag-object"]);
  assertSha("source commit", args["source-commit"]);
  assertSha("verifier commit", args["verifier-commit"]);
}

function candidateWrite(args) {
  candidateRequired(args);
  const runtimePack = runtimePackExpected(args, args["runtime-pack"] === "filesystem" ? "filesystem" : args["runtime-pack"] === "true");
  const buildId = runtimePack === "filesystem" ? filesystemBuildId(args["filesystem-build-id"]) : undefined;
  for (const required of [
    "producer-os",
    "producer-arch",
    "go-version",
    "goreleaser-version",
    "swift-version",
    "xcode-version",
    "xcode-build",
  ]) {
    if (!args[required]) throw new Error(`missing --${required}`);
  }
  const version = args.tag.slice(1);
  assertCandidateInventory(args.dir, version, false);
  const value = {
    schemaVersion: releaseSchema(runtimePack),
    repository: REPOSITORY,
    tag: args.tag,
    tagObject: args["tag-object"],
    sourceCommit: args["source-commit"],
    verifierCommit: args["verifier-commit"],
    producer: {
      platform: args["producer-os"],
      arch: args["producer-arch"],
      go: args["go-version"],
      goreleaser: args["goreleaser-version"],
      swift: args["swift-version"],
      xcodeVersion: args["xcode-version"],
      xcodeBuild: args["xcode-build"],
      releaseConfigSha256: RELEASE_CONFIG_SHA256,
    },
    inputs: candidateInputs(args.dir, version),
    ...(runtimePack ? { runtimePack: true } : {}),
    ...(runtimePack === "filesystem" ? { filesystemBuildId: buildId } : {}),
  };
  assertProducer(value.producer);
  const file = path.join(args.dir, CANDIDATE_MANIFEST);
  fs.writeFileSync(file, exactJson(value), { flag: "wx", mode: 0o600 });
  assertCandidateInventory(args.dir, version, true);
  process.stdout.write(`${sha256(file)}\n`);
}

function loadCandidateManifest(args) {
  candidateRequired(args);
  const version = args.tag.slice(1);
  assertCandidateInventory(args.dir, version, true);
  const file = path.join(args.dir, CANDIDATE_MANIFEST);
  const stat = fs.lstatSync(file);
  if (!stat.isFile() || stat.isSymbolicLink()) {
    throw new Error("candidate manifest must be a regular file");
  }
  const value = JSON.parse(fs.readFileSync(file, "utf8"));
  validateCandidateManifest(value, args.dir, args);
  return { value, sha256: sha256(file) };
}

function candidateVerify(args) {
  const loaded = loadCandidateManifest(args);
  process.stdout.write(`${loaded.sha256}\n`);
  return loaded.value;
}

function assertEmbeddedVmd(value) {
  const keys = [
    "arch",
    "entitlementsSha256",
    "hardenedRuntime",
    "identifier",
    "notarizationSubmissionId",
    "notarized",
    "sha256",
    "size",
    "teamId",
    "timestamp",
    "trustPolicyVersion",
  ];
  if (
    !value ||
    JSON.stringify(Object.keys(value).sort()) !== JSON.stringify(keys) ||
    !/^[0-9a-f]{64}$/.test(value.sha256 ?? "") ||
    !Number.isSafeInteger(value.size) ||
    value.size <= 0 ||
    value.identifier !== VMD_ID ||
    value.teamId !== TEAM_ID ||
    value.arch !== "arm64" ||
    value.hardenedRuntime !== true ||
    value.timestamp !== true ||
    value.notarized !== true ||
    !isNotaryId(value.notarizationSubmissionId) ||
    value.entitlementsSha256 !== VMD_ENTITLEMENTS_SHA256 ||
    value.trustPolicyVersion !== 1
  ) {
    throw new Error("embedded VMD provenance does not match release trust policy version 1");
  }
}

function write(args) {
  for (const required of [
    "dir",
    "tag",
    "tag-object",
    "source-commit",
    "verifier-commit",
    "notes",
    "candidate-dir",
    "candidate-manifest-sha256",
    "embedded-vmd-sha256",
    "embedded-vmd-size",
    "vmd-entitlements-sha256",
    "notary-cli-amd64",
    "notary-cli-arm64",
    "notary-helper-arm64",
    "notary-vmd-arm64",
    "packager-go-version",
    "packager-os",
    "packager-arch",
    "packager-xcode-version",
    "packager-xcode-build",
  ]) {
    if (!args[required]) throw new Error(`missing --${required}`);
  }
  if (!/^v[0-9]+\.[0-9]+\.[0-9]+$/.test(args.tag)) throw new Error("invalid stable release tag");
  assertSha("tag object", args["tag-object"]);
  assertSha("source commit", args["source-commit"]);
  assertSha("verifier commit", args["verifier-commit"]);
  if (!/^[0-9a-f]{64}$/.test(args["embedded-vmd-sha256"])) {
    throw new Error("embedded VMD SHA-256 is invalid");
  }
  const embeddedVmdSize = Number(args["embedded-vmd-size"]);
  if (!Number.isSafeInteger(embeddedVmdSize) || embeddedVmdSize <= 0) {
    throw new Error("embedded VMD size is invalid");
  }
  if (args["vmd-entitlements-sha256"] !== VMD_ENTITLEMENTS_SHA256) {
    throw new Error("embedded VMD entitlements do not match the protected policy");
  }
  const notaryIds = {
    "cli-amd64": args["notary-cli-amd64"],
    "cli-arm64": args["notary-cli-arm64"],
    "helper-arm64": args["notary-helper-arm64"],
    "vmd-arm64": args["notary-vmd-arm64"],
  };
  if (
    !Object.values(notaryIds).every(isNotaryId) ||
    new Set(Object.values(notaryIds)).size !== 4
  ) {
    throw new Error("notarization submission IDs must be four distinct UUIDs");
  }
  const embeddedVmd = {
    sha256: args["embedded-vmd-sha256"],
    size: embeddedVmdSize,
    identifier: VMD_ID,
    teamId: TEAM_ID,
    arch: "arm64",
    hardenedRuntime: true,
    timestamp: true,
    notarized: true,
    notarizationSubmissionId: notaryIds["vmd-arm64"],
    entitlementsSha256: VMD_ENTITLEMENTS_SHA256,
    trustPolicyVersion: 1,
  };
  const version = args.tag.slice(1);
  const archives = expectedArchives(version);
  const releaseAssets = [...archives, "checksums.txt", "provenance.json"].sort();
  const candidate = loadCandidateManifest({
    dir: args["candidate-dir"],
    tag: args.tag,
    "tag-object": args["tag-object"],
    "source-commit": args["source-commit"],
    "verifier-commit": args["verifier-commit"],
    "runtime-pack": args["runtime-pack"],
    "filesystem-build-id": args["filesystem-build-id"],
  });
  const runtimePack = runtimePackExpected(args, runtimeMode(candidate.value.schemaVersion));
  if (runtimePack && !args["runtime-reports"]) throw new Error("runtime pack layout requires --runtime-reports");
  if (
    !/^[0-9a-f]{64}$/.test(args["candidate-manifest-sha256"]) ||
    candidate.sha256 !== args["candidate-manifest-sha256"]
  ) {
    throw new Error("candidate manifest changed after the producer handoff was pinned");
  }
  const packager = {
    platform: args["packager-os"],
    arch: args["packager-arch"],
    go: args["packager-go-version"],
    xcodeVersion: args["packager-xcode-version"],
    xcodeBuild: args["packager-xcode-build"],
  };
  assertPackager(packager);
  const provenance = {
    schemaVersion: releaseSchema(runtimePack),
    repository: REPOSITORY,
    version,
    source: {
      tag: args.tag,
      tagObject: args["tag-object"],
      commit: args["source-commit"],
      clean: true,
    },
    verifier: { commit: args["verifier-commit"] },
    releaseNotes: releaseNotes(args.notes),
    signaturePolicy: {
      authority: AUTHORITY,
      teamId: TEAM_ID,
      hardenedRuntime: true,
      timestamp: true,
      onlineNotarization: true,
      identifiers: { crabbox: CLI_ID, appleVmHelper: HELPER_ID, appleVmVmd: VMD_ID,
        ...(runtimePack === "filesystem" ? { runtime: RUNTIME_ID } : {}) },
    },
    producer: {
      manifestSha256: candidate.sha256,
      ...candidate.value.producer,
      inputs: candidate.value.inputs,
      ...(runtimePack ? { runtimePack: true } : {}),
      ...(runtimePack === "filesystem" ? { filesystemBuildId: candidate.value.filesystemBuildId } : {}),
    },
    packager,
    releaseAssets,
    payloads: archives.map((name) =>
      payloadFor(args.dir, name, version, embeddedVmd, notaryIds, runtimePack ? args["runtime-reports"] : undefined, candidate.value.filesystemBuildId),
    ),
  };
  if (runtimePack === "filesystem") {
    notaryIds["runtime-amd64"] = args["notary-runtime-amd64"];
    notaryIds["runtime-arm64"] = args["notary-runtime-arm64"];
    provenance.runtimeSignatures = runtimeSignatures(provenance.payloads, notaryIds);
    if (new Set(Object.values(notaryIds).map((id) => id.toLowerCase())).size !== 6) {
      throw new Error("notarization provenance must contain six distinct submissions");
    }
  }
  fs.writeFileSync(path.join(args.dir, "provenance.json"), exactJson(provenance), {
    flag: "wx",
    mode: 0o644,
  });
}

function verify(args) {
  for (const required of [
    "dir",
    "tag",
    "tag-object",
    "source-commit",
    "verifier-commit",
    "notes",
  ]) {
    if (!args[required]) throw new Error(`missing --${required}`);
  }
  const version = args.tag.slice(1);
  const file = path.join(args.dir, "provenance.json");
  const value = JSON.parse(fs.readFileSync(file, "utf8"));
  const runtimePack = runtimePackExpected(args, runtimeMode(value.schemaVersion));
  if (runtimePack && !args["runtime-reports"]) throw new Error("runtime pack layout requires --runtime-reports");
  const archives = expectedArchives(version);
  const releaseAssets = [...archives, "checksums.txt", "provenance.json"].sort();
  assertExactKeys(
    value,
    [
      "packager",
      "payloads",
      "producer",
      "releaseAssets",
      "releaseNotes",
      "repository",
      "schemaVersion",
      "signaturePolicy",
      ...(runtimePack === "filesystem" ? ["runtimeSignatures"] : []),
      "source",
      "verifier",
      "version",
    ],
    "top-level provenance",
  );
  assertExactKeys(value.source, ["clean", "commit", "tag", "tagObject"], "source provenance");
  assertExactKeys(value.verifier, ["commit"], "verifier provenance");
  assertExactKeys(value.releaseNotes, ["bytes", "sha256"], "release notes provenance");
  assertExactKeys(
    value.signaturePolicy,
    ["authority", "hardenedRuntime", "identifiers", "onlineNotarization", "teamId", "timestamp"],
    "signature policy",
  );
  assertExactKeys(
    value.signaturePolicy.identifiers,
    ["appleVmHelper", "appleVmVmd", "crabbox", ...(runtimePack === "filesystem" ? ["runtime"] : [])],
    "signature identifiers",
  );
  const recordedToolchain = assertFinalProducer(
    value.producer,
    {
      tag: args.tag,
      tagObject: args["tag-object"],
      sourceCommit: args["source-commit"],
      verifierCommit: args["verifier-commit"],
    },
    version,
    runtimePack,
  );
  if (runtimePack === "filesystem" && args["filesystem-build-id"] !== undefined &&
      value.producer.filesystemBuildId !== filesystemBuildId(args["filesystem-build-id"])) {
    throw new Error("producer filesystem build ID does not match protected source");
  }
  assertPackager(value.packager, recordedToolchain);
  if (
    value.schemaVersion !== releaseSchema(runtimePack) ||
    value.repository !== REPOSITORY ||
    value.version !== version ||
    value.source?.tag !== args.tag ||
    value.source?.tagObject !== args["tag-object"] ||
    value.source?.commit !== args["source-commit"] ||
    value.source?.clean !== true ||
    value.verifier?.commit !== args["verifier-commit"] ||
    JSON.stringify(value.releaseNotes) !== JSON.stringify(releaseNotes(args.notes)) ||
    value.signaturePolicy?.authority !== AUTHORITY ||
    value.signaturePolicy?.teamId !== TEAM_ID ||
    value.signaturePolicy?.hardenedRuntime !== true ||
    value.signaturePolicy?.timestamp !== true ||
    value.signaturePolicy?.onlineNotarization !== true ||
    value.signaturePolicy?.identifiers?.crabbox !== CLI_ID ||
    value.signaturePolicy?.identifiers?.appleVmHelper !== HELPER_ID ||
    value.signaturePolicy?.identifiers?.appleVmVmd !== VMD_ID ||
    (runtimePack === "filesystem" && value.signaturePolicy.identifiers.runtime !== RUNTIME_ID) ||
    JSON.stringify(value.releaseAssets) !== JSON.stringify(releaseAssets)
  ) {
    throw new Error("release provenance metadata does not match the pinned contract");
  }
  if (!Array.isArray(value.payloads) || value.payloads.length !== archives.length) {
    throw new Error("release provenance payload inventory is not exact");
  }
  const payloads = new Map(value.payloads.map((entry) => [entry.name, entry]));
  if (payloads.size !== archives.length) throw new Error("duplicate release provenance payload");
  const verifiedNotaryIds = [];
  for (const name of archives) {
    const actual = payloads.get(name);
    if (!actual) throw new Error(`missing provenance payload ${name}`);
    const helperEntry = actual.binaries?.find((entry) => entry.name === "crabbox-apple-vm-helper");
    const cliEntry = actual.binaries?.find((entry) => entry.name === "crabbox");
    const notaryIds = {
      [`cli-${actual.arch}`]: cliEntry?.notarizationSubmissionId,
      "helper-arm64": helperEntry?.notarizationSubmissionId,
      "vmd-arm64": helperEntry?.embeddedVmd?.notarizationSubmissionId,
    };
    if (helperEntry) assertEmbeddedVmd(helperEntry.embeddedVmd);
    const expected = payloadFor(
      args.dir,
      name,
      version,
      helperEntry?.embeddedVmd,
      notaryIds,
      runtimePack ? args["runtime-reports"] : undefined,
      runtimePack === "filesystem" ? value.producer.filesystemBuildId : undefined,
    );
    if (JSON.stringify(actual) !== JSON.stringify(expected)) {
      throw new Error(`provenance payload mismatch: ${name}`);
    }
    if (
      actual.platform === "darwin" &&
      !actual.binaries.every((entry) =>
        /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(
          entry.notarizationSubmissionId ?? "",
        ),
      )
    ) {
      throw new Error(`invalid notarization provenance: ${name}`);
    }
    if (actual.platform === "darwin") {
      verifiedNotaryIds.push(...actual.binaries.map((entry) => entry.notarizationSubmissionId));
      if (helperEntry) verifiedNotaryIds.push(helperEntry.embeddedVmd.notarizationSubmissionId);
    }
  }
  if (runtimePack === "filesystem") {
    const records = value.runtimeSignatures;
    if (!Array.isArray(records) || records.length !== 2) throw new Error("runtime signatures inventory is not exact");
    const ids = Object.fromEntries(records.map((entry) => [`runtime-${entry.arch}`, entry.notarizationSubmissionId]));
    if (JSON.stringify(records) !== JSON.stringify(runtimeSignatures(value.payloads, ids))) {
      throw new Error("runtime signatures do not match the pinned contract");
    }
    verifiedNotaryIds.push(...records.map((entry) => entry.notarizationSubmissionId));
  }
  const expectedSubmissions = runtimePack === "filesystem" ? 6 : 4;
  if (new Set(verifiedNotaryIds.map((id) => id.toLowerCase())).size !== expectedSubmissions ||
      verifiedNotaryIds.length !== expectedSubmissions) {
    throw new Error(`notarization provenance must contain ${expectedSubmissions} distinct submissions`);
  }
}

const { command, args } = parseArgs(process.argv.slice(2));
if (command === "candidate-write") candidateWrite(args);
else if (command === "candidate-verify") candidateVerify(args);
else if (command === "write") write(args);
else if (command === "verify") verify(args);
else throw new Error("usage: release-provenance.mjs <candidate-write|candidate-verify|write|verify> --dir ...");
