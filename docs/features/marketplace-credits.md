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

The coordinator becomes the single source of truth for:

- marketplace feature status;
- customer-facing credit quotes;
- route candidate ranking;
- credit authorization and reservation, once the ledger exists;
- lease-to-credit reconciliation.

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
- `provider-default`: preserve configured provider order.

Later routing inputs can include:

- capacity hints and recent no-capacity failures;
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
    "enabled": true
  },
  "hetzner:beast": {
    "costHourlyUSD": 1,
    "markupBps": 1500
  }
}
```

## Product Decisions Still Required

Before real payment code lands, maintainers need explicit decisions for:

- payment processor and customer account model;
- whether credits are prepaid only, postpaid, or hybrid;
- ledger storage, backup, audit, and admin access;
- refund, failed-provisioning, expired-lease, and partial-use rules;
- tax, invoice, receipt, and chargeback ownership;
- provider cost variance and margin policy;
- whether marketplace routing may override explicit provider requests;
- customer-facing SLA and capacity failure messaging;
- privacy boundaries for provider account IDs and settlement records.

## Implementation Milestones

1. Preview skeleton: status and quote API, CLI visibility, docs, tests.
2. Ledger MVP: append-only transactions, balance reads, admin grants, no payment
   processor.
3. Payment MVP: customer checkout and webhooks with idempotent credit purchases.
4. Enforcement MVP: credit authorization before brokered lease provisioning.
5. Smart routing MVP: route selection from quote into lease creation.
6. Settlement reports: provider cost attribution and margin dashboards.
7. Delegated providers: extend the marketplace contract beyond coordinator-owned
   providers once external sandbox adapters expose comparable quote metadata.

## Related Docs

- [marketplace command](../commands/marketplace.md)
- [Cost and usage](cost-usage.md)
- [Capacity and fallback](capacity-fallback.md)
- [Providers](providers.md)
