import { readFileSync } from "node:fs";

import { describe, expect, it, vi } from "vitest";

import { EC2SpotClient } from "../src/aws";
import { AWSQualificationRun, AWSQualificationTransport } from "../src/aws-qualification-authority";
import type {
  AWSQualificationRequest,
  AWSQualificationRunIdentity,
} from "../src/aws-qualification-contract";
import type { Env } from "../src/types";

const identity: AWSQualificationRunIdentity = {
  runId: "qualification-test-1",
  owner: "maintainer",
  candidateSha: "a".repeat(40),
  expiresAt: new Date(Date.now() + 60 * 60_000).toISOString(),
};
const authorityConfig = readFileSync(
  new URL("../wrangler.aws-qualification-authority.jsonc", import.meta.url),
  "utf8",
);

describe("AWS qualification authority deployment", () => {
  it("has no public route, preview URL, workers.dev URL, or cron", () => {
    expect(authorityConfig).toContain('"workers_dev": false');
    expect(authorityConfig).toContain('"preview_urls": false');
    expect(authorityConfig).not.toContain('"routes"');
    expect(authorityConfig).not.toContain('"triggers"');
    expect(authorityConfig).not.toContain('"crons"');
  });
});

describe("AWS qualification authority", () => {
  it("rejects unregistered identity, forbidden actions, and foreign resources", async () => {
    const fixture = authorityFixture();
    await fixture.run.enroll(identity);

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
  });

  it("replays receipts and reconciles an ambiguous RunInstances response", async () => {
    const fixture = authorityFixture({ failOnce: "RunInstances" });
    await fixture.run.enroll(identity);
    await fixture.run.execute(
      identity,
      request(
        "ImportKeyPair",
        {
          KeyName: "crabbox-qualification-key",
          PublicKeyMaterial: "ssh-ed25519 AAAAqualification",
        },
        "ec2",
      ),
    );
    const launch = request("RunInstances", runInstancesParams(), "ec2");

    await expect(fixture.run.execute(identity, launch)).rejects.toThrow("lost response");
    const recovered = await fixture.run.execute(identity, launch);
    expect(recovered.status).toBe(200);
    expect(recovered.body).toContain("i-owned");

    const runCalls = fixture.signer.calls.filter((call) => call.action === "RunInstances");
    expect(runCalls).toHaveLength(2);
    expect(runCalls[0]!.parameters["ClientToken"]).toBe(runCalls[1]!.parameters["ClientToken"]);
    expect(String(runCalls[0]!.parameters["ClientToken"])).toMatch(/^cbxq-[0-9a-f]{59}$/);
    expect(runCalls[0]!.parameters).not.toHaveProperty("IamInstanceProfile.Name");
    expect(runCalls[0]!.parameters).not.toHaveProperty("InstanceMarketOptions.MarketType");

    await fixture.run.execute(identity, launch);
    expect(fixture.signer.calls.filter((call) => call.action === "RunInstances")).toHaveLength(2);
    await fixture.run.execute(identity, { ...launch, opId: crypto.randomUUID() });
    const lifecycleReplay = fixture.signer.calls.filter((call) => call.action === "RunInstances");
    expect(lifecycleReplay).toHaveLength(3);
    expect(lifecycleReplay[2]!.parameters["ClientToken"]).toBe(
      lifecycleReplay[0]!.parameters["ClientToken"],
    );
    await expect(
      fixture.run.execute(identity, {
        ...launch,
        parameters: { ...launch.parameters, InstanceType: "t3a.small" },
      }),
    ).rejects.toThrow("different request");
  });

  it("recovers a lost CreateImage response from authority-injected operation tags", async () => {
    const fixture = authorityFixture({ failOnce: "CreateImage" });
    await fixture.run.enroll(identity);
    await fixture.run.execute(
      identity,
      request(
        "ImportKeyPair",
        {
          KeyName: "crabbox-qualification-key",
          PublicKeyMaterial: "ssh-ed25519 AAAAqualification",
        },
        "ec2",
      ),
    );
    await fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2"));
    const create = request(
      "CreateImage",
      {
        InstanceId: "i-owned",
        Name: "crabbox-qualification-image",
        NoReboot: "true",
      },
      "ec2",
    );

    await expect(fixture.run.execute(identity, create)).rejects.toThrow("lost response");
    const recovered = await fixture.run.execute(identity, create);
    expect(recovered.body).toContain("ami-created");
    expect(fixture.signer.calls.filter((call) => call.action === "CreateImage")).toHaveLength(1);
    const reconcile = fixture.signer.calls.find(
      (call) =>
        call.action === "DescribeImages" &&
        call.parameters["Filter.2.Name"] === "tag:crabbox_qualification_op",
    );
    expect(reconcile?.parameters["Filter.1.Value.1"]).toBe(identity.runId);
  });

  it("rejects privileged launch shapes before AWS is called", async () => {
    const fixture = authorityFixture();
    await fixture.run.enroll(identity);
    await fixture.run.execute(
      identity,
      request(
        "ImportKeyPair",
        {
          KeyName: "crabbox-qualification-key",
          PublicKeyMaterial: "ssh-ed25519 AAAAqualification",
        },
        "ec2",
      ),
    );
    await expect(
      fixture.run.execute(
        identity,
        request(
          "RunInstances",
          {
            ...runInstancesParams(),
            "IamInstanceProfile.Name": "admin",
          },
          "ec2",
        ),
      ),
    ).rejects.toThrow("forbidden");
    expect(fixture.signer.calls.some((call) => call.action === "RunInstances")).toBe(false);
  });
});

describe("AWS qualification candidate transport", () => {
  it("fails closed when its service binding is unavailable", async () => {
    const execute = vi
      .fn<(request: AWSQualificationRequest) => Promise<{ status: number; body: string }>>()
      .mockRejectedValue(new Error("binding unavailable"));
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

  it("takes run identity only from immutable entrypoint props", async () => {
    const execute = vi
      .fn<
        (
          identity: AWSQualificationRunIdentity,
          request: AWSQualificationRequest,
        ) => Promise<{ status: number; body: string }>
      >()
      .mockResolvedValue({ status: 200, body: "ok" });
    const namespace = {
      idFromName: vi.fn<(name: string) => string>((name) => name),
      get: vi.fn<(id: string) => { execute: typeof execute }>(() => ({ execute })),
    };
    const transport = new AWSQualificationTransport(
      { AWS_QUALIFICATION_RUNS: namespace } as never,
      identity,
    );

    await transport.execute(request("GetCallerIdentity"));
    expect(namespace.idFromName).toHaveBeenCalledWith(identity.runId);
    expect(execute).toHaveBeenCalledWith(identity, expect.any(Object));
  });
});

function authorityFixture(options: { failOnce?: string } = {}) {
  const storage = new MemoryStorage();
  const signer = new FakeSigner(options.failOnce);
  const env = {
    CRABBOX_AWS_QUALIFICATION_ACCOUNT_ID: "123456789012",
    CRABBOX_AWS_QUALIFICATION_BASE_AMI_ID: "ami-base",
    CRABBOX_AWS_QUALIFICATION_REGION: "us-east-1",
    CRABBOX_AWS_QUALIFICATION_ROOT_GB: "20",
    CRABBOX_AWS_QUALIFICATION_SECURITY_GROUP_ID: "sg-fixed",
    CRABBOX_AWS_QUALIFICATION_SUBNET_ID: "subnet-fixed",
  };
  const run = new AWSQualificationRun({ storage } as never, env as never, signer);
  return { run, signer, storage };
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
    KeyName: "crabbox-qualification-key",
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

  constructor(private readonly failOnce?: string) {}

  async execute(
    service: string,
    action: string,
    region: string,
    parameters: Record<string, unknown>,
  ): Promise<Response> {
    this.calls.push({ service, action, region, parameters: structuredClone(parameters) });
    if (action === this.failOnce && !this.failed) {
      this.failed = true;
      throw new Error("lost response");
    }
    if (action === "GetCallerIdentity") {
      return xml(
        "<GetCallerIdentityResponse><GetCallerIdentityResult><Account>123456789012</Account></GetCallerIdentityResult></GetCallerIdentityResponse>",
      );
    }
    if (action === "ImportKeyPair") {
      return xml(
        "<ImportKeyPairResponse><keyPairId>key-owned</keyPairId><keyName>crabbox-qualification-key</keyName></ImportKeyPairResponse>",
      );
    }
    if (action === "RunInstances") {
      return xml(
        "<RunInstancesResponse><instancesSet><item><instanceId>i-owned</instanceId><instanceType>t3.small</instanceType><keyName>crabbox-qualification-key</keyName><instanceState><name>running</name></instanceState><blockDeviceMapping><item><ebs><volumeId>vol-owned</volumeId></ebs></item></blockDeviceMapping></item></instancesSet></RunInstancesResponse>",
      );
    }
    if (action === "DescribeImages") {
      return xml(
        "<DescribeImagesResponse><imagesSet><item><imageId>ami-created</imageId><imageState>available</imageState></item></imagesSet></DescribeImagesResponse>",
      );
    }
    if (action === "CreateImage") {
      return xml("<CreateImageResponse><imageId>ami-created</imageId></CreateImageResponse>");
    }
    return xml(`<${action}Response/>`);
  }
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

function xml(body: string): Response {
  return new Response(body, { status: 200, headers: { "content-type": "text/xml" } });
}
