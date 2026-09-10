import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { cp, mkdir, mkdtemp, readFile, rm, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import {
  assertProviderNeutral,
  assertRecipe,
  defaultRecipePath,
  loadDevtoolsRecipe,
  recipeDigest,
  validateRecipeSchemaDefinition,
} from "./devtools-image-contract.mjs";
import { canonicalJSON, parseStrictJSON } from "./generate-linux-readiness.mjs";

const repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");

async function fixture(t) {
  const root = await mkdtemp(join(tmpdir(), "crabbox-devtools-recipe-"));
  t.after(async () => rm(root, { force: true, recursive: true }));
  await mkdir(join(root, "recipes/devtools/v1"), { recursive: true });
  await mkdir(join(root, "scripts"), { recursive: true });
  for (const path of [
    "recipes/devtools/v1/recipe.schema.json",
    defaultRecipePath,
    "scripts/install-linux-developer-tools.sh",
    "scripts/linux-readiness.generated.sh",
  ]) {
    await cp(join(repoRoot, path), join(root, path));
  }
  async function recipe() {
    return parseStrictJSON(await readFile(join(root, defaultRecipePath), "utf8"), defaultRecipePath);
  }
  async function schema() {
    return parseStrictJSON(
      await readFile(join(root, "recipes/devtools/v1/recipe.schema.json"), "utf8"),
      "recipe.schema.json",
    );
  }
  async function writeRecipe(value) {
    await writeFile(join(root, defaultRecipePath), `${JSON.stringify(value, null, 2)}\n`);
  }
  return { root, recipe, schema, writeRecipe };
}

test("repository recipe validates exact inputs and emits a deterministic digest", async () => {
  const first = await loadDevtoolsRecipe();
  const second = await loadDevtoolsRecipe();
  assert.equal(first.digest, second.digest);
  assert.match(first.digest, /^sha256:[0-9a-f]{64}$/u);
  assert.equal(first.recipe.id, "linux-x86_64");
  assert.deepEqual(first.recipe.execution, {
    command: "bash",
    arguments: ["scripts/install-linux-developer-tools.sh"],
    environment: {},
  });
  assert.deepEqual(
    first.recipe.inputs.map((input) => input.path),
    ["scripts/install-linux-developer-tools.sh", "scripts/linux-readiness.generated.sh"],
  );
});

test("recipe digest binds every executable field and input hash but not object key order", async () => {
  const { recipe } = await loadDevtoolsRecipe();
  const reordered = {
    inputs: recipe.inputs.map(({ path, sha256 }) => ({ sha256, path })),
    execution: {
      environment: recipe.execution.environment,
      arguments: recipe.execution.arguments,
      command: recipe.execution.command,
    },
    platform: { architecture: recipe.platform.architecture, os: recipe.platform.os },
    id: recipe.id,
    schema: recipe.schema,
  };
  assert.equal(recipeDigest(reordered), recipeDigest(recipe));
  for (const mutate of [
    (value) => { value.id = "linux-altered"; },
    (value) => { value.platform.architecture = "arm64"; },
    (value) => { value.execution.command = "sh"; },
    (value) => { value.execution.arguments[0] = "scripts/another.sh"; },
    (value) => { value.execution.environment.MODE = "test"; },
    (value) => { value.inputs[0].sha256 = `sha256:${"0".repeat(64)}`; },
    (value) => { value.inputs[1].sha256 = `sha256:${"f".repeat(64)}`; },
  ]) {
    const changed = structuredClone(recipe);
    mutate(changed);
    assert.notEqual(recipeDigest(changed), recipeDigest(recipe));
  }
});

test("strict loading rejects duplicate keys, trailing documents, and unknown fields", async (t) => {
  const copy = await fixture(t);
  const recipe = await copy.recipe();
  await writeFile(
    join(copy.root, defaultRecipePath),
    `{"schema":"${recipe.schema}","schema":"${recipe.schema}"}\n`,
  );
  await assert.rejects(loadDevtoolsRecipe(copy.root), /duplicate key "schema"/u);
  await copy.writeRecipe(recipe);
  await writeFile(join(copy.root, defaultRecipePath), `${JSON.stringify(recipe)}\n{}\n`);
  await assert.rejects(loadDevtoolsRecipe(copy.root), /trailing JSON content/u);
  recipe.unexpected = true;
  await copy.writeRecipe(recipe);
  await assert.rejects(loadDevtoolsRecipe(copy.root), /recipe\.unexpected is not allowed/u);
});

test("schema validation rejects drift and unsupported keywords", async (t) => {
  const copy = await fixture(t);
  const schema = await copy.schema();
  validateRecipeSchemaDefinition(schema);
  const permissive = structuredClone(schema);
  permissive.additionalProperties = true;
  assert.throws(() => validateRecipeSchemaDefinition(permissive), /reject unknown object fields/u);
  const missing = structuredClone(schema);
  missing.required = missing.required.filter((key) => key !== "inputs");
  assert.throws(() => validateRecipeSchemaDefinition(missing), /require every contract field/u);
  const keyword = structuredClone(schema);
  keyword.oneOf = [];
  assert.throws(() => validateRecipeSchemaDefinition(keyword), /unsupported schema keyword oneOf/u);
  const digest = structuredClone(schema);
  digest.properties.inputs.items.properties.sha256.pattern = ".*";
  assert.throws(() => validateRecipeSchemaDefinition(digest), /lowercase SHA-256/u);
});

test("provider-neutral policy rejects cloud placement, credentials, and URLs", () => {
  for (const [name, value, expected] of [
    ["provider key", { provider: "cloud" }, /provider-specific/u],
    ["provider image", { value: "ami-1234567890abcdef0" }, /provider resource ID/u],
    ["subscription resource", { value: "/subscriptions/example/resourceGroups/group" }, /provider resource ID/u],
    ["project resource", { value: "projects/example/global/images/devtools" }, /provider resource ID/u],
    ["region", { value: "us-west-2" }, /forbidden region/u],
    ["instance type", { value: "m7i.large" }, /instance type/u],
    ["credential", { value: "API token" }, /credential/u],
    ["URL", { value: "https://downloads.example/devtools/latest" }, /forbidden URL/u],
  ]) {
    assert.throws(() => assertProviderNeutral(value), expected, name);
  }
});

test("recipe rejects changed execution, environment, platform, and input set", async (t) => {
  const copy = await fixture(t);
  const schema = await copy.schema();
  const original = await copy.recipe();
  for (const [name, mutate, expected] of [
    ["entrypoint", (recipe) => { recipe.execution.arguments = ["scripts/linux-readiness.generated.sh"]; }, /reviewed Linux developer-tools entrypoint/u],
    ["environment", (recipe) => { recipe.execution.environment.MODE = "test"; }, /not allowed/u],
    ["platform ID", (recipe) => { recipe.id = "linux-other"; }, /match its platform/u],
    ["missing input", (recipe) => { recipe.inputs.pop(); }, /contain exactly/u],
    ["extra input", (recipe) => { recipe.inputs.push({ path: "scripts/zz-extra.sh", sha256: `sha256:${"0".repeat(64)}` }); }, /contain exactly/u],
  ]) {
    const recipe = structuredClone(original);
    mutate(recipe);
    await assert.rejects(assertRecipe(recipe, schema, copy.root), expected, name);
  }
});

test("recipe rejects unsorted, duplicate, escaping, absolute, and backslash input paths", async (t) => {
  const copy = await fixture(t);
  const schema = await copy.schema();
  const original = await copy.recipe();
  for (const [name, mutate, expected] of [
    ["unsorted", (recipe) => { recipe.inputs.reverse(); }, /sorted and unique/u],
    ["duplicate", (recipe) => { recipe.inputs[1] = structuredClone(recipe.inputs[0]); }, /duplicate items/u],
    ["parent escape", (recipe) => { recipe.inputs[0].path = "../outside.sh"; }, /normalized repository-relative path/u],
    ["absolute", (recipe) => { recipe.inputs[0].path = "/tmp/outside.sh"; }, /normalized repository-relative path/u],
    ["backslash", (recipe) => { recipe.inputs[0].path = "scripts\\outside.sh"; }, /normalized repository-relative path/u],
  ]) {
    const recipe = structuredClone(original);
    mutate(recipe);
    await assert.rejects(assertRecipe(recipe, schema, copy.root), expected, name);
  }
});

test("recipe rejects symlinked inputs and symlinked input directories", async (t) => {
  const direct = await fixture(t);
  await rm(join(direct.root, "scripts/install-linux-developer-tools.sh"));
  await symlink(join(repoRoot, "scripts/install-linux-developer-tools.sh"), join(direct.root, "scripts/install-linux-developer-tools.sh"));
  await assert.rejects(loadDevtoolsRecipe(direct.root), /must not traverse a symlink/u);

  const parent = await fixture(t);
  await rm(join(parent.root, "scripts"), { recursive: true });
  await symlink(join(repoRoot, "scripts"), join(parent.root, "scripts"));
  await assert.rejects(loadDevtoolsRecipe(parent.root), /must not traverse a symlink/u);
});

test("recipe rejects content changes and malformed hashes", async (t) => {
  const changed = await fixture(t);
  await writeFile(join(changed.root, "scripts/install-linux-developer-tools.sh"), "#!/bin/sh\nexit 0\n");
  await assert.rejects(loadDevtoolsRecipe(changed.root), /install-linux-developer-tools\.sh SHA-256 mismatch/u);

  const malformed = await fixture(t);
  const recipe = await malformed.recipe();
  recipe.inputs[0].sha256 = "sha256:ABC";
  await malformed.writeRecipe(recipe);
  await assert.rejects(loadDevtoolsRecipe(malformed.root), /recipe\.inputs\[0\]\.sha256 has an invalid format/u);
});

test("CLI is import-safe and returns stable text and JSON output", async () => {
  const imported = spawnSync(
    process.execPath,
    ["--input-type=module", "--eval", "await import('./scripts/devtools-image-contract.mjs')"],
    { cwd: repoRoot, encoding: "utf8" },
  );
  assert.equal(imported.status, 0, imported.stderr);
  assert.equal(imported.stdout, "");

  const text = spawnSync(process.execPath, ["scripts/devtools-image-contract.mjs"], {
    cwd: repoRoot,
    encoding: "utf8",
  });
  assert.equal(text.status, 0, text.stderr);
  assert.match(text.stdout, /^sha256:[0-9a-f]{64}\n$/u);

  const json = spawnSync(process.execPath, ["scripts/devtools-image-contract.mjs", "--json"], {
    cwd: repoRoot,
    encoding: "utf8",
  });
  assert.equal(json.status, 0, json.stderr);
  assert.deepEqual(parseStrictJSON(json.stdout), {
    id: "linux-x86_64",
    recipeDigest: text.stdout.trim(),
    schema: "crabbox-devtools-image-recipe/v1",
  });
  assert.equal(json.stdout, `${canonicalJSON(parseStrictJSON(json.stdout))}\n`);

  const invalid = spawnSync(process.execPath, ["scripts/devtools-image-contract.mjs", "--check"], {
    cwd: repoRoot,
    encoding: "utf8",
  });
  assert.notEqual(invalid.status, 0);
  assert.match(invalid.stderr, /usage: devtools-image-contract\.mjs/u);
});
