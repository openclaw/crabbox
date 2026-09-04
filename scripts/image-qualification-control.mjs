#!/usr/bin/env node

import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import process from "node:process";
import { execFileSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const sha40 = /^[0-9a-f]{40}$/;
const sha64 = /^[0-9a-f]{64}$/;
const workerNamePattern = /^crabbox-image-qualification-[0-9]+-[0-9]+$/;
const maxRunMs = 120 * 60 * 1000;
const controllerName = "crabbox-image-qualification-controller";
const authorityName = "crabbox-aws-qualification-authority";
const candidateWorkflowFile = "image-qualification-candidate.yml";
const candidateWorkflowPath = `.github/workflows/${candidateWorkflowFile}`;
const controllerSource = path.join(root, "scripts/image-qualification-controller-worker.mjs");

export function canonical(value) {
  if (Array.isArray(value)) return `[${value.map(canonical).join(",")}]`;
  if (value && typeof value === "object") {
    return `{${Object.keys(value)
      .sort()
      .map((key) => `${JSON.stringify(key)}:${canonical(value[key])}`)
      .join(",")}}`;
  }
  return JSON.stringify(value);
}

export function digest(value) {
  const data = Buffer.isBuffer(value) ? value : Buffer.from(String(value));
  return crypto.createHash("sha256").update(data).digest("hex");
}

function required(name, pattern) {
  const value = process.env[name]?.trim() ?? "";
  if (!value || (pattern && !pattern.test(value))) throw new Error(`invalid or missing ${name}`);
  return value;
}

function boundedString(name, max = 256) {
  const value = required(name);
  if (Buffer.byteLength(value) > max) throw new Error(`${name} is too long`);
  return value;
}

function readJSON(file) {
  if (fs.statSync(file).size > 1024 * 1024) throw new Error(`JSON file is too large: ${file}`);
  return JSON.parse(fs.readFileSync(file, "utf8"));
}

function writeJSON(file, value, mode = 0o600) {
  fs.mkdirSync(path.dirname(file), { recursive: true, mode: 0o700 });
  fs.writeFileSync(file, `${JSON.stringify(value, null, 2)}\n`, { mode });
}

function appendOutput(name, value) {
  const output = required("GITHUB_OUTPUT");
  fs.appendFileSync(output, `${name}=${String(value).replaceAll("\n", "")}\n`, { mode: 0o600 });
}

async function github(pathname) {
  const token = required("GH_TOKEN");
  const response = await fetch(`https://api.github.com${pathname}`, {
    headers: {
      accept: "application/vnd.github+json",
      authorization: `Bearer ${token}`,
      "user-agent": "crabbox-image-qualification",
      "x-github-api-version": "2022-11-28",
    },
  });
  if (!response.ok) throw new Error(`GitHub API ${pathname} returned ${response.status}`);
  return await response.json();
}

async function candidateArtifact(repository, number, candidateSha) {
  const query = new URLSearchParams({
    event: "pull_request",
    head_sha: candidateSha,
    status: "completed",
    per_page: "100",
  });
  const response = await github(
    `/repos/${repository}/actions/workflows/${encodeURIComponent(candidateWorkflowFile)}/runs?${query}`,
  );
  const runs = (response.workflow_runs ?? [])
    .filter(
      (run) =>
        run.event === "pull_request" &&
        run.conclusion === "success" &&
        run.head_sha === candidateSha &&
        run.head_repository?.full_name === repository &&
        run.path === candidateWorkflowPath &&
        run.pull_requests?.some((pull) => String(pull.number) === number),
    )
    .sort((left, right) => right.id - left.id);
  if (runs.length === 0) {
    throw new Error("exact candidate build is absent");
  }
  const run = runs[0];
  const artifactResponse = await github(
    `/repos/${repository}/actions/runs/${run.id}/artifacts?per_page=100`,
  );
  const artifacts = (artifactResponse.artifacts ?? []).filter(
    (artifact) =>
      artifact.name === `image-qualification-candidate-${run.id}` &&
      artifact.expired === false &&
      artifact.workflow_run?.id === run.id &&
      artifact.workflow_run?.head_sha === candidateSha,
  );
  if (artifacts.length !== 1) {
    throw new Error("exact candidate artifact is absent or ambiguous");
  }
  const artifact = artifacts[0];
  if (!/^sha256:[0-9a-f]{64}$/.test(artifact.digest ?? "")) {
    throw new Error("candidate artifact digest is absent");
  }
  return {
    artifactDigest: artifact.digest,
    artifactId: String(artifact.id),
    runId: String(run.id),
  };
}

export async function verifyCandidateIdentity({ artifact = false, cleanup = false } = {}) {
  const repository = required("GITHUB_REPOSITORY", /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/);
  const number = required("QUALIFICATION_PULL_REQUEST", /^[1-9][0-9]*$/);
  const candidateSha = required("QUALIFICATION_CANDIDATE_SHA", sha40);
  const workflowSha = required("QUALIFICATION_WORKFLOW_SHA", sha40);
  const workflowRef = required("GITHUB_WORKFLOW_REF");
  const defaultBranch = boundedString("QUALIFICATION_DEFAULT_BRANCH", 128);
  const expectedRef = `${repository}/.github/workflows/image-qualification.yml@refs/heads/${defaultBranch}`;
  if (!cleanup) {
    if (required("QUALIFICATION_CONFIRM") !== "qualify") throw new Error("confirmation mismatch");
    if (required("GITHUB_RUN_ATTEMPT") !== "1") throw new Error("reruns are not allowed");
    if (workflowRef !== expectedRef)
      throw new Error("workflow is not the protected default-branch copy");
  }
  const [pull, branch] = await Promise.all([
    github(`/repos/${repository}/pulls/${number}`),
    github(`/repos/${repository}/commits/${encodeURIComponent(defaultBranch)}`),
  ]);
  const exact =
    pull.state === "open" &&
    pull.head?.repo?.full_name === repository &&
    pull.head?.sha === candidateSha &&
    pull.base?.sha === workflowSha &&
    branch.sha === workflowSha;
  if (!exact) throw new Error("pull request, candidate SHA, or protected workflow SHA changed");
  if (!artifact) return { candidateSha, number, repository, workflowSha };
  const candidate = await candidateArtifact(repository, number, candidateSha);
  const expected = {
    artifactDigest: process.env.QUALIFICATION_CANDIDATE_ARTIFACT_DIGEST?.trim(),
    artifactId: process.env.QUALIFICATION_CANDIDATE_ARTIFACT_ID?.trim(),
    runId: process.env.QUALIFICATION_CANDIDATE_RUN_ID?.trim(),
  };
  if (
    Object.values(expected).some(Boolean) &&
    (candidate.artifactDigest !== expected.artifactDigest ||
      candidate.artifactId !== expected.artifactId ||
      candidate.runId !== expected.runId)
  ) {
    throw new Error("candidate build or artifact changed");
  }
  return { candidateSha, number, repository, workflowSha, ...candidate };
}

function filesUnder(directory) {
  const result = [];
  const visit = (current) => {
    for (const entry of fs
      .readdirSync(current, { withFileTypes: true })
      .sort((a, b) => a.name.localeCompare(b.name))) {
      const absolute = path.join(current, entry.name);
      if (entry.isDirectory()) visit(absolute);
      else if (entry.isFile()) result.push(absolute);
      else throw new Error(`artifact contains a non-file entry: ${absolute}`);
    }
  };
  visit(directory);
  return result;
}

export function createManifest(candidateDir, artifactDir, candidateSha, workflowSha) {
  if (!sha40.test(candidateSha) || !sha40.test(workflowSha))
    throw new Error("invalid manifest SHA");
  const actualSha = execFileSync("git", ["rev-parse", "HEAD"], {
    cwd: candidateDir,
    encoding: "utf8",
  }).trim();
  if (actualSha !== candidateSha) throw new Error("candidate checkout SHA mismatch");
  const files = filesUnder(artifactDir)
    .filter((file) => path.basename(file) !== "manifest.json")
    .map((file) => ({
      path: path.relative(artifactDir, file).split(path.sep).join("/"),
      bytes: fs.statSync(file).size,
      sha256: digest(fs.readFileSync(file)),
    }));
  if (
    files.length > 1024 ||
    files.some((file) => file.bytes > 64 * 1024 * 1024) ||
    files.reduce((total, file) => total + file.bytes, 0) > 128 * 1024 * 1024
  ) {
    throw new Error("candidate artifact exceeds the file or byte limit");
  }
  if (!files.some((file) => file.path === "bin/crabbox"))
    throw new Error("candidate CLI is absent");
  const modules = files.filter(
    (file) => file.path.startsWith("worker/") && /\.(?:m?js)$/.test(file.path),
  );
  if (modules.length === 0) throw new Error("candidate Worker bundle is absent");
  const manifest = {
    version: 1,
    candidateSha,
    workflowSha,
    createdAt: new Date().toISOString(),
    mainModule: modules.find((file) => /(^|\/)index\.js$/.test(file.path))?.path ?? modules[0].path,
    files,
  };
  manifest.manifestSha256 = digest(canonical(manifest));
  writeJSON(path.join(artifactDir, "manifest.json"), manifest);
  return manifest;
}

export function verifyManifest(artifactDir, expectedCandidate, expectedWorkflow) {
  const manifest = readJSON(path.join(artifactDir, "manifest.json"));
  const unsigned = { ...manifest };
  delete unsigned.manifestSha256;
  if (
    manifest.version !== 1 ||
    manifest.candidateSha !== expectedCandidate ||
    manifest.workflowSha !== expectedWorkflow ||
    !sha64.test(manifest.manifestSha256) ||
    digest(canonical(unsigned)) !== manifest.manifestSha256
  ) {
    throw new Error("candidate manifest identity mismatch");
  }
  const expected = new Set(["manifest.json"]);
  for (const entry of manifest.files) {
    if (!/^[A-Za-z0-9_.\/-]+$/.test(entry.path) || entry.path.includes("..")) {
      throw new Error("candidate manifest path is unsafe");
    }
    const file = path.join(artifactDir, entry.path);
    expected.add(entry.path);
    if (
      !fs.statSync(file).isFile() ||
      fs.statSync(file).size !== entry.bytes ||
      digest(fs.readFileSync(file)) !== entry.sha256
    ) {
      throw new Error(`candidate artifact digest mismatch: ${entry.path}`);
    }
  }
  const actual = filesUnder(artifactDir).map((file) =>
    path.relative(artifactDir, file).split(path.sep).join("/"),
  );
  if (actual.some((file) => !expected.has(file)))
    throw new Error("candidate artifact has extra files");
  return manifest;
}

export function verifyPublisherContract(source) {
  if (typeof source !== "string" || Buffer.byteLength(source) > 256 * 1024) {
    throw new Error("candidate publisher source is invalid");
  }
  const requiredPatterns = [
    /CRABBOX_BIN="\$\{CRABBOX_BIN:-/,
    /trap cleanup EXIT/,
    /rollback_promoted_image\(\)/,
    /--expected-current-image "\$current_id".*--expected-current-revision "\$current_revision".*--retire-expected-catalog "\$rollback_image"/,
    /promote_args=\(image promote .*--expected-current-image capture\)/,
    /restored previous default image=%s/,
  ];
  if (requiredPatterns.some((pattern) => !pattern.test(source))) {
    throw new Error("candidate publisher lacks the transactional rollback contract");
  }
  const ordered = [
    'run_cmd "$CRABBOX_BIN" stop --provider aws --target "$target" "$candidate_lease"',
    'candidate_lease=""',
    "rollback_pending=1",
    'run_json_tee "$promotion_log" "$CRABBOX_BIN" "${promote_args[@]}"',
    'promoted_lease="$(warmup promoted)"',
    'smoke "$promoted_lease"',
    "rollback_pending=0",
  ];
  let cursor = 0;
  for (const marker of ordered) {
    const next = source.indexOf(marker, cursor);
    if (next === -1) {
      throw new Error("candidate publisher rollback ordering is not admissible");
    }
    cursor = next + marker.length;
  }
}

class Cloudflare {
  constructor(accountId, token) {
    this.accountId = accountId;
    this.token = token;
    this.base = `https://api.cloudflare.com/client/v4/accounts/${accountId}`;
  }

  async request(pathname, init = {}, { allow404 = false } = {}) {
    const response = await fetch(`${this.base}${pathname}`, {
      ...init,
      headers: { authorization: `Bearer ${this.token}`, ...(init.headers ?? {}) },
    });
    if (allow404 && response.status === 404) return undefined;
    const text = await response.text();
    let parsed;
    try {
      parsed = text ? JSON.parse(text) : {};
    } catch {
      parsed = { raw: text.slice(0, 200) };
    }
    if (!response.ok || parsed.success === false) {
      throw new Error(`Cloudflare API ${pathname} returned ${response.status}`);
    }
    return parsed.result;
  }

  async upload(name, moduleFile, metadata, extraModules = []) {
    const form = new FormData();
    form.set("metadata", new Blob([JSON.stringify(metadata)], { type: "application/json" }));
    form.set(
      metadata.main_module,
      new Blob([fs.readFileSync(moduleFile)], { type: "application/javascript+module" }),
      metadata.main_module,
    );
    for (const module of extraModules) {
      form.set(
        module.name,
        new Blob([fs.readFileSync(module.file)], { type: module.type }),
        module.name,
      );
    }
    return await this.request(`/workers/scripts/${name}`, { method: "PUT", body: form });
  }

  async settings(name) {
    return await this.request(`/workers/scripts/${name}/settings`);
  }

  async latestVersion(name) {
    const versions = await this.request(`/workers/scripts/${name}/versions`);
    const version = versions?.items?.[0] ?? versions?.[0];
    if (!version?.id) throw new Error("Cloudflare did not return a Worker version");
    return version.id;
  }

  async enableSubdomain(name) {
    await this.request(`/workers/scripts/${name}/subdomain`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ enabled: true, previews_enabled: false }),
    });
    const subdomain = await this.request("/workers/subdomain");
    if (!subdomain?.subdomain)
      throw new Error("Cloudflare account workers.dev subdomain is absent");
    return `https://${name}.${subdomain.subdomain}.workers.dev`;
  }

  async deleteScript(name) {
    await this.request(
      `/workers/scripts/${name}?force=true`,
      { method: "DELETE" },
      { allow404: true },
    );
    if (
      (await this.request(`/workers/scripts/${name}/settings`, {}, { allow404: true })) !==
      undefined
    ) {
      throw new Error(`Cloudflare Worker deletion was not observable for ${name}`);
    }
  }
}

function qualificationConfig() {
  const rootGB = Number(required("QUALIFICATION_ROOT_GB", /^(?:[89]|1[0-9]|20)$/));
  return {
    region: required("QUALIFICATION_AWS_REGION", /^[a-z]{2}-[a-z]+-[0-9]+$/),
    subnetId: required("QUALIFICATION_SUBNET_ID", /^subnet-[0-9a-f]+$/),
    securityGroupId: required("QUALIFICATION_SECURITY_GROUP_ID", /^sg-[0-9a-f]+$/),
    baseAmiId: required("QUALIFICATION_BASE_AMI_ID", /^ami-[0-9a-f]+$/),
    rootGB,
    instanceTypes: ["t3.small", "t3a.small"],
    maxMinutes: 120,
    maxMonthlyUSD: 10,
    maxConcurrentInstances: 1,
    maxLaunches: 3,
    fastSnapshotRestore: false,
  };
}

function candidateBindings(identity, adminToken, sharedToken, config) {
  const vars = {
    CRABBOX_DEFAULT_ORG: "image-qualification",
    CRABBOX_GITHUB_ALLOWED_ORG: "example-org.invalid",
    CRABBOX_WORKSPACE_PROVIDER: "aws",
    CRABBOX_WORKSPACE_CLASS: "standard",
    CRABBOX_WORKSPACE_PREWARM_COUNT: "0",
    CRABBOX_AWS_REGION: config.region,
    CRABBOX_CAPACITY_REGIONS: config.region,
    CRABBOX_AWS_AMI: config.baseAmiId,
    CRABBOX_AWS_SUBNET_ID: config.subnetId,
    CRABBOX_AWS_SECURITY_GROUP_ID: config.securityGroupId,
    CRABBOX_AWS_ROOT_GB: String(config.rootGB),
    CRABBOX_AWS_INSTANCE_PROFILE: "",
    CRABBOX_AWS_ORPHAN_SWEEP_ENABLED: "0",
    CRABBOX_AWS_ORPHAN_SWEEP_DELETE: "0",
    CRABBOX_MAX_ACTIVE_LEASES: "1",
    CRABBOX_MAX_ACTIVE_LEASES_PER_OWNER: "1",
    CRABBOX_MAX_ACTIVE_LEASES_PER_ORG: "1",
    CRABBOX_MAX_ACTIVE_LEASES_PER_CAPACITY_ADMIN: "1",
    CRABBOX_MAX_CHECKPOINTS: "1",
    CRABBOX_MAX_CHECKPOINTS_PER_OWNER: "1",
    CRABBOX_MAX_CHECKPOINTS_PER_ORG: "1",
    CRABBOX_MAX_MONTHLY_USD: "10",
    CRABBOX_MAX_MONTHLY_USD_PER_OWNER: "10",
    CRABBOX_MAX_MONTHLY_USD_PER_ORG: "10",
  };
  return [
    ...Object.entries(vars).map(([name, text]) => ({ name, type: "plain_text", text })),
    { name: "CRABBOX_ADMIN_TOKEN", type: "secret_text", text: adminToken },
    { name: "CRABBOX_SHARED_TOKEN", type: "secret_text", text: sharedToken },
    {
      name: "CRABBOX_AWS_QUALIFICATION_TRANSPORT",
      type: "service",
      service: authorityName,
      props: identity,
    },
    { name: "FLEET", type: "durable_object_namespace", class_name: "FleetDurableObject" },
    { name: "CF_VERSION_METADATA", type: "version_metadata" },
  ];
}

function candidateMetadata(mainModule, bindings, createNamespace = false) {
  return {
    main_module: mainModule.replace(/^worker\//, ""),
    compatibility_date: "2026-09-03",
    compatibility_flags: ["nodejs_compat"],
    bindings,
    ...(createNamespace
      ? { migrations: [{ tag: "qualification-v1", new_sqlite_classes: ["FleetDurableObject"] }] }
      : {}),
  };
}

function controllerMetadata(deploymentHash, token) {
  return {
    main_module: "controller.mjs",
    compatibility_date: "2026-09-03",
    bindings: [
      { name: "CONTROLLER_TOKEN", type: "secret_text", text: token },
      {
        name: "AUTHORITY",
        type: "service",
        service: authorityName,
        entrypoint: "AWSQualificationController",
        props: { deploymentHash },
      },
    ],
  };
}

function publicBindingShape(bindings) {
  return bindings
    .map((binding) => ({
      name: binding.name,
      type: binding.type,
      ...(binding.service ? { service: binding.service } : {}),
      ...(binding.entrypoint ? { entrypoint: binding.entrypoint } : {}),
      ...(binding.class_name ? { class_name: binding.class_name } : {}),
      ...(binding.props ? { props: binding.props } : {}),
      ...(binding.type === "plain_text" ? { text: binding.text } : {}),
    }))
    .sort((a, b) => a.name.localeCompare(b.name));
}

async function controllerCall(url, token, action, value = {}) {
  const response = await fetch(`${url}/${action}`, {
    method: "POST",
    headers: {
      authorization: `Bearer ${token}`,
      "content-type": "application/json",
    },
    body: JSON.stringify(value),
  });
  const data = await response.json();
  if (!response.ok)
    throw new Error(
      `qualification controller ${action} failed: ${data.message ?? response.status}`,
    );
  return data;
}

function cloudflareFromEnv() {
  return new Cloudflare(
    required("CLOUDFLARE_ACCOUNT_ID", /^[0-9a-f]{32}$/),
    required("CLOUDFLARE_API_TOKEN"),
  );
}

async function deployController(cf, deploymentHash, token) {
  await cf.upload(controllerName, controllerSource, controllerMetadata(deploymentHash, token));
  return await cf.enableSubdomain(controllerName);
}

async function assertWorkerIsolation(cf, worker, durableObjectCount) {
  const [routes, schedules, domains, namespaceCount] = await Promise.all([
    cf.request(`/workers/services/${worker}/environments/production/routes?show_zonename=true`),
    cf.request(`/workers/scripts/${worker}/schedules`),
    cf.request(`/workers/domains/records?page=0&per_page=100&service=${worker}`),
    namespaceResidue(cf, worker),
  ]);
  if (
    (routes ?? []).length !== 0 ||
    (schedules ?? []).length !== 0 ||
    (domains ?? []).length !== 0 ||
    namespaceCount !== durableObjectCount
  ) {
    throw new Error(`Worker isolation verification failed for ${worker}`);
  }
}

async function deploy() {
  const identityCheck = await verifyCandidateIdentity({ artifact: true });
  const artifactDir = path.resolve(required("QUALIFICATION_ARTIFACT_DIR"));
  const manifest = verifyManifest(
    artifactDir,
    identityCheck.candidateSha,
    identityCheck.workflowSha,
  );
  verifyPublisherContract(
    fs.readFileSync(path.join(artifactDir, "candidate/scripts/mint-aws-devtools-image.sh"), "utf8"),
  );
  const runId = `image-qualification-${required("GITHUB_RUN_ID", /^[1-9][0-9]*$/)}-${required("GITHUB_RUN_ATTEMPT", /^[1-9][0-9]*$/)}`;
  const candidateWorker = `crabbox-${runId}`;
  if (!workerNamePattern.test(candidateWorker)) throw new Error("candidate Worker name is invalid");
  const expiresAt = new Date(Date.now() + maxRunMs).toISOString();
  const owner = `${runId}@example.invalid`;
  const config = qualificationConfig();
  const authoritySha = required("QUALIFICATION_AUTHORITY_SHA", sha40);
  const authorityVersion = boundedString("QUALIFICATION_AUTHORITY_VERSION", 64);
  const baseIdentity = {
    runId,
    owner,
    candidateSha: identityCheck.candidateSha,
    candidateWorker,
    expiresAt,
  };
  const adminToken = crypto.randomBytes(32).toString("hex");
  const sharedToken = crypto.randomBytes(32).toString("hex");
  const controllerToken = required("QUALIFICATION_CONTROLLER_TOKEN");
  const placeholderIdentity = { ...baseIdentity, deploymentHash: "0".repeat(64) };
  const stagingBindings = candidateBindings(placeholderIdentity, adminToken, sharedToken, config);
  const mainFile = path.join(artifactDir, manifest.mainModule);
  const extraModules = manifest.files
    .filter(
      (entry) =>
        entry.path.startsWith("worker/") &&
        entry.path !== manifest.mainModule &&
        /\.(?:m?js|wasm)$/.test(entry.path),
    )
    .map((entry) => ({
      name: entry.path.replace(/^worker\//, ""),
      file: path.join(artifactDir, entry.path),
      type: entry.path.endsWith(".wasm") ? "application/wasm" : "application/javascript+module",
    }));
  const cf = cloudflareFromEnv();
  await cf.upload(
    candidateWorker,
    mainFile,
    candidateMetadata(manifest.mainModule, stagingBindings, true),
    extraModules,
  );
  const [stagingSettings, stagingVersion] = await Promise.all([
    cf.settings(candidateWorker),
    cf.latestVersion(candidateWorker),
  ]);
  const stagingShape = {
    bindings: publicBindingShape(stagingSettings.bindings ?? []),
    compatibilityDate: stagingSettings.compatibility_date,
    compatibilityFlags: stagingSettings.compatibility_flags ?? [],
  };
  if (canonical(stagingShape.bindings) !== canonical(publicBindingShape(stagingBindings))) {
    throw new Error("staging Worker settings do not match the reviewed binding contract");
  }
  const deploymentHash = digest(
    canonical({
      version: 1,
      manifestSha256: manifest.manifestSha256,
      candidateArtifact: {
        digest: identityCheck.artifactDigest,
        id: identityCheck.artifactId,
        runId: identityCheck.runId,
      },
      config,
      stagingVersion,
      stagingSettings: stagingShape,
      authority: { name: authorityName, sha: authoritySha, version: authorityVersion },
      props: baseIdentity,
      secretDigests: {
        admin: digest(adminToken),
        shared: digest(sharedToken),
      },
    }),
  );
  const identity = { ...baseIdentity, deploymentHash };
  const bindings = candidateBindings(identity, adminToken, sharedToken, config);
  await cf.upload(
    candidateWorker,
    mainFile,
    candidateMetadata(manifest.mainModule, bindings),
    extraModules,
  );
  const [settings, candidateVersion, candidateURL] = await Promise.all([
    cf.settings(candidateWorker),
    cf.latestVersion(candidateWorker),
    cf.enableSubdomain(candidateWorker),
  ]);
  const actualBindings = publicBindingShape(settings.bindings ?? []);
  const expectedBindings = publicBindingShape(bindings);
  if (canonical(actualBindings) !== canonical(expectedBindings)) {
    throw new Error("candidate Worker settings do not match the reviewed binding contract");
  }
  const controllerURL = await deployController(cf, deploymentHash, controllerToken);
  await Promise.all([
    assertWorkerIsolation(cf, candidateWorker, 1),
    assertWorkerIsolation(cf, controllerName, 0),
  ]);
  await verifyCandidateIdentity({ artifact: true });
  const record = await controllerCall(controllerURL, controllerToken, "claim", { identity });
  if (record.runId !== runId || record.cleanupState !== "claimed")
    throw new Error("authority claim readback mismatch");
  const proofDir = path.resolve(required("QUALIFICATION_PROOF_DIR"));
  writeJSON(path.join(proofDir, "deployment.json"), {
    version: 1,
    runId,
    candidateSha: identity.candidateSha,
    workflowSha: identityCheck.workflowSha,
    deploymentHash,
    manifestSha256: manifest.manifestSha256,
    candidateArtifactDigest: identityCheck.artifactDigest,
    candidateArtifactId: identityCheck.artifactId,
    candidateRunId: identityCheck.runId,
    candidateWorker,
    candidateVersionDigest: digest(candidateVersion),
    authoritySha,
    authorityVersion,
    expiresAt,
    isolation: {
      workersDev: true,
      previewURLs: false,
      routes: 0,
      customDomains: 0,
      schedules: 0,
      fleetDurableObjectNamespaces: 1,
    },
    limits: {
      instanceTypes: config.instanceTypes,
      maxMinutes: config.maxMinutes,
      maxMonthlyUSD: config.maxMonthlyUSD,
      maxConcurrentInstances: config.maxConcurrentInstances,
      maxLaunches: config.maxLaunches,
      fastSnapshotRestore: config.fastSnapshotRestore,
      rootGB: config.rootGB,
    },
    policyConfigDigest: digest(canonical(config)),
    bindingDigest: digest(canonical(expectedBindings)),
  });
  appendOutput("run_id", runId);
  appendOutput("candidate_worker", candidateWorker);
  appendOutput("candidate_url", candidateURL);
  appendOutput("candidate_admin_token", adminToken);
  appendOutput("candidate_shared_token", sharedToken);
  appendOutput("deployment_hash", deploymentHash);
  appendOutput("expires_at", expiresAt);
  appendOutput("region", config.region);
  appendOutput("base_ami_id", config.baseAmiId);
}

async function namespaceResidue(cf, worker) {
  const namespaces = await cf.request("/workers/durable_objects/namespaces");
  return (namespaces ?? []).filter(
    (namespace) => namespace.script === worker && namespace.class === "FleetDurableObject",
  ).length;
}

async function deleteCandidate(cf, worker) {
  if (!workerNamePattern.test(worker)) throw new Error("refusing to delete an unexpected Worker");
  const tombstone = path.join(
    process.env.RUNNER_TEMP ?? "/tmp",
    `qualification-tombstone-${process.pid}.mjs`,
  );
  fs.writeFileSync(
    tombstone,
    "export default {fetch(){return new Response(null,{status:410})}};\n",
    { mode: 0o600 },
  );
  try {
    await cf.upload(worker, tombstone, {
      main_module: "tombstone.mjs",
      compatibility_date: "2026-09-03",
      bindings: [],
      migrations: [
        {
          old_tag: "qualification-v1",
          new_tag: "qualification-delete",
          deleted_classes: ["FleetDurableObject"],
        },
      ],
    });
    await cf.deleteScript(worker);
  } finally {
    fs.rmSync(tombstone, { force: true });
  }
  const settings = await cf.request(`/workers/scripts/${worker}/settings`, {}, { allow404: true });
  if (settings !== undefined || (await namespaceResidue(cf, worker)) !== 0) {
    throw new Error("candidate Worker or Fleet Durable Object residue remains");
  }
}

async function qualificationCandidates(cf) {
  const scriptsResult = await cf.request("/workers/scripts");
  const scripts = scriptsResult?.items ?? scriptsResult ?? [];
  const candidates = [];
  for (const worker of scripts
    .map((item) => item.id ?? item.name)
    .filter((name) => workerNamePattern.test(name))) {
    const settings = await cf.settings(worker);
    const transport = (settings.bindings ?? []).find(
      (binding) =>
        binding.name === "CRABBOX_AWS_QUALIFICATION_TRANSPORT" &&
        binding.service === authorityName &&
        binding.props?.candidateWorker === worker &&
        sha64.test(binding.props?.deploymentHash ?? ""),
    );
    if (transport) candidates.push({ worker, deploymentHash: transport.props.deploymentHash });
  }
  return candidates;
}

async function cleanupRun({ reaper = false, requireProof = false } = {}) {
  const cf = cloudflareFromEnv();
  const token = required("QUALIFICATION_CONTROLLER_TOKEN");
  let controllerURL;
  let discovered;
  try {
    controllerURL = await cf.enableSubdomain(controllerName);
    discovered = await controllerCall(controllerURL, token, "discover");
  } catch {
    controllerURL = undefined;
  }
  if (!controllerURL || !discovered) {
    if (!reaper) throw new Error("qualification controller is unavailable");
    const candidates = await qualificationCandidates(cf);
    for (const candidate of candidates) {
      if (sha64.test(candidate.deploymentHash)) {
        const candidateControllerURL = await deployController(cf, candidate.deploymentHash, token);
        try {
          discovered = await controllerCall(candidateControllerURL, token, "discover");
          controllerURL = candidateControllerURL;
          break;
        } catch {
          // Try the next isolated candidate. Registry validation rejects the wrong hash.
        }
      }
    }
    if (!discovered) {
      return {
        result: "failed",
        failure: "authority registry could not be queried with any verified candidate binding",
      };
    }
  }
  if (!discovered.run) {
    for (const candidate of await qualificationCandidates(cf)) {
      await deleteCandidate(cf, candidate.worker);
    }
    await cf.deleteScript(controllerName);
    return { result: "idle", reason: "registry has no active run" };
  }
  const run = discovered.run;
  let failure;
  let firstAttestation;
  let qualificationFailure;
  try {
    await controllerCall(controllerURL, token, "finalize", { runId: run.runId });
    firstAttestation = await controllerCall(controllerURL, token, "attest", { runId: run.runId });
    if (requireProof) {
      try {
        verifyQualificationEvidence(firstAttestation, readExecutionProof());
      } catch (error) {
        qualificationFailure = error instanceof Error ? error.message : String(error);
      }
    }
    await deleteCandidate(cf, run.candidateWorker);
    await controllerCall(controllerURL, token, "finalize", { runId: run.runId });
    const finalAttestation = await controllerCall(controllerURL, token, "attest", {
      runId: run.runId,
    });
    if (
      !finalAttestation.finalized ||
      Object.values(finalAttestation.finalReceipt?.finalCounts ?? {}).some(Number)
    ) {
      throw new Error("authority did not attest zero AWS residue");
    }
    await controllerCall(controllerURL, token, "retire", { runId: run.runId });
    const after = await controllerCall(controllerURL, token, "discover");
    if (after.run !== null) throw new Error("authority registry still has an active run");
    await cf.deleteScript(controllerName);
    if (qualificationFailure) {
      return {
        result: "failed",
        runId: run.runId,
        failure: qualificationFailure,
        cleanup: "finalized",
        attestation: finalAttestation,
      };
    }
    return { result: "finalized", runId: run.runId, attestation: finalAttestation };
  } catch (error) {
    failure = error instanceof Error ? error.message : String(error);
    return { result: "failed", runId: run.runId, failure, attestation: firstAttestation };
  }
}

function readExecutionProof() {
  const directory = process.env.QUALIFICATION_EXECUTION_PROOF_DIR?.trim();
  if (!directory || !fs.existsSync(directory))
    throw new Error("execution proof artifact is absent");
  const checksumFile = path.join(directory, "checksums.sha256");
  const lines = fs.readFileSync(checksumFile, "utf8").trim().split("\n").filter(Boolean);
  for (const line of lines) {
    const match = line.match(/^([0-9a-f]{64})  (\.\/[A-Za-z0-9_.-]+)$/);
    if (!match) throw new Error("execution proof checksum manifest is invalid");
    const file = path.join(directory, match[2].slice(2));
    if (digest(fs.readFileSync(file)) !== match[1]) {
      throw new Error("execution proof checksum mismatch");
    }
  }
  return {
    spoof: readJSON(path.join(directory, "spoofed-admin.json")),
    fsr: readJSON(path.join(directory, "fsr-denial.json")),
    hardKill: readJSON(path.join(directory, "executor-hard-kill.json")),
    log: fs.readFileSync(path.join(directory, "candidate-execution.log"), "utf8"),
  };
}

export function verifyQualificationEvidence(attestation, proof) {
  const operations = attestation?.operations ?? [];
  const probeCompletedAt = Date.parse(proof?.spoof?.completedAt ?? "");
  if (
    proof?.spoof?.status !== 403 ||
    proof?.spoof?.catalogUnchanged !== true ||
    !Number.isFinite(probeCompletedAt) ||
    operations.some((operation) => Date.parse(operation.requestedAt) <= probeCompletedAt)
  ) {
    throw new Error(
      "evidence does not prove the spoofed admin probe left signer sequence unchanged",
    );
  }
  const fsr = operations.find((operation) => operation.action.includes("FastSnapshotRestore"));
  if (!fsr || fsr.denialReason !== "policy-denied" || (fsr.signerDispatches ?? []).length !== 0) {
    throw new Error("attestation does not prove pre-signer Fast Snapshot Restore denial");
  }
  const accepted = (action) =>
    operations
      .map((operation, index) => ({ operation, index }))
      .filter(
        ({ operation }) =>
          operation.action === action &&
          operation.signerDispatches?.some((dispatch) => dispatch.outcome === "accepted"),
      );
  const launches = accepted("RunInstances");
  const images = accepted("CreateImage");
  if (
    launches.length !== 3 ||
    images.length !== 1 ||
    !(
      launches[0].index < images[0].index &&
      images[0].index < launches[1].index &&
      launches[1].index < launches[2].index
    )
  ) {
    throw new Error("attestation does not prove source/image/candidate/promoted launch order");
  }
  const receipt = attestation.finalReceipt;
  if (
    !attestation.finalized ||
    !receipt ||
    receipt.failureCodes?.length !== 0 ||
    (receipt.resourcesAtStart?.images ?? 0) < 1 ||
    proof?.fsr?.rejected !== true ||
    proof?.hardKill?.executorKilled !== true ||
    proof?.hardKill?.cloudCredentialsPresent !== false ||
    !proof?.log?.includes("qualification mint exited 86 only after the promoted smoke") ||
    !proof?.log?.includes("restored previous default image=") ||
    !proof?.log?.includes("--retire-expected-catalog")
  ) {
    throw new Error("attestation does not prove owned image recovery after executor exit");
  }
}

function sanitizeEvidence(value) {
  if (Array.isArray(value)) return value.slice(0, 256).map(sanitizeEvidence);
  if (value && typeof value === "object") {
    return Object.fromEntries(
      Object.entries(value)
        .filter(
          ([key]) =>
            !/(token|secret|url|account|resource.?id|instance.?id|image.?id|snapshot.?id|key.?id|ip|user.?data)/i.test(
              key,
            ),
        )
        .slice(0, 256)
        .map(([key, child]) => [key, sanitizeEvidence(child)]),
    );
  }
  return typeof value === "string" ? redactText(value).slice(0, 512) : value;
}

function redactText(value) {
  return value
    .replace(/https?:\/\/[^\s"<>]+/g, "[url]")
    .replace(/\b(?:ami|i|vol|snap|key)-[0-9a-f]{8,}\b/gi, "[aws-resource]")
    .replace(/\b(?:\d{1,3}\.){3}\d{1,3}\b/g, "[ip]")
    .replace(/\b\d{12}\b/g, "[aws-account]");
}

function writeEvidence(result) {
  const proofDir = path.resolve(required("QUALIFICATION_PROOF_DIR"));
  writeJSON(path.join(proofDir, "finalization.json"), sanitizeEvidence(result));
  const files = filesUnder(proofDir).filter((file) => path.basename(file) !== "checksums.sha256");
  const checksums = files
    .map(
      (file) =>
        `${digest(fs.readFileSync(file))}  ${path.relative(proofDir, file).split(path.sep).join("/")}`,
    )
    .join("\n");
  fs.writeFileSync(path.join(proofDir, "checksums.sha256"), `${checksums}\n`, { mode: 0o600 });
}

async function main() {
  const [command, ...args] = process.argv.slice(2);
  if (command === "authorize") {
    const result = await verifyCandidateIdentity({ artifact: true });
    appendOutput("candidate_artifact_digest", result.artifactDigest);
    appendOutput("candidate_artifact_id", result.artifactId);
    appendOutput("candidate_run_id", result.runId);
    appendOutput("candidate_sha", result.candidateSha);
    return;
  }
  if (command === "admit") {
    const result = await verifyCandidateIdentity({ artifact: true });
    const artifactDir = path.resolve(required("QUALIFICATION_ARTIFACT_DIR"));
    verifyManifest(artifactDir, result.candidateSha, result.workflowSha);
    verifyPublisherContract(
      fs.readFileSync(
        path.join(artifactDir, "candidate/scripts/mint-aws-devtools-image.sh"),
        "utf8",
      ),
    );
    return;
  }
  if (command === "manifest") {
    createManifest(
      path.resolve(args[0]),
      path.resolve(args[1]),
      required("QUALIFICATION_CANDIDATE_SHA", sha40),
      required("QUALIFICATION_WORKFLOW_SHA", sha40),
    );
    return;
  }
  if (command === "deploy") return await deploy();
  if (command === "finalize") {
    try {
      await verifyCandidateIdentity({ cleanup: true });
    } catch (error) {
      console.error(
        `identity revalidation warning: ${error instanceof Error ? error.message : String(error)}`,
      );
    }
    const result = await cleanupRun({ reaper: true, requireProof: true });
    writeEvidence(result);
    if (result.result === "failed") throw new Error(result.failure);
    return;
  }
  if (command === "reap") {
    const result = await cleanupRun({ reaper: true });
    writeEvidence(result);
    if (result.result === "failed") throw new Error(result.failure);
    return;
  }
  throw new Error(
    "usage: image-qualification-control.mjs authorize|admit|manifest|deploy|finalize|reap",
  );
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  main().catch((error) => {
    console.error(redactText(error instanceof Error ? error.message : String(error)));
    process.exitCode = 1;
  });
}
