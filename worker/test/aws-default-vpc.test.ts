import { afterEach, expect, it, vi } from "vitest";

import { EC2SpotClient } from "../src/aws";
import { leaseConfig } from "../src/config";
import type { Env } from "../src/types";

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const xml = (body: string, status = 200) =>
  new Response(body, { status, headers: { "content-type": "text/xml" } });

function response(params: URLSearchParams): Response {
  switch (params.get("Action")) {
    case "DescribeVpcs":
      return xml(
        "<DescribeVpcsResponse><vpcSet><item><vpcId>vpc-default</vpcId></item></vpcSet></DescribeVpcsResponse>",
      );
    case "DescribeSubnets":
      return xml(
        "<DescribeSubnetsResponse><subnetSet><item><vpcId>vpc-custom</vpcId></item></subnetSet></DescribeSubnetsResponse>",
      );
    case "DescribeSecurityGroups":
      return xml(
        `<DescribeSecurityGroupsResponse><securityGroupInfo><item><groupId>sg-ready</groupId><groupName>crabbox-runners</groupName><vpcId>${params.get("Filter.2.Value.1") || "vpc-default"}</vpcId><ipPermissions /></item></securityGroupInfo></DescribeSecurityGroupsResponse>`,
      );
    case "CreateSecurityGroup":
      return xml(
        "<CreateSecurityGroupResponse><groupId>sg-created</groupId></CreateSecurityGroupResponse>",
      );
    case "AuthorizeSecurityGroupIngress":
    case "RevokeSecurityGroupIngress":
      return xml("<Response />");
    default:
      throw new Error(`Unexpected fixture action: ${params.get("Action")}`);
  }
}

function harness(
  read: (params: URLSearchParams) => Promise<Response> = async (p) => response(p),
  env: Partial<Env> = {},
) {
  const calls: URLSearchParams[] = [];
  vi.stubGlobal("fetch", async (input: RequestInfo | URL, init?: RequestInit) => {
    const request = input instanceof Request ? input : new Request(input, init);
    const params = new URLSearchParams(await request.clone().text());
    calls.push(params);
    return read(params);
  });
  const client = new EC2SpotClient(
    { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "secret", ...env } as Env,
    "eu-west-1",
  );
  const config = leaseConfig({
    provider: "aws",
    awsSSHCIDRs: ["198.51.100.42/32"],
    sshPublicKey: "ssh-ed25519 fixture",
  });
  const mutations = () => calls.filter((p) => !p.get("Action")?.startsWith("Describe"));
  return { client, config, calls, mutations };
}

it.each([
  { first: "DescribeVpcs", failure: "" },
  { first: "DescribeSecurityGroups", failure: "" },
  { first: "DescribeVpcs", failure: "DescribeVpcs" },
  { first: "DescribeSecurityGroups", failure: "DescribeVpcs" },
  { first: "DescribeVpcs", failure: "DescribeSecurityGroups" },
  { first: "DescribeSecurityGroups", failure: "DescribeSecurityGroups" },
  { first: "DescribeVpcs", failure: "both" },
  { first: "DescribeSecurityGroups", failure: "both" },
])(
  "overlaps default VPC reads and joins them before mutation or failure ($first/$failure)",
  async ({ first, failure }) => {
    const gates = new Map(
      ["DescribeVpcs", "DescribeSecurityGroups"].map((action) => [
        action,
        Promise.withResolvers<void>(),
      ]),
    );
    const entered = new Set<string>();
    const completed = new Set<string>();
    const fault = new Error("fixture read failure");
    const groupFault = new Error("fixture group failure");
    const { client, config, calls, mutations } = harness(async (params) => {
      const action = params.get("Action")!;
      const gate = gates.get(action);
      if (gate) {
        entered.add(action);
        await gate.promise;
        completed.add(action);
        if (action === failure) throw fault;
        if (failure === "both") throw action === "DescribeVpcs" ? fault : groupFault;
      }
      return response(params);
    });
    let settled = false;
    const refreshing = client.refreshSSHIngress(config).then(
      () => {
        settled = true;
        return undefined;
      },
      (error: unknown) => {
        settled = true;
        return error;
      },
    );
    try {
      await vi.waitFor(() => expect(entered.size).toBe(2));
      expect(mutations()).toEqual([]);
      gates.get(first)!.resolve();
      await vi.waitFor(() => expect(completed.has(first)).toBe(true));
      await new Promise<void>((resolve) => setImmediate(resolve));
      expect(settled).toBe(false);
      expect(mutations()).toEqual([]);
      for (const gate of gates.values()) gate.resolve();
      expect(await refreshing).toBe(failure ? fault : undefined);
      expect(mutations().length > 0).toBe(!failure);
      const groupRead = calls.find((p) => p.get("Action") === "DescribeSecurityGroups")!;
      expect(groupRead.get("GroupName.1")).toBe("crabbox-runners");
      expect(groupRead.has("Filter.2.Value.1")).toBe(false);
    } finally {
      for (const gate of gates.values()) gate.resolve();
      await refreshing;
    }
  },
);

it.each(["vpc", "name", "id", "multiple"])(
  "rejects a default group with an unexpected %s before changing ingress",
  async (field) => {
    const group = `<item><groupId>${field === "id" ? "" : "sg-other"}</groupId><groupName>${field === "name" ? "other" : "crabbox-runners"}</groupName><vpcId>${field === "vpc" ? "vpc-other" : "vpc-default"}</vpcId></item>`;
    const { client, config, mutations } = harness(async (p) =>
      p.get("Action") === "DescribeSecurityGroups"
        ? xml(
            `<DescribeSecurityGroupsResponse><securityGroupInfo>${group}${field === "multiple" ? group : ""}</securityGroupInfo></DescribeSecurityGroupsResponse>`,
          )
        : response(p),
    );
    await expect(client.refreshSSHIngress(config)).rejects.toThrow("unexpected group");
    expect(mutations()).toEqual([]);
  },
);

it("requires an actual default VPC even when the name lookup succeeds", async () => {
  const { client, config, mutations } = harness(async (p) =>
    p.get("Action") === "DescribeVpcs"
      ? xml("<DescribeVpcsResponse><vpcSet /></DescribeVpcsResponse>")
      : response(p),
  );
  await expect(client.refreshSSHIngress(config)).rejects.toThrow("no default VPC found");
  expect(mutations()).toEqual([]);
});

it("creates a missing named group in the discovered default VPC", async () => {
  const { client, config, calls, mutations } = harness(async (p) =>
    p.get("Action") === "DescribeSecurityGroups"
      ? xml(
          "<Response><Errors><Error><Code>InvalidGroup.NotFound</Code><Message>fixture missing group</Message></Error></Errors></Response>",
          400,
        )
      : response(p),
  );
  await client.refreshSSHIngress(config);
  const create = calls.find((p) => p.get("Action") === "CreateSecurityGroup")!;
  expect(create.get("VpcId")).toBe("vpc-default");
  expect(create.get("GroupName")).toBe("crabbox-runners");
  expect(
    mutations()
      .filter((p) => p.get("GroupId"))
      .every((p) => p.get("GroupId") === "sg-created"),
  ).toBe(true);
});

it.each(["UnauthorizedOperation", "InvalidGroupId.Malformed"])(
  "preserves a named-group %s even when its message mentions NotFound",
  async (code) => {
    const { client, config, mutations } = harness(async (p) =>
      p.get("Action") === "DescribeSecurityGroups"
        ? xml(
            `<Response><Errors><Error><Code>${code}</Code><Message>fixture mentions InvalidGroup.NotFound</Message></Error></Errors></Response>`,
            400,
          )
        : response(p),
    );
    await expect(client.refreshSSHIngress(config)).rejects.toThrow(code);
    expect(mutations()).toEqual([]);
  },
);

it("does not treat an unstructured error body as a missing-group code", async () => {
  const { client, config, mutations } = harness(async (p) =>
    p.get("Action") === "DescribeSecurityGroups" ? xml("InvalidGroup.NotFound", 400) : response(p),
  );
  await expect(client.refreshSSHIngress(config)).rejects.toThrow("InvalidGroup.NotFound");
  expect(mutations()).toEqual([]);
});

it.each(["config", "environment"])(
  "keeps a %s subnet's group lookup dependent on its VPC",
  async (source) => {
    const gate = Promise.withResolvers<void>();
    let entered = false;
    const { client, config, calls, mutations } = harness(
      async (p) => {
        if (p.get("Action") === "DescribeSubnets") {
          entered = true;
          await gate.promise;
        }
        return response(p);
      },
      source === "environment" ? { CRABBOX_AWS_SUBNET_ID: "subnet-custom" } : {},
    );
    const refresh = client.refreshSSHIngress(
      source === "config" ? { ...config, awsSubnetID: "subnet-custom" } : config,
    );
    try {
      await vi.waitFor(() => expect(entered).toBe(true));
      expect(calls.map((p) => p.get("Action"))).toEqual(["DescribeSubnets"]);
      expect(mutations()).toEqual([]);
      gate.resolve();
      await refresh;
      const groupRead = calls.find((p) => p.get("Action") === "DescribeSecurityGroups")!;
      expect(groupRead.get("Filter.2.Value.1")).toBe("vpc-custom");
      expect(groupRead.has("GroupName.1")).toBe(false);
    } finally {
      gate.resolve();
      await refresh;
    }
  },
);
