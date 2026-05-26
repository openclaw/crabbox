#!/usr/bin/env node
/**
 * Crabbox public-surface audit wrapper
 * Phase 16 D.16 pilot 3 (W2.4)
 *
 * Wires `@LDMB123/lj-shared-public-surface-audit` (vendor-fallback
 * import from `.shared/public-surface-audit/` until lj-shared publishes
 * to GitHub Packages — Phase 16 W1.8 Option B) against the crabbox
 * coordinator origin.
 *
 * Required env:
 *   CLOUDFLARE_ACCOUNT_ID
 *   CLOUDFLARE_API_TOKEN (or CLOUDFLARE_BROWSER_RENDERING_TOKEN)
 *
 * Optional env:
 *   CRABBOX_PRODUCTION_ORIGIN  — defaults to https://crabbox.openclaw.ai
 *   CRABBOX_CUTOVER_ID         — defaults to cut-<unix-seconds>
 *
 * Note: crabbox sits behind Cloudflare Access. The route matrix expects
 * 302 to Access login OR 401 from the Worker for gated routes; 200 only
 * for /v1/health and /portal entrypoints. See
 * ~/Projects/docs/system/D16_PILOT_CRABBOX_2026-05-27.md.
 */
import { runAudit } from '@LDMB123/lj-shared-public-surface-audit';
import routes from './public-surface-routes.mjs';

const origin = process.env.CRABBOX_PRODUCTION_ORIGIN ?? 'https://crabbox.openclaw.ai';
const cutoverId =
  process.env.CRABBOX_CUTOVER_ID ?? `cut-${Math.floor(Date.now() / 1000)}`;

const accountId = process.env.CLOUDFLARE_ACCOUNT_ID;
const apiToken =
  process.env.CLOUDFLARE_API_TOKEN ?? process.env.CLOUDFLARE_BROWSER_RENDERING_TOKEN;

if (!accountId || !apiToken) {
  console.error(
    'crabbox public-surface audit requires CLOUDFLARE_ACCOUNT_ID + ' +
      'CLOUDFLARE_API_TOKEN (or CLOUDFLARE_BROWSER_RENDERING_TOKEN). Skipping.',
  );
  process.exit(2);
}

const summary = await runAudit({
  routeMatrix: routes,
  origin,
  outputDir: 'output/public-surface-audit',
  cutoverId,
  accountId,
  apiToken,
  finalOriginHosts: ['crabbox.openclaw.ai', 'crabbox-access.openclaw.ai'],
});

if (summary.status !== 'pass') {
  process.exitCode = 1;
}
