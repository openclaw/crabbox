// oxlint-disable eslint/no-await-in-loop -- Each case has an isolated provider fixture.
import { expect, it, vi } from "vitest";

import { EC2SpotClient } from "../src/aws";
import { leaseConfig } from "../src/config";
import { GCPClient } from "../src/gcp";
import type { Env } from "../src/types";

it("keeps implicit root disk sizes unset until provider image resolution", () => {
  const config = leaseConfig({ sshPublicKey: "ssh-ed25519 test", provider: "aws", class: "tiny" });
  expect(config.awsRootGB).toBe(0);
  expect(config.gcpRootGB).toBe(0);
});

const classes = [
  ["tiny", 40],
  ["small", 80],
  ["standard", 150],
  ["fast", 150],
  ["large", 250],
  ["beast", 400],
] as const;

it.each(classes)(
  "sizes AWS %s roots by class and image minimum",
  async (machineClass, expected) => {
    for (const minimum of [10, 200, 500]) {
      const client = new EC2SpotClient(
        { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "test" } as Env,
        "eu-west-1",
      );
      const ec2 = vi.fn<
        (action: string, params?: Record<string, string>) => Promise<Record<string, unknown>>
      >(async (action) =>
        action === "DescribeImages"
          ? {
              imagesSet: {
                item: {
                  imageId: "ami-test",
                  rootDeviceName: "/dev/xvda",
                  blockDeviceMapping: {
                    item: [
                      { deviceName: "/dev/xvda", ebs: { volumeSize: minimum } },
                      { deviceName: "/dev/sdb", ebs: { volumeSize: 900 } },
                    ],
                  },
                },
              },
            }
          : { instancesSet: { item: { instanceId: "i-test" } } },
      );
      Reflect.set(client, "ec2", ec2);
      const config = leaseConfig({
        sshPublicKey: "ssh-ed25519 test",
        provider: "aws",
        class: machineClass,
        awsRootGB: 0,
      });
      await Reflect.get(client, "createServer").call(
        client,
        config,
        "cbx_abcdef123456",
        "test",
        "alice@example.com",
        "ami-test",
        "sg-test",
      );
      const call = ec2.mock.calls.find(([action]) => action === "RunInstances");
      const params = call![1]!;
      expect(params["BlockDeviceMapping.1.Ebs.VolumeSize"]).toBe(
        String(Math.max(expected, minimum)),
      );
      expect(params["BlockDeviceMapping.1.DeviceName"]).toBe("/dev/xvda");
    }
  },
);

it.each([
  [8, "500", 8],
  [0, "90", 90],
  [400, undefined, 400],
] as const)(
  "preserves AWS explicit request %s and operator %s",
  async (requested, operator, expected) => {
    const client = new EC2SpotClient(
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "test",
        CRABBOX_AWS_ROOT_GB: operator,
      } as Env,
      "eu-west-1",
    );
    const ec2 = vi.fn<() => Promise<Record<string, unknown>>>(async () => ({
      instancesSet: { item: { instanceId: "i-test" } },
    }));
    Reflect.set(client, "ec2", ec2);
    const config = leaseConfig({
      sshPublicKey: "ssh-ed25519 test",
      provider: "aws",
      class: "tiny",
      awsRootGB: requested,
    });
    await Reflect.get(client, "createServer").call(
      client,
      config,
      "cbx_abcdef123456",
      "test",
      "alice@example.com",
      "ami-test",
      "sg-test",
    );
    expect(ec2).toHaveBeenCalledOnce();
    expect(ec2).toHaveBeenCalledWith(
      "RunInstances",
      expect.objectContaining({ "BlockDeviceMapping.1.Ebs.VolumeSize": String(expected) }),
      expect.anything(),
    );
  },
);

it.each(classes)(
  "sizes GCP %s roots by class and source minimum",
  async (machineClass, expected) => {
    for (const snapshot of [false, true]) {
      for (const minimum of [10, 200, 500]) {
        const client = new GCPClient({
          CRABBOX_GCP_PROJECT: "test-project",
          CRABBOX_GCP_CREDENTIAL_SOURCE: "metadata",
        } as Env);
        Reflect.set(client, "ensureFirewall", async () => {});
        Reflect.set(Reflect.get(client, "tokenCache"), "cached", {
          token: "test",
          expiresAt: Math.trunc(Date.now() / 1000) + 3600,
        });
        const fetcher = vi.fn<typeof fetch>(async () =>
          Response.json({ name: "resolved-source", diskSizeGb: String(minimum) }),
        );
        client.fetcher = fetcher;
        const insert = vi.fn<() => Promise<never>>(async () => {
          throw new Error("captured insert");
        });
        Reflect.set(client, "insertInstanceAndWait", insert);
        Reflect.set(client, "rollbackDirectCreate", async () => {});
        const config = leaseConfig({
          sshPublicKey: "ssh-ed25519 test",
          provider: "gcp",
          class: machineClass,
          ...(snapshot
            ? { gcpSnapshot: "projects/source-project/global/snapshots/test" }
            : { gcpImage: "projects/source-project/global/images/family/test" }),
        });
        await expect(
          client.createServer(config, "cbx_abcdef123456", "test", "alice@example.com"),
        ).rejects.toThrow("captured insert");
        expect(fetcher.mock.calls[0]?.[0]).toBe(
          snapshot
            ? "https://compute.googleapis.com/compute/v1/projects/source-project/global/snapshots/test"
            : "https://compute.googleapis.com/compute/v1/projects/source-project/global/images/family/test",
        );
        expect(insert).toHaveBeenCalledWith(
          expect.any(String),
          expect.objectContaining({
            disks: [
              expect.objectContaining({
                initializeParams: expect.objectContaining({
                  diskSizeGb: Math.max(expected, minimum),
                  [snapshot ? "sourceSnapshot" : "sourceImage"]:
                    `projects/source-project/global/${snapshot ? "snapshots" : "images"}/resolved-source`,
                }),
              }),
            ],
          }),
          expect.anything(),
          expect.anything(),
        );
      }
    }
  },
);

it.each([
  [8, "500", 8],
  [0, "90", 90],
  [400, undefined, 400],
] as const)(
  "preserves GCP explicit request %s and operator %s for images and snapshots",
  async (requested, operator, expected) => {
    for (const snapshot of [false, true]) {
      const client = new GCPClient({
        CRABBOX_GCP_PROJECT: "test-project",
        CRABBOX_GCP_CREDENTIAL_SOURCE: "metadata",
        CRABBOX_GCP_ROOT_GB: operator,
      } as Env);
      Reflect.set(client, "ensureFirewall", async () => {});
      const fetcher = vi.fn<typeof fetch>();
      client.fetcher = fetcher;
      const insert = vi.fn<() => Promise<never>>(async () => {
        throw new Error("captured insert");
      });
      Reflect.set(client, "insertInstanceAndWait", insert);
      Reflect.set(client, "rollbackDirectCreate", async () => {});
      const config = leaseConfig({
        sshPublicKey: "ssh-ed25519 test",
        provider: "gcp",
        class: "tiny",
        gcpRootGB: requested,
        ...(snapshot ? { gcpSnapshot: "snapshot-test" } : {}),
      });
      await expect(
        client.createServer(config, "cbx_abcdef123456", "test", "alice@example.com"),
      ).rejects.toThrow("captured insert");
      expect(fetcher).not.toHaveBeenCalled();
      expect(insert).toHaveBeenCalledWith(
        expect.any(String),
        expect.objectContaining({
          disks: [
            expect.objectContaining({
              initializeParams: expect.objectContaining({ diskSizeGb: expected }),
            }),
          ],
        }),
        expect.anything(),
        expect.anything(),
      );
    }
  },
);

it("preserves GCP machine-image disks without unsupported size or type overrides", async () => {
  const client = new GCPClient({
    CRABBOX_GCP_PROJECT: "test-project",
    CRABBOX_GCP_CREDENTIAL_SOURCE: "metadata",
  } as Env);
  Reflect.set(client, "ensureFirewall", async () => {});
  const insert = vi.fn<() => Promise<never>>(async () => {
    throw new Error("captured insert");
  });
  Reflect.set(client, "insertInstanceAndWait", insert);
  Reflect.set(client, "rollbackDirectCreate", async () => {});
  await expect(
    client.createServer(
      leaseConfig({
        sshPublicKey: "ssh-ed25519 test",
        provider: "gcp",
        class: "tiny",
        gcpMachineImage: "source",
      }),
      "cbx_abcdef123456",
      "test",
      "alice@example.com",
    ),
  ).rejects.toThrow("captured insert");
  expect(insert).toHaveBeenCalledWith(
    expect.stringContaining("sourceMachineImage="),
    expect.not.objectContaining({ disks: expect.anything() }),
    expect.anything(),
    expect.anything(),
  );
});

it("refuses an automatic AWS launch when the root minimum is unavailable", async () => {
  const client = new EC2SpotClient(
    { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "test" } as Env,
    "eu-west-1",
  );
  const ec2 = vi.fn<() => Promise<Record<string, unknown>>>(async () => ({
    imagesSet: { item: { imageId: "ami-test", rootDeviceName: "/dev/sda1" } },
  }));
  Reflect.set(client, "ec2", ec2);
  await expect(
    Reflect.get(client, "createServer").call(
      client,
      leaseConfig({ provider: "aws", class: "tiny", sshPublicKey: "ssh-ed25519 test" }),
      "cbx_abcdef123456",
      "test",
      "alice@example.com",
      "ami-test",
      "sg-test",
    ),
  ).rejects.toThrow("no root EBS volume size");
  expect(ec2).toHaveBeenCalledOnce();
  expect(ec2).toHaveBeenCalledWith("DescribeImages", { "ImageId.1": "ami-test" });
});

it("refuses an automatic GCP launch when source size lookup fails", async () => {
  const client = new GCPClient({
    CRABBOX_GCP_PROJECT: "test-project",
    CRABBOX_GCP_CREDENTIAL_SOURCE: "metadata",
  } as Env);
  Reflect.set(client, "ensureFirewall", async () => {});
  Reflect.set(Reflect.get(client, "tokenCache"), "cached", {
    token: "test",
    expiresAt: Math.trunc(Date.now() / 1000) + 3600,
  });
  client.fetcher = async () => Response.json({ name: "source", diskSizeGb: "0" });
  const insert = vi.fn<() => Promise<void>>();
  Reflect.set(client, "insertInstanceAndWait", insert);
  await expect(
    client.createServer(
      leaseConfig({ provider: "gcp", class: "tiny", sshPublicKey: "ssh-ed25519 test" }),
      "cbx_abcdef123456",
      "test",
      "alice@example.com",
    ),
  ).rejects.toThrow("no name or disk size");
  expect(insert).not.toHaveBeenCalled();
});
