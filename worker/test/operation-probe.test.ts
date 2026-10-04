import { afterEach, describe, expect, it, vi } from "vitest";

import {
  observeOperation,
  observeOperationSync,
  startOperationProbe,
} from "../src/operation-probe";
import { coordinatorStorageEntries } from "../src/storage-scan";

afterEach(() => vi.restoreAllMocks());

describe("temporary operation-only diagnostics", () => {
  it("emits only fixed labels and scalar measurements without traversing result records", async () => {
    const log = vi.spyOn(console, "debug").mockImplementation(() => undefined);
    const value = new Map([
      [
        "private-owner",
        {
          token: "secret-sentinel",
          toJSON() {
            throw new Error("must not serialize records");
          },
        },
      ],
    ]);
    expect(
      await observeOperation(
        "lease_history",
        async () => value,
        (rows) => ({ count: rows.size }),
      ),
    ).toBe(value);
    const messages = log.mock.calls.map(([line]) => JSON.parse(String(line)));
    expect(messages).toEqual([
      { component: "crabbox_operation_probe", operation: "lease_history", phase: "start" },
      {
        component: "crabbox_operation_probe",
        operation: "lease_history",
        phase: "end",
        durationMs: expect.any(Number),
        count: 1,
      },
    ]);
    expect(JSON.stringify(messages)).not.toMatch(/private-owner|secret-sentinel|token/);
  });

  it("preserves values and the exact rejection when the log sink throws", async () => {
    vi.spyOn(console, "debug").mockImplementation(() => {
      throw new Error("logging failed");
    });
    const value = {};
    expect(await observeOperation("runner_history", async () => value)).toBe(value);
    expect(observeOperationSync("aws_ec2_parse", () => value)).toBe(value);
    const failure = new Error("private provider error");
    await expect(
      observeOperation("aws_ec2_body", async () => {
        throw failure;
      }),
    ).rejects.toBe(failure);
    expect(() =>
      observeOperationSync("aws_ec2_parse", () => {
        throw failure;
      }),
    ).toThrow(failure);
  });

  it("does not expose failures or unexpected measurement properties", async () => {
    const log = vi.spyOn(console, "debug").mockImplementation(() => undefined);
    const failure = new Error("credential-sentinel");
    await expect(
      observeOperation("aws_ec2_body", async () => {
        throw failure;
      }),
    ).rejects.toBe(failure);
    startOperationProbe("lease_history")({
      count: -1,
      codeUnits: Infinity,
      token: "secret-sentinel",
    } as never);
    startOperationProbe("owner-sentinel" as never)({ count: 1 });
    const messages = log.mock.calls.map(([line]) => JSON.parse(String(line)));
    expect(messages[1]).toMatchObject({ phase: "error" });
    expect(messages.at(-1)).toEqual({
      component: "crabbox_operation_probe",
      operation: "lease_history",
      phase: "end",
      durationMs: expect.any(Number),
    });
    expect(JSON.stringify(messages)).not.toMatch(
      /credential-sentinel|secret-sentinel|owner-sentinel/,
    );
  });

  it("keeps observation failures separate from operation success", async () => {
    const log = vi.spyOn(console, "debug").mockImplementation(() => undefined);
    const value = {};
    expect(
      await observeOperation(
        "runner_history",
        async () => value,
        () => {
          throw new Error("measurement failed");
        },
      ),
    ).toBe(value);
    expect(JSON.parse(String(log.mock.calls.at(-1)?.[0]))).toMatchObject({ phase: "end" });
  });

  it("observes one complete lease scan without extra reads or changing early iterator close", async () => {
    const log = vi.spyOn(console, "debug").mockImplementation(() => undefined);
    const list = vi.fn<() => Promise<Map<string, unknown>>>(
      async () =>
        new Map([
          ["lease:one", { private: "sentinel" }],
          ["lease:two", {}],
        ]),
    );
    for await (const [key] of coordinatorStorageEntries({ list } as never, {
      prefix: "lease:",
      limit: 2,
      noCache: true,
    })) {
      expect(key).toBe("lease:one");
      break;
    }
    expect(list).toHaveBeenCalledExactlyOnceWith({ prefix: "lease:", limit: 2, noCache: true });
    expect(log).toHaveBeenCalledTimes(2);
    expect(JSON.parse(String(log.mock.calls[1]?.[0]))).toMatchObject({
      operation: "lease_scan",
      phase: "end",
      count: 2,
    });
  });
});
