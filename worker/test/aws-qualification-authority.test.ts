import { readFileSync } from "node:fs";

import { afterEach, describe, expect, it, vi } from "vitest";

import { EC2SpotClient } from "../src/aws";
import {
  AWSQualificationController,
  AWSQualificationRun,
  AWSQualificationTransport,
} from "../src/aws-qualification-authority";
import type {
  AWSQualificationControllerProps,
  AWSQualificationRequest,
  AWSQualificationRunIdentity,
} from "../src/aws-qualification-contract";
import { leaseConfig } from "../src/config";
import { AWSProvider } from "../src/fleet";
import type { CoordinatorCheckpointRecord, Env } from "../src/types";

const controller: AWSQualificationControllerProps = { deploymentHash: "d".repeat(64) };
const identity: AWSQualificationRunIdentity = {
  runId: "qualification-test-1",
  owner: "maintainer",
  candidateSha: "a".repeat(40),
  deploymentHash: controller.deploymentHash,
  expiresAt: new Date(Date.now() + 60 * 60_000).toISOString(),
};
const authorityConfig = readFileSync(
  new URL("../wrangler.aws-qualification-authority.jsonc", import.meta.url),
  "utf8",
);

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("AWS qualification authority deployment", () => {
  it("has no public route, preview URL, workers.dev URL, or cron", () => {
    expect(authorityConfig).toContain('"workers_dev": false');
    expect(authorityConfig).toContain('"preview_urls": false');
    expect(authorityConfig).not.toContain('"routes"');
    expect(authorityConfig).not.toContain('"triggers"');
    expect(authorityConfig).not.toContain('"crons"');
  });

  it("separates the candidate transport from the protected controller entrypoint", async () => {
    const enroll =
      vi.fn<
        (
          controller: AWSQualificationControllerProps,
          identity: AWSQualificationRunIdentity,
        ) => Promise<void>
      >();
    const finalize = vi.fn<(controller: AWSQualificationControllerProps) => Promise<unknown>>();
    const execute =
      vi.fn<
        (
          identity: AWSQualificationRunIdentity,
          request: AWSQualificationRequest,
        ) => Promise<{ status: number; body: string }>
      >();
    execute.mockResolvedValue({ status: 200, body: "ok" });
    const namespace = {
      idFromName: vi.fn<(name: string) => string>((name) => name),
      get: vi.fn<
        (id: string) => {
          enroll: typeof enroll;
          execute: typeof execute;
          finalize: typeof finalize;
        }
      >(() => ({ enroll, execute, finalize })),
    };
    const env = { AWS_QUALIFICATION_RUNS: namespace } as never;
    const candidate = new AWSQualificationTransport(env, identity);
    const protectedController = new AWSQualificationController(env, controller);

    expect("enroll" in candidate).toBe(false);
    expect("finalize" in candidate).toBe(false);
    await candidate.execute(request("GetCallerIdentity"));
    await protectedController.enroll(identity);
    await protectedController.finalize(identity.runId);
    expect(execute).toHaveBeenCalledWith(identity, expect.any(Object));
    expect(enroll).toHaveBeenCalledWith(controller, identity);
    expect(finalize).toHaveBeenCalledWith(controller);
  });
});

describe("AWS qualification authority", () => {
  it("rejects cross-run identity, policy drift, FSR, and foreign resources", async () => {
    const fixture = authorityFixture();
    await expect(fixture.run.enroll({ deploymentHash: "e".repeat(64) }, identity)).rejects.toThrow(
      "not bound",
    );
    await fixture.run.enroll(controller, identity);

    await expect(
      fixture.run.execute(
        { ...identity, candidateSha: "b".repeat(40) },
        request("GetCallerIdentity"),
      ),
    ).rejects.toThrow("not enrolled");
    await expect(
      fixture.run.execute(identity, request("EnableFastSnapshotRestores", {}, "ec2")),
    ).rejects.toThrow("fast snapshot restore is disabled");
    await expect(
      fixture.run.execute(
        identity,
        request("TerminateInstances", { "InstanceId.1": "i-foreign" }, "ec2"),
      ),
    ).rejects.toThrow("outside the run ledger");
    fixture.env.CRABBOX_AWS_QUALIFICATION_ROOT_GB = "19";
    await expect(fixture.run.execute(identity, request("GetCallerIdentity"))).rejects.toThrow(
      "policy changed",
    );
  });

  it("accepts the real EC2SpotClient key contract and owns only the verified physical key", async () => {
    const fixture = authorityFixture();
    await fixture.run.enroll(controller, identity);
    const client = new EC2SpotClient(
      {
        CRABBOX_AWS_QUALIFICATION_TRANSPORT: {
          execute: (value) => fixture.run.execute(identity, value),
        },
      } as Env,
      "us-east-1",
    ) as unknown as {
      ensureSSHKey(name: string, publicKey: string, leaseID: string): Promise<void>;
    };

    await client.ensureSSHKey(
      "crabbox-cbx-abcdef123456",
      "ssh-ed25519 AAAAqualification reviewer",
      "cbx_abcdef123456",
    );

    const imported = fixture.signer.calls.find((call) => call.action === "ImportKeyPair");
    expect(imported?.parameters["PublicKeyMaterial"]).toBe(
      btoa("ssh-ed25519 AAAAqualification reviewer"),
    );
    expect(imported?.parameters["KeyName"]).toMatch(/^cbxq-[0-9a-f]{32}$/);
    expect(imported?.parameters["KeyName"]).not.toBe("crabbox-cbx-abcdef123456");
    expect(fixture.signer.calls.filter((call) => call.action === "DescribeKeyPairs")).toHaveLength(
      2,
    );
  });

  it("serializes concurrent launches and enforces three confirmed sequential launches", async () => {
    const fixture = authorityFixture();
    await fixture.run.enroll(controller, identity);
    await importKey(fixture);

    const outcomes = await Promise.allSettled([
      fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2")),
      fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2")),
    ]);
    expect(outcomes.filter((result) => result.status === "fulfilled")).toHaveLength(1);
    expect(outcomes.filter((result) => result.status === "rejected")).toHaveLength(1);
    expect(fixture.signer.calls.filter((call) => call.action === "RunInstances")).toHaveLength(1);

    await terminateOwned(fixture);
    await fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2"));
    await terminateOwned(fixture);
    await fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2"));
    await terminateOwned(fixture);
    await expect(
      fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2")),
    ).rejects.toThrow("launch budget");
  });

  it("runs the real public stop path through source, candidate, and promoted image order", async () => {
    vi.stubGlobal("setTimeout", ((callback: () => void) => {
      queueMicrotask(callback);
      return 0;
    }) as typeof setTimeout);
    const fixture = authorityFixture();
    await fixture.run.enroll(controller, identity);
    const env = {
      CRABBOX_AWS_QUALIFICATION_TRANSPORT: {
        execute: (value: AWSQualificationRequest) => fixture.run.execute(identity, value),
      },
    } as Env;
    const client = new EC2SpotClient(env, "us-east-1") as unknown as {
      createServer(
        config: ReturnType<typeof leaseConfig>,
        leaseID: string,
        slug: string,
        owner: string,
        imageID: string,
        securityGroupID: string,
      ): Promise<{ cloudID: string }>;
      ensureSSHKey(name: string, publicKey: string, leaseID: string): Promise<void>;
    };
    const provider = new AWSProvider(env, "us-east-1", {} as never);
    const config = {
      ...leaseConfig({
        provider: "aws",
        target: "linux",
        serverType: "t3.small",
        serverTypeExplicit: true,
        capacity: { market: "on-demand" },
        providerKey: "crabbox-cbx-abcdef123456",
        sshPublicKey: "ssh-ed25519 AAAAqualification",
      }),
      awsProfile: "",
      awsRootGB: 20,
      awsSubnetID: "subnet-fixed",
    };
    await client.ensureSSHKey(config.providerKey, config.sshPublicKey, "cbx_abcdef123456");

    const source = await client.createServer(
      config,
      "cbx_source000001",
      "qualification-source",
      "maintainer",
      "ami-base",
      "sg-fixed",
    );
    await fixture.run.execute(
      identity,
      request(
        "CreateImage",
        { InstanceId: source.cloudID, Name: "qualification-image", NoReboot: "true" },
        "ec2",
      ),
    );
    await provider.deleteServer(source.cloudID);
    const candidate = await client.createServer(
      config,
      "cbx_candidate001",
      "qualification-candidate",
      "maintainer",
      "ami-created",
      "sg-fixed",
    );
    await provider.deleteServer(candidate.cloudID);
    const promoted = await client.createServer(
      config,
      "cbx_promoted0001",
      "qualification-promoted",
      "maintainer",
      "ami-created",
      "sg-fixed",
    );
    await provider.deleteServer(promoted.cloudID);

    expect(
      fixture.signer.calls
        .filter((call) => call.action === "RunInstances" || call.action === "CreateImage")
        .map((call) =>
          call.action === "RunInstances"
            ? `RunInstances:${call.parameters["ImageId"]}`
            : call.action,
        ),
    ).toEqual([
      "RunInstances:ami-base",
      "CreateImage",
      "RunInstances:ami-created",
      "RunInstances:ami-created",
    ]);
    expect(
      fixture.signer.calls.filter((call) => call.action === "TerminateInstances"),
    ).toHaveLength(3);
  });

  it("reconciles an ambiguous launch with a run-and-op scoped client token", async () => {
    const fixture = authorityFixture({ failOnce: "RunInstances" });
    await fixture.run.enroll(controller, identity);
    await importKey(fixture);
    const launch = request("RunInstances", runInstancesParams(), "ec2");

    await expect(fixture.run.execute(identity, launch)).rejects.toThrow("lost response");
    await expect(fixture.run.execute(identity, launch)).resolves.toMatchObject({ status: 200 });
    const calls = fixture.signer.calls.filter((call) => call.action === "RunInstances");
    expect(calls).toHaveLength(2);
    expect(calls[0]!.parameters["ClientToken"]).toBe(calls[1]!.parameters["ClientToken"]);
    expect(String(calls[0]!.parameters["ClientToken"])).toMatch(/^cbxq-[0-9a-f]{59}$/);
    await fixture.run.execute(identity, launch);
    expect(fixture.signer.calls.filter((call) => call.action === "RunInstances")).toHaveLength(2);
  });

  it("never redispatches a no-effect pending launch after expiry", async () => {
    vi.useFakeTimers();
    useImmediateTimeouts();
    const now = new Date("2026-09-04T00:00:00Z");
    vi.setSystemTime(now);
    const expiring = { ...identity, expiresAt: new Date(now.getTime() + 1_000).toISOString() };
    const fixture = authorityFixture({ failOnce: "RunInstances" });
    await fixture.run.enroll(controller, expiring);
    await fixture.run.execute(
      expiring,
      request(
        "ImportKeyPair",
        {
          KeyName: "crabbox-cbx-abcdef123456",
          PublicKeyMaterial: btoa("ssh-ed25519 AAAAqualification"),
        },
        "ec2",
      ),
    );
    await expect(
      fixture.run.execute(expiring, request("RunInstances", runInstancesParams(), "ec2")),
    ).rejects.toThrow("lost response");
    const launchCalls = fixture.signer.calls.filter((call) => call.action === "RunInstances");
    expect(launchCalls).toHaveLength(1);
    const callsBeforeExpiry = fixture.signer.calls.length;

    vi.setSystemTime(new Date(now.getTime() + 2_000));
    await expect(fixture.run.execute(expiring, request("GetCallerIdentity"))).rejects.toThrow(
      "expired",
    );
    expect(fixture.signer.calls.filter((call) => call.action === "RunInstances")).toHaveLength(1);
    expect(
      fixture.signer.calls
        .slice(callsBeforeExpiry)
        .filter((call) => call.action === "RunInstances"),
    ).toHaveLength(0);
    expect(fixture.signer.calls.some((call) => call.action === "DeleteKeyPair")).toBe(true);
    expect((await fixture.storage.list({ prefix: "intent:" })).size).toBe(0);
  });

  it("drops an undispatched generic mutation when account verification crosses expiry", async () => {
    vi.useFakeTimers();
    useImmediateTimeouts();
    const now = new Date("2026-09-04T00:00:00Z");
    vi.setSystemTime(now);
    const expiring = { ...identity, expiresAt: new Date(now.getTime() + 1_000).toISOString() };
    const fixture = authorityFixture();
    await fixture.run.enroll(controller, expiring);
    await importKey(fixture, expiring);
    fixture.signer.advanceNextIdentityByMs = 2_000;

    await expect(
      fixture.run.execute(
        expiring,
        request(
          "CreateTags",
          {
            "ResourceId.1": "key-owned",
            "Tag.1.Key": "Name",
            "Tag.1.Value": "expired-candidate-mutation",
          },
          "ec2",
        ),
      ),
    ).rejects.toThrow("expired before mutation dispatch");
    expect(fixture.signer.calls.filter((call) => call.action === "CreateTags")).toHaveLength(0);
    expect(fixture.signer.calls.some((call) => call.action === "DeleteKeyPair")).toBe(true);
    expect((await fixture.storage.list({ prefix: "intent:" })).size).toBe(0);
  });

  it("does not recover a prepared mutation that never reached the signer", async () => {
    useImmediateTimeouts();
    const fixture = authorityFixture();
    await fixture.run.enroll(controller, identity);
    await importKey(fixture);
    const prepared = request(
      "CreateTags",
      {
        "ResourceId.1": "key-owned",
        "Tag.1.Key": "Name",
        "Tag.1.Value": "never-dispatched",
      },
      "ec2",
    );
    await fixture.storage.put(`intent:${prepared.opId}`, {
      phase: "prepared",
      requestHash: "prepared-request",
      request: prepared,
      startedAt: new Date().toISOString(),
    });

    await expect(fixture.run.finalize(controller)).resolves.toBeDefined();
    expect(fixture.signer.calls.filter((call) => call.action === "CreateTags")).toHaveLength(0);
    expect((await fixture.storage.list({ prefix: "intent:" })).size).toBe(0);
  });

  it("discovers and cleans a lost-response launch after expiry without redispatch", async () => {
    vi.useFakeTimers();
    useImmediateTimeouts();
    const now = new Date("2026-09-04T00:00:00Z");
    vi.setSystemTime(now);
    const expiring = { ...identity, expiresAt: new Date(now.getTime() + 1_000).toISOString() };
    const fixture = authorityFixture({ loseAfterEffect: "RunInstances" });
    await fixture.run.enroll(controller, expiring);
    await fixture.run.execute(
      expiring,
      request(
        "ImportKeyPair",
        {
          KeyName: "crabbox-cbx-abcdef123456",
          PublicKeyMaterial: btoa("ssh-ed25519 AAAAqualification"),
        },
        "ec2",
      ),
    );
    await expect(
      fixture.run.execute(expiring, request("RunInstances", runInstancesParams(), "ec2")),
    ).rejects.toThrow("lost response");
    const callsBeforeExpiry = fixture.signer.calls.length;

    vi.setSystemTime(new Date(now.getTime() + 2_000));
    await expect(fixture.run.execute(expiring, request("GetCallerIdentity"))).rejects.toThrow(
      "expired",
    );
    expect(
      fixture.signer.calls
        .slice(callsBeforeExpiry)
        .filter((call) => call.action === "RunInstances"),
    ).toHaveLength(0);
    expect(fixture.signer.calls.some((call) => call.action === "TerminateInstances")).toBe(true);
  });

  it("captures the CreateImage child snapshot and permits only one active checkpoint set", async () => {
    const fixture = authorityFixture();
    await fixture.run.enroll(controller, identity);
    await importKey(fixture);
    await fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2"));
    const create = request(
      "CreateImage",
      { InstanceId: "i-owned", Name: "qualification-image", NoReboot: "true" },
      "ec2",
    );

    await fixture.run.execute(identity, create);
    await expect(
      fixture.run.execute(identity, { ...create, opId: crypto.randomUUID() }),
    ).rejects.toThrow("one active checkpoint");
    const ledger = await fixture.storage.get<{
      imageIds: string[];
      snapshotIds: string[];
    }>("ledger");
    expect(ledger).toMatchObject({ imageIds: ["ami-created"], snapshotIds: ["snap-child"] });
  });

  it("keeps bounded deletion tombstones for provider verification and retry", async () => {
    const fixture = authorityFixture({ deleteNotFound: "DeregisterImage" });
    await fixture.run.enroll(controller, identity);
    await importKey(fixture);
    await fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2"));
    await fixture.run.execute(
      identity,
      request(
        "CreateImage",
        { InstanceId: "i-owned", Name: "qualification-image", NoReboot: "true" },
        "ec2",
      ),
    );
    const env = {
      CRABBOX_AWS_QUALIFICATION_TRANSPORT: {
        execute: (value: AWSQualificationRequest) => fixture.run.execute(identity, value),
      },
    } as Env;
    const provider = new AWSProvider(env, "us-east-1", fixture.storage as never);
    const checkpoint = {
      leaseID: "cbx_source000001",
      name: "qualification-image",
      scope: { accountID: "123456789012", region: "us-east-1" },
      image: {
        id: "ami-created",
        resourceID: "ami-created",
        kind: "aws-ami",
        immutableID: "ami-created",
        snapshotIDs: ["snap-child"],
        state: "available",
      },
    } as CoordinatorCheckpointRecord;

    await expect(provider.deleteCheckpointImage(checkpoint)).resolves.toBeUndefined();
    const ledger = await fixture.storage.get<{
      imageIds: string[];
      retiredImageIds: string[];
      retiredSnapshotIds: string[];
      snapshotIds: string[];
    }>("ledger");
    expect(ledger).toMatchObject({
      imageIds: [],
      retiredImageIds: ["ami-created"],
      retiredSnapshotIds: ["snap-child"],
      snapshotIds: [],
    });
    await expect(
      fixture.run.execute(identity, request("DeregisterImage", { ImageId: "ami-created" }, "ec2")),
    ).resolves.toMatchObject({ status: 400 });
    expect(fixture.signer.calls.filter((call) => call.action === "DeregisterImage")).toHaveLength(
      2,
    );
    await expect(
      fixture.run.execute(identity, request("DeleteKeyPair", { KeyPairId: "key-owned" }, "ec2")),
    ).resolves.toMatchObject({ status: 200 });
    await expect(
      fixture.run.execute(
        identity,
        request("DescribeKeyPairs", { "KeyPairId.1": "key-owned" }, "ec2"),
      ),
    ).resolves.toMatchObject({ status: 400 });
    await expect(
      fixture.run.execute(identity, request("DeleteKeyPair", { KeyPairId: "key-owned" }, "ec2")),
    ).resolves.toMatchObject({ status: 200 });
    await expect(
      fixture.run.execute(
        identity,
        request("DescribeImages", { "ImageId.1": "ami-foreign" }, "ec2"),
      ),
    ).rejects.toThrow("outside the run ledger");
  });

  it("allows only the fixed base AMI or an active run-derived AMI", async () => {
    const fixture = authorityFixture();
    await fixture.run.enroll(controller, identity);
    await importKey(fixture);
    await fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2"));
    await fixture.run.execute(
      identity,
      request(
        "CreateImage",
        { InstanceId: "i-owned", Name: "qualification-image", NoReboot: "true" },
        "ec2",
      ),
    );
    await terminateOwned(fixture);

    await expect(
      fixture.run.execute(
        identity,
        request("RunInstances", { ...runInstancesParams(), ImageId: "ami-foreign" }, "ec2"),
      ),
    ).rejects.toThrow("outside the run ledger");
    await expect(
      fixture.run.execute(
        identity,
        request("RunInstances", { ...runInstancesParams(), ImageId: "ami-created" }, "ec2"),
      ),
    ).resolves.toMatchObject({ status: 200 });
    await terminateOwned(fixture);
    await fixture.run.execute(
      identity,
      request("DeregisterImage", { ImageId: "ami-created" }, "ec2"),
    );
    await fixture.run.execute(
      identity,
      request("DeleteSnapshot", { SnapshotId: "snap-child" }, "ec2"),
    );
    await expect(
      fixture.run.execute(
        identity,
        request("RunInstances", { ...runInstancesParams(), ImageId: "ami-created" }, "ec2"),
      ),
    ).rejects.toThrow("outside the run ledger");
  });

  it("reconciles lost and delayed key creation without redispatching ImportKeyPair", async () => {
    useImmediateTimeouts();
    const fixture = authorityFixture({
      delayedKeyVisibility: 2,
      loseAfterEffect: "ImportKeyPair",
    });
    await fixture.run.enroll(controller, identity);
    const keyRequest = request(
      "ImportKeyPair",
      {
        KeyName: "crabbox-cbx-abcdef123456",
        PublicKeyMaterial: btoa("ssh-ed25519 AAAAqualification"),
      },
      "ec2",
    );
    const imported = fixture.run.execute(identity, keyRequest);
    await expect(imported).rejects.toThrow("lost response");

    const replay = fixture.run.execute(identity, keyRequest);
    await expect(replay).resolves.toMatchObject({ status: 200 });
    expect(fixture.signer.calls.filter((call) => call.action === "ImportKeyPair")).toHaveLength(1);
  });

  it("reconciles a 5xx and delayed image without redispatching CreateImage", async () => {
    useImmediateTimeouts();
    const fixture = authorityFixture({
      delayedImageVisibility: 3,
      http500AfterEffect: "CreateImage",
    });
    await fixture.run.enroll(controller, identity);
    await importKey(fixture);
    await fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2"));
    const create = request(
      "CreateImage",
      { InstanceId: "i-owned", Name: "qualification-image", NoReboot: "true" },
      "ec2",
    );
    await expect(fixture.run.execute(identity, create)).rejects.toThrow("response is ambiguous");

    const replay = fixture.run.execute(identity, create);
    await expect(replay).resolves.toMatchObject({ status: 200 });
    expect(fixture.signer.calls.filter((call) => call.action === "CreateImage")).toHaveLength(1);
  });

  it("retires a no-effect ImportKeyPair intent after authoritative absence", async () => {
    useImmediateTimeouts();
    const fixture = authorityFixture({ failOnce: "ImportKeyPair" });
    await fixture.run.enroll(controller, identity);
    await expect(importKey(fixture)).rejects.toThrow("lost response");

    await expect(fixture.run.finalize(controller)).resolves.toBeDefined();
    expect((await fixture.storage.list({ prefix: "intent:" })).size).toBe(0);
    expect(fixture.signer.calls.some((call) => call.action === "DeleteKeyPair")).toBe(false);
  });

  it("retires a no-effect CreateImage intent after authoritative cleanup", async () => {
    useImmediateTimeouts();
    const fixture = authorityFixture({ failOnce: "CreateImage" });
    await fixture.run.enroll(controller, identity);
    await importKey(fixture);
    await fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2"));
    await expect(
      fixture.run.execute(
        identity,
        request(
          "CreateImage",
          { InstanceId: "i-owned", Name: "qualification-image", NoReboot: "true" },
          "ec2",
        ),
      ),
    ).rejects.toThrow("lost response");

    await expect(fixture.run.finalize(controller)).resolves.toBeDefined();
    expect((await fixture.storage.list({ prefix: "intent:" })).size).toBe(0);
    expect(fixture.signer.calls.some((call) => call.action === "DeregisterImage")).toBe(false);
  });

  it("cleans up an image that becomes visible only during final inventory", async () => {
    useImmediateTimeouts();
    const fixture = authorityFixture({
      delayedImageVisibility: 8,
      loseAfterEffect: "CreateImage",
    });
    await fixture.run.enroll(controller, identity);
    await importKey(fixture);
    await fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2"));
    await expect(
      fixture.run.execute(
        identity,
        request(
          "CreateImage",
          { InstanceId: "i-owned", Name: "qualification-image", NoReboot: "true" },
          "ec2",
        ),
      ),
    ).rejects.toThrow("lost response");

    await expect(fixture.run.finalize(controller)).resolves.toBeDefined();
    expect(fixture.signer.calls.some((call) => call.action === "DeregisterImage")).toBe(true);
    expect(fixture.signer.calls.some((call) => call.action === "DeleteSnapshot")).toBe(true);
    expect((await fixture.storage.list({ prefix: "intent:" })).size).toBe(0);
  });

  it("cleans up a key pair that becomes visible only during final inventory", async () => {
    useImmediateTimeouts();
    const fixture = authorityFixture({
      delayedKeyVisibility: 8,
      loseAfterEffect: "ImportKeyPair",
    });
    await fixture.run.enroll(controller, identity);
    await expect(importKey(fixture)).rejects.toThrow("lost response");

    await expect(fixture.run.finalize(controller)).resolves.toBeDefined();
    expect(fixture.signer.calls.some((call) => call.action === "DeleteKeyPair")).toBe(true);
    expect((await fixture.storage.list({ prefix: "intent:" })).size).toBe(0);
  });

  it("retires a pending snapshot deletion after recovery confirms absence", async () => {
    useImmediateTimeouts();
    const fixture = authorityFixture({
      deleteNotFound: "DeleteSnapshot",
      loseAfterEffect: "DeleteSnapshot",
    });
    await fixture.run.enroll(controller, identity);
    await importKey(fixture);
    await fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2"));
    await fixture.run.execute(
      identity,
      request(
        "CreateImage",
        { InstanceId: "i-owned", Name: "qualification-image", NoReboot: "true" },
        "ec2",
      ),
    );
    await fixture.run.execute(
      identity,
      request("DeregisterImage", { ImageId: "ami-created" }, "ec2"),
    );
    await expect(
      fixture.run.execute(identity, request("DeleteSnapshot", { SnapshotId: "snap-child" }, "ec2")),
    ).rejects.toThrow("lost response");

    await expect(fixture.run.finalize(controller)).resolves.toBeDefined();
    expect((await fixture.storage.list({ prefix: "intent:" })).size).toBe(0);
    expect(fixture.signer.calls.filter((call) => call.action === "DeleteSnapshot").length).toBe(3);
  });

  it("revalidates the expected STS account before every mutation", async () => {
    const fixture = authorityFixture();
    await fixture.run.enroll(controller, identity);
    await importKey(fixture);
    fixture.signer.accountId = "999999999999";

    await expect(
      fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2")),
    ).rejects.toThrow("wrong account");
    expect(fixture.signer.calls.filter((call) => call.action === "RunInstances")).toHaveLength(0);
  });

  it("revalidates the expected STS account before finalization and reconciliation", async () => {
    useImmediateTimeouts();
    const finalizeFixture = authorityFixture();
    await finalizeFixture.run.enroll(controller, identity);
    await importKey(finalizeFixture);
    finalizeFixture.signer.accountId = "999999999999";
    await expect(finalizeFixture.run.finalize(controller)).rejects.toThrow("wrong account");
    expect(
      finalizeFixture.signer.calls.filter((call) => call.action === "DescribeInstances"),
    ).toHaveLength(0);

    const reconcileFixture = authorityFixture({ loseAfterEffect: "CreateImage" });
    await reconcileFixture.run.enroll(controller, identity);
    await importKey(reconcileFixture);
    await reconcileFixture.run.execute(
      identity,
      request("RunInstances", runInstancesParams(), "ec2"),
    );
    const create = request(
      "CreateImage",
      { InstanceId: "i-owned", Name: "qualification-image", NoReboot: "true" },
      "ec2",
    );
    await expect(reconcileFixture.run.execute(identity, create)).rejects.toThrow("lost response");
    reconcileFixture.signer.accountId = "999999999999";
    await expect(reconcileFixture.run.execute(identity, create)).rejects.toThrow("wrong account");
    expect(
      reconcileFixture.signer.calls.filter((call) => call.action === "DescribeImages"),
    ).toHaveLength(0);
  });

  it("retains active instance ownership until Describe confirms termination", async () => {
    const fixture = authorityFixture();
    await fixture.run.enroll(controller, identity);
    await importKey(fixture);
    await fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2"));
    await fixture.run.execute(
      identity,
      request("TerminateInstances", { "InstanceId.1": "i-owned" }, "ec2"),
    );

    await expect(
      fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2")),
    ).rejects.toThrow("one active instance");
    await fixture.run.execute(
      identity,
      request("DescribeInstances", { "InstanceId.1": "i-owned" }, "ec2"),
    );
    await expect(
      fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2")),
    ).rejects.toThrow("one active instance");
    await fixture.run.execute(
      identity,
      request("DescribeInstances", { "InstanceId.1": "i-owned" }, "ec2"),
    );
    await expect(
      fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2")),
    ).resolves.toMatchObject({ status: 200 });
  });

  it("retires an acknowledged missing instance before the next public launch", async () => {
    useImmediateTimeouts();
    const fixture = authorityFixture();
    await fixture.run.enroll(controller, identity);
    await importKey(fixture);
    await fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2"));
    fixture.signer.instanceDescribeNotFound = true;
    await expect(
      fixture.run.execute(
        identity,
        request("DescribeInstances", { "InstanceId.1": "i-owned" }, "ec2"),
      ),
    ).resolves.toMatchObject({ status: 400 });
    await expect(
      fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2")),
    ).rejects.toThrow("one active instance");

    const env = {
      CRABBOX_AWS_QUALIFICATION_TRANSPORT: {
        execute: (value: AWSQualificationRequest) => fixture.run.execute(identity, value),
      },
    } as Env;
    const provider = new AWSProvider(env, "us-east-1", {} as never);
    await provider.deleteServer("i-owned");
    expect(
      await fixture.storage.get<{
        instanceIds: string[];
        retiredInstanceIds: string[];
        terminatingInstanceIds: string[];
      }>("ledger"),
    ).toMatchObject({
      instanceIds: [],
      retiredInstanceIds: ["i-owned"],
      terminatingInstanceIds: [],
    });
    fixture.signer.instanceDescribeNotFound = false;
    await expect(
      fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2")),
    ).resolves.toMatchObject({ status: 200 });
  });

  it("confirms mixed DescribeInstances absence one instance at a time", async () => {
    const fixture = authorityFixture();
    await fixture.run.enroll(controller, identity);
    await importKey(fixture);
    await fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2"));
    await fixture.run.execute(
      identity,
      request("TerminateInstances", { "InstanceId.1": "i-owned" }, "ec2"),
    );
    const ledger = await fixture.storage.get<{
      instanceIds: string[];
      retiredInstanceIds: string[];
      terminatingInstanceIds: string[];
    }>("ledger");
    expect(ledger).toBeDefined();
    ledger!.retiredInstanceIds = ["i-retired"];
    await fixture.storage.put("ledger", ledger);

    await expect(
      fixture.run.execute(
        identity,
        request(
          "DescribeInstances",
          { "InstanceId.1": "i-retired", "InstanceId.2": "i-owned" },
          "ec2",
        ),
      ),
    ).resolves.toMatchObject({ status: 400 });
    expect(
      await fixture.storage.get<{
        instanceIds: string[];
        retiredInstanceIds: string[];
        terminatingInstanceIds: string[];
      }>("ledger"),
    ).toMatchObject({
      instanceIds: ["i-owned"],
      retiredInstanceIds: ["i-retired"],
      terminatingInstanceIds: ["i-owned"],
    });
    await expect(
      fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2")),
    ).rejects.toThrow("one active instance");
  });

  it("recovers a lost termination only after repeated per-instance absence", async () => {
    useImmediateTimeouts();
    const fixture = authorityFixture({
      loseAfterEffect: "TerminateInstances",
      terminationDescribeNotFound: true,
    });
    await fixture.run.enroll(controller, identity);
    await importKey(fixture);
    await fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2"));

    await expect(
      fixture.run.execute(
        identity,
        request("TerminateInstances", { "InstanceId.1": "i-owned" }, "ec2"),
      ),
    ).rejects.toThrow("lost response");
    expect(
      await fixture.storage.get<{ instanceIds: string[]; terminatingInstanceIds: string[] }>(
        "ledger",
      ),
    ).toMatchObject({ instanceIds: ["i-owned"], terminatingInstanceIds: [] });

    await expect(fixture.run.finalize(controller)).resolves.toBeDefined();
    const terminationCalls = fixture.signer.calls
      .map((call, index) => ({ ...call, index }))
      .filter((call) => call.action === "TerminateInstances");
    const individualConfirmations = fixture.signer.calls
      .map((call, index) => ({ ...call, index }))
      .filter(
        (call) =>
          call.action === "DescribeInstances" && call.parameters["InstanceId.1"] === "i-owned",
      );
    expect(terminationCalls).toHaveLength(2);
    expect(individualConfirmations.length).toBeGreaterThanOrEqual(2);
    expect(individualConfirmations[1]!.index).toBeLessThan(terminationCalls[1]!.index);
    expect((await fixture.storage.list({ prefix: "intent:" })).size).toBe(0);
  });

  it("never adopts or deletes a duplicate foreign physical key", async () => {
    useImmediateTimeouts();
    const fixture = authorityFixture({ duplicateForeignKey: true });
    await fixture.run.enroll(controller, identity);
    const result = await fixture.run.execute(
      identity,
      request(
        "ImportKeyPair",
        {
          KeyName: "crabbox-cbx-abcdef123456",
          PublicKeyMaterial: btoa("ssh-ed25519 AAAAqualification"),
        },
        "ec2",
      ),
    );
    expect(result.status).toBe(400);

    await fixture.run.finalize(controller);
    expect(fixture.signer.calls.some((call) => call.action === "DeleteKeyPair")).toBe(false);
  });

  it("expires into cleanup and a final tag inventory catches unknown resources", async () => {
    vi.useFakeTimers();
    useImmediateTimeouts();
    const now = new Date("2026-09-03T12:00:00Z");
    vi.setSystemTime(now);
    const expiring = { ...identity, expiresAt: new Date(now.getTime() + 1_000).toISOString() };
    const fixture = authorityFixture({ unknownTaggedInstance: true, unknownVisibilityDelay: 2 });
    await fixture.run.enroll(controller, expiring);
    vi.setSystemTime(new Date(now.getTime() + 2_000));

    await expect(fixture.run.execute(expiring, request("GetCallerIdentity"))).rejects.toThrow(
      "expired",
    );
    expect(
      fixture.signer.calls.some(
        (call) =>
          call.action === "TerminateInstances" && call.parameters["InstanceId.1"] === "i-unknown",
      ),
    ).toBe(true);
  });

  it("requires one parsed security group from a successful response", async () => {
    useImmediateTimeouts();
    const errorFixture = authorityFixture({ securityGroupErrorWithId: true });
    await errorFixture.run.enroll(controller, identity);

    await expect(errorFixture.run.finalize(controller)).rejects.toThrow(
      "DescribeSecurityGroups verification http 403",
    );

    const duplicateFixture = authorityFixture({ duplicateSecurityGroup: true });
    await duplicateFixture.run.enroll(controller, identity);
    await expect(duplicateFixture.run.finalize(controller)).rejects.toThrow(
      "preprovisioned security group is missing",
    );
  });

  it("rejects oversized and privileged candidate payloads before AWS", async () => {
    const fixture = authorityFixture();
    await fixture.run.enroll(controller, identity);
    await expect(
      fixture.run.execute(identity, request("GetCallerIdentity", { value: "x".repeat(70 * 1024) })),
    ).rejects.toThrow("64 KiB");
    await importKey(fixture);
    await expect(
      fixture.run.execute(
        identity,
        request(
          "RunInstances",
          { ...runInstancesParams(), "IamInstanceProfile.Name": "admin" },
          "ec2",
        ),
      ),
    ).rejects.toThrow("forbidden");
  });

  it("bounds the complete envelope and rejects unknown, cyclic, and nested input", async () => {
    const fixture = authorityFixture();
    await fixture.run.enroll(controller, identity);
    const cyclic: Record<string, unknown> = {};
    cyclic["self"] = cyclic;
    await expect(
      fixture.run.execute(identity, request("GetCallerIdentity", cyclic)),
    ).rejects.toThrow("contains a cycle");
    await expect(
      fixture.run.execute(
        identity,
        request("GetCallerIdentity", { nested: { deeper: { value: "nope" } } }),
      ),
    ).rejects.toThrow("nesting exceeds policy");
    await expect(
      fixture.run.execute(identity, {
        ...request("GetCallerIdentity"),
        unexpected: "nope",
      } as AWSQualificationRequest),
    ).rejects.toThrow("unknown field");
    await expect(
      fixture.run.execute(identity, {
        ...request("GetCallerIdentity"),
        unexpected: "x".repeat(70 * 1024),
      } as AWSQualificationRequest),
    ).rejects.toThrow("64 KiB");
    expect(fixture.signer.calls).toHaveLength(0);
  });
});

describe("AWS qualification candidate transport", () => {
  it("fails closed without falling back to stray raw credentials", async () => {
    const execute = vi.fn<(request: AWSQualificationRequest) => Promise<never>>();
    execute.mockRejectedValue(new Error("binding unavailable"));
    const client = new EC2SpotClient(
      {
        CRABBOX_AWS_QUALIFICATION_TRANSPORT: { execute },
        AWS_ACCESS_KEY_ID: "must-not-be-used",
        AWS_SECRET_ACCESS_KEY: "must-not-be-used",
      } as Env,
      "us-east-1",
    );

    await expect(client.identity()).rejects.toThrow("binding unavailable");
    expect(execute).toHaveBeenCalledTimes(2);
  });
});

function authorityFixture(
  options: {
    delayedImageVisibility?: number;
    delayedKeyVisibility?: number;
    deleteNotFound?: string;
    duplicateForeignKey?: boolean;
    duplicateSecurityGroup?: boolean;
    failOnce?: string;
    http500AfterEffect?: string;
    loseAfterEffect?: string;
    securityGroupErrorWithId?: boolean;
    terminationDescribeNotFound?: boolean;
    unknownTaggedInstance?: boolean;
    unknownVisibilityDelay?: number;
  } = {},
) {
  const storage = new MemoryStorage();
  const signer = new FakeSigner(options);
  const env = {
    CRABBOX_AWS_QUALIFICATION_ACCOUNT_ID: "123456789012",
    CRABBOX_AWS_QUALIFICATION_BASE_AMI_ID: "ami-base",
    CRABBOX_AWS_QUALIFICATION_REGION: "us-east-1",
    CRABBOX_AWS_QUALIFICATION_ROOT_GB: "20",
    CRABBOX_AWS_QUALIFICATION_SECURITY_GROUP_ID: "sg-fixed",
    CRABBOX_AWS_QUALIFICATION_SUBNET_ID: "subnet-fixed",
  };
  const run = new AWSQualificationRun({ storage } as never, env as never, signer);
  return { env, run, signer, storage };
}

async function importKey(
  fixture: ReturnType<typeof authorityFixture>,
  runIdentity: AWSQualificationRunIdentity = identity,
): Promise<void> {
  await fixture.run.execute(
    runIdentity,
    request(
      "ImportKeyPair",
      {
        KeyName: "crabbox-cbx-abcdef123456",
        PublicKeyMaterial: btoa("ssh-ed25519 AAAAqualification"),
      },
      "ec2",
    ),
  );
}

async function terminateOwned(fixture: ReturnType<typeof authorityFixture>): Promise<void> {
  await fixture.run.execute(
    identity,
    request("TerminateInstances", { "InstanceId.1": "i-owned" }, "ec2"),
  );
  await fixture.run.execute(
    identity,
    request("DescribeInstances", { "InstanceId.1": "i-owned" }, "ec2"),
  );
  await fixture.run.execute(
    identity,
    request("DescribeInstances", { "InstanceId.1": "i-owned" }, "ec2"),
  );
}

function useImmediateTimeouts(): void {
  vi.stubGlobal("setTimeout", ((callback: () => void) => {
    queueMicrotask(callback);
    return 0;
  }) as typeof setTimeout);
}

function request(
  action: string,
  parameters: Record<string, unknown> = {},
  service: AWSQualificationRequest["service"] = "sts",
): AWSQualificationRequest {
  return {
    opId: crypto.randomUUID(),
    region: "us-east-1",
    service,
    action,
    parameters,
  };
}

function runInstancesParams(): Record<string, unknown> {
  return {
    ImageId: "ami-base",
    InstanceType: "t3.small",
    KeyName: "crabbox-cbx-abcdef123456",
    MaxCount: "1",
    MinCount: "1",
    UserData: "IyEvYmluL3NoCg==",
    "BlockDeviceMapping.1.DeviceName": "/dev/sda1",
    "BlockDeviceMapping.1.Ebs.DeleteOnTermination": "true",
    "BlockDeviceMapping.1.Ebs.Encrypted": "true",
    "BlockDeviceMapping.1.Ebs.VolumeSize": "20",
    "BlockDeviceMapping.1.Ebs.VolumeType": "gp3",
    "NetworkInterface.1.AssociatePublicIpAddress": "true",
    "NetworkInterface.1.DeleteOnTermination": "true",
    "NetworkInterface.1.DeviceIndex": "0",
    "NetworkInterface.1.SecurityGroupId.1": "sg-fixed",
    "NetworkInterface.1.SubnetId": "subnet-fixed",
  };
}

class FakeSigner {
  readonly calls: Array<{
    service: string;
    action: string;
    region: string;
    parameters: Record<string, unknown>;
  }> = [];
  accountId = "123456789012";
  advanceNextIdentityByMs = 0;
  instanceDescribeNotFound = false;
  private failed = false;
  private imageDescribeCalls = 0;
  private key?: { id: string; name: string; publicKey: string; runId?: string; sha?: string };
  private keyDescribeCalls = 0;
  private instanceOp = "";
  private instanceState: "absent" | "running" | "shutting-down" | "terminated" = "absent";
  private terminationDescribeCalls = 0;
  private terminationMissingCalls = 0;
  private imageActive = false;
  private snapshotActive = false;
  private unknownActive: boolean;
  private unknownDescribeCalls = 0;

  constructor(
    private readonly options: {
      delayedImageVisibility?: number;
      delayedKeyVisibility?: number;
      deleteNotFound?: string;
      duplicateForeignKey?: boolean;
      duplicateSecurityGroup?: boolean;
      failOnce?: string;
      http500AfterEffect?: string;
      loseAfterEffect?: string;
      securityGroupErrorWithId?: boolean;
      terminationDescribeNotFound?: boolean;
      unknownTaggedInstance?: boolean;
      unknownVisibilityDelay?: number;
    },
  ) {
    this.unknownActive = Boolean(options.unknownTaggedInstance);
  }

  async execute(
    service: string,
    action: string,
    region: string,
    parameters: Record<string, unknown>,
  ): Promise<Response> {
    this.calls.push({ service, action, region, parameters: structuredClone(parameters) });
    if (action === this.options.failOnce && !this.failed) {
      this.failed = true;
      throw new Error("lost response");
    }
    if (action === "GetCallerIdentity") {
      if (this.advanceNextIdentityByMs > 0) {
        const advanceBy = this.advanceNextIdentityByMs;
        this.advanceNextIdentityByMs = 0;
        vi.setSystemTime(new Date(Date.now() + advanceBy));
      }
      return xml(
        `<GetCallerIdentityResponse><GetCallerIdentityResult><Account>${this.accountId}</Account></GetCallerIdentityResult></GetCallerIdentityResponse>`,
      );
    }
    if (action === "DescribeKeyPairs") {
      this.keyDescribeCalls += 1;
      const visibilityDelayed = this.keyDescribeCalls <= (this.options.delayedKeyVisibility ?? 0);
      if (parameters["Filter.1.Name"]) {
        const tagged = !visibilityDelayed && this.key?.runId ? keyXML(this.key) : "";
        return xml(
          `<DescribeKeyPairsResponse><keySet>${tagged}</keySet></DescribeKeyPairsResponse>`,
        );
      }
      if (!this.key) return awsError("InvalidKeyPair.NotFound");
      if (visibilityDelayed) {
        return awsError("InvalidKeyPair.NotFound");
      }
      return xml(
        `<DescribeKeyPairsResponse><keySet>${keyXML(this.key)}</keySet></DescribeKeyPairsResponse>`,
      );
    }
    if (action === "ImportKeyPair") {
      if (this.options.duplicateForeignKey) {
        this.key = {
          id: "key-foreign",
          name: String(parameters["KeyName"]),
          publicKey: "ssh-ed25519 AAAAforeign",
        };
        return awsError("InvalidKeyPair.Duplicate");
      }
      this.key = {
        id: "key-owned",
        name: String(parameters["KeyName"]),
        publicKey: atob(String(parameters["PublicKeyMaterial"])),
        runId: tagValue(parameters, "crabbox_qualification_run"),
        sha: tagValue(parameters, "crabbox_qualification_sha"),
      };
      if (this.options.loseAfterEffect === action && !this.failed) {
        this.failed = true;
        throw new Error("lost response");
      }
      if (this.options.http500AfterEffect === action && !this.failed) {
        this.failed = true;
        return awsServerError();
      }
      return xml(
        `<ImportKeyPairResponse><keyPairId>${this.key.id}</keyPairId><keyName>${this.key.name}</keyName></ImportKeyPairResponse>`,
      );
    }
    if (action === "RunInstances") {
      this.instanceState = "running";
      this.instanceOp = tagValue(parameters, "crabbox_qualification_op");
      this.terminationDescribeCalls = 0;
      if (this.options.loseAfterEffect === action && !this.failed) {
        this.failed = true;
        throw new Error("lost response");
      }
      return xml(
        "<RunInstancesResponse><instancesSet><item><instanceId>i-owned</instanceId><instanceType>t3.small</instanceType><ipAddress>203.0.113.10</ipAddress><instanceState><name>running</name></instanceState><blockDeviceMapping><item><ebs><volumeId>vol-owned</volumeId></ebs></item></blockDeviceMapping></item></instancesSet></RunInstancesResponse>",
      );
    }
    if (action === "TerminateInstances") {
      if (parameters["InstanceId.1"] === "i-unknown") this.unknownActive = false;
      else this.instanceState = "shutting-down";
      if (this.options.loseAfterEffect === action && !this.failed) {
        this.failed = true;
        throw new Error("lost response");
      }
      return xml(
        `<TerminateInstancesResponse><instancesSet><item><instanceId>${parameters["InstanceId.1"]}</instanceId><currentState><name>shutting-down</name></currentState></item></instancesSet></TerminateInstancesResponse>`,
      );
    }
    if (action === "CreateImage") {
      this.imageActive = true;
      this.snapshotActive = true;
      if (this.options.loseAfterEffect === action && !this.failed) {
        this.failed = true;
        throw new Error("lost response");
      }
      if (this.options.http500AfterEffect === action && !this.failed) {
        this.failed = true;
        return awsServerError();
      }
      return xml("<CreateImageResponse><imageId>ami-created</imageId></CreateImageResponse>");
    }
    if (action === "DescribeImages") {
      this.imageDescribeCalls += 1;
      const visible =
        this.imageActive && this.imageDescribeCalls > (this.options.delayedImageVisibility ?? 0);
      return xml(
        `<DescribeImagesResponse><imagesSet>${visible ? `<item><imageId>ami-created</imageId><tagSet><item><key>crabbox_qualification_run</key><value>${identity.runId}</value></item><item><key>crabbox_qualification_op</key><value>${imageOp(this.calls)}</value></item></tagSet><blockDeviceMapping><item><ebs><snapshotId>snap-child</snapshotId></ebs></item></blockDeviceMapping></item>` : ""}</imagesSet></DescribeImagesResponse>`,
      );
    }
    if (action === "DeregisterImage") {
      this.imageActive = false;
      if (this.options.loseAfterEffect === action && !this.failed) {
        this.failed = true;
        throw new Error("lost response");
      }
      if (this.options.deleteNotFound === action) return awsError("InvalidAMIID.NotFound");
      return xml("<DeregisterImageResponse />");
    }
    if (action === "DeleteSnapshot") {
      this.snapshotActive = false;
      if (this.options.loseAfterEffect === action && !this.failed) {
        this.failed = true;
        throw new Error("lost response");
      }
      if (this.options.deleteNotFound === action) return awsError("InvalidSnapshot.NotFound");
      return xml("<DeleteSnapshotResponse />");
    }
    if (action === "DeleteKeyPair") {
      if (this.key?.id === parameters["KeyPairId"]) this.key = undefined;
      if (this.options.loseAfterEffect === action && !this.failed) {
        this.failed = true;
        throw new Error("lost response");
      }
      if (this.options.deleteNotFound === action) return awsError("InvalidKeyPair.NotFound");
      return xml("<DeleteKeyPairResponse />");
    }
    if (action === "DescribeInstances") {
      const requested = Object.entries(parameters)
        .filter(([key]) => /^InstanceId\.\d+$/.test(key))
        .map(([, value]) => String(value));
      if (requested.includes("i-retired")) {
        return awsError("InvalidInstanceID.NotFound");
      }
      if (
        this.options.terminationDescribeNotFound &&
        requested.length === 1 &&
        requested[0] === "i-owned" &&
        this.instanceState === "shutting-down"
      ) {
        this.terminationMissingCalls += 1;
        if (this.terminationMissingCalls >= 2) this.instanceState = "absent";
        return awsError("InvalidInstanceID.NotFound");
      }
      if (parameters["InstanceId.1"] === "i-owned" && this.instanceDescribeNotFound) {
        if (this.instanceState === "shutting-down") this.instanceState = "absent";
        return awsError("InvalidInstanceID.NotFound");
      }
      if (this.instanceState === "shutting-down") {
        this.terminationDescribeCalls += 1;
        if (this.terminationDescribeCalls >= 2) this.instanceState = "terminated";
      }
      this.unknownDescribeCalls += 1;
      const unknownVisible =
        this.unknownActive &&
        this.unknownDescribeCalls > (this.options.unknownVisibilityDelay ?? 0);
      const ownedVisible = this.instanceState !== "absent";
      const items = [
        ...(unknownVisible
          ? [
              "<item><instanceId>i-unknown</instanceId><instanceState><name>running</name></instanceState></item>",
            ]
          : []),
        ...(ownedVisible
          ? [
              `<item><instanceId>i-owned</instanceId><instanceType>t3.small</instanceType><ipAddress>203.0.113.10</ipAddress><instanceState><name>${this.instanceState}</name></instanceState><tagSet><item><key>crabbox_qualification_run</key><value>${identity.runId}</value></item><item><key>crabbox_qualification_op</key><value>${this.instanceOp}</value></item></tagSet><blockDeviceMapping><item><ebs><volumeId>vol-owned</volumeId></ebs></item></blockDeviceMapping></item>`,
            ]
          : []),
      ].join("");
      return xml(
        `<DescribeInstancesResponse><reservationSet>${items ? `<item><instancesSet>${items}</instancesSet></item>` : ""}</reservationSet></DescribeInstancesResponse>`,
      );
    }
    if (action === "DescribeSnapshots") {
      return xml(
        `<DescribeSnapshotsResponse><snapshotSet>${this.snapshotActive ? "<item><snapshotId>snap-child</snapshotId></item>" : ""}</snapshotSet></DescribeSnapshotsResponse>`,
      );
    }
    if (action === "DescribeVolumes") {
      return xml(
        `<DescribeVolumesResponse><volumeSet>${this.instanceState === "running" || this.instanceState === "shutting-down" ? "<item><volumeId>vol-owned</volumeId></item>" : ""}</volumeSet></DescribeVolumesResponse>`,
      );
    }
    if (action === "DescribeSecurityGroups") {
      if (this.options.securityGroupErrorWithId) {
        return new Response(
          "<Response><Errors><Error><Message>sg-fixed is unavailable</Message></Error></Errors></Response>",
          { status: 403, headers: { "content-type": "text/xml" } },
        );
      }
      const group =
        "<item><groupId>sg-fixed</groupId></item>" +
        (this.options.duplicateSecurityGroup ? "<item><groupId>sg-fixed</groupId></item>" : "");
      return xml(
        `<DescribeSecurityGroupsResponse><securityGroupInfo>${group}</securityGroupInfo></DescribeSecurityGroupsResponse>`,
      );
    }
    return xml(`<${action}Response/>`);
  }
}

function imageOp(calls: Array<{ action: string; parameters: Record<string, unknown> }>): string {
  const create = calls.findLast((call) => call.action === "CreateImage");
  for (let index = 1; index <= 64; index += 1) {
    if (create?.parameters[`TagSpecification.1.Tag.${index}.Key`] === "crabbox_qualification_op") {
      return String(create.parameters[`TagSpecification.1.Tag.${index}.Value`]);
    }
  }
  return "";
}

function tagValue(parameters: Record<string, unknown>, key: string): string {
  for (let index = 1; index <= 64; index += 1) {
    if (parameters[`TagSpecification.1.Tag.${index}.Key`] === key) {
      return String(parameters[`TagSpecification.1.Tag.${index}.Value`] ?? "");
    }
  }
  return "";
}

function keyXML(key: {
  id: string;
  name: string;
  publicKey: string;
  runId?: string;
  sha?: string;
}): string {
  return `<item>
    <keyPairId>${key.id}</keyPairId><keyName>${key.name}</keyName>
    <publicKey>${key.publicKey}</publicKey>
    <tagSet>${key.runId ? `<item><key>crabbox_qualification_run</key><value>${key.runId}</value></item><item><key>crabbox_qualification_sha</key><value>${key.sha}</value></item>` : ""}</tagSet>
  </item>`;
}

class MemoryStorage {
  private readonly values = new Map<string, unknown>();
  alarm?: number;

  async get<T>(key: string): Promise<T | undefined> {
    return structuredClone(this.values.get(key)) as T | undefined;
  }

  async put(key: string | Record<string, unknown>, value?: unknown): Promise<void> {
    if (typeof key === "string") {
      this.values.set(key, structuredClone(value));
      return;
    }
    for (const [name, entry] of Object.entries(key)) {
      this.values.set(name, structuredClone(entry));
    }
  }

  async delete(key: string): Promise<boolean> {
    return this.values.delete(key);
  }

  async list<T>(options: { prefix: string }): Promise<Map<string, T>> {
    return new Map(
      [...this.values]
        .filter(([key]) => key.startsWith(options.prefix))
        .map(([key, value]) => [key, structuredClone(value) as T]),
    );
  }

  async setAlarm(value: number): Promise<void> {
    this.alarm = value;
  }

  async deleteAlarm(): Promise<void> {
    delete this.alarm;
  }
}

function awsError(code: string): Response {
  return new Response(
    `<Response><Errors><Error><Code>${code}</Code><Message>rejected</Message></Error></Errors></Response>`,
    { status: 400, headers: { "content-type": "text/xml" } },
  );
}

function awsServerError(): Response {
  return new Response(
    "<Response><Errors><Error><Code>InternalError</Code><Message>uncertain</Message></Error></Errors></Response>",
    { status: 500, headers: { "content-type": "text/xml" } },
  );
}

function xml(body: string): Response {
  return new Response(body, { status: 200, headers: { "content-type": "text/xml" } });
}
