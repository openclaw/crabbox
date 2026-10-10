import type { Env, LeaseRecord } from "./types";

const daySeconds = 86_400;
export const lifetimeRetryMs = 5 * 60_000;
export const lifetimeWarningMs = 60 * 60_000;

export interface LeaseLifetimePolicy {
  mode: "report" | "enforce";
  workload: "batch" | "interactive";
  deadlineAt: string;
  retainSeconds: number;
  extension?: { owner: string; reason: string; grantedAt: string; expiresAt: string };
  warnedAt?: string;
  checkedAt?: string;
  nextCheckAt?: string;
  status?: "due" | "unsupported" | "stopping" | "retained" | "deleting" | "complete" | "failed";
  error?: string;
  stoppedAt?: string;
  deleteStartedAt?: string;
  deleteAfterAt?: string;
  retainedIdentity?: string;
}

function boundedSeconds(value: string | undefined, fallback: number): number {
  if (value === undefined || value === "") return fallback;
  if (!/^\d+$/.test(value)) throw new Error("lifetime limits must be integer seconds");
  const seconds = Number(value);
  if (!Number.isSafeInteger(seconds) || seconds < 60 || seconds > 31 * daySeconds) {
    throw new Error("lifetime limits must be between 60 seconds and 31 days");
  }
  return seconds;
}

// Only the create owner calls this, before dispatch. Never enroll retained history.
export function newLeaseLifetime(
  env: Env,
  createdAt: Date,
  interactive: boolean,
): LeaseLifetimePolicy | undefined {
  const mode = env.CRABBOX_LEASE_LIFETIME_MODE ?? "off";
  if (mode === "off") return undefined;
  if (mode !== "report" && mode !== "enforce") {
    throw new Error("CRABBOX_LEASE_LIFETIME_MODE must be off, report, or enforce");
  }
  const seconds = interactive
    ? boundedSeconds(env.CRABBOX_INTERACTIVE_MAX_AGE_SECONDS, 7 * daySeconds)
    : boundedSeconds(env.CRABBOX_BATCH_MAX_AGE_SECONDS, daySeconds);
  return {
    mode,
    workload: interactive ? "interactive" : "batch",
    deadlineAt: new Date(createdAt.getTime() + seconds * 1000).toISOString(),
    retainSeconds: boundedSeconds(env.CRABBOX_STOPPED_RETENTION_SECONDS, 7 * daySeconds),
  };
}

export function lifetimeDeadline(policy: LeaseLifetimePolicy): number {
  const base = Date.parse(policy.deadlineAt);
  const extension = policy.extension;
  if (!extension) return base;
  if (!extension.owner.trim() || !extension.reason.trim()) return Number.NaN;
  return Math.max(base, Date.parse(extension.expiresAt));
}

export function lifetimeCandidate(lease: LeaseRecord): boolean {
  return Boolean(
    lease.lifetimePolicy &&
    lease.lifecycle !== "registered" &&
    lease.lifetimePolicy.status !== "complete" &&
    !lease.cleanupCompletedAt &&
    // Failed/provisioning resources remain with their recovery owner.
    (lease.state === "active" ||
      (lease.state === "released" && lease.releaseDeletesServer === false)),
  );
}

export function lifetimeNextAlarm(lease: LeaseRecord, now: number): number | undefined {
  if (!lifetimeCandidate(lease)) return undefined;
  const policy = lease.lifetimePolicy!;
  const deadline = policy.deleteAfterAt
    ? Date.parse(policy.deleteAfterAt)
    : lifetimeDeadline(policy);
  if (!Number.isFinite(deadline)) return now + lifetimeRetryMs;
  const warning = policy.warnedAt || policy.stoppedAt ? deadline : deadline - lifetimeWarningMs;
  const nextCheck = Date.parse(policy.nextCheckAt ?? "");
  return Math.max(
    now + 1,
    Number.isFinite(nextCheck)
      ? Math.min(nextCheck, deadline > now ? warning : nextCheck)
      : warning,
  );
}

export function extendLeaseLifetime(
  policy: LeaseLifetimePolicy,
  actor: string,
  reason: unknown,
  expiresAt: unknown,
  now: number,
): LeaseLifetimePolicy {
  if (policy.stoppedAt || policy.status === "stopping" || policy.status === "deleting") {
    throw new Error("a stopped lease or in-flight cleanup cannot be extended");
  }
  if (
    typeof reason !== "string" ||
    !reason.trim() ||
    reason.length > 512 ||
    !actor.trim() ||
    actor === "unknown"
  ) {
    throw new Error("an extension requires an authenticated owner and a reason (1-512 characters)");
  }
  const expiry = typeof expiresAt === "string" ? Date.parse(expiresAt) : Number.NaN;
  if (
    !Number.isFinite(expiry) ||
    expiry <= now ||
    expiry <= lifetimeDeadline(policy) ||
    expiry > now + 7 * daySeconds * 1000
  ) {
    throw new Error("extension expiry must advance the deadline and be within seven days");
  }
  const next = {
    ...policy,
    extension: {
      owner: actor,
      reason: reason.trim(),
      grantedAt: new Date(now).toISOString(),
      expiresAt: new Date(expiry).toISOString(),
    },
  };
  delete next.warnedAt;
  delete next.nextCheckAt;
  delete next.error;
  return next;
}

export class LifetimeStopRequiredError extends Error {}

// Retained release revokes access, not billing. Unconfirmed stop work still owns capacity.
export function lifetimeOwnsCompute(lease: LeaseRecord): boolean {
  return Boolean(
    lease.lifetimePolicy &&
    lease.state === "released" &&
    lease.releaseDeletesServer === false &&
    lease.lifetimePolicy.status !== "retained" &&
    lease.lifetimePolicy.status !== "complete",
  );
}
