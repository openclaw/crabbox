import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { localizeHTML, readTranslation, sourceHash } from "./lib/docs-i18n.mjs";
import { validateTranslation, checkTranslations } from "./check-docs-i18n.mjs";

test("pilot translations preserve all code, links, anchors and glossary terms", () => {
  assert.deepEqual(checkTranslations(path.resolve(import.meta.dirname, "..")).failures, []);
});

test("translation validation catches edited code, flags, links and anchors", () => {
  const source = '# Hello\n\nUse Crabbox `--id` and [help](getting-started.md).\n\n```sh\ncrabbox run -- pnpm test\n```';
  const translated = '# Bonjour {#hello}\n\nCrabbox `--id` et [aide](getting-started.md).\n\n```sh\ncrabbox run -- pnpm test\n```';
  assert.deepEqual(validateTranslation(source, translated, ["Crabbox"]), []);
  for (const [from, to, expected] of [["--id", "--identifiant", "inline"], ["pnpm test", "pnpm essai", "blocks"],
    ["getting-started.md", "absent.md", "links"], ["{#hello}", "{#bonjour}", "headings"], ["Crabbox", "Crabe", "glossary"]]) {
    assert.ok(validateTranslation(source, translated.replace(from, to), ["Crabbox"]).some((issue) => issue.includes(expected)));
  }
});

test("source changes mark a translation stale without rejecting its previous content", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "cbx-i18n-"));
  try {
    const file = path.join(dir, "page.md");
    fs.writeFileSync(file, `<!-- source: page.md; sha256: ${sourceHash("old")} -->\n# Bonjour`);
    assert.equal(readTranslation(file, "old").stale, false);
    assert.equal(readTranslation(file, "new").stale, true);
    assert.equal(readTranslation(file, "new").markdown, "# Bonjour");
  } finally { fs.rmSync(dir, { recursive: true, force: true }); }
});

test("site localization preserves executable examples and escapes translated text", () => {
  const html = '<h1>Hello</h1><p>83 providers</p><input placeholder="Search"><pre data-copy-text="Hello"><code>Hello --id</code></pre><script>Hello</script><style>Hello</style>';
  assert.equal(localizeHTML(html, { Hello: "<Bonjour>", "{count} providers": "{count} fournisseurs", Search: "Recherche" }),
    '<h1>&lt;Bonjour&gt;</h1><p>83 fournisseurs</p><input placeholder="Recherche"><pre data-copy-text="Hello"><code>Hello --id</code></pre><script>Hello</script><style>Hello</style>');
});

test("unknown messages do not resolve inherited Object properties", () => {
  assert.equal(localizeHTML('<p>constructor</p><input title="toString">', {}), '<p>constructor</p><input title="toString">');
});
