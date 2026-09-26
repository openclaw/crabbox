import { createServer, type RequestListener } from "node:http";
import type { AddressInfo } from "node:net";

import { expect, vi } from "vitest";

export async function withProviderHTTP(
  origins: string[],
  handler: RequestListener,
  test: () => Promise<void>,
): Promise<void> {
  const server = createServer(handler);
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const { port } = server.address() as AddressInfo;
  const nativeFetch = globalThis.fetch;
  try {
    vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) => {
      const wire = input instanceof Request ? input : new Request(input, init);
      const url = new URL(wire.url);
      expect(origins).toContain(url.origin);
      return nativeFetch(new Request(`http://127.0.0.1:${port}${url.pathname}${url.search}`, wire));
    });
    await test();
  } finally {
    await new Promise<void>((resolve, reject) =>
      server.close((error) => (error ? reject(error) : resolve())),
    );
  }
}
