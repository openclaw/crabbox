import { afterEach, expect, it, vi } from "vitest";

import { EC2SpotClient } from "../src/aws";

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

it.each([
  {
    name: "Spot reclaimed mid-wait",
    state: "shutting-down",
    reason: "Server.SpotInstanceTermination",
    want: "Server.SpotInstanceTermination",
  },
  {
    name: "terminated mid-wait",
    state: "terminated",
    reason: "Client.UserInitiatedShutdown",
    want: "Client.UserInitiatedShutdown",
  },
  {
    name: "closed Spot request before instance state converges",
    state: "pending",
    spot: true,
    want: "instance-terminated-no-capacity",
  },
  {
    name: "terminal with stale address",
    state: "terminated",
    ip: "203.0.113.44",
    want: "terminated",
  },
  { name: "healthy slow boot", state: "running", ip: "203.0.113.44", want: "" },
])("acquire detects $name", async (tc) => {
  vi.useFakeTimers();
  let describes = 0;
  const client = new EC2SpotClient(
    { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "test" } as never,
    "us-east-1",
  );
  vi.spyOn(
    client as unknown as { ec2(action: string, params: Record<string, string>): Promise<unknown> },
    "ec2",
  ).mockImplementation(async (action, params) => {
    if (action === "DescribeSpotInstanceRequests") {
      if (params["SpotInstanceRequestId.1"] !== "sir-test") throw new Error("wrong Spot request");
      return {
        spotInstanceRequestSet: {
          item: {
            spotInstanceRequestId: "sir-test",
            instanceId: "i-test",
            state: describes > 1 ? "closed" : "active",
            status: { code: describes > 1 ? "instance-terminated-no-capacity" : "fulfilled" },
          },
        },
      };
    }
    expect(action).toBe("DescribeInstances");
    expect(params["InstanceId.1"]).toBe("i-test");
    describes++;
    return {
      requestId: "req-test",
      reservationSet: {
        item: {
          instancesSet: {
            item: {
              instanceId: "i-test",
              instanceType: "t3.small",
              instanceState: { name: describes > 1 ? tc.state : "pending" },
              stateReason: { code: describes > 1 ? (tc.reason ?? "") : "" },
              spotInstanceRequestId: tc.spot ? "sir-test" : "",
              ipAddress: describes > 1 ? (tc.ip ?? "") : "",
            },
          },
        },
      },
    };
  });
  const result = client.waitForServerIP("i-test").then(
    (value) => ({ value, error: "" }),
    (error) => ({ value: undefined, error: String(error) }),
  );
  await vi.advanceTimersByTimeAsync(600_000);
  const outcome = await result;
  expect(outcome.error).toContain(tc.want);
  expect(Boolean(outcome.error)).toBe(Boolean(tc.want));
  expect(outcome.value?.host).toBe(tc.want ? undefined : tc.ip);
  expect(outcome.error ? outcome.error.includes("i-test") : true).toBe(true);
  expect(describes).toBe(2);
});

it.each(["registration", "bootstrap"])(
  "stops SSM %s when EC2 terminates mid-wait",
  async (phase) => {
    vi.useFakeTimers();
    const client = new EC2SpotClient(
      { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "test" } as never,
      "us-east-1",
    );
    const find = vi.spyOn(client, "findServer").mockResolvedValue({
      provider: "aws",
      id: 0,
      cloudID: "i-test",
      name: "test",
      status: "terminated",
      host: "",
      serverType: "t3.small",
      labels: {},
      awsStateReasonCode: "Server.SpotInstanceTermination",
    });
    vi.spyOn(
      client as unknown as { ssm(action: string): Promise<unknown> },
      "ssm",
    ).mockImplementation(async (action) => {
      if (action === "SendCommand") return { Command: { CommandId: "cmd-test" } };
      if (action === "GetCommandInvocation") return { Status: "InProgress" };
      return { InstanceInformationList: [] };
    });
    const result = (
      phase === "registration"
        ? client.waitForSSMOnline("i-test")
        : client.runSSMBootstrap("i-test", "cbx_test", "true", "test-log")
    ).then(
      () => "unexpected success",
      (error) => String(error),
    );
    await vi.advanceTimersByTimeAsync(20 * 60_000);
    expect(await result).toContain("Server.SpotInstanceTermination");
    expect(find).toHaveBeenCalledTimes(1);
  },
);
