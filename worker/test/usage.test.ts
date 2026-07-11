import { describe, expect, it } from "vitest";

import {
  InvalidOrgLabelError,
  MISSING_ORG_KEY,
  orgAuthLabelFromKey,
  orgKeyForLabel,
  orgLabelForDisplay,
  orgLabelFromKey,
  orgMatchesForAccounting,
  orgMatchesForFilter,
  requestOrg,
  requestOrgLabel,
  sameOrgIdentityKey,
} from "../src/org-identity";
import type { LeaseRecord } from "../src/types";
import { costLimits, enforceCostLimits, leaseCost, usageSummary } from "../src/usage";

describe("organization identity", () => {
  it("keeps colliding legacy labels distinct in authorization keys", () => {
    const spaced = orgKeyForLabel("science team");
    const underscored = orgKeyForLabel("science_team");

    expect(spaced).not.toBe(underscored);
    expect(spaced).toMatch(/^~1\./);
    expect(orgLabelFromKey(spaced)).toBe("science team");
    expect(orgLabelFromKey(underscored)).toBe("science_team");
    expect(sameOrgIdentityKey(spaced, underscored)).toBe(false);
  });

  it("rejects malformed labels and non-canonical keys", () => {
    expect(() => orgKeyForLabel(" science team")).toThrow(InvalidOrgLabelError);
    expect(() => orgKeyForLabel("science team ")).toThrow(InvalidOrgLabelError);
    expect(() => orgKeyForLabel("science	t eam")).toThrow(InvalidOrgLabelError);
    expect(() => orgKeyForLabel("science\n")).toThrow(InvalidOrgLabelError);
    expect(() => orgKeyForLabel("science\r\n")).toThrow(InvalidOrgLabelError);
    expect(() => orgKeyForLabel("sciencé")).toThrow(InvalidOrgLabelError);
    expect(() => orgKeyForLabel("a".repeat(64))).toThrow(InvalidOrgLabelError);

    const key = orgKeyForLabel("science team");
    expect(orgLabelFromKey(`${key}=`)).toBeUndefined();
    expect(orgLabelFromKey("~1.!!")).toBeUndefined();
    expect(orgLabelFromKey("science_team")).toBeUndefined();
    expect(orgLabelForDisplay("~1.!!")).toBe("unknown");
    expect(orgLabelForDisplay("~2.future-key")).toBe("unknown");
  });

  it("keeps a missing identity distinct from the literal unknown label", () => {
    const request = new Request("https://coordinator.example/v1/leases");

    expect(requestOrg(request, {})).toBe(MISSING_ORG_KEY);
    expect(requestOrgLabel(request, {})).toBe("unknown");
    expect(requestOrg(request, { CRABBOX_DEFAULT_ORG: "unknown" })).not.toBe(MISSING_ORG_KEY);
    expect(orgAuthLabelFromKey(MISSING_ORG_KEY)).toBe("");
    expect(orgLabelForDisplay(MISSING_ORG_KEY)).toBe("unknown");
  });

  it("never authorizes legacy identities but conservatively accounts for them", () => {
    const spaced = orgKeyForLabel("science team");
    const underscored = orgKeyForLabel("science_team");
    const unrelated = orgKeyForLabel("unrelated");

    expect(sameOrgIdentityKey("science_team", "science_team")).toBe(false);
    expect(orgMatchesForFilter("science_team", "science_team")).toBe(true);
    expect(orgMatchesForFilter("science_team", underscored)).toBe(false);
    expect(orgMatchesForAccounting("science_team", spaced)).toBe(true);
    expect(orgMatchesForAccounting("science_team", underscored)).toBe(true);
    expect(orgMatchesForAccounting("science_team", unrelated)).toBe(false);
    expect(orgMatchesForAccounting("unknown", MISSING_ORG_KEY)).toBe(true);
    expect(orgMatchesForAccounting("unknown", orgKeyForLabel("unknown"))).toBe(true);
    expect(orgMatchesForAccounting("unknown", unrelated)).toBe(false);
    expect(orgMatchesForAccounting("unknown", "unrelated")).toBe(false);
  });
});

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
    const org = orgKeyForLabel("openclaw");
    const lease = testLease({
      owner: "peter@example.com",
      org,
      createdAt: "2026-05-01T01:00:00Z",
      endedAt: "2026-05-01T02:00:00Z",
      state: "released",
      estimatedHourlyUSD: 2,
      maxEstimatedUSD: 3,
    });
    const usage = usageSummary([lease], { scope: "org", org, month: "2026-05" }, now);
    expect(usage.leases).toBe(1);
    expect(usage.runtimeSeconds).toBe(3600);
    expect(usage.estimatedUSD).toBe(2);
    expect(usage.reservedUSD).toBe(3);
    expect(usage.byOwner[0]?.key).toBe("peter@example.com");
    expect(usage.byOrg[0]?.key).toBe("openclaw");
  });

  it("keeps user-visible usage inside the requested owner and org", () => {
    const now = new Date("2026-05-01T02:00:00Z");
    const org = orgKeyForLabel("org-a");
    const matching = testLease({ owner: "alice@example.com", org });
    const otherOrg = testLease({
      id: "cbx_other_org",
      owner: "alice@example.com",
      org: orgKeyForLabel("org-b"),
    });

    const usage = usageSummary(
      [matching, otherOrg],
      { scope: "user", owner: "alice@example.com", org, month: "2026-05" },
      now,
    );

    expect(usage.leases).toBe(1);
    expect(usage.byOrg.map((group) => group.key)).toEqual(["org-a"]);
  });

  it("keeps collision-resistant org budgets independent", () => {
    const now = new Date("2026-05-01T00:30:00Z");
    const existing = testLease({
      org: orgKeyForLabel("science team"),
      expiresAt: "2026-05-01T03:00:00Z",
    });
    const candidate = testLease({
      id: "cbx_collision_candidate",
      org: orgKeyForLabel("science_team"),
      expiresAt: "2026-05-01T03:00:00Z",
    });
    const limits = {
      ...costLimits({} as never),
      maxActiveLeasesPerOrg: 1,
    };

    expect(enforceCostLimits([existing], candidate, limits, now)).toBe("");
  });

  it("counts ambiguous legacy org records against every matching current budget", () => {
    const now = new Date("2026-05-01T00:30:00Z");
    const legacy = testLease({
      org: "science_team",
      expiresAt: "2026-05-01T03:00:00Z",
    });
    const candidate = testLease({
      id: "cbx_legacy_candidate",
      org: orgKeyForLabel("science team"),
      expiresAt: "2026-05-01T03:00:00Z",
    });
    const limits = {
      ...costLimits({} as never),
      maxActiveLeasesPerOrg: 1,
    };

    expect(enforceCostLimits([legacy], candidate, limits, now)).toContain(
      "active lease limit for org exceeded: 2/1",
    );
  });

  it.each(["active", "provisioning"] as const)(
    "counts expired %s records against active lease limits until cleanup completes",
    (state) => {
      const now = new Date("2026-05-01T02:00:00Z");
      const existing = testLease({
        state,
        expiresAt: "2026-05-01T01:00:00Z",
      });
      const candidate = testLease({
        id: "cbx_expired_limit_candidate",
        createdAt: now.toISOString(),
        expiresAt: "2026-05-01T03:00:00Z",
      });

      expect(
        enforceCostLimits(
          [existing],
          candidate,
          { ...costLimits({} as never), maxActiveLeases: 1 },
          now,
        ),
      ).toContain("active lease limit exceeded: 2/1");
      expect(
        enforceCostLimits(
          [existing],
          candidate,
          { ...costLimits({} as never), maxActiveLeasesPerOwner: 1 },
          now,
        ),
      ).toContain("active lease limit for owner exceeded: 2/1");
      expect(
        enforceCostLimits(
          [existing],
          candidate,
          { ...costLimits({} as never), maxActiveLeasesPerOrg: 1 },
          now,
        ),
      ).toContain("active lease limit for org exceeded: 2/1");
      expect(usageSummary([existing], { scope: "all", month: "2026-05" }, now).activeLeases).toBe(
        1,
      );
    },
  );

  it("keeps legacy reservations in matching monthly org budgets", () => {
    const now = new Date("2026-05-01T02:00:00Z");
    const legacy = testLease({
      org: "science_team",
      state: "released",
      endedAt: "2026-05-01T01:00:00Z",
      maxEstimatedUSD: 9,
    });
    const candidate = testLease({
      id: "cbx_legacy_monthly_candidate",
      org: orgKeyForLabel("science team"),
      createdAt: "2026-05-01T02:00:00Z",
      maxEstimatedUSD: 2,
    });
    const limits = {
      ...costLimits({} as never),
      maxMonthlyUSDPerOrg: 10,
    };

    expect(enforceCostLimits([legacy], candidate, limits, now)).toContain(
      "monthly budget for org exceeded",
    );
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

  it.each([
    ["fleet", { maxMonthlyUSD: 10 }, "monthly budget exceeded"],
    ["owner", { maxMonthlyUSDPerOwner: 10 }, "monthly budget for owner exceeded"],
    ["org", { maxMonthlyUSDPerOrg: 10 }, "monthly budget for org exceeded"],
  ] as const)(
    "counts live prior-month reservations against the %s monthly budget",
    (_, limit, message) => {
      const now = new Date("2026-06-01T00:30:00Z");
      const existing = testLease({
        createdAt: "2026-05-31T23:30:00Z",
        expiresAt: "2026-06-01T23:30:00Z",
        maxEstimatedUSD: 9,
      });
      const candidate = testLease({
        id: "cbx_month_boundary_candidate",
        createdAt: now.toISOString(),
        expiresAt: "2026-06-01T01:30:00Z",
        maxEstimatedUSD: 2,
      });

      expect(
        enforceCostLimits([existing], candidate, { ...costLimits({} as never), ...limit }, now),
      ).toContain(message);
    },
  );

  it("does not carry terminal prior-month reservations into the current budget", () => {
    const now = new Date("2026-06-01T00:30:00Z");
    const released = testLease({
      createdAt: "2026-05-31T22:00:00Z",
      endedAt: "2026-05-31T23:00:00Z",
      state: "released",
      maxEstimatedUSD: 9,
    });
    const candidate = testLease({
      id: "cbx_month_boundary_released_candidate",
      createdAt: now.toISOString(),
      maxEstimatedUSD: 2,
    });

    expect(
      enforceCostLimits(
        [released],
        candidate,
        { ...costLimits({} as never), maxMonthlyUSD: 10 },
        now,
      ),
    ).toBe("");
  });

  it("allows configured capacity admins to use the admin owner active cap", () => {
    const now = new Date("2026-05-01T02:00:00Z");
    const activeLeases = Array.from({ length: 4 }, (_, index) =>
      testLease({
        id: `cbx_admin_${index}`,
        owner: "Admin@Example.com",
        org: "openclaw",
        expiresAt: "2026-05-01T03:00:00Z",
      }),
    );
    const candidate = testLease({
      id: "cbx_admin_next",
      owner: "admin@example.com",
      org: "openclaw",
      createdAt: "2026-05-01T02:00:00Z",
      expiresAt: "2026-05-01T03:00:00Z",
    });
    const limits = {
      ...costLimits({
        CRABBOX_MAX_ACTIVE_LEASES_PER_OWNER: "4",
        CRABBOX_CAPACITY_ADMIN_OWNERS: "admin@example.com",
        CRABBOX_MAX_ACTIVE_LEASES_PER_CAPACITY_ADMIN: "8",
      } as never),
      maxActiveLeases: 8,
      maxActiveLeasesPerOrg: 8,
    };

    const error = enforceCostLimits(activeLeases, candidate, limits, now);

    expect(error).toBe("");
  });

  it("keeps the regular owner active cap for non-admin owners", () => {
    const now = new Date("2026-05-01T02:00:00Z");
    const activeLeases = Array.from({ length: 4 }, (_, index) =>
      testLease({
        id: `cbx_user_${index}`,
        owner: "user@example.com",
        org: "openclaw",
        expiresAt: "2026-05-01T03:00:00Z",
      }),
    );
    const candidate = testLease({
      id: "cbx_user_next",
      owner: "user@example.com",
      org: "openclaw",
      createdAt: "2026-05-01T02:00:00Z",
      expiresAt: "2026-05-01T03:00:00Z",
    });

    const limits = {
      ...costLimits({
        CRABBOX_MAX_ACTIVE_LEASES_PER_OWNER: "4",
        CRABBOX_CAPACITY_ADMIN_OWNERS: "admin@example.com",
        CRABBOX_MAX_ACTIVE_LEASES_PER_CAPACITY_ADMIN: "8",
      } as never),
      maxActiveLeases: 8,
      maxActiveLeasesPerOrg: 8,
    };

    const error = enforceCostLimits(activeLeases, candidate, limits, now);

    expect(error).toContain("active lease limit for owner exceeded: 5/4");
  });

  it("excludes registered inventory from managed usage and active limits", () => {
    const now = new Date("2026-05-01T02:00:00Z");
    const registered = testLease({
      id: "cbx_registered",
      lifecycle: "registered",
      owner: "alice@example.com",
      org: "example-org",
      expiresAt: "2026-05-01T03:00:00Z",
    });
    const candidate = testLease({
      id: "cbx_managed",
      owner: "alice@example.com",
      org: "example-org",
      expiresAt: "2026-05-01T03:00:00Z",
    });
    const limits = {
      ...costLimits({} as never),
      maxActiveLeases: 1,
      maxActiveLeasesPerOwner: 1,
      maxActiveLeasesPerOrg: 1,
    };

    expect(enforceCostLimits([registered], candidate, limits, now)).toBe("");
    expect(usageSummary([registered], { scope: "all", month: "2026-05" }, now)).toMatchObject({
      leases: 0,
      activeLeases: 0,
      estimatedUSD: 0,
      reservedUSD: 0,
    });
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
    org: orgKeyForLabel("example.com"),
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
