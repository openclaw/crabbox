import { afterEach, expect, it, vi } from "vitest";

import { creationEvent, mergeClientCreationEvents, observedRunning } from "../src/creation-events";
import type { LeaseRecord } from "../src/types";

afterEach(() => vi.useRealTimers());

it("timestamps provider observations without inventing a running transition", () => {
  vi.useFakeTimers();
  vi.setSystemTime(new Date("2026-01-01T00:00:00Z"));
  const request = creationEvent("provider_create_request");
  vi.advanceTimersByTime(250);
  const response = creationEvent("provider_create_response");
  expect(Date.parse(response.at) - Date.parse(request.at)).toBe(250);
  expect(observedRunning("pending")).toEqual([]);
  expect(observedRunning("RUNNING")).toEqual([
    { phase: "instance_running", at: response.at, source: "provider_observation" },
  ]);
});

it("accepts bounded client/guest events once and cannot replace coordinator facts", () => {
  const createdAt = "2026-01-01T00:00:00Z";
  const now = Date.parse(createdAt) + 20_000;
  const lease = {
    createdAt,
    creationEvents: [{ phase: "admission_started", at: createdAt, source: "coordinator" }],
  } as LeaseRecord;
  const events = [
    { phase: "ssh_tcp_accept", at: createdAt, source: "client" },
    { phase: "bootstrap_complete", at: createdAt, source: "guest" },
  ];
  mergeClientCreationEvents(lease, events, now);
  mergeClientCreationEvents(
    lease,
    events.map((e) => ({ ...e, at: new Date(now).toISOString() })),
    now,
  );
  expect(lease.creationEvents).toHaveLength(3);
  expect(lease.creationEvents?.[1]?.at).toBe(new Date(createdAt).toISOString());
  for (const item of [
    { phase: ["workspace_ready"], at: createdAt, source: "client" },
    { phase: "admission_started", at: createdAt, source: "client" },
    { phase: "workspace_ready", at: createdAt, source: "coordinator" },
    { phase: "workspace_ready", at: "invalid", source: "client" },
    { phase: "workspace_ready", at: "2025-01-01T00:00:00Z", source: "client" },
    { phase: "workspace_ready", at: "2027-01-01T00:00:00Z", source: "client" },
  ])
    mergeClientCreationEvents(lease, [item], now);
  expect(lease.creationEvents).toHaveLength(3);
});
