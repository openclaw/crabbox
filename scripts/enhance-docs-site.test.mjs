import assert from "node:assert/strict";
import test from "node:test";
import { enhanceFeaturesPage } from "./enhance-docs-site.mjs";

const fixture = `<!doctype html><html><head><title>Features - Crabbox Docs</title><style>.base{display:block}</style></head><body><main><header class="hero"><div class="hero-text"><p class="eyebrow">Features</p><h1>Features</h1></div><div class="hero-meta"><a class="edit" href="edit">Edit page</a></div></header><div class="doc-grid"><article class="doc"><h1 id="features">Features</h1><p>Capability docs.</p><h2 id="foundations"><a class="anchor" href="#foundations">#</a>Foundations</h2><ul><li><a href="configuration.html">Configuration</a>: precedence and schema.</li><li><a href="network.html">Network</a>: public and private reachability.</li></ul><h2 id="sync-execution-and-evidence"><a class="anchor" href="#sync-execution-and-evidence">#</a>Sync, execution, and evidence</h2><ul><li><a href="artifacts.html">Artifacts</a>: screenshots and logs.</li></ul><nav class="page-nav"><a href="next.html">Next</a></nav></article><nav class="toc"><h2>On this page</h2></nav></div></main><script>const existing=true;
</script>
</body></html>`;

test("builds the polished feature explorer", () => {
  const out = enhanceFeaturesPage(fixture);
  assert.match(out, /Build locally\./);
  assert.match(out, /Prove every result/);
  assert.match(out, /data-feature-explorer/);
  assert.match(out, /data-fx-filter="foundations"/);
  assert.match(out, /Choose a provider/);
  assert.match(out, /Open command reference/);
  assert.match(out, /Press <kbd>\/</);
  assert.equal((out.match(/data-fx-card(?:\s|>)/g) || []).length, 3);
  assert.match(out, /3 capabilities/);
  assert.doesNotMatch(out, /<nav class="toc"/);
  assert.doesNotMatch(out, /<nav class="page-nav"/);
});

test("adds accessible search and filter controls", () => {
  const out = enhanceFeaturesPage(fixture);
  assert.match(out, /aria-live="polite" data-fx-count/);
  assert.match(out, /role="group" aria-label="Capability area"/);
  assert.match(out, /data-fx-clear hidden/);
  assert.match(out, /No matching capabilities/);
  assert.match(out, /event|keydown|addEventListener/);
});

test("is idempotent and ignores unrelated pages", () => {
  const once = enhanceFeaturesPage(fixture);
  assert.equal(enhanceFeaturesPage(once), once);
  assert.equal(enhanceFeaturesPage("<title>Other</title>"), "<title>Other</title>");
});