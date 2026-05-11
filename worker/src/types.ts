export interface Env {
  FLEET: DurableObjectNamespace;
  HETZNER_TOKEN: string;
  AWS_ACCESS_KEY_ID?: string;
  AWS_SECRET_ACCESS_KEY?: string;
  AWS_SESSION_TOKEN?: string;
  CRABBOX_AWS_REGION?: string;
  CRABBOX_AWS_AMI?: string;
  CRABBOX_AWS_SECURITY_GROUP_ID?: string;
  CRABBOX_AWS_SUBNET_ID?: string;
  CRABBOX_AWS_INSTANCE_PROFILE?: string;
  CRABBOX_AWS_ROOT_GB?: string;
  CRABBOX_AWS_SSH_CIDRS?: string;
  CRABBOX_AWS_MAC_HOST_ID?: string;
  CRABBOX_CAPACITY_REGIONS?: string;
  CRABBOX_CAPACITY_AVAILABILITY_ZONES?: string;
  CRABBOX_CAPACITY_HINTS?: string;
  CRABBOX_CAPACITY_LARGE_CLASSES?: string;
  AZURE_TENANT_ID?: string;
  AZURE_CLIENT_ID?: string;
  AZURE_CLIENT_SECRET?: string;
  AZURE_SUBSCRIPTION_ID?: string;
  CRABBOX_AZURE_LOCATION?: string;
  CRABBOX_AZURE_RESOURCE_GROUP?: string;
  CRABBOX_AZURE_IMAGE?: string;
  CRABBOX_AZURE_VNET?: string;
  CRABBOX_AZURE_SUBNET?: string;
  CRABBOX_AZURE_NSG?: string;
  CRABBOX_AZURE_SSH_CIDRS?: string;
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
  CRABBOX_SHARED_TOKEN?: string;
  CRABBOX_SHARED_OWNER?: string;
  CRABBOX_ADMIN_TOKEN?: string;
  CRABBOX_SESSION_SECRET?: string;
  CRABBOX_GITHUB_CLIENT_ID?: string;
  CRABBOX_GITHUB_CLIENT_SECRET?: string;
  CRABBOX_GITHUB_ALLOWED_ORG?: string;
  CRABBOX_GITHUB_ALLOWED_ORGS?: string;
  CRABBOX_GITHUB_ALLOWED_TEAM?: string;
  CRABBOX_GITHUB_ALLOWED_TEAMS?: string;
  CRABBOX_PUBLIC_URL?: string;
  CRABBOX_DEFAULT_ORG?: string;
  CRABBOX_ACCESS_TEAM_DOMAIN?: string;
  CRABBOX_ACCESS_AUD?: string;
  CRABBOX_COST_RATES_JSON?: string;
  CRABBOX_EUR_TO_USD?: string;
  CRABBOX_MAX_ACTIVE_LEASES?: string;
  CRABBOX_MAX_ACTIVE_LEASES_PER_OWNER?: string;
  CRABBOX_MAX_ACTIVE_LEASES_PER_ORG?: string;
  CRABBOX_MAX_MONTHLY_USD?: string;
  CRABBOX_MAX_MONTHLY_USD_PER_OWNER?: string;
  CRABBOX_MAX_MONTHLY_USD_PER_ORG?: string;
  CRABBOX_TAILSCALE_ENABLED?: string;
  CRABBOX_TAILSCALE_CLIENT_ID?: string;
  CRABBOX_TAILSCALE_CLIENT_SECRET?: string;
  CRABBOX_TAILSCALE_TAILNET?: string;
  CRABBOX_TAILSCALE_TAGS?: string;
  CRABBOX_ARTIFACTS_BACKEND?: string;
  CRABBOX_ARTIFACTS_BUCKET?: string;
  CRABBOX_ARTIFACTS_PREFIX?: string;
  CRABBOX_ARTIFACTS_BASE_URL?: string;
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
  windowsMode?: WindowsMode;
  desktop?: boolean;
  browser?: boolean;
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
  location?: string;
  image?: string;
  awsRegion?: string;
  awsAMI?: string;
  awsSGID?: string;
  awsSubnetID?: string;
  awsProfile?: string;
  awsRootGB?: number;
  awsSSHCIDRs?: string[];
  awsMacHostID?: string;
  azureLocation?: string;
  azureImage?: string;
  gcpProject?: string;
  gcpZone?: string;
  gcpImage?: string;
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
}

export type Provider = "hetzner" | "aws" | "azure" | "gcp";
export type TargetOS = "linux" | "macos" | "windows";
export type WindowsMode = "normal" | "wsl2";

export interface LeaseTelemetry {
  capturedAt: string;
  source?: string;
  load1?: number;
  load5?: number;
  load15?: number;
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
  provider: Provider;
  target: TargetOS;
  windowsMode?: WindowsMode;
  desktop?: boolean;
  browser?: boolean;
  code?: boolean;
  tailscale?: TailscaleMetadata;
  cloudID: string;
  region?: string;
  providerProject?: string;
  owner: string;
  org: string;
  share?: LeaseShare | undefined;
  profile: string;
  class: string;
  serverType: string;
  requestedServerType?: string;
  market?: string;
  provisioningAttempts?: ProvisioningAttempt[];
  capacityHints?: CapacityHint[];
  serverID: number;
  serverName: string;
  providerKey: string;
  host: string;
  sshUser: string;
  sshPort: string;
  sshFallbackPorts?: string[];
  workRoot: string;
  keep: boolean;
  ttlSeconds: number;
  idleTimeoutSeconds?: number;
  estimatedHourlyUSD: number;
  maxEstimatedUSD: number;
  state: "active" | "released" | "expired" | "failed";
  createdAt: string;
  updatedAt: string;
  lastTouchedAt?: string;
  expiresAt: string;
  telemetry?: LeaseTelemetry;
  telemetryHistory?: LeaseTelemetry[];
  releasedAt?: string;
  endedAt?: string;
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
  region?: string;
}

export interface PromotedImageRecord extends ProviderImage {
  promotedAt: string;
}

export interface RunRecord {
  id: string;
  leaseID: string;
  slug?: string;
  owner: string;
  org: string;
  provider: Provider;
  target?: TargetOS;
  windowsMode?: WindowsMode;
  class: string;
  serverType: string;
  command: string[];
  state: "running" | "succeeded" | "failed";
  phase?: string;
  exitCode?: number;
  syncMs?: number;
  commandMs?: number;
  durationMs?: number;
  logBytes: number;
  logTruncated: boolean;
  results?: TestResultSummary;
  telemetry?: RunTelemetrySummary;
  startedAt: string;
  lastEventAt?: string;
  eventCount?: number;
  endedAt?: string;
}

export interface RunCreateRequest {
  leaseID?: string;
  provider?: Provider;
  target?: TargetOS;
  windowsMode?: WindowsMode;
  class?: string;
  serverType?: string;
  command?: string[];
}

export interface RunFinishRequest {
  exitCode: number;
  syncMs?: number;
  commandMs?: number;
  log?: string;
  logChunks?: string[];
  logTruncated?: boolean;
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
  provider?: Provider;
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
  provider?: Provider;
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
}

export interface MachineView {
  id: string;
  provider: Provider;
  cloudID: string;
  name: string;
  status: string;
  serverType: string;
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
  host: string;
  labels: Record<string, string>;
}
