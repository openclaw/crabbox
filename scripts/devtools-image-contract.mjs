#!/usr/bin/env node

import { createHash } from "node:crypto";
import { realpathSync } from "node:fs";
import { lstat, readFile, realpath } from "node:fs/promises";
import { dirname, isAbsolute, relative, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";
import {
  canonicalJSON,
  parseStrictJSON,
  validateJSONSchema,
} from "./generate-linux-readiness.mjs";

const scriptPath = fileURLToPath(import.meta.url);
const repoRoot = resolve(dirname(scriptPath), "..");
export const devtoolsRecipeSchema = "crabbox-devtools-image-recipe/v1";
export const defaultRecipePath = "recipes/devtools/v1/linux-x86_64.json";
const schemaPath = "recipes/devtools/v1/recipe.schema.json";
const requiredInputs = [
  "scripts/install-linux-developer-tools.sh",
  "scripts/linux-readiness.generated.sh",
];
const recipeKeys = ["execution", "id", "inputs", "platform", "schema"];
const platformKeys = ["architecture", "os"];
const executionKeys = ["arguments", "command", "environment"];
const inputKeys = ["path", "sha256"];
const allowedSchemaKeys = new Set([
  "$schema", "$id", "title", "description", "type", "additionalProperties",
  "required", "properties", "const", "enum", "minItems", "uniqueItems", "items",
  "pattern",
]);
const forbiddenKey = /(?:^|_)(?:account|credential|image|instance|location|password|privatekey|project|provider|region|secret|snapshot|subscription|token|zone)(?:id|type)?(?:$|_)/iu;
const forbiddenString = [
  { name: "URL", pattern: /\b[a-z][a-z0-9+.-]*:\/\//iu },
  { name: "provider resource ID", pattern: /\b(?:ami|snap)-[0-9a-f]{8,17}\b|\/subscriptions\/|projects\/[^/]+\/(?:global|regions|zones)\//iu },
  { name: "region", pattern: /\b(?:af|ap|ca|eu|il|me|sa|us)-(?:gov-)?[a-z]+-\d\b|\b(?:africa|asia|australia|europe|me|northamerica|southamerica|us)-(?:central|east|west|north|south)\d\b/iu },
  { name: "instance type", pattern: /\b[a-z][a-z0-9-]*\.(?:metal|micro|small|medium|large|[0-9]+xlarge)\b/iu },
  { name: "credential", pattern: /\b(?:api[ _-]?key|credential|password|private[ _-]?key|secret|token)\b/iu },
];

function assertExactKeys(value, expected, context) {
  const actual = Object.keys(value).sort();
  const wanted = [...expected].sort();
  if (actual.length !== wanted.length || actual.some((key, index) => key !== wanted[index])) {
    throw new Error(`${context} must contain exactly: ${wanted.join(", ")}`);
  }
}

function assertRequired(schema, keys, context) {
  if (canonicalJSON([...(schema.required ?? [])].sort()) !== canonicalJSON([...keys].sort())) {
    throw new Error(`${context} must require every contract field`);
  }
}

function assertSchemaNode(schema, keys, context) {
  if (!schema || typeof schema !== "object" || Array.isArray(schema)) {
    throw new Error(`${context} must be an object`);
  }
  for (const key of Object.keys(schema)) {
    if (!allowedSchemaKeys.has(key)) throw new Error(`${context} uses unsupported schema keyword ${key}`);
  }
  if (schema.type !== "object" || schema.additionalProperties !== false) {
    throw new Error(`${context} must reject unknown object fields`);
  }
  assertExactKeys(schema.properties ?? {}, keys, `${context} properties`);
  assertRequired(schema, keys, context);
}

export function validateRecipeSchemaDefinition(schema) {
  assertSchemaNode(schema, recipeKeys, "recipe schema");
  if (schema.$schema !== "https://json-schema.org/draft/2020-12/schema") {
    throw new Error("recipe schema must declare JSON Schema draft 2020-12");
  }
  if (schema.properties.schema?.const !== devtoolsRecipeSchema) {
    throw new Error("recipe schema has an invalid contract version");
  }
  if (schema.properties.id?.pattern !== "^[a-z0-9]+(?:-[a-z0-9_]+)*$") {
    throw new Error("recipe schema has an invalid recipe ID contract");
  }
  assertSchemaNode(schema.properties.platform, platformKeys, "platform schema");
  if (canonicalJSON(schema.properties.platform.properties.os?.enum) !== '["linux"]' ||
      canonicalJSON(schema.properties.platform.properties.architecture?.enum) !== '["x86_64"]') {
    throw new Error("platform schema has unsupported targets");
  }
  assertSchemaNode(schema.properties.execution, executionKeys, "execution schema");
  if (schema.properties.execution.properties.command?.const !== "bash") {
    throw new Error("execution schema must require bash");
  }
  const argumentsSchema = schema.properties.execution.properties.arguments;
  if (argumentsSchema?.type !== "array" || argumentsSchema.minItems !== 1 ||
      argumentsSchema.uniqueItems !== true || argumentsSchema.items?.type !== "string") {
    throw new Error("execution schema has an invalid arguments contract");
  }
  assertSchemaNode(schema.properties.execution.properties.environment, [], "environment schema");
  const inputsSchema = schema.properties.inputs;
  if (inputsSchema?.type !== "array" || inputsSchema.minItems !== 1 ||
      inputsSchema.uniqueItems !== true) {
    throw new Error("recipe schema has an invalid inputs contract");
  }
  assertSchemaNode(inputsSchema.items, inputKeys, "input schema");
  if (inputsSchema.items.properties.sha256?.pattern !== "^sha256:[0-9a-f]{64}$") {
    throw new Error("input schema must require a lowercase SHA-256 digest");
  }
}

export function assertProviderNeutral(value, context = "recipe") {
  if (Array.isArray(value)) {
    value.forEach((item, index) => assertProviderNeutral(item, `${context}[${index}]`));
    return;
  }
  if (value && typeof value === "object") {
    for (const [key, item] of Object.entries(value)) {
      if (forbiddenKey.test(key)) throw new Error(`${context}.${key} is provider-specific or credential-bearing`);
      assertProviderNeutral(item, `${context}.${key}`);
    }
    return;
  }
  if (typeof value !== "string") return;
  for (const forbidden of forbiddenString) {
    if (forbidden.pattern.test(value)) throw new Error(`${context} contains a forbidden ${forbidden.name}`);
  }
}

function assertRepoRelativePath(path, context) {
  if (typeof path !== "string" || path.length === 0 || isAbsolute(path) ||
      path.includes("\\") || path.split("/").some((part) => part === "" || part === "." || part === "..")) {
    throw new Error(`${context} must be a normalized repository-relative path`);
  }
}

async function assertRegularRepoFile(root, path, context) {
  assertRepoRelativePath(path, context);
  const rootPath = await realpath(root);
  let current = rootPath;
  for (const part of path.split("/")) {
    current = resolve(current, part);
    const info = await lstat(current);
    if (info.isSymbolicLink()) throw new Error(`${context} must not traverse a symlink`);
  }
  const info = await lstat(current);
  if (!info.isFile()) throw new Error(`${context} must identify a regular file`);
  const actual = await realpath(current);
  const rel = relative(rootPath, actual);
  if (rel === ".." || rel.startsWith(`..${sep}`) || isAbsolute(rel)) {
    throw new Error(`${context} escapes the repository`);
  }
  return actual;
}

function sha256(bytes) {
  return `sha256:${createHash("sha256").update(bytes).digest("hex")}`;
}

export function recipeDigest(recipe) {
  return sha256(canonicalJSON({
    execution: recipe.execution,
    id: recipe.id,
    inputs: recipe.inputs,
    platform: recipe.platform,
    schema: recipe.schema,
  }));
}

export async function assertRecipe(recipe, schema, root = repoRoot) {
  assertProviderNeutral(recipe);
  if (Array.isArray(recipe?.inputs)) {
    for (const input of recipe.inputs) {
      if (input && typeof input === "object" && !Array.isArray(input) && typeof input.path === "string") {
        assertRepoRelativePath(input.path, `recipe input ${input.path}`);
      }
    }
  }
  validateJSONSchema(recipe, schema, "recipe");
  if (recipe.id !== `${recipe.platform.os}-${recipe.platform.architecture}`) {
    throw new Error("recipe.id must match its platform");
  }
  if (canonicalJSON(recipe.execution.arguments) !== canonicalJSON(["scripts/install-linux-developer-tools.sh"])) {
    throw new Error("recipe execution must use the reviewed Linux developer-tools entrypoint");
  }
  if (Object.keys(recipe.execution.environment).length !== 0) {
    throw new Error("recipe execution environment must remain empty");
  }
  const inputPaths = recipe.inputs.map((input) => input.path);
  if (inputPaths.some((path, index) => index > 0 && inputPaths[index - 1] >= path)) {
    throw new Error("recipe inputs must be sorted and unique");
  }
  if (canonicalJSON(inputPaths) !== canonicalJSON(requiredInputs)) {
    throw new Error(`recipe inputs must contain exactly: ${requiredInputs.join(", ")}`);
  }
  for (const input of recipe.inputs) {
    const path = await assertRegularRepoFile(root, input.path, `recipe input ${input.path}`);
    const actual = sha256(await readFile(path));
    if (actual !== input.sha256) throw new Error(`recipe input ${input.path} SHA-256 mismatch`);
  }
  return recipeDigest(recipe);
}

export async function loadDevtoolsRecipe(root = repoRoot, recipePath = defaultRecipePath) {
  const safeSchemaPath = await assertRegularRepoFile(root, schemaPath, "recipe schema path");
  const safeRecipePath = await assertRegularRepoFile(root, recipePath, "recipe path");
  const schema = parseStrictJSON(await readFile(safeSchemaPath, "utf8"), schemaPath);
  validateRecipeSchemaDefinition(schema);
  const recipe = parseStrictJSON(await readFile(safeRecipePath, "utf8"), recipePath);
  const digest = await assertRecipe(recipe, schema, root);
  return { digest, recipe, schema };
}

export async function main(args = process.argv.slice(2)) {
  if (args.length > 1 || args.some((argument) => argument !== "--json")) {
    throw new Error("usage: devtools-image-contract.mjs [--json]");
  }
  const { digest, recipe } = await loadDevtoolsRecipe();
  if (args.includes("--json")) {
    process.stdout.write(`${canonicalJSON({
      id: recipe.id,
      recipeDigest: digest,
      schema: recipe.schema,
    })}\n`);
  } else {
    process.stdout.write(`${digest}\n`);
  }
}

if (process.argv[1] && realpathSync(process.argv[1]) === realpathSync(scriptPath)) {
  await main();
}
