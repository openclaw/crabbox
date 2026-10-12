import { json } from "./http";

type DurableObjectFailureKind = "overloaded" | "unavailable" | "internal_error";

type ErrorDetails = {
  name?: unknown;
  message?: unknown;
  overloaded?: unknown;
  retryable?: unknown;
  remote?: unknown;
};

function errorDetails(error: unknown): ErrorDetails {
  return error !== null && typeof error === "object" ? (error as ErrorDetails) : {};
}

function errorMessage(error: unknown): string {
  const message = errorDetails(error).message;
  return typeof message === "string" ? message : typeof error === "string" ? error : "";
}

export function classifyDurableObjectFailure(error: unknown): DurableObjectFailureKind {
  const details = errorDetails(error);
  const message = errorMessage(error);
  if (details.overloaded === true || /Durable Object is overloaded/i.test(message)) {
    return "overloaded";
  }
  if (
    details.retryable === true ||
    /Durable Object.*\b(reset|exceeded)\b/i.test(message) ||
    /Internal error in Durable Object storage/i.test(message) ||
    /Network connection lost/i.test(message) ||
    /Durable Object storage operation exceeded timeout/i.test(message)
  ) {
    return "unavailable";
  }
  return "internal_error";
}

export function logDurableObjectFailure(
  error: unknown,
  request: Request,
): DurableObjectFailureKind {
  const kind = classifyDurableObjectFailure(error);
  const details = errorDetails(error);
  console.error(
    JSON.stringify({
      event: "coordinator.durable_object_failure",
      kind,
      method: request.method,
      path: new URL(request.url).pathname,
      retryable: details.retryable === true,
      overloaded: details.overloaded === true,
      remote: details.remote === true,
      name: typeof details.name === "string" ? details.name : "",
      message: errorMessage(error).slice(0, 200),
    }),
  );
  return kind;
}

export function durableObjectFailureResponse(kind: DurableObjectFailureKind): Response {
  const headers = new Headers({ "cache-control": "no-store" });
  if (kind === "internal_error") {
    return json({ error: "coordinator_internal_error" }, { status: 500, headers });
  }
  const overloaded = kind === "overloaded";
  headers.set("retry-after", overloaded ? "5" : "2");
  return json(
    {
      error: overloaded ? "coordinator_overloaded" : "coordinator_unavailable",
      message: overloaded
        ? "The coordinator is overloaded. Please retry shortly."
        : "The coordinator is temporarily unavailable. Please retry shortly.",
      retryable: true,
    },
    { status: 503, headers },
  );
}
