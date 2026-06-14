import { describe, expect, it } from "vitest";

import { MarketplaceInputError, marketplaceQuote, marketplaceStatus } from "../src/marketplace";
import type { Env } from "../src/types";

describe("marketplace skeleton", () => {
  it("is disabled by default and keeps payment mutation features off", () => {
    const status = marketplaceStatus({} as Env);

    expect(status.enabled).toBe(false);
    expect(status.mode).toBe("disabled");
    expect(status.features).toMatchObject({
      quotes: false,
      payments: false,
      creditLedger: false,
      leaseEnforcement: false,
    });
    expect(status.supportedProviders).toContain("aws");
  });

  it("quotes provider candidates from the preview rate card", () => {
    const quote = marketplaceQuote(
      {
        CRABBOX_MARKETPLACE_ENABLED: "1",
        CRABBOX_MARKETPLACE_BIDDING_ENABLED: "1",
        CRABBOX_MARKETPLACE_ALLOWED_PROVIDERS: "aws,hetzner",
        CRABBOX_MARKETPLACE_RATE_CARD_JSON: JSON.stringify({
          "aws:beast": { costHourlyUSD: 2, retailHourlyUSD: 3 },
          "hetzner:beast": { costHourlyUSD: 1, retailHourlyUSD: 1.5 },
        }),
      } as Env,
      { provider: "auto", class: "beast", ttlSeconds: 7200, strategy: "cheapest" },
    );

    expect(quote.selected?.provider).toBe("hetzner");
    expect(quote.candidates).toHaveLength(2);
    expect(quote.candidates[0]).toMatchObject({
      provider: "hetzner",
      credits: 3,
      estimatedCostUSD: 2,
      marginUSD: 1,
      routeKey: "hetzner:linux:beast",
      available: true,
    });
  });

  it("marks candidates above the credit ceiling unavailable", () => {
    const quote = marketplaceQuote(
      {
        CRABBOX_MARKETPLACE_ENABLED: "1",
        CRABBOX_MARKETPLACE_ALLOWED_PROVIDERS: "aws",
        CRABBOX_MARKETPLACE_RATE_CARD_JSON: JSON.stringify({
          "aws:standard": 5,
        }),
      } as Env,
      { provider: "aws", class: "standard", ttlSeconds: 3600, maxCredits: 4 },
    );

    expect(quote.selected).toBeUndefined();
    expect(quote.candidates[0]).toMatchObject({
      available: false,
      unavailableReason: "above_max_credits",
    });
    expect(quote.warnings).toContain("no candidate fits the requested credit ceiling");
  });

  it("uses rate-card priority before price strategy", () => {
    const quote = marketplaceQuote(
      {
        CRABBOX_MARKETPLACE_ENABLED: "1",
        CRABBOX_MARKETPLACE_ALLOWED_PROVIDERS: "aws,hetzner",
        CRABBOX_MARKETPLACE_RATE_CARD_JSON: JSON.stringify({
          "aws:beast": { costHourlyUSD: 2, retailHourlyUSD: 4, priority: 20, weight: 3 },
          "hetzner:beast": { costHourlyUSD: 1, retailHourlyUSD: 2, priority: 10, weight: 1 },
        }),
      } as Env,
      { provider: "auto", class: "beast", ttlSeconds: 3600, strategy: "cheapest" },
    );

    expect(quote.selected).toMatchObject({
      provider: "aws",
      priority: 20,
      weight: 3,
      credits: 4,
    });
  });

  it("load-balances same-priority candidates by weight and previews the route share", () => {
    const quote = marketplaceQuote(
      {
        CRABBOX_MARKETPLACE_ENABLED: "1",
        CRABBOX_MARKETPLACE_ALLOWED_PROVIDERS: "aws,hetzner",
        CRABBOX_MARKETPLACE_RATE_CARD_JSON: JSON.stringify({
          "aws:beast": { costHourlyUSD: 2, retailHourlyUSD: 4, priority: 10, weight: 3 },
          "hetzner:beast": { costHourlyUSD: 1, retailHourlyUSD: 2, priority: 10, weight: 1 },
        }),
      } as Env,
      { provider: "auto", class: "beast", ttlSeconds: 3600, strategy: "weighted" },
    );

    // heavier weight wins selection within the shared priority tier (even though it is pricier)
    expect(quote.strategy).toBe("weighted");
    expect(quote.selected?.provider).toBe("aws");
    expect(quote.candidates.map((candidate) => candidate.provider)).toEqual(["aws", "hetzner"]);

    // route shares split traffic by weight (3:1) and sum to 1 across the tier
    const shares = Object.fromEntries(
      quote.candidates.map((candidate) => [candidate.provider, candidate.routeShare]),
    );
    expect(shares.aws).toBeCloseTo(0.75, 5);
    expect(shares.hetzner).toBeCloseTo(0.25, 5);
    expect((shares.aws ?? 0) + (shares.hetzner ?? 0)).toBeCloseTo(1, 5);
    expect(quote.warnings).toContain(
      "weighted strategy previews the load-balancing split across 2 same-priority candidates; no traffic is routed in preview mode",
    );
  });

  it("keeps higher priority ahead of weight under the weighted strategy", () => {
    const quote = marketplaceQuote(
      {
        CRABBOX_MARKETPLACE_ENABLED: "1",
        CRABBOX_MARKETPLACE_ALLOWED_PROVIDERS: "aws,hetzner",
        CRABBOX_MARKETPLACE_RATE_CARD_JSON: JSON.stringify({
          "aws:beast": { costHourlyUSD: 2, retailHourlyUSD: 4, priority: 5, weight: 9 },
          "hetzner:beast": { costHourlyUSD: 1, retailHourlyUSD: 2, priority: 10, weight: 1 },
        }),
      } as Env,
      { provider: "auto", class: "beast", ttlSeconds: 3600, strategy: "weighted" },
    );

    // priority is the failover tier and dominates weight; the lone top-tier route gets the full share
    expect(quote.selected?.provider).toBe("hetzner");
    expect(quote.selected?.routeShare).toBeCloseTo(1, 5);
    // the lower-priority candidate is outside the winning tier and carries no share
    const aws = quote.candidates.find((candidate) => candidate.provider === "aws");
    expect(aws?.routeShare).toBeUndefined();
  });

  it("does not attach route shares for non-weighted strategies", () => {
    const quote = marketplaceQuote(
      {
        CRABBOX_MARKETPLACE_ENABLED: "1",
        CRABBOX_MARKETPLACE_ALLOWED_PROVIDERS: "aws,hetzner",
        CRABBOX_MARKETPLACE_RATE_CARD_JSON: JSON.stringify({
          "aws:beast": { costHourlyUSD: 2, retailHourlyUSD: 4, priority: 10, weight: 3 },
          "hetzner:beast": { costHourlyUSD: 1, retailHourlyUSD: 2, priority: 10, weight: 1 },
        }),
      } as Env,
      { provider: "auto", class: "beast", ttlSeconds: 3600, strategy: "cheapest" },
    );

    expect(quote.candidates.every((candidate) => candidate.routeShare === undefined)).toBe(true);
  });

  it("rejects quotes while preview mode is disabled", () => {
    expect(() => marketplaceQuote({} as Env, { provider: "aws" })).toThrow(MarketplaceInputError);
  });

  it("rejects unknown routing strategies", () => {
    expect(() =>
      marketplaceQuote({ CRABBOX_MARKETPLACE_ENABLED: "1" } as Env, {
        provider: "aws",
        strategy: "fastest" as never,
      }),
    ).toThrow(MarketplaceInputError);
  });
});
