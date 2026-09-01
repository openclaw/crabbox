import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { leaseConfig } from "../src/config";
import { DaytonaClient, daytonaAccessNeedsRefresh, daytonaSSHEndpoint } from "../src/daytona";
import { ProviderResourceUnresolvedError } from "../src/provider-provisioning";
import type { Env } from "../src/types";

const baseEnv: Env = {
  FLEET: {} as DurableObjectNamespace,
  HETZNER_TOKEN: "",
  DAYTONA_CRABBOX_KEY: "daytona-test-key",
  CRABBOX_DAYTONA_API_URL: "https://daytona.example/api",
  CRABBOX_DAYTONA_SNAPSHOT: "crabbox-ready",
};
const baseImage = `daytonaio/sandbox@sha256:${"a".repeat(64)}`;
const sandboxID = "11111111-1111-4111-8111-111111111111";
const otherSandboxID = "22222222-2222-4222-8222-222222222222";
const ownedLease = {
  id: "cbx_abcdef123456",
  slug: "blue-lobster",
  provider: "daytona" as const,
  owner: "alice@example.com",
  cloudID: sandboxID,
};
const ownedSandbox = {
  id: sandboxID,
  name: "crabbox-blue-lobster",
  state: "started",
  labels: {
    crabbox: "true",
    created_by: "crabbox",
    lease: "cbx_abcdef123456",
    slug: "blue-lobster",
    provider: "daytona",
    owner: "alice_example.com",
  },
};

type SandboxReply = Response | (() => Response | Promise<Response>);

function failedResponseBody(message: string): Response {
  return new Response(
    new ReadableStream({
      start(controller) {
        controller.error(new TypeError(message));
      },
    }),
  );
}

function mockOwnedSandboxRequests(client: DaytonaClient, replies: SandboxReply[]): string[] {
  const methods: string[] = [];
  client.fetcher = vi.fn<typeof fetch>(async (input, init) => {
    const request = new Request(input, init);
    const url = new URL(request.url);
    const query = request.method === "GET" ? "?verbose=true" : "";
    if (
      url.origin !== "https://daytona.example" ||
      url.pathname !== `/api/sandbox/${sandboxID}` ||
      url.search !== query
    ) {
      throw new Error(`unexpected sandbox request ${request.method} ${request.url}`);
    }
    methods.push(request.method);
    const reply = replies.shift();
    if (!reply) throw new Error(`unexpected extra sandbox request ${request.method}`);
    return typeof reply === "function" ? reply() : reply;
  });
  return methods;
}

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
      ttlMinutes: 90,
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

  it.each([
    { ttlSeconds: 0.5, keep: false, ttlMinutes: 1 },
    { ttlSeconds: 1, keep: false, ttlMinutes: 1 },
    { ttlSeconds: 60, keep: false, ttlMinutes: 1 },
    { ttlSeconds: 61, keep: false, ttlMinutes: 2 },
    { ttlSeconds: 61, keep: true, ttlMinutes: 2 },
  ])(
    "requests native TTL=$ttlMinutes minutes for ttlSeconds=$ttlSeconds, keep=$keep",
    async ({ ttlSeconds, keep, ttlMinutes }) => {
      const client = new DaytonaClient(baseEnv);
      const requests: Request[] = [];
      client.fetcher = vi.fn<typeof fetch>(async (input, init) => {
        requests.push(new Request(input, init));
        return Response.json(ownedSandbox);
      });
      const config = leaseConfig({
        provider: "daytona",
        sshPublicKey: "ssh-ed25519 test",
        ttlSeconds,
        keep,
      });

      await client.createServer(config, ownedLease.id, ownedLease.slug, ownedLease.owner);

      expect(requests).toHaveLength(1);
      expect(requests[0]?.method).toBe("POST");
      expect(requests[0]?.url).toBe("https://daytona.example/api/sandbox");
      expect(await requests[0]!.json()).toMatchObject({
        ttlMinutes,
        autoStopInterval: 0,
        autoDeleteInterval: -1,
        labels: { keep: String(keep) },
      });
    },
  );

  it("does not retry allocation without native TTL when the API rejects it", async () => {
    const client = new DaytonaClient(baseEnv);
    const bodies: Record<string, unknown>[] = [];
    client.fetcher = vi.fn<typeof fetch>(async (input, init) => {
      const request = new Request(input, init);
      const body = (await request.json()) as Record<string, unknown>;
      bodies.push(body);
      return body["ttlMinutes"] === undefined
        ? Response.json(ownedSandbox)
        : new Response("ttlMinutes is not supported", { status: 400 });
    });
    const config = leaseConfig({
      provider: "daytona",
      sshPublicKey: "ssh-ed25519 test",
      ttlSeconds: 600,
      keep: true,
    });

    await expect(
      client.createServer(config, ownedLease.id, ownedLease.slug, ownedLease.owner),
    ).rejects.toThrow("http 400: ttlMinutes is not supported");
    expect(bodies).toHaveLength(1);
    expect(bodies[0]).toMatchObject({ ttlMinutes: 10, labels: { keep: "true" } });
  });

  it.each<{
    name: string;
    original?: Partial<Env>;
    replacement: Partial<Env>;
    sameScope: boolean;
    outcome: string;
    requestCount: number;
  }>([
    {
      name: "equivalent URL, trimmed key, and omitted organization",
      replacement: {
        CRABBOX_DAYTONA_API_URL: " HTTPS://DAYTONA.EXAMPLE:443/api/// ",
        DAYTONA_CRABBOX_KEY: " daytona-test-key ",
        CRABBOX_DAYTONA_ORGANIZATION_ID: " ",
      },
      sameScope: true,
      outcome: "owned",
      requestCount: 1,
    },
    {
      name: "explicit default URL instead of an omitted URL",
      original: { CRABBOX_DAYTONA_API_URL: undefined },
      replacement: { CRABBOX_DAYTONA_API_URL: "https://app.daytona.io:443/api/" },
      sameScope: true,
      outcome: "owned",
      requestCount: 1,
    },
    {
      name: "trimmed configured organization",
      original: { CRABBOX_DAYTONA_ORGANIZATION_ID: "organization-one" },
      replacement: { CRABBOX_DAYTONA_ORGANIZATION_ID: " organization-one " },
      sameScope: true,
      outcome: "owned",
      requestCount: 1,
    },
    {
      name: "changed API host",
      replacement: { CRABBOX_DAYTONA_API_URL: "https://other-daytona.example/api" },
      sameScope: false,
      outcome: "unresolved",
      requestCount: 0,
    },
    {
      name: "changed API path",
      replacement: { CRABBOX_DAYTONA_API_URL: "https://daytona.example/api/v2" },
      sameScope: false,
      outcome: "unresolved",
      requestCount: 0,
    },
    {
      name: "new explicit organization",
      replacement: { CRABBOX_DAYTONA_ORGANIZATION_ID: "organization-one" },
      sameScope: false,
      outcome: "unresolved",
      requestCount: 0,
    },
    {
      name: "changed explicit organization",
      original: { CRABBOX_DAYTONA_ORGANIZATION_ID: "organization-one" },
      replacement: { CRABBOX_DAYTONA_ORGANIZATION_ID: "organization-two" },
      sameScope: false,
      outcome: "unresolved",
      requestCount: 0,
    },
    {
      name: "removed explicit organization",
      original: { CRABBOX_DAYTONA_ORGANIZATION_ID: "organization-one" },
      replacement: {},
      sameScope: false,
      outcome: "unresolved",
      requestCount: 0,
    },
    {
      name: "rotated key within the same organization",
      original: { CRABBOX_DAYTONA_ORGANIZATION_ID: "organization-one" },
      replacement: {
        CRABBOX_DAYTONA_ORGANIZATION_ID: "organization-one",
        DAYTONA_CRABBOX_KEY: "daytona-replacement-test-key",
      },
      sameScope: false,
      outcome: "unresolved",
      requestCount: 0,
    },
  ])("binds cleanup to the original request context: $name", async (testCase) => {
    const originalClient = new DaytonaClient({ ...baseEnv, ...testCase.original });
    const client = new DaytonaClient({ ...baseEnv, ...testCase.replacement });
    const lease = { ...ownedLease, providerScope: await originalClient.providerScope() };
    const replacementScope = await client.providerScope();
    client.fetcher = vi.fn<typeof fetch>(async () => Response.json(ownedSandbox));

    const result = await client.getOwnedServer(lease).then(
      () => "owned",
      (error: unknown) => (error instanceof ProviderResourceUnresolvedError ? "unresolved" : error),
    );

    expect(lease.providerScope).toMatch(/^daytona:context:v1:[a-f0-9]{64}$/);
    expect(replacementScope).toMatch(/^daytona:context:v1:[a-f0-9]{64}$/);
    expect(replacementScope === lease.providerScope).toBe(testCase.sameScope);
    expect(JSON.stringify(lease)).not.toContain("daytona-test-key");
    expect(replacementScope).not.toContain("daytona-replacement-test-key");
    expect(result).toBe(testCase.outcome);
    expect(client.fetcher).toHaveBeenCalledTimes(testCase.requestCount);
  });

  it("sends the same canonical scope used by the fingerprint in HTTP requests", async () => {
    const client = new DaytonaClient({
      ...baseEnv,
      CRABBOX_DAYTONA_API_URL: " HTTPS://DAYTONA.EXAMPLE:443/api/// ",
      CRABBOX_DAYTONA_ORGANIZATION_ID: " organization-one ",
      DAYTONA_CRABBOX_KEY: " daytona-test-key ",
    });
    const requests: Request[] = [];
    client.fetcher = async (input, init) => {
      requests.push(new Request(input, init));
      return Response.json(ownedSandbox);
    };

    await client.getOwnedServer({ ...ownedLease, providerScope: await client.providerScope() });

    const requestScopes = requests.map((request) => ({
      url: request.url,
      organization: request.headers.get("x-daytona-organization-id"),
      authorization: request.headers.get("authorization"),
    }));
    expect(requestScopes).toEqual([
      {
        url: `https://daytona.example/api/sandbox/${sandboxID}?verbose=true`,
        organization: "organization-one",
        authorization: "Bearer daytona-test-key",
      },
    ]);
  });

  it("redacts credentials from provider error diagnostics", async () => {
    const client = new DaytonaClient(baseEnv);
    client.fetcher = async () =>
      new Response(
        'route=iad https://passwordless-url-token@provider.example/path {"access_token":"access-value","refresh_token":"refresh-value","refreshToken":"refresh-camel-value","id_token":"id-value","idToken":"id-camel-value","secretAccessKey":"aws-camel-value","secret_access_key":"aws-snake-value","apiSecret":"api-value","workerKey":"daytona-test-key"}',
        { status: 401 },
      );

    const error = await client.listCrabboxServers().catch((caught: unknown) => caught);
    const diagnostic = String(error);
    for (const secret of [
      "passwordless-url-token",
      "access-value",
      "refresh-value",
      "refresh-camel-value",
      "id-value",
      "id-camel-value",
      "aws-camel-value",
      "aws-snake-value",
      "api-value",
      "daytona-test-key",
    ]) {
      expect(diagnostic).not.toContain(secret);
    }
    expect(diagnostic).toContain("[redacted]");
    expect(diagnostic).toContain("provider.example/path");
    expect(diagnostic).toContain("route=iad");
  });

  it("treats an already deleted sandbox as successful cleanup", async () => {
    const client = new DaytonaClient(baseEnv);
    client.fetcher = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(new Response(null, { status: 404 }))
      .mockResolvedValueOnce(new Response(null, { status: 204 }))
      .mockResolvedValueOnce(new Response("unavailable", { status: 503 }));

    await expect(client.deleteServer("sandbox-one")).resolves.toBeUndefined();
    await expect(client.deleteServer("sandbox-one")).resolves.toBeUndefined();
    await expect(client.deleteServer("sandbox-one")).rejects.toThrow("http 503");
  });

  describe("scoped managed cleanup", () => {
    beforeEach(() => vi.useFakeTimers());
    afterEach(() => vi.useRealTimers());

    it.each([
      { name: "legacy missing scope", patch: { providerScope: undefined } },
      { name: "malformed scope", patch: { providerScope: "daytona:context:v1:not-a-digest" } },
      { name: "unknown resource ID", patch: { cloudID: "" } },
      { name: "malformed UUID", patch: { cloudID: "11111111-1111-4111-8111-not-a-uuid" } },
      { name: "sandbox name instead of UUID", patch: { cloudID: "crabbox-blue-lobster" } },
    ])("rejects $name before any provider request", async ({ patch }) => {
      const client = new DaytonaClient(baseEnv);
      client.fetcher = vi.fn<typeof fetch>();
      const lease = { ...ownedLease, providerScope: await client.providerScope(), ...patch };

      await expect(client.getOwnedServer(lease)).rejects.toBeInstanceOf(
        ProviderResourceUnresolvedError,
      );
      await expect(client.deleteOwnedServer(lease)).rejects.toBeInstanceOf(
        ProviderResourceUnresolvedError,
      );
      expect(client.fetcher).not.toHaveBeenCalled();
    });

    it.each([
      {
        name: "a different returned UUID with matching labels",
        sandbox: { ...ownedSandbox, id: otherSandboxID },
      },
      {
        name: "the exact UUID with a different owner",
        sandbox: { ...ownedSandbox, labels: { ...ownedSandbox.labels, owner: "bob_example.com" } },
      },
    ])("rejects $name instead of granting cleanup authority", async ({ sandbox }) => {
      const client = new DaytonaClient(baseEnv);
      const lease = { ...ownedLease, providerScope: await client.providerScope() };
      const methods = mockOwnedSandboxRequests(client, [
        Response.json(sandbox),
        Response.json(sandbox),
      ]);

      await expect(client.getOwnedServer(lease)).rejects.toBeInstanceOf(
        ProviderResourceUnresolvedError,
      );
      await expect(client.deleteOwnedServer(lease)).rejects.toBeInstanceOf(
        ProviderResourceUnresolvedError,
      );
      expect(methods).toEqual(["GET", "GET"]);
    });

    it.each([204, 404])(
      "does not complete cleanup on DELETE HTTP %i without a terminal observation",
      async (deleteStatus) => {
        const client = new DaytonaClient(baseEnv);
        const lease = { ...ownedLease, providerScope: await client.providerScope() };
        client.pollDelayMs = 1_000;
        const methods = mockOwnedSandboxRequests(client, [
          Response.json(ownedSandbox),
          new Response(null, { status: deleteStatus }),
          Response.json({ ...ownedSandbox, state: "destroying" }),
          new Response(null, { status: 404 }),
        ]);
        let settled = false;
        const cleanup = client.deleteOwnedServer(lease).then(
          () => {
            settled = true;
            return "deleted";
          },
          (error: unknown) => {
            settled = true;
            return error;
          },
        );

        await vi.advanceTimersByTimeAsync(0);
        expect(settled).toBe(false);
        expect(methods).toEqual(["GET", "DELETE", "GET"]);

        await vi.advanceTimersByTimeAsync(1_000);
        expect(await cleanup).toBe("deleted");
        expect(methods).toEqual(["GET", "DELETE", "GET", "GET"]);
      },
    );

    it.each([
      { name: "exact-resource 404", response: () => new Response(null, { status: 404 }) },
      { name: "deleted", response: () => Response.json({ ...ownedSandbox, state: "deleted" }) },
      { name: "destroyed", response: () => Response.json({ ...ownedSandbox, state: "destroyed" }) },
    ])("accepts $name without another DELETE", async ({ response }) => {
      const client = new DaytonaClient(baseEnv);
      const lease = { ...ownedLease, providerScope: await client.providerScope() };
      const methods = mockOwnedSandboxRequests(client, [response]);

      await expect(client.deleteOwnedServer(lease)).resolves.toBeUndefined();
      expect(methods).toEqual(["GET"]);
    });

    it.each(["destroying", "deleting"])(
      "observes a prior %s transition without resubmitting DELETE",
      async (state) => {
        const client = new DaytonaClient(baseEnv);
        const lease = { ...ownedLease, providerScope: await client.providerScope() };
        const methods = mockOwnedSandboxRequests(client, [
          Response.json({ ...ownedSandbox, state }),
          Response.json({ ...ownedSandbox, state }),
          Response.json({ ...ownedSandbox, state: "deleted" }),
        ]);
        const cleanup = client.deleteOwnedServer(lease).then(
          () => "deleted",
          (error: unknown) => error,
        );

        await vi.runAllTimersAsync();

        expect(await cleanup).toBe("deleted");
        expect(methods).toEqual(["GET", "GET", "GET"]);
      },
    );

    it.each(["destroying", "build_failed"])(
      "leaves cleanup pending when an accepted DELETE remains in state=%s",
      async (state) => {
        const client = new DaytonaClient(baseEnv);
        const lease = { ...ownedLease, providerScope: await client.providerScope() };
        client.maxWaitMs = 1_000;
        client.pollDelayMs = 1_000;
        const methods = mockOwnedSandboxRequests(client, [
          Response.json(ownedSandbox),
          new Response(null, { status: 204 }),
          Response.json({ ...ownedSandbox, state }),
          Response.json({ ...ownedSandbox, state }),
        ]);
        const cleanup = client.deleteOwnedServer(lease).catch((error: unknown) => error);

        await vi.runAllTimersAsync();

        expect(await cleanup).toMatchObject({
          message: `timed out waiting for daytona sandbox ${sandboxID} cleanup (state=${state})`,
        });
        expect(methods[0]).toBe("GET");
        expect(methods.filter((method) => method === "DELETE")).toHaveLength(1);
        expect(methods.filter((method) => method === "GET").length).toBeGreaterThan(1);
      },
    );

    it.each([
      {
        name: "unchanged ownership",
        owner: "alice_example.com",
        outcome: "deleted",
        methods: ["GET", "DELETE", "GET", "DELETE", "GET"],
      },
      {
        name: "changed ownership",
        owner: "bob_example.com",
        outcome: "unresolved",
        methods: ["GET", "DELETE", "GET"],
      },
    ])("rechecks $name before retrying a conflicted DELETE", async (testCase) => {
      const client = new DaytonaClient(baseEnv);
      const lease = { ...ownedLease, providerScope: await client.providerScope() };
      const methods = mockOwnedSandboxRequests(client, [
        Response.json(ownedSandbox),
        new Response("Sandbox state change in progress", { status: 409 }),
        Response.json({
          ...ownedSandbox,
          labels: { ...ownedSandbox.labels, owner: testCase.owner },
        }),
        new Response(null, { status: 204 }),
        Response.json({ ...ownedSandbox, state: "destroyed" }),
      ]);
      const cleanup = client.deleteOwnedServer(lease).then(
        () => "deleted",
        (error: unknown) =>
          error instanceof ProviderResourceUnresolvedError ? "unresolved" : error,
      );

      await vi.runAllTimersAsync();

      expect(await cleanup).toBe(testCase.outcome);
      expect(methods).toEqual(testCase.methods);
    });

    it.each([
      {
        name: "ownership-read fetch failure",
        replies: [() => Promise.reject(new TypeError("ownership read disconnected"))],
        error: "ownership read disconnected",
        methods: ["GET"],
      },
      {
        name: "ownership-read body failure",
        replies: [() => failedResponseBody("ownership body disconnected")],
        error: "ownership body disconnected",
        methods: ["GET"],
      },
      {
        name: "DELETE fetch failure",
        replies: [
          Response.json(ownedSandbox),
          () => Promise.reject(new TypeError("delete disconnected")),
        ],
        error: "delete disconnected",
        methods: ["GET", "DELETE"],
      },
      {
        name: "DELETE body failure",
        replies: [
          Response.json(ownedSandbox),
          () => failedResponseBody("delete body disconnected"),
        ],
        error: "delete body disconnected",
        methods: ["GET", "DELETE"],
      },
      {
        name: "malformed deletion observation",
        replies: [
          Response.json(ownedSandbox),
          new Response(null, { status: 204 }),
          new Response("{"),
        ],
        error: SyntaxError,
        methods: ["GET", "DELETE", "GET"],
      },
      {
        name: "another UUID returned as deleted",
        replies: [
          Response.json(ownedSandbox),
          new Response(null, { status: 204 }),
          Response.json({ ...ownedSandbox, id: otherSandboxID, state: "deleted" }),
        ],
        error: "identity mismatch",
        methods: ["GET", "DELETE", "GET"],
      },
    ])("does not mistake $name for cleanup success", async ({ replies, error, methods }) => {
      const client = new DaytonaClient(baseEnv);
      const lease = { ...ownedLease, providerScope: await client.providerScope() };
      const observedMethods = mockOwnedSandboxRequests(client, replies);

      await expect(client.deleteOwnedServer(lease)).rejects.toThrow(error);
      expect(observedMethods).toEqual(methods);
    });

    it.each([
      {
        name: "fetch failures",
        reply: () => Promise.reject(new TypeError("cleanup observation disconnected")),
        state: "network_unavailable",
      },
      {
        name: "response-body read failures",
        reply: () => failedResponseBody("cleanup body disconnected"),
        state: "network_unavailable",
      },
      {
        name: "provider HTTP failures",
        reply: () => new Response("provider unavailable", { status: 503 }),
        state: "http_503",
      },
    ])("keeps accepted cleanup pending through repeated $name", async ({ reply, state }) => {
      const client = new DaytonaClient(baseEnv);
      const lease = { ...ownedLease, providerScope: await client.providerScope() };
      client.maxWaitMs = 1_000;
      client.pollDelayMs = 1_000;
      const methods = mockOwnedSandboxRequests(client, [
        Response.json(ownedSandbox),
        new Response(null, { status: 204 }),
        reply,
        reply,
      ]);
      const cleanup = client.deleteOwnedServer(lease).catch((error: unknown) => error);

      await vi.runAllTimersAsync();

      expect(await cleanup).toMatchObject({
        message: `timed out waiting for daytona sandbox ${sandboxID} cleanup (state=${state})`,
      });
      expect(methods[0]).toBe("GET");
      expect(methods.filter((method) => method === "DELETE")).toHaveLength(1);
      expect(methods.filter((method) => method === "GET").length).toBeGreaterThan(1);
    });
  });

  it("creates a larger snapshot from the configured base and deletes its builder", async () => {
    const calls: Array<{ method: string; path: string; body?: unknown }> = [];
    let state = "creating";
    let cleanupPolls = 0;
    let snapshotPolls = 0;
    let snapshotTransitionPolls = 0;
    let cpu = 1;
    let memory = 1;
    let disk = 3;
    const client = new DaytonaClient(baseEnv);
    client.pollDelayMs = 0;
    client.fetcher = vi.fn<typeof fetch>(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = new Request(input, init);
      const url = new URL(request.url);
      const body = request.body ? await request.clone().json() : undefined;
      calls.push({ method: request.method, path: url.pathname, ...(body ? { body } : {}) });
      if (request.method === "GET" && url.pathname === "/api/snapshots/crabbox-ready-2x4x10") {
        if (calls.length === 1) return new Response(null, { status: 404 });
        snapshotPolls += 1;
        if (snapshotPolls === 1) {
          return new Response("temporary provider failure", { status: 503 });
        }
        return Response.json({
          id: "snapshot-one",
          name: "crabbox-ready-2x4x10",
          state: "active",
          cpu,
          mem: memory,
          disk,
        });
      }
      if (request.method === "POST" && url.pathname === "/api/sandbox") {
        state = "started";
        ({ cpu, memory, disk } = body as { cpu: number; memory: number; disk: number });
        return Response.json({
          id: "snapshot-builder",
          name: "crabbox-snapshot-bootstrap-test",
          state: "creating",
          cpu,
          memory,
          disk,
        });
      }
      if (request.method === "GET" && url.pathname === "/api/sandbox/snapshot-builder") {
        if (state === "snapshotting") {
          snapshotTransitionPolls += 1;
          if (snapshotTransitionPolls === 1) {
            return Response.json({
              id: "snapshot-builder",
              name: "crabbox-snapshot-bootstrap-test",
              state,
              cpu,
              memory,
              disk,
            });
          }
          state = "stopped";
        }
        if (state === "destroying") {
          cleanupPolls += 1;
          if (cleanupPolls === 1) {
            return new Response("temporary provider failure", { status: 503 });
          }
          return Response.json({
            id: "snapshot-builder",
            name: "crabbox-snapshot-bootstrap-test",
            state: cleanupPolls === 2 ? "build_failed" : "destroyed",
          });
        }
        return Response.json({
          id: "snapshot-builder",
          name: "crabbox-snapshot-bootstrap-test",
          state,
          cpu,
          memory,
          disk,
        });
      }
      if (request.method === "POST" && url.pathname.endsWith("/stop")) {
        state = "stopped";
        return Response.json({ id: "snapshot-builder", state });
      }
      if (request.method === "POST" && url.pathname.endsWith("/snapshot")) {
        state = "snapshotting";
        return Response.json({ id: "snapshot-builder", state: "snapshotting", disk });
      }
      if (request.method === "DELETE" && url.pathname.endsWith("/snapshot-builder")) {
        state = "destroying";
        return new Response(null, { status: 204 });
      }
      throw new Error(`unexpected request ${request.method} ${request.url}`);
    });

    await expect(
      client.bootstrapSnapshot("crabbox-ready-2x4x10", 2, 4, 10, baseImage),
    ).resolves.toEqual({
      sourceSnapshot: baseImage,
      sourceCPU: 2,
      sourceMemoryGiB: 4,
      sourceDiskGiB: 10,
      snapshot: "crabbox-ready-2x4x10",
      cpu: 2,
      memoryGiB: 4,
      diskGiB: 10,
      sandboxID: "snapshot-builder",
      cleanup: "deleted",
    });
    expect(calls.map(({ method, path }) => `${method} ${path}`)).toEqual([
      "GET /api/snapshots/crabbox-ready-2x4x10",
      "POST /api/sandbox",
      "GET /api/sandbox/snapshot-builder",
      "GET /api/sandbox/snapshot-builder",
      "POST /api/sandbox/snapshot-builder/stop",
      "GET /api/sandbox/snapshot-builder",
      "POST /api/sandbox/snapshot-builder/snapshot",
      "GET /api/snapshots/crabbox-ready-2x4x10",
      "GET /api/snapshots/crabbox-ready-2x4x10",
      "GET /api/sandbox/snapshot-builder",
      "GET /api/sandbox/snapshot-builder",
      "DELETE /api/sandbox/snapshot-builder",
      "GET /api/sandbox/snapshot-builder",
      "GET /api/sandbox/snapshot-builder",
      "GET /api/sandbox/snapshot-builder",
    ]);
    expect(calls[1]?.body).toMatchObject({
      buildInfo: {
        dockerfileContent: `FROM ${baseImage}`,
      },
      autoStopInterval: 30,
      autoDeleteInterval: 60,
      cpu: 2,
      memory: 4,
      disk: 10,
      labels: {
        created_by: "crabbox",
        purpose: "snapshot-bootstrap",
        snapshot_name: "crabbox-ready-2x4x10",
      },
    });
    expect(calls[1]?.body).not.toMatchObject({
      labels: { crabbox: "true" },
    });
    expect(calls[6]?.body).toEqual({ name: "crabbox-ready-2x4x10" });
  });

  it("deletes the builder when Daytona snapshot bootstrap fails", async () => {
    const methods: string[] = [];
    let deleted = false;
    const client = new DaytonaClient(baseEnv);
    client.pollDelayMs = 0;
    client.fetcher = vi.fn<typeof fetch>(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = new Request(input, init);
      const url = new URL(request.url);
      methods.push(`${request.method} ${url.pathname}`);
      if (request.method === "GET" && url.pathname.startsWith("/api/snapshots/")) {
        return new Response(null, { status: 404 });
      }
      if (request.method === "POST" && url.pathname === "/api/sandbox") {
        return Response.json({ id: "snapshot-builder", state: "creating" });
      }
      if (request.method === "GET") {
        if (deleted) return new Response(null, { status: 404 });
        return Response.json({
          id: "snapshot-builder",
          state: "started",
          cpu: 2,
          memory: 4,
          disk: 10,
        });
      }
      if (request.method === "POST" && url.pathname.endsWith("/stop")) {
        return new Response("provider unavailable", { status: 503 });
      }
      if (request.method === "DELETE") {
        deleted = true;
        return new Response(null, { status: 204 });
      }
      throw new Error(`unexpected request ${request.method} ${request.url}`);
    });

    await expect(
      client.bootstrapSnapshot("crabbox-ready-2x4x10", 2, 4, 10, baseImage),
    ).rejects.toThrow("http 503");
    expect(methods.slice(-2)).toEqual([
      "DELETE /api/sandbox/snapshot-builder",
      "GET /api/sandbox/snapshot-builder",
    ]);
  });

  it("rejects a failed snapshot and still waits for builder cleanup", async () => {
    const methods: string[] = [];
    let preflight = true;
    let state = "started";
    let snapshotRequested = false;
    let transitionReadFailed = false;
    const client = new DaytonaClient(baseEnv);
    client.pollDelayMs = 0;
    client.fetcher = vi.fn<typeof fetch>(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = new Request(input, init);
      const url = new URL(request.url);
      methods.push(`${request.method} ${url.pathname}`);
      if (request.method === "GET" && url.pathname.startsWith("/api/snapshots/")) {
        if (preflight) {
          preflight = false;
          return new Response(null, { status: 404 });
        }
        return Response.json({
          id: "snapshot-one",
          name: "crabbox-ready-2x4x10",
          state: "build_failed",
          errorReason: "registry push failed",
        });
      }
      if (request.method === "POST" && url.pathname === "/api/sandbox") {
        return Response.json({ id: "snapshot-builder", state: "creating" });
      }
      if (request.method === "GET" && url.pathname.endsWith("/snapshot-builder")) {
        if (state === "destroying") return new Response(null, { status: 404 });
        if (snapshotRequested && !transitionReadFailed) {
          transitionReadFailed = true;
          return new Response("temporary provider failure", { status: 503 });
        }
        return Response.json({
          id: "snapshot-builder",
          state,
          cpu: 2,
          memory: 4,
          disk: 10,
        });
      }
      if (request.method === "POST" && url.pathname.endsWith("/stop")) {
        state = "stopped";
        return Response.json({ id: "snapshot-builder", state });
      }
      if (request.method === "POST" && url.pathname.endsWith("/snapshot")) {
        snapshotRequested = true;
        return Response.json({ id: "snapshot-builder", state: "snapshotting" });
      }
      if (request.method === "DELETE") {
        state = "destroying";
        return new Response(null, { status: 204 });
      }
      throw new Error(`unexpected request ${request.method} ${request.url}`);
    });

    await expect(
      client.bootstrapSnapshot("crabbox-ready-2x4x10", 2, 4, 10, baseImage),
    ).rejects.toThrow(
      "daytona snapshot crabbox-ready-2x4x10 entered terminal state=build_failed: registry push failed",
    );
    expect(transitionReadFailed).toBe(true);
    expect(methods.slice(-2)).toEqual([
      "DELETE /api/sandbox/snapshot-builder",
      "GET /api/sandbox/snapshot-builder",
    ]);
  });

  it("waits out snapshotting when the accepted snapshot response is lost", async () => {
    const methods: string[] = [];
    const snapshotError = new TypeError("network reset after snapshot acceptance");
    let state = "started";
    let transitionPolls = 0;
    let snapshotAttempted = false;
    let deleteAttempts = 0;
    let deleted = false;
    const client = new DaytonaClient(baseEnv);
    client.maxWaitMs = 25;
    client.snapshotWaitMs = 100;
    client.pollDelayMs = 10;
    client.snapshotAcceptanceWaitMs = 20;
    client.fetcher = vi.fn<typeof fetch>(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = new Request(input, init);
      const url = new URL(request.url);
      methods.push(`${request.method} ${url.pathname}`);
      if (request.method === "GET" && url.pathname.startsWith("/api/snapshots/")) {
        return new Response(null, { status: 404 });
      }
      if (request.method === "POST" && url.pathname === "/api/sandbox") {
        return Response.json({ id: "snapshot-builder", state: "creating" });
      }
      if (request.method === "GET" && url.pathname.endsWith("/snapshot-builder")) {
        if (deleted) return new Response(null, { status: 404 });
        if (snapshotAttempted) {
          transitionPolls += 1;
          state =
            transitionPolls === 1 ? "stopped" : transitionPolls <= 3 ? "snapshotting" : "stopped";
        }
        return Response.json({
          id: "snapshot-builder",
          state,
          cpu: 2,
          memory: 4,
          disk: 10,
        });
      }
      if (request.method === "POST" && url.pathname.endsWith("/stop")) {
        state = "stopped";
        return Response.json({ id: "snapshot-builder", state });
      }
      if (request.method === "POST" && url.pathname.endsWith("/snapshot")) {
        snapshotAttempted = true;
        throw snapshotError;
      }
      if (request.method === "DELETE") {
        deleteAttempts += 1;
        if (deleteAttempts === 1) {
          return new Response("Sandbox state change in progress", { status: 409 });
        }
        deleted = true;
        return new Response(null, { status: 204 });
      }
      throw new Error(`unexpected request ${request.method} ${request.url}`);
    });

    // Keep the acceptance window independent of host scheduling.
    vi.useFakeTimers();
    try {
      const result = client
        .bootstrapSnapshot("crabbox-ready-2x4x10", 2, 4, 10, baseImage)
        .catch((error: unknown) => error);

      await vi.runAllTimersAsync();

      expect(await result).toBe(snapshotError);
      expect(transitionPolls).toBe(4);
      expect(methods.slice(-6)).toEqual([
        "GET /api/sandbox/snapshot-builder",
        "GET /api/sandbox/snapshot-builder",
        "GET /api/sandbox/snapshot-builder",
        "DELETE /api/sandbox/snapshot-builder",
        "DELETE /api/sandbox/snapshot-builder",
        "GET /api/sandbox/snapshot-builder",
      ]);
    } finally {
      vi.useRealTimers();
    }
  });

  it("fails cleanup when Daytona accepts delete but never destroys the builder", async () => {
    let preflight = true;
    let state = "started";
    const client = new DaytonaClient(baseEnv);
    client.pollDelayMs = 25;
    client.maxWaitMs = 20;
    client.fetcher = vi.fn<typeof fetch>(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = new Request(input, init);
      const url = new URL(request.url);
      if (request.method === "GET" && url.pathname.startsWith("/api/snapshots/")) {
        if (preflight) {
          preflight = false;
          return new Response(null, { status: 404 });
        }
        return Response.json({
          id: "snapshot-one",
          name: "crabbox-ready-2x4x10",
          state: "active",
          cpu: 2,
          mem: 4,
          disk: 10,
        });
      }
      if (request.method === "POST" && url.pathname === "/api/sandbox") {
        return Response.json({ id: "snapshot-builder", state: "creating" });
      }
      if (request.method === "GET" && url.pathname.endsWith("/snapshot-builder")) {
        return Response.json({
          id: "snapshot-builder",
          state,
          cpu: 2,
          memory: 4,
          disk: 10,
        });
      }
      if (request.method === "POST" && url.pathname.endsWith("/stop")) {
        state = "stopped";
        return Response.json({ id: "snapshot-builder", state });
      }
      if (request.method === "POST" && url.pathname.endsWith("/snapshot")) {
        return Response.json({ id: "snapshot-builder", state: "snapshotting" });
      }
      if (request.method === "DELETE") {
        state = "destroying";
        return new Response(null, { status: 204 });
      }
      throw new Error(`unexpected request ${request.method} ${request.url}`);
    });

    await expect(
      client.bootstrapSnapshot("crabbox-ready-2x4x10", 2, 4, 10, baseImage),
    ).rejects.toThrow(
      "timed out waiting for daytona sandbox snapshot-builder cleanup (state=destroying)",
    );
  });

  it("rejects an existing snapshot name before creating a paid builder", async () => {
    const client = new DaytonaClient(baseEnv);
    const fetchMock = vi.fn<typeof fetch>(async () =>
      Response.json({
        id: "snapshot-existing",
        name: "crabbox-ready-2x4x10",
        state: "active",
      }),
    );
    client.fetcher = fetchMock;

    await expect(
      client.bootstrapSnapshot("crabbox-ready-2x4x10", 2, 4, 10, baseImage),
    ).rejects.toThrow("daytona snapshot crabbox-ready-2x4x10 already exists (state=active)");
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("uses a dedicated bounded snapshot wait longer than sandbox lifecycle waits", async () => {
    let snapshotPolls = 0;
    let state = "started";
    let deleted = false;
    const client = new DaytonaClient(baseEnv);
    client.maxWaitMs = 50;
    client.snapshotWaitMs = 200;
    client.pollDelayMs = 60;
    client.fetcher = vi.fn<typeof fetch>(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = new Request(input, init);
      const url = new URL(request.url);
      if (request.method === "GET" && url.pathname.startsWith("/api/snapshots/")) {
        snapshotPolls += 1;
        if (snapshotPolls < 3) return new Response(null, { status: 404 });
        return Response.json({
          id: "snapshot-one",
          name: "crabbox-ready-2x4x10",
          state: "active",
          cpu: 2,
          mem: 4,
          disk: 10,
        });
      }
      if (request.method === "POST" && url.pathname === "/api/sandbox") {
        return Response.json({ id: "snapshot-builder", state: "creating" });
      }
      if (request.method === "GET" && url.pathname.endsWith("/snapshot-builder")) {
        if (deleted) return new Response(null, { status: 404 });
        return Response.json({
          id: "snapshot-builder",
          state,
          cpu: 2,
          memory: 4,
          disk: 10,
        });
      }
      if (request.method === "POST" && url.pathname.endsWith("/stop")) {
        state = "stopped";
        return Response.json({ id: "snapshot-builder", state });
      }
      if (request.method === "POST" && url.pathname.endsWith("/snapshot")) {
        return Response.json({ id: "snapshot-builder", state: "snapshotting" });
      }
      if (request.method === "DELETE") {
        deleted = true;
        return new Response(null, { status: 204 });
      }
      throw new Error(`unexpected request ${request.method} ${request.url}`);
    });

    await expect(
      client.bootstrapSnapshot("crabbox-ready-2x4x10", 2, 4, 10, baseImage),
    ).resolves.toMatchObject({
      snapshot: "crabbox-ready-2x4x10",
      cleanup: "deleted",
    });
    expect(client.snapshotWaitMs).toBeGreaterThan(client.maxWaitMs);
    expect(client.snapshotWaitMs).toBeLessThan(30 * 60_000);
    expect(snapshotPolls).toBe(3);
  });

  it.each([2, 6])(
    "deletes the builder when Daytona applies %d GiB instead of the requested memory",
    async (appliedMemoryGiB) => {
      const methods: string[] = [];
      let deleted = false;
      const client = new DaytonaClient(baseEnv);
      client.pollDelayMs = 0;
      client.fetcher = vi.fn<typeof fetch>(async (input: RequestInfo | URL, init?: RequestInit) => {
        const request = new Request(input, init);
        const url = new URL(request.url);
        methods.push(`${request.method} ${url.pathname}`);
        if (request.method === "GET" && url.pathname.startsWith("/api/snapshots/")) {
          return new Response(null, { status: 404 });
        }
        if (request.method === "POST" && url.pathname === "/api/sandbox") {
          return Response.json({ id: "undersized-builder", state: "creating" });
        }
        if (request.method === "GET") {
          if (deleted) return new Response(null, { status: 404 });
          return Response.json({
            id: "undersized-builder",
            state: "started",
            cpu: 2,
            memory: appliedMemoryGiB,
            disk: 10,
          });
        }
        if (request.method === "DELETE") {
          deleted = true;
          return new Response(null, { status: 204 });
        }
        throw new Error(`unexpected request ${request.method} ${request.url}`);
      });

      await expect(
        client.bootstrapSnapshot("crabbox-ready-2x4x10", 2, 4, 10, baseImage),
      ).rejects.toThrow(`has ${appliedMemoryGiB} GiB memory after 4 GiB image build`);
      expect(methods.slice(-2)).toEqual([
        "DELETE /api/sandbox/undersized-builder",
        "GET /api/sandbox/undersized-builder",
      ]);
    },
  );

  it.each([
    { name: "missing expiry", lifetime: undefined },
    { name: "invalid expiry", lifetime: "invalid" },
    { name: "expired lifetime", lifetime: -1 },
    { name: "excess lifetime", lifetime: 601 },
  ])("rejects ready sandboxes with $name instead of assuming native TTL", async ({ lifetime }) => {
    const client = new DaytonaClient(baseEnv);
    const createdAt = Date.now();
    const autoDestroyAt =
      typeof lifetime === "number"
        ? new Date(createdAt + lifetime * 1_000).toISOString()
        : lifetime;
    client.fetcher = vi.fn<typeof fetch>(async () =>
      Response.json({
        ...ownedSandbox,
        state: "started",
        autoDestroyAt,
      }),
    );
    await expect(client.waitForStarted(sandboxID, 600)).rejects.toThrow(
      "did not confirm a valid native TTL",
    );
    expect(client.fetcher).toHaveBeenCalledTimes(1);
  });

  it.each(["started", "running", "ready", "active", " Active "])(
    "accepts Daytona ready state %j with independently anchored native TTL",
    async (state) => {
      const client = new DaytonaClient(baseEnv);
      const observedAt = Date.now();
      client.fetcher = async () =>
        Response.json({
          id: "sandbox-one",
          name: "crabbox-one",
          state,
          // Live Daytona reports creation and TTL anchoring on separate clock ticks.
          createdAt: new Date(observedAt - 64).toISOString(),
          autoDestroyAt: new Date(observedAt + 600_000 - 63).toISOString(),
          labels: { crabbox: "true" },
        });

      await expect(client.waitForStarted("sandbox-one", 600)).resolves.toMatchObject({
        cloudID: "sandbox-one",
        status: state,
      });
    },
  );

  it("does not extend the native deadline bound while polling readiness", async () => {
    vi.useFakeTimers();
    try {
      const client = new DaytonaClient(baseEnv);
      const startedAt = Date.now();
      client.pollDelayMs = 10_000;
      let reads = 0;
      client.fetcher = async () =>
        Response.json({
          ...ownedSandbox,
          state: ++reads === 1 ? "creating" : "started",
          createdAt: new Date(startedAt).toISOString(),
          autoDestroyAt: new Date(startedAt + 610_000).toISOString(),
        });
      const result = client.waitForStarted(sandboxID, 600).catch((error: unknown) => error);

      await vi.advanceTimersByTimeAsync(10_000);

      expect(await result).toMatchObject({
        message: `Daytona sandbox ${sandboxID} did not confirm a valid native TTL`,
      });
      expect(reads).toBe(2);
    } finally {
      vi.useRealTimers();
    }
  });

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

      await expect(client.waitForStarted("sandbox-one", 600)).rejects.toThrow(
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
