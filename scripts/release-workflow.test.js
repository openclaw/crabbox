import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";

const repoRoot = path.resolve(import.meta.dirname, "..");

test("manual release checks out requested tag before setup-go reads go.mod", () => {
  const workflow = fs.readFileSync(path.join(repoRoot, ".github", "workflows", "release.yml"), "utf8");
  const verifySource = workflow.indexOf("- name: Verify manual release source and tag");
  const stashConfig = workflow.indexOf("- name: Stash current GoReleaser config");
  const checkoutTag = workflow.indexOf("- name: Checkout release tag");
  const setupGo = workflow.indexOf("- name: Setup Go");

  assert.notEqual(verifySource, -1);
  assert.notEqual(stashConfig, -1);
  assert.notEqual(checkoutTag, -1);
  assert.notEqual(setupGo, -1);
  assert.ok(verifySource < stashConfig, "manual source and tag should be verified before config is saved");
  assert.ok(stashConfig < checkoutTag, "GoReleaser config should be saved before checking out historical tags");
  assert.ok(checkoutTag < setupGo, "setup-go should read go.mod from the requested release tag");
  assert.match(
    workflow.slice(verifySource, stashConfig),
    /WORKFLOW_REF.*refs\/heads\/\$DEFAULT_BRANCH[\s\S]*\^v\(0\|\[1-9\]\[0-9\]\*\)[\s\S]*tag_ref="refs\/tags\/\$RELEASE_TAG"[\s\S]*git merge-base --is-ancestor "\$tag_ref\^\{commit\}" HEAD/,
  );
  assert.match(workflow.slice(checkoutTag, setupGo), /git checkout --detach "refs\/tags\/\$RELEASE_TAG"/);
  assert.doesNotMatch(
    workflow.slice(checkoutTag),
    /cp \.goreleaser\.yaml \/tmp\/\.goreleaser\.yaml/,
    "the selected release tag must not replace the reviewed GoReleaser config",
  );
  assert.match(workflow.slice(setupGo), /go-version-file:\s+go\.mod/);
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

test("manual retries replace only incomplete releases", () => {
  const workflow = fs.readFileSync(path.join(repoRoot, ".github", "workflows", "release.yml"), "utf8");
  const resolveTag = workflow.indexOf("- name: Resolve release tag");
  const removeIncomplete = workflow.indexOf("- name: Remove incomplete manual release");
  const goreleaser = workflow.indexOf("- name: GoReleaser");
  const retryStep = workflow.slice(removeIncomplete, goreleaser);

  assert.ok(resolveTag < removeIncomplete, "the release tag must be resolved before retry cleanup");
  assert.ok(removeIncomplete < goreleaser, "an incomplete release must be removed before publishing");
  assert.match(retryStep, /if:\s+\$\{\{ github\.event_name == 'workflow_dispatch' \}\}/);
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
