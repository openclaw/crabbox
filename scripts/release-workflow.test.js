import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

const repoRoot = path.resolve(import.meta.dirname, "..");

test("repository dispatch checks out the requested default-branch tag", () => {
  const workflow = fs.readFileSync(path.join(repoRoot, ".github", "workflows", "release.yml"), "utf8");
  const trigger = workflow.slice(workflow.indexOf("on:"), workflow.indexOf("permissions:"));
  const verifySource = workflow.indexOf("- name: Verify release source and tag");
  const stashConfig = workflow.indexOf("- name: Stash current GoReleaser config");
  const checkoutTag = workflow.indexOf("- name: Checkout release tag");
  const setupGo = workflow.indexOf("- name: Setup Go");

  assert.match(trigger, /repository_dispatch:[\s\S]*types:\s*\[release\]/);
  assert.doesNotMatch(
    trigger,
    /workflow_dispatch:/,
    "ref-selectable workflow dispatches must not start production releases",
  );
  assert.doesNotMatch(trigger, /push:/, "tag pushes must not start production releases");
  assert.notEqual(verifySource, -1);
  assert.notEqual(stashConfig, -1);
  assert.notEqual(checkoutTag, -1);
  assert.notEqual(setupGo, -1);
  assert.ok(verifySource < stashConfig, "release source and tag should be verified before config is saved");
  assert.ok(stashConfig < checkoutTag, "GoReleaser config should be saved before checking out historical tags");
  assert.ok(checkoutTag < setupGo, "setup-go should read go.mod from the requested release tag");
  assert.match(
    workflow.slice(verifySource, stashConfig),
    /DEFAULT_BRANCH:[\s\S]*github\.event\.client_payload\.tag[\s\S]*WORKFLOW_REF:[\s\S]*run: scripts\/verify-release-source\.sh/,
  );
  assert.match(workflow.slice(checkoutTag, setupGo), /git checkout --detach "refs\/tags\/\$RELEASE_TAG"/);
  assert.doesNotMatch(
    workflow.slice(checkoutTag),
    /cp \.goreleaser\.yaml \/tmp\/\.goreleaser\.yaml/,
    "the selected release tag must not replace the reviewed GoReleaser config",
  );
  assert.match(workflow.slice(setupGo), /go-version-file:\s+go\.mod/);
});

test("release source guard accepts only reviewed default-branch tags", () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-release-source-"));
  const guard = path.join(repoRoot, "scripts", "verify-release-source.sh");
  const git = (...args) => execFileSync("git", args, { cwd: root, stdio: "pipe" });
  const runGuard = (env) =>
    spawnSync(guard, [], {
      cwd: root,
      env: { ...process.env, ...env },
      encoding: "utf8",
    });

  try {
    git("init", "-b", "main");
    git("config", "user.name", "Crabbox Test");
    git("config", "user.email", "test@example.com");
    fs.writeFileSync(path.join(root, "reviewed.txt"), "reviewed\n");
    git("add", "reviewed.txt");
    git("commit", "-m", "reviewed");
    git("tag", "--no-sign", "v1.2.3");

    const valid = runGuard({
      DEFAULT_BRANCH: "main",
      RELEASE_TAG: "v1.2.3",
      WORKFLOW_REF: "refs/heads/main",
    });
    assert.equal(valid.status, 0, valid.stderr || valid.stdout);

    const wrongBranch = runGuard({
      DEFAULT_BRANCH: "main",
      RELEASE_TAG: "v1.2.3",
      WORKFLOW_REF: "refs/heads/unreviewed",
    });
    assert.equal(wrongBranch.status, 1);
    assert.match(wrongBranch.stdout, /release events must run from main/);

    const malformed = runGuard({
      DEFAULT_BRANCH: "main",
      RELEASE_TAG: "v1.2",
      WORKFLOW_REF: "refs/heads/main",
    });
    assert.equal(malformed.status, 1);
    assert.match(malformed.stdout, /must match vMAJOR\.MINOR\.PATCH/);

    const missing = runGuard({
      DEFAULT_BRANCH: "main",
      RELEASE_TAG: "v9.9.9",
      WORKFLOW_REF: "refs/heads/main",
    });
    assert.equal(missing.status, 1);
    assert.match(missing.stdout, /release tag does not exist/);

    git("switch", "--orphan", "unreviewed");
    fs.writeFileSync(path.join(root, "unreviewed.txt"), "unreviewed\n");
    git("add", "unreviewed.txt");
    git("commit", "-m", "unreviewed");
    git("tag", "--no-sign", "v2.0.0");
    git("switch", "main");

    const outsideHistory = runGuard({
      DEFAULT_BRANCH: "main",
      RELEASE_TAG: "v2.0.0",
      WORKFLOW_REF: "refs/heads/main",
    });
    assert.equal(outsideHistory.status, 1);
    assert.match(outsideHistory.stdout, /not in main history/);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test("release credentials stay behind the dispatch source guard", () => {
  const workflow = fs.readFileSync(path.join(repoRoot, ".github", "workflows", "release.yml"), "utf8");
  const verifySource = workflow.indexOf("- name: Verify release source and tag");
  const verifyTapToken = workflow.indexOf("- name: Verify Homebrew tap token");
  const goreleaser = workflow.indexOf("- name: GoReleaser");

  assert.ok(verifySource < verifyTapToken);
  assert.ok(verifyTapToken < goreleaser);
  assert.doesNotMatch(
    workflow.slice(0, verifyTapToken),
    /\$\{\{\s*secrets\./,
    "release secrets must not be loaded before source and ancestry checks",
  );
});

test("macOS GoReleaser jobs bound build parallelism", () => {
  const ciWorkflow = fs.readFileSync(path.join(repoRoot, ".github", "workflows", "ci.yml"), "utf8");
  const releaseWorkflow = fs.readFileSync(
    path.join(repoRoot, ".github", "workflows", "release.yml"),
    "utf8",
  );

  assert.match(
    ciWorkflow,
    /args:\s+release --snapshot --clean --skip=publish --parallelism 1/,
  );
  assert.match(
    releaseWorkflow,
    /args:\s+release --clean --config \/tmp\/\.goreleaser\.yaml --parallelism 1/,
  );
});

test("production and snapshot releases have the same build budget", () => {
  const ciWorkflow = fs.readFileSync(path.join(repoRoot, ".github", "workflows", "ci.yml"), "utf8");
  const releaseWorkflow = fs.readFileSync(
    path.join(repoRoot, ".github", "workflows", "release.yml"),
    "utf8",
  );

  assert.match(ciWorkflow, /release-check:[\s\S]*?timeout-minutes:\s+45/);
  assert.match(releaseWorkflow, /goreleaser:[\s\S]*?timeout-minutes:\s+45/);
});

test("retries replace only incomplete releases", () => {
  const workflow = fs.readFileSync(path.join(repoRoot, ".github", "workflows", "release.yml"), "utf8");
  const resolveTag = workflow.indexOf("- name: Resolve release tag");
  const removeIncomplete = workflow.indexOf("- name: Remove incomplete release");
  const goreleaser = workflow.indexOf("- name: GoReleaser");
  const retryStep = workflow.slice(removeIncomplete, goreleaser);

  assert.ok(resolveTag < removeIncomplete, "the release tag must be resolved before retry cleanup");
  assert.ok(removeIncomplete < goreleaser, "an incomplete release must be removed before publishing");
  assert.match(retryStep, /404\) exit 0/);
  assert.match(retryStep, /grep -q "\^## \$RELEASE_VERSION "/);
  assert.match(retryStep, /already finalized; refusing to replace its artifacts/);
  assert.match(
    retryStep,
    /gh api --method DELETE "repos\/\$GITHUB_REPOSITORY\/releases\/\$release_id"/,
  );
});

test("Apple VM release helper embeds the Swift daemon", () => {
  const ciWorkflow = fs.readFileSync(path.join(repoRoot, ".github", "workflows", "ci.yml"), "utf8");
  const goreleaser = fs.readFileSync(path.join(repoRoot, ".goreleaser.yaml"), "utf8");

  assert.match(goreleaser, /before:\s*\n\s+hooks:[\s\S]*?scripts\/build-vmd\.sh/);
  assert.match(
    goreleaser,
    /id:\s+crabbox-apple-vm-helper[\s\S]*?env:\s*\n\s+- CGO_ENABLED=0[\s\S]*?flags:\s*\n\s+- -tags=vmdembed/,
  );
  assert.match(
    ciWorkflow,
    /name:\s+Apple VM[\s\S]*?name:\s+Build Swift VM daemon[\s\S]*?scripts\/build-vmd\.sh[\s\S]*?-tags vmdembed[\s\S]*?name:\s+Verify embedded daemon payload[\s\S]*?vmd-info/,
  );
  assert.match(
    ciWorkflow,
    /name:\s+Verify snapshot helper embeds VM daemon[\s\S]*?find dist -type f -name crabbox-apple-vm-helper[\s\S]*?vmd-info[\s\S]*?"embedded":true/,
  );
  const buildScript = fs.readFileSync(path.join(repoRoot, "scripts", "build-vmd.sh"), "utf8");
  assert.match(buildScript, /swift build --package-path vmd -c release/);
  assert.match(buildScript, /expected 13\.0/);
});
