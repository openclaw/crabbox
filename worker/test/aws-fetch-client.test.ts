import { createServer, type IncomingMessage, type ServerResponse } from "node:http";

import { afterEach, expect, it, vi } from "vitest";

import { EC2SpotClient } from "../src/aws";
import {
  FixedAWSFetchClient,
  RefreshingAWSFetchClient,
  resolvedAWSCredentials,
} from "../src/aws-fetch-client";
import { createAWSProvisioningDiagnostics } from "../src/aws-provisioning-diagnostics";
import { leaseConfig, type LeaseConfig } from "../src/config";
import type { Env } from "../src/types";

const cleanups: Array<() => Promise<void>> = [];

afterEach(async () => {
  await Promise.all(cleanups.splice(0).map((close) => close()));
  vi.unstubAllGlobals();
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

it.each(
  [500, 503].flatMap((status) =>
    [false, true].map((fixedOperation) => ({ status, fixedOperation })),
  ),
)(
  "hands a parsed HTTP $status capacity rejection to the next configured type without retrying it (fixed operation=$fixedOperation)",
  async ({ status, fixedOperation }) => {
    const { launch, requests, credentialProvider } = await capacityTransport({
      status,
      fixedOperation,
      reject: (request) => request.get("InstanceType") === "t3.small",
    });
    const result = await launch();
    expect(credentialProvider.mock.calls.length === 1).toBe(fixedOperation);
    expect(requests.map((request) => request.get("InstanceType"))).toEqual([
      "t3.small",
      "t3.medium",
    ]);
    expect(result.serverType).toBe("t3.medium");
    expect(result.attempts).toEqual([
      expect.objectContaining({ serverType: "t3.small", category: "capacity" }),
    ]);
    expect(requests.map((request) => request.get("ClientToken"))).toEqual([
      "cbx_000000000001",
      "cbx_000000000001",
    ]);
  },
);

it.each([
  {
    name: "generic 500",
    status: 500,
    body: "<Response><Errors><Error><Code>InternalError</Code></Error></Errors></Response>",
  },
  { name: "opaque 503", status: 503, body: "InsufficientInstanceCapacity" },
  {
    name: "incomplete XML",
    status: 503,
    body: "<Response><Errors><Error><Code>InsufficientInstanceCapacity</Code>",
  },
  {
    name: "capacity mentioned only in the message",
    status: 500,
    body: "<Response><Errors><Error><Code>InternalError</Code><Message>InsufficientInstanceCapacity</Message></Error></Errors></Response>",
  },
  { name: "429", status: 429 },
])("preserves the SDK retry budget for $name with alternatives", async ({ status, body }) => {
  const { launch, requests } = await capacityTransport({
    status,
    errorBody: body,
    reject: (_request, index) => index < 10,
  });
  const result = await launch();
  expect(requests.map((request) => request.get("InstanceType"))).toEqual(
    Array(11).fill("t3.small"),
  );
  expect(result.serverType).toBe("t3.small");
  expect(result.attempts).toBeUndefined();
});

it.each([
  { name: "explicit exact type", overrides: { serverTypeExplicit: true }, type: "t3.small" },
  {
    name: "no alternate type or market",
    overrides: { awsInstanceTypes: ["t3.small"] },
    type: "t3.small",
  },
  {
    name: "pinned Mac host",
    overrides: {
      target: "macos",
      hostID: "h-pinned",
      serverType: "mac1.metal",
      capacityMarket: "on-demand",
    },
    type: "mac1.metal",
  },
] satisfies Array<{ name: string; overrides: Partial<LeaseConfig>; type: string }>)(
  "preserves capacity retries for $name",
  async ({ overrides, type }) => {
    const { launch, requests } = await capacityTransport({
      status: 500,
      reject: (_request, index) => index < 10,
    });
    const result = await launch(overrides);
    expect(requests.map((request) => request.get("InstanceType"))).toEqual(Array(11).fill(type));
    expect(result.serverType).toBe(type);
    expect(requests.map((request) => request.get("Placement.HostId"))).toEqual(
      Array(11).fill(overrides.hostID ?? null),
    );
  },
);

it("hands capacity rejection to the configured market without retrying Spot", async () => {
  const { launch, requests } = await capacityTransport({
    status: 500,
    reject: (request) => request.has("InstanceMarketOptions.MarketType"),
  });
  const result = await launch({
    awsInstanceTypes: ["t3.small"],
    capacityFallback: "on-demand-after-120s",
  });
  expect(requests.map((request) => request.has("InstanceMarketOptions.MarketType"))).toEqual([
    true,
    false,
  ]);
  expect(result.market).toBe("on-demand");
});

it("also hands off capacity between eligible On-Demand candidates", async () => {
  const { launch, requests } = await capacityTransport({
    status: 500,
    reject: (request) =>
      request.has("InstanceMarketOptions.MarketType") || request.get("InstanceType") === "t3.small",
  });
  const result = await launch({ capacityFallback: "on-demand-after-120s" });
  expect(
    requests.map(
      (request) =>
        `${request.has("InstanceMarketOptions.MarketType") ? "spot" : "on-demand"}:${request.get("InstanceType")}`,
    ),
  ).toEqual(["spot:t3.small", "spot:t3.medium", "on-demand:t3.small", "on-demand:t3.medium"]);
  expect(result.market).toBe("on-demand");
  expect(result.serverType).toBe("t3.medium");
});

it("keeps the final candidate's capacity retries after an earlier handoff", async () => {
  const { launch, requests } = await capacityTransport({
    status: 500,
    reject: (_request, index) => index < 11,
  });
  const result = await launch();
  expect(requests.map((request) => request.get("InstanceType"))).toEqual([
    "t3.small",
    ...Array(11).fill("t3.medium"),
  ]);
  expect(result.serverType).toBe("t3.medium");
});

it.each([
  {
    name: "larger later type",
    spot: 2,
    onDemand: 999,
    marketFallback: false,
    types: ["t3.small", "c7i.2xlarge"],
  },
  {
    name: "larger type and On-Demand market",
    spot: 2,
    onDemand: 1,
    marketFallback: true,
    types: ["t3.small", "c7i.2xlarge"],
  },
  {
    name: "On-Demand market only",
    spot: 2,
    onDemand: 1,
    marketFallback: true,
    types: ["t3.small"],
  },
  {
    name: "larger later On-Demand type",
    spot: 1,
    onDemand: 2,
    marketFallback: true,
    types: ["t3.small", "c7i.2xlarge"],
  },
])(
  "retains full retries when preflight blocks the $name",
  async ({ spot, onDemand, marketFallback, types }) => {
    const { launch, requests, quotaRequests } = await capacityTransport({
      status: 500,
      reject: (_request, index) => index < 10,
      quotas: { spot, onDemand },
    });
    const result = await launch({
      awsInstanceTypes: types,
      capacityFallback: marketFallback ? "on-demand-after-120s" : "none",
    });
    expect(requests.map((request) => request.get("InstanceType"))).toEqual(
      Array(11).fill("t3.small"),
    );
    expect(result.serverType).toBe("t3.small");
    expect(quotaRequests).toEqual(marketFallback ? ["L-34B43A08", "L-1216C47A"] : ["L-34B43A08"]);
  },
);

it("does not query On-Demand quota when the current Spot candidate succeeds", async () => {
  const { launch, quotaRequests } = await capacityTransport({
    status: 500,
    reject: () => false,
    quotas: { spot: 2, onDemand: 1 },
  });
  const result = await launch({
    awsInstanceTypes: ["t3.small", "c7i.2xlarge"],
    capacityFallback: "on-demand-after-120s",
  });
  expect(result.serverType).toBe("t3.small");
  expect(quotaRequests).toEqual(["L-34B43A08"]);
});

it("hands off past a quota-blocked type without changing attempt history order", async () => {
  const { launch, requests, quotaRequests } = await capacityTransport({
    status: 500,
    reject: (request) => request.get("InstanceType") === "t3.small",
    quotas: { spot: 2, onDemand: 1 },
  });
  const result = await launch({ awsInstanceTypes: ["t3.small", "c7i.2xlarge", "t3.medium"] });
  expect(requests.map((request) => request.get("InstanceType"))).toEqual(["t3.small", "t3.medium"]);
  expect(result.attempts?.map(({ serverType, category }) => ({ serverType, category }))).toEqual([
    { serverType: "t3.small", category: "capacity" },
    { serverType: "c7i.2xlarge", category: "quota" },
  ]);
  expect(quotaRequests).toEqual(["L-34B43A08"]);
});

it("does not use an oversized UTF-8 capacity body to skip retrying, and preserves its response", async () => {
  const body = `<Response><Errors><Error><Code>InsufficientInstanceCapacity</Code><Message>${"é".repeat(40_000)}</Message></Error></Errors></Response>`;
  const { launch, requests, responses } = await capacityTransport({
    status: 500,
    reject: (_request, index) => index === 0,
    writeError: (response) => {
      response.write(body);
      response.end();
    },
  });
  const result = await launch();
  expect(requests.map((request) => request.get("InstanceType"))).toEqual(["t3.small", "t3.small"]);
  expect(result.server.cloudID).toBe("i-fallback");
  expect(responses[0]!.bodyUsed).toBe(false);
  expect(await responses[0]!.text()).toBe(body);
});

it.each([false, true])(
  "bounds the whole response probe without consuming the original (drip=%s)",
  async (drip) => {
    let guard: ReturnType<typeof setTimeout> | undefined;
    let interval: ReturnType<typeof setInterval> | undefined;
    let guardReleasedBody = false;
    let finishBody: (() => void) | undefined;
    const chunks = ["<Response>"];
    const { launch, requests, responses } = await capacityTransport({
      status: 503,
      reject: (_request, index) => index === 0,
      writeError: (response) => {
        response.write(chunks[0]);
        let finished = false;
        finishBody = () => {
          if (finished) return;
          finished = true;
          clearInterval(interval);
          chunks.push("</Response>");
          response.end(chunks.at(-1));
        };
        if (drip)
          interval = setInterval(() => {
            chunks.push(" ");
            response.write(" ");
          }, 100);
        // Release a regressed unbounded reader so the test can still settle its HTTP request.
        guard = setTimeout(() => {
          guardReleasedBody = true;
          finishBody?.();
        }, 2_000);
      },
    });
    try {
      const result = await launch();
      expect(guardReleasedBody).toBe(false);
      expect(requests.map((request) => request.get("InstanceType"))).toEqual([
        "t3.small",
        "t3.small",
      ]);
      expect(result.server.cloudID).toBe("i-fallback");
      expect(responses[0]!.bodyUsed).toBe(false);
      finishBody?.();
      expect(await responses[0]!.text()).toBe(chunks.join(""));
    } finally {
      clearTimeout(guard);
      finishBody?.();
    }
  },
);

it("keeps normal retries when the intermediate response body becomes unreadable", async () => {
  let disconnect: (() => void) | undefined;
  const { launch, requests, firstResponse } = await capacityTransport({
    status: 500,
    reject: (_request, index) => index === 0,
    writeError: (response) => {
      response.write("<Response>");
      disconnect = () => response.destroy();
    },
  });
  const launched = launch();
  const response = await firstResponse;
  expect(disconnect).toBeTypeOf("function");
  disconnect!();
  const result = await launched;
  expect(requests.map((request) => request.get("InstanceType"))).toEqual(["t3.small", "t3.small"]);
  expect(result.server.cloudID).toBe("i-fallback");
  expect(response.bodyUsed).toBe(false);
});

async function capacityTransport(options: {
  status: number;
  reject: (request: URLSearchParams, index: number) => boolean;
  errorBody?: string;
  writeError?: (response: ServerResponse) => void;
  quotas?: { spot: number; onDemand: number };
  fixedOperation?: boolean;
}) {
  const credentialProvider = vi.fn<typeof credentials>(credentials);
  if (options.fixedOperation) {
    credentialProvider.mockImplementationOnce(credentials).mockImplementation(async () => ({
      accessKeyId: "fixture-rotated-access",
      secretAccessKey: "fixture-rotated-secret",
      sessionToken: "fixture-rotated-session",
    }));
  }
  vi.spyOn(Math, "random").mockReturnValue(0);
  vi.spyOn(console, "info").mockImplementation(() => {});
  const requests: URLSearchParams[] = [];
  const quotaRequests: string[] = [];
  const responses: Response[] = [];
  const firstResponse = Promise.withResolvers<Response>();
  const errorBody =
    options.errorBody ??
    "<Response><Errors><Error><Code>InsufficientInstanceCapacity</Code><Message>No capacity</Message></Error></Errors></Response>";
  const url = await localTransport((request, response) => {
    let body = "";
    request.setEncoding("utf8");
    request.on("data", (chunk: string) => {
      body += chunk;
    });
    request.on("end", () => {
      const params = new URLSearchParams(body);
      const action = params.get("Action");
      response.setHeader("content-type", "application/xml");
      if (!action) {
        const { QuotaCode } = JSON.parse(body);
        quotaRequests.push(QuotaCode);
        const quota = options.quotas?.[QuotaCode === "L-34B43A08" ? "spot" : "onDemand"] ?? 999;
        response.setHeader("content-type", "application/json");
        response.end(JSON.stringify({ Quota: { Value: quota } }));
      } else if (action === "GetCallerIdentity") {
        response.end(
          "<GetCallerIdentityResponse><GetCallerIdentityResult><Account>123456789012</Account><Arn>arn:aws:iam::123456789012:user/fixture</Arn></GetCallerIdentityResult></GetCallerIdentityResponse>",
        );
      } else if (action === "DescribeKeyPairs") {
        response.end(
          "<DescribeKeyPairsResponse><keySet><item><keyName>test-key</keyName><publicKey>ssh-ed25519 test</publicKey></item></keySet></DescribeKeyPairsResponse>",
        );
      } else if (action === "DescribeSecurityGroups") {
        response.end(
          "<DescribeSecurityGroupsResponse><securityGroupInfo><item><groupId>sg-123</groupId></item></securityGroupInfo></DescribeSecurityGroupsResponse>",
        );
      } else if (
        action === "RevokeSecurityGroupIngress" ||
        action === "AuthorizeSecurityGroupIngress"
      ) {
        response.end(`<${action}Response />`);
      } else if (action === "DescribeHosts") {
        response.end(
          "<DescribeHostsResponse><hostSet><item><hostId>h-pinned</hostId><hostState>available</hostState><hostProperties><instanceType>mac1.metal</instanceType></hostProperties></item></hostSet></DescribeHostsResponse>",
        );
      } else if (action === "RunInstances") {
        requests.push(params);
        if (options.reject(params, requests.length - 1)) {
          response.statusCode = options.status;
          if (options.writeError) options.writeError(response);
          else response.end(errorBody);
        } else {
          response.end(
            `<RunInstancesResponse><instancesSet><item><instanceId>i-fallback</instanceId><instanceType>${params.get("InstanceType")}</instanceType><instanceState><name>pending</name></instanceState></item></instancesSet></RunInstancesResponse>`,
          );
        }
      } else {
        response.statusCode = 400;
        response.end(
          `<Response><Errors><Error><Code>Unexpected</Code><Message>${action}</Message></Error></Errors></Response>`,
        );
      }
    });
  });
  const actualFetch = globalThis.fetch;
  vi.stubGlobal("fetch", async (input: RequestInfo | URL, init?: RequestInit) => {
    const request = input instanceof Request ? input : new Request(input, init);
    expect(request.headers.get("authorization")).toMatch(
      /^AWS4-HMAC-SHA256 Credential=fixture-access-canary\//,
    );
    expect(new URL(request.url).hostname).toMatch(
      /^(ec2|servicequotas|sts)\.eu-west-1\.amazonaws\.com$/,
    );
    // The real signer, fetch implementation and HTTP response handling still run; only the destination is local.
    const body = await request.text();
    const response = await actualFetch(url, {
      method: request.method,
      headers: request.headers,
      body,
    });
    if (new URLSearchParams(body).get("Action") === "RunInstances") {
      responses.push(response);
      firstResponse.resolve(response);
    }
    return response;
  });
  const client = new EC2SpotClient(
    {
      awsCredentialProvider: credentialProvider,
      CRABBOX_AWS_SECURITY_GROUP_ID: "sg-123",
      CRABBOX_AWS_SSH_CIDRS: "203.0.113.7/32",
      CRABBOX_AWS_AMI: "ami-test",
    } as Env,
    "eu-west-1",
  );
  const config = leaseConfig({
    provider: "aws",
    serverType: "t3.small",
    serverTypeExplicit: false,
    awsInstanceTypes: ["t3.small", "t3.medium"],
    capacity: { market: "spot", fallback: "none" },
    providerKey: "test-key",
    sshPublicKey: "ssh-ed25519 test",
  });
  return {
    requests,
    quotaRequests,
    responses,
    credentialProvider,
    firstResponse: firstResponse.promise,
    launch: (overrides: Partial<LeaseConfig> = {}) => {
      const create = (operationClient: EC2SpotClient) =>
        operationClient.createServerWithFallback(
          { ...config, ...overrides },
          "cbx_000000000001",
          "violet-prawn",
          "alice@example.com",
        );
      return options.fixedOperation
        ? client.withLeaseOperation(async (session) => {
            await session.verifiedIdentity();
            return create(session.client);
          })
        : create(client);
    },
  };
}

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

it.each(
  [
    { name: "normal success", statuses: [200], attempts: 1 },
    { name: "original SDK retries 503 and 429", statuses: [503, 429, 200], attempts: 3 },
    { name: "original SDK returns 400 without retry", statuses: [400], attempts: 1 },
  ].flatMap((scenario) => [false, true].map((fixed) => ({ ...scenario, fixed }))),
)(
  "observes $name without changing request bytes or response (fixed=$fixed)",
  async ({ statuses, attempts, fixed }) => {
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
    const client = fixed
      ? new FixedAWSFetchClient(resolvedAWSCredentials(await credentials()), "ec2", "eu-west-1")
      : new RefreshingAWSFetchClient(credentials, "ec2", "eu-west-1");
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
  },
);

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

it.each([false, true])(
  "records signing failure without calling the transport (response policy=%s)",
  async (responsePolicy) => {
    let requests = 0;
    const url = await localTransport((_request, response) => {
      requests += 1;
      response.end();
    });
    const { diagnostics, finish } = observe();
    const client = new RefreshingAWSFetchClient(credentials, "ec2", "eu-west-1");
    await expect(
      diagnostics.measure("key_pair", () =>
        client.fetch(
          url,
          { headers: { "invalid\nheader": "payload-canary" } },
          responsePolicy ? async () => false : undefined,
        ),
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
  },
);

it.each([false, true])(
  "preserves a network failure without adding retries (response policy=%s)",
  async (responsePolicy) => {
    let requests = 0;
    const url = await localTransport((request) => {
      requests += 1;
      request.socket.destroy();
    });
    const { diagnostics, finish } = observe();
    const client = new RefreshingAWSFetchClient(credentials, "ec2", "eu-west-1");
    await expect(
      diagnostics.measure("key_pair", () =>
        client.fetch(
          url,
          { method: "POST", body: "payload-canary" },
          responsePolicy ? async () => false : undefined,
        ),
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
  },
);

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
