// Protected build code. Package contents are data, never imported or executed.
import { execFileSync } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";

export const qualificationPackagePath = "qualification/first-walk/v1";
const digest = (bytes) => crypto.createHash("sha256").update(bytes).digest("hex");
const canonical = (value) =>
  Array.isArray(value)
    ? `[${value.map(canonical).join(",")}]`
    : value && typeof value === "object"
      ? `{${Object.keys(value)
          .sort()
          .map((key) => `${JSON.stringify(key)}:${canonical(value[key])}`)
          .join(",")}}`
      : JSON.stringify(value);
const git = (directory, ...args) =>
  execFileSync("git", args, { cwd: directory, encoding: "utf8", maxBuffer: 64 * 1024 * 1024 });

export function packageFile(directory, relative, limit = 2 * 1024 * 1024) {
  if (
    !/^[A-Za-z0-9_./-]+$/.test(relative) ||
    relative.split("/").some((p) => !p || p === "." || p === "..")
  )
    throw new Error("unsafe qualification package path");
  let current = directory;
  for (const part of relative.split("/")) {
    current = path.join(current, part);
    const stat = fs.lstatSync(current);
    if (
      stat.isSymbolicLink() ||
      (current !== path.join(directory, relative) && !stat.isDirectory())
    )
      throw new Error("qualification package contains a link or non-directory");
  }
  const stat = fs.lstatSync(current);
  if (!stat.isFile() || stat.size > limit) throw new Error("qualification package file is invalid");
  return fs.readFileSync(current);
}

export function readQualificationPackage(directory) {
  const bytes = packageFile(directory, `${qualificationPackagePath}/manifest.json`);
  const contract = JSON.parse(bytes);
  if (
    contract.version !== 1 ||
    !/^[a-f0-9]{40}$/.test(contract.baselineCommit) ||
    !/^[a-f0-9]{40}$/.test(contract.baselineTree) ||
    !Array.isArray(contract.files) ||
    !contract.files.length ||
    contract.files.length > 64
  )
    throw new Error("invalid qualification package contract");
  for (const [file, field] of [
    ["runtime.patch", "overlaySha256"],
    ["linuxOwnershipRepair.sh", "recipeSha256"],
    ["wrangler.private.json", "privateConfigSha256"],
  ]) {
    if (digest(packageFile(directory, `${qualificationPackagePath}/${file}`)) !== contract[field])
      throw new Error(`qualification package digest mismatch: ${file}`);
  }
  if (!/^[a-f0-9]{64}$/.test(contract.authorityConfigSha256))
    throw new Error("invalid private authority configuration digest");
  if (!/^[a-f0-9]{64}$/.test(contract.workerSourceSha256))
    throw new Error("invalid private source digest");
  const seen = new Set();
  for (const file of contract.files) {
    if (
      !/^(?:worker\/(?:src|test)\/[^\s]+|recipes\/bootstrap\/v1\/linuxOwnershipRepair\.sh|internal\/cli\/bootstrap[^/]*\.go)$/.test(
        file.path,
      ) ||
      file.path.split("/").some((p) => !p || p === "." || p === "..") ||
      seen.has(file.path) ||
      (file.beforeSha256 !== null && !/^[a-f0-9]{64}$/.test(file.beforeSha256)) ||
      !/^[a-f0-9]{64}$/.test(file.afterSha256)
    )
      throw new Error("invalid private overlay file contract");
    seen.add(file.path);
  }
  return {
    contract,
    identity: {
      version: 1,
      baselineCommit: contract.baselineCommit,
      baselineTree: contract.baselineTree,
      packageSha256: digest(bytes),
      overlaySha256: contract.overlaySha256,
      recipeSha256: contract.recipeSha256,
      privateConfigSha256: contract.privateConfigSha256,
      authorityConfigSha256: contract.authorityConfigSha256,
      workerSourceSha256: contract.workerSourceSha256,
    },
  };
}

// These are qualification controls/data only. Every other tracked input,
// including production config, recipes, generated files and docs, must match.
function toolingPath(file) {
  return (
    file.startsWith("qualification/") ||
    /^scripts\/image-qualification[^/]*$/.test(file) ||
    /^scripts\/qualification-first-walk[^/]*$/.test(file) ||
    file === ".github/workflows/image-qualification.yml"
  );
}

function productionRecords(directory) {
  const records = new Map();
  for (const entry of git(directory, "ls-files", "--stage", "-z").split("\0").filter(Boolean)) {
    const match = entry.match(/^(100644|100755|120000) ([a-f0-9]{40}) 0\t(.+)$/s);
    if (!match) throw new Error("unsupported production tree entry");
    const [, mode, object, file] = match;
    if (toolingPath(file)) continue;
    let bytes;
    if (mode === "120000") bytes = Buffer.from(fs.readlinkSync(path.join(directory, file)));
    else {
      bytes = packageFile(directory, file, 128 * 1024 * 1024);
      const executable = Boolean(fs.statSync(path.join(directory, file)).mode & 0o111);
      if (executable !== (mode === "100755")) throw new Error("production executable mode changed");
    }
    const actual = crypto
      .createHash("sha1")
      .update(`blob ${bytes.length}\0`)
      .update(bytes)
      .digest("hex");
    if (actual !== object) throw new Error(`production working input changed: ${file}`);
    records.set(file, `${mode} ${object}`);
  }
  for (const file of git(directory, "ls-files", "--others", "--exclude-standard", "-z")
    .split("\0")
    .filter(Boolean))
    if (!toolingPath(file)) throw new Error(`untracked production input: ${file}`);
  return [...records].sort(([a], [b]) => a.localeCompare(b));
}

export function assertProductionBaseline(candidate, baseline, contract) {
  if (
    git(baseline, "rev-parse", "HEAD").trim() !== contract.baselineCommit ||
    git(baseline, "rev-parse", "HEAD^{tree}").trim() !== contract.baselineTree
  )
    throw new Error("private baseline identity mismatch");
  const expected = productionRecords(baseline);
  if (canonical(productionRecords(candidate)) !== canonical(expected))
    throw new Error("production inputs differ from pinned baseline");
  return digest(canonical(expected));
}

export function qualificationIdentity(directory) {
  return readQualificationPackage(directory).identity;
}

function workerSourceDigest(directory) {
  const records = [];
  const visit = (relative) => {
    for (const entry of fs.readdirSync(path.join(directory, relative), { withFileTypes: true })) {
      const file = `${relative}/${entry.name}`;
      if (entry.isDirectory()) visit(file);
      else if (entry.isFile()) {
        const bytes = packageFile(directory, file, 4 * 1024 * 1024);
        records.push({
          path: file.slice("worker/src/".length),
          bytes: bytes.length,
          sha256: digest(bytes),
        });
      } else throw new Error("private source contains a non-file entry");
    }
  };
  visit("worker/src");
  records.sort((a, b) => (a.path < b.path ? -1 : a.path > b.path ? 1 : 0));
  return digest(canonical(records));
}

export function prepareQualificationBuild(protectedRoot, candidate, baseline) {
  const roots = [protectedRoot, candidate, baseline].map((p) => fs.realpathSync(p));
  for (let i = 0; i < roots.length; i++)
    for (let j = i + 1; j < roots.length; j++)
      if (
        roots[i] === roots[j] ||
        roots[i].startsWith(roots[j] + path.sep) ||
        roots[j].startsWith(roots[i] + path.sep)
      )
        throw new Error("private build requires separate checkouts");
  const { contract, identity } = readQualificationPackage(protectedRoot);
  for (const file of [
    "manifest.json",
    "runtime.patch",
    "linuxOwnershipRepair.sh",
    "wrangler.private.json",
  ])
    if (
      !packageFile(protectedRoot, `${qualificationPackagePath}/${file}`).equals(
        packageFile(candidate, `${qualificationPackagePath}/${file}`),
      )
    )
      throw new Error("candidate changed protected qualification package");
  const productionInputsSha256 = assertProductionBaseline(candidate, baseline, contract);
  if (
    digest(packageFile(baseline, "worker/wrangler.aws-qualification-authority.jsonc")) !==
    identity.authorityConfigSha256
  )
    throw new Error("private authority configuration mismatch");
  for (const file of contract.files) {
    const target = path.join(baseline, file.path);
    if (file.beforeSha256 === null) {
      if (fs.existsSync(target)) throw new Error("private overlay addition already exists");
    } else if (digest(packageFile(baseline, file.path)) !== file.beforeSha256)
      throw new Error("private overlay preimage mismatch");
  }
  const overlay = path.join(protectedRoot, qualificationPackagePath, "runtime.patch");
  git(baseline, "apply", "--check", "--", overlay);
  git(baseline, "apply", "--", overlay);
  const changed = git(baseline, "diff", "--name-only").trim().split("\n").filter(Boolean);
  const added = git(baseline, "ls-files", "--others", "--exclude-standard")
    .trim()
    .split("\n")
    .filter(Boolean);
  if (
    canonical([...changed, ...added].sort()) !== canonical(contract.files.map((f) => f.path).sort())
  )
    throw new Error("private overlay changed unexpected paths");
  for (const file of contract.files)
    if (digest(packageFile(baseline, file.path)) !== file.afterSha256)
      throw new Error("private overlay result mismatch");
  if (
    digest(packageFile(baseline, "recipes/bootstrap/v1/linuxOwnershipRepair.sh")) !==
    identity.recipeSha256
  )
    throw new Error("private ownership recipe mismatch");
  if (workerSourceDigest(baseline) !== identity.workerSourceSha256)
    throw new Error("private Worker source mismatch");
  fs.writeFileSync(
    path.join(baseline, "worker/wrangler.qualification.json"),
    packageFile(protectedRoot, `${qualificationPackagePath}/wrangler.private.json`),
    { flag: "wx" },
  );
  return { qualification: identity, productionInputsSha256 };
}
