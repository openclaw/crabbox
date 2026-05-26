/**
 * Crabbox public-surface route matrix
 * Phase 16 D.16 pilot 3 (W2.4)
 *
 * Crabbox is wrapped by Cloudflare Access (team_domain
 * `crabbox-openclaw.cloudflareaccess.com`, AUD pinned). Unauthenticated
 * traffic to most paths is intercepted by Access at the edge and returned
 * as a 302 to the Access login page. The Worker only sees requests that
 * either (a) bypass Access (rare; usually crons / internal) or (b) carry
 * a valid `cf-access-jwt-assertion`.
 *
 * For the public-surface audit we probe what the *edge* exposes to an
 * unauthenticated browser/script — that is what an external attacker
 * would see. Most routes are expected to return 302 (Access redirect)
 * or 401 from the Worker for the few paths that escape Access.
 *
 * Routes are split into three buckets:
 *   1. Unauthenticated reachable (smoke checks the Worker bypasses Access for)
 *   2. Access-gated (expected 302 to teamDomain or 401)
 *   3. Internal/admin (must NEVER be reachable; expected 401/403/404/302)
 *
 * Origin under audit (from wrangler.jsonc):
 *   - https://crabbox.openclaw.ai          (primary, Access-gated)
 *   - https://crabbox-access.openclaw.ai   (secondary, Access-gated)
 *   - https://crabbox-coordinator.<accounts>.workers.dev (workers.dev fallback)
 *
 * See ~/Projects/docs/system/D16_PILOT_CRABBOX_2026-05-27.md for findings.
 */

export default [
  // ---- Bucket 1: unauthenticated reachable surface ----
  // Worker handles GET /v1/health before any auth check.
  {
    label: 'health',
    kind: 'request',
    path: '/v1/health',
    method: 'GET',
    expect: [200, 302],
    bodyIncludes: [],
    // If Access is in front of the whole zone, /v1/health may also redirect.
    // Both 200 (Worker-handled) and 302 (Access-intercepted) are acceptable
    // signals here — both mean nothing leaks.
  },
  // Root redirects to /portal (Worker-handled before auth).
  {
    label: 'root-redirects-to-portal',
    kind: 'request',
    path: '/',
    method: 'GET',
    expect: [302],
    expectNot: [200, 500],
  },
  // Portal entrypoint — should land on Access login or portal login route.
  {
    label: 'portal-root',
    kind: 'request',
    path: '/portal',
    method: 'GET',
    expect: [200, 302],
    expectNot: [500],
  },
  // Portal login route is exempted from bearer auth (returns HTML form / Access redirect).
  {
    label: 'portal-login',
    kind: 'request',
    path: '/portal/login',
    method: 'GET',
    expect: [200, 302],
    expectNot: [500],
  },

  // ---- Bucket 2: Access-gated API surface ----
  // These should NOT return 200 to an unauthenticated request. Expected
  // posture: 302 to Access login OR 401 from the Worker (if Access permits
  // the path through unauthenticated, the Worker auth-gates it).
  {
    label: 'api-pool',
    kind: 'request',
    path: '/v1/pool',
    method: 'GET',
    expect: [302, 401, 403],
    expectNot: [200, 500],
  },
  {
    label: 'api-usage',
    kind: 'request',
    path: '/v1/usage',
    method: 'GET',
    expect: [302, 401, 403],
    expectNot: [200, 500],
  },
  {
    label: 'api-whoami',
    kind: 'request',
    path: '/v1/whoami',
    method: 'GET',
    expect: [302, 401, 403],
    expectNot: [200, 500],
  },
  {
    label: 'api-runs-list',
    kind: 'request',
    path: '/v1/runs',
    method: 'GET',
    expect: [302, 401, 403],
    expectNot: [200, 500],
  },
  {
    label: 'api-runners-list',
    kind: 'request',
    path: '/v1/runners',
    method: 'GET',
    expect: [302, 401, 403],
    expectNot: [200, 500],
  },
  {
    label: 'api-leases-list',
    kind: 'request',
    path: '/v1/leases',
    method: 'GET',
    expect: [302, 401, 403],
    expectNot: [200, 500],
  },

  // ---- Bucket 3: must-be-unreachable internal surface ----
  // The Worker explicitly 404s /v1/internal/* paths before auth. The
  // scheduled-maintenance endpoint should never be callable from outside.
  {
    label: 'internal-scheduled-must-404',
    kind: 'request',
    path: '/v1/internal/scheduled',
    method: 'POST',
    expect: [404, 302, 401, 403],
    expectNot: [200, 500],
  },
  {
    label: 'internal-foo-must-404',
    kind: 'request',
    path: '/v1/internal/whatever',
    method: 'GET',
    expect: [404, 302, 401, 403],
    expectNot: [200, 500],
  },

  // ---- Admin routes — must require admin token ----
  {
    label: 'admin-leases',
    kind: 'request',
    path: '/v1/admin/leases',
    method: 'GET',
    expect: [302, 401, 403],
    expectNot: [200, 500],
  },
  {
    label: 'admin-aws-identity',
    kind: 'request',
    path: '/v1/admin/aws-identity',
    method: 'GET',
    expect: [302, 401, 403],
    expectNot: [200, 500],
  },
  {
    label: 'admin-aws-orphan-sweep',
    kind: 'request',
    path: '/v1/admin/aws-orphan-sweep',
    method: 'GET',
    expect: [302, 401, 403],
    expectNot: [200, 500],
  },
];
