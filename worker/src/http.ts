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

const maxDiagnosticRedactionPasses = 64;
type DiagnosticRedactionRange = { start: number; end: number; preserve: boolean };

export function redactDiagnosticSecrets(
  value: string,
  secrets: readonly (string | undefined)[] = [],
): string {
  const exactSecrets = [
    ...new Set(
      secrets.flatMap((secret) => {
        const trimmed = secret?.trim();
        return trimmed ? [trimmed] : [];
      }),
    ),
  ].toSorted((left, right) => right.length - left.length);
  let redacted = value;
  for (let pass = 0; pass < maxDiagnosticRedactionPasses; pass++) {
    const next = redactDiagnosticPass(redacted, exactSecrets);
    if (next === redacted) return next;
    redacted = next;
  }
  return "[redacted]";
}

function redactDiagnosticPass(value: string, secrets: readonly string[]): string {
  // Collect structural and exact spans from the same source text so one
  // rewrite cannot hide a still-unredacted part of another credential.
  const markerRanges = diagnosticMarkerRanges(value);
  const ranges = [...markerRanges];
  let added = false;
  const addRedaction = (start: number, end: number) => {
    if (start >= end || diagnosticRangeInsideMarker(start, end, markerRanges)) return;
    ranges.push({ start, end, preserve: false });
    added = true;
  };

  for (const match of value.matchAll(
    /(^|[^?&A-Za-z0-9_-])(authorization|proxy-authorization)[ \t]*[:=][ \t]*[!#$%&'*+.^_`|~0-9A-Za-z-]+(?:(?:\r?\n[ \t]+)|[^\r\n])*/gi,
  )) {
    const full = match[0];
    const keyEnd = (match[1]?.length ?? 0) + (match[2]?.length ?? 0);
    const separator = diagnosticSeparator(full, keyEnd);
    if (separator < 0) continue;
    const scheme = full
      .slice(separator + 1)
      .trim()
      .split(/\s+/, 1)[0]
      ?.replace(/:$/, "")
      .toLowerCase();
    if (scheme === "bearer" || scheme === "basic") continue;
    const fullStart = match.index;
    addRedaction(
      fullStart + diagnosticSkipHorizontalSpace(full, separator + 1, full.length),
      fullStart + full.length,
    );
  }
  for (const match of value.matchAll(
    /(^|[^?&A-Za-z0-9_-])(authorization|proxy-authorization|x-api-key|api[-_]?key|access[-_]?token|refresh[-_]?token|id[-_]?token|client[-_]?secret|secret[-_]?access[-_]?key|api[-_]?secret|session[-_]?token|token|password)[ \t]*[:=][ \t]*(?:(?:bearer|basic)(?:[ \t]*:[ \t]*\r?\n[ \t]+|[ \t]*:[ \t]*|[ \t]*\r?\n[ \t]+|[ \t]+))?(?:\\.|[^\s"])+/gi,
  )) {
    const full = match[0];
    const keyEnd = (match[1]?.length ?? 0) + (match[2]?.length ?? 0);
    const separator = diagnosticSeparator(full, keyEnd);
    if (separator < 0) continue;
    const fullStart = match.index;
    addRedaction(
      fullStart + diagnosticSkipHorizontalSpace(full, separator + 1, full.length),
      fullStart + full.length,
    );
  }
  for (const match of value.matchAll(
    /"(authorization|proxy-authorization|x-api-key|apiKey|api-key|api_key|accessToken|access-token|access_token|refreshToken|refresh-token|refresh_token|idToken|id-token|id_token|clientSecret|client-secret|client_secret|secretAccessKey|secret-access-key|secret_access_key|apiSecret|api-secret|api_secret|credential|credentials|privateKey|private-key|private_key|secret|sessionToken|session-token|session_token|token|password)"\s*:\s*"(?:\\(?:[\s\S]|$)|[^"\\])*(?:"|$)/gi,
  )) {
    const full = match[0];
    const separator = full.indexOf(":");
    const quote = full.indexOf('"', separator + 1);
    if (separator < 0 || quote < 0) continue;
    const fullStart = match.index;
    const secretEnd = full.endsWith('"') ? full.length - 1 : full.length;
    addRedaction(fullStart + quote + 1, fullStart + secretEnd);
  }
  for (const match of value.matchAll(
    /([?&](?:authorization|proxy-authorization|x-api-key|api[_-]?key|access[_-]?token|refresh[_-]?token|id[_-]?token|client[_-]?secret|secret[_-]?access[_-]?key|api[_-]?secret|session[_-]?token|password|token|signature|sig|x-amz-credential|x-amz-signature|x-amz-security-token|x-goog-credential|x-goog-signature|x-goog-security-token)=)[^&#\s]+/gi,
  )) {
    addRedaction(match.index + match[1]!.length, match.index + match[0].length);
  }
  for (const match of value.matchAll(/\b(https?:\/\/)([^/?#\s]+)@/gi)) {
    const userinfo = match[2]!;
    if (userinfo !== "[redacted]" && userinfo !== "<redacted>") {
      const start = match.index + match[1]!.length;
      addRedaction(start, start + userinfo.length);
    }
  }
  for (const match of value.matchAll(
    /\bbearer(?:[ \t]*:[ \t]*\r?\n[ \t]+|[ \t]*:[ \t]*|[ \t]*\r?\n[ \t]+|[ \t]+)((?:\\.|[^\s"])+)/gi,
  )) {
    const credential = match[1]!;
    const end = match.index + match[0].length;
    addRedaction(end - credential.length, end);
  }
  for (const match of value.matchAll(
    /-----BEGIN ([A-Z0-9 ]*PRIVATE KEY)-----[\s\S]*?(?:-----END \1-----|$)/gi,
  )) {
    addRedaction(match.index, match.index + match[0].length);
  }
  for (const secret of secrets) {
    for (let offset = 0; offset < value.length; ) {
      const start = value.indexOf(secret, offset);
      if (start < 0) break;
      addRedaction(start, start + secret.length);
      offset = start + 1;
    }
  }
  return added ? applyDiagnosticRedactionRanges(value, ranges) : value;
}

function diagnosticMarkerRanges(value: string): DiagnosticRedactionRange[] {
  const ranges: DiagnosticRedactionRange[] = [];
  for (let offset = 0; offset < value.length; ) {
    const squareStart = value.indexOf("[redacted]", offset);
    const angleStart = value.indexOf("<redacted>", offset);
    if (squareStart < 0 && angleStart < 0) break;
    const squareIsFirst = squareStart >= 0 && (angleStart < 0 || squareStart < angleStart);
    const start = squareIsFirst ? squareStart : angleStart;
    const markerLength = squareIsFirst ? "[redacted]".length : "<redacted>".length;
    ranges.push({ start, end: start + markerLength, preserve: true });
    offset = start + markerLength;
  }
  return ranges;
}

function diagnosticRangeInsideMarker(
  start: number,
  end: number,
  markers: readonly DiagnosticRedactionRange[],
): boolean {
  let low = 0;
  let high = markers.length;
  while (low < high) {
    const middle = (low + high) >>> 1;
    if (markers[middle]!.start <= start) low = middle + 1;
    else high = middle;
  }
  const marker = markers[low - 1];
  return marker !== undefined && end <= marker.end;
}

function diagnosticSeparator(value: string, start: number): number {
  const colon = value.indexOf(":", start);
  const equals = value.indexOf("=", start);
  return colon < 0 ? equals : equals < 0 ? colon : Math.min(colon, equals);
}

function diagnosticSkipHorizontalSpace(value: string, start: number, end: number): number {
  while (start < end && (value[start] === " " || value[start] === "\t")) start++;
  return start;
}

function applyDiagnosticRedactionRanges(value: string, ranges: DiagnosticRedactionRange[]): string {
  ranges.sort((left, right) => left.start - right.start || left.end - right.end);
  const merged: DiagnosticRedactionRange[] = [];
  for (const candidate of ranges) {
    const previous = merged.at(-1);
    if (!previous || candidate.start >= previous.end) {
      merged.push({ ...candidate });
    } else {
      previous.preserve = previous.preserve && candidate.preserve;
      if (candidate.end > previous.end) previous.end = candidate.end;
    }
  }
  let redacted = "";
  let offset = 0;
  for (const range of merged) {
    const replacement = range.preserve ? value.slice(range.start, range.end) : "[redacted]";
    redacted += value.slice(offset, range.start) + replacement;
    offset = range.end;
  }
  return redacted + value.slice(offset);
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
