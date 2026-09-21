import { describe, expect, it, vi } from "vitest";

vi.mock("@cloudflare/containers", () => ({
  Container: class {
    constructor(readonly ctx: unknown) {}
  },
  getContainer: vi.fn<() => void>(),
}));
const { default: container } = await import("../src/cloudflare-container-runner");
const { default: dynamic } = await import("../src/cloudflare-dynamic-worker-runner");

function request(body: string, authorization = "Bearer synthetic-token"): Request {
  return new Request("https://runner.example/v1/sandboxes", {
    method: "POST",
    headers: { Authorization: authorization, "Content-Type": "application/json" },
    body,
  });
}

function fetchRunner(kind: "container" | "dynamic", body: string, auth: string, tokens = {}) {
  const env = { CRABBOX_RUNNER_TOKEN: "synthetic-token", ...tokens };
  if (kind === "container") {
    return container.fetch(request(body, auth), env as Parameters<typeof container.fetch>[1]);
  }
  return dynamic.fetch(
    new Request("https://runner.example/v1/runs", request(body, auth)),
    { ...env, LOADER: {}, RUN_COORDINATOR: {} } as Parameters<typeof dynamic.fetch>[1],
    {} as Parameters<typeof dynamic.fetch>[2],
  );
}

describe.each(["container", "dynamic"] as const)("D5 %s runner HTTP contract", (kind) => {
  it.each([
    "",
    "bearer synthetic-token",
    "Bearer synthetic-toke",
    "Bearer synthetic-tokenx",
    "Bearer synthétic-token",
  ])("rejects non-exact bearer %s before parsing the body", async (auth) => {
    const response = await fetchRunner(kind, "{", auth);
    expect(response.status).toBe(401);
    expect(await response.json()).toEqual({ error: "unauthorized" });
  });

  it("reports a missing token before parsing the body", async () => {
    const response = await fetchRunner(kind, "{", "", { CRABBOX_RUNNER_TOKEN: "" });
    expect(response.status).toBe(503);
    expect(await response.json()).toEqual({ error: "runner token is not configured" });
  });

  it.each(["null", "[]", '"value"', "17"])("preserves non-object policy for %s", async (body) => {
    const response = await fetchRunner(kind, body, "Bearer synthetic-token");
    expect(response.status).toBe(400);
    expect(await response.json()).toEqual({
      error: kind === "container" ? "id is required" : "json body must be an object",
    });
  });

  it("returns the existing malformed JSON error", async () => {
    const response = await fetchRunner(kind, "{", "Bearer synthetic-token");
    expect(response.status).toBe(400);
    expect(await response.json()).toEqual({ error: "invalid json" });
  });
});

it("D5 dynamic token takes precedence, including an explicitly empty token", async () => {
  await Promise.all(
    ["dynamic-token", ""].map(async (token) => {
      const response = await fetchRunner("dynamic", "{", "Bearer synthetic-token", {
        CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN: token,
      });
      expect(response.status).toBe(token ? 401 : 503);
    }),
  );
  const response = await fetchRunner("dynamic", "{", "Bearer dynamic-token", {
    CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN: "dynamic-token",
  });
  expect(await response.json()).toEqual({ error: "invalid json" });
});
