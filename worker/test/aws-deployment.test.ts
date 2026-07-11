import { describe, expect, it, vi } from "vitest";

import {
  createAWSDeploymentGuard,
  nodeCoordinatorEnv,
  privateWorkspacePreflightConfig,
  requiresAWSDeploymentReadiness,
  type AWSDeploymentGuardDependencies,
} from "../node/aws-deployment";
import type { AWSPrivateWorkspaceConfig } from "../src/aws";
import type { AWSCredentialProvider, Env } from "../src/types";

const expectedAccountID = "123456789012";
const expectedRegion = "us-west-2";
const privateBearer = "x".repeat(20);
type Preflight = NonNullable<AWSDeploymentGuardDependencies["preflight"]>;

describe("Node AWS deployment guard", () => {
  it("gates only private workspace creation on deployment readiness", () => {
    expect(
      requiresAWSDeploymentReadiness(
        new Request("https://coordinator.test/v1/workspaces", { method: "POST" }),
      ),
    ).toBe(true);
    for (const [method, path] of [
      ["GET", "/v1/workspaces/example"],
      ["DELETE", "/v1/workspaces/example"],
      ["POST", "/v1/leases"],
      ["POST", "/v1/adapters/example/proxy/v1/workspaces"],
    ]) {
      expect(
        requiresAWSDeploymentReadiness(new Request(`https://coordinator.test${path}`, { method })),
      ).toBe(false);
    }
  });

  it("preserves public Node startup behavior when the ECS guard is disabled", async () => {
    const fetchImpl = vi.fn<typeof fetch>();
    const preflight = vi.fn<Preflight>();
    const guard = createAWSDeploymentGuard(
      {
        AWS_ACCESS_KEY_ID: "legacy-access-key",
        AWS_SECRET_ACCESS_KEY: "legacy",
      } as Env,
      { fetch: fetchImpl, preflight },
    );

    await guard.start();
    await guard.ready();

    expect(fetchImpl).not.toHaveBeenCalled();
    expect(preflight).not.toHaveBeenCalled();
  });

  it("injects the refreshable Node credential provider without mutating source values", async () => {
    const provider = vi.fn<AWSCredentialProvider>(async () => ({
      accessKeyId: "task-access-key",
      secretAccessKey: "task-key",
    }));
    const source = { DATABASE_URL: "postgres://coordinator.test/crabbox" };

    const env = nodeCoordinatorEnv(source, provider);

    expect(env.DATABASE_URL).toBe(source.DATABASE_URL);
    expect(env.awsCredentialProvider).toBe(provider);
    expect(source).not.toHaveProperty("awsCredentialProvider");
  });

  it.each([
    { requireECSTask: "1", privateWorkspaces: "0" },
    { requireECSTask: "0", privateWorkspaces: "1" },
  ])(
    "rejects static AWS credentials with ECS=$requireECSTask private=$privateWorkspaces",
    async ({ requireECSTask, privateWorkspaces }) => {
      const fetchImpl = vi.fn<typeof fetch>();
      const guard = createAWSDeploymentGuard(
        privateEnv({
          CRABBOX_AWS_REQUIRE_ECS_TASK: requireECSTask,
          CRABBOX_WORKSPACE_AWS_PRIVATE: privateWorkspaces,
          AWS_ACCESS_KEY_ID: "forbidden-static-key",
        }),
        { fetch: fetchImpl, preflight: vi.fn<Preflight>() },
      );

      await expect(guard.start()).rejects.toThrow(
        "AWS deployment guard failed: static_credentials_forbidden",
      );
      expect(fetchImpl).not.toHaveBeenCalled();
    },
  );

  it.each([
    { CRABBOX_RUNTIME_ADAPTER_OWNER: "" },
    { CRABBOX_RUNTIME_ADAPTER_OWNER: " owner@example.com" },
    { CRABBOX_RUNTIME_ADAPTER_ORG: "" },
    { CRABBOX_RUNTIME_ADAPTER_ORG: " example-org" },
    { CRABBOX_RUNTIME_ADAPTER_ORG: "x".repeat(64) },
  ])("fails startup on an invalid runtime adapter identity", (override) => {
    expect(() =>
      createAWSDeploymentGuard(privateEnv(override), {
        preflight: vi.fn<Preflight>(),
      }),
    ).toThrow("AWS deployment guard failed: runtime_identity_invalid");
  });

  it.each([undefined, "", "hetzner"])(
    "fails startup when private workspace provider is %s",
    (provider) => {
      expect(() =>
        createAWSDeploymentGuard(privateEnv({ CRABBOX_WORKSPACE_PROVIDER: provider }), {
          preflight: vi.fn<Preflight>(),
        }),
      ).toThrow("AWS deployment guard failed: private_provider_invalid");
    },
  );

  it("requires the exact expected task role for private deployments", () => {
    expect(() =>
      createAWSDeploymentGuard(privateEnv({ CRABBOX_AWS_EXPECTED_TASK_ROLE_NAME: undefined }), {
        preflight: vi.fn<Preflight>(),
      }),
    ).toThrow("AWS deployment guard failed: expected_task_role_required");
  });

  it.each([undefined, "", "short", "runtime token value", "runtime-token\nvalue"])(
    "fails startup when runtime adapter token is invalid",
    (token) => {
      expect(() =>
        createAWSDeploymentGuard(privateEnv({ CRABBOX_RUNTIME_ADAPTER_TOKEN: token }), {
          preflight: vi.fn<Preflight>(),
        }),
      ).toThrow("AWS deployment guard failed: runtime_auth_required");
    },
  );

  it("requires ECS task-role and metadata-v4 environment", async () => {
    const missingCredentials = privateEnv();
    const missingMetadata = privateEnv();
    delete missingCredentials.AWS_CONTAINER_CREDENTIALS_RELATIVE_URI;
    delete missingMetadata.ECS_CONTAINER_METADATA_URI_V4;

    await expect(
      createAWSDeploymentGuard(missingCredentials, { preflight: vi.fn<Preflight>() }).start(),
    ).rejects.toThrow("AWS deployment guard failed: container_credentials_required");
    await expect(
      createAWSDeploymentGuard(missingMetadata, { preflight: vi.fn<Preflight>() }).start(),
    ).rejects.toThrow("AWS deployment guard failed: metadata_endpoint_invalid");
  });

  it.each([
    "https://metadata.example.test/v4/task-id",
    "http://metadata.example.test/v4/task-id",
    "http://localhost/v4/task-id",
    "http://169.254.170.3/v4/task-id",
    "http://169.254.170.2:8080/v4/task-id",
    "http://169.254.170.2/not-v4/task-id",
  ])("rejects non-local ECS metadata endpoint %s without requesting it", async (endpoint) => {
    const fetchImpl = vi.fn<typeof fetch>();
    const guard = createAWSDeploymentGuard(
      privateEnv({ ECS_CONTAINER_METADATA_URI_V4: endpoint }),
      { fetch: fetchImpl, preflight: vi.fn<Preflight>() },
    );

    await expect(guard.start()).rejects.toThrow(
      "AWS deployment guard failed: metadata_endpoint_invalid",
    );
    expect(fetchImpl).not.toHaveBeenCalled();
  });

  it.each([
    {
      name: "account",
      metadata: taskMetadata({
        TaskARN: "arn:aws:ecs:us-west-2:999999999999:task/cluster/task-id",
      }),
    },
    {
      name: "region",
      metadata: taskMetadata({
        TaskARN: "arn:aws:ecs:eu-west-1:123456789012:task/cluster/task-id",
      }),
    },
    {
      name: "availability zone",
      metadata: taskMetadata({ AvailabilityZone: "eu-west-1a" }),
    },
    {
      name: "lookalike availability zone",
      metadata: taskMetadata({ AvailabilityZone: "us-west-20a" }),
    },
    {
      name: "launch type",
      metadata: taskMetadata({ LaunchType: "EC2" }),
    },
  ])(
    "fails closed on ECS task metadata $name mismatch without exposing values",
    async ({ metadata }) => {
      const guard = createAWSDeploymentGuard(privateEnv(), {
        fetch: async () => Response.json(metadata),
        preflight: vi.fn<Preflight>(),
      });

      const error = await guard.start().catch((caught: unknown) => caught);

      expect(error).toBeInstanceOf(Error);
      expect((error as Error).message).toBe("AWS deployment guard failed: metadata_mismatch");
      expect((error as Error).message).not.toContain(expectedAccountID);
      expect((error as Error).message).not.toContain(expectedRegion);
      expect((error as Error).message).not.toContain("999999999999");
      expect((error as Error).message).not.toContain("eu-west-1");
    },
  );

  it("rejects ECS task metadata from an unsupported AWS partition", async () => {
    const govRegion = "us-gov-west-1";
    const guard = createAWSDeploymentGuard(privateEnv({ CRABBOX_AWS_EXPECTED_REGION: govRegion }), {
      fetch: async () =>
        Response.json({
          TaskARN: `arn:aws-us-gov:ecs:${govRegion}:${expectedAccountID}:task/cluster/task-id`,
          AvailabilityZone: `${govRegion}a`,
          LaunchType: "FARGATE",
        }),
      preflight: vi.fn<Preflight>(),
    });

    await expect(guard.start()).rejects.toThrow("AWS deployment guard failed: metadata_mismatch");
  });

  it("runs the private permission preflight and caches successful readiness checks", async () => {
    let now = 1_000;
    const fetchImpl = vi.fn<() => Promise<Response>>(async () => Response.json(taskMetadata()));
    const preflight = vi.fn<Preflight>(async () => {});
    const env = privateEnv({ CRABBOX_AWS_AMI: "ami-legacy-override" });
    const guard = createAWSDeploymentGuard(env, {
      fetch: fetchImpl,
      now: () => now,
      readinessCacheMs: 60_000,
      preflight,
    });

    await guard.start();
    await guard.ready();

    expect(fetchImpl).toHaveBeenCalledTimes(1);
    expect(preflight).toHaveBeenCalledTimes(1);
    expect(preflight).toHaveBeenCalledWith(
      env,
      expect.objectContaining({
        provider: "aws",
        awsPrivate: true,
        awsRequireSSM: true,
        awsUseStockImage: true,
        awsRegion: expectedRegion,
        awsRootGB: 20,
        awsInstanceTypes: ["t3a.small", "t3.small"],
        capacityMarket: "on-demand",
        capacityFallback: "none",
        sshUser: "crabbox",
        sshPublicKey: "",
      }),
      expect.objectContaining({
        accountID: expectedAccountID,
        region: expectedRegion,
        securityGroupID: "sg-workspace1",
      }),
    );

    now += 60_001;
    await guard.ready();

    expect(fetchImpl).toHaveBeenCalledTimes(2);
    expect(preflight).toHaveBeenCalledTimes(2);
  });

  it("reports a generic readiness failure and retries it", async () => {
    let now = 1_000;
    const preflight = vi
      .fn<() => Promise<void>>()
      .mockResolvedValueOnce()
      .mockRejectedValueOnce(new Error(`wrong account ${expectedAccountID}`))
      .mockResolvedValueOnce();
    const guard = createAWSDeploymentGuard(privateEnv(), {
      fetch: async () => Response.json(taskMetadata()),
      now: () => now,
      readinessCacheMs: 60_000,
      preflight,
    });
    await guard.start();
    now += 60_001;

    await expect(guard.ready()).rejects.toThrow(
      "AWS deployment guard failed: private_preflight_failed",
    );
    await expect(guard.ready()).resolves.toBeUndefined();
    expect(preflight).toHaveBeenCalledTimes(3);
  });

  it("builds the preflight lease from the exact private policy", () => {
    const policy: AWSPrivateWorkspaceConfig = {
      accountID: expectedAccountID,
      region: expectedRegion,
      instanceTypes: ["t3a.small"],
      maxVCPUs: 2,
      maxMemoryMiB: 4096,
      rootGB: 20,
      subnetID: "subnet-private1",
      securityGroupID: "sg-workspace1",
      controllerSecurityGroupID: "sg-controller1",
      instanceProfile: "crabbox-workspace",
      market: "on-demand",
      ssmLogGroup: "/crabbox/workspaces",
    };

    expect(privateWorkspacePreflightConfig(policy)).toMatchObject({
      serverType: "t3a.small",
      serverTypeExplicit: true,
      awsSubnetID: policy.subnetID,
      awsSGID: policy.securityGroupID,
      awsProfile: policy.instanceProfile,
      awsUseStockImage: true,
      awsSSMLogGroup: policy.ssmLogGroup,
      awsSSHCIDRs: [],
      awsSSHCIDRsPinned: true,
      capacityRegions: [expectedRegion],
      capacityHints: false,
    });
  });
});

function privateEnv(overrides: Partial<Env> = {}): Env {
  return {
    CRABBOX_AWS_REQUIRE_ECS_TASK: "1",
    CRABBOX_WORKSPACE_AWS_PRIVATE: "1",
    CRABBOX_WORKSPACE_PROVIDER: "aws",
    CRABBOX_AWS_EXPECTED_ACCOUNT_ID: expectedAccountID,
    CRABBOX_AWS_EXPECTED_REGION: expectedRegion,
    CRABBOX_AWS_EXPECTED_TASK_ROLE_NAME: "crabbox-coordinator-task",
    CRABBOX_WORKSPACE_AWS_INSTANCE_TYPES: "t3a.small,t3.small",
    CRABBOX_WORKSPACE_AWS_MAX_VCPUS: "2",
    CRABBOX_WORKSPACE_AWS_MAX_MEMORY_MIB: "4096",
    CRABBOX_WORKSPACE_AWS_ROOT_GB: "20",
    CRABBOX_WORKSPACE_AWS_SUBNET_ID: "subnet-private1",
    CRABBOX_WORKSPACE_AWS_SECURITY_GROUP_ID: "sg-workspace1",
    CRABBOX_WORKSPACE_AWS_CONTROLLER_SECURITY_GROUP_ID: "sg-controller1",
    CRABBOX_WORKSPACE_AWS_INSTANCE_PROFILE: "crabbox-workspace",
    CRABBOX_WORKSPACE_AWS_MARKET: "on-demand",
    CRABBOX_WORKSPACE_AWS_SSM_LOG_GROUP: "/crabbox/workspaces",
    CRABBOX_RUNTIME_ADAPTER_OWNER: "controller@example.com",
    CRABBOX_RUNTIME_ADAPTER_ORG: "example-org",
    CRABBOX_RUNTIME_ADAPTER_TOKEN: privateBearer,
    AWS_CONTAINER_CREDENTIALS_RELATIVE_URI: "/v2/credentials/task-role-id",
    ECS_CONTAINER_METADATA_URI_V4: "http://169.254.170.2/v4/container-id",
    awsCredentialProvider: async () => ({
      accessKeyId: "task-access-key",
      secretAccessKey: "task-key",
      sessionToken: "session-1",
    }),
    ...overrides,
  } as Env;
}

function taskMetadata(overrides: Record<string, string> = {}): Record<string, string> {
  return {
    TaskARN: `arn:aws:ecs:${expectedRegion}:${expectedAccountID}:task/cluster/task-id`,
    AvailabilityZone: `${expectedRegion}a`,
    LaunchType: "FARGATE",
    ...overrides,
  };
}
