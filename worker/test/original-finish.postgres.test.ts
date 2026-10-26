import { spawn } from "node:child_process";
import { createHash, generateKeyPairSync, randomBytes, sign } from "node:crypto";
import fs from "node:fs/promises";
import { createServer } from "node:http";
import path from "node:path";

import { Pool } from "pg";
import { expect, it } from "vitest";

import { NodeCoordinatorRuntime } from "../node/node-runtime";
import { routeCoordinatorRequest } from "../src/coordinator-entry";
import { FleetCoordinator } from "../src/fleet";
import type { Env, RunRecord, TerminalRunReceipt } from "../src/types";

// No Whoami/authentication/storage mocks. These are synthetic, newly admitted
// fixture runs, not historical execution evidence. Never use ambient DATABASE_URL.
// Run with vitest.finish-postgres.config.ts and a mode-0600 JSON config containing
// database (empty loopback PostgreSQL fixture), cli (built executable), evidence
// (private output directory), sharedToken and adminToken (fixture-only values).
it("recovers exact original bytes through the CLI, real auth and PostgreSQL", async () => {
  const configPath = process.env.CRABBOX_FINISH_PROOF_CONFIG;
  if (!configPath) throw new Error("CRABBOX_FINISH_PROOF_CONFIG private fixture file required");
  expect((await fs.stat(configPath)).mode & 0o077).toBe(0);
  const config = JSON.parse(await fs.readFile(configPath, "utf8")) as {
    database: string;
    cli: string;
    evidence: string;
    sharedToken: string;
    adminToken: string;
  };
  const database = new URL(config.database);
  expect(database.hostname).toBe("127.0.0.1");
  expect(database.pathname).toBe("/crabbox_recovery_test");
  const pool = new Pool({ connectionString: database.href });
  let runtime: NodeCoordinatorRuntime | undefined;
  const cases: Record<string, unknown>[] = [];
  const report: Record<string, unknown> = { cases, syntheticOnly: true };
  const env = {
    CRABBOX_SHARED_TOKEN: config.sharedToken,
    CRABBOX_SHARED_OWNER: "alice@example.com",
    CRABBOX_DEFAULT_ORG: "example-org",
    CRABBOX_ADMIN_TOKEN: config.adminToken,
  } as Env;
  let fleet: FleetCoordinator;
  let revokeAfterRunInspect = false;
  let origin = "";
  const requests: { method: string; path: string; status: number; bodySHA256?: string }[] = [];
  report.requests = requests;
  const server = createServer(async (incoming, outgoing) => {
    try {
      const chunks: Buffer[] = [];
      for await (const chunk of incoming) chunks.push(Buffer.from(chunk));
      const body = Buffer.concat(chunks),
        headers = new Headers();
      for (const [key, value] of Object.entries(incoming.headers)) {
        if (typeof value === "string") headers.set(key, value);
      }
      const request = new Request(origin + incoming.url, {
        method: incoming.method,
        headers,
        ...(body.length ? { body } : {}),
      });
      const response = await routeCoordinatorRequest(request, env, (req) => fleet.fetch(req));
      requests.push({
        method: request.method,
        path: incoming.url!,
        status: response.status,
        ...(body.length ? { bodySHA256: digest(body) } : {}),
      });
      // Rotate actual configured authority after real inspection, before CLI
      // POST. The authentication and storage implementations are not replaced.
      if (
        revokeAfterRunInspect &&
        request.method === "GET" &&
        /^\/v1\/runs\/run_[a-f0-9]+$/.test(incoming.url!)
      ) {
        env.CRABBOX_SHARED_TOKEN = randomBytes(32).toString("hex");
        revokeAfterRunInspect = false;
      }
      outgoing.writeHead(response.status, Object.fromEntries(response.headers));
      outgoing.end(Buffer.from(await response.arrayBuffer()));
    } catch {
      outgoing.writeHead(500).end();
    }
  });
  try {
    // Exercise actual migration from the previous JSONB-only shape. Refuse an
    // existing database rather than deleting or borrowing its records.
    expect(
      (await pool.query("select to_regclass('crabbox.coordinator_kv') as relation")).rows[0]
        .relation,
    ).toBeNull();
    await pool.query("create schema crabbox");
    await pool.query(
      "create table crabbox.coordinator_kv (key text primary key, value jsonb not null, updated_at timestamptz not null default now())",
    );
    const legacy = { nested: ["old-data", 17], text: "preserved" };
    await pool.query("insert into crabbox.coordinator_kv (key,value) values ($1,$2::jsonb)", [
      "proof:legacy",
      JSON.stringify(legacy),
    ]);
    runtime = new NodeCoordinatorRuntime(database.href);
    await runtime.storage.initialize();
    fleet = new FleetCoordinator(runtime, env);
    expect(await runtime.storage.get("proof:legacy")).toEqual(legacy);
    const columns = (
      await pool.query(
        "select column_name from information_schema.columns where table_schema='crabbox' and table_name='coordinator_kv' order by column_name",
      )
    ).rows.map((r) => r.column_name);
    expect(columns).toContain("value_text");
    expect(columns).toContain("value_text_updated_at");
    report.compatibility = {
      oldJSONBRowRetained: true,
      columns,
      scope:
        "actual additive initialization and JSONB fallback; not arbitrary historical release compatibility",
    };
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    const address = server.address();
    if (!address || typeof address === "string") throw new Error("loopback address missing");
    origin = "http://127.0.0.1:" + address.port;
    const snapshot = async (runID: string) =>
      (
        await pool.query(
          "select key, value, value_text, updated_at::text, value_text_updated_at::text from crabbox.coordinator_kv where key like $1 order by key",
          ["%" + runID + "%"],
        )
      ).rows;
    const admit = async (label: string) => {
      env.CRABBOX_SHARED_TOKEN = config.sharedToken;
      env.CRABBOX_SHARED_OWNER = "alice@example.com";
      env.CRABBOX_DEFAULT_ORG = "example-org";
      const runID = "run_" + randomBytes(16).toString("hex"),
        command = ["fixture-only", label];
      const response = await fetch(origin + "/v1/runs/" + runID, {
        method: "PUT",
        headers: {
          authorization: "Bearer " + config.sharedToken,
          "content-type": "application/json",
        },
        body: JSON.stringify({ command, provider: "hetzner", target: "linux" }),
      });
      expect(response.status).toBe(201);
      const { run } = (await response.json()) as { run: RunRecord };
      const original = signedOriginal(run, command),
        file = path.join(config.evidence, label + ".pending.json");
      const envelope = Buffer.from(
        JSON.stringify({
          schema_version: 1,
          coordinator: origin,
          run_id: runID,
          owner: run.owner,
          org: run.org,
          body: original.body.toString("base64"),
          body_sha256: digest(original.body),
        }),
      );
      await fs.writeFile(file, envelope, { mode: 0o600, flag: "wx" });
      return { runID, run, file, envelope, ...original };
    };
    const cli = async (
      label: string,
      original: Awaited<ReturnType<typeof admit>>,
      token = config.sharedToken,
      owner = "alice@example.com",
      org = "example-org",
    ) => {
      const home = path.join(config.evidence, "home-" + label);
      await fs.mkdir(home, { mode: 0o700 });
      const args = [
        "receipt",
        original.runID,
        "--recover",
        original.file,
        "--expected-signer",
        original.receipt.signer,
        "--expected-finish-sha256",
        digest(original.body),
      ];
      const started = requests.length;
      const result = await child(config.cli, args, {
        PATH: process.env.PATH!,
        HOME: home,
        XDG_CONFIG_HOME: home,
        XDG_STATE_HOME: home,
        CRABBOX_COORDINATOR: origin,
        CRABBOX_COORDINATOR_TOKEN: token,
        CRABBOX_OWNER: owner,
        CRABBOX_ORG: org,
      });
      await fs.writeFile(path.join(config.evidence, label + ".stdout"), result.stdout, {
        mode: 0o600,
      });
      await fs.writeFile(path.join(config.evidence, label + ".stderr"), result.stderr, {
        mode: 0o600,
      });
      expect(await fs.readFile(original.file)).toEqual(original.envelope);
      return { ...result, requests: requests.slice(started) };
    };
    /* oxlint-disable eslint/no-await-in-loop -- Each case rotates shared fixture authority and must finish before the next admission. */
    for (const spec of [
      { label: "other-owner", owner: "bob@example.com" },
      { label: "other-org", org: "other-org" },
      { label: "admin-other-owner", owner: "bob@example.com", admin: true },
      { label: "admin-other-org", org: "other-org", admin: true },
      { label: "revoked-before-post", revoke: true },
    ]) {
      const original = await admit(spec.label),
        before = await snapshot(original.runID);
      if (!spec.admin) {
        env.CRABBOX_SHARED_OWNER = spec.owner ?? "alice@example.com";
        env.CRABBOX_DEFAULT_ORG = spec.org ?? "example-org";
      }
      revokeAfterRunInspect = !!spec.revoke;
      const result = await cli(
        spec.label,
        original,
        spec.admin ? config.adminToken : config.sharedToken,
        spec.owner ?? "alice@example.com",
        spec.org ?? "example-org",
      );
      expect(result.code).toBe(spec.revoke ? 7 : 2);
      expect(result.stderr).toContain(spec.revoke ? "http 401" : "owner or organization mismatch");
      const after = await snapshot(original.runID);
      expect(after).toEqual(before);
      await fs.writeFile(
        path.join(config.evidence, spec.label + ".storage.json"),
        JSON.stringify({ before, after }, null, 2),
        { mode: 0o600 },
      );
      const posts = result.requests.filter((r) => r.method === "POST");
      expect(posts.map((request) => request.status)).toEqual(spec.revoke ? [401] : []);
      cases.push({
        label: spec.label,
        code: result.code,
        runID: original.runID,
        terminalMutation: false,
        originalFileUnchanged: true,
        requests: result.requests,
      });
    }
    /* oxlint-enable eslint/no-await-in-loop */
    const original = await admit("exact-byte-recovery"),
      first = await cli("exact-byte-recovery", original);
    expect(first.code).toBe(0);
    expect(JSON.parse(first.stdout)).toEqual(original.receipt);
    expect(first.requests.find((r) => r.method === "POST")).toMatchObject({
      status: 200,
      bodySHA256: digest(original.body),
    });
    const committed = await snapshot(original.runID);
    const stored = await runtime.storage.get<RunRecord>("run:" + original.runID);
    expect(stored?.terminalReceipt).toEqual(original.receipt);
    expect(stored?.eventCount).toBe(2);
    expect(stored?.state).toBe("succeeded");
    cases.push({
      label: "exact-byte-recovery",
      code: first.code,
      runID: original.runID,
      exactBodySHA256: digest(original.body),
      signer: original.receipt.signer,
      storedSignature: stored?.terminalReceipt?.signature,
      eventCount: stored?.eventCount,
      requests: first.requests,
    });
    await fs.writeFile(
      path.join(config.evidence, "committed-storage.json"),
      JSON.stringify(committed, null, 2),
      { mode: 0o600 },
    );
    // New real Pool, same database: no in-memory records survive reopening.
    await runtime.stop();
    runtime = new NodeCoordinatorRuntime(database.href);
    await runtime.storage.initialize();
    fleet = new FleetCoordinator(runtime, env);
    expect(await snapshot(original.runID)).toEqual(committed);
    expect(await runtime.storage.get("proof:legacy")).toEqual(legacy);
    expect(
      (await runtime.storage.get<RunRecord>("run:" + original.runID))?.terminalReceipt,
    ).toEqual(original.receipt);
    const duplicate = await cli("exact-byte-idempotent-replay", original);
    expect(duplicate.code).toBe(0);
    expect(JSON.parse(duplicate.stdout)).toEqual(original.receipt);
    expect(await snapshot(original.runID)).toEqual(committed);
    cases.push({
      label: "reopen-idempotent-replay",
      code: duplicate.code,
      runID: original.runID,
      storageByteValuesUnchanged: true,
      requests: duplicate.requests,
    });
    report.outcome = "passed";
  } finally {
    await new Promise<void>((resolve, reject) =>
      server.close((error) =>
        error && error.code !== "ERR_SERVER_NOT_RUNNING" ? reject(error) : resolve(),
      ),
    );
    await runtime?.stop();
    await pool.end();
    report.serverClosed = true;
    report.poolsClosed = true;
    await fs.writeFile(
      path.join(config.evidence, "real-proof.json"),
      JSON.stringify(report, null, 2),
      { mode: 0o600 },
    );
  }
}, 120_000);
function digest(value: Buffer): string {
  return "sha256:" + createHash("sha256").update(value).digest("hex");
}
function payload(prefix: string, values: string[]): Buffer {
  return Buffer.concat([
    Buffer.from(prefix),
    ...values.flatMap((value) => {
      const bytes = Buffer.from(value),
        length = Buffer.alloc(4);
      length.writeUInt32BE(bytes.length);
      return [length, bytes];
    }),
  ]);
}
function signedOriginal(
  run: RunRecord,
  command: string[],
): { body: Buffer; receipt: TerminalRunReceipt } {
  const { privateKey, publicKey } = generateKeyPairSync("ed25519");
  const rawKey = Buffer.from(publicKey.export({ format: "jwk" }).x!, "base64url"),
    log = "synthetic local fixture output\n";
  const endedAt = new Date().toISOString();
  const receipt: TerminalRunReceipt = {
    schema_version: 2,
    receipt_type: "terminal",
    started_at: run.startedAt,
    ended_at: endedAt,
    provider: run.provider,
    run_id: run.id,
    command: command.join(" "),
    command_sha256: digest(payload("crabbox-command-v1\0", command)),
    exit_code: 0,
    sync_ms: 0,
    command_ms: 0,
    duration_ms: Date.parse(endedAt) - Date.parse(run.startedAt),
    log_sha256: digest(Buffer.from(log)),
    retained_log_sha256: digest(Buffer.from(log)),
    log_truncated: false,
    public_key: rawKey.toString("base64"),
    signer: digest(rawKey),
    signature: "",
  };
  const fields = [
    "schema_version",
    "receipt_type",
    "started_at",
    "ended_at",
    "provider",
    "lease_id",
    "slug",
    "run_id",
    "command",
    "command_sha256",
    "exit_code",
    "sync_ms",
    "command_ms",
    "duration_ms",
    "log_sha256",
    "retained_log_sha256",
    "log_truncated",
    "public_key",
    "signer",
  ] as const;
  receipt.signature = sign(
    null,
    payload(
      "crabbox-terminal-receipt-v2\0",
      fields.map((f) => String(receipt[f] ?? "")),
    ),
    privateKey,
  ).toString("base64");
  // Noncanonical whitespace detects reconstruction. The fixture private key is
  // discarded; each original envelope is written once and remains immutable.
  return {
    body: Buffer.from(
      JSON.stringify(
        {
          exitCode: 0,
          syncMs: 0,
          commandMs: 0,
          log,
          logChunks: [log],
          logTruncated: false,
          results: null,
          receipt,
        },
        null,
        2,
      ) + "\n",
    ),
    receipt,
  };
}
async function child(
  command: string,
  args: string[],
  env: NodeJS.ProcessEnv,
): Promise<{ code: number | null; stdout: string; stderr: string }> {
  return new Promise((resolve, reject) => {
    const proc = spawn(command, args, { env, stdio: ["ignore", "pipe", "pipe"] });
    let stdout = "",
      stderr = "";
    proc.stdout.on("data", (b) => {
      stdout += b;
    });
    proc.stderr.on("data", (b) => {
      stderr += b;
    });
    const timer = setTimeout(() => proc.kill("SIGKILL"), 30_000);
    proc.once("error", reject);
    proc.once("close", (code) => {
      clearTimeout(timer);
      resolve({ code, stdout, stderr });
    });
  });
}
