import { AwsClient } from "aws4fetch";
import { afterEach, describe, expect, it, vi } from "vitest";

import { AWSProvider } from "../src/fleet";
import { orgKeyForLabel } from "../src/org-identity";
import type { Env, LeaseRecord } from "../src/types";
import { ProvisioningTestStorage } from "./provisioning-fixtures";

type FixtureContext = {
  closed: boolean;
  fetch: (request: Request) => Promise<Response>;
};

const transport = await vi.hoisted(async () => {
  const { AsyncLocalStorage } = await import("node:async_hooks");
  return { contexts: new AsyncLocalStorage<FixtureContext>() };
});

// This matrix covers reconciliation and real SDK signing, not SDK response retries.
// A file-scoped transport never consults global fetch, including after test timeout.
vi.mock("aws4fetch", async (importOriginal) => {
  const original = await importOriginal<typeof import("aws4fetch")>();
  return {
    ...original,
    AwsClient: class extends original.AwsClient {
      override async fetch(...args: Parameters<InstanceType<typeof original.AwsClient>["fetch"]>) {
        const context = transport.contexts.getStore();
        if (!context || context.closed) throw new Error("AWS test transport is closed");
        const request = await this.sign(...args);
        if (context.closed) throw new Error("AWS test transport is closed");
        return await context.fetch(request);
      }
    },
  };
});

const contexts = new Set<FixtureContext>();
function fixtureContext(fetch: FixtureContext["fetch"]): FixtureContext {
  const context = { closed: false, fetch };
  contexts.add(context);
  return context;
}

afterEach(() => {
  // Synchronous closure remains effective even if later cleanup fails. It does not
  // cancel WebCrypto already in flight; that work can only return to its closed context.
  for (const context of contexts) context.closed = true;
  contexts.clear();
});

function stressLease(overrides: Partial<LeaseRecord>): LeaseRecord {
  return {
    id: "cbx_000000000000",
    slug: "blue-lobster",
    provider: "aws",
    cloudID: "123",
    owner: "alice@example.com",
    org: orgKeyForLabel("example-org"),
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

describe("AWS reconciliation stress transport", () => {
  it("rejects late signing without dispatching into the next fixture", async () => {
    const oldFetch = vi.fn<FixtureContext["fetch"]>(async () => new Response("old"));
    const nextFetch = vi.fn<FixtureContext["fetch"]>(async () => new Response("next"));
    const oldContext = fixtureContext(oldFetch);
    const nextContext = fixtureContext(nextFetch);
    const client = new AwsClient({
      accessKeyId: "fixture",
      secretAccessKey: "fixture",
      service: "ec2",
      region: "eu-west-1",
    });
    let resume!: () => void;
    let entered!: () => void;
    const gate = new Promise<void>((resolve) => {
      resume = resolve;
    });
    const started = new Promise<void>((resolve) => {
      entered = resolve;
    });
    const sign = client.sign.bind(client);
    vi.spyOn(client, "sign").mockImplementation(async (...args) => {
      entered();
      await gate;
      return await sign(...args);
    });
    const pending = transport.contexts.run(oldContext, () =>
      client.fetch("https://ec2.eu-west-1.amazonaws.com/"),
    );
    const rejected = pending.then(
      () => undefined,
      (error: unknown) => error,
    );
    try {
      await started;
      oldContext.closed = true;
      await transport.contexts.run(nextContext, async () => {
        resume();
        expect(await rejected).toEqual(new Error("AWS test transport is closed"));
      });
      expect(oldFetch).not.toHaveBeenCalled();
      expect(nextFetch).not.toHaveBeenCalled();
      await expect(
        transport.contexts.run(oldContext, () =>
          client.fetch("https://ec2.eu-west-1.amazonaws.com/"),
        ),
      ).rejects.toThrow("AWS test transport is closed");
      await expect(client.fetch("https://ec2.eu-west-1.amazonaws.com/")).rejects.toThrow(
        "AWS test transport is closed",
      );
    } finally {
      oldContext.closed = true;
      resume();
    }
  });

  it.each(
    [0, 1, 31, 97].flatMap((rotation) =>
      [false, true].map((unboundMember) => ({ rotation, unboundMember })),
    ),
  )(
    "stress reconciles mixed AWS access without cross-group grants (rotation $rotation, unbound member $unboundMember)",
    async ({ rotation, unboundMember }) => {
      const leases: LeaseRecord[] = [];
      const expected = new Set<string>();
      const groups = new Map<string, { region: string; rules: Map<string, Set<string>> }>();
      for (let group = 0; group < 64; group++) {
        const groupID = `sg-stress-${group}`;
        if (unboundMember) {
          // Unknown account provenance permits additions, never pruning that member's ports.
          expected.add(`${groupID}|22|203.0.113.254/32`);
          expected.add(`${groupID}|2222|203.0.113.254/32`);
        }
        const region = group % 2 ? "us-east-1" : "eu-west-1";
        groups.set(groupID, {
          region,
          rules: new Map(
            ["22", "2222", "443"].map((port) => [port, new Set(["203.0.113.254/32"])]),
          ),
        });
        for (let member = 0; member < 4; member++) {
          const address = group % 2 ? member + 1 : 1;
          const cidrs = [
            `198.51.100.${((group * 4 + address) % 254) + 1}/32`,
            `2001:db8:${group}::${address}/128`,
          ];
          const ports = member === 1 ? ["2222", "22"] : member === 2 ? ["443", "22"] : ["22"];
          leases.push(
            stressLease({
              id: `cbx_${(group * 4 + member).toString(16).padStart(12, "0")}`,
              provider: "aws",
              ...(unboundMember && member === 1
                ? {}
                : { providerScope: "aws:account:123456789012" }),
              region,
              sshPort: ports[0]!,
              sshFallbackPorts: ports.slice(1),
              state: member >= 2 ? "released" : "active",
              releaseDeletesServer: member !== 2,
              network: {
                awsSecurityGroupID: groupID,
                sshSourceCIDRs: cidrs,
                sshSourceCIDRsComplete: true,
              },
            }),
          );
          if (member < 3)
            for (const port of ports)
              for (const cidr of cidrs) expected.add(`${groupID}|${port}|${cidr}`);
        }
      }
      const context = fixtureContext(async (fetchRequest) => {
        const params = new URLSearchParams(await fetchRequest.clone().text());
        const action = params.get("Action") ?? "";
        if (action === "GetCallerIdentity") {
          return new Response(`<GetCallerIdentityResponse><GetCallerIdentityResult>
            <Arn>arn:aws:iam::123456789012:user/crabbox</Arn>
            <UserId>AIDAEXAMPLE</UserId><Account>123456789012</Account>
          </GetCallerIdentityResult></GetCallerIdentityResponse>`);
        }
        const groupID = params.get("GroupId.1") ?? params.get("GroupId")!;
        const group = groups.get(groupID);
        if (!group || new URL(fetchRequest.url).hostname !== `ec2.${group.region}.amazonaws.com`)
          throw new Error("fixture group/region mismatch");
        if (params.get("Action") === "DescribeSecurityGroups") {
          const permissions = [...group.rules]
            .map(([port, cidrs]) => {
              const ranges = (ipv6: boolean) =>
                [...cidrs]
                  .filter((cidr) => cidr.includes(":") === ipv6)
                  .map(
                    (cidr) =>
                      `<item><${ipv6 ? "cidrIpv6" : "cidrIp"}>${cidr}</${ipv6 ? "cidrIpv6" : "cidrIp"}><description>Crabbox SSH</description></item>`,
                  )
                  .join("");
              return `<item><ipProtocol>tcp</ipProtocol><fromPort>${port}</fromPort><toPort>${port}</toPort><ipRanges>${ranges(false)}</ipRanges><ipv6Ranges>${ranges(true)}</ipv6Ranges></item>`;
            })
            .join("");
          return new Response(
            `<DescribeSecurityGroupsResponse><securityGroupInfo><item><groupId>${groupID}</groupId><ipPermissions>${permissions}</ipPermissions></item></securityGroupInfo></DescribeSecurityGroupsResponse>`,
          );
        }
        const port = params.get("IpPermissions.1.FromPort")!;
        const cidr =
          params.get("IpPermissions.1.IpRanges.1.CidrIp") ??
          params.get("IpPermissions.1.Ipv6Ranges.1.CidrIpv6")!;
        const rules = group.rules.get(port);
        if (
          !rules ||
          params.get("IpPermissions.1.ToPort") !== port ||
          params.get("IpPermissions.1.IpProtocol") !== "tcp"
        )
          throw new Error("fixture permission mismatch");
        if (params.get("Action") === "AuthorizeSecurityGroupIngress") rules.add(cidr);
        else if (params.get("Action") === "RevokeSecurityGroupIngress") rules.delete(cidr);
        else throw new Error(`Unexpected fixture action ${params.get("Action")}`);
        return new Response("<Response />");
      });
      const provider = new AWSProvider(
        { AWS_ACCESS_KEY_ID: "fixture", AWS_SECRET_ACCESS_KEY: "fixture" } as Env,
        "eu-west-1",
        new ProvisioningTestStorage(),
      );
      const ordered = [...leases.slice(rotation), ...leases.slice(0, rotation)];
      if (rotation % 2) ordered.reverse();
      await transport.contexts.run(context, () =>
        provider.reconcileLeaseAccess(leases[0]!, {
          requestSourceCIDRs: [],
          activeLeases: ordered,
        }),
      );
      const actual = new Set(
        [...groups].flatMap(([groupID, group]) =>
          [...group.rules].flatMap(([port, cidrs]) =>
            [...cidrs].map((cidr) => `${groupID}|${port}|${cidr}`),
          ),
        ),
      );
      expect(actual).toEqual(expected);
    },
    // Signing requests for 64 groups can exceed the default budget on a cold runtime.
    30_000,
  );
});
