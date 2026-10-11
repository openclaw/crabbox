import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { AzureClient } from "../src/azure";
import {
  ProviderRequestTimeoutError,
  providerRequestSignal,
  providerRequestTimeoutMs,
  waitForProviderSignal,
  withProviderOperationDeadline,
} from "../src/provider-deadline";

beforeEach(() => {
  vi.useFakeTimers();
  // Native AbortSignal.timeout uses Node's internal clock; route it through the test clock.
  vi.spyOn(AbortSignal, "timeout").mockImplementation((ms) => {
    const controller = new AbortController();
    setTimeout(() => controller.abort(new DOMException("timed out", "TimeoutError")), ms);
    return controller.signal;
  });
});
afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

it("takes the earlier nested deadline without leaking into concurrent operations", async () => {
  const scoped = withProviderOperationDeadline(20, () =>
    withProviderOperationDeadline(100, async () =>
      providerRequestSignal(1_000, undefined, "test", "nested"),
    ),
  );
  const independent = providerRequestSignal(1_000, undefined, "test", "independent");
  const signal = await scoped;
  await vi.advanceTimersByTimeAsync(20);
  expect(signal.reason).toBeInstanceOf(ProviderRequestTimeoutError);
  expect(independent.aborted).toBe(false);
});

it("does not start transport work after an ambient deadline has expired", async () => {
  const operation = vi.fn<() => Promise<string>>(async () => "unexpected");
  await expect(
    withProviderOperationDeadline(0, () =>
      waitForProviderSignal(providerRequestSignal(1_000, undefined, "test", "expired"), operation),
    ),
  ).rejects.toBeInstanceOf(ProviderRequestTimeoutError);
  expect(operation).not.toHaveBeenCalled();
});

describe("Azure default transport deadlines", () => {
  const env = {
    AZURE_TENANT_ID: "tenant",
    AZURE_CLIENT_ID: "client",
    AZURE_CLIENT_SECRET: "secret",
    AZURE_SUBSCRIPTION_ID: "subscription",
  };
  it.each(["request", "token", "body"])(
    "bounds stalled %s I/O without a caller signal",
    async (stage) => {
      const client = new AzureClient(env);
      const fetchMock = vi.fn<() => Promise<Response>>(() =>
        stage === "body"
          ? Promise.resolve(new Response(new ReadableStream()))
          : new Promise<Response>(() => {}),
      );
      vi.stubGlobal("fetch", fetchMock);
      const operation =
        stage === "token"
          ? (Reflect.get(client, "token") as () => Promise<string>).call(client)
          : client.fetcher("https://management.azure.com/test").then((response) => response.text());
      await Promise.all([
        expect(operation).rejects.toBeInstanceOf(ProviderRequestTimeoutError),
        vi.advanceTimersByTimeAsync(providerRequestTimeoutMs),
      ]);
      expect(fetchMock).toHaveBeenCalledTimes(1);
    },
  );
  it("preserves the earlier create deadline and its existing error", async () => {
    const client = new AzureClient(env);
    Reflect.set(client, "createDeadline", Date.now() + 5);
    vi.stubGlobal(
      "fetch",
      vi.fn(() => new Promise(() => {})),
    );
    await Promise.all([
      expect((Reflect.get(client, "token") as () => Promise<string>).call(client)).rejects.toThrow(
        "Azure provisioning deadline exceeded after 25m",
      ),
      vi.advanceTimersByTimeAsync(5),
    ]);
  });
});
