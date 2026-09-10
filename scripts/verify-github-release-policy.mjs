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
const requiredReleaseWorkflow = {
  path: ".github/workflows/crabbox-release-check.yml",
  ref: "refs/heads/main",
  repository_id: 1304559357,
};
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
    Array.isArray(value.rules) &&
    value.conditions?.ref_name &&
    Array.isArray(value.conditions.ref_name.include) &&
    Array.isArray(value.conditions.ref_name.exclude)
  );
}

function fnmatch(pattern, value) {
  if (pattern.includes("\\")) {
    fail("ruleset selector uses unsupported backslash escaping");
  }
  let source = "^";
  for (let index = 0; index < pattern.length; index += 1) {
    const char = pattern[index];
    if (char === "*") {
      if (pattern[index + 1] === "*" && pattern[index + 2] === "/") {
        source += "(?:[^/]+/)*";
        index += 2;
      } else {
        while (pattern[index + 1] === "*") index += 1;
        source += "[^/]*";
      }
    } else if (char === "?") {
      source += "[^/]";
    } else if (char === "[") {
      const end = pattern.indexOf("]", index + 1);
      if (end <= index + 1) {
        fail("ruleset selector has an invalid character class");
      }
      const content = pattern.slice(index + 1, end);
      if (
        content.startsWith("^") ||
        content.startsWith("!") ||
        !/^[A-Za-z0-9._/-]+$/.test(content)
      ) {
        fail("ruleset selector uses an unsupported character class");
      }
      source += `(?!/)[${content}]`;
      index = end;
    } else {
      source += char.replace(/[\\^$+.()|{}]/g, "\\$&");
    }
  }
  try {
    return new RegExp(`${source}$`).test(value);
  } catch {
    fail("ruleset selector has an invalid pattern");
  }
}

function selectorMatchesBranch(selector, branchRef) {
  if (selector === "~ALL") return true;
  if (selector === "~DEFAULT_BRANCH") return true;
  if (selector.startsWith("~")) fail("ruleset selector uses an unsupported special target");
  return fnmatch(selector, branchRef);
}

function includesBranch(value) {
  const includes = value.conditions.ref_name.include;
  const excludes = value.conditions.ref_name.exclude;
  const branchRef = `refs/heads/${defaultBranch}`;
  return (
    includes.some((selector) => selectorMatchesBranch(selector, branchRef)) &&
    !excludes.some((selector) => selectorMatchesBranch(selector, branchRef))
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

const bypassableHistoryPolicy = rulesets.find(
  (value) =>
    exactActiveRuleset(value, "branch") &&
    includesBranch(value) &&
    (rule(value, "deletion") || rule(value, "non_fast_forward")) &&
    value.bypass_actors.length !== 0,
);
if (bypassableHistoryPolicy) {
  fail("default branch has bypassable history protection");
}

const branchHistoryPolicy = rulesets.find((value) => {
  if (
    !exactActiveRuleset(value, "branch") ||
    value.bypass_actors.length !== 0 ||
    !includesBranch(value)
  ) {
    return false;
  }
  return rule(value, "deletion") && rule(value, "non_fast_forward");
});
if (!branchHistoryPolicy) {
  fail(
    "default branch lacks active no-bypass deletion and non-fast-forward protection",
  );
}

const branchWorkflowPolicy = rulesets.find((value) => {
  if (
    !exactActiveRuleset(value, "branch") ||
    value.source_type !== "Organization" ||
    value.source !== "openclaw" ||
    value.bypass_actors.length !== 0 ||
    !includesBranch(value)
  ) {
    return false;
  }
  const workflows = rule(value, "workflows")?.parameters;
  return (
    workflows?.do_not_enforce_on_create === false &&
    Array.isArray(workflows?.workflows) &&
    workflows.workflows.some(
      (entry) =>
        entry?.path === requiredReleaseWorkflow.path &&
        entry?.ref === requiredReleaseWorkflow.ref &&
        entry?.repository_id === requiredReleaseWorkflow.repository_id &&
        entry?.sha == null,
    )
  );
});
if (!branchWorkflowPolicy) {
  fail(
    "default branch lacks the active no-bypass OpenClaw organization release workflow",
  );
}

const tagPolicy = rulesets.find(
  (value) =>
    exactActiveRuleset(value, "tag") &&
    value.bypass_actors.length === 0 &&
    includesStableTags(value) &&
    rule(value, "deletion") &&
    rule(value, "non_fast_forward"),
);
if (!tagPolicy) {
  fail("stable release tags lack one active no-bypass deletion and update protection ruleset");
}

process.stdout.write(
  `${JSON.stringify({
    branchHistoryRulesetId: branchHistoryPolicy.id,
    branchWorkflowRulesetId: branchWorkflowPolicy.id,
    tagRulesetId: tagPolicy.id,
  })}\n`,
);
