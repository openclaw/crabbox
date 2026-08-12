import { describe, expect, it } from "vitest";

import {
  InvalidLeaseSlugError,
  leaseProviderName,
  leaseSlugFromID,
  maxRequestedLeaseSlugLength,
  normalizeLeaseSlug,
  requestedLeaseSlug,
  slugWithCollisionSuffix,
} from "../src/slug";

describe("lease slugs", () => {
  it("generates deterministic DNS-ish slugs", () => {
    const slug = leaseSlugFromID("cbx_abcdef123456");
    expect(leaseSlugFromID("cbx_abcdef123456")).toBe(slug);
    expect(slug).toMatch(/^[a-z0-9]+-[a-z0-9]+$/);
    expect(leaseProviderName("cbx_abcdef123456", slug).length).toBeLessThanOrEqual(63);
  });

  it("matches Go golden fixtures", () => {
    expect(leaseSlugFromID("cbx_000000000001")).toBe("tidal-lobster");
    expect(leaseSlugFromID("cbx_abcdef123456")).toBe("blue-prawn");
    expect(leaseSlugFromID("cbx_deadbeefcafe")).toBe("silver-crab");
  });

  it("normalizes requested slugs and appends collision suffixes", () => {
    expect(normalizeLeaseSlug(" Blue Lobster ")).toBe("blue-lobster");
    expect(normalizeLeaseSlug(" --- Blue__Lobster!! ")).toBe("blue-lobster");
    expect(slugWithCollisionSuffix("Blue Lobster", "cbx_abcdef123456")).toMatch(
      /^blue-lobster-[a-f0-9]{4}$/,
    );
  });

  it("uses slug for provider names while preserving ID fallback", () => {
    expect(leaseProviderName("cbx_abcdef123456", "blue-lobster")).toBe(
      "crabbox-blue-lobster-c80c2195",
    );
    expect(leaseProviderName("cbx_abcdef123456", "")).toBe("crabbox-cbx-abcdef123456");
  });

  it("accepts a requested slug up to the provider-name bound", () => {
    const longest = "a".repeat(maxRequestedLeaseSlugLength);
    expect(requestedLeaseSlug(longest)).toBe(longest);
    expect(requestedLeaseSlug(undefined)).toBe("");
    // 8 ("crabbox-") + 41 + 1 + 8 (lease hash); the 5-char collision suffix fills it to 63.
    expect(leaseProviderName("cbx_abcdef123456", longest).length).toBe(58);
    expect(
      leaseProviderName("cbx_abcdef123456", slugWithCollisionSuffix(longest, "cbx_abcdef123456"))
        .length,
    ).toBe(63);
  });

  it("rejects a requested slug longer than the provider-name bound", () => {
    expect(() => requestedLeaseSlug("a".repeat(maxRequestedLeaseSlugLength + 1))).toThrow(
      InvalidLeaseSlugError,
    );
    // Normalization collapses separators, so length is measured after normalizing.
    expect(() => requestedLeaseSlug("A! ".repeat(maxRequestedLeaseSlugLength))).toThrow(
      InvalidLeaseSlugError,
    );
  });

  it("keeps collision suffixes inside the provider-name bound", () => {
    const suffixed = slugWithCollisionSuffix("b".repeat(80), "cbx_abcdef123456");
    expect(suffixed).toMatch(/^b{41}-[a-f0-9]{4}$/);
    expect(leaseProviderName("cbx_abcdef123456", suffixed).length).toBeLessThanOrEqual(63);
  });
});
