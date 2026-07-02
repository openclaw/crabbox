const runtimeAdapterIDPattern = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/;

export const runtimeAdapterRelayBodyLimit = 64 * 1024;
// Covers a maximally JSON-escaped body plus the bounded response envelope.
export const runtimeAdapterRelayFrameLimit = 512 * 1024;
// The connector allows ordinary local calls 9s, then needs up to 5s to write
// the response back over the relay WebSocket.
export const runtimeAdapterRelayTimeoutMs = 14_000;
export const runtimeAdapterDesktopRelayTimeoutMs = 150_000;
export const runtimeAdapterDesktopRelayMaxTimeoutMs = 24 * 60 * 60 * 1_000 + 35_000;

export type RuntimeAdapterRelayRequest = {
  type: "request";
  id: string;
  method: "GET" | "POST" | "DELETE";
  path: string;
  /** Absolute Unix epoch deadline. The connector must not start expired work. */
  deadlineMs: number;
  headers?: Record<string, string>;
  body?: string;
};

export type RuntimeAdapterRelayResponse = {
  type: "response";
  id: string;
  status: number;
  headers?: Record<string, string>;
  body?: string;
};

export function validRuntimeAdapterID(value: unknown): value is string {
  return typeof value === "string" && runtimeAdapterIDPattern.test(value);
}

export function runtimeAdapterProxyPath(parts: string[]): string | undefined {
  if (parts[0] !== "v1" || parts[1] !== "workspaces") {
    return undefined;
  }
  if (parts.length === 2) {
    return "/v1/workspaces";
  }
  if (!validRuntimeAdapterID(parts[2])) {
    return undefined;
  }
  if (parts.length === 3) {
    return `/v1/workspaces/${parts[2]}`;
  }
  if (
    parts.length === 5 &&
    parts[3] === "connections" &&
    (parts[4] === "desktop" || parts[4] === "native-vnc")
  ) {
    return `/v1/workspaces/${parts[2]}/connections/${parts[4]}`;
  }
  return undefined;
}

export function runtimeAdapterRelayMethodAllowed(method: string, path: string): boolean {
  if (path === "/v1/workspaces") {
    return method === "POST";
  }
  if (path.endsWith("/connections/desktop") || path.endsWith("/connections/native-vnc")) {
    return method === "POST";
  }
  return method === "GET" || method === "DELETE";
}

export function runtimeAdapterRelayBodyAllowed(
  method: string,
  path: string,
  body: string | undefined,
): boolean {
  return body === undefined || body === "" || (method === "POST" && path === "/v1/workspaces");
}

export function validRuntimeAdapterDesktopRelayTimeout(value: unknown): value is number {
  return (
    typeof value === "number" &&
    Number.isSafeInteger(value) &&
    value >= runtimeAdapterRelayTimeoutMs &&
    value <= runtimeAdapterDesktopRelayMaxTimeoutMs
  );
}

export function runtimeAdapterRelayTimeoutForPath(path: string, desktopTimeoutMs?: number): number {
  if (!path.endsWith("/connections/desktop") && !path.endsWith("/connections/native-vnc")) {
    return runtimeAdapterRelayTimeoutMs;
  }
  return validRuntimeAdapterDesktopRelayTimeout(desktopTimeoutMs)
    ? desktopTimeoutMs
    : runtimeAdapterDesktopRelayTimeoutMs;
}

export function runtimeAdapterRelayHeaders(request: Request): Record<string, string> | undefined {
  const idempotencyKey = request.headers.get("idempotency-key")?.trim();
  if (!idempotencyKey) return undefined;
  if (idempotencyKey.length > 128) {
    throw new RangeError("runtime adapter idempotency key is too long");
  }
  return { "idempotency-key": idempotencyKey };
}

export function runtimeAdapterRelayContentType(
  headers: Record<string, string> | undefined,
): string | undefined {
  return Object.entries(headers ?? {}).find(([key]) => key.toLowerCase() === "content-type")?.[1];
}

export async function readRuntimeAdapterRelayBody(request: Request): Promise<string | undefined> {
  if (!request.body) {
    return undefined;
  }
  const declared = Number(request.headers.get("content-length"));
  if (Number.isFinite(declared) && declared > runtimeAdapterRelayBodyLimit) {
    throw new RangeError("runtime adapter request body is too large");
  }
  const reader = request.body.getReader();
  const chunks: Uint8Array[] = [];
  let size = 0;
  while (true) {
    // eslint-disable-next-line no-await-in-loop -- a bounded stream must be consumed in order.
    const { done, value } = await reader.read();
    if (done) break;
    size += value.byteLength;
    if (size > runtimeAdapterRelayBodyLimit) {
      void reader.cancel();
      throw new RangeError("runtime adapter request body is too large");
    }
    chunks.push(value);
  }
  const body = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) {
    body.set(chunk, offset);
    offset += chunk.byteLength;
  }
  try {
    return new TextDecoder("utf-8", { fatal: true, ignoreBOM: false }).decode(body);
  } catch {
    throw new TypeError("runtime adapter request body must be valid UTF-8");
  }
}

export function validRuntimeAdapterRelayResponse(
  value: unknown,
  expectedID: string,
): value is RuntimeAdapterRelayResponse {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const response = value as Partial<RuntimeAdapterRelayResponse>;
  const headers = response.headers;
  if (
    headers !== undefined &&
    (!headers ||
      typeof headers !== "object" ||
      Array.isArray(headers) ||
      Object.entries(headers).some(
        ([key, header]) =>
          key.toLowerCase() !== "content-type" ||
          typeof header !== "string" ||
          /[\r\n]/.test(header) ||
          new TextEncoder().encode(header).byteLength > 256,
      ))
  ) {
    return false;
  }
  return Boolean(
    response.type === "response" &&
    response.id === expectedID &&
    Number.isInteger(response.status) &&
    (response.status ?? 0) >= 200 &&
    (response.status ?? 0) <= 599 &&
    (response.body === undefined ||
      (typeof response.body === "string" &&
        new TextEncoder().encode(response.body).byteLength <= runtimeAdapterRelayBodyLimit)),
  );
}
