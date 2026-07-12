export interface AWSCredentials {
  accessKeyId: string;
  secretAccessKey: string;
  sessionToken?: string;
  expiration?: Date;
}

export type AWSCredentialProvider = () => Promise<AWSCredentials>;

export interface Env {
  FLEET: DurableObjectNamespace;
  HETZNER_TOKEN: string;
  awsCredentialProvider?: AWSCredentialProvider;
  AWS_ACCESS_KEY_ID?: string;
  AWS_SECRET_ACCESS_KEY?: string;
  AWS_SESSION_TOKEN?: string;
  AWS_PROFILE?: string;
  AWS_CONFIG_FILE?: string;
  AWS_SHARED_CREDENTIALS_FILE?: string;
  AWS_ROLE_ARN?: string;
  AWS_WEB_IDENTITY_TOKEN_FILE?: string;
  AWS_REGION?: string;
  AWS_DEFAULT_REGION?: string;
  AWS_CONTAINER_CREDENTIALS_RELATIVE_URI?: string;
  AWS_CONTAINER_CREDENTIALS_FULL_URI?: string;
  ECS_CONTAINER_METADATA_URI_V4?: string;
  CRABBOX_AWS_REGION?: string;
  CRABBOX_AWS_EXPECTED_ACCOUNT_ID?: string;
  CRABBOX_AWS_EXPECTED_REGION?: string;
  CRABBOX_AWS_EXPECTED_TASK_ROLE_NAME?: string;
  CRABBOX_AWS_REQUIRE_ECS_TASK?: string;
  CRABBOX_AWS_AMI?: string;
  CRABBOX_AWS_SECURITY_GROUP_ID?: string;
  CRABBOX_AWS_SUBNET_ID?: string;
  CRABBOX_AWS_INSTANCE_PROFILE?: string;
  CRABBOX_AWS_ROOT_GB?: string;
  CRABBOX_AWS_SSH_CIDRS?: string;
  CRABBOX_AWS_FAST_SNAPSHOT_RESTORE_AZS?: string;
  CRABBOX_HOST_ID?: string;
  CRABBOX_AWS_MAC_HOST_ID?: string;
  CRABBOX_AWS_ORPHAN_SWEEP_ENABLED?: string;
  CRABBOX_AWS_ORPHAN_SWEEP_DELETE?: string;
  CRABBOX_AWS_ORPHAN_SWEEP_INTERVAL_SECONDS?: string;
  CRABBOX_AWS_ORPHAN_SWEEP_GRACE_SECONDS?: string;
  CRABBOX_AWS_MAC_HOST_SWEEP_RELEASE?: string;
  CRABBOX_CAPACITY_REGIONS?: string;
  CRABBOX_CAPACITY_AVAILABILITY_ZONES?: string;
  CRABBOX_CAPACITY_HINTS?: string;
  CRABBOX_CAPACITY_LARGE_CLASSES?: string;
  AZURE_TENANT_ID?: string;
  AZURE_CLIENT_ID?: string;
  AZURE_CLIENT_SECRET?: string;
  AZURE_SUBSCRIPTION_ID?: string;
  CRABBOX_AZURE_LOCATION?: string;
  CRABBOX_AZURE_REGIONS?: string;
  CRABBOX_AZURE_RESOURCE_GROUP?: string;
  CRABBOX_AZURE_IMAGE?: string;
  CRABBOX_AZURE_WINDOWS_ARM64_IMAGE?: string;
  CRABBOX_AZURE_OS_DISK?: string;
  CRABBOX_AZURE_VNET?: string;
  CRABBOX_AZURE_SUBNET?: string;
  CRABBOX_AZURE_NSG?: string;
  CRABBOX_AZURE_SSH_CIDRS?: string;
  CRABBOX_AZURE_ORPHAN_SWEEP_ENABLED?: string;
  CRABBOX_AZURE_ORPHAN_SWEEP_DELETE?: string;
  CRABBOX_AZURE_ORPHAN_SWEEP_INTERVAL_SECONDS?: string;
  CRABBOX_AZURE_ORPHAN_SWEEP_GRACE_SECONDS?: string;
  GCP_PROJECT_ID?: string;
  GCP_CLIENT_EMAIL?: string;
  GCP_PRIVATE_KEY?: string;
  CRABBOX_GCP_PROJECT?: string;
  CRABBOX_GCP_ZONE?: string;
  CRABBOX_GCP_IMAGE?: string;
  CRABBOX_GCP_NETWORK?: string;
  CRABBOX_GCP_SUBNET?: string;
  CRABBOX_GCP_TAGS?: string;
  CRABBOX_GCP_SSH_CIDRS?: string;
  CRABBOX_GCP_ROOT_GB?: string;
  CRABBOX_GCP_SERVICE_ACCOUNT?: string;
  CRABBOX_GCP_CREDENTIAL_SOURCE?: string;
  DAYTONA_CRABBOX_KEY?: string;
  CRABBOX_DAYTONA_API_URL?: string;
  CRABBOX_DAYTONA_ORGANIZATION_ID?: string;
  CRABBOX_DAYTONA_SNAPSHOT?: string;
  CRABBOX_DAYTONA_TARGET?: string;
  CRABBOX_DAYTONA_USER?: string;
  CRABBOX_DAYTONA_WORK_ROOT?: string;
  CRABBOX_DAYTONA_SSH_GATEWAY_HOST?: string;
  CRABBOX_DAYTONA_SSH_ACCESS_MINUTES?: string;
  CRABBOX_RUNTIME_ADAPTER_TOKEN?: string;
  CRABBOX_SHARED_TOKEN?: string;
  CRABBOX_SHARED_OWNER?: string;
  CRABBOX_ADMIN_TOKEN?: string;
  CRABBOX_SESSION_SECRET?: string;
  CRABBOX_USER_TOKEN_TTL_SECONDS?: string;
  CRABBOX_RUN_RETENTION_DAYS?: string;
  CRABBOX_GITHUB_CLIENT_ID?: string;
  CRABBOX_GITHUB_CLIENT_SECRET?: string;
  CRABBOX_GITHUB_ALLOWED_ORG?: string;
  CRABBOX_GITHUB_ALLOWED_ORGS?: string;
  CRABBOX_GITHUB_ALLOWED_TEAM?: string;
  CRABBOX_GITHUB_ALLOWED_TEAMS?: string;
  CRABBOX_GITHUB_REVOKED_USERS?: string;
  CRABBOX_GITHUB_MEMBERSHIP_CACHE_SECONDS?: string;
  CRABBOX_GITHUB_ADMIN_OWNERS?: string;
  CRABBOX_GITHUB_ADMIN_LOGINS?: string;
  CRABBOX_PUBLIC_URL?: string;
  CRABBOX_CODE_ORIGIN_TEMPLATE?: string;
  CRABBOX_ALLOW_QUERY_BRIDGE_TICKETS?: string;
  CRABBOX_WORKSPACE_PROVIDER?: string;
  CRABBOX_WORKSPACE_CLASS?: string;
  CRABBOX_WORKSPACE_PREWARM_COUNT?: string;
  CRABBOX_WORKSPACE_SSH_PUBLIC_KEY?: string;
  CRABBOX_WORKSPACE_SSH_PRIVATE_KEY?: string;
  CRABBOX_WORKSPACE_AWS_PRIVATE?: string;
  CRABBOX_WORKSPACE_AWS_INSTANCE_TYPES?: string;
  CRABBOX_WORKSPACE_AWS_MAX_VCPUS?: string;
  CRABBOX_WORKSPACE_AWS_MAX_MEMORY_MIB?: string;
  CRABBOX_WORKSPACE_AWS_ROOT_GB?: string;
  CRABBOX_WORKSPACE_AWS_SUBNET_ID?: string;
  CRABBOX_WORKSPACE_AWS_SECURITY_GROUP_ID?: string;
  CRABBOX_WORKSPACE_AWS_CONTROLLER_SECURITY_GROUP_ID?: string;
  CRABBOX_WORKSPACE_AWS_INSTANCE_PROFILE?: string;
  CRABBOX_WORKSPACE_AWS_MARKET?: string;
  CRABBOX_WORKSPACE_AWS_SSM_LOG_GROUP?: string;
  CRABBOX_RUNTIME_ADAPTER_OWNER?: string;
  CRABBOX_RUNTIME_ADAPTER_ORG?: string;
  CRABBOX_DEFAULT_ORG?: string;
  CRABBOX_ACCESS_TEAM_DOMAIN?: string;
  CRABBOX_ACCESS_AUD?: string;
  CRABBOX_TRUSTED_USER_HEADER?: string;
  CRABBOX_TRUSTED_USER_ORG?: string;
  CRABBOX_TRUSTED_PROXY_CIDRS?: string;
  CRABBOX_TRUSTED_PROXY_SECRET?: string;
  CRABBOX_COST_RATES_JSON?: string;
  CRABBOX_EUR_TO_USD?: string;
  CRABBOX_MARKETPLACE_ENABLED?: string;
  CRABBOX_MARKETPLACE_REQUIRE_CREDITS?: string;
  CRABBOX_MARKETPLACE_ALLOWED_PROVIDERS?: string;
  CRABBOX_MARKETPLACE_RATE_CARD_JSON?: string;
  CRABBOX_MARKETPLACE_MARKUP_BPS?: string;
  CRABBOX_MARKETPLACE_BIDDING_ENABLED?: string;
  CRABBOX_MARKETPLACE_PAYMENT_PROVIDER?: string;
  CRABBOX_MARKETPLACE_LEDGER_PROVIDER?: string;
  CRABBOX_MAX_ACTIVE_LEASES?: string;
  CRABBOX_MAX_ACTIVE_LEASES_PER_OWNER?: string;
  CRABBOX_MAX_ACTIVE_LEASES_PER_ORG?: string;
  CRABBOX_CAPACITY_ADMIN_OWNERS?: string;
  CRABBOX_MAX_ACTIVE_LEASES_PER_CAPACITY_ADMIN?: string;
  CRABBOX_MAX_MONTHLY_USD?: string;
  CRABBOX_MAX_MONTHLY_USD_PER_OWNER?: string;
  CRABBOX_MAX_MONTHLY_USD_PER_ORG?: string;
  CRABBOX_TAILSCALE_ENABLED?: string;
  CRABBOX_TAILSCALE_CLIENT_ID?: string;
  CRABBOX_TAILSCALE_CLIENT_SECRET?: string;
  CRABBOX_TAILSCALE_TAILNET?: string;
  CRABBOX_TAILSCALE_TAGS?: string;
  CRABBOX_TAILSCALE_INSTALL_MODE?: string;
  CRABBOX_TAILSCALE_VERSION?: string;
  CRABBOX_TAILSCALE_SHA256_AMD64?: string;
  CRABBOX_TAILSCALE_SHA256_ARM64?: string;
  CRABBOX_ARTIFACTS_BACKEND?: string;
  CRABBOX_ARTIFACTS_BUCKET?: string;
  CRABBOX_ARTIFACTS_PREFIX?: string;
  CRABBOX_ARTIFACTS_BASE_URL?: string;
  CRABBOX_ARTIFACTS_PUBLIC_READS?: string;
  CRABBOX_ARTIFACTS_REGION?: string;
  CRABBOX_ARTIFACTS_ENDPOINT_URL?: string;
  CRABBOX_ARTIFACTS_ACCESS_KEY_ID?: string;
  CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY?: string;
  CRABBOX_ARTIFACTS_SESSION_TOKEN?: string;
  CRABBOX_ARTIFACTS_UPLOAD_EXPIRES_SECONDS?: string;
  CRABBOX_ARTIFACTS_URL_EXPIRES_SECONDS?: string;
}

export interface LeaseRequest {
  leaseID?: string;
  slug?: string;
  requestedSlug?: string;
  provider?: Provider;
  target?: TargetOS;
  targetOS?: TargetOS;
  architecture?: string;
  os?: string;
  windowsMode?: WindowsMode;
  desktop?: boolean;
  desktopEnv?: string;
  browser?: boolean;
  imageRequirements?: ImageRequirements;
  code?: boolean;
  tailscale?: boolean;
  tailscaleTags?: string[];
  tailscaleHostname?: string;
  tailscaleExitNode?: string;
  tailscaleExitNodeAllowLanAccess?: boolean;
  profile?: string;
  class?: string;
  serverType?: string;
  serverTypeExplicit?: boolean;
  hostId?: string;
  hostID?: string;
  location?: string;
  image?: string;
  awsRegion?: string;
  awsAMI?: string;
  awsSnapshot?: string;
  awsSGID?: string;
  awsSubnetID?: string;
  awsProfile?: string;
  awsRootGB?: number;
  awsInstanceTypes?: string[];
  awsPrivate?: boolean;
  awsRequireSSM?: boolean;
  awsSSMBootstrapCommand?: string;
  awsSSMLogGroup?: string;
  awsSSHCIDRs?: string[];
  awsSSHCIDRsPinned?: boolean;
  awsMacHostID?: string;
  azureLocation?: string;
  azureImage?: string;
  azureSnapshot?: string;
  azureOSDisk?: string;
  gcpProject?: string;
  gcpZone?: string;
  gcpImage?: string;
  gcpMachineImage?: string;
  gcpSnapshot?: string;
  gcpNetwork?: string;
  gcpSubnet?: string;
  gcpTags?: string[];
  gcpSSHCIDRs?: string[];
  gcpRootGB?: number;
  gcpServiceAccount?: string;
  capacity?: {
    market?: "spot" | "on-demand";
    strategy?: "most-available" | "price-capacity-optimized" | "capacity-optimized" | "sequential";
    fallback?: string;
    regions?: string[];
    availabilityZones?: string[];
    hints?: boolean;
  };
  sshUser?: string;
  sshPort?: string;
  sshFallbackPorts?: string[];
  providerKey?: string;
  workRoot?: string;
  ttlSeconds?: number;
  idleTimeoutSeconds?: number;
  keep?: boolean;
  sshPublicKey?: string;
  pond?: string;
  exposedPorts?: string[];
}

export interface ImageCapabilities {
  osVersion?: string;
  sdks?: Record<string, string>;
  runtimes?: Record<string, string>;
  browser?: boolean;
  webview2?: boolean;
  desktop?: boolean;
}

export interface ImageRequirements {
  minOS?: string;
  sdks?: Record<string, string>;
  runtimes?: Record<string, string>;
  browser?: boolean;
  webview2?: boolean;
  desktop?: boolean;
}

export interface LeaseRegistrationRequest {
  slug?: string;
  provider?: string;
  target?: TargetOS;
  windowsMode?: WindowsMode;
  desktop?: boolean;
  desktopEnv?: string;
  browser?: boolean;
  code?: boolean;
  cloudID?: string;
  serverID?: number;
  serverName?: string;
  serverType?: string;
  host?: string;
  sshUser?: string;
  sshPort?: string;
  sshFallbackPorts?: string[];
  workRoot?: string;
  profile?: string;
  class?: string;
  pond?: string;
  exposedPorts?: string[];
  ttlSeconds?: number;
  idleTimeoutSeconds?: number;
  runtimeAdapterID?: string;
  runtimeAdapterWorkspaceID?: string;
  runtimeAdapterRegistrationID?: string;
}

export const coordinatorProviderRegistry = [
  {
    provider: "hetzner",
    label: "Hetzner",
    requiredSecrets: ["HETZNER_TOKEN"],
    adminAudit: false,
  },
  {
    provider: "aws",
    label: "AWS",
    requiredSecrets: ["AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"],
    adminAudit: true,
  },
  {
    provider: "azure",
    label: "Azure",
    requiredSecrets: [
      "AZURE_TENANT_ID",
      "AZURE_CLIENT_ID",
      "AZURE_CLIENT_SECRET",
      "AZURE_SUBSCRIPTION_ID",
    ],
    adminAudit: true,
  },
  {
    provider: "gcp",
    label: "GCP",
    requiredSecrets: ["GCP_CLIENT_EMAIL", "GCP_PRIVATE_KEY"],
    adminAudit: false,
  },
  {
    provider: "daytona",
    label: "Daytona",
    requiredSecrets: ["DAYTONA_CRABBOX_KEY"],
    adminAudit: false,
  },
] as const satisfies readonly {
  provider: string;
  label: string;
  requiredSecrets: readonly (keyof Env)[];
  adminAudit: boolean;
}[];

export type CoordinatorProviderSpec = (typeof coordinatorProviderRegistry)[number];
export type Provider = CoordinatorProviderSpec["provider"];
export const coordinatorProviders = coordinatorProviderRegistry.map(
  (spec) => spec.provider,
) as Provider[];
export const providers = coordinatorProviders;

export function isCoordinatorProvider(provider: string): provider is Provider {
  return coordinatorProviders.includes(provider as Provider);
}

export function coordinatorProviderSpec(provider: Provider): CoordinatorProviderSpec {
  return coordinatorProviderRegistry.find((spec) => spec.provider === provider)!;
}

export type TargetOS = "linux" | "macos" | "windows";
export type WindowsMode = "normal" | "wsl2";
export type LeaseLifecycle = "managed" | "registered";

export interface LeaseTelemetry {
  capturedAt: string;
  source?: string;
  load1?: number;
  load5?: number;
  load15?: number;
  cpuCount?: number;
  memoryUsedBytes?: number;
  memoryTotalBytes?: number;
  memoryPercent?: number;
  diskUsedBytes?: number;
  diskTotalBytes?: number;
  diskPercent?: number;
  uptimeSeconds?: number;
}

export interface RunTelemetrySummary {
  start?: LeaseTelemetry;
  end?: LeaseTelemetry;
  samples?: LeaseTelemetry[];
}

export interface LeaseRecord {
  id: string;
  slug?: string;
  workspaceID?: string;
  provider: string;
  lifecycle?: LeaseLifecycle;
  runtimeAdapterID?: string;
  runtimeAdapterWorkspaceID?: string;
  runtimeAdapterRegistrationID?: string;
  runtimeAdapterDeleteRequestedAt?: string;
  runtimeAdapterDeleteClaimID?: string;
  runtimeAdapterDeleteRetryAt?: string;
  runtimeAdapterDeleteDispatchUntil?: string;
  runtimeAdapterDeleteAttempts?: number;
  runtimeAdapterDeleteError?: string;
  target: TargetOS;
  os?: string;
  windowsMode?: WindowsMode;
  desktop?: boolean;
  desktopEnv?: string;
  browser?: boolean;
  code?: boolean;
  tailscale?: TailscaleMetadata;
  cloudID: string;
  region?: string;
  providerProject?: string;
  providerScope?: string;
  providerOwner?: string;
  network?: LeaseNetworkState;
  owner: string;
  org: string;
  share?: LeaseShare | undefined;
  profile: string;
  class: string;
  serverType: string;
  requestedServerType?: string;
  pond?: string;
  exposedPorts?: string[];
  hostId?: string;
  hostID?: string;
  market?: string;
  provisioningAttempts?: ProvisioningAttempt[];
  awsSSMCommandID?: string;
  awsSSMCommandStatus?: string;
  awsSSMLogGroup?: string;
  capacityHints?: CapacityHint[];
  serverID: number;
  serverName: string;
  providerKey: string;
  providerKeyCleanupOwned?: boolean;
  providerKeyCleanupPending?: boolean;
  providerKeyCleanupID?: string;
  host: string;
  sshUser: string;
  sshPort: string;
  sshFallbackPorts?: string[];
  sshHostKey?: string;
  providerAccessExpiresAt?: string;
  workRoot: string;
  keep: boolean;
  ttlSeconds: number;
  idleTimeoutSeconds?: number;
  estimatedHourlyUSD: number;
  maxEstimatedUSD: number;
  state: "provisioning" | "active" | "released" | "expired" | "failed";
  createdAt: string;
  updatedAt: string;
  lastTouchedAt?: string;
  expiresAt: string;
  telemetry?: LeaseTelemetry;
  telemetryHistory?: LeaseTelemetry[];
  cleanupAttempts?: number;
  cleanupError?: string;
  cleanupFailedAt?: string;
  cleanupRetryAt?: string;
  cleanupStartedAt?: string;
  cleanupClaimExpiresAt?: string;
  failureError?: string;
  provisioningResourceMayExist?: boolean;
  provisioningFailureRetryable?: boolean;
  provisioningRequestStartedAt?: string;
  releaseDeletesServer?: boolean;
  releasedAt?: string;
  endedAt?: string;
  registeredAt?: string;
}

export type ReadyPoolEntryState = "ready" | "busy" | "draining" | "stale";

export interface ReadyPoolEntry {
  key: string;
  leaseID: string;
  state: ReadyPoolEntryState;
  owner: string;
  org: string;
  repo?: string;
  ref?: string;
  commit?: string;
  fingerprint?: string;
  image?: string;
  provider?: string;
  target?: TargetOS;
  windowsMode?: WindowsMode;
  class?: string;
  serverType?: string;
  sshHost?: string;
  sshUser?: string;
  sshPort?: string;
  workRoot?: string;
  borrowedBy?: string;
  borrowedAt?: string;
  borrowToken?: string;
  lastReadyAt?: string;
  lastUsedAt?: string;
  lastResult?: string;
  failureCount?: number;
  createdAt: string;
  updatedAt: string;
  expiresAt: string;
}

export interface ReadyPoolRegisterRequest {
  leaseID?: string;
  repo?: string;
  ref?: string;
  commit?: string;
  fingerprint?: string;
  image?: string;
  sshHost?: string;
  sshUser?: string;
  sshPort?: string;
  workRoot?: string;
}

export interface ReadyPoolBorrowRequest {
  repo?: string;
  ref?: string;
  commit?: string;
  allowMissingCommit?: boolean;
  fingerprint?: string;
  provider?: Provider;
  target?: TargetOS;
}

export interface ReadyPoolReturnRequest {
  leaseID?: string;
  result?: "ready" | "drain" | "release";
  reason?: string;
  borrowToken?: string;
}

export interface LeaseNetworkState {
  sshSourceCIDRs?: string[];
  sshPinnedSourceCIDRs?: string[];
  sshSourceCIDRsComplete?: boolean;
  awsSecurityGroupID?: string;
  awsSecurityGroupName?: string;
  awsSubnetID?: string;
  awsPrivate?: boolean;
}

export type LeaseShareRole = "use" | "manage";

export interface LeaseShare {
  users?: Record<string, LeaseShareRole> | undefined;
  org?: LeaseShareRole | undefined;
  updatedAt?: string | undefined;
  updatedBy?: string | undefined;
}

export interface TailscaleMetadata {
  enabled: boolean;
  hostname?: string;
  fqdn?: string;
  ipv4?: string;
  tags?: string[];
  state?: "requested" | "ready" | "failed";
  error?: string;
  version?: string;
  deviceID?: string;
  exitNode?: string;
  exitNodeAllowLanAccess?: boolean;
}

export interface ProvisioningAttempt {
  region?: string;
  serverType: string;
  market?: string;
  category?: string;
  message: string;
}

export interface CapacityHint {
  code: string;
  message: string;
  action?: string;
  region?: string;
  market?: string;
  class?: string;
  serverType?: string;
  regionsTried?: string[];
}

export interface ProviderImage {
  id: string;
  name: string;
  state: string;
  provider?: Provider;
  kind?: string;
  region?: string;
  target?: TargetOS;
  os?: string;
  windowsMode?: WindowsMode;
  serverType?: string;
  architecture?: string;
  project?: string;
  resourceID?: string;
  snapshots?: string[];
  fastSnapshotRestores?: ProviderFastSnapshotRestore[];
  capabilities?: ImageCapabilities;
}

export interface ProviderFastSnapshotRestore {
  snapshotID: string;
  availabilityZone: string;
  state?: string;
  stateTransitionReason?: string;
}

export interface PromotedImageRecord extends ProviderImage {
  promotedAt: string;
}

export interface RunRecord {
  id: string;
  leaseID: string;
  leaseIDs?: string[];
  slug?: string;
  owner: string;
  org: string;
  leaseOwners?: Array<{ owner: string; org: string }>;
  provider: string;
  target?: TargetOS;
  windowsMode?: WindowsMode;
  class: string;
  serverType: string;
  command: string[];
  label?: string;
  state: "running" | "succeeded" | "failed";
  phase?: string;
  exitCode?: number;
  syncMs?: number;
  commandMs?: number;
  durationMs?: number;
  logBytes: number;
  logTruncated: boolean;
  blockedStage?: string;
  retryLikely?: string;
  results?: TestResultSummary;
  telemetry?: RunTelemetrySummary;
  startedAt: string;
  lastEventAt?: string;
  eventCount?: number;
  endedAt?: string;
}

export interface RunCreateRequest {
  leaseID?: string;
  provider?: string;
  target?: TargetOS;
  windowsMode?: WindowsMode;
  class?: string;
  serverType?: string;
  command?: string[];
  label?: string;
}

export interface RunFinishRequest {
  exitCode: number;
  syncMs?: number;
  commandMs?: number;
  log?: string;
  logChunks?: string[];
  logTruncated?: boolean;
  blockedStage?: string;
  retryLikely?: string;
  results?: TestResultSummary;
  telemetry?: RunTelemetrySummary;
}

export interface RunTelemetryRequest {
  telemetry?: Partial<LeaseTelemetry>;
}

export interface ExternalRunnerInput {
  id?: string;
  provider?: string;
  status?: string;
  repo?: string;
  workflow?: string;
  job?: string;
  ref?: string;
  createdAt?: string;
  actionsRepo?: string;
  actionsRunID?: string;
  actionsRunURL?: string;
  actionsRunStatus?: string;
  actionsRunConclusion?: string;
  actionsWorkflowName?: string;
  actionsWorkflowURL?: string;
}

export interface ExternalRunnerSyncRequest {
  provider?: string;
  runners?: ExternalRunnerInput[];
}

export interface ExternalRunnerRecord {
  id: string;
  provider: string;
  owner: string;
  org: string;
  status: string;
  repo?: string;
  workflow?: string;
  job?: string;
  ref?: string;
  createdAt?: string;
  actionsRepo?: string;
  actionsRunID?: string;
  actionsRunURL?: string;
  actionsRunStatus?: string;
  actionsRunConclusion?: string;
  actionsWorkflowName?: string;
  actionsWorkflowURL?: string;
  firstSeenAt: string;
  lastSeenAt: string;
  updatedAt: string;
  stale?: boolean;
}

export interface RunEventRecord {
  runID: string;
  seq: number;
  type: string;
  phase?: string;
  stream?: "stdout" | "stderr";
  message?: string;
  data?: string;
  leaseID?: string;
  slug?: string;
  provider?: string;
  target?: TargetOS;
  windowsMode?: WindowsMode;
  class?: string;
  serverType?: string;
  exitCode?: number;
  createdAt: string;
}

export interface RunEventRequest {
  type?: string;
  phase?: string;
  stream?: "stdout" | "stderr";
  message?: string;
  data?: string;
  leaseID?: string;
  slug?: string;
  provider?: string;
  target?: TargetOS;
  windowsMode?: WindowsMode;
  class?: string;
  serverType?: string;
  exitCode?: number;
}

export interface TestResultSummary {
  format: "junit";
  files: string[];
  suites: number;
  tests: number;
  failures: number;
  errors: number;
  skipped: number;
  timeSeconds: number;
  failed: TestFailure[];
}

export interface TestFailure {
  suite: string;
  name: string;
  classname?: string;
  file?: string;
  message?: string;
  type?: string;
  kind: "failure" | "error";
}

export interface HetznerServer {
  id: number;
  name: string;
  status: string;
  labels: Record<string, string>;
  public_net: {
    ipv4: {
      ip: string;
    };
  };
  server_type: {
    name: string;
  };
}

export interface HetznerSSHKey {
  id: number;
  name: string;
  fingerprint: string;
  public_key: string;
  labels?: Record<string, string>;
}

export interface MachineView {
  id: string;
  provider: Provider;
  cloudID: string;
  name: string;
  status: string;
  serverType: string;
  hostId?: string;
  host: string;
  labels: Record<string, string>;
}

export interface ProviderMachine {
  provider: Provider;
  id: number;
  cloudID: string;
  region?: string;
  name: string;
  status: string;
  serverType: string;
  hostID?: string;
  host: string;
  privateHost?: string;
  awsIPv6Addresses?: string[];
  awsKeyName?: string;
  awsSubnetID?: string;
  awsSecurityGroupIDs?: string[];
  awsInstanceProfileARN?: string;
  awsMetadataHttpEndpoint?: string;
  awsMetadataHttpTokens?: string;
  awsMetadataHttpPutResponseHopLimit?: number;
  awsMetadataInstanceTags?: string;
  awsRootVolumeID?: string;
  awsRootDeleteOnTermination?: boolean;
  labels: Record<string, string>;
  providerKey?: string;
  awsSSMCommandID?: string;
  awsSSMCommandStatus?: string;
}
