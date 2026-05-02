# MPP payments (Tempo)

The broker can gate `POST /v1/leases` behind a [Machine Payments Protocol]
402 challenge. When enabled, an agent that is not authenticated against
the broker's existing GitHub/admin paths can lease a runner by signing a
Tempo payment for the estimated maximum lease cost.

The payment covers the full TTL upfront (allowance model). Heartbeats,
runs, and release requests do **not** trigger additional 402s. Releases
do not refund.

[Machine Payments Protocol]: https://mpp.dev

## Enable

Set the following Worker secrets / vars:

```text
CRABBOX_MPP_RECIPIENT      0x... wallet that receives charges (required)
CRABBOX_MPP_CURRENCY       0x... TIP-20 token contract (default: pathUSD)
CRABBOX_MPP_DECIMALS       integer 0-32 (default: 6)
CRABBOX_MPP_SECRET_KEY     HMAC secret binding challenges to their contents
CRABBOX_MPP_TESTNET        "1" / "true" to use Tempo testnet
CRABBOX_MPP_REALM          override the auto-detected realm (see below)
```

If `CRABBOX_MPP_RECIPIENT` is unset, the lease endpoint behaves exactly
as before — no 402 is emitted and existing GitHub/admin auth applies.

## Wire format

The challenge follows the standard MPP HTTP 402 wire format:

```
HTTP/1.1 402 Payment Required
WWW-Authenticate: Payment realm="...", method="tempo", intent="charge", ...
```

The agent signs a payment credential (via `mppx`, `mppx/client`, or any
MPP-compliant client) and retries with:

```
Authorization: Payment <credential>
```

On a successful retry, the broker provisions the runner and attaches an
MPP receipt to the `201 Created` response.

## Amount

The amount charged equals `cost.maxUSD` for the requested
`{provider, serverType, ttlSeconds}` triple, computed using the same
rate table as the existing `/v1/usage` endpoint and the provider's
hourly price API. AWS spot prices are looked up live; Hetzner prices
are fixed-list.

## Realm pinning for `wrangler dev`

`wrangler dev` rewrites the `realm` field of `WWW-Authenticate` headers on the
wire to match the request's `Host` header. mppx auto-detects realm from
`URL.hostname` (no port), so the issued HMAC binds to `"localhost"` while the
client signs against `"localhost:8787"` — verification then fails with
`"challenge was not issued by this server."` Set `CRABBOX_MPP_REALM=localhost:8787`
in `.dev.vars` to pre-pin the realm to what wrangler will emit. In production
the rewrite does not occur, so leave `CRABBOX_MPP_REALM` unset and let mppx
auto-detect from the public hostname.

## Caveats

- Releases are no-refund. A bounded TTL keeps the maximum loss small.
- Cost is an upper bound — the agent may be billed for the full TTL even
  if the lease is released or expires idle earlier.
- The Worker's existing cost-limit guards (`CRABBOX_MAX_*`) apply
  per-payer wallet (the credential's `source` address is the owner), not
  per-broker-recipient.
- Charge settles before provisioning. If Hetzner/AWS provisioning fails
  after a successful charge, the funds have already moved; the broker
  returns a 500 with no lease and no automated refund. To minimize the
  window, MPP-paid leases skip the provider's serverType-fallback list:
  the requested type is tried once and either succeeds or fails. A future
  PR should add a paid-failed-provision reconciliation table backed by
  DO storage so operators can refund out-of-band.
- mppx replay protection is backed by Durable Object storage so
  consumed transaction-hash markers survive DO eviction/rehydration.

## CLI client behaviour

The `crabbox` Go CLI does not sign Tempo payments natively. Instead, when:

1. `CRABBOX_MPP_PAY=auto` is set in the environment, AND
2. the broker returns `402 Payment Required` for `POST /v1/leases`, AND
3. the `mppx` binary is available on `PATH`,

the CLI shells out to `mppx` to handle the 402 → sign → retry cycle, using
whichever account `MPPX_ACCOUNT` (or `MPPX_PRIVATE_KEY`) selects. Without
the opt-in env var or without `mppx` installed, the original 402 error is
returned unchanged.

Successful MPP-paid lease creations include a `bearer` field in the response
alongside the lease record. That `cbxl_…` token is HMAC-bound to the lease
ID, owner, and TTL, and is the credential to use for `crabbox heartbeat`,
`stop`, and `runs` against that lease without re-paying.

Related docs:

- [MPP E2E recipe](./payments-mpp-e2e.md)
- [Cost and usage](./cost-usage.md)
- [Coordinator](./coordinator.md)
- [Broker auth and routing](./broker-auth-routing.md)
