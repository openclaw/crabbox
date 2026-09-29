/// <reference types="node/async_hooks" />
import { AsyncLocalStorage } from "node:async_hooks";

import type { CreationEvent, LeaseRecord } from "./types";

// Observations only: never use this timeline to authorize readiness or ownership.
export function creationEvent(
  phase: CreationEvent["phase"],
  source: CreationEvent["source"] = "coordinator",
): CreationEvent {
  return { phase, source, at: new Date().toISOString() };
}

export function observedRunning(status: string): CreationEvent[] {
  return status.toLowerCase() === "running"
    ? [creationEvent("instance_running", "provider_observation")]
    : [];
}

export function mergeClientCreationEvents(lease: LeaseRecord, input: unknown, now: number): void {
  if (!Array.isArray(input) || input.length > 4) return;
  const sources = {
    ssh_tcp_accept: "client",
    ssh_authenticated: "client",
    bootstrap_complete: "guest",
    workspace_ready: "client",
  } as const;
  for (const item of input) {
    if (!item || typeof item !== "object") continue;
    const { phase, at, source } = item;
    if (
      typeof phase !== "string" ||
      !Object.hasOwn(sources, phase) ||
      source !== sources[phase as keyof typeof sources]
    )
      continue;
    const timestamp = typeof at === "string" ? Date.parse(at) : NaN;
    // Reject inherited image markers and bound clock skew; missing observations stay absent.
    if (
      !Number.isFinite(timestamp) ||
      timestamp < Date.parse(lease.createdAt) - 60_000 ||
      timestamp > now + 60_000
    )
      continue;
    const events = (lease.creationEvents ??= []);
    if (!events.some((event) => event.phase === phase))
      events.push({
        phase: phase as keyof typeof sources,
        source,
        at: new Date(timestamp).toISOString(),
      });
  }
}

// Request-local, fixed-size buckets. Only call with static step names, never resource data.
const creationSteps = new AsyncLocalStorage<Map<string, CreationEvent>>();
const maxCreationSteps = 64;

export function withCreationSteps<T>(operation: () => T): T {
  return creationSteps.run(new Map(), operation);
}

export function recordCreationStep(step: string, durationMs: number, failed = false): void {
  const steps = creationSteps.getStore();
  if (!steps || !/^[a-z][a-z0-9_.]{0,63}$/.test(step) || !Number.isFinite(durationMs)) return;
  const previous = steps.get(step);
  if (!previous && steps.size >= maxCreationSteps) return;
  steps.set(step, {
    phase: "coordinator_step",
    source: "coordinator",
    at: new Date().toISOString(),
    step,
    durationMs: Math.min(
      Number.MAX_SAFE_INTEGER,
      (previous?.durationMs ?? 0) + Math.max(0, durationMs),
    ),
    count: Math.min(Number.MAX_SAFE_INTEGER, (previous?.count ?? 0) + 1),
    errors: Math.min(Number.MAX_SAFE_INTEGER, (previous?.errors ?? 0) + Number(failed)),
  });
}

export async function measureCreationStep<T>(
  step: string,
  operation: () => Promise<T>,
): Promise<T> {
  if (!creationSteps.getStore()) return operation();
  const startedAt = Date.now();
  let failed = true;
  try {
    const result = await operation();
    failed = false;
    return result;
  } finally {
    recordCreationStep(step, Date.now() - startedAt, failed);
  }
}

export function measureCreationStepSync<T>(step: string, operation: () => T): T {
  if (!creationSteps.getStore()) return operation();
  const startedAt = Date.now();
  let failed = true;
  try {
    const result = operation();
    failed = false;
    return result;
  } finally {
    recordCreationStep(step, Date.now() - startedAt, failed);
  }
}

// Snapshot into existing lease writes; telemetry never adds a storage/network operation.
export function appendCreationSteps(lease: Pick<LeaseRecord, "creationEvents">): void {
  const steps = creationSteps.getStore();
  if (!steps?.size) return;
  lease.creationEvents = [
    ...(lease.creationEvents ?? []).filter((event) => event.phase !== "coordinator_step"),
    ...[...steps.values()].map((event) => ({ ...event })),
  ];
}
