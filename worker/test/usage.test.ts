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
import type { Env, LeaseRecord } from "../src/types";
import {
  activeLeaseLimitsForOwner,
  addLeaseToCostLimitUsage,
  costLimits,
  createCostLimitUsage,
  enforceCostLimitUsage,
  leaseCost,
  usageSummary,
  type CostLimits,
} from "../src/usage";

function enforceCostLimits(
  leases: LeaseRecord[],
  candidate: LeaseRecord,
  limits: CostLimits,
  now: Date,
): string {
  const usage = createCostLimitUsage(candidate, now);
  for (const lease of leases) {
    addLeaseToCostLimitUsage(usage, lease, now);
  }
  return enforceCostLimitUsage(usage, candidate, limits);
}

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
  it("reserves retained running compute and settles its confirmed stop/resume intervals", () => {
    const now = new Date("2026-05-20T00:00:00Z");
    const running = testLease({
      state: "released",
      releaseDeletesServer: false,
      endedAt: "2026-05-01T00:10:00Z",
      lifetimePolicy: {
        mode: "enforce",
        workload: "batch",
        deadlineAt: "2026-05-21T00:00:00Z",
        retainSeconds: 3600,
        compute: { state: "running", accumulatedMs: 600_000, runningSince: "2026-05-19T23:50:00Z" },
      },
    });
    expect(usageSummary([running], { scope: "all", month: "2026-05" }, now)).toMatchObject({
      activeLeases: 1,
      estimatedUSD: 3,
      reservedUSD: 13.5,
    });
    const stopped = testLease({
      ...running,
      lifetimePolicy: {
        ...running.lifetimePolicy!,
        stoppedAt: "2026-05-20T00:00:00Z",
        compute: { state: "stopped", accumulatedMs: 1_200_000 },
      },
    });
    expect(usageSummary([stopped], { scope: "all", month: "2026-05" }, now)).toMatchObject({
      activeLeases: 0,
      runtimeSeconds: 1200,
      estimatedUSD: 3,
      reservedUSD: 13.5,
      committedUSD: 3,
    });
  });

  it.each(["released", "failed", "expired"] as const)(
    "settles %s leases at elapsed cost capped by their admission reservation",
    (state) => {
      const now = new Date("2026-05-20T00:00:00Z");
      const leases = [
        testLease({ state, endedAt: "2026-05-01T00:30:00Z", maxEstimatedUSD: 13.5 }),
        testLease({ state, endedAt: "2026-05-01T02:00:00Z", maxEstimatedUSD: 13.5 }),
        testLease({ state, endedAt: "2026-05-01T02:00:00Z", maxEstimatedUSD: 0 }),
      ];
      const summary = usageSummary(leases, { scope: "all", month: "2026-05" }, now);
      expect(summary).toMatchObject({
        activeLeases: 0,
        estimatedUSD: 40.5,
        reservedUSD: 27,
        committedUSD: 18,
      });
      const budget = createCostLimitUsage(leases[0]!, now);
      for (const lease of leases) addLeaseToCostLimitUsage(budget, lease, now);
      expect(budget).toMatchObject({
        committedUSD: 18,
        ownerCommittedUSD: 18,
        orgCommittedUSD: 18,
      });
    },
  );

  it("stops failed runtime at its persisted observation and charges nothing when never allocated", () => {
    const now = new Date("2026-05-20T00:00:00Z");
    const observed = testLease({
      state: "failed",
      endedAt: "2026-05-01T00:10:00Z",
      updatedAt: "2026-05-01T00:10:00Z",
    });
    const legacy = testLease({ ...observed, endedAt: undefined });
    const neverRan = testLease({ ...observed, cloudID: "", provisioningResourceMayExist: false });
    expect(
      usageSummary([observed, legacy, neverRan], { scope: "all", month: "2026-05" }, now),
    ).toMatchObject({ runtimeSeconds: 1200, estimatedUSD: 3, reservedUSD: 40.5, committedUSD: 3 });
  });

  it.each([
    ["fleet", { maxMonthlyUSD: 10 }],
    ["owner", { maxMonthlyUSDPerOwner: 10 }],
    ["org", { maxMonthlyUSDPerOrg: 10 }],
  ])(
    "admits exactly at the %s cap and refuses the next candidate reservation",
    (_scope, limits) => {
      const now = new Date("2026-05-01T02:00:00Z");
      const settled = testLease({ state: "released", endedAt: "2026-05-01T00:20:00Z" });
      const live = testLease({ maxEstimatedUSD: 5 });
      const candidate = testLease({ maxEstimatedUSD: 2 });
      const caps = { ...costLimits({} as never), ...limits };
      expect(enforceCostLimits([settled, live], candidate, caps, now)).toBe("");
      expect(
        enforceCostLimits([settled, live], { ...candidate, maxEstimatedUSD: 2.01 }, caps, now),
      ).toContain("monthly budget");
    },
  );

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
    expect(usage.committedUSD).toBe(2);
    expect(usage.byOwner[0]?.key).toBe("peter@example.com");
    expect(usage.byOrg[0]?.key).toBe("openclaw");
  });

  it.each(["active", "provisioning"] as const)("keeps the full %s reservation", (state) => {
    const now = new Date("2026-05-01T00:30:00Z");
    const lease = testLease({ state, cloudID: state === "provisioning" ? "" : "i-live" });
    expect(usageSummary([lease], { scope: "all", month: "2026-05" }, now)).toMatchObject({
      estimatedUSD: 4.5,
      reservedUSD: 13.5,
      committedUSD: 13.5,
    });
  });

  it.each(["released", "expired", "failed"] as const)(
    "settles a provisioned %s lease on read",
    (state) => {
      const lease = testLease({ state, endedAt: "2026-05-01T00:30:00Z", host: "" });
      const before = structuredClone(lease);
      for (const now of [new Date("2026-05-01T02:00:00Z"), new Date("2026-05-30T02:00:00Z")]) {
        const usage = usageSummary([lease], { scope: "all", month: "2026-05" }, now);
        expect(usage).toMatchObject({ activeLeases: 0, reservedUSD: 13.5, committedUSD: 4.5 });
        for (const groups of [usage.byOwner, usage.byOrg, usage.byProvider, usage.byServerType]) {
          expect(groups[0]).toMatchObject({ reservedUSD: 13.5, committedUSD: 4.5 });
        }
      }
      expect(lease).toEqual(before);
    },
  );

  it.each(["released", "expired", "failed"] as const)(
    "settles an unprovisioned %s lease to zero",
    (state) => {
      const lease = testLease({
        state,
        cloudID: "",
        serverID: 0,
        host: "",
        endedAt: "2026-05-01T00:30:00Z",
      });
      const now = new Date("2026-05-01T02:00:00Z");
      const usage = usageSummary([lease], { scope: "all", month: "2026-05" }, now);
      expect(usage).toMatchObject({ reservedUSD: 13.5, committedUSD: 0 });
      const candidate = testLease({ maxEstimatedUSD: 1 });
      expect(
        enforceCostLimits(
          [lease],
          candidate,
          { ...costLimits({} as never), maxMonthlyUSD: 1 },
          now,
        ),
      ).toBe("");
    },
  );

  it.each([0, 1, 13.5])("caps settlement at the recorded $%s reservation", (maxEstimatedUSD) => {
    const lease = testLease({
      state: "released",
      endedAt: "2026-05-02T00:00:00Z",
      maxEstimatedUSD,
    });
    expect(
      usageSummary([lease], { scope: "all", month: "2026-05" }, new Date("2026-05-03T00:00:00Z"))
        .committedUSD,
    ).toBe(maxEstimatedUSD);
  });

  it("uses retained compute intervals and reserves until compute stops", () => {
    const now = new Date("2026-05-02T00:00:00Z");
    const lease = testLease({
      state: "released",
      releaseDeletesServer: false,
      endedAt: "2026-05-01T00:10:00Z",
      lifetimePolicy: {
        mode: "enforce",
        workload: "batch",
        status: "stopping",
        retainSeconds: 3600,
        deadlineAt: "2026-05-02T00:00:00Z",
        compute: { state: "running", accumulatedMs: 900000, runningSince: "2026-05-01T23:45:00Z" },
      },
    });
    expect(usageSummary([lease], { scope: "all", month: "2026-05" }, now).committedUSD).toBe(13.5);
    lease.lifetimePolicy!.compute = { state: "stopped", accumulatedMs: 1800000 };
    lease.lifetimePolicy!.stoppedAt = "2026-05-02T00:00:00Z";
    expect(usageSummary([lease], { scope: "all", month: "2026-05" }, now).committedUSD).toBe(4.5);
  });

  it.each(["released", "expired", "failed"] as const)(
    "keeps retained %s resources reserved without lifetime enrollment",
    (state) => {
      const lease = testLease({
        state,
        releaseDeletesServer: false,
        endedAt: "2026-05-01T00:30:00Z",
      });
      const now = new Date("2026-06-01T00:00:00Z");
      expect(usageSummary([lease], { scope: "all", month: "2026-05" }, now).committedUSD).toBe(
        13.5,
      );
      const candidate = testLease({ createdAt: now.toISOString(), maxEstimatedUSD: 1 });
      expect(
        enforceCostLimits(
          [lease],
          candidate,
          { ...costLimits({} as never), maxMonthlyUSD: 14 },
          now,
        ),
      ).toContain("monthly budget exceeded");
      lease.cleanupCompletedAt = "2026-05-01T00:35:00Z";
      expect(usageSummary([lease], { scope: "all", month: "2026-05" }, now).committedUSD).toBe(4.5);
      expect(
        enforceCostLimits(
          [lease],
          candidate,
          { ...costLimits({} as never), maxMonthlyUSD: 14 },
          now,
        ),
      ).toBe("");
    },
  );

  it.each(["", "i-cleanup"])(
    "reserves unresolved failed provisioning with resource %s until cleanup",
    (cloudID) => {
      const now = new Date("2026-05-01T02:00:00Z");
      const lease = testLease({
        state: "failed",
        cloudID,
        serverID: 0,
        endedAt: "2026-05-01T00:30:00Z",
        provisioningResourceMayExist: true,
      });
      expect(usageSummary([lease], { scope: "all", month: "2026-05" }, now).committedUSD).toBe(
        13.5,
      );
      const candidate = testLease({ maxEstimatedUSD: 1 });
      expect(
        enforceCostLimits(
          [lease],
          candidate,
          { ...costLimits({} as never), maxMonthlyUSD: 14 },
          now,
        ),
      ).toContain("monthly budget exceeded");
      lease.cleanupCompletedAt = "2026-05-01T00:30:00Z";
      lease.provisioningResourceMayExist = false;
      expect(usageSummary([lease], { scope: "all", month: "2026-05" }, now).committedUSD).toBe(
        cloudID ? 4.5 : 0,
      );
      expect(
        enforceCostLimits(
          [lease],
          candidate,
          { ...costLimits({} as never), maxMonthlyUSD: 14 },
          now,
        ),
      ).toBe("");
    },
  );

  it("aggregates mixed months and identities consistently for fleet, owner and org budgets", () => {
    const now = new Date("2026-06-02T00:00:00Z");
    const org = orgKeyForLabel("example-org");
    const base = testLease({
      owner: "alice@example.com",
      org,
      createdAt: "2026-06-01T00:00:00Z",
      endedAt: "2026-06-01T01:00:00Z",
      state: "released",
      estimatedHourlyUSD: 1,
      maxEstimatedUSD: 10,
    });
    const leases = [
      base,
      {
        ...base,
        id: "live-prior",
        state: "active" as const,
        createdAt: "2026-05-31T23:00:00Z",
        maxEstimatedUSD: 4,
      },
      {
        ...base,
        id: "finished-prior",
        createdAt: "2026-05-31T20:00:00Z",
        endedAt: "2026-05-31T21:00:00Z",
      },
      { ...base, id: "other-owner", owner: "bob@example.com", estimatedHourlyUSD: 2 },
      { ...base, id: "other-org", org: orgKeyForLabel("another-org"), estimatedHourlyUSD: 3 },
      { ...base, id: "unprovisioned", cloudID: "", serverID: 0 },
      { ...base, id: "registered", lifecycle: "registered" as const },
    ];
    const totals = createCostLimitUsage(base, now);
    leases.forEach((lease) => addLeaseToCostLimitUsage(totals, lease, now));
    expect(totals).toMatchObject({
      activeLeases: 1,
      committedUSD: 10,
      ownerCommittedUSD: 8,
      orgCommittedUSD: 7,
    });
    expect(usageSummary(leases, { scope: "all", month: "2026-06" }, now).committedUSD).toBe(6);
    expect(
      usageSummary(leases, { scope: "user", owner: base.owner, month: "2026-06" }, now)
        .committedUSD,
    ).toBe(4);
    expect(usageSummary(leases, { scope: "org", org, month: "2026-06" }, now).committedUSD).toBe(3);
    for (const [field, amount, message] of [
      ["maxMonthlyUSD", 10, "monthly budget exceeded"],
      ["maxMonthlyUSDPerOwner", 8, "monthly budget for owner exceeded"],
      ["maxMonthlyUSDPerOrg", 7, "monthly budget for org exceeded"],
    ] as const) {
      const candidate = { ...base, id: "candidate", maxEstimatedUSD: 1 };
      expect(
        enforceCostLimits(leases, candidate, { ...costLimits({} as never), [field]: amount }, now),
      ).toContain(message);
      expect(
        enforceCostLimits(
          leases,
          candidate,
          { ...costLimits({} as never), [field]: amount + 1 },
          now,
        ),
      ).toBe("");
    }
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
      ).toContain("fleet active lease limit exceeded: 2/1");
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

  it("keeps legacy settled costs in matching monthly org budgets", () => {
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

  it.each([
    { legacy: " Legacy@Example.com , preserved@example.com ", deployment: "", deployedLimit: 10 },
    { legacy: "", deployment: " Deployed@Example.com ", deployedLimit: 50 },
    {
      legacy: "Legacy@Example.com, preserved@example.com",
      deployment: "Deployed@Example.com, LEGACY@example.com",
      deployedLimit: 50,
    },
  ])(
    "combines capacity membership from legacy '$legacy' and deployment '$deployment'",
    ({ legacy, deployment, deployedLimit }) => {
      const limits = costLimits({
        CRABBOX_CAPACITY_ADMIN_OWNERS: legacy,
        CRABBOX_DEPLOYMENT_CAPACITY_ADMIN_OWNERS: deployment,
        CRABBOX_MAX_ACTIVE_LEASES_PER_OWNER: "10",
        CRABBOX_MAX_ACTIVE_LEASES_PER_CAPACITY_ADMIN: "50",
      } as Env);

      expect(activeLeaseLimitsForOwner(limits, "DEPLOYED@example.com").owner).toBe(deployedLimit);
      expect(activeLeaseLimitsForOwner(limits, "legacy@example.com").owner).toBe(legacy ? 50 : 10);
      expect(activeLeaseLimitsForOwner(limits, "preserved@example.com").owner).toBe(
        legacy ? 50 : 10,
      );
      expect(activeLeaseLimitsForOwner(limits, "ordinary@example.com").owner).toBe(10);
    },
  );

  it("allows configured capacity admins to use the admin owner active cap", () => {
    const now = new Date("2026-05-01T02:00:00Z");
    const activeLeases = Array.from({ length: 4 }, (_, index) =>
      testLease({
        id: `cbx_admin_${index}`,
        owner: "Admin@Example.com",
        org: "example-org",
        expiresAt: "2026-05-01T03:00:00Z",
      }),
    );
    const candidate = testLease({
      id: "cbx_admin_next",
      owner: "Admin@Example.com",
      org: "example-org",
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
    expect(
      enforceCostLimits(
        activeLeases,
        candidate,
        { ...limits, maxActiveLeasesPerCapacityAdmin: 0 },
        now,
      ),
    ).toBe("active lease limit for owner exceeded: 5/4");
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
