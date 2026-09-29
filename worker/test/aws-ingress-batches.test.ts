import { afterEach, expect, it, vi } from "vitest";

import { EC2SpotClient } from "../src/aws";
import { leaseConfig } from "../src/config";
import type { Env } from "../src/types";

afterEach(() => vi.unstubAllGlobals());

const xml = (body = "<Response />", status = 200) =>
  new Response(body, { status, headers: { "content-type": "text/xml" } });
const awsError = (code: string) =>
  xml(`<Response><Errors><Error><Code>${code}</Code></Error></Errors></Response>`, 400);
const cidrs = [
  "198.51.100.1/32",
  "198.51.100.2/32",
  "2001:db8::1/128",
  "198.51.100.3/32",
  "198.51.100.4/32",
];

function harness(
  authorize: (params: URLSearchParams, index: number) => Promise<Response>,
  permissions = ["2222", "22"].map((port) => permission(port, "0.0.0.0/0", "")).join(""),
) {
  const calls: URLSearchParams[] = [];
  let index = 0;
  vi.stubGlobal("fetch", async (input: RequestInfo | URL, init?: RequestInit) => {
    const request = input instanceof Request ? input : new Request(input, init);
    const params = new URLSearchParams(await request.clone().text());
    calls.push(params);
    switch (params.get("Action")) {
      case "DescribeSecurityGroups":
        return xml(
          `<DescribeSecurityGroupsResponse><securityGroupInfo><item><groupId>sg-fixture</groupId><groupName>crabbox-runners</groupName><ipPermissions>${permissions}</ipPermissions></item></securityGroupInfo></DescribeSecurityGroupsResponse>`,
        );
      case "RevokeSecurityGroupIngress":
        return xml();
      case "AuthorizeSecurityGroupIngress":
        return authorize(params, index++);
      default:
        throw new Error(`Unexpected fixture action ${params.get("Action")}`);
    }
  });
  const client = new EC2SpotClient(
    { AWS_ACCESS_KEY_ID: "fixture", AWS_SECRET_ACCESS_KEY: "fixture" } as Env,
    "eu-west-1",
  );
  const config = leaseConfig({
    provider: "aws",
    awsSGID: "sg-fixture",
    awsSSHCIDRs: cidrs,
    sshPort: "2222",
    sshFallbackPorts: ["22"],
    sshPublicKey: "ssh-ed25519 fixture",
  });
  return { client, config, calls };
}

it.each(["additive", "authoritative"] as const)(
  "bounds concurrent authorizations and preserves port revocation order in %s mode",
  async (reconcile) => {
    const gates = Array.from({ length: 10 }, () => Promise.withResolvers<void>());
    const entered: number[] = [];
    const completed: number[] = [];
    const { client, config, calls } = harness(async (_, index) => {
      entered.push(index);
      await gates[index]!.promise;
      completed.push(index);
      return index % 2 ? awsError("InvalidPermission.Duplicate") : xml();
    });
    const refresh = client.refreshSSHIngress(config, { reconcile });
    try {
      await vi.waitFor(() => expect(entered).toHaveLength(4));
      expect(calls.slice(0, 2).map((p) => p.get("Action"))).toEqual([
        "DescribeSecurityGroups",
        "RevokeSecurityGroupIngress",
      ]);
      for (const index of [3, 2, 1]) gates[index]!.resolve();
      await vi.waitFor(() => expect(completed).toHaveLength(3));
      expect(entered).toHaveLength(4);
      gates[0]!.resolve();
      await vi.waitFor(() => expect(entered).toHaveLength(5));
      expect(calls.filter((p) => p.get("Action") === "RevokeSecurityGroupIngress")).toHaveLength(1);
      gates[4]!.resolve();
      await vi.waitFor(() => expect(entered).toHaveLength(9));
      expect(calls.filter((p) => p.get("Action") === "RevokeSecurityGroupIngress")).toHaveLength(2);
      for (const gate of gates) gate.resolve();
      await refresh;
      const rules = calls.filter((p) => p.get("Action") === "AuthorizeSecurityGroupIngress");
      expect(
        rules
          .map((p) => [
            p.get("IpPermissions.1.FromPort"),
            p.get("IpPermissions.1.IpRanges.1.CidrIp") ??
              p.get("IpPermissions.1.Ipv6Ranges.1.CidrIpv6"),
          ])
          .toSorted(),
      ).toEqual(["2222", "22"].flatMap((port) => cidrs.map((cidr) => [port, cidr])).toSorted());
      expect(
        rules.every(
          (p) =>
            p.get("GroupId") === "sg-fixture" &&
            p.get("IpPermissions.1.IpProtocol") === "tcp" &&
            p.get("IpPermissions.1.FromPort") === p.get("IpPermissions.1.ToPort"),
        ),
      ).toBe(true);
    } finally {
      for (const gate of gates) gate.resolve();
      await refresh;
    }
  },
);

it("joins a full batch before one compaction and retries every rule rejected before it", async () => {
  const gate = Promise.withResolvers<void>();
  let entered = 0;
  const { client, config, calls } = harness(
    async (_, index) => {
      entered++;
      if (index === 3) await gate.promise;
      return index < 2 ? awsError("RulesPerSecurityGroupLimitExceeded") : xml();
    },
    "<item><ipProtocol>tcp</ipProtocol><fromPort>2222</fromPort><toPort>2222</toPort><ipRanges><item><cidrIp>203.0.113.1/32</cidrIp></item></ipRanges></item>",
  );
  const refresh = client.refreshSSHIngress({ ...config, sshFallbackPorts: [] });
  try {
    await vi.waitFor(() => expect(entered).toBe(4));
    expect(calls.filter((p) => p.get("Action") === "DescribeSecurityGroups")).toHaveLength(1);
    expect(calls.filter((p) => p.get("Action") === "RevokeSecurityGroupIngress")).toHaveLength(0);
    gate.resolve();
    await refresh;
    expect(entered).toBe(7);
    expect(calls.filter((p) => p.get("Action") === "DescribeSecurityGroups")).toHaveLength(2);
    const revoked = calls.filter((p) => p.get("Action") === "RevokeSecurityGroupIngress");
    expect(revoked.map((p) => p.get("IpPermissions.1.IpRanges.1.CidrIp"))).toEqual([
      "203.0.113.1/32",
    ]);
    const authorized = calls.filter((p) => p.get("Action") === "AuthorizeSecurityGroupIngress");
    expect(
      authorized
        .slice(4, 6)
        .map(
          (p) =>
            p.get("IpPermissions.1.IpRanges.1.CidrIp") ??
            p.get("IpPermissions.1.Ipv6Ranges.1.CidrIpv6"),
        )
        .toSorted(),
    ).toEqual(
      authorized
        .slice(0, 2)
        .map(
          (p) =>
            p.get("IpPermissions.1.IpRanges.1.CidrIp") ??
            p.get("IpPermissions.1.Ipv6Ranges.1.CidrIpv6"),
        )
        .toSorted(),
    );
  } finally {
    gate.resolve();
    await refresh;
  }
});

it("reports the first desired rule's failure after every in-flight request settles", async () => {
  const gate = Promise.withResolvers<void>();
  let entered = 0;
  const { client, config, calls } = harness(async (params) => {
    entered++;
    if (params.get("IpPermissions.1.IpRanges.1.CidrIp") === cidrs[0]) {
      await gate.promise;
      return awsError("UnauthorizedOperation");
    }
    return awsError("RulesPerSecurityGroupLimitExceeded");
  });
  let settled = false;
  const refresh = client.refreshSSHIngress(config).then(
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
    await vi.waitFor(() => expect(entered).toBe(4));
    expect(settled).toBe(false);
    expect(calls.filter((p) => p.get("Action") === "DescribeSecurityGroups")).toHaveLength(1);
    gate.resolve();
    const error = await refresh;
    expect(error).toBeInstanceOf(Error);
    expect((error as Error).message).toContain("UnauthorizedOperation");
    expect(entered).toBe(4);
    expect(calls.filter((p) => p.get("Action") === "DescribeSecurityGroups")).toHaveLength(1);
  } finally {
    gate.resolve();
    await refresh;
  }
});

it.each([
  {
    code: "UnauthorizedOperation",
    expected: expect.stringContaining("UnauthorizedOperation"),
    count: 4,
  },
  { code: "InvalidGroup.NotFound", expected: undefined, count: 14 },
])(
  "joins pending authorization before reporting or retrying $code",
  async ({ code, expected, count }) => {
    const gate = Promise.withResolvers<void>();
    let entered = 0;
    const { client, config, calls } = harness(async (_, index) => {
      entered++;
      if (index === 1) await gate.promise;
      return index === 0 ? awsError(code) : xml();
    });
    let settled = false;
    const refresh = client.refreshSSHIngress(config, { reconcile: "additive" }).then(
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
      await vi.waitFor(() => expect(entered).toBe(4));
      await new Promise<void>((resolve) => setImmediate(resolve));
      expect(settled).toBe(false);
      expect(calls.filter((p) => p.get("Action") === "RevokeSecurityGroupIngress")).toHaveLength(1);
      gate.resolve();
      const result = await refresh;
      expect(result instanceof Error ? result.message : result).toEqual(expected);
      expect(entered).toBe(count);
    } finally {
      gate.resolve();
      await refresh;
    }
  },
);

function permission(port: string, cidr: string, description = "Crabbox SSH"): string {
  const ipv6 = cidr.includes(":");
  const ranges = ipv6 ? "ipv6Ranges" : "ipRanges";
  const field = ipv6 ? "cidrIpv6" : "cidrIp";
  return `<item><ipProtocol>tcp</ipProtocol><fromPort>${port}</fromPort><toPort>${port}</toPort><${ranges}><item><${field}>${cidr}</${field}><description>${description}</description></item></${ranges}></item>`;
}

it.each(["additive", "authoritative"] as const)(
  "skips all mutations for an already matching IPv4 and IPv6 policy in %s mode",
  async (reconcile) => {
    const { client, config, calls } = harness(
      async () => xml(),
      ["2222", "22"].flatMap((port) => cidrs.map((cidr) => permission(port, cidr))).join(""),
    );
    await client.refreshSSHIngress(config, { reconcile });
    expect(calls.map((p) => p.get("Action"))).toEqual(["DescribeSecurityGroups"]);
  },
);

it.each(["additive", "authoritative"] as const)(
  "revokes observed legacy world rules in both families before adding missing sources in %s mode",
  async (reconcile) => {
    const permissions = ["0.0.0.0/0", "::/0"].map((cidr) => permission("2222", cidr, "")).join("");
    const { client, config, calls } = harness(async () => xml(), permissions);
    await client.refreshSSHIngress({ ...config, sshFallbackPorts: [] }, { reconcile });
    const revoked = calls.filter((p) => p.get("Action") === "RevokeSecurityGroupIngress");
    expect(
      revoked.map(
        (p) =>
          p.get("IpPermissions.1.IpRanges.1.CidrIp") ??
          p.get("IpPermissions.1.Ipv6Ranges.1.CidrIpv6"),
      ),
    ).toEqual(["0.0.0.0/0", "::/0"]);
    expect(calls.slice(1, 3).every((p) => p.get("Action") === "RevokeSecurityGroupIngress")).toBe(
      true,
    );
  },
);

it("adds only missing exact port/source pairs and retains the duplicate race fallback", async () => {
  const { client, config, calls } = harness(
    async () => awsError("InvalidPermission.Duplicate"),
    cidrs.map((cidr) => permission("2222", cidr)).join(""),
  );
  await client.refreshSSHIngress(config, { reconcile: "additive" });
  const writes = calls.filter((p) => p.get("Action") === "AuthorizeSecurityGroupIngress");
  expect(writes).toHaveLength(cidrs.length);
  expect(writes.every((p) => p.get("IpPermissions.1.FromPort") === "22")).toBe(true);
  expect(calls.filter((p) => p.get("Action") === "RevokeSecurityGroupIngress")).toEqual([]);
});
