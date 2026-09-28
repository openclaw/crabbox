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
