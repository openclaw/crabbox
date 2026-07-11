import {
  EC2SpotClient,
  awsExpectedIdentityConfig,
  awsPrivateWorkspaceConfig,
  type AWSPrivateWorkspaceConfig,
} from "../src/aws";
import { leaseConfig, type LeaseConfig } from "../src/config";
import { orgKeyForLabel } from "../src/org-identity";
import type { AWSCredentialProvider, Env } from "../src/types";
import { nodeAWSCredentialProvider } from "./aws-credentials";

const defaultReadinessCacheMs = 60_000;
const metadataTimeoutMs = 5_000;

export interface AWSDeploymentGuard {
  start(): Promise<void>;
  ready(): Promise<void>;
}

export interface AWSDeploymentGuardDependencies {
  fetch?: typeof globalThis.fetch;
  now?: () => number;
  readinessCacheMs?: number;
  preflight?: (env: Env, config: LeaseConfig, policy: AWSPrivateWorkspaceConfig) => Promise<void>;
}

export function requiresAWSDeploymentReadiness(request: Request): boolean {
  const path = new URL(request.url).pathname.split("/").filter(Boolean);
  return (
    request.method === "POST" && path.length === 2 && path[0] === "v1" && path[1] === "workspaces"
  );
}

interface ECSTaskMetadata {
  TaskARN?: unknown;
  AvailabilityZone?: unknown;
  LaunchType?: unknown;
}

export function nodeCoordinatorEnv(
  source: NodeJS.ProcessEnv,
  credentialProvider: AWSCredentialProvider = nodeAWSCredentialProvider(),
): Env {
  return { ...source, awsCredentialProvider: credentialProvider } as unknown as Env;
}

export function createAWSDeploymentGuard(
  env: Env,
  dependencies: AWSDeploymentGuardDependencies = {},
): AWSDeploymentGuard {
  const requireECSTask = deploymentFlag(
    env.CRABBOX_AWS_REQUIRE_ECS_TASK,
    "CRABBOX_AWS_REQUIRE_ECS_TASK",
  );
  const privateWorkspaces = deploymentFlag(
    env.CRABBOX_WORKSPACE_AWS_PRIVATE,
    "CRABBOX_WORKSPACE_AWS_PRIVATE",
  );
  if (!requireECSTask && !privateWorkspaces) {
    return disabledDeploymentGuard;
  }

  const expected = awsExpectedIdentityConfig(env);
  if (!expected) {
    throw deploymentError("expected_identity_required");
  }
  if (privateWorkspaces && !expected.taskRoleName) {
    throw deploymentError("expected_task_role_required");
  }
  const policy = privateWorkspaces ? awsPrivateWorkspaceConfig(env) : undefined;
  if (privateWorkspaces && !policy) {
    throw deploymentError("private_policy_required");
  }
  if (privateWorkspaces) {
    if (env.CRABBOX_WORKSPACE_PROVIDER?.trim() !== "aws") {
      throw deploymentError("private_provider_invalid");
    }
    const bearer = env.CRABBOX_RUNTIME_ADAPTER_TOKEN ?? "";
    if (bearer.length < 16 || bearer.length > 8192 || /\s/u.test(bearer)) {
      throw deploymentError("runtime_auth_required");
    }
    verifyRuntimeAdapterIdentity(env);
  }

  const fetchImpl = dependencies.fetch ?? globalThis.fetch;
  const now = dependencies.now ?? Date.now;
  const readinessCacheMs = dependencies.readinessCacheMs ?? defaultReadinessCacheMs;
  const preflight =
    dependencies.preflight ??
    (async (preflightEnv, config, preflightPolicy) => {
      await new EC2SpotClient(preflightEnv, preflightPolicy.region).privateWorkspacePreflight(
        config,
        preflightPolicy,
      );
    });
  const config = policy ? privateWorkspacePreflightConfig(policy) : undefined;

  let lastSuccess = Number.NEGATIVE_INFINITY;
  let pending: Promise<void> | undefined;

  const verify = async (force: boolean): Promise<void> => {
    if (!force && now() - lastSuccess < readinessCacheMs) return;
    pending ??= (async () => {
      rejectStaticCredentials(env);
      await verifyECSTaskMetadata(env, expected.accountID, expected.region, fetchImpl);
      if (policy && config) {
        try {
          await preflight(env, config, policy);
        } catch {
          throw deploymentError("private_preflight_failed");
        }
      }
      lastSuccess = now();
    })().finally(() => {
      pending = undefined;
    });
    await pending;
  };

  return {
    start: () => verify(true),
    ready: () => verify(false),
  };
}

export function privateWorkspacePreflightConfig(policy: AWSPrivateWorkspaceConfig): LeaseConfig {
  const config = leaseConfig({
    provider: "aws",
    target: "linux",
    architecture: "amd64",
    serverType: policy.instanceTypes[0]!,
    serverTypeExplicit: true,
    awsRegion: policy.region,
    awsSGID: policy.securityGroupID,
    awsSubnetID: policy.subnetID,
    awsProfile: policy.instanceProfile,
    awsRootGB: policy.rootGB,
    awsInstanceTypes: policy.instanceTypes,
    awsPrivate: true,
    awsRequireSSM: true,
    awsSSMBootstrapCommand: "true",
    awsSSMLogGroup: policy.ssmLogGroup,
    awsSSHCIDRs: [],
    awsSSHCIDRsPinned: true,
    capacity: {
      market: policy.market,
      fallback: "none",
      regions: [policy.region],
      hints: false,
    },
    sshUser: "crabbox",
    sshPublicKey: "",
  });
  config.awsUseStockImage = true;
  return config;
}

async function verifyECSTaskMetadata(
  env: Env,
  expectedAccountID: string,
  expectedRegion: string,
  fetchImpl: typeof globalThis.fetch,
): Promise<void> {
  const credentialsPath = env.AWS_CONTAINER_CREDENTIALS_RELATIVE_URI?.trim() ?? "";
  if (!/^\/v2\/credentials\/[A-Za-z0-9_-]+$/.test(credentialsPath)) {
    throw deploymentError("container_credentials_required");
  }
  const metadataBase = validMetadataBase(env.ECS_CONTAINER_METADATA_URI_V4);
  let response: Response;
  try {
    response = await fetchImpl(`${metadataBase}/task`, {
      headers: { accept: "application/json" },
      signal: AbortSignal.timeout(metadataTimeoutMs),
    });
  } catch {
    throw deploymentError("metadata_unavailable");
  }
  if (!response.ok) {
    throw deploymentError("metadata_unavailable");
  }
  let metadata: ECSTaskMetadata;
  try {
    metadata = (await response.json()) as ECSTaskMetadata;
  } catch {
    throw deploymentError("metadata_invalid");
  }
  const taskARN = typeof metadata.TaskARN === "string" ? metadata.TaskARN : "";
  const availabilityZone =
    typeof metadata.AvailabilityZone === "string" ? metadata.AvailabilityZone : "";
  const launchType = typeof metadata.LaunchType === "string" ? metadata.LaunchType : "";
  if (
    !taskARNMatchesIdentity(taskARN, expectedAccountID, expectedRegion) ||
    !availabilityZoneMatchesRegion(availabilityZone, expectedRegion) ||
    launchType !== "FARGATE"
  ) {
    throw deploymentError("metadata_mismatch");
  }
}

function taskARNMatchesIdentity(taskARN: string, accountID: string, region: string): boolean {
  const match = /^arn:aws:ecs:([^:]+):(\d{12}):task\/.+$/.exec(taskARN);
  return match?.[1] === region && match[2] === accountID;
}

function availabilityZoneMatchesRegion(availabilityZone: string, region: string): boolean {
  if (!availabilityZone.startsWith(region)) return false;
  const suffix = availabilityZone.slice(region.length);
  return /^[a-z]$/.test(suffix) || /^-[a-z0-9-]+$/.test(suffix);
}

function validMetadataBase(value: string | undefined): string {
  const normalized = value?.trim() ?? "";
  let url: URL;
  try {
    url = new URL(normalized);
  } catch {
    throw deploymentError("metadata_endpoint_invalid");
  }
  if (
    url.protocol !== "http:" ||
    url.username ||
    url.password ||
    url.search ||
    url.hash ||
    url.hostname !== "169.254.170.2" ||
    url.port ||
    !/^\/v4\/[A-Za-z0-9_-]+\/?$/.test(url.pathname)
  ) {
    throw deploymentError("metadata_endpoint_invalid");
  }
  return url.href.replace(/\/$/, "");
}

function rejectStaticCredentials(env: Env): void {
  if (
    env.AWS_ACCESS_KEY_ID?.trim() ||
    env.AWS_SECRET_ACCESS_KEY?.trim() ||
    env.AWS_SESSION_TOKEN?.trim()
  ) {
    throw deploymentError("static_credentials_forbidden");
  }
}

function verifyRuntimeAdapterIdentity(env: Env): void {
  const owner = env.CRABBOX_RUNTIME_ADAPTER_OWNER ?? "";
  const org = env.CRABBOX_RUNTIME_ADAPTER_ORG ?? "";
  if (
    owner.length < 1 ||
    owner.length > 254 ||
    owner !== owner.trim() ||
    [...owner].some((character) => {
      const code = character.charCodeAt(0);
      return code < 0x20 || code > 0x7e;
    })
  ) {
    throw deploymentError("runtime_identity_invalid");
  }
  try {
    orgKeyForLabel(org);
  } catch {
    throw deploymentError("runtime_identity_invalid");
  }
}

function deploymentFlag(value: string | undefined, name: string): boolean {
  const normalized = value?.trim() ?? "";
  if (!normalized || normalized === "0") return false;
  if (normalized === "1") return true;
  throw new Error(`${name} must be 0 or 1`);
}

function deploymentError(code: string): Error {
  return new Error(`AWS deployment guard failed: ${code}`);
}

const disabledDeploymentGuard: AWSDeploymentGuard = {
  async start() {},
  async ready() {},
};
