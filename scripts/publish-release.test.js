import assert from "node:assert/strict";
import crypto from "node:crypto";
import { execFileSync, spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

const sourceRoot = path.resolve(import.meta.dirname, "..");
const tag = "v0.37.0";
const releaseId = 123;
const runId = 9001;
const workflowId = 77;
const repository = "openclaw/crabbox";

function sha256(value) {
  return crypto.createHash("sha256").update(value).digest("hex");
}

function writeJson(file, value) {
  fs.writeFileSync(file, `${JSON.stringify(value, null, 2)}\n`);
}

function copy(root, relative) {
  const destination = path.join(root, relative);
  fs.mkdirSync(path.dirname(destination), { recursive: true });
  fs.copyFileSync(path.join(sourceRoot, relative), destination);
}

function git(root, ...args) {
  return execFileSync("git", args, {
    cwd: root,
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
  }).trim();
}

function prepareFixture({ publishable = true, dynamicRunName = true } = {}) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-publish-test-"));
  const checkout = path.join(root, "repo");
  const api = path.join(root, "api");
  const bin = path.join(root, "bin");
  fs.mkdirSync(api);
  fs.mkdirSync(bin);
  execFileSync("git", ["clone", "--quiet", "--no-local", sourceRoot, checkout]);
  for (const file of [
    ".github/CODEOWNERS",
    ".github/release-allowed-signers",
    ".github/workflows/release-assets.yml",
    "scripts/extract-release-notes.sh",
    "scripts/publish-release.sh",
    "scripts/release-config.sh",
    "scripts/validate-release-publication.mjs",
    "scripts/verify-github-release-policy.mjs",
    "scripts/verify-release-source.sh",
  ]) {
    copy(checkout, file);
  }

  const tagObject = git(checkout, "rev-parse", `refs/tags/${tag}`);
  const sourceCommit = git(checkout, "rev-parse", `refs/tags/${tag}^{commit}`);
  const record = {
    schemaVersion: 1,
    repository,
    tag,
    tagObject,
    sourceCommit,
    publicationStatus: publishable ? "ready" : "blocked",
    ...(publishable ? {} : { blocker: "test safety stop" }),
  };
  const recordFile = path.join(checkout, "release", "records", `${tag}.json`);
  fs.mkdirSync(path.dirname(recordFile), { recursive: true });
  writeJson(recordFile, record);
  git(checkout, "config", "user.name", "Publication Test");
  git(checkout, "config", "user.email", "publication@example.test");
  git(checkout, "add", ".github", "release", "scripts");
  git(
    checkout,
    "-c",
    "commit.gpgsign=false",
    "commit",
    "-m",
    "test: protected publication fixture",
  );
  const verifierCommit = git(checkout, "rev-parse", "HEAD");

  const version = tag.slice(1);
  const assetNames = execFileSync(
    path.join(checkout, "scripts", "release-config.sh"),
    ["assets", version],
    {
      encoding: "utf8",
    },
  )
    .trim()
    .split("\n")
    .sort();
  assert.equal(assetNames.length, 8);
  const notes = execFileSync(
    "bash",
    [
      "-c",
      'git show "$1:CHANGELOG.md" | "$2" "$3"',
      "bash",
      sourceCommit,
      path.join(checkout, "scripts", "extract-release-notes.sh"),
      tag,
    ],
    { cwd: checkout, encoding: "utf8" },
  );
  const releaseUpdatedAt = "2026-07-10T10:01:00Z";
  const assets = assetNames.map((name, index) => {
    const id = 1001 + index;
    const bytes = Buffer.from(`exact fixture bytes for ${name}\n`);
    fs.writeFileSync(path.join(api, `asset-${id}`), bytes);
    return {
      id,
      name,
      size: bytes.length,
      state: "uploaded",
      digest: `sha256:${sha256(bytes)}`,
      updated_at: "2026-07-10T10:00:00Z",
      url: `https://api.github.com/repos/${repository}/releases/assets/${id}`,
      browser_download_url: `https://github.com/${repository}/releases/download/${tag}/${name}`,
    };
  });
  const release = {
    id: releaseId,
    tag_name: tag,
    target_commitish: "main",
    name: tag,
    body: notes,
    draft: true,
    immutable: false,
    prerelease: false,
    created_at: "2026-07-10T09:50:00Z",
    updated_at: releaseUpdatedAt,
    published_at: null,
    assets: [...assets].reverse(),
  };
  writeJson(path.join(api, "release.json"), release);
  writeJson(path.join(api, "repository.json"), { full_name: repository, default_branch: "main" });
  writeJson(path.join(api, "branch.json"), {
    name: "main",
    protected: true,
    commit: { sha: verifierCommit },
  });
  writeJson(path.join(api, "ruleset-list.json"), [{ id: 701 }, { id: 702 }]);
  const branchRuleset = {
    id: 701,
    target: "branch",
    enforcement: "active",
    bypass_actors: [],
    conditions: { ref_name: { include: ["~DEFAULT_BRANCH"], exclude: [] } },
    rules: [
      { type: "deletion" },
      { type: "non_fast_forward" },
      {
        type: "pull_request",
        parameters: {
          dismiss_stale_reviews_on_push: true,
          require_code_owner_review: true,
          require_last_push_approval: true,
          required_approving_review_count: 1,
        },
      },
      {
        type: "required_status_checks",
        parameters: {
          strict_required_status_checks_policy: true,
          required_status_checks: [{ context: "CI" }],
        },
      },
    ],
  };
  writeJson(path.join(api, "ruleset-branch.json"), branchRuleset);
  writeJson(path.join(api, "ruleset-branch-missing.json"), {
    ...branchRuleset,
    rules: [{ type: "deletion" }, { type: "non_fast_forward" }],
  });
  writeJson(path.join(api, "ruleset-tag.json"), {
    id: 702,
    target: "tag",
    enforcement: "active",
    bypass_actors: [],
    conditions: { ref_name: { include: ["refs/tags/v*"], exclude: [] } },
    rules: [{ type: "deletion" }, { type: "non_fast_forward" }],
  });
  writeJson(path.join(api, "ruleset-tag-excluded.json"), {
    id: 702,
    target: "tag",
    enforcement: "active",
    bypass_actors: [],
    conditions: {
      ref_name: { include: ["refs/tags/v*"], exclude: ["refs/tags/v0.38.0"] },
    },
    rules: [{ type: "deletion" }, { type: "non_fast_forward" }],
  });
  writeJson(path.join(api, "ruleset-tag-missing.json"), {
    id: 702,
    target: "tag",
    enforcement: "active",
    bypass_actors: [],
    conditions: { ref_name: { include: ["refs/tags/v*"], exclude: [] } },
    rules: [{ type: "deletion" }],
  });
  writeJson(path.join(api, "tag-ref.json"), {
    ref: `refs/tags/${tag}`,
    object: {
      type: "tag",
      sha: tagObject,
      url: `https://api.github.com/repos/${repository}/git/tags/${tagObject}`,
    },
  });
  writeJson(path.join(api, "tag-object.json"), {
    tag,
    object: {
      type: "commit",
      sha: sourceCommit,
      url: `https://api.github.com/repos/${repository}/git/commits/${sourceCommit}`,
    },
    verification: { verified: true, reason: "valid" },
  });
  const workflowUrl = `https://api.github.com/repos/${repository}/actions/workflows/${workflowId}`;
  writeJson(path.join(api, "run.json"), {
    id: runId,
    name: dynamicRunName
      ? `Verify draft ${releaseId} for ${tag} at ${verifierCommit}`
      : "Verify Release Assets",
    display_title: `Verify draft ${releaseId} for ${tag} at ${verifierCommit}`,
    path: ".github/workflows/release-assets.yml",
    workflow_id: workflowId,
    workflow_url: workflowUrl,
    event: "workflow_dispatch",
    status: "completed",
    conclusion: "success",
    head_branch: "main",
    head_sha: verifierCommit,
    repository: { full_name: repository },
    head_repository: { full_name: repository },
    head_commit: { id: verifierCommit },
    run_attempt: 1,
    created_at: "2026-07-10T10:05:00Z",
    run_started_at: "2026-07-10T10:05:01Z",
  });
  writeJson(path.join(api, "workflow.json"), {
    id: workflowId,
    name: "Verify Release Assets",
    path: ".github/workflows/release-assets.yml",
    state: "active",
    url: workflowUrl,
  });

  const proofAssets = [...assets]
    .sort((a, b) => a.name.localeCompare(b.name))
    .map((asset) => ({
      id: asset.id,
      name: asset.name,
      size: asset.size,
      sha256: asset.digest.slice("sha256:".length),
      updatedAt: asset.updated_at,
    }));
  const artifactRows = [
    {
      id: 500,
      name: "release-input",
      size_in_bytes: 4096,
      digest: `sha256:${"0".repeat(64)}`,
      expired: false,
      created_at: "2026-07-10T10:04:00Z",
      updated_at: "2026-07-10T10:04:00Z",
      workflow_run: { id: runId, head_branch: "main", head_sha: verifierCommit },
    },
  ];
  let driftArmZipSize = 0;
  let metadataDriftArmZipSize = 0;
  for (const [arch, artifactId] of [
    ["arm64", 501],
    ["x86_64", 502],
  ]) {
    const proof = {
      schemaVersion: 1,
      state: "draft",
      repository,
      releaseId,
      tag,
      tagObject,
      sourceCommit,
      verifierCommit,
      verifierArch: arch,
      title: release.name,
      targetCommitish: release.target_commitish,
      notesBytes: Buffer.byteLength(notes, "utf8"),
      notesSha256: sha256(Buffer.from(notes, "utf8")),
      publishedAt: release.published_at,
      releaseUpdatedAt,
      assets: proofAssets,
    };
    proof.manifestSha256 = sha256(Buffer.from(JSON.stringify(proof), "utf8"));
    const proofDirectory = path.join(root, `proof-${arch}`);
    fs.mkdirSync(proofDirectory);
    writeJson(path.join(proofDirectory, "verified-assets.json"), proof);
    const zip = path.join(api, `proof-${arch}.zip`);
    execFileSync("zip", ["-q", "-j", zip, path.join(proofDirectory, "verified-assets.json")]);
    if (arch === "arm64") {
      const driftProof = structuredClone(proof);
      driftProof.assets[0].sha256 = "f".repeat(64);
      delete driftProof.manifestSha256;
      driftProof.manifestSha256 = sha256(Buffer.from(JSON.stringify(driftProof), "utf8"));
      const driftDirectory = path.join(root, "proof-arm64-drift");
      fs.mkdirSync(driftDirectory);
      writeJson(path.join(driftDirectory, "verified-assets.json"), driftProof);
      const driftZip = path.join(api, "proof-arm64-drift.zip");
      execFileSync("zip", [
        "-q",
        "-j",
        driftZip,
        path.join(driftDirectory, "verified-assets.json"),
      ]);
      driftArmZipSize = fs.statSync(driftZip).size;

      const metadataDriftProof = structuredClone(proof);
      metadataDriftProof.notesSha256 = "e".repeat(64);
      delete metadataDriftProof.manifestSha256;
      metadataDriftProof.manifestSha256 = sha256(
        Buffer.from(JSON.stringify(metadataDriftProof), "utf8"),
      );
      const metadataDriftDirectory = path.join(root, "proof-arm64-metadata-drift");
      fs.mkdirSync(metadataDriftDirectory);
      writeJson(path.join(metadataDriftDirectory, "verified-assets.json"), metadataDriftProof);
      const metadataDriftZip = path.join(api, "proof-arm64-metadata-drift.zip");
      execFileSync("zip", [
        "-q",
        "-j",
        metadataDriftZip,
        path.join(metadataDriftDirectory, "verified-assets.json"),
      ]);
      metadataDriftArmZipSize = fs.statSync(metadataDriftZip).size;
    }
    artifactRows.push({
      id: artifactId,
      name: `verified-assets-${arch}`,
      size_in_bytes: fs.statSync(zip).size,
      digest: `sha256:${sha256(fs.readFileSync(zip))}`,
      expired: false,
      created_at: "2026-07-10T10:06:00Z",
      updated_at: "2026-07-10T10:06:00Z",
      workflow_run: {
        id: runId,
        head_branch: "main",
        head_sha: verifierCommit,
      },
    });
  }
  writeJson(path.join(api, "artifacts.json"), { total_count: 3, artifacts: artifactRows });
  writeJson(path.join(api, "artifacts-no-proof.json"), {
    total_count: 2,
    artifacts: artifactRows.slice(0, 2),
  });
  const proofDriftArtifacts = structuredClone(artifactRows);
  proofDriftArtifacts[1].size_in_bytes = driftArmZipSize;
  proofDriftArtifacts[1].digest = `sha256:${sha256(fs.readFileSync(path.join(api, "proof-arm64-drift.zip")))}`;
  writeJson(path.join(api, "artifacts-proof-drift.json"), {
    total_count: 3,
    artifacts: proofDriftArtifacts,
  });
  const proofMetadataDriftArtifacts = structuredClone(artifactRows);
  proofMetadataDriftArtifacts[1].size_in_bytes = metadataDriftArmZipSize;
  proofMetadataDriftArtifacts[1].digest = `sha256:${sha256(fs.readFileSync(path.join(api, "proof-arm64-metadata-drift.zip")))}`;
  writeJson(path.join(api, "artifacts-proof-metadata-drift.json"), {
    total_count: 3,
    artifacts: proofMetadataDriftArtifacts,
  });

  const gh = path.join(bin, "gh");
  fs.writeFileSync(
    gh,
    `#!/usr/bin/env node
const fs = require("node:fs");
const path = require("node:path");
const args = process.argv.slice(2);
if (args.shift() !== "api") process.exit(90);
const methodIndex = args.indexOf("--method");
const method = methodIndex >= 0 ? args[methodIndex + 1] : "GET";
const endpoint = args.find((arg) => arg.startsWith("repos/"));
fs.appendFileSync(process.env.MOCK_LOG, method + "\\t" + endpoint + "\\n");
const api = process.env.MOCK_API;
const json = (name) => JSON.parse(fs.readFileSync(path.join(api, name), "utf8"));
const outputJson = (value) => process.stdout.write(JSON.stringify(value));
const outputFile = (name) => process.stdout.write(fs.readFileSync(path.join(api, name)));
const published = fs.existsSync(path.join(api, "published"));
if (method === "PATCH") {
  if (endpoint !== "repos/${repository}/releases/${releaseId}") process.exit(91);
  const inputIndex = args.indexOf("--input");
  if (inputIndex < 0 || fs.readFileSync(args[inputIndex + 1], "utf8") !== '{"draft":false}\\n') process.exit(92);
  fs.writeFileSync(path.join(api, "published"), "yes\\n");
  const value = json("release.json");
  value.draft = false;
  value.immutable = true;
  value.updated_at = "2026-07-10T10:10:00Z";
  value.published_at = "2026-07-10T10:10:00Z";
  outputJson(value);
  process.exit(0);
}
if (method !== "GET") process.exit(93);
if (endpoint === "repos/${repository}") outputFile("repository.json");
else if (endpoint === "repos/${repository}/branches/main") outputFile("branch.json");
else if (endpoint === "repos/${repository}/rulesets?per_page=100") outputFile("ruleset-list.json");
else if (endpoint === "repos/${repository}/rulesets/701") {
  outputFile(process.env.MOCK_MODE === "missing-rules" ? "ruleset-branch-missing.json" : "ruleset-branch.json");
}
else if (endpoint === "repos/${repository}/rulesets/702") {
  outputFile(
    process.env.MOCK_MODE === "missing-tag-rules"
      ? "ruleset-tag-missing.json"
      : process.env.MOCK_MODE === "excluded-tag-rules"
        ? "ruleset-tag-excluded.json"
        : "ruleset-tag.json",
  );
}
else if (endpoint === "repos/${repository}/git/ref/tags/${tag}") outputFile("tag-ref.json");
else if (endpoint === "repos/${repository}/git/tags/${tagObject}") outputFile("tag-object.json");
else if (endpoint === "repos/${repository}/actions/runs/${runId}") outputFile("run.json");
else if (endpoint === "repos/${repository}/immutable-releases") {
  outputJson({
    enabled: process.env.MOCK_MODE !== "immutable-disabled",
    enforced_by_owner: process.env.MOCK_MODE !== "immutable-repository-only",
  });
}
else if (endpoint === "repos/${repository}/actions/workflows/${workflowId}") outputFile("workflow.json");
else if (endpoint === "repos/${repository}/actions/runs/${runId}/artifacts?per_page=100") {
  outputFile(
    process.env.MOCK_MODE === "no-proof"
      ? "artifacts-no-proof.json"
      : process.env.MOCK_MODE === "proof-drift"
        ? "artifacts-proof-drift.json"
        : process.env.MOCK_MODE === "proof-metadata-drift"
          ? "artifacts-proof-metadata-drift.json"
          : "artifacts.json",
  );
} else if (endpoint === "repos/${repository}/actions/artifacts/501/zip") {
  outputFile(
    process.env.MOCK_MODE === "proof-drift"
      ? "proof-arm64-drift.zip"
      : process.env.MOCK_MODE === "proof-metadata-drift"
        ? "proof-arm64-metadata-drift.zip"
        : "proof-arm64.zip",
  );
}
else if (endpoint === "repos/${repository}/actions/artifacts/502/zip") outputFile("proof-x86_64.zip");
else if (endpoint === "repos/${repository}/releases/${releaseId}") {
  const countFile = path.join(api, "release-get-count");
  const count = fs.existsSync(countFile) ? Number(fs.readFileSync(countFile, "utf8")) + 1 : 1;
  fs.writeFileSync(countFile, String(count));
  const value = json("release.json");
  if (published) {
    value.draft = false;
    value.immutable = true;
    value.updated_at = "2026-07-10T10:10:00Z";
    value.published_at = "2026-07-10T10:10:00Z";
  } else if (process.env.MOCK_MODE === "drift" && count >= 2) {
    value.assets[0].digest = "sha256:" + "f".repeat(64);
  } else if (process.env.MOCK_MODE === "prepatch-drift" && count >= 4) {
    value.assets[0].digest = "sha256:" + "e".repeat(64);
  }
  outputJson(value);
} else {
  const asset = /^repos\\/${repository.replace("/", "\\/")}\\/releases\\/assets\\/(\\d+)$/.exec(endpoint);
  if (!asset) process.exit(94);
  outputFile("asset-" + asset[1]);
}
`,
  );
  fs.chmodSync(gh, 0o755);

  const log = path.join(root, "gh.log");
  const run = (mode, { serializationConfirmed = true } = {}) => {
    const childEnv = {
      ...process.env,
      PATH: `${bin}:${process.env.PATH}`,
      MOCK_API: api,
      MOCK_LOG: log,
      MOCK_MODE: mode,
    };
    if (serializationConfirmed) {
      childEnv.CRABBOX_RELEASE_SERIALIZATION_CONFIRMED = `${tag}:${releaseId}`;
    } else {
      delete childEnv.CRABBOX_RELEASE_SERIALIZATION_CONFIRMED;
    }
    return spawnSync(
      "bash",
      [
        path.join(checkout, "scripts", "publish-release.sh"),
        String(releaseId),
        tag,
        tagObject,
        sourceCommit,
        verifierCommit,
        String(runId),
        tag,
      ],
      {
        cwd: checkout,
        env: childEnv,
        encoding: "utf8",
      },
    );
  };
  const mutations = () => {
    if (!fs.existsSync(log)) return [];
    return fs
      .readFileSync(log, "utf8")
      .trim()
      .split("\n")
      .filter(Boolean)
      .filter((line) => !line.startsWith("GET\t"));
  };
  return { checkout, root, run, mutations };
}

function withFixture(options, callback) {
  const fixture = prepareFixture(options);
  try {
    callback(fixture);
  } finally {
    fs.rmSync(fixture.root, { recursive: true, force: true });
  }
}

test("publication drift fails before any mutation", () => {
  withFixture({}, ({ run, mutations }) => {
    const result = run("drift");
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /asset identity drifted|digest/);
    assert.deepEqual(mutations(), []);
  });
});

test("missing native proof fails before any mutation", () => {
  withFixture({}, ({ run, mutations }) => {
    const result = run("no-proof");
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /proof artifacts/);
    assert.deepEqual(mutations(), []);
  });
});

test("tampered native proof asset record fails before any mutation", () => {
  withFixture({}, ({ run, mutations }) => {
    const result = run("proof-drift");
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /proof draft record differs from current release/);
    assert.deepEqual(mutations(), []);
  });
});

test("tampered native proof draft metadata fails before any mutation", () => {
  withFixture({}, ({ run, mutations }) => {
    const result = run("proof-metadata-drift");
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /proof draft record differs from current release/);
    assert.deepEqual(mutations(), []);
  });
});

test("missing protected branch rules fail before any mutation", () => {
  withFixture({}, ({ run, mutations }) => {
    const result = run("missing-rules");
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /default branch lacks one active no-bypass/);
    assert.deepEqual(mutations(), []);
  });
});

test("missing stable-tag update protection fails before any mutation", () => {
  withFixture({}, ({ run, mutations }) => {
    const result = run("missing-tag-rules");
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /stable release tags lack one active no-bypass/);
    assert.deepEqual(mutations(), []);
  });
});

test("stable-tag ruleset exclusions fail before any mutation", () => {
  withFixture({}, ({ run, mutations }) => {
    const result = run("excluded-tag-rules");
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /stable release tags lack one active no-bypass/);
    assert.deepEqual(mutations(), []);
  });
});

test("final immediately pre-PATCH draft drift fails before any mutation", () => {
  withFixture({}, ({ run, mutations }) => {
    const result = run("prepatch-drift");
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /asset identity drifted|digest/);
    assert.deepEqual(mutations(), []);
  });
});

test("disabled release immutability fails before publication", () => {
  withFixture({}, ({ run, mutations }) => {
    const result = run("immutable-disabled");
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /organization-enforced release immutability is required/);
    assert.deepEqual(mutations(), []);
  });
});

test("repository-only release immutability fails before publication", () => {
  withFixture({}, ({ run, mutations }) => {
    const result = run("immutable-repository-only");
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /organization-enforced release immutability is required/);
    assert.deepEqual(mutations(), []);
  });
});

test("dirty protected publication tooling fails before any network call", () => {
  withFixture({}, ({ checkout, run, mutations }) => {
    fs.appendFileSync(path.join(checkout, "scripts", "validate-release-publication.mjs"), "\n// dirty\n");
    const result = run("success");
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /protected release tooling is dirty/);
    assert.deepEqual(mutations(), []);
  });
});

test("local HEAD drift fails before any network call", () => {
  withFixture({}, ({ checkout, run, mutations }) => {
    fs.writeFileSync(path.join(checkout, "unrelated.txt"), "new commit\n");
    git(checkout, "add", "unrelated.txt");
    git(checkout, "-c", "commit.gpgsign=false", "commit", "-m", "test: move local head");
    const result = run("success");
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /local HEAD must exactly equal protected verifier commit/);
    assert.deepEqual(mutations(), []);
  });
});

test("missing administrative serialization confirmation fails closed", () => {
  withFixture({}, ({ run, mutations }) => {
    const result = run("success", { serializationConfirmed: false });
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /requires an exclusive administrative release freeze/);
    assert.deepEqual(mutations(), []);
  });
});

test("protected blocked record fails before any mutation", () => {
  withFixture({ publishable: false }, ({ run, mutations }) => {
    const result = run("success");
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /release v0\.37\.0 is blocked/);
    assert.deepEqual(mutations(), []);
  });
});

test("exact protected proof performs one draft-state PATCH and no other mutation", () => {
  withFixture({}, ({ run, mutations }) => {
    const result = run("success");
    assert.equal(result.status, 0, result.stderr || result.stdout);
    assert.match(result.stdout, /Published exact verified release v0\.37\.0/);
    assert.deepEqual(mutations(), [`PATCH\trepos/${repository}/releases/${releaseId}`]);
  });
});

test("static workflow name remains accepted for compatible run metadata", () => {
  withFixture({ dynamicRunName: false }, ({ run, mutations }) => {
    const result = run("success");
    assert.equal(result.status, 0, result.stderr || result.stdout);
    assert.deepEqual(mutations(), [`PATCH\trepos/${repository}/releases/${releaseId}`]);
  });
});

test("native verifier workflow prevents overlap for one numeric release without cross-release cancellation", () => {
  const workflow = fs.readFileSync(
    path.join(sourceRoot, ".github", "workflows", "release-assets.yml"),
    "utf8",
  );
  assert.match(
    workflow,
    /group: crabbox-release-\$\{\{ github\.repository_id \}\}-\$\{\{ inputs\.release_id \}\}/,
  );
  assert.match(workflow, /cancel-in-progress: false/);
});
