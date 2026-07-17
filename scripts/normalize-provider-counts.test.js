import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

import {
  normalizeProviderCountFiles,
  normalizeProviderCounts,
} from "./normalize-provider-counts.mjs";

test("normalizes the homepage CTA and Providers navigation count", () => {
  const source = [
    '<a class="cta-secondary">Browse 76 providers</a>',
    '<summary><h2>Providers</h2><span class="nav-count">77</span></summary>',
    '<summary><h2>Features</h2><span class="nav-count">42</span></summary>',
  ].join("\n");

  const normalized = normalizeProviderCounts(source, 76);

  assert.match(normalized, /Browse 76 providers/);
  assert.match(normalized, /<h2>Providers<\/h2><span class="nav-count">76<\/span>/);
  assert.match(normalized, /<h2>Features<\/h2><span class="nav-count">42<\/span>/);
});

test("derives the canonical count from provider metadata and updates every HTML page", () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-provider-count-"));
  const metadataDir = path.join(root, "docs", "providers");
  const siteDir = path.join(root, "dist", "docs-site");
  const nestedDir = path.join(siteDir, "providers");
  fs.mkdirSync(metadataDir, { recursive: true });
  fs.mkdirSync(nestedDir, { recursive: true });

  fs.writeFileSync(
    path.join(metadataDir, "provider-metadata.json"),
    JSON.stringify({ alpha: {}, beta: {} }),
  );
  fs.writeFileSync(
    path.join(siteDir, "index.html"),
    '<a>Browse 99 providers</a><summary><h2>Providers</h2><span class="nav-count">100</span></summary>',
  );
  fs.writeFileSync(
    path.join(nestedDir, "index.html"),
    '<summary><h2>Providers</h2><span class="nav-count">100</span></summary>',
  );

  const result = normalizeProviderCountFiles({ root, siteDir });

  assert.deepEqual(result, { providerCount: 2, changed: 2 });
  assert.match(fs.readFileSync(path.join(siteDir, "index.html"), "utf8"), /Browse 2 providers/);
  assert.match(
    fs.readFileSync(path.join(nestedDir, "index.html"), "utf8"),
    /<span class="nav-count">2<\/span>/,
  );
});

test("rejects invalid provider counts", () => {
  assert.throws(() => normalizeProviderCounts("", -1), /non-negative integer/);
  assert.throws(() => normalizeProviderCounts("", 1.5), /non-negative integer/);
});
