export function json(data: unknown, init: ResponseInit = {}): Response {
  const headers = new Headers(init.headers);
  headers.set("content-type", "application/json; charset=utf-8");
  return new Response(JSON.stringify(redactStackTraceFields(data)), { ...init, headers });
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
  return request.headers.get("x-crabbox-owner") ?? "unknown";
}

export function pathParts(request: Request): string[] {
  return new URL(request.url).pathname.split("/").filter(Boolean);
}

export function errorMessage(
  error: unknown,
  secrets: readonly (string | undefined)[] = [],
): string {
  return redactDiagnosticSecrets(
    firstLine(error instanceof Error ? error.message : String(error)),
    secrets,
  );
}

export function redactDiagnosticSecrets(
  value: string,
  secrets: readonly (string | undefined)[] = [],
): string {
  let redacted = value;
  const exactSecrets = [
    ...new Set(
      secrets.flatMap((secret) => {
        const trimmed = secret?.trim();
        return trimmed ? [trimmed] : [];
      }),
    ),
  ].toSorted((left, right) => right.length - left.length);
  for (const secret of exactSecrets) {
    redacted = redacted.replaceAll(secret, "[redacted]");
  }
  redacted = redacted.replaceAll(
    /(^|[^?&A-Za-z0-9_-])(authorization|proxy-authorization|x-api-key|api-key|api_key|access-token|access_token|client-secret|client_secret|session-token|session_token|token|password)[ \t]*[:=][ \t]*(?:(?:bearer|basic)(?:[ \t]*:[ \t]*\r?\n[ \t]+|[ \t]*:[ \t]*|[ \t]*\r?\n[ \t]+|[ \t]+))?(?:\\.|[^\s"])+/gi,
    (match, prefix: string) => {
      const colon = match.indexOf(":", prefix.length);
      const equals = match.indexOf("=", prefix.length);
      const separator = colon < 0 ? equals : equals < 0 ? colon : Math.min(colon, equals);
      return separator >= 0 ? `${match.slice(0, separator + 1)} [redacted]` : match;
    },
  );
  redacted = redacted.replaceAll(
    /"(authorization|proxy-authorization|x-api-key|apiKey|api-key|api_key|accessToken|access-token|access_token|clientSecret|client-secret|client_secret|credential|credentials|privateKey|private-key|private_key|secret|sessionToken|session-token|session_token|token|password)"\s*:\s*"(?:\\[\s\S]|[^"\\])*(?:"|$)/gi,
    (match) => {
      const separator = match.indexOf(":");
      return separator >= 0 ? `${match.slice(0, separator + 1)}"[redacted]"` : match;
    },
  );
  redacted = redacted.replaceAll(
    /([?&](?:authorization|proxy-authorization|x-api-key|api[_-]?key|access[_-]?token|client[_-]?secret|session[_-]?token|password|token|signature|sig|x-amz-credential|x-amz-signature|x-amz-security-token)=)[^&#\s]+/gi,
    "$1[redacted]",
  );
  redacted = redacted.replaceAll(/\b(https?:\/\/)[^/\s:@]+:[^@\s/]+@/gi, "$1[redacted]@");
  redacted = redacted.replaceAll(
    /\bbearer(?:[ \t]*:[ \t]*\r?\n[ \t]+|[ \t]*:[ \t]*|[ \t]*\r?\n[ \t]+|[ \t]+)(?:\\.|[^\s"])+/gi,
    "Bearer [redacted]",
  );
  return redacted.replaceAll(
    /-----BEGIN ([A-Z0-9 ]*PRIVATE KEY)-----[\s\S]*?(?:-----END \1-----|$)/gi,
    "[redacted]",
  );
}

function redactStackTraceFields(value: unknown, seen = new WeakSet<object>()): unknown {
  if (value instanceof Error) {
    return { name: value.name, message: firstLine(value.message) };
  }
  if (!value || typeof value !== "object") {
    return value;
  }
  if (seen.has(value)) {
    return "[Circular]";
  }
  seen.add(value);
  const jsonValue = (value as { toJSON?: () => unknown }).toJSON;
  if (typeof jsonValue === "function") {
    const next = jsonValue.call(value);
    if (next !== value) {
      const output = redactStackTraceFields(next, seen);
      seen.delete(value);
      return output;
    }
  }
  if (Array.isArray(value)) {
    const output = value.map((item) => redactStackTraceFields(item, seen));
    seen.delete(value);
    return output;
  }
  const output: Record<string, unknown> = {};
  for (const [key, item] of Object.entries(value)) {
    if (key !== "stack" && typeof item !== "function") {
      output[key] = redactStackTraceFields(item, seen);
    }
  }
  seen.delete(value);
  return output;
}

function firstLine(value: string): string {
  const index = value.indexOf("\n");
  return index >= 0 ? value.slice(0, index) : value;
}
