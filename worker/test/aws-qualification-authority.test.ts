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
import type { Env } from "../src/types";

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

  it("serializes concurrent launches and enforces two total sequential launch attempts", async () => {
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

    await fixture.run.execute(
      identity,
      request("TerminateInstances", { "InstanceId.1": "i-owned" }, "ec2"),
    );
    await fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2"));
    await fixture.run.execute(
      identity,
      request("TerminateInstances", { "InstanceId.1": "i-owned" }, "ec2"),
    );
    await expect(
      fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2")),
    ).rejects.toThrow("launch budget");
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

  it("never adopts or deletes a duplicate foreign physical key", async () => {
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
    const now = new Date("2026-09-03T12:00:00Z");
    vi.setSystemTime(now);
    const expiring = { ...identity, expiresAt: new Date(now.getTime() + 1_000).toISOString() };
    const fixture = authorityFixture({ unknownTaggedInstance: true });
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
    duplicateForeignKey?: boolean;
    failOnce?: string;
    unknownTaggedInstance?: boolean;
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

async function importKey(fixture: ReturnType<typeof authorityFixture>): Promise<void> {
  await fixture.run.execute(
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
  private failed = false;
  private key?: { id: string; name: string; publicKey: string; runId?: string; sha?: string };
  private instanceActive = false;
  private imageActive = false;
  private snapshotActive = false;
  private unknownActive: boolean;

  constructor(
    private readonly options: {
      duplicateForeignKey?: boolean;
      failOnce?: string;
      unknownTaggedInstance?: boolean;
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
      return xml(
        "<GetCallerIdentityResponse><GetCallerIdentityResult><Account>123456789012</Account></GetCallerIdentityResult></GetCallerIdentityResponse>",
      );
    }
    if (action === "DescribeKeyPairs") {
      if (parameters["Filter.1.Name"]) {
        return xml("<DescribeKeyPairsResponse><keySet /></DescribeKeyPairsResponse>");
      }
      if (!this.key) return awsError("InvalidKeyPair.NotFound");
      return xml(`<DescribeKeyPairsResponse><keySet><item>
        <keyPairId>${this.key.id}</keyPairId><keyName>${this.key.name}</keyName>
        <publicKey>${this.key.publicKey}</publicKey>
        <tagSet>${this.key.runId ? `<item><key>crabbox_qualification_run</key><value>${this.key.runId}</value></item><item><key>crabbox_qualification_sha</key><value>${this.key.sha}</value></item>` : ""}</tagSet>
      </item></keySet></DescribeKeyPairsResponse>`);
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
      return xml(
        `<ImportKeyPairResponse><keyPairId>${this.key.id}</keyPairId><keyName>${this.key.name}</keyName></ImportKeyPairResponse>`,
      );
    }
    if (action === "RunInstances") {
      this.instanceActive = true;
      return xml(
        "<RunInstancesResponse><instancesSet><item><instanceId>i-owned</instanceId><instanceState><name>running</name></instanceState><blockDeviceMapping><item><ebs><volumeId>vol-owned</volumeId></ebs></item></blockDeviceMapping></item></instancesSet></RunInstancesResponse>",
      );
    }
    if (action === "TerminateInstances") {
      if (parameters["InstanceId.1"] === "i-unknown") this.unknownActive = false;
      else this.instanceActive = false;
      return xml(
        `<TerminateInstancesResponse><instancesSet><item><instanceId>${parameters["InstanceId.1"]}</instanceId><currentState><name>shutting-down</name></currentState></item></instancesSet></TerminateInstancesResponse>`,
      );
    }
    if (action === "CreateImage") {
      this.imageActive = true;
      this.snapshotActive = true;
      return xml("<CreateImageResponse><imageId>ami-created</imageId></CreateImageResponse>");
    }
    if (action === "DescribeImages") {
      const visible = this.imageActive;
      return xml(
        `<DescribeImagesResponse><imagesSet>${visible ? `<item><imageId>ami-created</imageId><tagSet><item><key>crabbox_qualification_run</key><value>${identity.runId}</value></item><item><key>crabbox_qualification_op</key><value>${imageOp(this.calls)}</value></item></tagSet><blockDeviceMapping><item><ebs><snapshotId>snap-child</snapshotId></ebs></item></blockDeviceMapping></item>` : ""}</imagesSet></DescribeImagesResponse>`,
      );
    }
    if (action === "DeregisterImage") {
      this.imageActive = false;
      return xml("<DeregisterImageResponse />");
    }
    if (action === "DeleteSnapshot") {
      this.snapshotActive = false;
      return xml("<DeleteSnapshotResponse />");
    }
    if (action === "DeleteKeyPair") {
      if (this.key?.id === parameters["KeyPairId"]) this.key = undefined;
      return xml("<DeleteKeyPairResponse />");
    }
    if (action === "DescribeInstances") {
      const item = this.unknownActive
        ? "<item><instanceId>i-unknown</instanceId><instanceState><name>running</name></instanceState></item>"
        : this.instanceActive
          ? "<item><instanceId>i-owned</instanceId><instanceState><name>running</name></instanceState></item>"
          : "";
      return xml(
        `<DescribeInstancesResponse><reservationSet>${item ? `<item><instancesSet>${item}</instancesSet></item>` : ""}</reservationSet></DescribeInstancesResponse>`,
      );
    }
    if (action === "DescribeSnapshots") {
      return xml(
        `<DescribeSnapshotsResponse><snapshotSet>${this.snapshotActive ? "<item><snapshotId>snap-child</snapshotId></item>" : ""}</snapshotSet></DescribeSnapshotsResponse>`,
      );
    }
    if (action === "DescribeVolumes") {
      return xml(
        `<DescribeVolumesResponse><volumeSet>${this.instanceActive ? "<item><volumeId>vol-owned</volumeId></item>" : ""}</volumeSet></DescribeVolumesResponse>`,
      );
    }
    if (action === "DescribeSecurityGroups") {
      return xml(
        "<DescribeSecurityGroupsResponse><securityGroupInfo><item><groupId>sg-fixed</groupId></item></securityGroupInfo></DescribeSecurityGroupsResponse>",
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

function xml(body: string): Response {
  return new Response(body, { status: 200, headers: { "content-type": "text/xml" } });
}
