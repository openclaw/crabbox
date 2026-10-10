import { describe, expect, it } from "vitest";

import {
  extendLeaseLifetime,
  lifetimeCandidate,
  lifetimeDeadline,
  lifetimeNextAlarm,
  newLeaseLifetime,
} from "../src/lease-lifetime";
import type { Env, LeaseRecord } from "../src/types";

const now = Date.parse("2026-10-01T00:00:00Z");
const env = { CRABBOX_LEASE_LIFETIME_MODE: "enforce" } as Env;
const policy = () => newLeaseLifetime(env, new Date(now), false)!;
const lease = (overrides: Partial<LeaseRecord> = {}) =>
  ({
    state: "released",
    releaseDeletesServer: false,
    lifetimePolicy: policy(),
    ...overrides,
  }) as LeaseRecord;

describe("lease lifetime safety policy", () => {
  it("does not enroll history or default installations", () => {
    expect(newLeaseLifetime({} as Env, new Date(now), false)).toBeUndefined();
    expect(lifetimeCandidate(lease({ lifetimePolicy: undefined }))).toBe(false);
    expect(lifetimeCandidate(lease({ lifecycle: "registered" }))).toBe(false);
    expect(lifetimeCandidate(lease({ cleanupCompletedAt: new Date(now).toISOString() }))).toBe(
      false,
    );
  });
  it("snapshots 24-hour batch and 7-day interactive limits independent of keep and heartbeats", () => {
    expect(lifetimeDeadline(policy())).toBe(now + 86_400_000);
    expect(lifetimeDeadline(newLeaseLifetime(env, new Date(now), true)!)).toBe(
      now + 7 * 86_400_000,
    );
    const kept = lease({
      keep: true,
      lastTouchedAt: new Date(now + 30 * 86_400_000).toISOString(),
    });
    expect(lifetimeDeadline(kept.lifetimePolicy!)).toBe(now + 86_400_000);
    expect(lifetimeCandidate(kept)).toBe(true);
  });
  it("validates configured limits rather than permitting an infinite deadline", () => {
    for (const value of ["0", "-1", "NaN", "Infinity", "6000000000", "1.5"]) {
      expect(() =>
        newLeaseLifetime({ ...env, CRABBOX_BATCH_MAX_AGE_SECONDS: value }, new Date(now), false),
      ).toThrow("lifetime limits");
    }
    expect(
      newLeaseLifetime({ ...env, CRABBOX_BATCH_MAX_AGE_SECONDS: "3600" }, new Date(now), false)
        ?.deadlineAt,
    ).toBe("2026-10-01T01:00:00.000Z");
  });
  it("requires an expiring, attributed extension and cannot extend in-flight cleanup", () => {
    const expiry = new Date(now + 2 * 86_400_000).toISOString();
    expect(() => extendLeaseLifetime(policy(), "", "debugging", expiry, now)).toThrow(
      "authenticated owner",
    );
    expect(() => extendLeaseLifetime(policy(), "alice", "", expiry, now)).toThrow(
      "authenticated owner",
    );
    expect(() =>
      extendLeaseLifetime(
        policy(),
        "alice",
        "debugging",
        new Date(now + 8 * 86_400_000).toISOString(),
        now,
      ),
    ).toThrow("advance the deadline");
    expect(() =>
      extendLeaseLifetime({ ...policy(), status: "stopping" }, "alice", "debugging", expiry, now),
    ).toThrow("cannot be extended");
    const extended = extendLeaseLifetime(policy(), "alice", "debugging", expiry, now);
    expect(lifetimeDeadline(extended)).toBe(Date.parse(expiry));
    expect(extended.extension?.owner).toBe("alice");
  });
  it("schedules warnings, throttles overdue reports, and retains the deletion deadline", () => {
    const record = lease();
    expect(lifetimeNextAlarm(record, now)).toBe(now + 23 * 3_600_000);
    const overdue = now + 2 * 86_400_000;
    record.lifetimePolicy!.warnedAt = new Date(now).toISOString();
    record.lifetimePolicy!.nextCheckAt = new Date(overdue + 300_000).toISOString();
    expect(lifetimeNextAlarm(record, overdue)).toBe(overdue + 300_000);
    record.lifetimePolicy!.stoppedAt = new Date(overdue).toISOString();
    record.lifetimePolicy!.deleteAfterAt = new Date(overdue + 60_000).toISOString();
    expect(lifetimeNextAlarm(record, overdue)).toBe(overdue + 60_000);
  });
});
