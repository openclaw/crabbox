import { afterEach, describe, expect, it, vi } from "vitest";

import { EC2SpotClient } from "../src/aws";
import { lookupAWSOriginalRootDisk } from "../src/aws-cleanup-recovery";
import { leaseConfig } from "../src/config";
import { providerKeyForLease } from "../src/provider-key";
import { leaseProviderLabels } from "../src/provider-labels";
import type { LeaseRecord } from "../src/types";

const account = "123456789012";
const now = Date.parse("2026-10-20T12:00:00Z");
const lease = {
  id: "cbx_abcdef123456",
  cloudID: "i-0123456789abcdef0",
  provider: "aws",
  region: "eu-west-1",
  providerScope: "aws:account:" + account,
  owner: "alice",
  slug: "test",
  state: "expired",
  createdAt: new Date(now - 60000).toISOString(),
  endedAt: new Date(now).toISOString(),
  providerKey: providerKeyForLease("cbx_abcdef123456"),
  providerKeyCleanupOwned: true,
} as LeaseRecord;
const disk = {
  leaseID: lease.id,
  instanceID: lease.cloudID,
  volumeID: "vol-0123456789abcdef0",
  deviceName: "/dev/sda1",
  region: lease.region!,
  providerScope: lease.providerScope!,
};
function allocation() {
  const tags = leaseProviderLabels(
    {
      ...leaseConfig({ provider: "aws", sshPublicKey: "ssh-ed25519 test" }),
      providerKey: lease.providerKey,
    },
    lease.id,
    lease.slug!,
    lease.owner,
    "aws",
    new Date(now),
  );
  return {
    eventName: "RunInstances",
    eventSource: "ec2.amazonaws.com",
    eventType: "AwsApiCall",
    recipientAccountId: account,
    awsRegion: lease.region,
    eventID: "00000000-0000-4000-8000-000000000001",
    eventTime: new Date(now - 30000).toISOString(),
    requestParameters: {
      keyName: lease.providerKey,
      tagSpecificationSet: {
        items: [
          {
            resourceType: "instance",
            tags: Object.entries(tags).map(([key, value]) => ({ key, value })),
          },
        ],
      },
    },
    responseElements: {
      instancesSet: {
        items: [
          {
            instanceId: lease.cloudID,
            rootDeviceName: disk.deviceName,
            blockDeviceMapping: {
              items: [{ deviceName: disk.deviceName, ebs: { volumeId: disk.volumeID } }],
            },
          },
        ],
      },
    },
  };
}
function summary(event = allocation()) {
  return {
    EventName: "RunInstances",
    EventId: event.eventID,
    EventSource: event.eventSource,
    CloudTrailEvent: JSON.stringify(event),
  };
}
function lookup(events = [summary()]) {
  return vi.fn<() => Promise<Response>>(async () => Response.json({ Events: events }));
}
afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe("authenticated original root evidence", () => {
  it("recovers only the exact allocation response and returns no raw provider payload", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(now);
    const fetch = lookup();
    const before = structuredClone(lease);
    const result = await lookupAWSOriginalRootDisk(lease, account, fetch);
    expect(result).toEqual({
      disk,
      region: lease.region,
      providerScope: lease.providerScope,
      eventID: allocation().eventID,
      eventTime: allocation().eventTime,
    });
    expect(fetch).toHaveBeenCalledWith(
      "https://cloudtrail.eu-west-1.amazonaws.com/",
      expect.objectContaining({
        body: JSON.stringify({
          LookupAttributes: [{ AttributeKey: "ResourceName", AttributeValue: lease.cloudID }],
          StartTime: (now - 60000) / 1000,
          EndTime: now / 1000,
          MaxResults: 50,
        }),
      }),
    );
    expect(lease).toEqual(before);
  });
  it.each([
    "account",
    "region",
    "instance",
    "tags",
    "key",
    "missing disk",
    "duplicate root",
    "ambiguous allocation",
    "old history",
  ])("refuses %s rather than substituting a live disk", async (kind) => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(now);
    const event = allocation();
    if (kind === "account") event.recipientAccountId = "999999999999";
    if (kind === "region") event.awsRegion = "us-east-1";
    if (kind === "instance")
      event.responseElements.instancesSet.items[0]!.instanceId = "i-fffffffffffffffff";
    if (kind === "tags") event.requestParameters.tagSpecificationSet.items[0]!.tags = [];
    if (kind === "key") event.requestParameters.keyName = "foreign";
    if (kind === "missing disk")
      event.responseElements.instancesSet.items[0]!.blockDeviceMapping.items = [];
    if (kind === "duplicate root")
      event.responseElements.instancesSet.items[0]!.blockDeviceMapping.items.push({
        deviceName: disk.deviceName,
        ebs: { volumeId: "vol-fffffffffffffffff" },
      });
    const events = [summary(event)];
    if (kind === "ambiguous allocation")
      events.push(summary({ ...event, eventID: "00000000-0000-4000-8000-000000000002" }));
    const historical =
      kind === "old history"
        ? { ...lease, createdAt: new Date(now - 91 * 86400000).toISOString() }
        : lease;
    await expect(lookupAWSOriginalRootDisk(historical, account, lookup(events))).rejects.toThrow(
      /original root disk|AWS CloudTrail|AWS scope recovery/,
    );
  });
  it.each([
    { providerScope: "aws:account:999999999999" },
    { providerKeyCleanupOwned: false },
    { lifecycle: "registered" as const },
    { region: "eu-west-1/other" },
  ])("rejects unbound or caller-owned lease before lookup: %j", async (change) => {
    const fetch = lookup();
    await expect(
      lookupAWSOriginalRootDisk({ ...lease, ...change }, account, fetch),
    ).rejects.toThrow("requires an exact owned lease");
    expect(fetch).not.toHaveBeenCalled();
  });
  it("never treats denied or incomplete provider history as absence", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(now);
    await expect(
      lookupAWSOriginalRootDisk(
        lease,
        account,
        async () => new Response("private provider diagnostics", { status: 403 }),
      ),
    ).rejects.toThrow("HTTP 403");
    await expect(lookupAWSOriginalRootDisk(lease, account, lookup([]))).rejects.toThrow(
      "unambiguous original allocation",
    );
    await expect(
      lookupAWSOriginalRootDisk(lease, account, async () =>
        Response.json({ Events: [], NextToken: "repeat" }),
      ),
    ).rejects.toThrow(/original root disk|AWS CloudTrail|AWS scope recovery/);
  });
  it.each([
    ["AccessDeniedException", { __type: "AccessDeniedException" }],
    ["InvalidTimeRangeException", { __type: "com.amazonaws.cloudtrail#InvalidTimeRangeException" }],
    ["InvalidLookupAttributesException", { code: "InvalidLookupAttributesException" }],
    ["ThrottlingException", { __type: "ThrottlingException" }],
    ["UnrecognizedClientException", { __type: "UnrecognizedClientException" }],
  ])("reports only the known provider error code %s", async (code, body) => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(now);
    const fetch = vi.fn<() => Promise<Response>>(async () =>
      Response.json(
        { ...body, message: "private provider message", credentials: "private" },
        { status: 400 },
      ),
    );
    const before = structuredClone(lease);
    const error = await lookupAWSOriginalRootDisk(lease, account, fetch).catch(
      (value: Error) => value,
    );
    expect(String(error)).toContain(`HTTP 400, code ${code}`);
    expect(String(error)).not.toContain("private");
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(lease).toEqual(before);
  });
  it.each([
    JSON.stringify({ __type: "PrivateUnknownCode", message: "private" }),
    JSON.stringify({ __type: { code: "AccessDeniedException" } }),
    "malformed private response",
    JSON.stringify({ __type: "AccessDeniedException", message: "x".repeat(4096) }),
  ])("preserves the generic failure for untrusted diagnostic bodies", async (body) => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(now);
    const error = await lookupAWSOriginalRootDisk(
      lease,
      account,
      async () => new Response(body, { status: 400 }),
    ).catch((value: Error) => value);
    expect(String(error)).toContain("HTTP 400)");
    expect(String(error)).not.toContain("code ");
    expect(String(error)).not.toContain("private");
  });
  it("uses the error-type header without reading or exposing the body", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(now);
    const cancel = vi.fn<() => void>();
    const body = new ReadableStream<Uint8Array>({ cancel });
    const response = new Response(body, {
      status: 400,
      headers: { "x-amzn-errortype": "ThrottlingException:private-suffix" },
    });
    await expect(lookupAWSOriginalRootDisk(lease, account, async () => response)).rejects.toThrow(
      "HTTP 400, code ThrottlingException",
    );
    expect(cancel).toHaveBeenCalledTimes(1);
    expect(body.locked).toBe(false);
  });
  it("does not replace an unknown header code with a recognized body code", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(now);
    const response = Response.json(
      { __type: "AccessDeniedException" },
      { status: 400, headers: { "x-amzn-errortype": "private-unknown-code" } },
    );
    const result = await lookupAWSOriginalRootDisk(lease, account, async () => response).catch(
      (value: Error) => value,
    );
    expect(String(result)).toContain("HTTP 400)");
    expect(String(result)).not.toMatch(/private|AccessDeniedException/);
    expect(response.body?.locked).toBe(false);
  });
  it("preserves the HTTP failure when the diagnostic body read fails", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(now);
    const body = new ReadableStream<Uint8Array>({
      pull(controller) {
        controller.error(new Error("private read failure"));
      },
    });
    const result = await lookupAWSOriginalRootDisk(
      lease,
      account,
      async () => new Response(body, { status: 400 }),
    ).catch((value: Error) => value);
    expect(String(result)).toContain("HTTP 400)");
    expect(String(result)).not.toContain("private");
    expect(body.locked).toBe(false);
  });
  it("bounds a stalled diagnostic read and cancellation without replacing the HTTP failure", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(now);
    const timeout = new AbortController();
    vi.spyOn(AbortSignal, "timeout").mockReturnValue(timeout.signal);
    const cancel = vi.fn<() => Promise<void>>(() => new Promise<void>(() => {}));
    const body = new ReadableStream<Uint8Array>({ cancel });
    const fetch = vi.fn<() => Promise<Response>>(async () => new Response(body, { status: 400 }));
    const result = lookupAWSOriginalRootDisk(lease, account, fetch).catch((value: Error) => value);
    await vi.advanceTimersByTimeAsync(1_000);
    timeout.abort(new Error("private timeout diagnostic"));
    expect(String(await result)).toContain("HTTP 400)");
    expect(String(await result)).not.toContain("private");
    expect(cancel).toHaveBeenCalledTimes(1);
    expect(body.locked).toBe(false);
    expect(fetch).toHaveBeenCalledTimes(1);
  });
  it("uses the operation's fixed provider credentials and original Region for history", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(now);
    const actions: string[] = [];
    const requests: { url: string; authorization: string | null; body: string }[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const request = input instanceof Request ? input : new Request(input, init);
        const body = await request.text();
        requests.push({
          url: request.url,
          authorization: request.headers.get("authorization"),
          body,
        });
        if (request.url.includes("cloudtrail")) {
          actions.push("LookupEvents");
          return Response.json({ Events: [summary()] });
        }
        actions.push(new URLSearchParams(body).get("Action")!);
        return new Response(
          "<GetCallerIdentityResponse><GetCallerIdentityResult><Account>123456789012</Account><Arn>arn:aws:iam::123456789012:user/test</Arn><UserId>test</UserId></GetCallerIdentityResult></GetCallerIdentityResponse>",
          { headers: { "content-type": "text/xml" } },
        );
      }),
    );
    const client = new EC2SpotClient(
      { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "test" } as never,
      lease.region!,
    );
    await expect(
      client.withLeaseOperation(async (op) =>
        op.client.originalRootDiskEvidence(lease, (await op.verifiedIdentity()).account),
      ),
    ).resolves.toMatchObject({ disk });
    expect(actions).toEqual(["GetCallerIdentity", "GetCallerIdentity", "LookupEvents"]);
    expect(requests[2]).toMatchObject({
      url: "https://cloudtrail.eu-west-1.amazonaws.com/",
      authorization: expect.stringContaining("/eu-west-1/cloudtrail/"),
    });
    expect(JSON.parse(requests[2]!.body).LookupAttributes).toEqual([
      { AttributeKey: "ResourceName", AttributeValue: lease.cloudID },
    ]);
  });
});
