import { afterEach, describe, expect, it, vi } from "vitest";

import { EC2SpotClient } from "../src/aws";

const leaseID = "cbx_abcdef123456";
const region = "eu-west-1";
const inventoryFilters = {
  "Filter.1.Name": "tag:crabbox",
  "Filter.1.Value.1": "true",
  "Filter.2.Name": "instance-state-name",
  "Filter.2.Value.1": "pending",
  "Filter.2.Value.2": "running",
  "Filter.2.Value.3": "stopping",
  "Filter.2.Value.4": "stopped",
};
const workspaceFilters = {
  "Filter.1.Name": "tag:crabbox",
  "Filter.1.Value.1": "true",
  "Filter.2.Name": "tag:created_by",
  "Filter.2.Value.1": "crabbox",
  "Filter.3.Name": "tag:crabbox_workspace",
  "Filter.3.Value.1": "true",
  "Filter.4.Name": "tag:access_mode",
  "Filter.4.Value.1": "ssm",
  "Filter.5.Name": "tag:lease",
  "Filter.5.Value.1": leaseID,
  "Filter.6.Name": "instance-state-name",
  "Filter.6.Value.1": "pending",
  "Filter.6.Value.2": "running",
  "Filter.6.Value.3": "stopping",
  "Filter.6.Value.4": "stopped",
};

afterEach(() => vi.unstubAllGlobals());

describe.each([
  { lookup: "inventory", filters: inventoryFilters },
  { lookup: "workspace", filters: workspaceFilters },
])("AWS $lookup pagination", ({ lookup, filters }) => {
  function read(client: EC2SpotClient) {
    return lookup === "inventory"
      ? client.listCrabboxServers()
      : client.findWorkspaceServerByLease(leaseID);
  }

  function createClient() {
    return new EC2SpotClient(
      { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "secret" } as never,
      region,
    );
  }

  function expected(ids: string[]) {
    const machines = ids.map((cloudID) =>
      expect.objectContaining({ cloudID, region, labels: { lease: leaseID } }),
    );
    return lookup === "inventory" ? machines : machines[0];
  }

  it.each([{ ids: [] }, { ids: ["i-only"] }])(
    "handles a complete one-page response $ids",
    async ({ ids }) => {
      const fetchMock = vi.fn<typeof fetch>(async () => instancePage(ids));
      vi.stubGlobal("fetch", fetchMock);

      await expect(read(createClient())).resolves.toEqual(expected(ids));
      expect(fetchMock).toHaveBeenCalledTimes(1);
    },
  );

  it("includes a later-page resource and preserves all filters, region and NextToken", async () => {
    const calls: Record<string, string>[] = [];
    const token = "opaque+/=next-page";
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const request = input instanceof Request ? input : new Request(input, init);
        expect(new URL(request.url).hostname).toBe(`ec2.${region}.amazonaws.com`);
        expect(request.method).toBe("POST");
        calls.push(Object.fromEntries(new URLSearchParams(await request.text())));
        return calls.length === 1 ? instancePage([], token) : instancePage(["i-later"]);
      }),
    );

    await expect(read(createClient())).resolves.toEqual(expected(["i-later"]));
    const params = { Action: "DescribeInstances", Version: "2016-11-15", ...filters };
    expect(calls).toEqual([params, { ...params, NextToken: token }]);
  });

  it.each(["i-second", "i-first"])(
    "retains later-page match %s for ambiguity checks",
    async (secondID) => {
      const fetchMock = vi
        .fn<typeof fetch>()
        .mockResolvedValueOnce(instancePage(["i-first"], "next-page"))
        .mockResolvedValueOnce(instancePage([secondID]));
      vi.stubGlobal("fetch", fetchMock);

      const result = await read(createClient()).catch((error: unknown) => error);
      expect(result).toEqual(
        lookup === "workspace"
          ? new Error(`AWS private workspace recovery is ambiguous for lease ${leaseID}`)
          : expected(["i-first", secondID]),
      );
      expect(fetchMock).toHaveBeenCalledTimes(2);
    },
  );

  it.each([
    { name: "repeated token", tokens: ["opaque-a", "opaque-a"] },
    { name: "token cycle", tokens: ["opaque-a", "opaque-b", "opaque-a"] },
  ])("rejects the entire inventory on a $name", async ({ tokens }) => {
    let page = 0;
    const fetchMock = vi.fn<typeof fetch>(async () => instancePage(["i-first"], tokens[page++]));
    vi.stubGlobal("fetch", fetchMock);

    await expect(read(createClient())).rejects.toThrow(
      `aws DescribeInstances inventory incomplete in ${region}: repeated pagination token`,
    );
    expect(fetchMock).toHaveBeenCalledTimes(tokens.length);
  });

  it.each([false, true])("bounds inventory at 100 pages (more pages: %s)", async (morePages) => {
    let page = 0;
    const fetchMock = vi.fn<typeof fetch>(async () => {
      page++;
      return instancePage(
        page === 100 ? ["i-last"] : [],
        page < 100 || morePages ? `opaque-page-${page}` : undefined,
      );
    });
    vi.stubGlobal("fetch", fetchMock);

    const result = await read(createClient()).catch((error: unknown) => error);
    expect(result).toEqual(
      morePages
        ? new Error(
            `aws DescribeInstances inventory incomplete in ${region}: pagination exceeded 100 pages`,
          )
        : expected(["i-last"]),
    );
    expect(fetchMock).toHaveBeenCalledTimes(100);
  });

  it.each(["http", "transport"])(
    "rejects the whole result on a later-page %s failure without leaking upstream details",
    async (failure) => {
      const fetchMock = vi
        .fn<typeof fetch>()
        .mockResolvedValueOnce(instancePage(["i-first"], "opaque-token"));
      const detail = "opaque-token synthetic-credential";
      if (failure === "http") {
        fetchMock.mockResolvedValueOnce(
          new Response(
            `<Response><Errors><Error><Code>InvalidNextToken</Code><Message>${detail}</Message></Error></Errors></Response>`,
            { status: 400 },
          ),
        );
      } else {
        fetchMock.mockRejectedValueOnce(new Error(detail));
      }
      vi.stubGlobal("fetch", fetchMock);

      await expect(read(createClient())).rejects.toEqual(
        new Error(`aws DescribeInstances inventory incomplete in ${region}: page 2 request failed`),
      );
      expect(fetchMock).toHaveBeenCalledTimes(2);
    },
  );
});

function instancePage(ids: string[], nextToken?: string): Response {
  const reservations = ids.map(
    (id) => `<item><instancesSet><item>
      <instanceId>${id}</instanceId>
      <instanceState><name>running</name></instanceState>
      <tagSet><item><key>lease</key><value>${leaseID}</value></item></tagSet>
    </item></instancesSet></item>`,
  );
  return new Response(
    `<DescribeInstancesResponse><reservationSet>${reservations.join("")}</reservationSet>
      ${nextToken ? `<nextToken>${nextToken}</nextToken>` : ""}
    </DescribeInstancesResponse>`,
    { headers: { "content-type": "text/xml" } },
  );
}
