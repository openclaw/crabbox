import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { HetznerProvider } from "../src/fleet";
import { HetznerClient, HetznerHTTPError, hetznerExactNotFound } from "../src/hetzner";
import { providerLabelValue } from "../src/provider-labels";
import type { Env, HetznerCleanupEvidence, LeaseRecord } from "../src/types";

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(new Date("2026-09-01T08:00:00Z"));
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

function missing(): Response {
  return Response.json({ error: { code: "not_found" } }, { status: 404 });
}

function fixture() {
  let stored: LeaseRecord = {
    id: "cbx_abcdef123456",
    slug: "blue-lobster",
    provider: "hetzner",
    cloudID: "123",
    owner: "alice@example.com",
    org: "example-org",
    profile: "default",
    class: "small",
    serverType: "cx23",
    serverID: 123,
    serverName: "crabbox-blue-lobster",
    providerKey: "crabbox-cbx-abcdef123456",
    providerKeyCleanupOwned: true,
    providerKeyCleanupPending: true,
    providerKeyCleanupID: "7",
    host: "192.0.2.1",
    sshUser: "crabbox",
    sshPort: "2222",
    workRoot: "/work/my-app",
    keep: false,
    ttlSeconds: 5400,
    estimatedHourlyUSD: 1,
    maxEstimatedUSD: 1.5,
    state: "released",
    createdAt: "2026-09-01T07:00:00Z",
    updatedAt: "2026-09-01T08:00:00Z",
    expiresAt: "2026-09-01T09:00:00Z",
  };
  const server = {
    id: 123,
    name: stored.serverName,
    status: "running",
    labels: {
      crabbox: "true",
      created_by: "crabbox",
      lease: stored.id,
      slug: stored.slug!,
      provider: "hetzner",
      owner: providerLabelValue(stored.owner),
    } as Record<string, string>,
  };
  const action = {
    id: 456,
    command: "delete_server",
    status: "running",
    resources: [{ id: 123, type: "server" }],
    error: null as unknown,
  };
  const requests: string[] = [];
  const writes: HetznerCleanupEvidence[] = [];
  let deleted = false;
  let actionPolls = 0;
  let postReads = 0;
  const behavior = {
    alreadyAbsent: false,
    delete404: false,
    permanentlyPresent: false,
    running: false,
    keyFailure: false,
    lostAcknowledgement: false,
    failWrite: "" as "" | "dispatch" | "action" | "confirmation",
    actionReply: undefined as unknown,
    override: undefined as
      | ((path: string, init?: RequestInit) => Response | Promise<Response> | undefined)
      | undefined,
  };
  const fetchMock = vi.fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>(
    async (input, init) => {
      const path = new URL(String(input)).pathname.replace("/v1", "");
      const method = init?.method ?? "GET";
      requests.push(`${method} ${path}`);
      const override = behavior.override?.(path, init);
      if (override !== undefined) return override;
      if (path === "/servers/123" && method === "GET") {
        if (behavior.alreadyAbsent) return missing();
        if (deleted && !behavior.permanentlyPresent && ++postReads >= 2) return missing();
        return Response.json({ server });
      }
      if (path === "/servers/123" && method === "DELETE") {
        // oxlint-disable-next-line vitest/no-conditional-expect -- assert ordering at the exact mocked mutation.
        expect(stored.providerCleanup?.dispatchStartedAt).toBeTruthy();
        // oxlint-disable-next-line vitest/no-conditional-expect -- assert ordering at the exact mocked mutation.
        expect(stored.providerCleanup?.confirmation).toBeUndefined();
        deleted = true;
        if (behavior.lostAcknowledgement) throw new Error("connection lost after dispatch");
        if (behavior.delete404) return missing();
        return Response.json({ action });
      }
      if (path === "/servers/actions/456") {
        actionPolls++;
        return Response.json(
          behavior.actionReply ?? {
            action: {
              ...action,
              status: behavior.running || actionPolls === 1 ? "running" : "success",
            },
          },
        );
      }
      if (path === "/ssh_keys/7" && method === "DELETE") {
        // oxlint-disable-next-line vitest/no-conditional-expect -- assert ordering at the exact mocked mutation.
        expect(stored.providerCleanup?.confirmation).toBeTruthy();
        if (behavior.keyFailure) return Response.json({ error: "key busy" }, { status: 503 });
        return new Response(null, { status: 204 });
      }
      throw new Error(`unexpected request ${method} ${path}`);
    },
  );
  vi.stubGlobal("fetch", fetchMock);
  const run = () =>
    new HetznerProvider({ HETZNER_TOKEN: "synthetic-token" } as Env).releaseLease(
      structuredClone(stored),
      {
        saveCleanupEvidence: async (evidence) => {
          if (
            (behavior.failWrite === "dispatch" && !evidence.action) ||
            (behavior.failWrite === "action" && evidence.action) ||
            (behavior.failWrite === "confirmation" && evidence.confirmation)
          )
            throw new Error("storage unavailable");
          writes.push(structuredClone(evidence));
          stored = JSON.parse(
            JSON.stringify({ ...stored, providerCleanup: evidence }),
          ) as LeaseRecord;
        },
      },
    );
  return {
    run,
    requests,
    writes,
    behavior,
    action,
    server,
    fetchMock,
    get stored() {
      return stored;
    },
  };
}

async function settle(run: Promise<void>): Promise<string> {
  const result = run.then(
    () => "complete",
    (error: unknown) => String(error),
  );
  await vi.advanceTimersByTimeAsync(61_000);
  return result;
}

const confirmationMethod = "delete-action-success-and-server-absent";

describe("brokered Hetzner cleanup evidence", () => {
  it("waits for a running action, success, and delayed exact absence before deleting the key", async () => {
    const f = fixture();
    expect(await settle(f.run())).toBe("complete");
    expect(f.requests).toEqual([
      "GET /servers/123",
      "DELETE /servers/123",
      "GET /servers/actions/456",
      "GET /servers/actions/456",
      "GET /servers/123",
      "GET /servers/123",
      "DELETE /ssh_keys/7",
    ]);
    expect(f.writes[0]).toEqual({
      version: 1,
      provider: "hetzner",
      leaseID: f.stored.id,
      serverID: 123,
      dispatchStartedAt: "2026-09-01T08:00:00.000Z",
    });
    expect(f.writes[1]?.action).toEqual({ id: 456, status: "running" });
    expect(f.stored.providerCleanup).toMatchObject({
      version: 1,
      provider: "hetzner",
      leaseID: f.stored.id,
      serverID: 123,
      action: { id: 456, status: "success" },
      confirmation: { method: confirmationMethod, at: "2026-09-01T08:00:04.000Z" },
    });
  });

  it("accepts a delete action with one bound server and associated non-server resources", async () => {
    const f = fixture();
    f.action.resources.push({ id: 789, type: "primary_ip" }, { id: 321, type: "volume" });
    expect(await settle(f.run())).toBe("complete");
    expect(f.stored.providerCleanup?.confirmation?.method).toBe(confirmationMethod);
    expect(f.requests.filter((value) => value.startsWith("DELETE"))).toEqual([
      "DELETE /servers/123",
      "DELETE /ssh_keys/7",
    ]);
  });

  it.each([
    ["another server", { id: 999, type: "server" }],
    ["duplicate server", { id: 123, type: "server" }],
    ["invalid ID", { id: 0, type: "volume" }],
    ["unsafe ID", { id: Number.MAX_SAFE_INTEGER + 1, type: "primary_ip" }],
    ["missing type", { id: 789 }],
    ["empty type", { id: 789, type: " " }],
    ["null resource", null],
  ])("rejects a delete action accompanied by %s", async (_name, resource) => {
    const f = fixture();
    Object.assign(f.action, { resources: [...f.action.resources, resource] });
    expect(await settle(f.run())).toContain("invalid Hetzner delete action");
    expect(f.stored.providerCleanup?.action).toBeUndefined();
    expect(f.requests).not.toContain("DELETE /ssh_keys/7");
  });

  it.each([false, true])("uses typed exact key DELETE404 with deadline=%s", async (bounded) => {
    const client = new HetznerClient({ HETZNER_TOKEN: "synthetic-token" } as Env);
    const deadline = bounded ? Date.now() + 60_000 : undefined;
    const fetchMock = vi.fn<() => Promise<Response>>(async () => missing());
    vi.stubGlobal("fetch", fetchMock);
    await expect(client.deleteSSHKeyByID(7, deadline)).resolves.toBeUndefined();
    fetchMock.mockResolvedValue(Response.json({ error: { code: "not_found" } }, { status: 500 }));
    await expect(client.deleteSSHKeyByID(7, deadline)).rejects.toThrow("http 500");
    fetchMock.mockRejectedValue(new Error("not found: http 404"));
    await expect(client.deleteSSHKeyByID(7, deadline)).rejects.toThrow("not found: http 404");
    fetchMock.mockRejectedValue(new HetznerHTTPError("GET", "/ssh_keys/7", 404, "not_found"));
    await expect(client.deleteSSHKeyByID(7, deadline)).rejects.toThrow("GET /ssh_keys/7");
  });

  it.each([400, 401, 403, 405, 409, 410, 412, 423, 429])(
    "retires only a definite Hetzner DELETE rejection %s and revalidates before retry",
    async (status) => {
      const f = fixture();
      f.behavior.override = (_path, init) =>
        init?.method === "DELETE"
          ? Response.json({ error: { message: "request rejected" } }, { status })
          : undefined;
      expect(await settle(f.run())).toContain(`http ${status}`);
      expect(f.stored.providerCleanup).toEqual({
        version: 1,
        provider: "hetzner",
        leaseID: f.stored.id,
        serverID: 123,
      });
      expect(f.requests).toEqual(["GET /servers/123", "DELETE /servers/123"]);
      f.behavior.override = undefined;
      expect(await settle(f.run())).toBe("complete");
      expect(f.requests.slice(2, 4)).toEqual(["GET /servers/123", "DELETE /servers/123"]);
      expect(f.stored.providerCleanup?.confirmation?.method).toBe(confirmationMethod);
    },
  );

  it.each([
    "408",
    "422",
    "499",
    "500",
    "502",
    "503",
    "504",
    "network",
    "parse",
    "fetch timeout",
    "429 body timeout",
    "500 body timeout",
    "wrong method",
    "wrong path",
  ])("does not retire ambiguous Hetzner DELETE dispatch on %s", async (failure) => {
    const f = fixture();
    f.behavior.override = (_path, init) => {
      if (init?.method !== "DELETE") return undefined;
      if (failure === "network") throw new Error("http 429 rate_limit_exceeded: connection lost");
      if (failure === "wrong method")
        throw new HetznerHTTPError("GET", "/servers/123", 429, "rejected");
      if (failure === "wrong path")
        throw new HetznerHTTPError("DELETE", "/servers/999", 429, "rejected");
      if (failure === "parse") return new Response("invalid JSON", { status: 200 });
      if (failure === "fetch timeout") return new Promise<Response>(() => {});
      if (failure.endsWith("body timeout"))
        return new Response(new ReadableStream({ start() {} }), {
          status: failure.startsWith("429") ? 429 : 500,
        });
      return Response.json(
        { error: { code: "rate_limit_exceeded", message: "request rejected" } },
        { status: Number(failure) },
      );
    };
    expect(await settle(f.run())).not.toBe("complete");
    const uncertainty = structuredClone(f.stored.providerCleanup);
    expect(uncertainty?.dispatchStartedAt).toBeTruthy();
    expect(uncertainty?.action).toBeUndefined();
    expect(uncertainty?.deleteNotFoundAt).toBeUndefined();
    expect(uncertainty?.confirmation).toBeUndefined();
    f.behavior.override = undefined;
    f.behavior.alreadyAbsent = true;
    expect(await settle(f.run())).toContain("unresolved Hetzner delete acknowledgement");
    expect(f.stored.providerCleanup).toEqual(uncertainty);
    expect(f.requests).toEqual(["GET /servers/123", "DELETE /servers/123"]);
  });

  it("does not interpret GET404 as completion while an acknowledged action is running, including after restart", async () => {
    const f = fixture();
    f.behavior.running = true;
    expect(await settle(f.run())).toContain("timed out");
    expect(f.stored.providerCleanup?.action?.status).toBe("running");
    expect(f.stored.providerCleanup?.confirmation).toBeUndefined();
    f.behavior.alreadyAbsent = true;
    const reads = f.requests.filter((value) => value === "GET /servers/123").length;
    expect(await settle(f.run())).toContain("timed out");
    expect(f.requests.filter((value) => value === "GET /servers/123")).toHaveLength(reads);
    expect(f.requests.filter((value) => value === "DELETE /servers/123")).toHaveLength(1);
    expect(f.requests).not.toContain("DELETE /ssh_keys/7");
    f.behavior.running = false;
    expect(await settle(f.run())).toBe("complete");
    expect(f.stored.providerCleanup?.confirmation?.method).toBe(confirmationMethod);
    expect(f.requests.filter((value) => value === "DELETE /servers/123")).toHaveLength(1);
  });

  it("retains action error across retries even if the server is absent", async () => {
    const f = fixture();
    f.action.status = "error";
    f.action.error = { code: "failed", message: "deletion failed" };
    expect(await settle(f.run())).toContain("action 456 failed");
    f.behavior.alreadyAbsent = true;
    expect(await settle(f.run())).toContain("action 456 failed");
    expect(f.stored.providerCleanup?.action?.status).toBe("error");
    expect(f.requests).toEqual(["GET /servers/123", "DELETE /servers/123"]);
  });

  it.each([
    ["foreign server", { resources: [{ id: 999, type: "server" }] }],
    ["foreign resource type", { resources: [{ id: 123, type: "volume" }] }],
    ["missing resource", { resources: [] }],
    ["wrong command", { command: "start_server" }],
    ["invalid status", { status: "done" }],
    ["unsafe id", { id: Number.MAX_SAFE_INTEGER + 1 }],
    ["contradictory error", { status: "success", error: { code: "failed" } }],
  ])("retains uncertainty after a %s DELETE action", async (_name, fields) => {
    const f = fixture();
    Object.assign(f.action, fields);
    expect(await settle(f.run())).toContain("invalid Hetzner delete action");
    expect(f.stored.providerCleanup?.dispatchStartedAt).toBeTruthy();
    expect(f.stored.providerCleanup?.action).toBeUndefined();
    f.behavior.alreadyAbsent = true;
    expect(await settle(f.run())).toContain("unresolved Hetzner delete acknowledgement");
    expect(f.requests).toEqual(["GET /servers/123", "DELETE /servers/123"]);
  });

  it.each(["foreign", "malformed", "missing"])(
    "retains the original acknowledged action after a %s action read",
    async (kind) => {
      const f = fixture();
      if (kind === "foreign") f.behavior.actionReply = { action: { ...f.action, id: 999 } };
      if (kind === "malformed") f.behavior.actionReply = {};
      if (kind === "missing")
        f.behavior.override = (path) =>
          path.includes("actions")
            ? Response.json({ error: { code: "not_found" } }, { status: 404 })
            : undefined;
      expect(await settle(f.run())).not.toBe("complete");
      f.behavior.alreadyAbsent = true;
      expect(await settle(f.run())).not.toBe("complete");
      expect(f.stored.providerCleanup?.action).toEqual({ id: 456, status: "running" });
      expect(f.requests.filter((value) => value === "DELETE /servers/123")).toHaveLength(1);
      expect(f.requests).not.toContain("DELETE /ssh_keys/7");
    },
  );

  it("bounds permanently present server observations and resumes successful action evidence", async () => {
    const f = fixture();
    f.behavior.permanentlyPresent = true;
    expect(await settle(f.run())).toContain("observation timed out");
    expect(f.stored.providerCleanup?.action?.status).toBe("success");
    expect(f.requests.length).toBeLessThan(36);
    expect(f.requests).not.toContain("DELETE /ssh_keys/7");
    f.behavior.permanentlyPresent = false;
    expect(await settle(f.run())).toBe("complete");
    expect(f.requests.filter((value) => value === "DELETE /servers/123")).toHaveLength(1);
  });

  it("records already-absent separately without dispatching DELETE", async () => {
    const f = fixture();
    f.behavior.alreadyAbsent = true;
    expect(await settle(f.run())).toBe("complete");
    expect(f.stored.providerCleanup).toEqual({
      version: 1,
      provider: "hetzner",
      leaseID: f.stored.id,
      serverID: 123,
      confirmation: { method: "already-absent", at: "2026-09-01T08:00:00.000Z" },
    });
    expect(f.requests).toEqual(["GET /servers/123", "DELETE /ssh_keys/7"]);
  });

  it.each([false, true])(
    "requires exact GET absence after DELETE404 (persistent=%s)",
    async (persistent) => {
      const f = fixture();
      f.behavior.delete404 = true;
      f.behavior.permanentlyPresent = persistent;
      const result = await settle(f.run());
      expect(result).toBe(
        persistent ? "Error: Hetzner cleanup observation timed out for server 123" : "complete",
      );
      expect(f.stored.providerCleanup?.deleteNotFoundAt).toBeTruthy();
      expect(f.stored.providerCleanup?.confirmation?.method).toBe(
        persistent ? undefined : "delete-not-found-and-server-absent",
      );
      expect(f.requests.includes("DELETE /ssh_keys/7")).toBe(!persistent);
    },
  );

  it.each(["GET", "DELETE"])("rejects misleading 500 not_found bodies on %s", async (method) => {
    const f = fixture();
    f.behavior.override = (path, init) =>
      path === "/servers/123" && init?.method === method
        ? Response.json(
            { error: { code: "not_found", message: "server does not exist" } },
            { status: 500 },
          )
        : undefined;
    expect(await settle(f.run())).toContain("http 500");
    expect(f.requests).not.toContain("DELETE /ssh_keys/7");
    expect(f.stored.providerCleanup?.confirmation).toBeUndefined();
  });

  it.each(["network", "fetch timeout", "body timeout", "error body timeout"])(
    "bounds %s and retains cleanup debt",
    async (kind) => {
      const f = fixture();
      let signal: AbortSignal | null | undefined;
      f.behavior.override = (_path, init) => {
        signal = init?.signal;
        if (kind === "network") throw new Error("network down");
        if (kind === "fetch timeout") return new Promise<Response>(() => {});
        return new Response(new ReadableStream({ start() {} }), {
          status: kind === "error body timeout" ? 500 : 200,
        });
      };
      const result = await settle(f.run());
      expect(result).toContain(kind === "network" ? "network down" : "request timed out");
      expect(signal?.aborted).toBe(kind !== "network");
      expect(f.requests).toEqual(["GET /servers/123"]);
      expect(f.stored.providerCleanup).toBeUndefined();
    },
  );

  it.each(["dispatch", "action", "confirmation"] as const)(
    "fails closed when the %s evidence write fails",
    async (phase) => {
      const f = fixture();
      f.behavior.failWrite = phase;
      expect(await settle(f.run())).toContain("storage unavailable");
      expect(f.requests).not.toContain("DELETE /ssh_keys/7");
      expect(Boolean(f.stored.providerCleanup?.dispatchStartedAt)).toBe(phase !== "dispatch");
      expect(f.stored.providerCleanup?.confirmation).toBeUndefined();
      expect(f.requests.includes("DELETE /servers/123")).toBe(phase !== "dispatch");
      f.behavior.failWrite = "";
      f.behavior.alreadyAbsent = true;
      expect(await settle(f.run())).toContain(
        phase === "action" ? "unresolved Hetzner delete acknowledgement" : "complete",
      );
      expect(f.requests.filter((value) => value === "DELETE /servers/123")).toHaveLength(
        phase === "dispatch" ? 0 : 1,
      );
    },
  );

  it("preserves lost DELETE acknowledgement without a second DELETE or GET404 downgrade", async () => {
    const f = fixture();
    f.behavior.lostAcknowledgement = true;
    expect(await settle(f.run())).toContain("connection lost");
    f.behavior.alreadyAbsent = true;
    expect(await settle(f.run())).toContain("unresolved Hetzner delete acknowledgement");
    expect(f.requests).toEqual(["GET /servers/123", "DELETE /servers/123"]);
  });

  it("resumes action success after a transient server GET failure without another DELETE", async () => {
    const f = fixture();
    f.behavior.override = (path, init) =>
      path === "/servers/123" &&
      init?.method === "GET" &&
      f.stored.providerCleanup?.action?.status === "success"
        ? Response.json({}, { status: 503 })
        : undefined;
    expect(await settle(f.run())).toContain("http 503");
    expect(f.stored.providerCleanup?.action?.status).toBe("success");
    f.behavior.override = undefined;
    f.behavior.alreadyAbsent = true;
    expect(await settle(f.run())).toBe("complete");
    expect(f.requests.filter((value) => value === "DELETE /servers/123")).toHaveLength(1);
  });

  it("preserves durable server confirmation across a key failure and retries only the key", async () => {
    const f = fixture();
    f.behavior.keyFailure = true;
    expect(await settle(f.run())).toContain("http 503");
    const evidence = structuredClone(f.stored.providerCleanup);
    expect(evidence?.confirmation?.method).toBe(confirmationMethod);
    const count = f.requests.length;
    f.behavior.keyFailure = false;
    expect(await settle(f.run())).toBe("complete");
    expect(f.stored.providerCleanup).toEqual(evidence);
    expect(f.requests.slice(count)).toEqual(["DELETE /ssh_keys/7"]);
  });

  it.each([
    ["owner", { owner: "mallory" }],
    ["missing modern owner", { owner: undefined }],
    ["lease", { lease: "cbx_111111111111" }],
    ["slug", { slug: "other" }],
    ["provider", { provider: "aws" }],
    ["created_by", { created_by: "other" }],
    ["crabbox", { crabbox: "false" }],
  ])("rejects %s ownership mismatch before DELETE or key cleanup", async (_name, fields) => {
    const f = fixture();
    Object.assign(f.server.labels, fields);
    expect(await settle(f.run())).toContain("ownership does not match");
    expect(f.requests).toEqual(["GET /servers/123"]);
  });

  it.each(["modern", "legacy", "legacy owner mismatch", "legacy name mismatch"])(
    "checks exact %s ownership",
    async (kind) => {
      const f = fixture();
      if (kind.startsWith("legacy")) {
        delete f.stored.slug;
        delete f.server.labels["slug"];
        delete f.server.labels["provider"];
        delete f.server.labels["owner"];
        f.server.name = f.stored.serverName = "crabbox-cbx-abcdef123456";
        if (kind === "legacy owner mismatch") f.server.labels["owner"] = "mallory";
        if (kind === "legacy name mismatch") f.server.name = "other";
      }
      const result = await settle(f.run());
      expect(result).toContain(kind.includes("mismatch") ? "ownership does not match" : "complete");
      expect(f.requests.includes("DELETE /servers/123")).toBe(!kind.includes("mismatch"));
      expect(f.requests.includes("DELETE /ssh_keys/7")).toBe(!kind.includes("mismatch"));
    },
  );

  it.each([0, -1, 1.5, Number.MAX_SAFE_INTEGER + 1])(
    "rejects invalid numeric server identity %s without discovery",
    async (serverID) => {
      const f = fixture();
      f.stored.serverID = serverID;
      f.stored.cloudID = "";
      f.stored.provisioningResourceMayExist = true;
      expect(await settle(f.run())).toContain("exact server identity");
      expect(f.fetchMock).not.toHaveBeenCalled();
    },
  );

  it.each(["cloud", "returned"])("rejects conflicting %s server identity", async (kind) => {
    const f = fixture();
    if (kind === "cloud") f.stored.cloudID = "999";
    else f.server.id = 999;
    expect(await settle(f.run())).not.toBe("complete");
    expect(f.requests).not.toContain("DELETE /servers/123");
    expect(f.requests).not.toContain("DELETE /ssh_keys/7");
  });

  it("requires a durable callback, never optional in-memory confirmation", async () => {
    const f = fixture();
    await expect(
      new HetznerProvider({ HETZNER_TOKEN: "synthetic-token" } as Env).releaseLease(f.stored),
    ).rejects.toThrow("durable evidence storage");
    expect(f.fetchMock).not.toHaveBeenCalled();
  });

  it("recognizes not-found only by exact method, path and HTTP status", () => {
    const error = new HetznerHTTPError("GET", "/servers/123", 404, "not_found");
    expect(hetznerExactNotFound(error, "GET", "/servers/123")).toBe(true);
    expect(hetznerExactNotFound(error, "DELETE", "/servers/123")).toBe(false);
    expect(hetznerExactNotFound(error, "GET", "/servers/999")).toBe(false);
    expect(hetznerExactNotFound(new Error("not_found http 404"), "GET", "/servers/123")).toBe(
      false,
    );
    expect(
      hetznerExactNotFound(
        new HetznerHTTPError("GET", "/servers/123", 500, "not_found"),
        "GET",
        "/servers/123",
      ),
    ).toBe(false);
  });
});
