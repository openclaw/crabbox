import { json } from "./http";

export function runEventAppendFailureResponse(request: Request): Response | undefined {
  if (
    request.method !== "POST" ||
    !/^\/v1\/runs\/[^/]+\/events\/?$/.test(new URL(request.url).pathname)
  ) {
    return undefined;
  }
  // A reset can follow a durable write. Never replay an append or claim it was not recorded.
  return json({ error: "run_event_append_unavailable" }, { status: 503 });
}
