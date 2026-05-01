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
  CRABBOX_SESSION_SECRET?: string;
  CRABBOX_GITHUB_CLIENT_ID?: string;
  CRABBOX_GITHUB_CLIENT_SECRET?: string;
  CRABBOX_GITHUB_ALLOWED_ORG?: string;
  CRABBOX_GITHUB_ALLOWED_ORGS?: string;
  CRABBOX_PUBLIC_URL?: string;
  CRABBOX_DEFAULT_ORG?: string;
  CRABBOX_COST_RATES_JSON?: string;
  CRABBOX_EUR_TO_USD?: string;
  CRABBOX_MAX_ACTIVE_LEASES?: string;
  CRABBOX_MAX_ACTIVE_LEASES_PER_OWNER?: string;
  CRABBOX_MAX_ACTIVE_LEASES_PER_ORG?: string;
  CRABBOX_MAX_MONTHLY_USD?: string;
  CRABBOX_MAX_MONTHLY_USD_PER_OWNER?: string;
  CRABBOX_MAX_MONTHLY_USD_PER_ORG?: string;
  CRABBOX_MPP_RECIPIENT?: string;
  CRABBOX_MPP_CURRENCY?: string;
  CRABBOX_MPP_DECIMALS?: string;
  CRABBOX_MPP_SECRET_KEY?: string;
  CRABBOX_MPP_TESTNET?: string;
  CRABBOX_MPP_REALM?: string;
}

export interface LeaseRequest {
  leaseID?: string;
  slug?: string;
  requestedSlug?: string;
  provider?: Provider;
  profile?: string;
  class?: string;
  serverType?: string;
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

export interface LeaseRecord {
  id: string;
  slug?: string;
  provider: Provider;
  cloudID: string;
  region?: string;
  owner: string;
  org: string;
  profile: string;
  class: string;
  serverType: string;
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
  class: string;
  serverType: string;
  command: string[];
  state: "running" | "succeeded" | "failed";
  exitCode?: number;
  syncMs?: number;
  commandMs?: number;
  durationMs?: number;
  logBytes: number;
  logTruncated: boolean;
  results?: TestResultSummary;
  startedAt: string;
  endedAt?: string;
}

export interface RunCreateRequest {
  leaseID: string;
  provider?: Provider;
  class?: string;
  serverType?: string;
  command?: string[];
}

export interface RunFinishRequest {
  exitCode: number;
  syncMs?: number;
  commandMs?: number;
  log?: string;
  logTruncated?: boolean;
  results?: TestResultSummary;
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

export interface HetznerImage {
  id: number;
  description: string | null;
  status: "creating" | "available" | "unavailable";
  type: "snapshot" | "system" | "backup" | "app" | "temporary";
  labels: Record<string, string>;
  created: string;
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
