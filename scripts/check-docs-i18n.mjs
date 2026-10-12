#!/usr/bin/env node
import fs from "node:fs";
import path from "node:path";
import { isDeepStrictEqual } from "node:util";
import { fileURLToPath } from "node:url";
import { locales, pilotPages, protectedMarkdown, readTranslation, sourceHash } from "./lib/docs-i18n.mjs";

export function validateTranslation(source, translation, glossary = []) {
  const failures = [];
  const before = protectedMarkdown(source);
  const after = protectedMarkdown(translation);
  for (const [kind, value] of Object.entries(before)) {
    if (!isDeepStrictEqual(value, after[kind])) failures.push(`changed ${kind}`);
  }
  for (const term of glossary) {
    const escaped = term.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    const pattern = new RegExp(`(?<![A-Za-z0-9])${escaped}(?![A-Za-z0-9])`);
    if (pattern.test(source) && !pattern.test(translation)) failures.push(`missing glossary term: ${term}`);
  }
  return failures;
}

export function checkTranslations(root) {
  const glossary = JSON.parse(fs.readFileSync(path.join(root, "docs/.i18n/glossary.json"), "utf8")).keep;
  const stale = [];
  const failures = [];
  let count = 0;
  for (const locale of Object.keys(locales).filter((lang) => lang !== "en")) {
    const localeDir = path.join(root, "docs/.i18n", locale);
    const translatedPages = fs.existsSync(localeDir) ? markdownPaths(localeDir) : [];
    for (const page of new Set([...pilotPages, ...translatedPages])) {
      const rel = `docs/.i18n/${locale}/${page}`;
      try {
        const source = fs.readFileSync(path.join(root, "docs", page), "utf8");
        const translation = readTranslation(path.join(root, rel), source);
        if (translation.source !== page) throw new Error("wrong source path");
        count++;
        // A stale translation intentionally retains the previous English examples and anchors.
        if (translation.stale) stale.push(rel);
        else failures.push(...validateTranslation(source, translation.markdown, glossary).map((issue) => `${rel}: ${issue}`));
      } catch (error) { failures.push(`${rel}: ${error.message}`); }
    }
    const rel = `docs/.i18n/${locale}/site.json`;
    try {
      const catalog = JSON.parse(fs.readFileSync(path.join(root, rel), "utf8"));
      if (catalog.source !== "scripts/build-docs-site.mjs" || !/^[a-f0-9]{64}$/.test(catalog.sourceHash)
        || typeof catalog.outdated !== "string" || !catalog.outdated
        || !catalog.messages || Object.values(catalog.messages).some((value) => typeof value !== "string" || !value)) {
        throw new Error("invalid site catalog");
      }
      if (catalog.sourceHash !== sourceHash(fs.readFileSync(path.join(root, catalog.source)))) stale.push(rel);
    } catch (error) { failures.push(`${rel}: ${error.message}`); }
  }
  return { count, stale, failures };
}

function markdownPaths(dir, prefix = "") {
  return fs.readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const rel = path.posix.join(prefix, entry.name);
    if (entry.isDirectory()) return markdownPaths(path.join(dir, entry.name), rel);
    return entry.name.endsWith(".md") ? [rel] : [];
  });
}

if (process.argv[1] && fs.realpathSync(process.argv[1]) === fs.realpathSync(fileURLToPath(import.meta.url))) {
  const result = checkTranslations(process.cwd());
  for (const file of result.stale) console.log(`stale: ${file}`);
  for (const issue of result.failures) console.error(issue);
  console.log(`checked ${result.count} translations; ${result.stale.length} stale; ${result.failures.length} invalid`);
  if (result.failures.length || (process.argv.includes("--strict") && result.stale.length)) process.exitCode = 1;
}
