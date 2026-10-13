import { describe, expect, it } from "vitest";

import {
  clampLeaseDurations,
  leaseConfig,
  leasePolicyOverridesInvalid,
  resolveLeasePolicy,
} from "../src/config";
import { MISSING_ORG_KEY, orgKeyForLabel } from "../src/org-identity";
import { leaseCost, leaseExtensionReservationUSD } from "../src/usage";

describe("lease lifetime policy", () => {
  it.each([0, 0.08, 1.13, 9])(
    "uses create-time reservation rounding at hourly rate %s",
    (hourlyUSD) => {
      for (const ttl of [1800, 3600, 7199]) {
        const created =
          hourlyUSD === 0 ? 0 : leaseCost({}, "hetzner", "custom", ttl, hourlyUSD).maxUSD;
        expect(
          leaseExtensionReservationUSD({ estimatedHourlyUSD: hourlyUSD, maxEstimatedUSD: 0 }, ttl),
        ).toBe(created);
      }
      expect(
        leaseExtensionReservationUSD({ estimatedHourlyUSD: hourlyUSD, maxEstimatedUSD: 100 }, 1800),
      ).toBe(100);
    },
  );
  it("defaults to 30 minutes with a four-hour ceiling", () => {
    expect(resolveLeasePolicy({})).toEqual({
      defaultTTLSeconds: 1800,
      defaultIdleTimeoutSeconds: 1800,
      maxTTLSeconds: 14400,
      extension: "allow",
      autoExtend: "allow",
    });
    expect(leaseConfig({ sshPublicKey: "ssh-ed25519 test" })).toMatchObject({
      ttlSeconds: 1800,
      idleTimeoutSeconds: 1800,
    });
  });
  it("resolves owner over org over global independently per field", () => {
    const env = {
      CRABBOX_LEASE_MAX_TTL_SECONDS: "7200",
      CRABBOX_LEASE_POLICY_OVERRIDES: JSON.stringify({
        orgs: { team: { defaultTTLSeconds: 2400, extension: "deny" } },
        owners: { alice: { defaultTTLSeconds: 3600, autoExtend: "deny" } },
      }),
    };
    expect(resolveLeasePolicy(env, "alice", orgKeyForLabel("team"))).toEqual(
      resolveLeasePolicy(env, "alice", "team"),
    );
    expect(resolveLeasePolicy(env, "alice", "team")).toEqual({
      defaultTTLSeconds: 3600,
      defaultIdleTimeoutSeconds: 1800,
      maxTTLSeconds: 7200,
      extension: "deny",
      autoExtend: "deny",
    });
  });
  it.each([
    "{",
    "[]",
    '{"owners":{"alice":{"maxTTLSeconds":86401}}}',
    '{"owners":{"alice":{"extension":"yes"}}}',
    '{"orgs":{"team":{"defaultTTLSeconds":3600}},"owners":{"bob":{"maxTTLSeconds":0}}}',
  ])("rejects the entire invalid override document %s", (overrides) => {
    const env = { CRABBOX_LEASE_POLICY_OVERRIDES: overrides };
    expect(leasePolicyOverridesInvalid(env)).toBe(true);
    expect(resolveLeasePolicy(env, "alice", "team")).toEqual(resolveLeasePolicy({}));
  });
  it("caps configured globals at 86400 seconds and normalizes defaults against max", () => {
    expect(
      resolveLeasePolicy({
        CRABBOX_LEASE_MAX_TTL_SECONDS: "90000",
        CRABBOX_LEASE_DEFAULT_TTL_SECONDS: "999999",
      }),
    ).toMatchObject({ maxTTLSeconds: 86400, defaultTTLSeconds: 86400 });
    expect(resolveLeasePolicy({ CRABBOX_LEASE_MAX_TTL_SECONDS: "600" })).toMatchObject({
      maxTTLSeconds: 600,
      defaultTTLSeconds: 600,
      defaultIdleTimeoutSeconds: 600,
    });
  });
  it("clamps old clients without rejecting creation and bounds idle by TTL", () => {
    expect(
      clampLeaseDurations({ ttlSeconds: 86400, idleTimeoutSeconds: 18000 }, resolveLeasePolicy({})),
    ).toMatchObject({
      ttlSeconds: 14400,
      idleTimeoutSeconds: 14400,
      policyNotice: expect.any(String),
    });
    expect(
      clampLeaseDurations({ ttlSeconds: 600, idleTimeoutSeconds: 1800 }, resolveLeasePolicy({})),
    ).toMatchObject({ ttlSeconds: 600, idleTimeoutSeconds: 600 });
    expect(
      clampLeaseDurations(
        { ttlSeconds: 86400 },
        resolveLeasePolicy({ CRABBOX_LEASE_EXTENSION: "deny" }),
      ),
    ).toMatchObject({ ttlSeconds: 1800, policyNotice: expect.any(String) });
  });
});

it("keeps missing organization policy separate from a literal reserved-looking label", () => {
  const env = {
    CRABBOX_LEASE_POLICY_OVERRIDES: JSON.stringify({ orgs: { "~0": { defaultTTLSeconds: 600 } } }),
  };
  expect(resolveLeasePolicy(env, "alice", MISSING_ORG_KEY).defaultTTLSeconds).toBe(1800);
  expect(resolveLeasePolicy(env, "alice", orgKeyForLabel("~0")).defaultTTLSeconds).toBe(600);
});
