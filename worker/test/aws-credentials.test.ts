import { beforeEach, describe, expect, it, vi } from "vitest";

interface TestAWSCredentialIdentity {
  accessKeyId: string;
  secretAccessKey: string;
  sessionToken?: string;
  expiration?: Date;
}

const awsCredentialSDK = vi.hoisted(() => {
  const source = vi.fn<() => Promise<TestAWSCredentialIdentity>>();
  return {
    source,
    fromNodeProviderChain: vi.fn<() => typeof source>(() => source),
  };
});

vi.mock("@aws-sdk/credential-providers", () => ({
  fromNodeProviderChain: awsCredentialSDK.fromNodeProviderChain,
}));

import { nodeAWSCredentialProvider } from "../node/aws-credentials";

describe("Node AWS credentials", () => {
  beforeEach(() => {
    awsCredentialSDK.source.mockReset();
    awsCredentialSDK.fromNodeProviderChain.mockClear();
  });

  it("normalizes credentials from the Node provider chain", async () => {
    awsCredentialSDK.source.mockResolvedValueOnce({
      accessKeyId: "task-access-key",
      secretAccessKey: "task-key",
      sessionToken: "session-1",
      expiration: new Date("2030-01-01T00:00:00.000Z"),
    });

    const provider = nodeAWSCredentialProvider();

    await expect(provider()).resolves.toEqual({
      accessKeyId: "task-access-key",
      secretAccessKey: "task-key",
      sessionToken: "session-1",
    });
    expect(awsCredentialSDK.fromNodeProviderChain).toHaveBeenCalledOnce();
  });

  it("delegates every resolution so the SDK can refresh expiring credentials", async () => {
    awsCredentialSDK.source
      .mockResolvedValueOnce({
        accessKeyId: "first-access-key",
        secretAccessKey: "first-key",
        sessionToken: "session-1",
      })
      .mockResolvedValueOnce({
        accessKeyId: "refreshed-access-key",
        secretAccessKey: "next-key",
      });
    const provider = nodeAWSCredentialProvider();

    await expect(provider()).resolves.toEqual({
      accessKeyId: "first-access-key",
      secretAccessKey: "first-key",
      sessionToken: "session-1",
    });
    await expect(provider()).resolves.toEqual({
      accessKeyId: "refreshed-access-key",
      secretAccessKey: "next-key",
    });
    expect(awsCredentialSDK.source).toHaveBeenCalledTimes(2);
    expect(awsCredentialSDK.fromNodeProviderChain).toHaveBeenCalledOnce();
  });
});
