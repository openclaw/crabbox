#!/usr/bin/env node

import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { identityFromManifest, nativeTargets, nativeTarget, receiptName, validateReceipt, verifyReleaseInputs, verifyPair } from "../tools/jj-source/artifacts.mjs";
import { verifyNoticeBundle, noticeName, attributionName } from "../tools/jj-source/notice-artifacts.mjs";

const REPOSITORY = "openclaw/crabbox";
const TEAM_ID = "FWJYW4S8P8";
const AUTHORITY = `Developer ID Application: OpenClaw Foundation (${TEAM_ID})`;
const CLI_ID = "org.openclaw.crabbox";
const HELPER_ID = "org.openclaw.crabbox.apple-vm-helper";
const VMD_ID = "org.openclaw.crabbox.apple-vm-vmd";
const JJ_ID = "org.openclaw.crabbox.jj-source";
const GO_VERSION = "go1.26.4";
const GORELEASER_VERSION = "2.17.0";
const CANDIDATE_MANIFEST = ".components/candidate-manifest.json";
const VMD_COMPONENT = ".components/crabbox-apple-vm-vmd";
const NATIVE_COMPONENTS = ".components/native-helpers";
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

function schemaVersion(args) {
  if (!["1", "2"].includes(args["schema-version"])) throw new Error("--schema-version must be bound to the verified source tag (1 or 2)");
  return Number(args["schema-version"]);
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

function candidateInputs(directory, version, schema) {
  return candidateInputSpecs(version, schema).map(({ path: file, kind }) => candidateInput(directory, file, kind));
}

function candidateInputSpecs(version, schema) {
  return [
    ...expectedArchives(version).map((file) => ({ path: file, kind: "archive" })),
    { path: VMD_COMPONENT, kind: "embedded-vmd" },
    ...(schema === 2 ? nativeTargets.flatMap((target) => [
      { path: `${NATIVE_COMPONENTS}/${target.key}/${target.binaryName}`, kind: "native-helper" },
      { path: `${NATIVE_COMPONENTS}/${target.key}/${receiptName}`, kind: "native-receipt" },
      { path: `${NATIVE_COMPONENTS}/${target.key}/${noticeName}`, kind: "native-notices" },
      { path: `${NATIVE_COMPONENTS}/${target.key}/${attributionName}`, kind: "native-attribution" },
    ]) : []),
  ];
}

function nativeIdentity(args) {
  if (!args["native-source-manifest"]) throw new Error("missing --native-source-manifest from the verified source tag");
  return identityFromManifest(JSON.parse(fs.readFileSync(args["native-source-manifest"], "utf8")));
}

function assertNativeRecords(records, identity, inputs) {
  if (!Array.isArray(records) || records.length !== nativeTargets.length) throw new Error("native producer inventory must contain all six targets");
  for (let i = 0; i < nativeTargets.length; i += 1) {
    const target = nativeTargets[i];
    const entry = records[i];
    assertExactKeys(entry, ["target", "receipt", "receiptSHA256", "binarySize", "binaryMode", "format", "notices"], "native producer input");
    validateReceipt(entry.receipt, identity);
    const binary = inputs.find((item) => item.path === `${NATIVE_COMPONENTS}/${target.key}/${target.binaryName}`);
    const receipt = inputs.find((item) => item.path === `${NATIVE_COMPONENTS}/${target.key}/${receiptName}`);
    assertExactKeys(entry.notices, ["text", "attribution", "unresolved"], "native notice input");
    for (const [field, name] of [["text", noticeName], ["attribution", attributionName]]) {
      const record = entry.notices[field];
      const input = inputs.find((item) => item.path === `${NATIVE_COMPONENTS}/${target.key}/${name}`);
      assertExactKeys(record, ["name", "sha256", "size"], "native notice artifact");
      if (record.name !== name || record.sha256 !== input?.sha256 || record.size !== input?.size) {
        throw new Error("native notice facts do not bind the frozen attribution inputs");
      }
    }
    if (!Array.isArray(entry.notices.unresolved)) throw new Error("native notice facts must retain unresolved attribution");
    const expectedFormat = { format: { darwin: "mach-o", linux: "elf", windows: "pe" }[target.targetOS], architecture: target.targetArch, bits: 64 };
    if (entry.target !== target.key || entry.receipt.targetOS !== target.targetOS || entry.receipt.targetArch !== target.targetArch ||
        entry.receipt.profile !== "release" || entry.receipt.binarySHA256 !== binary?.sha256 || entry.binarySize !== binary?.size ||
        entry.binaryMode !== Number.parseInt(binary?.mode, 8) || entry.receiptSHA256 !== receipt?.sha256 ||
        entry.receiptSHA256 !== crypto.createHash("sha256").update(exactJson(entry.receipt)).digest("hex") ||
        JSON.stringify(entry.format) !== JSON.stringify(expectedFormat)) {
      throw new Error("native producer facts do not bind the frozen helper and receipt inputs");
    }
  }
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

function payloadFor(directory, name, version, embeddedVmd, notaryIds = {}, native) {
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
  if (native) {
    binaries.push({
      name: nativeTarget(platform, arch).binaryName,
      nativeInput: native.target,
      sha256: native.receipt.binarySHA256,
      size: native.binarySize,
      mode: native.binaryMode,
      format: native.format,
      receipt: { name: receiptName, sha256: native.receiptSHA256, contents: native.receipt },
      notices: native.notices,
      ...(platform === "darwin" ? {
        identifier: JJ_ID, teamId: TEAM_ID, hardenedRuntime: true, timestamp: true,
        notarized: true, notarizationSubmissionId: notaryIds[`jj-${arch}`],
      } : {}),
    });
  }
  const file = path.join(directory, name);
  return { name, sha256: sha256(file), size: fs.statSync(file).size, platform, arch, format, binaries };
}

function nativeRecordFromPayload(entry, original, identity) {
  const target = nativeTarget(original.receipt.targetOS, original.receipt.targetArch);
  const receipt = entry?.receipt?.contents;
  validateReceipt(receipt, identity);
  const expectedReceipt = { ...original.receipt, binarySHA256: entry.sha256 };
  if (entry.nativeInput !== original.target || entry.name !== target.binaryName || entry.receipt.name !== receiptName ||
      JSON.stringify(receipt) !== JSON.stringify(expectedReceipt) ||
      entry.receipt.sha256 !== crypto.createHash("sha256").update(exactJson(receipt)).digest("hex") ||
      !Number.isSafeInteger(entry.size) || entry.size <= 0 || !Number.isSafeInteger(entry.mode) ||
      entry.mode < 0 || entry.mode > 0o777 || (target.targetOS !== "windows" && (entry.mode & 0o111) === 0) ||
      JSON.stringify(entry.format) !== JSON.stringify(original.format)) {
    throw new Error("native final payload does not preserve its original build identity");
  }
  if (JSON.stringify(entry.notices) !== JSON.stringify(original.notices)) throw new Error("native final attribution changed after the producer handoff");
  const final = { target: original.target, receipt, receiptSHA256: entry.receipt.sha256,
    binarySize: entry.size, binaryMode: entry.mode, format: entry.format, notices: entry.notices };
  if (target.targetOS !== "darwin" && JSON.stringify(final) !== JSON.stringify(original)) {
    throw new Error("non-Darwin native payload changed after the producer handoff");
  }
  return final;
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

function assertCandidateInventory(directory, version, manifestPresent, schema) {
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
  const expectedComponents = [path.basename(VMD_COMPONENT),
    ...(manifestPresent ? [path.basename(CANDIDATE_MANIFEST)] : []),
    ...(schema === 2 ? ["native-helpers"] : []),
  ].sort();
  if (JSON.stringify(componentNames) !== JSON.stringify(expectedComponents)) {
    throw new Error("candidate private-component inventory is not exact");
  }
  if (components.some((entry) => entry.name === "native-helpers" ? !entry.isDirectory() : !entry.isFile())) {
    throw new Error("candidate private component types do not match the manifest");
  }
}

function assertProducer(value) {
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
    value.go !== GO_VERSION ||
    value.goreleaser !== GORELEASER_VERSION ||
    typeof value.swift !== "string" ||
    !/^Apple Swift version [^\r\n]+$/.test(value.swift) ||
    typeof value.xcodeVersion !== "string" ||
    !/^\d+(?:\.\d+){0,2}$/.test(value.xcodeVersion) ||
    typeof value.xcodeBuild !== "string" ||
    !/^[A-Za-z0-9.]+$/.test(value.xcodeBuild) ||
    value.releaseConfigSha256 !== RELEASE_CONFIG_SHA256
  ) {
    throw new Error("candidate producer does not match the pinned toolchain contract");
  }
}

function assertPackager(value) {
  assertExactKeys(
    value,
    ["arch", "go", "platform", "xcodeBuild", "xcodeVersion"],
    "release packager",
  );
  if (
    typeof value.platform !== "string" ||
    !/^\d+(?:\.\d+){1,2}$/.test(value.platform) ||
    value.arch !== "arm64" ||
    value.go !== GO_VERSION ||
    typeof value.xcodeVersion !== "string" ||
    !/^\d+(?:\.\d+){0,2}$/.test(value.xcodeVersion) ||
    typeof value.xcodeBuild !== "string" ||
    !/^[A-Za-z0-9.]+$/.test(value.xcodeBuild)
  ) {
    throw new Error("release packager does not match the pinned toolchain contract");
  }
}

function assertCandidateInputRecords(value, directory, version, schema) {
  assertRecordedCandidateInputs(value, version, schema);
  const actual = candidateInputs(directory, version, schema);
  if (JSON.stringify(value) !== JSON.stringify(actual)) {
    throw new Error("candidate input bytes, sizes, or modes do not match their manifest");
  }
}

function assertRecordedCandidateInputs(value, version, schema) {
  const expected = candidateInputSpecs(version, schema);
  if (!Array.isArray(value) || value.length !== expected.length) {
    throw new Error("recorded candidate input inventory is not exact");
  }
  for (let index = 0; index < value.length; index += 1) {
    const entry = value[index];
    assertExactKeys(entry, ["kind", "mode", "path", "sha256", "size"], "candidate input");
    const expectedKind = expected[index].kind;
    if (
      entry.path !== expected[index].path ||
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

function assertFinalProducer(value, releaseIdentity, version, identity, schema) {
  assertExactKeys(
    value,
    [
      "arch",
      "go",
      "goreleaser",
      "inputs",
      ...(schema === 2 ? ["nativeHelpers"] : []),
      "manifestSha256",
      "platform",
      "releaseConfigSha256",
      "swift",
      "xcodeBuild",
      "xcodeVersion",
    ],
    "release producer",
  );
  const { inputs, nativeHelpers, manifestSha256, ...toolchain } = value;
  assertProducer(toolchain);
  assertRecordedCandidateInputs(inputs, version, schema);
  if (schema === 2) assertNativeRecords(nativeHelpers, identity, inputs);
  if (!/^[0-9a-f]{64}$/.test(manifestSha256 ?? "")) {
    throw new Error("candidate manifest digest is invalid");
  }
  const originalManifest = {
    schemaVersion: schema,
    repository: REPOSITORY,
    tag: releaseIdentity.tag,
    tagObject: releaseIdentity.tagObject,
    sourceCommit: releaseIdentity.sourceCommit,
    verifierCommit: releaseIdentity.verifierCommit,
    producer: toolchain,
    inputs,
    ...(schema === 2 ? { nativeHelpers } : {}),
  };
  const actualManifestSha256 = crypto.createHash("sha256").update(exactJson(originalManifest)).digest("hex");
  if (manifestSha256 !== actualManifestSha256) {
    throw new Error("candidate manifest digest does not bind the recorded producer handoff");
  }
}

async function validateCandidateManifest(value, directory, args) {
  const schema = schemaVersion(args);
  assertExactKeys(
    value,
    [
      "inputs",
      ...(schema === 2 ? ["nativeHelpers"] : []),
      "producer",
      "repository",
      "schemaVersion",
      "sourceCommit",
      "tag",
      "tagObject",
      "verifierCommit",
    ],
    "candidate manifest",
  );
  const version = args.tag?.slice(1);
  if (
    value.schemaVersion !== schema ||
    value.repository !== REPOSITORY ||
    value.tag !== args.tag ||
    value.tagObject !== args["tag-object"] ||
    value.sourceCommit !== args["source-commit"] ||
    value.verifierCommit !== args["verifier-commit"]
  ) {
    throw new Error("candidate manifest does not match the pinned release identity");
  }
  assertProducer(value.producer);
  assertCandidateInputRecords(value.inputs, directory, version, schema);
  if (schema === 2) {
    const identity = nativeIdentity(args);
    assertNativeRecords(value.nativeHelpers, identity, value.inputs);
    if (JSON.stringify(value.nativeHelpers) !== JSON.stringify(await verifyReleaseInputs(path.join(directory, NATIVE_COMPONENTS), { identity }))) {
      throw new Error("native build inputs do not match the frozen candidate manifest");
    }
  }
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

async function candidateWrite(args) {
  candidateRequired(args);
  const schema = schemaVersion(args);
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
  assertCandidateInventory(args.dir, version, false, schema);
  const value = {
    schemaVersion: schema,
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
    inputs: candidateInputs(args.dir, version, schema),
    ...(schema === 2 ? { nativeHelpers: await verifyReleaseInputs(path.join(args.dir, NATIVE_COMPONENTS), { identity: nativeIdentity(args) }) } : {}),
  };
  assertProducer(value.producer);
  if (schema === 2) assertNativeRecords(value.nativeHelpers, nativeIdentity(args), value.inputs);
  assertCandidateInputRecords(value.inputs, args.dir, version, schema);
  const file = path.join(args.dir, CANDIDATE_MANIFEST);
  fs.writeFileSync(file, exactJson(value), { flag: "wx", mode: 0o600 });
  assertCandidateInventory(args.dir, version, true, schema);
  process.stdout.write(`${sha256(file)}\n`);
}

async function loadCandidateManifest(args) {
  candidateRequired(args);
  const version = args.tag.slice(1);
  assertCandidateInventory(args.dir, version, true, schemaVersion(args));
  const file = path.join(args.dir, CANDIDATE_MANIFEST);
  const stat = fs.lstatSync(file);
  if (!stat.isFile() || stat.isSymbolicLink()) {
    throw new Error("candidate manifest must be a regular file");
  }
  const value = JSON.parse(fs.readFileSync(file, "utf8"));
  await validateCandidateManifest(value, args.dir, args);
  return { value, sha256: sha256(file) };
}

async function candidateVerify(args) {
  const loaded = await loadCandidateManifest(args);
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

async function write(args) {
  const schema = schemaVersion(args);
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
    ...(schema === 2 ? { "jj-amd64": args["notary-jj-amd64"], "jj-arm64": args["notary-jj-arm64"] } : {}),
  };
  if (
    !Object.values(notaryIds).every(isNotaryId) ||
    new Set(Object.values(notaryIds)).size !== (schema === 2 ? 6 : 4)
  ) {
    throw new Error(`notarization submission IDs must be ${schema === 2 ? 6 : 4} distinct UUIDs`);
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
  const candidate = await loadCandidateManifest({
    dir: args["candidate-dir"],
    tag: args.tag,
    "tag-object": args["tag-object"],
    "source-commit": args["source-commit"],
    "verifier-commit": args["verifier-commit"],
    "native-source-manifest": args["native-source-manifest"],
    "schema-version": args["schema-version"],
  });
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
  let nativeFinal;
  if (schema === 2) {
    if (!args["native-final-dir"]) throw new Error("missing --native-final-dir");
    nativeFinal = await verifyReleaseInputs(args["native-final-dir"], { identity: nativeIdentity(args), originals: candidate.value.nativeHelpers });
    for (let i = 0; i < nativeTargets.length; i += 1) {
      const target = nativeTargets[i];
      const entry = payloadFor(args.dir, archives[i], version, embeddedVmd, notaryIds, nativeFinal[i]).binaries.find((item) => item.name === target.binaryName);
      nativeRecordFromPayload(entry, candidate.value.nativeHelpers[i], nativeIdentity(args));
    }
  }
  assertPackager(packager);
  const provenance = {
    schemaVersion: schema,
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
      identifiers: { crabbox: CLI_ID, appleVmHelper: HELPER_ID, appleVmVmd: VMD_ID, ...(schema === 2 ? { jjSource: JJ_ID } : {}) },
    },
    producer: {
      manifestSha256: candidate.sha256,
      ...candidate.value.producer,
      inputs: candidate.value.inputs,
      ...(schema === 2 ? { nativeHelpers: candidate.value.nativeHelpers } : {}),
    },
    packager,
    releaseAssets,
    payloads: archives.map((name, index) =>
      payloadFor(args.dir, name, version, embeddedVmd, notaryIds, nativeFinal?.[index]),
    ),
  };
  fs.writeFileSync(path.join(args.dir, "provenance.json"), exactJson(provenance), {
    flag: "wx",
    mode: 0o644,
  });
}

function verify(args) {
  const schema = schemaVersion(args);
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
    ["appleVmHelper", "appleVmVmd", "crabbox", ...(schema === 2 ? ["jjSource"] : [])],
    "signature identifiers",
  );
  assertFinalProducer(
    value.producer,
    {
      tag: args.tag,
      tagObject: args["tag-object"],
      sourceCommit: args["source-commit"],
      verifierCommit: args["verifier-commit"],
    },
    version,
    schema === 2 ? nativeIdentity(args) : undefined,
    schema,
  );
  assertPackager(value.packager);
  if (
    value.schemaVersion !== schema ||
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
    (schema === 2 && value.signaturePolicy?.identifiers?.jjSource !== JJ_ID) ||
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
    const original = schema === 2 ? value.producer.nativeHelpers.find((entry) => entry.target === `${actual.platform}_${actual.arch}`) : undefined;
    const nativeEntry = original ? actual.binaries?.find((entry) => entry.nativeInput === original.target) : undefined;
    const native = original ? nativeRecordFromPayload(nativeEntry, original, nativeIdentity(args)) : undefined;
    const notaryIds = {
      [`cli-${actual.arch}`]: cliEntry?.notarizationSubmissionId,
      "helper-arm64": helperEntry?.notarizationSubmissionId,
      "vmd-arm64": helperEntry?.embeddedVmd?.notarizationSubmissionId,
      [`jj-${actual.arch}`]: nativeEntry?.notarizationSubmissionId,
    };
    if (helperEntry) assertEmbeddedVmd(helperEntry.embeddedVmd);
    const expected = payloadFor(
      args.dir,
      name,
      version,
      helperEntry?.embeddedVmd,
      notaryIds,
      native,
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
  const notaryCount = schema === 2 ? 6 : 4;
  if (new Set(verifiedNotaryIds).size !== notaryCount || verifiedNotaryIds.length !== notaryCount) {
    throw new Error(`notarization provenance must contain ${notaryCount} distinct submissions`);
  }
}

async function verifyNativePayload(args) {
  if (schemaVersion(args) !== 2) throw new Error("native payload verification requires schema 2");
  const target = nativeTarget(args.platform, args.arch);
  const provenance = JSON.parse(fs.readFileSync(args.provenance, "utf8"));
  const original = provenance.producer.nativeHelpers.find((entry) => entry.target === target.key);
  const payload = provenance.payloads.find((entry) => entry.platform === target.targetOS && entry.arch === target.targetArch);
  const entry = payload?.binaries.find((item) => item.name === target.binaryName);
  const identity = nativeIdentity(args);
  const expected = nativeRecordFromPayload(entry, original, identity);
  const pair = await verifyPair(args.dir, { target, identity, release: true, coinstalled: true });
  const actual = { ...pair, notices: await verifyNoticeBundle(args.dir, original.receipt, { coinstalled: true }) };
  if (JSON.stringify(actual) !== JSON.stringify(expected)) throw new Error("installed native helper bytes or receipt do not match release provenance");
}

const { command, args } = parseArgs(process.argv.slice(2));
if (command === "candidate-write") await candidateWrite(args);
else if (command === "candidate-verify") await candidateVerify(args);
else if (command === "write") await write(args);
else if (command === "verify") verify(args);
else if (command === "verify-native-payload") await verifyNativePayload(args);
else throw new Error("usage: release-provenance.mjs <candidate-write|candidate-verify|write|verify> --dir ...");
