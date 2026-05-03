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
  CRABBOX_SHARED_TOKEN?: string;
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
  capacity?: {
    market?: "spot" | "on-demand";
    strategy?: "most-available" | "price-capacity-optimized" | "capacity-optimized" | "sequential";
    fallback?: string;
    regions?: string[];
    availabilityZones?: string[];
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

export type Provider = "hetzner" | "aws";
export type TargetOS = "linux" | "macos" | "windows";
export type WindowsMode = "normal" | "wsl2";

export interface LeaseRecord {
  id: string;
  slug?: string;
  provider: Provider;
  target: TargetOS;
  windowsMode?: WindowsMode;
  desktop?: boolean;
  browser?: boolean;
  cloudID: string;
  region?: string;
  owner: string;
  org: string;
  profile: string;
  class: string;
  serverType: string;
  requestedServerType?: string;
  provisioningAttempts?: ProvisioningAttempt[];
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
  releasedAt?: string;
  endedAt?: string;
}

export interface ProvisioningAttempt {
  serverType: string;
  market?: string;
  category?: string;
  message: string;
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
  name: string;
  status: string;
  serverType: string;
  host: string;
  labels: Record<string, string>;
}
