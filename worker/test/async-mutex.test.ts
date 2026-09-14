import { describe, expect, it } from "vitest";

import { AsyncMutex, KeyedAsyncMutex } from "../src/async-mutex";

describe("operation mutexes", () => {
  it("preserves FIFO ordering and releases after a rejected operation", async () => {
    const mutex = new AsyncMutex();
    const events: number[] = [];
    let release!: () => void;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    const failure = new Error("operation failed");
    const first = mutex.run(async () => {
      events.push(1);
      await gate;
      throw failure;
    });
    const firstOutcome = first.catch((error: unknown) => error);
    const second = mutex.run(async () => {
      events.push(2);
      return "second";
    });
    const third = mutex.run(async () => {
      events.push(3);
    });
    await Promise.resolve();
    expect(events).toEqual([1]);
    release();
    expect(await firstOutcome).toBe(failure);
    expect(await second).toBe("second");
    await third;
    await mutex.drain();
    expect(events).toEqual([1, 2, 3]);
  });

  it("keeps different keys concurrent and all same-key waiters serialized", async () => {
    const mutex = new KeyedAsyncMutex<string>();
    const events: string[] = [];
    let releaseFirst!: () => void;
    let releaseSecond!: () => void;
    const firstGate = new Promise<void>((resolve) => {
      releaseFirst = resolve;
    });
    const secondGate = new Promise<void>((resolve) => {
      releaseSecond = resolve;
    });
    const first = mutex.run("same", async () => {
      events.push("first");
      await firstGate;
    });
    const second = mutex.run("same", async () => {
      events.push("second");
      await secondGate;
    });
    await mutex.run("other", async () => {
      events.push("other");
    });
    expect(events).toEqual(["first", "other"]);
    releaseFirst();
    await first;
    const third = mutex.run("same", async () => {
      events.push("third");
    });
    await Promise.resolve();
    expect(events).toEqual(["first", "other", "second"]);
    releaseSecond();
    await Promise.all([second, third]);
    expect(events).toEqual(["first", "other", "second", "third"]);
    await expect(mutex.run("same", async () => "reused")).resolves.toBe("reused");
  });

  it("releases a keyed turn after a synchronous throw", async () => {
    const mutex = new KeyedAsyncMutex<string>();
    const failure = new Error("synchronous failure");
    await expect(
      mutex.run("key", () => {
        throw failure;
      }),
    ).rejects.toBe(failure);
    await expect(mutex.run("key", async () => 7)).resolves.toBe(7);
  });
});
