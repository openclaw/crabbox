import { describe, expect, it } from "vitest";

import type { LeaseRecord } from "../src/types";
import { costLimits, enforceCostLimits, leaseCost, usageSummary } from "../src/usage";

describe("azure cost overrides", () => {
  it("honors CRABBOX_COST_RATES_JSON for Azure SKUs", () => {
    const cost = leaseCost(
      {
        CRABBOX_COST_RATES_JSON: JSON.stringify({ "azure:Standard_D16as_v5": 0.77 }),
      },
      "azure",
      "Standard_D16as_v5",
      3600,
      undefined,
    );
    expect(cost.hourlyUSD).toBe(0.77);
  });
});

describe("usage accounting", () => {
  it("estimates cost and aggregates by owner and org", () => {
    const now = new Date("2026-05-01T02:00:00Z");
    const lease = testLease({
      owner: "peter@example.com",
      org: "openclaw",
      createdAt: "2026-05-01T01:00:00Z",
      endedAt: "2026-05-01T02:00:00Z",
      state: "released",
      estimatedHourlyUSD: 2,
      maxEstimatedUSD: 3,
    });
    const usage = usageSummary([lease], { scope: "org", org: "openclaw", month: "2026-05" }, now);
    expect(usage.leases).toBe(1);
    expect(usage.runtimeSeconds).toBe(3600);
    expect(usage.estimatedUSD).toBe(2);
    expect(usage.reservedUSD).toBe(3);
    expect(usage.byOwner[0]?.key).toBe("peter@example.com");
    expect(usage.byOrg[0]?.key).toBe("openclaw");
  });

  it("blocks new leases when owner monthly budget would be exceeded", () => {
    const now = new Date("2026-05-01T02:00:00Z");
    const existing = testLease({
      owner: "peter@example.com",
      org: "openclaw",
      createdAt: "2026-05-01T01:00:00Z",
      maxEstimatedUSD: 9,
    });
    const candidate = testLease({
      id: "cbx_000000000002",
      owner: "peter@example.com",
      org: "openclaw",
      createdAt: "2026-05-01T02:00:00Z",
      maxEstimatedUSD: 2,
    });
    const error = enforceCostLimits(
      [existing],
      candidate,
      { ...costLimits({} as never), maxMonthlyUSDPerOwner: 10 },
      now,
    );
    expect(error).toContain("monthly budget for owner exceeded");
  });

  it("supports cost rate overrides", () => {
    const cost = leaseCost(
      { CRABBOX_COST_RATES_JSON: '{"aws:c7a.48xlarge":12}' },
      "aws",
      "c7a.48xlarge",
      1800,
    );
    expect(cost.hourlyUSD).toBe(12);
    expect(cost.maxUSD).toBe(6);
  });

  it("uses provider-fetched hourly prices when no override is configured", () => {
    const cost = leaseCost({}, "aws", "c7a.48xlarge", 1800, 4);
    expect(cost.hourlyUSD).toBe(4);
    expect(cost.maxUSD).toBe(2);
  });

  it("keeps explicit rate overrides above provider-fetched prices", () => {
    const cost = leaseCost(
      { CRABBOX_COST_RATES_JSON: '{"aws:c7a.48xlarge":12}' },
      "aws",
      "c7a.48xlarge",
      1800,
      4,
    );
    expect(cost.hourlyUSD).toBe(12);
  });
});

function testLease(overrides: Partial<LeaseRecord>): LeaseRecord {
  return {
    id: "cbx_000000000001",
    provider: "aws",
    cloudID: "i-123",
    region: "eu-west-1",
    owner: "owner@example.com",
    org: "example.com",
    profile: "project-check",
    class: "beast",
    serverType: "c7a.48xlarge",
    serverID: 0,
    serverName: "crabbox-cbx-000000000001",
    providerKey: "crabbox-cbx-000000000001",
    host: "203.0.113.10",
    sshUser: "crabbox",
    sshPort: "2222",
    sshFallbackPorts: ["22"],
    workRoot: "/work/crabbox",
    keep: false,
    ttlSeconds: 5400,
    estimatedHourlyUSD: 9,
    maxEstimatedUSD: 13.5,
    state: "active",
    createdAt: "2026-05-01T00:00:00Z",
    updatedAt: "2026-05-01T00:00:00Z",
    expiresAt: "2026-05-01T01:30:00Z",
    ...overrides,
  };
}
