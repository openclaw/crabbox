# Translating the Documentation

English files in `docs/` remain the source of truth. The translation pilot covers
the landing page, Getting Started, How Crabbox Works, Concepts, and Use Cases in
Simplified Chinese (`zh-CN`), Japanese (`ja`), Korean (`ko`), Spanish (`es`), German
(`de`), French (`fr`), and Brazilian Portuguese (`pt-BR`).

Translations live in `docs/.i18n/<locale>/`, mirroring the English file names.
The build publishes them at `/<locale>/`, with the same page names and heading
IDs as English. The language menu stays on the corresponding page. If a page is
not translated, its menu entry says `(English)` and links to the English original;
the build does not publish English copies under translated URLs. Only actual
translations appear in `hreflang` alternates. English is the `x-default` version.

## Add or Refresh a Page

1. Copy the English page into `docs/.i18n/<locale>/` and translate its prose,
   headings, table cells, and link labels. Keep all content and safety caveats.
2. Follow `docs/.i18n/glossary.json`. Keep product/provider names unchanged.
   Copy fenced code blocks, including their comments, byte for byte. Preserve
   inline code, commands, flags, paths, and link destinations exactly.
3. Add an explicit English ID to every translated heading, for example
   `## Premiers pas {#getting-started}`. Use the original renderer's heading ID,
   not the translated heading text. Keep heading levels and order unchanged.
4. Record the SHA-256 of the **exact English file bytes**, including its final
   newline, on the first line:

```text
<!-- source: getting-started.md; sha256: <64 lowercase hex characters> -->
```

Compute a hash without rewriting the file:

```sh
shasum -a 256 docs/getting-started.md
```

Update the recorded hash only after reviewing the translation against the new
English text. A hash update alone does not refresh a translation.

## Landing Page and Navigation

The landing page uses a custom layout, not the Markdown README body. Each
locale's `site.json` translates its visible text, navigation, metadata and
interactive labels. Its `source` is `scripts/build-docs-site.mjs`; `sourceHash`
records that file's exact SHA-256. Any generator change conservatively marks
the catalog stale, including changes that do not affect its English text.
Review the resulting landing page and labels before updating the hash.

Catalog keys are the English text emitted by the renderer, including HTML
entities such as `&amp;`. Values are plain text, escaped during rendering.
Use `{count}` for dynamic provider counts. Code, provider names, markup,
attributes used by scripts, flags, and URLs are outside the translation catalog.
Add new English UI text to every locale catalog when changing the landing page.

New locales must be added to `locales` in `scripts/lib/docs-i18n.mjs`, with a
complete pilot and `site.json`. Additional pages can be translated by mirroring
their path; the build discovers them automatically. The pilot list defines the
minimum required coverage, not the complete set of translatable pages.

## Check and Preview

```sh
node scripts/check-docs-i18n.mjs
node --test scripts/docs-i18n.test.mjs scripts/build-docs-site.test.js scripts/enhance-docs-site.test.mjs
node scripts/check-docs-links.mjs
node scripts/build-docs-site.mjs
node scripts/enhance-docs-site.mjs
node scripts/normalize-provider-counts.mjs
```

The check lists stale files and succeeds so English edits can ship immediately.
The build keeps the older translation and displays a notice linking the current
English page. Invalid metadata or changed code, links, heading IDs, or protected
terms in a current translation fail validation. `--strict` also fails on stale
files and is useful when preparing a translation refresh; publishing uses the
advisory default.

Serve `dist/docs-site` locally and inspect a translated landing page and guide
at desktop and narrow widths. Verify the language menu, English fallback, table
of contents, code copy buttons, text wrapping, and light/dark themes. No
translation service, credentials, or extra runtime dependency is required.
