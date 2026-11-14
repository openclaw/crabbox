import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import {
  packageFile,
  qualificationPackagePath as pkg,
  readQualificationPackage,
  prepareQualificationBuild,
  assertProductionBaseline,
  qualificationIdentity,
} from "./image-qualification-package.mjs";
import {
  createManifest,
  verifyManifest,
  stageFirstWalkRecipe,
  canonical,
} from "./image-qualification-control.mjs";

const root = path.resolve(import.meta.dirname, "..");
const hash = (bytes) => crypto.createHash("sha256").update(bytes).digest("hex");
const git = (dir, ...args) =>
  execFileSync("git", args, {
    cwd: dir,
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
  }).trim();
const write = (dir, file, bytes) => {
  fs.mkdirSync(path.dirname(path.join(dir, file)), { recursive: true });
  fs.writeFileSync(path.join(dir, file), bytes);
};

function fixture() {
  const temp = fs.mkdtempSync(path.join(os.tmpdir(), "qualification-package-"));
  const baseline = path.join(temp, "baseline");
  fs.mkdirSync(baseline);
  git(baseline, "init", "-q");
  git(baseline, "config", "user.name", "Qualification Fixture");
  git(baseline, "config", "user.email", "qualification@example.invalid");
  write(baseline, "worker/src/index.ts", "export default {};\n");
  write(baseline, "worker/wrangler.aws-qualification-authority.jsonc", "{}\n");
  write(baseline, "recipes/bootstrap/v1/linuxOwnershipRepair.sh", "original\n");
  git(baseline, "add", ".");
  git(baseline, "commit", "-qm", "baseline");
  const baselineCommit = git(baseline, "rev-parse", "HEAD");
  const baselineTree = git(baseline, "rev-parse", "HEAD^{tree}");
  const author = path.join(temp, "author");
  git(temp, "clone", "-q", baseline, author);
  const recipe = fs.readFileSync(path.join(root, pkg, "linuxOwnershipRepair.sh"));
  write(author, "recipes/bootstrap/v1/linuxOwnershipRepair.sh", recipe);
  write(author, "worker/src/diagnostic.ts", 'throw new Error("source must remain inert");\n');
  git(author, "add", ".");
  const patch = execFileSync("git", ["diff", "--cached", "--binary", "--full-index", "HEAD"], {
    cwd: author,
  });
  const overlayPaths = ["recipes/bootstrap/v1/linuxOwnershipRepair.sh", "worker/src/diagnostic.ts"];
  const files = overlayPaths.map((file) => ({
    path: file,
    beforeSha256: fs.existsSync(path.join(baseline, file))
      ? hash(fs.readFileSync(path.join(baseline, file)))
      : null,
    afterSha256: hash(fs.readFileSync(path.join(author, file))),
  }));
  const records = ["diagnostic.ts", "index.ts"].map((file) => {
    const bytes = fs.readFileSync(path.join(author, "worker/src", file));
    return { path: file, bytes: bytes.length, sha256: hash(bytes) };
  });
  const config = Buffer.from('{"name":"qualification-fixture","workers_dev":false}\n');
  const contract = {
    version: 1,
    baselineCommit,
    baselineTree,
    overlaySha256: hash(patch),
    recipeSha256: hash(recipe),
    privateConfigSha256: hash(config),
    authorityConfigSha256: hash("{}\n"),
    workerSourceSha256: hash(canonical(records)),
    files,
  };
  const protectedRoot = path.join(temp, "protected");
  const candidate = path.join(temp, "candidate");
  for (const dir of [protectedRoot, candidate]) {
    git(temp, "clone", "-q", baseline, dir);
    write(dir, `${pkg}/manifest.json`, JSON.stringify(contract));
    write(dir, `${pkg}/runtime.patch`, patch);
    write(dir, `${pkg}/linuxOwnershipRepair.sh`, recipe);
    write(dir, `${pkg}/wrangler.private.json`, config);
  }
  return { temp, baseline, candidate, protectedRoot, contract };
}

test("private build applies only protected pinned data in a separate exact baseline", () => {
  const f = fixture();
  try {
    write(
      f.candidate,
      `${pkg}/never-execute.mjs`,
      'throw new Error("candidate control executed");',
    );
    const before = fs.readFileSync(path.join(f.protectedRoot, "worker/src/index.ts"));
    const result = prepareQualificationBuild(f.protectedRoot, f.candidate, f.baseline);
    assert.deepEqual(result.qualification, readQualificationPackage(f.protectedRoot).identity);
    assert.match(result.productionInputsSha256, /^[a-f0-9]{64}$/);
    assert.deepEqual(fs.readFileSync(path.join(f.protectedRoot, "worker/src/index.ts")), before);
    assert.equal(fs.existsSync(path.join(f.candidate, "worker/src/diagnostic.ts")), false);
    assert.match(
      fs.readFileSync(path.join(f.baseline, "worker/src/diagnostic.ts"), "utf8"),
      /remain inert/,
    );
    assert.equal(fs.existsSync(path.join(f.baseline, pkg, "never-execute.mjs")), false);
    assert.equal(
      JSON.parse(fs.readFileSync(path.join(f.baseline, "worker/wrangler.qualification.json")))
        .workers_dev,
      false,
    );
  } finally {
    fs.rmSync(f.temp, { recursive: true, force: true });
  }
});

test("private preparation rejects production edits, missing roots, hooks and package substitutions before overlay", () => {
  const f = fixture();
  try {
    const run = () => prepareQualificationBuild(f.protectedRoot, f.candidate, f.baseline);
    const source = "worker/src/index.ts";
    write(f.candidate, source, "tamper\n");
    assert.throws(run, /production working input changed/);
    write(f.candidate, source, "export default {};\n");
    write(f.candidate, "worker/src/untracked.ts", "unexpected");
    assert.throws(run, /untracked production input/);
    fs.unlinkSync(path.join(f.candidate, "worker/src/untracked.ts"));
    fs.unlinkSync(path.join(f.candidate, source));
    fs.symlinkSync(path.join(f.baseline, source), path.join(f.candidate, source));
    assert.throws(run, /link/);
    fs.unlinkSync(path.join(f.candidate, source));
    write(f.candidate, source, "export default {};\n");
    const overlay = packageFile(f.candidate, `${pkg}/runtime.patch`);
    write(f.candidate, `${pkg}/runtime.patch`, "changed");
    assert.throws(run, /changed protected qualification package/);
    write(f.candidate, `${pkg}/runtime.patch`, overlay);
    write(f.protectedRoot, `${pkg}/runtime.patch`, "changed");
    assert.throws(run, /digest mismatch/);
    write(f.protectedRoot, `${pkg}/runtime.patch`, overlay);
    assert.throws(
      () => prepareQualificationBuild(f.protectedRoot, f.candidate, f.protectedRoot),
      /separate checkouts/,
    );
    assert.throws(
      () =>
        assertProductionBaseline(f.candidate, f.baseline, {
          ...f.contract,
          baselineCommit: "0".repeat(40),
        }),
      /baseline identity/,
    );
    assert.equal(fs.existsSync(path.join(f.baseline, "worker/src/diagnostic.ts")), false);
    assert.equal(
      fs.readFileSync(
        path.join(f.baseline, "recipes/bootstrap/v1/linuxOwnershipRepair.sh"),
        "utf8",
      ),
      "original\n",
    );
  } finally {
    fs.rmSync(f.temp, { recursive: true, force: true });
  }
});

test("overlay result and complete source digest are verified rather than trusting an applied patch", () => {
  for (const changed of ["afterSha256", "workerSourceSha256", "authorityConfigSha256"]) {
    const f = fixture();
    try {
      if (changed === "afterSha256") f.contract.files[0].afterSha256 = "0".repeat(64);
      else f.contract[changed] = "0".repeat(64);
      for (const dir of [f.protectedRoot, f.candidate])
        write(dir, `${pkg}/manifest.json`, JSON.stringify(f.contract));
      assert.throws(
        () => prepareQualificationBuild(f.protectedRoot, f.candidate, f.baseline),
        /overlay result mismatch|Worker source mismatch|authority configuration mismatch/,
      );
    } finally {
      fs.rmSync(f.temp, { recursive: true, force: true });
    }
  }
});

test("private v2 manifests bind baseline, overlay, recipe, configuration and source; legacy cannot substitute", () => {
  const f = fixture();
  const priorMode = process.env.QUALIFICATION_MODE;
  try {
    process.env.QUALIFICATION_MODE = "first-walk";
    // Use the real protected package for the manifest identity. Build-output
    // bytes here are inert fixtures; the separate preparation/build proof owns provenance.
    const identity = qualificationIdentity(root);
    fs.cpSync(path.join(root, pkg), path.join(f.candidate, pkg), { recursive: true });
    const artifact = path.join(f.temp, "artifact");
    write(artifact, "worker/index.js", "export default {};\n");
    write(artifact, "authority/aws-qualification-authority.js", "export default {};\n");
    write(artifact, "bin/crabbox", "inert fixture\n");
    const candidateSha = git(f.candidate, "rev-parse", "HEAD");
    const workflowSha = git(f.protectedRoot, "rev-parse", "HEAD");
    const receipt = {
      version: 2,
      candidateSha,
      workflowSha,
      qualification: identity,
      workerSourceSha256: identity.workerSourceSha256,
      protectedBuildInputsSha256: "a".repeat(64),
    };
    write(artifact, "build-inputs.json", JSON.stringify(receipt));
    stageFirstWalkRecipe(f.candidate, artifact);
    const authorityFile = path.join(artifact, "authority/aws-qualification-authority.js");
    const authorityBytes = fs.readFileSync(authorityFile);
    fs.unlinkSync(authorityFile);
    assert.throws(
      () => createManifest(f.candidate, artifact, candidateSha, workflowSha),
      /private authority bundle is absent/,
    );
    fs.writeFileSync(authorityFile, authorityBytes);
    const sealed = createManifest(f.candidate, artifact, candidateSha, workflowSha);
    assert.equal(sealed.version, 2);
    assert.deepEqual(sealed.qualification, identity);
    assert.deepEqual(verifyManifest(artifact, candidateSha, workflowSha), sealed);
    for (const key of [
      "baselineCommit",
      "baselineTree",
      "packageSha256",
      "overlaySha256",
      "recipeSha256",
      "privateConfigSha256",
      "authorityConfigSha256",
      "workerSourceSha256",
    ]) {
      const altered = structuredClone(sealed);
      altered.qualification[key] = "0".repeat(identity[key].length);
      const unsigned = { ...altered };
      delete unsigned.manifestSha256;
      altered.manifestSha256 = hash(canonical(unsigned));
      write(artifact, "manifest.json", JSON.stringify(altered));
      assert.throws(() => verifyManifest(artifact, candidateSha, workflowSha), /identity mismatch/);
    }
    write(artifact, "manifest.json", JSON.stringify(sealed));
    write(artifact, "build-inputs.json", JSON.stringify({ ...receipt, version: 1 }));
    assert.throws(() => verifyManifest(artifact, candidateSha, workflowSha), /identity mismatch/);
    write(artifact, "build-inputs.json", JSON.stringify(receipt));
    process.env.QUALIFICATION_MODE = "mint";
    assert.throws(() => verifyManifest(artifact, candidateSha, workflowSha), /identity mismatch/);
  } finally {
    if (priorMode === undefined) delete process.env.QUALIFICATION_MODE;
    else process.env.QUALIFICATION_MODE = priorMode;
    fs.rmSync(f.temp, { recursive: true, force: true });
  }
});

test("private configuration and production entrypoints cannot select the diagnostic package", () => {
  const config = JSON.parse(packageFile(root, `${pkg}/wrangler.private.json`));
  assert.equal(config.workers_dev, false);
  assert.equal(config.preview_urls, false);
  for (const field of [
    "routes",
    "vars",
    "triggers",
    "durable_objects",
    "assets",
    "services",
    "secrets_store_secrets",
  ])
    assert.equal(Object.hasOwn(config, field), false);
  for (const dir of ["worker/src", "cmd", "internal"]) {
    const walk = (p) => {
      for (const entry of fs.readdirSync(p, { withFileTypes: true })) {
        const file = path.join(p, entry.name);
        if (entry.isDirectory()) walk(file);
        else if (/\.(go|ts|js|mjs|cjs)$/.test(entry.name))
          assert.doesNotMatch(fs.readFileSync(file, "utf8"), /qualification\/first-walk\/v1/);
      }
    };
    walk(path.join(root, dir));
  }
  const workflow = fs.readFileSync(
    path.join(root, ".github/workflows/image-qualification.yml"),
    "utf8",
  );
  const contract = readQualificationPackage(root).contract;
  assert.ok(workflow.includes(`ref: ${contract.baselineCommit}`));
  assert.match(workflow, /--config wrangler\.qualification\.json/);
  assert.match(workflow, /build-inputs\.json" private-baseline/);
});

// This guards the existing publication contract; changing path filters is not
// the isolation mechanism. The source/build-input guard above is independent.
test("qualification paths do not trigger automatic coordinator or Pages publication", () => {
  const deploy = fs.readFileSync(
    path.join(root, ".github/workflows/coordinator-deploy.yml"),
    "utf8",
  );
  const pages = fs.readFileSync(path.join(root, ".github/workflows/pages.yml"), "utf8");
  const paths = (source) =>
    source
      .split("    paths:\n")[1]
      .split("  workflow_dispatch:")[0]
      .split("\n")
      .filter((line) => /^      - "/.test(line))
      .map((line) => JSON.parse(line.trim().slice(2)));
  assert.deepEqual(paths(deploy), [
    "worker/**",
    "!worker/test/**",
    ".github/workflows/coordinator-deploy.yml",
    "scripts/prepare-coordinator-deploy-secrets.mjs",
    "scripts/verify-coordinator-deployment.mjs",
    "scripts/verify-coordinator-lease-policy.mjs",
    "scripts/deploy-coordinator.mjs",
  ]);
  assert.deepEqual(paths(pages), [
    "docs/**",
    "skills/**",
    "scripts/build-docs-site.mjs",
    "scripts/enhance-docs-site.mjs",
    "scripts/check-docs-i18n.mjs",
    "scripts/lib/**",
    "scripts/normalize-provider-counts.mjs",
    ".github/workflows/pages.yml",
  ]);
  for (const source of [deploy, pages]) assert.doesNotMatch(source, /qualification\//);
  const qualification = fs.readFileSync(
    path.join(root, ".github/workflows/image-qualification.yml"),
    "utf8",
  );
  assert.match(qualification, /on:\n  workflow_dispatch:/);
  assert.doesNotMatch(
    qualification.split("permissions:")[0],
    /(?:^|\n)  (?:push|pull_request|workflow_run|schedule):/,
  );
});
