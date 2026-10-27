// A subprocess fixture, not a deployment entrypoint. Only the cloud adapter is
// synthetic; HTTP authentication, FleetCoordinator, jobs and PostgreSQL are real.
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import fs from "node:fs/promises";
import { createServer } from "node:http";

import { NodeCoordinatorRuntime } from "../../node/node-runtime";
import { AsyncMutex } from "../../src/async-mutex";
import { routeCoordinatorRequest } from "../../src/coordinator-entry";
import { AWSProvider, FleetCoordinator } from "../../src/fleet";
import { providerLabelValue } from "../../src/provider-labels";

const configPath = process.argv[2];
assert.equal((await fs.stat(configPath)).mode & 0o077, 0);
const fixtureConfig = JSON.parse(await fs.readFile(configPath, "utf8"));
const database = new URL(fixtureConfig.database);
assert.equal(database.hostname, "127.0.0.1");
assert.ok(
  ["/crabbox_finish_upgrade_test", "/crabbox_finish_fresh_test"].includes(database.pathname),
);
// Never use ambient cloud credentials or allow an accidental provider request.
globalThis.fetch = async () => {
  throw new Error("fixture forbids outbound fetch");
};
const env = {
  CRABBOX_SHARED_TOKEN: fixtureConfig.sharedToken,
  CRABBOX_ADMIN_TOKEN: fixtureConfig.adminToken,
  CRABBOX_SHARED_OWNER: "alice@example.com",
  CRABBOX_DEFAULT_ORG: "example-org",
  CRABBOX_AWS_REGION: "eu-west-1",
  CRABBOX_AWS_ORPHAN_SWEEP_ENABLED: "0",
};
const runtime = new NodeCoordinatorRuntime(database.href);
const scope = "aws:account:123456789012";
const region = "eu-west-1";
const serverFor = (lease) => ({
  provider: "aws",
  id: 0,
  cloudID: "i-00000" + lease.id.slice(4),
  name: "crabbox-" + lease.slug,
  status: "running",
  host: "192.0.2.10",
  serverType: "t3.small",
  region,
  labels: {
    lease: lease.id,
    crabbox: "true",
    created_by: "crabbox",
    provider: "aws",
    slug: providerLabelValue(lease.slug),
    owner: providerLabelValue(lease.owner),
  },
});
// Only cloud operations are synthetic. Auth, routing and storage remain real.
class SyntheticAWS extends AWSProvider {
  async prepareLeaseConfig(config) {
    return config;
  }
  async prepareLeaseCreate(config, lease) {
    return { config, lease: { ...lease, providerScope: scope }, provisioning: {} };
  }
  async refreshLeaseAccess() {}
  async refreshLeaseAccessForResolution() {}
  async reconcileLeaseAccess() {}
  async listCrabboxServers() {
    return [];
  }
  async listReconciliationResources() {
    return [];
  }
  async hourlyPriceUSD() {
    return 0;
  }
  async createServerWithFallback(config, leaseID, slug, owner, provisioning) {
    const server = serverFor({ id: leaseID, slug, owner });
    assert.equal(
      await provisioning.onResourceCreated({
        provider: "aws",
        cloudID: server.cloudID,
        region,
        providerScope: scope,
      }),
      true,
    );
    return { server, serverType: "t3.small", attempts: [] };
  }
  async finalizeLeaseCreate(config, lease) {
    return { config, lease };
  }
  async releaseLease() {
    throw new Error("fixture must not release a provider resource");
  }
  async deleteServer() {
    throw new Error("fixture must not delete a provider resource");
  }
}
const fleet = new FleetCoordinator(runtime, env, {
  aws: new SyntheticAWS(
    {
      ...env,
      AWS_ACCESS_KEY_ID: "synthetic-fixture-not-a-key",
      AWS_SECRET_ACCESS_KEY: "synthetic-fixture-not-a-secret",
    },
    region,
    runtime.storage,
  ),
});
const mutex = new AsyncMutex();
runtime.setOperationRunner((callback) => mutex.run(callback));
await runtime.start(() => fleet.alarm());
let origin;
const server = createServer(async (incoming, outgoing) => {
  try {
    const chunks = [];
    for await (const chunk of incoming) chunks.push(chunk);
    const body = Buffer.concat(chunks);
    const request = new Request(origin + incoming.url, {
      method: incoming.method,
      headers: incoming.headers,
      ...(body.length ? { body } : {}),
    });
    const response = await routeCoordinatorRequest(request, env, (req) => fleet.fetch(req));
    process.send?.({
      type: "request",
      method: request.method,
      path: incoming.url,
      status: response.status,
      ...(body.length
        ? { bodySHA256: "sha256:" + createHash("sha256").update(body).digest("hex") }
        : {}),
    });
    outgoing.writeHead(response.status, Object.fromEntries(response.headers));
    outgoing.end(Buffer.from(await response.arrayBuffer()));
  } catch (error) {
    console.error(error);
    outgoing.writeHead(500).end();
  }
});
await new Promise((resolve) => server.listen(fixtureConfig.port, "127.0.0.1", resolve));
origin = "http://127.0.0.1:" + server.address().port;
process.send?.({ type: "ready", origin, pid: process.pid, node: process.version });
let stopping = false;
process.on("SIGTERM", async () => {
  if (stopping) return;
  stopping = true;
  await new Promise((resolve) => server.close(resolve));
  await runtime.stop();
  process.send?.({ type: "stopped", pid: process.pid });
  process.disconnect?.();
});
