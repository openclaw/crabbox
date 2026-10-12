import type { Env, LeaseRecord } from "./types";

const daySeconds = 86_400;
export const lifetimeRetryMs = 5 * 60_000;
// StopInstances usually settles within a minute; confirm quickly before falling back to the retry cadence.
export const lifetimeStopConfirmMs = 30_000;
export const lifetimeWarningMs = 60 * 60_000;

export interface LeaseLifetimePolicy {
  source?: "admin-canary";
  mode: "report" | "enforce";
  workload: "batch" | "interactive";
  deadlineAt: string;
  retainSeconds: number;
  extension?: {
    owner: string;
    reason: string;
    grantedAt: string;
    expiresAt: string;
    grantedBy?: string;
  };
  warnedAt?: string;
  deletionWarnedAt?: string;
  checkedAt?: string;
  nextCheckAt?: string;
  status?: "due" | "unsupported" | "stopping" | "retained" | "deleting" | "complete" | "failed";
  error?: string;
  stoppedAt?: string;
  stopStartedAt?: string;
  compute?: { state: "running" | "stopped"; runningSince?: string; accumulatedMs: number };
  deleteStartedAt?: string;
  deleteAfterAt?: string;
  retainedIdentity?: string;
  effectiveStopAt?: string;
  warnings?: string[];
  readiness?: { status: "ready" | "failed"; checkedAt: string; identity?: string; error?: string };
}

export interface LeaseLifetimeRequest {
  mode: "enforce";
  maxAgeSeconds: number;
  retainSeconds: number;
}

export function normalizeLifetimeRequest(value: unknown): LeaseLifetimeRequest | undefined {
  if (value === undefined) return undefined;
  if (!value || typeof value !== "object" || Array.isArray(value))
    throw new Error("lifetimePolicy must specify enforce, maxAgeSeconds and retainSeconds");
  const record = value as Record<string, unknown>;
  if (
    record["mode"] !== "enforce" ||
    Object.keys(record).some((key) => !["mode", "maxAgeSeconds", "retainSeconds"].includes(key))
  )
    throw new Error("lifetimePolicy only accepts enforce, maxAgeSeconds and retainSeconds");
  for (const key of ["maxAgeSeconds", "retainSeconds"] as const) {
    const seconds = record[key];
    if (
      typeof seconds !== "number" ||
      !Number.isSafeInteger(seconds) ||
      seconds < 60 ||
      seconds > 31 * daySeconds
    )
      throw new Error("lifetime limits must be between 60 seconds and 31 days");
  }
  return {
    mode: "enforce",
    maxAgeSeconds: record["maxAgeSeconds"] as number,
    retainSeconds: record["retainSeconds"] as number,
  };
}

export function lifetimeEnforced(policy: LeaseLifetimePolicy, env: Env): boolean {
  return (
    policy.mode === "enforce" &&
    (policy.source === "admin-canary" || env.CRABBOX_LEASE_LIFETIME_MODE === "enforce")
  );
}

export function publicLifetimePolicy(
  policy: LeaseLifetimePolicy,
  now = Date.now(),
): LeaseLifetimePolicy {
  const deadline = lifetimeDeadline(policy);
  const warnings: string[] = [];
  if (
    Number.isFinite(deadline) &&
    !policy.stoppedAt &&
    policy.status !== "complete" &&
    (policy.warnedAt || now >= deadline - lifetimeWarningMs)
  ) {
    warnings.push(
      policy.stopStartedAt || policy.retainedIdentity
        ? "Compute stopping has begun; lifetime extensions are no longer accepted."
        : policy.mode === "report"
          ? `Report-only compute-stop deadline: ${new Date(deadline).toISOString()}; this policy does not stop compute.`
          : `Compute-stop deadline: ${new Date(deadline).toISOString()}; keep and activity do not extend this deadline. Request an administrator extension before stopping starts.`,
    );
  }
  if (
    policy.status !== "complete" &&
    policy.deleteAfterAt &&
    (policy.deletionWarnedAt || now >= Date.parse(policy.deleteAfterAt) - lifetimeWarningMs)
  ) {
    warnings.push(
      `DELETION: the retained root disk will be permanently deleted at ${policy.deleteAfterAt}; no backup is created and a lifetime extension cannot recover it.`,
    );
  }
  if (policy.error) warnings.push(policy.error);
  return {
    ...policy,
    ...(Number.isFinite(deadline) ? { effectiveStopAt: new Date(deadline).toISOString() } : {}),
    warnings,
  };
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
  canary?: LeaseLifetimeRequest,
): LeaseLifetimePolicy | undefined {
  if (canary)
    return {
      source: "admin-canary",
      mode: "enforce",
      workload: interactive ? "interactive" : "batch",
      deadlineAt: new Date(createdAt.getTime() + canary.maxAgeSeconds * 1000).toISOString(),
      retainSeconds: canary.retainSeconds,
    };
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
  const warned = policy.stoppedAt ? policy.deletionWarnedAt : policy.warnedAt;
  const warning = warned ? deadline : deadline - lifetimeWarningMs;
  const nextCheck = Date.parse(policy.nextCheckAt ?? "");
  const ownerDeadline = lease.cleanupStartedAt ? Date.parse(lease.cleanupClaimExpiresAt ?? "") : 0;
  return Math.max(
    now + 1,
    Number.isFinite(ownerDeadline) ? ownerDeadline : 0,
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
  if (
    policy.stopStartedAt ||
    policy.retainedIdentity ||
    policy.stoppedAt ||
    policy.status === "stopping" ||
    policy.status === "deleting"
  ) {
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

// Keep confirmed compute intervals independent of transient cleanup diagnostics.
export function observeLifetimeCompute(lease: LeaseRecord, stopped: boolean, now: number): void {
  const policy = lease.lifetimePolicy!;
  const previous = policy.compute ?? {
    state: policy.stoppedAt ? ("stopped" as const) : ("running" as const),
    accumulatedMs: policy.stoppedAt
      ? Math.max(0, Date.parse(policy.stoppedAt) - Date.parse(lease.createdAt))
      : 0,
    ...(!policy.stoppedAt ? { runningSince: lease.createdAt } : {}),
  };
  if (stopped && previous.state === "running") {
    previous.accumulatedMs += Math.max(0, now - Date.parse(previous.runningSince!));
    delete previous.runningSince;
  } else if (!stopped && previous.state === "stopped") {
    previous.runningSince = new Date(now).toISOString();
  }
  previous.state = stopped ? "stopped" : "running";
  policy.compute = previous;
  if (stopped) {
    policy.stoppedAt ??= new Date(now).toISOString();
    policy.deleteAfterAt ??= new Date(
      Date.parse(policy.stoppedAt) + policy.retainSeconds * 1000,
    ).toISOString();
  }
}

export function lifetimeRuntimeMs(lease: LeaseRecord, now: number): number | undefined {
  const compute = lease.lifetimePolicy?.compute;
  if (!compute) return undefined;
  const completedAt = Date.parse(lease.cleanupCompletedAt ?? "");
  const observedUntil = Number.isFinite(completedAt) ? Math.min(now, completedAt) : now;
  return (
    compute.accumulatedMs +
    (compute.state === "running"
      ? Math.max(0, observedUntil - Date.parse(compute.runningSince!))
      : 0)
  );
}

// Retained release revokes access, not billing. Unconfirmed stop work still owns capacity.
export function lifetimeOwnsCompute(lease: LeaseRecord): boolean {
  return Boolean(
    lease.lifetimePolicy &&
    !lease.cleanupCompletedAt &&
    lease.state === "released" &&
    lease.releaseDeletesServer === false &&
    (lease.lifetimePolicy.compute
      ? lease.lifetimePolicy.compute.state === "running"
      : !lease.lifetimePolicy.stoppedAt) &&
    lease.lifetimePolicy.status !== "complete",
  );
}
