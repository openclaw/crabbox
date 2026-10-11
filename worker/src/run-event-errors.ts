import { json } from "./http";

export function runEventAppendFailureResponse(request: Request): Response | undefined {
  if (
    (request.method !== "POST" && request.method !== "PUT") ||
    !/^\/v1\/runs\/[^/]+\/events\/?$/.test(new URL(request.url).pathname)
  ) {
    return undefined;
  }
  // A reset can follow a durable write. Only a client with a stable idempotency key can safely replay.
  return json({ error: "run_event_append_unavailable" }, { status: 503 });
}
