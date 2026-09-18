import { describe, expect, it, vi } from "vitest";

import { coordinatorStorageEntries } from "../src/storage-scan";
import { ProvisioningTestStorage } from "./provisioning-fixtures";

function records(count: number): ProvisioningTestStorage {
  const storage = new ProvisioningTestStorage();
  for (let index = 0; index < count; index++) storage.values.set(`record:${index}`, index);
  storage.values.set("unrelated:0", "untouched");
  return storage;
}

async function collect(storage: ProvisioningTestStorage): Promise<number[]> {
  const values: number[] = [];
  for await (const [, value] of coordinatorStorageEntries<number>(storage, {
    prefix: "record:",
    limit: 2,
    noCache: true,
  })) {
    values.push(value);
  }
  return values;
}

describe("coordinator storage scans", () => {
  it("preserves key order, bounds, and cache policy across partial pages", async () => {
    const storage = records(5);
    expect(await collect(storage)).toEqual([0, 1, 2, 3, 4]);
    expect(storage.listOptions).toEqual([
      { prefix: "record:", limit: 2, noCache: true },
      { prefix: "record:", limit: 2, noCache: true, startAfter: "record:1" },
      { prefix: "record:", limit: 2, noCache: true, startAfter: "record:3" },
    ]);
  });

  it("terminates an empty scan and checks the page after an exact multiple", async () => {
    const empty = records(0);
    expect(await collect(empty)).toEqual([]);
    expect(empty.listOptions).toHaveLength(1);
    const exact = records(4);
    expect(await collect(exact)).toEqual([0, 1, 2, 3]);
    expect(exact.listOptions).toHaveLength(3);
  });

  it("does not prefetch when the consumer stops at a page boundary", async () => {
    const storage = records(5);
    const seen: number[] = [];
    for await (const [, value] of coordinatorStorageEntries<number>(storage, {
      prefix: "record:",
      limit: 2,
    })) {
      seen.push(value);
      if (seen.length === 2) break;
    }
    expect(seen).toEqual([0, 1]);
    expect(storage.listOptions).toEqual([{ prefix: "record:", limit: 2 }]);
  });

  it("continues after deleted keys within the caller's transaction", async () => {
    const storage = records(5);
    await storage.transaction(async (transaction) => {
      for await (const [key] of coordinatorStorageEntries(transaction, {
        prefix: "record:",
        limit: 2,
      })) {
        await transaction.delete(key);
      }
    });
    expect([...storage.values]).toEqual([["unrelated:0", "untouched"]]);
    expect(storage.listOptions).toHaveLength(3);
  });

  it("does not publish transaction mutations when the consumer fails", async () => {
    const storage = records(2);
    const failure = new Error("consumer failed");
    await expect(
      storage.transaction(async (transaction) => {
        for await (const [key] of coordinatorStorageEntries(transaction, {
          prefix: "record:",
          limit: 2,
        })) {
          await transaction.delete(key);
          throw failure;
        }
      }),
    ).rejects.toBe(failure);
    expect([...storage.values.keys()]).toEqual(["record:0", "record:1", "unrelated:0"]);
  });

  it("rejects a backing store that repeats a full page", async () => {
    const storage = records(1);
    const list = vi.spyOn(storage, "list").mockResolvedValue(
      new Map([
        ["record:0", 0],
        ["record:1", 1],
      ]),
    );
    await expect(collect(storage)).rejects.toThrow("record: record scan did not advance");
    expect(list).toHaveBeenCalledTimes(2);
  });

  it("preserves storage read failures", async () => {
    const storage = records(1);
    const failure = new Error("read failed");
    vi.spyOn(storage, "list").mockRejectedValue(failure);
    await expect(collect(storage)).rejects.toBe(failure);
  });
});
