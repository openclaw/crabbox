import { afterEach, expect, it, vi } from "vitest";

import {
  appendCreationSteps,
  withCreationSteps,
  measureCreationStep,
  recordCreationStep,
  creationEvent,
  mergeClientCreationEvents,
  observedRunning,
} from "../src/creation-events";
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

it("aggregates bounded coordinator steps without exposing results or errors", async () => {
  vi.useFakeTimers();
  vi.setSystemTime(0);
  await withCreationSteps(async () => {
    const secret = new Error("synthetic-private-error");
    await expect(
      measureCreationStep("test.step", async () => {
        vi.advanceTimersByTime(25);
        throw secret;
      }),
    ).rejects.toBe(secret);
    const result = await measureCreationStep("test.step", async () => {
      vi.advanceTimersByTime(75);
      return "synthetic-private-result";
    });
    expect(result).toBe("synthetic-private-result");
    const lease: Pick<LeaseRecord, "creationEvents"> = {};
    appendCreationSteps(lease);
    expect(lease.creationEvents).toEqual([
      {
        phase: "coordinator_step",
        source: "coordinator",
        at: new Date(100).toISOString(),
        step: "test.step",
        durationMs: 100,
        count: 2,
        errors: 1,
      },
    ]);
    recordCreationStep("test.step", 0);
    expect(lease.creationEvents?.[0]?.count).toBe(2);
    for (let i = 0; i < 1000; i++) recordCreationStep(`test.step${i}`, 1);
    appendCreationSteps(lease);
    expect(lease.creationEvents).toHaveLength(64);
    expect(JSON.stringify(lease)).not.toContain("synthetic-private");
    expect(JSON.stringify(lease).length).toBeLessThan(16384);
  });
});

it("isolates concurrent request timings and ignores out-of-scope measurements", async () => {
  let release!: () => void;
  const gate = new Promise<void>((resolve) => {
    release = resolve;
  });
  const first = withCreationSteps(async () => {
    await measureCreationStep("test.first", () => gate);
    const lease: Pick<LeaseRecord, "creationEvents"> = {};
    appendCreationSteps(lease);
    return lease;
  });
  const second = await withCreationSteps(async () => {
    await measureCreationStep("test.second", async () => {});
    const lease: Pick<LeaseRecord, "creationEvents"> = {};
    appendCreationSteps(lease);
    return lease;
  });
  release();
  expect((await first).creationEvents?.map((e) => e.step)).toEqual(["test.first"]);
  expect(second.creationEvents?.map((e) => e.step)).toEqual(["test.second"]);
  recordCreationStep("test.outside", 10);
  const outside = {};
  appendCreationSteps(outside);
  expect(outside).toEqual({});
});

it("rejects forged coordinator steps and strips timing payloads from client observations", () => {
  const at = new Date().toISOString();
  const lease = { createdAt: at } as LeaseRecord;
  mergeClientCreationEvents(
    lease,
    [
      { phase: "coordinator_step", source: "coordinator", at, step: "aws.image", durationMs: 123 },
      { phase: "workspace_ready", source: "client", at, step: "aws.image", durationMs: 123 },
    ],
    Date.now(),
  );
  expect(lease.creationEvents).toEqual([{ phase: "workspace_ready", source: "client", at }]);
});
