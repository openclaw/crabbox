/// <reference types="node/async_hooks" />
import { AsyncLocalStorage } from "node:async_hooks";

export const providerRequestTimeoutMs = 60_000;

export class ProviderRequestTimeoutError extends Error {
  readonly retryable = true;

  constructor(
    readonly provider: string,
    readonly operation: string,
  ) {
    super(
      `${provider} ${operation} timed out; the outcome of a mutation may be unknown. Retry the request.`,
    );
    this.name = "ProviderRequestTimeoutError";
  }
}

const operationDeadline = new AsyncLocalStorage<number>();

export function withProviderOperationDeadline<T>(
  timeoutMs: number,
  fn: () => Promise<T>,
): Promise<T> {
  return operationDeadline.run(
    Math.min(Date.now() + timeoutMs, operationDeadline.getStore() ?? Infinity),
    fn,
  );
}

export function providerRequestSignal(
  timeoutMs: number,
  callerSignal: AbortSignal | null | undefined,
  provider: string,
  operation: string,
): AbortSignal {
  const remaining = Math.max(
    0,
    Math.min(timeoutMs, (operationDeadline.getStore() ?? Infinity) - Date.now()),
  );
  const timeout = new AbortController();
  const expire = () => timeout.abort(new ProviderRequestTimeoutError(provider, operation));
  if (remaining === 0) expire();
  else AbortSignal.timeout(Math.ceil(remaining)).addEventListener("abort", expire, { once: true });
  return callerSignal ? AbortSignal.any([callerSignal, timeout.signal]) : timeout.signal;
}

// Only race transport work (credentials, signing, fetch, body reads), never an operation
// that can resume and commit coordinator state after its owner releases the mutex.
export async function waitForProviderSignal<T>(
  signal: AbortSignal,
  operation: () => Promise<T>,
): Promise<T> {
  signal.throwIfAborted();
  let aborted!: () => void;
  const stopped = new Promise<never>((_, reject) => {
    aborted = () => reject(signal.reason);
    signal.addEventListener("abort", aborted, { once: true });
  });
  try {
    return await Promise.race([operation(), stopped]);
  } finally {
    signal.removeEventListener("abort", aborted);
  }
}

export async function providerSleep(ms: number, signal: AbortSignal): Promise<void> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    await waitForProviderSignal(
      signal,
      () =>
        new Promise<void>((resolve) => {
          timer = setTimeout(resolve, ms);
        }),
    );
  } finally {
    if (timer !== undefined) clearTimeout(timer);
  }
}

export async function providerFetch(
  input: RequestInfo | URL,
  init: RequestInit | undefined,
  signal: AbortSignal,
): Promise<Response> {
  const response = await waitForProviderSignal(signal, () => fetch(input, { ...init, signal }));
  return providerResponse(response, signal);
}

export function providerResponse(response: Response, signal: AbortSignal): Response {
  if (!response.body) return response;
  const reader = response.body.getReader();
  let aborted!: () => void;
  const body = new ReadableStream<Uint8Array>(
    {
      start(controller) {
        aborted = () => {
          controller.error(signal.reason);
          void reader.cancel(signal.reason).catch(() => undefined);
        };
        if (signal.aborted) aborted();
        else signal.addEventListener("abort", aborted, { once: true });
      },
      async pull(controller) {
        try {
          const chunk = await waitForProviderSignal(signal, () => reader.read());
          if (chunk.done) {
            signal.removeEventListener("abort", aborted);
            controller.close();
          } else controller.enqueue(chunk.value);
        } catch (error) {
          signal.removeEventListener("abort", aborted);
          controller.error(signal.aborted ? signal.reason : error);
        }
      },
      cancel(reason) {
        signal.removeEventListener("abort", aborted);
        return reader.cancel(reason);
      },
    },
    { highWaterMark: 0 },
  );
  const bounded = new Response(body, response);
  // Preserve fetch metadata while making all body consumers observe the same deadline.
  for (const key of ["url", "redirected", "type"] as const) {
    Object.defineProperty(bounded, key, { value: response[key] });
  }
  return bounded;
}
