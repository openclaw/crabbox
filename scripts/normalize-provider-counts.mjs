#!/usr/bin/env node
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

export function normalizeProviderCounts(source, providerCount) {
  if (!Number.isInteger(providerCount) || providerCount < 0) {
    throw new TypeError("providerCount must be a non-negative integer");
  }

  return source
    .replace(/Browse \d+ providers/g, `Browse ${providerCount} providers`)
    .replace(
      /(<summary><h2>Providers<\/h2><span class="nav-count">)\d+(<\/span>)/g,
      `$1${providerCount}$2`,
    );
}

export function normalizeProviderCountFiles({
  root = process.cwd(),
  siteDir = path.join(root, "dist", "docs-site"),
  metadataFile = path.join(root, "docs", "providers", "provider-metadata.json"),
} = {}) {
  const metadata = JSON.parse(fs.readFileSync(metadataFile, "utf8"));
  const providerCount = Object.keys(metadata).length;
  let changed = 0;

  for (const file of htmlFiles(siteDir)) {
    const source = fs.readFileSync(file, "utf8");
    const normalized = normalizeProviderCounts(source, providerCount);
    if (normalized === source) continue;
    fs.writeFileSync(file, normalized, "utf8");
    changed += 1;
  }

  return { providerCount, changed };
}

function htmlFiles(dir) {
  return fs
    .readdirSync(dir, { withFileTypes: true })
    .flatMap((entry) => {
      const full = path.join(dir, entry.name);
      if (entry.isDirectory()) return htmlFiles(full);
      return entry.name.endsWith(".html") ? [full] : [];
    });
}

const isMain = process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isMain) {
  const result = normalizeProviderCountFiles();
  console.log(`normalized provider count to ${result.providerCount} across ${result.changed} pages`);
}
