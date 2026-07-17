import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

import {
  normalizeNavigationCounts,
  normalizeProviderCountFiles,
} from "./normalize-provider-counts.mjs";

function section(name, count, hrefs) {
  const links = hrefs
    .map((href) => `<a class="nav-link" href="${href}">Page</a>`)
    .join("");
  return `<details class="nav-section"><summary><h2>${name}</h2><span class="nav-count">${count}</span></summary><div class="nav-links">${links}</div></details>`;
}

test("normalizes every sidebar section while excluding index pages", () => {
  const source = [
    '<a class="cta-secondary">Browse 99 providers</a>',
    section("Start", 6, ["index.html", "getting-started.html", "cli.html"]),
    section("Providers", 77, ["providers/index.html", "providers/aws.html", "providers/azure.html"]),
    section("Features", 4, ["features/index.html", "features/sync.html"]),
    section("Commands", 3, ["commands/index.html", "commands/run.html", "commands/ssh.html"]),
    section("Operate", 2, ["operations.html", "security.html"]),
  ].join("\n");

  const normalized = normalizeNavigationCounts(source, 2);

  assert.match(normalized, /Browse 2 providers/);
  assert.match(normalized, /<h2>Start<\/h2><span class="nav-count">2<\/span>/);
  assert.match(normalized, /<h2>Providers<\/h2><span class="nav-count">2<\/span>/);
  assert.match(normalized, /<h2>Features<\/h2><span class="nav-count">1<\/span>/);
  assert.match(normalized, /<h2>Commands<\/h2><span class="nav-count">2<\/span>/);
  assert.match(normalized, /<h2>Operate<\/h2><span class="nav-count">2<\/span>/);
});

test("handles active links and nested relative index links", () => {
  const source = `<details class="nav-section" open><summary><h2>Providers</h2><span class="nav-count">9</span></summary><div class="nav-links"><a class="nav-link active" href="../providers/index.html">Index</a><a class="nav-link" href="../providers/aws.html?x=1#y">AWS</a></div></details>`;
  const normalized = normalizeNavigationCounts(source, 1);
  assert.match(normalized, /<span class="nav-count">1<\/span>/);
});

test("derives the canonical provider count and updates every generated HTML page", () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-nav-count-"));
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
    `<a>Browse 99 providers</a>${section("Providers", 100, ["providers/index.html", "providers/alpha.html", "providers/beta.html"])}`,
  );
  fs.writeFileSync(
    path.join(nestedDir, "index.html"),
    section("Providers", 100, ["index.html", "alpha.html", "beta.html"]),
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
  assert.throws(() => normalizeNavigationCounts("", -1), /non-negative integer/);
  assert.throws(() => normalizeNavigationCounts("", 1.5), /non-negative integer/);
});
