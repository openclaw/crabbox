import { AwsClient } from "aws4fetch";
import { XMLParser } from "fast-xml-parser";

import { requireAWSRegion, sanitizeAWSRegion } from "./aws-region";
import { awsRunInstancesUserData } from "./bootstrap";
import {
  awsPromotedAMIConfigKey,
  awsInstanceTypeCandidatesForTargetClass,
  sshPorts,
  validatedAWSInstanceTypes,
  validatedCIDRs,
  workspaceProviderKeyPrefix,
  type LeaseConfig,
} from "./config";
import { hasImageRequirements } from "./image-capabilities";
import { osImageSpec } from "./os-image";
import {
  leaseIDForProviderKey,
  providerKeyForLease,
  providerKeyOwnedByLease,
  sshPublicKeyIdentity,
} from "./provider-key";
import { leaseProviderLabels } from "./provider-labels";
import { leaseProviderName } from "./slug";
import type {
  AWSCredentialProvider,
  Env,
  ProviderFastSnapshotRestore,
  ProviderImage,
  ProviderMachine,
  ProvisioningAttempt,
} from "./types";

const awsUbuntuOwner = "099720109477";
const ec2Version = "2016-11-15";
const stsVersion = "2011-06-15";
const awsSpotQuotaCode = "L-34B43A08";
const awsOnDemandQuotaCode = "L-1216C47A";
const awsSSHIngressDescription = "Crabbox SSH";
const awsRunInstancesOutcomeUncertain = "crabbox_aws_run_instances_outcome_uncertain";
const awsMacHostQuotaSpecs: Record<string, { quotaCode: string; quotaName: string }> = {
  mac1: { quotaCode: "L-A8448DC5", quotaName: "Running Dedicated mac1 Hosts" },
  mac2: { quotaCode: "L-5D8DADF5", quotaName: "Running Dedicated mac2 Hosts" },
  "mac2-m1ultra": {
    quotaCode: "L-AE4D744C",
    quotaName: "Running Dedicated mac2-m1ultra Hosts",
  },
  "mac2-m2": { quotaCode: "L-B90B5B66", quotaName: "Running Dedicated mac2-m2 Hosts" },
  "mac2-m2pro": {
    quotaCode: "L-14F120D1",
    quotaName: "Running Dedicated mac2-m2pro Hosts",
  },
  "mac-m3ultra": {
    quotaCode: "L-7108A7B5",
    quotaName: "Running Dedicated mac-m3ultra Hosts",
  },
  "mac-m4": { quotaCode: "L-2CBA8B92", quotaName: "Running Dedicated mac-m4 Hosts" },
  "mac-m4max": {
    quotaCode: "L-D82CB68A",
    quotaName: "Running Dedicated mac-m4max Hosts",
  },
  "mac-m4pro": {
    quotaCode: "L-6919FC30",
    quotaName: "Running Dedicated mac-m4pro Hosts",
  },
};
const snapshotDeleteBackoffMs = [1_000, 2_000, 4_000, 8_000, 15_000, 30_000];
const securityGroupVisibilityBackoffMs = [100, 200, 400, 800, 1_600, 3_200];
const awsInstanceVisibilityBackoffMs = [1_000, 2_000, 4_000, 8_000, 15_000, 30_000];

export interface AWSPrivateWorkspaceConfig {
  accountID: string;
  region: string;
  instanceTypes: string[];
  maxVCPUs: number;
  maxMemoryMiB: number;
  rootGB: number;
  subnetID: string;
  securityGroupID: string;
  controllerSecurityGroupID: string;
  instanceProfile: string;
  market: "spot" | "on-demand";
  ssmLogGroup: string;
}

export function awsPrivateWorkspaceModeEnabled(
  env: Pick<Env, "CRABBOX_WORKSPACE_AWS_PRIVATE">,
): boolean {
  const enabled = env.CRABBOX_WORKSPACE_AWS_PRIVATE?.trim() ?? "";
  if (!enabled || enabled === "0") return false;
  if (enabled !== "1") {
    throw new Error("CRABBOX_WORKSPACE_AWS_PRIVATE must be 0 or 1");
  }
  return true;
}

export function awsPrivateWorkspaceConfig(
  env: Pick<
    Env,
    | "CRABBOX_WORKSPACE_AWS_PRIVATE"
    | "CRABBOX_WORKSPACE_AWS_INSTANCE_TYPES"
    | "CRABBOX_WORKSPACE_AWS_MAX_VCPUS"
    | "CRABBOX_WORKSPACE_AWS_MAX_MEMORY_MIB"
    | "CRABBOX_WORKSPACE_AWS_ROOT_GB"
    | "CRABBOX_WORKSPACE_AWS_SUBNET_ID"
    | "CRABBOX_WORKSPACE_AWS_SECURITY_GROUP_ID"
    | "CRABBOX_WORKSPACE_AWS_CONTROLLER_SECURITY_GROUP_ID"
    | "CRABBOX_WORKSPACE_AWS_INSTANCE_PROFILE"
    | "CRABBOX_WORKSPACE_AWS_MARKET"
    | "CRABBOX_WORKSPACE_AWS_SSM_LOG_GROUP"
    | "CRABBOX_AWS_EXPECTED_ACCOUNT_ID"
    | "CRABBOX_AWS_EXPECTED_REGION"
  >,
): AWSPrivateWorkspaceConfig | undefined {
  if (!awsPrivateWorkspaceModeEnabled(env)) return undefined;
  const expected = awsExpectedIdentityConfig(env);
  if (!expected) {
    throw new Error(
      "private AWS workspaces require CRABBOX_AWS_EXPECTED_ACCOUNT_ID and CRABBOX_AWS_EXPECTED_REGION",
    );
  }
  const instanceTypes = validatedAWSInstanceTypes(
    (env.CRABBOX_WORKSPACE_AWS_INSTANCE_TYPES ?? "").split(","),
  );
  if (instanceTypes.length === 0) {
    throw new Error("CRABBOX_WORKSPACE_AWS_INSTANCE_TYPES is required");
  }
  const maxVCPUs = privateWorkspaceInt(
    env.CRABBOX_WORKSPACE_AWS_MAX_VCPUS,
    2,
    1,
    64,
    "CRABBOX_WORKSPACE_AWS_MAX_VCPUS",
  );
  const maxMemoryMiB = privateWorkspaceInt(
    env.CRABBOX_WORKSPACE_AWS_MAX_MEMORY_MIB,
    4096,
    512,
    262_144,
    "CRABBOX_WORKSPACE_AWS_MAX_MEMORY_MIB",
  );
  const rootGB = privateWorkspaceInt(
    env.CRABBOX_WORKSPACE_AWS_ROOT_GB,
    20,
    8,
    100,
    "CRABBOX_WORKSPACE_AWS_ROOT_GB",
  );
  const subnetID = requiredPrivateWorkspaceID(
    env.CRABBOX_WORKSPACE_AWS_SUBNET_ID,
    /^subnet-[a-z0-9]+$/,
    "CRABBOX_WORKSPACE_AWS_SUBNET_ID",
  );
  const securityGroupID = requiredPrivateWorkspaceID(
    env.CRABBOX_WORKSPACE_AWS_SECURITY_GROUP_ID,
    /^sg-[a-z0-9]+$/,
    "CRABBOX_WORKSPACE_AWS_SECURITY_GROUP_ID",
  );
  const controllerSecurityGroupID = requiredPrivateWorkspaceID(
    env.CRABBOX_WORKSPACE_AWS_CONTROLLER_SECURITY_GROUP_ID,
    /^sg-[a-z0-9]+$/,
    "CRABBOX_WORKSPACE_AWS_CONTROLLER_SECURITY_GROUP_ID",
  );
  const instanceProfile = requiredPrivateWorkspaceID(
    env.CRABBOX_WORKSPACE_AWS_INSTANCE_PROFILE,
    /^[\w+=,.@-]{1,128}$/,
    "CRABBOX_WORKSPACE_AWS_INSTANCE_PROFILE",
  );
  const marketValue = env.CRABBOX_WORKSPACE_AWS_MARKET?.trim() || "on-demand";
  if (marketValue !== "on-demand" && marketValue !== "spot") {
    throw new Error("CRABBOX_WORKSPACE_AWS_MARKET must be on-demand or spot");
  }
  const ssmLogGroup = requiredPrivateWorkspaceID(
    env.CRABBOX_WORKSPACE_AWS_SSM_LOG_GROUP,
    /^[A-Za-z0-9_./#-]{1,512}$/,
    "CRABBOX_WORKSPACE_AWS_SSM_LOG_GROUP",
  );
  return {
    accountID: expected.accountID,
    region: expected.region,
    instanceTypes,
    maxVCPUs,
    maxMemoryMiB,
    rootGB,
    subnetID,
    securityGroupID,
    controllerSecurityGroupID,
    instanceProfile,
    market: marketValue,
    ssmLogGroup,
  };
}

function privateWorkspaceInt(
  value: string | undefined,
  fallback: number,
  minimum: number,
  maximum: number,
  name: string,
): number {
  if (!value?.trim()) return fallback;
  const parsed = Number(value);
  if (!Number.isInteger(parsed) || parsed < minimum || parsed > maximum) {
    throw new Error(`${name} must be an integer from ${minimum} through ${maximum}`);
  }
  return parsed;
}

function requiredPrivateWorkspaceID(
  value: string | undefined,
  pattern: RegExp,
  name: string,
): string {
  const normalized = value?.trim() ?? "";
  if (!pattern.test(normalized)) {
    throw new Error(`${name} is required and malformed`);
  }
  return normalized;
}

function assertPrivateWorkspaceSecurityGroupShape(group: Record<string, unknown>): void {
  if (items(record(group["ipPermissions"])["item"]).length > 0) {
    throw new Error("AWS private workspace security group must have no ingress rules");
  }
  const egress = items(record(group["ipPermissionsEgress"])["item"]).map(record);
  const httpsRanges =
    egress.length === 1
      ? items(record(egress[0]!["ipRanges"])["item"])
          .map(record)
          .map((range) => asString(range["cidrIp"]))
      : [];
  if (
    egress.length !== 1 ||
    asString(egress[0]!["ipProtocol"]) !== "tcp" ||
    asString(egress[0]!["fromPort"]) !== "443" ||
    asString(egress[0]!["toPort"]) !== "443" ||
    httpsRanges.length !== 1 ||
    httpsRanges[0] !== "0.0.0.0/0" ||
    items(record(egress[0]!["ipv6Ranges"])["item"]).length > 0 ||
    items(record(egress[0]!["groups"])["item"]).length > 0 ||
    items(record(egress[0]!["prefixListIds"])["item"]).length > 0
  ) {
    throw new Error(
      "AWS private workspace security group must have exactly one IPv4 TCP 443 egress rule",
    );
  }
}

interface AWSFetchClient {
  fetch(input: string, init?: RequestInit): Promise<Response>;
}

class RefreshingAWSFetchClient implements AWSFetchClient {
  constructor(
    private readonly credentials: AWSCredentialProvider,
    private readonly service: string,
    private readonly region: string,
  ) {}

  async fetch(input: string, init?: RequestInit): Promise<Response> {
    const credentials = await this.credentials();
    const accessKeyId = credentials.accessKeyId?.trim();
    const secretAccessKey = credentials.secretAccessKey?.trim();
    if (!accessKeyId || !secretAccessKey) {
      throw new Error("AWS credential provider returned incomplete credentials");
    }
    const options: ConstructorParameters<typeof AwsClient>[0] = {
      accessKeyId,
      secretAccessKey,
      service: this.service,
      region: this.region,
    };
    const session = credentials.sessionToken?.trim();
    if (session) {
      options.sessionToken = session;
    }
    return await new AwsClient(options).fetch(input, init);
  }
}

export function awsCredentialsConfigured(
  env: Pick<Env, "awsCredentialProvider" | "AWS_ACCESS_KEY_ID" | "AWS_SECRET_ACCESS_KEY">,
): boolean {
  return Boolean(
    env.awsCredentialProvider ||
    (env.AWS_ACCESS_KEY_ID?.trim() && env.AWS_SECRET_ACCESS_KEY?.trim()),
  );
}

export function awsAutomaticProbesConfigured(
  env: Pick<
    Env,
    | "awsCredentialProvider"
    | "AWS_ACCESS_KEY_ID"
    | "AWS_SECRET_ACCESS_KEY"
    | "AWS_PROFILE"
    | "AWS_CONFIG_FILE"
    | "AWS_SHARED_CREDENTIALS_FILE"
    | "AWS_ROLE_ARN"
    | "AWS_WEB_IDENTITY_TOKEN_FILE"
    | "AWS_REGION"
    | "AWS_DEFAULT_REGION"
    | "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"
    | "AWS_CONTAINER_CREDENTIALS_FULL_URI"
    | "CRABBOX_AWS_REGION"
    | "CRABBOX_AWS_EXPECTED_ACCOUNT_ID"
    | "CRABBOX_AWS_EXPECTED_REGION"
    | "CRABBOX_WORKSPACE_PROVIDER"
  >,
): boolean {
  if (env.AWS_ACCESS_KEY_ID?.trim() && env.AWS_SECRET_ACCESS_KEY?.trim()) {
    return true;
  }
  if (!env.awsCredentialProvider) {
    return false;
  }
  const expectedIdentity = Boolean(
    env.CRABBOX_AWS_EXPECTED_ACCOUNT_ID?.trim() && env.CRABBOX_AWS_EXPECTED_REGION?.trim(),
  );
  const explicitDefaultChain = Boolean(
    env.AWS_PROFILE?.trim() ||
    env.AWS_CONFIG_FILE?.trim() ||
    env.AWS_SHARED_CREDENTIALS_FILE?.trim() ||
    env.AWS_REGION?.trim() ||
    env.AWS_DEFAULT_REGION?.trim() ||
    env.AWS_CONTAINER_CREDENTIALS_RELATIVE_URI?.trim() ||
    env.AWS_CONTAINER_CREDENTIALS_FULL_URI?.trim() ||
    env.CRABBOX_AWS_REGION?.trim(),
  );
  const webIdentity = Boolean(env.AWS_ROLE_ARN?.trim() && env.AWS_WEB_IDENTITY_TOKEN_FILE?.trim());
  return (
    expectedIdentity ||
    explicitDefaultChain ||
    webIdentity ||
    env.CRABBOX_WORKSPACE_PROVIDER?.trim() === "aws"
  );
}

export function awsOrphanSweepCredentialsConfigured(
  env: Pick<
    Env,
    | "awsCredentialProvider"
    | "AWS_ACCESS_KEY_ID"
    | "AWS_SECRET_ACCESS_KEY"
    | "CRABBOX_AWS_ORPHAN_SWEEP_ENABLED"
    | "CRABBOX_WORKSPACE_PROVIDER"
  >,
): boolean {
  if (env.AWS_ACCESS_KEY_ID?.trim() && env.AWS_SECRET_ACCESS_KEY?.trim()) {
    return true;
  }
  if (!env.awsCredentialProvider) {
    return false;
  }
  const sweepEnabled = ["1", "true", "yes", "on"].includes(
    (env.CRABBOX_AWS_ORPHAN_SWEEP_ENABLED ?? "").trim().toLowerCase(),
  );
  return sweepEnabled || env.CRABBOX_WORKSPACE_PROVIDER?.trim() === "aws";
}

export interface AWSExpectedIdentityConfig {
  accountID: string;
  region: string;
  taskRoleName?: string;
}

export function awsExpectedIdentityConfig(
  env: Pick<
    Env,
    | "CRABBOX_AWS_EXPECTED_ACCOUNT_ID"
    | "CRABBOX_AWS_EXPECTED_REGION"
    | "CRABBOX_AWS_EXPECTED_TASK_ROLE_NAME"
  >,
): AWSExpectedIdentityConfig | undefined {
  const accountID = env.CRABBOX_AWS_EXPECTED_ACCOUNT_ID?.trim() ?? "";
  const regionValue = env.CRABBOX_AWS_EXPECTED_REGION?.trim() ?? "";
  if (!accountID && !regionValue) return undefined;
  if (!accountID || !regionValue) {
    throw new Error(
      "CRABBOX_AWS_EXPECTED_ACCOUNT_ID and CRABBOX_AWS_EXPECTED_REGION must be configured together",
    );
  }
  if (!/^\d{12}$/.test(accountID)) {
    throw new Error("CRABBOX_AWS_EXPECTED_ACCOUNT_ID must be a 12-digit AWS account ID");
  }
  const taskRoleName = env.CRABBOX_AWS_EXPECTED_TASK_ROLE_NAME?.trim() ?? "";
  if (taskRoleName && !/^[\w+=,.@-]{1,64}$/.test(taskRoleName)) {
    throw new Error("CRABBOX_AWS_EXPECTED_TASK_ROLE_NAME is malformed");
  }
  return {
    accountID,
    region: requireAWSRegion(regionValue, "CRABBOX_AWS_EXPECTED_REGION"),
    ...(taskRoleName ? { taskRoleName } : {}),
  };
}

function awsCredentialProvider(
  env: Pick<
    Env,
    "awsCredentialProvider" | "AWS_ACCESS_KEY_ID" | "AWS_SECRET_ACCESS_KEY" | "AWS_SESSION_TOKEN"
  >,
): AWSCredentialProvider {
  if (env.awsCredentialProvider) {
    return env.awsCredentialProvider;
  }
  const accessKeyId = env.AWS_ACCESS_KEY_ID?.trim();
  const secretAccessKey = env.AWS_SECRET_ACCESS_KEY?.trim();
  if (!accessKeyId || !secretAccessKey) {
    throw new Error("AWS credentials are required");
  }
  const sessionToken = env.AWS_SESSION_TOKEN?.trim();
  return async () => ({
    accessKeyId,
    secretAccessKey,
    ...(sessionToken ? { sessionToken } : {}),
  });
}

export function awsManagedSecurityGroupName(
  config: Pick<LeaseConfig, "providerKey"> & Partial<Pick<LeaseConfig, "awsSGName">>,
): string {
  if (config.awsSGName) {
    return config.awsSGName;
  }
  return config.providerKey.startsWith(workspaceProviderKeyPrefix)
    ? "crabbox-workspaces"
    : "crabbox-runners";
}

export function awsConfiguredSecurityGroupID(
  config: Pick<LeaseConfig, "awsSGID" | "providerKey">,
  env: Pick<Env, "CRABBOX_AWS_SECURITY_GROUP_ID">,
): string {
  return (
    config.awsSGID ||
    (config.providerKey.startsWith(workspaceProviderKeyPrefix)
      ? ""
      : env.CRABBOX_AWS_SECURITY_GROUP_ID || "")
  );
}

export interface AWSMacHost {
  id: string;
  state: string;
  region: string;
  availabilityZone: string;
  instanceType: string;
  autoPlacement: string;
  availableCapacity?: number;
  availableVCpus?: number;
  allocationTime?: string;
  releaseTime?: string;
  tags: Record<string, string>;
}

export interface AWSMacHostOffering {
  region: string;
  availabilityZone: string;
  instanceType: string;
}

export interface AWSMacHostAllocationDryRun {
  region: string;
  availabilityZone: string;
  instanceType: string;
  ok: boolean;
  message: string;
}

export interface AWSReleaseHostFailure {
  resourceID: string;
  code: string;
  message: string;
}

export interface AWSReleaseHostsResult {
  successful: string[];
  unsuccessful: AWSReleaseHostFailure[];
}

export interface AWSServiceQuota {
  serviceCode?: string;
  quotaCode: string;
  quotaName: string;
  value?: number;
  adjustable?: boolean;
  globalQuota?: boolean;
  unit?: string;
}

export interface AWSCapacityReadinessCheck {
  status: "ok" | "skip" | "warning";
  check: "capacity";
  message: string;
  details: Record<string, string>;
}

export interface AWSIdentity {
  account: string;
  arn: string;
  userId: string;
  region: string;
  policyTarget?: AWSPolicyTarget;
}

export interface AWSPolicyTarget {
  type: "role" | "user";
  name: string;
  source: "iam-role" | "iam-user" | "assumed-role";
}

export function awsPolicyTargetFromArn(arn: string): AWSPolicyTarget | undefined {
  const parts = arn.split(":");
  if (parts.length < 6 || parts[0] !== "arn" || !parts[1]) {
    return undefined;
  }
  const service = parts[2];
  const resource = parts.slice(5).join(":");
  const segments = resource.split("/").filter(Boolean);
  const leafName = segments[segments.length - 1];
  if (service === "iam" && segments[0] === "user" && leafName) {
    return { type: "user", name: leafName, source: "iam-user" };
  }
  if (service === "iam" && segments[0] === "role" && leafName) {
    return { type: "role", name: leafName, source: "iam-role" };
  }
  const assumedRoleName = segments[1];
  if (service === "sts" && segments[0] === "assumed-role" && assumedRoleName) {
    return { type: "role", name: assumedRoleName, source: "assumed-role" };
  }
  return undefined;
}

export function createSecurityGroupParams(name: string, vpcID: string): Record<string, string> {
  return {
    GroupDescription: "Crabbox ephemeral test runners",
    GroupName: name,
    VpcId: vpcID,
    "TagSpecification.1.ResourceType": "security-group",
    "TagSpecification.1.Tag.1.Key": "Name",
    "TagSpecification.1.Tag.1.Value": name,
    "TagSpecification.1.Tag.2.Key": "crabbox",
    "TagSpecification.1.Tag.2.Value": "true",
    "TagSpecification.1.Tag.3.Key": "created_by",
    "TagSpecification.1.Tag.3.Value": "crabbox",
  };
}

type SSHIngressRule = {
  cidr: string;
  family: "ipv4" | "ipv6";
  port: string;
};

type DescribedSSHIngressRule = SSHIngressRule & { description: string };

interface AWSIngressOptions {
  reconcile?: "authoritative" | "additive";
  allowEmpty?: boolean;
}

const sshIngressRangeFamilies = [
  {
    cidrField: "cidrIp",
    descriptionParam: "IpPermissions.1.IpRanges.1.Description",
    family: "ipv4",
    param: "IpPermissions.1.IpRanges.1.CidrIp",
    rangesField: "ipRanges",
  },
  {
    cidrField: "cidrIpv6",
    descriptionParam: "IpPermissions.1.Ipv6Ranges.1.Description",
    family: "ipv6",
    param: "IpPermissions.1.Ipv6Ranges.1.CidrIpv6",
    rangesField: "ipv6Ranges",
  },
] as const;

export class EC2SpotClient {
  private readonly aws: AWSFetchClient;
  private readonly serviceQuotas: AWSFetchClient;
  private readonly stsClient: AWSFetchClient;
  private readonly ssmClient: AWSFetchClient;
  private readonly endpoint: string;
  private readonly serviceQuotasEndpoint: string;
  private readonly stsEndpoint: string;
  private readonly ssmEndpoint: string;
  private readonly region: string;
  private readonly parser = new XMLParser({ ignoreAttributes: false });
  private expectedIdentity?: Promise<AWSIdentity>;

  constructor(
    private readonly env: Env,
    region: string,
  ) {
    this.region = requireAWSRegion(region || env.CRABBOX_AWS_REGION || "eu-west-1");
    const expected = awsExpectedIdentityConfig(env);
    if (expected && expected.region !== this.region) {
      throw new Error(
        `AWS region mismatch: expected ${expected.region}, configured ${this.region}`,
      );
    }
    const credentials = awsCredentialProvider(env);
    this.endpoint = `https://ec2.${this.region}.amazonaws.com/`;
    this.serviceQuotasEndpoint = `https://servicequotas.${this.region}.amazonaws.com/`;
    this.stsEndpoint = `https://sts.${this.region}.amazonaws.com/`;
    this.ssmEndpoint = `https://ssm.${this.region}.amazonaws.com/`;
    this.aws = new RefreshingAWSFetchClient(credentials, "ec2", this.region);
    this.serviceQuotas = new RefreshingAWSFetchClient(credentials, "servicequotas", this.region);
    this.stsClient = new RefreshingAWSFetchClient(credentials, "sts", this.region);
    this.ssmClient = new RefreshingAWSFetchClient(credentials, "ssm", this.region);
  }

  async capacityReadinessChecks(config: LeaseConfig): Promise<AWSCapacityReadinessCheck[]> {
    if (config.target === "macos") {
      return [];
    }
    return await Promise.all(
      awsCapacityReadinessMarkets(config).map(async (market) => {
        const code = awsQuotaCodeForMarket(market);
        const quota = await this.appliedServiceQuota(code);
        return awsCapacityReadinessCheckForQuota(config, market, this.region, quota);
      }),
    );
  }

  async identity(): Promise<AWSIdentity> {
    const root = await this.sts("GetCallerIdentity", {});
    const result = record(root["GetCallerIdentityResult"] ?? root);
    const arn = asString(result["Arn"]);
    const identity: AWSIdentity = {
      account: asString(result["Account"]),
      arn,
      userId: asString(result["UserId"]),
      region: this.region,
    };
    const policyTarget = awsPolicyTargetFromArn(arn);
    if (policyTarget) {
      identity.policyTarget = policyTarget;
    }
    return identity;
  }

  async verifiedIdentity(): Promise<AWSIdentity> {
    const expected = awsExpectedIdentityConfig(this.env);
    if (!expected) return await this.identity();
    const verification =
      this.expectedIdentity ??
      (async () => {
        const identity = await this.identity();
        if (identity.account !== expected.accountID) {
          throw new Error(
            `AWS account mismatch: expected ${expected.accountID}, authenticated ${identity.account || "unknown"}`,
          );
        }
        if (
          expected.taskRoleName &&
          (identity.policyTarget?.source !== "assumed-role" ||
            identity.policyTarget.name !== expected.taskRoleName)
        ) {
          throw new Error("AWS task role mismatch");
        }
        return identity;
      })();
    this.expectedIdentity = verification;
    try {
      return await verification;
    } catch (error) {
      if (this.expectedIdentity === verification) {
        delete this.expectedIdentity;
      }
      throw error;
    }
  }

  async privateWorkspacePreflight(
    config: LeaseConfig,
    policy: AWSPrivateWorkspaceConfig,
  ): Promise<AWSIdentity> {
    const identity = await this.verifiedIdentity();
    if (!config.awsPrivate || config.awsRegion !== policy.region) {
      throw new Error("private AWS workspace preflight received a mismatched lease policy");
    }
    await this.assertPrivateWorkspaceInstanceTypes(policy);
    const vpcID = await this.assertPrivateWorkspaceSubnet(policy.subnetID);
    await this.assertPrivateWorkspaceSecurityGroups(policy, vpcID);
    const imageID = await this.resolveAMI(config);
    const labels = leaseProviderLabels(
      config,
      "cbx_000000000000",
      "preflight",
      "deployment-preflight",
      "aws",
      new Date(0),
      { market: config.capacityMarket, crabbox_workspace: "true", access_mode: "ssm" },
    );
    for (const instanceType of policy.instanceTypes) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- each allowed launch shape needs proof.
      const params = await awsRunInstancesParams({
        config: { ...config, serverType: instanceType },
        leaseID: "cbx-private-workspace-preflight",
        imageID,
        securityGroupID: policy.securityGroupID,
        rootGB: policy.rootGB,
        instanceProfile: policy.instanceProfile,
        subnetID: policy.subnetID,
        labels: {
          ...labels,
          server_type: instanceType,
          Name: "crabbox-private-workspace-preflight",
        },
      });
      // oxlint-disable-next-line eslint/no-await-in-loop -- fail closed before checking next shape.
      await this.expectDryRun("RunInstances", { ...params, DryRun: "true" });
    }
    await this.ssm("DescribeInstanceInformation", { MaxResults: 5 });
    return identity;
  }

  private async assertPrivateWorkspaceInstanceTypes(
    policy: AWSPrivateWorkspaceConfig,
  ): Promise<void> {
    const params: Record<string, string> = {};
    policy.instanceTypes.forEach((instanceType, index) => {
      params[`InstanceType.${index + 1}`] = instanceType;
    });
    const root = await this.ec2("DescribeInstanceTypes", params);
    const described = items(record(root["instanceTypeSet"])["item"]).map(record);
    for (const instanceType of policy.instanceTypes) {
      const item = described.find(
        (candidate) => asString(candidate["instanceType"]) === instanceType,
      );
      if (!item) {
        throw new Error(`AWS instance type is unavailable in ${this.region}: ${instanceType}`);
      }
      const architectures = items(
        record(record(item["processorInfo"])["supportedArchitectures"])["item"],
      ).map(asString);
      const vcpus = positiveInt(asString(record(item["vCpuInfo"])["defaultVCpus"]));
      const memoryMiB = positiveInt(asString(record(item["memoryInfo"])["sizeInMiB"]));
      if (!architectures.includes("x86_64")) {
        throw new Error(`AWS private workspace instance type must support x86_64: ${instanceType}`);
      }
      if (!vcpus || vcpus > policy.maxVCPUs) {
        throw new Error(
          `AWS private workspace instance type ${instanceType} exceeds ${policy.maxVCPUs} vCPUs`,
        );
      }
      if (!memoryMiB || memoryMiB > policy.maxMemoryMiB) {
        throw new Error(
          `AWS private workspace instance type ${instanceType} exceeds ${policy.maxMemoryMiB} MiB`,
        );
      }
    }
  }

  private async assertPrivateWorkspaceSubnet(subnetID: string): Promise<string> {
    const root = await this.ec2("DescribeSubnets", { "SubnetId.1": subnetID });
    const subnet = record(items(record(root["subnetSet"])["item"])[0]);
    const vpcID = asString(subnet["vpcId"]);
    if (
      asString(subnet["subnetId"]) !== subnetID ||
      asString(subnet["state"]) !== "available" ||
      !vpcID
    ) {
      throw new Error(`AWS private workspace subnet is unavailable: ${subnetID}`);
    }
    if (asString(subnet["mapPublicIpOnLaunch"]) === "true") {
      throw new Error(`AWS private workspace subnet maps public IPs on launch: ${subnetID}`);
    }
    if (asString(subnet["assignIpv6AddressOnCreation"]) === "true") {
      throw new Error(`AWS private workspace subnet auto-assigns IPv6 addresses: ${subnetID}`);
    }
    let routes = await this.ec2("DescribeRouteTables", {
      "Filter.1.Name": "association.subnet-id",
      "Filter.1.Value.1": subnetID,
    });
    let tables = items(record(routes["routeTableSet"])["item"]).map(record);
    if (tables.length === 0) {
      routes = await this.ec2("DescribeRouteTables", {
        "Filter.1.Name": "vpc-id",
        "Filter.1.Value.1": vpcID,
        "Filter.2.Name": "association.main",
        "Filter.2.Value.1": "true",
      });
      tables = items(record(routes["routeTableSet"])["item"]).map(record);
    }
    if (tables.length !== 1) {
      throw new Error(`AWS private workspace subnet route table is ambiguous: ${subnetID}`);
    }
    const publicDefault = items(record(tables[0]!["routeSet"])["item"])
      .map(record)
      .some(
        (route) =>
          asString(route["state"]) !== "blackhole" &&
          (asString(route["destinationCidrBlock"]) === "0.0.0.0/0" ||
            asString(route["destinationIpv6CidrBlock"]) === "::/0") &&
          asString(route["gatewayId"]).startsWith("igw-"),
      );
    if (publicDefault) {
      throw new Error(
        `AWS private workspace subnet has an internet-gateway default route: ${subnetID}`,
      );
    }
    return vpcID;
  }

  private async assertPrivateWorkspaceSecurityGroups(
    policy: AWSPrivateWorkspaceConfig,
    vpcID: string,
  ): Promise<void> {
    if (policy.securityGroupID === policy.controllerSecurityGroupID) {
      throw new Error("AWS controller and workspace security groups must be distinct");
    }
    const root = await this.ec2("DescribeSecurityGroups", {
      "GroupId.1": policy.securityGroupID,
      "GroupId.2": policy.controllerSecurityGroupID,
    });
    const groups = items(record(root["securityGroupInfo"])["item"]).map(record);
    const workspace = groups.find((group) => asString(group["groupId"]) === policy.securityGroupID);
    const controller = groups.find(
      (group) => asString(group["groupId"]) === policy.controllerSecurityGroupID,
    );
    if (!workspace || !controller) {
      throw new Error("AWS private workspace security groups could not be read");
    }
    if (asString(workspace["vpcId"]) !== vpcID || asString(controller["vpcId"]) !== vpcID) {
      throw new Error(
        "AWS private workspace and controller security groups must share the subnet VPC",
      );
    }
    assertPrivateWorkspaceSecurityGroupShape(workspace);
  }

  private async expectDryRun(action: string, params: Record<string, string>): Promise<void> {
    try {
      await this.ec2(action, params);
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      if (message.includes("DryRunOperation")) return;
      throw error;
    }
    throw new Error(`AWS ${action} permission preflight did not return DryRunOperation`);
  }

  private async ensureExpectedIdentity(): Promise<void> {
    if (!awsExpectedIdentityConfig(this.env)) return;
    await this.verifiedIdentity();
  }

  async listCrabboxServers(): Promise<ProviderMachine[]> {
    const root = await this.ec2("DescribeInstances", {
      "Filter.1.Name": "tag:crabbox",
      "Filter.1.Value.1": "true",
      "Filter.2.Name": "instance-state-name",
      "Filter.2.Value.1": "pending",
      "Filter.2.Value.2": "running",
      "Filter.2.Value.3": "stopping",
      "Filter.2.Value.4": "stopped",
    });
    return reservations(root).flatMap((reservation) =>
      items(record(record(reservation)["instancesSet"])["item"]).map((instance) =>
        this.withRegion(instanceToMachine(instance)),
      ),
    );
  }

  async findWorkspaceServerByLease(leaseID: string): Promise<ProviderMachine | undefined> {
    const root = await this.ec2("DescribeInstances", {
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
    });
    const matches = reservations(root).flatMap((reservation) =>
      items(record(record(reservation)["instancesSet"])["item"]).map((instance) =>
        this.withRegion(instanceToMachine(instance)),
      ),
    );
    if (matches.length > 1) {
      throw new Error(`AWS private workspace recovery is ambiguous for lease ${leaseID}`);
    }
    return matches[0];
  }

  async refreshSSHIngress(config: LeaseConfig, options: AWSIngressOptions = {}): Promise<void> {
    await this.ensureSecurityGroup(config, options);
  }

  async createServerWithFallback(
    config: LeaseConfig,
    leaseID: string,
    slug: string,
    owner: string,
    options: AWSIngressOptions = {},
  ): Promise<{
    server: ProviderMachine;
    serverType: string;
    market?: string;
    attempts?: ProvisioningAttempt[];
  }> {
    if (!config.awsPrivate) {
      await this.ensureSSHKey(config.providerKey, config.sshPublicKey, leaseID);
    }
    let transientImageID = "";
    try {
      const capabilityImageRequired = hasImageRequirements(config.imageRequirements);
      const defaultImageID = config.awsSnapshot
        ? await (async () => {
            transientImageID = await this.registerSnapshotImage(config.awsSnapshot, leaseID);
            return this.waitForImageAvailable(transientImageID);
          })()
        : config.target === "macos" || capabilityImageRequired
          ? ""
          : await this.resolveAMI(config);
      const securityGroupID = await this.ensureSecurityGroup(config, options);
      const pinnedMacHostID = config.target === "macos" ? config.hostID || config.awsMacHostID : "";
      // An explicit Mac host is tied to one hardware family. Resolve that family once so a
      // defaulted --type cannot send an incompatible instance type to the pinned host.
      const pinnedMacHostType = pinnedMacHostID
        ? await this.macHostInstanceType(pinnedMacHostID)
        : "";
      if (
        pinnedMacHostType &&
        config.serverTypeExplicit &&
        pinnedMacHostType !== config.serverType
      ) {
        throw new Error(
          `EC2 Mac Dedicated Host ${pinnedMacHostID} requires ${pinnedMacHostType}, not requested ${config.serverType}`,
        );
      }
      const candidates = pinnedMacHostType ? [pinnedMacHostType] : awsLaunchCandidates(config);
      const failures: string[] = [];
      const attempts: ProvisioningAttempt[] = [];
      const quotaCache = new Map<string, number | undefined>();
      const imageCache = new Map<string, string>();
      const pinnedMacOSImageID =
        config.target === "macos" ? config.awsAMI || this.env.CRABBOX_AWS_AMI || "" : "";
      const resolveCandidateImageID = async (candidateConfig: LeaseConfig): Promise<string> => {
        if (defaultImageID) {
          return defaultImageID;
        }
        const promotedImageID =
          config.awsPromotedAMIs[
            awsPromotedAMIConfigKey(this.region, candidateConfig.serverType)
          ] ?? "";
        if (promotedImageID) {
          return promotedImageID;
        }
        if (capabilityImageRequired) {
          throw new Error(
            `no AWS AMI found in ${this.region} for ${candidateConfig.serverType} satisfying the requested image capabilities`,
          );
        }
        if (candidateConfig.target !== "macos") {
          return this.resolveAMI(candidateConfig);
        }
        if (pinnedMacOSImageID && candidateConfig.serverType === config.serverType) {
          return pinnedMacOSImageID;
        }
        const query = awsMacOSAMIQuery(candidateConfig.serverType);
        const cacheKey = `${query.name}\0${query.architecture}`;
        const cached = imageCache.get(cacheKey);
        if (cached) {
          return cached;
        }
        const imageID = pinnedMacOSImageID
          ? await this.resolveLatestAmazonAMI(query.name, query.architecture)
          : await this.resolveAMI(candidateConfig);
        imageCache.set(cacheKey, imageID);
        return imageID;
      };
      for (const serverType of candidates) {
        // oxlint-disable-next-line eslint/no-await-in-loop -- quota preflight follows sequential fallback order.
        const preflight = await this.quotaPreflightAttempt(
          serverType,
          config.capacityMarket,
          quotaCache,
        );
        if (preflight) {
          attempts.push(preflight);
          failures.push(`${serverType}: ${preflight.message}`);
          continue;
        }
        try {
          // oxlint-disable-next-line eslint/no-await-in-loop -- instance-type fallback may need an architecture-specific AMI.
          const imageID = await resolveCandidateImageID({ ...config, serverType });
          // oxlint-disable-next-line eslint/no-await-in-loop -- instance-type fallback must stay sequential.
          const server = await this.createServer(
            { ...config, serverType },
            leaseID,
            slug,
            owner,
            imageID,
            securityGroupID,
          );
          const result: {
            server: ProviderMachine;
            serverType: string;
            market?: string;
            attempts?: ProvisioningAttempt[];
          } = { server, serverType, market: config.capacityMarket };
          if (attempts.length > 0) {
            result.attempts = attempts;
          }
          return result;
        } catch (error) {
          const message = error instanceof Error ? error.message : String(error);
          attempts.push({
            region: this.region,
            serverType,
            market: config.capacityMarket,
            category: awsProvisioningErrorCategory(message) || "fatal",
            message: conciseAWSProvisioningMessage(message),
          });
          failures.push(`${serverType}: ${message}`);
          if (!isRetryableAWSProvisioningError(message)) {
            break;
          }
        }
      }
      if (config.capacityMarket === "spot" && config.capacityFallback.startsWith("on-demand")) {
        for (const serverType of candidates) {
          // oxlint-disable-next-line eslint/no-await-in-loop -- on-demand fallback must stay sequential.
          const preflight = await this.quotaPreflightAttempt(serverType, "on-demand", quotaCache);
          if (preflight) {
            attempts.push(preflight);
            failures.push(`on-demand ${serverType}: ${preflight.message}`);
            continue;
          }
          try {
            // oxlint-disable-next-line eslint/no-await-in-loop -- on-demand fallback may need an architecture-specific AMI.
            const imageID = await resolveCandidateImageID({
              ...config,
              capacityMarket: "on-demand",
              serverType,
            });
            // oxlint-disable-next-line eslint/no-await-in-loop -- on-demand fallback must stay sequential.
            const server = await this.createServer(
              { ...config, capacityMarket: "on-demand", serverType },
              leaseID,
              slug,
              owner,
              imageID,
              securityGroupID,
            );
            const result: {
              server: ProviderMachine;
              serverType: string;
              market?: string;
              attempts?: ProvisioningAttempt[];
            } = { server, serverType, market: "on-demand" };
            if (attempts.length > 0) {
              result.attempts = attempts;
            }
            return result;
          } catch (error) {
            const message = error instanceof Error ? error.message : String(error);
            attempts.push({
              region: this.region,
              serverType,
              market: "on-demand",
              category: awsProvisioningErrorCategory(message) || "fatal",
              message: conciseAWSProvisioningMessage(message),
            });
            failures.push(`on-demand ${serverType}: ${message}`);
            if (!isRetryableAWSProvisioningError(message)) {
              break;
            }
          }
        }
      }
      if (config.serverTypeExplicit) {
        throw new Error(
          `requested exact AWS instance type ${config.serverType} failed; remove --type to allow class fallback: ${failures.join("; ")}`,
        );
      }
      throw new Error(failures.join("; "));
    } finally {
      if (transientImageID) {
        await this.ec2("DeregisterImage", { ImageId: transientImageID }).catch(() => undefined);
      }
    }
  }

  async getServer(instanceID: string): Promise<ProviderMachine> {
    const root = await this.ec2("DescribeInstances", {
      "InstanceId.1": instanceID,
    });
    for (const reservation of reservations(root)) {
      for (const instance of items(record(record(reservation)["instancesSet"])["item"])) {
        return this.withRegion(instanceToMachine(instance));
      }
    }
    throw new Error(`aws instance not found: ${instanceID}`);
  }

  async findServer(instanceID: string): Promise<ProviderMachine | undefined> {
    try {
      return await this.getServer(instanceID);
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      if (isAWSInstanceNotFoundError(message)) return undefined;
      throw error;
    }
  }

  async waitForServerIP(instanceID: string, allowPrivateAddress = false): Promise<ProviderMachine> {
    const deadline = Date.now() + 600_000;
    while (Date.now() < deadline) {
      let server: ProviderMachine | undefined;
      try {
        // oxlint-disable-next-line eslint/no-await-in-loop -- polling waits between EC2 reads.
        server = await this.getServer(instanceID);
      } catch (error) {
        const message = error instanceof Error ? error.message : String(error);
        if (!isAWSInstanceNotFoundError(message)) {
          throw error;
        }
      }
      const address = server?.host || (allowPrivateAddress ? server?.privateHost : "");
      if (server && address) {
        return { ...server, host: address };
      }
      // oxlint-disable-next-line eslint/no-await-in-loop -- this delay is the polling interval.
      await sleep(5_000);
    }
    throw new Error(`timed out waiting for AWS instance network address: ${instanceID}`);
  }

  async waitForSSMOnline(instanceID: string): Promise<void> {
    const deadline = Date.now() + 10 * 60_000;
    while (Date.now() < deadline) {
      let result: Record<string, unknown>;
      try {
        // oxlint-disable-next-line eslint/no-await-in-loop -- SSM registration is polled serially.
        result = await this.ssm("DescribeInstanceInformation", {
          Filters: [{ Key: "InstanceIds", Values: [instanceID] }],
          MaxResults: 5,
        });
      } catch (error) {
        const message = error instanceof Error ? error.message : String(error);
        if (!isAWSManagedNodePendingError(message)) {
          throw error;
        }
        result = {};
      }
      const instances = items(result["InstanceInformationList"]).map(record);
      if (
        instances.some(
          (instance) =>
            asString(instance["InstanceId"]) === instanceID &&
            asString(instance["PingStatus"]) === "Online",
        )
      ) {
        return;
      }
      // oxlint-disable-next-line eslint/no-await-in-loop -- SSM registration is eventually consistent.
      await sleep(5_000);
    }
    throw new Error(`timed out waiting for AWS SSM managed-node readiness: ${instanceID}`);
  }

  async assertPrivateWorkspaceRootVolume(
    server: ProviderMachine,
    leaseID: string,
    expectedRootGB: number,
  ): Promise<void> {
    if (!server.awsRootVolumeID || server.awsRootDeleteOnTermination !== true) {
      throw new Error("AWS private workspace root volume mapping is outside deployment policy");
    }
    const root = await this.ec2("DescribeVolumes", { "VolumeId.1": server.awsRootVolumeID });
    const volume = record(items(record(root["volumeSet"])["item"])[0]);
    const labels = tagMap(volume["tagSet"]);
    if (
      asString(volume["volumeId"]) !== server.awsRootVolumeID ||
      asString(volume["volumeType"]) !== "gp3" ||
      positiveInt(asString(volume["size"])) !== expectedRootGB ||
      asString(volume["encrypted"]) !== "true" ||
      labels["lease"] !== leaseID ||
      labels["crabbox"] !== "true" ||
      labels["created_by"] !== "crabbox" ||
      labels["crabbox_workspace"] !== "true" ||
      labels["access_mode"] !== "ssm"
    ) {
      throw new Error("AWS private workspace root volume is outside deployment policy");
    }
  }

  async runSSMBootstrap(
    instanceID: string,
    leaseID: string,
    command: string,
    logGroup: string,
  ): Promise<{ commandID: string; status: string }> {
    const bootstrapScript = [
      "set -euo pipefail",
      "cloud-init status --wait",
      "/usr/local/bin/crabbox-ready",
      command,
    ].join("\n");
    const guardedCommand = `/bin/bash -lc ${shellQuote(bootstrapScript)}`;
    if (!command.trim() || new TextEncoder().encode(guardedCommand).byteLength > 24 * 1024) {
      throw new Error("AWS SSM workspace bootstrap command is empty or too large");
    }
    const sent = await this.ssm("SendCommand", {
      CloudWatchOutputConfig: {
        CloudWatchLogGroupName: logGroup,
        CloudWatchOutputEnabled: true,
      },
      Comment: `Crabbox workspace ${leaseID}`,
      DocumentName: "AWS-RunShellScript",
      InstanceIds: [instanceID],
      Parameters: {
        commands: [guardedCommand],
        executionTimeout: ["900"],
      },
      TimeoutSeconds: 900,
    });
    const commandID = asString(record(sent["Command"])["CommandId"]);
    if (!commandID) {
      throw new Error("AWS SSM SendCommand returned no command ID");
    }
    const deadline = Date.now() + 17 * 60_000;
    while (Date.now() < deadline) {
      try {
        // oxlint-disable-next-line eslint/no-await-in-loop -- command status is sequential.
        const invocation = await this.ssm("GetCommandInvocation", {
          CommandId: commandID,
          InstanceId: instanceID,
        });
        const status = asString(invocation["Status"]);
        if (status === "Success") {
          return { commandID, status };
        }
        if (["Cancelled", "Failed", "TimedOut", "Cancelling"].includes(status)) {
          const detail = asString(invocation["StatusDetails"]) || status || "unknown";
          throw new Error(`AWS SSM workspace bootstrap failed: ${detail}`);
        }
      } catch (error) {
        const message = error instanceof Error ? error.message : String(error);
        if (!message.includes("InvocationDoesNotExist")) throw error;
      }
      // oxlint-disable-next-line eslint/no-await-in-loop -- SSM command delivery is asynchronous.
      await sleep(2_000);
    }
    throw new Error(`timed out waiting for AWS SSM workspace bootstrap: ${commandID}`);
  }

  async hourlySpotPriceUSD(instanceType: string): Promise<number | undefined> {
    const root = await this.ec2("DescribeSpotPriceHistory", {
      "InstanceType.1": instanceType,
      MaxResults: "1",
      "ProductDescription.1": "Linux/UNIX",
      StartTime: new Date().toISOString(),
    });
    const item = items(record(root["spotPriceHistorySet"])["item"])[0];
    return positiveFloat(asString(record(item)["spotPrice"]));
  }

  async deleteServer(instanceID: string): Promise<void> {
    await this.ec2("TerminateInstances", { "InstanceId.1": instanceID });
  }

  async terminateServerAndWait(instanceID: string): Promise<void> {
    let terminated: Record<string, unknown>;
    try {
      terminated = await this.ec2("TerminateInstances", { "InstanceId.1": instanceID });
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      if (isAWSInstanceNotFoundError(message)) return;
      throw error;
    }
    const returnedIDs = items(record(terminated["instancesSet"])["item"])
      .map((item) => asString(record(item)["instanceId"]))
      .filter(Boolean);
    if (!returnedIDs.includes(instanceID)) {
      throw new Error(`AWS TerminateInstances did not confirm instance ${instanceID}`);
    }
    for (const delay of awsInstanceVisibilityBackoffMs) {
      try {
        // oxlint-disable-next-line eslint/no-await-in-loop -- termination confirmation is ordered.
        const server = await this.getServer(instanceID);
        if (server.status === "terminated") return;
      } catch (error) {
        const message = error instanceof Error ? error.message : String(error);
        if (isAWSInstanceNotFoundError(message)) return;
        throw error;
      }
      // oxlint-disable-next-line eslint/no-await-in-loop -- wait between termination reads.
      await sleep(delay);
    }
    try {
      const server = await this.getServer(instanceID);
      if (server.status === "terminated") return;
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      if (isAWSInstanceNotFoundError(message)) return;
      throw error;
    }
    throw new Error(`timed out confirming AWS instance termination: ${instanceID}`);
  }

  async createDiskSnapshot(instanceID: string, name: string): Promise<ProviderImage> {
    const source = await this.instanceRootVolumeMetadata(instanceID);
    const root = await this.ec2("CreateSnapshot", {
      VolumeId: source.volumeID,
      Description: `Crabbox checkpoint from ${instanceID}`,
      "TagSpecification.1.ResourceType": "snapshot",
      "TagSpecification.1.Tag.1.Key": "crabbox",
      "TagSpecification.1.Tag.1.Value": "true",
      "TagSpecification.1.Tag.2.Key": "created_by",
      "TagSpecification.1.Tag.2.Value": "crabbox",
      "TagSpecification.1.Tag.3.Key": "Name",
      "TagSpecification.1.Tag.3.Value": name,
      "TagSpecification.1.Tag.4.Key": "crabbox_root_device_name",
      "TagSpecification.1.Tag.4.Value": source.rootDeviceName,
      "TagSpecification.1.Tag.5.Key": "crabbox_architecture",
      "TagSpecification.1.Tag.5.Value": source.architecture,
    });
    const snapshotID = asString(root["snapshotId"]);
    if (!snapshotID) {
      throw new Error("aws returned no snapshot id");
    }
    return {
      id: snapshotID,
      name,
      state: asString(root["status"]) || "pending",
      provider: "aws",
      kind: "aws-ebs-snapshot",
      region: this.region,
      resourceID: snapshotID,
      snapshots: [snapshotID],
    };
  }

  async createImage(instanceID: string, name: string, noReboot: boolean): Promise<ProviderImage> {
    const params: Record<string, string> = {
      InstanceId: instanceID,
      Name: name,
      NoReboot: noReboot ? "true" : "false",
      "TagSpecification.1.ResourceType": "image",
      "TagSpecification.1.Tag.1.Key": "crabbox",
      "TagSpecification.1.Tag.1.Value": "true",
      "TagSpecification.1.Tag.2.Key": "created_by",
      "TagSpecification.1.Tag.2.Value": "crabbox",
      "TagSpecification.1.Tag.3.Key": "Name",
      "TagSpecification.1.Tag.3.Value": name,
    };
    const root = await this.ec2("CreateImage", params);
    const imageID = asString(root["imageId"]);
    if (!imageID) {
      throw new Error("aws returned no image id");
    }
    return {
      id: imageID,
      name,
      state: "pending",
      provider: "aws",
      kind: "aws-ami",
      region: this.region,
      resourceID: imageID,
    };
  }

  async getImage(imageID: string): Promise<ProviderImage> {
    if (imageID.startsWith("snap-")) {
      return await this.getSnapshot(imageID);
    }
    const root = await this.ec2("DescribeImages", {
      "ImageId.1": imageID,
    });
    const image = record(items(record(root["imagesSet"])["item"])[0]);
    const id = asString(image["imageId"]);
    if (!id) {
      throw new Error(`aws image not found: ${imageID}`);
    }
    return {
      id,
      name: asString(image["name"]),
      state: asString(image["imageState"]),
      provider: "aws",
      kind: "aws-ami",
      region: this.region,
      resourceID: id,
      architecture: asString(image["architecture"]),
      snapshots: imageSnapshotIDs(image),
    };
  }

  async deleteImage(imageID: string): Promise<void> {
    if (imageID.startsWith("snap-")) {
      await this.deleteSnapshotWithRetry(imageID);
      return;
    }
    const image = await this.getImage(imageID);
    await this.ec2("DeregisterImage", { ImageId: imageID }).catch((error: unknown) => {
      const message = error instanceof Error ? error.message : String(error);
      if (!message.includes("InvalidAMIID.NotFound")) {
        throw error;
      }
    });
    for (const snapshotID of image.snapshots ?? []) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- EBS snapshot deletes are independent cleanup calls.
      await this.deleteSnapshotWithRetry(snapshotID);
    }
  }

  async enableFastSnapshotRestore(
    snapshotIDs: string[],
    availabilityZones: string[],
  ): Promise<ProviderFastSnapshotRestore[]> {
    const snapshots = uniqueStrings(snapshotIDs);
    const zones = uniqueStrings(availabilityZones);
    if (snapshots.length === 0 || zones.length === 0) {
      return [];
    }
    const params: Record<string, string> = {};
    snapshots.forEach((snapshotID, index) => {
      params[`SourceSnapshotId.${index + 1}`] = snapshotID;
    });
    zones.forEach((zone, index) => {
      params[`AvailabilityZone.${index + 1}`] = zone;
    });
    const root = await this.ec2("EnableFastSnapshotRestores", params);
    const successes = fastSnapshotRestoreItems(root, "enableFastSnapshotRestoreSuccessSet");
    const errors = fastSnapshotRestoreItems(root, "enableFastSnapshotRestoreErrorSet");
    if (errors.length > 0) {
      const message = errors
        .map((error) =>
          [
            error.snapshotID,
            error.availabilityZone,
            error.stateTransitionReason || error.state || "failed",
          ]
            .filter(Boolean)
            .join(":"),
        )
        .join(", ");
      throw new Error(`aws fast snapshot restore failed: ${message}`);
    }
    return successes.length > 0
      ? successes
      : snapshots.flatMap((snapshotID) =>
          zones.map((availabilityZone) => ({
            snapshotID,
            availabilityZone,
            state: "enabling",
          })),
        );
  }

  async fastSnapshotRestoreStatus(
    snapshotIDs: string[],
    availabilityZones: string[] = [],
  ): Promise<ProviderFastSnapshotRestore[]> {
    const snapshots = uniqueStrings(snapshotIDs);
    const zones = uniqueStrings(availabilityZones);
    if (snapshots.length === 0) {
      return [];
    }
    const params: Record<string, string> = {
      "Filter.1.Name": "snapshot-id",
    };
    snapshots.forEach((snapshotID, index) => {
      params[`Filter.1.Value.${index + 1}`] = snapshotID;
    });
    if (zones.length > 0) {
      params["Filter.2.Name"] = "availability-zone";
      zones.forEach((zone, index) => {
        params[`Filter.2.Value.${index + 1}`] = zone;
      });
    }
    const statuses: ProviderFastSnapshotRestore[] = [];
    let nextToken = "";
    for (let page = 0; page < 100; page++) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- EC2 pagination depends on the previous token.
      const root = await this.ec2("DescribeFastSnapshotRestores", {
        ...params,
        ...(nextToken ? { NextToken: nextToken } : {}),
      });
      statuses.push(...fastSnapshotRestoreItems(root, "fastSnapshotRestoreSet"));
      nextToken = asString(root["nextToken"]);
      if (!nextToken) {
        return statuses;
      }
    }
    throw new Error("aws fast snapshot restore status pagination exceeded 100 pages");
  }

  async listMacHosts(serverType = "", state = ""): Promise<AWSMacHost[]> {
    const params: Record<string, string> = {};
    let filter = 1;
    if (serverType) {
      params[`Filter.${filter}.Name`] = "instance-type";
      params[`Filter.${filter}.Value.1`] = serverType;
      filter++;
    }
    if (state) {
      params[`Filter.${filter}.Name`] = "state";
      params[`Filter.${filter}.Value.1`] = state;
    }
    const root = await this.ec2("DescribeHosts", params);
    return items(record(root["hostSet"])["item"])
      .map((host) => this.macHostFromDescribeHost(host))
      .filter((host) => host.instanceType.startsWith("mac"));
  }

  async listMacHostOfferings(serverType = "mac2.metal"): Promise<AWSMacHostOffering[]> {
    const root = await this.ec2("DescribeInstanceTypeOfferings", {
      LocationType: "availability-zone",
      "Filter.1.Name": "instance-type",
      "Filter.1.Value.1": serverType,
    });
    return awsMacHostOfferingsFromDescribeInstanceTypeOfferings(root, this.region);
  }

  async allocateMacHost(
    serverType: string,
    availabilityZone: string,
    clientToken: string,
  ): Promise<AWSMacHost[]> {
    const params = this.allocateMacHostParams(serverType, availabilityZone, clientToken);
    const root = await this.ec2("AllocateHosts", params);
    const hostIDs = awsHostIDsFromSet(root["hostIdSet"]);
    if (hostIDs.length === 0) {
      return [];
    }
    const fallbackHosts = this.macHostsFromAllocatedIDs(hostIDs, availabilityZone, serverType);
    let hosts: AWSMacHost[];
    try {
      hosts = await this.describeMacHostsByID(hostIDs);
    } catch {
      return fallbackHosts;
    }
    return hosts.length > 0 ? hosts : fallbackHosts;
  }

  async dryRunAllocateMacHost(
    serverType: string,
    availabilityZone: string,
    clientToken: string,
  ): Promise<AWSMacHostAllocationDryRun> {
    try {
      await this.ec2("AllocateHosts", {
        ...this.allocateMacHostParams(serverType, availabilityZone, clientToken),
        DryRun: "true",
      });
      return {
        region: this.region,
        availabilityZone,
        instanceType: serverType,
        ok: true,
        message: "dry run accepted",
      };
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      return {
        region: this.region,
        availabilityZone,
        instanceType: serverType,
        ok: message.includes("DryRunOperation"),
        message: conciseAWSMacHostDryRunMessage(message),
      };
    }
  }

  async listMacHostQuotas(serverType = "mac2.metal"): Promise<AWSServiceQuota[]> {
    const family = awsMacHostFamily(serverType);
    const direct = await this.macHostQuotasByFamily(family);
    if (direct) {
      return direct;
    }
    const all = await this.listEC2ServiceQuotas();
    const macHostQuotas = all.filter((quota) => {
      const name = quota.quotaName.toLowerCase();
      return name.includes("running dedicated") && name.includes("mac") && name.includes("hosts");
    });
    const exactName = `running dedicated ${family} hosts`;
    const exact = macHostQuotas.filter((quota) => quota.quotaName.toLowerCase() === exactName);
    return (exact.length > 0 ? exact : macHostQuotas).toSorted((left, right) => {
      const leftName = left.quotaName.toLowerCase();
      const rightName = right.quotaName.toLowerCase();
      const leftFamily = family && leftName.includes(family) ? 0 : 1;
      const rightFamily = family && rightName.includes(family) ? 0 : 1;
      return leftFamily - rightFamily || left.quotaName.localeCompare(right.quotaName);
    });
  }

  private async macHostQuotasByFamily(family: string): Promise<AWSServiceQuota[] | undefined> {
    const spec = awsMacHostQuotaSpecs[family];
    if (!spec) {
      return undefined;
    }
    const quota = await this.getEC2ServiceQuota(spec.quotaCode);
    if (!quota) {
      return [];
    }
    return [
      {
        ...quota,
        quotaCode: quota.quotaCode || spec.quotaCode,
        quotaName: quota.quotaName || spec.quotaName,
        serviceCode: quota.serviceCode || "ec2",
      },
    ];
  }

  async releaseMacHost(hostID: string): Promise<string[]> {
    const root = await this.ec2("ReleaseHosts", { "HostId.1": hostID });
    const result = awsReleaseHostsResult(root);
    if (result.unsuccessful.length > 0) {
      const details = result.unsuccessful.map((failure) =>
        [failure.resourceID || hostID, failure.code, failure.message].filter(Boolean).join(": "),
      );
      throw new Error(`aws ReleaseHosts failed: ${details.join("; ")}`);
    }
    if (!result.successful.includes(hostID)) {
      throw new Error(`aws ReleaseHosts did not confirm release for ${hostID}`);
    }
    return result.successful;
  }

  private allocateMacHostParams(
    serverType: string,
    availabilityZone: string,
    clientToken: string,
  ): Record<string, string> {
    const params: Record<string, string> = {
      AutoPlacement: "off",
      ClientToken: clientToken,
      InstanceType: serverType,
      Quantity: "1",
      "TagSpecification.1.ResourceType": "dedicated-host",
      "TagSpecification.1.Tag.1.Key": "crabbox",
      "TagSpecification.1.Tag.1.Value": "true",
      "TagSpecification.1.Tag.2.Key": "created_by",
      "TagSpecification.1.Tag.2.Value": "crabbox",
    };
    if (availabilityZone) {
      params["AvailabilityZone"] = availabilityZone;
    }
    return params;
  }

  private async getSnapshot(snapshotID: string): Promise<ProviderImage> {
    const root = await this.ec2("DescribeSnapshots", {
      "SnapshotId.1": snapshotID,
    });
    const snapshot = record(items(record(root["snapshotSet"])["item"])[0]);
    const id = asString(snapshot["snapshotId"]);
    if (!id) {
      throw new Error(`aws snapshot not found: ${snapshotID}`);
    }
    return {
      id,
      name: tagValue(snapshot, "Name") || id,
      state: asString(snapshot["status"]),
      provider: "aws",
      kind: "aws-ebs-snapshot",
      region: this.region,
      resourceID: id,
      snapshots: [id],
    };
  }

  private async instanceRootVolumeMetadata(instanceID: string): Promise<{
    volumeID: string;
    rootDeviceName: string;
    architecture: string;
  }> {
    const root = await this.ec2("DescribeInstances", {
      "InstanceId.1": instanceID,
    });
    for (const reservation of reservations(root)) {
      for (const instance of items(record(record(reservation)["instancesSet"])["item"])) {
        const inst = record(instance);
        const rootDevice = asString(inst["rootDeviceName"]) || "/dev/sda1";
        for (const mapping of items(record(inst["blockDeviceMapping"])["item"])) {
          const map = record(mapping);
          if (asString(map["deviceName"]) !== rootDevice) continue;
          const volumeID = asString(record(map["ebs"])["volumeId"]);
          if (volumeID) {
            return {
              volumeID,
              rootDeviceName: rootDevice,
              architecture: asString(inst["architecture"]) || "x86_64",
            };
          }
        }
      }
    }
    throw new Error(`aws root volume not found for instance ${instanceID}`);
  }

  private async registerSnapshotImage(snapshotID: string, leaseID: string): Promise<string> {
    const name = `crabbox-${leaseID.replaceAll("_", "-")}-${Date.now()}`;
    const metadata = await this.snapshotBootMetadata(snapshotID);
    const root = await this.ec2("RegisterImage", {
      Name: name,
      Architecture: metadata.architecture,
      RootDeviceName: metadata.rootDeviceName,
      VirtualizationType: "hvm",
      EnaSupport: "true",
      "BlockDeviceMapping.1.DeviceName": metadata.rootDeviceName,
      "BlockDeviceMapping.1.Ebs.SnapshotId": snapshotID,
      "BlockDeviceMapping.1.Ebs.DeleteOnTermination": "true",
      "BlockDeviceMapping.1.Ebs.VolumeType": "gp3",
      "TagSpecification.1.ResourceType": "image",
      "TagSpecification.1.Tag.1.Key": "crabbox",
      "TagSpecification.1.Tag.1.Value": "true",
      "TagSpecification.1.Tag.2.Key": "created_by",
      "TagSpecification.1.Tag.2.Value": "crabbox",
      "TagSpecification.1.Tag.3.Key": "transient_checkpoint_snapshot",
      "TagSpecification.1.Tag.3.Value": snapshotID,
    });
    const imageID = asString(root["imageId"]);
    if (!imageID) {
      throw new Error("aws returned no transient image id");
    }
    return imageID;
  }

  private async snapshotBootMetadata(snapshotID: string): Promise<{
    rootDeviceName: string;
    architecture: string;
  }> {
    const root = await this.ec2("DescribeSnapshots", {
      "SnapshotId.1": snapshotID,
    });
    const snapshot = record(items(record(root["snapshotSet"])["item"])[0]);
    return {
      rootDeviceName: tagValue(snapshot, "crabbox_root_device_name") || "/dev/sda1",
      architecture: tagValue(snapshot, "crabbox_architecture") || "x86_64",
    };
  }

  private async waitForImageAvailable(imageID: string): Promise<string> {
    const deadline = Date.now() + 600_000;
    for (;;) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- EC2 AMI availability is eventually consistent.
      const image = await this.getImage(imageID);
      if (image.state === "available") return imageID;
      if (image.state === "failed" || image.state === "invalid") {
        throw new Error(`aws transient image ${imageID} is ${image.state}`);
      }
      if (Date.now() > deadline) {
        throw new Error(`timed out waiting for AWS transient image ${imageID}`);
      }
      // oxlint-disable-next-line eslint/no-await-in-loop -- polling interval.
      await sleep(5_000);
    }
  }

  private async deleteSnapshotWithRetry(snapshotID: string): Promise<void> {
    let lastError: unknown;
    for (let attempt = 0; attempt < snapshotDeleteBackoffMs.length + 1; attempt++) {
      try {
        // oxlint-disable-next-line eslint/no-await-in-loop -- retry preserves snapshot IDs after AMI deregistration.
        await this.ec2("DeleteSnapshot", { SnapshotId: snapshotID });
        return;
      } catch (error) {
        const message = error instanceof Error ? error.message : String(error);
        if (message.includes("InvalidSnapshot.NotFound")) {
          return;
        }
        if (!isRetryableSnapshotDeleteError(message)) {
          throw error;
        }
        lastError = error;
        const backoffMs = snapshotDeleteBackoffMs[attempt];
        if (backoffMs === undefined) {
          break;
        }
        // oxlint-disable-next-line eslint/no-await-in-loop -- AWS can keep just-deregistered AMI snapshots locked briefly.
        await sleep(backoffMs);
      }
    }
    throw lastError;
  }

  async deleteSSHKey(name: string, leaseID: string): Promise<void> {
    if (name !== providerKeyForLease(leaseID)) {
      return;
    }
    let described: Record<string, unknown>;
    try {
      described = await this.ec2("DescribeKeyPairs", { "KeyName.1": name });
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      if (message.includes("InvalidKeyPair.NotFound")) {
        return;
      }
      throw error;
    }
    const existing = items(record(described["keySet"])["item"]).map(record)[0];
    if (!existing || !providerKeyOwnedByLease(tagMap(existing["tagSet"]), leaseID)) {
      console.warn(`AWS SSH key cleanup skipped unowned key lease=${leaseID} key=${name}`);
      return;
    }
    const keyPairID = asString(existing["keyPairId"]);
    if (!keyPairID) {
      throw new Error(`AWS SSH key ${name} is missing its immutable key pair ID`);
    }
    await this.ec2("DeleteKeyPair", { KeyPairId: keyPairID }).catch((error: unknown) => {
      const message = error instanceof Error ? error.message : String(error);
      if (!message.includes("InvalidKeyPair.NotFound")) {
        throw error;
      }
    });
  }

  async setTags(instanceID: string, labels: Record<string, string>): Promise<void> {
    const params: Record<string, string> = { "ResourceId.1": instanceID };
    addTags(params, "Tag", labels);
    await this.ec2("CreateTags", params);
  }

  private async ensureSSHKey(name: string, publicKey: string, leaseID: string): Promise<void> {
    const keyLeaseID = leaseIDForProviderKey(name);
    if (keyLeaseID && keyLeaseID !== leaseID) {
      throw new Error(`aws ssh key ${name} is reserved for lease ${keyLeaseID}`);
    }
    const leaseOwned = keyLeaseID === leaseID;
    let described: Record<string, unknown> | undefined;
    try {
      described = await this.ec2("DescribeKeyPairs", {
        IncludePublicKey: "true",
        "KeyName.1": name,
      });
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      if (!message.includes("InvalidKeyPair.NotFound")) {
        throw error;
      }
    }
    if (described) {
      const existing = items(record(described["keySet"])["item"]).map(record)[0];
      if (!existing) {
        throw new Error(`aws ssh key ${name} could not be read`);
      }
      const existingPublicKey = asString(existing["publicKey"]);
      if (
        !existingPublicKey ||
        sshPublicKeyIdentity(existingPublicKey) !== sshPublicKeyIdentity(publicKey)
      ) {
        throw new Error(`aws ssh key ${name} exists with different public key`);
      }
      if (leaseOwned && !providerKeyOwnedByLease(tagMap(existing["tagSet"]), leaseID)) {
        throw new Error(`aws ssh key ${name} is not owned by lease ${leaseID}`);
      }
      return;
    }
    const params: Record<string, string> = {
      KeyName: name,
      PublicKeyMaterial: btoa(publicKey),
      "TagSpecification.1.ResourceType": "key-pair",
      "TagSpecification.1.Tag.1.Key": "crabbox",
      "TagSpecification.1.Tag.1.Value": "true",
      "TagSpecification.1.Tag.2.Key": "created_by",
      "TagSpecification.1.Tag.2.Value": "crabbox",
    };
    if (leaseOwned) {
      params["TagSpecification.1.Tag.3.Key"] = "lease";
      params["TagSpecification.1.Tag.3.Value"] = leaseID;
    }
    await this.ec2("ImportKeyPair", params);
  }

  private async createServer(
    config: LeaseConfig,
    leaseID: string,
    slug: string,
    owner: string,
    imageID: string,
    securityGroupID: string,
  ): Promise<ProviderMachine> {
    const now = new Date();
    const name = leaseProviderName(leaseID, slug);
    const labels = leaseProviderLabels(config, leaseID, slug, owner, "aws", now, {
      market: config.capacityMarket,
    });
    const rootGB = config.awsRootGB || positiveInt(this.env.CRABBOX_AWS_ROOT_GB) || 400;
    const instanceProfile = config.awsProfile || this.env.CRABBOX_AWS_INSTANCE_PROFILE || "";
    const subnetID = config.awsSubnetID || this.env.CRABBOX_AWS_SUBNET_ID || "";
    const configuredMacHostID =
      config.hostID ||
      config.awsMacHostID ||
      this.env.CRABBOX_HOST_ID ||
      this.env.CRABBOX_AWS_MAC_HOST_ID ||
      "";
    const macHostAvailabilityZone =
      config.target === "macos"
        ? await this.macHostAvailabilityZoneForLaunch(config, subnetID)
        : "";
    let lastMacHostID = "";
    const run = async (macHostID: string): Promise<ProviderMachine> => {
      const params = await awsRunInstancesParams({
        config,
        leaseID,
        imageID,
        securityGroupID,
        rootGB,
        instanceProfile,
        subnetID,
        labels: {
          ...labels,
          ...(config.awsPrivate ? { crabbox_workspace: "true", access_mode: "ssm" } : {}),
          Name: name,
        },
      });
      applyAWSRunInstanceTargetOptions(params, config);
      if (config.target === "macos") {
        const hostID =
          macHostID ||
          (await this.discoverMacHostID(config.serverType, "", macHostAvailabilityZone));
        lastMacHostID = hostID;
        params["Placement.HostId"] = hostID;
        params["Placement.Tenancy"] = "host";
      } else if (!subnetID) {
        const availabilityZone = awsAvailabilityZoneForRegion(config, this.env, this.region);
        if (availabilityZone) {
          params["Placement.AvailabilityZone"] = availabilityZone;
        }
      }
      let root: Record<string, unknown>;
      try {
        root = await this.ec2("RunInstances", params);
      } catch (error) {
        const message = error instanceof Error ? error.message : String(error);
        if (!config.awsPrivate || message.includes("aws RunInstances: http 4")) {
          throw error;
        }
        throw new Error(`${awsRunInstancesOutcomeUncertain}: ${message}`, { cause: error });
      }
      const instance = items(record(root["instancesSet"])["item"])[0];
      if (!instance) {
        const message = "aws returned no instances";
        if (config.awsPrivate) {
          throw new Error(`${awsRunInstancesOutcomeUncertain}: ${message}`);
        }
        throw new Error(message);
      }
      return this.withRegion(instanceToMachine(instance));
    };
    try {
      return await run(configuredMacHostID);
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      if (
        config.target === "macos" &&
        ((configuredMacHostID && isAWSInvalidHostIDError(message)) ||
          (!configuredMacHostID && isAWSInsufficientCapacityOnHostError(message)))
      ) {
        const discovered = await this.discoverMacHostID(
          config.serverType,
          configuredMacHostID || lastMacHostID,
          macHostAvailabilityZone,
        );
        return run(discovered);
      }
      throw error;
    }
  }

  private async resolveAMI(config: LeaseConfig): Promise<string> {
    if (!config.awsUseStockImage && (config.awsAMI || this.env.CRABBOX_AWS_AMI)) {
      return config.awsAMI || this.env.CRABBOX_AWS_AMI || "";
    }
    if (config.target === "windows") {
      return this.resolveLatestAmazonAMI("Windows_Server-2022-English-Full-Base-*", "x86_64");
    }
    if (config.target === "macos") {
      const query = awsMacOSAMIQuery(config.serverType);
      return this.resolveLatestAmazonAMI(query.name, query.architecture);
    }
    const os = osImageSpec(config.os);
    const architecture = config.architecture === "arm64" ? "arm64" : "x86_64";
    const name = config.architecture === "arm64" ? os.awsArm64Name : os.awsName;
    return this.resolveLatestAMI(
      awsUbuntuOwner,
      name,
      architecture,
      `no ${os.awsLabel} ${architecture} AMI found in ${this.region}`,
    );
  }

  private async resolveLatestAmazonAMI(name: string, architecture: string): Promise<string> {
    return this.resolveLatestAMI(
      "amazon",
      name,
      architecture,
      `no AWS AMI found in ${this.region} for name=${name} architecture=${architecture}`,
    );
  }

  private async resolveLatestAMI(
    owner: string,
    name: string,
    architecture: string,
    emptyMessage: string,
  ): Promise<string> {
    const root = await this.ec2("DescribeImages", {
      "Owner.1": owner,
      "Filter.1.Name": "architecture",
      "Filter.1.Value.1": architecture,
      "Filter.2.Name": "name",
      "Filter.2.Value.1": name,
      "Filter.3.Name": "root-device-type",
      "Filter.3.Value.1": "ebs",
      "Filter.4.Name": "virtualization-type",
      "Filter.4.Value.1": "hvm",
    });
    const images = items(record(root["imagesSet"])["item"]).toSorted((left, right) =>
      asString(record(right)["creationDate"]).localeCompare(asString(record(left)["creationDate"])),
    );
    const imageID = asString(record(images[0])["imageId"]);
    if (!imageID) {
      throw new Error(emptyMessage);
    }
    return imageID;
  }

  private async ensureSecurityGroup(
    config: LeaseConfig,
    options: AWSIngressOptions = {},
  ): Promise<string> {
    const configuredGroupID = awsConfiguredSecurityGroupID(config, this.env);
    if (config.awsPrivate) {
      if (!configuredGroupID) {
        throw new Error("AWS private workspace security group is required");
      }
      const existing = await this.ec2("DescribeSecurityGroups", {
        "GroupId.1": configuredGroupID,
      });
      const groups = items(record(existing["securityGroupInfo"])["item"]).map(record);
      const group = groups.find(
        (candidate) => asString(candidate["groupId"]) === configuredGroupID,
      );
      if (!group) {
        throw new Error(
          `AWS private workspace security group is unavailable: ${configuredGroupID}`,
        );
      }
      assertPrivateWorkspaceSecurityGroupShape(group);
      return configuredGroupID;
    }
    let group: unknown;
    let groupID = configuredGroupID;
    if (groupID) {
      const existing = await this.ec2("DescribeSecurityGroups", {
        "GroupId.1": groupID,
      });
      group = items(record(existing["securityGroupInfo"])["item"])[0];
    } else {
      const vpcID = await this.securityGroupVPC(config);
      const name = awsManagedSecurityGroupName(config);
      const existing = await this.ec2("DescribeSecurityGroups", {
        "Filter.1.Name": "group-name",
        "Filter.1.Value.1": name,
        "Filter.2.Name": "vpc-id",
        "Filter.2.Value.1": vpcID,
      });
      group = items(record(existing["securityGroupInfo"])["item"])[0];
      groupID = asString(record(group)["groupId"]);
      if (!groupID) {
        try {
          const created = await this.ec2(
            "CreateSecurityGroup",
            createSecurityGroupParams(name, vpcID),
          );
          groupID = asString(record(created)["groupId"]);
        } catch (error) {
          const message = error instanceof Error ? error.message : String(error);
          if (!message.includes("InvalidGroup.Duplicate")) {
            throw error;
          }
          group = await this.waitForSecurityGroup(name, vpcID, error);
          groupID = asString(record(group)["groupId"]);
        }
      }
    }
    if (!groupID) {
      throw new Error("aws security group id is empty");
    }
    const cidrs = awsSSHCIDRs(config, this.env, options.allowEmpty);
    const ports = sshPorts(config);
    for (let attempt = 0; ; attempt += 1) {
      try {
        if (options.reconcile !== "additive") {
          // oxlint-disable-next-line eslint/no-await-in-loop -- the whole ingress pass retries only on group propagation.
          await this.pruneStaleSSHIngress(groupID, group, ports, cidrs);
        }
        let compactedAfterRuleLimit = false;
        for (const port of ports) {
          if (!cidrs.includes("0.0.0.0/0")) {
            // oxlint-disable-next-line eslint/no-await-in-loop -- cleanup is per port.
            await this.revokeWorldTCP(groupID, port).catch((error: unknown) => {
              const message = error instanceof Error ? error.message : String(error);
              if (!message.includes("InvalidPermission.NotFound")) {
                throw error;
              }
            });
          }
          for (const cidr of cidrs) {
            // oxlint-disable-next-line eslint/no-await-in-loop -- duplicate ingress handling is per CIDR.
            await this.allowTCP(groupID, port, cidr).catch((error: unknown) => {
              const message = error instanceof Error ? error.message : String(error);
              if (message.includes("InvalidPermission.Duplicate")) {
                return;
              }
              if (
                options.reconcile !== "additive" &&
                !compactedAfterRuleLimit &&
                isAWSSecurityGroupRuleLimitError(message)
              ) {
                compactedAfterRuleLimit = true;
                return this.compactSSHIngressForRuleLimit(groupID, ports, cidrs).then(
                  (compacted) => {
                    if (!compacted) {
                      throw error;
                    }
                    return this.allowTCP(groupID, port, cidr).catch((retryError: unknown) => {
                      const retryMessage =
                        retryError instanceof Error ? retryError.message : String(retryError);
                      if (!retryMessage.includes("InvalidPermission.Duplicate")) {
                        throw retryError;
                      }
                    });
                  },
                );
              }
              throw error;
            });
          }
        }
        return groupID;
      } catch (error) {
        const message = error instanceof Error ? error.message : String(error);
        const delay = securityGroupVisibilityBackoffMs[attempt];
        if (delay === undefined || !message.includes("InvalidGroup.NotFound")) {
          throw error;
        }
        // oxlint-disable-next-line eslint/no-await-in-loop -- EC2 group propagation is eventually consistent.
        await sleep(delay);
        group = undefined;
      }
    }
  }

  private async waitForSecurityGroup(
    name: string,
    vpcID: string,
    duplicateError: unknown,
  ): Promise<unknown> {
    for (const delay of [0, ...securityGroupVisibilityBackoffMs]) {
      if (delay > 0) {
        // oxlint-disable-next-line eslint/no-await-in-loop -- EC2 group discovery is eventually consistent.
        await sleep(delay);
      }
      try {
        // oxlint-disable-next-line eslint/no-await-in-loop -- each lookup follows the bounded backoff.
        const raced = await this.ec2("DescribeSecurityGroups", {
          "Filter.1.Name": "group-name",
          "Filter.1.Value.1": name,
          "Filter.2.Name": "vpc-id",
          "Filter.2.Value.1": vpcID,
        });
        const group = items(record(raced["securityGroupInfo"])["item"])[0];
        if (asString(record(group)["groupId"])) {
          return group;
        }
      } catch (error) {
        const message = error instanceof Error ? error.message : String(error);
        if (!message.includes("InvalidGroup.NotFound")) {
          throw error;
        }
      }
    }
    throw duplicateError;
  }

  private async discoverMacHostID(
    serverType: string,
    excludedHostID = "",
    availabilityZone = "",
  ): Promise<string> {
    const params: Record<string, string> = {
      "Filter.1.Name": "instance-type",
      "Filter.1.Value.1": serverType,
      "Filter.2.Name": "state",
      "Filter.2.Value.1": "available",
      "Filter.3.Name": "tag:crabbox",
      "Filter.3.Value.1": "true",
    };
    if (availabilityZone) {
      params["Filter.4.Name"] = "availability-zone";
      params["Filter.4.Value.1"] = availabilityZone;
    }
    const root = await this.ec2("DescribeHosts", params);
    const hostID = awsMacHostIDFromDescribeHosts(root, excludedHostID, serverType);
    if (!hostID) {
      const zoneHint = availabilityZone ? ` in ${availabilityZone}` : "";
      throw new Error(
        `no available EC2 Mac Dedicated Host found in ${this.region}${zoneHint} for ${serverType}; allocate a host or set CRABBOX_HOST_ID`,
      );
    }
    return hostID;
  }

  private async macHostAvailabilityZoneForLaunch(
    config: LeaseConfig,
    subnetID: string,
  ): Promise<string> {
    if (subnetID) {
      return this.subnetAvailabilityZone(subnetID);
    }
    return awsAvailabilityZoneForRegion(config, this.env, this.region);
  }

  private async subnetAvailabilityZone(subnetID: string): Promise<string> {
    const root = await this.ec2("DescribeSubnets", { "SubnetId.1": subnetID });
    const subnet = record(items(record(root["subnetSet"])["item"])[0]);
    const availabilityZone = asString(subnet["availabilityZone"]);
    if (!availabilityZone) {
      throw new Error(`AWS subnet not found: ${subnetID}`);
    }
    return availabilityZone;
  }

  private async describeMacHostsByID(hostIDs: string[]): Promise<AWSMacHost[]> {
    const params: Record<string, string> = {};
    hostIDs.forEach((hostID, index) => {
      params[`HostId.${index + 1}`] = hostID;
    });
    const root = await this.ec2("DescribeHosts", params);
    return items(record(root["hostSet"])["item"]).map((host) => this.macHostFromDescribeHost(host));
  }

  private async macHostInstanceType(hostID: string): Promise<string> {
    const host = (await this.describeMacHostsByID([hostID]))[0];
    if (!host) {
      throw new Error(`AWS EC2 Mac Dedicated Host not found: ${hostID}`);
    }
    if (!host.instanceType.startsWith("mac")) {
      throw new Error(`AWS Dedicated Host ${hostID} is not an EC2 Mac host`);
    }
    return host.instanceType;
  }

  private macHostsFromAllocatedIDs(
    hostIDs: string[],
    availabilityZone: string,
    serverType: string,
  ): AWSMacHost[] {
    return hostIDs.map((id) => ({
      id,
      state: "available",
      region: this.region,
      availabilityZone,
      instanceType: serverType,
      autoPlacement: "off",
      tags: {},
    }));
  }

  private macHostFromDescribeHost(input: unknown): AWSMacHost {
    const host = record(input);
    const properties = record(host["hostProperties"]);
    const instanceType = asString(properties["instanceType"] ?? host["instanceType"]);
    const macHost: AWSMacHost = {
      id: asString(host["hostId"]),
      state: asString(host["hostState"] ?? host["state"]),
      region: this.region,
      availabilityZone: asString(host["availabilityZone"]),
      instanceType,
      autoPlacement: asString(host["autoPlacement"]),
      tags: tagMap(host["tagSet"]),
    };
    const availableCapacity = awsMacHostAvailableCapacity(host, instanceType);
    if (availableCapacity !== undefined) {
      macHost.availableCapacity = availableCapacity;
    }
    const availableVCpus = finiteNumber(record(host["availableCapacity"])["availableVCpus"]);
    if (availableVCpus !== undefined) {
      macHost.availableVCpus = availableVCpus;
    }
    const allocationTime = asString(host["allocationTime"]);
    if (allocationTime) {
      macHost.allocationTime = allocationTime;
    }
    const releaseTime = asString(host["releaseTime"]);
    if (releaseTime) {
      macHost.releaseTime = releaseTime;
    }
    return macHost;
  }

  private async pruneStaleSSHIngress(
    groupID: string,
    group: unknown,
    ports: string[],
    cidrs: string[],
  ): Promise<void> {
    for (const rule of staleCrabboxSSHIngressRules(group, ports, cidrs)) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- revoke calls are per exact rule.
      await this.revokeTCP(groupID, rule).catch((error: unknown) => {
        const message = error instanceof Error ? error.message : String(error);
        if (!message.includes("InvalidPermission.NotFound")) {
          throw error;
        }
      });
    }
  }

  private async compactSSHIngressForRuleLimit(
    groupID: string,
    ports: string[],
    cidrs: string[],
  ): Promise<boolean> {
    const existing = await this.ec2("DescribeSecurityGroups", {
      "GroupId.1": groupID,
    });
    const group = items(record(existing["securityGroupInfo"])["item"])[0];
    const rules = reclaimableCrabboxSSHIngressRules(group, ports, cidrs);
    for (const rule of rules) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- revoke calls are per exact rule.
      await this.revokeTCP(groupID, rule).catch((error: unknown) => {
        const message = error instanceof Error ? error.message : String(error);
        if (!message.includes("InvalidPermission.NotFound")) {
          throw error;
        }
      });
    }
    return rules.length > 0;
  }

  private async securityGroupVPC(config: LeaseConfig): Promise<string> {
    const subnetID = config.awsSubnetID || this.env.CRABBOX_AWS_SUBNET_ID || "";
    if (!subnetID) {
      const root = await this.ec2("DescribeVpcs", {
        "Filter.1.Name": "is-default",
        "Filter.1.Value.1": "true",
      });
      const vpcID = asString(record(items(record(root["vpcSet"])["item"])[0])["vpcId"]);
      if (!vpcID) {
        throw new Error("no default VPC found; set awsSubnetID and awsSGID");
      }
      return vpcID;
    }
    const root = await this.ec2("DescribeSubnets", { "SubnetId.1": subnetID });
    const vpcID = asString(record(items(record(root["subnetSet"])["item"])[0])["vpcId"]);
    if (!vpcID) {
      throw new Error(`AWS subnet not found: ${subnetID}`);
    }
    return vpcID;
  }

  private async allowTCP(groupID: string, port: string, cidr: string): Promise<void> {
    if (!/^[1-9][0-9]{0,4}$/.test(port) || Number(port) > 65_535) {
      throw new Error(`invalid SSH port: ${port}`);
    }
    const params: Record<string, string> = {
      GroupId: groupID,
      "IpPermissions.1.FromPort": port,
      "IpPermissions.1.IpProtocol": "tcp",
      "IpPermissions.1.ToPort": port,
    };
    assignSSHIngressRange(params, cidr, true);
    await this.ec2("AuthorizeSecurityGroupIngress", params);
  }

  private async revokeWorldTCP(groupID: string, port: string): Promise<void> {
    await this.ec2("RevokeSecurityGroupIngress", {
      GroupId: groupID,
      "IpPermissions.1.FromPort": port,
      "IpPermissions.1.IpProtocol": "tcp",
      "IpPermissions.1.IpRanges.1.CidrIp": "0.0.0.0/0",
      "IpPermissions.1.ToPort": port,
    });
  }

  private async revokeTCP(groupID: string, rule: SSHIngressRule): Promise<void> {
    const params: Record<string, string> = {
      GroupId: groupID,
      "IpPermissions.1.FromPort": rule.port,
      "IpPermissions.1.IpProtocol": "tcp",
      "IpPermissions.1.ToPort": rule.port,
    };
    assignSSHIngressRule(params, rule);
    await this.ec2("RevokeSecurityGroupIngress", params);
  }

  private async ec2(
    action: string,
    params: Record<string, string>,
  ): Promise<Record<string, unknown>> {
    await this.ensureExpectedIdentity();
    const body = new URLSearchParams({ Action: action, Version: ec2Version, ...params });
    const response = await this.aws.fetch(this.endpoint, {
      method: "POST",
      headers: { "content-type": "application/x-www-form-urlencoded; charset=utf-8" },
      body: body.toString(),
    });
    const text = await response.text();
    if (!response.ok) {
      throw new Error(this.awsQueryErrorMessage(action, response.status, text));
    }
    const parsed = this.parser.parse(text) as unknown;
    const parsedRecord = record(parsed);
    const root = parsedRecord[`${action}Response`] ?? parsedRecord["Response"] ?? parsedRecord;
    return record(root);
  }

  private async sts(
    action: string,
    params: Record<string, string>,
  ): Promise<Record<string, unknown>> {
    const body = new URLSearchParams({ Action: action, Version: stsVersion, ...params });
    const response = await this.stsClient.fetch(this.stsEndpoint, {
      method: "POST",
      headers: { "content-type": "application/x-www-form-urlencoded; charset=utf-8" },
      body: body.toString(),
    });
    const text = await response.text();
    if (!response.ok) {
      throw new Error(this.awsQueryErrorMessage(action, response.status, text));
    }
    const parsed = this.parser.parse(text) as unknown;
    const parsedRecord = record(parsed);
    const root = parsedRecord[`${action}Response`] ?? parsedRecord["Response"] ?? parsedRecord;
    return record(root);
  }

  private async ssm(
    action: string,
    body: Record<string, unknown>,
  ): Promise<Record<string, unknown>> {
    await this.ensureExpectedIdentity();
    const response = await this.ssmClient.fetch(this.ssmEndpoint, {
      method: "POST",
      headers: {
        "content-type": "application/x-amz-json-1.1",
        "x-amz-target": `AmazonSSM.${action}`,
      },
      body: JSON.stringify(body),
    });
    const text = await response.text();
    if (!response.ok) {
      let detail = trimBody(text);
      try {
        const parsed = record(JSON.parse(text));
        const code = asString(parsed["__type"] ?? parsed["Code"]);
        const message = asString(parsed["message"] ?? parsed["Message"]);
        detail = code && message ? `${code}: ${message}` : code || message || detail;
      } catch {
        // Preserve bounded response text when AWS did not return JSON.
      }
      throw new Error(`aws ${action}: http ${response.status}: ${detail}`);
    }
    return record(text ? JSON.parse(text) : {});
  }

  private awsQueryErrorMessage(action: string, status: number, text: string): string {
    let detail = "";
    try {
      const parsed = this.parser.parse(text) as unknown;
      const parsedRecord = record(parsed);
      const root = record(
        parsedRecord["ErrorResponse"] ?? parsedRecord["Response"] ?? parsedRecord,
      );
      const error = record(
        items(
          root["Error"] ??
            root["error"] ??
            record(root["Errors"])["Error"] ??
            record(root["Errors"])["error"],
        )[0],
      );
      const code = asString(error["Code"] ?? error["code"]);
      const message = asString(error["Message"] ?? error["message"]);
      detail = code && message ? `${code}: ${message}` : code || message;
    } catch {
      detail = "";
    }
    return `aws ${action}: http ${status}: ${detail || trimBody(text).replace(/\s+/g, " ")}`;
  }

  private async quotaPreflightAttempt(
    serverType: string,
    market: LeaseConfig["capacityMarket"],
    quotaCache: Map<string, number | undefined>,
  ): Promise<ProvisioningAttempt | undefined> {
    const code = awsQuotaCodeForMarket(market);
    let quota = quotaCache.get(code);
    if (!quotaCache.has(code)) {
      quota = await this.appliedServiceQuota(code);
      quotaCache.set(code, quota);
    }
    return awsQuotaPreflightAttempt(serverType, market, this.region, quota);
  }

  private async appliedServiceQuota(quotaCode: string): Promise<number | undefined> {
    await this.ensureExpectedIdentity();
    try {
      const response = await this.serviceQuotas.fetch(this.serviceQuotasEndpoint, {
        method: "POST",
        headers: {
          "content-type": "application/x-amz-json-1.1",
          "x-amz-target": "ServiceQuotasV20190624.GetServiceQuota",
        },
        body: JSON.stringify({ ServiceCode: "ec2", QuotaCode: quotaCode }),
      });
      if (!response.ok) {
        return undefined;
      }
      const parsed = record(await response.json());
      const quota = record(parsed["Quota"]);
      return positiveNumber(quota["Value"]);
    } catch {
      return undefined;
    }
  }

  private async listEC2ServiceQuotas(): Promise<AWSServiceQuota[]> {
    await this.ensureExpectedIdentity();
    const quotas: AWSServiceQuota[] = [];
    let nextToken = "";
    do {
      const body: Record<string, string | number> = { ServiceCode: "ec2", MaxResults: 100 };
      if (nextToken) {
        body["NextToken"] = nextToken;
      }
      // oxlint-disable-next-line eslint/no-await-in-loop -- Service Quotas pagination is token-ordered.
      const response = await this.serviceQuotas.fetch(this.serviceQuotasEndpoint, {
        method: "POST",
        headers: {
          "content-type": "application/x-amz-json-1.1",
          "x-amz-target": "ServiceQuotasV20190624.ListServiceQuotas",
        },
        body: JSON.stringify(body),
      });
      // oxlint-disable-next-line eslint/no-await-in-loop -- response body belongs to the current page.
      const text = await response.text();
      if (!response.ok) {
        throw new Error(`aws ListServiceQuotas: http ${response.status}: ${trimBody(text)}`);
      }
      const parsed = record(JSON.parse(text));
      for (const quota of items(parsed["Quotas"]).map(record)) {
        const out = awsServiceQuotaFromRecord(quota);
        if (out) {
          quotas.push(out);
        }
      }
      nextToken = asString(parsed["NextToken"]);
    } while (nextToken);
    return quotas;
  }

  private async getEC2ServiceQuota(quotaCode: string): Promise<AWSServiceQuota | undefined> {
    await this.ensureExpectedIdentity();
    const response = await this.serviceQuotas.fetch(this.serviceQuotasEndpoint, {
      method: "POST",
      headers: {
        "content-type": "application/x-amz-json-1.1",
        "x-amz-target": "ServiceQuotasV20190624.GetServiceQuota",
      },
      body: JSON.stringify({ ServiceCode: "ec2", QuotaCode: quotaCode }),
    });
    const text = await response.text();
    if (!response.ok) {
      if (text.includes("NoSuchResourceException")) {
        return undefined;
      }
      throw new Error(`aws GetServiceQuota: http ${response.status}: ${trimBody(text)}`);
    }
    const quota = awsServiceQuotaFromRecord(record(JSON.parse(text))["Quota"]);
    if (!quota) {
      throw new Error(`aws GetServiceQuota: missing quota ${quotaCode}`);
    }
    return quota;
  }

  private withRegion(server: ProviderMachine): ProviderMachine {
    return { ...server, region: this.region };
  }
}

function awsSSHCIDRs(config: LeaseConfig, env: Env, allowEmpty = false): string[] {
  const configured = [...config.awsSSHCIDRs, ...(env.CRABBOX_AWS_SSH_CIDRS ?? "").split(",")];
  const cidrs = validatedCIDRs(configured, "CRABBOX_AWS_SSH_CIDRS");
  if (cidrs.length === 0 && !allowEmpty) {
    throw new Error(
      "AWS SSH source CIDR is required; set CRABBOX_AWS_SSH_CIDRS or use Cloudflare request IP forwarding",
    );
  }
  return cidrs;
}

function reservations(root: Record<string, unknown>): Record<string, unknown>[] {
  return items(record(root["reservationSet"])["item"]).map(record);
}

function instanceToMachine(input: unknown): ProviderMachine {
  const instance = record(input);
  const metadataOptions = record(instance["metadataOptions"]);
  const metadataHopLimit = finiteNumber(metadataOptions["httpPutResponseHopLimit"]);
  const ipv6Addresses = uniqueStrings([
    asString(instance["ipv6Address"]),
    ...items(record(instance["networkInterfaceSet"])["item"]).flatMap((networkInterface) =>
      items(record(record(networkInterface)["ipv6AddressesSet"])["item"]).map((address) =>
        asString(record(address)["ipv6Address"]),
      ),
    ),
  ]).filter(Boolean);
  const rootDeviceName = asString(instance["rootDeviceName"]) || "/dev/sda1";
  const rootMapping = items(record(instance["blockDeviceMapping"])["item"])
    .map(record)
    .find((mapping) => asString(mapping["deviceName"]) === rootDeviceName);
  const rootEBS = record(rootMapping?.["ebs"]);
  const tags = tagMap(instance["tagSet"]);
  const cloudID = asString(instance["instanceId"]);
  const securityGroupIDs = items(record(instance["groupSet"])["item"])
    .map((group) => asString(record(group)["groupId"]))
    .filter(Boolean)
    .toSorted();
  return {
    provider: "aws",
    id: 0,
    cloudID,
    name: tags["Name"] || cloudID,
    status: asString(record(instance["instanceState"])["name"]),
    serverType: asString(instance["instanceType"]),
    hostID: asString(record(instance["placement"])["hostId"]),
    host: asString(instance["ipAddress"]),
    ...(asString(instance["privateIpAddress"])
      ? { privateHost: asString(instance["privateIpAddress"]) }
      : {}),
    ...(ipv6Addresses.length > 0 ? { awsIPv6Addresses: ipv6Addresses } : {}),
    ...(asString(instance["keyName"]) ? { awsKeyName: asString(instance["keyName"]) } : {}),
    ...(asString(instance["subnetId"]) ? { awsSubnetID: asString(instance["subnetId"]) } : {}),
    ...(securityGroupIDs.length > 0 ? { awsSecurityGroupIDs: securityGroupIDs } : {}),
    awsInstanceProfileAttached:
      Object.hasOwn(instance, "iamInstanceProfile") && instance["iamInstanceProfile"] != null,
    ...(asString(record(instance["iamInstanceProfile"])["arn"])
      ? { awsInstanceProfileARN: asString(record(instance["iamInstanceProfile"])["arn"]) }
      : {}),
    ...(asString(metadataOptions["httpEndpoint"])
      ? { awsMetadataHttpEndpoint: asString(metadataOptions["httpEndpoint"]) }
      : {}),
    ...(asString(metadataOptions["httpTokens"])
      ? { awsMetadataHttpTokens: asString(metadataOptions["httpTokens"]) }
      : {}),
    ...(metadataHopLimit !== undefined
      ? { awsMetadataHttpPutResponseHopLimit: metadataHopLimit }
      : {}),
    ...(asString(metadataOptions["instanceMetadataTags"])
      ? { awsMetadataInstanceTags: asString(metadataOptions["instanceMetadataTags"]) }
      : {}),
    ...(asString(rootEBS["volumeId"]) ? { awsRootVolumeID: asString(rootEBS["volumeId"]) } : {}),
    ...(asString(rootEBS["deleteOnTermination"])
      ? { awsRootDeleteOnTermination: asString(rootEBS["deleteOnTermination"]) === "true" }
      : {}),
    labels: tags,
  };
}

export function awsMacHostIDFromDescribeHosts(
  root: Record<string, unknown>,
  excludedHostID = "",
  serverType = "",
): string {
  const selectedHost = items(record(root["hostSet"])["item"])
    .map(record)
    .find((host) => {
      const hostID = asString(host["hostId"]);
      return (
        hostID &&
        hostID !== excludedHostID &&
        asString(host["hostState"] ?? host["state"]) === "available" &&
        awsMacHostHasLaunchCapacity(host, serverType)
      );
    });
  return asString(selectedHost?.["hostId"]);
}

function awsMacHostHasLaunchCapacity(host: Record<string, unknown>, serverType: string): boolean {
  const availableCapacity = awsMacHostAvailableCapacity(host, serverType);
  if (availableCapacity !== undefined) {
    return availableCapacity > 0;
  }
  const availableVCpus = finiteNumber(record(host["availableCapacity"])["availableVCpus"]);
  return availableVCpus === undefined || availableVCpus > 0;
}

function awsMacHostAvailableCapacity(
  host: Record<string, unknown>,
  serverType: string,
): number | undefined {
  if (!serverType) {
    return undefined;
  }
  const capacity = record(host["availableCapacity"]);
  const itemsSet = record(capacity["availableInstanceCapacity"]);
  for (const item of items(itemsSet["item"]).map(record)) {
    if (asString(item["instanceType"]) !== serverType) {
      continue;
    }
    return finiteNumber(item["availableCapacity"]);
  }
  return undefined;
}

export function awsHostIDsFromSet(input: unknown): string[] {
  return items(record(input)["item"])
    .map((item) => asString(record(item)["hostId"]) || asString(item))
    .filter(Boolean);
}

export function awsReleaseHostsResult(root: Record<string, unknown>): AWSReleaseHostsResult {
  const unsuccessful = items(record(root["unsuccessful"])["item"])
    .map((item) => {
      const failure = record(item);
      const error = record(failure["error"]);
      return {
        resourceID: asString(failure["resourceId"]),
        code: asString(error["code"]),
        message: asString(error["message"]),
      };
    })
    .filter((failure) => failure.resourceID || failure.code || failure.message);
  return {
    successful: awsHostIDsFromSet(root["successful"]),
    unsuccessful,
  };
}

export function awsMacHostOfferingsFromDescribeInstanceTypeOfferings(
  root: Record<string, unknown>,
  region: string,
): AWSMacHostOffering[] {
  const seen = new Set<string>();
  return items(record(root["instanceTypeOfferingSet"])["item"])
    .map(record)
    .map((item) => ({
      region,
      availabilityZone: asString(item["location"]),
      instanceType: asString(item["instanceType"]),
    }))
    .filter((offering) => {
      if (
        !offering.availabilityZone.startsWith(region) ||
        !offering.instanceType.startsWith("mac") ||
        !offering.instanceType.endsWith(".metal")
      ) {
        return false;
      }
      const key = `${offering.availabilityZone}\0${offering.instanceType}`;
      if (seen.has(key)) {
        return false;
      }
      seen.add(key);
      return true;
    })
    .toSorted((left, right) => {
      const azCompare = left.availabilityZone.localeCompare(right.availabilityZone);
      return azCompare || left.instanceType.localeCompare(right.instanceType);
    });
}

function tagMap(input: unknown): Record<string, string> {
  const out: Record<string, string> = {};
  for (const item of items(record(input)["item"])) {
    const tag = record(item);
    const key = asString(tag["key"]);
    if (key) {
      out[key] = asString(tag["value"]);
    }
  }
  return out;
}

function addTags(
  params: Record<string, string>,
  prefix: string,
  labels: Record<string, string>,
): void {
  Object.entries(labels)
    .toSorted(([left], [right]) => left.localeCompare(right))
    .forEach(([key, value], index) => {
      const tag = index + 1;
      params[`${prefix}.${tag}.Key`] = key;
      params[`${prefix}.${tag}.Value`] = value;
    });
}

export function addRunInstancesTagSpecifications(
  params: Record<string, string>,
  labels: Record<string, string>,
  market: string,
): void {
  params["TagSpecification.1.ResourceType"] = "instance";
  params["TagSpecification.2.ResourceType"] = "volume";
  addTags(params, "TagSpecification.1.Tag", labels);
  addTags(params, "TagSpecification.2.Tag", labels);
  if (market !== "on-demand") {
    params["TagSpecification.3.ResourceType"] = "spot-instances-request";
    addTags(params, "TagSpecification.3.Tag", labels);
  }
}

export async function awsRunInstancesParams(input: {
  config: LeaseConfig;
  leaseID: string;
  imageID: string;
  securityGroupID: string;
  rootGB: number;
  instanceProfile: string;
  subnetID: string;
  labels: Record<string, string>;
}): Promise<Record<string, string>> {
  const { config } = input;
  const params: Record<string, string> = {
    ClientToken: input.leaseID,
    ImageId: input.imageID,
    InstanceType: config.serverType,
    MaxCount: "1",
    MinCount: "1",
    UserData: await awsRunInstancesUserData(config),
    "BlockDeviceMapping.1.DeviceName": "/dev/sda1",
    "BlockDeviceMapping.1.Ebs.DeleteOnTermination": "true",
    "BlockDeviceMapping.1.Ebs.Encrypted": "true",
    "BlockDeviceMapping.1.Ebs.VolumeSize": String(Math.max(1, input.rootGB)),
    "BlockDeviceMapping.1.Ebs.VolumeType": "gp3",
  };
  if (config.awsPrivate) {
    params["MetadataOptions.HttpEndpoint"] = "enabled";
    params["MetadataOptions.HttpPutResponseHopLimit"] = "1";
    params["MetadataOptions.HttpTokens"] = "required";
    params["MetadataOptions.InstanceMetadataTags"] = "disabled";
  } else {
    params["KeyName"] = config.providerKey;
  }
  if (config.capacityMarket !== "on-demand") {
    params["InstanceMarketOptions.MarketType"] = "spot";
    params["InstanceMarketOptions.SpotOptions.InstanceInterruptionBehavior"] = "terminate";
    params["InstanceMarketOptions.SpotOptions.SpotInstanceType"] = "one-time";
  }
  if (input.instanceProfile) {
    params["IamInstanceProfile.Name"] = input.instanceProfile;
  }
  if (input.subnetID) {
    params["NetworkInterface.1.AssociatePublicIpAddress"] = config.awsPrivate ? "false" : "true";
    params["NetworkInterface.1.DeleteOnTermination"] = "true";
    params["NetworkInterface.1.DeviceIndex"] = "0";
    params["NetworkInterface.1.SecurityGroupId.1"] = input.securityGroupID;
    params["NetworkInterface.1.SubnetId"] = input.subnetID;
  } else {
    params["SecurityGroupId.1"] = input.securityGroupID;
  }
  addRunInstancesTagSpecifications(params, input.labels, config.capacityMarket);
  return params;
}

function record(value: unknown): Record<string, unknown> {
  return value && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {};
}

function items(value: unknown): unknown[] {
  if (Array.isArray(value)) {
    return value;
  }
  return value === undefined ? [] : [value];
}

function imageSnapshotIDs(image: Record<string, unknown>): string[] {
  const mappings = items(record(image["blockDeviceMapping"])["item"]);
  const snapshots = mappings
    .map((mapping) => asString(record(record(mapping)["ebs"])["snapshotId"]))
    .filter((snapshotID) => snapshotID !== "");
  return [...new Set(snapshots)];
}

function fastSnapshotRestoreItems(
  root: Record<string, unknown>,
  field: string,
): ProviderFastSnapshotRestore[] {
  return items(record(root[field])["item"]).map((entry) => {
    const item = record(entry);
    const error = record(item["error"]);
    const state = asString(item["state"] ?? error["code"]);
    const stateTransitionReason = asString(item["stateTransitionReason"] ?? error["message"]);
    return {
      snapshotID: asString(item["snapshotId"] ?? item["sourceSnapshotId"]),
      availabilityZone: asString(item["availabilityZone"]),
      ...(state ? { state } : {}),
      ...(stateTransitionReason ? { stateTransitionReason } : {}),
    };
  });
}

function tagValue(resource: Record<string, unknown>, key: string): string {
  for (const tag of items(record(resource["tagSet"])["item"])) {
    const item = record(tag);
    if (asString(item["key"]) === key) return asString(item["value"]);
  }
  return "";
}

function isRetryableSnapshotDeleteError(message: string): boolean {
  return (
    message.includes("InvalidSnapshot.InUse") ||
    message.includes("RequestLimitExceeded") ||
    message.includes("Throttl") ||
    message.includes("ServiceUnavailable") ||
    message.includes("InternalError") ||
    message.includes("http 5") ||
    message.toLowerCase().includes("currently in use")
  );
}

function asString(value: unknown): string {
  if (typeof value === "string") {
    return value;
  }
  if (typeof value === "number" || typeof value === "boolean") {
    return String(value);
  }
  return "";
}

function assignSSHIngressRange(
  params: Record<string, string>,
  cidr: string,
  describe: boolean,
): void {
  const family = sshIngressRangeFamily(ingressFamily(cidr));
  params[family.param] = cidr;
  if (describe) {
    params[family.descriptionParam] = awsSSHIngressDescription;
  }
}

function assignSSHIngressRule(params: Record<string, string>, rule: SSHIngressRule): void {
  params[sshIngressRangeFamily(rule.family).param] = rule.cidr;
}

function ingressFamily(cidr: string): SSHIngressRule["family"] {
  return cidr.includes(":") ? "ipv6" : "ipv4";
}

function sshIngressRangeFamily(family: SSHIngressRule["family"]) {
  return family === "ipv6" ? sshIngressRangeFamilies[1] : sshIngressRangeFamilies[0];
}

export function crabboxSSHIngressRules(group: unknown, ports: string[]): SSHIngressRule[] {
  return sshIngressRules(group, ports)
    .filter((rule) => rule.description === awsSSHIngressDescription)
    .map(stripSSHIngressRuleDescription);
}

export function reclaimableCrabboxSSHIngressRules(
  group: unknown,
  ports: string[],
  cidrs: string[],
): SSHIngressRule[] {
  const desired = new Set(cidrs.map((cidr) => cidr.trim()).filter(Boolean));
  const includeLegacyUnlabeled = isCrabboxManagedSecurityGroup(group);
  return sshIngressRules(group, ports)
    .filter((rule) => {
      if (desired.has(rule.cidr)) {
        return false;
      }
      return (
        rule.description === awsSSHIngressDescription ||
        (includeLegacyUnlabeled && rule.description === "")
      );
    })
    .map(stripSSHIngressRuleDescription);
}

function sshIngressRules(group: unknown, ports: string[]): DescribedSSHIngressRule[] {
  const wantedPorts = new Set(ports);
  const rules: DescribedSSHIngressRule[] = [];
  for (const permission of items(record(record(group)["ipPermissions"])["item"])) {
    const entry = record(permission);
    const protocol = asString(entry["ipProtocol"]);
    const fromPort = asString(entry["fromPort"]);
    const toPort = asString(entry["toPort"]);
    if (protocol !== "tcp" || fromPort !== toPort || !wantedPorts.has(fromPort)) {
      continue;
    }
    for (const family of sshIngressRangeFamilies) {
      for (const range of items(record(entry[family.rangesField])["item"])) {
        const value = record(range);
        rules.push({
          cidr: asString(value[family.cidrField]),
          description: asString(value["description"]),
          family: family.family,
          port: fromPort,
        });
      }
    }
  }
  return rules.filter((rule) => rule.cidr);
}

function stripSSHIngressRuleDescription(rule: DescribedSSHIngressRule): SSHIngressRule {
  return {
    cidr: rule.cidr,
    family: rule.family,
    port: rule.port,
  };
}

export function staleCrabboxSSHIngressRules(
  group: unknown,
  ports: string[],
  cidrs: string[],
): SSHIngressRule[] {
  const desired = new Set(cidrs.map((cidr) => cidr.trim()).filter(Boolean));
  return crabboxSSHIngressRules(group, ports).filter((rule) => !desired.has(rule.cidr));
}

function isCrabboxManagedSecurityGroup(group: unknown): boolean {
  const entry = record(group);
  const tags = tagMap(entry["tagSet"]);
  return (
    asString(entry["groupName"]) === "crabbox-runners" ||
    tags["crabbox"] === "true" ||
    tags["created_by"] === "crabbox"
  );
}

export function isAWSSecurityGroupRuleLimitError(message: string): boolean {
  return message.includes("RulesPerSecurityGroupLimitExceeded");
}

function isAWSInsufficientCapacityOnHostError(message: string): boolean {
  return message.includes("InsufficientCapacityOnHost");
}

export function awsLaunchCandidates(
  config: Pick<
    LeaseConfig,
    "serverType" | "serverTypeExplicit" | "class" | "target" | "windowsMode" | "architecture"
  > &
    Partial<Pick<LeaseConfig, "awsInstanceTypes">>,
): string[] {
  const configuredInstanceTypes = config.awsInstanceTypes ?? [];
  if (configuredInstanceTypes.length > 0) {
    return uniqueStrings(configuredInstanceTypes);
  }
  if (config.serverTypeExplicit) {
    return [config.serverType];
  }
  if (config.target === "macos") {
    return uniqueStrings([
      config.serverType,
      ...awsInstanceTypeCandidatesForTargetClass(
        config.target,
        config.class,
        config.windowsMode,
        config.architecture,
      ),
    ]);
  }
  const policyFallback =
    config.target === "windows"
      ? config.windowsMode === "wsl2"
        ? "m8i.large"
        : "t3.large"
      : config.architecture === "arm64"
        ? "t4g.small"
        : "t3.small";
  return uniqueStrings([
    config.serverType,
    ...awsInstanceTypeCandidatesForTargetClass(
      config.target,
      config.class,
      config.windowsMode,
      config.architecture,
    ),
    policyFallback,
  ]);
}

export function awsRegionCandidates(
  config: Pick<LeaseConfig, "awsRegion" | "capacityRegions"> &
    Partial<
      Pick<
        LeaseConfig,
        "awsPrivate" | "target" | "hostID" | "awsMacHostID" | "awsAMI" | "awsSnapshot"
      >
    >,
  env: Pick<Env, "CRABBOX_AWS_REGION" | "CRABBOX_CAPACITY_REGIONS">,
  preferredRegion = "eu-west-1",
): string[] {
  if (config.awsPrivate) {
    return [requireAWSRegion(config.awsRegion, "awsRegion")];
  }
  if (
    config.target === "macos" &&
    (config.hostID || config.awsMacHostID || config.awsAMI || config.awsSnapshot)
  ) {
    return validAWSRegionCandidates([
      config.awsRegion || preferredRegion || env.CRABBOX_AWS_REGION || "",
    ]);
  }
  return validAWSRegionCandidates([
    preferredRegion,
    config.awsRegion,
    env.CRABBOX_AWS_REGION ?? "",
    ...splitCommaList(env.CRABBOX_CAPACITY_REGIONS ?? ""),
    ...config.capacityRegions,
  ]);
}

function validAWSRegionCandidates(values: string[]): string[] {
  return uniqueStrings(values.map(sanitizeAWSRegion).filter(Boolean));
}

export function awsAvailabilityZoneForRegion(
  config: Pick<LeaseConfig, "capacityAvailabilityZones">,
  env: Pick<Env, "CRABBOX_CAPACITY_AVAILABILITY_ZONES">,
  region: string,
): string {
  return (
    uniqueStrings([
      ...config.capacityAvailabilityZones,
      ...splitCommaList(env.CRABBOX_CAPACITY_AVAILABILITY_ZONES ?? ""),
    ]).find((zone) => zone.startsWith(region)) ?? ""
  );
}

export function applyAWSRunInstanceTargetOptions(
  params: Record<string, string>,
  config: Pick<LeaseConfig, "target" | "windowsMode">,
): void {
  if (config.target === "windows" && config.windowsMode === "wsl2") {
    params["CpuOptions.NestedVirtualization"] = "enabled";
  }
}

export function awsQuotaCodeForMarket(market: string): string {
  return market === "on-demand" ? awsOnDemandQuotaCode : awsSpotQuotaCode;
}

function awsMacHostFamily(serverType: string): string {
  return serverType.split(".")[0]?.toLowerCase() ?? "";
}

function awsMacOSAMIQuery(serverType: string): {
  name: string;
  architecture: "arm64_mac" | "x86_64_mac";
} {
  if (serverType.startsWith("mac1.")) {
    return { name: "amzn-ec2-macos-14.*", architecture: "x86_64_mac" };
  }
  if (serverType.startsWith("mac-m")) {
    return { name: "amzn-ec2-macos-15.*-arm64", architecture: "arm64_mac" };
  }
  return { name: "amzn-ec2-macos-14.*-arm64", architecture: "arm64_mac" };
}

function awsServiceQuotaFromRecord(value: unknown): AWSServiceQuota | undefined {
  const quota = record(value);
  const quotaCode = asString(quota["QuotaCode"]);
  const quotaName = asString(quota["QuotaName"]);
  if (!quotaCode || !quotaName) {
    return undefined;
  }
  const out: AWSServiceQuota = { quotaCode, quotaName };
  const serviceCode = asString(quota["ServiceCode"]);
  const valueNumber = finiteNumber(quota["Value"]);
  const unit = asString(quota["Unit"]);
  if (serviceCode) {
    out.serviceCode = serviceCode;
  }
  if (valueNumber !== undefined) {
    out.value = valueNumber;
  }
  if (typeof quota["Adjustable"] === "boolean") {
    out.adjustable = quota["Adjustable"];
  }
  if (typeof quota["GlobalQuota"] === "boolean") {
    out.globalQuota = quota["GlobalQuota"];
  }
  if (unit) {
    out.unit = unit;
  }
  return out;
}

export function awsInstanceTypeVCPUs(serverType: string): number | undefined {
  const match = /\.([0-9]+)xlarge$/.exec(serverType);
  if (match?.[1]) {
    return Number.parseInt(match[1], 10) * 4;
  }
  if (serverType.endsWith(".xlarge")) {
    return 4;
  }
  if (/\.(nano|micro|small|medium|large)$/.test(serverType)) {
    return 2;
  }
  return undefined;
}

export function awsQuotaPreflightAttempt(
  serverType: string,
  market: string,
  region: string,
  quotaValue: number | undefined,
): ProvisioningAttempt | undefined {
  const needed = awsInstanceTypeVCPUs(serverType);
  if (!needed || quotaValue === undefined || quotaValue >= needed) {
    return undefined;
  }
  const quotaCode = awsQuotaCodeForMarket(market);
  return {
    region,
    serverType,
    market,
    category: "quota",
    message: `quota ${quotaCode} in ${region} is ${quotaValue} vCPUs; ${serverType} needs ${needed} vCPUs`,
  };
}

type AWSCapacityReadinessConfig = Pick<
  LeaseConfig,
  | "target"
  | "windowsMode"
  | "architecture"
  | "class"
  | "serverType"
  | "capacityMarket"
  | "capacityFallback"
>;

export function awsCapacityReadinessCheckForQuota(
  config: AWSCapacityReadinessConfig,
  market: string,
  region: string,
  quotaValue: number | undefined,
): AWSCapacityReadinessCheck {
  const quotaCode = awsQuotaCodeForMarket(market);
  const needed = awsInstanceTypeVCPUs(config.serverType);
  const details: Record<string, string> = {
    provider: "aws",
    market,
    region,
    quota_code: quotaCode,
    default_class: config.class,
    default_type: config.serverType,
    default_needed_vcpus: String(needed ?? 0),
  };
  if (quotaValue === undefined) {
    details["hint"] = "servicequotas_unavailable_or_forbidden";
    return {
      status: "skip",
      check: "capacity",
      message: awsCapacityReadinessMessage("provider=aws capacity=unknown", details),
      details,
    };
  }
  details["limit_vcpus"] = String(Math.trunc(quotaValue));
  if (!needed) {
    details["hint"] = "unknown_instance_vcpus";
    return {
      status: "skip",
      check: "capacity",
      message: awsCapacityReadinessMessage("provider=aws capacity=unknown", details),
      details,
    };
  }
  if (quotaValue < needed) {
    const recommendation = awsRecommendedClassForQuota(config, Math.trunc(quotaValue));
    if (recommendation) {
      details["recommended_class"] = recommendation.machineClass;
      details["recommended_type"] = recommendation.serverType;
    }
    details["hint"] = "lower_class_or_request_quota";
    return {
      status: "warning",
      check: "capacity",
      message: awsCapacityReadinessMessage("provider=aws capacity=quota_pressure", details),
      details,
    };
  }
  details["hint"] = "quota_satisfies_default_class";
  return {
    status: "ok",
    check: "capacity",
    message: awsCapacityReadinessMessage("provider=aws capacity=ready", details),
    details,
  };
}

function awsCapacityReadinessMarkets(config: AWSCapacityReadinessConfig): string[] {
  const markets = [config.capacityMarket || "spot"];
  if (config.capacityMarket === "spot" && config.capacityFallback.startsWith("on-demand")) {
    markets.push("on-demand");
  }
  return uniqueStrings(markets);
}

function awsCapacityReadinessMessage(prefix: string, details: Record<string, string>): string {
  const suffix = Object.entries(details)
    .filter(([key, value]) => key !== "provider" && value)
    .toSorted(([left], [right]) => left.localeCompare(right))
    .map(([key, value]) => `${key}=${value.replaceAll(" ", "_")}`)
    .join(" ");
  return suffix ? `${prefix} ${suffix}` : prefix;
}

function awsRecommendedClassForQuota(
  config: Pick<LeaseConfig, "target" | "windowsMode" | "architecture">,
  limitVCPUs: number,
): { machineClass: string; serverType: string } | undefined {
  if (limitVCPUs <= 0) {
    return undefined;
  }
  for (const machineClass of ["beast", "large", "fast", "standard"]) {
    const [serverType] = awsInstanceTypeCandidatesForTargetClass(
      config.target,
      machineClass,
      config.windowsMode,
      config.architecture,
    );
    if (serverType && (awsInstanceTypeVCPUs(serverType) ?? 0) <= limitVCPUs) {
      return { machineClass, serverType };
    }
  }
  for (const serverType of awsInstanceTypeCandidatesForTargetClass(
    config.target,
    "standard",
    config.windowsMode,
    config.architecture,
  )) {
    if ((awsInstanceTypeVCPUs(serverType) ?? 0) <= limitVCPUs) {
      return { machineClass: "standard", serverType };
    }
  }
  return undefined;
}

function uniqueStrings(values: string[]): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  for (const value of values) {
    const normalized = value.trim();
    if (normalized && !seen.has(normalized)) {
      seen.add(normalized);
      out.push(normalized);
    }
  }
  return out;
}

function splitCommaList(value: string): string[] {
  return value
    .split(",")
    .map((item) => item.trim())
    .filter(Boolean);
}

function shellQuote(value: string): string {
  return `'${value.replaceAll("'", `'"'"'`)}'`;
}

function positiveInt(value: string | undefined): number {
  if (!value) {
    return 0;
  }
  const parsed = Number.parseInt(value, 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : 0;
}

function positiveFloat(value: string): number | undefined {
  const parsed = Number.parseFloat(value);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : undefined;
}

function positiveNumber(value: unknown): number | undefined {
  const parsed = typeof value === "number" ? value : Number.parseFloat(String(value));
  return Number.isFinite(parsed) && parsed > 0 ? parsed : undefined;
}

function finiteNumber(value: unknown): number | undefined {
  const parsed = typeof value === "number" ? value : Number.parseFloat(String(value));
  return Number.isFinite(parsed) ? parsed : undefined;
}

const opaqueAWSHTTP400XMLDeclaration = '<?xml version="1.0" encoding="UTF-8"?>';

export function awsProvisioningErrorCategory(message: string): string {
  if (message.includes("no available EC2 Mac Dedicated Host")) {
    return "capacity";
  }
  if (message.includes("no AWS AMI found")) {
    return "region";
  }
  if (
    message.includes("InsufficientInstanceCapacity") ||
    isAWSInsufficientCapacityOnHostError(message)
  ) {
    return "capacity";
  }
  if (message.includes("MaxSpotInstanceCountExceeded") || message.includes("VcpuLimitExceeded")) {
    return "quota";
  }
  if (message.includes("Unsupported") || message.includes("InvalidParameterValue")) {
    return "unsupported";
  }
  if (
    message.includes("InvalidParameterCombination") &&
    (message.includes("Free Tier") ||
      message.includes("eligible") ||
      message.includes("InstanceType") ||
      message.includes("instance type"))
  ) {
    return "policy";
  }
  if (isOpaqueAWSHTTP400XMLOnlyError(message)) {
    return "capacity";
  }
  return "";
}

function isOpaqueAWSHTTP400XMLOnlyError(message: string): boolean {
  const entries = message
    .split(";")
    .map((entry) => entry.trim())
    .filter(Boolean);
  return entries.length > 0 && entries.every(isOpaqueAWSHTTP400XMLEntry);
}

function isOpaqueAWSHTTP400XMLEntry(entry: string): boolean {
  const awsIndex = entry.indexOf("aws ");
  if (awsIndex < 0) {
    return false;
  }
  if (awsIndex > 0 && !entry.slice(0, awsIndex).trimEnd().endsWith(":")) {
    return false;
  }

  const payload = entry.slice(awsIndex);
  const separator = ": http 400: ";
  const separatorIndex = payload.indexOf(separator);
  if (separatorIndex <= "aws ".length) {
    return false;
  }
  const operation = payload.slice("aws ".length, separatorIndex);
  if (!isAWSOperationName(operation)) {
    return false;
  }
  return payload.slice(separatorIndex + separator.length).trim() === opaqueAWSHTTP400XMLDeclaration;
}

function isAWSOperationName(value: string): boolean {
  if (!value) {
    return false;
  }
  for (const char of value) {
    const code = char.charCodeAt(0);
    const isUpper = code >= 65 && code <= 90;
    const isLower = code >= 97 && code <= 122;
    if (!isUpper && !isLower) {
      return false;
    }
  }
  return true;
}

export function isRetryableAWSProvisioningError(message: string): boolean {
  return awsProvisioningErrorCategory(message) !== "";
}

export function isAWSInstanceNotFoundError(message: string): boolean {
  return message.includes("InvalidInstanceID.NotFound");
}

export function isAWSManagedNodePendingError(message: string): boolean {
  return message.includes("InvalidInstanceId");
}

export function isAWSRunInstancesOutcomeUncertain(message: string): boolean {
  return message.includes(awsRunInstancesOutcomeUncertain);
}

export function isAWSInvalidHostIDError(message: string): boolean {
  return message.includes("InvalidHostID.NotFound");
}

export function isAWSInstanceCleanedAfterReadinessFailure(
  waitMessage: string,
  cleanupMessage: string,
): boolean {
  if (cleanupMessage === "") {
    return true;
  }
  return isAWSInstanceNotFoundError(waitMessage) && isAWSInstanceNotFoundError(cleanupMessage);
}

function trimBody(text: string): string {
  return text.length > 500 ? `${text.slice(0, 500)}...` : text;
}

function conciseAWSProvisioningMessage(message: string): string {
  const code = /<Code>([^<]+)<\/Code>/.exec(message)?.[1] ?? "";
  const detail = /<Message>([^<]+)<\/Message>/.exec(message)?.[1] ?? "";
  if (code && detail) {
    return `${code}: ${detail}`;
  }
  return trimBody(message).replace(/\s+/g, " ");
}

function conciseAWSMacHostDryRunMessage(message: string): string {
  const code = /<Code>([^<]+)<\/Code>/.exec(message)?.[1] ?? "";
  if (code === "DryRunOperation" || message.includes("DryRunOperation")) {
    return "DryRunOperation: request would have succeeded";
  }
  if (code === "UnauthorizedOperation" || message.includes("UnauthorizedOperation")) {
    return "UnauthorizedOperation: coordinator AWS identity needs EC2 Mac host lifecycle permissions, including ec2:AllocateHosts and ec2:CreateTags";
  }
  if (code) {
    return code;
  }
  if (message.includes("Encoded authorization failure") || message.includes("arn:aws:iam::")) {
    return "AWS authorization failure: details omitted";
  }
  return trimBody(message).replace(/\s+/g, " ");
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
