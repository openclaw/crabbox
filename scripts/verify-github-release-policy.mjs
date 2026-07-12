#!/usr/bin/env node

import fs from "node:fs";

function fail(message) {
  throw new Error(message);
}

if (process.argv.length !== 7) {
  fail(
    "usage: verify-github-release-policy.mjs <repository-json> <rulesets-json> <owner/repo> <default-branch> <tag>",
  );
}

const [, , repositoryFile, rulesetsFile, expectedRepository, defaultBranch, tag] = process.argv;
if (!/^[a-zA-Z0-9_.-]+\/[a-zA-Z0-9_.-]+$/.test(expectedRepository)) {
  fail("invalid repository identity");
}
if (!/^[a-zA-Z0-9._/-]+$/.test(defaultBranch) || defaultBranch.includes("..")) {
  fail("invalid default branch");
}
if (!/^v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)$/.test(tag)) {
  fail("invalid release tag");
}

const repository = JSON.parse(fs.readFileSync(repositoryFile, "utf8"));
const rulesets = JSON.parse(fs.readFileSync(rulesetsFile, "utf8"));
if (
  repository?.full_name !== expectedRepository ||
  repository?.default_branch !== defaultBranch ||
  !Array.isArray(rulesets)
) {
  fail("repository or ruleset inventory does not match the release policy input");
}

function exactActiveRuleset(value, target) {
  return (
    value &&
    value.target === target &&
    value.enforcement === "active" &&
    Array.isArray(value.bypass_actors) &&
    value.bypass_actors.length === 0 &&
    Array.isArray(value.rules) &&
    value.conditions?.ref_name &&
    Array.isArray(value.conditions.ref_name.include) &&
    Array.isArray(value.conditions.ref_name.exclude)
  );
}

function includesBranch(value) {
  const includes = value.conditions.ref_name.include;
  const excludes = value.conditions.ref_name.exclude;
  return (
    (includes.includes("~DEFAULT_BRANCH") || includes.includes(`refs/heads/${defaultBranch}`)) &&
    excludes.length === 0
  );
}

function includesStableTags(value) {
  const includes = value.conditions.ref_name.include;
  const excludes = value.conditions.ref_name.exclude;
  return (
    (includes.includes("~ALL") ||
      includes.includes("refs/tags/v*") ||
      includes.includes("refs/tags/v**")) &&
    excludes.length === 0
  );
}

function rule(value, type) {
  return value.rules.find((entry) => entry?.type === type);
}

const branchPolicy = rulesets.find((value) => {
  if (!exactActiveRuleset(value, "branch") || !includesBranch(value)) return false;
  const pullRequest = rule(value, "pull_request")?.parameters;
  const statusChecks = rule(value, "required_status_checks")?.parameters;
  return (
    rule(value, "deletion") &&
    rule(value, "non_fast_forward") &&
    pullRequest?.dismiss_stale_reviews_on_push === true &&
    pullRequest?.require_code_owner_review === true &&
    pullRequest?.require_last_push_approval === true &&
    Number.isSafeInteger(pullRequest?.required_approving_review_count) &&
    pullRequest.required_approving_review_count >= 1 &&
    statusChecks?.strict_required_status_checks_policy === true &&
    Array.isArray(statusChecks?.required_status_checks) &&
    statusChecks.required_status_checks.length >= 1
  );
});
if (!branchPolicy) {
  fail("default branch lacks one active no-bypass PR, CODEOWNER, status, deletion, and non-fast-forward ruleset");
}

const tagPolicy = rulesets.find(
  (value) =>
    exactActiveRuleset(value, "tag") &&
    includesStableTags(value) &&
    rule(value, "deletion") &&
    rule(value, "non_fast_forward"),
);
if (!tagPolicy) {
  fail("stable release tags lack one active no-bypass deletion and update protection ruleset");
}

process.stdout.write(
  `${JSON.stringify({ branchRulesetId: branchPolicy.id, tagRulesetId: tagPolicy.id })}\n`,
);
