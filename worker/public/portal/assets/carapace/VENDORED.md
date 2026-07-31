# Vendored Carapace assets

Source: https://github.com/openclaw/carapace
Tag: `v0.6.1`
Commit: `3a8bcfbc7f0563f501626d37245f31efc0d7be9f`

## Files

- `carapace-core.css` — concatenation, in order, of:
  1. `styles/tokens.css`
  2. `styles/themes.css`
  3. `styles/typography.css`
  4. `styles/themes/product.css`
  5. `styles/components.css` (oc-action buttons, oc-card, oc-segmented)
  6. `styles/candidate/feedback.css` (oc-badge, oc-banner, oc-empty, oc-loader)
  7. `styles/candidate/data.css` (oc-table, oc-sparkline, oc-bars, oc-delta, oc-split)

## Regenerating

From a carapace checkout at the pinned tag:

```sh
{ printf '/*! Carapace core bundle (vendored) ... */\n';
  git show vX.Y.Z:styles/tokens.css;
  git show vX.Y.Z:styles/themes.css;
  git show vX.Y.Z:styles/typography.css;
  git show vX.Y.Z:styles/themes/product.css;
  git show vX.Y.Z:styles/components.css;
  git show vX.Y.Z:styles/candidate/feedback.css;
  git show vX.Y.Z:styles/candidate/data.css; } > carapace-core.css
```

Update the tag/commit above and in the file header when upgrading. Upgrades
replace file contents in place; the asset is served same-origin by the Worker
assets binding, so caching is handled by asset etags — no hashed filenames.

Theme contract: the portal sets `data-theme="dark"|"light"` on `<html>`,
matching carapace's canonical selectors. Font binaries are intentionally not
vendored; `--oc-font-body`/`--oc-font-mono` fall back to system stacks.
