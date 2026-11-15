import assert from "node:assert/strict";
import test from "node:test";

import { verifyCandidateIdentity } from "./image-qualification-control.mjs";

const candidateSha = "a".repeat(40);
const workflowSha = "b".repeat(40);
const artifactDigest = `sha256:${"c".repeat(64)}`;

function fixture(t) {
  const environment = {
    GH_TOKEN: "test-token",
    GITHUB_REPOSITORY: "example/repo",
    GITHUB_RUN_ID: "42",
    GITHUB_RUN_ATTEMPT: "1",
    GITHUB_WORKFLOW_REF: "example/repo/.github/workflows/image-qualification.yml@refs/heads/main",
    QUALIFICATION_MODE: "first-walk",
    QUALIFICATION_PULL_REQUEST: "",
    QUALIFICATION_CANDIDATE_SHA: candidateSha,
    QUALIFICATION_WORKFLOW_SHA: workflowSha,
    QUALIFICATION_DEFAULT_BRANCH: "main",
    QUALIFICATION_CONFIRM: "qualify",
    QUALIFICATION_CANDIDATE_ARTIFACT_DIGEST: artifactDigest,
    QUALIFICATION_CANDIDATE_ARTIFACT_ID: "7",
    QUALIFICATION_CANDIDATE_RUN_ID: "42",
    QUALIFICATION_RETAINED_IMAGE_ID: "",
    QUALIFICATION_RETAINED_SNAPSHOT_ID: "",
    QUALIFICATION_RETAINED_SOURCE_SHA: "",
    QUALIFICATION_RETAINED_CAPSULE_SHA256: "",
  };
  const original = Object.fromEntries(Object.keys(environment).map((k) => [k, process.env[k]]));
  const originalFetch = globalThis.fetch;
  Object.assign(process.env, environment);
  t.after(() => {
    globalThis.fetch = originalFetch;
    for (const [key, value] of Object.entries(original)) {
      if (value === undefined) delete process.env[key];
      else process.env[key] = value;
    }
  });
  const state = {
    comparison: {
      status: "ahead",
      base_commit: { sha: candidateSha },
      merge_base_commit: { sha: candidateSha },
    },
    branch: { sha: workflowSha },
    run: {
      id: 42,
      run_attempt: 1,
      event: "workflow_dispatch",
      head_sha: workflowSha,
      head_repository: { full_name: "example/repo" },
      path: ".github/workflows/image-qualification.yml@refs/heads/main",
    },
    artifact: {
      id: 7,
      name: "image-qualification-candidate-42",
      expired: false,
      digest: artifactDigest,
      workflow_run: { id: 42, head_sha: workflowSha },
    },
    comparisonStatus: 200,
    requests: [],
  };
  globalThis.fetch = async (input) => {
    const url = new URL(input);
    assert.equal(url.origin, "https://api.github.com");
    state.requests.push(url.pathname);
    let body;
    if (url.pathname.startsWith("/repos/example/repo/compare/"))
      return new Response(JSON.stringify(state.comparison), { status: state.comparisonStatus });
    if (url.pathname === "/repos/example/repo/commits/main") body = state.branch;
    else if (url.pathname === "/repos/example/repo/actions/runs/42") body = state.run;
    else if (url.pathname === "/repos/example/repo/actions/artifacts/7") body = state.artifact;
    else throw new Error(`unexpected API request: ${url.pathname}`);
    return new Response(JSON.stringify(body), { status: 200 });
  };
  return state;
}

test("first-walk admits an exact accepted ancestor without a placeholder PR", async (t) => {
  const state = fixture(t);
  assert.deepEqual(await verifyCandidateIdentity({ artifact: true }), {
    candidateSha,
    workflowSha,
    repository: "example/repo",
    artifactDigest,
    artifactId: "7",
    runId: "42",
  });
  assert.ok(
    state.requests.includes(`/repos/example/repo/compare/${candidateSha}...${workflowSha}`),
  );
  assert.ok(state.requests.every((p) => !p.includes("/pulls/")));
});

test("first-walk also admits the exact protected commit when package guards allow it", async (t) => {
  const state = fixture(t);
  process.env.QUALIFICATION_CANDIDATE_SHA = workflowSha;
  state.comparison = {
    status: "identical",
    base_commit: { sha: workflowSha },
    merge_base_commit: { sha: workflowSha },
  };
  assert.equal((await verifyCandidateIdentity()).candidateSha, workflowSha);
});

for (const [name, change] of [
  ["unmerged source", (s) => (s.comparison.status = "diverged")],
  ["source ahead of protected main", (s) => (s.comparison.status = "behind")],
  ["different merge base", (s) => (s.comparison.merge_base_commit.sha = "d".repeat(40))],
  ["wrong comparison base", (s) => (s.comparison.base_commit.sha = "d".repeat(40))],
  ["inconsistent identical status", (s) => (s.comparison.status = "identical")],
  ["missing ancestry evidence", (s) => (s.comparison = {})],
  ["advanced protected branch", (s) => (s.branch.sha = "d".repeat(40))],
]) {
  test(`first-walk rejects ${name} before artifact admission`, async (t) => {
    const state = fixture(t);
    change(state);
    await assert.rejects(
      verifyCandidateIdentity({ artifact: true }),
      /source admission.*accepted ancestor SHA.*cleanup.*fresh dispatch.*current protected main/,
    );
    assert.ok(state.requests.every((p) => !p.includes("/actions/") && !p.includes("/pulls/")));
  });
}

for (const status of [403, 404]) {
  test(`ancestry API ${status} fails closed without PR fallback`, async (t) => {
    const state = fixture(t);
    state.comparisonStatus = status;
    await assert.rejects(verifyCandidateIdentity(), new RegExp(`returned ${status}`));
    assert.ok(state.requests.every((p) => !p.includes("/pulls/")));
  });
}

for (const [name, key, value, error] of [
  ["ambiguous PR input", "QUALIFICATION_PULL_REQUEST", "7", /not a pull request/],
  ["abbreviated source", "QUALIFICATION_CANDIDATE_SHA", "abc123", /CANDIDATE_SHA/],
  ["branch-name source", "QUALIFICATION_CANDIDATE_SHA", "main", /CANDIDATE_SHA/],
  ["missing confirmation", "QUALIFICATION_CONFIRM", "no", /confirmation mismatch/],
  ["workflow rerun", "GITHUB_RUN_ATTEMPT", "2", /reruns are not allowed/],
  [
    "unprotected workflow",
    "GITHUB_WORKFLOW_REF",
    "example/repo/other@refs/heads/main",
    /protected/,
  ],
]) {
  test(`first-walk rejects ${name} before requests`, async (t) => {
    const state = fixture(t);
    process.env[key] = value;
    await assert.rejects(verifyCandidateIdentity(), error);
    assert.deepEqual(state.requests, []);
  });
}

for (const [name, change] of [
  ["foreign producer", (s) => (s.run.head_repository.full_name = "other/repo")],
  ["different producer source", (s) => (s.run.head_sha = candidateSha)],
  ["producer rerun", (s) => (s.run.run_attempt = 2)],
  ["unprotected producer", (s) => (s.run.path = ".github/workflows/other.yml")],
  ["expired artifact", (s) => (s.artifact.expired = true)],
  ["replaced artifact", (s) => (s.artifact.digest = `sha256:${"d".repeat(64)}`)],
  ["another run's artifact", (s) => (s.artifact.workflow_run.id = 99)],
]) {
  test(`first-walk retains rejection of ${name}`, async (t) => {
    const state = fixture(t);
    change(state);
    await assert.rejects(
      verifyCandidateIdentity({ artifact: true }),
      /candidate build or artifact/,
    );
  });
}

for (const mode of ["mint", "retained"]) {
  test(`${mode} qualification still requires an open PR input`, async (t) => {
    const state = fixture(t);
    process.env.QUALIFICATION_MODE = mode;
    if (mode === "retained") {
      Object.assign(process.env, {
        QUALIFICATION_RETAINED_IMAGE_ID: "ami-00000000000000001",
        QUALIFICATION_RETAINED_SNAPSHOT_ID: "snap-00000000000000001",
        QUALIFICATION_RETAINED_SOURCE_SHA: candidateSha,
        QUALIFICATION_RETAINED_CAPSULE_SHA256: "d".repeat(64),
      });
    }
    await assert.rejects(verifyCandidateIdentity(), /QUALIFICATION_PULL_REQUEST/);
    assert.deepEqual(state.requests, []);
  });
}
