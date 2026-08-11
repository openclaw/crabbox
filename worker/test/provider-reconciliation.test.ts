import { describe, expect, it } from "vitest";

import {
  observeProviderReconciliationCandidate,
  providerReconciliationCircuitOpen,
  providerReconciliationFingerprint,
  recordProviderReconciliationFailure,
  recordProviderReconciliationSuccess,
} from "../src/provider-reconciliation";
import type { ProviderMachine } from "../src/types";

const machine: ProviderMachine = {
  provider: "aws",
  id: 1,
  cloudID: "i-123",
  name: "crabbox-cbx_123456789abc",
  status: "running",
  serverType: "c7a.large",
  host: "192.0.2.10",
  labels: {
    crabbox: "true",
    created_by: "crabbox",
    lease: "cbx_123456789abc",
    owner: "alice",
    provider: "aws",
    slug: "blue-box",
  },
};

describe("provider reconciliation", () => {
  it("builds a stable fingerprint independent of label insertion order", () => {
    const reordered = {
      ...machine,
      labels: Object.fromEntries([
        ["e\u0301", "decomposed"],
        ["\u00e9", "precomposed"],
        ...Object.entries(machine.labels).toReversed(),
      ]),
    };
    const original = {
      ...machine,
      labels: Object.fromEntries([
        ...Object.entries(machine.labels),
        ["\u00e9", "precomposed"],
        ["e\u0301", "decomposed"],
      ]),
    };

    expect(providerReconciliationFingerprint("aws", "eu-west-1", original)).toBe(
      providerReconciliationFingerprint("aws", "eu-west-1", reordered),
    );
  });

  it("preserves legacy fingerprints for providers without resource identity", () => {
    expect(providerReconciliationFingerprint("aws", "eu-west-1", machine)).toBe(
      JSON.stringify({
        provider: "aws",
        scope: "eu-west-1",
        cloudID: "i-123",
        name: "crabbox-cbx_123456789abc",
        lease: "cbx_123456789abc",
        owner: "alice",
        providerLabel: "aws",
        slug: "blue-box",
        keep: "",
        createdAt: "",
        expiresAt: "",
      }),
    );
  });

  it("changes the fingerprint when ownership identity changes", () => {
    const changed = {
      ...machine,
      labels: { ...machine.labels, lease: "cbx_abcdefabcdef" },
    };

    expect(providerReconciliationFingerprint("aws", "eu-west-1", changed)).not.toBe(
      providerReconciliationFingerprint("aws", "eu-west-1", machine),
    );
  });

  it("keeps arbitrary provider metadata out of the persisted fingerprint", () => {
    const withLargeDiagnosticLabel = {
      ...machine,
      labels: {
        ...machine.labels,
        provider_diagnostic: "x".repeat(256 * 1024),
      },
    };

    expect(providerReconciliationFingerprint("aws", "eu-west-1", withLargeDiagnosticLabel)).toBe(
      providerReconciliationFingerprint("aws", "eu-west-1", machine),
    );
  });

  it("changes the provider fingerprint for reconciliation resource identity while ignoring metadata", () => {
    const withIdentity: ProviderMachine = {
      ...machine,
      resourceIdentity: "azure-components-v1",
    };
    const changedIdentity: ProviderMachine = {
      ...withIdentity,
      resourceIdentity: "azure-components-v2",
    };
    const withMetadata: ProviderMachine = {
      ...withIdentity,
      host: "198.51.100.42",
      labels: {
        ...withIdentity.labels,
        provider_diagnostic: "inventory-page-2",
      },
    };

    expect(providerReconciliationFingerprint("azure", "eastus", changedIdentity)).not.toBe(
      providerReconciliationFingerprint("azure", "eastus", withIdentity),
    );
    expect(providerReconciliationFingerprint("azure", "eastus", withMetadata)).toBe(
      providerReconciliationFingerprint("azure", "eastus", withIdentity),
    );
  });

  it("requires the same candidate in two inventories after quarantine", () => {
    const first = observeProviderReconciliationCandidate({
      fingerprint: "abc",
      now: Date.parse("2026-07-31T12:00:00Z"),
      quarantineSeconds: 300,
    });
    expect(first.action).toBe("quarantine");

    const tooEarly = observeProviderReconciliationCandidate({
      fingerprint: "abc",
      previous: first.quarantine,
      now: Date.parse("2026-07-31T12:04:59Z"),
      quarantineSeconds: 300,
    });
    expect(tooEarly.action).toBe("quarantine");

    const eligible = observeProviderReconciliationCandidate({
      fingerprint: "abc",
      previous: tooEarly.quarantine,
      now: Date.parse("2026-07-31T12:05:00Z"),
      quarantineSeconds: 300,
    });
    expect(eligible.action).toBe("release");
    expect(eligible.quarantine.observations).toBe(3);
  });

  it("resets quarantine when resource identity changes", () => {
    const first = observeProviderReconciliationCandidate({
      fingerprint: "old",
      now: Date.parse("2026-07-31T12:00:00Z"),
      quarantineSeconds: 300,
    });
    const changed = observeProviderReconciliationCandidate({
      fingerprint: "new",
      previous: first.quarantine,
      now: Date.parse("2026-07-31T12:10:00Z"),
      quarantineSeconds: 300,
    });

    expect(changed.action).toBe("quarantine");
    expect(changed.quarantine.observations).toBe(1);
    expect(changed.quarantine.eligibleAt).toBe("2026-07-31T12:15:00.000Z");
  });

  it("opens a bounded circuit after three consecutive failures", () => {
    const now = Date.parse("2026-07-31T12:00:00Z");
    const first = recordProviderReconciliationFailure({ now, error: "one" });
    const second = recordProviderReconciliationFailure({ previous: first, now, error: "two" });
    const third = recordProviderReconciliationFailure({ previous: second, now, error: "three" });
    const fourth = recordProviderReconciliationFailure({ previous: third, now, error: "four" });
    const sixth = recordProviderReconciliationFailure({
      previous: recordProviderReconciliationFailure({
        previous: fourth,
        now,
        error: "five",
      }),
      now,
      error: "six",
    });

    expect(first.retryAt).toBeUndefined();
    expect(second.retryAt).toBeUndefined();
    expect(third.retryAt).toBe("2026-07-31T12:05:00.000Z");
    expect(fourth.retryAt).toBe("2026-07-31T12:15:00.000Z");
    expect(sixth.retryAt).toBe("2026-07-31T13:00:00.000Z");
    expect(providerReconciliationCircuitOpen(third, now)).toBe(true);
    expect(providerReconciliationCircuitOpen(third, Date.parse(third.retryAt!))).toBe(false);
    expect(recordProviderReconciliationSuccess()).toEqual({ consecutiveFailures: 0 });
  });
});
