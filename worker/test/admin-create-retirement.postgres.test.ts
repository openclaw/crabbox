import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { randomBytes } from "node:crypto";
import fs from "node:fs/promises";
import { createServer } from "node:http";
import os from "node:os";
import path from "node:path";

import { Pool } from "pg";
import { afterEach, expect, it, vi } from "vitest";

import { NodeCoordinatorRuntime } from "../node/node-runtime";
import { AsyncMutex } from "../src/async-mutex";
import { routeCoordinatorRequest } from "../src/coordinator-entry";
import { FleetCoordinator } from "../src/fleet";
import { orgKeyForLabel } from "../src/org-identity";
import { providerKeyForLease } from "../src/provider-key";
import type { CreateAttemptRecord, Env, LeaseRecord } from "../src/types";
import { withProviderHTTP } from "./fixtures/provider-http";

// This job owns a fresh PostgreSQL service. Never use an ambient DATABASE_URL.
const database = new URL("postgresql://127.0.0.1:55432/crabbox_recovery_test");
database.username = "crabbox_test";
database.password = "crabbox-test-only";
const owner = "alice@example.com";
const org = orgKeyForLabel("example-org");
const region = "eu-west-1";
const cutoff = new Date(Date.now() - 1_000).toISOString();
const createdAt = new Date(Date.now() - 60_000).toISOString();

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

it("retires lost-token creates through real auth, CLI, PostgreSQL and provider HTTP", async () => {
  const pool = new Pool({ connectionString: database.href });
  const home = await fs.mkdtemp(path.join(os.tmpdir(), "crabbox-retire-test-"));
  const adminToken = randomBytes(32).toString("hex");
  const sharedToken = randomBytes(32).toString("hex");
  const env = {
    CRABBOX_SHARED_TOKEN: sharedToken,
    CRABBOX_SHARED_OWNER: owner,
    CRABBOX_ADMIN_TOKEN: adminToken,
    CRABBOX_DEFAULT_ORG: "example-org",
    CRABBOX_AWS_REGION: region,
    CRABBOX_AWS_ORPHAN_SWEEP_ENABLED: "0",
    AWS_ACCESS_KEY_ID: "synthetic-key",
    AWS_SECRET_ACCESS_KEY: "synthetic-secret",
  } as Env;
  const nativeFetch = globalThis.fetch;
  let active: ReturnType<typeof coordinator> | undefined;
  let origin = "";
  const server = createServer(async (incoming, outgoing) => {
    try {
      const chunks: Buffer[] = [];
      for await (const chunk of incoming) chunks.push(Buffer.from(chunk));
      const body = Buffer.concat(chunks);
      const headers = new Headers();
      for (const [key, value] of Object.entries(incoming.headers)) {
        if (typeof value === "string") headers.set(key, value);
      }
      const request = new Request(origin + incoming.url, {
        method: incoming.method,
        headers,
        ...(body.length ? { body } : {}),
      });
      // Use the shared production entry point: callers cannot inject trusted admin headers.
      const response = await routeCoordinatorRequest(request, env, (req) =>
        active!.fleet.fetch(req),
      );
      outgoing.writeHead(response.status, Object.fromEntries(response.headers));
      outgoing.end(Buffer.from(await response.arrayBuffer()));
    } catch (error) {
      outgoing.writeHead(500).end(String(error));
    }
  });
  const request = (url: string, body?: unknown, token = adminToken, spoof = false) =>
    nativeFetch(origin + url, {
      signal: AbortSignal.timeout(10_000),
      method: body === undefined ? "GET" : "POST",
      headers: {
        authorization: "Bearer " + token,
        "content-type": "application/json",
        "x-crabbox-owner": owner,
        "x-crabbox-org": "example-org",
        ...(spoof
          ? { "x-crabbox-admin": "true", "x-crabbox-admin-grant-version": "a".repeat(64) }
          : {}),
      },
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    });
  const preview = async (id: string) => {
    const result = await request(retirementPath(id));
    expect(result.status).toBe(200);
    const text = await result.text();
    expect(text).not.toContain("cat_");
    return JSON.parse(text) as {
      revision: string;
      cleanup: string;
      binding: string;
      fenced: boolean;
    };
  };
  const cli = (id: string, revision?: string) =>
    child(
      path.resolve("../bin/crabbox"),
      [
        "admin",
        "retire-create",
        "--id",
        id,
        "--json",
        ...(revision
          ? [
              "--force",
              "--revision",
              revision,
              "--owner",
              owner,
              "--org",
              org,
              "--created-before",
              cutoff,
              "--reason",
              "recover synthetic interrupted creation",
            ]
          : []),
      ],
      {
        PATH: process.env.PATH!,
        HOME: home,
        XDG_CONFIG_HOME: home,
        XDG_STATE_HOME: home,
        CRABBOX_COORDINATOR: origin,
        CRABBOX_COORDINATOR_TOKEN: adminToken,
        CRABBOX_OWNER: owner,
        CRABBOX_ORG: "example-org",
      },
      home,
    );
  const reopen = async () => {
    await active?.runtime.stop();
    active = coordinator(env);
    await active.runtime.storage.initialize();
  };
  try {
    expect(
      (await pool.query("select to_regclass('crabbox.coordinator_kv') as relation")).rows[0]
        .relation,
    ).toBeNull();
    // Previous JSONB-only storage and v1 record shapes must survive current initialization.
    await pool.query("create schema crabbox");
    await pool.query(
      "create table crabbox.coordinator_kv (key text primary key, value jsonb not null, updated_at timestamptz not null default now())",
    );
    const oldAttempts = [
      attempt("cbx_000000000091", "pending"),
      attempt("cbx_000000000092", "canceled"),
    ];
    await Promise.all(
      oldAttempts.map((value) =>
        pool.query("insert into crabbox.coordinator_kv (key,value) values ($1,$2::jsonb)", [
          "create-attempt:" + value.requestedLeaseID,
          JSON.stringify(value),
        ]),
      ),
    );
    await reopen();
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    const address = server.address();
    if (!address || typeof address === "string") throw new Error("loopback address missing");
    origin = "http://127.0.0.1:" + address.port;
    const first = oldAttempts[0]!;
    expect(await active!.runtime.storage.get("create-attempt:" + first.requestedLeaseID)).toEqual(
      first,
    );
    const before = await preview(first.requestedLeaseID);
    const input = retirementInput(before.revision);
    const oldRow = await physicalAttempt(pool, first.requestedLeaseID);
    // A valid shared bearer cannot escalate through client-controlled headers.
    const spoofed = await Promise.all(
      [undefined, input].map((body) =>
        request(retirementPath(first.requestedLeaseID), body, sharedToken, true),
      ),
    );
    expect(spoofed.map((response) => response.status)).toEqual([403, 403]);
    env.CRABBOX_ADMIN_TOKEN = randomBytes(32).toString("hex");
    expect((await request(retirementPath(first.requestedLeaseID), input)).status).toBe(401);
    expect((await request(retirementPath(first.requestedLeaseID), undefined, "")).status).toBe(401);
    expect(await physicalAttempt(pool, first.requestedLeaseID)).toEqual(oldRow);
    env.CRABBOX_ADMIN_TOKEN = adminToken;

    // Exact preview/CAS also rejects a generation changed through another real connection.
    await active!.runtime.storage.put("create-attempt:" + first.requestedLeaseID, {
      ...first,
      token: "cat_" + "a".repeat(32),
    });
    expect((await request(retirementPath(first.requestedLeaseID), input)).status).toBe(409);
    await active!.runtime.storage.put("create-attempt:" + first.requestedLeaseID, first);

    const replayNetwork: { origin: string; action: string | null }[] = [];
    vi.stubGlobal("fetch", async (resource: RequestInfo | URL, init?: RequestInit) => {
      const wire = resource instanceof Request ? resource : new Request(resource, init);
      const observed = {
        origin: new URL(wire.url).origin,
        action: new URLSearchParams(await wire.clone().text()).get("Action"),
      };
      replayNetwork.push(observed);
      // Legacy tokenless admission prices before checking its ID fence. Serve
      // that read only; any allocation/deletion is both denied and asserted below.
      assert.deepEqual(observed, {
        origin: "https://ec2." + region + ".amazonaws.com",
        action: "DescribeSpotPriceHistory",
      });
      return new Response(
        "<DescribeSpotPriceHistoryResponse><spotPriceHistorySet /></DescribeSpotPriceHistoryResponse>",
        {
          headers: { "content-type": "text/xml" },
        },
      );
    });
    /* oxlint-disable eslint/no-await-in-loop -- each CLI retirement is followed by a fresh Pool and replay against its persisted fence. */
    for (const original of oldAttempts) {
      const id = original.requestedLeaseID;
      const inspected = await cli(id);
      expect(inspected).toMatchObject({ code: 0 });
      const reviewed = JSON.parse(inspected.stdout) as { revision: string };
      const retired = await cli(id, reviewed.revision);
      expect(retired).toMatchObject({ code: 0 });
      expect(retired.stdout).not.toContain(original.token);
      expect(JSON.parse(retired.stdout)).toMatchObject({
        binding: "unbound",
        fenced: true,
        cleanup: "not-allocated",
      });
      const saved = await physicalAttempt(pool, id);
      expect(saved).toMatchObject({ version: 2, state: "canceled", token: original.token });
      await reopen();
      expect(await active!.runtime.storage.get("create-attempt:" + id)).toEqual(saved);
      const replayBodies = [{ leaseID: id, createAttemptID: original.token }, { leaseID: id }];
      for (const body of replayBodies) {
        const replay = await request(
          "/v1/leases",
          {
            ...body,
            provider: "aws",
            sshPublicKey: "ssh-ed25519 synthetic",
          },
          sharedToken,
        );
        expect(replay.status).toBe(409);
      }
      const fixed = await nativeFetch(origin + "/v1/leases/" + id, {
        signal: AbortSignal.timeout(10_000),
        method: "PUT",
        headers: { authorization: "Bearer " + sharedToken, "content-type": "application/json" },
        body: JSON.stringify({ provider: "aws", sshPublicKey: "ssh-ed25519 synthetic" }),
      });
      expect(fixed.status).toBe(409);
      expect(await active!.runtime.storage.get("lease:" + id)).toBeUndefined();
      expect(await physicalAttempt(pool, id)).toEqual(saved);
    }
    /* oxlint-enable eslint/no-await-in-loop */
    expect(replayNetwork).toEqual(
      oldAttempts.map(() => ({
        origin: "https://ec2." + region + ".amazonaws.com",
        action: "DescribeSpotPriceHistory",
      })),
    );
    vi.unstubAllGlobals();

    // The actual AWS adapter/client perform signed HTTP against a synthetic loopback service.
    // No cloud credentials or resources are used; no auth, storage or provider methods are mocked.
    const positive = lease("cbx_000000000093");
    const reassigned = lease("cbx_000000000094");
    const actions: string[] = [];
    let observed = () => {};
    let resume = () => {};
    const observation = new Promise<void>((resolve) => {
      observed = resolve;
    });
    const resumed = new Promise<void>((resolve) => {
      resume = resolve;
    });
    const terminated = new Set<string>();
    const resources = [positive, reassigned];
    await withProviderHTTP(
      ["https://sts." + region + ".amazonaws.com", "https://ec2." + region + ".amazonaws.com"],
      async (incoming, outgoing) => {
        try {
          const chunks: Buffer[] = [];
          for await (const chunk of incoming) chunks.push(Buffer.from(chunk));
          const params = new URLSearchParams(Buffer.concat(chunks).toString());
          const action = params.get("Action")!;
          actions.push(action);
          let xml: string;
          if (action === "GetCallerIdentity") {
            xml =
              "<GetCallerIdentityResponse><GetCallerIdentityResult><Account>123456789012</Account><Arn>arn:aws:iam::123456789012:user/synthetic</Arn><UserId>synthetic</UserId></GetCallerIdentityResult></GetCallerIdentityResponse>";
          } else if (action === "DescribeInstances") {
            const value = resources.find((item) => item.cloudID === params.get("InstanceId.1"));
            if (!value) throw new Error("unexpected synthetic instance");
            if (value.id === reassigned.id) {
              observed();
              await resumed;
            }
            xml =
              "<DescribeInstancesResponse><requestId>synthetic-read</requestId><reservationSet>" +
              (terminated.has(value.cloudID)
                ? ""
                : "<item><instancesSet><item><instanceId>" +
                  value.cloudID +
                  "</instanceId><instanceState><name>running</name></instanceState><tagSet>" +
                  tags(value) +
                  "</tagSet></item></instancesSet></item>") +
              "</reservationSet></DescribeInstancesResponse>";
          } else if (action === "TerminateInstances") {
            assert.equal(params.get("InstanceId.1"), positive.cloudID);
            terminated.add(positive.cloudID);
            xml =
              "<TerminateInstancesResponse><instancesSet><item><instanceId>" +
              positive.cloudID +
              "</instanceId></item></instancesSet></TerminateInstancesResponse>";
          } else if (action === "DescribeKeyPairs") {
            assert.equal(params.get("KeyName.1"), positive.providerKey);
            xml =
              "<DescribeKeyPairsResponse><keySet><item><keyPairId>key-00000000000000093</keyPairId><tagSet>" +
              tags(positive) +
              "</tagSet></item></keySet></DescribeKeyPairsResponse>";
          } else if (action === "DeleteKeyPair") {
            assert.equal(params.get("KeyPairId"), "key-00000000000000093");
            xml = "<DeleteKeyPairResponse />";
          } else {
            throw new Error("unexpected synthetic AWS action: " + action);
          }
          outgoing.writeHead(200, { "content-type": "text/xml" }).end(xml);
        } catch (error) {
          outgoing.writeHead(500).end(String(error));
        }
      },
      async () => {
        await Promise.all(
          resources.map(async (value) => {
            await active!.runtime.storage.put("lease:" + value.id, value);
            await active!.runtime.storage.put("create-attempt:" + value.id, boundAttempt(value));
          }),
        );
        const inspected = await preview(positive.id);
        const retired = await cli(positive.id, inspected.revision);
        expect(retired).toMatchObject({ code: 0 });
        expect(JSON.parse(retired.stdout)).toMatchObject({
          binding: "bound",
          fenced: true,
          cleanup: "confirmed",
        });
        expect(actions.filter((action) => action === "TerminateInstances")).toHaveLength(1);
        expect(actions.filter((action) => action === "DeleteKeyPair")).toHaveLength(1);
        await reopen();
        expect(await preview(positive.id)).toMatchObject({
          binding: "bound",
          fenced: true,
          cleanup: "confirmed",
        });
        expect(
          (await active!.runtime.storage.get<LeaseRecord>("lease:" + positive.id))
            ?.cleanupCompletedAt,
        ).toBeTruthy();

        const beforeRace = actions.length;
        const reviewed = await preview(reassigned.id);
        const retiring = cli(reassigned.id, reviewed.revision);
        try {
          await Promise.race([
            observation,
            retiring.then((result) => {
              throw new Error("retirement ended before provider observation: " + result.stderr);
            }),
          ]);
          const replacement = { ...reassigned, createAttemptGeneration: "cag_" + "e".repeat(32) };
          // Another SQL connection replaces the generation while the provider read is in flight.
          await pool.query(
            "update crabbox.coordinator_kv set value=$2::jsonb, value_text=null, value_text_updated_at=null, updated_at=clock_timestamp() where key=$1",
            ["lease:" + replacement.id, JSON.stringify(replacement)],
          );
          resume();
          const result = await retiring;
          expect(result.code).not.toBe(0);
          expect(actions.slice(beforeRace)).toContain("DescribeInstances");
          expect(actions.slice(beforeRace)).not.toContain("TerminateInstances");
          expect(actions.slice(beforeRace)).not.toContain("DeleteKeyPair");
          expect(await active!.runtime.storage.get("lease:" + replacement.id)).toEqual(replacement);
          expect(await preview(replacement.id)).toMatchObject({
            binding: "conflict",
            fenced: false,
            cleanup: "unproven",
          });
          await reopen();
          expect(await active!.runtime.storage.get("lease:" + replacement.id)).toEqual(replacement);
        } finally {
          resume();
          await retiring;
        }
      },
    );
    console.info(
      "retirement proof: real auth + CLI + PostgreSQL reopen; v1 records retained; delayed creates fenced; synthetic AWS cleanup confirmed; reassigned generation preserved",
    );
  } finally {
    server.closeAllConnections();
    await new Promise<void>((resolve) => server.close(() => resolve()));
    await active?.runtime.stop();
    await pool.end();
    await fs.rm(home, { recursive: true, force: true });
  }
}, 60_000);

function coordinator(env: Env) {
  const runtime = new NodeCoordinatorRuntime(database.href);
  const mutex = new AsyncMutex();
  runtime.setOperationRunner((callback) => mutex.run(callback));
  return { runtime, fleet: new FleetCoordinator(runtime, env) };
}

function attempt(id: string, state: "pending" | "canceled"): CreateAttemptRecord {
  return {
    version: 1,
    requestedLeaseID: id,
    token: "cat_" + id.slice(4).padStart(32, "0"),
    owner,
    org,
    state,
    createdAt,
    updatedAt: createdAt,
  };
}

function boundAttempt(value: LeaseRecord): CreateAttemptRecord {
  return {
    ...attempt(value.id, "pending"),
    canonicalLeaseID: value.id,
    cloudID: value.cloudID,
    generation: value.createAttemptGeneration,
  };
}

function lease(id: string): LeaseRecord {
  return {
    id,
    cloudID: "i-" + id.slice(4).padStart(17, "0"),
    slug: "synthetic-" + id.slice(-2),
    provider: "aws",
    providerScope: "aws:account:123456789012",
    region,
    owner,
    org,
    profile: "default",
    class: "tiny",
    serverType: "t3.micro",
    serverID: 0,
    serverName: "crabbox-synthetic-" + id.slice(-2),
    providerKey: providerKeyForLease(id),
    providerKeyCleanupOwned: true,
    host: "192.0.2.10",
    sshUser: "ubuntu",
    sshPort: "22",
    workRoot: "/work/crabbox",
    keep: false,
    ttlSeconds: 3600,
    estimatedHourlyUSD: 0.01,
    maxEstimatedUSD: 1,
    state: "active",
    createdAt,
    updatedAt: createdAt,
    expiresAt: new Date(Date.now() + 3_600_000).toISOString(),
    createAttemptID: attempt(id, "pending").token,
    createAttemptGeneration: "cag_" + id.slice(4).padStart(32, "0"),
  };
}

function tags(value: LeaseRecord): string {
  return Object.entries({
    crabbox: "true",
    created_by: "crabbox",
    lease: value.id,
    owner: "alice_example.com",
    provider: "aws",
    slug: value.slug!,
    provider_key: value.providerKey,
  })
    .map(([key, item]) => "<item><key>" + key + "</key><value>" + item + "</value></item>")
    .join("");
}

function retirementPath(id: string) {
  return "/v1/admin/leases/" + id + "/retire-create";
}
function retirementInput(revision: string) {
  return {
    revision,
    owner,
    org,
    createdBefore: cutoff,
    reason: "recover synthetic interrupted creation",
  };
}
async function physicalAttempt(pool: Pool, id: string): Promise<unknown> {
  return (
    await pool.query("select value from crabbox.coordinator_kv where key=$1", [
      "create-attempt:" + id,
    ])
  ).rows[0]?.value;
}

async function child(binary: string, args: string[], env: NodeJS.ProcessEnv, cwd: string) {
  return new Promise<{ code: number | null; stdout: string; stderr: string }>((resolve, reject) => {
    // Bound only this fixture's child; await close so timeout cannot leave a CLI behind.
    const process = spawn(binary, args, {
      env,
      cwd,
      stdio: ["ignore", "pipe", "pipe"],
      timeout: 20_000,
      killSignal: "SIGKILL",
    });
    let stdout = "",
      stderr = "";
    process.stdout.on("data", (chunk) => {
      stdout += chunk;
    });
    process.stderr.on("data", (chunk) => {
      stderr += chunk;
    });
    process.once("error", reject);
    process.once("close", (code, signal) => {
      if (signal || code === null)
        reject(new Error("fixture CLI did not exit normally: " + signal));
      else resolve({ code, stdout, stderr });
    });
  });
}
