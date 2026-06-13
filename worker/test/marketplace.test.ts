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
