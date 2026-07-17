# Marketplace Credits Gateway

Read when:

- designing a payment or credit layer for brokered leases;
- changing marketplace quote, routing, or credit enforcement behavior;
- deciding whether a provider can participate in Crabbox-managed billing.

This feature is a skeleton for an OpenRouter-like gateway for sandbox capacity.
The user puts payment credentials in one place, buys or receives Crabbox credits,
and asks Crabbox for a sandbox by intent. Crabbox then chooses among compatible
providers using broker-owned credentials, prices, capacity hints, policy, and
the user's credit balance.

The current implementation is preview-only:

```text
implemented: status API, quote API, CLI status, CLI quote, docs, tests
not implemented: payment capture, durable credit ledger, credit reservation, lease enforcement, provider settlement
```

## Product Boundary

Crabbox should be the customer-facing gateway:

- Customer identity, credit balance, invoices, refunds, and payment methods
  belong to the Crabbox billing layer.
- Provider credentials, live prices, and settlement records remain behind the
  coordinator and provider adapters.
- Provider adapters should not know about customer payment instruments.
- Direct-provider mode remains outside Crabbox billing unless a user explicitly
  routes through a coordinator.

The billing operator should be configurable. The same marketplace contract needs
to work for an OpenClaw-hosted gateway, a neutral Crabbox-hosted gateway such as
`crabbox.sh`, and self-hosted coordinators that only want BYOK routing and spend
policy. Product names, checkout URLs, tax settings, support contact, and legal
entity data should be deployment configuration, not provider-adapter behavior.

The coordinator becomes the single source of truth for:

- marketplace feature status;
- customer-facing credit quotes;
- route candidate ranking;
- gateway API keys or scoped service tokens;
- credit authorization and reservation, once the ledger exists;
- lease-to-credit reconciliation.

## Gateway Patterns To Copy

AI gateways that already operate across multiple upstream providers point to
several product requirements that should exist before Crabbox credits become
mandatory:

- One gateway credential should access many upstream providers, while upstream
  provider credentials stay broker-owned or BYOK-managed.
- Spend limits need multiple scopes: fleet, org or project, user, and API key.
- BYOK and managed-provider modes should be explicit. BYOK customers bring
  provider credentials and use Crabbox for routing, observability, and policy;
  managed-provider customers pay Crabbox and consume credits.
- Routing groups should be named policy objects with active members, priority
  for failover, and weight for load balancing among equivalent providers.
- Every credit-enforced request needs observability records: quote ID, route
  group, selected provider, fallback attempts, estimated credits, captured
  credits, and provider cost attribution.
- Credit enforcement must require pricing data. If the broker cannot price a
  route, it should reject credit-enforced requests before provisioning rather
  than silently creating untracked spend.

## Payment Operator Model

Separate the technical gateway from the commercial operator:

```text
gateway tenant        example domain       payment mode
openclaw-hosted       openclaw.ai          OpenClaw merchant of record or reseller
crabbox-hosted        crabbox.sh           Crabbox merchant of record or reseller
self-hosted           customer domain      BYOK, internal chargeback, or disabled payments
```

The coordinator should expose a deployment-level billing profile:

```text
CRABBOX_MARKETPLACE_BRAND_NAME
CRABBOX_MARKETPLACE_PUBLIC_URL
CRABBOX_MARKETPLACE_SUPPORT_EMAIL
CRABBOX_MARKETPLACE_LEGAL_ENTITY
CRABBOX_MARKETPLACE_TERMS_URL
CRABBOX_MARKETPLACE_PRIVACY_URL
CRABBOX_MARKETPLACE_PAYMENT_PROVIDER
CRABBOX_MARKETPLACE_LEDGER_PROVIDER
```

That profile lets a hosted OpenClaw deployment and a hosted Crabbox deployment
share the same APIs while keeping invoices, support, terms, and settlement under
the right operator. Payment webhooks should include the tenant/operator ID in
their idempotency keys so credits cannot cross domains accidentally.

Payment modes:

- `managed`: customer pays the gateway operator; Crabbox credits are consumed
  across managed providers.
- `byok`: customer supplies provider credentials; Crabbox still provides routing
  policy, observability, and optional internal spend caps.
- `hybrid`: managed providers consume credits, BYOK providers record usage
  without charging credits unless the operator configures internal chargeback.
- `disabled`: quotes and usage only.

Do not make OpenClaw-specific names part of the protocol. Use neutral field
names such as `billingProfile`, `tenant`, `owner`, `org`, `checkoutURL`, and
`ledgerAccountID`.

## APIs

Status:

```text
GET /v1/marketplace/status
```

Response includes:

- `enabled`: whether preview mode is on;
- `supportedProviders`: brokered providers eligible for marketplace quotes;
- `features`: quotes, bidding, payments, ledger, and lease enforcement flags;
- `settlement`: configured payment and ledger providers;
- `decisionsRequired`: product decisions still blocking real billing.

Quote:

```text
POST /v1/marketplace/quotes
```

Request:

```json
{
  "provider": "auto",
  "providers": ["aws", "hetzner"],
  "class": "beast",
  "serverType": "c7a.48xlarge",
  "target": "linux",
  "ttlSeconds": 3600,
  "maxCredits": 5,
  "strategy": "cheapest"
}
```

Response:

```json
{
  "quote": {
    "id": "mq_...",
    "mode": "preview",
    "currency": "USD",
    "creditUnit": "usd",
    "strategy": "cheapest",
    "ttlSeconds": 3600,
    "selected": {
      "provider": "hetzner",
      "routeKey": "hetzner:linux:beast",
      "credits": 1.5
    },
    "candidates": [],
    "warnings": []
  }
}
```

Preview quotes are advisory. They do not reserve credits and they do not
provision a lease.

## Smart Routing

The first route policy should be intentionally small:

```text
input: provider intent, class or server type, target OS, TTL, optional max credits
output: ranked compatible candidates and one selected candidate
```

Initial strategies:

- `cheapest`: lowest retail credits to the customer.
- `balanced`: prefer routes with enough margin while still minimizing credits.
- `weighted`: within the winning priority tier, load-balance by routing-group
  `weight` and surface a per-candidate `routeShare` (0..1, summing to 1 across the
  tier) plus a `routingPlan` failover ladder previewing the split. Selection stays
  deterministic (heaviest available route); the shares are a display-only projection.
- `provider-default`: preserve configured provider order.

Later routing inputs can include:

- capacity hints and recent no-capacity failures;
- routing-group member priority, weight, and active flags;
- warm pool availability;
- user/org policy and provider allowlists;
- historical reliability;
- latency or region preference;
- target-specific support, such as Windows, desktop, browser, code, or GPU.

Routing must stay provider-neutral. Provider-specific capacity, API errors,
quota, and cleanup semantics stay behind provider adapters and existing
capacity-fallback code.

## Credit Ledger Model

The durable ledger should be append-only. Do not store only a mutable balance.

Suggested transaction types:

```text
credit_purchase      customer payment created credits
credit_grant         operator/promotional credits
credit_authorize     hold before provisioning
credit_capture       final charge after lease starts
credit_release       unused hold returned
credit_refund        refund or support adjustment
provider_cost        internal provider cost attribution
tax_adjustment       tax or jurisdiction adjustment
manual_adjustment    operator correction with reason
```

Each transaction should include:

- transaction ID and idempotency key;
- owner and org;
- amount and currency or credit unit;
- source, such as Stripe payment intent, admin grant, or lease ID;
- related quote ID and lease ID when applicable;
- created timestamp;
- actor and reason for admin changes.

## Lease Enforcement Flow

Real enforcement should happen in phases:

1. Quote: rank candidates and show estimated credits.
2. Authorize: hold worst-case credits before cloud provisioning starts.
3. Provision: create the lease using the selected provider route.
4. Capture or adjust: capture elapsed or reserved credits when the lease starts,
   stops, expires, or fails.
5. Reconcile: compare captured credits with provider cost and emit internal
   settlement records.

The first enforceable version should reject lease creation only when all of
these are true:

- `CRABBOX_MARKETPLACE_ENABLED=1`;
- `CRABBOX_MARKETPLACE_REQUIRE_CREDITS=1`;
- the request routes through brokered provider mode;
- the owner/org has a real ledger account;
- authorization succeeds with an idempotency key.

If authorization fails after a provider resource was created, cleanup must run
before returning the error.

## Configuration

Preview environment variables:

```text
CRABBOX_MARKETPLACE_ENABLED
CRABBOX_MARKETPLACE_ALLOWED_PROVIDERS
CRABBOX_MARKETPLACE_RATE_CARD_JSON
CRABBOX_MARKETPLACE_MARKUP_BPS
CRABBOX_MARKETPLACE_BIDDING_ENABLED
CRABBOX_MARKETPLACE_REQUIRE_CREDITS
CRABBOX_MARKETPLACE_PAYMENT_PROVIDER
CRABBOX_MARKETPLACE_LEDGER_PROVIDER
```

`CRABBOX_MARKETPLACE_ALLOWED_PROVIDERS` defaults to all coordinator providers.
The rate card accepts `provider:type`, `provider:class`, `provider:*`,
`*:type`, `*:class`, or `*` keys.

Example:

```json
{
  "aws:beast": {
    "costHourlyUSD": 2,
    "retailHourlyUSD": 3,
    "priority": 20,
    "weight": 1,
    "enabled": true
  },
  "hetzner:beast": {
    "costHourlyUSD": 1,
    "markupBps": 1500,
    "priority": 10,
    "weight": 2
  }
}
```

Higher `priority` values are ranked first. `weight` drives the `weighted`
strategy: the broker load-balances across providers sharing the winning priority
and returns a per-candidate `routeShare` (0..1) previewing the traffic split,
plus a `routingPlan` array describing the whole failover ladder — one entry per
priority tier (highest first), each with its `members` (provider, routeKey,
weight, routeShare) and an `active` flag on the tier that serves the selected
candidate. Per-tier `routeShare` values sum to exactly 1. The plan is a preview
projection of priority failover plus weighted load balancing; it routes no
traffic and moves no credits.

## Product Decisions Still Required

Before real payment code lands, maintainers need explicit decisions for:

- payment processor and customer account model;
- OpenClaw-hosted versus Crabbox-hosted merchant/support/legal ownership;
- whether credits are prepaid only, postpaid, or hybrid;
- ledger storage, backup, audit, and admin access;
- refund, failed-provisioning, expired-lease, and partial-use rules;
- tax, invoice, receipt, and chargeback ownership;
- provider cost variance and margin policy;
- whether unknown pricing data rejects credit-enforced leases;
- gateway API key scopes and spend caps per key;
- BYOK versus managed-provider account boundaries;
- routing group priority, weight, and active-member semantics;
- whether marketplace routing may override explicit provider requests;
- customer-facing SLA and capacity failure messaging;
- privacy boundaries for provider account IDs and settlement records.

## Implementation Milestones

1. Preview skeleton: status and quote API, CLI visibility, docs, tests.
2. Ledger MVP: append-only transactions, balance reads, admin grants, no payment
   processor.
3. Payment MVP: customer checkout and webhooks with idempotent credit purchases.
4. Enforcement MVP: credit authorization before brokered lease provisioning.
5. Routing groups MVP: active members, priority failover, weighted same-priority
   load balancing (preview `routeShare` split and the ordered `routingPlan`
   failover ladder shipped; live routing and route audit events still pending).
6. Smart routing MVP: route selection from quote into lease creation.
7. Settlement reports: provider cost attribution and margin dashboards.
8. Delegated providers: extend the marketplace contract beyond coordinator-owned
   providers once external sandbox adapters expose comparable quote metadata.

## Related Docs

- [marketplace command](../commands/marketplace.md)
- [Cost and usage](cost-usage.md)
- [Capacity and fallback](capacity-fallback.md)
- [Provider Reference](../providers/README.md)
