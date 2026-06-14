# marketplace

`crabbox marketplace` previews the Crabbox credits gateway: one Crabbox billing
relationship, one credit balance, and smart routing across brokered sandbox
providers.

This is a skeleton command surface. It can show broker status and compute
preview quotes, but it does not capture payment, mutate a credit ledger, reserve
credits, or enforce credits during lease creation.

```sh
crabbox marketplace status
crabbox marketplace status --json
crabbox marketplace quote --provider auto --class beast --ttl 1h
crabbox marketplace quote --providers aws,hetzner --class beast --max-credits 5
crabbox marketplace quote --provider aws --class standard --ttl 30m --json
```

## Subcommands

```text
status    show gateway mode, supported providers, enabled features, and open product decisions
quote     preview provider candidates, credit price, provider cost, margin, and selected route
```

`marketplace` requires a configured coordinator. Direct-provider mode has no
central customer identity, credit balance, or provider routing policy to query.

## Quote Flags

```text
--provider <name|auto>       single provider or auto smart routing (default auto)
--providers <a,b>            explicit provider candidate list
--class <name>               machine class or marketplace SKU (default standard)
--type <name>                provider-native server type or marketplace SKU
--target <os>                target OS (default linux)
--ttl <duration>             quote duration, e.g. 30m, 1h, or 3600s
--max-credits <amount>       mark routes above this credit ceiling unavailable
--strategy <name>            cheapest, balanced, weighted, or provider-default
--json                       print raw broker response
```

## Gateway Model

The intended product shape is OpenRouter-like for sandbox capacity:

- Customers put a credit card in one place: Crabbox.
- Crabbox exposes credits as the customer-facing unit, currently denominated as
  USD-equivalent preview credits.
- Crabbox keeps provider credentials and settlement behind the broker, not in
  each user checkout.
- Users request intent such as `provider=auto`, `class=beast`, `target=linux`,
  `ttl=1h`, and optionally a credit ceiling.
- The broker ranks compatible provider candidates by routing policy such as
  cheapest, balanced margin/cost, weighted same-priority load balancing, or
  provider-default order.
- Direct-provider mode remains available and is not forced through payment.

The current skeleton intentionally stops before real money movement:

```text
quote       implemented preview API
bidding     preview ranking only
payment     not implemented
ledger      not implemented
enforcement not wired into lease creation
settlement  external/product decision
```

## Worker Configuration

```text
CRABBOX_MARKETPLACE_ENABLED=1
CRABBOX_MARKETPLACE_ALLOWED_PROVIDERS=aws,azure,gcp,hetzner
CRABBOX_MARKETPLACE_BIDDING_ENABLED=1
CRABBOX_MARKETPLACE_MARKUP_BPS=1500
CRABBOX_MARKETPLACE_RATE_CARD_JSON='{"aws:beast":{"costHourlyUSD":2,"retailHourlyUSD":3}}'
CRABBOX_MARKETPLACE_REQUIRE_CREDITS=0
CRABBOX_MARKETPLACE_PAYMENT_PROVIDER=none
CRABBOX_MARKETPLACE_LEDGER_PROVIDER=none
```

`CRABBOX_MARKETPLACE_RATE_CARD_JSON` accepts keys such as
`aws:c7a.48xlarge`, `aws:beast`, `aws:*`, `*:beast`, or `*`. Values can be a
number, interpreted as retail hourly credits, or an object:

```json
{
  "aws:beast": {
    "costHourlyUSD": 2,
    "retailHourlyUSD": 3,
    "markupBps": 1500,
    "priority": 20,
    "weight": 1,
    "enabled": true
  }
}
```

`priority` and `weight` are routing-policy fields. Higher-priority candidates
are ranked first for failover-style routing. `weight` drives the `weighted`
strategy: within a single priority tier the broker load-balances by weight and
returns a `routeShare` (0..1) per candidate previewing how traffic would split
(e.g. weights `3` and `1` preview `share=75%` and `share=25%`). Per-tier
`routeShare` values sum to exactly 1; the `share=NN%` the CLI prints is the
rounded display and always totals 100% within a tier.

Selection is deterministic, not probabilistic: the quote always selects the
heaviest available candidate in the winning tier (ties broken by cheapest, then
provider name). `routeShare` is a display-only projection of how traffic *would*
split; it does not change which single candidate is selected.

### Routing plan

Under the `weighted` strategy the quote also returns a `routingPlan`: the
failover ladder as an ordered array of priority tiers (highest priority first),
each with its available `members` and their weighted `routeShare`, and an
`active` flag on the single tier that contains the selected candidate. The CLI
renders it as:

```text
routing plan (failover order; preview only, no traffic routed):
  tier priority=10 [active  ] aws 75% | hetzner 25%
  tier priority=5  [failover] gcp 100%
```

It is a preview projection of priority failover plus weighted load balancing:
no tier is locked, no traffic is routed, and no credits move. Non-weighted
strategies omit `routingPlan`.

## Related Docs

- [Marketplace credits gateway](../features/marketplace-credits.md)
- [Cost and usage](../features/cost-usage.md)
- [Capacity and fallback](../features/capacity-fallback.md)
- [Providers](providers.md)
