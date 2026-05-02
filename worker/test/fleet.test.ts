import { afterEach, describe, expect, it, vi } from "vitest";

import { FleetDurableObject } from "../src/fleet";
import type { PaymentGuard } from "../src/payments";
import type { Env, LeaseRecord, RunRecord } from "../src/types";

class MemoryStorage {
  private readonly values = new Map<string, unknown>();

  async get<T>(key: string): Promise<T | undefined> {
    return this.values.get(key) as T | undefined;
  }

  async put<T>(key: string, value: T): Promise<void> {
    this.values.set(key, value);
  }

  async delete(key: string): Promise<void> {
    this.values.delete(key);
  }

  async deleteAlarm(): Promise<void> {}

  async setAlarm(_time: number): Promise<void> {}

  async list<T>({ prefix = "" }: { prefix?: string } = {}): Promise<Map<string, T>> {
    const matches = new Map<string, T>();
    for (const [key, value] of this.values) {
      if (key.startsWith(prefix)) {
        matches.set(key, value as T);
      }
    }
    return matches;
  }

  seed<T>(key: string, value: T): void {
    this.values.set(key, value);
  }

  value<T>(key: string): T | undefined {
    return this.values.get(key) as T | undefined;
  }
}

describe("fleet lease identity and idle", () => {
  it("creates leases through the public route with slug and idle metadata", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage, {
      hetzner: fakeProvider(),
    });
    const create = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "cf-access-authenticated-user-email": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          slug: "Blue Lobster",
          provider: "hetzner",
          class: "standard",
          serverType: "cpx62",
          ttlSeconds: 1200,
          idleTimeoutSeconds: 360,
          keep: true,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );
    expect(create.status).toBe(201);
    const { lease } = (await create.json()) as { lease: LeaseRecord };
    expect(lease.id).toBe("cbx_abcdef123456");
    expect(lease.slug).toBe("blue-lobster");
    expect(lease.idleTimeoutSeconds).toBe(360);
    expect(lease.ttlSeconds).toBe(1200);
    expect(lease.lastTouchedAt).toBeTruthy();
    expect(Date.parse(lease.expiresAt)).toBeGreaterThan(Date.parse(lease.createdAt));

    const bySlug = await fleet.fetch(
      request("GET", "/v1/leases/blue-lobster", {
        headers: {
          "cf-access-authenticated-user-email": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
      }),
    );
    expect(bySlug.status).toBe(200);
    const found = (await bySlug.json()) as { lease: LeaseRecord };
    expect(found.lease.id).toBe("cbx_abcdef123456");
    expect(found.lease.slug).toBe("blue-lobster");
  });

  it("passes the Cloudflare request source IP as AWS SSH ingress CIDR", async () => {
    let awsCIDRs: string[] = [];
    const fleet = testFleet(new MemoryStorage(), {
      aws: fakeProvider((config) => {
        awsCIDRs = config.awsSSHCIDRs;
      }),
    });
    const create = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "cf-access-authenticated-user-email": "peter@example.com",
          "cf-connecting-ip": "203.0.113.7",
          "x-crabbox-org": "openclaw",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "aws",
          class: "standard",
          serverType: "c7a.8xlarge",
          ttlSeconds: 1200,
          idleTimeoutSeconds: 360,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );
    expect(create.status).toBe(201);
    expect(awsCIDRs).toEqual(["203.0.113.7/32"]);
  });

  it("honors requested AWS SSH ingress CIDRs over request source IP", async () => {
    let awsCIDRs: string[] = [];
    const fleet = testFleet(new MemoryStorage(), {
      aws: fakeProvider((config) => {
        awsCIDRs = config.awsSSHCIDRs;
      }),
    });
    const create = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "cf-access-authenticated-user-email": "peter@example.com",
          "cf-connecting-ip": "203.0.113.7",
          "x-crabbox-org": "openclaw",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "aws",
          class: "standard",
          serverType: "c7a.8xlarge",
          awsSSHCIDRs: ["198.51.100.0/24"],
          ttlSeconds: 1200,
          idleTimeoutSeconds: 360,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );
    expect(create.status).toBe(201);
    expect(awsCIDRs).toEqual(["198.51.100.0/24"]);
  });

  it("scopes non-admin usage to the current owner", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        owner: "peter@example.com",
        org: "openclaw",
        estimatedHourlyUSD: 1,
        maxEstimatedUSD: 1,
      }),
    );
    storage.seed(
      "lease:cbx_000000000002",
      testLease({
        id: "cbx_000000000002",
        owner: "friend@example.com",
        org: "openclaw",
        estimatedHourlyUSD: 1,
        maxEstimatedUSD: 1,
      }),
    );
    const usage = await fleet.fetch(
      request("GET", "/v1/usage?scope=all&owner=peter@example.com", {
        headers: {
          "cf-access-authenticated-user-email": "friend@example.com",
          "x-crabbox-org": "openclaw",
        },
      }),
    );
    expect(usage.status).toBe(200);
    const body = (await usage.json()) as {
      usage: { scope: string; owner: string; leases: number };
    };
    expect(body.usage.scope).toBe("user");
    expect(body.usage.owner).toBe("friend@example.com");
    expect(body.usage.leases).toBe(1);
  });

  it("resolves owner-scoped slugs and heartbeat extends idle expiry", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const touchedAt = new Date(Date.now() - 10 * 60 * 1000);
    const expiresAt = new Date(touchedAt.getTime() + 1800 * 1000);
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "peter@example.com",
        org: "openclaw",
        createdAt: touchedAt.toISOString(),
        updatedAt: touchedAt.toISOString(),
        lastTouchedAt: touchedAt.toISOString(),
        ttlSeconds: 5400,
        idleTimeoutSeconds: 1800,
        expiresAt: expiresAt.toISOString(),
      }),
    );

    const heartbeat = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/heartbeat", {
        headers: {
          "cf-access-authenticated-user-email": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
        body: { idleTimeoutSeconds: 2400 },
      }),
    );
    expect(heartbeat.status).toBe(200);
    const { lease } = (await heartbeat.json()) as { lease: LeaseRecord };
    expect(lease.id).toBe("cbx_000000000001");
    expect(lease.slug).toBe("blue-lobster");
    expect(lease.idleTimeoutSeconds).toBe(2400);
    expect(Date.parse(lease.expiresAt)).toBeGreaterThan(expiresAt.getTime());
  });

  it("hides exact lease IDs and lists from other non-admin users", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "peter@example.com",
        org: "openclaw",
      }),
    );
    storage.seed(
      "lease:cbx_000000000002",
      testLease({
        id: "cbx_000000000002",
        slug: "amber-krill",
        owner: "friend@example.com",
        org: "openclaw",
      }),
    );
    const friendHeaders = {
      "cf-access-authenticated-user-email": "friend@example.com",
      "x-crabbox-org": "openclaw",
    };

    const byExactID = await fleet.fetch(
      request("GET", "/v1/leases/cbx_000000000001", { headers: friendHeaders }),
    );
    expect(byExactID.status).toBe(404);

    const heartbeat = await fleet.fetch(
      request("POST", "/v1/leases/cbx_000000000001/heartbeat", {
        headers: friendHeaders,
        body: {},
      }),
    );
    expect(heartbeat.status).toBe(404);

    const list = await fleet.fetch(request("GET", "/v1/leases", { headers: friendHeaders }));
    const body = (await list.json()) as { leases: LeaseRecord[] };
    expect(body.leases.map((lease) => lease.id)).toEqual(["cbx_000000000002"]);
  });

  it("keeps pool inventory admin-only", async () => {
    const fleet = testFleet(new MemoryStorage(), {
      aws: fakeProvider(),
      hetzner: fakeProvider(),
    });
    const denied = await fleet.fetch(
      request("GET", "/v1/pool", {
        headers: {
          "cf-access-authenticated-user-email": "friend@example.com",
          "x-crabbox-org": "openclaw",
        },
      }),
    );
    expect(denied.status).toBe(403);

    const allowed = await fleet.fetch(
      request("GET", "/v1/pool", {
        headers: { "x-crabbox-admin": "true" },
      }),
    );
    expect(allowed.status).toBe(200);
  });

  it("creates, waits, and promotes AWS images through admin routes", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage, {
      aws: fakeProvider(),
    });
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        provider: "aws",
        cloudID: "i-123",
        region: "eu-west-1",
      }),
    );

    const denied = await fleet.fetch(
      request("POST", "/v1/images", {
        body: { leaseID: "cbx_000000000001", name: "openclaw-crabbox-test" },
      }),
    );
    expect(denied.status).toBe(403);

    const created = await fleet.fetch(
      request("POST", "/v1/images", {
        headers: { "x-crabbox-admin": "true" },
        body: { leaseID: "cbx_000000000001", name: "openclaw-crabbox-test" },
      }),
    );
    expect(created.status).toBe(201);
    const createdBody = (await created.json()) as { image: { id: string; state: string } };
    expect(createdBody.image).toEqual(
      expect.objectContaining({ id: "ami-000000000001", state: "pending" }),
    );

    const promoted = await fleet.fetch(
      request("POST", "/v1/images/ami-000000000001/promote", {
        headers: { "x-crabbox-admin": "true" },
        body: {},
      }),
    );
    expect(promoted.status).toBe(200);
    expect(storage.value("image:aws:promoted")).toEqual(
      expect.objectContaining({ id: "ami-000000000001", state: "available" }),
    );
  });

  it("creates and promotes Hetzner snapshots through admin routes", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage, {
      hetzner: fakeProvider(undefined, { imageID: "382206402" }),
    });
    storage.seed(
      "lease:cbx_000000000002",
      testLease({
        id: "cbx_000000000002",
        provider: "hetzner",
        serverID: 128794819,
        cloudID: "128794819",
      }),
    );

    const created = await fleet.fetch(
      request("POST", "/v1/images", {
        headers: { "x-crabbox-admin": "true" },
        body: { leaseID: "cbx_000000000002", name: "crabbox-rust-stable" },
      }),
    );
    expect(created.status).toBe(201);
    const createdBody = (await created.json()) as { image: { id: string; state: string } };
    expect(createdBody.image).toEqual(
      expect.objectContaining({ id: "382206402", state: "pending" }),
    );

    const promoted = await fleet.fetch(
      request("POST", "/v1/images/382206402/promote", {
        headers: { "x-crabbox-admin": "true" },
        body: {},
      }),
    );
    expect(promoted.status).toBe(200);
    expect(storage.value("image:hetzner:promoted")).toEqual(
      expect.objectContaining({ id: "382206402", state: "available" }),
    );
  });

  it("uses promoted Hetzner image when lease request omits image", async () => {
    const storage = new MemoryStorage();
    let observedImage = "";
    const fleet = testFleet(storage, {
      hetzner: fakeProvider((config) => {
        observedImage = config.image ?? "";
      }),
    });
    storage.seed("image:hetzner:promoted", {
      id: "382206402",
      name: "crabbox-rust-stable",
      state: "available",
      promotedAt: "2026-05-01T00:00:00.000Z",
    });

    const create = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "cf-access-authenticated-user-email": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
        body: {
          leaseID: "cbx_111111111111",
          provider: "hetzner",
          class: "standard",
          serverType: "cpx62",
          ttlSeconds: 1200,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );
    expect(create.status).toBe(201);
    expect(observedImage).toBe("382206402");
  });

  it("returns 402 from POST /v1/leases when a payment guard is configured and no payment is attached", async () => {
    let createdServer = false;
    const guard: PaymentGuard = {
      charge: (amount: string) => async () => ({
        status: 402,
        challenge: new Response(null, {
          status: 402,
          headers: {
            "www-authenticate": `Payment realm="crabbox", method="tempo", amount="${amount}"`,
          },
        }),
      }),
    };
    const fleet = testFleet(
      new MemoryStorage(),
      {
        hetzner: fakeProvider(() => {
          createdServer = true;
        }),
      },
      guard,
    );

    const create = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "cf-access-authenticated-user-email": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
        body: {
          leaseID: "cbx_aaaaaaaaaaaa",
          provider: "hetzner",
          class: "standard",
          serverType: "cpx62",
          ttlSeconds: 1200,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );
    expect(create.status).toBe(402);
    expect(create.headers.get("www-authenticate")).toContain('method="tempo"');
    expect(createdServer).toBe(false);
  });

  it("issues a lease bearer for MPP-authenticated lease creation", async () => {
    const guard: PaymentGuard = {
      charge: () => async () => ({
        withReceipt: (response: Response) => response,
      }),
    };
    const fleet = testFleet(new MemoryStorage(), { hetzner: fakeProvider() }, guard);

    const create = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-auth": "mpp",
          "x-crabbox-owner": "mpp:0xfeed",
          "x-crabbox-org": "openclaw",
          authorization: "Payment stub-credential",
        },
        body: {
          leaseID: "cbx_cccccccccccc",
          provider: "hetzner",
          class: "standard",
          serverType: "cpx62",
          ttlSeconds: 1200,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );
    expect(create.status).toBe(201);
    const body = (await create.json()) as { lease: LeaseRecord; bearer?: string };
    expect(body.bearer).toMatch(/^cbxl_/);
    expect(body.lease.id).toBe("cbx_cccccccccccc");
    expect(body.lease.owner).toBe("mpp:0xfeed");
  });

  it("scopes lease bearer access to a single lease ID", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    storage.seed(
      "lease:cbx_lease_aaaaa",
      testLease({
        id: "cbx_lease_aaaaa",
        owner: "mpp:0xfeed",
        org: "openclaw",
      }),
    );
    storage.seed(
      "lease:cbx_lease_bbbbb",
      testLease({
        id: "cbx_lease_bbbbb",
        owner: "mpp:0xfeed",
        org: "openclaw",
      }),
    );

    const ownLease = await fleet.fetch(
      request("GET", "/v1/leases/cbx_lease_aaaaa", {
        headers: {
          "x-crabbox-auth": "lease",
          "x-crabbox-owner": "mpp:0xfeed",
          "x-crabbox-org": "openclaw",
          "x-crabbox-lease-id": "cbx_lease_aaaaa",
        },
      }),
    );
    expect(ownLease.status).toBe(200);

    const otherLease = await fleet.fetch(
      request("GET", "/v1/leases/cbx_lease_bbbbb", {
        headers: {
          "x-crabbox-auth": "lease",
          "x-crabbox-owner": "mpp:0xfeed",
          "x-crabbox-org": "openclaw",
          "x-crabbox-lease-id": "cbx_lease_aaaaa",
        },
      }),
    );
    expect(otherLease.status).toBe(404);
  });

  it("does not issue a lease bearer for non-MPP auth", async () => {
    const fleet = testFleet(new MemoryStorage(), { hetzner: fakeProvider() });

    const create = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "cf-access-authenticated-user-email": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
        body: {
          leaseID: "cbx_dddddddddddd",
          provider: "hetzner",
          class: "standard",
          serverType: "cpx62",
          ttlSeconds: 1200,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );
    expect(create.status).toBe(201);
    const body = (await create.json()) as { lease: LeaseRecord; bearer?: string };
    expect(body.bearer).toBeUndefined();
  });

  it("provisions and attaches the receipt when payment guard accepts", async () => {
    let receiptHeader = "";
    const guard: PaymentGuard = {
      charge: () => async () => ({
        withReceipt: (response: Response) => {
          const headers = new Headers(response.headers);
          headers.set("payment-receipt", "stub-receipt-ok");
          receiptHeader = "stub-receipt-ok";
          return new Response(response.body, {
            status: response.status,
            headers,
          });
        },
      }),
    };
    const fleet = testFleet(new MemoryStorage(), { hetzner: fakeProvider() }, guard);

    const create = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "cf-access-authenticated-user-email": "peter@example.com",
          "x-crabbox-org": "openclaw",
          authorization: "Payment stub-credential",
        },
        body: {
          leaseID: "cbx_bbbbbbbbbbbb",
          provider: "hetzner",
          class: "standard",
          serverType: "cpx62",
          ttlSeconds: 1200,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );
    expect(create.status).toBe(201);
    expect(create.headers.get("payment-receipt")).toBe("stub-receipt-ok");
    expect(receiptHeader).toBe("stub-receipt-ok");
  });

  it("respects explicit lease image even when a promoted Hetzner image exists", async () => {
    const storage = new MemoryStorage();
    let observedImage = "";
    const fleet = testFleet(storage, {
      hetzner: fakeProvider((config) => {
        observedImage = config.image ?? "";
      }),
    });
    storage.seed("image:hetzner:promoted", {
      id: "382206402",
      name: "crabbox-rust-stable",
      state: "available",
      promotedAt: "2026-05-01T00:00:00.000Z",
    });

    const create = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "cf-access-authenticated-user-email": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
        body: {
          leaseID: "cbx_222222222222",
          provider: "hetzner",
          class: "standard",
          serverType: "cpx62",
          image: "ubuntu-24.04",
          ttlSeconds: 1200,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );
    expect(create.status).toBe(201);
    expect(observedImage).toBe("ubuntu-24.04");
  });
});

describe("fleet run history", () => {
  it("records finished runs and serves logs", async () => {
    const fleet = testFleet();
    const ownerHeaders = {
      "cf-access-authenticated-user-email": "peter@example.com",
      "x-crabbox-org": "openclaw",
    };
    const create = await fleet.fetch(
      request("POST", "/v1/runs", {
        headers: ownerHeaders,
        body: {
          leaseID: "cbx_000000000001",
          provider: "aws",
          class: "beast",
          serverType: "c7a.48xlarge",
          command: ["go", "test", "./..."],
        },
      }),
    );
    expect(create.status).toBe(201);
    const { run } = (await create.json()) as { run: { id: string } };

    const finish = await fleet.fetch(
      request("POST", `/v1/runs/${run.id}/finish`, {
        headers: ownerHeaders,
        body: {
          exitCode: 0,
          syncMs: 12,
          commandMs: 34,
          log: "ok\n",
          results: {
            format: "junit",
            files: ["junit.xml"],
            suites: 1,
            tests: 2,
            failures: 1,
            errors: 0,
            skipped: 0,
            timeSeconds: 1.2,
            failed: [{ suite: "pkg", name: "fails", kind: "failure" }],
          },
        },
      }),
    );
    expect(finish.status).toBe(200);
    const finished = (await finish.json()) as {
      run: { state: string; logBytes: number; results?: { tests: number } };
    };
    expect(finished.run.state).toBe("succeeded");
    expect(finished.run.logBytes).toBe(3);
    expect(finished.run.results?.tests).toBe(2);

    const listed = await fleet.fetch(
      request("GET", "/v1/runs?leaseID=cbx_000000000001", { headers: ownerHeaders }),
    );
    const listBody = (await listed.json()) as { runs: Array<{ id: string; owner: string }> };
    expect(listBody.runs).toHaveLength(1);
    expect(listBody.runs[0]?.id).toBe(run.id);
    expect(listBody.runs[0]?.owner).toBe("peter@example.com");

    const logs = await fleet.fetch(
      request("GET", `/v1/runs/${run.id}/logs`, { headers: ownerHeaders }),
    );
    expect(await logs.text()).toBe("ok\n");
  });

  it("hides run records and logs from other non-admin users", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    storage.seed(
      "run:run_000000000001",
      testRun({
        id: "run_000000000001",
        leaseID: "cbx_000000000001",
        owner: "peter@example.com",
        org: "openclaw",
      }),
    );
    storage.seed("runlog:run_000000000001", "secret log\n");
    storage.seed(
      "run:run_000000000002",
      testRun({
        id: "run_000000000002",
        leaseID: "cbx_000000000002",
        owner: "friend@example.com",
        org: "openclaw",
      }),
    );
    const friendHeaders = {
      "cf-access-authenticated-user-email": "friend@example.com",
      "x-crabbox-org": "openclaw",
    };

    const list = await fleet.fetch(request("GET", "/v1/runs", { headers: friendHeaders }));
    const listBody = (await list.json()) as { runs: RunRecord[] };
    expect(listBody.runs.map((run) => run.id)).toEqual(["run_000000000002"]);

    const read = await fleet.fetch(
      request("GET", "/v1/runs/run_000000000001", { headers: friendHeaders }),
    );
    expect(read.status).toBe(404);

    const logs = await fleet.fetch(
      request("GET", "/v1/runs/run_000000000001/logs", { headers: friendHeaders }),
    );
    expect(logs.status).toBe(404);

    const finish = await fleet.fetch(
      request("POST", "/v1/runs/run_000000000001/finish", {
        headers: friendHeaders,
        body: { exitCode: 0, log: "overwrite\n" },
      }),
    );
    expect(finish.status).toBe(404);
    expect(storage.value<string>("runlog:run_000000000001")).toBe("secret log\n");
  });

  it("bounds stored result summaries", async () => {
    const fleet = testFleet();
    const create = await fleet.fetch(
      request("POST", "/v1/runs", {
        body: {
          leaseID: "cbx_000000000001",
          provider: "aws",
          class: "beast",
          serverType: "c7a.48xlarge",
          command: ["go", "test", "./..."],
        },
      }),
    );
    expect(create.status).toBe(201);
    const { run } = (await create.json()) as { run: { id: string } };
    const failed = Array.from({ length: 150 }, (_, index) => ({
      suite: "pkg",
      name: `fails-${index}`,
      kind: "failure" as const,
      message: "x".repeat(5000),
    }));

    const finish = await fleet.fetch(
      request("POST", `/v1/runs/${run.id}/finish`, {
        body: {
          exitCode: 1,
          log: "",
          results: {
            format: "junit",
            files: Array.from({ length: 80 }, (_, index) => `junit-${index}.xml`),
            suites: 1,
            tests: 150,
            failures: 150,
            errors: 0,
            skipped: 0,
            timeSeconds: 1.2,
            failed,
          },
        },
      }),
    );
    expect(finish.status).toBe(200);
    const finished = (await finish.json()) as {
      run: { results?: { files: string[]; failed: Array<{ message?: string }> } };
    };
    expect(finished.run.results?.files).toHaveLength(50);
    expect(finished.run.results?.failed).toHaveLength(100);
    expect(
      new TextEncoder().encode(finished.run.results?.failed[0]?.message ?? "").byteLength,
    ).toBe(4096);
  });
});

describe("fleet identity", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("reports owner and org from request context", async () => {
    const fleet = testFleet();
    const response = await fleet.fetch(
      request("GET", "/v1/whoami", {
        headers: {
          "cf-access-authenticated-user-email": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
      }),
    );
    expect(await response.json()).toEqual({
      owner: "peter@example.com",
      org: "openclaw",
      auth: "bearer",
    });
  });

  it("reports forwarded GitHub auth mode", async () => {
    const fleet = testFleet();
    const response = await fleet.fetch(
      request("GET", "/v1/whoami", {
        headers: {
          "x-crabbox-auth": "github",
          "x-crabbox-owner": "friend@example.com",
          "x-crabbox-org": "openclaw",
        },
      }),
    );
    expect(await response.json()).toEqual({
      owner: "friend@example.com",
      org: "openclaw",
      auth: "github",
    });
  });

  it("rejects admin routes without an admin token context", async () => {
    const fleet = testFleet();
    const response = await fleet.fetch(request("GET", "/v1/admin/leases"));
    expect(response.status).toBe(403);
  });

  it("starts GitHub login and keeps polling secret server-side", async () => {
    const storage = new MemoryStorage();
    const fleet = new FleetDurableObject(
      { storage } as unknown as DurableObjectState,
      {
        CRABBOX_DEFAULT_ORG: "openclaw",
        CRABBOX_GITHUB_CLIENT_ID: "github-client",
        CRABBOX_GITHUB_CLIENT_SECRET: "github-secret",
        CRABBOX_SHARED_TOKEN: "shared",
      } as Env,
    );
    const pollSecret = "local-poll-secret";
    const start = await fleet.fetch(
      request("POST", "/v1/auth/github/start", {
        body: {
          pollSecretHash: await sha256HexForTest(pollSecret),
          provider: "aws",
        },
      }),
    );
    expect(start.status).toBe(200);
    const body = (await start.json()) as { loginID: string; url: string };
    expect(body.loginID).toMatch(/^login_/);
    const url = new URL(body.url);
    expect(url.origin + url.pathname).toBe("https://github.com/login/oauth/authorize");
    expect(url.searchParams.get("client_id")).toBe("github-client");
    expect(url.searchParams.get("scope")).toBe("read:user user:email read:org");

    const poll = await fleet.fetch(
      request("POST", "/v1/auth/github/poll", {
        body: {
          loginID: body.loginID,
          pollSecret,
        },
      }),
    );
    expect(poll.status).toBe(200);
    await expect(poll.json()).resolves.toMatchObject({ status: "pending" });
  });

  it("cleans expired GitHub login attempts before rate limiting", async () => {
    const storage = new MemoryStorage();
    const fleet = new FleetDurableObject(
      { storage } as unknown as DurableObjectState,
      {
        CRABBOX_DEFAULT_ORG: "openclaw",
        CRABBOX_GITHUB_CLIENT_ID: "github-client",
        CRABBOX_GITHUB_CLIENT_SECRET: "github-secret",
        CRABBOX_SHARED_TOKEN: "shared",
      } as Env,
    );
    storage.seed("oauth:login_old", {
      id: "login_old",
      state: "state_old",
      pollSecretHash: "0".repeat(64),
      createdAt: "2026-05-01T00:00:00.000Z",
      expiresAt: "2026-05-01T00:00:00.000Z",
    });
    storage.seed("oauth_state:state_old", "login_old");

    const start = await fleet.fetch(
      request("POST", "/v1/auth/github/start", {
        body: {
          pollSecretHash: await sha256HexForTest("new-secret"),
          provider: "aws",
        },
      }),
    );
    expect(start.status).toBe(200);
    expect(storage.value("oauth:login_old")).toBeUndefined();
    expect(storage.value("oauth_state:state_old")).toBeUndefined();
  });

  it("requires GitHub org membership before completing login", async () => {
    const { fleet, loginID, state, pollSecret } = await startGitHubLogin();
    vi.stubGlobal("fetch", githubFetchMock({ member: false }));

    const callback = await fleet.fetch(
      request("GET", `/v1/auth/github/callback?code=ok&state=${state}`),
    );
    expect(callback.status).toBe(403);

    const poll = await fleet.fetch(
      request("POST", "/v1/auth/github/poll", {
        body: {
          loginID,
          pollSecret,
        },
      }),
    );
    expect(poll.status).toBe(400);
    await expect(poll.json()).resolves.toMatchObject({
      status: "failed",
      error: "GitHub user friend is not an active member of openclaw.",
    });
  });

  it("mints GitHub login tokens for allowed org members", async () => {
    const { fleet, loginID, state, pollSecret } = await startGitHubLogin();
    vi.stubGlobal("fetch", githubFetchMock({ member: true }));

    const callback = await fleet.fetch(
      request("GET", `/v1/auth/github/callback?code=ok&state=${state}`),
    );
    expect(callback.status).toBe(200);

    const poll = await fleet.fetch(
      request("POST", "/v1/auth/github/poll", {
        body: {
          loginID,
          pollSecret,
        },
      }),
    );
    expect(poll.status).toBe(200);
    const body = (await poll.json()) as {
      status: string;
      token?: string;
      owner?: string;
      org?: string;
      login?: string;
    };
    expect(body).toMatchObject({
      status: "complete",
      owner: "friend@example.com",
      org: "openclaw",
      login: "friend",
    });
    expect(body.token).toMatch(/^cbxu_/);
  });
});

async function startGitHubLogin(): Promise<{
  fleet: FleetDurableObject;
  loginID: string;
  pollSecret: string;
  state: string;
}> {
  const storage = new MemoryStorage();
  const fleet = new FleetDurableObject(
    { storage } as unknown as DurableObjectState,
    {
      CRABBOX_DEFAULT_ORG: "openclaw",
      CRABBOX_GITHUB_CLIENT_ID: "github-client",
      CRABBOX_GITHUB_CLIENT_SECRET: "github-secret",
      CRABBOX_SHARED_TOKEN: "shared",
      CRABBOX_SESSION_SECRET: "session-secret",
    } as Env,
  );
  const pollSecret = "local-poll-secret";
  const start = await fleet.fetch(
    request("POST", "/v1/auth/github/start", {
      body: {
        pollSecretHash: await sha256HexForTest(pollSecret),
        provider: "aws",
      },
    }),
  );
  expect(start.status).toBe(200);
  const body = (await start.json()) as { loginID: string; url: string };
  const url = new URL(body.url);
  const state = url.searchParams.get("state");
  expect(state).toBeTruthy();
  return { fleet, loginID: body.loginID, pollSecret, state: state || "" };
}

function githubFetchMock({ member }: { member: boolean }) {
  return vi.fn<(input: RequestInfo | URL) => Promise<Response>>(async (input) => {
    const url =
      typeof input === "string" ? input : input instanceof URL ? input.toString() : input.url;
    if (url === "https://github.com/login/oauth/access_token") {
      return jsonResponse({ access_token: "github-access-token" });
    }
    if (url === "https://api.github.com/user") {
      return jsonResponse({ login: "friend", name: "Friendly User", email: null });
    }
    if (url === "https://api.github.com/user/emails") {
      return jsonResponse([{ email: "friend@example.com", primary: true, verified: true }]);
    }
    if (url === "https://api.github.com/user/memberships/orgs/openclaw") {
      return member
        ? jsonResponse({ state: "active", organization: { login: "openclaw" } })
        : jsonResponse({ message: "Not Found" }, 404);
    }
    return jsonResponse({ message: `unexpected ${url}` }, 500);
  });
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

function testFleet(
  storage = new MemoryStorage(),
  providers = {},
  paymentGuard?: PaymentGuard,
): FleetDurableObject {
  return new FleetDurableObject(
    { storage } as unknown as DurableObjectState,
    {
      CRABBOX_DEFAULT_ORG: "default-org",
      CRABBOX_SESSION_SECRET: "test-session-secret",
    } as Env,
    providers,
    paymentGuard,
  );
}

function fakeProvider(
  onCreate?: (config: { awsSSHCIDRs: string[]; image?: string }) => void,
  options: { imageID?: string } = {},
) {
  const imageID = options.imageID ?? "ami-000000000001";
  return {
    async listCrabboxServers() {
      return [];
    },
    async createServerWithFallback(
      config: { awsSSHCIDRs: string[]; image?: string },
      _leaseID: string,
      slug: string,
    ) {
      onCreate?.(config);
      return {
        server: {
          provider: "hetzner",
          id: 123,
          cloudID: "123",
          name: `crabbox-${slug}`,
          status: "running",
          serverType: "cpx62",
          host: "192.0.2.10",
          labels: {},
        },
        serverType: "cpx62",
      };
    },
    async deleteServer() {},
    async createImage(_instanceID: string, name: string) {
      return { id: imageID, name, state: "pending", region: "eu-west-1" };
    },
    async getImage(id: string) {
      return {
        id,
        name: "openclaw-crabbox-test",
        state: "available",
        region: "eu-west-1",
      };
    },
    async deleteSSHKey() {},
    async hourlyPriceUSD() {
      return 0.1;
    },
  };
}

function testLease(overrides: Partial<LeaseRecord>): LeaseRecord {
  return {
    id: "cbx_000000000000",
    provider: "hetzner",
    cloudID: "123",
    owner: "peter@example.com",
    org: "openclaw",
    profile: "default",
    class: "beast",
    serverType: "ccx63",
    serverID: 123,
    serverName: "crabbox-blue-lobster",
    providerKey: "crabbox-cbx-000000000000",
    host: "192.0.2.1",
    sshUser: "crabbox",
    sshPort: "2222",
    sshFallbackPorts: ["22"],
    workRoot: "/work/crabbox",
    keep: true,
    ttlSeconds: 5400,
    estimatedHourlyUSD: 1,
    maxEstimatedUSD: 1.5,
    state: "active",
    createdAt: "2026-05-01T00:00:00.000Z",
    updatedAt: "2026-05-01T00:00:00.000Z",
    expiresAt: "2026-05-01T01:30:00.000Z",
    ...overrides,
  };
}

function testRun(overrides: Partial<RunRecord>): RunRecord {
  return {
    id: "run_000000000000",
    leaseID: "cbx_000000000000",
    owner: "peter@example.com",
    org: "openclaw",
    provider: "hetzner",
    class: "standard",
    serverType: "cpx62",
    command: ["echo", "ok"],
    state: "running",
    logBytes: 0,
    logTruncated: false,
    startedAt: "2026-05-01T00:00:00.000Z",
    ...overrides,
  };
}

function request(
  method: string,
  path: string,
  init: { headers?: Record<string, string>; body?: unknown } = {},
): Request {
  return new Request(`https://crabbox.test${path}`, {
    method,
    headers: {
      ...(init.body === undefined ? {} : { "content-type": "application/json" }),
      ...init.headers,
    },
    body: init.body === undefined ? undefined : JSON.stringify(init.body),
  });
}

async function sha256HexForTest(value: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(value));
  return [...new Uint8Array(digest)].map((byte) => byte.toString(16).padStart(2, "0")).join("");
}
