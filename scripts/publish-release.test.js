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

function prepareFixture({
  publishable = true,
  dynamicRunName = true,
  splitCommit = false,
  divergentWorkflow = false,
} = {}) {
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
  let workflowCommit = verifierCommit;
  if (splitCommit) {
    fs.writeFileSync(path.join(checkout, "workflow-tooling.txt"), "protected workflow tooling\n");
    git(checkout, "add", "workflow-tooling.txt");
    git(
      checkout,
      "-c",
      "commit.gpgsign=false",
      "commit",
      "-m",
      "test: advance protected workflow tooling",
    );
    workflowCommit = git(checkout, "rev-parse", "HEAD");
  } else if (divergentWorkflow) {
    const tree = git(checkout, "write-tree");
    workflowCommit = git(
      checkout,
      "-c",
      "commit.gpgsign=false",
      "commit-tree",
      tree,
      "-p",
      sourceCommit,
      "-m",
      "test: divergent workflow tooling",
    );
    git(checkout, "switch", "--detach", workflowCommit);
  }

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
    commit: { sha: workflowCommit },
  });
  writeJson(path.join(api, "branch-wrong.json"), {
    name: "main",
    protected: true,
    commit: { sha: "f".repeat(40) },
  });
  writeJson(path.join(api, "ruleset-list.json"), [
    { id: 702 },
    { id: 703 },
    { id: 705 },
  ]);
  const optionalApprovalRuleset = {
    id: 701,
    target: "branch",
    enforcement: "active",
    bypass_actors: [
      {
        actor_id: 42,
        actor_type: "Team",
        bypass_mode: "always",
      },
    ],
    conditions: { ref_name: { include: ["~DEFAULT_BRANCH"], exclude: [] } },
    rules: [
      {
        type: "pull_request",
        parameters: {
          dismiss_stale_reviews_on_push: false,
          require_code_owner_review: false,
          require_last_push_approval: false,
          required_approving_review_count: 0,
        },
      },
    ],
  };
  writeJson(path.join(api, "ruleset-optional-approval.json"), optionalApprovalRuleset);
  writeJson(path.join(api, "ruleset-optional-approval-with-status.json"), {
    ...optionalApprovalRuleset,
    id: 706,
    bypass_actors: [],
    rules: [
      ...optionalApprovalRuleset.rules,
      {
        type: "required_status_checks",
        parameters: {
          strict_required_status_checks_policy: true,
          required_status_checks: [{ context: "Release Check", integration_id: 15368 }],
        },
      },
    ],
  });
  writeJson(path.join(api, "ruleset-list-with-approvals.json"), [
    { id: 701 },
    { id: 702 },
    { id: 703 },
    { id: 705 },
    { id: 706 },
  ]);
  const branchHistoryRuleset = {
    id: 703,
    target: "branch",
    enforcement: "active",
    bypass_actors: [],
    conditions: { ref_name: { include: ["~DEFAULT_BRANCH"], exclude: [] } },
    rules: [
      { type: "deletion" },
      { type: "non_fast_forward" },
    ],
  };
  writeJson(path.join(api, "ruleset-branch-history.json"), branchHistoryRuleset);
  writeJson(path.join(api, "ruleset-branch-history-missing.json"), {
    ...branchHistoryRuleset,
    rules: [{ type: "deletion" }],
  });
  writeJson(path.join(api, "ruleset-branch-history-with-approval.json"), {
    ...branchHistoryRuleset,
    rules: [
      ...branchHistoryRuleset.rules,
      {
        type: "pull_request",
        parameters: {
          dismiss_stale_reviews_on_push: true,
          require_code_owner_review: true,
          require_last_push_approval: true,
          required_approving_review_count: 1,
        },
      },
    ],
  });
  writeJson(path.join(api, "ruleset-branch-history-bypassable.json"), {
    ...branchHistoryRuleset,
    bypass_actors: [
      {
        actor_id: 16654667,
        actor_type: "Team",
        bypass_mode: "pull_request",
      },
    ],
  });
  const branchWorkflowRuleset = {
    id: 705,
    source_type: "Organization",
    source: "openclaw",
    target: "branch",
    enforcement: "active",
    bypass_actors: [],
    conditions: { ref_name: { include: ["~DEFAULT_BRANCH"], exclude: [] } },
    rules: [
      {
        type: "workflows",
        parameters: {
          do_not_enforce_on_create: false,
          workflows: [
            {
              path: ".github/workflows/crabbox-release-check.yml",
              ref: "refs/heads/main",
              repository_id: 1304559357,
            },
          ],
        },
      },
    ],
  };
  writeJson(path.join(api, "ruleset-branch-workflow.json"), branchWorkflowRuleset);
  writeJson(path.join(api, "ruleset-branch-workflow-missing.json"), {
    ...branchWorkflowRuleset,
    rules: [],
  });
  writeJson(path.join(api, "ruleset-branch-workflow-bypassable.json"), {
    ...branchWorkflowRuleset,
    bypass_actors: [
      {
        actor_id: 16654667,
        actor_type: "Team",
        bypass_mode: "pull_request",
      },
    ],
  });
  writeJson(path.join(api, "ruleset-branch-workflow-wrong-source.json"), {
    ...branchWorkflowRuleset,
    source_type: "Repository",
    source: repository,
  });
  writeJson(path.join(api, "ruleset-branch-workflow-wrong-file.json"), {
    ...branchWorkflowRuleset,
    rules: [
      {
        type: "workflows",
        parameters: {
          do_not_enforce_on_create: false,
          workflows: [
            {
              path: ".github/workflows/other.yml",
              ref: "refs/heads/main",
              repository_id: 1304559357,
            },
          ],
        },
      },
    ],
  });
  writeJson(path.join(api, "ruleset-branch-workflow-wrong-ref.json"), {
    ...branchWorkflowRuleset,
    rules: [
      {
        type: "workflows",
        parameters: {
          do_not_enforce_on_create: false,
          workflows: [
            {
              path: ".github/workflows/crabbox-release-check.yml",
              ref: "refs/heads/feature",
              repository_id: 1304559357,
            },
          ],
        },
      },
    ],
  });
  writeJson(path.join(api, "ruleset-branch-workflow-wrong-repository.json"), {
    ...branchWorkflowRuleset,
    rules: [
      {
        type: "workflows",
        parameters: {
          do_not_enforce_on_create: false,
          workflows: [
            {
              path: ".github/workflows/crabbox-release-check.yml",
              ref: "refs/heads/main",
              repository_id: 42,
            },
          ],
        },
      },
    ],
  });
  const otherActorHistoryRuleset = {
    id: 704,
    target: "branch",
    enforcement: "active",
    bypass_actors: [
      {
        actor_id: 42,
        actor_type: "Team",
        bypass_mode: "pull_request",
      },
    ],
    conditions: { ref_name: { include: ["refs/heads/main"], exclude: [] } },
    rules: branchHistoryRuleset.rules,
  };
  writeJson(path.join(api, "ruleset-branch-other-actor-history.json"), otherActorHistoryRuleset);
  writeJson(path.join(api, "ruleset-branch-other-actor-history-all.json"), {
    ...otherActorHistoryRuleset,
    conditions: { ref_name: { include: ["~ALL"], exclude: [] } },
  });
  writeJson(path.join(api, "ruleset-branch-other-actor-history-glob.json"), {
    ...otherActorHistoryRuleset,
    conditions: { ref_name: { include: ["refs/heads/ma*"], exclude: [] } },
  });
  writeJson(path.join(api, "ruleset-branch-other-actor-history-excluded.json"), {
    ...otherActorHistoryRuleset,
    conditions: { ref_name: { include: ["~ALL"], exclude: ["refs/heads/main"] } },
  });
  writeJson(path.join(api, "ruleset-branch-other-actor-history-double-star.json"), {
    ...otherActorHistoryRuleset,
    conditions: { ref_name: { include: ["refs/**"], exclude: [] } },
  });
  writeJson(path.join(api, "ruleset-branch-other-actor-history-recursive.json"), {
    ...otherActorHistoryRuleset,
    conditions: { ref_name: { include: ["refs/**/main"], exclude: [] } },
  });
  writeJson(path.join(api, "ruleset-branch-other-actor-history-slash-class.json"), {
    ...otherActorHistoryRuleset,
    conditions: { ref_name: { include: ["refs[/]heads/main"], exclude: [] } },
  });
  writeJson(path.join(api, "ruleset-list-overlap.json"), [
    { id: 702 },
    { id: 703 },
    { id: 704 },
    { id: 705 },
  ]);
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
  const runRecord = {
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
    head_sha: workflowCommit,
    repository: { full_name: repository },
    head_repository: { full_name: repository },
    head_commit: { id: workflowCommit },
    run_attempt: 1,
    created_at: "2026-07-10T10:05:00Z",
    run_started_at: "2026-07-10T10:05:01Z",
  };
  writeJson(path.join(api, "run.json"), runRecord);
  writeJson(path.join(api, "run-wrong-head.json"), {
    ...runRecord,
    head_sha: verifierCommit,
    head_commit: { id: verifierCommit },
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
      workflow_run: { id: runId, head_branch: "main", head_sha: workflowCommit },
    },
  ];
  let driftArmZipSize = 0;
  let metadataDriftArmZipSize = 0;
  let workflowDriftArmZipSize = 0;
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
      ...(workflowCommit === verifierCommit ? {} : { workflowCommit }),
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

      const workflowDriftProof = structuredClone(proof);
      workflowDriftProof.workflowCommit = "d".repeat(40);
      delete workflowDriftProof.manifestSha256;
      workflowDriftProof.manifestSha256 = sha256(
        Buffer.from(JSON.stringify(workflowDriftProof), "utf8"),
      );
      const workflowDriftDirectory = path.join(root, "proof-arm64-workflow-drift");
      fs.mkdirSync(workflowDriftDirectory);
      writeJson(path.join(workflowDriftDirectory, "verified-assets.json"), workflowDriftProof);
      const workflowDriftZip = path.join(api, "proof-arm64-workflow-drift.zip");
      execFileSync("zip", [
        "-q",
        "-j",
        workflowDriftZip,
        path.join(workflowDriftDirectory, "verified-assets.json"),
      ]);
      workflowDriftArmZipSize = fs.statSync(workflowDriftZip).size;
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
        head_sha: workflowCommit,
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
  const proofWorkflowDriftArtifacts = structuredClone(artifactRows);
  proofWorkflowDriftArtifacts[1].size_in_bytes = workflowDriftArmZipSize;
  proofWorkflowDriftArtifacts[1].digest = `sha256:${sha256(fs.readFileSync(path.join(api, "proof-arm64-workflow-drift.zip")))}`;
  writeJson(path.join(api, "artifacts-proof-workflow-drift.json"), {
    total_count: 3,
    artifacts: proofWorkflowDriftArtifacts,
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
  if (process.env.MOCK_MODE === "patch-response-drift") value.assets[0].digest = "sha256:" + "e".repeat(64);
  outputJson(value);
  process.exit(0);
}
if (method !== "GET") process.exit(93);
if (endpoint === "repos/${repository}") outputFile("repository.json");
else if (endpoint === "repos/${repository}/branches/main") {
  outputFile(
    process.env.MOCK_MODE === "current-main-drift" ||
    (process.env.MOCK_MODE === "published-main-drift" && published)
      ? "branch-wrong.json" : "branch.json",
  );
}
else if (endpoint === "repos/${repository}/rulesets?per_page=100") {
  outputFile(
    process.env.MOCK_MODE === "optional-approvals"
      ? "ruleset-list-with-approvals.json"
      : process.env.MOCK_MODE.startsWith("other-actor-history-")
        ? "ruleset-list-overlap.json"
        : "ruleset-list.json",
  );
}
else if (endpoint === "repos/${repository}/rulesets/701") {
  outputFile("ruleset-optional-approval.json");
}
else if (endpoint === "repos/${repository}/rulesets/706") {
  outputFile("ruleset-optional-approval-with-status.json");
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
else if (endpoint === "repos/${repository}/rulesets/703") {
  outputFile(
    process.env.MOCK_MODE === "missing-history-rules"
      ? "ruleset-branch-history-missing.json"
      : process.env.MOCK_MODE === "history-with-approval"
        ? "ruleset-branch-history-with-approval.json"
        : process.env.MOCK_MODE === "history-bypassable"
          ? "ruleset-branch-history-bypassable.json"
          : "ruleset-branch-history.json",
  );
}
else if (endpoint === "repos/${repository}/rulesets/704") {
  outputFile(
    process.env.MOCK_MODE === "other-actor-history-overlap-all"
      ? "ruleset-branch-other-actor-history-all.json"
      : process.env.MOCK_MODE === "other-actor-history-overlap-glob"
        ? "ruleset-branch-other-actor-history-glob.json"
        : process.env.MOCK_MODE === "other-actor-history-excluded"
          ? "ruleset-branch-other-actor-history-excluded.json"
          : process.env.MOCK_MODE === "other-actor-history-double-star"
            ? "ruleset-branch-other-actor-history-double-star.json"
            : process.env.MOCK_MODE === "other-actor-history-recursive"
              ? "ruleset-branch-other-actor-history-recursive.json"
              : process.env.MOCK_MODE === "other-actor-history-slash-class"
                ? "ruleset-branch-other-actor-history-slash-class.json"
                : "ruleset-branch-other-actor-history.json",
  );
}
else if (endpoint === "repos/${repository}/rulesets/705") {
  outputFile(
    process.env.MOCK_MODE === "missing-workflow-rules"
      ? "ruleset-branch-workflow-missing.json"
      : process.env.MOCK_MODE === "workflow-bypassable"
        ? "ruleset-branch-workflow-bypassable.json"
        : process.env.MOCK_MODE === "workflow-wrong-source"
          ? "ruleset-branch-workflow-wrong-source.json"
          : process.env.MOCK_MODE === "workflow-wrong-file"
            ? "ruleset-branch-workflow-wrong-file.json"
            : process.env.MOCK_MODE === "workflow-wrong-ref"
              ? "ruleset-branch-workflow-wrong-ref.json"
              : process.env.MOCK_MODE === "workflow-wrong-repository"
                ? "ruleset-branch-workflow-wrong-repository.json"
                : "ruleset-branch-workflow.json",
  );
}
else if (endpoint === "repos/${repository}/git/ref/tags/${tag}") outputFile("tag-ref.json");
else if (endpoint === "repos/${repository}/git/tags/${tagObject}") outputFile("tag-object.json");
else if (endpoint === "repos/${repository}/actions/runs/${runId}") {
  outputFile(process.env.MOCK_MODE === "run-head-drift" ? "run-wrong-head.json" : "run.json");
}
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
        : process.env.MOCK_MODE === "proof-workflow-drift"
          ? "artifacts-proof-workflow-drift.json"
          : "artifacts.json",
  );
} else if (endpoint === "repos/${repository}/actions/artifacts/501/zip") {
  outputFile(
    process.env.MOCK_MODE === "proof-drift"
      ? "proof-arm64-drift.zip"
      : process.env.MOCK_MODE === "proof-metadata-drift"
        ? "proof-arm64-metadata-drift.zip"
        : process.env.MOCK_MODE === "proof-workflow-drift"
          ? "proof-arm64-workflow-drift.zip"
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
    if (process.env.MOCK_MODE === "public-readback-drift") value.assets[0].digest = "sha256:" + "e".repeat(64);
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
  const run = (
    mode,
    {
      suppliedWorkflowCommit = workflowCommit,
      explicitWorkflow = splitCommit || divergentWorkflow,
    } = {},
  ) => {
    const childEnv = {
      ...process.env,
      PATH: `${bin}:${process.env.PATH}`,
      MOCK_API: api,
      MOCK_LOG: log,
      MOCK_MODE: mode,
    };
    delete childEnv.CRABBOX_RELEASE_SERIALIZATION_CONFIRMED;
    const args = [
      path.join(checkout, "scripts", "publish-release.sh"),
      String(releaseId),
      tag,
      tagObject,
      sourceCommit,
      verifierCommit,
    ];
    if (explicitWorkflow) args.push(suppliedWorkflowCommit);
    args.push(String(runId), tag);
    return spawnSync(
      "bash",
      args,
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
  return { checkout, root, run, mutations, verifierCommit, workflowCommit };
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

test("tampered native proof workflow commit fails before any mutation", () => {
  withFixture({}, ({ run, mutations }) => {
    const result = run("proof-workflow-drift");
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /proof does not bind/);
    assert.deepEqual(mutations(), []);
  });
});

test("optional approval rules do not constrain publication", () => {
  withFixture({}, ({ run, mutations }) => {
    const result = run("optional-approvals");
    assert.equal(result.status, 0, result.stderr);
    assert.deepEqual(mutations(), [`PATCH\trepos/${repository}/releases/${releaseId}`]);
  });
});

test("missing no-bypass history rules fail before any mutation", () => {
  withFixture({}, ({ run, mutations }) => {
    const result = run("missing-history-rules");
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /no-bypass deletion and non-fast-forward protection/);
    assert.deepEqual(mutations(), []);
  });
});

test("optional approval rules can share no-bypass history protection", () => {
  withFixture({}, ({ run, mutations }) => {
    const result = run("history-with-approval");
    assert.equal(result.status, 0, result.stderr);
    assert.deepEqual(mutations(), [`PATCH\trepos/${repository}/releases/${releaseId}`]);
  });
});

test("overlapping bypassable history rules fail before any mutation", () => {
  withFixture({}, ({ run, mutations }) => {
    const result = run("other-actor-history-overlap");
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /bypassable history protection/);
    assert.deepEqual(mutations(), []);
  });
});

test("all-branch bypassable history rules fail before any mutation", () => {
  withFixture({}, ({ run, mutations }) => {
    const result = run("other-actor-history-overlap-all");
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /bypassable history protection/);
    assert.deepEqual(mutations(), []);
  });
});

test("globbed bypassable history rules fail before any mutation", () => {
  withFixture({}, ({ run, mutations }) => {
    const result = run("other-actor-history-overlap-glob");
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /bypassable history protection/);
    assert.deepEqual(mutations(), []);
  });
});

test("excluded bypassable history rules do not block publication", () => {
  withFixture({}, ({ run, mutations }) => {
    const result = run("other-actor-history-excluded");
    assert.equal(result.status, 0, result.stderr);
    assert.deepEqual(mutations().map((line) => line.split("\t")[0]), ["PATCH"]);
  });
});

test("non-recursive double-star history rules do not match nested refs", () => {
  withFixture({}, ({ run, mutations }) => {
    const result = run("other-actor-history-double-star");
    assert.equal(result.status, 0, result.stderr);
    assert.deepEqual(mutations().map((line) => line.split("\t")[0]), ["PATCH"]);
  });
});

test("recursive double-star bypassable history rules fail before any mutation", () => {
  withFixture({}, ({ run, mutations }) => {
    const result = run("other-actor-history-recursive");
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /bypassable history protection/);
    assert.deepEqual(mutations(), []);
  });
});

test("slash character classes do not match path separators", () => {
  withFixture({}, ({ run, mutations }) => {
    const result = run("other-actor-history-slash-class");
    assert.equal(result.status, 0, result.stderr);
    assert.deepEqual(mutations().map((line) => line.split("\t")[0]), ["PATCH"]);
  });
});

test("bypassable history protection fails before any mutation", () => {
  withFixture({}, ({ run, mutations }) => {
    const result = run("history-bypassable");
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /bypassable history protection/);
    assert.deepEqual(mutations(), []);
  });
});

test("missing organization release workflow fails before any mutation", () => {
  withFixture({}, ({ run, mutations }) => {
    const result = run("missing-workflow-rules");
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /OpenClaw organization release workflow/);
    assert.deepEqual(mutations(), []);
  });
});

test("bypassable organization release workflow fails before any mutation", () => {
  withFixture({}, ({ run, mutations }) => {
    const result = run("workflow-bypassable");
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /OpenClaw organization release workflow/);
    assert.deepEqual(mutations(), []);
  });
});

for (const mode of [
  "workflow-wrong-source",
  "workflow-wrong-file",
  "workflow-wrong-ref",
  "workflow-wrong-repository",
]) {
  test(`${mode} fails before any mutation`, () => {
    withFixture({}, ({ run, mutations }) => {
      const result = run(mode);
      assert.notEqual(result.status, 0);
      assert.match(result.stderr, /OpenClaw organization release workflow/);
      assert.deepEqual(mutations(), []);
    });
  });
}

test("exact organization release workflow permits publication", () => {
  withFixture({}, ({ run, mutations }) => {
    const result = run("workflow-exact");
    assert.equal(result.status, 0, result.stderr);
    assert.deepEqual(mutations().map((line) => line.split("\t")[0]), ["PATCH"]);
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

for (const mode of ["patch-response-drift", "public-readback-drift"]) {
  test(`${mode} reports an incident after one PATCH without corrective mutations`, () => {
    withFixture({}, ({ run, mutations }) => {
      const result = run(mode);
      assert.notEqual(result.status, 0);
      assert.match(result.stderr, /asset identity drifted|digest/);
      assert.doesNotMatch(result.stdout, /Published exact verified release/);
      assert.deepEqual(mutations(), [`PATCH\trepos/${repository}/releases/${releaseId}`]);
    });
  });
}

test("post-PATCH protected-main drift reports an incident without corrective mutations", () => {
  withFixture({}, ({ run, mutations }) => {
    const result = run("published-main-drift");
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /protected default-branch head does not match the workflow commit/);
    assert.doesNotMatch(result.stdout, /Published exact verified release/);
    assert.deepEqual(mutations(), [`PATCH\trepos/${repository}/releases/${releaseId}`]);
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
    assert.match(result.stderr, /local HEAD must exactly equal protected workflow commit/);
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

test("exact proof publishes without approval rules or a serialization attestation using one PATCH", () => {
  withFixture({}, ({ run, mutations }) => {
    const result = run("success");
    assert.equal(result.status, 0, result.stderr || result.stdout);
    assert.match(result.stdout, /Published exact verified release v0\.37\.0/);
    assert.deepEqual(mutations(), [`PATCH\trepos/${repository}/releases/${releaseId}`]);
  });
});

test("split verifier and workflow commits permit publication from exact protected main", () => {
  withFixture({ splitCommit: true }, ({ run, mutations, verifierCommit, workflowCommit }) => {
    assert.notEqual(workflowCommit, verifierCommit);
    const result = run("success");
    assert.equal(result.status, 0, result.stderr || result.stdout);
    assert.deepEqual(mutations(), [`PATCH\trepos/${repository}/releases/${releaseId}`]);
  });
});

test("mismatched supplied workflow commit fails before any network call", () => {
  withFixture({ splitCommit: true }, ({ run, mutations, verifierCommit }) => {
    const result = run("success", {
      explicitWorkflow: true,
      suppliedWorkflowCommit: verifierCommit,
    });
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /local HEAD must exactly equal protected workflow commit/);
    assert.deepEqual(mutations(), []);
  });
});

test("selected verifier run with a different workflow head fails before publication", () => {
  withFixture({ splitCommit: true }, ({ run, mutations }) => {
    const result = run("run-head-drift");
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /protected workflow identity/);
    assert.deepEqual(mutations(), []);
  });
});

test("divergent workflow commit fails before any network call", () => {
  withFixture({ divergentWorkflow: true }, ({ run, mutations }) => {
    const result = run("success");
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /provenance verifier commit is not an ancestor/);
    assert.deepEqual(mutations(), []);
  });
});

test("workflow commit that is not current protected main fails before publication", () => {
  withFixture({ splitCommit: true }, ({ run, mutations }) => {
    const result = run("current-main-drift");
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /protected default-branch head does not match the workflow commit/);
    assert.deepEqual(mutations(), []);
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
