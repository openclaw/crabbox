import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { execFile } from "node:child_process";
import {
  after,
  before,
  test,
} from "node:test";
import {
  access,
  chmod,
  cp,
  mkdir,
  mkdtemp,
  readFile,
  realpath,
  readdir,
  rm,
  symlink,
  writeFile,
} from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import {
  canonicalJSON,
  digestBytes,
  digestJSON,
  digestJSONLine,
} from "./aws-image-candidate.mjs";

const execFileAsync = promisify(execFile);
const repoRoot = path.resolve(import.meta.dirname, "..");
let sourceRoot;
let script;
let sourceCommit;
let otherSourceCommit;
let replacementSourceCommit;
let symlinkSourceCommit;
let committedImageRecipe;
let committedReadinessRecipe;
const candidateScript = path.join(repoRoot, "scripts", "aws-image-candidate.mjs");
const schema = path.join(repoRoot, "recipes", "aws", "v1", "qualification-input.schema.json");
const schemaValidatorSource = path.join(
  repoRoot,
  "scripts",
  "validate-json-schema",
  "main.go",
);
const sourceRepository = "example-org/crabbox";
const workflowRef =
  "example-org/crabbox/.github/workflows/devtools-image-publish.yml@refs/heads/main";
const workflowRunId = "12345";
const workflowRunAttempt = "2";
const repository = "ghcr.io/example-org/crabbox-aws-image-candidates";
const emptyConfigDigest = "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a";
const mediaTypes = new Map([
  ["bundle.json", "application/vnd.crabbox.aws-image-evidence-manifest.v1+json"],
  ["candidate.json", "application/vnd.crabbox.aws-image-candidate.v1+json"],
  ["provenance.intoto.jsonl", "application/vnd.in-toto+json"],
  ["recipe.json", "application/json"],
  ["sbom.spdx.json", "application/spdx+json"],
  ["scrub-report.json", "application/json"],
]);
const schemaValidatorRoots = new Set();
const schemaValidatorHistory = [];
let schemaValidatorBuildCount = 0;
let schemaValidatorPromise;

async function sourceGit(args) {
  return execFileAsync("git", args, {
    cwd: sourceRoot,
    env: {
      ...process.env,
      GIT_AUTHOR_NAME: "Crabbox Test",
      GIT_AUTHOR_EMAIL: "test@example.invalid",
      GIT_COMMITTER_NAME: "Crabbox Test",
      GIT_COMMITTER_EMAIL: "test@example.invalid",
    },
  });
}

before(async () => {
  sourceRoot = await realpath(
    await mkdtemp(path.join(os.tmpdir(), "crabbox-qualification-source-")),
  );
  for (const directory of [
    "scripts",
    "recipes/aws/v1",
    "recipes/linux/v1",
  ]) {
    await mkdir(path.join(sourceRoot, directory), { recursive: true });
  }
  for (const relative of [
    "scripts/aws-image-candidate.mjs",
    "scripts/consume-aws-image-candidate.mjs",
    "scripts/consume-aws-image-candidate.sh",
    "recipes/aws/v1/linux-devtools.json",
    "recipes/aws/v1/windows-devtools.json",
    "recipes/linux/v1/linux-builder.json",
    "recipes/linux/v1/linux-minimal.json",
  ]) {
    await cp(path.join(repoRoot, relative), path.join(sourceRoot, relative));
  }
  script = path.join(sourceRoot, "scripts", "consume-aws-image-candidate.sh");
  await chmod(script, 0o755);
  await sourceGit(["init", "-q", "--object-format=sha1", "--initial-branch=main"]);
  await sourceGit(["add", "."]);
  await sourceGit(["commit", "-q", "-m", "test: source recipes"]);
  sourceCommit = (await sourceGit(["rev-parse", "HEAD"])).stdout.trim();
  committedImageRecipe = JSON.parse(
    await readFile(path.join(sourceRoot, "recipes", "aws", "v1", "linux-devtools.json"), "utf8"),
  );
  committedReadinessRecipe = JSON.parse(
    await readFile(path.join(sourceRoot, "recipes", "linux", "v1", "linux-builder.json"), "utf8"),
  );

  await writeFile(path.join(sourceRoot, "unrelated.txt"), "other source commit\n");
  await sourceGit(["add", "unrelated.txt"]);
  await sourceGit(["commit", "-q", "-m", "test: alternate commit"]);
  otherSourceCommit = (await sourceGit(["rev-parse", "HEAD"])).stdout.trim();

  const readinessPath = path.join(sourceRoot, "recipes", "linux", "v1", "linux-builder.json");
  const readinessBytes = await readFile(readinessPath);
  await rm(readinessPath);
  await symlink("linux-minimal.json", readinessPath);
  await sourceGit(["add", "recipes/linux/v1/linux-builder.json"]);
  await sourceGit(["commit", "-q", "-m", "test: symlink recipe"]);
  symlinkSourceCommit = (await sourceGit(["rev-parse", "HEAD"])).stdout.trim();
  await rm(readinessPath);
  await writeFile(readinessPath, readinessBytes);
  await sourceGit(["add", "recipes/linux/v1/linux-builder.json"]);
  await sourceGit(["commit", "-q", "-m", "test: restore recipe"]);

  const imageRecipePath = path.join(sourceRoot, "recipes", "aws", "v1", "linux-devtools.json");
  const imageRecipeBytes = await readFile(imageRecipePath);
  await writeFile(
    imageRecipePath,
    `${JSON.stringify({
      ...committedImageRecipe,
      components: committedImageRecipe.components.map((component) =>
        component.name === "node" ? { ...component, version: "999" } : component,
      ),
    })}\n`,
  );
  await sourceGit(["add", "recipes/aws/v1/linux-devtools.json"]);
  await sourceGit(["commit", "-q", "-m", "test: replacement recipe"]);
  replacementSourceCommit = (await sourceGit(["rev-parse", "HEAD"])).stdout.trim();
  await writeFile(imageRecipePath, imageRecipeBytes);
  await sourceGit(["add", "recipes/aws/v1/linux-devtools.json"]);
  await sourceGit(["commit", "-q", "-m", "test: restore image recipe"]);
});

after(async () => {
  const roots = [...schemaValidatorRoots];
  try {
    await Promise.all(roots.map(removeSchemaValidator));
    for (const root of roots) {
      await assert.rejects(access(root), { code: "ENOENT" });
    }
    assert.equal(schemaValidatorRoots.size, 0);
  } finally {
    await rm(sourceRoot, { recursive: true, force: true });
  }
});

async function removeSchemaValidator(root) {
  await rm(root, { recursive: true, force: true });
  schemaValidatorRoots.delete(root);
}

async function schemaValidator() {
  if (schemaValidatorPromise) return schemaValidatorPromise;
  const pending = (async () => {
    const root = await mkdtemp(path.join(os.tmpdir(), "crabbox-schema-validator-"));
    schemaValidatorRoots.add(root);
    schemaValidatorHistory.push(root);
    const binary = path.join(root, "validate-json-schema");
    schemaValidatorBuildCount += 1;
    try {
      await execFileAsync("go", ["build", "-trimpath", "-o", binary, schemaValidatorSource], {
        cwd: repoRoot,
        env: {
          ...process.env,
          GOCACHE: path.join(root, "go-build"),
        },
      });
      return { binary, root };
    } catch (error) {
      await removeSchemaValidator(root);
      throw error;
    }
  })();
  schemaValidatorPromise = pending;
  try {
    return await pending;
  } catch (error) {
    if (schemaValidatorPromise === pending) schemaValidatorPromise = undefined;
    throw error;
  }
}

async function validateQualificationInput(document) {
  const validator = await schemaValidator();
  try {
    return await execFileAsync(validator.binary, [schema, document], { cwd: repoRoot });
  } catch (error) {
    if (typeof error.code === "string") {
      schemaValidatorPromise = undefined;
      await removeSchemaValidator(validator.root);
    }
    throw error;
  }
}

function sha256(value) {
  return `sha256:${createHash("sha256").update(value).digest("hex")}`;
}

async function createBundle(root) {
  const checkpoint = path.join(root, "checkpoint.json");
  const scrub = path.join(root, "scrub.json");
  const checks = path.join(root, "checks.json");
  const bundle = path.join(root, "source-bundle");
  await writeFile(
    checkpoint,
    JSON.stringify({
      kind: "aws-ami",
      provider: "aws",
      targetOS: "linux",
      native: {
        provider: "aws",
        kind: "aws-ami",
        imageId: "ami-123abc",
        region: "us-west-2",
        architecture: "x86_64",
        snapshotIds: ["snap-def456", "snap-abc123"],
      },
    }),
  );
  const scrubEvidence = {
    schema: "crabbox-aws-image-scrub/v1",
    target: "linux",
    removed: {
      credentials: 2,
      hostIdentity: 1,
      workspaces: 1,
    },
    findings: [],
  };
  await writeFile(
    scrub,
    `${canonicalJSON({
      ...scrubEvidence,
      evidenceDigest: digestJSON(scrubEvidence),
    })}\n`,
  );
  await writeFile(
    checks,
    JSON.stringify({
      schema: "crabbox-aws-image-checks/v1",
      checks: [
        { name: "source-smoke", status: "passed" },
        {
          name: "scrub",
          status: "passed",
          evidenceDigest: digestJSON(scrubEvidence),
        },
        { name: "candidate-boot", status: "passed" },
        { name: "candidate-smoke", status: "passed" },
      ],
    }),
  );
  await execFileAsync(process.execPath, [
    candidateScript,
    "create",
    "--checkpoint",
    checkpoint,
    "--scrub-report",
    scrub,
    "--checks",
    checks,
    "--recipe",
    path.join(repoRoot, "recipes", "aws", "v1", "linux-devtools.json"),
    "--out-dir",
    bundle,
    "--target",
    "linux",
    "--region",
    "us-west-2",
    "--instance-type",
    "m7i.large",
    "--architecture",
    "x86_64",
    "--base-image",
    "ami-base123",
    "--source-repository",
    sourceRepository,
    "--source-commit",
    sourceCommit,
    "--workflow-ref",
    workflowRef,
    "--workflow-run-id",
    workflowRunId,
    "--workflow-run-attempt",
    workflowRunAttempt,
    "--oci-repository",
    repository,
    "--created-at",
    "2026-08-21T00:00:00.000Z",
  ]);
  return bundle;
}

async function rewriteCandidateSchema(bundle) {
  const candidatePath = path.join(bundle, "candidate.json");
  const bundlePath = path.join(bundle, "bundle.json");
  const candidate = JSON.parse(await readFile(candidatePath, "utf8"));
  candidate.schema = "crabbox-aws-image-candidate/v2";
  const candidateText = `${canonicalJSON(candidate)}\n`;
  await writeFile(candidatePath, candidateText);
  const manifest = JSON.parse(await readFile(bundlePath, "utf8"));
  const candidateDigest = digestBytes(candidateText);
  manifest.candidate.digest = candidateDigest;
  manifest.candidate.size = Buffer.byteLength(candidateText);
  manifest.oci.immutableTag = candidateDigest.replace("sha256:", "sha256-");
  const descriptor = manifest.files.find((item) => item.name === "candidate.json");
  descriptor.digest = candidateDigest;
  descriptor.size = Buffer.byteLength(candidateText);
  await writeFile(bundlePath, `${canonicalJSON(manifest)}\n`);
}

async function writeRegistryEvidence(root, bundle, options = {}) {
  const layers = [];
  for (const [name, mediaType] of mediaTypes) {
    const bytes = await readFile(path.join(bundle, name));
    layers.push({
      mediaType:
        options.layerMediaType && name === "candidate.json" ? options.layerMediaType : mediaType,
      digest: sha256(bytes),
      size: bytes.length,
      annotations: { "org.opencontainers.image.title": name },
    });
  }
  if (options.duplicateLayer) layers.push({ ...layers[0] });
  if (options.missingLayer) layers.pop();
  const raw = Buffer.from(
    canonicalJSON({
      schemaVersion: 2,
      mediaType: "application/vnd.oci.image.manifest.v1+json",
      artifactType: options.artifactType ?? "application/vnd.crabbox.aws-image-evidence.v1",
      config: {
        mediaType: "application/vnd.oci.empty.v1+json",
        digest: emptyConfigDigest,
        size: 2,
        data: "e30=",
      },
      layers,
    }),
  );
  const digest = sha256(raw);
  const candidateRef = `${repository}@${digest}`;
  const rawFile = path.join(root, "manifest.raw.json");
  const metadataFile = path.join(root, "manifest.json");
  const referrersFile = path.join(root, "referrers.json");
  const signatureBundleFile = path.join(root, "signature.bundle.json");
  const signatureBundle = Buffer.from(
    `${canonicalJSON({
      mediaType: "application/vnd.dev.sigstore.bundle.v0.3+json",
      verificationMaterial: {},
    })}\n`,
  );
  const signatureBundleDigest = sha256(signatureBundle);
  const signatureManifestRaw = Buffer.from(
    canonicalJSON({
      schemaVersion: 2,
      mediaType: "application/vnd.oci.image.manifest.v1+json",
      artifactType: "application/vnd.dev.sigstore.bundle.v0.3+json",
      config: {
        mediaType: "application/vnd.oci.empty.v1+json",
        artifactType: "application/vnd.dev.sigstore.bundle.v0.3+json",
        digest: emptyConfigDigest,
        size: 2,
      },
      layers: [
        {
          mediaType:
            options.signatureLayerMediaType ?? "application/vnd.dev.sigstore.bundle.v0.3+json",
          digest: signatureBundleDigest,
          size: signatureBundle.length,
        },
      ],
      subject: {
        mediaType: "application/vnd.oci.image.manifest.v1+json",
        digest: options.signatureSubjectDigest ?? digest,
        size: raw.length,
      },
      annotations: {
        "org.opencontainers.image.created": "2026-08-21T00:00:00Z",
        "dev.sigstore.bundle.content": "dsse-envelope",
        "dev.sigstore.bundle.predicateType":
          options.signaturePredicate ?? "https://sigstore.dev/cosign/sign/v1",
      },
    }),
  );
  const discoveredSignatureDigest = sha256(signatureManifestRaw);
  const signatureRef = `${repository}@${discoveredSignatureDigest}`;
  const signatureManifestRawFile = path.join(root, "signature-manifest.raw.json");
  const signatureManifestFile = path.join(root, "signature-manifest.json");
  await writeFile(rawFile, raw);
  await writeFile(
    metadataFile,
    JSON.stringify({
      reference: candidateRef,
      mediaType: "application/vnd.oci.image.manifest.v1+json",
      digest,
      size: raw.length,
      content: JSON.parse(raw),
    }),
  );
  const referrerType = options.referrerType ?? "application/vnd.dev.sigstore.bundle.v0.3+json";
  await writeFile(
    referrersFile,
    JSON.stringify({
      reference: candidateRef,
      mediaType: "application/vnd.oci.image.manifest.v1+json",
      digest,
      size: raw.length,
      referrers: options.missingReferrer
        ? []
        : [
            {
              reference: signatureRef,
              mediaType: options.referrerMediaType ?? "application/vnd.oci.image.manifest.v1+json",
              digest: discoveredSignatureDigest,
              size: signatureManifestRaw.length,
              artifactType: referrerType,
              referrers: [],
              ...(referrerType === "application/vnd.dev.sigstore.bundle.v0.3+json"
                ? {
                    annotations: {
                      "dev.sigstore.bundle.predicateType":
                        options.signaturePredicate ?? "https://sigstore.dev/cosign/sign/v1",
                    },
                  }
                : {}),
            },
            ...(options.duplicateReferrer
              ? [
                  {
                    reference: `${repository}@sha256:${"8".repeat(64)}`,
                    mediaType: "application/vnd.oci.image.manifest.v1+json",
                    digest: `sha256:${"8".repeat(64)}`,
                    size: 800,
                    artifactType: referrerType,
                  },
                ]
              : []),
          ],
    }),
  );
  await writeFile(signatureManifestRawFile, signatureManifestRaw);
  await writeFile(
    signatureManifestFile,
    JSON.stringify({
      reference: signatureRef,
      mediaType: "application/vnd.oci.image.manifest.v1+json",
      digest: discoveredSignatureDigest,
      size: signatureManifestRaw.length,
      content: JSON.parse(signatureManifestRaw),
    }),
  );
  await writeFile(
    signatureBundleFile,
    options.tamperSignatureBundle
      ? Buffer.concat([signatureBundle, Buffer.from(" ")])
      : signatureBundle,
  );
  return {
    candidateRef,
    digest,
    rawFile,
    metadataFile,
    referrersFile,
    signatureRef,
    signatureManifestRawFile,
    signatureManifestFile,
    signatureBundleFile,
    signatureBundleDigest,
  };
}

async function writeFakeTools(root) {
  const bin = path.join(root, "bin");
  const log = path.join(root, "commands.log");
  await mkdir(bin);
  await writeFile(
    path.join(bin, "oras"),
    `#!/usr/bin/env node
const fs = require("node:fs");
const path = require("node:path");
const args = process.argv.slice(2);
fs.appendFileSync(process.env.CRABBOX_FAKE_LOG, "oras\\t" + args.join("\\t") + "\\n");
const command = args[0];
if (command === "resolve") {
  process.stdout.write((process.env.CRABBOX_FAKE_RESOLVED || process.env.CRABBOX_FAKE_DIGEST) + "\\n");
} else if (command === "manifest" && args[1] === "fetch") {
  const output = args[args.indexOf("--output") + 1];
  const signature = args[2] === process.env.CRABBOX_FAKE_SIGNATURE_REFERENCE;
  fs.copyFileSync(
    signature ? process.env.CRABBOX_FAKE_SIGNATURE_MANIFEST_RAW : process.env.CRABBOX_FAKE_MANIFEST_RAW,
    output
  );
  process.stdout.write(fs.readFileSync(
    signature ? process.env.CRABBOX_FAKE_SIGNATURE_MANIFEST : process.env.CRABBOX_FAKE_MANIFEST,
    "utf8"
  ));
} else if (command === "discover") {
  process.stdout.write(fs.readFileSync(process.env.CRABBOX_FAKE_REFERRERS, "utf8"));
} else if (command === "blob" && args[1] === "fetch") {
  const output = args[args.indexOf("--output") + 1];
  if (args.at(-1) !== process.env.CRABBOX_FAKE_SIGNATURE_BUNDLE_REFERENCE) process.exit(89);
  fs.copyFileSync(process.env.CRABBOX_FAKE_SIGNATURE_BUNDLE, output);
} else if (command === "pull") {
  const output = args[args.indexOf("--output") + 1];
  fs.mkdirSync(output, { recursive: true });
  for (const name of fs.readdirSync(process.env.CRABBOX_FAKE_BUNDLE)) {
    fs.copyFileSync(path.join(process.env.CRABBOX_FAKE_BUNDLE, name), path.join(output, name));
  }
  const mode = process.env.CRABBOX_FAKE_PULL_MODE || "";
  if (mode === "missing") fs.rmSync(path.join(output, "sbom.spdx.json"));
  if (mode === "extra") fs.writeFileSync(path.join(output, "extra.json"), "{}\\n");
  if (mode === "tamper") fs.appendFileSync(path.join(output, "candidate.json"), " ");
} else {
  process.stderr.write("unexpected fake oras command\\n");
  process.exit(90);
}
`,
  );
  await writeFile(
    path.join(bin, "cosign"),
    `#!/usr/bin/env node
const fs = require("node:fs");
const args = process.argv.slice(2);
fs.appendFileSync(process.env.CRABBOX_FAKE_LOG, "cosign\\t" + args.join("\\t") + "\\n");
const identity = args[args.indexOf("--certificate-identity") + 1];
const issuer = args[args.indexOf("--certificate-oidc-issuer") + 1];
const digest = args.at(-1);
const bundle = args[args.indexOf("--bundle") + 1];
if (
  args[0] !== "verify-blob" ||
  !args.includes("--new-bundle-format") ||
  identity !== process.env.CRABBOX_FAKE_EXPECT_IDENTITY ||
  issuer !== process.env.CRABBOX_FAKE_EXPECT_ISSUER ||
  digest !== process.env.CRABBOX_FAKE_DIGEST ||
  !fs.readFileSync(bundle).equals(fs.readFileSync(process.env.CRABBOX_FAKE_SIGNATURE_BUNDLE))
) process.exit(91);
`,
  );
  await chmod(path.join(bin, "oras"), 0o755);
  await chmod(path.join(bin, "cosign"), 0o755);
  return { bin, log };
}

async function fixture(options = {}) {
  const root = await mkdtemp(path.join(os.tmpdir(), "crabbox-aws-consume-"));
  const bundle = await createBundle(root);
  if (options.schemaMismatch) await rewriteCandidateSchema(bundle);
  const registry = await writeRegistryEvidence(root, bundle, options);
  const tools = await writeFakeTools(root);
  const output = path.join(root, "qualification-input.json");
  const args = [
    "--candidate-ref",
    registry.candidateRef,
    "--repository",
    repository,
    "--source-repository",
    sourceRepository,
    "--source-commit",
    sourceCommit,
    "--workflow-ref",
    workflowRef,
    "--workflow-run-id",
    workflowRunId,
    "--workflow-run-attempt",
    workflowRunAttempt,
    "--target",
    "linux",
    "--region",
    "us-west-2",
    "--instance-type",
    "m7i.large",
    "--architecture",
    "x86_64",
    "--base-image",
    "ami-base123",
    "--ami-id",
    "ami-123abc",
    "--profile",
    "linux-builder",
    "--image-recipe",
    "recipes/aws/v1/linux-devtools.json",
    "--readiness-recipe",
    "recipes/linux/v1/linux-builder.json",
    "--output",
    output,
  ];
  const env = {
    ...process.env,
    PATH: `${tools.bin}${path.delimiter}${process.env.PATH ?? ""}`,
    CRABBOX_FAKE_LOG: tools.log,
    CRABBOX_FAKE_DIGEST: registry.digest,
    CRABBOX_FAKE_MANIFEST_RAW: registry.rawFile,
    CRABBOX_FAKE_MANIFEST: registry.metadataFile,
    CRABBOX_FAKE_REFERRERS: registry.referrersFile,
    CRABBOX_FAKE_SIGNATURE_REFERENCE: registry.signatureRef,
    CRABBOX_FAKE_SIGNATURE_MANIFEST_RAW: registry.signatureManifestRawFile,
    CRABBOX_FAKE_SIGNATURE_MANIFEST: registry.signatureManifestFile,
    CRABBOX_FAKE_SIGNATURE_BUNDLE: registry.signatureBundleFile,
    CRABBOX_FAKE_SIGNATURE_BUNDLE_REFERENCE: `${repository}@${registry.signatureBundleDigest}`,
    CRABBOX_FAKE_BUNDLE: bundle,
    CRABBOX_FAKE_EXPECT_IDENTITY: `https://github.com/${workflowRef}`,
    CRABBOX_FAKE_EXPECT_ISSUER: "https://token.actions.githubusercontent.com",
  };
  return { root, bundle, registry, tools, output, args, env };
}

async function run(value, options = {}) {
  try {
    const result = await execFileAsync("bash", [script, ...(options.args ?? value.args)], {
      cwd: repoRoot,
      env: { ...value.env, ...options.env },
    });
    return { code: 0, ...result };
  } catch (error) {
    return {
      code: error.code,
      stdout: error.stdout ?? "",
      stderr: error.stderr ?? "",
    };
  }
}

function replaceArg(args, name, value) {
  const result = [...args];
  result[result.indexOf(name) + 1] = value;
  return result;
}

test("consumer verifies immutable evidence and emits deterministic qualification input", async () => {
  const value = await fixture();
  const result = await run(value);
  assert.equal(result.code, 0, result.stderr);
  const output = JSON.parse(await readFile(value.output, "utf8"));
  assert.equal(output.schema, "crabbox-aws-image-qualification-input/v1");
  assert.equal(output.artifact.reference, value.registry.candidateRef);
  assert.equal(output.source.commit, sourceCommit);
  assert.equal(output.target.amiId, "ami-123abc");
  assert.equal(output.readiness.profile, "linux-builder");
  assert.equal(output.imageRecipe.digest, digestJSONLine(committedImageRecipe));
  assert.equal(output.readiness.recipeDigest, digestJSON(committedReadinessRecipe));
  assert.equal(output.evidence.length, 6);
  assert.doesNotMatch(
    JSON.stringify(output),
    /\/Users\/|\/home\/|https?:\/\/|192\.168\.|127\.0\.0\.1/,
  );

  const commands = (await readFile(value.tools.log, "utf8"))
    .trim()
    .split("\n")
    .map((line) => line.split("\t"));
  assert.deepEqual(
    commands.map((command) => command.slice(0, 3)),
    [
      ["oras", "resolve", value.registry.candidateRef],
      ["oras", "manifest", "fetch"],
      ["oras", "discover", value.registry.candidateRef],
      ["oras", "manifest", "fetch"],
      ["oras", "blob", "fetch"],
      ["cosign", "verify-blob", "--new-bundle-format"],
      ["oras", "pull", value.registry.candidateRef],
    ],
  );
  assert.deepEqual(commands[2].slice(-4), ["--depth", "1", "--format", "json"]);
  assert.equal(commands[3][3], value.registry.signatureRef);
  assert.equal(commands[4].at(-1), `${repository}@${value.registry.signatureBundleDigest}`);
  assert.equal(commands[5].at(-1), value.registry.digest);
  assert.ok(commands[5].includes("--bundle"));
  assert.equal(
    commands[5][commands[5].indexOf("--certificate-identity") + 1],
    `https://github.com/${workflowRef}`,
  );
  assert.equal(
    commands[5][commands[5].indexOf("--certificate-oidc-issuer") + 1],
    "https://token.actions.githubusercontent.com",
  );

  const secondOutput = path.join(value.root, "qualification-input-second.json");
  const second = await run(value, {
    args: replaceArg(value.args, "--output", secondOutput),
  });
  assert.equal(second.code, 0, second.stderr);
  assert.equal(await readFile(secondOutput, "utf8"), await readFile(value.output, "utf8"));
  await validateQualificationInput(value.output);
});

test("consumer accepts Cosign bundle JSON and ORAS nested depth metadata", async () => {
  const value = await fixture();
  const result = await run(value);
  assert.equal(result.code, 0, result.stderr);
  const output = JSON.parse(await readFile(value.output, "utf8"));
  assert.equal(output.artifact.signatureType, "application/vnd.dev.sigstore.bundle.v0.3+json");
});

test("consumer rejects tags, mutable references, wrong registries, and wrong repositories", async () => {
  const value = await fixture();
  for (const reference of [
    `${repository}:latest`,
    `${repository}:sha256-${"1".repeat(64)}`,
    `docker.io/example-org/candidate@sha256:${"1".repeat(64)}`,
    `ghcr.io/other-org/candidate@sha256:${"1".repeat(64)}`,
  ]) {
    const result = await run(value, {
      args: replaceArg(value.args, "--candidate-ref", reference),
    });
    assert.notEqual(result.code, 0);
  }
});

test("consumer rejects ORAS digest disagreement before other registry reads", async () => {
  const value = await fixture();
  const result = await run(value, {
    env: { CRABBOX_FAKE_RESOLVED: `sha256:${"f".repeat(64)}` },
  });
  assert.notEqual(result.code, 0);
  assert.match(result.stderr, /resolved digest does not match/);
  const commands = (await readFile(value.tools.log, "utf8")).trim().split("\n");
  assert.equal(commands.length, 1);
});

test("consumer enforces exact keyless certificate identity and issuer", async () => {
  for (const env of [
    {
      CRABBOX_FAKE_EXPECT_IDENTITY:
        "https://github.com/example-org/crabbox/.github/workflows/other.yml@refs/heads/main",
    },
    { CRABBOX_FAKE_EXPECT_ISSUER: "https://issuer.example.invalid" },
  ]) {
    const value = await fixture();
    const result = await run(value, { env });
    assert.notEqual(result.code, 0);
    await assert.rejects(readFile(value.output));
  }
});

test("consumer verifies the discovered signature bundle by immutable layer digest", async () => {
  const value = await fixture({ tamperSignatureBundle: true });
  const result = await run(value);
  assert.notEqual(result.code, 0);
  assert.match(result.stderr, /signature bundle does not match/);
  await assert.rejects(readFile(value.output));
  const commands = (await readFile(value.tools.log, "utf8")).trim().split("\n");
  assert.equal(
    commands.some((command) => command.startsWith("cosign\t")),
    false,
  );
});

test("consumer rejects wrong artifact, layer, and referrer descriptor types", async () => {
  for (const options of [
    { artifactType: "application/vnd.example.wrong" },
    { layerMediaType: "application/octet-stream" },
    { referrerMediaType: "application/vnd.oci.image.index.v1+json" },
    { signatureLayerMediaType: "application/octet-stream" },
    { signatureSubjectDigest: `sha256:${"7".repeat(64)}` },
    { referrerType: "application/vnd.dev.cosign.simplesigning.v1+json" },
    { referrerType: "application/vnd.example.signature" },
    {
      referrerType: "application/vnd.dev.sigstore.bundle.v0.3+json",
      signaturePredicate: "https://example.invalid/not-a-signature",
    },
  ]) {
    const value = await fixture(options);
    const result = await run(value);
    assert.notEqual(result.code, 0);
    await assert.rejects(readFile(value.output));
  }
});

test("consumer rejects missing and duplicate descriptors or signature referrers", async () => {
  for (const options of [
    { missingLayer: true },
    { duplicateLayer: true },
    { missingReferrer: true },
    { duplicateReferrer: true },
  ]) {
    const value = await fixture(options);
    const result = await run(value);
    assert.notEqual(result.code, 0);
    await assert.rejects(readFile(value.output));
  }
});

test("consumer rejects missing, extra, and tampered pulled evidence without partial output", async () => {
  for (const mode of ["missing", "extra", "tamper"]) {
    const value = await fixture();
    const result = await run(value, {
      env: { CRABBOX_FAKE_PULL_MODE: mode },
    });
    assert.notEqual(result.code, 0);
    await assert.rejects(readFile(value.output));
    const leftovers = (await readdir(value.root)).filter((name) =>
      name.includes("qualification-input.json."),
    );
    assert.deepEqual(leftovers, []);
  }
});

test("consumer rejects CandidateRecordV1 schema mismatch", async () => {
  const value = await fixture({ schemaMismatch: true });
  const result = await run(value);
  assert.notEqual(result.code, 0);
  assert.match(result.stderr, /invalid CandidateRecordV1 header/);
  await assert.rejects(readFile(value.output));
});

test("consumer rejects source and workflow mismatches", async () => {
  for (const [name, replacement] of [
    ["--source-commit", otherSourceCommit],
    ["--workflow-run-id", "54321"],
  ]) {
    const value = await fixture();
    const result = await run(value, {
      args: replaceArg(value.args, name, replacement),
    });
    assert.notEqual(result.code, 0);
    assert.match(result.stderr, /source or workflow identity mismatch/);
  }
});

test("consumer rejects a nonexistent source commit before registry access", async () => {
  const value = await fixture();
  const result = await run(value, {
    args: replaceArg(value.args, "--source-commit", "f".repeat(40)),
  });
  assert.notEqual(result.code, 0);
  assert.match(result.stderr, /source commit is unavailable/);
  await assert.rejects(readFile(value.tools.log));
});

test("consumer derives recipes from the commit instead of dirty working-tree files", async () => {
  const recipePath = path.join(sourceRoot, "recipes", "aws", "v1", "linux-devtools.json");
  const original = await readFile(recipePath);
  await writeFile(recipePath, "{dirty working tree");
  try {
    const value = await fixture();
    const result = await run(value);
    assert.equal(result.code, 0, result.stderr);
    const output = JSON.parse(await readFile(value.output, "utf8"));
    assert.equal(output.imageRecipe.digest, digestJSONLine(committedImageRecipe));
  } finally {
    await writeFile(recipePath, original);
  }
});

test("consumer ignores Git replacement objects for source-commit recipe trust", async () => {
  await sourceGit(["replace", sourceCommit, replacementSourceCommit]);
  try {
    const value = await fixture();
    const result = await run(value);
    assert.equal(result.code, 0, result.stderr);
    const output = JSON.parse(await readFile(value.output, "utf8"));
    assert.equal(output.imageRecipe.digest, digestJSONLine(committedImageRecipe));
  } finally {
    await sourceGit(["replace", "-d", sourceCommit]);
  }
});

test("consumer rejects a symlink recipe at the exact source commit", async () => {
  const value = await fixture();
  const result = await run(value, {
    args: replaceArg(value.args, "--source-commit", symlinkSourceCommit),
  });
  assert.notEqual(result.code, 0);
  assert.match(result.stderr, /regular source-commit blob/);
  await assert.rejects(readFile(value.tools.log));
});

test("consumer rejects target, AMI, profile, recipe, and readiness mismatches", async () => {
  const cases = [
    ["--region", "us-east-1", /target identity mismatch/],
    ["--architecture", "arm64", /target identity mismatch/],
    ["--ami-id", "ami-other123", /AMI identity mismatch/],
    ["--profile", "linux-minimal", /readiness identity mismatch/],
    ["--image-recipe", "recipes/aws/v1/windows-devtools.json", /image recipe identity mismatch/],
    ["--readiness-recipe", "recipes/linux/v1/linux-minimal.json", /readiness identity mismatch/],
  ];
  for (const [name, replacement, message] of cases) {
    const value = await fixture();
    const result = await run(value, {
      args: replaceArg(value.args, name, replacement),
    });
    assert.notEqual(result.code, 0);
    assert.match(result.stderr, message);
    await assert.rejects(readFile(value.output));
  }
});

test("consumer rejects unsafe recipe paths before registry access", async () => {
  for (const [name, replacement] of [
    ["--image-recipe", "recipes/aws/v1/../linux-devtools.json"],
    ["--readiness-recipe", "recipes/linux/v1/./linux-builder.json"],
    ["--image-recipe", "/tmp/linux-devtools.json"],
    ["--readiness-recipe", "recipes\\linux\\v1\\linux-builder.json"],
  ]) {
    const value = await fixture();
    const result = await run(value, {
      args: replaceArg(value.args, name, replacement),
    });
    assert.notEqual(result.code, 0);
    assert.match(result.stderr, /invalid (?:image|readiness) recipe/);
    await assert.rejects(readFile(value.tools.log));
  }
});

test("qualification input schema is strict and names every evidence file", async () => {
  const schemaDocument = JSON.parse(await readFile(schema, "utf8"));
  assert.equal(schemaDocument.additionalProperties, false);
  assert.equal(
    schemaDocument.properties.schema.const,
    "crabbox-aws-image-qualification-input/v1",
  );
  assert.equal(
    schemaDocument.properties.artifact.properties.signatureType.const,
    "application/vnd.dev.sigstore.bundle.v0.3+json",
  );
  assert.equal(schemaDocument.properties.evidence.minItems, 6);
  assert.equal(schemaDocument.properties.evidence.maxItems, 6);
  assert.equal(schemaDocument.properties.evidence.allOf.length, 6);
  assert.deepEqual(
    schemaDocument.properties.evidence.allOf
      .map((rule) => rule.contains.properties.name.const)
      .sort(),
    [...mediaTypes.keys()].sort(),
  );
  assert.deepEqual(
    [...schemaDocument.properties.evidence.items.properties.name.enum].sort(),
    [...mediaTypes.keys()].sort(),
  );
});

test("real JSON Schema validation rejects unsupported signatures and unsafe recipe paths", async () => {
  const value = await fixture();
  const result = await run(value);
  assert.equal(result.code, 0, result.stderr);
  await validateQualificationInput(value.output);

  for (const mutate of [
    (record) => {
      record.artifact.signatureType = "application/vnd.dev.cosign.simplesigning.v1+json";
    },
    (record) => {
      record.imageRecipe.path = "recipes/aws/v1/../linux-devtools.json";
    },
    (record) => {
      record.readiness.recipe = "recipes/linux/v1/./linux-builder.json";
    },
  ]) {
    const record = JSON.parse(await readFile(value.output, "utf8"));
    mutate(record);
    const invalid = path.join(value.root, `invalid-${Math.random()}.json`);
    await writeFile(invalid, `${JSON.stringify(record)}\n`);
    await assert.rejects(validateQualificationInput(invalid), /validate document/);
  }
  assert.equal(schemaValidatorBuildCount, 1);
  assert.equal(schemaValidatorHistory.length, 1);
  assert.deepEqual([...schemaValidatorRoots], [schemaValidatorHistory[0]]);
  await access(schemaValidatorHistory[0]);
});

test("consumer atomically refuses concurrent output replacement", async () => {
  const value = await fixture();
  const results = await Promise.all([run(value), run(value)]);
  assert.equal(results.filter((result) => result.code === 0).length, 1);
  assert.equal(results.filter((result) => result.code !== 0).length, 1);
  assert.match(
    results.find((result) => result.code !== 0).stderr,
    /qualification output already exists/,
  );
  assert.equal(
    JSON.parse(await readFile(value.output, "utf8")).artifact.reference,
    value.registry.candidateRef,
  );
});
