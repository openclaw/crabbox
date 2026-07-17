#!/usr/bin/env node

import crypto from "node:crypto";
import fs from "node:fs";

function fail(message) {
  throw new Error(message);
}

function requireValue(name) {
  const value = process.env[name];
  if (!value) fail(`${name} is required`);
  return value;
}

function readJson(file, label) {
  try {
    return JSON.parse(fs.readFileSync(file, "utf8"));
  } catch (error) {
    fail(`${label} is not valid JSON: ${error.message}`);
  }
}

function positiveSafeInteger(value, label) {
  if (!Number.isSafeInteger(value) || value <= 0) fail(`${label} must be a positive safe integer`);
  return value;
}

function timestamp(value, label) {
  if (typeof value !== "string" || !Number.isFinite(Date.parse(value))) {
    fail(`${label} must be an RFC 3339 timestamp`);
  }
  return Date.parse(value);
}

function exactEqual(actual, expected, message) {
  if (JSON.stringify(actual) !== JSON.stringify(expected)) fail(message);
}

function sha256(value) {
  return crypto.createHash("sha256").update(value).digest("hex");
}

const metadata = {
  repository: requireValue("CRABBOX_PUBLISH_REPOSITORY"),
  releaseId: Number(requireValue("CRABBOX_PUBLISH_RELEASE_ID")),
  tag: requireValue("CRABBOX_PUBLISH_TAG"),
  tagObject: requireValue("CRABBOX_PUBLISH_TAG_OBJECT"),
  sourceCommit: requireValue("CRABBOX_PUBLISH_SOURCE_COMMIT"),
  verifierCommit: requireValue("CRABBOX_PUBLISH_VERIFIER_COMMIT"),
  verifierRunId: Number(requireValue("CRABBOX_PUBLISH_VERIFIER_RUN_ID")),
  defaultBranch: requireValue("CRABBOX_PUBLISH_DEFAULT_BRANCH"),
  workflowPath: requireValue("CRABBOX_PUBLISH_WORKFLOW_PATH"),
  workflowName: "Verify Release Assets",
};
positiveSafeInteger(metadata.releaseId, "release ID");
positiveSafeInteger(metadata.verifierRunId, "verifier run ID");

function expectedAssetNames(file) {
  const names = fs.readFileSync(file, "utf8").split("\n").filter(Boolean);
  const sorted = [...names].sort((a, b) => a.localeCompare(b));
  if (names.length !== 8 || new Set(names).size !== names.length) {
    fail("expected release inventory must contain exactly eight unique assets");
  }
  exactEqual(names, sorted, "expected release inventory must be sorted");
  return names;
}

function releaseAssets(release, expectedNames) {
  if (!Array.isArray(release.assets) || release.assets.length !== expectedNames.length) {
    fail("release does not contain exactly eight assets");
  }
  const assets = release.assets.map((asset) => {
    positiveSafeInteger(asset.id, `asset ID for ${asset.name ?? "<unnamed>"}`);
    if (typeof asset.name !== "string" || !expectedNames.includes(asset.name)) {
      fail(`unexpected release asset name: ${asset.name ?? "<missing>"}`);
    }
    positiveSafeInteger(asset.size, `asset size for ${asset.name}`);
    if (asset.state !== "uploaded") fail(`release asset is not uploaded: ${asset.name}`);
    if (typeof asset.digest !== "string" || !/^sha256:[0-9a-f]{64}$/.test(asset.digest)) {
      fail(`release asset digest is not an exact SHA-256: ${asset.name}`);
    }
    timestamp(asset.updated_at, `updated_at for ${asset.name}`);
    const expectedApiUrl = `https://api.github.com/repos/${metadata.repository}/releases/assets/${asset.id}`;
    if (asset.url !== expectedApiUrl) {
      fail(`release asset URL does not match its immutable identity: ${asset.name}`);
    }
    return {
      id: asset.id,
      name: asset.name,
      size: asset.size,
      sha256: asset.digest.slice("sha256:".length),
      updatedAt: asset.updated_at,
    };
  });
  assets.sort((a, b) => a.name.localeCompare(b.name));
  exactEqual(
    assets.map((asset) => asset.name),
    expectedNames,
    "release asset names do not match the exact eight-asset inventory",
  );
  if (new Set(assets.map((asset) => asset.id)).size !== assets.length)
    fail("release asset IDs are not unique");
  return assets;
}

function validateReleaseIdentity(release, expectedNames, expectedNotes, expectedState, snapshot) {
  positiveSafeInteger(release.id, "release ID");
  if (
    release.id !== metadata.releaseId ||
    release.tag_name !== metadata.tag ||
    release.name !== metadata.tag ||
    release.body !== expectedNotes ||
    release.prerelease !== false
  ) {
    fail("release ID, tag, target, title, body, or prerelease state drifted");
  }
  const createdAt = release.created_at;
  const updatedAt = release.updated_at;
  timestamp(createdAt, "release created_at");
  timestamp(updatedAt, "release updated_at");
  if (snapshot && createdAt !== snapshot.createdAt) fail("release creation timestamp drifted");

  if (expectedState === "draft") {
    if (release.draft !== true || release.immutable !== false || release.published_at !== null)
      fail("release is not the exact unpublished draft");
    if (snapshot && updatedAt !== snapshot.releaseUpdatedAt)
      fail("draft release timestamp drifted after verification");
  } else if (expectedState === "published") {
    if (release.draft !== false || release.immutable !== true)
      fail("release publication response is not immutable");
    timestamp(release.published_at, "release published_at");
    if (snapshot && Date.parse(updatedAt) < Date.parse(snapshot.releaseUpdatedAt)) {
      fail("published release timestamp predates the verified draft");
    }
  } else {
    fail(`unsupported release state: ${expectedState}`);
  }

  const assets = releaseAssets(release, expectedNames);
  if (snapshot)
    exactEqual(assets, snapshot.assets, "release asset identity drifted after native verification");
  return {
    releaseId: release.id,
    tag: release.tag_name,
    targetCommitish: release.target_commitish,
    title: release.name,
    bodySha256: sha256(Buffer.from(release.body, "utf8")),
    draft: release.draft,
    immutable: release.immutable,
    prerelease: release.prerelease,
    createdAt,
    updatedAt,
    publishedAt: release.published_at,
    assets,
  };
}

function validateWorkflowRun(run, workflow, state = "draft") {
  if (state !== "draft" && state !== "published") fail(`unsupported verifier state: ${state}`);
  const expectedTitle = `Verify ${state} ${metadata.releaseId} for ${metadata.tag} at ${metadata.verifierCommit}`;
  const acceptedRunNames = new Set([metadata.workflowName, expectedTitle]);
  if (
    run.id !== metadata.verifierRunId ||
    !acceptedRunNames.has(run.name) ||
    run.display_title !== expectedTitle ||
    run.path !== metadata.workflowPath ||
    run.event !== "workflow_dispatch" ||
    run.status !== "completed" ||
    run.conclusion !== "success" ||
    run.head_branch !== metadata.defaultBranch ||
    run.head_sha !== metadata.verifierCommit ||
    run.repository?.full_name !== metadata.repository ||
    run.head_repository?.full_name !== metadata.repository ||
    run.head_commit?.id !== metadata.verifierCommit
  ) {
    fail("native verifier run does not match the exact protected workflow identity");
  }
  positiveSafeInteger(run.workflow_id, "workflow ID");
  positiveSafeInteger(run.run_attempt, "workflow run attempt");
  const createdAt = timestamp(run.created_at, "workflow run created_at");
  const startedAt = timestamp(run.run_started_at, "workflow run started_at");
  if (startedAt < createdAt) fail("workflow run started before it was created");

  if (
    workflow.id !== run.workflow_id ||
    workflow.name !== metadata.workflowName ||
    workflow.path !== metadata.workflowPath ||
    workflow.state !== "active" ||
    run.workflow_url !== workflow.url
  ) {
    fail("workflow metadata does not match the protected verifier path");
  }
  return startedAt;
}

function validateArtifactList(value) {
  if (value.total_count !== 3 || !Array.isArray(value.artifacts) || value.artifacts.length !== 3) {
    fail("native verifier run must contain one opaque input and exactly two proof artifacts");
  }
  const artifacts = [...value.artifacts].sort((a, b) => a.name.localeCompare(b.name));
  exactEqual(
    artifacts.map((artifact) => artifact.name),
    ["release-input", "verified-assets-arm64", "verified-assets-x86_64"],
    "native verifier input/proof artifacts are incomplete or ambiguous",
  );
  for (const artifact of artifacts) {
    positiveSafeInteger(artifact.id, `artifact ID for ${artifact.name}`);
    positiveSafeInteger(artifact.size_in_bytes, `artifact size for ${artifact.name}`);
    if (artifact.expired !== false) fail(`native proof artifact expired: ${artifact.name}`);
    if (typeof artifact.digest !== "string" || !/^sha256:[0-9a-f]{64}$/.test(artifact.digest)) {
      fail(`native proof artifact digest is invalid: ${artifact.name}`);
    }
    if (
      artifact.workflow_run?.id !== metadata.verifierRunId ||
      artifact.workflow_run?.head_branch !== metadata.defaultBranch ||
      artifact.workflow_run?.head_sha !== metadata.verifierCommit
    ) {
      fail(`native proof artifact is not bound to the selected verifier run: ${artifact.name}`);
    }
    timestamp(artifact.created_at, `created_at for ${artifact.name}`);
    timestamp(artifact.updated_at, `updated_at for ${artifact.name}`);
  }
  if (new Set(artifacts.map((artifact) => artifact.id)).size !== 3)
    fail("native verifier artifact IDs are not unique");
}

function proofAssets(manifest, arch) {
  if (!Array.isArray(manifest.assets) || manifest.assets.length !== 8) {
    fail(`${arch} proof does not contain exactly eight assets`);
  }
  const assets = manifest.assets.map((asset) => {
    positiveSafeInteger(asset.id, `${arch} proof asset ID`);
    positiveSafeInteger(asset.size, `${arch} proof asset size`);
    if (typeof asset.name !== "string" || !/^[0-9a-f]{64}$/.test(asset.sha256 ?? "")) {
      fail(`${arch} proof asset identity is invalid`);
    }
    timestamp(asset.updatedAt, `${arch} proof asset updatedAt`);
    return {
      id: asset.id,
      name: asset.name,
      size: asset.size,
      sha256: asset.sha256,
      updatedAt: asset.updatedAt,
    };
  });
  const sorted = [...assets].sort((a, b) => a.name.localeCompare(b.name));
  exactEqual(assets, sorted, `${arch} proof assets must use deterministic name ordering`);
  if (
    new Set(assets.map((asset) => asset.name)).size !== 8 ||
    new Set(assets.map((asset) => asset.id)).size !== 8
  ) {
    fail(`${arch} proof asset names and IDs must be unique`);
  }
  return assets;
}

function validateProof(manifest, arch, expectedRecord, state = "draft") {
  const manifestForHash = { ...manifest };
  delete manifestForHash.manifestSha256;
  if (
    typeof manifest.manifestSha256 !== "string" ||
    manifest.manifestSha256 !== sha256(Buffer.from(JSON.stringify(manifestForHash), "utf8"))
  ) {
    fail(`${arch} proof manifest digest is invalid`);
  }
  if (
    manifest.schemaVersion !== 1 ||
    manifest.state !== state ||
    manifest.repository !== metadata.repository ||
    manifest.releaseId !== metadata.releaseId ||
    manifest.tag !== metadata.tag ||
    manifest.tagObject !== metadata.tagObject ||
    manifest.sourceCommit !== metadata.sourceCommit ||
    manifest.verifierCommit !== metadata.verifierCommit ||
    manifest.verifierArch !== arch
  ) {
    fail(`${arch} proof does not bind the exact ${state} release and protected verifier inputs`);
  }
  const releaseRecord = {
    title: manifest.title,
    targetCommitish: manifest.targetCommitish,
    notesBytes: manifest.notesBytes,
    notesSha256: manifest.notesSha256,
    publishedAt: manifest.publishedAt,
    releaseUpdatedAt: manifest.releaseUpdatedAt,
    assets: proofAssets(manifest, arch),
  };
  exactEqual(releaseRecord, expectedRecord, `${arch} proof ${state} record differs from current release`);
  return releaseRecord;
}

function publicProof(args) {
  if (args.length !== 9) {
    fail(
      "usage: validate-release-publication.mjs public-proof <release> <run> <workflow> <artifacts> <arm-proof> <x86-proof> <asset-names> <notes> <asset-directory>",
    );
  }
  const [releaseFile, runFile, workflowFile, artifactsFile, armFile, x86File, namesFile, notesFile, assetDirectory] =
    args;
  const names = expectedAssetNames(namesFile);
  const notes = fs.readFileSync(notesFile, "utf8");
  const release = readJson(releaseFile, "public release");
  const current = validateReleaseIdentity(release, names, notes, "published");
  const runStartedAt = validateWorkflowRun(
    readJson(runFile, "public native verifier run"),
    readJson(workflowFile, "public native verifier workflow"),
    "published",
  );
  validateArtifactList(readJson(artifactsFile, "public native verifier artifacts"));
  for (const [label, value] of [
    ["release updated_at", release.updated_at],
    ["release published_at", release.published_at],
    ...current.assets.map((asset) => [`updatedAt for ${asset.name}`, asset.updatedAt]),
  ]) {
    if (runStartedAt <= timestamp(value, label)) {
      fail(`public native verifier run is not newer than ${label}`);
    }
  }
  const expectedRecord = {
    title: current.title,
    targetCommitish: current.targetCommitish,
    notesBytes: Buffer.byteLength(notes, "utf8"),
    notesSha256: current.bodySha256,
    publishedAt: current.publishedAt,
    releaseUpdatedAt: release.updated_at,
    assets: current.assets,
  };
  const armProof = validateProof(
    readJson(armFile, "arm64 public native proof"),
    "arm64",
    expectedRecord,
    "published",
  );
  const x86Proof = validateProof(
    readJson(x86File, "x86_64 public native proof"),
    "x86_64",
    expectedRecord,
    "published",
  );
  exactEqual(armProof, x86Proof, "public native proofs do not bind the same exact release");

  const actualNames = fs.readdirSync(assetDirectory).sort((a, b) => a.localeCompare(b));
  exactEqual(actualNames, names, "local public asset inventory is not exact");
  for (const asset of current.assets) {
    const file = `${assetDirectory}/${asset.name}`;
    const stat = fs.lstatSync(file);
    if (!stat.isFile() || stat.isSymbolicLink() || stat.size !== asset.size) {
      fail(`local public asset identity differs from GitHub: ${asset.name}`);
    }
    if (sha256(fs.readFileSync(file)) !== asset.sha256) {
      fail(`local public asset digest differs from GitHub: ${asset.name}`);
    }
  }
  process.stdout.write(`${JSON.stringify(current, null, 2)}\n`);
}

function preflight(args) {
  if (args.length !== 9) {
    fail(
      "usage: validate-release-publication.mjs preflight <release> <run> <workflow> <artifacts> <arm-proof> <x86-proof> <asset-names> <notes> <snapshot>",
    );
  }
  const [
    releaseFile,
    runFile,
    workflowFile,
    artifactsFile,
    armFile,
    x86File,
    namesFile,
    notesFile,
    snapshotFile,
  ] = args;
  const names = expectedAssetNames(namesFile);
  const notes = fs.readFileSync(notesFile, "utf8");
  const release = readJson(releaseFile, "draft release");
  const initial = validateReleaseIdentity(release, names, notes, "draft");
  const runStartedAt = validateWorkflowRun(
    readJson(runFile, "native verifier run"),
    readJson(workflowFile, "native verifier workflow"),
  );
  validateArtifactList(readJson(artifactsFile, "native verifier artifacts"));
  if (runStartedAt <= timestamp(release.updated_at, "release updated_at")) {
    fail("native verifier run is not newer than the draft release");
  }
  for (const asset of initial.assets) {
    if (runStartedAt <= timestamp(asset.updatedAt, `updatedAt for ${asset.name}`)) {
      fail(`native verifier run is not newer than release asset ${asset.name}`);
    }
  }
  const expectedDraftRecord = {
    title: initial.title,
    targetCommitish: initial.targetCommitish,
    notesBytes: Buffer.byteLength(notes, "utf8"),
    notesSha256: initial.bodySha256,
    publishedAt: initial.publishedAt,
    releaseUpdatedAt: release.updated_at,
    assets: initial.assets,
  };
  const armProof = validateProof(
    readJson(armFile, "arm64 native proof"),
    "arm64",
    expectedDraftRecord,
  );
  const x86Proof = validateProof(
    readJson(x86File, "x86_64 native proof"),
    "x86_64",
    expectedDraftRecord,
  );
  exactEqual(armProof, x86Proof, "native proofs do not bind the same exact draft record");

  const snapshot = {
    schemaVersion: 1,
    repository: metadata.repository,
    releaseId: metadata.releaseId,
    tag: metadata.tag,
    tagObject: metadata.tagObject,
    sourceCommit: metadata.sourceCommit,
    verifierCommit: metadata.verifierCommit,
    verifierRunId: metadata.verifierRunId,
    createdAt: release.created_at,
    releaseUpdatedAt: release.updated_at,
    expectedNames: names,
    notesSha256: sha256(Buffer.from(notes, "utf8")),
    assets: initial.assets,
  };
  fs.writeFileSync(snapshotFile, `${JSON.stringify(snapshot, null, 2)}\n`, {
    flag: "wx",
    mode: 0o600,
  });
}

function assertState(args) {
  if (args.length !== 5) {
    fail(
      "usage: validate-release-publication.mjs state <release> <snapshot> <asset-names> <notes> <draft|published>",
    );
  }
  const [releaseFile, snapshotFile, namesFile, notesFile, state] = args;
  const snapshot = readJson(snapshotFile, "trusted draft snapshot");
  const names = expectedAssetNames(namesFile);
  const notes = fs.readFileSync(notesFile, "utf8");
  if (
    snapshot.schemaVersion !== 1 ||
    snapshot.repository !== metadata.repository ||
    snapshot.releaseId !== metadata.releaseId ||
    snapshot.tag !== metadata.tag ||
    snapshot.tagObject !== metadata.tagObject ||
    snapshot.sourceCommit !== metadata.sourceCommit ||
    snapshot.verifierCommit !== metadata.verifierCommit ||
    snapshot.verifierRunId !== metadata.verifierRunId ||
    snapshot.notesSha256 !== sha256(Buffer.from(notes, "utf8"))
  ) {
    fail("trusted draft snapshot does not match publication inputs");
  }
  exactEqual(snapshot.expectedNames, names, "trusted draft inventory changed");
  const canonical = validateReleaseIdentity(
    readJson(releaseFile, `${state} release`),
    names,
    notes,
    state,
    snapshot,
  );
  process.stdout.write(`${JSON.stringify(canonical, null, 2)}\n`);
}

try {
  const [command, ...args] = process.argv.slice(2);
  if (command === "preflight") preflight(args);
  else if (command === "public-proof") publicProof(args);
  else if (command === "state") assertState(args);
  else fail("usage: validate-release-publication.mjs <preflight|public-proof|state> ...");
} catch (error) {
  console.error(`release publication validation failed: ${error.message}`);
  process.exit(1);
}
