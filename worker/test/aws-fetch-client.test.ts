import { createServer, type IncomingMessage, type ServerResponse } from "node:http";

import { afterEach, expect, it, vi } from "vitest";

import { RefreshingAWSFetchClient } from "../src/aws-fetch-client";
import { createAWSProvisioningDiagnostics } from "../src/aws-provisioning-diagnostics";

const cleanups: Array<() => Promise<void>> = [];

afterEach(async () => {
  await Promise.all(cleanups.splice(0).map((close) => close()));
  vi.restoreAllMocks();
});

async function localTransport(
  handle: (request: IncomingMessage, response: ServerResponse) => void,
) {
  const server = createServer(handle);
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  cleanups.push(
    () =>
      new Promise<void>((resolve, reject) => {
        server.closeAllConnections();
        server.close((error) => (error ? reject(error) : resolve()));
      }),
  );
  const address = server.address();
  if (!address || typeof address === "string") throw new Error("missing local transport address");
  return `http://127.0.0.1:${address.port}/`;
}

const credentials = async () => ({
  accessKeyId: "fixture-access-canary",
  secretAccessKey: "fixture-secret-canary",
  sessionToken: "fixture-session-canary",
});

it("separates credential preparation from the signed request span", async () => {
  const url = await localTransport((_request, response) => {
    setTimeout(() => response.end("ready"), 30);
  });
  const { diagnostics, finish } = observe();
  const client = new RefreshingAWSFetchClient(
    async () => {
      await new Promise((resolve) => setTimeout(resolve, 20));
      return credentials();
    },
    "ec2",
    "eu-west-1",
  );
  const response = await diagnostics.measure("key_pair", () => client.fetch(url));
  expect(await response.text()).toBe("ready");
  const transport = finish("success");
  expect(transport.credentialsMs).toBeGreaterThanOrEqual(15);
  expect(transport.requestMs).toBeGreaterThanOrEqual(25);
  expect(transport.signMs).toBeGreaterThanOrEqual(0);
  expect(transport.signMs).toBeLessThanOrEqual(transport.requestMs);
});

function observe() {
  const log = vi.spyOn(console, "info").mockImplementation(() => {});
  const diagnostics = createAWSProvisioningDiagnostics("cbx_000000000001", "eu-west-1");
  return {
    diagnostics,
    log,
    finish(outcome: "success" | "failure") {
      diagnostics.finish(outcome);
      const encoded = String(log.mock.calls.at(-1)![0]);
      for (const privateValue of [
        "fixture-access-canary",
        "fixture-secret-canary",
        "fixture-session-canary",
        "payload-canary",
        "failure-canary",
        "127.0.0.1",
      ])
        expect(encoded).not.toContain(privateValue);
      expect(encoded.length).toBeLessThan(8192);
      return JSON.parse(encoded).steps[0].transport;
    },
  };
}

it.each([
  { name: "normal success", statuses: [200], attempts: 1 },
  { name: "original SDK retries 503 and 429", statuses: [503, 429, 200], attempts: 3 },
  { name: "original SDK returns 400 without retry", statuses: [400], attempts: 1 },
])("observes $name without changing request bytes or response", async ({ statuses, attempts }) => {
  const bodies: string[] = [];
  const url = await localTransport((request, response) => {
    let body = "";
    request.setEncoding("utf8");
    request.on("data", (chunk: string) => {
      body += chunk;
    });
    request.on("end", () => {
      bodies.push(body);
      response.statusCode = statuses[bodies.length - 1] ?? 500;
      response.end("response-canary");
    });
  });
  const { diagnostics, finish } = observe();
  const client = new RefreshingAWSFetchClient(credentials, "ec2", "eu-west-1");
  const result = await diagnostics.measure("key_pair", () =>
    client.fetch(url, { method: "POST", body: "payload-canary" }),
  );
  expect(result.status).toBe(statuses.at(-1));
  expect(await result.text()).toBe("response-canary");
  expect(bodies).toEqual(Array(attempts).fill("payload-canary"));
  expect(finish("success")).toMatchObject({
    requests: 1,
    credentialFailures: 0,
    signInvocations: attempts,
    signCompletions: attempts,
    signFailures: 0,
    requestFailures: 0,
  });
});

it("records credential failure before signing and preserves the original error", async () => {
  let requests = 0;
  const url = await localTransport((_request, response) => {
    requests += 1;
    response.end();
  });
  const failure = new Error("failure-canary");
  const { diagnostics, finish } = observe();
  const client = new RefreshingAWSFetchClient(
    async () => {
      throw failure;
    },
    "ec2",
    "eu-west-1",
  );
  await expect(diagnostics.measure("key_pair", () => client.fetch(url))).rejects.toBe(failure);
  expect(requests).toBe(0);
  expect(finish("failure")).toMatchObject({
    requests: 1,
    credentialFailures: 1,
    signInvocations: 0,
    signCompletions: 0,
    signFailures: 0,
    requestFailures: 0,
  });
});

it("records actual SDK signing failure without calling the local transport", async () => {
  let requests = 0;
  const url = await localTransport((_request, response) => {
    requests += 1;
    response.end();
  });
  const { diagnostics, finish } = observe();
  const client = new RefreshingAWSFetchClient(credentials, "ec2", "eu-west-1");
  await expect(
    diagnostics.measure("key_pair", () =>
      client.fetch(url, { headers: { "invalid\nheader": "payload-canary" } }),
    ),
  ).rejects.toBeInstanceOf(TypeError);
  expect(requests).toBe(0);
  expect(finish("failure")).toMatchObject({
    requests: 1,
    credentialFailures: 0,
    signInvocations: 1,
    signCompletions: 0,
    signFailures: 1,
    requestFailures: 1,
  });
});

it("preserves a real network failure without adding retries", async () => {
  let requests = 0;
  const url = await localTransport((request) => {
    requests += 1;
    request.socket.destroy();
  });
  const { diagnostics, finish } = observe();
  const client = new RefreshingAWSFetchClient(credentials, "ec2", "eu-west-1");
  await expect(
    diagnostics.measure("key_pair", () =>
      client.fetch(url, { method: "POST", body: "payload-canary" }),
    ),
  ).rejects.toBeInstanceOf(TypeError);
  expect(requests).toBe(1);
  expect(finish("failure")).toMatchObject({
    requests: 1,
    credentialFailures: 0,
    signInvocations: 1,
    signCompletions: 1,
    signFailures: 0,
    requestFailures: 1,
  });
});

it("attributes interleaved shared-client requests to their deepest operation and lease", async () => {
  const entered = Promise.withResolvers<void>();
  const release = Promise.withResolvers<void>();
  const url = await localTransport((request, response) => {
    if (request.url === "/slow") {
      entered.resolve();
      void release.promise.then(() => response.end("slow"));
    } else response.end("fast");
  });
  const log = vi.spyOn(console, "info").mockImplementation(() => {});
  const first = createAWSProvisioningDiagnostics("cbx_000000000001", "eu-west-1");
  const second = createAWSProvisioningDiagnostics("cbx_000000000002", "eu-west-1");
  const client = new RefreshingAWSFetchClient(credentials, "ec2", "eu-west-1");
  const slow = first.measure("security_group", () =>
    first.measure("authorize_ingress", async () => (await client.fetch(url + "slow")).text()),
  );
  try {
    await Promise.race([
      entered.promise,
      slow.then(() => {
        throw new Error("slow request finished before its gate");
      }),
    ]);
    expect(
      await second.measure("quota", async () => (await client.fetch(url + "fast")).text()),
    ).toBe("fast");
    second.finish("success");
    expect(JSON.parse(String(log.mock.calls[0]![0]))).toMatchObject({
      leaseId: "cbx_000000000002",
      steps: [{ name: "quota", transport: { requests: 1, signInvocations: 1 } }],
    });
    expect(
      await first.measure("quota", async () => (await client.fetch(url + "fast")).text()),
    ).toBe("fast");
  } finally {
    release.resolve();
    expect(await slow).toBe("slow");
  }
  first.finish("success");
  const record = JSON.parse(String(log.mock.calls[1]![0]));
  expect(record.leaseId).toBe("cbx_000000000001");
  expect(
    record.steps.find((step: { name: string }) => step.name === "security_group").transport,
  ).toBeUndefined();
  expect(
    record.steps.find((step: { name: string }) => step.name === "authorize_ingress").transport,
  ).toMatchObject({ requests: 1, signInvocations: 1, signCompletions: 1 });
  expect(
    record.steps.find((step: { name: string }) => step.name === "quota").transport,
  ).toMatchObject({ requests: 1, signInvocations: 1, signCompletions: 1 });
  const count = log.mock.calls.length;
  expect(await (await client.fetch(url)).text()).toBe("fast");
  expect(log.mock.calls).toHaveLength(count);
});
