# OpenClaw config compatibility fixtures

Snapshots of `openclaw.json` known to load on a baseline OpenClaw version and
known to trip `openclaw doctor`'s legacy-config rules on a later version. Each
fixture is consumed by `scripts/openclaw-config-compat.sh`:

```sh
CRABBOX_LIVE=1 \
  OPENCLAW_VERSION_X=2026.4.27 \
  OPENCLAW_VERSION_Y=2026.5.18 \
  OPENCLAW_FIXTURE=scripts/fixtures/openclaw/2026.4.27/openclaw.json \
  scripts/openclaw-config-compat.sh
```

The five fixtures span the five top-level legacy-config-migration buckets
OpenClaw groups itself by, so the suite covers each category of breakage
the project's own doctor knows how to repair:

| # | Bucket     | Fixture                              | Suggested X | Suggested Y |
|---|------------|--------------------------------------|-------------|-------------|
| 1 | queue      | `2026.4.27/openclaw.json`            | `2026.4.27` | `2026.5.18` |
| 2 | web-search | `2026.5.12/openclaw.json`            | `2026.5.12` | `2026.5.18` |
| 3 | audio      | `2026.4.25/openclaw.json`            | `2026.4.25` | `2026.5.18` |
| 4 | channels   | `2026.5.3/openclaw.json`             | `2026.5.3`  | `2026.5.18` |
| 5 | runtime    | `2026.4.1/openclaw.json` (mcp)       | `2026.4.1`  | `2026.5.18` |

Strength of the X→Y boundary varies. Buckets 1 and 2 line up with a
changelog-cited migration landing inside the npm-published version range, so
the baseline really did accept the legacy shape natively. Buckets 3, 4, and 5
document the legacy shape exactly as OpenClaw's own migration tests do, but
the rename predates the earliest stable on npm; if phase 1 fails for those,
the fixture is still valuable as documentation of the deprecated shape that
doctor handles today. The script exits 2 in that case, with a clear message
distinguishing fixture trouble from a real Y-regression.

All five legacy shapes are lifted from OpenClaw's own migration tests, so the
JSON matches verbatim what `migrateLegacyConfigForTest` exercises in CI.

## 1. `messages.queue.mode = "queue"` (queue migration)

- Migration rule: `src/commands/doctor/shared/legacy-config-migrations.queue.ts:8-49`
- Test that proves doctor rewrites it: `src/commands/doctor/shared/legacy-config-migrate.test.ts:336-365`
- Changelog evidence: `2026.4.29` made `steer` the default and demoted `"queue"`
  to a retired mode. By `2026.5.x`, doctor maps `"queue"` → `"steer"` and
  `"steer-backlog"` → `"followup"`.
- Expected doctor output: `Moved deprecated messages.queue.mode "queue" → "steer"`.

## 2. `tools.web.search.<provider>` (web-search migration)

- Migration rule: `src/commands/doctor/shared/legacy-config-migrations.web-search.ts:11-19`
- Test that proves doctor rewrites it: `src/commands/doctor/shared/legacy-web-search-migrate.test.ts:20-62`
- Changelog evidence: `2026.5.14` introduced the canonical doctor migration
  from `tools.web.search.<provider>` to
  `plugins.entries.<plugin>.config.webSearch`. `2026.5.12` is the last stable
  release on npm that still treats the legacy shape as canonical.
- Expected doctor output: `Moved tools.web.search.grok → plugins.entries.xai.config.webSearch`, etc.

## 3. `audio.transcription` (audio migration)

- Migration rule: `src/commands/doctor/shared/legacy-config-migrations.audio.ts:36-60`
- Test that proves doctor rewrites it: `src/commands/doctor/shared/legacy-config-migrate.test.ts:417-437`
- Changelog evidence: `2026.4.26` adds the `{input}` → `{{MediaPath}}` placeholder
  rewrite inside this migration. The whole `audio.transcription` shape predates
  that release and now migrates to `tools.media.audio.models`.
- Expected doctor output: `Moved audio.transcription → tools.media.audio.models`.

## 4. `routing.allowFrom` and `routing.groupChat.*` (channels migration)

- Migration rule: `src/commands/doctor/shared/legacy-config-migrations.channels.ts:384-410`
- Test that proves the schema rejects these shapes today: `src/config/config.legacy-config-detection.rejects-routing-allowfrom.test.ts:6-17`
- Changelog evidence: `2026.5.4` restored the doctor migration "so upgrades
  keep WhatsApp, Telegram, and iMessage group mention gates and history
  settings instead of leaving configs invalid or silently blocked."
- Expected doctor output: `Moved routing.allowFrom → channels.whatsapp.allowFrom`,
  `Moved routing.groupChat.historyLimit → messages.groupChat.historyLimit`, etc.

## 5. `mcp.servers.<name>.type` (runtime migration)

- Migration rule: `src/commands/doctor/shared/legacy-config-migrations.runtime.mcp.ts:12-19`
- Migration logic: same file, lines 21-52
- Alias map: `src/config/mcp-config-normalize.ts:6-11` maps `http` →
  `streamable-http`, accepts `sse` and `stdio` as-is.
- Expected doctor output: `Moved mcp.servers.example-http.type "http" → transport "streamable-http"`.

## Adding a fixture

1. Find the deprecated shape in `../openclaw/src/commands/doctor/shared/legacy-config-migrations.*.ts`.
2. Copy the input from the matching test in `legacy-config-migrate.test.ts` (or
   the per-bucket `legacy-*-migrate.test.ts`) verbatim — same field names, same
   sample values — so the fixture is grounded in OpenClaw's own contract.
3. Place it at `scripts/fixtures/openclaw/<baseline-version>/openclaw.json`.
4. Add a row to the table above with a citation to the migration rule and
   the matching test.
