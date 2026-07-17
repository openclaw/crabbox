import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";

import { markdownToHtml } from "./build-docs-site.mjs";

const repoRoot = path.resolve(import.meta.dirname, "..");
const providersDir = path.join(repoRoot, "docs", "providers");
const siteDir = path.join(repoRoot, "dist", "docs-site");
const providerIndexFile = path.join(siteDir, "providers", "index.html");
const generatedTest = fs.existsSync(providerIndexFile) ? test : test.skip;

const providerMarkdown = fs
  .readdirSync(providersDir)
  .filter((name) => name.endsWith(".md"))
  .sort((a, b) => (a === "README.md" ? -1 : b === "README.md" ? 1 : a.localeCompare(b)));

generatedTest("generated navigation includes every provider page exactly once", () => {
  const html = readGenerated("providers/index.html");
  const providerNav = navSection(html, "Providers");

  assert.match(
    providerNav,
    new RegExp(`<span class="nav-count">${providerMarkdown.length}</span>`),
  );
  assert.equal(
    occurrences(providerNav, '<a class="nav-link'),
    providerMarkdown.length,
    "provider navigation count should match the provider Markdown count",
  );

  for (const markdown of providerMarkdown) {
    const output = markdown === "README.md" ? "index.html" : markdown.replace(/\.md$/, ".html");
    const href = `href="../providers/${output}"`;
    assert.equal(
      occurrences(providerNav, href),
      1,
      `${markdown} should be linked exactly once from the Providers navigation`,
    );
  }
});

generatedTest("generated AWS page is active in Providers navigation and pager", () => {
  const html = readGenerated("providers/aws.html");

  assert.match(html, /<p class="eyebrow">Providers<\/p>/);
  assert.equal(occurrences(html, 'aria-current="page"'), 1);
  assert.match(
    html,
    /<a class="nav-link active" href="\.\.\/providers\/aws\.html"[^>]*aria-current="page">AWS Provider<\/a>/,
  );

  const awsIndex = providerMarkdown.indexOf("aws.md");
  assert.notEqual(awsIndex, -1);
  const previous = providerOutput(providerMarkdown[awsIndex - 1]);
  const next = providerOutput(providerMarkdown[awsIndex + 1]);
  const pager = element(html, "nav", /class="page-nav"/);

  assert.match(pager, new RegExp(`href="\.\./providers/${escapeRegExp(previous)}"`));
  assert.match(pager, new RegExp(`href="\.\./providers/${escapeRegExp(next)}"`));
});

generatedTest("homepage primary CTA targets an indexed Start page", () => {
  const home = readGenerated("index.html");
  const startNav = navSection(home, "Start");
  const gettingStarted = readGenerated("getting-started.html");

  assert.match(home, /<a class="cta-primary" href="getting-started\.html">Get started<\/a>/);
  assert.match(startNav, /href="getting-started\.html"[^>]*>Getting Started<\/a>/);
  assert.match(gettingStarted, /<a class="nav-link active" href="getting-started\.html"[^>]*aria-current="page">Getting Started<\/a>/);
  assert.match(gettingStarted, /<nav class="page-nav"/);
});

generatedTest("provider index renders filterable rows in a scroll region", () => {
  const html = readGenerated("providers/index.html");
  const metadata = JSON.parse(fs.readFileSync(path.join(providersDir, "provider-metadata.json"), "utf8"));
  const filter = element(html, "div", /class="provider-filter"[^>]*data-provider-filter/);
  const matrixRegion = element(html, "div", /class="table-scroll"[^>]*role="region"/);
  const rows = [...matrixRegion.matchAll(/<tr data-provider="([^"]+)"[^>]*data-provider-groups="([^"]+)"[^>]*data-provider-search="[^"]+">/g)];

  assert.match(filter, /<input id="provider-filter-input"[^>]*type="search"/);
  assert.match(filter, /<output[^>]*aria-live="polite"[^>]*data-provider-count>/);
  assert.match(filter, /data-provider-group-filter="all"[^>]*aria-pressed="true"/);
  assert.match(filter, /data-provider-empty/);
  assert.match(matrixRegion, /<table class="provider-matrix">/);
  assert.equal(
    occurrences(html, "split(/\\s+/)"),
    3,
    "generated search and provider filters should split on whitespace",
  );
  assert.equal(rows.length, Object.keys(metadata).length);
  assert.deepEqual(
    new Set(rows.map((match) => match[1])),
    new Set(Object.keys(metadata)),
    "generated provider rows should match provider metadata",
  );
  assert.ok(rows.every((match) => match[2]), "every provider row should have a filter group");
  const daytona = rows.find((match) => match[1] === "daytona");
  assert.ok(daytona, "Daytona should be present in the provider matrix");
  assert.deepEqual(
    new Set(daytona[2].split(/\s+/)),
    new Set(["managed-cloud", "team-cloud"]),
    "Daytona should be discoverable as both a managed sandbox and coordinator-backed provider",
  );
});

generatedTest("provider search includes credential and API-key metadata", () => {
  const html = readGenerated("providers/index.html");
  const matrixRegion = element(html, "div", /class="table-scroll"[^>]*role="region"/);
  const searchByProvider = new Map(
    [...matrixRegion.matchAll(/<tr data-provider="([^"]+)"[^>]*data-provider-search="([^"]*)">/g)]
      .map((match) => [match[1], match[2]]),
  );

  for (const [provider, terms] of [
    ["opencomputer", ["api key", "x-api-key", "crabbox opencomputer api key"]],
    ["digitalocean", ["api token", "digitalocean token"]],
    ["linode", ["api token", "linode token"]],
    ["vultr", ["api key", "vultr api key"]],
    ["runpod", ["api key", "runpod api key"]],
    ["vast", ["api key", "crabbox vast api key"]],
    ["wandb", ["api key", "crabbox wandb api key"]],
  ]) {
    const search = searchByProvider.get(provider);
    assert.ok(search, `${provider} should be indexed in provider search`);
    for (const term of terms) {
      assert.match(search, new RegExp(escapeRegExp(term)), `${provider} search should include ${term}`);
    }
  }

  assert.match(searchByProvider.get("aws"), /broker-owned credentials/);
  assert.match(searchByProvider.get("azure"), /defaultazurecredential/);
  assert.match(searchByProvider.get("gcp"), /google adc/);
});

generatedTest("generated Features navigation stays capability-focused", () => {
  const html = readGenerated("features/index.html");
  const featuresNav = navSection(html, "Features");

  for (const legacy of [
    "aws",
    "azure",
    "aws-private-workspaces",
    "blacksmith-testbox",
    "capacity-fallback",
    "daytona",
    "delegated-runner-contract",
    "e2b",
    "hetzner",
    "islo",
    "namespace-devbox",
    "namespace-devbox-setup",
    "provider-authoring",
    "provider-landscape",
    "provider-live-smoke",
    "provider-selection",
    "providers",
    "semaphore",
    "slurm-academic-sandboxes",
    "sprites",
  ]) {
    assert.doesNotMatch(featuresNav, new RegExp(`href="\\.\\./features/${legacy}\\.html"`));
    assert.ok(fs.existsSync(path.join(siteDir, "features", `${legacy}.html`)), `${legacy} legacy page should still build`);
  }

  assert.match(featuresNav, /href="\.\.\/features\/configuration\.html"/);
  assert.match(featuresNav, /href="\.\.\/features\/sync\.html"/);
  assert.match(featuresNav, /href="\.\.\/features\/artifacts\.html"/);
});

generatedTest("generated provider markup hides comments and preserves list structure", () => {
  const indexHtml = readGenerated("providers/index.html");
  const awsHtml = readGenerated("providers/aws.html");

  assert.doesNotMatch(indexHtml, /BEGIN GENERATED PROVIDER MATRIX|END GENERATED PROVIDER MATRIX/);
  assertValidListChildren(article(indexHtml), "provider index");
  assertValidListChildren(article(awsHtml), "AWS provider");
});

test("Markdown comments remain literal inside fenced code", () => {
  const html = markdownToHtml(
    "```html\n<!-- marker -->\n<div>example</div>\n```\n\nVisible paragraph.",
    "example.md",
  );

  assert.match(html, /&lt;!-- marker --&gt;/);
  assert.match(html, /&lt;div&gt;example&lt;\/div&gt;/);
  assert.match(html, /<p>Visible paragraph\.<\/p>/);
});

function readGenerated(relativePath) {
  return fs.readFileSync(path.join(siteDir, relativePath), "utf8");
}

function navSection(html, heading) {
  const details = [...html.matchAll(/<details class="nav-section"[^>]*>[\s\S]*?<\/details>/g)];
  const match = details.find((candidate) => candidate[0].includes(`<h2>${heading}</h2>`));
  assert.ok(match, `${heading} navigation section should exist`);
  return match[0];
}

function article(html) {
  return element(html, "article", /class="[^"]*\bdoc\b[^"]*"/);
}

function element(html, tag, attributes) {
  const openings = new RegExp(`<${tag}\\b[^>]*>`, "g");
  let opening;
  while ((opening = openings.exec(html)) && !attributes.test(opening[0])) {
    // Find the requested element before balancing nested elements of the same type.
  }
  assert.ok(opening, `expected generated <${tag}> matching ${attributes}`);

  const tags = new RegExp(`<\\/?${tag}\\b[^>]*>`, "g");
  tags.lastIndex = opening.index;
  let depth = 0;
  let match;
  while ((match = tags.exec(html))) {
    depth += match[0].startsWith("</") ? -1 : 1;
    if (depth === 0) return html.slice(opening.index, tags.lastIndex);
  }
  assert.fail(`generated <${tag}> matching ${attributes} is not closed`);
}

function providerOutput(markdown) {
  assert.ok(markdown, "AWS should have adjacent provider pages");
  return markdown === "README.md" ? "index.html" : markdown.replace(/\.md$/, ".html");
}

function occurrences(haystack, needle) {
  return haystack.split(needle).length - 1;
}

function escapeRegExp(value) {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

function assertValidListChildren(html, label) {
  const stack = [];
  const token = /<\/?([a-zA-Z][\w:-]*)\b[^>]*>/g;
  let cursor = 0;
  let match;

  while ((match = token.exec(html))) {
    const text = html.slice(cursor, match.index);
    const parent = stack.at(-1);
    if ((parent === "ul" || parent === "ol") && text.trim()) {
      assert.fail(`${label} has text directly inside <${parent}>: ${text.trim().slice(0, 80)}`);
    }

    const tag = match[1].toLowerCase();
    const closing = match[0].startsWith("</");
    if (closing) {
      const index = stack.lastIndexOf(tag);
      assert.notEqual(index, -1, `${label} has an unmatched </${tag}>`);
      stack.length = index;
    } else {
      if (parent === "ul" || parent === "ol") {
        assert.equal(tag, "li", `${label} has <${tag}> directly inside <${parent}>`);
      }
      if (!voidElements.has(tag) && !match[0].endsWith("/>")) stack.push(tag);
    }
    cursor = token.lastIndex;
  }
}

const voidElements = new Set([
  "area",
  "base",
  "br",
  "col",
  "embed",
  "hr",
  "img",
  "input",
  "link",
  "meta",
  "param",
  "source",
  "track",
  "wbr",
]);
