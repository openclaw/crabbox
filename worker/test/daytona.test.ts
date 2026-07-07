import { describe, expect, it, vi } from "vitest";

import { leaseConfig } from "../src/config";
import { DaytonaClient, daytonaAccessNeedsRefresh, daytonaSSHEndpoint } from "../src/daytona";
import type { Env } from "../src/types";

const baseEnv: Env = {
  FLEET: {} as DurableObjectNamespace,
  HETZNER_TOKEN: "",
  DAYTONA_CRABBOX_KEY: "daytona-test-key",
  CRABBOX_DAYTONA_API_URL: "https://daytona.example/api",
  CRABBOX_DAYTONA_SNAPSHOT: "crabbox-ready",
};

describe("daytona coordinator client", () => {
  it("requires a dedicated Worker secret and a safe API URL", () => {
    expect(() => new DaytonaClient({ ...baseEnv, DAYTONA_CRABBOX_KEY: "" })).toThrow(
      "DAYTONA_CRABBOX_KEY secret is required",
    );
    expect(
      () =>
        new DaytonaClient({
          ...baseEnv,
          CRABBOX_DAYTONA_API_URL: "https://user:secret@daytona.example/api",
        }),
    ).toThrow("must not contain credentials");
    expect(
      () =>
        new DaytonaClient({
          ...baseEnv,
          CRABBOX_DAYTONA_API_URL: "http://daytona.example/api",
        }),
    ).toThrow("must use https");
  });

  it("creates owned sandboxes, paginates inventory, and mints SSH access", async () => {
    const requests: Request[] = [];
    const authorizationHeaders: Array<string | null> = [];
    const createBodies: Record<string, unknown>[] = [];
    const listLabels: Array<string | null> = [];
    const accessMinutes: Array<string | null> = [];
    const client = new DaytonaClient(baseEnv);
    client.fetcher = vi.fn<typeof fetch>(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = new Request(input, init);
      requests.push(request.clone());
      authorizationHeaders.push(request.headers.get("authorization"));
      const url = new URL(request.url);
      if (request.method === "POST" && url.pathname === "/api/sandbox") {
        const body = (await request.json()) as Record<string, unknown>;
        createBodies.push(body);
        return Response.json({
          id: "sandbox-one",
          name: "crabbox-blue-lobster",
          snapshot: "crabbox-ready",
          state: "creating",
          labels: body["labels"],
        });
      }
      if (request.method === "GET" && url.pathname === "/api/sandbox") {
        const cursor = url.searchParams.get("cursor");
        listLabels.push(url.searchParams.get("labels"));
        return Response.json(
          cursor
            ? {
                items: [
                  {
                    id: "sandbox-two",
                    name: "crabbox-two",
                    state: "started",
                    labels: { crabbox: "true" },
                  },
                ],
                nextCursor: null,
              }
            : {
                items: [
                  {
                    id: "sandbox-one",
                    name: "crabbox-one",
                    state: "started",
                    labels: { crabbox: "true" },
                  },
                ],
                nextCursor: "next",
              },
        );
      }
      if (request.method === "POST" && url.pathname.endsWith("/ssh-access")) {
        accessMinutes.push(url.searchParams.get("expiresInMinutes"));
        return Response.json({
          token: "ssh-secret",
          expiresAt: "2026-07-06T12:00:00Z",
          sshCommand: "ssh -p 2222 ssh-secret@ssh.daytona.example",
        });
      }
      throw new Error(`unexpected request ${request.method} ${request.url}`);
    });

    const config = leaseConfig({
      provider: "daytona",
      sshPublicKey: "ssh-ed25519 test",
      idleTimeoutSeconds: 600,
    });
    const created = await client.createServer(
      config,
      "cbx_abcdef123456",
      "blue-lobster",
      "alice@example.com",
    );
    expect(created).toMatchObject({
      provider: "daytona",
      cloudID: "sandbox-one",
      status: "creating",
      serverType: "crabbox-ready",
    });
    await expect(client.listCrabboxServers()).resolves.toHaveLength(2);
    await expect(client.createSSHAccess("sandbox-one")).resolves.toEqual({
      user: "ssh-secret",
      host: "ssh.daytona.example",
      port: "2222",
      expiresAt: "2026-07-06T12:00:00Z",
    });
    expect(requests).toHaveLength(4);
    expect(authorizationHeaders).toEqual(Array(4).fill("Bearer daytona-test-key"));
    expect(createBodies).toHaveLength(1);
    expect(createBodies[0]).toMatchObject({
      snapshot: "crabbox-ready",
      autoStopInterval: 0,
      autoDeleteInterval: -1,
      labels: {
        crabbox: "true",
        created_by: "crabbox",
        lease: "cbx_abcdef123456",
        owner: "alice_example.com",
        provider: "daytona",
        slug: "blue-lobster",
      },
    });
    expect(listLabels).toEqual(['{"crabbox":"true"}', '{"crabbox":"true"}']);
    expect(accessMinutes).toEqual(["120"]);
  });

  it("redacts the Worker key from provider errors", async () => {
    const client = new DaytonaClient(baseEnv);
    client.fetcher = async () => new Response('{"token":"daytona-test-key"}', { status: 401 });

    const error = await client.listCrabboxServers().catch((caught: unknown) => caught);
    expect(String(error)).not.toContain("daytona-test-key");
    expect(String(error)).toContain("[redacted]");
  });

  it.each(["started", "running", "ready", "active", " Active "])(
    "accepts Daytona ready state %j",
    async (state) => {
      const client = new DaytonaClient(baseEnv);
      client.fetcher = async () =>
        Response.json({
          id: "sandbox-one",
          name: "crabbox-one",
          state,
          labels: { crabbox: "true" },
        });

      await expect(client.waitForStarted("sandbox-one")).resolves.toMatchObject({
        cloudID: "sandbox-one",
        status: state,
      });
    },
  );

  it.each(["error", "errored", "failed", "build_failed", "destroyed", "destroying", "deleted"])(
    "rejects Daytona terminal state %j",
    async (state) => {
      const client = new DaytonaClient(baseEnv);
      client.fetcher = async () =>
        Response.json({
          id: "sandbox-one",
          name: "crabbox-one",
          state,
          labels: { crabbox: "true" },
        });

      await expect(client.waitForStarted("sandbox-one")).rejects.toThrow(
        `entered terminal state=${state}`,
      );
    },
  );

  it("parses SSH commands and refreshes expiring access", () => {
    expect(
      daytonaSSHEndpoint({
        token: "fallback-token",
        expiresAt: "2026-07-06T12:00:00Z",
        sshCommand: "ssh -o StrictHostKeyChecking=no -p 2200 live-token@ssh.example",
      }),
    ).toEqual({
      user: "live-token",
      host: "ssh.example",
      port: "2200",
      expiresAt: "2026-07-06T12:00:00Z",
    });
    expect(
      daytonaAccessNeedsRefresh(
        { providerAccessExpiresAt: "2026-07-06T12:05:00Z" },
        Date.parse("2026-07-06T12:00:00Z"),
      ),
    ).toBe(true);
    expect(
      daytonaAccessNeedsRefresh(
        { providerAccessExpiresAt: "2026-07-06T12:30:00Z" },
        Date.parse("2026-07-06T12:00:00Z"),
      ),
    ).toBe(false);
  });
});
