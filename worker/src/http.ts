export function json(data: unknown, init: ResponseInit = {}): Response {
  const headers = new Headers(init.headers);
  headers.set("content-type", "application/json; charset=utf-8");
  return new Response(JSON.stringify(data), { ...init, headers });
}

export function text(message: string, status = 200): Response {
  return new Response(message, {
    status,
    headers: { "content-type": "text/plain; charset=utf-8" },
  });
}

export async function readJson<T>(request: Request): Promise<T> {
  const value = (await request.json()) as unknown;
  return value as T;
}

export function bearerToken(request: Request): string {
  const header = request.headers.get("authorization") ?? "";
  const [scheme, token] = header.split(" ", 2);
  if (scheme?.toLowerCase() !== "bearer" || !token) {
    return "";
  }
  return token;
}

export function requestOwner(request: Request): string {
  return (
    request.headers.get("x-crabbox-owner") ??
    request.headers.get("cf-access-authenticated-user-email") ??
    "unknown"
  );
}

export function pathParts(request: Request): string[] {
  return new URL(request.url).pathname.split("/").filter(Boolean);
}

export function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
