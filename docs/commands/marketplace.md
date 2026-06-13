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
--strategy <name>            cheapest, balanced, or provider-default
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
  cheapest, balanced margin/cost, or provider-default order.
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
    "enabled": true
  }
}
```

## Related Docs

- [Marketplace credits gateway](../features/marketplace-credits.md)
- [Cost and usage](../features/cost-usage.md)
- [Capacity and fallback](../features/capacity-fallback.md)
- [Providers](providers.md)
