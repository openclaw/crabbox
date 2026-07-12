import ssh2, { type Client as SSHClient, type ClientChannel } from "ssh2";

const { Client: SSHClientConstructor, utils: sshUtils } = ssh2;

import { artifactUploadResponse, type ArtifactUploadRequest } from "./artifacts";
import {
  adminGrantVersion as configuredAdminGrantVersion,
  githubUserGrantIsCurrent,
  githubUserIsAdmin,
  isAdminRequest,
  requestWithAuthContext,
  sha256Hex,
  verifiedUserTokenExpiresAtForRevocation,
  type AuthContext,
  type GitHubUserGrant,
} from "./auth";
import {
  EC2SpotClient,
  awsAutomaticProbesConfigured,
  awsCredentialsConfigured,
  awsConfiguredSecurityGroupID,
  awsOrphanSweepCredentialsConfigured,
  awsManagedSecurityGroupName,
  awsLaunchCandidates,
  awsPrivateWorkspaceConfig,
  awsPrivateWorkspaceModeEnabled,
  awsProvisioningErrorCategory,
  awsRegionCandidates,
  isAWSInstanceCleanedAfterReadinessFailure,
  isAWSInstanceNotFoundError,
  isAWSRunInstancesOutcomeUncertain,
  isRetryableAWSProvisioningError,
  isAWSSecurityGroupRuleLimitError,
  type AWSMacHost,
  type AWSPrivateWorkspaceConfig,
} from "./aws";
import { InvalidAWSRegionError, sanitizeAWSRegion } from "./aws-region";
import { AzureClient, azureRegionCandidates, type AzureDeferredCleanupRequest } from "./azure";
import {
  codeOriginForLease,
  codeProxyRequestBodyBytes,
  isIsolatedCodeRequest,
} from "./code-origin";
import {
  awsPromotedAMIConfigKey,
  azureLocationFor,
  leaseConfig,
  validCIDRs,
  workspaceProviderKeyPrefix,
  type LeaseConfig,
  type LeaseConfigDefaults,
} from "./config";
import { cookieValue } from "./cookies";
import {
  CloudflareCoordinatorRuntime,
  bufferCoordinatorRequestBody,
  coordinatorRequestQueue,
  type CoordinatorRuntime,
} from "./coordinator-runtime";
import {
  DaytonaClient,
  daytonaAccessNeedsRefresh,
  isDaytonaNotFound,
  type DaytonaSSHEndpoint,
} from "./daytona";
import { GCPClient, gcpProviderLabelValue } from "./gcp";
import {
  HetznerClient,
  HetznerProvisioningError,
  hetznerProvisioningFailureMayHaveResource,
  hetznerProvisioningFailureRetryable,
  hetznerProvisioningResourceID,
  hetznerServerOwnedByLease,
} from "./hetzner";
import {
  bearerToken,
  errorMessage,
  json,
  pathParts,
  readJson,
  redactDiagnosticSecrets,
  requestOwner,
} from "./http";
import {
  hasImageRequirements,
  imageSatisfiesRequirements,
  InvalidImageCapabilitiesError,
  normalizeImageCapabilities,
} from "./image-capabilities";
import {
  MarketplaceInputError,
  marketplaceQuote,
  marketplaceStatus,
  type MarketplaceQuoteRequest,
} from "./marketplace";
import {
  githubAuthRoute,
  githubPortalLogin,
  githubPortalLogout,
  githubPortalLogoutConfirmation,
} from "./oauth";
import {
  MISSING_ORG_KEY,
  isCurrentOrgKey,
  isLegacyOrgKey,
  orgAuthLabelFromKey,
  orgKeyForLabel,
  orgLabelForDisplay,
  orgLabelFromKey,
  orgMatchesForFilter,
  requestOrgLabel,
  sameOrgIdentityKey,
} from "./org-identity";
import {
  publicExternalRunnerRecord,
  publicLeaseRecord,
  publicReadyPoolEntry,
  publicRunRecord,
  portalExternalRunnerRecord,
  portalLeaseRecord,
} from "./org-records";
import { defaultOSImage, normalizeOSImage } from "./os-image";
import {
  portalCode,
  portalCodeBootstrapHandoff,
  portalAdmin,
  portalError,
  portalExternalRunnerDetail,
  portalHome,
  portalLeaseDetail,
  portalMacHostDetail,
  portalRunDetail,
  portalShareLease,
  portalVNC,
  type PortalAdminLeaseSummary,
  type PortalLeaseBridgeStatus,
  type PortalAdminProviderStatus,
  type PortalAdminUserSummary,
  type PortalAdminView,
  type PortalMacHostRecord,
  webVNCBridgeCommand,
} from "./portal";
import { leaseIDForProviderKey, providerKeyForLease, sshPublicKeyIdentity } from "./provider-key";
import {
  providerLabelsOwnedByLease,
  providerMachineOwnedByLease,
  workspacePrewarmProviderOwner,
} from "./provider-labels";
import {
  ProviderProvisioningCleanupError,
  providerProvisioningCleanupClaim,
  type ProviderProvisioningCleanupClaim,
  validatedProviderProvisioningCleanupClaim,
} from "./provider-provisioning";
import {
  readRuntimeAdapterRelayBody,
  runtimeAdapterProxyPath,
  runtimeAdapterRelayBodyAllowed,
  runtimeAdapterRelayContentType,
  runtimeAdapterRelayFrameLimit,
  runtimeAdapterRelayHeaders,
  runtimeAdapterRelayMethodAllowed,
  runtimeAdapterRelayTimeoutForPath,
  runtimeAdapterRelayTimeoutMs,
  validRuntimeAdapterID,
  validRuntimeAdapterDesktopRelayTimeout,
  validRuntimeAdapterRelayResponse,
  type RuntimeAdapterRelayRequest,
  type RuntimeAdapterRelayResponse,
} from "./runtime-adapter-relay";
import { leaseSlugFromID, normalizeLeaseSlug, slugWithCollisionSuffix } from "./slug";
import {
  createTailscaleAuthKey,
  renderTailscaleHostname,
  tailscaleAllowed,
  tailscaleDefaultTags,
  tailscaleInstallConfig,
  tailscalePreflight,
  tailscaleTagOwnershipErrorMessage,
  validateTailscaleTags,
} from "./tailscale";
import type {
  CapacityHint,
  Env,
  ExternalRunnerInput,
  ExternalRunnerRecord,
  ExternalRunnerSyncRequest,
  HetznerServer,
  LeaseRecord,
  LeaseRegistrationRequest,
  LeaseRequest,
  LeaseShare,
  LeaseShareRole,
  LeaseTelemetry,
  ImageCapabilities,
  Provider,
  ProviderFastSnapshotRestore,
  ProviderImage,
  ProviderMachine,
  ProvisioningAttempt,
  ReadyPoolBorrowRequest,
  ReadyPoolEntry,
  ReadyPoolRegisterRequest,
  ReadyPoolReturnRequest,
  PromotedImageRecord,
  RunCreateRequest,
  RunEventRecord,
  RunEventRequest,
  RunFinishRequest,
  RunRecord,
  RunTelemetryRequest,
  RunTelemetrySummary,
  TargetOS,
  TestFailure,
  TestResultSummary,
  TailscaleMetadata,
  WindowsMode,
} from "./types";
import {
  coordinatorProviderRegistry,
  coordinatorProviders,
  coordinatorProviderSpec,
  isCoordinatorProvider,
} from "./types";
import {
  addLeaseToCostLimitUsage,
  costLimits,
  createCostLimitUsage,
  enforceCostLimitUsage,
  leaseCost,
  requestOrg,
  usageSummary,
  type CostLimitUsage,
} from "./usage";
import { WebVNCCredentialHandoffs } from "./webvnc-handoff";

const fleetID = "default";
const maxStoredRunLogBytes = 8 * 1024 * 1024;
const runLogChunkBytes = 64 * 1024;
const maxLeaseTelemetryHistory = 60;
const maxRunTelemetrySamples = 60;
const maxExternalRunnerSyncItems = 200;
const webVNCTicketTTLSeconds = 120;
const codeTicketTTLSeconds = 120;
const codeViewerTicketTTLSeconds = 120;
const codeViewerSessionTTLSeconds = 8 * 60 * 60;
const egressTicketTTLSeconds = 120;
const runtimeAdapterTicketTTLSeconds = 120;
const nativeVNCTicketTTLSeconds = 60;
const runtimeAdapterProvisionalClaimTTLSeconds = 10 * 60;
const runtimeAdapterDeleteRetryBaseMs = 5_000;
const runtimeAdapterDeleteRetryMaxMs = 60_000;
const runtimeAdapterDeleteDispatchGraceMs = 1_000;
const runtimeAdapterDeleteInitialRetryMs =
  runtimeAdapterRelayTimeoutMs + runtimeAdapterDeleteDispatchGraceMs;
const runtimeAdapterMaxPendingPerAdapter = 16;
const runtimeAdapterMaxPendingPerOwner = 32;
const runtimeAdapterMaxPendingGlobal = 128;
const runtimeAdapterReservedDeletesPerAdapter = 4;
const runtimeAdapterReservedDeletesPerOwner = 8;
const runtimeAdapterReservedDeletesGlobal = 16;
const runtimeAdapterMaxBufferedBytes = runtimeAdapterRelayFrameLimit * 2;
const leaseCleanupRetryDelayMs = 5 * 60 * 1000;
const leaseCleanupClaimStaleMs = 30 * 60 * 1000;
const leaseCleanupBatchSize = 16;
const awsOrphanSweepInitialDelayMs = 60 * 1000;
const azureOrphanSweepInitialDelayMs = 60 * 1000;
const defaultAWSOrphanSweepIntervalSeconds = 60 * 60;
const defaultAWSOrphanSweepGraceSeconds = 15 * 60;
const defaultAzureOrphanSweepIntervalSeconds = 60 * 60;
const defaultAzureOrphanSweepGraceSeconds = 15 * 60;
const storageRecordScanBatchSize = 128;
const terminalRunPruneBatchSize = 16;
const runtimeAdapterDeleteBatchSize = 16;
const defaultTerminalRunRetentionDays = 30;
const runPruneCursorKey = "maintenance:run-prune-cursor";
const providerAccessReservationTTLMS = 15 * 60 * 1000;
const maxPendingWebVNCBytes = 1024 * 1024;
const maxCodeWebSocketFrameChunkBytes = 15 * 1024;
const textEncoder = new TextEncoder();
const textDecoder = new TextDecoder();
const fatalTextDecoder = new TextDecoder("utf-8", { fatal: true, ignoreBOM: false });
const awsOrphanSweepRecordKey = "aws-orphan-sweep:last";
const awsOrphanSweepFirstAlarmKey = "aws-orphan-sweep:first-alarm";
const azureOrphanSweepRecordKey = "azure-orphan-sweep:last";
const azureOrphanSweepFirstAlarmKey = "azure-orphan-sweep:first-alarm";
const awsIngressReconcileRecordKey = "aws-ingress-reconcile:pending";
const azureDeferredCleanupPrefix = "azure-cleanup:";
const readyPoolPrefix = "ready-pool:";
const workspaceReconcileIntervalMs = 10_000;
const workspaceReconcileMaxIntervalMs = 5 * 60_000;
const workspaceProvisionClaimMs = 15 * 60_000;
const workspaceProvisionRecoveryGraceMs = 15 * 60_000;
const workspaceMinimumTTLSeconds =
  (workspaceProvisionClaimMs + workspaceProvisionRecoveryGraceMs) / 1000;
const workspaceMaxRecordsPerOwner = 100;
const workspaceTerminalRetentionMs = 24 * 60 * 60_000;
const workspacePrewarmOwner = workspacePrewarmProviderOwner;
const workspacePrewarmReplacementLeadMs = 5 * 60_000;
const workspacePrewarmRetryDelayMs = 5 * 60_000;
const workspaceTerminalMaxBufferedBytes = 1024 * 1024;
const workspaceTerminalMaxBufferedFrames = 1024;
const workspaceTerminalMaxPerWorkspace = 4;
const workspaceTerminalMaxPerOwner = 16;
const workspaceTerminalMaxGlobal = 64;
const workspaceTerminalTransportMemoryBudgetBytes = 64 * 1024 * 1024;
const workspaceCommandMaxBootstrapBytes = 3_000;
const workspaceTerminalSSHReadyTimeoutMs = 2 * 60_000;
const workspaceSSHHostPrivateKeyHeader = "x-crabbox-workspace-ssh-host-private-key";
const workspaceSSHHostPublicKeyHeader = "x-crabbox-workspace-ssh-host-public-key";
const privateAWSWorkspaceWorkRoot = "/work/crabbox";
const adminGrantRevalidationIntervalMs = 1_000;
const userGrantRevalidationIntervalMs = 1_000;

function coordinatorErrorMessage(env: Env, error: unknown): string {
  return errorMessage(error, coordinatorDiagnosticSecrets(env));
}

function coordinatorDiagnosticText(env: Env, value: string): string {
  return redactDiagnosticSecrets(value, coordinatorDiagnosticSecrets(env));
}

function coordinatorDiagnosticSecrets(env: Env): Array<string | undefined> {
  return [
    ...coordinatorProviderRegistry.flatMap((provider) =>
      provider.requiredSecrets.map((name) => env[name]),
    ),
    env.AWS_SESSION_TOKEN,
    env.CRABBOX_RUNTIME_ADAPTER_TOKEN,
    env.CRABBOX_SHARED_TOKEN,
    env.CRABBOX_ADMIN_TOKEN,
    env.CRABBOX_SESSION_SECRET,
    env.CRABBOX_GITHUB_CLIENT_SECRET,
    env.CRABBOX_WORKSPACE_SSH_PRIVATE_KEY,
    env.CRABBOX_TRUSTED_PROXY_SECRET,
    env.CRABBOX_TAILSCALE_CLIENT_SECRET,
    env.CRABBOX_ARTIFACTS_ACCESS_KEY_ID,
    env.CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY,
    env.CRABBOX_ARTIFACTS_SESSION_TOKEN,
  ];
}

interface CachedAdminGrant {
  auth?: AuthContext["auth"];
  login?: string;
  adminTokenHash?: string;
  adminGrantVersion?: string;
}

interface CachedBridgeGrant extends CachedAdminGrant {
  auth?: AuthContext["auth"];
  login?: string;
  sharedTokenHash?: string;
  portalSessionHash?: string;
  githubGrant?: GitHubUserGrant;
}

interface WebVNCTicketRecord extends CachedBridgeGrant {
  ticket: string;
  leaseID: string;
  owner: string;
  org: string;
  admin?: boolean;
  createdAt: string;
  expiresAt: string;
}

interface NativeVNCTicketRecord {
  ticket: string;
  workspaceID: string;
  leaseID: string;
  owner: string;
  org: string;
  createdAt: string;
  expiresAt: string;
}

interface CodeTicketRecord extends CachedBridgeGrant {
  ticket: string;
  leaseID: string;
  owner: string;
  org: string;
  admin?: boolean;
  createdAt: string;
  expiresAt: string;
}

interface CodeViewerTicketRecord extends CachedBridgeGrant {
  ticket: string;
  leaseID: string;
  auth: AuthContext["auth"];
  admin: boolean;
  owner: string;
  org: string;
  login?: string;
  portalSessionHash?: string;
  returnTo: string;
  viewerExpiresAt?: string;
  createdAt: string;
  expiresAt: string;
}

interface CodeViewerSessionRecord extends CachedBridgeGrant {
  session: string;
  leaseID: string;
  auth: AuthContext["auth"];
  admin: boolean;
  owner: string;
  org: string;
  login?: string;
  portalSessionHash?: string;
  createdAt: string;
  expiresAt: string;
}

interface CodeViewerSessionRevocationRecord {
  portalSessionHash: string;
  createdAt: string;
  expiresAt: string;
}

interface RuntimeAdapterTicketRecord {
  ticket: string;
  adapterID: string;
  owner: string;
  org: string;
  createdAt: string;
  expiresAt: string;
  desktopTimeoutMs?: number;
}

interface RuntimeAdapterIdentityRecord {
  adapterID: string;
  owner: string;
  org: string;
  createdAt: string;
  claimVersion?: 1;
  claimState?: "provisional" | "confirmed";
  claimExpiresAt?: string;
  confirmedAt?: string;
}

interface RuntimeAdapterPendingRequest {
  adapterID: string;
  owner: string;
  org: string;
  dispatched: boolean;
  clientSettled: boolean;
  resolve: (result: RuntimeAdapterRelayResult) => void;
  timeout: ReturnType<typeof setTimeout>;
  signal: AbortSignal;
  abortHandler: () => void;
}

interface RuntimeAdapterRelayResult {
  origin: "relay" | "upstream";
  response: RuntimeAdapterRelayResponse;
}

interface RuntimeAdapterProxyResult {
  origin: "relay" | "upstream";
  dispatched: boolean;
  response: Response;
}

interface RuntimeAdapterProxyScope {
  owner: string;
  org: string;
}

interface RuntimeAdapterDeleteClaim {
  requestedAt: string;
  claimID: string;
  created: boolean;
}

interface RuntimeAdapterDeleteVersion {
  requestedAt: string;
  claimID?: string;
}

interface RuntimeAdapterDeleteDispatch {
  lease: LeaseRecord;
  version: RuntimeAdapterDeleteVersion;
  deadlineMs: number;
  fenceUntil: string;
}

type RuntimeAdapterDeleteFinalization =
  | { status: "completed"; lease: LeaseRecord }
  | { status: "in-flight"; retryAt: string }
  | { status: "mismatch" };

interface RuntimeAdapterDeleteCompletion {
  adapterID: string;
  workspaceID: string;
  registrationID: string;
  status: "absent";
}

interface RuntimeAdapterLegacyDeleteCompletion {
  adapterID: string;
  workspaceID: string;
  status: "absent";
}

type EgressRole = "host" | "client";

interface EgressTicketRecord extends CachedBridgeGrant {
  ticket: string;
  leaseID: string;
  owner: string;
  org: string;
  admin?: boolean;
  role: EgressRole;
  sessionID: string;
  profile?: string;
  allow?: string[];
  createdAt: string;
  expiresAt: string;
}

type LeaseBridgeTicketConsumption<T> =
  | { status: "invalid" }
  | { status: "not_found" }
  | { status: "accepted"; ticket: T; lease: LeaseRecord };

type RuntimeAdapterTicketConsumption =
  | { status: "invalid" }
  | { status: "accepted"; ticket: RuntimeAdapterTicketRecord };

interface EgressSessionStatus {
  leaseID: string;
  sessionID: string;
  profile?: string;
  allow: string[];
  createdAt: string;
  updatedAt: string;
}

interface WorkspaceCreateRequest {
  id?: string;
  repo?: string;
  branch?: string;
  command?: string;
  runtime?: string;
  profile?: string;
  ttlSeconds?: number;
  idleTimeoutSeconds?: number;
  capabilities?: {
    desktop?: boolean;
  };
}

interface WorkspaceRecord {
  id: string;
  leaseID: string;
  owner: string;
  org: string;
  profile: string;
  repo: string;
  branch: string;
  command: string;
  provider: Provider;
  class: string;
  desktop: boolean;
  desktopCapabilityVersion?: 1;
  ttlSeconds: number;
  idleTimeoutSeconds: number;
  createdAt: string;
  updatedAt: string;
  prewarm?: boolean;
  sshHostKeySha256?: string;
  provisionClaim?: string;
  provisionClaimExpiresAt?: string;
  reconcileAfter?: string;
  recoveryMisses?: number;
  releaseRequestedAt?: string;
  error?: string;
}

interface CodeProxyRequest {
  type: "http";
  id: string;
  method: string;
  path: string;
  headers: Record<string, string>;
  body?: string;
}

interface CodeProxyResponse {
  type: "http";
  id: string;
  status: number;
  headers?: Record<string, string>;
  body?: string;
  error?: string;
}

interface CodePendingRequest {
  leaseID: string;
  resolve: (response: CodeProxyResponse) => void;
  timeout: ReturnType<typeof setTimeout>;
  response?: CodeProxyResponse;
  chunks: string[];
}

interface CodeWebSocketOpen {
  type: "ws_open";
  id: string;
  path: string;
  headers: Record<string, string>;
}

interface CodeWebSocketData {
  type: "ws_data";
  id: string;
  body: string;
  frame?: "text" | "binary";
}

interface CodeWebSocketFrameStart {
  type: "ws_start";
  id: string;
  chunkID: string;
  frame?: "text" | "binary";
}

interface CodeWebSocketFrameBody {
  type: "ws_body";
  id?: string;
  chunkID: string;
  body: string;
}

interface CodeWebSocketFrameEnd {
  type: "ws_end";
  id?: string;
  chunkID: string;
}

interface CodeWebSocketClose {
  type: "ws_close";
  id: string;
  code?: number;
  reason?: string;
}

interface CodePendingWebSocketFrame {
  leaseID: string;
  id: string;
  frame: "text" | "binary";
  chunks: string[];
}

interface LeaseCloudAudit {
  leaseID: string;
  slug?: string;
  provider: string;
  state: LeaseRecord["state"];
  target: LeaseRecord["target"];
  owner: string;
  org: string;
  region?: string;
  cloudID: string;
  host: string;
  serverType: string;
  expiresAt: string;
  cleanupAttempts?: number;
  cleanupError?: string;
  cleanupRetryAt?: string;
  cloudStatus: "found" | "missing" | "error";
  cloudState?: string;
  cloudHost?: string;
  cloudServerType?: string;
  message?: string;
}

interface AWSOrphanSweepConfig {
  enabled: boolean;
  deleteEnabled: boolean;
  macHostReleaseEnabled: boolean;
  intervalSeconds: number;
  graceSeconds: number;
  regions: string[];
}

interface AzureOrphanSweepConfig {
  enabled: boolean;
  deleteEnabled: boolean;
  intervalSeconds: number;
  graceSeconds: number;
  regions: string[];
}

interface AzureDeferredCleanupRecord extends AzureDeferredCleanupRequest {
  attempts: number;
  updatedAt: string;
  retryAt: string;
  lastError?: string;
  terminalAt?: string;
}

class ProviderCleanupManualResolutionError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ProviderCleanupManualResolutionError";
  }
}

interface AWSIngressReconcileTarget {
  anchor: LeaseRecord;
  attempts: number;
  generation: string;
  updatedAt: string;
  retryAt: string;
  lastError?: string;
}

interface AWSIngressReconcileRecord {
  targets: AWSIngressReconcileTarget[];
}

interface LegacyAWSIngressReconcileRecord {
  anchor: LeaseRecord;
  attempts: number;
  updatedAt: string;
  retryAt: string;
  lastError?: string;
}

type StoredAWSIngressReconcileRecord = AWSIngressReconcileRecord | LegacyAWSIngressReconcileRecord;

interface CloudOrphanSweepCandidate {
  region: string;
  cloudID: string;
  name: string;
  status: string;
  serverType: string;
  host?: string;
  leaseID?: string;
  slug?: string;
  owner?: string;
  reason: string;
  createdAt?: string;
  expiresAt?: string;
  activeCloudID?: string;
  ownership: "coordinator-lease" | "provider-tags-only";
  ownershipLeaseID?: string;
  action: "reported" | "terminated" | "terminate_failed";
  error?: string;
}

type AWSOrphanSweepCandidate = CloudOrphanSweepCandidate;
type AzureOrphanSweepCandidate = CloudOrphanSweepCandidate;

interface AWSMacHostSweepCandidate {
  region: string;
  hostID: string;
  state: string;
  instanceType: string;
  availabilityZone: string;
  allocationTime?: string;
  activeLeaseID?: string;
  reason: string;
  ownership: "coordinator-lease" | "provider-tags-only";
  ownershipLeaseID?: string;
  action: "reported" | "released" | "release_failed";
  error?: string;
}

interface AWSOrphanSweepRecord {
  startedAt: string;
  finishedAt: string;
  mode: "report" | "delete";
  trigger: "alarm" | "admin";
  enabled: boolean;
  regions: string[];
  scanned: number;
  candidates: AWSOrphanSweepCandidate[];
  terminated: number;
  macHostsScanned?: number;
  macHostCandidates?: AWSMacHostSweepCandidate[];
  macHostsReleased?: number;
  errors: Array<{ region: string; message: string }>;
  nextRunAt?: string;
}

interface AzureOrphanSweepRecord {
  startedAt: string;
  finishedAt: string;
  mode: "report" | "delete";
  trigger: "alarm" | "admin";
  enabled: boolean;
  regions: string[];
  scanned: number;
  candidates: AzureOrphanSweepCandidate[];
  terminated: number;
  errors: Array<{ region: string; message: string }>;
  nextRunAt: string;
}

type BridgeAttachment =
  | (CachedBridgeGrant & {
      kind: "webvnc-agent";
      leaseID: string;
      id: string;
      capabilities: Set<string>;
      owner?: string;
      org?: string;
      admin?: boolean;
    })
  | (CachedBridgeGrant & {
      kind: "webvnc-viewer";
      leaseID: string;
      id: string;
      agentID: string;
      owner: string;
      org?: string;
      admin?: boolean;
      label: string;
    })
  | (CachedBridgeGrant & {
      kind: "code-agent";
      leaseID: string;
      owner?: string;
      org?: string;
      admin?: boolean;
    })
  | (CachedBridgeGrant & {
      kind: "code-viewer";
      leaseID: string;
      id: string;
      auth: AuthContext["auth"];
      owner?: string;
      org?: string;
      admin?: boolean;
    })
  | (CachedBridgeGrant & {
      kind: "egress-host";
      leaseID: string;
      sessionID: string;
      owner?: string;
      org?: string;
      admin?: boolean;
    })
  | (CachedBridgeGrant & {
      kind: "egress-client";
      leaseID: string;
      sessionID: string;
      owner?: string;
      org?: string;
      admin?: boolean;
    })
  | {
      kind: "runtime-adapter-agent";
      adapterID: string;
      owner: string;
      org: string;
      desktopTimeoutMs?: number;
    }
  | {
      kind: "control";
      clientID: string;
      owner: string;
      org: string;
      admin?: boolean;
      auth?: AuthContext["auth"];
      login?: string;
      adminTokenHash?: string;
      adminGrantVersion?: string;
      subscriptions?: Record<string, number>;
    };

type ControlMessage =
  | { type: "subscribe_run"; runID?: string; after?: number; limit?: number }
  | { type: "ack"; runID?: string; seq?: number }
  | {
      type: "heartbeat";
      leaseID?: string;
      idleTimeoutSeconds?: number;
      telemetry?: Partial<LeaseTelemetry>;
    }
  | { type: "ping" };

interface WebVNCEvent {
  at: string;
  event: string;
  reason?: string;
}

interface WebVNCViewerSession extends CachedBridgeGrant {
  id: string;
  agentID: string;
  socket: WebSocket;
  owner: string;
  org?: string;
  admin?: boolean;
  label: string;
  connectedAt: string;
}

export class FleetCoordinator {
  private readonly webVNCAgents = new Map<string, Map<string, WebSocket>>();
  private readonly webVNCAgentCapabilities = new Map<string, Map<string, Set<string>>>();
  private readonly webVNCViewers = new Map<string, Map<string, WebVNCViewerSession>>();
  private readonly webVNCControllers = new Map<string, string>();
  private readonly pendingWebVNCToViewer = new Map<string, WebVNCBuffer>();
  private readonly webVNCEvents = new Map<string, WebVNCEvent[]>();
  private readonly codeAgents = new Map<string, WebSocket>();
  private readonly codeViewers = new Map<string, WebSocket>();
  private readonly pendingCodeRequests = new Map<string, CodePendingRequest>();
  private readonly pendingCodeFrames = new Map<string, CodePendingWebSocketFrame>();
  private readonly egressHosts = new Map<string, WebSocket>();
  private readonly egressClients = new Map<string, WebSocket>();
  private readonly egressSessions = new Map<string, EgressSessionStatus>();
  private readonly runtimeAdapterAgents = new Map<string, WebSocket>();
  private readonly runtimeAdapterPending = new Map<string, RuntimeAdapterPendingRequest>();
  private readonly runtimeAdapterDeleteQueues = new Map<string, Promise<void>>();
  private readonly controlSockets = new Map<string, WebSocket>();
  private readonly workspaceTerminals = new Map<string, Set<WebSocket>>();
  private readonly restoredBridgeSockets = new Set<WebSocket>();
  private readonly adminGrantValidationTimes = new WeakMap<WebSocket, number>();
  private readonly userGrantValidationTimes = new WeakMap<WebSocket, number>();
  private currentAdminGrantVersion: string | undefined;
  private bridgeRestoreReady: Promise<boolean> | undefined;
  private readyPoolBorrowQueue: Promise<void> = Promise.resolve();
  private bridgeTicketQueue: Promise<void> = Promise.resolve();
  private awsIngressBarrier: Promise<void> = Promise.resolve();
  private readonly awsIngressAdditiveOperations = new Set<Promise<void>>();
  private providerMaintenanceQueue: Promise<void> = Promise.resolve();
  private readonly webVNCCredentialHandoffs: WebVNCCredentialHandoffs;

  constructor(
    private readonly state: CoordinatorRuntime,
    private readonly env: Env,
    private readonly testProviders: Partial<Record<Provider, CloudProvider>> = {},
  ) {
    this.webVNCCredentialHandoffs = new WebVNCCredentialHandoffs(state);
    this.restoreBridgeWebSockets();
  }

  async fetch(request: Request): Promise<Response> {
    try {
      await this.reconcileAdminGrantVersion(request);
      if (!(await this.restoredBridgesReady())) {
        return json(
          {
            error: "bridge_state_unavailable",
            message: "restored bridge lease state is temporarily unavailable",
          },
          { status: 503, headers: { "retry-after": "1" } },
        );
      }
      const parts = pathParts(request);
      const method = request.method.toUpperCase();
      const adminError = adminRouteError(request, method, parts);
      if (adminError) {
        return adminError;
      }
      if (method === "GET" && parts.join("/") === "v1/health") {
        return json({ ok: true, fleet: fleetID });
      }
      if (method === "POST" && parts.join("/") === "v1/internal/scheduled") {
        return await this.scheduledMaintenance(request);
      }
      if (parts[0] === "v1" && parts[1] === "auth" && parts[2] === "github") {
        return await githubAuthRoute(request, parts[3], this.state, this.env);
      }
      if (method === "GET" && parts.join("/") === "portal/login") {
        return await githubPortalLogin(request, this.state, this.env);
      }
      if (method === "GET" && parts.join("/") === "portal/logout") {
        return githubPortalLogoutConfirmation();
      }
      if (method === "POST" && parts.join("/") === "portal/logout") {
        return await this.portalLogout(request);
      }
      if (parts[0] === "portal") {
        return await this.portalRoute(request, parts);
      }
      if (method === "GET" && parts.join("/") === "v1/pool") {
        return await this.pool(request);
      }
      if (parts[0] === "v1" && parts[1] === "ready-pools") {
        return await this.readyPoolRoute(request, parts[2], parts[3]);
      }
      if (method === "GET" && parts.join("/") === "v1/usage") {
        return await this.usage(request);
      }
      if (method === "GET" && parts.join("/") === "v1/marketplace/status") {
        return this.marketplaceStatus(request);
      }
      if (method === "POST" && parts.join("/") === "v1/marketplace/quotes") {
        return await this.marketplaceQuote(request);
      }
      if (method === "GET" && parts.join("/") === "v1/whoami") {
        return this.whoami(request);
      }
      if (
        method === "GET" &&
        parts[0] === "v1" &&
        parts[1] === "providers" &&
        parts[2] &&
        parts[3] === "readiness"
      ) {
        return await this.providerReadiness(request, parts[2]);
      }
      if (method === "GET" && parts.join("/") === "v1/control") {
        return await this.controlSocket(request);
      }
      if (method === "GET" && parts.join("/") === "v1/admin/leases") {
        return await this.adminLeases(request);
      }
      if (method === "GET" && parts.join("/") === "v1/admin/lease-audit") {
        return await this.adminLeaseAudit(request);
      }
      if (method === "GET" && parts.join("/") === "v1/admin/aws-identity") {
        return await this.adminAWSIdentity(request);
      }
      if (method === "GET" && parts.join("/") === "v1/admin/providers/identity") {
        return await this.adminProviderIdentity(request);
      }
      if (method === "POST" && parts.join("/") === "v1/admin/tailscale-preflight") {
        return await this.adminTailscalePreflight();
      }
      if (parts[0] === "v1" && parts[1] === "admin" && parts[2] === "hosts") {
        return await this.adminHostsRoute(request, parts[3]);
      }
      if (parts[0] === "v1" && parts[1] === "admin" && parts[2] === "mac-hosts") {
        return await this.adminMacHostsRoute(request, parts[3]);
      }
      if (
        (method === "GET" || method === "POST") &&
        parts.join("/") === "v1/admin/aws-orphan-sweep"
      ) {
        return await this.adminAWSOrphanSweep(request);
      }
      if (
        (method === "GET" || method === "POST") &&
        parts.join("/") === "v1/admin/azure-orphan-sweep"
      ) {
        return await this.adminAzureOrphanSweep(request);
      }
      if (parts[0] === "v1" && parts[1] === "admin" && parts[2] === "leases" && parts[3]) {
        return await this.adminLeaseRoute(request, parts[3], parts[4]);
      }
      if (method === "GET" && parts.join("/") === "v1/runs") {
        return await this.listRuns(request);
      }
      if (method === "GET" && parts.join("/") === "v1/runners") {
        return await this.listExternalRunners(request);
      }
      if (method === "POST" && parts.join("/") === "v1/runners/sync") {
        return await this.syncExternalRunners(request);
      }
      if (method === "POST" && parts.join("/") === "v1/runs") {
        return await this.createRun(request);
      }
      if (method === "POST" && parts.join("/") === "v1/artifacts/uploads") {
        return await this.createArtifactUploads(request);
      }
      if (parts[0] === "v1" && parts[1] === "runs" && parts[2]) {
        return await this.runRoute(request, parts[2], parts[3]);
      }
      if (method === "POST" && parts.join("/") === "v1/images") {
        return await this.createImage(request);
      }
      if (parts[0] === "v1" && parts[1] === "images" && parts[2]) {
        return await this.imageRoute(request, parts[2], parts[3]);
      }
      if (method === "GET" && parts.join("/") === "v1/leases") {
        return await this.listLeases(request);
      }
      if (method === "POST" && parts.join("/") === "v1/leases") {
        return await this.createLease(request);
      }
      if (method === "POST" && parts.join("/") === "v1/leases/capability-aware") {
        return await this.createLease(request);
      }
      if (method === "POST" && parts.join("/") === "v1/workspaces") {
        return await this.createWorkspace(request);
      }
      if (method === "GET" && parts.join("/") === "v1/native-vnc/handoff") {
        return await this.workspaceNativeVNC(request);
      }
      if (
        method === "GET" &&
        parts[0] === "v1" &&
        parts[1] === "workspaces" &&
        parts[2] &&
        parts[3] === "terminal"
      ) {
        return await this.workspaceTerminal(request, parts[2]);
      }
      if (parts[0] === "v1" && parts[1] === "workspaces" && parts[2]) {
        return await this.workspaceRoute(request, parts[2], parts[3], parts[4]);
      }
      if (parts[0] === "v1" && parts[1] === "adapters" && parts[2]) {
        if (parts[3] === "ticket" && parts.length === 4) {
          return await this.createRuntimeAdapterTicket(request, parts[2]);
        }
        if (parts[3] === "agent" && parts.length === 4) {
          return await this.runtimeAdapterAgent(request, parts[2]);
        }
        if (parts[3] === "proxy") {
          return await this.runtimeAdapterProxy(request, parts[2], parts.slice(4));
        }
        if (method === "GET" && parts.length === 3) {
          return await this.runtimeAdapterStatus(request, parts[2]);
        }
      }
      if (
        parts[0] === "v1" &&
        parts[1] === "leases" &&
        parts[2] &&
        parts[3] === "egress" &&
        parts[4] === "ticket"
      ) {
        return await this.createEgressTicket(request, parts[2]);
      }
      if (
        parts[0] === "v1" &&
        parts[1] === "leases" &&
        parts[2] &&
        parts[3] === "egress" &&
        parts[4] === "host"
      ) {
        return await this.egressAgent(request, parts[2], "host");
      }
      if (
        parts[0] === "v1" &&
        parts[1] === "leases" &&
        parts[2] &&
        parts[3] === "egress" &&
        parts[4] === "client"
      ) {
        return await this.egressAgent(request, parts[2], "client");
      }
      if (
        parts[0] === "v1" &&
        parts[1] === "leases" &&
        parts[2] &&
        parts[3] === "egress" &&
        parts[4] === "status"
      ) {
        return await this.egressStatus(request, parts[2]);
      }
      if (
        parts[0] === "v1" &&
        parts[1] === "leases" &&
        parts[2] &&
        parts[3] === "webvnc" &&
        parts[4] === "handoff"
      ) {
        return await this.webVNCCredentialHandoff(request, parts[2]);
      }
      if (
        parts[0] === "v1" &&
        parts[1] === "leases" &&
        parts[2] &&
        parts[3] === "webvnc" &&
        parts[4] === "ticket"
      ) {
        return await this.createWebVNCTicket(request, parts[2]);
      }
      if (
        parts[0] === "v1" &&
        parts[1] === "leases" &&
        parts[2] &&
        parts[3] === "webvnc" &&
        parts[4] === "status"
      ) {
        return await this.webVNCStatus(request, parts[2]);
      }
      if (
        parts[0] === "v1" &&
        parts[1] === "leases" &&
        parts[2] &&
        parts[3] === "webvnc" &&
        parts[4] === "reset"
      ) {
        return await this.webVNCReset(request, parts[2]);
      }
      if (
        parts[0] === "v1" &&
        parts[1] === "leases" &&
        parts[2] &&
        parts[3] === "webvnc" &&
        parts[4] === "agent"
      ) {
        return await this.webVNCAgent(request, parts[2]);
      }
      if (
        parts[0] === "v1" &&
        parts[1] === "leases" &&
        parts[2] &&
        parts[3] === "code" &&
        parts[4] === "ticket"
      ) {
        return await this.createCodeTicket(request, parts[2]);
      }
      if (
        parts[0] === "v1" &&
        parts[1] === "leases" &&
        parts[2] &&
        parts[3] === "code" &&
        parts[4] === "agent"
      ) {
        return await this.codeAgent(request, parts[2]);
      }
      if (parts[0] === "v1" && parts[1] === "leases" && parts[2]) {
        return await this.leaseRoute(request, parts[2], parts[3]);
      }
      return json({ error: "not_found" }, { status: 404 });
    } catch (error) {
      return json({ error: coordinatorErrorMessage(this.env, error) }, { status: 500 });
    }
  }

  async webSocketMessage(socket: WebSocket, message: string | ArrayBuffer): Promise<void> {
    const attachment = this.bridgeAttachment(socket);
    if (!attachment) {
      this.rejectRestoredBridgeSocket(socket, undefined);
      return;
    }
    await this.handleBridgeMessage(socket, attachment, message);
  }

  webSocketClose(socket: WebSocket, code: number, reason: string, _wasClean: boolean): void {
    this.handleBridgeClose(socket, code, reason);
  }

  webSocketError(socket: WebSocket, _error: unknown): void {
    this.handleBridgeClose(socket, 1011, "bridge socket error");
  }

  private restoreBridgeWebSockets(): void {
    const candidates: Array<{
      socket: WebSocket;
      attachment: BridgeAttachment;
      endpoint: string;
    }> = [];
    const endpointCounts = new Map<string, number>();
    const egressSessions = new Map<string, Set<string>>();
    for (const socket of this.state.getWebSockets()) {
      const attachment = this.bridgeAttachment(socket);
      if (!attachment) {
        this.rejectRestoredBridgeSocket(socket, undefined);
        continue;
      }
      if (socket.readyState !== WebSocket.OPEN) {
        continue;
      }
      const endpoint = restoredBridgeEndpoint(attachment);
      candidates.push({ socket, attachment, endpoint });
      endpointCounts.set(endpoint, (endpointCounts.get(endpoint) ?? 0) + 1);
      if (attachment.kind === "egress-host" || attachment.kind === "egress-client") {
        const sessions = egressSessions.get(attachment.leaseID) ?? new Set<string>();
        sessions.add(attachment.sessionID);
        egressSessions.set(attachment.leaseID, sessions);
      }
    }
    for (const { socket, attachment, endpoint } of candidates) {
      const ambiguousEgressLease =
        (attachment.kind === "egress-host" || attachment.kind === "egress-client") &&
        (egressSessions.get(attachment.leaseID)?.size ?? 0) > 1;
      if ((endpointCounts.get(endpoint) ?? 0) > 1 || ambiguousEgressLease) {
        this.rejectRestoredBridgeSocket(socket, attachment);
        continue;
      }
      this.trackBridgeSocket(socket, attachment);
      this.restoredBridgeSockets.add(socket);
    }
  }

  private async reconcileRestoredBridgeSockets(): Promise<void> {
    const leaseIDs = new Set<string>();
    for (const socket of this.restoredBridgeSockets) {
      const attachment = this.bridgeAttachment(socket);
      if (attachment && "leaseID" in attachment) {
        leaseIDs.add(attachment.leaseID);
      }
    }
    const [leaseEntries, adminGrants] = await Promise.all([
      Promise.all(
        [...leaseIDs].map(async (leaseID) => [leaseID, await this.getLease(leaseID)] as const),
      ),
      this.currentAdminGrantValidation(),
    ]);
    const leases = new Map(leaseEntries);
    const now = Date.now();
    const revoked = new Map<string, string>();
    const revokedAdmins = new Set<WebSocket>();
    const revokedAdminEgressSessions = new Map<string, { leaseID: string; sessionID: string }>();
    const revokedViewers = new Map<WebSocket, string>();
    const revokedEgressSessions = new Map<
      string,
      { leaseID: string; sessionID: string; reason: string }
    >();
    const githubBridgesToCheck: Array<{
      socket: WebSocket;
      attachment: Extract<
        BridgeAttachment,
        {
          kind:
            | "webvnc-agent"
            | "webvnc-viewer"
            | "code-agent"
            | "code-viewer"
            | "egress-host"
            | "egress-client";
        }
      >;
    }> = [];
    const sharedBridgesToCheck: Array<{
      socket: WebSocket;
      attachment: Extract<
        BridgeAttachment,
        {
          kind:
            | "webvnc-agent"
            | "webvnc-viewer"
            | "code-agent"
            | "code-viewer"
            | "egress-host"
            | "egress-client";
        }
      >;
    }> = [];
    const adminPortalAgentsToCheck: Array<{
      socket: WebSocket;
      attachment: Extract<BridgeAttachment, { kind: "webvnc-agent" | "code-agent" }>;
      portalSessionHash: string;
    }> = [];
    for (const socket of this.restoredBridgeSockets) {
      const attachment = this.bridgeAttachment(socket);
      if (!attachment) {
        continue;
      }
      if ("admin" in attachment && !cachedAdminGrantIsCurrent(attachment, adminGrants)) {
        revokedAdmins.add(socket);
        continue;
      }
      if (!("leaseID" in attachment)) {
        continue;
      }
      const lease = leases.get(attachment.leaseID);
      if (!lease || !leaseIsLive(lease) || Date.parse(lease.expiresAt) <= now) {
        revoked.set(
          attachment.leaseID,
          lease && leaseIsLive(lease) ? "lease expired" : "lease ended",
        );
        continue;
      }
      if (
        (attachment.kind === "webvnc-agent" || attachment.kind === "code-agent") &&
        attachment.admin === true &&
        validPortalSessionHash(attachment.portalSessionHash)
      ) {
        adminPortalAgentsToCheck.push({
          socket,
          attachment,
          portalSessionHash: attachment.portalSessionHash,
        });
      }
      if (
        (attachment.kind === "webvnc-viewer" || attachment.kind === "code-viewer") &&
        !this.leaseViewerAuthorized(lease, attachment)
      ) {
        revokedViewers.set(socket, "lease access revoked");
      }
      if (
        (attachment.kind === "webvnc-agent" || attachment.kind === "code-agent") &&
        completeBridgePrincipal(attachment) &&
        !this.leaseManagerAuthorized(lease, attachment)
      ) {
        revokedViewers.set(socket, "lease access revoked");
        continue;
      }
      if (
        (attachment.kind === "egress-host" || attachment.kind === "egress-client") &&
        !this.leaseManagerAuthorized(lease, attachment)
      ) {
        revokedEgressSessions.set(egressSocketKey(lease.id, attachment.sessionID), {
          leaseID: lease.id,
          sessionID: attachment.sessionID,
          reason: "lease access revoked",
        });
        continue;
      }
      if (revocableUserBridge(attachment)) {
        const portalRevocationCanOverrideLeaseAccess =
          attachment.kind === "code-viewer" &&
          attachment.auth === "github" &&
          validPortalSessionHash(attachment.portalSessionHash);
        if (revokedViewers.has(socket) && !portalRevocationCanOverrideLeaseAccess) {
          continue;
        }
        if (attachment.auth === undefined) {
          if (attachment.kind === "egress-host" || attachment.kind === "egress-client") {
            revokedEgressSessions.set(egressSocketKey(lease.id, attachment.sessionID), {
              leaseID: lease.id,
              sessionID: attachment.sessionID,
              reason: "bridge authentication expired",
            });
          } else {
            revokedViewers.set(socket, "bridge authentication expired");
          }
          continue;
        }
        if (
          attachment.auth === "github" &&
          (attachment.admin !== true ||
            attachment.kind === "webvnc-agent" ||
            attachment.kind === "code-agent")
        ) {
          githubBridgesToCheck.push({ socket, attachment });
        } else if (attachment.auth === "bearer" && attachment.admin !== true) {
          sharedBridgesToCheck.push({ socket, attachment });
        }
      }
    }
    const [githubBridgeRevocations, sharedBridgeRevocations, adminPortalAgentRevocations] =
      await Promise.all([
        Promise.all(
          githubBridgesToCheck.map(async ({ socket, attachment }) => ({
            socket,
            attachment,
            reason: await this.githubBridgeGrantFailureReason(attachment),
          })),
        ),
        Promise.all(
          sharedBridgesToCheck.map(async ({ socket, attachment }) => ({
            socket,
            attachment,
            reason: (await this.sharedBridgeGrantIsCurrent(attachment))
              ? undefined
              : "shared access revoked",
          })),
        ),
        Promise.all(
          adminPortalAgentsToCheck.map(async ({ socket, attachment, portalSessionHash }) => ({
            socket,
            attachment,
            reason: (await this.portalSessionIsRevoked(portalSessionHash))
              ? "portal session ended"
              : undefined,
          })),
        ),
      ]);
    for (const { socket, attachment, reason } of [
      ...githubBridgeRevocations,
      ...sharedBridgeRevocations,
      ...adminPortalAgentRevocations,
    ]) {
      if (!reason) continue;
      if (attachment.kind === "egress-host" || attachment.kind === "egress-client") {
        revokedEgressSessions.set(egressSocketKey(attachment.leaseID, attachment.sessionID), {
          leaseID: attachment.leaseID,
          sessionID: attachment.sessionID,
          reason,
        });
      } else {
        revokedViewers.set(socket, reason);
      }
    }
    for (const socket of revokedAdmins) {
      const attachment = this.bridgeAttachment(socket);
      if (attachment?.kind === "egress-host" || attachment?.kind === "egress-client") {
        revokedAdminEgressSessions.set(egressSocketKey(attachment.leaseID, attachment.sessionID), {
          leaseID: attachment.leaseID,
          sessionID: attachment.sessionID,
        });
        continue;
      }
      this.handleBridgeClose(socket, 1008, "admin access revoked");
      closeSocket(socket, 1008, "admin access revoked");
    }
    for (const { leaseID, sessionID } of revokedAdminEgressSessions.values()) {
      this.clearEgressSession(leaseID, sessionID, 1008, "admin access revoked");
    }
    for (const [leaseID, reason] of revoked) {
      this.closeLeaseBridges(leaseID, 1008, reason);
    }
    for (const { leaseID, sessionID, reason } of revokedEgressSessions.values()) {
      this.clearEgressSession(leaseID, sessionID, 1008, reason);
    }
    for (const socket of this.restoredBridgeSockets) {
      const attachment = this.bridgeAttachment(socket);
      const reason =
        attachment && "leaseID" in attachment ? revoked.get(attachment.leaseID) : undefined;
      if (reason) {
        closeSocket(socket, 1008, reason);
      }
    }
    const revokedUserBridges = [...revokedViewers].toSorted(([left], [right]) => {
      const leftAttachment = this.bridgeAttachment(left);
      const rightAttachment = this.bridgeAttachment(right);
      const leftIsAgent =
        leftAttachment?.kind === "webvnc-agent" || leftAttachment?.kind === "code-agent";
      const rightIsAgent =
        rightAttachment?.kind === "webvnc-agent" || rightAttachment?.kind === "code-agent";
      return Number(leftIsAgent) - Number(rightIsAgent);
    });
    for (const [socket, reason] of revokedUserBridges) {
      const attachment = this.bridgeAttachment(socket);
      if (attachment && revocableUserBridge(attachment)) {
        this.closeRevokedUserBridge(socket, attachment, reason);
      }
    }
    this.restoredBridgeSockets.clear();
  }

  private async restoredBridgesReady(): Promise<boolean> {
    if (this.restoredBridgeSockets.size === 0) {
      return true;
    }
    const pending =
      this.bridgeRestoreReady ??
      this.reconcileRestoredBridgeSockets().then(
        () => true,
        (error) => {
          console.warn(
            "could not reconcile restored bridges",
            coordinatorErrorMessage(this.env, error),
          );
          return false;
        },
      );
    this.bridgeRestoreReady = pending;
    const ready = await pending;
    if (this.bridgeRestoreReady === pending) {
      this.bridgeRestoreReady = undefined;
    }
    return ready;
  }

  private async reconcileAdminGrantVersion(request: Request): Promise<void> {
    const version = trustedAdminGrantVersion(request);
    if (!version) {
      return;
    }
    if (version === this.currentAdminGrantVersion) return;
    await this.applyAdminGrantVersion(version);
  }

  private async applyAdminGrantVersion(version: string): Promise<void> {
    this.currentAdminGrantVersion = version;
    const validation = await adminGrantValidation(this.env, version);
    if (this.currentAdminGrantVersion === version) {
      this.reconcileAdminBridgeSockets(validation);
    }
  }

  private adminBridgeSockets(): Set<WebSocket> {
    const sockets = new Set<WebSocket>([
      ...this.controlSockets.values(),
      ...this.codeAgents.values(),
      ...this.codeViewers.values(),
      ...this.egressHosts.values(),
      ...this.egressClients.values(),
    ]);
    for (const agents of this.webVNCAgents.values()) {
      for (const socket of agents.values()) {
        sockets.add(socket);
      }
    }
    for (const viewers of this.webVNCViewers.values()) {
      for (const viewer of viewers.values()) {
        sockets.add(viewer.socket);
      }
    }
    return sockets;
  }

  private reconcileAdminBridgeSockets(validation: AdminGrantValidation): void {
    const revokedEgressSessions = new Set<string>();
    for (const socket of this.adminBridgeSockets()) {
      const attachment = this.bridgeAttachment(socket);
      if (
        !attachment ||
        !("admin" in attachment) ||
        attachment.admin !== true ||
        cachedAdminGrantIsCurrent(attachment, validation)
      ) {
        continue;
      }
      this.adminGrantValidationTimes.delete(socket);
      if (attachment.kind === "egress-host" || attachment.kind === "egress-client") {
        const key = egressSocketKey(attachment.leaseID, attachment.sessionID);
        if (!revokedEgressSessions.has(key)) {
          revokedEgressSessions.add(key);
          this.clearEgressSession(
            attachment.leaseID,
            attachment.sessionID,
            1008,
            "admin access revoked",
          );
        }
        continue;
      }
      this.handleBridgeClose(socket, 1008, "admin access revoked");
      closeSocket(socket, 1008, "admin access revoked");
    }
  }

  private async reconcileScheduledAdminGrants(
    forwardedVersion?: string,
    preserveForwardedVersion = false,
  ): Promise<void> {
    let version =
      forwardedVersion ?? (preserveForwardedVersion ? this.currentAdminGrantVersion : undefined);
    if (!version) {
      const derivedVersion = await configuredAdminGrantVersion(this.env);
      version =
        preserveForwardedVersion && this.currentAdminGrantVersion
          ? this.currentAdminGrantVersion
          : derivedVersion;
    }
    await this.applyAdminGrantVersion(version);
  }

  private async currentAdminGrantValidation(): Promise<AdminGrantValidation> {
    if (!this.currentAdminGrantVersion) {
      const derivedVersion = await configuredAdminGrantVersion(this.env);
      this.currentAdminGrantVersion ||= derivedVersion;
    }
    return adminGrantValidation(this.env, this.currentAdminGrantVersion);
  }

  private rejectRestoredBridgeSocket(
    socket: WebSocket,
    attachment: BridgeAttachment | undefined,
  ): void {
    const webVNCAgent = attachment?.kind === "webvnc-agent";
    closeSocket(
      socket,
      webVNCAgent ? 1011 : 1012,
      webVNCAgent ? "WebVNC bridge reset" : "coordinator restarted",
    );
  }

  private acceptBridgeWebSocket(socket: WebSocket, attachment: BridgeAttachment): void {
    this.state.acceptWebSocket(socket, attachment, bridgeTags(attachment), {
      message: (data) => this.handleBridgeMessage(socket, attachment, data),
      close: (code, reason) => {
        this.handleBridgeClose(socket, code, reason);
      },
      error: () => {
        this.handleBridgeClose(socket, 1011, "bridge socket error");
      },
    });
  }

  private bridgeAttachment(socket: WebSocket): BridgeAttachment | undefined {
    return bridgeAttachment(this.state.socketAttachment(socket));
  }

  private bridgeSocketIsCurrent(socket: WebSocket, attachment: BridgeAttachment): boolean {
    switch (attachment.kind) {
      case "webvnc-agent":
        return this.webVNCAgents.get(attachment.leaseID)?.get(attachment.id) === socket;
      case "webvnc-viewer":
        return this.webVNCViewers.get(attachment.leaseID)?.get(attachment.id)?.socket === socket;
      case "code-agent":
        return this.codeAgents.get(attachment.leaseID) === socket;
      case "code-viewer":
        return this.codeViewers.get(attachment.id) === socket;
      case "egress-host":
        return (
          this.egressHosts.get(egressSocketKey(attachment.leaseID, attachment.sessionID)) === socket
        );
      case "egress-client":
        return (
          this.egressClients.get(egressSocketKey(attachment.leaseID, attachment.sessionID)) ===
          socket
        );
      case "runtime-adapter-agent":
        return this.runtimeAdapterAgents.get(attachment.adapterID) === socket;
      case "control":
        return this.controlSockets.get(attachment.clientID) === socket;
    }
  }

  private trackBridgeSocket(socket: WebSocket, attachment: BridgeAttachment): void {
    switch (attachment.kind) {
      case "webvnc-agent":
        this.trackWebVNCAgent(attachment.leaseID, attachment.id, socket, attachment.capabilities);
        break;
      case "webvnc-viewer":
        this.trackWebVNCViewer(attachment.leaseID, {
          id: attachment.id,
          agentID: attachment.agentID,
          socket,
          owner: attachment.owner,
          ...(attachment.org ? { org: attachment.org } : {}),
          ...(attachment.admin !== undefined ? { admin: attachment.admin } : {}),
          ...(attachment.auth ? { auth: attachment.auth } : {}),
          ...(attachment.login ? { login: attachment.login } : {}),
          ...(attachment.sharedTokenHash ? { sharedTokenHash: attachment.sharedTokenHash } : {}),
          ...(attachment.adminTokenHash ? { adminTokenHash: attachment.adminTokenHash } : {}),
          ...(attachment.adminGrantVersion
            ? { adminGrantVersion: attachment.adminGrantVersion }
            : {}),
          ...(attachment.portalSessionHash
            ? { portalSessionHash: attachment.portalSessionHash }
            : {}),
          ...(attachment.githubGrant ? { githubGrant: attachment.githubGrant } : {}),
          label: attachment.label,
          connectedAt: new Date().toISOString(),
        });
        break;
      case "code-agent":
        this.codeAgents.set(attachment.leaseID, socket);
        break;
      case "code-viewer":
        this.codeViewers.set(attachment.id, socket);
        break;
      case "egress-host":
        this.egressHosts.set(egressSocketKey(attachment.leaseID, attachment.sessionID), socket);
        this.trackEgressSession(attachment);
        break;
      case "egress-client":
        this.egressClients.set(egressSocketKey(attachment.leaseID, attachment.sessionID), socket);
        this.trackEgressSession(attachment);
        break;
      case "runtime-adapter-agent":
        closeSocket(
          this.runtimeAdapterAgents.get(attachment.adapterID),
          1012,
          "replaced by a newer runtime adapter agent",
        );
        this.runtimeAdapterAgents.set(attachment.adapterID, socket);
        break;
      case "control":
        this.controlSockets.set(attachment.clientID, socket);
        break;
    }
  }

  private trackWebVNCAgent(
    leaseID: string,
    agentID: string,
    socket: WebSocket,
    capabilities: Set<string>,
  ): void {
    const agents = this.webVNCAgents.get(leaseID) ?? new Map<string, WebSocket>();
    agents.set(agentID, socket);
    this.webVNCAgents.set(leaseID, agents);
    const agentsCapabilities = this.webVNCAgentCapabilities.get(leaseID) ?? new Map();
    agentsCapabilities.set(agentID, capabilities);
    this.webVNCAgentCapabilities.set(leaseID, agentsCapabilities);
  }

  private trackWebVNCViewer(leaseID: string, session: WebVNCViewerSession): void {
    const viewers = this.webVNCViewers.get(leaseID) ?? new Map<string, WebVNCViewerSession>();
    viewers.set(session.id, session);
    this.webVNCViewers.set(leaseID, viewers);
  }

  private trackEgressSession(attachment: Extract<BridgeAttachment, { sessionID: string }>): void {
    this.activateEgressSession(
      attachment.leaseID,
      attachment.sessionID,
      undefined,
      undefined,
      new Date(),
    );
  }

  private activateEgressSession(
    leaseID: string,
    sessionID: string,
    profile: string | undefined,
    allow: string[] | undefined,
    nowDate: Date,
  ): void {
    const previous = this.egressSessions.get(leaseID);
    if (!shouldActivateEgressSession(previous, sessionID, nowDate.toISOString())) {
      return;
    }
    if (previous && previous.sessionID !== sessionID) {
      this.clearEgressSession(
        leaseID,
        previous.sessionID,
        1012,
        "replaced by a newer egress session",
      );
    }
    const now = nowDate.toISOString();
    const sessionStatus: EgressSessionStatus = {
      leaseID,
      sessionID,
      allow: allow && allow.length > 0 ? allow : (previous?.allow ?? []),
      createdAt: previous?.sessionID === sessionID ? previous.createdAt : now,
      updatedAt: now,
    };
    const sessionProfile = profile || previous?.profile;
    if (sessionProfile) {
      sessionStatus.profile = sessionProfile;
    }
    this.egressSessions.set(leaseID, sessionStatus);
  }

  private async controlSocket(request: Request): Promise<Response> {
    if (request.headers.get("upgrade")?.toLowerCase() !== "websocket") {
      return json({ error: "websocket_required" }, { status: 426 });
    }
    const upgrade = this.state.createWebSocketUpgrade();
    const server = upgrade.socket;
    const admin = isAdminRequest(request);
    const attachment: BridgeAttachment = {
      kind: "control",
      clientID: crypto.randomUUID(),
      owner: requestOwner(request),
      org: requestOrg(request, this.env),
      admin,
      ...(await adminGrantForRequest(request, admin)),
      subscriptions: {},
    };
    this.controlSockets.set(attachment.clientID, server);
    this.acceptBridgeWebSocket(server, attachment);
    sendControl(server, {
      type: "hello",
      protocol: 1,
      clientID: attachment.clientID,
    });
    return upgrade.response;
  }

  private async handleControlMessage(
    socket: WebSocket,
    attachment: Extract<BridgeAttachment, { kind: "control" }>,
    message: string | ArrayBuffer | Blob,
  ): Promise<void> {
    if (typeof message !== "string") {
      sendControl(socket, {
        type: "error",
        code: "invalid_message",
        message: "expected JSON text",
      });
      return;
    }
    let input: ControlMessage;
    try {
      input = JSON.parse(message) as ControlMessage;
    } catch {
      sendControl(socket, { type: "error", code: "invalid_json", message: "invalid JSON" });
      return;
    }
    switch (input.type) {
      case "subscribe_run":
        await this.subscribeControlRun(socket, attachment, input);
        return;
      case "ack":
        this.ackControlRun(socket, attachment, input);
        return;
      case "heartbeat":
        await this.controlHeartbeat(socket, attachment, input);
        return;
      case "ping":
        sendControl(socket, { type: "pong" });
        return;
      default:
        sendControl(socket, {
          type: "error",
          code: "unknown_type",
          message: "unknown control message",
        });
    }
  }

  private async subscribeControlRun(
    socket: WebSocket,
    attachment: Extract<BridgeAttachment, { kind: "control" }>,
    input: Extract<ControlMessage, { type: "subscribe_run" }>,
  ): Promise<void> {
    const runID = typeof input.runID === "string" ? input.runID : "";
    const run = runID ? await this.getRun(runID) : undefined;
    const lease = run ? await this.ensureRunLeaseAttribution(run) : undefined;
    if (!run || !this.runReadableToControl(run, attachment, lease)) {
      sendControl(socket, { type: "error", code: "not_found", message: "run not found" });
      return;
    }
    const after = finiteControlNumber(input.after) ?? 0;
    const limit = Math.min(finiteControlNumber(input.limit) ?? 100, 500);
    const events = await this.runEvents(runID, after, limit);
    const nextSeq = events.at(-1)?.seq ?? after;
    attachment.subscriptions = { ...attachment.subscriptions, [runID]: nextSeq };
    this.serializeBridgeAttachment(socket, attachment);
    sendControl(socket, { type: "run_events", runID, events, nextSeq });
  }

  private ackControlRun(
    socket: WebSocket,
    attachment: Extract<BridgeAttachment, { kind: "control" }>,
    input: Extract<ControlMessage, { type: "ack" }>,
  ): void {
    if (typeof input.runID !== "string") {
      return;
    }
    const seq = finiteControlNumber(input.seq);
    if (seq === undefined) {
      return;
    }
    attachment.subscriptions = { ...attachment.subscriptions, [input.runID]: seq };
    this.serializeBridgeAttachment(socket, attachment);
  }

  private async controlHeartbeat(
    socket: WebSocket,
    attachment: Extract<BridgeAttachment, { kind: "control" }>,
    input: Extract<ControlMessage, { type: "heartbeat" }>,
  ): Promise<void> {
    const leaseID = typeof input.leaseID === "string" ? input.leaseID : "";
    const lease = leaseID ? await this.resolveLeaseForControl(leaseID, attachment) : undefined;
    if (!lease) {
      sendControl(socket, { type: "heartbeat", leaseID, ok: false, error: "not_found" });
      return;
    }
    if (lease.workspaceID) {
      sendControl(socket, {
        type: "heartbeat",
        leaseID: lease.id,
        ok: false,
        error: "workspace_managed_lease",
      });
      return;
    }
    if (lease.cleanupStartedAt) {
      sendControl(socket, {
        type: "heartbeat",
        leaseID: lease.id,
        ok: false,
        error: "cleanup_in_progress",
      });
      return;
    }
    const heartbeatError = leaseHeartbeatStateError(lease);
    if (heartbeatError) {
      sendControl(socket, {
        type: "heartbeat",
        leaseID: lease.id,
        ok: false,
        error: heartbeatError,
      });
      return;
    }
    const heartbeat: { idleTimeoutSeconds?: number; telemetry?: Partial<LeaseTelemetry> } = {};
    if (input.idleTimeoutSeconds !== undefined) {
      heartbeat.idleTimeoutSeconds = input.idleTimeoutSeconds;
    }
    if (input.telemetry !== undefined) {
      heartbeat.telemetry = input.telemetry;
    }
    const updated = await this.applyLeaseHeartbeatState(lease, heartbeat);
    sendControl(socket, {
      type: "heartbeat",
      leaseID: lease.id,
      ok: true,
      expiresAt: updated.expiresAt,
    });
  }

  private serializeBridgeAttachment(socket: WebSocket, attachment: BridgeAttachment): void {
    this.state.setSocketAttachment(socket, attachment);
  }

  private async handleBridgeMessage(
    socket: WebSocket,
    attachment: BridgeAttachment,
    message: string | ArrayBuffer | Blob,
  ): Promise<void> {
    if (socket.readyState !== WebSocket.OPEN || !this.bridgeSocketIsCurrent(socket, attachment)) {
      this.rejectRestoredBridgeSocket(socket, attachment);
      return;
    }
    if (!(await this.restoredBridgesReady())) {
      this.restoredBridgeSockets.delete(socket);
      this.rejectRestoredBridgeSocket(socket, attachment);
      return;
    }
    if (socket.readyState !== WebSocket.OPEN || !this.bridgeSocketIsCurrent(socket, attachment)) {
      return;
    }
    if (!(await this.activeBridgeGrantIsCurrent(socket, attachment))) {
      return;
    }
    switch (attachment.kind) {
      case "webvnc-agent":
        await forwardOrBufferWebVNC(
          message,
          await this.currentBridgeRecipient(
            this.webVNCViewerForAgent(attachment.leaseID, attachment.id)?.socket,
          ),
          this.pendingWebVNCToViewer,
          webVNCBufferKey(attachment.leaseID, attachment.id),
        );
        break;
      case "webvnc-viewer":
        if (isReservedWebVNCControlFrame(message)) {
          return;
        }
        await forwardWebVNC(
          message,
          await this.currentBridgeRecipient(
            this.webVNCAgents.get(attachment.leaseID)?.get(attachment.agentID),
          ),
        );
        break;
      case "code-agent":
        await this.handleCodeAgentMessage(attachment.leaseID, message);
        break;
      case "code-viewer": {
        const agent = await this.currentBridgeRecipient(this.codeAgents.get(attachment.leaseID));
        if (!agent) {
          return;
        }
        const data = await normalizeWebVNCData(message);
        const bytes = typeof data === "string" ? textEncoder.encode(data) : new Uint8Array(data);
        this.sendCodeWebSocketData(agent, {
          type: "ws_data",
          id: attachment.id,
          frame: typeof data === "string" ? "text" : "binary",
          body: bytesToBase64(bytes),
        });
        break;
      }
      case "egress-host":
        await forwardEgress(
          message,
          await this.currentBridgeRecipient(
            this.egressClients.get(egressSocketKey(attachment.leaseID, attachment.sessionID)),
          ),
        );
        break;
      case "egress-client":
        await forwardEgress(
          message,
          await this.currentBridgeRecipient(
            this.egressHosts.get(egressSocketKey(attachment.leaseID, attachment.sessionID)),
          ),
        );
        break;
      case "runtime-adapter-agent":
        await this.handleRuntimeAdapterAgentMessage(attachment.adapterID, socket, message);
        break;
      case "control":
        await this.handleControlMessage(socket, attachment, message);
        break;
    }
    void socket;
  }

  private async currentBridgeRecipient(
    socket: WebSocket | undefined,
  ): Promise<WebSocket | undefined> {
    if (!socket || socket.readyState !== WebSocket.OPEN) {
      return undefined;
    }
    const attachment = this.bridgeAttachment(socket);
    if (
      !attachment ||
      !this.bridgeSocketIsCurrent(socket, attachment) ||
      !(await this.activeBridgeGrantIsCurrent(socket, attachment))
    ) {
      return undefined;
    }
    return socket;
  }

  private async activeBridgeGrantIsCurrent(
    socket: WebSocket,
    attachment: BridgeAttachment,
  ): Promise<boolean> {
    if (
      revocableUserBridge(attachment) &&
      attachment.auth === "bearer" &&
      attachment.admin !== true
    ) {
      const now = Date.now();
      const lastValidatedAt = this.userGrantValidationTimes.get(socket) ?? 0;
      if (now - lastValidatedAt >= userGrantRevalidationIntervalMs) {
        if (!(await this.sharedBridgeGrantIsCurrent(attachment))) {
          this.userGrantValidationTimes.delete(socket);
          this.closeRevokedUserBridge(socket, attachment, "shared access revoked");
          return false;
        }
        this.userGrantValidationTimes.set(socket, now);
      }
    }
    if (
      revocableUserBridge(attachment) &&
      attachment.auth === "github" &&
      (attachment.admin !== true ||
        attachment.kind === "webvnc-agent" ||
        attachment.kind === "code-agent")
    ) {
      const now = Date.now();
      const lastValidatedAt = this.userGrantValidationTimes.get(socket) ?? 0;
      if (now - lastValidatedAt >= userGrantRevalidationIntervalMs) {
        const failureReason = await this.githubBridgeGrantFailureReason(attachment);
        if (failureReason) {
          this.userGrantValidationTimes.delete(socket);
          this.closeRevokedUserBridge(socket, attachment, failureReason);
          return false;
        }
        this.userGrantValidationTimes.set(socket, now);
      }
    }
    if (!("admin" in attachment) || attachment.admin !== true) {
      return true;
    }
    const now = Date.now();
    const lastValidatedAt = this.adminGrantValidationTimes.get(socket) ?? 0;
    if (now - lastValidatedAt < adminGrantRevalidationIntervalMs) {
      return true;
    }
    if (cachedAdminGrantIsCurrent(attachment, await this.currentAdminGrantValidation())) {
      this.adminGrantValidationTimes.set(socket, now);
      return true;
    }
    this.adminGrantValidationTimes.delete(socket);
    if (attachment.kind === "egress-host" || attachment.kind === "egress-client") {
      this.clearEgressSession(
        attachment.leaseID,
        attachment.sessionID,
        1008,
        "admin access revoked",
      );
      return false;
    }
    this.handleBridgeClose(socket, 1008, "admin access revoked");
    closeSocket(socket, 1008, "admin access revoked");
    return false;
  }

  private async sharedBridgeGrantIsCurrent(attachment: CachedBridgeGrant): Promise<boolean> {
    const sharedToken = this.env.CRABBOX_SHARED_TOKEN;
    return (
      typeof sharedToken === "string" &&
      sharedToken.length > 0 &&
      /^[a-f0-9]{64}$/.test(attachment.sharedTokenHash ?? "") &&
      attachment.sharedTokenHash === (await sha256Hex(sharedToken))
    );
  }

  private async githubBridgeGrantFailureReason(
    attachment: CachedBridgeGrant & { owner?: string; org?: string; admin?: boolean },
  ): Promise<string | undefined> {
    if (
      validPortalSessionHash(attachment.portalSessionHash) &&
      (await this.portalSessionIsRevoked(attachment.portalSessionHash))
    ) {
      return "portal session ended";
    }
    const org = typeof attachment.org === "string" ? orgLabelFromKey(attachment.org) : undefined;
    if (
      typeof attachment.owner !== "string" ||
      !org ||
      typeof attachment.admin !== "boolean" ||
      typeof attachment.login !== "string" ||
      !validPortalSessionHash(attachment.portalSessionHash) ||
      !attachment.githubGrant
    ) {
      return "user access revoked";
    }
    return (await githubUserGrantIsCurrent(
      attachment.githubGrant,
      {
        owner: attachment.owner,
        org,
        login: attachment.login,
      },
      this.env,
    ))
      ? undefined
      : "user access revoked";
  }

  private closeRevokedUserBridge(
    socket: WebSocket,
    attachment: Extract<
      BridgeAttachment,
      {
        kind:
          | "webvnc-agent"
          | "webvnc-viewer"
          | "code-agent"
          | "code-viewer"
          | "egress-host"
          | "egress-client";
      }
    >,
    reason: string,
  ): void {
    if (attachment.kind === "egress-host" || attachment.kind === "egress-client") {
      this.clearEgressSession(attachment.leaseID, attachment.sessionID, 1008, reason);
      return;
    }
    this.handleBridgeClose(socket, 1008, reason);
    closeSocket(socket, 1008, reason);
  }

  private handleBridgeClose(socket: WebSocket, code: number, reason: string): void {
    this.restoredBridgeSockets.delete(socket);
    const attachment = this.bridgeAttachment(socket);
    if (!attachment || !this.bridgeSocketIsCurrent(socket, attachment)) {
      return;
    }
    switch (attachment.kind) {
      case "webvnc-agent":
        this.clearWebVNCAgent(attachment.leaseID, attachment.id, socket);
        break;
      case "webvnc-viewer":
        this.clearWebVNCViewer(attachment.leaseID, attachment.id, socket);
        break;
      case "code-agent":
        this.clearCodeAgent(attachment.leaseID, socket);
        break;
      case "code-viewer":
        this.clearCodeViewer(attachment.leaseID, attachment.id, socket, code, reason);
        break;
      case "egress-host":
        this.clearEgressHost(attachment.leaseID, attachment.sessionID, socket);
        break;
      case "egress-client":
        this.clearEgressClient(attachment.leaseID, attachment.sessionID, socket);
        break;
      case "runtime-adapter-agent":
        this.clearRuntimeAdapterAgent(attachment.adapterID, socket);
        break;
      case "control":
        if (this.controlSockets.get(attachment.clientID) === socket) {
          this.controlSockets.delete(attachment.clientID);
        }
        break;
    }
  }

  async alarm(): Promise<void> {
    await this.runScheduledMaintenance();
  }

  protected async durableObjectAlarm(): Promise<void> {
    await this.runScheduledMaintenance(undefined, true);
  }

  private async runScheduledMaintenance(
    forwardedAdminGrantVersion?: string,
    preserveForwardedVersion = false,
  ): Promise<void> {
    if (!(await this.restoredBridgesReady())) {
      throw new Error("restored bridge lease state is temporarily unavailable");
    }
    await this.reconcileScheduledAdminGrants(forwardedAdminGrantVersion, preserveForwardedVersion);
    await this.quarantineLegacyWorkspaces();
    await this.expireLeases();
    await this.webVNCCredentialHandoffs.cleanupExpired();
    await this.reconcileRuntimeAdapterDeletes();
    await this.maintainWorkspacePrewarm();
    await this.provisionPendingWorkspace();
    await this.maintainWorkspacePrewarm();
    await this.pruneTerminalWorkspaces();
    await this.pruneTerminalRuns();
    await this.runAzureDeferredCleanups();
    await this.runAWSOrphanSweepIfDue("alarm");
    await this.runAzureOrphanSweepIfDue("alarm");
    await this.reconcileAWSIngressIfIdle();
    await this.state.runExclusive(() => this.scheduleAlarm());
  }

  private async scheduledMaintenance(request: Request): Promise<Response> {
    if (request.headers.get("x-crabbox-internal") !== "scheduled") {
      return json({ error: "unauthorized" }, { status: 401 });
    }
    await this.runScheduledMaintenance(trustedAdminGrantVersion(request));
    return json({ ok: true });
  }

  private async createLease(
    request: Request,
    reservationGuard?: () => Promise<Response | undefined>,
    workspaceID?: string,
    workspaceCapability?: ProviderWorkspaceCapability,
  ): Promise<Response> {
    const owner = requestOwner(request);
    const org = requestOrg(request, this.env);
    const input = await readJson<LeaseRequest>(request);
    const defaults: LeaseConfigDefaults = {};
    const azureImage = this.env.CRABBOX_AZURE_IMAGE?.trim();
    if (azureImage) defaults.azureImage = azureImage;
    const azureWindowsARM64Image = this.env.CRABBOX_AZURE_WINDOWS_ARM64_IMAGE?.trim();
    if (azureWindowsARM64Image) defaults.azureWindowsARM64Image = azureWindowsARM64Image;
    if (this.env.CRABBOX_AZURE_OS_DISK) defaults.azureOSDisk = this.env.CRABBOX_AZURE_OS_DISK;
    let config: LeaseConfig;
    try {
      config = leaseConfig(input, defaults);
    } catch (error) {
      if (error instanceof InvalidAWSRegionError) {
        return json({ error: "invalid_region", message: error.message }, { status: 400 });
      }
      if (error instanceof InvalidImageCapabilitiesError) {
        return json(
          { error: "invalid_image_requirements", message: error.message },
          { status: 400 },
        );
      }
      throw error;
    }
    if (hasImageRequirements(config.imageRequirements) && config.provider !== "aws") {
      return json(
        {
          error: "image_capability_unsupported",
          message: "image capability selection currently requires provider=aws",
        },
        { status: 409 },
      );
    }
    if (workspaceID) {
      config = { ...config, awsUseStockImage: config.provider === "aws" };
      if (!workspaceCapability) {
        const hostKeys = workspaceSSHHostKeysFromRequest(request);
        if (!hostKeys) {
          return json(
            {
              error: "workspace_host_key_missing",
              message: "workspace SSH host identity is required",
            },
            { status: 500 },
          );
        }
        config = {
          ...config,
          sshHostPrivateKey: hostKeys.privateKey,
          sshHostPublicKey: hostKeys.publicKey,
        };
      }
    }
    if (!workspaceID && config.providerKey.startsWith(workspaceProviderKeyPrefix)) {
      return json(
        {
          error: "reserved_provider_key",
          message: `${workspaceProviderKeyPrefix}* is reserved for workspace provisioning`,
        },
        { status: 400 },
      );
    }
    const configProvider = this.provider(
      config.provider,
      providerRegionForConfig(config),
      providerProjectForConfig(config),
    );
    if (!workspaceID && !isAdminRequest(request)) {
      const restrictedFields = configProvider.restrictedLeaseRequestFields?.(input) ?? [];
      if (restrictedFields.length > 0) {
        return json(
          {
            error: "admin_required",
            message: `brokered ${config.provider} resource selectors require admin-token auth: ${restrictedFields.join(", ")}`,
            fields: restrictedFields,
          },
          { status: 403 },
        );
      }
    }
    if (!isAdminRequest(request) && hasNativeLeaseSource(config)) {
      return json(
        {
          error: "admin_required",
          message: "native snapshot/image lease sources require admin-token auth",
        },
        { status: 403 },
      );
    }
    const requestedHostID = config.hostID || config.awsMacHostID;
    const retainedMacHostLease =
      requestedHostID && config.provider === "aws" && config.target === "macos"
        ? await this.retainedMacHostLease(
            owner,
            org,
            requestedHostID,
            config.serverTypeExplicit ? config.serverType : undefined,
          )
        : undefined;
    const reusesOwnedReleasedMacHost = Boolean(retainedMacHostLease);
    if (!isAdminRequest(request) && requestedHostID && !reusesOwnedReleasedMacHost) {
      return json(
        {
          error: "admin_required",
          message: "provider host pinning requires admin-token auth",
        },
        { status: 403 },
      );
    }
    if (retainedMacHostLease) {
      if (hasImageRequirements(config.imageRequirements)) {
        return json(
          {
            error: "image_capability_mismatch",
            message: "image capability requirements cannot be verified when reusing an instance",
          },
          { status: 409 },
        );
      }
      const missingCapabilities = [
        config.desktop && !retainedMacHostLease.desktop ? "desktop" : "",
        config.browser && !retainedMacHostLease.browser ? "browser" : "",
        config.code && !retainedMacHostLease.code ? "code" : "",
      ].filter(Boolean);
      if (missingCapabilities.length > 0) {
        return json(
          {
            error: "retained_instance_capability_mismatch",
            message: `retained EC2 Mac instance lacks requested capabilities: ${missingCapabilities.join(", ")}`,
          },
          { status: 409 },
        );
      }
    }
    if (retainedMacHostLease && !config.serverTypeExplicit) {
      config = { ...config, serverType: retainedMacHostLease.serverType };
    }
    if (retainedMacHostLease) {
      config = { ...config, providerKey: retainedMacHostLease.providerKey };
    }
    const leaseID = validLeaseID(input.leaseID) ? input.leaseID : newLeaseID();
    const canonicalProviderKey = providerKeyForLease(leaseID);
    const providerKeyLeaseID = leaseIDForProviderKey(config.providerKey);
    if (!retainedMacHostLease && providerKeyLeaseID && providerKeyLeaseID !== leaseID) {
      return json(
        {
          error: "reserved_provider_key",
          message: `provider key ${config.providerKey} is reserved for lease ${providerKeyLeaseID}`,
        },
        { status: 400 },
      );
    }
    if (
      !workspaceID &&
      !retainedMacHostLease &&
      !isAdminRequest(request) &&
      config.providerKey &&
      config.providerKey !== canonicalProviderKey
    ) {
      return json(
        {
          error: "admin_required",
          message: "custom provider key names require admin-token auth",
        },
        { status: 403 },
      );
    }
    if (!config.providerKey) {
      config = { ...config, providerKey: canonicalProviderKey };
    }
    const readiness = this.providerConfigurationReadiness(
      config.provider,
      providerProjectForConfig(config),
    );
    if (!readiness.configured) {
      return json(
        {
          error: "provider_not_configured",
          provider: readiness.provider,
          missing: readiness.missing,
          message: readiness.message,
        },
        { status: 424 },
      );
    }
    try {
      config = (await configProvider.prepareLeaseConfig?.(config)) ?? config;
    } catch (error) {
      if (error instanceof ImageCapabilityMismatchError) {
        return json(
          { error: "image_capability_mismatch", message: error.message },
          { status: 409 },
        );
      }
      throw error;
    }
    const provider = this.provider(
      config.provider,
      providerRegionForConfig(config),
      providerProjectForConfig(config),
    );
    const injectsSSHHostKey = !workspaceCapability && provider.supportsSSHHostKeyInjection(config);
    if (injectsSSHHostKey) {
      config = withSSHHostKey(config, `crabbox-${leaseID}`);
    }
    const providerHourlyUSD = await provider
      .hourlyPriceUSD(config.serverType, config)
      .catch(() => undefined);
    const cost = leaseCost(
      this.env,
      config.provider,
      config.serverType,
      config.ttlSeconds,
      providerHourlyUSD,
    );
    if (!workspaceID && retainedMacHostLease) {
      const reactivation = await this.state.runExclusive(async () => {
        const current = await this.getLease(retainedMacHostLease.id);
        if (
          !current ||
          current.state !== "released" ||
          current.releaseDeletesServer !== false ||
          !current.cloudID ||
          current.owner !== owner ||
          current.org !== org ||
          current.provider !== "aws" ||
          current.target !== "macos" ||
          leaseHostID(current) !== requestedHostID ||
          current.serverType !== config.serverType
        ) {
          return undefined;
        }
        const blocked = await reservationGuard?.();
        if (blocked) {
          return blocked;
        }
        const now = new Date();
        const admission = await this.leaseAdmissionState({ owner, org }, now, current.id);
        let reactivated: LeaseRecord = {
          ...current,
          profile: config.profile,
          class: config.class,
          requestedServerType: config.serverType,
          keep: config.keep,
          ttlSeconds: config.ttlSeconds,
          idleTimeoutSeconds: config.idleTimeoutSeconds,
          estimatedHourlyUSD: cost.hourlyUSD,
          maxEstimatedUSD: cost.maxUSD,
          state: "active",
          createdAt: now.toISOString(),
          updatedAt: now.toISOString(),
          lastTouchedAt: now.toISOString(),
          expiresAt: leaseExpiresAt(
            now,
            now,
            config.ttlSeconds,
            config.idleTimeoutSeconds,
          ).toISOString(),
        };
        const sourceCIDRs = awsLeaseSSHSourceCIDRs(
          config,
          providerAccessContext(requestSourceCIDRs(request), admission.accessLeases),
        );
        reactivated = withLeaseSSHSourceCIDRs(
          reactivated,
          uniqueNonEmpty([...(current.network?.sshSourceCIDRs ?? []), ...sourceCIDRs]),
          Boolean(
            current.network?.sshSourceCIDRsComplete ||
            sourceCIDRs.length > 0 ||
            awsGlobalSSHSourceCIDRs(this.env).length > 0,
          ),
        );
        if (config.awsSSHCIDRsPinned) {
          reactivated.network = {
            ...reactivated.network,
            sshPinnedSourceCIDRs: uniqueNonEmpty([
              ...(current.network?.sshPinnedSourceCIDRs ?? []),
              ...config.awsSSHCIDRs,
            ]),
          };
        }
        delete reactivated.releasedAt;
        delete reactivated.endedAt;
        delete reactivated.releaseDeletesServer;
        delete reactivated.failureError;
        delete reactivated.provisioningRequestStartedAt;
        clearLeaseCleanupMetadata(reactivated);
        const limitError = enforceCostLimitUsage(
          admission.costUsage,
          reactivated,
          costLimits(this.env),
        );
        if (limitError) {
          return json({ error: "cost_limit_exceeded", message: limitError }, { status: 429 });
        }
        await this.putLease(reactivated);
        await this.markAWSIngressReconcilePending(reactivated);
        await this.scheduleAlarm();
        return { previous: structuredClone(current), reactivated };
      });
      if (reactivation instanceof Response) {
        return reactivation;
      }
      if (reactivation) {
        try {
          if (provider.reconcileLeaseAccess) {
            const accessLeases = await this.providerAccessLeaseRecords();
            await this.withAWSIngressOperationLock(() =>
              provider.reconcileLeaseAccess!(
                reactivation.reactivated,
                providerAccessContext(requestSourceCIDRs(request), accessLeases),
              ),
            );
          }
        } catch (error) {
          await this.state.runExclusive(async () => {
            const current = await this.getLease(reactivation.reactivated.id);
            if (
              current?.state === "active" &&
              current.updatedAt === reactivation.reactivated.updatedAt
            ) {
              await this.putLease(reactivation.previous);
              await this.scheduleAlarm();
            }
          });
          throw error;
        }
        return json({ lease: publicLeaseRecord(reactivation.reactivated) }, { status: 201 });
      }
      return json(
        {
          error: "retained_instance_unavailable",
          message: "retained EC2 Mac instance is no longer available for reactivation",
        },
        { status: 409 },
      );
    }
    const reservation = await this.state.runExclusive(async () => {
      const blocked = await reservationGuard?.();
      if (blocked) {
        return blocked;
      }
      if (!workspaceID && (await this.state.storage.get(workspaceLeaseReservationKey(leaseID)))) {
        return workspaceManagedLeaseResponse();
      }
      if (await this.getLease(leaseID)) {
        return json(
          { error: "lease_id_conflict", message: "lease id already exists" },
          { status: 409 },
        );
      }
      const now = new Date();
      const admission = await this.leaseAdmissionState({ owner, org }, now);
      const slug = allocateLeaseSlug(
        normalizeLeaseSlug(input.slug ?? input.requestedSlug) || leaseSlugFromID(leaseID),
        leaseID,
        owner,
        org,
        admission.accessLeases,
      );
      const providerProject = providerProjectForConfig(config);
      const providerRegion = ["aws", "azure", "gcp"].includes(config.provider)
        ? providerRegionForConfig(config)
        : undefined;
      let record: LeaseRecord = {
        id: leaseID,
        slug,
        ...(workspaceID ? { workspaceID } : {}),
        provider: config.provider,
        target: config.target,
        os: config.os,
        desktop: config.desktop,
        desktopEnv: config.desktopEnv,
        browser: config.browser,
        code: config.code,
        cloudID: "",
        owner,
        providerOwner: owner,
        org,
        profile: config.profile,
        class: config.class,
        serverType: config.serverType,
        requestedServerType: config.serverType,
        serverID: 0,
        serverName: "",
        providerKey: config.providerKey,
        host: "",
        sshUser: config.sshUser,
        sshPort: config.sshPort,
        sshFallbackPorts: config.sshFallbackPorts,
        ...(injectsSSHHostKey ? { sshHostKey: sshPublicKeyIdentity(config.sshHostPublicKey) } : {}),
        workRoot: config.workRoot,
        keep: config.keep,
        ttlSeconds: config.ttlSeconds,
        idleTimeoutSeconds: config.idleTimeoutSeconds,
        estimatedHourlyUSD: cost.hourlyUSD,
        maxEstimatedUSD: cost.maxUSD,
        state: "provisioning",
        createdAt: now.toISOString(),
        updatedAt: now.toISOString(),
        lastTouchedAt: now.toISOString(),
        expiresAt: leaseExpiresAt(
          now,
          now,
          config.ttlSeconds,
          config.idleTimeoutSeconds,
        ).toISOString(),
      };
      if (providerProject) {
        record.providerProject = providerProject;
      }
      if (providerRegion) {
        record.region = providerRegion;
      }
      if (requestedHostID) {
        record.hostId = requestedHostID;
      }
      if (config.target === "windows") {
        record.windowsMode = config.windowsMode;
      }
      if (config.pond) {
        record.pond = config.pond;
      }
      if (config.exposedPorts.length > 0) {
        record.exposedPorts = config.exposedPorts;
      }
      if (config.tailscale) {
        record.tailscale = {
          enabled: true,
          state: "requested",
        };
      }
      const limitError = enforceCostLimitUsage(admission.costUsage, record, costLimits(this.env));
      if (limitError) {
        return json({ error: "cost_limit_exceeded", message: limitError }, { status: 429 });
      }
      await this.putLease(record);
      await this.scheduleAlarm();
      return { record, slug };
    });
    if (reservation instanceof Response) {
      return reservation;
    }
    let { record } = reservation;
    const { slug } = reservation;
    const tailscaleError = await this.prepareTailscaleConfig(config, input, leaseID, slug);
    if (tailscaleError) {
      await this.cancelLeaseReservation(record);
      await this.removeReleasedLeaseReservation(record);
      return tailscaleError;
    }
    record = withRequestedTailscaleMetadata(record, config);
    let preparation:
      | {
          committed: false;
          current: LeaseRecord | undefined;
        }
      | {
          committed: true;
          config: LeaseConfig;
          prepared: ProviderLeaseCreatePreparation | undefined;
          record: LeaseRecord;
        };
    try {
      preparation = await this.state.runExclusive(async () => {
        const latest = await this.getLease(record.id);
        if (!latest || latest.state !== "provisioning") {
          return { committed: false as const, current: latest };
        }
        const accessLeases = await this.providerAccessLeaseRecords();
        const accessContext = providerAccessContext(requestSourceCIDRs(request), accessLeases);
        const prepared = await provider.prepareLeaseCreate?.(config, record, accessContext);
        const preparedConfig = prepared?.config ?? config;
        const preparedRecord = prepared?.lease ?? record;
        const committedRecord = applyLeaseRecordChanges(latest, reservation.record, preparedRecord);
        if (prepared?.provisioning?.publishAccessBeforeProvisioning) {
          await this.putProviderAccess(providerAccessReservation(committedRecord, new Date()));
        }
        await this.putLease(committedRecord);
        if (
          committedRecord.provider === "aws" &&
          prepared?.provisioning?.sshIngressReconcile === "additive"
        ) {
          await this.markAWSIngressReconcilePending(committedRecord);
        }
        await this.scheduleAlarm();
        return {
          committed: true as const,
          config: preparedConfig,
          prepared,
          record: committedRecord,
        };
      });
    } catch (error) {
      await this.cancelLeaseReservation(record);
      await this.removeReleasedLeaseReservation(record);
      throw error;
    }
    if (!preparation.committed) {
      const current = await this.removeReleasedLeaseReservation(record);
      return json(
        {
          error: "lease_state_changed",
          message: "lease changed state while provider preparation was in progress",
          lease: current ? publicLeaseRecord(current) : undefined,
        },
        { status: 409 },
      );
    }
    config = preparation.config;
    const { prepared } = preparation;
    record = preparation.record;
    let lastProvisioningTarget: ProviderProvisioningTarget | undefined;
    const provisioning = prepared?.provisioning
      ? {
          ...prepared.provisioning,
          onTargetAttempt: async (target: ProviderProvisioningTarget) => {
            lastProvisioningTarget = { ...target };
            await prepared.provisioning?.onTargetAttempt?.(target);
            const region = target.region;
            if (!region) {
              return;
            }
            await this.state.runExclusive(async () => {
              const current = (await this.getLease(record.id)) ?? record;
              if (record.provider === "aws") {
                await this.markAWSIngressReconcilePending({ ...current, region });
              } else {
                current.region = region;
                current.updatedAt = new Date().toISOString();
                await this.putLease(current);
              }
              await this.scheduleAlarm();
            });
          },
        }
      : undefined;
    const provisioningStart = await this.state.runExclusive(async () => {
      const current = await this.getLease(record.id);
      if (!current || current.state !== "provisioning") {
        return { started: false as const, current };
      }
      if (current.workspaceID) {
        const workspace = await this.state.storage.get<WorkspaceRecord>(
          workspaceKey(current.owner, current.org, current.workspaceID),
        );
        if (workspace?.releaseRequestedAt && workspaceLeaseMatchesCleanup(workspace, current)) {
          if (prepared?.provisioning?.publishAccessBeforeProvisioning) {
            await this.deleteProviderAccess(current.id);
          }
          return { started: false as const, current, workspace };
        }
      }
      current.provisioningRequestStartedAt = new Date().toISOString();
      current.updatedAt = current.provisioningRequestStartedAt;
      await this.putLease(current);
      return { started: true as const, current };
    });
    if (!provisioningStart.started) {
      const current = provisioningStart.workspace
        ? await this.finalizeAbsentWorkspaceLease(
            provisioningStart.workspace,
            provisioningStart.current,
          )
        : await this.removeReleasedLeaseReservation(record);
      return json(
        {
          error: "lease_state_changed",
          message: "lease changed state before provider provisioning began",
          lease: current ? publicLeaseRecord(current) : undefined,
        },
        { status: 409 },
      );
    }
    record = provisioningStart.current;
    const provision = () =>
      provider.createServerWithFallback(config, leaseID, slug, owner, provisioning);
    const provisioned =
      config.provider !== "aws" || config.awsPrivate
        ? provision()
        : prepared?.provisioning?.sshIngressReconcile === "additive"
          ? this.withAWSIngressAdditiveOperation(provision).catch(async (error: unknown) => {
              if (!isAWSSecurityGroupRuleLimitError(coordinatorErrorMessage(this.env, error))) {
                throw error;
              }
              return await this.withAWSIngressOperationLock(async () => {
                const recovery = await this.state.runExclusive(async () => {
                  const current = await this.getLease(record.id);
                  if (!current || current.state !== "provisioning") {
                    return undefined;
                  }
                  const accessLeases = await this.providerAccessLeaseRecords();
                  const recoveryRegion = lastProvisioningTarget?.region;
                  const recoveryLease = recoveryRegion
                    ? { ...current, region: recoveryRegion }
                    : current;
                  return {
                    current: structuredClone(recoveryLease),
                    accessState: structuredClone(
                      replaceProviderAccessState(accessLeases, recoveryLease),
                    ),
                  };
                });
                if (!recovery) {
                  throw error;
                }
                await provider.reconcileLeaseAccess?.(
                  recovery.current,
                  providerAccessContext([], recovery.accessState),
                );
                return await provision();
              });
            })
          : this.withAWSIngressOperationLock(provision);
    const { server, serverType, market, attempts } = await provisioned.catch(
      async (error: unknown) => {
        const cleanupClaim = validatedProviderProvisioningCleanupClaim(error, config.provider);
        await this.state.runExclusive(async () => {
          if (prepared?.provisioning?.publishAccessBeforeProvisioning) {
            await this.deleteProviderAccess(record.id);
          }
          const current = await this.getLease(record.id);
          if (!current || current.state !== "provisioning") {
            if (cleanupClaim) {
              const failedAt = new Date().toISOString();
              record = structuredClone(current ?? record);
              if (record.state !== "released" && record.state !== "expired") {
                record.state = "failed";
                record.endedAt = failedAt;
              }
              retainProvisioningCleanupClaim(
                record,
                cleanupClaim,
                coordinatorErrorMessage(this.env, error),
                failedAt,
              );
              await this.putLease(record);
            }
            await this.markAWSIngressReconcilePending(record);
            await this.scheduleAlarm();
            return;
          }
          const failedAt = new Date().toISOString();
          record = { ...current };
          record.state = "failed";
          record.updatedAt = failedAt;
          record.endedAt = failedAt;
          record.cleanupFailedAt = failedAt;
          record.cleanupError = coordinatorErrorMessage(this.env, error);
          const awsOutcomeUncertain =
            config.provider === "aws" &&
            config.awsPrivate &&
            isAWSRunInstancesOutcomeUncertain(record.cleanupError);
          record.provisioningResourceMayExist = cleanupClaim
            ? true
            : awsOutcomeUncertain ||
              (config.provider === "hetzner" && hetznerProvisioningFailureMayHaveResource(error));
          record.provisioningFailureRetryable = cleanupClaim
            ? false
            : !awsOutcomeUncertain &&
              config.provider === "hetzner" &&
              hetznerProvisioningFailureRetryable(error);
          if (cleanupClaim) {
            retainProvisioningCleanupClaim(record, cleanupClaim, record.cleanupError, failedAt);
          }
          const failedHetznerServerID =
            config.provider === "hetzner" ? hetznerProvisioningResourceID(error) : undefined;
          if (failedHetznerServerID !== undefined) {
            record.cloudID = String(failedHetznerServerID);
            record.serverID = failedHetznerServerID;
            record.releaseDeletesServer = true;
          }
          if (
            error instanceof HetznerProvisioningError &&
            error.providerKeyCleanupID !== undefined
          ) {
            record.providerKeyCleanupPending = true;
            record.providerKeyCleanupID = String(error.providerKeyCleanupID);
          }
          if (
            cleanupClaim ||
            failedHetznerServerID !== undefined ||
            record.providerKeyCleanupPending
          ) {
            record.cleanupRetryAt = new Date(
              Date.parse(failedAt) + leaseCleanupRetryDelayMs,
            ).toISOString();
          }
          if (record.provisioningResourceMayExist || record.provisioningFailureRetryable) {
            delete record.failureError;
          } else {
            record.failureError = record.cleanupError;
          }
          await this.putLease(record);
          await this.markAWSIngressReconcilePending(record);
          await this.scheduleAlarm();
        });
        throw error;
      },
    );
    let current = await this.getLease(record.id);
    if (!current || current.state !== "provisioning") {
      return this.abortProvisionedLeaseAfterStateChange(
        record,
        config,
        server,
        serverType,
        Boolean(prepared?.provisioning?.publishAccessBeforeProvisioning),
      );
    }
    const finalizationBase = structuredClone(current);
    record = structuredClone(current);
    record.state = "active";
    delete record.provisioningRequestStartedAt;
    record.cloudID = server.cloudID;
    record.serverType = serverType;
    if (server.hostID) {
      record.hostId = server.hostID;
    }
    if (market) {
      record.market = market;
    }
    if (attempts && attempts.length > 0) {
      record.provisioningAttempts = attempts;
    }
    record.serverID = server.id;
    record.serverName = server.name;
    record.host = server.host;
    const finalized = await provider.finalizeLeaseCreate?.(config, record, server, attempts ?? []);
    if (finalized) {
      config = finalized.config;
      record = finalized.lease;
    }
    const finalProviderHourlyUSD = await provider
      .hourlyPriceUSD(serverType, config)
      .catch(() => undefined);
    const finalCost = leaseCost(
      this.env,
      config.provider,
      serverType,
      config.ttlSeconds,
      finalProviderHourlyUSD,
    );
    record.estimatedHourlyUSD = finalCost.hourlyUSD;
    record.maxEstimatedUSD = finalCost.maxUSD;
    const finalization = await this.state.runExclusive(async () => {
      const latest = await this.getLease(record.id);
      if (!latest || latest.state !== "provisioning") {
        return { committed: false as const, current: latest };
      }
      const committedRecord = applyLeaseRecordChanges(latest, finalizationBase, record);
      await this.putLease(committedRecord);
      if (prepared?.provisioning?.publishAccessBeforeProvisioning) {
        await this.deleteProviderAccess(committedRecord.id);
      }
      await this.markAWSIngressReconcilePending(committedRecord);
      await this.scheduleAlarm();
      return { committed: true as const, record: committedRecord };
    });
    if (!finalization.committed) {
      return this.abortProvisionedLeaseAfterStateChange(
        record,
        config,
        server,
        serverType,
        Boolean(prepared?.provisioning?.publishAccessBeforeProvisioning),
      );
    }
    record = finalization.record;
    provider.workspaceCapability?.(record, "observe")?.log("ready", {
      lease_id: record.id,
      cloud_id: record.cloudID,
      region: record.region,
      instance_type: record.serverType,
      ssm_command_id: record.awsSSMCommandID,
    });
    return json({ lease: publicLeaseRecord(record) }, { status: 201 });
  }

  private async createWorkspace(request: Request): Promise<Response> {
    const input = workspaceCreateInput(await readJson<unknown>(request).catch(() => undefined));
    if (!input) {
      return json(
        { error: "invalid_workspace_request", message: "workspace request has invalid fields" },
        { status: 400 },
      );
    }
    const id = validWorkspaceID(input.id) ? input.id : undefined;
    if (!id) {
      return json({ error: "invalid_workspace_id" }, { status: 400 });
    }
    if (input.runtime && input.runtime !== "crabbox") {
      return json(
        { error: "unsupported_runtime", message: "workspace runtime must be crabbox" },
        { status: 400 },
      );
    }
    const idempotencyKey = request.headers.get("idempotency-key");
    if (idempotencyKey && idempotencyKey !== id) {
      return json({ error: "idempotency_key_mismatch" }, { status: 400 });
    }
    const owner = requestOwner(request);
    const org = requestOrg(request, this.env);
    let provider: Provider;
    let workspaceCapability: ProviderWorkspaceCapability | undefined;
    try {
      provider = workspaceProvider(this.env.CRABBOX_WORKSPACE_PROVIDER);
      if (awsPrivateWorkspaceModeEnabled(this.env) && provider !== "aws") {
        throw new Error("private AWS workspace mode requires CRABBOX_WORKSPACE_PROVIDER=aws");
      }
      workspaceCapability = this.provider(provider).workspaceCapability?.();
    } catch (error) {
      return json(
        { error: "workspace_not_configured", message: coordinatorErrorMessage(this.env, error) },
        { status: 424 },
      );
    }
    const profile = workspaceProfile(input.profile);
    const repo = workspaceRepo(input.repo);
    const branch = workspaceBranch(input.branch);
    const command = workspaceCommand(input.command);
    if (!profile) {
      return json({ error: "invalid_profile" }, { status: 400 });
    }
    if (repo === undefined || branch === undefined || command === undefined) {
      return json(
        {
          error: "invalid_workspace_request",
          message: "workspace repo, branch, or command is invalid",
        },
        { status: 400 },
      );
    }
    if (workspaceCapability?.requiresCommand && !input.command?.trim()) {
      return json(
        {
          error: "invalid_workspace_request",
          message: "private AWS workspaces require an explicit command",
        },
        { status: 400 },
      );
    }
    const ttlSeconds = workspaceSeconds(input.ttlSeconds, 14_400);
    const idleTimeoutSeconds = workspaceSeconds(input.idleTimeoutSeconds, 1_800);
    if (ttlSeconds === undefined || idleTimeoutSeconds === undefined) {
      return json(
        { error: "invalid_duration", message: "workspace durations must be whole seconds" },
        { status: 400 },
      );
    }
    if (ttlSeconds < workspaceMinimumTTLSeconds) {
      return json(
        {
          error: "invalid_duration",
          message: `workspace ttlSeconds must be at least ${workspaceMinimumTTLSeconds}`,
        },
        { status: 400 },
      );
    }
    const desktop = input.capabilities?.desktop === true;
    if (workspaceCapability && !workspaceCapability.supportsDesktop && desktop) {
      return json(
        {
          error: "unsupported_workspace_capability",
          message: "private AWS workspaces do not support desktop access",
        },
        { status: 400 },
      );
    }
    const key = workspaceKey(owner, org, id);
    const existingResponse = await this.state.runExclusive(async () => {
      const existing = await this.state.storage.get<WorkspaceRecord>(key);
      if (!existing) {
        return undefined;
      }
      if (existing.prewarm) {
        return notFound();
      }
      await this.state.storage.put(workspaceLeaseReservationKey(existing.leaseID), true);
      const conflict = workspaceConflictResponse(
        existing,
        profile,
        repo,
        branch,
        command,
        desktop,
        ttlSeconds,
        idleTimeoutSeconds,
      );
      if (conflict) return conflict;
      const lease = await this.getLease(existing.leaseID);
      if (workspaceNextReconcileAt(existing, lease) !== undefined) {
        await this.scheduleAlarm();
      }
      return json(workspaceResponse(existing, lease, this.env, workspaceCapability), {
        status: workspaceHTTPStatus(existing, lease),
      });
    });
    if (existingResponse) {
      return existingResponse;
    }
    if (!workspaceCapability && !this.env.CRABBOX_WORKSPACE_SSH_PUBLIC_KEY?.trim()) {
      return json(
        {
          error: "workspace_not_configured",
          message: "CRABBOX_WORKSPACE_SSH_PUBLIC_KEY is required",
        },
        { status: 424 },
      );
    }
    const machineClass = workspaceClass(this.env.CRABBOX_WORKSPACE_CLASS);
    const reservation = await this.state.runExclusive(async () => {
      const current = await this.state.storage.get<WorkspaceRecord>(key);
      if (current) {
        if (current.prewarm) {
          return { response: notFound() };
        }
        await this.state.storage.put(workspaceLeaseReservationKey(current.leaseID), true);
        const conflict = workspaceConflictResponse(
          current,
          profile,
          repo,
          branch,
          command,
          desktop,
          ttlSeconds,
          idleTimeoutSeconds,
        );
        return conflict
          ? { response: conflict }
          : { record: current, lease: await this.getLease(current.leaseID) };
      }
      const adoptionInput = {
        id,
        owner,
        org,
        profile,
        repo,
        branch,
        command,
        provider,
        class: machineClass,
        desktop,
        ttlSeconds,
        idleTimeoutSeconds,
      };
      const prewarmEnabled =
        (!workspaceCapability || workspaceCapability.supportsPrewarm) &&
        workspacePrewarmCount(this.env.CRABBOX_WORKSPACE_PREWARM_COUNT) > 0;
      const now = Date.now();
      let ownerWorkspaceCount = 0;
      let prewarmCandidate:
        | { key: string; workspace: WorkspaceRecord; lease: LeaseRecord }
        | undefined;
      await this.visitStorageRecords<WorkspaceRecord>(
        "workspace:",
        async (workspace, workspaceStorageKey) => {
          if (!workspace.prewarm && workspace.owner === owner && workspace.org === org) {
            ownerWorkspaceCount += 1;
          }
          if (
            !prewarmEnabled ||
            !workspace.prewarm ||
            workspace.org !== org ||
            !workspacePrewarmMatches(workspace, adoptionInput) ||
            !workspace.sshHostKeySha256
          ) {
            return;
          }
          const lease = await this.getLease(workspace.leaseID, { noCache: true });
          if (
            !lease ||
            workspaceStatus(workspace, lease) !== "ready" ||
            Date.parse(lease.expiresAt) <= now + workspacePrewarmReplacementLeadMs ||
            (prewarmCandidate !== undefined &&
              Date.parse(prewarmCandidate.workspace.createdAt) <= Date.parse(workspace.createdAt))
          ) {
            return;
          }
          prewarmCandidate = { key: workspaceStorageKey, workspace, lease };
        },
      );
      if (ownerWorkspaceCount >= workspaceMaxRecordsPerOwner) {
        return {
          response: json(
            {
              error: "workspace_limit_exceeded",
              message: `workspace record limit exceeded: ${ownerWorkspaceCount}/${workspaceMaxRecordsPerOwner}`,
            },
            { status: 429 },
          ),
        };
      }
      const prewarmed = await this.adoptPrewarmedWorkspace({
        ...adoptionInput,
        ...(prewarmCandidate ? { candidate: prewarmCandidate } : {}),
      });
      if (prewarmed) {
        return prewarmed;
      }
      const leaseID = await this.allocateWorkspaceLeaseID();
      const nowISO = new Date().toISOString();
      const record: WorkspaceRecord = {
        id,
        leaseID,
        owner,
        org,
        profile,
        repo,
        branch,
        command,
        provider,
        class: machineClass,
        desktop,
        desktopCapabilityVersion: 1,
        ttlSeconds,
        idleTimeoutSeconds,
        createdAt: nowISO,
        updatedAt: nowISO,
      };
      await this.state.storage.put(key, record);
      await this.state.storage.put(workspaceLeaseReservationKey(record.leaseID), true);
      return { record, lease: undefined };
    });
    if ("response" in reservation) {
      return reservation.response;
    }
    workspaceCapability?.log("create_accepted", {
      lease_id: reservation.record.leaseID,
    });
    try {
      await this.maintainWorkspacePrewarm();
    } catch (error) {
      console.warn(
        `workspace prewarm maintenance deferred: ${coordinatorErrorMessage(this.env, error)}`,
      );
    }
    await this.state.runExclusive(() => this.scheduleAlarm());
    return json(
      workspaceResponse(reservation.record, reservation.lease, this.env, workspaceCapability),
      {
        status: workspaceHTTPStatus(reservation.record, reservation.lease),
      },
    );
  }

  private async adoptPrewarmedWorkspace(input: {
    id: string;
    owner: string;
    org: string;
    profile: string;
    repo: string;
    branch: string;
    command: string;
    provider: Provider;
    class: string;
    desktop: boolean;
    ttlSeconds: number;
    idleTimeoutSeconds: number;
    candidate?: { key: string; workspace: WorkspaceRecord; lease: LeaseRecord };
  }): Promise<{ record: WorkspaceRecord; lease: LeaseRecord } | undefined> {
    if (!input.candidate) {
      return undefined;
    }
    const now = new Date();
    const { key: candidateKey, workspace: prewarm } = input.candidate;
    const lease = structuredClone(input.candidate.lease);
    if (!workspaceOwnsLease(prewarm, lease)) {
      return undefined;
    }
    const expiresAt = new Date(now.getTime() + input.ttlSeconds * 1000);
    const createdAt = Date.parse(lease.createdAt);
    const adoptedTTLSeconds = Math.max(1, Math.ceil((expiresAt.getTime() - createdAt) / 1000));
    lease.workspaceID = input.id;
    lease.providerOwner ??= lease.owner;
    lease.owner = input.owner;
    lease.org = input.org;
    lease.profile = input.profile;
    lease.ttlSeconds = adoptedTTLSeconds;
    lease.idleTimeoutSeconds = adoptedTTLSeconds;
    lease.lastTouchedAt = now.toISOString();
    lease.updatedAt = now.toISOString();
    lease.expiresAt = expiresAt.toISOString();
    delete lease.share;
    const cost = leaseCost(
      this.env,
      input.provider,
      lease.serverType,
      adoptedTTLSeconds,
      lease.estimatedHourlyUSD,
    );
    lease.estimatedHourlyUSD = cost.hourlyUSD;
    lease.maxEstimatedUSD = cost.maxUSD;
    const admission = await this.leaseAdmissionState(
      { owner: input.owner, org: input.org },
      now,
      lease.id,
    );
    if (enforceCostLimitUsage(admission.costUsage, lease, costLimits(this.env))) {
      return undefined;
    }
    const nowISO = now.toISOString();
    const record: WorkspaceRecord = {
      id: input.id,
      leaseID: lease.id,
      owner: input.owner,
      org: input.org,
      profile: input.profile,
      repo: input.repo,
      branch: input.branch,
      command: input.command,
      provider: input.provider,
      class: input.class,
      desktop: input.desktop,
      desktopCapabilityVersion: 1,
      ttlSeconds: input.ttlSeconds,
      idleTimeoutSeconds: input.idleTimeoutSeconds,
      createdAt: nowISO,
      updatedAt: nowISO,
      sshHostKeySha256: prewarm.sshHostKeySha256!,
    };
    await this.putLease(lease);
    await this.state.storage.put(workspaceKey(record.owner, record.org, record.id), record);
    await this.state.storage.delete(candidateKey);
    return { record, lease };
  }

  private async maintainWorkspacePrewarm(): Promise<void> {
    const configuredWorkspaceProvider = workspaceProvider(this.env.CRABBOX_WORKSPACE_PROVIDER);
    const privateAWSMode = awsPrivateWorkspaceModeEnabled(this.env);
    const workspaceProviderCapability = privateAWSMode
      ? undefined
      : this.provider(configuredWorkspaceProvider).workspaceCapability?.();
    const count =
      privateAWSMode ||
      (workspaceProviderCapability && !workspaceProviderCapability.supportsPrewarm)
        ? 0
        : workspacePrewarmCount(this.env.CRABBOX_WORKSPACE_PREWARM_COUNT);
    await this.state.runExclusive(async () => {
      const now = Date.now();
      const templates = new Map<string, WorkspaceRecord>();
      if (count > 0) {
        await this.visitStorageRecords<WorkspaceRecord>("workspace:", async (workspace) => {
          if (workspace.prewarm || !isCurrentOrgKey(workspace.org)) {
            return;
          }
          const lease = await this.getLease(workspace.leaseID, { noCache: true });
          const status = workspaceStatus(workspace, lease);
          if (status !== "provisioning" && status !== "ready") {
            return;
          }
          const shape = workspacePrewarmShape(workspace);
          const current = templates.get(shape);
          if (!current || Date.parse(workspace.createdAt) > Date.parse(current.createdAt)) {
            templates.set(shape, workspace);
          }
        });
      }
      let changed = false;
      const usableByShape = new Map<string, number>();
      const recentFailureShapes = new Set<string>();
      await this.visitStorageRecords<WorkspaceRecord>("workspace:", async (workspace, key) => {
        if (!workspace.prewarm || !isCurrentOrgKey(workspace.org)) {
          return;
        }
        const lease = await this.getLease(workspace.leaseID, { noCache: true });
        const shape = workspacePrewarmShape(workspace);
        const template = templates.get(shape);
        const status = workspaceStatus(workspace, lease);
        const expiresSoon =
          status === "ready" &&
          Date.parse(lease?.expiresAt ?? "") <= now + workspacePrewarmReplacementLeadMs;
        const usable =
          count > 0 &&
          template &&
          workspacePrewarmMatches(workspace, template) &&
          (status === "provisioning" || status === "ready") &&
          !expiresSoon;
        if (usable) {
          usableByShape.set(shape, (usableByShape.get(shape) ?? 0) + 1);
          return;
        }
        const failedMatchingTemplate = Boolean(
          (status === "failed" || workspace.error || lease?.state === "failed") &&
          template &&
          workspacePrewarmMatches(workspace, template),
        );
        if (
          failedMatchingTemplate &&
          (!workspace.releaseRequestedAt ||
            Date.parse(workspace.updatedAt) > now - workspacePrewarmRetryDelayMs)
        ) {
          recentFailureShapes.add(shape);
        }
        if (!workspace.releaseRequestedAt) {
          workspace.releaseRequestedAt = new Date(now).toISOString();
          workspace.updatedAt = workspace.releaseRequestedAt;
          delete workspace.reconcileAfter;
          await this.state.storage.put(key, workspace, { noCache: true });
          changed = true;
        }
      });
      if (count > 0) {
        for (const [shape, template] of templates) {
          if (recentFailureShapes.has(shape)) {
            continue;
          }
          for (let index = usableByShape.get(shape) ?? 0; index < count; index += 1) {
            const id = newWorkspacePrewarmID();
            // oxlint-disable-next-line eslint/no-await-in-loop -- each ID must be reserved before allocating the next spare.
            const leaseID = await this.allocateWorkspaceLeaseID();
            const nowISO = new Date(now).toISOString();
            const record: WorkspaceRecord = {
              id,
              leaseID,
              owner: workspacePrewarmOwner,
              org: template.org,
              profile: template.profile,
              repo: "",
              branch: "main",
              command: "exec bash -l",
              provider: template.provider,
              class: template.class,
              desktop: template.desktop,
              desktopCapabilityVersion: 1,
              ttlSeconds: template.ttlSeconds,
              idleTimeoutSeconds: template.idleTimeoutSeconds,
              createdAt: nowISO,
              updatedAt: nowISO,
              prewarm: true,
            };
            // oxlint-disable-next-line eslint/no-await-in-loop -- preserve serialized ID allocation and reservation.
            await Promise.all([
              this.state.storage.put(workspaceKey(record.owner, record.org, record.id), record),
              this.state.storage.put(workspaceLeaseReservationKey(record.leaseID), true),
            ]);
            changed = true;
          }
        }
      }
      if (changed) {
        await this.scheduleAlarm();
      }
    });
  }

  private async quarantineLegacyWorkspaces(): Promise<void> {
    await this.state.runExclusive(async () => {
      const now = new Date().toISOString();
      await this.visitStorageRecords<WorkspaceRecord>("workspace:", async (workspace, key) => {
        if (!isLegacyOrgKey(workspace.org) || workspace.releaseRequestedAt) {
          return;
        }
        const quarantined = { ...workspace, releaseRequestedAt: now, updatedAt: now };
        delete quarantined.reconcileAfter;
        await this.state.storage.put(key, quarantined, { noCache: true });
      });
    });
  }

  private async allocateWorkspaceLeaseID(): Promise<string> {
    const leaseID = newLeaseID();
    if (
      (await this.getLease(leaseID)) ||
      (await this.state.storage.get(workspaceLeaseReservationKey(leaseID)))
    ) {
      return await this.allocateWorkspaceLeaseID();
    }
    return leaseID;
  }

  private async provisionPendingWorkspace(): Promise<void> {
    const now = Date.now();
    const candidates: Array<{ workspace: WorkspaceRecord; lease: LeaseRecord | undefined }> = [];
    await this.visitStorageRecords<WorkspaceRecord>("workspace:", async (workspace) => {
      if (candidates.length >= 3) {
        return false;
      }
      const lease = await this.getLease(workspace.leaseID, { noCache: true });
      const dueAt = workspaceNextReconcileAt(workspace, lease, now);
      if (dueAt !== undefined && dueAt <= now) {
        candidates.push({ workspace, lease });
        if (candidates.length >= 3) {
          return false;
        }
      }
      return true;
    });
    await Promise.all(
      candidates.map(({ workspace, lease }) => this.reconcileWorkspace(workspace, lease)),
    );
  }

  private async pruneTerminalWorkspaces(): Promise<void> {
    const cutoff = Date.now() - workspaceTerminalRetentionMs;
    await this.state.runExclusive(async () => {
      await this.visitStorageRecords<WorkspaceRecord>("workspace:", async (workspace, key) => {
        const lease = await this.getLease(workspace.leaseID, { noCache: true });
        const terminalAt = workspaceTerminalTimestamp(workspace, lease);
        if (terminalAt === undefined || terminalAt > cutoff) {
          return;
        }
        if (lease && workspaceLeaseMatchesCleanup(workspace, lease)) {
          const detachedLease = structuredClone(lease);
          delete detachedLease.workspaceID;
          await this.putLease(detachedLease, { noCache: true });
        }
        this.closeWorkspaceTerminals(key, 1008, "workspace archived");
        const cleanup = [
          this.state.storage.delete(key),
          this.state.storage.delete(workspaceLeaseReservationKey(workspace.leaseID)),
        ];
        await Promise.all(cleanup);
      });
    });
  }

  private async reconcileWorkspace(
    workspace: WorkspaceRecord,
    initialLease?: LeaseRecord,
  ): Promise<void> {
    let lease = initialLease;
    if (lease && !workspaceLeaseMatchesCleanup(workspace, lease)) {
      await this.recordWorkspaceError(
        workspace,
        "workspace lease reservation conflicts with another lifecycle",
      );
      return;
    }
    if (lease?.providerKeyCleanupPending) {
      return;
    }
    if (!lease && workspaceProvisionDeadline(workspace) <= Date.now()) {
      if (!workspace.releaseRequestedAt) {
        await this.recordWorkspaceError(workspace, "workspace provisioning deadline expired");
      }
      return;
    }
    const claimExpiresAt = Date.parse(workspace.provisionClaimExpiresAt ?? "");
    if (
      lease?.state === "provisioning" &&
      !lease.cloudID &&
      !lease.provisioningRequestStartedAt &&
      (!Number.isFinite(claimExpiresAt) || claimExpiresAt <= Date.now())
    ) {
      if (workspace.releaseRequestedAt) {
        await this.finalizeAbsentWorkspaceLease(workspace, lease);
      } else {
        await this.retryWorkspaceProvisioning(workspace, lease);
      }
      return;
    }
    if (lease?.state === "failed" && lease.provisioningResourceMayExist === false) {
      if (workspace.releaseRequestedAt) {
        await this.finalizeAbsentWorkspaceLease(workspace, lease);
      } else if (lease.provisioningFailureRetryable) {
        await this.retryWorkspaceProvisioning(workspace, lease);
      } else {
        await this.recordWorkspaceError(
          workspace,
          lease.failureError || lease.cleanupError || "workspace provisioning failed",
        );
      }
      return;
    }
    if (
      lease?.state === "provisioning" ||
      lease?.state === "failed" ||
      (lease?.state === "released" && lease.releaseDeletesServer === true && !lease.cloudID)
    ) {
      lease = await this.recoverWorkspaceProvisioning(workspace, lease);
    }
    if (workspace.releaseRequestedAt) {
      if (
        !lease ||
        lease.state === "expired" ||
        (lease.state === "released" &&
          !lease.cleanupStartedAt &&
          !lease.cleanupError &&
          lease.releaseDeletesServer === undefined)
      ) {
        return;
      }
      if (lease.state === "provisioning" && !lease.cloudID) {
        return;
      }
      try {
        await this.releaseWorkspaceLease(workspace);
      } catch {
        // Lease cleanup state carries the provider error and remains retryable.
      }
      return;
    }
    if (lease?.state === "failed") {
      const currentWorkspace = await this.state.storage.get<WorkspaceRecord>(
        workspaceKey(workspace.owner, workspace.org, workspace.id),
      );
      if (!lease.failureError && currentWorkspace?.reconcileAfter && !currentWorkspace.error) {
        return;
      }
      await this.recordWorkspaceError(
        workspace,
        lease.failureError || lease.cleanupError || "workspace provisioning failed",
      );
      return;
    }
    if (lease) {
      return;
    }
    let workspaceCapability: ProviderWorkspaceCapability | undefined;
    try {
      if (awsPrivateWorkspaceModeEnabled(this.env) && workspace.provider !== "aws") {
        throw new Error("private AWS workspace mode requires provider=aws");
      }
      workspaceCapability = this.provider(workspace.provider).workspaceCapability?.();
    } catch (error) {
      await this.recordWorkspaceError(workspace, coordinatorErrorMessage(this.env, error));
      return;
    }
    const sshPublicKey = workspaceCapability
      ? undefined
      : this.env.CRABBOX_WORKSPACE_SSH_PUBLIC_KEY?.trim();
    if (!workspaceCapability && !sshPublicKey) {
      await this.recordWorkspaceError(workspace, "CRABBOX_WORKSPACE_SSH_PUBLIC_KEY is required");
      return;
    }
    let createResponse: Response;
    const provisionClaim = crypto.randomUUID();
    const sshHostKeys = workspaceCapability
      ? undefined
      : generateSSHHostKeyPair(`crabbox-${workspace.id}`);
    const sshHostKeySha256 = sshHostKeys
      ? await workspaceSSHHostKeyFingerprint(sshHostKeys.public)
      : undefined;
    try {
      createResponse = await this.createLease(
        await workspaceLeaseRequest(
          workspace,
          workspaceCapability,
          sshPublicKey && sshHostKeys
            ? { publicKey: sshPublicKey, hostKeys: sshHostKeys }
            : undefined,
        ),
        async () => {
          const current = await this.state.storage.get<WorkspaceRecord>(
            workspaceKey(workspace.owner, workspace.org, workspace.id),
          );
          if (
            !current ||
            current.leaseID !== workspace.leaseID ||
            current.releaseRequestedAt ||
            current.error
          ) {
            return json(
              {
                error: "workspace_state_changed",
                message: "workspace changed before lease reservation",
              },
              { status: 409 },
            );
          }
          const activeClaimExpiresAt = Date.parse(current.provisionClaimExpiresAt ?? "");
          if (
            current.provisionClaim &&
            Number.isFinite(activeClaimExpiresAt) &&
            activeClaimExpiresAt > Date.now()
          ) {
            return json(
              {
                error: "workspace_provisioning_claimed",
                message: "workspace provisioning is already in progress",
              },
              { status: 409 },
            );
          }
          const now = new Date();
          const hardDeadline = workspaceProvisionDeadline(current);
          current.provisionClaim = provisionClaim;
          current.provisionClaimExpiresAt = new Date(
            Math.min(now.getTime() + workspaceProvisionClaimMs, hardDeadline),
          ).toISOString();
          if (sshHostKeySha256) {
            current.sshHostKeySha256 = sshHostKeySha256;
          } else {
            delete current.sshHostKeySha256;
          }
          current.updatedAt = now.toISOString();
          delete current.reconcileAfter;
          await this.state.storage.put(
            workspaceKey(workspace.owner, workspace.org, workspace.id),
            current,
          );
          return undefined;
        },
        workspace.id,
        workspaceCapability,
      );
    } catch (error) {
      const persistedLease = await this.getLease(workspace.leaseID);
      if (persistedLease && workspaceOwnsLease(workspace, persistedLease)) {
        await this.deferWorkspaceReconciliation(workspace, true);
      } else {
        await this.recordWorkspaceError(workspace, coordinatorErrorMessage(this.env, error));
      }
      return;
    }
    if (!createResponse.ok) {
      const current = await this.state.storage.get<WorkspaceRecord>(
        workspaceKey(workspace.owner, workspace.org, workspace.id),
      );
      if (current?.releaseRequestedAt) {
        await this.completeWorkspaceCreate(current, provisionClaim);
        return;
      }
      const persistedLease = await this.getLease(workspace.leaseID);
      if (!persistedLease || !workspaceOwnsLease(workspace, persistedLease)) {
        if (await workspaceAdmissionRetryable(createResponse)) {
          await this.deferWorkspaceReconciliation(workspace, true);
          return;
        }
        await this.recordWorkspaceError(
          workspace,
          await workspaceResponseError(
            createResponse,
            `lease create HTTP ${createResponse.status}`,
          ),
        );
        return;
      }
    }
    await this.completeWorkspaceCreate(workspace, provisionClaim);
    const current = await this.state.storage.get<WorkspaceRecord>(
      workspaceKey(workspace.owner, workspace.org, workspace.id),
    );
    if (current?.leaseID === workspace.leaseID && current.releaseRequestedAt) {
      try {
        await this.releaseWorkspaceLease(current);
      } catch {
        // Lease cleanup state carries the provider error and remains retryable.
      }
    }
  }

  private async recoverWorkspaceProvisioning(
    workspace: WorkspaceRecord,
    lease: LeaseRecord,
  ): Promise<LeaseRecord> {
    const providerResourceDefinitelyAbsent =
      lease.state === "failed" && lease.provisioningResourceMayExist === false;
    const claimExpiresAt = Date.parse(workspace.provisionClaimExpiresAt ?? "");
    const now = Date.now();
    const recoveryDeadline = workspaceProvisionRecoveryDeadline(workspace, lease);
    if (Number.isFinite(claimExpiresAt) && claimExpiresAt > now) {
      return lease;
    }
    const provider = this.provider(workspace.provider, lease.region, lease.providerProject);
    let server: ProviderMachine | undefined;
    try {
      if (provider.recoverServer) {
        server = await provider.recoverServer(lease);
      } else if (lease.cloudID && provider.getServer) {
        server = await provider.getServer(lease.cloudID);
      } else if (provider.findServerByLease) {
        server = await provider.findServerByLease(lease.id);
      } else {
        server = (await provider.listCrabboxServers()).find(
          (machine) => machine.labels?.["lease"] === lease.id,
        );
      }
    } catch (error) {
      if (!providerResourceNotFound(error)) {
        await this.recordWorkspaceRecoveryMiss(workspace, Number.POSITIVE_INFINITY);
        return lease;
      }
    }
    if (server) {
      let recoveredServer = server;
      let workspaceCapability: ProviderWorkspaceCapability | undefined;
      try {
        workspaceCapability = provider.workspaceCapability?.(lease);
      } catch (error) {
        await this.recordWorkspaceError(workspace, coordinatorErrorMessage(this.env, error));
        return lease;
      }
      let recoveryConfig = leaseConfig({
        provider: workspace.provider,
        target: lease.target,
        profile: lease.profile,
        class: lease.class,
        serverType: recoveredServer.serverType,
        providerKey: lease.providerKey,
        desktop: lease.desktop ?? false,
        browser: lease.browser ?? false,
        code: lease.code ?? false,
        ttlSeconds: lease.ttlSeconds,
        idleTimeoutSeconds: lease.idleTimeoutSeconds ?? lease.ttlSeconds,
        keep: lease.keep,
        ...(workspaceCapability
          ? workspaceCapability.recoveryLeaseRequestFields(workspace, recoveredServer)
          : {
              sshPublicKey:
                this.env.CRABBOX_WORKSPACE_SSH_PUBLIC_KEY?.trim() || readinessDummySSHPublicKey,
            }),
      });
      const resumeAllowed = await this.state.runExclusive(async () => {
        const currentWorkspace = await this.state.storage.get<WorkspaceRecord>(
          workspaceKey(workspace.owner, workspace.org, workspace.id),
        );
        const current = await this.getLease(lease.id);
        return Boolean(
          currentWorkspace &&
          current &&
          workspaceLeaseMatchesCleanup(currentWorkspace, current) &&
          !workspaceReleaseOrCleanupInProgress(currentWorkspace, current),
        );
      });
      try {
        if (resumeAllowed) {
          recoveredServer =
            (await provider.resumeRecoveredServer?.(recoveryConfig, lease, recoveredServer)) ??
            recoveredServer;
        }
      } catch (error) {
        const cleanupClaim = validatedProviderProvisioningCleanupClaim(error, workspace.provider);
        const failure = coordinatorErrorMessage(this.env, error);
        const failed = await this.state.runExclusive(async () => {
          const currentWorkspace = await this.state.storage.get<WorkspaceRecord>(
            workspaceKey(workspace.owner, workspace.org, workspace.id),
          );
          const current = await this.getLease(lease.id);
          if (
            !currentWorkspace ||
            !current ||
            !workspaceLeaseMatchesCleanup(currentWorkspace, current)
          ) {
            return current ?? lease;
          }
          const failedAt = new Date().toISOString();
          if (workspaceReleaseOrCleanupInProgress(currentWorkspace, current)) {
            if (!current.cleanupStartedAt) {
              applyRecoveredServerIdentity(current, recoveredServer);
              current.updatedAt = failedAt;
              if (cleanupClaim) {
                retainProvisioningCleanupClaim(current, cleanupClaim, failure, failedAt);
                current.cleanupRetryAt = new Date(
                  Date.parse(failedAt) + leaseCleanupRetryDelayMs,
                ).toISOString();
              }
              await this.putLease(current);
              await this.scheduleAlarm();
            }
            return current;
          }
          current.state = "failed";
          applyRecoveredServerIdentity(current, recoveredServer);
          current.updatedAt = failedAt;
          current.endedAt = failedAt;
          current.cleanupFailedAt = failedAt;
          current.cleanupError = failure;
          current.provisioningResourceMayExist = Boolean(cleanupClaim);
          current.provisioningFailureRetryable = false;
          if (cleanupClaim) {
            retainProvisioningCleanupClaim(current, cleanupClaim, failure, failedAt);
            current.cleanupRetryAt = new Date(
              Date.parse(failedAt) + leaseCleanupRetryDelayMs,
            ).toISOString();
            delete current.failureError;
          } else {
            current.failureError = failure;
            delete current.cleanupRetryAt;
          }
          await this.putLease(current);
          await this.scheduleAlarm();
          return current;
        });
        const currentWorkspace = await this.state.storage.get<WorkspaceRecord>(
          workspaceKey(workspace.owner, workspace.org, workspace.id),
        );
        if (
          currentWorkspace &&
          workspaceLeaseMatchesCleanup(currentWorkspace, failed) &&
          workspaceReleaseOrCleanupInProgress(currentWorkspace, failed)
        ) {
          if (currentWorkspace.releaseRequestedAt) {
            try {
              await this.releaseWorkspaceLease(currentWorkspace);
            } catch {
              // Durable cleanup state carries the provider error and remains retryable.
            }
          }
          return (await this.getLease(lease.id)) ?? failed;
        }
        if (cleanupClaim) {
          await this.deferWorkspaceReconciliation(workspace, true);
        } else {
          await this.recordWorkspaceError(workspace, failure);
        }
        return failed;
      }
      const providerHourlyUSD = await provider
        .hourlyPriceUSD(recoveredServer.serverType, recoveryConfig)
        .catch(() => undefined);
      const recoveredCost = leaseCost(
        this.env,
        workspace.provider,
        recoveredServer.serverType,
        lease.ttlSeconds,
        providerHourlyUSD,
      );
      const recovered = await this.state.runExclusive(async () => {
        const currentWorkspace = await this.state.storage.get<WorkspaceRecord>(
          workspaceKey(workspace.owner, workspace.org, workspace.id),
        );
        const current = await this.getLease(lease.id);
        if (
          !currentWorkspace ||
          !current ||
          !workspaceLeaseMatchesCleanup(currentWorkspace, current)
        ) {
          return current ?? lease;
        }
        const recoveredAt = new Date().toISOString();
        if (workspaceReleaseOrCleanupInProgress(currentWorkspace, current)) {
          if (!current.cleanupStartedAt) {
            applyRecoveredServerIdentity(current, recoveredServer);
            current.updatedAt = recoveredAt;
            await this.putLease(current);
            await this.scheduleAlarm();
          }
          return current;
        }
        if (current.state !== "provisioning" && current.state !== "failed") {
          return current;
        }
        applyRecoveredServerIdentity(current, recoveredServer);
        current.estimatedHourlyUSD = recoveredCost.hourlyUSD;
        current.maxEstimatedUSD = recoveredCost.maxUSD;
        if (
          workspaceCapability
            ? workspaceCapability.recoveredReady(recoveredServer)
            : recoveredServer.status === "running" && recoveredServer.host.trim()
        ) {
          current.state = "active";
          current.host = workspaceCapability
            ? workspaceCapability.recoveredHost(recoveredServer)
            : recoveredServer.host;
        } else {
          current.state = "provisioning";
        }
        if (recoveredServer.region) {
          current.region = recoveredServer.region;
        }
        if (recoveredServer.hostID) {
          current.hostId = recoveredServer.hostID;
        }
        workspaceCapability?.applyRecoveredEvidence(current, recoveryConfig, recoveredServer);
        current.updatedAt = recoveredAt;
        current.lastTouchedAt = recoveredAt;
        delete current.failureError;
        delete current.provisioningResourceMayExist;
        delete current.provisioningFailureRetryable;
        delete current.provisioningRequestStartedAt;
        delete current.endedAt;
        delete current.releasedAt;
        clearLeaseCleanupMetadata(current);
        await this.putLease(current);
        await this.scheduleAlarm();
        return current;
      });
      const currentWorkspace = await this.state.storage.get<WorkspaceRecord>(
        workspaceKey(workspace.owner, workspace.org, workspace.id),
      );
      if (
        currentWorkspace &&
        workspaceLeaseMatchesCleanup(currentWorkspace, recovered) &&
        workspaceReleaseOrCleanupInProgress(currentWorkspace, recovered)
      ) {
        if (currentWorkspace.releaseRequestedAt) {
          try {
            await this.releaseWorkspaceLease(currentWorkspace);
          } catch {
            // Durable cleanup state carries the provider error and remains retryable.
          }
        }
        return (await this.getLease(lease.id)) ?? recovered;
      }
      if (
        currentWorkspace &&
        workspaceOwnsLease(currentWorkspace, recovered) &&
        recovered.state === "active"
      ) {
        await this.completeWorkspaceCreate(currentWorkspace, currentWorkspace.provisionClaim);
        workspaceCapability?.log("recovered_ready", {
          lease_id: recovered.id,
          cloud_id: recovered.cloudID,
          region: recovered.region,
          instance_type: recovered.serverType,
          ssm_command_id: recovered.awsSSMCommandID,
        });
      } else if (currentWorkspace && workspaceOwnsLease(currentWorkspace, recovered)) {
        await this.deferWorkspaceReconciliation(currentWorkspace);
      }
      return recovered;
    }
    if (!providerResourceDefinitelyAbsent && recoveryDeadline > now) {
      await this.recordWorkspaceRecoveryMiss(workspace, recoveryDeadline);
      return lease;
    }
    const retryableLease = await this.state.runExclusive(async () => {
      const currentWorkspace = await this.state.storage.get<WorkspaceRecord>(
        workspaceKey(workspace.owner, workspace.org, workspace.id),
      );
      const current = await this.getLease(lease.id);
      if (
        !currentWorkspace ||
        currentWorkspace.leaseID !== workspace.leaseID ||
        currentWorkspace.releaseRequestedAt ||
        !current ||
        !workspaceOwnsLease(currentWorkspace, current) ||
        !(
          current.state === "provisioning" ||
          (current.state === "failed" && current.provisioningFailureRetryable)
        )
      ) {
        return undefined;
      }
      current.state = "failed";
      current.provisioningResourceMayExist = false;
      current.provisioningFailureRetryable = true;
      delete current.provisioningRequestStartedAt;
      current.updatedAt = new Date().toISOString();
      await this.putLease(current);
      return current;
    });
    if (retryableLease) {
      await this.retryWorkspaceProvisioning(workspace, retryableLease);
      return retryableLease;
    }
    return await this.finalizeAbsentWorkspaceLease(workspace, lease);
  }

  private async workspaceRoute(
    request: Request,
    workspaceID: string,
    action?: string,
    connection?: string,
  ): Promise<Response> {
    if (!validWorkspaceID(workspaceID)) {
      return notFound();
    }
    const owner = requestOwner(request);
    const org = requestOrg(request, this.env);
    const key = workspaceKey(owner, org, workspaceID);
    const method = request.method.toUpperCase();
    if (method === "GET" && action === undefined) {
      return await this.state.runExclusive(async () => {
        const record = await this.state.storage.get<WorkspaceRecord>(key);
        if (!record || record.prewarm) {
          return notFound();
        }
        const lease = await this.getLease(record.leaseID);
        const workspaceCapability = this.provider(
          record.provider,
          lease?.region,
          lease?.providerProject,
        ).workspaceCapability?.(lease, "observe");
        return json(workspaceResponse(record, lease, this.env, workspaceCapability));
      });
    }
    if (method === "POST" && action === "connections" && connection === "desktop") {
      const record = await this.state.storage.get<WorkspaceRecord>(key);
      if (!record || record.prewarm) {
        return notFound();
      }
      return json(
        {
          error: "desktop_unavailable",
          message: "workspace desktop handoff is not configured",
        },
        { status: 409 },
      );
    }
    if (method === "POST" && action === "connections" && connection === "native-vnc") {
      return await this.createWorkspaceNativeVNCTicket(request, key, workspaceID);
    }
    if (method === "DELETE" && action === undefined) {
      const release = await this.state.runExclusive(async () => {
        const record = await this.state.storage.get<WorkspaceRecord>(key);
        if (!record || record.prewarm) {
          return { response: notFound() };
        }
        const lease = await this.getLease(record.leaseID);
        if (!record.releaseRequestedAt) {
          record.releaseRequestedAt = new Date().toISOString();
          record.updatedAt = record.releaseRequestedAt;
          delete record.reconcileAfter;
          await this.state.storage.put(key, record);
        }
        await this.scheduleAlarm();
        return {
          record,
          lease,
          release:
            Boolean(lease) &&
            !(
              !lease?.cloudID &&
              (lease?.state === "provisioning" ||
                lease?.state === "failed" ||
                (lease?.state === "released" && lease.releaseDeletesServer === true))
            ) &&
            lease?.state !== "expired" &&
            (lease?.state !== "released" || lease.releaseDeletesServer !== undefined),
        };
      });
      if ("response" in release) {
        return release.response;
      }
      this.closeWorkspaceTerminals(key, 1008, "workspace stopping");
      this.provider(release.record.provider, release.lease?.region, release.lease?.providerProject)
        .workspaceCapability?.(release.lease, "observe")
        ?.log("delete_requested", {
          lease_id: release.record.leaseID,
          cloud_id: release.lease?.cloudID,
          region: release.lease?.region,
        });
      if (release.release) {
        try {
          await this.releaseWorkspaceLease(release.record);
        } catch {
          // Lease cleanup state carries the provider error and remains retryable.
        }
      }
      const record = (await this.state.storage.get<WorkspaceRecord>(key)) ?? release.record;
      const lease = await this.getLease(record.leaseID);
      const workspaceCapability = this.provider(
        record.provider,
        lease?.region,
        lease?.providerProject,
      ).workspaceCapability?.(lease, "observe");
      return json(workspaceResponse(record, lease, this.env, workspaceCapability));
    }
    return json({ error: "not_found" }, { status: 404 });
  }

  private async createWorkspaceNativeVNCTicket(
    request: Request,
    key: string,
    workspaceID: string,
  ): Promise<Response> {
    const brokerURL = workspacePublicURL(this.env);
    if (!brokerURL) {
      return json(
        { error: "native_vnc_unavailable", message: "workspace public URL is not configured" },
        { status: 409 },
      );
    }
    const result = await this.state.runExclusive(async () => {
      const workspace = await this.state.storage.get<WorkspaceRecord>(key);
      if (!workspace || workspace.prewarm || workspace.id !== workspaceID) return undefined;
      const lease = await this.getLease(workspace.leaseID);
      const unavailable = workspaceNativeVNCError(workspace, lease, this.env);
      if (unavailable || !lease) {
        return { unavailable: unavailable ?? "workspace lease is unavailable" };
      }
      const now = new Date();
      const ticket: NativeVNCTicketRecord = {
        ticket: newNativeVNCTicket(),
        workspaceID: workspace.id,
        leaseID: lease.id,
        owner: workspace.owner,
        org: workspace.org,
        createdAt: now.toISOString(),
        expiresAt: new Date(now.getTime() + nativeVNCTicketTTLSeconds * 1000).toISOString(),
      };
      await this.cleanupExpiredNativeVNCTickets(now.getTime());
      await this.state.storage.put(nativeVNCTicketKey(ticket.ticket), ticket);
      return { ticket };
    });
    if (!result) return notFound();
    if ("unavailable" in result) {
      return json(
        { error: "native_vnc_unavailable", message: result.unavailable },
        { status: 409 },
      );
    }
    return json(
      {
        schema: "crabbox/native-vnc-grant/v1",
        brokerUrl: brokerURL,
        leaseId: result.ticket.leaseID,
        ticket: result.ticket.ticket,
        expiresAt: result.ticket.expiresAt,
      },
      { headers: { "cache-control": "no-store" } },
    );
  }

  private async workspaceNativeVNC(request: Request): Promise<Response> {
    if (request.headers.get("upgrade")?.toLowerCase() !== "websocket") {
      return json({ error: "upgrade_required" }, { status: 426 });
    }
    const token = bearerToken(request);
    if (!validNativeVNCTicket(token)) {
      return json({ error: "native_vnc_ticket_required" }, { status: 401 });
    }
    const lease = await this.state.runExclusive(async () => {
      const key = nativeVNCTicketKey(token);
      const ticket = await this.state.storage.get<NativeVNCTicketRecord>(key);
      await this.state.storage.delete(key);
      if (
        !ticket ||
        ticket.ticket !== token ||
        !isCurrentOrgKey(ticket.org) ||
        Date.parse(ticket.expiresAt) <= Date.now()
      ) {
        return undefined;
      }
      const workspace = await this.state.storage.get<WorkspaceRecord>(
        workspaceKey(ticket.owner, ticket.org, ticket.workspaceID),
      );
      const current = workspace ? await this.getLease(ticket.leaseID) : undefined;
      return workspace &&
        current &&
        isCurrentOrgKey(workspace.org) &&
        isCurrentOrgKey(current.org) &&
        workspaceOwnsLease(workspace, current)
        ? { workspace, lease: current }
        : undefined;
    });
    if (!lease) return json({ error: "native_vnc_ticket_invalid" }, { status: 401 });
    const unavailable = workspaceNativeVNCError(lease.workspace, lease.lease, this.env);
    if (unavailable) {
      return json({ error: "native_vnc_unavailable", message: unavailable }, { status: 409 });
    }
    const key = workspaceKey(lease.workspace.owner, lease.workspace.org, lease.workspace.id);
    const admission = await this.state.runExclusive(async () => {
      const currentWorkspace = await this.state.storage.get<WorkspaceRecord>(key);
      const currentLease = currentWorkspace
        ? await this.getLease(currentWorkspace.leaseID)
        : undefined;
      const currentUnavailable = currentWorkspace
        ? workspaceNativeVNCError(currentWorkspace, currentLease, this.env)
        : "workspace is unavailable";
      if (
        currentUnavailable ||
        !currentWorkspace ||
        !currentLease ||
        currentLease.id !== lease.lease.id
      ) {
        return {
          error: "unavailable" as const,
          message: currentUnavailable ?? "workspace lease changed",
        };
      }
      if (this.workspaceConnectionLimitReached(key, lease.workspace.owner, lease.workspace.org)) {
        return { error: "limit" as const };
      }
      const upgrade = this.state.createWebSocketUpgrade({
        maxPayload: workspaceTerminalMaxBufferedBytes,
      });
      this.trackWorkspaceTerminal(key, upgrade.socket);
      return { upgrade, workspace: currentWorkspace, lease: currentLease };
    });
    if ("error" in admission && admission.error === "unavailable") {
      return json({ error: "native_vnc_unavailable", message: admission.message }, { status: 409 });
    }
    if ("error" in admission) {
      return json(
        {
          error: "native_vnc_connection_limit",
          message: "workspace connection limit reached",
        },
        { status: 429, headers: { "retry-after": "5" } },
      );
    }
    void this.connectWorkspaceNativeVNC(
      admission.upgrade.socket,
      key,
      admission.workspace,
      admission.lease,
    ).catch((error) =>
      closeSocket(
        admission.upgrade.socket,
        1011,
        boundedSocketReason(coordinatorErrorMessage(this.env, error)),
      ),
    );
    return admission.upgrade.response;
  }

  private async connectWorkspaceNativeVNC(
    socket: WebSocket,
    workspaceKeyValue: string,
    workspace: WorkspaceRecord,
    lease: LeaseRecord,
  ): Promise<void> {
    const privateKey = this.env.CRABBOX_WORKSPACE_SSH_PRIVATE_KEY?.trim();
    const expectedHostKey = workspace.sshHostKeySha256;
    if (!privateKey || !expectedHostKey || !lease.host) {
      throw new Error("workspace native VNC SSH access is not configured");
    }

    let client: SSHClient | undefined;
    let connectingClient: SSHClient | undefined;
    let channel: ClientChannel | undefined;
    let closed = false;
    let started = false;
    let resolveStart: (() => void) | undefined;
    let rejectStart: ((error: Error) => void) | undefined;
    let inputQueue = Promise.resolve();
    let queuedInputBytes = 0;
    let queuedInputFrames = 0;
    const start = new Promise<void>((resolve, reject) => {
      resolveStart = resolve;
      rejectStart = reject;
    });
    let startTimer: ReturnType<typeof setTimeout> | undefined;
    let expiryTimer: ReturnType<typeof setTimeout>;
    const clearStartTimer = () => {
      const timer = startTimer;
      startTimer = undefined;
      if (timer !== undefined) clearTimeout(timer as unknown as number);
    };
    const close = (code: number, reason: string) => {
      if (closed) return;
      closed = true;
      clearStartTimer();
      clearTimeout(expiryTimer);
      this.untrackWorkspaceTerminal(workspaceKeyValue, socket);
      rejectStart?.(new Error(reason));
      channel?.close();
      connectingClient?.end();
      client?.end();
      closeSocket(socket, code, boundedSocketReason(reason));
    };
    expiryTimer = setTimeout(
      () => close(1008, "workspace expired"),
      Math.min(Math.max(0, Date.parse(lease.expiresAt) - Date.now()), 2_147_483_647),
    );
    this.state.acceptEphemeralWebSocket(socket, {
      message: (data) => {
        if (closed) return;
        if (!started) {
          if (data === "start") {
            started = true;
            clearStartTimer();
            resolveStart?.();
          } else {
            close(1008, "native VNC start required");
          }
          return;
        }
        const length = workspaceTerminalDataLength(data);
        if (
          queuedInputFrames >= workspaceTerminalMaxBufferedFrames ||
          queuedInputBytes + length > workspaceTerminalMaxBufferedBytes
        ) {
          close(1009, "native VNC input buffer exceeded");
          return;
        }
        queuedInputBytes += length;
        queuedInputFrames += 1;
        inputQueue = inputQueue
          .then(async () => {
            try {
              const value = await workspaceTerminalMessage(data);
              if (typeof value === "string") throw new Error("native VNC binary frame required");
              if (channel) await writeWorkspaceTerminalChannel(channel, value);
              return undefined;
            } finally {
              queuedInputBytes -= length;
              queuedInputFrames -= 1;
            }
          })
          .catch((error) => close(1011, coordinatorErrorMessage(this.env, error)));
      },
      close: () => close(1000, "native VNC closed"),
      error: () => close(1011, "native VNC websocket error"),
    });

    try {
      client = await connectWorkspaceSSH(privateKey, expectedHostKey, lease, {
        shouldStop: () => closed,
        connecting: (candidate) => {
          connectingClient = candidate;
        },
      });
      connectingClient = undefined;
      const password = await readWorkspaceVNCPassword(client, lease);
      const username = lease.target === "windows" || lease.target === "macos" ? lease.sshUser : "";
      socket.send(
        JSON.stringify({
          schema: "crabbox/native-vnc-ready/v1",
          leaseId: lease.id,
          username: username ?? "",
          password,
        }),
      );
      startTimer = setTimeout(
        () => rejectStart?.(new Error("native VNC viewer did not connect")),
        30_000,
      );
      await start;
      if (closed) return;
      const connectedClient = client;
      channel = await new Promise<ClientChannel>((resolve, reject) => {
        connectedClient.forwardOut("127.0.0.1", 0, "127.0.0.1", 5900, (error, opened) =>
          error ? reject(error) : resolve(opened),
        );
      });
      channel.on("data", (data: Uint8Array) => {
        if (closed || socket.readyState !== WebSocket.OPEN) return;
        if (
          data.byteLength > workspaceTerminalMaxBufferedBytes ||
          workspaceTerminalSocketBufferedBytes(socket) + data.byteLength >
            workspaceTerminalMaxBufferedBytes
        ) {
          close(1009, "native VNC output buffer exceeded");
          return;
        }
        socket.send(data.slice());
      });
      channel.once("close", () => close(1000, "native VNC tunnel closed"));
      connectedClient.once("close", () => close(1011, "SSH connection closed"));
      connectedClient.once("error", (error) =>
        close(1011, coordinatorErrorMessage(this.env, error)),
      );
    } catch (error) {
      close(1011, coordinatorErrorMessage(this.env, error));
      throw error;
    }
  }

  private async cleanupExpiredNativeVNCTickets(now = Date.now()): Promise<void> {
    const tickets = await this.state.storage.list<NativeVNCTicketRecord>({
      prefix: nativeVNCTicketPrefix(),
    });
    await Promise.all(
      [...tickets.entries()]
        .filter(([, ticket]) => Date.parse(ticket.expiresAt) <= now)
        .map(([key]) => this.state.storage.delete(key)),
    );
  }

  private async workspaceTerminal(request: Request, workspaceID: string): Promise<Response> {
    if (
      request.headers.get("upgrade")?.toLowerCase() !== "websocket" ||
      !validWorkspaceID(workspaceID)
    ) {
      return json({ error: "upgrade_required" }, { status: 426 });
    }
    if (!workspaceTerminalOriginAllowed(request, this.env)) {
      return json({ error: "origin_forbidden" }, { status: 403 });
    }
    const owner = requestOwner(request);
    const org = requestOrg(request, this.env);
    const key = workspaceKey(owner, org, workspaceID);
    return await this.state.runExclusive(async () => {
      const workspace = await this.state.storage.get<WorkspaceRecord>(key);
      if (
        !workspace ||
        workspace.prewarm ||
        workspace.id !== workspaceID ||
        workspace.owner !== owner ||
        workspace.org !== org
      ) {
        return notFound();
      }
      const lease = await this.getLease(workspace.leaseID);
      const unavailable = workspaceTerminalError(workspace, lease, this.env);
      if (unavailable || !lease) {
        return json(
          {
            error: "terminal_unavailable",
            message: unavailable ?? "workspace lease is unavailable",
          },
          { status: 409 },
        );
      }
      if (this.workspaceConnectionLimitReached(key, owner, org)) {
        return json(
          {
            error: "terminal_connection_limit",
            message: "workspace terminal connection limit reached",
          },
          { status: 429, headers: { "retry-after": "5" } },
        );
      }

      const upgrade = this.state.createWebSocketUpgrade({
        maxPayload: workspaceTerminalMaxBufferedBytes,
      });
      const socket = upgrade.socket;
      this.trackWorkspaceTerminal(key, socket);
      void this.connectWorkspaceTerminal(socket, key, workspace, lease).catch((error) => {
        closeSocket(socket, 1011, boundedSocketReason(coordinatorErrorMessage(this.env, error)));
      });
      return upgrade.response;
    });
  }

  private async connectWorkspaceTerminal(
    socket: WebSocket,
    workspaceKeyValue: string,
    workspace: WorkspaceRecord,
    lease: LeaseRecord,
  ): Promise<void> {
    const privateKey = this.env.CRABBOX_WORKSPACE_SSH_PRIVATE_KEY?.trim();
    const expectedHostKey = workspace.sshHostKeySha256;
    if (!privateKey || !expectedHostKey || !lease.host) {
      throw new Error("workspace terminal SSH access is not configured");
    }

    let client: SSHClient | undefined;
    let connectingClient: SSHClient | undefined;
    let channel: ClientChannel | undefined;
    let cols = 120;
    let rows = 34;
    let pendingBytes = 0;
    let queuedInputBytes = 0;
    let queuedInputFrames = 0;
    let queuedOutputBytes = 0;
    let queuedOutputFrames = 0;
    let unacknowledgedOutputBytes = 0;
    let outputPaused = false;
    let acknowledgeOutput: ((bytes: number) => void) | undefined;
    const pending: Array<string | Uint8Array> = [];
    let inputQueue = Promise.resolve();
    let terminalReady = false;
    let closed = false;
    const expiresIn = Math.max(0, Date.parse(lease.expiresAt) - Date.now());
    const expiryTimer = setTimeout(
      () => close(1008, "workspace expired"),
      Math.min(expiresIn, 2_147_483_647),
    );
    const close = (code: number, reason: string) => {
      if (closed) return;
      closed = true;
      clearTimeout(expiryTimer);
      this.untrackWorkspaceTerminal(workspaceKeyValue, socket);
      channel?.close();
      connectingClient?.end();
      client?.end();
      closeSocket(socket, code, boundedSocketReason(reason));
    };
    const writeInput = async (value: string | Uint8Array) => {
      const length = workspaceTerminalDataLength(value);
      if (channel && terminalReady) {
        await writeWorkspaceTerminalChannel(channel, value);
        return;
      }
      pendingBytes += length;
      if (pendingBytes > workspaceTerminalMaxBufferedBytes) {
        close(1009, "terminal input buffer exceeded");
        return;
      }
      pending.push(value);
    };
    this.state.acceptEphemeralWebSocket(socket, {
      message: (data) => {
        if (closed) return;
        const length = workspaceTerminalDataLength(data);
        if (length === 0) return;
        if (
          pending.length + queuedInputFrames >= workspaceTerminalMaxBufferedFrames ||
          pendingBytes + queuedInputBytes + length > workspaceTerminalMaxBufferedBytes
        ) {
          close(1009, "terminal input buffer exceeded");
          return;
        }
        queuedInputBytes += length;
        queuedInputFrames += 1;
        inputQueue = inputQueue
          .then(async () => {
            try {
              const value = await workspaceTerminalMessage(data);
              if (typeof value === "string") {
                const size = workspaceTerminalResize(value);
                if (size) {
                  cols = size.cols;
                  rows = size.rows;
                  channel?.setWindow(rows, cols, rows * 16, cols * 8);
                  return undefined;
                }
                const acknowledgedBytes = workspaceTerminalAcknowledgement(value);
                if (acknowledgedBytes !== undefined) {
                  acknowledgeOutput?.(acknowledgedBytes);
                  return undefined;
                }
              }
              await writeInput(value);
              return undefined;
            } finally {
              queuedInputBytes -= length;
              queuedInputFrames -= 1;
            }
          })
          .catch((error) => close(1011, coordinatorErrorMessage(this.env, error)));
      },
      close: () => close(1000, "terminal closed"),
      error: () => close(1011, "terminal websocket error"),
    });

    try {
      client = await connectWorkspaceSSH(privateKey, expectedHostKey, lease, {
        shouldStop: () => closed,
        connecting: (candidate) => {
          connectingClient = candidate;
        },
      });
      connectingClient = undefined;
      if (closed) return;
      const connectedClient = client;

      channel = await new Promise<ClientChannel>((resolve, reject) => {
        connectedClient.shell(
          { term: "xterm-256color", cols, rows, width: cols * 8, height: rows * 16 },
          (error, opened) => (error ? reject(error) : resolve(opened)),
        );
      });
      const output: Uint8Array[] = [];
      let outputFlushing = false;
      const updateOutputBackpressure = () => {
        const bufferedBytes = queuedOutputBytes + unacknowledgedOutputBytes;
        if (!outputPaused && bufferedBytes >= workspaceTerminalMaxBufferedBytes / 2) {
          outputPaused = true;
          channel?.pause();
        } else if (outputPaused && bufferedBytes <= workspaceTerminalMaxBufferedBytes / 4) {
          outputPaused = false;
          channel?.resume();
        }
      };
      const flushOutput = async () => {
        if (outputFlushing) return;
        if (unacknowledgedOutputBytes >= workspaceTerminalMaxBufferedBytes) return;
        outputFlushing = true;
        try {
          const data = output.shift();
          if (!closed && socket.readyState === WebSocket.OPEN && data) {
            if (
              workspaceTerminalSocketBufferedBytes(socket) + data.byteLength >
              workspaceTerminalMaxBufferedBytes
            ) {
              close(1009, "terminal output transport buffer exceeded");
              return;
            }
            queuedOutputBytes -= data.byteLength;
            queuedOutputFrames -= 1;
            unacknowledgedOutputBytes += data.byteLength;
            socket.send(data);
            if (workspaceTerminalSocketBufferedBytes(socket) > workspaceTerminalMaxBufferedBytes) {
              close(1009, "terminal output transport buffer exceeded");
              return;
            }
            updateOutputBackpressure();
          }
        } catch (error) {
          close(1011, coordinatorErrorMessage(this.env, error));
        } finally {
          outputFlushing = false;
          if (
            !closed &&
            output.length > 0 &&
            unacknowledgedOutputBytes < workspaceTerminalMaxBufferedBytes
          ) {
            void flushOutput();
          }
        }
      };
      acknowledgeOutput = (bytes) => {
        if (
          bytes > unacknowledgedOutputBytes ||
          workspaceTerminalSocketBufferedBytes(socket) > workspaceTerminalMaxBufferedBytes
        ) {
          close(1008, "invalid terminal output acknowledgement");
          return;
        }
        unacknowledgedOutputBytes -= bytes;
        updateOutputBackpressure();
        void flushOutput();
      };
      const sendOutput = (data: Uint8Array) => {
        if (socket.readyState !== WebSocket.OPEN) return;
        if (
          queuedOutputFrames >= workspaceTerminalMaxBufferedFrames ||
          queuedOutputBytes + unacknowledgedOutputBytes + data.byteLength >
            workspaceTerminalMaxBufferedBytes
        ) {
          close(1009, "terminal output buffer exceeded");
          return;
        }
        queuedOutputBytes += data.byteLength;
        queuedOutputFrames += 1;
        output.push(data.slice());
        updateOutputBackpressure();
        void flushOutput();
      };
      channel.on("data", sendOutput);
      channel.stderr.on("data", sendOutput);
      channel.once("close", () => close(1000, "terminal process exited"));
      connectedClient.once("close", () => close(1011, "SSH connection closed"));
      connectedClient.once("error", (error) =>
        close(1011, coordinatorErrorMessage(this.env, error)),
      );
      await writeWorkspaceTerminalChannel(
        channel,
        `${workspaceTerminalBootstrapCommand(workspace, lease)}\n`,
      );
      const terminalChannel = channel;
      const flushPending = async (): Promise<void> => {
        const value = pending.shift();
        if (value === undefined) return;
        pendingBytes -= workspaceTerminalDataLength(value);
        await writeWorkspaceTerminalChannel(terminalChannel, value);
        await flushPending();
      };
      await flushPending();
      terminalReady = true;
    } catch (error) {
      close(1011, coordinatorErrorMessage(this.env, error));
      throw error;
    }
  }

  private trackWorkspaceTerminal(key: string, socket: WebSocket): void {
    const sockets = this.workspaceTerminals.get(key) ?? new Set<WebSocket>();
    sockets.add(socket);
    this.workspaceTerminals.set(key, sockets);
    socket.addEventListener("close", () => this.untrackWorkspaceTerminal(key, socket));
  }

  private workspaceConnectionLimitReached(key: string, owner: string, org: string): boolean {
    const ownerPrefix = workspaceKey(owner, org, "");
    let ownerConnections = 0;
    let globalConnections = 0;
    for (const [terminalKey, sockets] of this.workspaceTerminals) {
      globalConnections += sockets.size;
      if (terminalKey.startsWith(ownerPrefix)) {
        ownerConnections += sockets.size;
      }
    }
    const transportConnectionLimit = Math.max(
      1,
      Math.floor(
        workspaceTerminalTransportMemoryBudgetBytes / this.state.ephemeralWebSocketMaxPayloadBytes,
      ),
    );
    return (
      (this.workspaceTerminals.get(key)?.size ?? 0) >=
        Math.min(workspaceTerminalMaxPerWorkspace, transportConnectionLimit) ||
      ownerConnections >= Math.min(workspaceTerminalMaxPerOwner, transportConnectionLimit) ||
      globalConnections >= Math.min(workspaceTerminalMaxGlobal, transportConnectionLimit)
    );
  }

  private untrackWorkspaceTerminal(key: string, socket: WebSocket): void {
    const sockets = this.workspaceTerminals.get(key);
    sockets?.delete(socket);
    if (sockets?.size === 0) {
      this.workspaceTerminals.delete(key);
    }
  }

  private closeWorkspaceTerminals(key: string, code: number, reason: string): void {
    const sockets = this.workspaceTerminals.get(key);
    if (!sockets) return;
    this.workspaceTerminals.delete(key);
    for (const socket of sockets) {
      closeSocket(socket, code, boundedSocketReason(reason));
    }
  }

  private async recordWorkspaceError(workspace: WorkspaceRecord, message: string): Promise<void> {
    const key = workspaceKey(workspace.owner, workspace.org, workspace.id);
    await this.state.runExclusive(async () => {
      const current = await this.state.storage.get<WorkspaceRecord>(key);
      if (!current || current.leaseID !== workspace.leaseID) {
        return;
      }
      current.error = message.slice(0, 500);
      delete current.provisionClaim;
      delete current.provisionClaimExpiresAt;
      delete current.reconcileAfter;
      delete current.recoveryMisses;
      current.updatedAt = new Date().toISOString();
      await this.state.storage.put(key, current);
    });
  }

  private async completeWorkspaceCreate(
    workspace: WorkspaceRecord,
    expectedClaim: string | undefined,
  ): Promise<void> {
    const key = workspaceKey(workspace.owner, workspace.org, workspace.id);
    await this.state.runExclusive(async () => {
      const current = await this.state.storage.get<WorkspaceRecord>(key);
      if (!current || current.leaseID !== workspace.leaseID) {
        return;
      }
      if (expectedClaim && current.provisionClaim !== expectedClaim) {
        return;
      }
      delete current.error;
      delete current.provisionClaim;
      delete current.provisionClaimExpiresAt;
      delete current.reconcileAfter;
      delete current.recoveryMisses;
      current.updatedAt = new Date().toISOString();
      await this.state.storage.put(key, current);
    });
  }

  private async deferWorkspaceReconciliation(
    workspace: WorkspaceRecord,
    releaseProvisionClaim = false,
  ): Promise<void> {
    const key = workspaceKey(workspace.owner, workspace.org, workspace.id);
    await this.state.runExclusive(async () => {
      const current = await this.state.storage.get<WorkspaceRecord>(key);
      if (!current || current.leaseID !== workspace.leaseID) {
        return;
      }
      const now = new Date();
      if (releaseProvisionClaim) {
        delete current.provisionClaim;
        delete current.provisionClaimExpiresAt;
      }
      current.reconcileAfter = new Date(now.getTime() + workspaceReconcileIntervalMs).toISOString();
      current.updatedAt = now.toISOString();
      await this.state.storage.put(key, current);
    });
  }

  private async recordWorkspaceRecoveryMiss(
    workspace: WorkspaceRecord,
    provisioningDeadline: number,
  ): Promise<void> {
    const key = workspaceKey(workspace.owner, workspace.org, workspace.id);
    await this.state.runExclusive(async () => {
      const current = await this.state.storage.get<WorkspaceRecord>(key);
      if (!current || current.leaseID !== workspace.leaseID) {
        return;
      }
      const now = new Date();
      current.recoveryMisses = (current.recoveryMisses ?? 0) + 1;
      const delay = Math.min(
        workspaceReconcileIntervalMs * 2 ** Math.min(current.recoveryMisses - 1, 5),
        workspaceReconcileMaxIntervalMs,
      );
      const retryAt = Number.isFinite(provisioningDeadline)
        ? Math.min(now.getTime() + delay, provisioningDeadline)
        : now.getTime() + delay;
      current.reconcileAfter = new Date(retryAt).toISOString();
      current.updatedAt = now.toISOString();
      await this.state.storage.put(key, current);
    });
  }

  private async retryWorkspaceProvisioning(
    workspace: WorkspaceRecord,
    lease: LeaseRecord,
  ): Promise<void> {
    const key = workspaceKey(workspace.owner, workspace.org, workspace.id);
    await this.state.runExclusive(async () => {
      const currentWorkspace = await this.state.storage.get<WorkspaceRecord>(key);
      const currentLease = await this.getLease(lease.id);
      if (
        !currentWorkspace ||
        currentWorkspace.leaseID !== workspace.leaseID ||
        currentWorkspace.releaseRequestedAt ||
        !currentLease ||
        currentLease.providerKeyCleanupPending ||
        !workspaceOwnsLease(currentWorkspace, currentLease) ||
        !(
          (currentLease.state === "provisioning" && !currentLease.provisioningRequestStartedAt) ||
          (currentLease.state === "failed" &&
            currentLease.provisioningResourceMayExist === false &&
            currentLease.provisioningFailureRetryable)
        )
      ) {
        return;
      }
      const now = new Date();
      if (workspaceProvisionDeadline(currentWorkspace) <= now.getTime()) {
        const message = "workspace provisioning deadline expired";
        currentLease.state = "failed";
        currentLease.failureError = message;
        currentLease.provisioningFailureRetryable = false;
        currentLease.updatedAt = now.toISOString();
        currentLease.endedAt = currentLease.updatedAt;
        currentWorkspace.error = message;
        currentWorkspace.updatedAt = now.toISOString();
        delete currentWorkspace.reconcileAfter;
        delete currentWorkspace.provisionClaim;
        delete currentWorkspace.provisionClaimExpiresAt;
        await Promise.all([
          this.putLease(currentLease),
          this.state.storage.put(key, currentWorkspace),
        ]);
        await this.scheduleAlarm();
        return;
      }
      await this.state.storage.delete(leaseKey(currentLease.id));
      currentWorkspace.recoveryMisses = (currentWorkspace.recoveryMisses ?? 0) + 1;
      const delay = Math.min(
        workspaceReconcileIntervalMs * 2 ** Math.min(currentWorkspace.recoveryMisses - 1, 5),
        workspaceReconcileMaxIntervalMs,
      );
      currentWorkspace.reconcileAfter = new Date(now.getTime() + delay).toISOString();
      currentWorkspace.updatedAt = now.toISOString();
      delete currentWorkspace.error;
      delete currentWorkspace.provisionClaim;
      delete currentWorkspace.provisionClaimExpiresAt;
      await this.state.storage.put(key, currentWorkspace);
      await this.scheduleAlarm();
    });
  }

  private async finalizeAbsentWorkspaceLease(
    workspace: WorkspaceRecord,
    lease: LeaseRecord,
  ): Promise<LeaseRecord> {
    const message = "provider resource not found after interrupted workspace provisioning";
    const failed = await this.state.runExclusive(async () => {
      const currentWorkspace = await this.state.storage.get<WorkspaceRecord>(
        workspaceKey(workspace.owner, workspace.org, workspace.id),
      );
      const current = await this.getLease(lease.id);
      if (
        !current ||
        (current.state !== "provisioning" &&
          current.state !== "failed" &&
          !(current.state === "released" && current.releaseDeletesServer === true))
      ) {
        return current ?? lease;
      }
      const failedAt = new Date().toISOString();
      const releaseRequested = Boolean(currentWorkspace?.releaseRequestedAt);
      current.state = releaseRequested ? "released" : "failed";
      current.updatedAt = failedAt;
      current.endedAt = failedAt;
      current.cloudID = "";
      current.serverID = 0;
      current.serverName = "";
      current.host = "";
      current.provisioningResourceMayExist = false;
      current.provisioningFailureRetryable = false;
      delete current.provisioningRequestStartedAt;
      if (releaseRequested) {
        current.releasedAt = failedAt;
        delete current.releaseDeletesServer;
        delete current.failureError;
      } else {
        current.failureError = message;
      }
      clearLeaseCleanupMetadata(current);
      await this.putLease(current);
      return current;
    });
    if (failed.state === "released") {
      await this.clearWorkspaceReleaseError(failed);
    } else {
      await this.recordWorkspaceError(workspace, message);
    }
    return failed;
  }

  private async clearWorkspaceReleaseError(lease: LeaseRecord): Promise<void> {
    if (!lease.workspaceID) {
      return;
    }
    const key = workspaceKey(lease.owner, lease.org, lease.workspaceID);
    const workspace = await this.state.storage.get<WorkspaceRecord>(key);
    if (
      !workspace ||
      !workspaceLeaseMatchesCleanup(workspace, lease) ||
      !workspace.releaseRequestedAt ||
      !workspace.error
    ) {
      return;
    }
    delete workspace.error;
    workspace.updatedAt = new Date().toISOString();
    await this.state.storage.put(key, workspace);
  }

  private async releaseWorkspaceLease(
    workspace: WorkspaceRecord,
  ): Promise<LeaseRecord | undefined> {
    const lease = await this.getLease(workspace.leaseID);
    if (!lease || !workspaceLeaseMatchesCleanup(workspace, lease)) {
      return lease;
    }
    return await this.releaseResolvedLease(lease, { deleteServer: true, keep: false });
  }

  private async leaseRoute(request: Request, leaseID: string, action?: string): Promise<Response> {
    const method = request.method.toUpperCase();
    if (method === "PUT" && action === "registration") {
      return await this.registerLease(request, leaseID);
    }
    if (method === "GET" && action === undefined) {
      const admin = isAdminRequest(request);
      const lease = await this.resolveLease(leaseID, request, false);
      const refreshed =
        lease && this.leaseProviderAccessVisibleToRequest(lease, request, admin)
          ? await this.refreshLeaseAccessForResolution(lease)
          : lease;
      return refreshed
        ? json({ lease: this.leaseForRequest(refreshed, request, admin) })
        : notFound();
    }
    if (method === "POST" && action === "heartbeat") {
      return this.heartbeatLease(request, leaseID);
    }
    if (method === "POST" && action === "tailscale") {
      const admin = isAdminRequest(request);
      const lease = await this.resolveLease(leaseID, request, admin);
      if (!lease) {
        return notFound();
      }
      if (!this.leaseManageableByRequest(lease, request, admin)) {
        return json(
          { error: "forbidden", message: "lease manage access required" },
          { status: 403 },
        );
      }
      const input = await readJson<Partial<TailscaleMetadata>>(request);
      lease.tailscale = mergeTailscaleMetadata(lease.tailscale, input);
      lease.updatedAt = new Date().toISOString();
      await this.putLease(lease);
      return json({ lease: this.leaseForRequest(lease, request, admin) });
    }
    if (method === "POST" && action === "release") {
      return this.releaseLease(request, leaseID, false);
    }
    if (action === "share") {
      return await this.shareLeaseRoute(request, leaseID);
    }
    return json({ error: "not_found" }, { status: 404 });
  }

  private async registerLease(request: Request, leaseID: string): Promise<Response> {
    if (!validRegisteredLeaseID(leaseID)) {
      return json({ error: "invalid_lease_id" }, { status: 400 });
    }
    const owner = requestOwner(request);
    const org = requestOrg(request, this.env);
    const input = await readJson<LeaseRegistrationRequest>(request);
    const provider = sanitizeRunnerProvider(input.provider);
    const target = sanitizeRegisteredTarget(input.target);
    const host = sanitizeRegisteredHost(input.host);
    if (!provider) {
      return json({ error: "invalid_provider" }, { status: 400 });
    }
    if (!target) {
      return json({ error: "invalid_target" }, { status: 400 });
    }
    if (!host) {
      return json({ error: "host_required" }, { status: 400 });
    }
    const runtimeAdapterID = input.runtimeAdapterID;
    const runtimeAdapterWorkspaceID = input.runtimeAdapterWorkspaceID;
    const runtimeAdapterRegistrationID = input.runtimeAdapterRegistrationID;
    if (
      (runtimeAdapterID !== undefined ||
        runtimeAdapterWorkspaceID !== undefined ||
        runtimeAdapterRegistrationID !== undefined) &&
      (!validRuntimeAdapterID(runtimeAdapterID) ||
        !validRuntimeAdapterID(runtimeAdapterWorkspaceID) ||
        !validRuntimeAdapterID(runtimeAdapterRegistrationID))
    ) {
      return json(
        {
          error: "invalid_runtime_adapter_binding",
          message:
            "runtime adapter id, workspace id, and registration id must all be valid DNS-style identifiers",
        },
        { status: 400 },
      );
    }

    if (await this.state.storage.get(workspaceLeaseReservationKey(leaseID))) {
      return workspaceManagedLeaseResponse();
    }
    const existing = await this.getLease(leaseID);
    if (
      existing &&
      (existing.owner !== owner || existing.org !== org || !isRegisteredLease(existing))
    ) {
      return json(
        { error: "lease_id_conflict", message: "lease id belongs to another lifecycle or owner" },
        { status: 409 },
      );
    }
    if (existing && existing.provider !== provider) {
      return json(
        { error: "provider_conflict", message: "registered lease provider cannot change" },
        { status: 409 },
      );
    }
    if (existing?.runtimeAdapterDeleteRequestedAt) {
      return json(
        {
          error: "runtime_adapter_delete_pending",
          message: "registered lease cannot be refreshed while its workspace deletion is pending",
        },
        { status: 409 },
      );
    }
    if (
      existing &&
      leaseIsLive(existing) &&
      existing.runtimeAdapterID &&
      runtimeAdapterID &&
      (existing.runtimeAdapterID !== runtimeAdapterID ||
        existing.runtimeAdapterWorkspaceID !== runtimeAdapterWorkspaceID ||
        (existing.runtimeAdapterRegistrationID !== undefined &&
          existing.runtimeAdapterRegistrationID !== runtimeAdapterRegistrationID))
    ) {
      return json(
        {
          error: "runtime_adapter_conflict",
          message: "registered lease runtime adapter binding cannot change",
        },
        { status: 409 },
      );
    }
    const inheritRuntimeAdapterBinding =
      existing !== undefined && leaseIsLive(existing) && runtimeAdapterID === undefined;
    const effectiveRuntimeAdapterID =
      runtimeAdapterID ?? (inheritRuntimeAdapterBinding ? existing.runtimeAdapterID : undefined);
    const effectiveRuntimeAdapterWorkspaceID =
      runtimeAdapterWorkspaceID ??
      (inheritRuntimeAdapterBinding ? existing.runtimeAdapterWorkspaceID : undefined);
    const effectiveRuntimeAdapterRegistrationID =
      runtimeAdapterRegistrationID ??
      (inheritRuntimeAdapterBinding ? existing.runtimeAdapterRegistrationID : undefined);
    if (
      effectiveRuntimeAdapterID &&
      effectiveRuntimeAdapterWorkspaceID &&
      !validRuntimeAdapterID(effectiveRuntimeAdapterRegistrationID)
    ) {
      return json(
        {
          error: "runtime_adapter_registration_required",
          message: "runtime adapter registrations require an immutable registration id",
        },
        { status: 409 },
      );
    }
    if (
      existing &&
      effectiveRuntimeAdapterRegistrationID &&
      existing.runtimeAdapterRegistrationID === effectiveRuntimeAdapterRegistrationID &&
      (!leaseIsLive(existing) || !existing.runtimeAdapterID)
    ) {
      return json(
        {
          error: "runtime_adapter_registration_replayed",
          message: "a new runtime adapter registration requires a new registration id",
        },
        { status: 409 },
      );
    }
    let runtimeAdapterIdentity: RuntimeAdapterIdentityRecord | undefined;
    if (effectiveRuntimeAdapterID && effectiveRuntimeAdapterWorkspaceID) {
      const identity = await this.state.storage.get<RuntimeAdapterIdentityRecord>(
        runtimeAdapterIdentityKey(effectiveRuntimeAdapterID),
      );
      if (!identity) {
        return json(
          {
            error: "runtime_adapter_unclaimed",
            message: "runtime adapter id must be claimed before it can be bound to a lease",
          },
          { status: 409 },
        );
      }
      if (
        identity.adapterID !== effectiveRuntimeAdapterID ||
        identity.owner !== owner ||
        identity.org !== org
      ) {
        return json(
          {
            error: "runtime_adapter_conflict",
            message: "runtime adapter id belongs to another owner or organization",
          },
          { status: 409 },
        );
      }
      runtimeAdapterIdentity = identity;
    }

    const leases = await this.leaseRecords();
    if (
      effectiveRuntimeAdapterID &&
      effectiveRuntimeAdapterWorkspaceID &&
      leases.some(
        (lease) =>
          lease.id !== leaseID &&
          (leaseIsLive(lease) || Boolean(lease.runtimeAdapterDeleteRequestedAt)) &&
          lease.runtimeAdapterID === effectiveRuntimeAdapterID &&
          lease.runtimeAdapterWorkspaceID === effectiveRuntimeAdapterWorkspaceID,
      )
    ) {
      return json(
        {
          error: "runtime_adapter_workspace_conflict",
          message: "runtime adapter workspace is already bound to a live lease or pending deletion",
        },
        { status: 409 },
      );
    }
    const now = new Date();
    const nowISO = now.toISOString();
    const ttlSeconds = clampLeaseSeconds(input.ttlSeconds, 86_400);
    const idleTimeoutSeconds = clampLeaseSeconds(input.idleTimeoutSeconds, 86_400);
    const slug =
      existing?.slug ||
      allocateLeaseSlug(
        normalizeLeaseSlug(input.slug) || leaseSlugFromID(leaseID),
        leaseID,
        owner,
        org,
        leases,
      );
    const record: LeaseRecord = {
      ...existing,
      id: leaseID,
      slug,
      provider,
      lifecycle: "registered",
      ...(effectiveRuntimeAdapterID && effectiveRuntimeAdapterWorkspaceID
        ? {
            runtimeAdapterID: effectiveRuntimeAdapterID,
            runtimeAdapterWorkspaceID: effectiveRuntimeAdapterWorkspaceID,
            runtimeAdapterRegistrationID: effectiveRuntimeAdapterRegistrationID!,
          }
        : {}),
      target,
      ...(target === "windows"
        ? { windowsMode: input.windowsMode === "wsl2" ? "wsl2" : "normal" }
        : {}),
      desktop: input.desktop === true,
      desktopEnv: nonSecretString(input.desktopEnv),
      browser: input.browser === true,
      code: input.code === true,
      cloudID: nonSecretString(input.cloudID),
      owner,
      org,
      profile: nonSecretString(input.profile) || "default",
      class: nonSecretString(input.class) || "registered",
      serverType: nonSecretString(input.serverType) || "registered",
      serverID:
        Number.isSafeInteger(input.serverID) && (input.serverID ?? 0) >= 0
          ? (input.serverID ?? 0)
          : 0,
      serverName: nonSecretString(input.serverName) || slug,
      providerKey: "",
      host,
      sshUser: nonSecretString(input.sshUser) || "crabbox",
      sshPort: sanitizeRegisteredPort(input.sshPort) || "22",
      workRoot: nonSecretString(input.workRoot) || "/workspaces/crabbox",
      keep: true,
      ttlSeconds,
      idleTimeoutSeconds,
      estimatedHourlyUSD: 0,
      maxEstimatedUSD: 0,
      state: "active",
      createdAt: existing?.createdAt || nowISO,
      registeredAt: existing?.registeredAt || nowISO,
      updatedAt: nowISO,
      lastTouchedAt: nowISO,
      expiresAt: registeredLeaseExpiresAt(now, ttlSeconds, idleTimeoutSeconds).toISOString(),
    };
    if (input.pond) {
      record.pond = nonSecretString(input.pond);
    } else {
      delete record.pond;
    }
    if (target !== "windows") {
      delete record.windowsMode;
    }
    const fallbackPorts = sanitizeRegisteredPorts(input.sshFallbackPorts);
    if (fallbackPorts) {
      record.sshFallbackPorts = fallbackPorts;
    } else {
      delete record.sshFallbackPorts;
    }
    const exposedPorts = sanitizeRegisteredPorts(input.exposedPorts);
    if (exposedPorts) {
      record.exposedPorts = exposedPorts;
    } else {
      delete record.exposedPorts;
    }
    delete record.endedAt;
    delete record.releasedAt;
    delete record.releaseDeletesServer;
    clearLeaseCleanupMetadata(record);
    if (!effectiveRuntimeAdapterID || !effectiveRuntimeAdapterWorkspaceID) {
      delete record.runtimeAdapterID;
      delete record.runtimeAdapterWorkspaceID;
    }
    if (!existing || !leaseIsLive(existing)) {
      clearRuntimeAdapterDeleteMetadata(record);
    }
    await this.putLease(record);
    if (runtimeAdapterIdentity) {
      await this.confirmRuntimeAdapterIdentity(runtimeAdapterIdentity, nowISO);
    }
    await this.scheduleAlarm();
    return json({ lease: publicLeaseRecord(record) }, { status: existing ? 200 : 201 });
  }

  private async heartbeatLease(request: Request, leaseID: string): Promise<Response> {
    const input = await optionalJson<{
      idleTimeoutSeconds?: number;
      telemetry?: Partial<LeaseTelemetry>;
    }>(request);
    const requestCIDRs = requestSourceCIDRs(request);
    const committed = await this.state.runExclusive(async () => {
      const admin = isAdminRequest(request);
      const lease = await this.resolveLease(leaseID, request, admin);
      if (!lease) {
        return notFound();
      }
      if (!this.leaseManageableByRequest(lease, request, admin)) {
        return json(
          { error: "forbidden", message: "lease manage access required" },
          { status: 403 },
        );
      }
      if (lease.workspaceID) {
        return workspaceManagedLeaseResponse();
      }
      if (lease.cleanupStartedAt) {
        return json(
          { error: "cleanup_in_progress", message: "lease cleanup has already started" },
          { status: 409 },
        );
      }
      const heartbeatError = leaseHeartbeatStateError(lease);
      if (heartbeatError) {
        return json(
          {
            error: heartbeatError,
            message:
              heartbeatError === "lease_expired"
                ? "lease heartbeat deadline has passed"
                : "lease has already ended",
          },
          { status: 409 },
        );
      }
      return structuredClone(await this.applyLeaseHeartbeatState(lease, input));
    });
    if (committed instanceof Response) {
      return committed;
    }
    const managedProvider = managedLeaseProvider(committed);
    if (requestCIDRs.length === 0 || !managedProvider) {
      return json({ lease: this.leaseForRequest(committed, request, isAdminRequest(request)) });
    }

    const refresh = async (): Promise<LeaseRecord> => {
      const snapshot = await this.state.runExclusive(async () => {
        const latest = await this.getLease(committed.id);
        if (!latest || latest.state !== "active" || latest.cleanupStartedAt) {
          return undefined;
        }
        const accessLeases = await this.providerAccessLeaseRecords();
        return {
          lease: structuredClone(latest),
          context: providerAccessContext(
            requestCIDRs,
            replaceProviderAccessState(accessLeases, latest),
          ),
        };
      });
      if (!snapshot) {
        return (await this.getLease(committed.id)) ?? committed;
      }
      const provider = this.provider(
        managedProvider,
        snapshot.lease.region,
        snapshot.lease.providerProject,
      );
      const refreshed = await provider.refreshLeaseAccess?.(snapshot.lease, snapshot.context);
      if (!refreshed) {
        return snapshot.lease;
      }
      return await this.state.runExclusive(async () => {
        const latest = await this.getLease(committed.id);
        if (!latest || latest.state !== "active" || latest.cleanupStartedAt) {
          return latest ?? snapshot.lease;
        }
        const merged = applyLeaseRecordChanges(latest, snapshot.lease, refreshed);
        await this.putLease(merged);
        if (managedProvider === "aws") {
          await this.markAWSIngressReconcilePending(merged);
        }
        await this.scheduleAlarm();
        return merged;
      });
    };
    const lease =
      managedProvider === "aws" && !committed.network?.awsPrivate
        ? await this.withAWSIngressOperationLock(refresh)
        : await refresh();
    return json({ lease: this.leaseForRequest(lease, request, isAdminRequest(request)) });
  }

  private async refreshLeaseAccessForResolution(lease: LeaseRecord): Promise<LeaseRecord> {
    if (
      lease.state !== "active" ||
      lease.cleanupStartedAt ||
      Date.parse(lease.expiresAt) <= Date.now()
    ) {
      return lease;
    }
    const managedProvider = managedLeaseProvider(lease);
    if (!managedProvider) {
      return lease;
    }
    const provider = this.provider(managedProvider, lease.region, lease.providerProject);
    const refreshed = await provider.refreshLeaseAccessForResolution?.(structuredClone(lease));
    if (!refreshed) {
      return lease;
    }
    return await this.state.runExclusive(async () => {
      const latest = await this.getLease(lease.id);
      if (
        !latest ||
        latest.state !== "active" ||
        latest.cleanupStartedAt ||
        latest.cloudID !== lease.cloudID
      ) {
        return latest ?? lease;
      }
      const merged = applyLeaseRecordChanges(latest, lease, refreshed);
      await this.putLease(merged);
      await this.scheduleAlarm();
      return merged;
    });
  }

  private async applyLeaseHeartbeatState(
    lease: LeaseRecord,
    input: {
      idleTimeoutSeconds?: number;
      telemetry?: Partial<LeaseTelemetry>;
    },
  ): Promise<LeaseRecord> {
    const now = new Date();
    const requestedIdleTimeoutSeconds = input.idleTimeoutSeconds;
    if (
      Number.isFinite(requestedIdleTimeoutSeconds) &&
      requestedIdleTimeoutSeconds !== undefined &&
      requestedIdleTimeoutSeconds > 0
    ) {
      lease.idleTimeoutSeconds = clampLeaseSeconds(requestedIdleTimeoutSeconds, 86_400);
    }
    const telemetry = sanitizeLeaseTelemetry(input.telemetry, now);
    if (telemetry) {
      lease.telemetry = telemetry;
      lease.telemetryHistory = appendLeaseTelemetryHistory(lease.telemetryHistory, telemetry);
    }
    lease.updatedAt = now.toISOString();
    lease.lastTouchedAt = now.toISOString();
    lease.expiresAt = recomputeLeaseExpiresAt(lease, now).toISOString();
    clearLeaseCleanupMetadata(lease);
    await this.putLease(lease);
    await this.scheduleAlarm();
    return lease;
  }

  private async prepareTailscaleConfig(
    config: ReturnType<typeof leaseConfig>,
    input: LeaseRequest,
    leaseID: string,
    slug: string,
  ): Promise<Response | undefined> {
    if (!config.tailscale) {
      return undefined;
    }
    if (config.target !== "linux") {
      return json(
        {
          error: "unsupported_target",
          message: "brokered Tailscale provisioning currently supports managed Linux leases only",
        },
        { status: 400 },
      );
    }
    if (!tailscaleAllowed(this.env)) {
      return json(
        { error: "tailscale_disabled", message: "Tailscale is disabled for this coordinator" },
        { status: 403 },
      );
    }
    try {
      config.tailscaleTags = validateTailscaleTags(
        input.tailscaleTags ?? config.tailscaleTags,
        tailscaleDefaultTags(this.env),
      );
      config.tailscaleHostname = renderTailscaleHostname(
        input.tailscaleHostname || config.tailscaleHostname || "crabbox-{slug}",
        leaseID,
        slug,
        config.provider,
      );
      config.tailscaleExitNode =
        nonSecretString(input.tailscaleExitNode) || config.tailscaleExitNode;
      config.tailscaleExitNodeAllowLanAccess =
        input.tailscaleExitNodeAllowLanAccess ?? config.tailscaleExitNodeAllowLanAccess;
      if (!config.tailscaleExitNode) {
        config.tailscaleExitNodeAllowLanAccess = false;
      }
      const install = tailscaleInstallConfig(this.env);
      config.tailscaleInstallMode = install.mode;
      config.tailscaleVersion = install.version;
      config.tailscaleSHA256 = install.sha256;
      config.tailscaleAuthKey = await createTailscaleAuthKey(this.env, {
        hostname: config.tailscaleHostname,
        tags: config.tailscaleTags,
        description: `crabbox ${leaseID} ${slug}`,
      });
    } catch (error) {
      const message = coordinatorErrorMessage(this.env, error);
      if (message.includes("tags not allowed") || message.includes("requires at least one")) {
        return json({ error: "invalid_tailscale_tags", message }, { status: 400 });
      }
      const tagOwnershipMessage = tailscaleTagOwnershipErrorMessage(error);
      if (tagOwnershipMessage) {
        return json(
          { error: "invalid_tailscale_tags", message: tagOwnershipMessage },
          { status: 400 },
        );
      }
      return json({ error: "tailscale_unavailable", message }, { status: 502 });
    }
    return undefined;
  }

  private async releaseLease(request: Request, leaseID: string, admin: boolean): Promise<Response> {
    const lease = await this.resolveLease(leaseID, request, admin);
    if (!lease) {
      return notFound();
    }
    if (!this.leaseManageableByRequest(lease, request, admin)) {
      return json({ error: "forbidden", message: "lease manage access required" }, { status: 403 });
    }
    if (lease.workspaceID) {
      return workspaceManagedLeaseResponse();
    }
    const body = await optionalJson<{
      delete?: boolean;
      runtimeAdapterDeleteCompletion?: unknown;
      runtimeAdapterLegacyDeleteCompletion?: unknown;
    }>(request);
    const runtimeAdapterCompletion = runtimeAdapterDeleteCompletion(
      body.runtimeAdapterDeleteCompletion,
    );
    const runtimeAdapterLegacyCompletion = runtimeAdapterLegacyDeleteCompletion(
      body.runtimeAdapterLegacyDeleteCompletion,
    );
    if (
      body.runtimeAdapterDeleteCompletion !== undefined &&
      runtimeAdapterCompletion === undefined
    ) {
      return json(
        {
          error: "invalid_runtime_adapter_delete_completion",
          message:
            "runtime adapter delete completion must identify an absent adapter workspace registration",
        },
        { status: 400 },
      );
    }
    if (
      body.runtimeAdapterLegacyDeleteCompletion !== undefined &&
      runtimeAdapterLegacyCompletion === undefined
    ) {
      return json(
        {
          error: "invalid_runtime_adapter_legacy_delete_completion",
          message:
            "legacy runtime adapter delete completion must identify an absent adapter workspace binding",
        },
        { status: 400 },
      );
    }
    if (runtimeAdapterCompletion && runtimeAdapterLegacyCompletion) {
      return json(
        {
          error: "invalid_runtime_adapter_delete_completion",
          message: "runtime adapter delete completion must select exactly one generation mode",
        },
        { status: 400 },
      );
    }
    if (runtimeAdapterCompletion) {
      return await this.completeRuntimeAdapterDelete(
        request,
        lease,
        runtimeAdapterCompletion,
        admin,
      );
    }
    if (runtimeAdapterLegacyCompletion) {
      return await this.completeLegacyRuntimeAdapterDelete(
        request,
        lease,
        runtimeAdapterLegacyCompletion,
        admin,
      );
    }
    if (
      body.delete === true &&
      isRegisteredLease(lease) &&
      lease.runtimeAdapterID &&
      lease.runtimeAdapterWorkspaceID
    ) {
      if (!lease.runtimeAdapterRegistrationID) {
        return json(
          {
            error: "runtime_adapter_registration_required",
            message:
              "runtime adapter workspace deletion requires an immutable registration generation",
          },
          { status: 409 },
        );
      }
      return await this.deleteRegisteredRuntimeAdapterWorkspace(request, lease);
    }
    if (lease.runtimeAdapterDeleteRequestedAt) {
      return this.runtimeAdapterDeletePendingResponse(lease, request, admin);
    }
    const shouldDelete = body.delete ?? !lease.keep;
    if (lease.cleanupStartedAt) {
      if (!shouldDelete) {
        return json(
          {
            error: "cleanup_in_progress",
            message: "lease cleanup has already started",
            lease: this.leaseForRequest(lease, request, admin),
          },
          { status: 409 },
        );
      }
      await this.scheduleAlarm();
      return json({ lease: this.leaseForRequest(lease, request, admin) });
    }
    const released = await this.releaseResolvedLease(lease, { deleteServer: shouldDelete });
    if (released.runtimeAdapterDeleteRequestedAt) {
      return this.runtimeAdapterDeletePendingResponse(released, request, admin);
    }
    if (released.cleanupStartedAt && !shouldDelete) {
      return json(
        {
          error: "cleanup_in_progress",
          message: "lease cleanup has already started",
          lease: this.leaseForRequest(released, request, admin),
        },
        { status: 409 },
      );
    }
    return json({ lease: this.leaseForRequest(released, request, admin) });
  }

  private async completeRuntimeAdapterDelete(
    request: Request,
    lease: LeaseRecord,
    completion: RuntimeAdapterDeleteCompletion,
    admin: boolean,
  ): Promise<Response> {
    if (
      !isRegisteredLease(lease) ||
      lease.runtimeAdapterID !== completion.adapterID ||
      lease.runtimeAdapterWorkspaceID !== completion.workspaceID ||
      lease.runtimeAdapterRegistrationID !== completion.registrationID
    ) {
      return json(
        {
          error: "runtime_adapter_delete_completion_mismatch",
          message:
            "runtime adapter delete completion does not match the registered lease generation",
        },
        { status: 409 },
      );
    }
    if (
      !admin &&
      (lease.owner !== requestOwner(request) || lease.org !== requestOrg(request, this.env))
    ) {
      return json(
        {
          error: "forbidden",
          message: "runtime adapter delete completion requires the lease owner",
        },
        { status: 403 },
      );
    }
    return await this.serializeRuntimeAdapterDelete(lease.id, async () => {
      const result = await this.finalizeRuntimeAdapterDeleteCompletion(lease, completion);
      if (result.status === "in-flight") {
        return runtimeAdapterDeleteInFlightResponse(result.retryAt);
      }
      if (result.status === "mismatch") {
        const current = await this.getLease(lease.id);
        return json(
          {
            error: "lease_state_changed",
            message: "lease changed while completing its runtime adapter delete",
            lease: current ? publicLeaseRecord(current) : undefined,
          },
          { status: 409 },
        );
      }
      return json({ lease: publicLeaseRecord(result.lease) });
    });
  }

  private async completeLegacyRuntimeAdapterDelete(
    request: Request,
    lease: LeaseRecord,
    completion: RuntimeAdapterLegacyDeleteCompletion,
    admin: boolean,
  ): Promise<Response> {
    if (
      !isRegisteredLease(lease) ||
      lease.runtimeAdapterID !== completion.adapterID ||
      lease.runtimeAdapterWorkspaceID !== completion.workspaceID ||
      lease.runtimeAdapterRegistrationID
    ) {
      return json(
        {
          error: "runtime_adapter_delete_completion_mismatch",
          message:
            "legacy runtime adapter delete completion does not match a generation-less registered binding",
        },
        { status: 409 },
      );
    }
    if (
      !admin &&
      (lease.owner !== requestOwner(request) || lease.org !== requestOrg(request, this.env))
    ) {
      return json(
        {
          error: "forbidden",
          message: "legacy runtime adapter delete completion requires the lease owner",
        },
        { status: 403 },
      );
    }
    return await this.serializeRuntimeAdapterDelete(lease.id, async () => {
      const result = await this.finalizeLegacyRuntimeAdapterDelete(lease, completion);
      if (result.status === "in-flight") {
        return runtimeAdapterDeleteInFlightResponse(result.retryAt);
      }
      if (result.status === "mismatch") {
        const current = await this.getLease(lease.id);
        return json(
          {
            error: "lease_state_changed",
            message: "lease changed while completing its legacy runtime adapter delete",
            lease: current ? publicLeaseRecord(current) : undefined,
          },
          { status: 409 },
        );
      }
      return json({ lease: publicLeaseRecord(result.lease) });
    });
  }

  private async shareLeaseRoute(request: Request, leaseID: string): Promise<Response> {
    const method = request.method.toUpperCase();
    const admin = isAdminRequest(request);
    const lease = await this.resolveLease(leaseID, request, admin);
    if (!lease) {
      return notFound();
    }
    if (!this.leaseManageableByRequest(lease, request, admin)) {
      return json({ error: "forbidden", message: "lease manage access required" }, { status: 403 });
    }
    if (method === "GET") {
      return json({ leaseID: lease.id, share: normalizedLeaseShare(lease.share) });
    }
    if (method === "PUT") {
      const input = await readJson<Partial<LeaseShare>>(request);
      const previousShare = normalizedLeaseShare(lease.share);
      lease.share = sanitizeLeaseShare(input, requestOwner(request));
      lease.updatedAt = new Date().toISOString();
      await this.putLeaseShareAndRevokeBridges(lease, previousShare);
      return json({ leaseID: lease.id, share: normalizedLeaseShare(lease.share) });
    }
    if (method === "DELETE") {
      const previousShare = normalizedLeaseShare(lease.share);
      const input = await optionalJson<{ user?: string; org?: boolean }>(request);
      const share = normalizedLeaseShare(lease.share);
      const user = normalizeShareUser(input.user);
      if (user) {
        delete share.users[user];
      }
      if (input.org) {
        delete share.org;
      }
      if (!user && !input.org) {
        lease.share = undefined;
      } else {
        lease.share = sanitizeLeaseShare(share, requestOwner(request));
      }
      lease.updatedAt = new Date().toISOString();
      await this.putLeaseShareAndRevokeBridges(lease, previousShare);
      return json({ leaseID: lease.id, share: normalizedLeaseShare(lease.share) });
    }
    return json({ error: "not_found" }, { status: 404 });
  }

  private async putLeaseShareAndRevokeBridges(
    lease: LeaseRecord,
    previousShare: NormalizedLeaseShare,
  ): Promise<void> {
    await this.withBridgeTicketLock(async () => {
      await this.putLease(lease);
      if (leaseShareAccessShrank(previousShare, normalizedLeaseShare(lease.share))) {
        await this.revokeUnauthorizedLeaseBridges(lease);
      }
    });
  }

  private async revokeUnauthorizedLeaseBridges(lease: LeaseRecord): Promise<void> {
    const code = 1008;
    const reason = "lease access revoked";
    const adminGrants = await this.currentAdminGrantValidation();
    for (const agent of this.webVNCAgents.get(lease.id)?.values() ?? []) {
      const attachment = this.bridgeAttachment(agent);
      if (
        attachment?.kind !== "webvnc-agent" ||
        !completeBridgePrincipal(attachment) ||
        this.leaseManagerAuthorized(lease, withCurrentAdminGrant(attachment, adminGrants))
      ) {
        continue;
      }
      this.clearWebVNCAgent(lease.id, attachment.id, agent);
      closeSocket(agent, code, reason);
    }
    const codeAgent = this.codeAgents.get(lease.id);
    const codeAgentAttachment = codeAgent ? this.bridgeAttachment(codeAgent) : undefined;
    if (
      codeAgent &&
      codeAgentAttachment?.kind === "code-agent" &&
      completeBridgePrincipal(codeAgentAttachment) &&
      !this.leaseManagerAuthorized(lease, withCurrentAdminGrant(codeAgentAttachment, adminGrants))
    ) {
      this.clearCodeAgent(lease.id, codeAgent);
      closeSocket(codeAgent, code, reason);
    }
    for (const viewer of this.openWebVNCViewers(lease.id)) {
      if (this.leaseViewerAuthorized(lease, withCurrentAdminGrant(viewer, adminGrants))) {
        continue;
      }
      this.clearWebVNCViewer(lease.id, viewer.id, viewer.socket);
      closeSocket(viewer.socket, code, reason);
    }
    for (const [id, viewer] of this.codeViewers) {
      const attachment = this.bridgeAttachment(viewer);
      if (
        attachment?.kind !== "code-viewer" ||
        attachment.leaseID !== lease.id ||
        this.leaseViewerAuthorized(lease, withCurrentAdminGrant(attachment, adminGrants))
      ) {
        continue;
      }
      this.clearCodeViewer(lease.id, id, viewer, code, reason);
      closeSocket(viewer, code, reason);
    }
    const revokedEgressSessions = new Set<string>();
    for (const socket of [...this.egressHosts.values(), ...this.egressClients.values()]) {
      const attachment = this.bridgeAttachment(socket);
      if (
        (attachment?.kind !== "egress-host" && attachment?.kind !== "egress-client") ||
        attachment.leaseID !== lease.id ||
        this.leaseManagerAuthorized(lease, withCurrentAdminGrant(attachment, adminGrants))
      ) {
        continue;
      }
      revokedEgressSessions.add(attachment.sessionID);
    }
    for (const sessionID of revokedEgressSessions) {
      this.clearEgressSession(lease.id, sessionID, code, reason);
    }
  }

  private leaseManagerAuthorized(
    lease: LeaseRecord,
    principal: { owner?: string; org?: string; admin?: boolean },
  ): boolean {
    if (!completeBridgePrincipal(principal)) {
      return false;
    }
    const role = this.leaseAccessRoleForPrincipal(lease, principal);
    return role === "owner" || role === "manage";
  }

  private leaseViewerAuthorized(
    lease: LeaseRecord,
    principal: { owner?: string; org?: string; admin?: boolean },
  ): boolean {
    if (!completeBridgePrincipal(principal)) {
      return false;
    }
    return this.leaseAccessRoleForPrincipal(lease, principal) !== undefined;
  }

  private whoami(request: Request): Response {
    const response: {
      owner: string;
      org: string;
      auth: string;
      admin: boolean;
      tokenExpiresAt?: string;
    } = {
      owner: requestOwner(request),
      org: requestOrgLabel(request, this.env),
      auth: request.headers.get("x-crabbox-auth") || "bearer",
      admin: isAdminRequest(request),
    };
    const tokenExpiresAt = request.headers.get("x-crabbox-token-expires-at");
    if (tokenExpiresAt) {
      response.tokenExpiresAt = tokenExpiresAt;
    }
    return json(response);
  }

  private async providerReadiness(request: Request, provider: string): Promise<Response> {
    if (!isCoordinatorProvider(provider)) {
      return json(
        { error: "invalid_provider", message: `unsupported provider: ${provider}` },
        { status: 400 },
      );
    }
    const url = new URL(request.url);
    const requestedAWSRegion = provider === "aws" ? url.searchParams.get("region") : null;
    if (requestedAWSRegion && !sanitizeAWSRegion(requestedAWSRegion)) {
      return json(
        { error: "invalid_region", message: "region must be an AWS region name" },
        { status: 400 },
      );
    }
    const readiness = this.providerConfigurationReadiness(
      provider,
      url.searchParams.get("gcpProject") ?? undefined,
    );
    if (provider === "aws" && readiness.configured && !this.testProviders.aws) {
      readiness.checks = await this.awsProviderCapacityChecks(url.searchParams);
    }
    return json(readiness);
  }

  private providerConfigurationReadiness(
    provider: Provider,
    gcpProject?: string,
  ): ProviderReadiness {
    if (this.testProviders[provider]) {
      return {
        provider,
        configured: true,
        missing: [],
        message: `${provider} test provider is configured`,
      };
    }
    return providerReadiness(provider, this.env, gcpProject);
  }

  private async awsProviderCapacityChecks(
    params: URLSearchParams,
  ): Promise<ProviderReadinessCheck[]> {
    const capacity: NonNullable<LeaseRequest["capacity"]> = {};
    const market = normalizeReadinessMarket(params.get("market"));
    if (market) {
      capacity.market = market;
    }
    const fallback = params.get("fallback");
    if (fallback) {
      capacity.fallback = fallback;
    }
    const leaseRequest: LeaseRequest = {
      provider: "aws",
      target: normalizeReadinessTarget(params.get("target")),
      windowsMode: normalizeReadinessWindowsMode(params.get("windowsMode")),
      serverTypeExplicit: params.get("serverTypeExplicit") === "true",
      capacity,
      sshPublicKey: readinessDummySSHPublicKey,
    };
    const className = params.get("class");
    if (className) {
      leaseRequest.class = className;
    }
    const serverType = params.get("serverType") || params.get("type");
    if (serverType) {
      leaseRequest.serverType = serverType;
    }
    const region = params.get("region");
    if (region) {
      leaseRequest.awsRegion = sanitizeAWSRegion(region);
    }
    const config = leaseConfig(leaseRequest);
    return await new EC2SpotClient(this.env, config.awsRegion).capacityReadinessChecks(config);
  }

  private async portalRoute(request: Request, parts: string[]): Promise<Response> {
    const method = request.method.toUpperCase();
    if (method === "GET" && parts.length === 1) {
      const [visibleLeases, runners, macHosts] = await Promise.all([
        this.portalVisibleLeases(request),
        this.visibleExternalRunners(request),
        this.portalMacHosts(true),
      ]);
      const admin = isAdminRequest(request);
      const leases = this.filterLeases(visibleLeases, request);
      const hostLeases = this.filterLeasesWithoutLimit(visibleLeases, request);
      const manageableLeaseIDs = new Set(
        hostLeases
          .filter((lease) => this.leaseManageableByRequest(lease, request, admin))
          .map((lease) => lease.id),
      );
      return portalHome(
        leases.map(portalLeaseRecord),
        runners.map(portalExternalRunnerRecord),
        request,
        this.attachPortalMacHostLeases(
          this.portalMacHostsVisibleToRequest(macHosts, hostLeases, request),
          hostLeases,
        ).map((host) => (host.lease ? { ...host, lease: portalLeaseRecord(host.lease) } : host)),
        manageableLeaseIDs,
      );
    }
    if (method === "GET" && parts[1] === "admin") {
      if (!isAdminRequest(request)) {
        return portalError("Admin required", "That page requires admin access.", 403);
      }
      const tab =
        parts[2] === undefined || parts[2] === "health"
          ? "health"
          : parts[2] === "leases" || parts[2] === "users"
            ? parts[2]
            : undefined;
      if (!tab || parts[3] !== undefined) {
        return json({ error: "not_found" }, { status: 404 });
      }
      return portalAdmin(await this.portalAdminView(), request, tab);
    }
    if (method === "GET" && parts[1] === "runs" && parts[2]) {
      return await this.portalRunRoute(request, parts[2], parts[3]);
    }
    if (
      method === "GET" &&
      parts[1] === "runners" &&
      parts[2] &&
      parts[3] &&
      parts[4] === undefined
    ) {
      return await this.portalExternalRunnerPage(request, parts[2], parts[3]);
    }
    if (
      method === "GET" &&
      parts[1] === "hosts" &&
      parts[2] &&
      parts[3] &&
      parts[4] === undefined
    ) {
      return await this.portalMacHostPage(request, parts[2], parts[3]);
    }
    if (
      method === "GET" &&
      parts[1] === "hosts" &&
      parts[2] &&
      parts[3] &&
      parts[4] === "vnc" &&
      parts[5] === undefined
    ) {
      return await this.portalMacHostLeaseRedirect(request, parts[2], parts[3], "vnc");
    }
    if (
      method === "POST" &&
      parts[1] === "hosts" &&
      parts[2] &&
      parts[3] &&
      parts[4] === "vnc" &&
      parts[5] === undefined
    ) {
      return await this.portalEnableMacHostVNC(request, parts[2], parts[3]);
    }
    if (method === "GET" && parts[1] === "hosts" && parts[2] && parts[3] && parts[4] === "code") {
      return await this.portalMacHostLeaseRedirect(request, parts[2], parts[3], "code");
    }
    if (method === "GET" && parts[1] === "leases" && parts[2] && parts[3] === undefined) {
      return await this.portalLeasePage(request, parts[2]);
    }
    if (method === "GET" && parts[1] === "leases" && parts[2] && parts[3] === "share") {
      return await this.portalShareLeasePage(request, parts[2]);
    }
    if (method === "POST" && parts[1] === "leases" && parts[2] && parts[3] === "share") {
      return await this.portalShareLeaseAction(request, parts[2]);
    }
    if (
      method === "POST" &&
      parts[1] === "leases" &&
      parts[2] &&
      parts[3] === "release" &&
      parts[4] === undefined
    ) {
      return await this.portalReleaseLease(request, parts[2]);
    }
    if (
      method === "GET" &&
      parts[1] === "leases" &&
      parts[2] &&
      parts[3] === "vnc" &&
      parts[4] === undefined
    ) {
      const lease = await this.resolvePortalLease(parts[2], request);
      if (!lease) {
        return portalError(
          "Lease not found",
          "That lease is not active or is not visible to you.",
          404,
        );
      }
      const error = webVNCLeaseError(lease);
      if (error) {
        return portalError("WebVNC unavailable", error, 409);
      }
      return portalVNC(publicLeaseRecord(lease), {
        canManage: this.leaseManageableByRequest(lease, request, isAdminRequest(request)),
      });
    }
    if (
      method === "POST" &&
      parts[1] === "leases" &&
      parts[2] &&
      parts[3] === "vnc" &&
      parts[4] === "handoff" &&
      parts[5] === undefined
    ) {
      return await this.webVNCCredentialHandoff(request, parts[2]);
    }
    if (
      method === "GET" &&
      parts[1] === "leases" &&
      parts[2] &&
      parts[3] === "vnc" &&
      parts[4] === "status"
    ) {
      return await this.webVNCStatus(request, parts[2]);
    }
    if (
      method === "POST" &&
      parts[1] === "leases" &&
      parts[2] &&
      parts[3] === "vnc" &&
      parts[4] === "control"
    ) {
      return await this.webVNCTakeControl(request, parts[2]);
    }
    if (
      method === "POST" &&
      parts[1] === "leases" &&
      parts[2] &&
      parts[3] === "vnc" &&
      parts[4] === "theme"
    ) {
      return await this.webVNCTheme(request, parts[2]);
    }
    if (
      method === "GET" &&
      parts[1] === "leases" &&
      parts[2] &&
      parts[3] === "vnc" &&
      parts[4] === "viewer"
    ) {
      return await this.webVNCViewer(request, parts[2]);
    }
    if (parts[1] === "leases" && parts[2] && parts[3] === "code") {
      return await this.codePortalProxy(request, parts[2], parts.slice(4));
    }
    return json({ error: "not_found" }, { status: 404 });
  }

  private async portalLeases(request: Request): Promise<LeaseRecord[]> {
    return this.filterLeases(await this.portalVisibleLeases(request), request);
  }

  private async portalVisibleLeases(request: Request): Promise<LeaseRecord[]> {
    const leases = await this.leaseRecords();
    return isAdminRequest(request)
      ? leases
      : leases.filter((lease) => this.leaseVisibleToRequest(lease, request, false));
  }

  private async portalMacHosts(automatic = false): Promise<PortalMacHostRecord[]> {
    if (automatic && !awsAutomaticProbesConfigured(this.env)) {
      return [];
    }
    const region = sanitizeAWSRegion(this.env.CRABBOX_AWS_REGION || "eu-west-1");
    if (!region) {
      return [];
    }
    try {
      const hosts = await new EC2SpotClient(this.env, region).listMacHosts();
      return hosts.map((host) => ({
        id: host.id,
        provider: "aws",
        target: "macos",
        state: host.state,
        region: host.region,
        availabilityZone: host.availabilityZone,
        instanceType: host.instanceType,
        autoPlacement: host.autoPlacement,
        ...(host.allocationTime ? { allocationTime: host.allocationTime } : {}),
      }));
    } catch (error) {
      console.warn(
        `portal mac host inventory unavailable: ${coordinatorErrorMessage(this.env, error)}`,
      );
      return [];
    }
  }

  private attachPortalMacHostLeases(
    hosts: PortalMacHostRecord[],
    leases: LeaseRecord[],
  ): PortalMacHostRecord[] {
    const activeMacLeases = leases.filter(
      (lease) => lease.state === "active" && lease.target === "macos",
    );
    return hosts.map((host) => {
      const lease = activeMacLeases.find((item) => leaseHostID(item) === host.id);
      return lease ? { ...host, lease } : host;
    });
  }

  private portalMacHostsVisibleToRequest(
    hosts: PortalMacHostRecord[],
    leases: LeaseRecord[],
    request: Request,
  ): PortalMacHostRecord[] {
    if (isAdminRequest(request)) {
      return hosts;
    }
    const visibleHostIDs = new Set(
      leases
        .filter((lease) => lease.state === "active" && lease.target === "macos")
        .map(leaseHostID)
        .filter(Boolean),
    );
    return hosts.filter((host) => visibleHostIDs.has(host.id));
  }

  private async resolvePortalMacHost(
    request: Request,
    provider: string,
    hostID: string,
  ): Promise<PortalMacHostRecord | undefined> {
    if (provider !== "aws") {
      return undefined;
    }
    const [leases, hosts] = await Promise.all([
      this.portalVisibleLeases(request),
      this.portalMacHosts(),
    ]);
    const visibleHosts = this.portalMacHostsVisibleToRequest(hosts, leases, request);
    return this.attachPortalMacHostLeases(visibleHosts, leases).find(
      (host) => host.provider === provider && host.id === hostID,
    );
  }

  private async portalAdminView(): Promise<PortalAdminView> {
    const leases = this.portalAdminLeaseSummaries(await this.leaseRecords());
    const providers = await this.portalAdminProviderStatuses(leases);
    return {
      generatedAt: new Date().toISOString(),
      providers,
      users: this.portalAdminUserSummaries(leases),
      leases,
    };
  }

  private async portalAdminProviderStatuses(
    leases: PortalAdminLeaseSummary[],
  ): Promise<PortalAdminProviderStatus[]> {
    const providerSet = new Set<Provider>(coordinatorProviders);
    for (const lease of leases) {
      if (isCoordinatorProvider(lease.provider)) {
        providerSet.add(lease.provider);
      }
    }
    return await Promise.all(
      [...providerSet]
        .toSorted((a, b) => a.localeCompare(b))
        .map((provider) => this.portalAdminProviderStatus(provider, leases)),
    );
  }

  private async portalAdminProviderStatus(
    provider: Provider,
    leases: PortalAdminLeaseSummary[],
  ): Promise<PortalAdminProviderStatus> {
    const readiness =
      provider === "aws" && !this.testProviders.aws && !awsAutomaticProbesConfigured(this.env)
        ? {
            provider,
            configured: false,
            missing: ["explicit AWS configuration"],
            message: "aws automatic probes are disabled until AWS is explicitly configured",
          }
        : this.providerConfigurationReadiness(provider);
    const providerLeases = leases.filter(
      (lease) => lease.provider === provider && lease.lifecycle !== "registered",
    );
    const activeLeases = providerLeases.filter((lease) => lease.state === "active").length;
    const users = new Set(providerLeases.map((lease) => lease.owner || "unknown")).size;
    const recentLeases = providerLeases
      .toSorted((a, b) => b.createdAt.localeCompare(a.createdAt))
      .slice(0, 4);
    if (!readiness.configured) {
      return {
        provider,
        status: "disabled",
        configured: false,
        message: readiness.message,
        missing: readiness.missing,
        activeLeases,
        totalLeases: providerLeases.length,
        users,
        recentLeases,
      };
    }
    try {
      const machines = await this.provider(provider).listCrabboxServers();
      return {
        provider,
        status: "ok",
        configured: true,
        message: readiness.message,
        missing: [],
        machineCount: machines.length,
        activeLeases,
        totalLeases: providerLeases.length,
        users,
        recentLeases,
      };
    } catch (error) {
      return {
        provider,
        status: "bad",
        configured: true,
        message: readiness.message,
        missing: [],
        activeLeases,
        totalLeases: providerLeases.length,
        users,
        recentLeases,
        error: coordinatorErrorMessage(this.env, error),
      };
    }
  }

  private portalAdminLeaseSummaries(leases: LeaseRecord[]): PortalAdminLeaseSummary[] {
    return leases
      .toSorted((a, b) => b.createdAt.localeCompare(a.createdAt))
      .map((lease) => ({
        id: lease.id,
        ...(lease.slug ? { slug: lease.slug } : {}),
        provider: lease.provider,
        lifecycle: lease.lifecycle,
        ...(lease.runtimeAdapterID ? { runtimeAdapterID: lease.runtimeAdapterID } : {}),
        ...(lease.runtimeAdapterWorkspaceID
          ? { runtimeAdapterWorkspaceID: lease.runtimeAdapterWorkspaceID }
          : {}),
        state: lease.state,
        target: lease.target || "linux",
        owner: lease.owner,
        org: orgLabelForDisplay(lease.org),
        class: lease.class,
        serverType: lease.serverType,
        ...(lease.host ? { host: lease.host } : {}),
        createdAt: lease.createdAt,
        expiresAt: lease.expiresAt,
        updatedAt: lease.updatedAt,
      }));
  }

  private portalAdminUserSummaries(leases: PortalAdminLeaseSummary[]): PortalAdminUserSummary[] {
    const users = new Map<string, PortalAdminUserSummary>();
    for (const lease of leases) {
      const owner = lease.owner || "unknown";
      const current =
        users.get(owner) ??
        ({
          owner,
          orgs: [],
          activeLeases: 0,
          totalLeases: 0,
          providers: [],
          lastSeenAt: lease.updatedAt || lease.createdAt,
        } satisfies PortalAdminUserSummary);
      current.totalLeases += 1;
      if (lease.state === "active") {
        current.activeLeases += 1;
      }
      if (lease.org && !current.orgs.includes(lease.org)) {
        current.orgs.push(lease.org);
      }
      if (!current.providers.includes(lease.provider)) {
        current.providers.push(lease.provider);
      }
      const lastSeen = lease.updatedAt || lease.createdAt;
      if (lastSeen.localeCompare(current.lastSeenAt) > 0) {
        current.lastSeenAt = lastSeen;
      }
      users.set(owner, current);
    }
    return [...users.values()]
      .map((user) => ({
        ...user,
        orgs: user.orgs.toSorted((a, b) => a.localeCompare(b)),
        providers: user.providers.toSorted((a, b) => a.localeCompare(b)),
      }))
      .toSorted((a, b) => b.activeLeases - a.activeLeases || a.owner.localeCompare(b.owner));
  }

  private async portalMacHostPage(
    request: Request,
    provider: string,
    hostID: string,
  ): Promise<Response> {
    const host = await this.resolvePortalMacHost(request, provider, hostID);
    if (!host) {
      return portalError(
        "Host not found",
        "That dedicated host is not visible or the provider is not configured.",
        404,
      );
    }
    const canManage = Boolean(
      host.lease && this.leaseManageableByRequest(host.lease, request, isAdminRequest(request)),
    );
    const publicHost = host.lease ? { ...host, lease: publicLeaseRecord(host.lease) } : host;
    return portalMacHostDetail(
      publicHost,
      host.lease ? this.leaseBridgeStatus(host.lease) : undefined,
      { canManage },
    );
  }

  private async portalMacHostLeaseRedirect(
    request: Request,
    provider: string,
    hostID: string,
    action: "vnc" | "code",
  ): Promise<Response> {
    const host = await this.resolvePortalMacHost(request, provider, hostID);
    const lease = host?.lease?.state === "active" ? host.lease : undefined;
    if (!host) {
      return portalError(
        action === "vnc" ? "WebVNC unavailable" : "Code unavailable",
        "That dedicated host is not visible or the provider is not configured.",
        404,
      );
    }
    const publicHost = host.lease ? { ...host, lease: publicLeaseRecord(host.lease) } : host;
    if (!lease) {
      if (action === "vnc") {
        return portalMacHostDetail(publicHost, undefined);
      }
      return portalError(
        "Code unavailable",
        "No active Crabbox lease is attached to that dedicated host.",
        409,
      );
    }
    const error = action === "vnc" ? webVNCLeaseError(lease) : codeLeaseError(lease);
    if (action === "vnc" && error === "lease was not created with desktop=true") {
      return portalMacHostDetail(publicHost, this.leaseBridgeStatus(lease), {
        canManage: this.leaseManageableByRequest(lease, request, isAdminRequest(request)),
      });
    }
    if (error) {
      return portalError(action === "vnc" ? "WebVNC unavailable" : "Code unavailable", error, 409);
    }
    return new Response(null, {
      status: 303,
      headers: {
        location:
          action === "vnc"
            ? `/portal/leases/${encodeURIComponent(lease.id)}/vnc`
            : `/portal/leases/${encodeURIComponent(lease.id)}/code/`,
      },
    });
  }

  private async portalEnableMacHostVNC(
    request: Request,
    provider: string,
    hostID: string,
  ): Promise<Response> {
    const host = await this.resolvePortalMacHost(request, provider, hostID);
    const lease = host?.lease?.state === "active" ? host.lease : undefined;
    if (!host) {
      return portalError(
        "WebVNC unavailable",
        "That dedicated host is not visible or the provider is not configured.",
        404,
      );
    }
    if (!lease) {
      return portalError(
        "WebVNC unavailable",
        "No active Crabbox lease is attached to that dedicated host. Start a host-pinned desktop lease from the CLI, then open WebVNC for the new lease.",
        409,
      );
    }
    return await this.state.runExclusive(async () => {
      const current = await this.getLease(lease.id);
      if (!current || current.state !== "active" || leaseHostID(current) !== hostID) {
        return portalError(
          "WebVNC unavailable",
          "No active Crabbox lease is attached to that dedicated host. Start a host-pinned desktop lease from the CLI, then open WebVNC for the new lease.",
          409,
        );
      }
      if (!this.leaseManageableByRequest(current, request, isAdminRequest(request))) {
        return portalError("WebVNC unavailable", "Lease manage access is required.", 403);
      }
      if (current.target !== "macos") {
        return portalError(
          "WebVNC unavailable",
          "Only macOS host leases can be enabled here.",
          409,
        );
      }
      const updated = { ...current, desktop: true, updatedAt: new Date().toISOString() };
      await this.putLease(updated);
      return new Response(null, {
        status: 303,
        headers: { location: `/portal/leases/${encodeURIComponent(updated.id)}/vnc` },
      });
    });
  }

  private async resolvePortalLease(
    identifier: string,
    request: Request,
  ): Promise<LeaseRecord | undefined> {
    return this.resolveLease(identifier, request, isAdminRequest(request));
  }

  private async portalLeasePage(request: Request, identifier: string): Promise<Response> {
    const lease = await this.resolvePortalLease(identifier, request);
    if (!lease) {
      return portalError(
        "Lease not found",
        "That lease is not active or is not visible to you.",
        404,
      );
    }
    const visibleRuns = await this.recentRuns(12, async (run) => {
      if (run.leaseIDs !== undefined && !this.runReferencesLease(run, lease.id)) {
        return false;
      }
      await this.ensureRunLeaseAttribution(run);
      return (
        this.runReferencesLease(run, lease.id) && this.runReadableToRequest(run, request, lease)
      );
    });
    return portalLeaseDetail(
      publicLeaseRecord(lease),
      visibleRuns.map(publicRunRecord),
      this.leaseBridgeStatus(lease),
      {
        canManage: this.leaseManageableByRequest(lease, request, isAdminRequest(request)),
      },
    );
  }

  private leaseBridgeStatus(lease: LeaseRecord): PortalLeaseBridgeStatus {
    const egress = this.egressSessions.get(lease.id);
    const egressKey = egress ? egressSocketKey(lease.id, egress.sessionID) : undefined;
    const bridgeStatus = {
      webVNCBridgeConnected: this.openWebVNCAgents(lease.id).length > 0,
      webVNCViewerConnected: this.openWebVNCViewers(lease.id).length > 0,
      codeBridgeConnected: this.codeAgents.get(lease.id)?.readyState === WebSocket.OPEN,
    };
    return egress
      ? {
          ...bridgeStatus,
          egress: {
            profile: egress.profile ?? "",
            allow: egress.allow,
            hostConnected: egressKey
              ? this.egressHosts.get(egressKey)?.readyState === WebSocket.OPEN
              : false,
            clientConnected: egressKey
              ? this.egressClients.get(egressKey)?.readyState === WebSocket.OPEN
              : false,
            updatedAt: egress.updatedAt,
          },
        }
      : bridgeStatus;
  }

  private async portalReleaseLease(request: Request, identifier: string): Promise<Response> {
    const lease = await this.resolvePortalLease(identifier, request);
    if (!lease) {
      return portalError(
        "Lease not found",
        "That lease is not active or is not visible to you.",
        404,
      );
    }
    if (!this.leaseManageableByRequest(lease, request, isAdminRequest(request))) {
      return portalError("Stop unavailable", "Lease manage access is required.", 403);
    }
    if (isRegisteredLease(lease) && lease.runtimeAdapterID && lease.runtimeAdapterWorkspaceID) {
      const response = await this.deleteRegisteredRuntimeAdapterWorkspace(request, lease);
      if (response.ok) {
        return new Response(null, {
          status: 303,
          headers: { location: portalReturnLocation(request) },
        });
      }
      const body = (await response.json().catch(() => undefined)) as
        | { title?: unknown; message?: unknown }
        | undefined;
      return portalError(
        typeof body?.title === "string" ? body.title : "Delete unavailable",
        typeof body?.message === "string"
          ? body.message
          : "The runtime adapter could not delete this workspace.",
        response.status,
      );
    }
    await this.releaseResolvedLease(lease, { deleteServer: true, keep: false });
    return new Response(null, {
      status: 303,
      headers: { location: portalReturnLocation(request) },
    });
  }

  private async deleteRegisteredRuntimeAdapterWorkspace(
    request: Request,
    lease: LeaseRecord,
  ): Promise<Response> {
    const adapterID = lease.runtimeAdapterID;
    const workspaceID = lease.runtimeAdapterWorkspaceID;
    if (!adapterID || !workspaceID || !isRegisteredLease(lease)) {
      return runtimeAdapterWorkspaceDeleteError(
        "runtime_adapter_binding_required",
        "Delete unavailable",
        "That lease is not bound to a runtime adapter workspace.",
        409,
      );
    }
    return await this.serializeRuntimeAdapterDelete(lease.id, async () => {
      const current = await this.state.runExclusive(() => this.getLease(lease.id));
      if (!current || !leaseIsLive(current)) {
        return runtimeAdapterWorkspaceDeleteError(
          "runtime_adapter_lease_inactive",
          "Delete unavailable",
          "That lease is no longer active.",
          409,
        );
      }
      if (!this.leaseManageableByRequest(current, request, isAdminRequest(request))) {
        return runtimeAdapterWorkspaceDeleteError(
          "forbidden",
          "Delete unavailable",
          "Lease manage access is required.",
          403,
        );
      }
      if (
        current.runtimeAdapterID !== adapterID ||
        current.runtimeAdapterWorkspaceID !== workspaceID ||
        current.runtimeAdapterRegistrationID !== lease.runtimeAdapterRegistrationID
      ) {
        return runtimeAdapterWorkspaceDeleteError(
          "runtime_adapter_binding_changed",
          "Delete unavailable",
          "The runtime adapter binding changed.",
          409,
        );
      }
      const deleteClaim = await this.markRuntimeAdapterDeletePending(
        current,
        runtimeAdapterDeleteInitialRetryMs,
      );
      if (!deleteClaim) {
        return runtimeAdapterWorkspaceDeleteError(
          "runtime_adapter_lease_inactive",
          "Delete unavailable",
          "That lease is no longer active.",
          409,
        );
      }
      const dispatch = await this.beginRuntimeAdapterDeleteDispatch(current, deleteClaim);
      if (!dispatch) {
        return runtimeAdapterWorkspaceDeleteError(
          "runtime_adapter_delete_in_flight",
          "Delete unavailable",
          "Another delete for this workspace is still settling. Try again shortly.",
          503,
        );
      }
      const proxyResult = await (async () => {
        let result: RuntimeAdapterProxyResult | undefined;
        try {
          result = await this.runtimeAdapterProxyResult(
            new Request(request.url, { method: "DELETE", headers: request.headers }),
            adapterID,
            ["v1", "workspaces", workspaceID],
            { owner: current.owner, org: current.org },
            dispatch.deadlineMs,
          );
          return result;
        } finally {
          await this.finishRuntimeAdapterDeleteDispatch(
            dispatch,
            result !== undefined && (await runtimeAdapterDeleteDispatchSafeToClear(result)),
          );
        }
      })();
      const response = proxyResult.response;
      let body: { id?: unknown; status?: unknown; message?: unknown; error?: unknown } | undefined;
      if (response.status !== 204) {
        body = (await response
          .clone()
          .json()
          .catch(() => undefined)) as
          | { status?: unknown; message?: unknown; error?: unknown }
          | undefined;
      }
      if (!response.ok) {
        const relayRetryable =
          proxyResult.origin === "relay" &&
          ["runtime_adapter_busy", "runtime_adapter_backpressure"].includes(
            runtimeAdapterErrorCode(body) ?? "",
          );
        if (deleteClaim.created && !proxyResult.dispatched && !relayRetryable) {
          await this.clearRuntimeAdapterDeletePending(current, deleteClaim);
        }
        const unavailable =
          response.status === 429 || response.status === 503 || response.status === 504;
        return runtimeAdapterWorkspaceDeleteError(
          unavailable ? "runtime_adapter_unavailable" : "runtime_adapter_delete_rejected",
          unavailable ? "Delete unavailable" : "Delete failed",
          unavailable
            ? "The runtime adapter is offline or did not respond. Try again after its lifecycle agent reconnects."
            : "The runtime adapter rejected the delete request. The workspace is still registered.",
          unavailable ? 503 : 502,
        );
      }
      if (
        response.ok &&
        response.status !== 204 &&
        (body?.id !== workspaceID ||
          !["stopping", "stopped", "expired"].includes(String(body?.status)))
      ) {
        return runtimeAdapterWorkspaceDeleteError(
          "runtime_adapter_invalid_response",
          "Delete failed",
          "The runtime adapter returned an invalid workspace confirmation. The workspace is still registered.",
          502,
        );
      }
      const applied = await this.scheduleRuntimeAdapterDeleteRetry(
        current,
        deleteClaim,
        runtimeAdapterDeleteRetryBaseMs,
      );
      if (!applied) {
        const latest = await this.getLease(lease.id);
        if (
          latest &&
          !leaseIsLive(latest) &&
          !latest.runtimeAdapterDeleteRequestedAt &&
          latest.runtimeAdapterID === adapterID &&
          latest.runtimeAdapterWorkspaceID === workspaceID &&
          latest.runtimeAdapterRegistrationID === lease.runtimeAdapterRegistrationID
        ) {
          return json({
            lease: this.leaseForRequest(latest, request, isAdminRequest(request)),
            status: "deleted",
          });
        }
        return runtimeAdapterWorkspaceDeleteError(
          "runtime_adapter_lease_changed",
          "Delete unavailable",
          "The lease changed while the runtime adapter was deleting its workspace.",
          409,
        );
      }
      return json(
        {
          lease: this.leaseForRequest(
            (await this.getLease(lease.id)) ?? current,
            request,
            isAdminRequest(request),
          ),
          status: "deleting",
        },
        { status: 202 },
      );
    });
  }

  private async serializeRuntimeAdapterDelete<T>(
    leaseID: string,
    operation: () => Promise<T>,
  ): Promise<T> {
    const previous = this.runtimeAdapterDeleteQueues.get(leaseID) ?? Promise.resolve();
    let release!: () => void;
    const turn = new Promise<void>((resolve) => {
      release = resolve;
    });
    const tail = previous.then(() => turn);
    this.runtimeAdapterDeleteQueues.set(leaseID, tail);
    await previous;
    try {
      return await operation();
    } finally {
      release();
      if (this.runtimeAdapterDeleteQueues.get(leaseID) === tail) {
        this.runtimeAdapterDeleteQueues.delete(leaseID);
      }
    }
  }

  private async markRuntimeAdapterDeletePending(
    lease: LeaseRecord,
    retryDelay: number,
  ): Promise<RuntimeAdapterDeleteClaim | undefined> {
    return await this.state.runExclusive(async () => {
      const current = await this.getLease(lease.id);
      if (
        !current ||
        !leaseIsLive(current) ||
        !isRegisteredLease(current) ||
        current.runtimeAdapterID !== lease.runtimeAdapterID ||
        current.runtimeAdapterWorkspaceID !== lease.runtimeAdapterWorkspaceID ||
        current.runtimeAdapterRegistrationID !== lease.runtimeAdapterRegistrationID
      ) {
        return undefined;
      }
      const now = new Date();
      const created = !current.runtimeAdapterDeleteRequestedAt;
      const claimID = crypto.randomUUID();
      current.runtimeAdapterDeleteRequestedAt ??= now.toISOString();
      current.runtimeAdapterDeleteClaimID = claimID;
      current.runtimeAdapterDeleteRetryAt = new Date(now.getTime() + retryDelay).toISOString();
      if (created) {
        current.runtimeAdapterDeleteAttempts = 0;
        delete current.runtimeAdapterDeleteError;
      }
      current.updatedAt = now.toISOString();
      await this.putLease(current);
      await this.scheduleAlarm();
      return { requestedAt: current.runtimeAdapterDeleteRequestedAt, claimID, created };
    });
  }

  private async beginRuntimeAdapterDeleteDispatch(
    lease: LeaseRecord,
    version: RuntimeAdapterDeleteVersion,
  ): Promise<RuntimeAdapterDeleteDispatch | undefined> {
    return await this.state.runExclusive(async () => {
      const current = await this.getLease(lease.id);
      if (!current || !runtimeAdapterDeleteVersionMatches(current, lease, version)) {
        return undefined;
      }
      const now = Date.now();
      const existingFence = Date.parse(current.runtimeAdapterDeleteDispatchUntil ?? "");
      if (Number.isFinite(existingFence) && existingFence > now) {
        return undefined;
      }
      const deadlineMs = now + runtimeAdapterRelayTimeoutMs;
      const fenceUntil = new Date(deadlineMs + runtimeAdapterDeleteDispatchGraceMs).toISOString();
      current.runtimeAdapterDeleteDispatchUntil = fenceUntil;
      const retryAt = Date.parse(current.runtimeAdapterDeleteRetryAt ?? "");
      if (!Number.isFinite(retryAt) || retryAt < Date.parse(fenceUntil)) {
        current.runtimeAdapterDeleteRetryAt = fenceUntil;
      }
      current.updatedAt = new Date(now).toISOString();
      await this.putLease(current);
      await this.scheduleAlarm();
      return {
        lease: structuredClone(current),
        version,
        deadlineMs,
        fenceUntil,
      };
    });
  }

  private async finishRuntimeAdapterDeleteDispatch(
    dispatch: RuntimeAdapterDeleteDispatch,
    safeToClear: boolean,
  ): Promise<void> {
    if (!safeToClear) {
      return;
    }
    await this.state.runExclusive(async () => {
      const current = await this.getLease(dispatch.lease.id);
      if (
        !current ||
        current.runtimeAdapterDeleteDispatchUntil !== dispatch.fenceUntil ||
        !runtimeAdapterDeleteVersionMatches(current, dispatch.lease, dispatch.version)
      ) {
        return;
      }
      delete current.runtimeAdapterDeleteDispatchUntil;
      current.updatedAt = new Date().toISOString();
      await this.putLease(current);
      await this.scheduleAlarm();
    });
  }

  private async clearRuntimeAdapterDeletePending(
    lease: LeaseRecord,
    claim: RuntimeAdapterDeleteVersion,
  ): Promise<void> {
    await this.state.runExclusive(async () => {
      const current = await this.getLease(lease.id);
      if (!current || !runtimeAdapterDeleteVersionMatches(current, lease, claim)) {
        return;
      }
      clearRuntimeAdapterDeleteMetadata(current);
      current.updatedAt = new Date().toISOString();
      await this.putLease(current);
      await this.scheduleAlarm();
    });
  }

  private async finalizeRuntimeAdapterDeleteCompletion(
    lease: LeaseRecord,
    completion: RuntimeAdapterDeleteCompletion,
  ): Promise<RuntimeAdapterDeleteFinalization> {
    const result = await this.state.runExclusive(
      async (): Promise<RuntimeAdapterDeleteFinalization> => {
        const current = await this.getLease(lease.id);
        if (
          !current ||
          !isRegisteredLease(current) ||
          current.owner !== lease.owner ||
          current.org !== lease.org ||
          current.runtimeAdapterID !== completion.adapterID ||
          current.runtimeAdapterWorkspaceID !== completion.workspaceID ||
          current.runtimeAdapterRegistrationID !== completion.registrationID
        ) {
          return { status: "mismatch" };
        }
        const dispatchUntil = Date.parse(current.runtimeAdapterDeleteDispatchUntil ?? "");
        if (Number.isFinite(dispatchUntil) && dispatchUntil > Date.now()) {
          return {
            status: "in-flight",
            retryAt: current.runtimeAdapterDeleteDispatchUntil!,
          };
        }
        if (!leaseIsLive(current) && !current.runtimeAdapterDeleteRequestedAt) {
          return { status: "completed", lease: current };
        }
        const finalized = current.runtimeAdapterDeleteRequestedAt
          ? finalizedRuntimeAdapterDeleteLease(current)
          : finalizedReleasedLease(current, false);
        await this.putLease(finalized);
        await this.clearWorkspaceReleaseError(finalized);
        await this.markAWSIngressReconcilePending(finalized);
        await this.scheduleAlarm();
        return { status: "completed", lease: finalized };
      },
    );
    if (result.status === "completed") {
      this.closeLeaseBridges(lease.id, 1008, "lease ended");
    }
    return result;
  }

  private async finalizeLegacyRuntimeAdapterDelete(
    lease: LeaseRecord,
    completion: RuntimeAdapterLegacyDeleteCompletion,
  ): Promise<RuntimeAdapterDeleteFinalization> {
    const result = await this.state.runExclusive(
      async (): Promise<RuntimeAdapterDeleteFinalization> => {
        const current = await this.getLease(lease.id);
        if (
          !current ||
          !isRegisteredLease(current) ||
          current.runtimeAdapterRegistrationID ||
          current.owner !== lease.owner ||
          current.org !== lease.org ||
          current.runtimeAdapterID !== completion.adapterID ||
          current.runtimeAdapterWorkspaceID !== completion.workspaceID
        ) {
          return { status: "mismatch" };
        }
        const dispatchUntil = Date.parse(current.runtimeAdapterDeleteDispatchUntil ?? "");
        if (Number.isFinite(dispatchUntil) && dispatchUntil > Date.now()) {
          return {
            status: "in-flight",
            retryAt: current.runtimeAdapterDeleteDispatchUntil!,
          };
        }
        if (!leaseIsLive(current) && !current.runtimeAdapterDeleteRequestedAt) {
          return { status: "completed", lease: current };
        }
        const finalized = current.runtimeAdapterDeleteRequestedAt
          ? finalizedRuntimeAdapterDeleteLease(current)
          : finalizedReleasedLease(current, false);
        await this.putLease(finalized);
        await this.clearWorkspaceReleaseError(finalized);
        await this.markAWSIngressReconcilePending(finalized);
        await this.scheduleAlarm();
        return { status: "completed", lease: finalized };
      },
    );
    if (result.status === "completed") {
      this.closeLeaseBridges(lease.id, 1008, "lease ended");
    }
    return result;
  }

  private async scheduleRuntimeAdapterDeleteRetry(
    lease: LeaseRecord,
    claim: RuntimeAdapterDeleteVersion,
    retryDelay: number,
  ): Promise<boolean> {
    return await this.state.runExclusive(async () => {
      const current = await this.getLease(lease.id);
      if (!current || !runtimeAdapterDeleteVersionMatches(current, lease, claim)) {
        return false;
      }
      const now = new Date();
      const dispatchUntil = Date.parse(current.runtimeAdapterDeleteDispatchUntil ?? "");
      current.runtimeAdapterDeleteRetryAt = new Date(
        Math.max(now.getTime() + retryDelay, Number.isFinite(dispatchUntil) ? dispatchUntil : 0),
      ).toISOString();
      delete current.runtimeAdapterDeleteError;
      current.updatedAt = now.toISOString();
      await this.putLease(current);
      await this.scheduleAlarm();
      return true;
    });
  }

  private async portalShareLeasePage(request: Request, identifier: string): Promise<Response> {
    const lease = await this.resolvePortalLease(identifier, request);
    if (!lease) {
      return portalError(
        "Lease not found",
        "That lease is not active or is not visible to you.",
        404,
      );
    }
    if (!this.leaseManageableByRequest(lease, request, isAdminRequest(request))) {
      return portalError("Share unavailable", "Lease manage access is required.", 403);
    }
    const url = new URL(request.url);
    if (url.searchParams.get("format") === "json") {
      return json({
        leaseID: lease.id,
        slug: lease.slug || lease.id,
        owner: lease.owner,
        org: orgLabelForDisplay(lease.org),
        share: normalizedLeaseShare(lease.share),
      });
    }
    const embedded = url.searchParams.get("embed") === "1";
    return portalShareLease(publicLeaseRecord(lease), { embedded });
  }

  private async portalShareLeaseAction(request: Request, identifier: string): Promise<Response> {
    const lease = await this.resolvePortalLease(identifier, request);
    if (!lease) {
      return portalError(
        "Lease not found",
        "That lease is not active or is not visible to you.",
        404,
      );
    }
    if (!this.leaseManageableByRequest(lease, request, isAdminRequest(request))) {
      return portalError("Share unavailable", "Lease manage access is required.", 403);
    }
    const url = new URL(request.url);
    if (request.headers.get("content-type")?.includes("application/json")) {
      const input = await readJson<Partial<LeaseShare>>(request);
      const previousShare = normalizedLeaseShare(lease.share);
      lease.share = sanitizeLeaseShare(input, requestOwner(request));
      lease.updatedAt = new Date().toISOString();
      await this.putLeaseShareAndRevokeBridges(lease, previousShare);
      return json({
        leaseID: lease.id,
        slug: lease.slug || lease.id,
        owner: lease.owner,
        org: orgLabelForDisplay(lease.org),
        share: normalizedLeaseShare(lease.share),
      });
    }
    const form = await request.formData();
    const action = String(form.get("action") || "");
    const previousShare = normalizedLeaseShare(lease.share);
    const share = normalizedLeaseShare(lease.share);
    if (action === "add-user") {
      const user = normalizeShareUser(String(form.get("user") || ""));
      const role = sanitizeShareRole(String(form.get("role") || "")) || "use";
      if (user) {
        share.users[user] = role;
      }
    } else if (action === "remove-user") {
      const user = normalizeShareUser(String(form.get("user") || ""));
      if (user) {
        delete share.users[user];
      }
    } else if (action === "set-org") {
      const role = sanitizeShareRole(String(form.get("role") || ""));
      if (role) {
        share.org = role;
      } else {
        delete share.org;
      }
    } else if (action === "clear") {
      delete share.org;
      share.users = {};
    }
    lease.share = sanitizeLeaseShare(share, requestOwner(request));
    lease.updatedAt = new Date().toISOString();
    await this.putLeaseShareAndRevokeBridges(lease, previousShare);
    const embedded = url.searchParams.get("embed") === "1";
    return new Response(null, {
      status: 303,
      headers: {
        location: `/portal/leases/${encodeURIComponent(lease.id)}/share${embedded ? "?embed=1" : ""}`,
      },
    });
  }

  private async portalRunRoute(
    request: Request,
    runID: string,
    action?: string,
  ): Promise<Response> {
    const run = await this.getRun(runID);
    const lease = run ? await this.ensureRunLeaseAttribution(run) : undefined;
    if (!run || !this.runReadableToRequest(run, request, lease)) {
      return notFound();
    }
    if (request.method.toUpperCase() !== "GET") {
      return json({ error: "not_found" }, { status: 404 });
    }
    if (action === "logs") {
      const log = await this.readRunLog(runID);
      return new Response(log, {
        headers: { "content-type": "text/plain; charset=utf-8" },
      });
    }
    if (action === "events") {
      const url = new URL(request.url);
      const after = finiteQueryNumber(url.searchParams.get("after")) ?? 0;
      const limit = clampLimit(url.searchParams.get("limit"), 500);
      return json({ events: await this.runEvents(runID, after, limit) });
    }
    if (action === undefined) {
      const [events, log] = await Promise.all([
        this.runEvents(runID, 0, 100),
        this.readRunLog(runID),
      ]);
      return portalRunDetail(publicRunRecord(run), events, tailString(log, 12 * 1024));
    }
    return json({ error: "not_found" }, { status: 404 });
  }

  private async webVNCAgent(request: Request, identifier: string): Promise<Response> {
    if (request.headers.get("upgrade")?.toLowerCase() !== "websocket") {
      return json(
        { error: "upgrade_required", message: "WebVNC agent requires a websocket upgrade" },
        { status: 426 },
      );
    }
    return await this.withBridgeTicketLock(async () => {
      const consumed = await this.consumeWebVNCTicketUnderLock(request, identifier);
      if (consumed.status === "invalid") {
        return json(
          { error: "webvnc_ticket_required", message: "valid WebVNC bridge ticket required" },
          { status: 401 },
        );
      }
      if (consumed.status === "not_found") {
        return notFound();
      }
      const { lease, ticket } = consumed;
      const error = webVNCLeaseError(lease);
      if (error) {
        return json({ error: "webvnc_unavailable", message: error }, { status: 409 });
      }
      const upgrade = this.state.createWebSocketUpgrade();
      const agent = upgrade.socket;

      const agentID = newWebVNCSessionID("agent");
      const capabilities = webVNCAgentCapabilities(request);
      this.trackWebVNCAgent(lease.id, agentID, agent, capabilities);
      this.recordWebVNCEvent(lease.id, "bridge_connected");
      this.acceptBridgeWebSocket(agent, {
        ...leaseBridgeTicketPrincipal(ticket),
        kind: "webvnc-agent",
        leaseID: lease.id,
        id: agentID,
        capabilities,
      });
      return upgrade.response;
    });
  }

  private async createEgressTicket(request: Request, identifier: string): Promise<Response> {
    if (request.method.toUpperCase() !== "POST") {
      return json({ error: "not_found" }, { status: 404 });
    }
    const admin = isAdminRequest(request);
    const lease = await this.resolveLease(identifier, request, admin);
    if (!lease) {
      return notFound();
    }
    if (!this.leaseManageableByRequest(lease, request, admin)) {
      return json({ error: "forbidden", message: "lease manage access required" }, { status: 403 });
    }
    if (lease.state !== "active") {
      return json({ error: "egress_unavailable", message: "lease is not active" }, { status: 409 });
    }
    const input = await optionalJson<{
      role?: string;
      sessionID?: string;
      sessionId?: string;
      profile?: string;
      allow?: string[];
    }>(request);
    const role = input.role === "host" || input.role === "client" ? input.role : undefined;
    if (!role) {
      return json(
        { error: "invalid_egress_role", message: "egress ticket role must be host or client" },
        { status: 400 },
      );
    }
    const bridgeGrant = await bridgeGrantForRequest(request, admin, this.env.CRABBOX_SHARED_TOKEN);
    if (!bridgeGrant) {
      return json(
        { error: "user_session_invalid", message: "GitHub user session cannot be revalidated" },
        { status: 401 },
      );
    }
    await this.cleanupExpiredEgressTickets();
    const now = new Date();
    const requestedSessionID = input.sessionID ?? input.sessionId;
    const sessionID = validEgressSessionID(requestedSessionID)
      ? requestedSessionID
      : newEgressSessionID();
    const ticket: EgressTicketRecord = {
      ticket: newEgressTicket(),
      leaseID: lease.id,
      owner: requestOwner(request),
      org: requestOrg(request, this.env),
      admin,
      ...bridgeGrant,
      role,
      sessionID,
      allow: boundedEgressAllowlist(input.allow),
      createdAt: now.toISOString(),
      expiresAt: new Date(now.getTime() + egressTicketTTLSeconds * 1000).toISOString(),
    };
    const profile = boundedEgressString(input.profile);
    if (profile) {
      ticket.profile = profile;
    }
    await this.state.storage.put(egressTicketKey(ticket.ticket), ticket);
    this.activateEgressSession(lease.id, ticket.sessionID, profile, ticket.allow ?? [], now);
    return json({
      ticket: ticket.ticket,
      leaseID: ticket.leaseID,
      role: ticket.role,
      sessionID: ticket.sessionID,
      expiresAt: ticket.expiresAt,
    });
  }

  private async egressAgent(
    request: Request,
    identifier: string,
    role: EgressRole,
  ): Promise<Response> {
    if (request.headers.get("upgrade")?.toLowerCase() !== "websocket") {
      return json(
        { error: "upgrade_required", message: "egress bridge requires a websocket upgrade" },
        { status: 426 },
      );
    }
    return await this.withBridgeTicketLock(async () => {
      const consumed = await this.consumeEgressTicketUnderLock(request, identifier, role);
      if (consumed.status === "invalid") {
        return json(
          { error: "egress_ticket_required", message: "valid egress bridge ticket required" },
          { status: 401 },
        );
      }
      if (consumed.status === "not_found") {
        return notFound();
      }
      const { lease, ticket } = consumed;
      if (lease.state !== "active") {
        return json(
          { error: "egress_unavailable", message: "lease is not active" },
          { status: 409 },
        );
      }
      const upgrade = this.state.createWebSocketUpgrade();
      const agent = upgrade.socket;
      const principal = leaseBridgeTicketPrincipal(ticket);
      const attachment: BridgeAttachment = {
        kind: role === "host" ? "egress-host" : "egress-client",
        leaseID: lease.id,
        sessionID: ticket.sessionID,
        ...principal,
      };
      const ticketCreatedAt = new Date(ticket.createdAt);
      this.activateEgressSession(
        lease.id,
        ticket.sessionID,
        ticket.profile,
        ticket.allow ?? [],
        ticketCreatedAt,
      );
      const key = egressSocketKey(lease.id, ticket.sessionID);
      if (role === "host") {
        closeSocket(this.egressHosts.get(key), 1012, "replaced by a newer egress host");
        this.egressHosts.set(key, agent);
      } else {
        closeSocket(this.egressClients.get(key), 1012, "replaced by a newer egress client");
        this.egressClients.set(key, agent);
      }
      this.acceptBridgeWebSocket(agent, attachment);
      return upgrade.response;
    });
  }

  private async egressStatus(request: Request, identifier: string): Promise<Response> {
    const lease = await this.resolveLease(identifier, request, false);
    if (!lease) {
      return notFound();
    }
    const canManage = this.leaseManageableByRequest(lease, request, isAdminRequest(request));
    const session = this.egressSessions.get(lease.id);
    const key = session ? egressSocketKey(lease.id, session.sessionID) : undefined;
    const host = key ? this.egressHosts.get(key) : undefined;
    const client = key ? this.egressClients.get(key) : undefined;
    return json({
      leaseID: lease.id,
      slug: lease.slug,
      active: session !== undefined,
      sessionID: canManage ? (session?.sessionID ?? "") : "",
      profile: canManage ? (session?.profile ?? "") : "",
      allow: canManage ? (session?.allow ?? []) : [],
      ...(canManage
        ? {
            hostConnected: host?.readyState === WebSocket.OPEN,
            clientConnected: client?.readyState === WebSocket.OPEN,
          }
        : {}),
      createdAt: canManage ? (session?.createdAt ?? "") : "",
      updatedAt: canManage ? (session?.updatedAt ?? "") : "",
    });
  }

  private async reconcileRuntimeAdapterDeletes(): Promise<void> {
    const pending = await this.state.runExclusive(async () => {
      const now = Date.now();
      const due: LeaseRecord[] = [];
      await this.visitLeaseRecords((lease) => {
        if (
          due.length < runtimeAdapterDeleteBatchSize &&
          (leaseIsLive(lease) || lease.state === "expired") &&
          isRegisteredLease(lease) &&
          Boolean(lease.runtimeAdapterID && lease.runtimeAdapterWorkspaceID) &&
          lease.runtimeAdapterDeleteRequestedAt &&
          (!Number.isFinite(Date.parse(lease.runtimeAdapterDeleteRetryAt ?? "")) ||
            Date.parse(lease.runtimeAdapterDeleteRetryAt ?? "") <= now) &&
          (!Number.isFinite(Date.parse(lease.runtimeAdapterDeleteDispatchUntil ?? "")) ||
            Date.parse(lease.runtimeAdapterDeleteDispatchUntil ?? "") <= now)
        ) {
          due.push(structuredClone(lease));
          if (due.length >= runtimeAdapterDeleteBatchSize) {
            return false;
          }
        }
        return true;
      });
      return due;
    });
    await Promise.all(pending.map((lease) => this.reconcileRuntimeAdapterDelete(lease)));
  }

  private async reconcileRuntimeAdapterDelete(lease: LeaseRecord): Promise<void> {
    await this.serializeRuntimeAdapterDelete(lease.id, async () => {
      const requestedAt = lease.runtimeAdapterDeleteRequestedAt;
      const adapterID = lease.runtimeAdapterID;
      const workspaceID = lease.runtimeAdapterWorkspaceID;
      if (!requestedAt || !adapterID || !workspaceID) {
        return;
      }
      const version: RuntimeAdapterDeleteVersion = {
        requestedAt,
        ...(lease.runtimeAdapterDeleteClaimID === undefined
          ? {}
          : { claimID: lease.runtimeAdapterDeleteClaimID }),
      };
      const current = await this.state.runExclusive(async () => {
        const latest = await this.getLease(lease.id);
        if (!latest || !runtimeAdapterDeleteVersionMatches(latest, lease, version)) {
          return undefined;
        }
        const now = Date.now();
        const retryAt = Date.parse(latest.runtimeAdapterDeleteRetryAt ?? "");
        const dispatchUntil = Date.parse(latest.runtimeAdapterDeleteDispatchUntil ?? "");
        if (
          (Number.isFinite(retryAt) && retryAt > now) ||
          (Number.isFinite(dispatchUntil) && dispatchUntil > now)
        ) {
          return undefined;
        }
        return structuredClone(latest);
      });
      if (!current) {
        return;
      }
      const dispatch = await this.beginRuntimeAdapterDeleteDispatch(current, version);
      if (!dispatch) {
        return;
      }
      const proxyResult = await (async () => {
        let result: RuntimeAdapterProxyResult | undefined;
        try {
          result = await this.runtimeAdapterProxyResult(
            new Request("https://coordinator.invalid/runtime-adapter-delete", {
              method: "DELETE",
            }),
            adapterID,
            ["v1", "workspaces", workspaceID],
            { owner: dispatch.lease.owner, org: dispatch.lease.org },
            dispatch.deadlineMs,
          );
          return result;
        } finally {
          await this.finishRuntimeAdapterDeleteDispatch(
            dispatch,
            result !== undefined && (await runtimeAdapterDeleteDispatchSafeToClear(result)),
          );
        }
      })();
      const response = proxyResult.response;
      let body: { id?: unknown; status?: unknown; message?: unknown; error?: unknown } | undefined;
      if (response.status !== 204) {
        body = (await response
          .clone()
          .json()
          .catch(() => undefined)) as
          | { id?: unknown; status?: unknown; message?: unknown; error?: unknown }
          | undefined;
      }
      const acknowledged =
        proxyResult.origin === "upstream" &&
        (response.status === 204 ||
          (response.ok &&
            body?.id === workspaceID &&
            ["stopping", "stopped", "expired"].includes(String(body.status))));
      await this.state.runExclusive(async () => {
        const latest = await this.getLease(lease.id);
        if (!latest || !runtimeAdapterDeleteVersionMatches(latest, dispatch.lease, version)) {
          return;
        }
        const attempts = (latest.runtimeAdapterDeleteAttempts ?? 0) + 1;
        const retryDelay = Math.min(
          runtimeAdapterDeleteRetryMaxMs,
          runtimeAdapterDeleteRetryBaseMs * 2 ** Math.min(attempts - 1, 4),
        );
        const dispatchUntil = Date.parse(latest.runtimeAdapterDeleteDispatchUntil ?? "");
        latest.runtimeAdapterDeleteAttempts = attempts;
        latest.runtimeAdapterDeleteRetryAt = new Date(
          Math.max(Date.now() + retryDelay, Number.isFinite(dispatchUntil) ? dispatchUntil : 0),
        ).toISOString();
        if (acknowledged) {
          delete latest.runtimeAdapterDeleteError;
        } else {
          latest.runtimeAdapterDeleteError =
            response.status === 503 || response.status === 504
              ? "runtime_adapter_unavailable"
              : response.ok
                ? "runtime_adapter_invalid_response"
                : `runtime_adapter_http_${response.status}`;
        }
        latest.updatedAt = new Date().toISOString();
        await this.putLease(latest);
      });
    });
  }

  private async createRuntimeAdapterTicket(request: Request, adapterID: string): Promise<Response> {
    if (request.method.toUpperCase() !== "POST") {
      return notFound();
    }
    if (!validRuntimeAdapterID(adapterID)) {
      return json({ error: "invalid_adapter_id" }, { status: 400 });
    }
    let rawInput: unknown;
    try {
      rawInput = await optionalJson<unknown>(request);
    } catch {
      return json({ error: "invalid_adapter_ticket_request" }, { status: 400 });
    }
    if (!rawInput || typeof rawInput !== "object" || Array.isArray(rawInput)) {
      return json({ error: "invalid_adapter_ticket_request" }, { status: 400 });
    }
    const input = rawInput as { desktopTimeoutMs?: unknown };
    if (
      input.desktopTimeoutMs !== undefined &&
      !validRuntimeAdapterDesktopRelayTimeout(input.desktopTimeoutMs)
    ) {
      return json(
        {
          error: "invalid_desktop_timeout",
          message: "desktop timeout is outside the supported relay range",
        },
        { status: 400 },
      );
    }
    const owner = requestOwner(request);
    const org = requestOrg(request, this.env);
    const identityKey = runtimeAdapterIdentityKey(adapterID);
    await this.cleanupExpiredRuntimeAdapterTickets();
    const now = new Date();
    let identity = await this.state.storage.get<RuntimeAdapterIdentityRecord>(identityKey);
    if (
      identity &&
      (identity.adapterID !== adapterID || identity.owner !== owner || identity.org !== org)
    ) {
      if (!(await this.runtimeAdapterIdentityReclaimable(identity, adapterID, now.getTime()))) {
        return json(
          {
            error: "adapter_id_conflict",
            message: "runtime adapter id belongs to another owner or organization",
          },
          { status: 409 },
        );
      }
      identity = undefined;
    }
    if (!identity) {
      identity = {
        adapterID,
        owner,
        org,
        createdAt: now.toISOString(),
        claimVersion: 1,
        claimState: "provisional",
        claimExpiresAt: new Date(
          now.getTime() + runtimeAdapterProvisionalClaimTTLSeconds * 1000,
        ).toISOString(),
      };
      await this.state.storage.put<RuntimeAdapterIdentityRecord>(identityKey, identity);
    } else if (
      identity.claimVersion === 1 &&
      identity.claimState === "provisional" &&
      !identity.confirmedAt
    ) {
      identity = {
        ...identity,
        claimExpiresAt: new Date(
          now.getTime() + runtimeAdapterProvisionalClaimTTLSeconds * 1000,
        ).toISOString(),
      };
      await this.state.storage.put<RuntimeAdapterIdentityRecord>(identityKey, identity);
    }
    const ticket: RuntimeAdapterTicketRecord = {
      ticket: newRuntimeAdapterTicket(),
      adapterID,
      owner,
      org,
      createdAt: now.toISOString(),
      expiresAt: new Date(now.getTime() + runtimeAdapterTicketTTLSeconds * 1000).toISOString(),
      ...(input.desktopTimeoutMs === undefined ? {} : { desktopTimeoutMs: input.desktopTimeoutMs }),
    };
    await this.state.storage.put(runtimeAdapterTicketKey(ticket.ticket), ticket);
    return json({
      ticket: ticket.ticket,
      adapterID: ticket.adapterID,
      expiresAt: ticket.expiresAt,
    });
  }

  private async runtimeAdapterAgent(request: Request, adapterID: string): Promise<Response> {
    if (request.method.toUpperCase() !== "GET") {
      return notFound();
    }
    if (request.headers.get("upgrade")?.toLowerCase() !== "websocket") {
      return json(
        { error: "upgrade_required", message: "runtime adapter agent requires websocket upgrade" },
        { status: 426 },
      );
    }
    const consumed = await this.consumeRuntimeAdapterTicket(request, adapterID);
    if (consumed.status === "invalid") {
      return json(
        { error: "adapter_ticket_required", message: "valid runtime adapter ticket required" },
        { status: 401 },
      );
    }
    const { ticket } = consumed;
    const identity = await this.state.storage.get<RuntimeAdapterIdentityRecord>(
      runtimeAdapterIdentityKey(adapterID),
    );
    if (
      !identity ||
      !isCurrentOrgKey(identity.org) ||
      identity.adapterID !== adapterID ||
      identity.owner !== ticket.owner ||
      identity.org !== ticket.org
    ) {
      return json(
        {
          error: "adapter_identity_mismatch",
          message: "runtime adapter ticket no longer matches the claimed adapter identity",
        },
        { status: 401 },
      );
    }
    await this.confirmRuntimeAdapterIdentity(identity, new Date().toISOString());
    const upgrade = this.state.createWebSocketUpgrade({
      maxPayload: runtimeAdapterRelayFrameLimit,
    });
    const agent = upgrade.socket;
    const previous = this.runtimeAdapterAgents.get(adapterID);
    if (previous) {
      this.clearRuntimeAdapterAgent(adapterID, previous);
      closeSocket(previous, 1012, "replaced by a newer runtime adapter agent");
    }
    this.runtimeAdapterAgents.set(adapterID, agent);
    this.acceptBridgeWebSocket(agent, {
      kind: "runtime-adapter-agent",
      adapterID,
      owner: ticket.owner,
      org: ticket.org,
      ...(ticket.desktopTimeoutMs === undefined
        ? {}
        : { desktopTimeoutMs: ticket.desktopTimeoutMs }),
    });
    return upgrade.response;
  }

  private async runtimeAdapterStatus(request: Request, adapterID: string): Promise<Response> {
    if (!validRuntimeAdapterID(adapterID)) {
      return notFound();
    }
    const identity = await this.state.storage.get<RuntimeAdapterIdentityRecord>(
      runtimeAdapterIdentityKey(adapterID),
    );
    const agent = this.runtimeAdapterAgents.get(adapterID);
    const attachment = agent ? this.bridgeAttachment(agent) : undefined;
    if (
      !identity ||
      !isCurrentOrgKey(identity.org) ||
      identity.adapterID !== adapterID ||
      !agent ||
      agent.readyState !== WebSocket.OPEN ||
      attachment?.kind !== "runtime-adapter-agent" ||
      attachment.owner !== identity.owner ||
      attachment.org !== identity.org ||
      (!isAdminRequest(request) &&
        (identity.owner !== requestOwner(request) ||
          identity.org !== requestOrg(request, this.env)))
    ) {
      return json({ adapterID, connected: false });
    }
    return json({ adapterID, connected: true });
  }

  private async runtimeAdapterProxy(
    request: Request,
    adapterID: string,
    proxyParts: string[],
  ): Promise<Response> {
    const path = runtimeAdapterProxyPath(proxyParts);
    const method = request.method.toUpperCase();
    if (path && method === "DELETE" && runtimeAdapterRelayMethodAllowed(method, path)) {
      return json(
        {
          error: "runtime_adapter_delete_requires_lease",
          message:
            "runtime adapter workspace deletes must use a registered lease release so the lifecycle generation can be fenced",
        },
        { status: 409 },
      );
    }
    return (await this.runtimeAdapterProxyResult(request, adapterID, proxyParts)).response;
  }

  private async runtimeAdapterProxyResult(
    request: Request,
    adapterID: string,
    proxyParts: string[],
    scope?: RuntimeAdapterProxyScope,
    relayDeadlineMs?: number,
  ): Promise<RuntimeAdapterProxyResult> {
    if (!validRuntimeAdapterID(adapterID)) {
      return { origin: "relay", dispatched: false, response: notFound() };
    }
    const path = runtimeAdapterProxyPath(proxyParts);
    const method = request.method.toUpperCase();
    if (!path || !runtimeAdapterRelayMethodAllowed(method, path)) {
      return { origin: "relay", dispatched: false, response: notFound() };
    }
    let relayHeaders: Record<string, string> | undefined;
    try {
      relayHeaders = runtimeAdapterRelayHeaders(request);
    } catch (error) {
      if (error instanceof RangeError) {
        return {
          origin: "relay",
          dispatched: false,
          response: json(
            { error: "idempotency_key_too_long", message: error.message },
            { status: 431 },
          ),
        };
      }
      throw error;
    }
    const identity = await this.state.storage.get<RuntimeAdapterIdentityRecord>(
      runtimeAdapterIdentityKey(adapterID),
    );
    if (!identity || identity.adapterID !== adapterID) {
      return {
        origin: "relay",
        dispatched: false,
        response: json(
          {
            error: "runtime_adapter_unclaimed",
            message: "runtime adapter id has not been claimed",
          },
          { status: 409 },
        ),
      };
    }
    const legacyIdentity = isLegacyOrgKey(identity.org);
    if (!isCurrentOrgKey(identity.org) && (method !== "DELETE" || !legacyIdentity)) {
      return {
        origin: "relay",
        dispatched: false,
        response: json(
          {
            error: "runtime_adapter_identity_legacy",
            message: "runtime adapter identity must be reclaimed before use",
          },
          { status: 409 },
        ),
      };
    }
    const identityMismatch = scope
      ? identity.owner !== scope.owner || identity.org !== scope.org
      : identity.owner !== requestOwner(request) || identity.org !== requestOrg(request, this.env);
    if (identityMismatch && (scope !== undefined || !isAdminRequest(request))) {
      return {
        origin: "relay",
        dispatched: false,
        response: json(
          {
            error: "runtime_adapter_forbidden",
            message: "runtime adapter belongs to another owner or organization",
          },
          { status: 403 },
        ),
      };
    }
    let body: string | undefined;
    try {
      body = await readRuntimeAdapterRelayBody(request);
    } catch (error) {
      if (error instanceof RangeError) {
        return {
          origin: "relay",
          dispatched: false,
          response: json({ error: "request_too_large", message: error.message }, { status: 413 }),
        };
      }
      if (error instanceof TypeError) {
        return {
          origin: "relay",
          dispatched: false,
          response: json(
            { error: "invalid_request_body", message: "runtime adapter body must be valid UTF-8" },
            { status: 400 },
          ),
        };
      }
      throw error;
    }
    if (!runtimeAdapterRelayBodyAllowed(method, path, body)) {
      return {
        origin: "relay",
        dispatched: false,
        response: json(
          {
            error: "invalid_request_body",
            message: "runtime adapter request body is allowed only for workspace creation",
          },
          { status: 400 },
        ),
      };
    }
    const agent = this.runtimeAdapterAgents.get(adapterID);
    const attachment = agent ? this.bridgeAttachment(agent) : undefined;
    if (
      !agent ||
      agent.readyState !== WebSocket.OPEN ||
      attachment?.kind !== "runtime-adapter-agent"
    ) {
      return {
        origin: "relay",
        dispatched: false,
        response: json(
          { error: "runtime_adapter_unavailable", message: "runtime adapter is not connected" },
          { status: 503 },
        ),
      };
    }
    if (attachment.owner !== identity.owner || attachment.org !== identity.org) {
      return {
        origin: "relay",
        dispatched: false,
        response: json(
          {
            error: "runtime_adapter_identity_mismatch",
            message: "connected runtime adapter does not match its claimed identity",
          },
          { status: 503 },
        ),
      };
    }
    if (this.runtimeAdapterRelayAtCapacity(adapterID, identity, method)) {
      return {
        origin: "relay",
        dispatched: false,
        response: json(
          {
            error: "runtime_adapter_busy",
            message: "runtime adapter relay has too many in-flight requests",
          },
          { status: 429, headers: { "retry-after": "1" } },
        ),
      };
    }
    const bufferedAmount = (agent as WebSocket & { readonly bufferedAmount?: number })
      .bufferedAmount;
    if (typeof bufferedAmount === "number" && bufferedAmount > runtimeAdapterMaxBufferedBytes) {
      return {
        origin: "relay",
        dispatched: false,
        response: json(
          {
            error: "runtime_adapter_backpressure",
            message: "runtime adapter relay transport is congested",
          },
          { status: 503, headers: { "retry-after": "1" } },
        ),
      };
    }
    const relayTimeoutMs = runtimeAdapterRelayTimeoutForPath(path, attachment.desktopTimeoutMs);
    const maximumDeadlineMs = Date.now() + relayTimeoutMs;
    const deadlineMs =
      relayDeadlineMs === undefined
        ? maximumDeadlineMs
        : Math.min(relayDeadlineMs, maximumDeadlineMs);
    if (!Number.isSafeInteger(deadlineMs) || deadlineMs <= Date.now()) {
      return {
        origin: "relay",
        dispatched: false,
        response: json(
          {
            error: "runtime_adapter_timeout",
            message: "runtime adapter request expired before dispatch",
          },
          { status: 504 },
        ),
      };
    }
    const id = crypto.randomUUID();
    const relayRequest: RuntimeAdapterRelayRequest = {
      type: "request",
      id,
      method: method as RuntimeAdapterRelayRequest["method"],
      path,
      deadlineMs,
      ...(relayHeaders ? { headers: relayHeaders } : {}),
      ...(body === undefined ? {} : { body }),
    };
    let relayResult: RuntimeAdapterRelayResult;
    try {
      relayResult = await new Promise<RuntimeAdapterRelayResult>((resolve) => {
        const timeout = setTimeout(
          () => {
            this.settleRuntimeAdapterPending(id, {
              origin: "relay",
              response: runtimeAdapterRelayError(
                id,
                504,
                "runtime_adapter_timeout",
                "runtime adapter did not respond in time",
              ),
            });
          },
          Math.max(0, deadlineMs - Date.now()),
        );
        const abortHandler = () => {
          this.cancelRuntimeAdapterPending(id);
        };
        const pending: RuntimeAdapterPendingRequest = {
          adapterID,
          owner: identity.owner,
          org: identity.org,
          dispatched: false,
          clientSettled: false,
          resolve,
          timeout,
          signal: request.signal,
          abortHandler,
        };
        this.runtimeAdapterPending.set(id, pending);
        request.signal.addEventListener("abort", abortHandler, { once: true });
        if (request.signal.aborted) {
          abortHandler();
        } else {
          pending.dispatched = true;
          agent.send(JSON.stringify(relayRequest));
        }
      });
    } catch {
      this.takeRuntimeAdapterPending(id);
      return {
        origin: "relay",
        dispatched: false,
        response: json(
          { error: "runtime_adapter_unavailable", message: "runtime adapter disconnected" },
          { status: 503 },
        ),
      };
    }
    const response = relayResult.response;
    const headers = new Headers({ "cache-control": "no-store" });
    const contentType = runtimeAdapterRelayContentType(response.headers);
    headers.set(
      "content-type",
      contentType?.toLowerCase().startsWith("application/json")
        ? "application/json; charset=utf-8"
        : "text/plain; charset=utf-8",
    );
    const responseBody = [204, 205, 304].includes(response.status) ? null : (response.body ?? null);
    return {
      origin: relayResult.origin,
      dispatched: true,
      response: new Response(responseBody, {
        status: response.status,
        headers,
      }),
    };
  }

  private async handleRuntimeAdapterAgentMessage(
    adapterID: string,
    socket: WebSocket,
    rawData: unknown,
  ): Promise<void> {
    const messageBytes = runtimeAdapterRelayMessageBytes(rawData);
    if (messageBytes === undefined || messageBytes > runtimeAdapterRelayFrameLimit) {
      closeSocket(socket, 1009, "runtime adapter response too large");
      return;
    }
    let input: unknown;
    try {
      const raw = await normalizeWebVNCData(rawData);
      const text = typeof raw === "string" ? raw : fatalTextDecoder.decode(raw);
      input = JSON.parse(text);
    } catch {
      return;
    }
    const id =
      input && typeof input === "object" && !Array.isArray(input)
        ? (input as { id?: unknown }).id
        : undefined;
    if (typeof id !== "string") {
      return;
    }
    const pending = this.runtimeAdapterPending.get(id);
    if (!pending || pending.adapterID !== adapterID) {
      return;
    }
    if (!validRuntimeAdapterRelayResponse(input, id)) {
      return;
    }
    this.settleRuntimeAdapterPending(id, { origin: "upstream", response: input });
  }

  private runtimeAdapterRelayAtCapacity(
    adapterID: string,
    identity: RuntimeAdapterIdentityRecord,
    method: string,
  ): boolean {
    const isDelete = method === "DELETE";
    const globalLimit =
      runtimeAdapterMaxPendingGlobal + (isDelete ? runtimeAdapterReservedDeletesGlobal : 0);
    if (this.runtimeAdapterPending.size >= globalLimit) {
      return true;
    }
    let adapterPending = 0;
    let ownerPending = 0;
    for (const pending of this.runtimeAdapterPending.values()) {
      if (pending.adapterID === adapterID) adapterPending += 1;
      if (pending.owner === identity.owner) ownerPending += 1;
    }
    const adapterLimit =
      runtimeAdapterMaxPendingPerAdapter + (isDelete ? runtimeAdapterReservedDeletesPerAdapter : 0);
    const ownerLimit =
      runtimeAdapterMaxPendingPerOwner + (isDelete ? runtimeAdapterReservedDeletesPerOwner : 0);
    return adapterPending >= adapterLimit || ownerPending >= ownerLimit;
  }

  private takeRuntimeAdapterPending(id: string): RuntimeAdapterPendingRequest | undefined {
    const pending = this.runtimeAdapterPending.get(id);
    if (!pending) return undefined;
    this.runtimeAdapterPending.delete(id);
    clearTimeout(pending.timeout);
    pending.signal.removeEventListener("abort", pending.abortHandler);
    return pending;
  }

  private settleRuntimeAdapterPending(id: string, result: RuntimeAdapterRelayResult): boolean {
    const pending = this.takeRuntimeAdapterPending(id);
    if (!pending) return false;
    if (!pending.clientSettled) {
      pending.resolve(result);
    }
    return true;
  }

  private cancelRuntimeAdapterPending(id: string): void {
    const pending = this.runtimeAdapterPending.get(id);
    if (!pending || pending.clientSettled) return;
    const result: RuntimeAdapterRelayResult = {
      origin: "relay",
      response: runtimeAdapterRelayError(
        id,
        499,
        "client_closed_request",
        "runtime adapter request was cancelled",
      ),
    };
    if (!pending.dispatched) {
      this.settleRuntimeAdapterPending(id, result);
      return;
    }
    pending.clientSettled = true;
    pending.signal.removeEventListener("abort", pending.abortHandler);
    pending.resolve(result);
  }

  private clearRuntimeAdapterAgent(adapterID: string, socket: WebSocket): void {
    if (this.runtimeAdapterAgents.get(adapterID) !== socket) {
      return;
    }
    this.runtimeAdapterAgents.delete(adapterID);
    for (const [id, pending] of this.runtimeAdapterPending) {
      if (pending.adapterID !== adapterID) continue;
      this.settleRuntimeAdapterPending(id, {
        origin: "relay",
        response: runtimeAdapterRelayError(
          id,
          503,
          "runtime_adapter_unavailable",
          "runtime adapter disconnected",
        ),
      });
    }
  }

  private async createWebVNCTicket(request: Request, identifier: string): Promise<Response> {
    if (request.method.toUpperCase() !== "POST") {
      return json({ error: "not_found" }, { status: 404 });
    }
    const admin = isAdminRequest(request);
    const lease = await this.resolveLease(identifier, request, admin);
    if (!lease) {
      return notFound();
    }
    if (!this.leaseManageableByRequest(lease, request, admin)) {
      return json({ error: "forbidden", message: "lease manage access required" }, { status: 403 });
    }
    const error = webVNCLeaseError(lease);
    if (error) {
      return json({ error: "webvnc_unavailable", message: error }, { status: 409 });
    }
    const bridgeGrant = await bridgeGrantForRequest(request, admin, this.env.CRABBOX_SHARED_TOKEN);
    if (!bridgeGrant) {
      return json(
        { error: "user_session_invalid", message: "GitHub user session cannot be revalidated" },
        { status: 401 },
      );
    }
    await this.cleanupExpiredWebVNCTickets();
    const now = new Date();
    const ticket: WebVNCTicketRecord = {
      ticket: newWebVNCTicket(),
      leaseID: lease.id,
      owner: requestOwner(request),
      org: requestOrg(request, this.env),
      admin,
      ...bridgeGrant,
      createdAt: now.toISOString(),
      expiresAt: new Date(now.getTime() + webVNCTicketTTLSeconds * 1000).toISOString(),
    };
    await this.state.storage.put(webVNCTicketKey(ticket.ticket), ticket);
    return json({
      ticket: ticket.ticket,
      leaseID: ticket.leaseID,
      expiresAt: ticket.expiresAt,
    });
  }

  private async webVNCStatus(request: Request, identifier: string): Promise<Response> {
    const lease = request.url.includes("/portal/")
      ? await this.resolvePortalLease(identifier, request)
      : await this.resolveLease(identifier, request, false);
    if (!lease) {
      return notFound();
    }
    const error = webVNCLeaseError(lease);
    if (error) {
      return json({ error: "webvnc_unavailable", message: error }, { status: 409 });
    }
    const agents = this.openWebVNCAgents(lease.id);
    const viewers = this.openWebVNCViewers(lease.id);
    const availableAgents = agents.filter(
      ([agentID]) => !viewers.some((viewer) => viewer.agentID === agentID),
    );
    const bridgeConnected = agents.length > 0;
    const viewerConnected = viewers.length > 0;
    const url = new URL(request.url);
    const requestedViewerID = url.searchParams.get("viewer") || "";
    const viewerID = validWebVNCSessionID(requestedViewerID) ? requestedViewerID : "";
    const controllerID = this.activeWebVNCControllerID(lease.id);
    const currentViewer = viewerID ? this.webVNCViewers.get(lease.id)?.get(viewerID) : undefined;
    const controller = controllerID
      ? this.webVNCViewers.get(lease.id)?.get(controllerID)
      : undefined;
    const canManage = this.leaseManageableByRequest(lease, request, isAdminRequest(request));
    const command = canManage ? webVNCBridgeCommand(lease) : "";
    return json({
      leaseID: lease.id,
      slug: lease.slug ?? "",
      bridgeConnected,
      viewerConnected,
      viewerCount: viewers.length,
      observerCount: Math.max(0, viewers.length - (controller ? 1 : 0)),
      availableViewerSlots: availableAgents.length,
      viewerID,
      viewerRole: currentViewer
        ? currentViewer.id === controllerID
          ? "controller"
          : "observer"
        : "none",
      controllerID: controller?.id ?? "",
      controllerLabel: controller?.label ?? "",
      command,
      events: this.recentWebVNCEvents(lease.id),
      message: bridgeConnected
        ? currentViewer
          ? currentViewer.id === controllerID
            ? "you are controlling"
            : `${controller?.label || "another viewer"} is controlling`
          : availableAgents.length > 0
            ? viewerConnected
              ? "observer slots available"
              : "bridge connected"
            : "waiting for an available WebVNC observer slot"
        : canManage
          ? `WebVNC daemon not running; run: ${command}`
          : "WebVNC daemon not running; ask a lease manager to start or refresh the bridge",
    });
  }

  private async webVNCCredentialHandoff(request: Request, identifier: string): Promise<Response> {
    if (request.method.toUpperCase() !== "POST") {
      return json({ error: "not_found" }, { status: 404 });
    }
    const portalRequest = new URL(request.url).pathname.startsWith("/portal/");
    const admin = isAdminRequest(request);
    const lease = portalRequest
      ? await this.resolvePortalLease(identifier, request)
      : await this.resolveLease(identifier, request, admin);
    if (!lease) {
      return notFound();
    }
    const error = webVNCLeaseError(lease);
    if (error) {
      return json({ error: "webvnc_unavailable", message: error }, { status: 409 });
    }
    const body = (await request.json().catch(() => undefined)) as
      | { ticket?: unknown; username?: unknown; password?: unknown }
      | undefined;
    if (typeof body?.ticket === "string") {
      if (!portalRequest) {
        return json(
          { error: "invalid_handoff", message: "portal handoff required" },
          { status: 400 },
        );
      }
      const result = await this.webVNCCredentialHandoffs.consume(lease.id, body.ticket);
      if (result.status !== "accepted") {
        const expired = result.status === "expired";
        return json(
          {
            error: expired ? "expired_handoff" : "invalid_handoff",
            message: expired ? "VNC handoff expired" : "valid VNC handoff required",
          },
          { status: 401 },
        );
      }
      return json(
        { username: result.username, password: result.password },
        { headers: { "cache-control": "no-store" } },
      );
    }
    if (!this.leaseManageableByRequest(lease, request, admin)) {
      return json({ error: "forbidden", message: "lease manage access required" }, { status: 403 });
    }
    const username = typeof body?.username === "string" ? body.username : "";
    const password = typeof body?.password === "string" ? body.password : "";
    if ((!username && !password) || username.length > 256 || password.length > 1024) {
      return json(
        { error: "invalid_credentials", message: "valid VNC credentials required" },
        { status: 400 },
      );
    }
    const handoff = await this.webVNCCredentialHandoffs.issue(lease.id, { username, password });
    return json(
      { ticket: handoff.ticket, expiresAt: handoff.expiresAt },
      { headers: { "cache-control": "no-store" } },
    );
  }

  private async webVNCReset(request: Request, identifier: string): Promise<Response> {
    if (request.method.toUpperCase() !== "POST") {
      return json({ error: "not_found" }, { status: 404 });
    }
    const admin = isAdminRequest(request);
    const lease = await this.resolveLease(identifier, request, admin);
    if (!lease) {
      return notFound();
    }
    if (!this.leaseManageableByRequest(lease, request, admin)) {
      return json({ error: "forbidden", message: "lease manage access required" }, { status: 403 });
    }
    const error = webVNCLeaseError(lease);
    if (error) {
      return json({ error: "webvnc_unavailable", message: error }, { status: 409 });
    }
    const bridgeWasConnected = this.openWebVNCAgents(lease.id).length > 0;
    const viewerWasConnected = this.openWebVNCViewers(lease.id).length > 0;
    this.closeWebVNCViewers(lease.id, 1012, "WebVNC reset requested");
    resetWebVNCBridge(
      this.webVNCAgents,
      this.pendingWebVNCToViewer,
      lease.id,
      1012,
      "WebVNC reset requested",
    );
    this.webVNCAgentCapabilities.delete(lease.id);
    this.recordWebVNCEvent(lease.id, "reset", "WebVNC reset requested");
    return json({
      leaseID: lease.id,
      slug: lease.slug ?? "",
      bridgeWasConnected,
      viewerWasConnected,
      command: webVNCBridgeCommand(lease),
      events: this.recentWebVNCEvents(lease.id),
    });
  }

  private async webVNCTakeControl(request: Request, identifier: string): Promise<Response> {
    const lease = await this.resolvePortalLease(identifier, request);
    if (!lease) {
      return notFound();
    }
    const error = webVNCLeaseError(lease);
    if (error) {
      return json({ error: "webvnc_unavailable", message: error }, { status: 409 });
    }
    const input: { viewerID?: string } = await readJson<{ viewerID?: string }>(request).catch(
      () => ({}),
    );
    const viewerID = input.viewerID ?? "";
    if (!validWebVNCSessionID(viewerID)) {
      return json(
        { error: "invalid_viewer", message: "valid WebVNC viewer id required" },
        { status: 400 },
      );
    }
    const viewer = this.webVNCViewers.get(lease.id)?.get(viewerID);
    if (!viewer || viewer.socket.readyState !== WebSocket.OPEN) {
      return json(
        { error: "viewer_not_connected", message: "viewer is not connected" },
        { status: 409 },
      );
    }
    const previousID = this.activeWebVNCControllerID(lease.id);
    this.webVNCControllers.set(lease.id, viewerID);
    if (previousID !== viewerID) {
      this.recordWebVNCEvent(lease.id, "control_taken", `${viewer.label} took control`);
    }
    return await this.webVNCStatus(
      new Request(
        `${new URL(request.url).origin}/portal/leases/${encodeURIComponent(lease.id)}/vnc/status?viewer=${encodeURIComponent(viewerID)}`,
        {
          headers: request.headers,
        },
      ),
      lease.id,
    );
  }

  private async webVNCTheme(request: Request, identifier: string): Promise<Response> {
    const lease = await this.resolvePortalLease(identifier, request);
    if (!lease) {
      return notFound();
    }
    const error = webVNCLeaseError(lease);
    if (error) {
      return json({ error: "webvnc_unavailable", message: error }, { status: 409 });
    }
    const input = await optionalJson<{ theme?: string; viewerID?: string; viewerId?: string }>(
      request,
    );
    const theme = input.theme === "light" ? "light" : input.theme === "dark" ? "dark" : "";
    if (!theme) {
      return json(
        { error: "invalid_theme", message: "theme must be light or dark" },
        { status: 400 },
      );
    }
    const requestedViewerID = input.viewerID || input.viewerId || "";
    if (!validWebVNCSessionID(requestedViewerID)) {
      return json(
        { error: "invalid_viewer", message: "valid WebVNC viewer id required" },
        { status: 400 },
      );
    }
    const viewer = this.webVNCViewers.get(lease.id)?.get(requestedViewerID);
    if (!viewer || viewer.socket.readyState !== WebSocket.OPEN) {
      return json(
        { error: "viewer_not_connected", message: "viewer is not connected" },
        { status: 409 },
      );
    }
    const controllerID = this.activeWebVNCControllerID(lease.id);
    const canManage = this.leaseManageableByRequest(lease, request, isAdminRequest(request));
    if (viewer.id !== controllerID && !canManage) {
      return json(
        { error: "not_controller", message: "WebVNC control is required to change desktop theme" },
        { status: 403 },
      );
    }
    const agentSocket = await this.currentBridgeRecipient(
      this.webVNCAgents.get(lease.id)?.get(viewer.agentID),
    );
    if (!agentSocket) {
      return json(
        { error: "webvnc_bridge_missing", message: "No WebVNC backend is available yet." },
        { status: 409 },
      );
    }
    const agentCapabilities = this.webVNCAgentCapabilities.get(lease.id)?.get(viewer.agentID);
    if (!agentCapabilities?.has("desktop_theme")) {
      return json(
        {
          error: "webvnc_bridge_upgrade_required",
          message: "Refresh the WebVNC bridge before changing the desktop theme.",
        },
        { status: 409 },
      );
    }
    agentSocket.send(JSON.stringify({ type: "desktop_theme", theme }));
    this.recordWebVNCEvent(lease.id, "theme_changed", theme);
    return json({ ok: true, leaseID: lease.id, theme });
  }

  private async createCodeTicket(request: Request, identifier: string): Promise<Response> {
    if (request.method.toUpperCase() !== "POST") {
      return json({ error: "not_found" }, { status: 404 });
    }
    const admin = isAdminRequest(request);
    const lease = await this.resolveLease(identifier, request, admin);
    if (!lease) {
      return notFound();
    }
    if (!this.leaseManageableByRequest(lease, request, admin)) {
      return json({ error: "forbidden", message: "lease manage access required" }, { status: 403 });
    }
    const error = codeLeaseError(lease);
    if (error) {
      return json({ error: "code_unavailable", message: error }, { status: 409 });
    }
    const bridgeGrant = await bridgeGrantForRequest(request, admin, this.env.CRABBOX_SHARED_TOKEN);
    if (!bridgeGrant) {
      return json(
        { error: "user_session_invalid", message: "GitHub user session cannot be revalidated" },
        { status: 401 },
      );
    }
    await this.cleanupExpiredCodeTickets();
    const now = new Date();
    const ticket: CodeTicketRecord = {
      ticket: newCodeTicket(),
      leaseID: lease.id,
      owner: requestOwner(request),
      org: requestOrg(request, this.env),
      admin,
      ...bridgeGrant,
      createdAt: now.toISOString(),
      expiresAt: new Date(now.getTime() + codeTicketTTLSeconds * 1000).toISOString(),
    };
    await this.state.storage.put(codeTicketKey(ticket.ticket), ticket);
    return json({
      ticket: ticket.ticket,
      leaseID: ticket.leaseID,
      expiresAt: ticket.expiresAt,
    });
  }

  private async codeAgent(request: Request, identifier: string): Promise<Response> {
    if (request.headers.get("upgrade")?.toLowerCase() !== "websocket") {
      return json(
        { error: "upgrade_required", message: "code bridge requires a websocket upgrade" },
        { status: 426 },
      );
    }
    return await this.withBridgeTicketLock(async () => {
      const consumed = await this.consumeCodeTicketUnderLock(request, identifier);
      if (consumed.status === "invalid") {
        return json(
          { error: "code_ticket_required", message: "valid code bridge ticket required" },
          { status: 401 },
        );
      }
      if (consumed.status === "not_found") {
        return notFound();
      }
      const { lease, ticket } = consumed;
      const error = codeLeaseError(lease);
      if (error) {
        return json({ error: "code_unavailable", message: error }, { status: 409 });
      }
      const upgrade = this.state.createWebSocketUpgrade();
      const agent = upgrade.socket;

      closeSocket(this.codeAgents.get(lease.id), 1012, "replaced by a newer code bridge");
      this.clearCodeLease(lease.id);
      this.codeAgents.set(lease.id, agent);
      this.acceptBridgeWebSocket(agent, {
        ...leaseBridgeTicketPrincipal(ticket),
        kind: "code-agent",
        leaseID: lease.id,
      });
      return upgrade.response;
    });
  }

  private async codePortalProxy(
    request: Request,
    identifier: string,
    _rest: string[],
  ): Promise<Response> {
    const isolated = await isIsolatedCodeRequest(request, this.env);
    if (isolated && _rest.length === 1 && _rest[0] === "__crabbox_bootstrap") {
      return await this.bootstrapCodeViewer(request, identifier);
    }
    let authorizedRequest = request;
    let viewerSession: CodeViewerSessionRecord | undefined;
    if (isolated) {
      const session = await this.codeViewerSession(request, identifier);
      if (!session) {
        return this.codeViewerAuthenticationRequired(request, identifier);
      }
      if (!isolatedCodeRequestOriginAllowed(request)) {
        return json(
          { error: "code_viewer_origin_forbidden", message: "Code viewer origin mismatch" },
          { status: 403 },
        );
      }
      viewerSession = session;
      const org = orgAuthLabelFromKey(session.org);
      if (org === undefined) {
        return this.codeViewerAuthenticationRequired(request, identifier);
      }
      authorizedRequest = requestWithAuthContext(request, {
        authorized: true,
        admin: session.admin,
        auth: session.auth,
        owner: session.owner,
        org,
        ...(session.login ? { login: session.login } : {}),
        ...(session.githubGrant
          ? {
              githubGrant: session.githubGrant,
              tokenExpiresAt: session.githubGrant.expiresAt,
            }
          : {}),
      });
    }
    const lease = await this.resolvePortalLease(identifier, authorizedRequest);
    if (!lease) {
      return portalError(
        "Lease not found",
        "That lease is not active or is not visible to you.",
        404,
      );
    }
    const error = codeLeaseError(lease);
    if (error) {
      return portalError("Code unavailable", error, 409);
    }
    const agent = await this.currentBridgeRecipient(this.codeAgents.get(lease.id));
    if (request.method.toUpperCase() === "GET" && _rest.length === 1 && _rest[0] === "health") {
      return this.codePortalHealth(lease, agent);
    }
    const isolatedOrigin = await codeOriginForLease(this.env, lease.id);
    if (!isolatedOrigin) {
      return portalError(
        "Code origin required",
        "Browser Code requires a valid CRABBOX_CODE_ORIGIN_TEMPLATE with wildcard TLS and WebSocket routing.",
        409,
      );
    }
    if (!isolated) {
      return await this.redirectCodeViewer(request, lease, isolatedOrigin);
    }
    if (!agent || agent.readyState !== WebSocket.OPEN) {
      return portalCode(lease);
    }
    if (request.headers.get("upgrade")?.toLowerCase() === "websocket") {
      const bridgeGrant = viewerSession
        ? copyBridgeGrant(viewerSession)
        : await bridgeGrantForRequest(
            authorizedRequest,
            isAdminRequest(authorizedRequest),
            this.env.CRABBOX_SHARED_TOKEN,
          );
      if (!bridgeGrant) {
        return json(
          { error: "code_viewer_session_revoked", message: "Code viewer session has ended" },
          { status: 401, headers: { "cache-control": "no-store" } },
        );
      }
      return await this.codeViewerWebSocket(authorizedRequest, lease, agent, bridgeGrant);
    }
    return await this.codeProxyHTTP(authorizedRequest, lease, agent);
  }

  private async redirectCodeViewer(
    request: Request,
    lease: LeaseRecord,
    isolatedOrigin: string,
  ): Promise<Response> {
    await this.cleanupExpiredCodeViewerAuth();
    const now = new Date();
    const auth = requestAuthType(request);
    const admin = isAdminRequest(request);
    const bridgeGrant = await bridgeGrantForRequest(request, admin, this.env.CRABBOX_SHARED_TOKEN);
    if (!bridgeGrant) {
      return portalError("Code session ended", "Log in again to open Code.", 401);
    }
    if (
      bridgeGrant.portalSessionHash &&
      (await this.portalSessionIsRevoked(bridgeGrant.portalSessionHash))
    ) {
      return portalError("Code session ended", "Log in again to open Code.", 401);
    }
    const ticket: CodeViewerTicketRecord = {
      ticket: newCodeViewerTicket(),
      leaseID: lease.id,
      auth,
      admin,
      ...bridgeGrant,
      owner: requestOwner(request),
      org: requestOrg(request, this.env),
      returnTo: canonicalCodeReturnTo(request, lease.id),
      createdAt: now.toISOString(),
      expiresAt: new Date(now.getTime() + codeViewerTicketTTLSeconds * 1000).toISOString(),
    };
    const tokenExpiresAt =
      bridgeGrant.githubGrant?.expiresAt ??
      request.headers.get("x-crabbox-token-expires-at")?.trim();
    if (tokenExpiresAt && Number.isFinite(Date.parse(tokenExpiresAt))) {
      ticket.viewerExpiresAt = new Date(tokenExpiresAt).toISOString();
    }
    await this.state.storage.put(codeViewerTicketKey(ticket.ticket), ticket);
    const location = new URL(
      `/portal/leases/${encodeURIComponent(lease.id)}/code/__crabbox_bootstrap`,
      isolatedOrigin,
    );
    return portalCodeBootstrapHandoff(location, ticket.ticket);
  }

  private async bootstrapCodeViewer(request: Request, leaseID: string): Promise<Response> {
    if (request.method !== "POST") {
      return json(
        { error: "method_not_allowed", message: "Code viewer bootstrap requires POST" },
        { status: 405, headers: { allow: "POST", "cache-control": "no-store" } },
      );
    }
    const contentType = request.headers.get("content-type")?.split(";", 1)[0]?.trim().toLowerCase();
    if (contentType !== "application/x-www-form-urlencoded") {
      return json(
        { error: "unsupported_media_type", message: "form-encoded Code viewer ticket required" },
        { status: 415, headers: { "cache-control": "no-store" } },
      );
    }
    const value = new URLSearchParams(await request.text()).get("ticket") ?? "";
    const ticket = await this.consumeCodeViewerTicket(value, leaseID);
    if (!ticket) {
      return json(
        { error: "code_viewer_ticket_required", message: "valid Code viewer ticket required" },
        { status: 401, headers: { "cache-control": "no-store" } },
      );
    }
    if (
      (ticket.auth === "github" && !validPortalSessionHash(ticket.portalSessionHash)) ||
      (ticket.portalSessionHash && (await this.portalSessionIsRevoked(ticket.portalSessionHash)))
    ) {
      return json(
        { error: "code_viewer_session_revoked", message: "Code viewer session has ended" },
        { status: 401, headers: { "cache-control": "no-store" } },
      );
    }
    const now = new Date();
    const defaultExpiresAt = now.getTime() + codeViewerSessionTTLSeconds * 1000;
    const tokenExpiresAt = Date.parse(ticket.viewerExpiresAt ?? "");
    const expiresAt = Number.isFinite(tokenExpiresAt)
      ? Math.min(defaultExpiresAt, tokenExpiresAt)
      : defaultExpiresAt;
    const session: CodeViewerSessionRecord = {
      session: newCodeViewerSession(),
      leaseID: ticket.leaseID,
      auth: ticket.auth,
      admin: ticket.admin,
      owner: ticket.owner,
      org: ticket.org,
      ...copyBridgeGrant(ticket),
      createdAt: now.toISOString(),
      expiresAt: new Date(expiresAt).toISOString(),
    };
    await this.state.storage.put(codeViewerSessionKey(session.session), session);
    return new Response(null, {
      status: 303,
      headers: {
        "cache-control": "no-store",
        location: ticket.returnTo,
        "referrer-policy": "no-referrer",
        "set-cookie": codeViewerSessionCookie(
          session,
          Math.max(0, Math.floor((expiresAt - now.getTime()) / 1000)),
        ),
      },
    });
  }

  private async codeViewerSession(
    request: Request,
    leaseID: string,
  ): Promise<CodeViewerSessionRecord | undefined> {
    const value = cookieValue(request.headers.get("cookie") ?? "", "crabbox_code_session");
    if (!validCodeViewerSession(value)) {
      return undefined;
    }
    const session = await this.state.storage.get<CodeViewerSessionRecord>(
      codeViewerSessionKey(value),
    );
    if (
      !session ||
      session.session !== value ||
      session.leaseID !== leaseID ||
      Date.parse(session.expiresAt) <= Date.now() ||
      !isCurrentOrgKey(session.org) ||
      (session.auth === "github" && !validPortalSessionHash(session.portalSessionHash)) ||
      (session.portalSessionHash &&
        (await this.portalSessionIsRevoked(session.portalSessionHash))) ||
      (session.auth === "github" && (await this.githubBridgeGrantFailureReason(session))) ||
      (session.auth === "bearer" &&
        session.admin !== true &&
        !(await this.sharedBridgeGrantIsCurrent(session)))
    ) {
      if (session) {
        await this.state.storage.delete(codeViewerSessionKey(value));
      }
      return undefined;
    }
    const currentSession =
      session.admin === true
        ? withCurrentAdminGrant(session, await this.currentAdminGrantValidation())
        : session;
    if (currentSession !== session) {
      await this.state.storage.put(codeViewerSessionKey(value), currentSession);
    }
    return currentSession;
  }

  private codeViewerAuthenticationRequired(request: Request, leaseID: string): Response {
    if (
      request.method.toUpperCase() === "GET" &&
      request.headers.get("upgrade")?.toLowerCase() !== "websocket" &&
      this.env.CRABBOX_PUBLIC_URL
    ) {
      const location = new URL(
        `/portal/leases/${encodeURIComponent(leaseID)}/code/`,
        this.env.CRABBOX_PUBLIC_URL,
      );
      return new Response(null, {
        status: 302,
        headers: { "cache-control": "no-store", location: location.toString() },
      });
    }
    return json(
      { error: "code_viewer_session_required", message: "Code viewer session required" },
      { status: 401, headers: { "cache-control": "no-store" } },
    );
  }

  private async consumeCodeViewerTicket(
    value: string,
    leaseID: string,
  ): Promise<CodeViewerTicketRecord | undefined> {
    if (!validCodeViewerTicket(value)) {
      return undefined;
    }
    return await this.withBridgeTicketLock(async () => {
      const key = codeViewerTicketKey(value);
      const ticket = await this.state.storage.get<CodeViewerTicketRecord>(key);
      if (
        !ticket ||
        ticket.ticket !== value ||
        ticket.leaseID !== leaseID ||
        !isCurrentOrgKey(ticket.org) ||
        Date.parse(ticket.expiresAt) <= Date.now() ||
        (ticket.viewerExpiresAt !== undefined && Date.parse(ticket.viewerExpiresAt) <= Date.now())
      ) {
        if (ticket) {
          await this.state.storage.delete(key);
        }
        return undefined;
      }
      await this.state.storage.delete(key);
      return ticket.admin === true
        ? withCurrentAdminGrant(ticket, await this.currentAdminGrantValidation())
        : ticket;
    });
  }

  private async cleanupExpiredCodeViewerAuth(): Promise<void> {
    const now = Date.now();
    await Promise.all(
      [
        codeViewerTicketPrefix(),
        codeViewerSessionPrefix(),
        codeViewerSessionRevocationPrefix(),
      ].map(async (prefix) => {
        const records = await this.state.storage.list<
          CodeViewerTicketRecord | CodeViewerSessionRecord | CodeViewerSessionRevocationRecord
        >({ prefix });
        await Promise.all(
          [...records.entries()]
            .filter(([, record]) => Date.parse(record.expiresAt) <= now)
            .map(([key]) => this.state.storage.delete(key)),
        );
      }),
    );
  }

  private async portalLogout(request: Request): Promise<Response> {
    await this.cleanupExpiredCodeViewerAuth();
    const token = cookieValue(request.headers.get("cookie") ?? "", "crabbox_session");
    if (token) {
      const verifiedTokenExpiresAt = await verifiedUserTokenExpiresAtForRevocation(token, this.env);
      if (verifiedTokenExpiresAt) {
        const portalSessionHash = await sha256Hex(token);
        const now = new Date();
        const tokenExpiresAt = Date.parse(verifiedTokenExpiresAt);
        const revocationExpiresAt = now.getTime() + codeViewerSessionTTLSeconds * 1000;
        const expiresAt = Number.isFinite(tokenExpiresAt)
          ? Math.min(revocationExpiresAt, tokenExpiresAt)
          : revocationExpiresAt;
        await this.withBridgeTicketLock(async () => {
          await this.state.storage.put<CodeViewerSessionRevocationRecord>(
            codeViewerSessionRevocationKey(portalSessionHash),
            {
              portalSessionHash,
              createdAt: now.toISOString(),
              expiresAt: new Date(expiresAt).toISOString(),
            },
          );
          this.closeBridgesForPortalSession(portalSessionHash);
        });
      }
    }
    return githubPortalLogout();
  }

  private async portalSessionIsRevoked(portalSessionHash: string): Promise<boolean> {
    const key = codeViewerSessionRevocationKey(portalSessionHash);
    const revocation = await this.state.storage.get<CodeViewerSessionRevocationRecord>(key);
    if (!revocation || revocation.portalSessionHash !== portalSessionHash) {
      return false;
    }
    if (Date.parse(revocation.expiresAt) > Date.now()) {
      return true;
    }
    await this.state.storage.delete(key);
    return false;
  }

  private codePortalHealth(lease: LeaseRecord, agent: WebSocket | undefined): Response {
    return json({
      lease: {
        id: lease.id,
        slug: lease.slug,
        state: lease.state,
        code: lease.code === true,
      },
      code: {
        agentConnected: agent?.readyState === WebSocket.OPEN,
        pendingRequests: this.pendingCodeRequests.size,
      },
    });
  }

  private async codeProxyHTTP(
    request: Request,
    lease: LeaseRecord,
    agent: WebSocket,
  ): Promise<Response> {
    const bodyBytes = new Uint8Array(await request.arrayBuffer());
    if (bodyBytes.byteLength > codeProxyRequestBodyBytes) {
      return json({ error: "request_too_large" }, { status: 413 });
    }
    const id = crypto.randomUUID();
    const url = new URL(request.url);
    const message: CodeProxyRequest = {
      type: "http",
      id,
      method: request.method,
      path: `${url.pathname}${url.search}`,
      headers: codeForwardHeaders(request.headers),
    };
    if (bodyBytes.byteLength > 0) {
      message.body = bytesToBase64(bodyBytes);
    }
    const response = await new Promise<CodeProxyResponse>((resolve) => {
      const timeout = setTimeout(() => {
        this.pendingCodeRequests.delete(id);
        resolve({ type: "http", id, status: 504, error: "code bridge timed out" });
      }, 30_000);
      this.pendingCodeRequests.set(id, { leaseID: lease.id, resolve, timeout, chunks: [] });
      agent.send(JSON.stringify(message));
    });
    if (response.error) {
      return json(
        { error: "code_proxy_error", message: response.error },
        { status: response.status || 502 },
      );
    }
    return new Response(response.body ? base64ToBytes(response.body) : null, {
      status: response.status || 502,
      headers: codeResponseHeaders(response.headers ?? {}, {
        cookiePath: codeResponseCookiePath(request, lease.id),
        secure: new URL(request.url).protocol === "https:",
      }),
    });
  }

  private async codeViewerWebSocket(
    request: Request,
    lease: LeaseRecord,
    agent: WebSocket,
    bridgeGrant: CachedBridgeGrant,
  ): Promise<Response> {
    return await this.withBridgeTicketLock(async () => {
      const currentLease = await this.resolvePortalLease(lease.id, request);
      if (!currentLease) {
        return notFound();
      }
      if (
        (bridgeGrant.auth === "github" &&
          (!validPortalSessionHash(bridgeGrant.portalSessionHash) || !bridgeGrant.githubGrant)) ||
        (bridgeGrant.portalSessionHash &&
          (await this.portalSessionIsRevoked(bridgeGrant.portalSessionHash)))
      ) {
        return json(
          { error: "code_viewer_session_revoked", message: "Code viewer session has ended" },
          { status: 401, headers: { "cache-control": "no-store" } },
        );
      }
      const upgrade = this.state.createWebSocketUpgrade();
      const viewer = upgrade.socket;
      const id = crypto.randomUUID();
      this.codeViewers.set(id, viewer);
      this.acceptBridgeWebSocket(viewer, {
        kind: "code-viewer",
        leaseID: lease.id,
        id,
        auth: bridgeGrant.auth ?? requestAuthType(request),
        owner: requestOwner(request),
        org: requestOrg(request, this.env),
        admin: isAdminRequest(request),
        ...bridgeGrant,
      });
      const url = new URL(request.url);
      const open: CodeWebSocketOpen = {
        type: "ws_open",
        id,
        path: `${url.pathname}${url.search}`,
        headers: codeForwardHeaders(request.headers),
      };
      agent.send(JSON.stringify(open));
      return upgrade.response;
    });
  }

  private sendCodeWebSocketData(agent: WebSocket, message: CodeWebSocketData): void {
    const data = base64ToBytes(message.body);
    if (data.byteLength <= maxCodeWebSocketFrameChunkBytes) {
      agent.send(JSON.stringify(message));
      return;
    }
    const chunkID = crypto.randomUUID();
    const frame = message.frame ?? "binary";
    const start: CodeWebSocketFrameStart = {
      type: "ws_start",
      id: message.id,
      chunkID,
      frame,
    };
    agent.send(JSON.stringify(start));
    for (let offset = 0; offset < data.byteLength; offset += maxCodeWebSocketFrameChunkBytes) {
      const body: CodeWebSocketFrameBody = {
        type: "ws_body",
        id: message.id,
        chunkID,
        body: bytesToBase64(data.slice(offset, offset + maxCodeWebSocketFrameChunkBytes)),
      };
      agent.send(JSON.stringify(body));
    }
    const end: CodeWebSocketFrameEnd = { type: "ws_end", id: message.id, chunkID };
    agent.send(JSON.stringify(end));
  }

  private async sendCodeDataToViewer(message: CodeWebSocketData): Promise<void> {
    const viewer = await this.currentBridgeRecipient(this.codeViewers.get(message.id));
    if (!viewer) {
      return;
    }
    const data = base64ToBytes(message.body);
    viewer.send(message.frame === "text" ? textDecoder.decode(data) : data);
  }

  private async handleCodeAgentMessage(leaseID: string, rawData: unknown): Promise<void> {
    const raw = await normalizeWebVNCData(rawData);
    const text = typeof raw === "string" ? raw : textDecoder.decode(raw);
    let message:
      | CodeProxyResponse
      | CodeWebSocketData
      | CodeWebSocketFrameStart
      | CodeWebSocketFrameBody
      | CodeWebSocketFrameEnd
      | CodeWebSocketClose
      | { type?: string; id?: string; error?: string };
    try {
      message = JSON.parse(text);
    } catch {
      return;
    }
    if (message.type === "http" && message.id) {
      const pending = this.pendingCodeRequests.get(message.id);
      if (!pending) {
        return;
      }
      clearTimeout(pending.timeout);
      this.pendingCodeRequests.delete(message.id);
      pending.resolve(message as CodeProxyResponse);
      return;
    }
    if (message.type === "http_start" && message.id) {
      const pending = this.pendingCodeRequests.get(message.id);
      if (!pending) {
        return;
      }
      pending.response = { ...(message as CodeProxyResponse), type: "http", body: "" };
      pending.chunks = [];
      return;
    }
    if (message.type === "http_body" && message.id) {
      const pending = this.pendingCodeRequests.get(message.id);
      if (!pending) {
        return;
      }
      pending.chunks.push((message as CodeProxyResponse).body ?? "");
      return;
    }
    if (message.type === "http_end" && message.id) {
      const pending = this.pendingCodeRequests.get(message.id);
      if (!pending) {
        return;
      }
      clearTimeout(pending.timeout);
      this.pendingCodeRequests.delete(message.id);
      pending.resolve({
        ...(pending.response ?? { type: "http", id: message.id, status: 502 }),
        body: pending.chunks.join(""),
      });
      return;
    }
    if (message.type === "ws_data" && message.id) {
      await this.sendCodeDataToViewer(message as CodeWebSocketData);
      return;
    }
    if (message.type === "ws_start" && message.id) {
      const start = message as CodeWebSocketFrameStart;
      this.pendingCodeFrames.set(start.chunkID, {
        leaseID,
        id: start.id,
        frame: start.frame ?? "binary",
        chunks: [],
      });
      return;
    }
    if (message.type === "ws_body") {
      const body = message as CodeWebSocketFrameBody;
      const pending = this.pendingCodeFrames.get(body.chunkID);
      if (pending) {
        pending.chunks.push(body.body);
      }
      return;
    }
    if (message.type === "ws_end") {
      const end = message as CodeWebSocketFrameEnd;
      const pending = this.pendingCodeFrames.get(end.chunkID);
      this.pendingCodeFrames.delete(end.chunkID);
      if (pending) {
        await this.sendCodeDataToViewer({
          type: "ws_data",
          id: pending.id,
          frame: pending.frame,
          body: pending.chunks.join(""),
        });
      }
      return;
    }
    if (message.type === "ws_close" && message.id) {
      const viewer = this.codeViewers.get(message.id);
      this.codeViewers.delete(message.id);
      closeSocket(
        viewer,
        (message as CodeWebSocketClose).code ?? 1000,
        (message as CodeWebSocketClose).reason ?? "code socket closed",
      );
      return;
    }
    void leaseID;
  }

  private clearCodeAgent(leaseID: string, socket: WebSocket): void {
    if (this.codeAgents.get(leaseID) !== socket) {
      return;
    }
    this.codeAgents.delete(leaseID);
    this.clearCodeLease(leaseID);
  }

  private clearCodeViewer(
    leaseID: string,
    id: string,
    socket: WebSocket,
    code = 1000,
    reason = "viewer closed",
  ): void {
    if (this.codeViewers.get(id) !== socket) {
      return;
    }
    this.codeViewers.delete(id);
    const agent = this.codeAgents.get(leaseID);
    const message: CodeWebSocketClose = { type: "ws_close", id, code, reason };
    if (agent?.readyState === WebSocket.OPEN) {
      agent.send(JSON.stringify(message));
    }
  }

  private closeBridgesForPortalSession(portalSessionHash: string): void {
    for (const [leaseID, viewers] of this.webVNCViewers) {
      for (const [id, viewer] of viewers) {
        const attachment = this.bridgeAttachment(viewer.socket);
        if (
          attachment?.kind !== "webvnc-viewer" ||
          attachment.portalSessionHash !== portalSessionHash
        ) {
          continue;
        }
        this.clearWebVNCViewer(leaseID, id, viewer.socket);
        closeSocket(viewer.socket, 1008, "portal session ended");
      }
    }
    for (const [id, viewer] of this.codeViewers) {
      const attachment = this.bridgeAttachment(viewer);
      if (
        attachment?.kind !== "code-viewer" ||
        attachment.portalSessionHash !== portalSessionHash
      ) {
        continue;
      }
      this.clearCodeViewer(attachment.leaseID, id, viewer, 1008, "portal session ended");
      closeSocket(viewer, 1008, "portal session ended");
    }
    for (const [leaseID, agents] of this.webVNCAgents) {
      for (const [id, agent] of agents) {
        const attachment = this.bridgeAttachment(agent);
        if (
          attachment?.kind !== "webvnc-agent" ||
          attachment.portalSessionHash !== portalSessionHash
        ) {
          continue;
        }
        this.clearWebVNCAgent(leaseID, id, agent);
        closeSocket(agent, 1008, "portal session ended");
      }
    }
    for (const [leaseID, agent] of this.codeAgents) {
      const attachment = this.bridgeAttachment(agent);
      if (attachment?.kind !== "code-agent" || attachment.portalSessionHash !== portalSessionHash) {
        continue;
      }
      this.clearCodeAgent(leaseID, agent);
      closeSocket(agent, 1008, "portal session ended");
    }
    const egressSessions = new Map<string, { leaseID: string; sessionID: string }>();
    for (const socket of [...this.egressHosts.values(), ...this.egressClients.values()]) {
      const attachment = this.bridgeAttachment(socket);
      if (
        (attachment?.kind !== "egress-host" && attachment?.kind !== "egress-client") ||
        attachment.portalSessionHash !== portalSessionHash
      ) {
        continue;
      }
      egressSessions.set(egressSocketKey(attachment.leaseID, attachment.sessionID), {
        leaseID: attachment.leaseID,
        sessionID: attachment.sessionID,
      });
    }
    for (const { leaseID, sessionID } of egressSessions.values()) {
      this.clearEgressSession(leaseID, sessionID, 1008, "portal session ended");
    }
  }

  private clearCodeLease(leaseID: string, code = 1011, reason = "code bridge disconnected"): void {
    for (const [id, viewer] of this.codeViewers) {
      const attachment = this.bridgeAttachment(viewer);
      if (attachment?.kind !== "code-viewer" || attachment.leaseID !== leaseID) {
        continue;
      }
      this.codeViewers.delete(id);
      closeSocket(viewer, code, reason);
    }
    for (const [id, pending] of this.pendingCodeRequests) {
      if (pending.leaseID !== leaseID) {
        continue;
      }
      clearTimeout(pending.timeout);
      this.pendingCodeRequests.delete(id);
      pending.resolve({ type: "http", id, status: 502, error: reason });
    }
    for (const [chunkID, pending] of this.pendingCodeFrames) {
      if (pending.leaseID === leaseID) {
        this.pendingCodeFrames.delete(chunkID);
      }
    }
  }

  private clearEgressHost(leaseID: string, sessionID: string, socket: WebSocket): void {
    const key = egressSocketKey(leaseID, sessionID);
    if (this.egressHosts.get(key) !== socket) {
      return;
    }
    this.egressHosts.delete(key);
    closeSocket(this.egressClients.get(key), 1011, "egress host disconnected");
    this.egressClients.delete(key);
  }

  private clearEgressClient(leaseID: string, sessionID: string, socket: WebSocket): void {
    const key = egressSocketKey(leaseID, sessionID);
    if (this.egressClients.get(key) !== socket) {
      return;
    }
    this.egressClients.delete(key);
    closeSocket(this.egressHosts.get(key), 1011, "egress client disconnected");
    this.egressHosts.delete(key);
  }

  private clearEgressLease(leaseID: string, code = 1011, reason = "lease ended"): void {
    for (const [key, socket] of this.egressHosts) {
      if (egressSocketLeaseID(key) === leaseID) {
        closeSocket(socket, code, reason);
        this.egressHosts.delete(key);
      }
    }
    for (const [key, socket] of this.egressClients) {
      if (egressSocketLeaseID(key) === leaseID) {
        closeSocket(socket, code, reason);
        this.egressClients.delete(key);
      }
    }
    this.egressSessions.delete(leaseID);
  }

  private closeLeaseBridges(leaseID: string, code: number, reason: string): void {
    const webVNCAgents = this.webVNCAgents.get(leaseID);
    for (const [agentID, socket] of webVNCAgents ?? []) {
      closeSocket(socket, code, reason);
      this.pendingWebVNCToViewer.delete(webVNCBufferKey(leaseID, agentID));
    }
    this.webVNCAgents.delete(leaseID);
    this.webVNCAgentCapabilities.delete(leaseID);
    this.closeWebVNCViewers(leaseID, code, reason);

    const codeAgent = this.codeAgents.get(leaseID);
    this.codeAgents.delete(leaseID);
    closeSocket(codeAgent, code, reason);
    this.clearCodeLease(leaseID, code, reason);
    this.clearEgressLease(leaseID, code, reason);
  }

  private clearEgressSession(
    leaseID: string,
    sessionID: string,
    code: number,
    reason: string,
  ): void {
    const key = egressSocketKey(leaseID, sessionID);
    closeSocket(this.egressHosts.get(key), code, reason);
    closeSocket(this.egressClients.get(key), code, reason);
    this.egressHosts.delete(key);
    this.egressClients.delete(key);
    if (this.egressSessions.get(leaseID)?.sessionID === sessionID) {
      this.egressSessions.delete(leaseID);
    }
  }

  private async webVNCViewer(request: Request, identifier: string): Promise<Response> {
    if (request.headers.get("upgrade")?.toLowerCase() !== "websocket") {
      return json(
        { error: "upgrade_required", message: "WebVNC viewer requires a websocket upgrade" },
        { status: 426 },
      );
    }
    return await this.withBridgeTicketLock(async () => {
      const lease = await this.resolvePortalLease(identifier, request);
      if (!lease) {
        return notFound();
      }
      const error = webVNCLeaseError(lease);
      if (error) {
        return json({ error: "webvnc_unavailable", message: error }, { status: 409 });
      }
      const admin = isAdminRequest(request);
      const bridgeGrant = await bridgeGrantForRequest(
        request,
        admin,
        this.env.CRABBOX_SHARED_TOKEN,
      );
      if (!bridgeGrant) {
        return json(
          { error: "user_session_invalid", message: "GitHub user session cannot be revalidated" },
          { status: 401 },
        );
      }
      const owner = requestOwner(request);
      const org = requestOrg(request, this.env);
      if (
        bridgeGrant.auth === "github" &&
        (await this.githubBridgeGrantFailureReason({ ...bridgeGrant, owner, org, admin }))
      ) {
        return json(
          { error: "user_session_invalid", message: "GitHub user session cannot be revalidated" },
          { status: 401 },
        );
      }
      const agent = await this.claimIdleWebVNCAgent(lease.id);
      if (!agent) {
        const canManage = this.leaseManageableByRequest(lease, request, isAdminRequest(request));
        const command = canManage ? webVNCBridgeCommand(lease) : "";
        return json(
          {
            error: "webvnc_bridge_missing",
            message: canManage
              ? `No WebVNC backend is available yet; start or refresh the bridge with: ${command}`
              : "No WebVNC backend is available yet; ask a lease manager to start or refresh the bridge.",
            command,
          },
          { status: 409 },
        );
      }
      const url = new URL(request.url);
      const requestedViewerID = url.searchParams.get("viewer") || "";
      const viewerID = validWebVNCSessionID(requestedViewerID)
        ? requestedViewerID
        : newWebVNCSessionID("viewer");

      const upgrade = this.state.createWebSocketUpgrade();
      const viewer = upgrade.socket;
      const label = webVNCViewerLabel(owner);

      this.trackWebVNCViewer(lease.id, {
        id: viewerID,
        agentID: agent.id,
        socket: viewer,
        owner,
        org,
        admin,
        ...bridgeGrant,
        label,
        connectedAt: new Date().toISOString(),
      });
      if (!this.activeWebVNCControllerID(lease.id)) {
        this.webVNCControllers.set(lease.id, viewerID);
        this.recordWebVNCEvent(lease.id, "control_taken", `${label} is controlling`);
      }
      this.recordWebVNCEvent(lease.id, "viewer_connected", label);
      this.acceptBridgeWebSocket(viewer, {
        kind: "webvnc-viewer",
        leaseID: lease.id,
        id: viewerID,
        agentID: agent.id,
        owner,
        org,
        admin,
        ...bridgeGrant,
        label,
      });
      flushPendingWebVNC(this.pendingWebVNCToViewer, webVNCBufferKey(lease.id, agent.id), viewer);
      return upgrade.response;
    });
  }

  private clearWebVNCAgent(leaseID: string, agentID: string, socket: WebSocket): void {
    const agents = this.webVNCAgents.get(leaseID);
    if (agents?.get(agentID) !== socket) {
      return;
    }
    agents.delete(agentID);
    const capabilities = this.webVNCAgentCapabilities.get(leaseID);
    capabilities?.delete(agentID);
    if (capabilities?.size === 0) {
      this.webVNCAgentCapabilities.delete(leaseID);
    }
    if (agents.size === 0) {
      this.webVNCAgents.delete(leaseID);
    }
    this.pendingWebVNCToViewer.delete(webVNCBufferKey(leaseID, agentID));
    const viewer = this.webVNCViewerForAgent(leaseID, agentID);
    if (viewer) {
      closeSocket(viewer.socket, 1011, "WebVNC bridge disconnected");
      this.removeWebVNCViewer(leaseID, viewer.id);
    }
    this.recordWebVNCEvent(leaseID, "bridge_disconnected");
  }

  private clearWebVNCViewer(leaseID: string, viewerID: string, socket: WebSocket): void {
    const viewer = this.webVNCViewers.get(leaseID)?.get(viewerID);
    if (!viewer || viewer.socket !== socket) {
      return;
    }
    this.removeWebVNCViewer(leaseID, viewerID);
    this.recordWebVNCEvent(leaseID, "viewer_disconnected", viewer.label);
    const agent = this.webVNCAgents.get(leaseID)?.get(viewer.agentID);
    closeSocket(agent, 1011, "WebVNC viewer disconnected");
    const agents = this.webVNCAgents.get(leaseID);
    agents?.delete(viewer.agentID);
    if (agents?.size === 0) {
      this.webVNCAgents.delete(leaseID);
    }
    const capabilities = this.webVNCAgentCapabilities.get(leaseID);
    capabilities?.delete(viewer.agentID);
    if (capabilities?.size === 0) {
      this.webVNCAgentCapabilities.delete(leaseID);
    }
    this.pendingWebVNCToViewer.delete(webVNCBufferKey(leaseID, viewer.agentID));
    this.recordWebVNCEvent(leaseID, "bridge_reset", "WebVNC viewer disconnected");
  }

  private recordWebVNCEvent(leaseID: string, event: string, reason?: string): void {
    const events = this.webVNCEvents.get(leaseID) ?? [];
    const record: WebVNCEvent = { at: new Date().toISOString(), event };
    if (reason) {
      record.reason = reason;
    }
    events.push(record);
    this.webVNCEvents.set(leaseID, events.slice(-12));
  }

  private recentWebVNCEvents(leaseID: string): WebVNCEvent[] {
    return this.webVNCEvents.get(leaseID) ?? [];
  }

  private openWebVNCAgents(leaseID: string): Array<[string, WebSocket]> {
    return [...(this.webVNCAgents.get(leaseID) ?? new Map<string, WebSocket>())].filter(
      ([, socket]) => socket.readyState === WebSocket.OPEN,
    );
  }

  private openWebVNCViewers(leaseID: string): WebVNCViewerSession[] {
    return [
      ...(this.webVNCViewers.get(leaseID) ?? new Map<string, WebVNCViewerSession>()).values(),
    ].filter((viewer) => viewer.socket.readyState === WebSocket.OPEN);
  }

  private webVNCViewerForAgent(leaseID: string, agentID: string): WebVNCViewerSession | undefined {
    return this.openWebVNCViewers(leaseID).find((viewer) => viewer.agentID === agentID);
  }

  private async claimIdleWebVNCAgent(
    leaseID: string,
  ): Promise<{ id: string; socket: WebSocket } | undefined> {
    const viewers = this.openWebVNCViewers(leaseID);
    const candidates = this.openWebVNCAgents(leaseID).filter(
      ([id]) => !viewers.some((viewer) => viewer.agentID === id),
    );
    const current = await Promise.all(
      candidates.map(async ([id, socket]) =>
        (await this.currentBridgeRecipient(socket)) ? { id, socket } : undefined,
      ),
    );
    return current.find((agent) => agent !== undefined);
  }

  private activeWebVNCControllerID(leaseID: string): string {
    const viewers = this.openWebVNCViewers(leaseID);
    const current = this.webVNCControllers.get(leaseID);
    if (current && viewers.some((viewer) => viewer.id === current)) {
      return current;
    }
    const next = viewers[0]?.id ?? "";
    if (next) {
      this.webVNCControllers.set(leaseID, next);
    } else {
      this.webVNCControllers.delete(leaseID);
    }
    return next;
  }

  private removeWebVNCViewer(leaseID: string, viewerID: string): void {
    const viewers = this.webVNCViewers.get(leaseID);
    viewers?.delete(viewerID);
    if (!viewers || viewers.size === 0) {
      this.webVNCViewers.delete(leaseID);
      this.webVNCControllers.delete(leaseID);
      return;
    }
    if (this.webVNCControllers.get(leaseID) === viewerID) {
      const next = this.openWebVNCViewers(leaseID)[0]?.id;
      if (next) {
        this.webVNCControllers.set(leaseID, next);
      } else {
        this.webVNCControllers.delete(leaseID);
      }
    }
  }

  private closeWebVNCViewers(leaseID: string, code: number, reason: string): void {
    for (const viewer of this.openWebVNCViewers(leaseID)) {
      closeSocket(viewer.socket, code, reason);
    }
    this.webVNCViewers.delete(leaseID);
    this.webVNCControllers.delete(leaseID);
  }

  private async consumeRuntimeAdapterTicket(
    request: Request,
    adapterID: string,
  ): Promise<RuntimeAdapterTicketConsumption> {
    const value = runtimeAdapterTicketFromRequest(request);
    if (!validRuntimeAdapterTicket(value)) {
      return { status: "invalid" };
    }
    return this.withBridgeTicketLock(async () => {
      const key = runtimeAdapterTicketKey(value);
      const ticket = await this.state.storage.get<RuntimeAdapterTicketRecord>(key);
      if (!ticket || ticket.ticket !== value || !isCurrentOrgKey(ticket.org)) {
        return { status: "invalid" };
      }
      if (Date.parse(ticket.expiresAt) <= Date.now()) {
        await this.state.storage.delete(key);
        return { status: "invalid" };
      }
      if (ticket.adapterID !== adapterID) {
        return { status: "invalid" };
      }
      await this.state.storage.delete(key);
      return { status: "accepted", ticket };
    });
  }

  private async cleanupExpiredRuntimeAdapterTickets(): Promise<void> {
    const tickets = await this.state.storage.list<RuntimeAdapterTicketRecord>({
      prefix: runtimeAdapterTicketPrefix(),
    });
    const now = Date.now();
    await Promise.all(
      [...tickets.entries()]
        .filter(([, ticket]) => Date.parse(ticket.expiresAt) <= now)
        .map(([key]) => this.state.storage.delete(key)),
    );
  }

  private async runtimeAdapterIdentityReclaimable(
    identity: RuntimeAdapterIdentityRecord,
    adapterID: string,
    now: number,
  ): Promise<boolean> {
    if (
      identity.adapterID !== adapterID ||
      (!isCurrentOrgKey(identity.org) && !isLegacyOrgKey(identity.org)) ||
      identity.claimVersion !== 1 ||
      identity.claimState !== "provisional" ||
      identity.confirmedAt !== undefined ||
      !Number.isFinite(Date.parse(identity.claimExpiresAt ?? "")) ||
      Date.parse(identity.claimExpiresAt ?? "") > now ||
      this.runtimeAdapterAgents.has(adapterID) ||
      [...this.runtimeAdapterPending.values()].some((pending) => pending.adapterID === adapterID)
    ) {
      return false;
    }
    const [tickets, leases] = await Promise.all([
      this.state.storage.list<RuntimeAdapterTicketRecord>({
        prefix: runtimeAdapterTicketPrefix(),
      }),
      this.leaseRecords(),
    ]);
    if (
      [...tickets.values()].some(
        (ticket) => ticket.adapterID === adapterID && Date.parse(ticket.expiresAt) > now,
      )
    ) {
      return false;
    }
    return !leases.some(
      (lease) =>
        lease.runtimeAdapterID === adapterID &&
        (leaseIsLive(lease) || Boolean(lease.runtimeAdapterDeleteRequestedAt)),
    );
  }

  private async confirmRuntimeAdapterIdentity(
    identity: RuntimeAdapterIdentityRecord,
    confirmedAt: string,
  ): Promise<void> {
    if (
      identity.claimVersion !== 1 ||
      identity.claimState !== "provisional" ||
      identity.confirmedAt !== undefined
    ) {
      return;
    }
    const confirmed: RuntimeAdapterIdentityRecord = {
      ...identity,
      claimState: "confirmed",
      confirmedAt,
    };
    delete confirmed.claimExpiresAt;
    await this.state.storage.put(runtimeAdapterIdentityKey(identity.adapterID), confirmed);
  }

  private async consumeWebVNCTicket(
    request: Request,
    identifier: string,
  ): Promise<LeaseBridgeTicketConsumption<WebVNCTicketRecord>> {
    return this.withBridgeTicketLock(() => this.consumeWebVNCTicketUnderLock(request, identifier));
  }

  private async currentLeaseBridgeTicket<
    T extends CachedBridgeGrant & { owner: string; org: string; admin?: boolean },
  >(ticket: T, lease: LeaseRecord): Promise<T | undefined> {
    if (!isCurrentOrgKey(ticket.org) || !isCurrentOrgKey(lease.org)) {
      return undefined;
    }
    const currentTicket =
      ticket.admin === true
        ? withCurrentAdminGrant(ticket, await this.currentAdminGrantValidation())
        : ticket;
    if (!this.leaseManagerAuthorized(lease, leaseBridgeTicketPrincipal(currentTicket))) {
      return undefined;
    }
    if (
      currentTicket.auth === "github" &&
      (await this.githubBridgeGrantFailureReason(currentTicket))
    ) {
      return undefined;
    }
    if (
      currentTicket.auth === "bearer" &&
      currentTicket.admin !== true &&
      !(await this.sharedBridgeGrantIsCurrent(currentTicket))
    ) {
      return undefined;
    }
    return currentTicket;
  }

  private async consumeWebVNCTicketUnderLock(
    request: Request,
    identifier: string,
  ): Promise<LeaseBridgeTicketConsumption<WebVNCTicketRecord>> {
    const value = bridgeTicketFromRequest(request, this.env);
    if (!validWebVNCTicket(value)) {
      return { status: "invalid" };
    }
    const key = webVNCTicketKey(value);
    const ticket = await this.state.storage.get<WebVNCTicketRecord>(key);
    if (!ticket || ticket.ticket !== value) {
      return { status: "invalid" };
    }
    if (Date.parse(ticket.expiresAt) <= Date.now()) {
      await this.state.storage.delete(key);
      return { status: "invalid" };
    }
    const lease = await this.getLease(ticket.leaseID);
    if (!lease || !identifierMatchesLease(identifier, lease)) {
      return { status: "not_found" };
    }
    const currentTicket = await this.currentLeaseBridgeTicket(ticket, lease);
    if (!currentTicket) {
      await this.state.storage.delete(key);
      return { status: "invalid" };
    }
    await this.state.storage.delete(key);
    return { status: "accepted", ticket: currentTicket, lease };
  }

  private async cleanupExpiredWebVNCTickets(): Promise<void> {
    const tickets = await this.state.storage.list<WebVNCTicketRecord>({
      prefix: webVNCTicketPrefix(),
    });
    const now = Date.now();
    await Promise.all(
      [...tickets.entries()]
        .filter(([, ticket]) => Date.parse(ticket.expiresAt) <= now)
        .map(([key]) => this.state.storage.delete(key)),
    );
  }

  private async consumeCodeTicket(
    request: Request,
    identifier: string,
  ): Promise<LeaseBridgeTicketConsumption<CodeTicketRecord>> {
    return this.withBridgeTicketLock(() => this.consumeCodeTicketUnderLock(request, identifier));
  }

  private async consumeCodeTicketUnderLock(
    request: Request,
    identifier: string,
  ): Promise<LeaseBridgeTicketConsumption<CodeTicketRecord>> {
    const value = bridgeTicketFromRequest(request, this.env);
    if (!validCodeTicket(value)) {
      return { status: "invalid" };
    }
    const key = codeTicketKey(value);
    const ticket = await this.state.storage.get<CodeTicketRecord>(key);
    if (!ticket || ticket.ticket !== value) {
      return { status: "invalid" };
    }
    if (Date.parse(ticket.expiresAt) <= Date.now()) {
      await this.state.storage.delete(key);
      return { status: "invalid" };
    }
    const lease = await this.getLease(ticket.leaseID);
    if (!lease || !identifierMatchesLease(identifier, lease)) {
      return { status: "not_found" };
    }
    const currentTicket = await this.currentLeaseBridgeTicket(ticket, lease);
    if (!currentTicket) {
      await this.state.storage.delete(key);
      return { status: "invalid" };
    }
    await this.state.storage.delete(key);
    return { status: "accepted", ticket: currentTicket, lease };
  }

  private async cleanupExpiredCodeTickets(): Promise<void> {
    const tickets = await this.state.storage.list<CodeTicketRecord>({
      prefix: codeTicketPrefix(),
    });
    const now = Date.now();
    await Promise.all(
      [...tickets.entries()]
        .filter(([, ticket]) => Date.parse(ticket.expiresAt) <= now)
        .map(([key]) => this.state.storage.delete(key)),
    );
  }

  private async consumeEgressTicket(
    request: Request,
    identifier: string,
    role: EgressRole,
  ): Promise<LeaseBridgeTicketConsumption<EgressTicketRecord>> {
    return this.withBridgeTicketLock(() =>
      this.consumeEgressTicketUnderLock(request, identifier, role),
    );
  }

  private async consumeEgressTicketUnderLock(
    request: Request,
    identifier: string,
    role: EgressRole,
  ): Promise<LeaseBridgeTicketConsumption<EgressTicketRecord>> {
    const value = bridgeTicketFromRequest(request, this.env);
    if (!validEgressTicket(value)) {
      return { status: "invalid" };
    }
    const key = egressTicketKey(value);
    const ticket = await this.state.storage.get<EgressTicketRecord>(key);
    if (!ticket || ticket.ticket !== value) {
      return { status: "invalid" };
    }
    if (Date.parse(ticket.expiresAt) <= Date.now()) {
      await this.state.storage.delete(key);
      return { status: "invalid" };
    }
    if (ticket.role !== role) {
      return { status: "invalid" };
    }
    const lease = await this.getLease(ticket.leaseID);
    if (!lease || !identifierMatchesLease(identifier, lease)) {
      return { status: "not_found" };
    }
    const currentTicket = await this.currentLeaseBridgeTicket(ticket, lease);
    if (!currentTicket) {
      await this.state.storage.delete(key);
      return { status: "invalid" };
    }
    await this.state.storage.delete(key);
    return { status: "accepted", ticket: currentTicket, lease };
  }

  private async cleanupExpiredEgressTickets(): Promise<void> {
    const tickets = await this.state.storage.list<EgressTicketRecord>({
      prefix: egressTicketPrefix(),
    });
    const now = Date.now();
    await Promise.all(
      [...tickets.entries()]
        .filter(([, ticket]) => Date.parse(ticket.expiresAt) <= now)
        .map(([key]) => this.state.storage.delete(key)),
    );
  }

  private async pool(request: Request): Promise<Response> {
    const url = new URL(request.url);
    const provider = url.searchParams.get("provider");
    const machines =
      provider === "aws"
        ? await this.provider("aws").listCrabboxServers()
        : provider === "hetzner"
          ? await this.provider("hetzner").listCrabboxServers()
          : provider === "azure"
            ? await this.provider("azure").listCrabboxServers()
            : provider === "gcp"
              ? await this.provider("gcp").listCrabboxServers()
              : provider === "daytona"
                ? await this.provider("daytona").listCrabboxServers()
                : [
                    ...(await this.provider("hetzner").listCrabboxServers()),
                    ...(await this.listProviderMachinesSafe("aws")),
                    ...(await this.listProviderMachinesSafe("azure")),
                    ...(await this.listProviderMachinesSafe("gcp")),
                    ...(await this.listProviderMachinesSafe("daytona")),
                  ];
    return json({ machines });
  }

  private async readyPoolRoute(
    request: Request,
    rawKey?: string,
    action?: string,
  ): Promise<Response> {
    const method = request.method.toUpperCase();
    if (method === "GET" && !rawKey) {
      return json({
        pools: (await this.allReadyPoolStatus(request)).map(publicReadyPoolEntry),
      });
    }
    const decodedKey = decodeReadyPoolRouteKey(rawKey ?? "");
    const key = decodedKey === undefined ? "" : normalizeReadyPoolKey(decodedKey);
    if (!key) {
      return json({ error: "invalid_pool_key" }, { status: 400 });
    }
    if (method === "GET" && !action) {
      return json({ pool: (await this.readyPoolStatus(key, request)).map(publicReadyPoolEntry) });
    }
    if (method === "POST" && action === "register") {
      return await this.registerReadyPoolLease(request, key);
    }
    if (method === "POST" && action === "borrow") {
      return await this.borrowReadyPoolLease(request, key);
    }
    if (method === "POST" && action === "return") {
      return await this.returnReadyPoolLease(request, key);
    }
    return notFound();
  }

  private async readyPoolStatus(key: string, request: Request): Promise<ReadyPoolEntry[]> {
    return await this.withReadyPoolBorrowLock(() =>
      this.state.runExclusive(() => this.readyPoolStatusSnapshot(request, key)),
    );
  }

  private async allReadyPoolStatus(request: Request): Promise<ReadyPoolEntry[]> {
    return await this.withReadyPoolBorrowLock(() =>
      this.state.runExclusive(() => this.readyPoolStatusSnapshot(request)),
    );
  }

  private async readyPoolStatusSnapshot(request: Request, key?: string): Promise<ReadyPoolEntry[]> {
    const entries = await this.readyPoolEntries();
    const leases = new Map((await this.leaseRecords()).map((lease) => [lease.id, lease]));
    const visible = entries.filter(
      (entry) =>
        (!key || entry.key === key) &&
        this.readyPoolEntryVisibleToRequest(entry, request, leases.get(entry.leaseID)),
    );
    await this.markStaleReadyPoolEntries(visible, leases, Date.now());
    return (await this.readyPoolEntries())
      .filter(
        (entry) =>
          (!key || entry.key === key) &&
          this.readyPoolEntryVisibleToRequest(entry, request, leases.get(entry.leaseID)),
      )
      .map((entry) => redactReadyPoolEntry(entry))
      .toSorted((a, b) => a.key.localeCompare(b.key) || a.leaseID.localeCompare(b.leaseID));
  }

  private async registerReadyPoolLease(request: Request, key: string): Promise<Response> {
    const input = await readJson<ReadyPoolRegisterRequest>(request);
    const leaseID = input.leaseID ?? "";
    if (!validLeaseID(leaseID)) {
      return json({ error: "invalid_lease_id" }, { status: 400 });
    }
    const lease = await this.resolveLease(leaseID, request, isAdminRequest(request));
    if (!lease) {
      return notFound();
    }
    if (!this.leaseManageableByRequest(lease, request, isAdminRequest(request))) {
      return json({ error: "forbidden", message: "lease manage access required" }, { status: 403 });
    }
    if (isRegisteredLease(lease)) {
      return json(
        {
          error: "unsupported_lifecycle",
          message: "registered leases cannot join coordinator-managed ready pools",
        },
        { status: 400 },
      );
    }
    if (lease.provider === "daytona") {
      return json(
        {
          error: "provider_not_supported",
          message: "Daytona leases cannot join ready pools because their SSH token rotates",
        },
        { status: 409 },
      );
    }
    if (lease.state !== "active" || Date.parse(lease.expiresAt) <= Date.now()) {
      return json({ error: "lease_not_active" }, { status: 409 });
    }
    return await this.withReadyPoolBorrowLock(async () => {
      const existingPoolEntries = (await this.readyPoolEntries()).filter(
        (entry) => entry.leaseID === leaseID,
      );
      if (existingPoolEntries.some((entry) => entry.state === "busy")) {
        return json(
          {
            error: "lease_pool_busy",
            message: "lease is currently borrowed from a ready pool",
          },
          { status: 409 },
        );
      }
      const now = new Date().toISOString();
      const entry: ReadyPoolEntry = {
        key,
        leaseID,
        state: "ready",
        owner: lease.owner,
        org: lease.org,
        provider: lease.provider,
        target: lease.target,
        class: lease.class,
        serverType: lease.serverType,
        lastReadyAt: now,
        createdAt: now,
        updatedAt: now,
        expiresAt: lease.expiresAt,
      };
      addReadyPoolEntryString(entry, "repo", input.repo);
      addReadyPoolEntryString(entry, "ref", input.ref);
      addReadyPoolEntryString(entry, "commit", input.commit);
      addReadyPoolEntryString(entry, "fingerprint", input.fingerprint);
      addReadyPoolEntryString(entry, "image", input.image);
      addReadyPoolEntryString(entry, "sshHost", readyPoolLeaseSSHHost(lease, input.sshHost));
      addReadyPoolEntryString(entry, "sshUser", readyPoolLeaseSSHUser(lease, input.sshUser));
      addReadyPoolEntryString(entry, "sshPort", readyPoolLeaseSSHPort(lease, input.sshPort));
      addReadyPoolEntryString(entry, "workRoot", readyPoolLeaseWorkRoot(lease, input.workRoot));
      if (lease.windowsMode) {
        entry.windowsMode = lease.windowsMode;
      }
      await Promise.all(
        existingPoolEntries
          .filter((existing) => existing.key !== key)
          .map((existing) => this.deleteReadyPoolEntry(existing)),
      );
      await this.putReadyPoolEntry(entry);
      return json({ entry: publicReadyPoolEntry(entry), lease: publicLeaseRecord(lease) });
    });
  }

  private async borrowReadyPoolLease(request: Request, key: string): Promise<Response> {
    const input = await readJson<ReadyPoolBorrowRequest>(request);
    return await this.withReadyPoolBorrowLock(async () => {
      return await this.state.runExclusive(async () => {
        const entries = (await this.readyPoolStatusSnapshot(request, key)).filter((entry) =>
          readyPoolEntryMatches(entry, input),
        );
        const leases = new Map((await this.leaseRecords()).map((lease) => [lease.id, lease]));
        const nowMs = Date.now();
        const candidates: Array<{ entry: ReadyPoolEntry; lease: LeaseRecord }> = [];
        let blockedByManageAccess = false;
        for (const entry of entries) {
          const lease = leases.get(entry.leaseID);
          if (
            lease &&
            entry.state === "ready" &&
            lease.state === "active" &&
            Date.parse(lease.expiresAt) > nowMs
          ) {
            if (!this.leaseManageableByRequest(lease, request, isAdminRequest(request))) {
              blockedByManageAccess = true;
              continue;
            }
            candidates.push({ entry, lease });
          }
        }
        const ready = candidates.toSorted(
          (a, b) =>
            Date.parse(a.entry.lastReadyAt ?? a.entry.createdAt) -
            Date.parse(b.entry.lastReadyAt ?? b.entry.createdAt),
        );
        const first = ready[0];
        if (!first) {
          if (blockedByManageAccess) {
            return json(
              { error: "forbidden", message: "lease manage access required" },
              { status: 403 },
            );
          }
          return json(
            { error: "no_ready_lease", message: "no matching ready lease in pool" },
            { status: 409 },
          );
        }
        const { entry, lease } = first;
        const now = new Date().toISOString();
        const borrowed: ReadyPoolEntry = {
          ...entry,
          state: "busy",
          borrowedBy: requestOwner(request),
          borrowedAt: now,
          borrowToken: crypto.randomUUID(),
          lastUsedAt: now,
          updatedAt: now,
          expiresAt: lease.expiresAt,
        };
        await this.putReadyPoolEntry(borrowed);
        return json({
          entry: publicReadyPoolEntry(borrowed),
          lease: publicLeaseRecord(lease),
        });
      });
    });
  }

  private async returnReadyPoolLease(request: Request, key: string): Promise<Response> {
    const input = await readJson<ReadyPoolReturnRequest>(request);
    const leaseID = input.leaseID ?? "";
    if (!validLeaseID(leaseID)) {
      return json({ error: "invalid_lease_id" }, { status: 400 });
    }
    return await this.withReadyPoolBorrowLock(async () => {
      const current = await this.getReadyPoolEntry(key, leaseID);
      if (!current) {
        return notFound();
      }
      const lease = await this.getLease(leaseID);
      if (!this.readyPoolEntryVisibleToRequest(current, request, lease)) {
        return notFound();
      }
      const canManage =
        isAdminRequest(request) ||
        Boolean(lease && this.leaseManageableByRequest(lease, request, false));
      if (current.state !== "busy" && !canManage) {
        return json(
          { error: "not_borrowed", message: "pool entry is not borrowed" },
          { status: 409 },
        );
      }
      if (current.borrowedBy && current.borrowedBy !== requestOwner(request) && !canManage) {
        return json(
          { error: "forbidden", message: "lease is borrowed by another user" },
          { status: 403 },
        );
      }
      if (
        current.state === "busy" &&
        current.borrowToken &&
        nonSecretString(input.borrowToken) !== current.borrowToken
      ) {
        return json(
          { error: "borrow_token_mismatch", message: "borrow token does not match current borrow" },
          { status: 409 },
        );
      }
      const result = String(input.result ?? "ready");
      if (result !== "ready" && result !== "drain" && result !== "release") {
        return json(
          { error: "invalid_result", message: "result must be ready, drain, or release" },
          { status: 400 },
        );
      }
      if (result === "release" || result === "drain") {
        if (!canManage) {
          return json(
            { error: "forbidden", message: "lease manage access required" },
            { status: 403 },
          );
        }
        const drained = this.nextReturnedReadyPoolEntry(current, lease, "draining", input.reason);
        await this.putReadyPoolEntry(drained);
        if (lease && lease.state === "active") {
          return json({
            entry: publicReadyPoolEntry(drained),
            lease: publicLeaseRecord(
              await this.releaseResolvedLease(lease, { deleteServer: true, keep: false }),
            ),
          });
        }
        return json({
          entry: publicReadyPoolEntry(drained),
          lease: lease ? publicLeaseRecord(lease) : undefined,
        });
      }
      if (!lease || lease.state !== "active" || Date.parse(lease.expiresAt) <= Date.now()) {
        const stale = this.nextReturnedReadyPoolEntry(current, lease, "stale", input.reason);
        await this.putReadyPoolEntry(stale);
        return json({
          entry: publicReadyPoolEntry(stale),
          lease: lease ? publicLeaseRecord(lease) : undefined,
        });
      }
      const returned = this.nextReturnedReadyPoolEntry(current, lease, "ready", input.reason);
      await this.putReadyPoolEntry(returned);
      return json({
        entry: publicReadyPoolEntry(returned),
        lease: publicLeaseRecord(lease),
      });
    });
  }

  private nextReturnedReadyPoolEntry(
    current: ReadyPoolEntry,
    lease: LeaseRecord | undefined,
    state: ReadyPoolEntry["state"],
    reason?: string,
  ): ReadyPoolEntry {
    const now = new Date().toISOString();
    const failures = state === "ready" ? 0 : (current.failureCount ?? 0) + 1;
    const {
      borrowedAt: _borrowedAt,
      borrowedBy: _borrowedBy,
      borrowToken: _borrowToken,
      ...base
    } = current;
    void _borrowedAt;
    void _borrowedBy;
    void _borrowToken;
    const returned: ReadyPoolEntry = {
      ...base,
      state,
      lastResult: nonSecretString(reason) || state,
      failureCount: failures,
      updatedAt: now,
      expiresAt: lease?.expiresAt ?? current.expiresAt,
    };
    if (state === "ready") {
      returned.lastReadyAt = now;
    } else if (current.lastReadyAt) {
      returned.lastReadyAt = current.lastReadyAt;
    }
    return returned;
  }

  private async markStaleReadyPoolEntries(
    entries: ReadyPoolEntry[],
    leases: Map<string, LeaseRecord>,
    nowMs: number,
  ): Promise<void> {
    await Promise.all(
      entries
        .filter((entry) => {
          const lease = leases.get(entry.leaseID);
          return !lease || lease.state !== "active" || Date.parse(lease.expiresAt) <= nowMs;
        })
        .map((entry) =>
          this.putReadyPoolEntry({
            ...entry,
            state: "stale",
            updatedAt: new Date().toISOString(),
            lastResult: "lease expired or missing",
          }),
        ),
    );
  }

  private async listLeases(request: Request): Promise<Response> {
    const admin = isAdminRequest(request);
    const leases = admin
      ? this.filterLeases(await this.leaseRecords(), request)
      : this.filterLeasesForRequest(await this.leaseRecords(), request);
    return json({ leases: leases.map((lease) => this.leaseForListRequest(lease, request, admin)) });
  }

  private async adminLeases(request: Request): Promise<Response> {
    return json({
      leases: this.filterLeases(await this.leaseRecords(), request).map((lease) =>
        this.leaseForListRequest(lease, request, true),
      ),
    });
  }

  private async adminLeaseAudit(request: Request): Promise<Response> {
    const url = new URL(request.url);
    const provider = (url.searchParams.get("provider") ?? "aws").trim().toLowerCase();
    if (provider !== "aws" && provider !== "azure") {
      return json(
        {
          error: "unsupported_provider",
          message: "lease audit currently supports provider=aws or provider=azure",
        },
        { status: 400 },
      );
    }
    const state = url.searchParams.get("state") ?? "expired";
    const owner = url.searchParams.get("owner") ?? "";
    const org = orgFilterKey(url);
    if (org === null) {
      return json({ error: "invalid_org_identity" }, { status: 400 });
    }
    const limit = clampLimit(url.searchParams.get("limit"), 100);
    const leases = (await this.leaseRecords())
      .filter((lease) => lease.provider === provider && !isRegisteredLease(lease))
      .filter((lease) => !state || lease.state === state)
      .filter((lease) => !owner || lease.owner === owner)
      .filter((lease) => !org || orgMatchesForFilter(lease.org, org))
      .filter((lease) => Boolean(lease.cloudID))
      .toSorted((a, b) => b.createdAt.localeCompare(a.createdAt))
      .slice(0, limit);
    const audits = await Promise.all(
      leases.map((lease) =>
        provider === "aws" ? this.auditAWSLeaseCloud(lease) : this.auditAzureLeaseCloud(lease),
      ),
    );
    return json({ audits });
  }

  private async adminAWSIdentity(request: Request): Promise<Response> {
    const url = new URL(request.url);
    const queryRegion = url.searchParams.get("region") ?? this.env.CRABBOX_AWS_REGION ?? "";
    const region = sanitizeAWSRegion(queryRegion || "eu-west-1");
    if (!region) {
      return json(
        { error: "invalid_region", message: "region must be an AWS region name" },
        { status: 400 },
      );
    }
    const identity = await new EC2SpotClient(this.env, region).identity();
    return json({ identity });
  }

  private async adminProviderIdentity(request: Request): Promise<Response> {
    const url = new URL(request.url);
    const provider = (url.searchParams.get("provider") ?? "aws").trim().toLowerCase();
    if (provider !== "aws") {
      return json(
        {
          error: "unsupported_provider",
          message: "admin provider identity currently supports provider=aws",
        },
        { status: 400 },
      );
    }
    return await this.adminAWSIdentity(request);
  }

  private async adminTailscalePreflight(): Promise<Response> {
    return json({ tailscale: await tailscalePreflight(this.env) });
  }

  private async adminHostsRoute(request: Request, hostID?: string): Promise<Response> {
    const url = new URL(request.url);
    const provider = (url.searchParams.get("provider") ?? "aws").trim().toLowerCase();
    const target = (url.searchParams.get("target") ?? "macos").trim().toLowerCase();
    if (provider !== "aws" || target !== "macos") {
      return json(
        {
          error: "unsupported_host_scope",
          message: "admin hosts currently supports provider=aws and target=macos",
        },
        { status: 400 },
      );
    }
    return await this.adminMacHostsRoute(request, hostID);
  }

  private async adminMacHostsRoute(request: Request, hostID?: string): Promise<Response> {
    const method = request.method.toUpperCase();
    const url = new URL(request.url);
    const queryRegion = url.searchParams.get("region") ?? this.env.CRABBOX_AWS_REGION ?? "";
    const region = sanitizeAWSRegion(queryRegion || "eu-west-1");
    if (!region) {
      return json(
        { error: "invalid_region", message: "region must be an AWS region name" },
        { status: 400 },
      );
    }
    const client = new EC2SpotClient(this.env, region);
    if (method === "GET" && hostID === "offerings") {
      const serverType = (url.searchParams.get("type") ?? "mac2.metal").trim();
      if (!serverType.startsWith("mac") || !serverType.endsWith(".metal")) {
        return json(
          { error: "invalid_type", message: "type must be an EC2 Mac metal instance type" },
          { status: 400 },
        );
      }
      const offerings = await client.listMacHostOfferings(serverType);
      return json({ offerings });
    }
    if (method === "GET" && hostID === "quota") {
      const serverType = (url.searchParams.get("type") ?? "mac2.metal").trim();
      if (!serverType.startsWith("mac") || !serverType.endsWith(".metal")) {
        return json(
          { error: "invalid_type", message: "type must be an EC2 Mac metal instance type" },
          { status: 400 },
        );
      }
      try {
        const quotas = await client.listMacHostQuotas(serverType);
        return json({ quotas, region, type: serverType });
      } catch (error) {
        return json(
          {
            error: "mac_host_quota_failed",
            message: sanitizeMacHostQuotaError(coordinatorErrorMessage(this.env, error)),
          },
          { status: 502 },
        );
      }
    }
    if (method === "GET" && !hostID) {
      const serverType = (url.searchParams.get("type") ?? "").trim();
      const state = (url.searchParams.get("state") ?? "").trim();
      const hosts = await client.listMacHosts(serverType, state);
      return json({ hosts });
    }
    if (method === "POST" && hostID === "dry-run") {
      const input = await readJson<{
        type?: string;
        availabilityZone?: string;
        clientToken?: string;
      }>(request);
      const serverType = (input.type ?? "mac2.metal").trim();
      if (!serverType.startsWith("mac") || !serverType.endsWith(".metal")) {
        return json(
          { error: "invalid_type", message: "type must be an EC2 Mac metal instance type" },
          { status: 400 },
        );
      }
      const availabilityZone = input.availabilityZone?.trim().toLowerCase() ?? "";
      if (availabilityZone && !availabilityZone.startsWith(region)) {
        return json(
          {
            error: "invalid_availability_zone",
            message: "availabilityZone must be an AWS availability zone in the selected region",
          },
          { status: 400 },
        );
      }
      const offerings = availabilityZone
        ? [{ region, availabilityZone, instanceType: serverType }]
        : await client.listMacHostOfferings(serverType);
      if (offerings.length === 0) {
        return json(
          {
            error: "no_mac_host_offerings",
            message: `no EC2 Mac host offerings found in ${region} for ${serverType}`,
          },
          { status: 400 },
        );
      }
      const clientToken = input.clientToken?.trim() || `crabbox-mac-host-${newLeaseID().slice(4)}`;
      const checks = await Promise.all(
        offerings.map((offering) =>
          client.dryRunAllocateMacHost(
            serverType,
            offering.availabilityZone,
            `${clientToken}-${offering.availabilityZone.replaceAll("-", "")}`,
          ),
        ),
      );
      return json({ dryRun: true, checks, offerings });
    }
    if (method === "POST" && !hostID) {
      const input = await readJson<{
        type?: string;
        availabilityZone?: string;
        clientToken?: string;
      }>(request);
      const serverType = (input.type ?? "mac2.metal").trim();
      if (!serverType.startsWith("mac") || !serverType.endsWith(".metal")) {
        return json(
          { error: "invalid_type", message: "type must be an EC2 Mac metal instance type" },
          { status: 400 },
        );
      }
      const availabilityZone = input.availabilityZone?.trim().toLowerCase() ?? "";
      if (availabilityZone && !availabilityZone.startsWith(region)) {
        return json(
          {
            error: "invalid_availability_zone",
            message: "availabilityZone must be an AWS availability zone in the selected region",
          },
          { status: 400 },
        );
      }
      const clientToken = input.clientToken?.trim() || `crabbox-mac-host-${newLeaseID().slice(4)}`;
      if (!availabilityZone) {
        const offerings = await client.listMacHostOfferings(serverType);
        if (offerings.length === 0) {
          return json(
            {
              error: "no_mac_host_offerings",
              message: `no EC2 Mac host offerings found in ${region} for ${serverType}`,
            },
            { status: 400 },
          );
        }
        const failures: string[] = [];
        for (const offering of offerings) {
          try {
            // oxlint-disable-next-line eslint/no-await-in-loop -- Mac host allocation can bill capacity; try one AZ at a time.
            const hosts = await client.allocateMacHost(
              serverType,
              offering.availabilityZone,
              `${clientToken}-${offering.availabilityZone.replaceAll("-", "")}`,
            );
            return json(
              { hosts, availabilityZone: offering.availabilityZone, offerings },
              { status: 201 },
            );
          } catch (error) {
            const message = coordinatorErrorMessage(this.env, error);
            failures.push(`${offering.availabilityZone}: ${message}`);
          }
        }
        return json(
          {
            error: "mac_host_allocation_failed",
            message: failures.join("; "),
            offerings,
          },
          { status: 502 },
        );
      }
      const hosts = await client.allocateMacHost(serverType, availabilityZone, clientToken);
      return json({ hosts }, { status: 201 });
    }
    if (method === "DELETE" && hostID) {
      const released = await client.releaseMacHost(hostID);
      return json({ hostId: hostID, released });
    }
    return json({ error: "not_found" }, { status: 404 });
  }

  private async adminAWSOrphanSweep(request: Request): Promise<Response> {
    const config = this.awsOrphanSweepConfig();
    const lastRun =
      (await this.state.storage.get<AWSOrphanSweepRecord>(awsOrphanSweepRecordKey)) ?? null;
    if (request.method.toUpperCase() === "GET") {
      return json({ config, lastRun });
    }
    if (!config.enabled) {
      return json(
        {
          error: "aws_orphan_sweep_disabled",
          message: "AWS orphan sweep is disabled or AWS broker credentials are not configured",
          config,
          lastRun,
        },
        { status: 409 },
      );
    }
    const sweep = await this.runAWSOrphanSweepIfDue("admin", config);
    await this.state.runExclusive(() => this.scheduleAlarm());
    return json({ config, sweep });
  }

  private async adminAzureOrphanSweep(request: Request): Promise<Response> {
    const config = this.azureOrphanSweepConfig();
    const lastRun =
      (await this.state.storage.get<AzureOrphanSweepRecord>(azureOrphanSweepRecordKey)) ?? null;
    if (request.method.toUpperCase() === "GET") {
      return json({ config, lastRun });
    }
    if (!config.enabled) {
      return json(
        {
          error: "azure_orphan_sweep_disabled",
          message: "Azure orphan sweep is disabled or Azure broker credentials are not configured",
          config,
          lastRun,
        },
        { status: 409 },
      );
    }
    const sweep = await this.runAzureOrphanSweepIfDue("admin", config);
    await this.state.runExclusive(() => this.scheduleAlarm());
    return json({ config, sweep });
  }

  private async auditAWSLeaseCloud(lease: LeaseRecord): Promise<LeaseCloudAudit> {
    const audit: LeaseCloudAudit = {
      leaseID: lease.id,
      provider: lease.provider,
      state: lease.state,
      target: lease.target,
      owner: lease.owner,
      org: orgLabelForDisplay(lease.org),
      cloudID: lease.cloudID,
      host: lease.host,
      serverType: lease.serverType,
      expiresAt: lease.expiresAt,
      cloudStatus: "error",
    };
    if (lease.slug) {
      audit.slug = lease.slug;
    }
    if (lease.region) {
      audit.region = lease.region;
    }
    if (lease.cleanupAttempts !== undefined) {
      audit.cleanupAttempts = lease.cleanupAttempts;
    }
    if (lease.cleanupError) {
      audit.cleanupError = coordinatorDiagnosticText(this.env, lease.cleanupError);
    }
    if (lease.cleanupRetryAt) {
      audit.cleanupRetryAt = lease.cleanupRetryAt;
    }
    try {
      const server = await this.awsLeaseServer(lease);
      if (isAWSTerminalInstanceState(server.status)) {
        return {
          ...audit,
          cloudStatus: "missing",
          cloudState: server.status,
          message: `aws instance is ${server.status}`,
        };
      }
      return {
        ...audit,
        cloudStatus: "found",
        cloudState: server.status,
        cloudHost: server.host,
        cloudServerType: server.serverType,
      };
    } catch (error) {
      const message = coordinatorErrorMessage(this.env, error);
      if (isCloudNotFoundError(message)) {
        return { ...audit, cloudStatus: "missing", message };
      }
      return { ...audit, cloudStatus: "error", message };
    }
  }

  private async awsLeaseServer(lease: LeaseRecord): Promise<ProviderMachine> {
    const provider = this.provider("aws", lease.region);
    if (provider.getServer) {
      return await provider.getServer(lease.cloudID);
    }
    const machines = await provider.listCrabboxServers();
    const server = machines.find(
      (machine) => machine.cloudID === lease.cloudID || String(machine.id) === lease.cloudID,
    );
    if (!server) {
      throw new Error(`aws instance not found: ${lease.cloudID}`);
    }
    return server;
  }

  private async auditAzureLeaseCloud(lease: LeaseRecord): Promise<LeaseCloudAudit> {
    const audit: LeaseCloudAudit = {
      leaseID: lease.id,
      provider: lease.provider,
      state: lease.state,
      target: lease.target,
      owner: lease.owner,
      org: orgLabelForDisplay(lease.org),
      cloudID: lease.cloudID,
      host: lease.host,
      serverType: lease.serverType,
      expiresAt: lease.expiresAt,
      cloudStatus: "error",
    };
    if (lease.slug) {
      audit.slug = lease.slug;
    }
    if (lease.region) {
      audit.region = lease.region;
    }
    if (lease.cleanupAttempts !== undefined) {
      audit.cleanupAttempts = lease.cleanupAttempts;
    }
    if (lease.cleanupError) {
      audit.cleanupError = coordinatorDiagnosticText(this.env, lease.cleanupError);
    }
    if (lease.cleanupRetryAt) {
      audit.cleanupRetryAt = lease.cleanupRetryAt;
    }
    try {
      const machines = await this.provider("azure", lease.region).listCrabboxServers();
      const server = machines.find(
        (machine) =>
          machine.cloudID === lease.cloudID ||
          machine.name === lease.cloudID ||
          machine.labels?.["lease"] === lease.id,
      );
      if (!server) {
        return {
          ...audit,
          cloudStatus: "missing",
          message: `azure virtual machine not found: ${lease.cloudID}`,
        };
      }
      return {
        ...audit,
        cloudStatus: "found",
        cloudState: server.status,
        cloudHost: server.host,
        cloudServerType: server.serverType,
      };
    } catch (error) {
      const message = coordinatorErrorMessage(this.env, error);
      if (isCloudNotFoundError(message)) {
        return { ...audit, cloudStatus: "missing", message };
      }
      return { ...audit, cloudStatus: "error", message };
    }
  }

  private async adminLeaseRoute(
    request: Request,
    leaseID: string,
    action?: string,
  ): Promise<Response> {
    if (request.method.toUpperCase() !== "POST") {
      return json({ error: "not_found" }, { status: 404 });
    }
    if (action === "release") {
      return this.releaseLease(request, leaseID, true);
    }
    if (action === "delete") {
      return this.adminDeleteLease(request, leaseID);
    }
    return json({ error: "not_found" }, { status: 404 });
  }

  private async adminDeleteLease(request: Request, leaseID: string): Promise<Response> {
    const lease = await this.resolveLease(leaseID, request, true);
    if (!lease) {
      return notFound();
    }
    if (lease.runtimeAdapterDeleteRequestedAt) {
      return this.runtimeAdapterDeletePendingResponse(lease, request, true);
    }
    const released = await this.releaseResolvedLease(lease, { deleteServer: true, keep: false });
    return released.runtimeAdapterDeleteRequestedAt
      ? this.runtimeAdapterDeletePendingResponse(released, request, true)
      : json({ lease: publicLeaseRecord(released) });
  }

  private filterLeases(leases: LeaseRecord[], request: Request): LeaseRecord[] {
    const url = new URL(request.url);
    const limit = clampLimit(url.searchParams.get("limit"), 100);
    return this.filterLeasesWithoutLimit(leases, request).slice(0, limit);
  }

  private filterLeasesWithoutLimit(leases: LeaseRecord[], request: Request): LeaseRecord[] {
    const url = new URL(request.url);
    const state = url.searchParams.get("state") ?? "";
    const provider = url.searchParams.get("provider") ?? "";
    const owner = url.searchParams.get("owner") ?? "";
    const org = orgFilterKey(url);
    if (org === null) return [];
    return leases
      .filter((lease) => !state || lease.state === state)
      .filter((lease) => !provider || lease.provider === provider)
      .filter((lease) => !owner || lease.owner === owner)
      .filter((lease) => !org || orgMatchesForFilter(lease.org, org))
      .toSorted((a, b) => b.createdAt.localeCompare(a.createdAt));
  }

  private async createRun(request: Request): Promise<Response> {
    const owner = requestOwner(request);
    const org = requestOrg(request, this.env);
    const input = await readJson<RunCreateRequest>(request);
    const leaseID = input.leaseID ?? "";
    if (leaseID && !validLeaseID(leaseID)) {
      return json({ error: "invalid_lease_id" }, { status: 400 });
    }
    const lease = leaseID ? await this.getLease(leaseID) : undefined;
    if (lease && !this.leaseVisibleToRequest(lease, request, false)) {
      return json({ error: "not_found" }, { status: 404 });
    }
    const now = new Date().toISOString();
    const run: RunRecord = {
      id: newRunID(),
      leaseID,
      leaseIDs: [],
      owner,
      org,
      leaseOwners: [],
      provider: lease?.provider ?? input.provider ?? "hetzner",
      target: lease?.target ?? input.target ?? "linux",
      class: lease?.class ?? input.class ?? "",
      serverType: lease?.serverType ?? input.serverType ?? "",
      command: Array.isArray(input.command) ? input.command.map(String) : [],
      state: "running",
      phase: "starting",
      logBytes: 0,
      logTruncated: false,
      startedAt: now,
      lastEventAt: now,
      eventCount: 0,
    };
    if (lease) {
      this.setRunLeaseAttribution(run, lease);
    }
    const windowsMode = lease?.windowsMode ?? input.windowsMode;
    if (windowsMode) {
      run.windowsMode = windowsMode;
    }
    if (lease?.slug) {
      run.slug = lease.slug;
    }
    const label = sanitizeRunLabel(input.label);
    if (label) {
      run.label = label;
    }
    await this.putRun(run);
    await this.appendRunEventRecord(run, { type: "run.started", phase: "starting" });
    return json({ run: publicRunRecord(run) }, { status: 201 });
  }

  private async createArtifactUploads(request: Request): Promise<Response> {
    try {
      const input = await readJson<ArtifactUploadRequest>(request);
      // NUL cannot occur in a validated org label, so it preserves the missing-org tenant
      // without colliding with the literal "unknown" label or exposing coordinator keys.
      const artifactOrg =
        requestOrg(request, this.env) === MISSING_ORG_KEY
          ? "\0"
          : requestOrgLabel(request, this.env);
      return json(
        await artifactUploadResponse(this.env, input, {
          owner: requestOwner(request),
          org: artifactOrg,
        }),
        { status: 201 },
      );
    } catch (error) {
      return json(
        {
          error: "artifact_upload_unavailable",
          message: coordinatorErrorMessage(this.env, error),
        },
        { status: 400 },
      );
    }
  }

  private async runRoute(request: Request, runID: string, action?: string): Promise<Response> {
    const method = request.method.toUpperCase();
    if (method === "GET" && action === undefined) {
      const run = await this.getRun(runID);
      const lease = run ? await this.ensureRunLeaseAttribution(run) : undefined;
      return run && this.runReadableToRequest(run, request, lease)
        ? json({ run: publicRunRecord(run) })
        : notFound();
    }
    if (method === "GET" && action === "logs") {
      const run = await this.getRun(runID);
      const lease = run ? await this.ensureRunLeaseAttribution(run) : undefined;
      if (!run || !this.runReadableToRequest(run, request, lease)) {
        return notFound();
      }
      const log = await this.readRunLog(runID);
      return new Response(log, {
        headers: { "content-type": "text/plain; charset=utf-8" },
      });
    }
    if (method === "GET" && action === "events") {
      const run = await this.getRun(runID);
      const lease = run ? await this.ensureRunLeaseAttribution(run) : undefined;
      if (!run || !this.runReadableToRequest(run, request, lease)) {
        return notFound();
      }
      const url = new URL(request.url);
      const after = finiteQueryNumber(url.searchParams.get("after")) ?? 0;
      const limit = clampLimit(url.searchParams.get("limit"), 500);
      return json({ events: await this.runEvents(runID, after, limit) });
    }
    if (method === "POST" && action === "events") {
      const run = await this.getRun(runID);
      if (!run || !this.runWritableByRequest(run, request)) {
        return notFound();
      }
      const input = await readJson<RunEventRequest>(request);
      if (input.leaseID && input.leaseID !== run.leaseID) {
        const lease = validLeaseID(input.leaseID) ? await this.getLease(input.leaseID) : undefined;
        if (!lease || !this.leaseManageableByRequest(lease, request, isAdminRequest(request))) {
          return notFound();
        }
      }
      const event = await this.appendRunEventRecord(run, input);
      return json({ event }, { status: 201 });
    }
    if (method === "POST" && action === "telemetry") {
      return this.appendRunTelemetry(request, runID);
    }
    if (method === "POST" && action === "finish") {
      return this.finishRun(request, runID);
    }
    return json({ error: "not_found" }, { status: 404 });
  }

  private async appendRunTelemetry(request: Request, runID: string): Promise<Response> {
    const run = await this.getRun(runID);
    if (!run || !this.runWritableByRequest(run, request)) {
      return notFound();
    }
    const input = await readJson<RunTelemetryRequest>(request);
    const telemetry = sanitizeLeaseTelemetry(input.telemetry, new Date());
    if (!telemetry) {
      return json({ error: "invalid_telemetry" }, { status: 400 });
    }
    run.telemetry = appendRunTelemetrySample(run.telemetry, telemetry);
    await this.putRun(run);
    return json({ run: publicRunRecord(run) });
  }

  private async finishRun(request: Request, runID: string): Promise<Response> {
    const run = await this.getRun(runID);
    if (!run || !this.runWritableByRequest(run, request)) {
      return notFound();
    }
    const input = await readJson<RunFinishRequest>(request);
    const now = new Date();
    const started = Date.parse(run.startedAt);
    run.exitCode = Number.isFinite(input.exitCode) ? input.exitCode : 1;
    const syncMs = finiteNumber(input.syncMs);
    const commandMs = finiteNumber(input.commandMs);
    if (syncMs !== undefined) {
      run.syncMs = syncMs;
    }
    if (commandMs !== undefined) {
      run.commandMs = commandMs;
    }
    if (Number.isFinite(started)) {
      run.durationMs = now.getTime() - started;
    }
    const blockedStage = sanitizeRunClassification(input.blockedStage);
    const retryLikely = sanitizeRunClassification(input.retryLikely);
    if (blockedStage) {
      run.blockedStage = blockedStage;
    }
    if (retryLikely) {
      run.retryLikely = retryLikely;
    }
    run.state = run.exitCode === 0 ? "succeeded" : "failed";
    run.phase = run.state;
    run.endedAt = now.toISOString();
    const logInput = normalizeRunLogInput(input);
    run.logBytes = logInput.bytes;
    run.logTruncated = logInput.truncated;
    if (input.results) {
      run.results = boundedTestResults(input.results);
    }
    const telemetry = sanitizeRunTelemetry(input.telemetry, now);
    if (telemetry) {
      run.telemetry = mergeRunTelemetry(run.telemetry, telemetry);
    }
    await this.writeRunLog(runID, logInput.log);
    await this.putRun(run);
    await this.appendRunEventRecord(run, {
      type: "command.finished",
      phase: run.state,
      exitCode: run.exitCode,
    });
    return json({ run: publicRunRecord(run) });
  }

  private async readRunLog(runID: string): Promise<string> {
    const chunks = await this.state.storage.list<string>({ prefix: runLogChunkPrefix(runID) });
    if (chunks.size > 0) {
      return [...chunks.entries()]
        .toSorted(([left], [right]) => left.localeCompare(right))
        .map(([, chunk]) => chunk)
        .join("");
    }
    return (await this.state.storage.get<string>(runLogKey(runID))) ?? "";
  }

  private async writeRunLog(runID: string, log: string): Promise<void> {
    await this.deleteRunLogChunks(runID);
    if (textEncoder.encode(log).byteLength <= runLogChunkBytes) {
      await this.state.storage.put(runLogKey(runID), log);
      return;
    }
    await this.state.storage.put(runLogKey(runID), "");
    const chunks = splitRunLogByBytes(log, runLogChunkBytes);
    await Promise.all(
      chunks.map((chunk, index) => this.state.storage.put(runLogChunkKey(runID, index), chunk)),
    );
  }

  private async deleteRunLogChunks(runID: string): Promise<void> {
    const chunks = await this.state.storage.list<string>({ prefix: runLogChunkPrefix(runID) });
    await Promise.all([...chunks.keys()].map((key) => this.state.storage.delete(key)));
  }

  private async listRuns(request: Request): Promise<Response> {
    const url = new URL(request.url);
    const leaseID = url.searchParams.get("leaseID") ?? "";
    const owner = url.searchParams.get("owner") ?? "";
    const org = orgFilterKey(url);
    if (org === null) {
      return json({ error: "invalid_org_identity" }, { status: 400 });
    }
    const state = url.searchParams.get("state") ?? "";
    const limit = clampLimit(url.searchParams.get("limit"), 50);
    const admin = isAdminRequest(request);
    const runs = await this.recentRuns(limit, async (run) => {
      if (
        (owner && run.owner !== owner) ||
        (org && !orgMatchesForFilter(run.org, org)) ||
        (state && run.state !== state) ||
        (leaseID && run.leaseIDs !== undefined && !this.runReferencesLease(run, leaseID))
      ) {
        return false;
      }
      if (admin && !leaseID) {
        return true;
      }
      let currentLease = await this.ensureRunLeaseAttribution(run);
      if (leaseID && !this.runReferencesLease(run, leaseID)) {
        return false;
      }
      if (!admin && !run.leaseOwners?.length && !currentLease && validLeaseID(run.leaseID)) {
        currentLease = await this.getLease(run.leaseID);
      }
      return admin || this.runReadableToRequest(run, request, currentLease);
    });
    return json({ runs: runs.map(publicRunRecord) });
  }

  private async listExternalRunners(request: Request): Promise<Response> {
    const url = new URL(request.url);
    const provider = url.searchParams.get("provider") ?? "";
    const status = url.searchParams.get("status") ?? "";
    const stale = url.searchParams.get("stale") ?? "";
    const limit = clampLimit(url.searchParams.get("limit"), 100);
    return json({
      runners: (await this.visibleExternalRunners(request))
        .filter((runner) => !provider || runner.provider === provider)
        .filter((runner) => !status || runner.status === status)
        .filter((runner) => {
          if (stale === "true") {
            return runner.stale === true;
          }
          if (stale === "false") {
            return runner.stale !== true;
          }
          return true;
        })
        .toSorted((a, b) => runnerSortTime(b).localeCompare(runnerSortTime(a)))
        .slice(0, limit)
        .map(publicExternalRunnerRecord),
    });
  }

  private async portalExternalRunnerPage(
    request: Request,
    provider: string,
    runnerID: string,
  ): Promise<Response> {
    const admin = isAdminRequest(request);
    const url = new URL(request.url);
    const owner = admin ? url.searchParams.get("owner") : requestOwner(request);
    const org = admin ? orgFilterKey(url) : requestOrg(request, this.env);
    if (org === null) {
      return portalError("Invalid organization", "That organization identifier is invalid.", 400);
    }
    const runner = (await this.visibleExternalRunners(request)).find(
      (candidate) =>
        candidate.provider === provider &&
        candidate.id === runnerID &&
        (!owner || candidate.owner === owner) &&
        (!org || candidate.org === org),
    );
    if (!runner) {
      return portalError(
        "Runner not found",
        "That external runner is not visible to you or has not been synced yet.",
        404,
      );
    }
    return portalExternalRunnerDetail(publicExternalRunnerRecord(runner), { admin });
  }

  private async syncExternalRunners(request: Request): Promise<Response> {
    const owner = requestOwner(request);
    const org = requestOrg(request, this.env);
    const input = await readJson<ExternalRunnerSyncRequest>(request);
    const provider = sanitizeRunnerProvider(input.provider);
    if (!provider) {
      return json({ error: "invalid_provider" }, { status: 400 });
    }
    const rawRunners = Array.isArray(input.runners) ? input.runners : [];
    if (rawRunners.length > maxExternalRunnerSyncItems) {
      return json({ error: "too_many_runners" }, { status: 400 });
    }
    const now = new Date();
    const nowISO = now.toISOString();
    const existing = await this.externalRunnerRecords();
    const seenIDs = new Set<string>();
    const synced: ExternalRunnerRecord[] = [];
    const writes: Promise<void>[] = [];
    for (const raw of rawRunners) {
      const sanitized = sanitizeExternalRunner(raw, provider, now);
      if (!sanitized || seenIDs.has(sanitized.id)) {
        continue;
      }
      seenIDs.add(sanitized.id);
      const previous = existing.find(
        (runner) =>
          runner.provider === provider &&
          runner.id === sanitized.id &&
          runner.owner === owner &&
          runner.org === org,
      );
      const runner: ExternalRunnerRecord = {
        ...previous,
        ...sanitized,
        owner,
        org,
        provider,
        firstSeenAt: previous?.firstSeenAt || nowISO,
        lastSeenAt: nowISO,
        updatedAt: nowISO,
      };
      delete runner.stale;
      writes.push(this.putExternalRunner(runner));
      synced.push(runner);
    }
    const stale: ExternalRunnerRecord[] = [];
    for (const runner of existing) {
      if (
        runner.provider !== provider ||
        runner.owner !== owner ||
        runner.org !== org ||
        seenIDs.has(runner.id) ||
        runner.stale
      ) {
        continue;
      }
      const next: ExternalRunnerRecord = {
        ...runner,
        status: "missing",
        stale: true,
        updatedAt: nowISO,
      };
      writes.push(this.putExternalRunner(next));
      stale.push(next);
    }
    await Promise.all(writes);
    return json({
      runners: synced.map(publicExternalRunnerRecord),
      stale: stale.map(publicExternalRunnerRecord),
    });
  }

  private async usage(request: Request): Promise<Response> {
    const url = new URL(request.url);
    const requestedScope = url.searchParams.get("scope") ?? "user";
    const admin = isAdminRequest(request);
    const scope =
      admin && (requestedScope === "org" || requestedScope === "all" || requestedScope === "user")
        ? requestedScope
        : "user";
    const month = url.searchParams.get("month") ?? new Date().toISOString().slice(0, 7);
    const owner = admin
      ? (url.searchParams.get("owner") ?? requestOwner(request))
      : requestOwner(request);
    const requestedOrg = orgFilterKey(url);
    if (requestedOrg === null) {
      return json({ error: "invalid_org_identity" }, { status: 400 });
    }
    const org = admin
      ? (requestedOrg ?? (scope === "org" ? requestOrg(request, this.env) : undefined))
      : requestOrg(request, this.env);
    const usage = usageSummary(
      await this.leaseRecords(),
      { scope, owner, month, ...(org ? { org } : {}) },
      new Date(),
    );
    return json({ usage, limits: costLimits(this.env) });
  }

  private marketplaceStatus(request: Request): Response {
    return json({
      marketplace: marketplaceStatus(this.env),
      owner: requestOwner(request),
      org: requestOrgLabel(request, this.env),
    });
  }

  private async marketplaceQuote(request: Request): Promise<Response> {
    const status = marketplaceStatus(this.env);
    if (!status.enabled) {
      return json(
        {
          error: "marketplace_disabled",
          message: "marketplace preview is disabled",
          marketplace: status,
        },
        { status: 409 },
      );
    }
    try {
      const input = await readJson<MarketplaceQuoteRequest>(request);
      return json({
        quote: marketplaceQuote(this.env, input),
        marketplace: status,
        owner: requestOwner(request),
        org: requestOrgLabel(request, this.env),
      });
    } catch (error) {
      if (error instanceof MarketplaceInputError) {
        return json({ error: error.code, message: error.message }, { status: 400 });
      }
      throw error;
    }
  }

  private async createImage(request: Request): Promise<Response> {
    const input = await readJson<{
      leaseID?: string;
      id?: string;
      name?: string;
      noReboot?: boolean;
      strategy?: string;
    }>(request);
    const leaseID = input.leaseID ?? input.id ?? "";
    const name = input.name ?? "";
    if (!validLeaseID(leaseID)) {
      return json({ error: "invalid_lease_id" }, { status: 400 });
    }
    if (!validImageName(name)) {
      return json({ error: "invalid_image_name" }, { status: 400 });
    }
    const lease = await this.resolveLease(leaseID, request, true);
    if (!lease) {
      return notFound();
    }
    const managedProvider = managedLeaseProvider(lease);
    if (!managedProvider) {
      return json(
        {
          error: "unsupported_provider",
          message: "registered leases do not support coordinator-managed images",
        },
        { status: 400 },
      );
    }
    const provider = this.provider(managedProvider, lease.region, lease.providerProject);
    if (!lease.cloudID || !provider.supportsNativeImages()) {
      return json(
        {
          error: "unsupported_provider",
          message: provider.nativeImagesUnsupportedMessage(),
        },
        { status: 400 },
      );
    }
    const strategy = checkpointStrategy(input.strategy ?? provider.defaultImageStrategy(lease));
    if (!strategy) {
      return json(
        {
          error: "invalid_strategy",
          message: "checkpoint strategy must be auto, disk-snapshot, or image",
        },
        { status: 400 },
      );
    }
    const unsupportedStrategy = provider.validateLeaseImageStrategy(lease, strategy);
    if (unsupportedStrategy) {
      return json(
        {
          error: "unsupported_strategy",
          message: unsupportedStrategy,
        },
        { status: 400 },
      );
    }
    const image = await provider.createLeaseImage(lease, name, input.noReboot ?? true, strategy);
    return json({ image }, { status: 201 });
  }

  private async imageRoute(request: Request, imageID: string, action?: string): Promise<Response> {
    const method = request.method.toUpperCase();
    const decodedImageID = decodeImageRouteID(imageID);
    if (!validImageRouteID(decodedImageID)) {
      return json({ error: "invalid_image_id" }, { status: 400 });
    }
    const url = new URL(request.url);
    const provider = providerFromQuery(url.searchParams.get("provider"));
    if (!provider) {
      return json(
        { error: "unsupported_provider", message: "image provider must be aws, azure, or gcp" },
        { status: 400 },
      );
    }
    const region = url.searchParams.get("region") ?? undefined;
    const project = url.searchParams.get("project") ?? undefined;
    const kind = url.searchParams.get("kind") ?? undefined;
    const imageProvider = this.provider(provider, region, project);
    const metadata = await imageProvider.storedImageMetadata(decodedImageID);
    const knownRegion = metadata?.region ?? "";
    const providerRegion = region || knownRegion;
    const providerForRegion =
      providerRegion === region ? imageProvider : this.provider(provider, providerRegion, project);
    if (method === "GET" && action === undefined) {
      let image: ProviderImage;
      try {
        image = await providerForRegion.getImage(decodedImageID, kind);
      } catch (error) {
        if (isProviderImageNotFound(error)) {
          return json(
            { error: "not_found", message: `image ${decodedImageID} not found` },
            { status: 404 },
          );
        }
        throw error;
      }
      return json({
        image: providerForRegion.decorateImage(image, metadata),
      });
    }
    if (method === "GET" && (action === "fast-snapshot-restore" || action === "fsr-status")) {
      if (!providerForRegion.fastSnapshotRestoreForImage) {
        return json(
          { error: "unsupported_provider", message: "Fast Snapshot Restore is AWS-only" },
          { status: 400 },
        );
      }
      const result = await providerForRegion.fastSnapshotRestoreForImage(
        decodedImageID,
        metadata,
        url,
      );
      return result instanceof Response ? result : json(result);
    }
    if (method === "DELETE" && action === undefined) {
      const ownershipBlocked = validateProviderImageDeleteOwnership(
        provider,
        decodedImageID,
        metadata,
      );
      if (ownershipBlocked) {
        return json(ownershipBlocked.body, { status: ownershipBlocked.status });
      }
      const deleteBlocked = await providerForRegion.validateDeleteImage(decodedImageID, metadata);
      if (deleteBlocked) {
        return json(deleteBlocked.body, { status: deleteBlocked.status });
      }
      await providerForRegion.deleteImage(decodedImageID, kind);
      return json({ imageID: decodedImageID, deleted: true });
    }
    if (method === "POST" && action === "promote") {
      if (!providerForRegion.promoteImage) {
        return json(
          { error: "unsupported_provider", message: "image promotion is currently AWS-only" },
          { status: 400 },
        );
      }
      const result = await providerForRegion.promoteImage(decodedImageID, metadata, request, url);
      return result instanceof Response ? result : json(result);
    }
    return json({ error: "not_found" }, { status: 404 });
  }

  private async expireLeases(): Promise<void> {
    const claims = await this.state.runExclusive(async () => {
      const now = Date.now();
      const claimed: Array<{ claim: string; lease: LeaseRecord }> = [];
      await this.visitLeaseRecords(async (stored) => {
        if (!leaseIsLive(stored)) {
          this.closeLeaseBridges(stored.id, 1008, "lease ended");
        }
        const workspace = stored.workspaceID
          ? await this.state.storage.get<WorkspaceRecord>(
              workspaceKey(stored.owner, stored.org, stored.workspaceID),
              { noCache: true },
            )
          : undefined;
        if (
          workspace &&
          workspaceLeaseMatchesCleanup(workspace, stored) &&
          workspaceProvisioningNeedsRecovery(workspace, stored, now)
        ) {
          return;
        }
        if (!leaseNeedsCleanup(stored, now)) {
          return;
        }
        const lease = structuredClone(stored);
        const claimExpiresAt = cleanupClaimDeadline(lease);
        if (lease.cleanupStartedAt && claimExpiresAt > now) {
          return;
        }
        const retryAt = Date.parse(lease.cleanupRetryAt ?? "");
        if (Number.isFinite(retryAt) && retryAt > now) {
          return;
        }
        const nowDate = new Date();
        const nowISO = nowDate.toISOString();
        if (isRegisteredLease(lease)) {
          lease.state = "expired";
          lease.updatedAt = nowISO;
          lease.endedAt = nowISO;
          delete lease.releaseDeletesServer;
          clearLeaseCleanupMetadata(lease);
          if (!lease.runtimeAdapterDeleteRequestedAt) {
            clearRuntimeAdapterDeleteMetadata(lease);
          }
          delete lease.cleanupStartedAt;
          delete lease.cleanupClaimExpiresAt;
          await this.putLease(lease, { noCache: true });
          this.closeLeaseBridges(lease.id, 1008, "lease expired");
          return;
        }
        if (lease.state === "provisioning" && !lease.cloudID) {
          lease.state = "failed";
          lease.updatedAt = nowISO;
          lease.endedAt = nowISO;
          lease.cleanupFailedAt = nowISO;
          lease.cleanupError = "lease expired before provider returned a cloud resource";
          await this.putLease(lease, { noCache: true });
          return;
        }
        if (claimed.length >= leaseCleanupBatchSize) {
          return;
        }
        lease.cleanupStartedAt = nowISO;
        lease.cleanupClaimExpiresAt = new Date(
          nowDate.getTime() + leaseCleanupClaimStaleMs,
        ).toISOString();
        lease.updatedAt = nowISO;
        await this.putLease(lease, { noCache: true });
        claimed.push({ claim: nowISO, lease });
      });
      return claimed;
    });
    await Promise.all(
      claims.map(async ({ claim, lease }) => {
        const cleanup = async () => {
          let failure: string | undefined;
          let manualResolution = false;
          try {
            await this.deleteLeaseServer(lease);
          } catch (error) {
            failure = coordinatorErrorMessage(this.env, error);
            manualResolution = error instanceof ProviderCleanupManualResolutionError;
          }
          await this.state.runExclusive(async () => {
            const current = await this.getLease(lease.id);
            if (!current || current.cleanupStartedAt !== claim) {
              return;
            }
            const nowDate = new Date();
            const nowISO = nowDate.toISOString();
            if (failure && manualResolution) {
              terminalizeManualProviderCleanup(current, failure, nowISO);
              await this.putLease(current);
              return;
            }
            if (failure) {
              current.cleanupAttempts = (current.cleanupAttempts ?? 0) + 1;
              delete current.cleanupStartedAt;
              delete current.cleanupClaimExpiresAt;
              current.cleanupError = failure;
              current.cleanupFailedAt = nowISO;
              current.cleanupRetryAt = new Date(
                nowDate.getTime() + leaseCleanupRetryDelayMs,
              ).toISOString();
              current.updatedAt = nowISO;
              await this.putLease(current);
              console.warn(
                `lease cleanup failed lease=${current.id} provider=${current.provider} cloud=${current.cloudID}: ${failure}`,
              );
              return;
            }
            current.state = leaseIsLive(current) ? "expired" : current.state;
            current.updatedAt = nowISO;
            current.endedAt = nowISO;
            if (current.state === "failed" && current.provisioningResourceMayExist) {
              if (!current.failureError && current.cleanupError) {
                current.failureError = current.cleanupError;
              }
              current.provisioningResourceMayExist = false;
              current.provisioningFailureRetryable = false;
            }
            delete current.releaseDeletesServer;
            clearLeaseCleanupMetadata(current);
            delete current.providerKeyCleanupPending;
            delete current.providerKeyCleanupID;
            delete current.cleanupStartedAt;
            delete current.cleanupClaimExpiresAt;
            await this.putLease(current);
            await this.clearWorkspaceReleaseError(current);
            await this.markAWSIngressReconcilePending(current);
          });
        };
        if (managedLeaseProvider(lease) === "aws" && !lease.network?.awsPrivate) {
          await this.withAWSIngressOperationLock(cleanup);
        } else {
          await cleanup();
        }
      }),
    );
  }

  private async scheduleAlarm(): Promise<void> {
    const now = Date.now();
    let alarmTime: number | undefined;
    const retainAlarm = (candidate: number | undefined) => {
      if (candidate === undefined || !Number.isFinite(candidate)) {
        return;
      }
      alarmTime = alarmTime === undefined ? candidate : Math.min(alarmTime, candidate);
    };
    const prewarmEnabled = workspacePrewarmCount(this.env.CRABBOX_WORKSPACE_PREWARM_COUNT) > 0;
    const activeWorkspaceOrgs = new Set<string>();
    await this.visitStorageRecords<WorkspaceRecord>("workspace:", async (workspace) => {
      const lease = await this.getLease(workspace.leaseID, { noCache: true });
      const reconcileAt = workspaceNextReconcileAt(workspace, lease, now);
      if (reconcileAt !== undefined) {
        retainAlarm(Math.max(now + 1, reconcileAt));
      }
      const terminalAt = workspaceTerminalTimestamp(workspace, lease);
      if (terminalAt !== undefined) {
        retainAlarm(Math.max(now + 1, terminalAt + workspaceTerminalRetentionMs));
      }
      if (!prewarmEnabled) {
        return;
      }
      if (!workspace.prewarm) {
        const status = workspaceStatus(workspace, lease);
        if (status === "provisioning" || status === "ready") {
          activeWorkspaceOrgs.add(workspace.org);
        }
        return;
      }
      if (!workspace.releaseRequestedAt && lease?.state === "active") {
        const replacementAt = Date.parse(lease.expiresAt) - workspacePrewarmReplacementLeadMs;
        if (replacementAt > now) {
          retainAlarm(replacementAt);
        }
      }
    });
    if (prewarmEnabled && activeWorkspaceOrgs.size > 0) {
      await this.visitStorageRecords<WorkspaceRecord>("workspace:", async (workspace) => {
        if (!workspace.prewarm || !activeWorkspaceOrgs.has(workspace.org)) {
          return;
        }
        const lease = await this.getLease(workspace.leaseID, { noCache: true });
        if (!workspace.error && lease?.state !== "failed") {
          return;
        }
        const retryAt = Date.parse(workspace.updatedAt) + workspacePrewarmRetryDelayMs;
        if (Number.isFinite(retryAt)) {
          retainAlarm(Math.max(now + 1, retryAt));
        }
      });
    }
    await this.visitLeaseRecords(async (lease) => {
      const workspace = lease.workspaceID
        ? await this.state.storage.get<WorkspaceRecord>(
            workspaceKey(lease.owner, lease.org, lease.workspaceID),
            { noCache: true },
          )
        : undefined;
      if (
        workspace &&
        workspaceLeaseMatchesCleanup(workspace, lease) &&
        workspaceProvisioningNeedsRecovery(workspace, lease, now)
      ) {
        return;
      }
      if (
        leaseIsLive(lease) ||
        lease.runtimeAdapterDeleteRequestedAt ||
        leaseNeedsCleanup(lease, now)
      ) {
        retainAlarm(nextLeaseAlarmTime(lease));
      }
    });
    for (const handoffAlarm of await this.webVNCCredentialHandoffs.alarmTimes(now)) {
      retainAlarm(handoffAlarm);
    }
    const orphanSweepAlarm = await this.nextAWSOrphanSweepAlarmTime();
    retainAlarm(orphanSweepAlarm);
    const azureOrphanSweepAlarm = await this.nextAzureOrphanSweepAlarmTime();
    retainAlarm(azureOrphanSweepAlarm);
    const azureCleanupAlarm = await this.nextAzureDeferredCleanupAlarmTime();
    retainAlarm(azureCleanupAlarm);
    const awsIngressAlarm = await this.nextAWSIngressReconcileAlarmTime();
    retainAlarm(awsIngressAlarm);
    if ((await this.state.storage.get<string>(runPruneCursorKey)) !== undefined) {
      retainAlarm(now + 1000);
    }
    if (alarmTime === undefined) {
      await this.state.clearAlarm();
      return;
    }
    await this.state.scheduleAlarm(alarmTime);
  }

  private async nextAWSIngressReconcileAlarmTime(): Promise<number | undefined> {
    const record = await this.state.storage.get<StoredAWSIngressReconcileRecord>(
      awsIngressReconcileRecordKey,
    );
    const retryTimes = awsIngressReconcileTargets(record)
      .map((target) => Date.parse(target.retryAt))
      .filter((time) => Number.isFinite(time));
    return retryTimes.length > 0 ? Math.max(Date.now() + 1000, Math.min(...retryTimes)) : undefined;
  }

  private async markAWSIngressReconcilePending(anchor: LeaseRecord): Promise<void> {
    if (!leaseHasPublishedAWSAccess(anchor) || isRegisteredLease(anchor)) {
      return;
    }
    const current = await this.state.storage.get<StoredAWSIngressReconcileRecord>(
      awsIngressReconcileRecordKey,
    );
    const targets = awsIngressReconcileTargets(current);
    const key = awsIngressReconcileTargetKey(anchor);
    const previous = targets.find((target) => awsIngressReconcileTargetKey(target.anchor) === key);
    const now = new Date().toISOString();
    await this.state.storage.put<AWSIngressReconcileRecord>(awsIngressReconcileRecordKey, {
      targets: [
        ...targets.filter((target) => awsIngressReconcileTargetKey(target.anchor) !== key),
        {
          anchor: structuredClone(anchor),
          attempts: previous?.attempts ?? 0,
          generation: crypto.randomUUID(),
          updatedAt: now,
          retryAt: now,
          ...(previous?.lastError ? { lastError: previous.lastError } : {}),
        },
      ],
    });
  }

  private async reconcileAWSIngressIfIdle(): Promise<void> {
    const work = await this.state.runExclusive(async () => {
      const stored = await this.state.storage.get<StoredAWSIngressReconcileRecord>(
        awsIngressReconcileRecordKey,
      );
      const targets = awsIngressReconcileTargets(stored);
      if (targets.length === 0) {
        if (stored) {
          await this.state.storage.delete(awsIngressReconcileRecordKey);
        }
        return undefined;
      }
      const now = Date.now();
      const due = targets.filter((target) => {
        const retryAt = Date.parse(target.retryAt);
        return !Number.isFinite(retryAt) || retryAt <= now;
      });
      if (due.length === 0) {
        return undefined;
      }
      const accessState = await this.providerAccessLeaseRecords();
      if (
        accessState.some(
          (lease) =>
            lease.provider === "aws" && !isRegisteredLease(lease) && lease.state === "provisioning",
        )
      ) {
        const retryAt = new Date(now + 10_000).toISOString();
        await this.state.storage.put<AWSIngressReconcileRecord>(awsIngressReconcileRecordKey, {
          targets: targets.map((target) =>
            due.includes(target) ? { ...target, retryAt } : target,
          ),
        });
        return undefined;
      }
      return structuredClone(due);
    });
    if (!work) {
      return;
    }
    for (const target of work) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- one fence protects all shared AWS ingress mutations.
      await this.withAWSIngressOperationLock(async () => {
        const fresh = await this.state.runExclusive(async () => {
          const stored = await this.state.storage.get<StoredAWSIngressReconcileRecord>(
            awsIngressReconcileRecordKey,
          );
          const targets = awsIngressReconcileTargets(stored);
          const key = awsIngressReconcileTargetKey(target.anchor);
          const current = targets.find(
            (candidate) =>
              awsIngressReconcileTargetKey(candidate.anchor) === key &&
              candidate.generation === target.generation,
          );
          if (!current) {
            return undefined;
          }
          const accessState = await this.providerAccessLeaseRecords();
          if (
            accessState.some(
              (lease) =>
                lease.provider === "aws" &&
                !isRegisteredLease(lease) &&
                lease.state === "provisioning",
            )
          ) {
            current.retryAt = new Date(Date.now() + 10_000).toISOString();
            await this.state.storage.put<AWSIngressReconcileRecord>(awsIngressReconcileRecordKey, {
              targets,
            });
            return undefined;
          }
          return {
            accessState: structuredClone(accessState),
            target: structuredClone(current),
          };
        });
        if (!fresh) {
          return;
        }
        const key = awsIngressReconcileTargetKey(fresh.target.anchor);
        let failure: string | undefined;
        if (fresh.target.anchor.provider === "aws" && !isRegisteredLease(fresh.target.anchor)) {
          const provider = this.provider("aws", fresh.target.anchor.region);
          try {
            await provider.reconcileLeaseAccess?.(
              fresh.target.anchor,
              providerAccessContext([], fresh.accessState),
            );
          } catch (error) {
            failure = coordinatorErrorMessage(this.env, error);
            console.warn(
              `AWS SSH ingress reconciliation failed lease=${fresh.target.anchor.id} region=${fresh.target.anchor.region ?? ""}: ${failure}`,
            );
          }
        }
        await this.state.runExclusive(async () => {
          const stored = await this.state.storage.get<StoredAWSIngressReconcileRecord>(
            awsIngressReconcileRecordKey,
          );
          const targets = awsIngressReconcileTargets(stored);
          const index = targets.findIndex(
            (candidate) =>
              awsIngressReconcileTargetKey(candidate.anchor) === key &&
              candidate.generation === fresh.target.generation,
          );
          if (index < 0) {
            return;
          }
          if (failure) {
            const now = new Date();
            targets[index] = {
              ...targets[index]!,
              attempts: targets[index]!.attempts + 1,
              updatedAt: now.toISOString(),
              retryAt: new Date(now.getTime() + leaseCleanupRetryDelayMs).toISOString(),
              lastError: failure,
            };
          } else {
            targets.splice(index, 1);
          }
          if (targets.length === 0) {
            await this.state.storage.delete(awsIngressReconcileRecordKey);
          } else {
            await this.state.storage.put<AWSIngressReconcileRecord>(awsIngressReconcileRecordKey, {
              targets,
            });
          }
        });
      });
    }
  }

  private async nextAzureDeferredCleanupAlarmTime(): Promise<number | undefined> {
    const records = await this.state.storage.list<AzureDeferredCleanupRecord>({
      prefix: azureDeferredCleanupPrefix,
    });
    const times = [...records.values()]
      .filter((record) => !record.terminalAt)
      .map((record) => Date.parse(record.retryAt))
      .filter((time) => Number.isFinite(time));
    if (times.length === 0) {
      return undefined;
    }
    return Math.max(Date.now() + 1000, Math.min(...times));
  }

  private async runAzureDeferredCleanups(): Promise<void> {
    const records = await this.state.runExclusive(() =>
      this.state.storage.list<AzureDeferredCleanupRecord>({
        prefix: azureDeferredCleanupPrefix,
      }),
    );
    const now = Date.now();
    await Promise.all(
      [...records.entries()].map(async ([key, record]) => {
        if (record.terminalAt) {
          return;
        }
        const retryAt = Date.parse(record.retryAt);
        if (Number.isFinite(retryAt) && retryAt > now) {
          return;
        }
        try {
          const lease = azureDeferredCleanupLease(record);
          const scope = azureProviderScope(lease.providerScope)!;
          await new AzureClient(this.env, {
            location: record.location,
            subscription: scope.subscription,
            resourceGroup: scope.resourceGroup,
            ownedDeleteClaimStorage: this.state.storage,
          }).deleteOwnedServer(lease);
        } catch (error) {
          await this.state.runExclusive(async () => {
            const current = await this.state.storage.get<AzureDeferredCleanupRecord>(key);
            if (!current || current.updatedAt !== record.updatedAt) {
              return;
            }
            const terminal = error instanceof ProviderCleanupManualResolutionError;
            const nextRecord: AzureDeferredCleanupRecord = {
              ...current,
              attempts: current.attempts + 1,
              updatedAt: new Date().toISOString(),
              retryAt: terminal
                ? current.retryAt
                : new Date(now + leaseCleanupRetryDelayMs).toISOString(),
              lastError: coordinatorErrorMessage(this.env, error),
            };
            if (terminal) {
              nextRecord.terminalAt = nextRecord.updatedAt;
            }
            await this.state.storage.put(key, nextRecord);
            console.warn(
              `azure deferred cleanup failed name=${record.name} location=${record.location}: ${nextRecord.lastError}`,
            );
          });
          return;
        }
        await this.state.runExclusive(async () => {
          const current = await this.state.storage.get<AzureDeferredCleanupRecord>(key);
          if (current?.updatedAt === record.updatedAt) {
            await this.state.storage.delete(key);
          }
        });
      }),
    );
  }

  private async nextAWSOrphanSweepAlarmTime(): Promise<number | undefined> {
    const config = this.awsOrphanSweepConfig();
    if (!config.enabled) {
      return undefined;
    }
    const lastRun = await this.state.storage.get<AWSOrphanSweepRecord>(awsOrphanSweepRecordKey);
    const lastFinishedAt = Date.parse(lastRun?.finishedAt ?? "");
    const now = Date.now();
    if (!Number.isFinite(lastFinishedAt)) {
      const stored = await this.state.storage.get<number>(awsOrphanSweepFirstAlarmKey);
      if (typeof stored === "number" && Number.isFinite(stored)) {
        return Math.max(now + 1000, stored);
      }
      const next = now + Math.min(config.intervalSeconds * 1000, awsOrphanSweepInitialDelayMs);
      await this.state.storage.put(awsOrphanSweepFirstAlarmKey, next);
      return next;
    }
    return Math.max(now + 1000, lastFinishedAt + config.intervalSeconds * 1000);
  }

  private async runAWSOrphanSweepIfDue(
    trigger: "alarm" | "admin",
    requestedConfig?: AWSOrphanSweepConfig,
  ): Promise<AWSOrphanSweepRecord | undefined> {
    return this.withProviderMaintenanceLock(async () => {
      const config = requestedConfig ?? this.awsOrphanSweepConfig();
      if (!config.enabled) {
        return undefined;
      }
      const lastRun = await this.state.runExclusive(() =>
        this.state.storage.get<AWSOrphanSweepRecord>(awsOrphanSweepRecordKey),
      );
      const lastFinishedAt = Date.parse(lastRun?.finishedAt ?? "");
      if (
        trigger !== "admin" &&
        Number.isFinite(lastFinishedAt) &&
        Date.now() < lastFinishedAt + config.intervalSeconds * 1000
      ) {
        return undefined;
      }
      return await this.runAWSOrphanSweep(trigger, config);
    });
  }

  private async runAWSOrphanSweep(
    trigger: "alarm" | "admin",
    config = this.awsOrphanSweepConfig(),
  ): Promise<AWSOrphanSweepRecord> {
    const startedAt = new Date().toISOString();
    const now = Date.now();
    const candidates: AWSOrphanSweepCandidate[] = [];
    const macHostCandidates: AWSMacHostSweepCandidate[] = [];
    const errors: AWSOrphanSweepRecord["errors"] = [];
    const inventory: Array<{ machine: ProviderMachine; region: string }> = [];
    const macHostInventory: Array<{ client: EC2SpotClient; host: AWSMacHost; region: string }> = [];
    let scanned = 0;
    let macHostsScanned = 0;
    for (const region of config.regions) {
      try {
        // oxlint-disable-next-line eslint/no-await-in-loop -- regions are swept independently.
        const machines = await this.provider("aws", region).listCrabboxServers();
        scanned += machines.length;
        inventory.push(...machines.map((machine) => ({ machine, region })));
        if (config.macHostReleaseEnabled) {
          const client = new EC2SpotClient(this.env, region);
          // oxlint-disable-next-line eslint/no-await-in-loop -- keep host cleanup attached to its region.
          const macHosts = await client.listMacHosts();
          macHostsScanned += macHosts.length;
          macHostInventory.push(...macHosts.map((host) => ({ client, host, region })));
        }
      } catch (error) {
        const message = coordinatorErrorMessage(this.env, error);
        errors.push({ region, message });
        console.warn(`aws orphan sweep failed region=${region}: ${message}`);
      }
    }
    const inventoryKeys = new Set(
      inventory.map(({ machine, region }) =>
        cloudOrphanSweepResourceKey(machine.cloudID || machine.name || String(machine.id), region),
      ),
    );
    const macHostKeys = new Set(
      macHostInventory.map(({ host, region }) => cloudOrphanSweepResourceKey(host.id, region)),
    );
    const activeAWSLeases: LeaseRecord[] = [];
    const ownershipLeases = new Map<string, LeaseRecord>();
    const macHostOwnershipLeases = new Map<string, LeaseRecord>();
    await this.state.runExclusive(() =>
      this.visitLeaseRecords((lease) => {
        if (lease.provider !== "aws" || isRegisteredLease(lease)) {
          return;
        }
        if (leaseOwnsCloudResourceDuringSweep(lease, now)) {
          activeAWSLeases.push(lease);
          return;
        }
        if (lease.keep || !lease.region) {
          return;
        }
        if (lease.cloudID && lease.releaseDeletesServer !== false) {
          const key = cloudOrphanSweepResourceKey(lease.cloudID, lease.region);
          if (inventoryKeys.has(key) && !ownershipLeases.has(key)) {
            ownershipLeases.set(key, lease);
          }
        }
        const hostID = leaseHostID(lease);
        if (hostID) {
          const key = cloudOrphanSweepResourceKey(hostID, lease.region);
          if (macHostKeys.has(key) && !macHostOwnershipLeases.has(key)) {
            macHostOwnershipLeases.set(key, lease);
          }
        }
      }),
    );
    const activeLeases = new Map(activeAWSLeases.map((lease) => [lease.id, lease]));
    const activeCloudIDs = new Set(activeAWSLeases.map((lease) => lease.cloudID).filter(Boolean));
    for (const { machine, region } of inventory) {
      const candidate = awsOrphanSweepCandidate(
        machine,
        activeLeases,
        activeCloudIDs,
        region,
        config.graceSeconds,
      );
      if (!candidate) {
        continue;
      }
      const cloudID = machine.cloudID || machine.name || String(machine.id);
      const ownershipLease = ownershipLeases.get(cloudOrphanSweepResourceKey(cloudID, region));
      recordCloudOrphanSweepOwnership(candidate, ownershipLease);
      if (config.deleteEnabled && ownershipLease) {
        try {
          // oxlint-disable-next-line eslint/no-await-in-loop -- delete failures must stay attached to the candidate.
          await this.provider("aws", region).deleteServer(machine.cloudID || String(machine.id));
          candidate.action = "terminated";
        } catch (error) {
          candidate.action = "terminate_failed";
          candidate.error = coordinatorErrorMessage(this.env, error);
          console.warn(
            `aws orphan sweep terminate failed region=${region} cloud=${machine.cloudID}: ${candidate.error}`,
          );
        }
      }
      candidates.push(candidate);
    }
    for (const { client, host, region } of macHostInventory) {
      const candidate = awsMacHostSweepCandidate(
        host,
        activeAWSLeases,
        region,
        Math.max(config.graceSeconds, 3600),
      );
      if (!candidate) {
        continue;
      }
      const ownershipLease = macHostOwnershipLeases.get(
        cloudOrphanSweepResourceKey(host.id, region),
      );
      recordAWSMacHostSweepOwnership(candidate, ownershipLease);
      if (config.deleteEnabled && ownershipLease) {
        try {
          // oxlint-disable-next-line eslint/no-await-in-loop -- release failures must stay attached to the host.
          await client.releaseMacHost(host.id);
          candidate.action = "released";
        } catch (error) {
          candidate.action = "release_failed";
          candidate.error = coordinatorErrorMessage(this.env, error);
          console.warn(
            `aws orphan sweep mac host release failed region=${region} host=${host.id}: ${candidate.error}`,
          );
        }
      }
      macHostCandidates.push(candidate);
    }
    const finishedAt = new Date().toISOString();
    const record: AWSOrphanSweepRecord = {
      startedAt,
      finishedAt,
      mode: config.deleteEnabled ? "delete" : "report",
      trigger,
      enabled: config.enabled,
      regions: config.regions,
      scanned,
      candidates,
      terminated: candidates.filter((candidate) => candidate.action === "terminated").length,
      macHostsScanned,
      macHostCandidates,
      macHostsReleased: macHostCandidates.filter((candidate) => candidate.action === "released")
        .length,
      errors,
      nextRunAt: new Date(Date.parse(finishedAt) + config.intervalSeconds * 1000).toISOString(),
    };
    await this.state.runExclusive(async () => {
      await this.state.storage.put(awsOrphanSweepRecordKey, record);
      await this.state.storage.delete(awsOrphanSweepFirstAlarmKey);
    });
    if (candidates.length > 0 || macHostCandidates.length > 0 || errors.length > 0) {
      console.warn(
        `aws orphan sweep mode=${record.mode} scanned=${record.scanned} candidates=${candidates.length} terminated=${record.terminated} mac_hosts=${macHostCandidates.length} mac_hosts_released=${record.macHostsReleased ?? 0} errors=${errors.length}`,
      );
    }
    return record;
  }

  private awsOrphanSweepConfig(): AWSOrphanSweepConfig {
    const hasAWSCredentials = awsOrphanSweepCredentialsConfigured(this.env);
    const enabled =
      hasAWSCredentials && !envFlagDisabled(this.env.CRABBOX_AWS_ORPHAN_SWEEP_ENABLED);
    return {
      enabled,
      deleteEnabled: enabled && envFlagEnabled(this.env.CRABBOX_AWS_ORPHAN_SWEEP_DELETE),
      macHostReleaseEnabled:
        enabled &&
        envFlagEnabled(this.env.CRABBOX_AWS_ORPHAN_SWEEP_DELETE) &&
        envFlagEnabled(this.env.CRABBOX_AWS_MAC_HOST_SWEEP_RELEASE),
      intervalSeconds: positiveEnvInt(
        this.env.CRABBOX_AWS_ORPHAN_SWEEP_INTERVAL_SECONDS,
        defaultAWSOrphanSweepIntervalSeconds,
      ),
      graceSeconds: positiveEnvInt(
        this.env.CRABBOX_AWS_ORPHAN_SWEEP_GRACE_SECONDS,
        defaultAWSOrphanSweepGraceSeconds,
      ),
      regions: awsRegionCandidates(
        { awsRegion: "", capacityRegions: [] },
        this.env,
        this.env.CRABBOX_AWS_REGION || "eu-west-1",
      ),
    };
  }

  private async nextAzureOrphanSweepAlarmTime(): Promise<number | undefined> {
    const config = this.azureOrphanSweepConfig();
    if (!config.enabled) {
      return undefined;
    }
    const lastRun = await this.state.storage.get<AzureOrphanSweepRecord>(azureOrphanSweepRecordKey);
    const lastFinishedAt = Date.parse(lastRun?.finishedAt ?? "");
    const now = Date.now();
    if (!Number.isFinite(lastFinishedAt)) {
      const stored = await this.state.storage.get<number>(azureOrphanSweepFirstAlarmKey);
      if (typeof stored === "number" && Number.isFinite(stored)) {
        return Math.max(now + 1000, stored);
      }
      const next = now + Math.min(config.intervalSeconds * 1000, azureOrphanSweepInitialDelayMs);
      await this.state.storage.put(azureOrphanSweepFirstAlarmKey, next);
      return next;
    }
    return Math.max(now + 1000, lastFinishedAt + config.intervalSeconds * 1000);
  }

  private async runAzureOrphanSweepIfDue(
    trigger: "alarm" | "admin",
    requestedConfig?: AzureOrphanSweepConfig,
  ): Promise<AzureOrphanSweepRecord | undefined> {
    return this.withProviderMaintenanceLock(async () => {
      const config = requestedConfig ?? this.azureOrphanSweepConfig();
      if (!config.enabled) {
        return undefined;
      }
      const lastRun = await this.state.runExclusive(() =>
        this.state.storage.get<AzureOrphanSweepRecord>(azureOrphanSweepRecordKey),
      );
      const lastFinishedAt = Date.parse(lastRun?.finishedAt ?? "");
      if (
        trigger !== "admin" &&
        Number.isFinite(lastFinishedAt) &&
        Date.now() < lastFinishedAt + config.intervalSeconds * 1000
      ) {
        return undefined;
      }
      return await this.runAzureOrphanSweep(trigger, config);
    });
  }

  private async runAzureOrphanSweep(
    trigger: "alarm" | "admin",
    config = this.azureOrphanSweepConfig(),
  ): Promise<AzureOrphanSweepRecord> {
    const startedAt = new Date().toISOString();
    const now = Date.now();
    const candidates: AzureOrphanSweepCandidate[] = [];
    const errors: AzureOrphanSweepRecord["errors"] = [];
    const seenCloudIDs = new Set<string>();
    const inventory: Array<{ machine: ProviderMachine; region: string }> = [];
    let scanned = 0;
    for (const region of config.regions) {
      try {
        // oxlint-disable-next-line eslint/no-await-in-loop -- regions are swept independently.
        const machines = await this.provider("azure", region).listCrabboxServers();
        for (const machine of machines) {
          const cloudID = machine.cloudID || machine.name || String(machine.id);
          if (seenCloudIDs.has(cloudID)) {
            continue;
          }
          seenCloudIDs.add(cloudID);
          scanned += 1;
          const candidateRegion = machine.region || region;
          inventory.push({ machine, region: candidateRegion });
        }
      } catch (error) {
        const message = coordinatorErrorMessage(this.env, error);
        errors.push({ region, message });
        console.warn(`azure orphan sweep failed region=${region}: ${message}`);
      }
    }
    const inventoryKeys = new Set(
      inventory.map(({ machine, region }) =>
        cloudOrphanSweepResourceKey(machine.cloudID || machine.name || String(machine.id), region),
      ),
    );
    const liveLeases = new Map<string, LeaseRecord>();
    const liveCloudIDs = new Set<string>();
    const ownershipLeases = new Map<string, LeaseRecord>();
    await this.state.runExclusive(() =>
      this.visitLeaseRecords((lease) => {
        if (lease.provider !== "azure" || isRegisteredLease(lease)) {
          return;
        }
        if (leaseOwnsCloudResourceDuringSweep(lease, now)) {
          liveLeases.set(lease.id, lease);
          if (lease.cloudID) {
            liveCloudIDs.add(lease.cloudID);
          }
          return;
        }
        if (!lease.cloudID || !lease.region || lease.keep || lease.releaseDeletesServer === false) {
          return;
        }
        const resourceKey = cloudOrphanSweepResourceKey(lease.cloudID, lease.region);
        if (inventoryKeys.has(resourceKey) && !ownershipLeases.has(resourceKey)) {
          ownershipLeases.set(resourceKey, lease);
        }
      }),
    );
    for (const { machine, region } of inventory) {
      const cloudID = machine.cloudID || machine.name || String(machine.id);
      const candidate = cloudOrphanSweepCandidate(
        machine,
        liveLeases,
        liveCloudIDs,
        region,
        config.graceSeconds,
      );
      if (!candidate) {
        continue;
      }
      const ownershipLease = ownershipLeases.get(cloudOrphanSweepResourceKey(cloudID, region));
      recordCloudOrphanSweepOwnership(candidate, ownershipLease);
      if (config.deleteEnabled && ownershipLease) {
        try {
          const provider = this.provider("azure", region);
          if (!provider.deleteOwnedServer) {
            throw new ProviderCleanupManualResolutionError(
              `refusing to delete Azure lease ${ownershipLease.id}: exact owned-delete support is unavailable`,
            );
          }
          // oxlint-disable-next-line eslint/no-await-in-loop -- delete failures must stay attached to the candidate.
          await provider.deleteOwnedServer(ownershipLease);
          candidate.action = "terminated";
        } catch (error) {
          candidate.action = "terminate_failed";
          candidate.error = coordinatorErrorMessage(this.env, error);
          console.warn(
            `azure orphan sweep terminate failed region=${region} cloud=${machine.cloudID}: ${candidate.error}`,
          );
        }
      }
      candidates.push(candidate);
    }
    const finishedAt = new Date().toISOString();
    const record: AzureOrphanSweepRecord = {
      startedAt,
      finishedAt,
      mode: config.deleteEnabled ? "delete" : "report",
      trigger,
      enabled: config.enabled,
      regions: config.regions,
      scanned,
      candidates,
      terminated: candidates.filter((candidate) => candidate.action === "terminated").length,
      errors,
      nextRunAt: new Date(Date.parse(finishedAt) + config.intervalSeconds * 1000).toISOString(),
    };
    await this.state.runExclusive(async () => {
      await this.state.storage.put(azureOrphanSweepRecordKey, record);
      await this.state.storage.delete(azureOrphanSweepFirstAlarmKey);
    });
    if (candidates.length > 0 || errors.length > 0) {
      console.warn(
        `azure orphan sweep mode=${record.mode} scanned=${record.scanned} candidates=${candidates.length} terminated=${record.terminated} errors=${errors.length}`,
      );
    }
    return record;
  }

  private azureOrphanSweepConfig(): AzureOrphanSweepConfig {
    const hasAzureCredentials = Boolean(
      this.env.AZURE_TENANT_ID &&
      this.env.AZURE_CLIENT_ID &&
      this.env.AZURE_CLIENT_SECRET &&
      this.env.AZURE_SUBSCRIPTION_ID,
    );
    const enabled =
      hasAzureCredentials && !envFlagDisabled(this.env.CRABBOX_AZURE_ORPHAN_SWEEP_ENABLED);
    return {
      enabled,
      deleteEnabled: enabled && envFlagEnabled(this.env.CRABBOX_AZURE_ORPHAN_SWEEP_DELETE),
      intervalSeconds: positiveEnvInt(
        this.env.CRABBOX_AZURE_ORPHAN_SWEEP_INTERVAL_SECONDS,
        defaultAzureOrphanSweepIntervalSeconds,
      ),
      graceSeconds: positiveEnvInt(
        this.env.CRABBOX_AZURE_ORPHAN_SWEEP_GRACE_SECONDS,
        defaultAzureOrphanSweepGraceSeconds,
      ),
      regions: azureRegionCandidates(
        { azureLocation: "", capacityRegions: [] },
        this.env,
        this.env.CRABBOX_AZURE_LOCATION || "eastus",
      ),
    };
  }

  private async getLease(
    leaseID: string,
    options?: { noCache?: boolean },
  ): Promise<LeaseRecord | undefined> {
    return this.state.storage.get<LeaseRecord>(leaseKey(leaseID), options);
  }

  private async resolveLease(
    identifier: string,
    request: Request,
    admin: boolean,
  ): Promise<LeaseRecord | undefined> {
    const exact = await this.getLease(identifier);
    if (exact) {
      return this.leaseVisibleToRequest(exact, request, admin) ? exact : undefined;
    }
    const slug = normalizeLeaseSlug(identifier);
    if (!slug) {
      return undefined;
    }
    const now = Date.now();
    let matches = (await this.leaseRecords()).filter(
      (lease) =>
        leaseIsLive(lease) &&
        Date.parse(lease.expiresAt) > now &&
        normalizeLeaseSlug(lease.slug) === slug,
    );
    if (!admin) {
      matches = matches.filter((lease) => this.leaseVisibleToRequest(lease, request, false));
    }
    if (matches.length > 1) {
      throw new Error(
        `ambiguous slug ${slug}: ${matches.map((lease) => `${lease.id}:${lease.owner}`).join(", ")}`,
      );
    }
    return matches[0];
  }

  private async resolveLeaseForControl(
    identifier: string,
    attachment: Extract<BridgeAttachment, { kind: "control" }>,
  ): Promise<LeaseRecord | undefined> {
    const exact = await this.getLease(identifier);
    if (exact) {
      return this.leaseVisibleToControl(exact, attachment) ? exact : undefined;
    }
    const slug = normalizeLeaseSlug(identifier);
    if (!slug) {
      return undefined;
    }
    const now = Date.now();
    const matches = (await this.leaseRecords()).filter(
      (lease) =>
        leaseIsLive(lease) &&
        Date.parse(lease.expiresAt) > now &&
        normalizeLeaseSlug(lease.slug) === slug &&
        this.leaseVisibleToControl(lease, attachment),
    );
    if (matches.length > 1) {
      throw new Error(
        `ambiguous slug ${slug}: ${matches.map((lease) => `${lease.id}:${lease.owner}`).join(", ")}`,
      );
    }
    return matches[0];
  }

  private async leaseRecords(): Promise<LeaseRecord[]> {
    const leases = await this.state.storage.list<LeaseRecord>({ prefix: "lease:" });
    return [...leases.values()];
  }

  private async retainedMacHostLease(
    owner: string,
    org: string,
    hostID: string,
    serverType?: string,
  ): Promise<LeaseRecord | undefined> {
    let retained: LeaseRecord | undefined;
    await this.visitLeaseRecords((lease) => {
      if (
        lease.state !== "released" ||
        lease.releaseDeletesServer !== false ||
        !lease.cloudID ||
        lease.owner !== owner ||
        lease.org !== org ||
        lease.provider !== "aws" ||
        lease.target !== "macos" ||
        leaseHostID(lease) !== hostID ||
        (serverType !== undefined && lease.serverType !== serverType) ||
        (retained !== undefined && retained.updatedAt >= lease.updatedAt)
      ) {
        return;
      }
      retained = lease;
    });
    return retained;
  }

  private async leaseAdmissionState(
    candidate: Pick<LeaseRecord, "owner" | "org">,
    now: Date,
    excludedLeaseID?: string,
  ): Promise<{ accessLeases: LeaseRecord[]; costUsage: CostLimitUsage }> {
    const providerAccessLeases = await this.providerAccessRecords();
    const providerAccessByID = new Map(providerAccessLeases.map((lease) => [lease.id, lease]));
    const accessLeases: LeaseRecord[] = [];
    const costUsage = createCostLimitUsage(candidate, now);
    const addLease = (lease: LeaseRecord) => {
      if (lease.id !== excludedLeaseID) {
        addLeaseToCostLimitUsage(costUsage, lease, now);
      }
      if (leaseIsLive(lease) || leaseOwnsAWSSSHAccess(lease)) {
        accessLeases.push(lease);
      }
    };
    await this.visitLeaseRecords((lease) => {
      if (!providerAccessByID.has(lease.id)) {
        addLease(lease);
      }
    });
    for (const lease of providerAccessByID.values()) {
      addLease(lease);
    }
    return { accessLeases, costUsage };
  }

  private async providerAccessLeaseRecords(): Promise<LeaseRecord[]> {
    const providerAccessLeases = await this.providerAccessRecords();
    const providerAccessByID = new Map(providerAccessLeases.map((lease) => [lease.id, lease]));
    const accessLeases: LeaseRecord[] = [];
    await this.visitLeaseRecords((lease) => {
      if (
        !providerAccessByID.has(lease.id) &&
        (leaseIsLive(lease) || leaseOwnsAWSSSHAccess(lease))
      ) {
        accessLeases.push(lease);
      }
    });
    accessLeases.push(...providerAccessByID.values());
    return accessLeases;
  }

  private async visitLeaseRecords(
    visitor: (lease: LeaseRecord) => Promise<boolean | void> | boolean | void,
  ): Promise<void> {
    await this.visitStorageRecords("lease:", visitor);
  }

  private async visitStorageRecords<T>(
    prefix: string,
    visitor: (record: T, key: string) => Promise<boolean | void> | boolean | void,
  ): Promise<void> {
    let startAfter: string | undefined;
    for (;;) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- each bounded page starts after the previous page.
      const page = await this.state.storage.list<T>({
        prefix,
        limit: storageRecordScanBatchSize,
        noCache: true,
        ...(startAfter ? { startAfter } : {}),
      });
      if (page.size === 0) {
        break;
      }
      for (const [key, record] of page) {
        // oxlint-disable-next-line eslint/no-await-in-loop -- sequential visits bound legacy record hydration.
        const shouldContinue = await visitor(record, key);
        if (shouldContinue === false) {
          return;
        }
      }
      const nextStartAfter = [...page.keys()].at(-1);
      if (!nextStartAfter || nextStartAfter === startAfter) {
        throw new Error(`${prefix} record scan did not advance`);
      }
      startAfter = nextStartAfter;
      if (page.size < storageRecordScanBatchSize) {
        break;
      }
    }
  }

  private async providerAccessRecords(): Promise<LeaseRecord[]> {
    const now = Date.now();
    const active: LeaseRecord[] = [];
    await this.visitStorageRecords<LeaseRecord>(providerAccessPrefix(), async (record, key) => {
      if (leaseIsLive(record) && Date.parse(record.expiresAt) > now) {
        active.push(record);
        return;
      }
      await this.state.storage.delete(key);
    });
    return active;
  }

  private async readyPoolEntries(): Promise<ReadyPoolEntry[]> {
    const entries = await this.state.storage.list<ReadyPoolEntry>({ prefix: readyPoolPrefix });
    return [...entries.values()];
  }

  private async getReadyPoolEntry(
    key: string,
    leaseID: string,
  ): Promise<ReadyPoolEntry | undefined> {
    return this.state.storage.get<ReadyPoolEntry>(readyPoolKey(key, leaseID));
  }

  private async putReadyPoolEntry(entry: ReadyPoolEntry): Promise<void> {
    await this.state.storage.put(readyPoolKey(entry.key, entry.leaseID), entry);
  }

  private async deleteReadyPoolEntry(entry: ReadyPoolEntry): Promise<void> {
    await this.state.storage.delete(readyPoolKey(entry.key, entry.leaseID));
  }

  private async withReadyPoolBorrowLock<T>(operation: () => Promise<T>): Promise<T> {
    let release!: () => void;
    const previous = this.readyPoolBorrowQueue.catch(() => {});
    const next = new Promise<void>((resolve) => {
      release = resolve;
    });
    this.readyPoolBorrowQueue = previous.then(() => next);
    await previous;
    try {
      return await operation();
    } finally {
      release();
    }
  }

  private async withBridgeTicketLock<T>(operation: () => Promise<T>): Promise<T> {
    let release!: () => void;
    const previous = this.bridgeTicketQueue.catch(() => {});
    const next = new Promise<void>((resolve) => {
      release = resolve;
    });
    this.bridgeTicketQueue = previous.then(() => next);
    await previous;
    try {
      return await operation();
    } finally {
      release();
    }
  }

  private async withAWSIngressOperationLock<T>(operation: () => Promise<T>): Promise<T> {
    let release!: () => void;
    const previous = this.awsIngressBarrier.catch(() => {});
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    this.awsIngressBarrier = previous.then(() => gate);
    await previous;
    await Promise.all(this.awsIngressAdditiveOperations);
    try {
      return await operation();
    } finally {
      release();
    }
  }

  private async withAWSIngressAdditiveOperation<T>(operation: () => Promise<T>): Promise<T> {
    let release!: () => void;
    let active!: Promise<void>;
    for (;;) {
      const barrier = this.awsIngressBarrier;
      // oxlint-disable-next-line eslint/no-await-in-loop -- retry only when an authoritative fence arrived first.
      await barrier.catch(() => {});
      if (barrier !== this.awsIngressBarrier) {
        continue;
      }
      active = new Promise<void>((resolve) => {
        release = resolve;
      });
      this.awsIngressAdditiveOperations.add(active);
      break;
    }
    try {
      return await operation();
    } finally {
      this.awsIngressAdditiveOperations.delete(active);
      release();
    }
  }

  private async withProviderMaintenanceLock<T>(operation: () => Promise<T>): Promise<T> {
    let release!: () => void;
    const previous = this.providerMaintenanceQueue.catch(() => {});
    const next = new Promise<void>((resolve) => {
      release = resolve;
    });
    this.providerMaintenanceQueue = previous.then(() => next);
    await previous;
    try {
      return await operation();
    } finally {
      release();
    }
  }

  private async recentRuns(
    limit: number,
    accept: (run: RunRecord) => Promise<boolean>,
  ): Promise<RunRecord[]> {
    const recent: RunRecord[] = [];
    await this.visitStorageRecords<RunRecord>("run:", async (run) => {
      if (await accept(run)) {
        retainRecentRun(recent, run, limit);
      }
    });
    return recent;
  }

  private async pruneTerminalRuns(): Promise<void> {
    const cutoff = Date.now() - terminalRunRetentionMs(this.env.CRABBOX_RUN_RETENTION_DAYS);
    const storedCursor = await this.state.storage.get<string>(runPruneCursorKey);
    const startAfter = storedCursor?.startsWith("run:") ? storedCursor : undefined;
    const page = await this.state.storage.list<RunRecord>({
      prefix: "run:",
      limit: storageRecordScanBatchSize,
      ...(startAfter ? { startAfter } : {}),
    });
    if (page.size === 0) {
      if (storedCursor !== undefined) {
        await this.state.storage.delete(runPruneCursorKey);
      }
      return;
    }
    let deleted = 0;
    let lastScanned: string | undefined;
    for (const [key, run] of page) {
      lastScanned = key;
      const terminalAt = terminalRunTimestamp(run);
      if (key === runKey(run.id) && terminalAt !== undefined && terminalAt <= cutoff) {
        // oxlint-disable-next-line eslint/no-await-in-loop -- each run and its artifacts are removed before advancing the maintenance cursor.
        await this.deleteTerminalRun(run.id, cutoff);
        deleted += 1;
        if (deleted >= terminalRunPruneBatchSize) {
          break;
        }
      }
    }
    const pageEnd = [...page.keys()].at(-1);
    if (lastScanned && (lastScanned !== pageEnd || page.size === storageRecordScanBatchSize)) {
      await this.state.storage.put(runPruneCursorKey, lastScanned);
    } else {
      await this.state.storage.delete(runPruneCursorKey);
    }
  }

  private async deleteTerminalRun(runID: string, cutoff: number): Promise<void> {
    await this.state.runExclusive(async () => {
      const current = await this.getRun(runID);
      const terminalAt = current ? terminalRunTimestamp(current) : undefined;
      if (!current || terminalAt === undefined || terminalAt > cutoff) {
        return;
      }
      await this.deleteStoragePrefix(runEventPrefix(runID));
      await this.deleteStoragePrefix(runLogChunkPrefix(runID));
      await this.state.storage.delete(runLogKey(runID));
      await this.state.storage.delete(runKey(runID));
    });
  }

  private async deleteStoragePrefix(prefix: string): Promise<void> {
    for (;;) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- deletion advances by removing each bounded first page.
      const page = await this.state.storage.list({ prefix, limit: storageRecordScanBatchSize });
      if (page.size === 0) {
        return;
      }
      // oxlint-disable-next-line eslint/no-await-in-loop -- finish each bounded delete batch before loading the next one.
      await Promise.all([...page.keys()].map((key) => this.state.storage.delete(key)));
      if (page.size < storageRecordScanBatchSize) {
        return;
      }
    }
  }

  private async externalRunnerRecords(): Promise<ExternalRunnerRecord[]> {
    const runners = await this.state.storage.list<ExternalRunnerRecord>({
      prefix: externalRunnerPrefix(),
    });
    return [...runners.values()];
  }

  private async visibleExternalRunners(request: Request): Promise<ExternalRunnerRecord[]> {
    const runners = await this.externalRunnerRecords();
    const admin = isAdminRequest(request);
    const owner = requestOwner(request);
    const org = requestOrg(request, this.env);
    return runners.filter((runner) => admin || (runner.owner === owner && runner.org === org));
  }

  private async runEvents(runID: string, after = 0, limit = 500): Promise<RunEventRecord[]> {
    const events = await this.state.storage.list<RunEventRecord>({
      prefix: runEventPrefix(runID),
    });
    return [...events.values()]
      .toSorted((a, b) => a.seq - b.seq)
      .filter((event) => event.seq > after)
      .slice(0, limit);
  }

  private filterLeasesForRequest(leases: LeaseRecord[], request: Request): LeaseRecord[] {
    return this.filterLeases(leases, request).filter((lease) =>
      this.leaseVisibleToRequest(lease, request, false),
    );
  }

  private leaseVisibleToRequest(lease: LeaseRecord, request: Request, admin: boolean): boolean {
    return this.leaseAccessRole(lease, request, admin) !== undefined;
  }

  private readyPoolEntryVisibleToRequest(
    entry: ReadyPoolEntry,
    request: Request,
    lease: LeaseRecord | undefined,
  ): boolean {
    if (isAdminRequest(request)) {
      return true;
    }
    return Boolean(lease && this.leaseVisibleToRequest(lease, request, false));
  }

  private leaseManageableByRequest(lease: LeaseRecord, request: Request, admin: boolean): boolean {
    const role = this.leaseAccessRole(lease, request, admin);
    return role === "owner" || role === "manage";
  }

  private leaseProviderAccessVisibleToRequest(
    lease: LeaseRecord,
    request: Request,
    admin: boolean,
  ): boolean {
    return this.leaseAccessRole(lease, request, admin) === "owner";
  }

  private leaseForRequest(lease: LeaseRecord, request: Request, admin: boolean): LeaseRecord {
    const visible = publicLeaseRecord(lease);
    if (visible.cleanupError) {
      visible.cleanupError = coordinatorDiagnosticText(this.env, visible.cleanupError);
    }
    if (visible.failureError) {
      visible.failureError = coordinatorDiagnosticText(this.env, visible.failureError);
    }
    if (visible.share && !this.leaseManageableByRequest(lease, request, admin)) {
      delete visible.share;
    }
    if (
      visible.provider === "daytona" &&
      visible.sshUser &&
      !this.leaseProviderAccessVisibleToRequest(lease, request, admin)
    ) {
      visible.sshUser = "<token>";
    }
    return visible;
  }

  private runtimeAdapterDeletePendingResponse(
    lease: LeaseRecord,
    request: Request,
    admin: boolean,
  ): Response {
    return json(
      {
        error: "runtime_adapter_delete_pending",
        message: "lease release is blocked while its runtime adapter delete is pending",
        lease: this.leaseForRequest(lease, request, admin),
      },
      { status: 409 },
    );
  }

  private leaseForListRequest(lease: LeaseRecord, request: Request, admin: boolean): LeaseRecord {
    const visible = this.leaseForRequest(lease, request, admin);
    if (visible.provider === "daytona" && visible.sshUser) {
      visible.sshUser = "<token>";
    }
    return visible;
  }

  private leaseAccessRole(
    lease: LeaseRecord,
    request: Request,
    admin: boolean,
  ): "owner" | LeaseShareRole | undefined {
    return this.leaseAccessRoleForPrincipal(lease, {
      owner: requestOwner(request),
      org: requestOrg(request, this.env),
      admin,
    });
  }

  private leaseAccessRoleForPrincipal(
    lease: LeaseRecord,
    principal: { owner: string; org: string; admin: boolean },
  ): "owner" | LeaseShareRole | undefined {
    if (principal.admin) {
      return "owner";
    }
    // Legacy org values are lossy and cannot safely prove any non-admin relationship,
    // including an otherwise explicit user share carried by an ambiguous record.
    if (!isCurrentOrgKey(lease.org) || !isCurrentOrgKey(principal.org)) {
      return undefined;
    }
    const sameOrg = sameOrgIdentityKey(lease.org, principal.org);
    if (lease.owner === principal.owner && sameOrg) return "owner";
    const share = normalizedLeaseShare(lease.share);
    const userRole = share.users[normalizeShareUser(principal.owner)];
    const orgRole = sameOrg && lease.org !== MISSING_ORG_KEY ? share.org : undefined;
    if (userRole === "manage" || orgRole === "manage") {
      return "manage";
    }
    if (userRole === "use" || orgRole === "use") {
      return "use";
    }
    return undefined;
  }

  private runWritableByRequest(run: RunRecord, request: Request): boolean {
    return (
      isAdminRequest(request) ||
      (run.owner === requestOwner(request) && run.org === requestOrg(request, this.env))
    );
  }

  private runReadableToRequest(run: RunRecord, request: Request, lease?: LeaseRecord): boolean {
    if (this.runWritableByRequest(run, request)) {
      return true;
    }
    const owner = requestOwner(request);
    const org = requestOrg(request, this.env);
    return (
      run.leaseOwners?.some(
        (attribution) => attribution.owner === owner && attribution.org === org,
      ) ||
      (!run.leaseOwners?.length && lease?.owner === owner && lease.org === org)
    );
  }

  private runReadableToControl(
    run: RunRecord,
    attachment: Extract<BridgeAttachment, { kind: "control" }>,
    lease?: LeaseRecord,
  ): boolean {
    if (!attachment.admin && !isCurrentOrgKey(attachment.org)) {
      return false;
    }
    return Boolean(
      attachment.admin ||
      (run.owner === attachment.owner && sameOrgIdentityKey(run.org, attachment.org)) ||
      run.leaseOwners?.some(
        (attribution) =>
          attribution.owner === attachment.owner &&
          sameOrgIdentityKey(attribution.org, attachment.org),
      ) ||
      (!run.leaseOwners?.length &&
        lease?.owner === attachment.owner &&
        sameOrgIdentityKey(lease.org, attachment.org)),
    );
  }

  private setRunLeaseAttribution(run: RunRecord, lease: LeaseRecord): void {
    if (!run.leaseIDs?.includes(lease.id)) {
      run.leaseIDs = [...(run.leaseIDs ?? []), lease.id];
    }
    if (
      !run.leaseOwners?.some(
        (attribution) => attribution.owner === lease.owner && attribution.org === lease.org,
      )
    ) {
      run.leaseOwners = [...(run.leaseOwners ?? []), { owner: lease.owner, org: lease.org }];
    }
  }

  private runReferencesLease(run: RunRecord, leaseID: string): boolean {
    return run.leaseID === leaseID || run.leaseIDs?.includes(leaseID) === true;
  }

  private async ensureRunLeaseAttribution(
    run: RunRecord,
    knownLeases?: Map<string, LeaseRecord>,
  ): Promise<LeaseRecord | undefined> {
    if (run.leaseIDs !== undefined && run.leaseOwners !== undefined) {
      return undefined;
    }
    const events = await this.state.storage.list<RunEventRecord>({
      prefix: runEventPrefix(run.id),
    });
    const leaseIDs = new Set(
      [...events.values()]
        .toSorted((a, b) => a.seq - b.seq)
        .map((event) => event.leaseID)
        .filter((leaseID): leaseID is string => Boolean(leaseID && validLeaseID(leaseID))),
    );
    if (validLeaseID(run.leaseID)) {
      leaseIDs.add(run.leaseID);
    }
    const ids = [...leaseIDs];
    const leases = knownLeases
      ? ids.map((leaseID) => knownLeases.get(leaseID))
      : await Promise.all(ids.map((leaseID) => this.getLease(leaseID)));
    run.leaseIDs = ids;
    run.leaseOwners = [];
    let currentLease: LeaseRecord | undefined;
    for (const [index, lease] of leases.entries()) {
      if (!lease) {
        continue;
      }
      this.setRunLeaseAttribution(run, lease);
      if (ids[index] === run.leaseID) {
        currentLease = lease;
      }
    }
    await this.putRun(run);
    return currentLease;
  }

  private leaseVisibleToControl(
    lease: LeaseRecord,
    attachment: Extract<BridgeAttachment, { kind: "control" }>,
  ): boolean {
    return Boolean(
      attachment.admin ||
      (lease.owner === attachment.owner && sameOrgIdentityKey(lease.org, attachment.org)),
    );
  }

  private async putLease(lease: LeaseRecord, options?: { noCache?: boolean }): Promise<void> {
    await this.state.storage.put(leaseKey(lease.id), lease, options);
  }

  private async cancelLeaseReservation(reservation: LeaseRecord): Promise<void> {
    await this.state.runExclusive(async () => {
      const current = await this.getLease(reservation.id);
      if (
        !current ||
        current.state !== "provisioning" ||
        current.createdAt !== reservation.createdAt
      ) {
        return;
      }
      await this.deleteProviderAccess(reservation.id);
      await this.state.storage.delete(leaseKey(reservation.id));
      await this.scheduleAlarm();
    });
  }

  private async removeReleasedLeaseReservation(
    reservation: LeaseRecord,
  ): Promise<LeaseRecord | undefined> {
    const current = await this.state.runExclusive(async () => {
      const latest = await this.getLease(reservation.id);
      if (
        !latest ||
        latest.state !== "released" ||
        latest.cloudID ||
        latest.createdAt !== reservation.createdAt
      ) {
        return latest;
      }
      await this.deleteProviderAccess(reservation.id);
      await this.state.storage.delete(leaseKey(reservation.id));
      await this.markAWSIngressReconcilePending(reservation);
      await this.scheduleAlarm();
      return undefined;
    });
    return current;
  }

  private async putProviderAccess(lease: LeaseRecord): Promise<void> {
    await this.state.storage.put(providerAccessKey(lease.id), lease);
  }

  private async deleteProviderAccess(leaseID: string): Promise<void> {
    await this.state.storage.delete(providerAccessKey(leaseID));
  }

  private async getRun(runID: string): Promise<RunRecord | undefined> {
    return this.state.storage.get<RunRecord>(runKey(runID));
  }

  private async putRun(run: RunRecord): Promise<void> {
    await this.state.storage.put(runKey(run.id), run);
  }

  private async putExternalRunner(runner: ExternalRunnerRecord): Promise<void> {
    await this.state.storage.put(
      externalRunnerKey(runner.provider, runner.id, runner.owner, runner.org),
      runner,
    );
  }

  private async appendRunEventRecord(
    run: RunRecord,
    input: RunEventRequest,
  ): Promise<RunEventRecord> {
    const now = new Date().toISOString();
    const seq = (run.eventCount ?? 0) + 1;
    const event = boundedRunEvent(run.id, seq, now, input);
    const previousLeaseID = run.leaseID;
    applyRunEventSummary(run, event);
    if (
      validLeaseID(run.leaseID) &&
      (run.leaseID !== previousLeaseID || !run.leaseIDs?.includes(run.leaseID))
    ) {
      const lease = await this.getLease(run.leaseID);
      if (lease) {
        this.setRunLeaseAttribution(run, lease);
      }
    }
    run.eventCount = seq;
    run.lastEventAt = now;
    await this.state.storage.put(runEventKey(run.id, seq), event);
    await this.putRun(run);
    await this.broadcastRunEvent(run, event);
    return event;
  }

  private async listProviderMachinesSafe(provider: Provider): Promise<ProviderMachine[]> {
    try {
      return await this.provider(provider).listCrabboxServers();
    } catch {
      return [];
    }
  }

  private async broadcastRunEvent(run: RunRecord, event: RunEventRecord): Promise<void> {
    for (const socket of this.controlSockets.values()) {
      if (socket.readyState !== WebSocket.OPEN) {
        continue;
      }
      const attachment = this.bridgeAttachment(socket);
      if (!attachment || attachment.kind !== "control") {
        continue;
      }
      // oxlint-disable-next-line eslint/no-await-in-loop -- validate each control before its event can be sent.
      if (!(await this.activeBridgeGrantIsCurrent(socket, attachment))) {
        continue;
      }
      const after = attachment.subscriptions?.[run.id];
      if (
        after === undefined ||
        after >= event.seq ||
        !this.runReadableToControl(run, attachment)
      ) {
        continue;
      }
      attachment.subscriptions = { ...attachment.subscriptions, [run.id]: event.seq };
      this.serializeBridgeAttachment(socket, attachment);
      sendControl(socket, {
        type: "run_events",
        runID: run.id,
        events: [event],
        nextSeq: event.seq,
      });
    }
  }

  private provider(provider: Provider, region?: string, project?: string): CloudProvider {
    const testProvider = this.testProviders[provider];
    if (testProvider) {
      return testProvider;
    }
    if (provider === "aws") {
      return new AWSProvider(
        this.env,
        region || this.env.CRABBOX_AWS_REGION || "eu-west-1",
        this.state.storage,
      );
    }
    if (provider === "azure") {
      return new AzureProvider(
        this.env,
        (request) => recordAzureDeferredCleanup(this.state, () => this.scheduleAlarm(), request),
        this.state.storage,
        region,
      );
    }
    if (provider === "gcp") {
      return new GCPProvider(this.env, this.state.storage, region, project);
    }
    if (provider === "daytona") {
      return new DaytonaProvider(this.env);
    }
    return new HetznerProvider(this.env);
  }

  private async deleteLeaseServer(lease: LeaseRecord): Promise<void> {
    const provider = managedLeaseProvider(lease);
    if (!provider) {
      return;
    }
    await this.provider(provider, lease.region, lease.providerProject).releaseLease(lease);
  }

  private async abortProvisionedLeaseAfterStateChange(
    record: LeaseRecord,
    config: LeaseConfig,
    server: ProviderMachine,
    serverType: string,
    deletePublishedAccess: boolean,
  ): Promise<Response> {
    const preparation = await this.state.runExclusive(async () => {
      if (deletePublishedAccess) {
        await this.deleteProviderAccess(record.id);
      }
      const latest = await this.getLease(record.id);
      const previous = latest ? structuredClone(latest) : undefined;
      const cleanupLease = provisionedLeaseRecord(latest ?? record, config, server, serverType);
      if (latest?.state === "released" && latest.releaseDeletesServer === false) {
        cleanupLease.state = "released";
        cleanupLease.keep = true;
        clearLeaseCleanupMetadata(cleanupLease);
        delete cleanupLease.cleanupStartedAt;
        delete cleanupLease.cleanupClaimExpiresAt;
        await this.putLease(cleanupLease);
        await this.markAWSIngressReconcilePending(cleanupLease);
        await this.scheduleAlarm();
        return { keep: true as const, lease: cleanupLease };
      }
      const cleanupStarted = new Date();
      const cleanupStartedAt = cleanupStarted.toISOString();
      cleanupLease.state = latest?.state ?? cleanupLease.state;
      cleanupLease.cleanupStartedAt = cleanupStartedAt;
      cleanupLease.cleanupClaimExpiresAt = new Date(
        cleanupStarted.getTime() + leaseCleanupClaimStaleMs,
      ).toISOString();
      delete cleanupLease.cleanupRetryAt;
      cleanupLease.updatedAt = cleanupStartedAt;
      if (cleanupLease.state === "released") {
        cleanupLease.releaseDeletesServer = true;
      }
      await this.putLease(cleanupLease);
      await this.markAWSIngressReconcilePending(cleanupLease);
      await this.scheduleAlarm();
      return {
        keep: false as const,
        lease: cleanupLease,
        cleanupStartedAt,
        claimed: structuredClone(cleanupLease),
        previous,
      };
    });
    if (preparation.keep) {
      return json(
        {
          error: "lease_state_changed",
          message: "lease changed state while provider provisioning was in progress",
          lease: publicLeaseRecord(preparation.lease),
        },
        { status: 409 },
      );
    }
    try {
      await this.deleteLeaseServer(preparation.lease);
    } catch (error) {
      const failure = await this.state.runExclusive(async () => {
        const latest = await this.getLease(record.id);
        const cleanupLease = provisionedLeaseRecord(
          latest ?? preparation.lease,
          config,
          server,
          serverType,
        );
        if (latest?.state === "released" && latest.releaseDeletesServer === false) {
          cleanupLease.state = "released";
          cleanupLease.keep = true;
          clearLeaseCleanupMetadata(cleanupLease);
          delete cleanupLease.cleanupStartedAt;
          delete cleanupLease.cleanupClaimExpiresAt;
          await this.putLease(cleanupLease);
          await this.markAWSIngressReconcilePending(cleanupLease);
          await this.scheduleAlarm();
          return { suppressed: true as const, lease: cleanupLease };
        }
        if (latest?.cleanupStartedAt !== preparation.cleanupStartedAt) {
          return { suppressed: true as const, lease: latest ?? cleanupLease };
        }
        const failedAt = new Date().toISOString();
        cleanupLease.state = latest?.state ?? "active";
        if (cleanupLease.state === "released") {
          cleanupLease.releaseDeletesServer = true;
        }
        cleanupLease.cleanupAttempts = (cleanupLease.cleanupAttempts ?? 0) + 1;
        delete cleanupLease.cleanupStartedAt;
        delete cleanupLease.cleanupClaimExpiresAt;
        cleanupLease.cleanupFailedAt = failedAt;
        cleanupLease.cleanupError = coordinatorErrorMessage(this.env, error);
        cleanupLease.cleanupRetryAt = new Date(Date.now() + leaseCleanupRetryDelayMs).toISOString();
        cleanupLease.expiresAt = failedAt;
        cleanupLease.updatedAt = failedAt;
        await this.putLease(cleanupLease);
        await this.markAWSIngressReconcilePending(cleanupLease);
        await this.scheduleAlarm();
        return { suppressed: false as const, lease: cleanupLease };
      });
      if (failure.suppressed) {
        return json(
          {
            error: "lease_state_changed",
            message: "lease changed state while provider provisioning was in progress",
            lease: publicLeaseRecord(failure.lease),
          },
          { status: 409 },
        );
      }
      throw error;
    }
    const latest = await this.state.runExclusive(async () => {
      const current = await this.getLease(record.id);
      if (current?.cleanupStartedAt === preparation.cleanupStartedAt) {
        if (preparation.previous) {
          const completed = applyLeaseRecordChanges(
            current,
            preparation.claimed,
            preparation.previous,
          );
          clearLeaseCleanupMetadata(completed);
          delete completed.cleanupStartedAt;
          delete completed.cleanupClaimExpiresAt;
          delete completed.releaseDeletesServer;
          completed.updatedAt = new Date().toISOString();
          await this.putLease(completed);
        } else {
          await this.state.storage.delete(leaseKey(record.id));
        }
      }
      await this.markAWSIngressReconcilePending(preparation.lease);
      await this.scheduleAlarm();
      return this.getLease(record.id);
    });
    return json(
      {
        error: "lease_state_changed",
        message: "lease changed state while provider provisioning was in progress",
        lease: latest ? publicLeaseRecord(latest) : undefined,
      },
      { status: 409 },
    );
  }

  private async releaseResolvedLease(
    lease: LeaseRecord,
    options: { deleteServer: boolean; keep?: boolean },
  ): Promise<LeaseRecord> {
    const current = (await this.getLease(lease.id)) ?? lease;
    if (current.runtimeAdapterDeleteRequestedAt) {
      return current;
    }
    await this.markWorkspaceReleaseRequested(current);
    if (
      current.workspaceID &&
      !current.cloudID &&
      (current.state === "provisioning" ||
        current.state === "failed" ||
        (current.state === "released" && current.releaseDeletesServer === true))
    ) {
      await this.state.runExclusive(() => this.scheduleAlarm());
      return current;
    }
    const release = () => this.releaseResolvedLeaseOperation(current, options);
    return managedLeaseProvider(current) === "aws" &&
      !current.network?.awsPrivate &&
      (current.state === "active" || Boolean(current.cloudID))
      ? this.withAWSIngressOperationLock(release)
      : release();
  }

  private async markWorkspaceReleaseRequested(lease: LeaseRecord): Promise<void> {
    const workspaceID = lease.workspaceID;
    if (!workspaceID) {
      return;
    }
    const key = workspaceKey(lease.owner, lease.org, workspaceID);
    await this.state.runExclusive(async () => {
      const workspace = await this.state.storage.get<WorkspaceRecord>(key);
      if (!workspace || !workspaceLeaseMatchesCleanup(workspace, lease)) {
        throw new Error("workspace lease reservation conflicts with another lifecycle");
      }
      if (!workspace.releaseRequestedAt) {
        workspace.releaseRequestedAt = new Date().toISOString();
        workspace.updatedAt = workspace.releaseRequestedAt;
        await this.state.storage.put(key, workspace);
      }
    });
    this.closeWorkspaceTerminals(key, 1008, "workspace stopping");
  }

  private async releaseResolvedLeaseOperation(
    lease: LeaseRecord,
    options: { deleteServer: boolean; keep?: boolean },
  ): Promise<LeaseRecord> {
    const preparation = await this.state.runExclusive(async () => {
      const current = (await this.getLease(lease.id)) ?? structuredClone(lease);
      if (current.runtimeAdapterDeleteRequestedAt) {
        return { cleanup: false as const, blocked: true as const, lease: current };
      }
      if (current.cleanupStartedAt) {
        return { cleanup: false as const, blocked: false as const, lease: current };
      }
      const deleteServer = options.deleteServer && !isRegisteredLease(current);
      const shouldDelete = Boolean(
        deleteServer &&
        (current.providerKeyCleanupPending ||
          (current.cloudID &&
            (leaseIsLive(current) ||
              current.releaseDeletesServer !== undefined ||
              current.cleanupError))),
      );
      if (!shouldDelete) {
        const released = finalizedReleasedLease(current, deleteServer, options.keep);
        await this.putLease(released);
        await this.clearWorkspaceReleaseError(released);
        await this.markAWSIngressReconcilePending(released);
        await this.scheduleAlarm();
        return { cleanup: false as const, blocked: false as const, lease: released };
      }
      const now = new Date();
      const claimed = finalizedReleasedLease(current, true, options.keep);
      claimed.cleanupStartedAt = now.toISOString();
      claimed.cleanupClaimExpiresAt = new Date(
        now.getTime() + leaseCleanupClaimStaleMs,
      ).toISOString();
      claimed.releaseDeletesServer = true;
      await this.putLease(claimed);
      await this.markAWSIngressReconcilePending(claimed);
      await this.scheduleAlarm();
      return {
        cleanup: true as const,
        blocked: false as const,
        claim: claimed.cleanupStartedAt,
        lease: structuredClone(claimed),
      };
    });
    if (preparation.blocked) {
      return preparation.lease;
    }
    this.closeLeaseBridges(lease.id, 1008, "lease ended");
    if (!preparation.cleanup) {
      return preparation.lease;
    }
    try {
      await this.deleteLeaseServer(preparation.lease);
    } catch (error) {
      await this.state.runExclusive(async () => {
        const current = await this.getLease(preparation.lease.id);
        if (!current || current.cleanupStartedAt !== preparation.claim) {
          return;
        }
        if (error instanceof ProviderCleanupManualResolutionError) {
          terminalizeManualProviderCleanup(
            current,
            coordinatorErrorMessage(this.env, error),
            new Date().toISOString(),
          );
          await this.putLease(current);
          await this.scheduleAlarm();
          return;
        }
        const failedAt = new Date();
        current.cleanupAttempts = (current.cleanupAttempts ?? 0) + 1;
        delete current.cleanupStartedAt;
        delete current.cleanupClaimExpiresAt;
        current.cleanupError = coordinatorErrorMessage(this.env, error);
        current.cleanupFailedAt = failedAt.toISOString();
        current.cleanupRetryAt = new Date(
          failedAt.getTime() + leaseCleanupRetryDelayMs,
        ).toISOString();
        current.updatedAt = failedAt.toISOString();
        current.releaseDeletesServer = true;
        await this.putLease(current);
        await this.scheduleAlarm();
      });
      throw error;
    }
    return await this.state.runExclusive(async () => {
      const current = await this.getLease(preparation.lease.id);
      if (!current || current.cleanupStartedAt !== preparation.claim) {
        return current ?? preparation.lease;
      }
      const released = finalizedReleasedLease(current, true, options.keep);
      delete released.providerKeyCleanupPending;
      delete released.providerKeyCleanupID;
      await this.putLease(released);
      await this.clearWorkspaceReleaseError(released);
      await this.markAWSIngressReconcilePending(released);
      await this.scheduleAlarm();
      return released;
    });
  }
}

export class FleetDurableObject extends FleetCoordinator implements DurableObject {
  private readonly runtime: CloudflareCoordinatorRuntime;

  constructor(
    state: DurableObjectState,
    env: Env,
    testProviders: Partial<Record<Provider, CloudProvider>> = {},
  ) {
    const runtime = new CloudflareCoordinatorRuntime(state);
    super(runtime, env, testProviders);
    this.runtime = runtime;
  }

  override async fetch(request: Request): Promise<Response> {
    if (coordinatorRequestQueue(request) === "direct") {
      return super.fetch(request);
    }
    const bufferedRequest = await bufferCoordinatorRequestBody(request);
    return this.runtime.runExclusive(() => super.fetch(bufferedRequest));
  }

  override alarm(): Promise<void> {
    return super.durableObjectAlarm();
  }

  override webSocketMessage(socket: WebSocket, message: string | ArrayBuffer): Promise<void> {
    const attachment = this.runtime.socketAttachment<{ kind?: string }>(socket);
    if (attachment?.kind !== "control") {
      return super.webSocketMessage(socket, message);
    }
    return this.runtime.runExclusive(() => super.webSocketMessage(socket, message));
  }
}

interface ProviderReadiness {
  provider: Provider;
  configured: boolean;
  missing: string[];
  message: string;
  checks?: ProviderReadinessCheck[];
}

interface ProviderReadinessCheck {
  status: string;
  check: string;
  message?: string;
  details?: Record<string, string>;
}

function providerReadiness(provider: Provider, env: Env, gcpProject?: string): ProviderReadiness {
  const spec = coordinatorProviderSpec(provider);
  if (provider === "aws") {
    const configured = awsCredentialsConfigured(env);
    return {
      provider,
      configured,
      missing: configured ? [] : ["AWS credentials"],
      message: configured
        ? "aws coordinator credentials are configured"
        : "aws coordinator credentials are missing",
    };
  }
  if (provider === "gcp") {
    const missing: string[] = [];
    if (
      !nonSecretString(gcpProject) &&
      !nonSecretString(env.GCP_PROJECT_ID) &&
      !nonSecretString(env.CRABBOX_GCP_PROJECT)
    ) {
      missing.unshift("GCP_PROJECT_ID");
    }
    const hasClientEmail = Boolean(nonSecretString(env.GCP_CLIENT_EMAIL));
    const hasPrivateKey = Boolean(nonSecretString(env.GCP_PRIVATE_KEY));
    const credentialSource = env.CRABBOX_GCP_CREDENTIAL_SOURCE?.trim() ?? "";
    const validCredentialSource =
      credentialSource === "" ||
      credentialSource === "metadata" ||
      credentialSource === "service-account-key";
    if (!validCredentialSource) missing.push("CRABBOX_GCP_CREDENTIAL_SOURCE");
    if (hasClientEmail !== hasPrivateKey) {
      if (!hasClientEmail) missing.push("GCP_CLIENT_EMAIL");
      if (!hasPrivateKey) missing.push("GCP_PRIVATE_KEY");
    } else if (validCredentialSource && !hasClientEmail && credentialSource !== "metadata") {
      missing.push("GCP_CLIENT_EMAIL", "GCP_PRIVATE_KEY");
    }
    return {
      provider,
      configured: missing.length === 0,
      missing,
      message:
        missing.length === 0
          ? `${spec.provider} coordinator credentials are configured`
          : `${spec.provider} coordinator configuration missing: ${missing.join(", ")}`,
    };
  }
  const missing = providerRequiredSecrets(provider).filter((name) => !nonSecretString(env[name]));
  return {
    provider,
    configured: missing.length === 0,
    missing,
    message:
      missing.length === 0
        ? `${spec.provider} coordinator secrets are configured`
        : `${spec.provider} coordinator secrets missing: ${missing.join(", ")}`,
  };
}

const readinessDummySSHPublicKey =
  "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEcrabboxDoctorReadinessPlaceholder crabbox-doctor";

function normalizeReadinessTarget(value: string | null): TargetOS {
  return value === "windows" || value === "macos" ? value : "linux";
}

function normalizeReadinessWindowsMode(value: string | null): WindowsMode {
  return value === "wsl2" ? "wsl2" : "normal";
}

function normalizeReadinessMarket(value: string | null): "spot" | "on-demand" | undefined {
  if (value === "spot" || value === "on-demand") {
    return value;
  }
  return undefined;
}

function providerRequiredSecrets(provider: Provider): Array<keyof Env> {
  return [...coordinatorProviderSpec(provider).requiredSecrets];
}

function portalReturnLocation(request: Request): string {
  const value = new URL(request.url).searchParams.get("return") ?? "";
  return value.startsWith("/portal") && !value.startsWith("//") ? value : "/portal";
}

function leaseKey(leaseID: string): string {
  return `lease:${leaseID}`;
}

function workspaceLeaseReservationKey(leaseID: string): string {
  return `workspace-lease:${leaseID}`;
}

function workspaceKey(owner: string, org: string, workspaceID: string): string {
  return `workspace:${encodeURIComponent(org)}:${encodeURIComponent(owner)}:${workspaceID}`;
}

function providerAccessPrefix(): string {
  return "provider-access:";
}

function providerAccessKey(leaseID: string): string {
  return `${providerAccessPrefix()}${leaseID}`;
}

function normalizeReadyPoolKey(value: string): string {
  return value
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9._/-]+/g, "-")
    .replace(/^-+|-+$/g, "");
}

function decodeReadyPoolRouteKey(value: string): string | undefined {
  try {
    return decodeURIComponent(value);
  } catch {
    return undefined;
  }
}

function readyPoolEntryMatches(entry: ReadyPoolEntry, input: ReadyPoolBorrowRequest): boolean {
  return (
    readyPoolFieldMatches(entry.repo, input.repo) &&
    readyPoolFieldMatches(entry.ref, input.ref) &&
    readyPoolFieldMatches(entry.commit, input.commit, input.allowMissingCommit === true) &&
    readyPoolFieldMatches(entry.fingerprint, input.fingerprint) &&
    readyPoolFieldMatches(entry.provider, input.provider) &&
    readyPoolFieldMatches(entry.target, input.target)
  );
}

function addReadyPoolEntryString(
  entry: ReadyPoolEntry,
  key: keyof ReadyPoolEntry,
  value: unknown,
): void {
  const text = nonSecretString(value);
  if (text) {
    (entry as unknown as Record<string, unknown>)[key] = text;
  }
}

function readyPoolLeaseSSHHost(lease: LeaseRecord, requested: unknown): string {
  const host = nonSecretString(requested);
  const allowed = new Set(
    [lease.host, lease.tailscale?.fqdn, lease.tailscale?.ipv4].filter((value): value is string =>
      Boolean(value),
    ),
  );
  return host && allowed.has(host) ? host : lease.host;
}

function readyPoolLeaseSSHUser(lease: LeaseRecord, requested: unknown): string {
  const user = nonSecretString(requested);
  return safeReadyPoolSSHUser(user) ? user : lease.sshUser;
}

function safeReadyPoolSSHUser(value: string): boolean {
  return /^[A-Za-z_][A-Za-z0-9_.-]{0,63}$/.test(value);
}

function readyPoolLeaseSSHPort(lease: LeaseRecord, requested: unknown): string {
  const port = nonSecretString(requested);
  const allowed = new Set([lease.sshPort, ...(lease.sshFallbackPorts ?? [])]);
  return port && allowed.has(port) ? port : lease.sshPort;
}

function readyPoolLeaseWorkRoot(lease: LeaseRecord, requested: unknown): string {
  return nonSecretString(requested) || lease.workRoot;
}

function readyPoolFieldMatches(
  stored: string | undefined,
  requested: string | undefined,
  allowMissing = false,
): boolean {
  const want = nonSecretString(requested);
  if (!want) {
    return true;
  }
  const got = nonSecretString(stored);
  return got === want || (allowMissing && got === "");
}

function readyPoolKey(key: string, leaseID: string): string {
  return `${readyPoolPrefix}${key}:${leaseID}`;
}

function runKey(runID: string): string {
  return `run:${runID}`;
}

function externalRunnerPrefix(): string {
  return "runner:";
}

function externalRunnerKey(provider: string, runnerID: string, owner: string, org: string): string {
  return `${externalRunnerPrefix()}${[provider, runnerID, org, owner]
    .map((value) => encodeURIComponent(value))
    .join(":")}`;
}

function runLogKey(runID: string): string {
  return `runlog:${runID}`;
}

function runLogChunkPrefix(runID: string): string {
  return `runlog:${runID}:chunk:`;
}

function runLogChunkKey(runID: string, index: number): string {
  return `${runLogChunkPrefix(runID)}${String(index).padStart(6, "0")}`;
}

function runEventPrefix(runID: string): string {
  return `runevent:${runID}:`;
}

function runEventKey(runID: string, seq: number): string {
  return `${runEventPrefix(runID)}${String(seq).padStart(12, "0")}`;
}

function createdAWSImageKey(imageID: string): string {
  return `image:aws:created:${imageID}`;
}

function createdProviderImageKey(provider: Provider, imageID: string): string {
  return `image:${provider}:created:${encodeURIComponent(imageID)}`;
}

function validateProviderImageDeleteOwnership(
  provider: Provider,
  imageID: string,
  metadata?: Partial<ProviderImage>,
): { status: number; body: Record<string, unknown> } | undefined {
  const routeMatchesMetadata = metadata?.id === imageID || metadata?.resourceID === imageID;
  if (routeMatchesMetadata && (!metadata.provider || metadata.provider === provider)) {
    return undefined;
  }
  return {
    status: 409,
    body: {
      error: "image_not_owned",
      message: `refusing to delete ${provider} image ${imageID}: no Crabbox-created image metadata found`,
    },
  };
}

async function storeCreatedProviderImage(
  storage: ProviderStateStorage,
  provider: Provider,
  image: ProviderImage,
): Promise<void> {
  await storage.put(createdProviderImageKey(provider, image.id), image);
  if (image.resourceID && image.resourceID !== image.id) {
    await storage.put(createdProviderImageKey(provider, image.resourceID), image);
  }
}

function legacyPromotedAWSImageKey(): string {
  return promotedAWSImagePrefix();
}

function legacyPromotedAWSImageCompatible(image: Pick<ProviderImage, "architecture">): boolean {
  return !image.architecture || image.architecture === "x86_64";
}

function promotedAWSLinuxOSImageKey(image: Pick<ProviderImage, "architecture" | "os">): string {
  const architecture = image.architecture ?? awsImageArchitectureForTarget("linux", "");
  return `image:aws:promoted:linux:${architecture}:${sanitizePromotedAWSImageKeyPart(image.os ?? "")}`;
}

function promotedAWSImagePrefix(): string {
  return "image:aws:promoted";
}

function promotedAWSImageCatalogPrefix(
  image: Pick<ProviderImage, "target" | "architecture" | "region" | "serverType"> & {
    os?: string;
  },
): string {
  const scope = promotedAWSImageKey(image).slice(`${promotedAWSImagePrefix()}:`.length);
  return `image:aws:catalog:${scope}:`;
}

function promotedAWSImageCatalogKey(image: PromotedImageRecord): string {
  return `${promotedAWSImageCatalogPrefix(image)}${encodeURIComponent(image.id)}`;
}

class ImageCapabilityMismatchError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ImageCapabilityMismatchError";
  }
}

function promotionVersionMap(values: string[]): Record<string, string> | undefined {
  const entries = values.map((value) => {
    const separator = value.indexOf("=");
    return separator > 0 ? [value.slice(0, separator), value.slice(separator + 1)] : [value, ""];
  });
  return entries.length > 0 ? Object.fromEntries(entries) : undefined;
}

function promotedAWSImageKey(
  image: Pick<ProviderImage, "target" | "architecture" | "region" | "serverType"> & {
    os?: string;
  },
): string {
  const target = image.target ?? "linux";
  const architecture = image.architecture ?? awsImageArchitectureForTarget(target, "");
  const region = sanitizeAWSRegion(image.region ?? "");
  if (target === "macos") {
    return `image:aws:promoted:${target}:${architecture}:${sanitizePromotedAWSImageKeyPart(image.serverType ?? "")}:${region}`;
  }
  if (target === "linux" && image.os) {
    return `image:aws:promoted:${target}:${architecture}:${sanitizePromotedAWSImageKeyPart(image.os)}:${region}`;
  }
  return `image:aws:promoted:${target}:${architecture}:${region}`;
}

function legacyScopedPromotedAWSImageKey(
  image: Pick<ProviderImage, "target" | "architecture" | "region">,
): string {
  const target = image.target ?? "linux";
  const architecture = image.architecture ?? awsImageArchitectureForTarget(target, "");
  const region = sanitizeAWSRegion(image.region ?? "");
  return `image:aws:promoted:${target}:${architecture}:${region}`;
}

function sanitizePromotedAWSImageKeyPart(value: string): string {
  return value
    .trim()
    .toLowerCase()
    .replaceAll(/[^a-z0-9._-]/g, "");
}

function enrichAWSImage(image: ProviderImage, lease: LeaseRecord): ProviderImage {
  const metadata: Partial<ProviderImage> = {};
  if (lease.target) {
    metadata.target = lease.target;
  }
  if (lease.windowsMode) {
    metadata.windowsMode = lease.windowsMode;
  }
  if (lease.os) {
    metadata.os = lease.os;
  }
  if (lease.serverType) {
    metadata.serverType = lease.serverType;
  }
  const region = image.region ?? lease.region;
  if (region !== undefined && region !== "") {
    metadata.region = region;
  }
  return mergeAWSImageMetadata(image, metadata);
}

function mergeAWSImageMetadata(
  image: ProviderImage,
  metadata?: Partial<ProviderImage>,
): ProviderImage {
  const target = normalizeAWSImageTarget(metadata?.target ?? image.target ?? "linux") ?? "linux";
  const serverType = metadata?.serverType ?? image.serverType ?? "";
  const result: ProviderImage = {
    ...metadata,
    ...image,
    target,
    architecture:
      metadata?.architecture ??
      image.architecture ??
      awsImageArchitectureForTarget(target, serverType),
  };
  const windowsMode = metadata?.windowsMode ?? image.windowsMode;
  if (windowsMode !== undefined) {
    result.windowsMode = windowsMode;
  }
  if (serverType) {
    result.serverType = serverType;
  }
  const imageRegion = image.region;
  const metadataRegion = metadata?.region;
  if (imageRegion !== undefined && imageRegion !== "") {
    result.region = imageRegion;
  } else if (metadataRegion !== undefined && metadataRegion !== "") {
    result.region = metadataRegion;
  }
  return result;
}

function normalizeAWSImageTarget(value: string | undefined): TargetOS | undefined {
  switch ((value ?? "").trim().toLowerCase()) {
    case "":
    case "linux":
    case "ubuntu":
      return "linux";
    case "mac":
    case "macos":
    case "darwin":
    case "osx":
      return "macos";
    case "win":
    case "windows":
      return "windows";
    default:
      return undefined;
  }
}

function awsImageArchitectureForTarget(target: TargetOS, serverType: string): string {
  if (target === "macos") {
    return serverType.startsWith("mac1.") ? "x86_64_mac" : "arm64_mac";
  }
  return "x86_64";
}

function awsImageArchitectureForLease(
  target: TargetOS,
  serverType: string,
  architecture?: string,
): string {
  if (target === "linux" && architecture === "arm64") {
    return "arm64";
  }
  return awsImageArchitectureForTarget(target, serverType);
}

function boolFromUnknown(value: unknown): boolean {
  if (value === true) return true;
  if (value === false || value === undefined || value === null) return false;
  const normalized = String(value).trim().toLowerCase();
  return ["1", "true", "yes", "on"].includes(normalized);
}

function fastSnapshotRestoreAZs(
  inputZones: string[] | undefined,
  url: URL,
  region: string,
  env: Pick<Env, "CRABBOX_AWS_FAST_SNAPSHOT_RESTORE_AZS" | "CRABBOX_CAPACITY_AVAILABILITY_ZONES">,
): string[] {
  const zones = [
    ...(inputZones ?? []),
    ...url.searchParams.getAll("fsrAz"),
    ...splitCommaList(url.searchParams.get("fsrAzs") ?? ""),
    ...splitCommaList(env.CRABBOX_AWS_FAST_SNAPSHOT_RESTORE_AZS ?? ""),
    ...splitCommaList(env.CRABBOX_CAPACITY_AVAILABILITY_ZONES ?? ""),
  ];
  return [...new Set(zones.map((zone) => zone.trim()).filter((zone) => validAWSAZ(zone, region)))];
}

function fastSnapshotRestoreStatusAZs(url: URL, region: string): string[] {
  const zones = [
    ...url.searchParams.getAll("fsrAz"),
    ...url.searchParams.getAll("az"),
    ...splitCommaList(url.searchParams.get("fsrAzs") ?? ""),
    ...splitCommaList(url.searchParams.get("azs") ?? ""),
  ];
  return [...new Set(zones.map((zone) => zone.trim()).filter((zone) => validAWSAZ(zone, region)))];
}

function splitCommaList(value: string): string[] {
  return value
    .split(",")
    .map((item) => item.trim())
    .filter(Boolean);
}

function validAWSAZ(zone: string, region: string): boolean {
  if (!/^[a-z]{2}-[a-z-]+-[0-9][a-z]$/.test(zone)) {
    return false;
  }
  return region === "" || zone.startsWith(region);
}

function sanitizeMacHostQuotaError(message: string): string {
  if (
    message.includes("AccessDenied") ||
    message.includes("UnauthorizedOperation") ||
    message.includes("Encoded authorization") ||
    message.includes("arn:aws:iam::")
  ) {
    if (message.includes("GetServiceQuota")) {
      return "AWS authorization failure: coordinator AWS identity needs servicequotas:GetServiceQuota to inspect EC2 Mac Dedicated Host quotas";
    }
    return "AWS authorization failure: coordinator AWS identity needs servicequotas:GetServiceQuota or servicequotas:ListServiceQuotas to inspect EC2 Mac Dedicated Host quotas";
  }
  return message.replace(/\s+/g, " ");
}

function webVNCTicketPrefix(): string {
  return "webvnc-ticket:";
}

function webVNCTicketKey(ticket: string): string {
  return `${webVNCTicketPrefix()}${ticket}`;
}

function codeTicketPrefix(): string {
  return "code-ticket:";
}

function codeTicketKey(ticket: string): string {
  return `${codeTicketPrefix()}${ticket}`;
}

function codeViewerTicketPrefix(): string {
  return "code-viewer-ticket:";
}

function codeViewerTicketKey(ticket: string): string {
  return `${codeViewerTicketPrefix()}${ticket}`;
}

function codeViewerSessionPrefix(): string {
  return "code-viewer-session:";
}

function codeViewerSessionKey(session: string): string {
  return `${codeViewerSessionPrefix()}${session}`;
}

function codeViewerSessionRevocationPrefix(): string {
  return "code-viewer-session-revocation:";
}

function codeViewerSessionRevocationKey(portalSessionHash: string): string {
  return `${codeViewerSessionRevocationPrefix()}${portalSessionHash}`;
}

function egressTicketPrefix(): string {
  return "egress-ticket:";
}

function egressTicketKey(ticket: string): string {
  return `${egressTicketPrefix()}${ticket}`;
}

function runtimeAdapterTicketPrefix(): string {
  return "runtime-adapter-ticket:";
}

function runtimeAdapterTicketKey(ticket: string): string {
  return `${runtimeAdapterTicketPrefix()}${ticket}`;
}

function nativeVNCTicketPrefix(): string {
  return "native-vnc-ticket:";
}

function nativeVNCTicketKey(ticket: string): string {
  return `${nativeVNCTicketPrefix()}${ticket}`;
}

function runtimeAdapterIdentityKey(adapterID: string): string {
  return `runtime-adapter-identity:${adapterID}`;
}

function runtimeAdapterRelayError(
  id: string,
  status: number,
  error: string,
  message: string,
): RuntimeAdapterRelayResponse {
  return {
    type: "response",
    id,
    status,
    headers: { "content-type": "application/json; charset=utf-8" },
    body: JSON.stringify({ error, message }),
  };
}

function runtimeAdapterErrorCode(body: unknown): string | undefined {
  if (!body || typeof body !== "object" || Array.isArray(body)) {
    return undefined;
  }
  const error = (body as { error?: unknown }).error;
  if (typeof error === "string") {
    return error;
  }
  if (!error || typeof error !== "object" || Array.isArray(error)) {
    return undefined;
  }
  const code = (error as { code?: unknown }).code;
  return typeof code === "string" ? code : undefined;
}

async function runtimeAdapterDeleteDispatchSafeToClear(
  result: RuntimeAdapterProxyResult,
): Promise<boolean> {
  if (!result.dispatched) {
    return true;
  }
  if (result.origin !== "upstream") {
    return false;
  }
  if (result.response.ok) {
    return true;
  }
  const body = await result.response
    .clone()
    .json()
    .catch(() => undefined);
  return !["adapter_timeout", "adapter_unavailable"].includes(runtimeAdapterErrorCode(body) ?? "");
}

function runtimeAdapterDeleteCompletion(
  value: unknown,
): RuntimeAdapterDeleteCompletion | undefined {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    return undefined;
  }
  const completion = value as Partial<RuntimeAdapterDeleteCompletion>;
  if (
    completion.status !== "absent" ||
    !validRuntimeAdapterID(completion.adapterID) ||
    !validRuntimeAdapterID(completion.workspaceID) ||
    !validRuntimeAdapterID(completion.registrationID)
  ) {
    return undefined;
  }
  return {
    adapterID: completion.adapterID,
    workspaceID: completion.workspaceID,
    registrationID: completion.registrationID,
    status: completion.status,
  };
}

function runtimeAdapterLegacyDeleteCompletion(
  value: unknown,
): RuntimeAdapterLegacyDeleteCompletion | undefined {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    return undefined;
  }
  const completion = value as Partial<RuntimeAdapterLegacyDeleteCompletion>;
  if (
    completion.status !== "absent" ||
    !validRuntimeAdapterID(completion.adapterID) ||
    !validRuntimeAdapterID(completion.workspaceID)
  ) {
    return undefined;
  }
  return {
    adapterID: completion.adapterID,
    workspaceID: completion.workspaceID,
    status: completion.status,
  };
}

function newLeaseID(): string {
  const bytes = new Uint8Array(6);
  crypto.getRandomValues(bytes);
  return `cbx_${[...bytes].map((byte) => byte.toString(16).padStart(2, "0")).join("")}`;
}

function validWorkspaceID(value: string | undefined): value is string {
  return (
    typeof value === "string" &&
    value.length <= 63 &&
    /^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/.test(value)
  );
}

function newWorkspacePrewarmID(): string {
  return `prewarm-${crypto.randomUUID().replaceAll("-", "").slice(0, 20)}`;
}

function workspaceManagedLeaseResponse(): Response {
  return json(
    {
      error: "workspace_managed_lease",
      message: "workspace leases must be managed through the workspace lifecycle",
    },
    { status: 409 },
  );
}

function runtimeAdapterWorkspaceDeleteError(
  error: string,
  title: string,
  message: string,
  status: number,
): Response {
  return json({ error, title, message }, { status });
}

function runtimeAdapterDeleteInFlightResponse(retryAt: string): Response {
  const retryAtMs = Date.parse(retryAt);
  const retryAfter = Number.isFinite(retryAtMs)
    ? Math.max(1, Math.ceil((retryAtMs - Date.now()) / 1000))
    : 1;
  return json(
    {
      error: "runtime_adapter_delete_in_flight",
      message: "a generation-scoped runtime adapter delete is still settling",
      retryAt,
    },
    { status: 409, headers: { "retry-after": String(retryAfter) } },
  );
}

function workspaceCreateInput(value: unknown): WorkspaceCreateRequest | undefined {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    return undefined;
  }
  const input = value as Record<string, unknown>;
  if (input["id"] !== undefined && typeof input["id"] !== "string") return undefined;
  if (input["repo"] !== undefined && typeof input["repo"] !== "string") return undefined;
  if (input["branch"] !== undefined && typeof input["branch"] !== "string") return undefined;
  if (input["command"] !== undefined && typeof input["command"] !== "string") return undefined;
  if (input["runtime"] !== undefined && typeof input["runtime"] !== "string") return undefined;
  if (input["profile"] !== undefined && typeof input["profile"] !== "string") return undefined;
  if (input["ttlSeconds"] !== undefined && typeof input["ttlSeconds"] !== "number")
    return undefined;
  if (
    input["idleTimeoutSeconds"] !== undefined &&
    typeof input["idleTimeoutSeconds"] !== "number"
  ) {
    return undefined;
  }
  const capabilities = input["capabilities"];
  if (
    capabilities !== undefined &&
    (!capabilities || typeof capabilities !== "object" || Array.isArray(capabilities))
  ) {
    return undefined;
  }
  const desktop = (capabilities as Record<string, unknown> | undefined)?.["desktop"];
  if (desktop !== undefined && typeof desktop !== "boolean") return undefined;
  return input as WorkspaceCreateRequest;
}

function workspaceProvider(value: string | undefined): Provider {
  const provider = value?.trim() || "hetzner";
  if (!["hetzner", "aws", "azure", "gcp"].includes(provider)) {
    throw new Error(`unsupported workspace provider: ${provider}`);
  }
  return provider as Provider;
}

function workspaceProfile(value: string | undefined): string | undefined {
  const profile = value?.trim() || "default";
  return profile.length <= 120 ? profile : undefined;
}

function workspaceRepo(value: string | undefined): string | undefined {
  const repo = value?.trim() || "";
  if (!repo) return repo;
  const components = repo.split("/");
  return components.length === 2 &&
    components.every((component) => component.length <= 100 && /^[a-z0-9_.-]+$/i.test(component))
    ? repo
    : undefined;
}

function workspaceBranch(value: string | undefined): string | undefined {
  const branch = value?.trim() || "main";
  const components = branch.split("/");
  return branch.length <= 255 &&
    /^[a-z0-9][a-z0-9._/-]*$/i.test(branch) &&
    components.every(
      (component) =>
        component &&
        !component.startsWith(".") &&
        !component.endsWith(".") &&
        !component.toLowerCase().endsWith(".lock"),
    ) &&
    !branch.includes("..") &&
    !branch.includes("@{") &&
    !branch.includes("//") &&
    !branch.endsWith("/") &&
    !branch.endsWith(".")
    ? branch
    : undefined;
}

function workspaceCommand(value: string | undefined): string | undefined {
  const command = value?.trim() || "exec bash -l";
  return textEncoder.encode(shellQuote(shellQuote(shellQuote(command)))).byteLength <=
    workspaceCommandMaxBootstrapBytes && !command.includes("\u0000")
    ? command
    : undefined;
}

function workspaceClass(configured: string | undefined): string {
  const machineClass = configured?.trim() || "standard";
  return ["standard", "fast", "large", "beast"].includes(machineClass) ? machineClass : "standard";
}

function workspacePrewarmCount(value: string | undefined): number {
  const parsed = Number.parseInt(value?.trim() ?? "", 10);
  return Number.isFinite(parsed) ? Math.max(0, Math.min(parsed, 4)) : 0;
}

function workspacePrewarmMatches(
  workspace: WorkspaceRecord,
  target: Pick<WorkspaceRecord, "org" | "profile" | "provider" | "class" | "desktop">,
): boolean {
  return (
    sameOrgIdentityKey(workspace.org, target.org) &&
    workspace.profile === target.profile &&
    workspace.provider === target.provider &&
    workspace.class === target.class &&
    workspace.desktop === target.desktop
  );
}

function workspacePrewarmShape(
  workspace: Pick<WorkspaceRecord, "org" | "profile" | "provider" | "class" | "desktop">,
): string {
  return JSON.stringify([
    workspace.org,
    workspace.profile,
    workspace.provider,
    workspace.class,
    workspace.desktop,
  ]);
}

function workspaceSeconds(value: number | undefined, fallback: number): number | undefined {
  if (value === undefined) return fallback;
  if (!Number.isInteger(value) || value < 1) return undefined;
  return Math.min(value, 86_400);
}

async function workspaceAdmissionRetryable(response: Response): Promise<boolean> {
  if (response.status === 429) {
    const body = (await response
      .clone()
      .json()
      .catch(() => undefined)) as { error?: unknown } | undefined;
    return body?.error !== "cost_limit_exceeded";
  }
  return response.status === 424 || response.status >= 500;
}

async function workspaceLeaseRequest(
  workspace: WorkspaceRecord,
  workspaceCapability: ProviderWorkspaceCapability | undefined,
  sshAccess?: {
    publicKey: string;
    hostKeys: { private: string; public: string };
  },
): Promise<Request> {
  const remainingMs = workspaceProvisionDeadline(workspace) - Date.now();
  if (remainingMs < workspaceProvisionRecoveryGraceMs) {
    throw new Error("workspace provisioning recovery window no longer fits before hard TTL");
  }
  const remainingSeconds = Math.max(1, Math.ceil(remainingMs / 1000));
  if (!workspaceCapability && !sshAccess) {
    throw new Error("workspace SSH access is required");
  }
  const providerKey = `${workspaceProviderKeyPrefix}${(
    await sha256Hex(
      workspaceCapability ? workspace.leaseID : sshPublicKeyIdentity(sshAccess?.publicKey ?? ""),
    )
  ).slice(0, 12)}`;
  const headers = workspaceRecordHeaders(workspace);
  if (sshAccess) {
    headers.set(workspaceSSHHostPrivateKeyHeader, btoa(sshAccess.hostKeys.private));
    headers.set(workspaceSSHHostPublicKeyHeader, btoa(sshAccess.hostKeys.public));
  }
  const providerRequest: Partial<LeaseRequest> = workspaceCapability
    ? workspaceCapability.leaseRequestFields(workspace)
    : {
        sshPublicKey: sshAccess!.publicKey,
        ...(workspace.provider === "aws" ? { awsSSHCIDRs: ["0.0.0.0/0"] } : {}),
      };
  return new Request("https://crabbox.invalid/v1/leases", {
    method: "POST",
    headers,
    body: JSON.stringify({
      leaseID: workspace.leaseID,
      slug: workspace.id,
      provider: workspace.provider,
      profile: workspace.profile,
      class: workspace.class,
      providerKey,
      desktop: workspace.desktop,
      ttlSeconds: remainingSeconds,
      // The adapter has no activity channel yet, so idle expiry would terminate active workspaces.
      idleTimeoutSeconds: remainingSeconds,
      keep: false,
      ...providerRequest,
    } satisfies LeaseRequest),
  });
}

function privateAWSWorkspaceLeaseFields(
  workspace: WorkspaceRecord,
  policy: AWSPrivateWorkspaceConfig,
): Partial<LeaseRequest> {
  return {
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
    awsSSMBootstrapCommand: workspaceSSMBootstrapCommand(workspace),
    awsSSMLogGroup: policy.ssmLogGroup,
    capacity: {
      market: policy.market,
      fallback: "none",
      regions: [policy.region],
      hints: false,
    },
    sshUser: "crabbox",
    sshFallbackPorts: [],
    workRoot: privateAWSWorkspaceWorkRoot,
  };
}

function privateAWSWorkspaceCapability(
  policy: AWSPrivateWorkspaceConfig | undefined,
  env: Env,
): ProviderWorkspaceCapability {
  const requiredPolicy = (): AWSPrivateWorkspaceConfig => {
    if (!policy) {
      throw new Error("private AWS workspace policy is unavailable");
    }
    return policy;
  };
  return {
    requiresCommand: true,
    supportsDesktop: false,
    supportsPrewarm: false,
    leaseRequestFields: (workspace) => privateAWSWorkspaceLeaseFields(workspace, requiredPolicy()),
    recoveryLeaseRequestFields: (workspace, server) => {
      const activePolicy = requiredPolicy();
      return {
        ...privateAWSWorkspaceLeaseFields(workspace, activePolicy),
        serverType: server.serverType,
        serverTypeExplicit: true,
        awsUseStockImage: true,
      };
    },
    recoveredReady: (server) => server.awsSSMCommandStatus === "Success",
    recoveredHost: () => "",
    applyRecoveredEvidence: (lease, config, server) => {
      if (server.awsSSMCommandID) {
        lease.awsSSMCommandID = server.awsSSMCommandID;
      }
      if (server.awsSSMCommandStatus) {
        lease.awsSSMCommandStatus = server.awsSSMCommandStatus;
      }
      lease.awsSSMLogGroup = config.awsSSMLogGroup;
    },
    bootstrapEvidence: (lease, status) => ({
      transport: "ssm",
      status:
        lease?.awsSSMCommandStatus ??
        (status === "ready" ? "Success" : status === "failed" ? "Failed" : "Pending"),
      ...(lease?.awsSSMCommandID ? { commandId: lease.awsSSMCommandID } : {}),
      ...(lease?.awsSSMLogGroup?.trim() || env.CRABBOX_WORKSPACE_AWS_SSM_LOG_GROUP?.trim()
        ? {
            logGroup:
              lease?.awsSSMLogGroup?.trim() || env.CRABBOX_WORKSPACE_AWS_SSM_LOG_GROUP?.trim(),
          }
        : {}),
    }),
    log: (event, fields) =>
      privateAWSWorkspaceLifecycleLog(event, {
        ...(event === "create_accepted"
          ? {
              region: requiredPolicy().region,
              instance_type: requiredPolicy().instanceTypes[0],
            }
          : {}),
        ...fields,
      }),
  };
}

function workspaceSSMBootstrapCommand(workspace: WorkspaceRecord): string {
  const workspaceParent = `${privateAWSWorkspaceWorkRoot}/workspaces`;
  const workspaceRoot = `${workspaceParent}/${workspace.id}`;
  const serviceName = "crabbox-workspace.service";
  const startScript = "/usr/local/lib/crabbox/workspace-start";
  const successMarker = "/var/lib/crabbox/workspace-bootstrap-success";
  const branch = workspace.branch.trim() || "main";
  const command = workspace.command.trim();
  if (!command) {
    throw new Error("private AWS workspaces require an explicit command");
  }
  const setup = [
    "set -euo pipefail",
    "exec 9>/var/lock/crabbox-workspace-bootstrap.lock",
    "flock -x -w 600 9",
    `if test -f ${shellQuote(successMarker)} && systemctl is-active --quiet ${shellQuote(serviceName)}; then`,
    `  systemctl show --no-pager --property=ActiveState,SubState,Result,ExecMainStatus ${shellQuote(serviceName)}`,
    "  exit 0",
    "fi",
    `for workspace_ancestor in /work ${shellQuote(privateAWSWorkspaceWorkRoot)} ${shellQuote(workspaceParent)}; do`,
    '  test -d "$workspace_ancestor"',
    '  test ! -L "$workspace_ancestor"',
    '  test "$(stat -c %U:%G "$workspace_ancestor")" = root:root',
    "done",
    `test ! -L ${shellQuote(workspaceRoot)}`,
  ];
  if (workspace.repo) {
    const repoURL = `https://github.com/${workspace.repo}.git`;
    const cloneTemplate = `${workspaceRoot}.clone.XXXXXX`;
    setup.push(
      `if ! runuser -u crabbox -- git -C ${shellQuote(workspaceRoot)} rev-parse --verify 'HEAD^{commit}' >/dev/null 2>&1; then`,
      `  clone_root=$(mktemp -d ${shellQuote(cloneTemplate)})`,
      '  chown crabbox:crabbox "$clone_root"',
      `  if ! runuser -u crabbox -- git clone --quiet --depth=1 --branch ${shellQuote(branch)} ${shellQuote(repoURL)} "$clone_root" >/dev/null 2>&1; then`,
      '    rm -rf "$clone_root"',
      "    exit 1",
      "  fi",
      `  rm -rf ${shellQuote(workspaceRoot)}`,
      `  mv "$clone_root" ${shellQuote(workspaceRoot)}`,
      "fi",
    );
  } else {
    setup.push(`install -d -m 0755 -o crabbox -g crabbox ${shellQuote(workspaceRoot)}`);
  }
  const runner = [
    "#!/usr/bin/env bash",
    "set -euo pipefail",
    `cd ${shellQuote(workspaceRoot)}`,
    `exec /bin/bash -lc ${shellQuote(command)}`,
    "",
  ].join("\n");
  const unit = [
    "[Unit]",
    "Description=Crabbox private workspace command",
    "Wants=network-online.target",
    "After=network-online.target",
    "",
    "[Service]",
    "Type=simple",
    "User=crabbox",
    `WorkingDirectory=${workspaceRoot}`,
    `ExecStart=${startScript}`,
    "Restart=on-failure",
    "RestartSec=5",
    "StandardOutput=journal",
    "StandardError=journal",
    "",
    "[Install]",
    "WantedBy=multi-user.target",
    "",
  ].join("\n");
  setup.push(
    `install -d -m 0755 ${shellQuote(startScript.slice(0, startScript.lastIndexOf("/")))}`,
    `printf %s ${shellQuote(runner)} > ${shellQuote(startScript)}`,
    `chmod 0755 ${shellQuote(startScript)}`,
    `printf %s ${shellQuote(unit)} > ${shellQuote(`/etc/systemd/system/${serviceName}`)}`,
    "systemctl daemon-reload",
    `systemctl enable --now ${shellQuote(serviceName)}`,
    `if ! timeout 60 bash -c ${shellQuote(`until systemctl is-active --quiet ${serviceName}; do systemctl is-failed --quiet ${serviceName} && exit 1; sleep 1; done`)}; then`,
    `  systemctl show --no-pager --property=ActiveState,SubState,Result,ExecMainStatus ${shellQuote(serviceName)} || true`,
    "  exit 1",
    "fi",
    `systemctl show --no-pager --property=ActiveState,SubState,Result,ExecMainStatus ${shellQuote(serviceName)}`,
    `touch ${shellQuote(successMarker)}`,
  );
  return setup.join("\n");
}

function workspaceRecordHeaders(workspace: WorkspaceRecord): Headers {
  const org = orgAuthLabelFromKey(workspace.org);
  if (org === undefined) {
    throw new Error("workspace organization identity must be recreated before provisioning");
  }
  return new Headers({
    "content-type": "application/json",
    "x-crabbox-owner": workspace.owner,
    "x-crabbox-org": org,
  });
}

function workspaceHTTPStatus(workspace: WorkspaceRecord, lease?: LeaseRecord): number {
  return workspaceStatus(workspace, lease) === "provisioning" ? 202 : 200;
}

function workspaceConflictResponse(
  workspace: WorkspaceRecord,
  profile: string,
  repo: string,
  branch: string,
  command: string,
  desktop: boolean,
  ttlSeconds: number,
  idleTimeoutSeconds: number,
): Response | undefined {
  const desktopMatches =
    workspace.desktop === desktop ||
    (workspace.desktopCapabilityVersion === undefined && !workspace.desktop && desktop);
  if (
    workspace.profile === profile &&
    (workspace.repo ?? "") === repo &&
    (workspace.branch ?? "main") === branch &&
    (workspace.command ?? "exec bash -l") === command &&
    desktopMatches &&
    workspace.ttlSeconds === ttlSeconds &&
    workspace.idleTimeoutSeconds === idleTimeoutSeconds
  ) {
    return undefined;
  }
  return json(
    {
      error: "workspace_id_conflict",
      message: "workspace id already exists with different settings",
    },
    { status: 409 },
  );
}

function workspaceProvisionDeadline(workspace: WorkspaceRecord): number {
  return Date.parse(workspace.createdAt) + workspace.ttlSeconds * 1000;
}

function workspaceProvisionRecoveryDeadline(
  workspace: WorkspaceRecord,
  lease?: LeaseRecord,
): number {
  const requestStartedAt = Date.parse(lease?.provisioningRequestStartedAt ?? "");
  const hardDeadline = workspaceProvisionDeadline(workspace);
  return Math.min(
    hardDeadline,
    Number.isFinite(requestStartedAt)
      ? requestStartedAt + workspaceProvisionRecoveryGraceMs
      : hardDeadline,
  );
}

function workspaceProvisioningNeedsRecovery(
  workspace: WorkspaceRecord,
  lease: LeaseRecord,
  now = Date.now(),
): boolean {
  if (lease.providerKeyCleanupPending) {
    return false;
  }
  if (lease.cloudID) {
    return false;
  }
  if (
    lease.state !== "provisioning" &&
    lease.state !== "failed" &&
    !(lease.state === "released" && lease.releaseDeletesServer === true)
  ) {
    return false;
  }
  const deadline =
    lease.state === "provisioning" || lease.provisioningResourceMayExist === true
      ? workspaceProvisionRecoveryDeadline(workspace, lease)
      : workspaceProvisionDeadline(workspace);
  return now < deadline;
}

function workspaceNextReconcileAt(
  workspace: WorkspaceRecord,
  lease?: LeaseRecord,
  now = Date.now(),
): number | undefined {
  if (!isCurrentOrgKey(workspace.org) && !workspace.releaseRequestedAt) return undefined;
  if (lease && !workspaceLeaseMatchesCleanup(workspace, lease)) {
    return workspace.error ? undefined : now;
  }
  if (lease?.state === "released" && lease.releaseDeletesServer === false) {
    return undefined;
  }
  if (lease?.providerKeyCleanupPending) {
    const claimDeadline = cleanupClaimDeadline(lease);
    if (lease.cleanupStartedAt && Number.isFinite(claimDeadline)) {
      return claimDeadline;
    }
    const retryAt = Date.parse(lease.cleanupRetryAt ?? "");
    return Number.isFinite(retryAt) && retryAt > now ? retryAt : now;
  }
  const claimExpiresAt = Date.parse(workspace.provisionClaimExpiresAt ?? "");
  const deferredUntil = Date.parse(workspace.reconcileAfter ?? "");
  const provisioningDeadline =
    lease?.state === "provisioning" || lease?.provisioningResourceMayExist === true
      ? workspaceProvisionRecoveryDeadline(workspace, lease)
      : workspaceProvisionDeadline(workspace);
  if (Number.isFinite(deferredUntil) && deferredUntil > now) {
    return provisioningDeadline > now
      ? Math.min(deferredUntil, provisioningDeadline)
      : deferredUntil;
  }
  if (!lease && Number.isFinite(claimExpiresAt) && claimExpiresAt > now) {
    return claimExpiresAt;
  }
  if (
    lease?.state === "provisioning" ||
    lease?.state === "failed" ||
    (lease?.state === "released" && lease.releaseDeletesServer === true && !lease.cloudID)
  ) {
    if (Number.isFinite(claimExpiresAt) && claimExpiresAt > now) {
      return claimExpiresAt;
    }
  }
  if (workspace.releaseRequestedAt) {
    if (!lease || lease.state === "expired") return undefined;
    if (lease.cleanupStartedAt) {
      const claimDeadline = cleanupClaimDeadline(lease);
      return Number.isFinite(claimDeadline) ? claimDeadline : now + workspaceReconcileIntervalMs;
    }
    const retryAt = Date.parse(lease.cleanupRetryAt ?? "");
    if (Number.isFinite(retryAt) && retryAt > now) return retryAt;
    if (lease.state === "provisioning") return now;
    if (
      lease.state === "released" &&
      !lease.cleanupError &&
      lease.releaseDeletesServer === undefined
    ) {
      return undefined;
    }
    return now;
  }
  if (workspace.error) return undefined;
  if (!lease || lease.state === "failed") return now;
  if (lease.state === "provisioning") return now;
  return undefined;
}

function workspaceStatus(
  workspace: WorkspaceRecord,
  lease?: LeaseRecord,
): "provisioning" | "ready" | "stopping" | "stopped" | "expired" | "failed" {
  if (lease && !workspaceOwnsLease(workspace, lease)) return "failed";
  if (workspace.releaseRequestedAt) {
    if (!lease) return "stopped";
    if (
      lease?.state === "released" &&
      !lease.cleanupStartedAt &&
      !lease.cleanupError &&
      lease.releaseDeletesServer === undefined
    ) {
      return "stopped";
    }
    if (lease?.state === "expired") return "expired";
    return "stopping";
  }
  if (workspace.error && !lease) return "failed";
  switch (lease?.state) {
    case "active": {
      if (Date.parse(lease.expiresAt) <= Date.now()) return "expired";
      if (lease.cleanupStartedAt || lease.cleanupError) return "failed";
      return "ready";
    }
    case "released":
      return "stopped";
    case "expired":
      return "expired";
    case "failed":
      return workspace.error || lease.failureError ? "failed" : "provisioning";
    default:
      return "provisioning";
  }
}

function workspaceTerminalTimestamp(
  workspace: WorkspaceRecord,
  lease?: LeaseRecord,
): number | undefined {
  const terminal =
    (!lease && Boolean(workspace.releaseRequestedAt || workspace.error)) ||
    lease?.state === "expired" ||
    (lease?.state === "released" &&
      !lease.cleanupStartedAt &&
      !lease.cleanupError &&
      lease.releaseDeletesServer === undefined) ||
    (lease?.state === "failed" &&
      Boolean(workspace.error) &&
      lease.provisioningResourceMayExist !== true);
  if (!terminal) {
    return undefined;
  }
  const timestamps = [workspace.updatedAt, lease?.endedAt, lease?.releasedAt, lease?.updatedAt]
    .map((value) => Date.parse(value ?? ""))
    .filter(Number.isFinite);
  return timestamps.length > 0 ? Math.max(...timestamps) : undefined;
}

function workspaceOwnsLease(workspace: WorkspaceRecord, lease: LeaseRecord): boolean {
  return (
    lease.workspaceID === workspace.id &&
    lease.id === workspace.leaseID &&
    lease.owner === workspace.owner &&
    sameOrgIdentityKey(lease.org, workspace.org)
  );
}

function workspaceReleaseOrCleanupInProgress(
  workspace: WorkspaceRecord,
  lease: LeaseRecord,
): boolean {
  return Boolean(
    workspace.releaseRequestedAt ||
    lease.runtimeAdapterDeleteRequestedAt ||
    lease.cleanupStartedAt ||
    lease.releaseDeletesServer ||
    lease.state === "released" ||
    lease.state === "expired",
  );
}

function applyRecoveredServerIdentity(lease: LeaseRecord, server: ProviderMachine): void {
  lease.cloudID = server.cloudID || String(server.id);
  lease.serverID = server.id;
  lease.serverName = server.name;
  lease.serverType = server.serverType;
  if (server.region) {
    lease.region = server.region;
  }
}

/** Legacy orgs can prove only cleanup lifecycle linkage, never user access or adoption. */
function workspaceLeaseMatchesCleanup(workspace: WorkspaceRecord, lease: LeaseRecord): boolean {
  if (workspaceOwnsLease(workspace, lease)) return true;
  return (
    Boolean(workspace.releaseRequestedAt) &&
    isLegacyOrgKey(workspace.org) &&
    isLegacyOrgKey(lease.org) &&
    lease.workspaceID === workspace.id &&
    lease.id === workspace.leaseID &&
    lease.owner === workspace.owner &&
    lease.org === workspace.org
  );
}

function workspaceTerminalError(
  workspace: WorkspaceRecord,
  lease: LeaseRecord | undefined,
  env: Env,
): string | undefined {
  const sshError = workspaceSSHError(workspace, lease, env);
  if (sshError) return sshError;
  if (!workspaceTerminalPublicURL(env)) return "workspace public URL is not configured";
  return undefined;
}

function workspaceSSHError(
  workspace: WorkspaceRecord,
  lease: LeaseRecord | undefined,
  env: Env,
): string | undefined {
  if (workspaceStatus(workspace, lease) !== "ready") return "workspace is not ready";
  if (!lease || !workspaceOwnsLease(workspace, lease)) return "workspace lease is unavailable";
  if (!lease.host?.trim()) return "workspace SSH host is unavailable";
  if (!workspace.sshHostKeySha256) return "workspace SSH host identity is unavailable";
  if (!env.CRABBOX_WORKSPACE_SSH_PRIVATE_KEY?.trim()) {
    return "workspace terminal SSH access is not configured";
  }
  return undefined;
}

function workspaceNativeVNCError(
  workspace: WorkspaceRecord,
  lease: LeaseRecord | undefined,
  env: Env,
): string | undefined {
  if (!workspace.desktop) return "workspace desktop access was not requested";
  const sshError = workspaceSSHError(workspace, lease, env);
  if (sshError) return sshError;
  if (!workspacePublicURL(env)) return "workspace public URL is not configured";
  return undefined;
}

function workspacePublicURL(env: Env): string | undefined {
  const value = env.CRABBOX_PUBLIC_URL?.trim();
  if (!value) return undefined;
  try {
    const url = new URL(value);
    if (url.protocol !== "https:" && !(url.protocol === "http:" && url.hostname === "127.0.0.1")) {
      return undefined;
    }
    url.pathname = url.pathname.replace(/\/+$/, "");
    url.search = "";
    url.hash = "";
    return url.toString().replace(/\/$/, "");
  } catch {
    return undefined;
  }
}

// Controller-to-controller URL; callers attach with bearer auth through their browser-facing proxy.
function workspaceTerminalURL(
  workspace: WorkspaceRecord,
  lease: LeaseRecord | undefined,
  env: Env,
): string | undefined {
  if (workspaceTerminalError(workspace, lease, env)) return undefined;
  const publicURL = workspaceTerminalPublicURL(env);
  if (!publicURL) return undefined;
  publicURL.protocol = "wss:";
  publicURL.pathname = `/v1/workspaces/${encodeURIComponent(workspace.id)}/terminal`;
  publicURL.search = "?flow=ack-v1";
  publicURL.hash = "";
  return publicURL.toString();
}

function workspaceTerminalPublicURL(env: Env): URL | undefined {
  try {
    const url = new URL(env.CRABBOX_PUBLIC_URL ?? "");
    return url.protocol === "https:" ? url : undefined;
  } catch {
    return undefined;
  }
}

async function workspaceTerminalMessage(
  value: string | ArrayBuffer | Blob,
): Promise<string | Uint8Array> {
  if (typeof value === "string") return value;
  if (value instanceof ArrayBuffer) return new Uint8Array(value);
  return new Uint8Array(await value.arrayBuffer());
}

async function connectWorkspaceSSH(
  privateKey: string,
  expectedHostKey: string,
  lease: LeaseRecord,
  options: {
    shouldStop?: () => boolean;
    connecting?: (client: SSHClient) => void;
  } = {},
): Promise<SSHClient> {
  const readyDeadline = Date.now() + workspaceTerminalSSHReadyTimeoutMs;
  const ports = uniqueNonEmpty([lease.sshPort, ...(lease.sshFallbackPorts ?? [])]);
  let lastError: unknown = new Error("workspace SSH service is not ready");
  let observedHostKey = "";
  while (Date.now() < readyDeadline) {
    for (const port of ports) {
      if (options.shouldStop?.()) throw new Error("workspace connection closed");
      const candidate = new SSHClientConstructor();
      options.connecting?.(candidate);
      try {
        // oxlint-disable-next-line eslint/no-await-in-loop -- SSH readiness retries are sequential.
        await new Promise<void>((resolve, reject) => {
          let settled = false;
          const settle = (callback: () => void) => {
            if (settled) return;
            settled = true;
            callback();
          };
          candidate
            .once("ready", () => settle(resolve))
            .once("error", (error) => settle(() => reject(error)))
            .once("close", () =>
              settle(() => reject(new Error("SSH connection closed before ready"))),
            )
            .connect({
              host: lease.host,
              port: Number.parseInt(port || "22", 10) || 22,
              username: lease.sshUser || "root",
              privateKey,
              readyTimeout: 10_000,
              keepaliveInterval: 15_000,
              keepaliveCountMax: 3,
              algorithms: {
                serverHostKey: ["ssh-ed25519"],
                cipher: ["aes128-ctr", "aes192-ctr", "aes256-ctr"],
                hmac: [
                  "hmac-sha2-256-etm@openssh.com",
                  "hmac-sha2-512-etm@openssh.com",
                  "hmac-sha2-256",
                  "hmac-sha2-512",
                ],
              },
              hostHash: "sha256",
              hostVerifier: (fingerprint: string) => {
                observedHostKey = fingerprint;
                return expectedHostKey === fingerprint;
              },
            });
        });
        if (options.shouldStop?.()) {
          candidate.end();
          throw new Error("workspace connection closed");
        }
        return candidate;
      } catch (error) {
        lastError = error;
        candidate.end();
      }
    }
    if (!options.shouldStop?.() && Date.now() < readyDeadline) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- delay before the next SSH sweep.
      await new Promise<void>((resolve) => setTimeout(resolve, 2_000));
    }
  }
  if (observedHostKey && observedHostKey !== expectedHostKey) {
    throw new Error(
      `workspace SSH host key mismatch expected=${expectedHostKey.slice(0, 16)} observed=${observedHostKey.slice(0, 16)}`,
    );
  }
  throw lastError;
}

async function readWorkspaceVNCPassword(client: SSHClient, lease: LeaseRecord): Promise<string> {
  const output = await new Promise<Uint8Array>((resolve, reject) => {
    client.exec(workspaceVNCPasswordCommand(lease), (error, channel) => {
      if (error) {
        reject(error);
        return;
      }
      const chunks: Uint8Array[] = [];
      let bytes = 0;
      let exitCode: number | undefined;
      let settled = false;
      const fail = (failure: Error) => {
        if (settled) return;
        settled = true;
        reject(failure);
      };
      channel.on("data", (chunk: Uint8Array) => {
        if (settled) return;
        bytes += chunk.byteLength;
        if (bytes > 4096) {
          channel.close();
          fail(new Error("workspace VNC password output is too large"));
          return;
        }
        chunks.push(chunk.slice());
      });
      channel.once("exit", (code?: number) => {
        exitCode = code;
      });
      channel.once("close", () => {
        if (settled) return;
        if (exitCode !== undefined && exitCode !== 0) {
          fail(new Error("workspace VNC password command failed"));
          return;
        }
        const joined = new Uint8Array(bytes);
        let offset = 0;
        for (const chunk of chunks) {
          joined.set(chunk, offset);
          offset += chunk.byteLength;
        }
        settled = true;
        resolve(joined);
      });
    });
  });
  const password = new TextDecoder().decode(output).trim();
  if (
    !password ||
    password.length > 256 ||
    [...password].some((character) => {
      const code = character.charCodeAt(0);
      return code === 0 || code === 10 || code === 13;
    })
  ) {
    throw new Error("workspace VNC password is invalid");
  }
  return password;
}

function workspaceVNCPasswordCommand(lease: LeaseRecord): string {
  if (lease.target === "windows") {
    return "powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command \"Get-Content -Raw -LiteralPath 'C:\\\\ProgramData\\\\crabbox\\\\vnc.password'\"";
  }
  if (lease.target === "macos") return "sudo cat '/var/db/crabbox/vnc.password'";
  return "cat '/var/lib/crabbox/vnc.password'";
}

function workspaceTerminalDataLength(value: string | ArrayBuffer | Blob | Uint8Array): number {
  if (typeof value === "string") {
    if (value.length > workspaceTerminalMaxBufferedBytes) {
      return workspaceTerminalMaxBufferedBytes + 1;
    }
    return textEncoder.encode(value).byteLength;
  }
  if (value instanceof Blob) return value.size;
  return value.byteLength;
}

async function writeWorkspaceTerminalChannel(
  channel: ClientChannel,
  value: string | Uint8Array,
): Promise<void> {
  if (channel.write(value)) return;
  await new Promise<void>((resolve, reject) => {
    const cleanup = () => {
      clearTimeout(timeout);
      channel.off("drain", drained);
      channel.off("close", closed);
    };
    const drained = () => {
      cleanup();
      resolve();
    };
    const closed = () => {
      cleanup();
      reject(new Error("SSH terminal channel closed during backpressure"));
    };
    const timeout = setTimeout(() => {
      cleanup();
      reject(new Error("SSH terminal channel backpressure timed out"));
    }, 10_000);
    channel.once("drain", drained);
    channel.once("close", closed);
  });
}

function workspaceTerminalResize(value: string): { cols: number; rows: number } | undefined {
  if (!value.startsWith("{") || value.length > 200) return undefined;
  try {
    const input = JSON.parse(value) as Record<string, unknown>;
    const cols = input["cols"];
    const rows = input["rows"];
    if (
      input["type"] !== "resize" ||
      !Number.isInteger(cols) ||
      !Number.isInteger(rows) ||
      Number(cols) < 2 ||
      Number(cols) > 500 ||
      Number(rows) < 1 ||
      Number(rows) > 200
    ) {
      return undefined;
    }
    return { cols: Number(cols), rows: Number(rows) };
  } catch {
    return undefined;
  }
}

function workspaceTerminalAcknowledgement(value: string): number | undefined {
  if (!value.startsWith("{") || value.length > 100) return undefined;
  try {
    const input = JSON.parse(value) as Record<string, unknown>;
    const bytes = input["bytes"];
    return input["type"] === "ack" &&
      Number.isInteger(bytes) &&
      Number(bytes) > 0 &&
      Number(bytes) <= workspaceTerminalMaxBufferedBytes
      ? Number(bytes)
      : undefined;
  } catch {
    return undefined;
  }
}

export function workspaceTerminalOriginAllowed(request: Request, env: Env): boolean {
  const origin = request.headers.get("origin");
  if (!origin) return true;
  const publicURL = env.CRABBOX_PUBLIC_URL?.trim();
  if (!publicURL) return false;
  try {
    return new URL(origin).origin === new URL(publicURL).origin;
  } catch {
    return false;
  }
}

function workspaceTerminalSocketBufferedBytes(socket: WebSocket): number {
  const value = (socket as WebSocket & { bufferedAmount?: unknown }).bufferedAmount;
  return typeof value === "number" && Number.isFinite(value) && value > 0 ? value : 0;
}

function workspaceTerminalBootstrapCommand(workspace: WorkspaceRecord, lease: LeaseRecord): string {
  const workRoot = lease.workRoot?.trim() || "/workspace";
  const workspaceParent = `${workRoot.replace(/\/+$/u, "")}/workspaces`;
  const workspaceRoot = `${workspaceParent}/${workspace.id}`;
  const branch = workspace.branch?.trim() || "main";
  const command = workspace.command?.trim() || "exec bash -l";
  const setup = ["set -e", "umask 077", `mkdir -p ${shellQuote(workspaceParent)}`];
  if (workspace.repo) {
    const repoURL = `https://github.com/${workspace.repo}.git`;
    const cloneTemplate = `${workspaceRoot}.clone.XXXXXX`;
    setup.push(
      `if ! git -C ${shellQuote(workspaceRoot)} rev-parse --verify 'HEAD^{commit}' >/dev/null 2>&1; then`,
      `  clone_root=$(mktemp -d ${shellQuote(cloneTemplate)})`,
      `  if ! git clone --depth=1 --branch ${shellQuote(branch)} ${shellQuote(repoURL)} "$clone_root"; then`,
      '    rm -rf "$clone_root"',
      "    exit 1",
      "  fi",
      `  rm -rf ${shellQuote(workspaceRoot)}`,
      `  mv "$clone_root" ${shellQuote(workspaceRoot)}`,
      "fi",
    );
  } else {
    setup.push(`mkdir -p ${shellQuote(workspaceRoot)}`);
  }
  setup.push(
    `cd ${shellQuote(workspaceRoot)}`,
    "printf '\\033[2J\\033[H'",
    "set +e",
    `bash -lc ${shellQuote(command)}`,
    "command_status=$?",
    "printf '\\nWorkspace command exited with status %s. The terminal remains available.\\n' \"$command_status\"",
    "exec bash -l",
  );
  const runner = `bash -lc ${shellQuote(setup.join("\n"))}`;
  const session = `crabbox-workspace-${workspace.id}`.slice(0, 80);
  return [
    "if systemctl cat crabbox-workspace-ready.service >/dev/null 2>&1; then",
    "  timeout 20m bash -c 'until test -f /run/crabbox/workspace-ready; do sleep 2; done' || exit $?",
    "else",
    "  timeout 2m bash -c 'until crabbox-ready >/dev/null 2>&1; do sleep 2; done' || exit $?",
    "fi",
    "if command -v tmux >/dev/null 2>&1; then",
    `  exec tmux new-session -A -s ${shellQuote(session)} ${shellQuote(runner)}`,
    "else",
    `  exec ${runner}`,
    "fi",
  ].join("\n");
}

function shellQuote(value: string): string {
  return `'${value.replaceAll("'", `'"'"'`)}'`;
}

export function boundedSocketReason(value: string): string {
  let safe = "";
  for (const character of value) {
    const code = character.charCodeAt(0);
    safe += code < 0x20 || code === 0x7f ? " " : character;
  }
  const normalized = safe.replace(/\s+/gu, " ").trim() || "terminal error";
  let bounded = "";
  let bytes = 0;
  for (const character of normalized) {
    const size = textEncoder.encode(character).byteLength;
    if (bytes + size > 120) break;
    bounded += character;
    bytes += size;
  }
  return bounded || "terminal error";
}

function workspaceResponse(
  workspace: WorkspaceRecord,
  lease?: LeaseRecord,
  env?: Env,
  workspaceCapability?: ProviderWorkspaceCapability,
): Record<string, unknown> {
  const status = workspaceStatus(workspace, lease);
  const terminalUrl = env ? workspaceTerminalURL(workspace, lease, env) : undefined;
  const nativeVnc = env ? !workspaceNativeVNCError(workspace, lease, env) : false;
  const bootstrap = workspaceCapability?.bootstrapEvidence(lease, status);
  const message =
    workspace.error ??
    (lease && !workspaceOwnsLease(workspace, lease)
      ? "workspace lease reservation conflicts with another lifecycle"
      : undefined) ??
    lease?.failureError ??
    (lease?.state === "failed" ? "workspace provisioning recovery pending" : undefined) ??
    lease?.cleanupError ??
    (status === "ready"
      ? "workspace ready"
      : status === "failed"
        ? "workspace provisioning failed"
        : `workspace ${status}`);
  return {
    id: workspace.id,
    workspaceId: workspace.id,
    providerResourceId: workspace.leaseID,
    leaseId: workspace.leaseID,
    provider: workspace.provider,
    status,
    profile: workspace.profile,
    createdAt: workspace.createdAt,
    updatedAt: lease?.updatedAt ?? workspace.updatedAt,
    ...(lease?.cloudID ? { cloudResourceId: lease.cloudID } : {}),
    ...(lease?.region ? { region: lease.region } : {}),
    ...(lease?.serverType ? { serverType: lease.serverType } : {}),
    ...(bootstrap ? { bootstrap } : {}),
    capabilities: {
      terminal: Boolean(terminalUrl),
      takeover: false,
      vnc: false,
      desktop: false,
      nativeVnc,
      logs: false,
      artifacts: false,
    },
    ...(terminalUrl ? { attachUrl: terminalUrl } : {}),
    ...(lease?.expiresAt ? { expiresAt: lease.expiresAt } : {}),
    message: env ? coordinatorDiagnosticText(env, message) : redactDiagnosticSecrets(message),
  };
}

function privateAWSWorkspaceLifecycleLog(
  event: "create_accepted" | "ready" | "recovered_ready" | "delete_requested" | "terminated",
  fields: Record<string, string | undefined>,
): void {
  console.info(
    JSON.stringify({
      component: "crabbox_private_aws_workspace",
      event,
      ...Object.fromEntries(
        Object.entries(fields).filter(([, value]) => value !== undefined && value !== ""),
      ),
    }),
  );
}

async function workspaceResponseError(response: Response, fallback: string): Promise<string> {
  const body = (await response.json().catch(() => undefined)) as
    | { message?: unknown; error?: unknown }
    | undefined;
  for (const value of [body?.message, body?.error]) {
    if (typeof value === "string" && value.trim()) {
      return value.trim();
    }
  }
  return fallback;
}

function newRunID(): string {
  const bytes = new Uint8Array(6);
  crypto.getRandomValues(bytes);
  return `run_${[...bytes].map((byte) => byte.toString(16).padStart(2, "0")).join("")}`;
}

function newWebVNCTicket(): string {
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  return `wvnc_${[...bytes].map((byte) => byte.toString(16).padStart(2, "0")).join("")}`;
}

function newWebVNCSessionID(prefix: "agent" | "viewer"): string {
  const bytes = new Uint8Array(8);
  crypto.getRandomValues(bytes);
  return `${prefix}_${[...bytes].map((byte) => byte.toString(16).padStart(2, "0")).join("")}`;
}

function newCodeTicket(): string {
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  return `code_${[...bytes].map((byte) => byte.toString(16).padStart(2, "0")).join("")}`;
}

function newEgressTicket(): string {
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  return `egress_${[...bytes].map((byte) => byte.toString(16).padStart(2, "0")).join("")}`;
}

function newRuntimeAdapterTicket(): string {
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  return `adapter_${[...bytes].map((byte) => byte.toString(16).padStart(2, "0")).join("")}`;
}

function newNativeVNCTicket(): string {
  return randomHexToken("native_vnc_");
}

function validNativeVNCTicket(value: string): boolean {
  return /^native_vnc_[a-f0-9]{32}$/.test(value);
}

function newCodeViewerTicket(): string {
  return randomHexToken("code_view_");
}

function newCodeViewerSession(): string {
  return randomHexToken("code_session_");
}

function randomHexToken(prefix: string): string {
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  return `${prefix}${[...bytes].map((byte) => byte.toString(16).padStart(2, "0")).join("")}`;
}

function newEgressSessionID(): string {
  const bytes = new Uint8Array(6);
  crypto.getRandomValues(bytes);
  return `egress_${[...bytes].map((byte) => byte.toString(16).padStart(2, "0")).join("")}`;
}

function egressSocketKey(leaseID: string, sessionID: string): string {
  return `${leaseID}\u0000${sessionID}`;
}

function egressSocketLeaseID(key: string): string {
  return key.split("\u0000", 1)[0] ?? key;
}

export function shouldActivateEgressSession(
  previous: { sessionID: string; createdAt: string } | undefined,
  sessionID: string,
  createdAt: string,
): boolean {
  return !previous || previous.sessionID === sessionID || previous.createdAt <= createdAt;
}

function validLeaseID(value: string | undefined): value is string {
  return typeof value === "string" && /^cbx_[a-f0-9]{12}$/.test(value);
}

function validRegisteredLeaseID(value: string | undefined): value is string {
  return typeof value === "string" && /^[A-Za-z0-9][A-Za-z0-9_.:-]{2,127}$/.test(value);
}

function validWebVNCTicket(value: string | undefined): value is string {
  return typeof value === "string" && /^wvnc_[a-f0-9]{32}$/.test(value);
}

function validRuntimeAdapterTicket(value: string | undefined): value is string {
  return typeof value === "string" && /^adapter_[a-f0-9]{32}$/.test(value);
}

function runtimeAdapterTicketFromRequest(request: Request): string {
  // Adapter-agent upgrades bypass user auth; keep the single-use ticket out of URLs and dedicated proxy headers.
  const authorization = request.headers.get("authorization") ?? "";
  const [scheme, token] = authorization.split(/\s+/, 2);
  return scheme?.toLowerCase() === "bearer" ? (token ?? "") : "";
}

function validWebVNCSessionID(value: string | undefined): value is string {
  return typeof value === "string" && /^(agent|viewer)_[A-Za-z0-9_.:-]{6,80}$/.test(value);
}

function webVNCAgentCapabilities(request: Request): Set<string> {
  const params = new URL(request.url).searchParams;
  const raw = [params.get("capabilities"), ...params.getAll("cap")].filter(
    (value): value is string => typeof value === "string",
  );
  return new Set(
    raw.flatMap((value) => value.split(",").map((item) => item.trim())).filter(Boolean),
  );
}

function isReservedWebVNCControlFrame(message: unknown): boolean {
  if (typeof message !== "string" || message[0] !== "{") {
    return false;
  }
  try {
    const parsed = JSON.parse(message) as { type?: unknown };
    return parsed.type === "desktop_theme";
  } catch {
    return false;
  }
}

function webVNCBufferKey(leaseID: string, agentID: string): string {
  return `${leaseID}:${agentID}`;
}

function webVNCViewerLabel(owner: string): string {
  const trimmed = owner.trim();
  if (!trimmed) {
    return "someone";
  }
  const at = trimmed.indexOf("@");
  return at > 0 ? trimmed.slice(0, at) : trimmed;
}

function validCodeTicket(value: string | undefined): value is string {
  return typeof value === "string" && /^code_[a-f0-9]{32}$/.test(value);
}

function validCodeViewerTicket(value: string | undefined): value is string {
  return typeof value === "string" && /^code_view_[a-f0-9]{32}$/.test(value);
}

function validCodeViewerSession(value: string | undefined): value is string {
  return typeof value === "string" && /^code_session_[a-f0-9]{32}$/.test(value);
}

function validPortalSessionHash(value: string | undefined): value is string {
  return typeof value === "string" && /^[a-f0-9]{64}$/.test(value);
}

function canonicalCodeReturnTo(request: Request, leaseID: string): string {
  const url = new URL(request.url);
  const match = /^\/portal\/leases\/[^/]+\/code(.*)$/.exec(url.pathname);
  const suffix = match?.[1] ?? "/";
  return `/portal/leases/${encodeURIComponent(leaseID)}/code${suffix}${url.search}`;
}

function requestAuthType(request: Request): AuthContext["auth"] {
  const auth = request.headers.get("x-crabbox-auth");
  return auth === "bearer" || auth === "github" || auth === "proxy" ? auth : "github";
}

function trustedAdminGrantVersion(request: Request): string | undefined {
  const version = request.headers.get("x-crabbox-admin-grant-version")?.trim();
  return version && /^[a-f0-9]{64}$/.test(version) ? version : undefined;
}

async function adminGrantForRequest(request: Request, admin: boolean): Promise<CachedAdminGrant> {
  if (!admin) {
    return {};
  }
  const auth = requestAuthType(request);
  const adminGrantVersion = trustedAdminGrantVersion(request);
  if (auth === "github") {
    const login = request.headers.get("x-crabbox-github-login")?.trim();
    return {
      auth,
      ...(login ? { login } : {}),
      ...(adminGrantVersion ? { adminGrantVersion } : {}),
    };
  }
  if (auth === "bearer") {
    const token = bearerToken(request);
    return {
      auth,
      ...(token ? { adminTokenHash: await sha256Hex(token) } : {}),
      ...(adminGrantVersion ? { adminGrantVersion } : {}),
    };
  }
  return { auth, ...(adminGrantVersion ? { adminGrantVersion } : {}) };
}

async function bridgeGrantForRequest(
  request: Request,
  admin: boolean,
  sharedToken?: string,
): Promise<CachedBridgeGrant | undefined> {
  const auth = requestAuthType(request);
  const adminGrant = await adminGrantForRequest(request, admin);
  if (!request.headers.has("x-crabbox-auth")) {
    return adminGrant;
  }
  if (auth !== "github") {
    if (auth === "bearer" && !admin && sharedToken) {
      const token = bearerToken(request);
      if (!token) {
        return undefined;
      }
      return { auth, sharedTokenHash: await sha256Hex(token), ...adminGrant };
    }
    return { auth, ...adminGrant };
  }
  const token = bearerToken(request);
  const login = request.headers.get("x-crabbox-github-login")?.trim();
  const tokenID = request.headers.get("x-crabbox-github-token-id")?.trim();
  const sealedCredential = request.headers.get("x-crabbox-github-sealed-credential")?.trim();
  const expiresAt = request.headers.get("x-crabbox-token-expires-at")?.trim();
  if (
    !token ||
    !login ||
    !tokenID ||
    !sealedCredential ||
    !expiresAt ||
    !Number.isFinite(Date.parse(expiresAt))
  ) {
    return undefined;
  }
  return {
    auth,
    login,
    ...adminGrant,
    portalSessionHash: await sha256Hex(token),
    githubGrant: {
      tokenID,
      sealedCredential,
      expiresAt: new Date(expiresAt).toISOString(),
    },
  };
}

function copyBridgeGrant(principal: CachedBridgeGrant): CachedBridgeGrant {
  return {
    ...(principal.auth ? { auth: principal.auth } : {}),
    ...(principal.login ? { login: principal.login } : {}),
    ...(principal.sharedTokenHash ? { sharedTokenHash: principal.sharedTokenHash } : {}),
    ...(principal.adminTokenHash ? { adminTokenHash: principal.adminTokenHash } : {}),
    ...(principal.adminGrantVersion ? { adminGrantVersion: principal.adminGrantVersion } : {}),
    ...(principal.portalSessionHash ? { portalSessionHash: principal.portalSessionHash } : {}),
    ...(principal.githubGrant ? { githubGrant: principal.githubGrant } : {}),
  };
}

type AdminGrantEnv = Pick<
  Env,
  "CRABBOX_ADMIN_TOKEN" | "CRABBOX_GITHUB_ADMIN_OWNERS" | "CRABBOX_GITHUB_ADMIN_LOGINS"
>;

interface AdminGrantValidation {
  env: AdminGrantEnv;
  adminTokenHash?: string;
  adminGrantVersion?: string;
}

async function adminGrantValidation(
  env: AdminGrantEnv,
  adminGrantVersion?: string,
): Promise<AdminGrantValidation> {
  return {
    env,
    ...(env.CRABBOX_ADMIN_TOKEN
      ? { adminTokenHash: await sha256Hex(env.CRABBOX_ADMIN_TOKEN) }
      : {}),
    ...(adminGrantVersion ? { adminGrantVersion } : {}),
  };
}

function cachedAdminGrantIsCurrent(
  principal: CachedAdminGrant & { admin?: boolean; owner?: string },
  validation: AdminGrantValidation,
): boolean {
  if (principal.admin !== true) {
    return true;
  }
  if (validation.adminGrantVersion) {
    return principal.adminGrantVersion === validation.adminGrantVersion;
  }
  if (
    principal.auth === "github" &&
    typeof principal.owner === "string" &&
    typeof principal.login === "string"
  ) {
    return githubUserIsAdmin({ owner: principal.owner, login: principal.login }, validation.env);
  }
  if (
    principal.auth === "bearer" &&
    /^[a-f0-9]{64}$/.test(principal.adminTokenHash ?? "") &&
    validation.adminTokenHash
  ) {
    return principal.adminTokenHash === validation.adminTokenHash;
  }
  return false;
}

function withCurrentAdminGrant<T extends CachedAdminGrant & { admin?: boolean; owner?: string }>(
  principal: T,
  validation: AdminGrantValidation,
): T {
  return cachedAdminGrantIsCurrent(principal, validation)
    ? principal
    : { ...principal, admin: false };
}

function isolatedCodeRequestOriginAllowed(request: Request): boolean {
  const requestOrigin = new URL(request.url).origin;
  const origin = request.headers.get("origin")?.trim();
  if (origin) {
    try {
      return new URL(origin).origin === requestOrigin;
    } catch {
      return false;
    }
  }
  const method = request.method.toUpperCase();
  return (
    (method === "GET" && request.headers.get("upgrade")?.toLowerCase() !== "websocket") ||
    method === "HEAD"
  );
}

function codeViewerSessionCookie(session: CodeViewerSessionRecord, maxAgeSeconds: number): string {
  return [
    `crabbox_code_session=${encodeURIComponent(session.session)}`,
    `Path=/portal/leases/${encodeURIComponent(session.leaseID)}/code/`,
    "HttpOnly",
    "Secure",
    "SameSite=Lax",
    `Max-Age=${Math.max(0, Math.trunc(maxAgeSeconds))}`,
  ].join("; ");
}

export function bridgeTicketFromRequest(
  request: Request,
  env?: Pick<Env, "CRABBOX_ALLOW_QUERY_BRIDGE_TICKETS">,
): string {
  const upgradeTicket = request.headers.get("x-crabbox-bridge-ticket")?.trim() ?? "";
  if (validBridgeTicket(upgradeTicket)) {
    return upgradeTicket;
  }
  const auth = request.headers.get("authorization")?.trim() ?? "";
  const match = /^Bearer\s+(.+)$/i.exec(auth);
  const bearerTicket = match?.[1]?.trim() ?? "";
  if (validBridgeTicket(bearerTicket)) {
    return bearerTicket;
  }
  if (envFlagEnabled(env?.CRABBOX_ALLOW_QUERY_BRIDGE_TICKETS)) {
    const queryTicket = new URL(request.url).searchParams.get("ticket") ?? "";
    if (validBridgeTicket(queryTicket)) {
      return queryTicket;
    }
  }
  return upgradeTicket || bearerTicket;
}

function validBridgeTicket(value: string): boolean {
  return validWebVNCTicket(value) || validCodeTicket(value) || validEgressTicket(value);
}

function validEgressTicket(value: string | undefined): value is string {
  return typeof value === "string" && /^egress_[a-f0-9]{32}$/.test(value);
}

function validEgressSessionID(value: string | undefined): value is string {
  return typeof value === "string" && /^egress_[A-Za-z0-9_.:-]{6,80}$/.test(value);
}

function validImageRouteID(value: string | undefined): value is string {
  return typeof value === "string" && /^[A-Za-z0-9_./:-]{1,512}$/.test(value);
}

function decodeImageRouteID(value: string): string {
  try {
    return decodeURIComponent(value);
  } catch {
    return "";
  }
}

function validImageName(value: string): boolean {
  return /^[A-Za-z0-9()[\]./_ -]{3,128}$/.test(value);
}

function hasNativeLeaseSource(config: LeaseConfig): boolean {
  return Boolean(
    config.awsSnapshot || config.azureSnapshot || config.gcpMachineImage || config.gcpSnapshot,
  );
}

function isProviderImageNotFound(error: unknown): boolean {
  const message = errorMessage(error);
  return (
    message.includes("InvalidAMIID.NotFound") ||
    message.includes("InvalidSnapshot.NotFound") ||
    message.includes("ResourceNotFound") ||
    message.includes("aws image not found") ||
    message.includes("aws snapshot not found") ||
    message.includes("http 404")
  );
}

function providerResourceNotFound(error: unknown): boolean {
  const message = errorMessage(error);
  return message.includes("http 404") || isCloudNotFoundError(message);
}

function azureProviderScope(
  value: string | undefined,
): { subscription: string; resourceGroup: string } | undefined {
  const match = /^\/subscriptions\/([^/]+)\/resourceGroups\/([^/]+)$/i.exec(value?.trim() ?? "");
  if (!match?.[1] || !match[2]) return undefined;
  return { subscription: match[1], resourceGroup: match[2] };
}

function azureDeferredCleanupLease(
  record: AzureDeferredCleanupRecord,
): Pick<LeaseRecord, "id" | "slug" | "provider" | "cloudID" | "owner" | "providerScope"> {
  const subscription = record.subscription?.trim();
  const resourceGroup = record.resourceGroup?.trim();
  const providerScope = `/subscriptions/${subscription}/resourceGroups/${resourceGroup}`;
  if (
    !subscription ||
    !resourceGroup ||
    !validLeaseID(record.leaseID) ||
    !normalizeLeaseSlug(record.slug) ||
    !record.owner?.trim() ||
    !azureProviderScope(providerScope)
  ) {
    throw new ProviderCleanupManualResolutionError(
      `refusing Azure deferred cleanup for ${record.name}: canonical lease claim and provider scope were not persisted`,
    );
  }
  return {
    id: record.leaseID,
    slug: record.slug,
    provider: "azure",
    cloudID: record.name,
    owner: record.owner,
    providerScope,
  };
}

async function ownedProviderMachineForRelease(
  provider: Extract<Provider, "aws" | "azure" | "gcp">,
  lease: LeaseRecord,
  findServer: (id: string) => Promise<ProviderMachine | undefined>,
  options: {
    labelValue?: (value: string) => string;
  } = {},
): Promise<ProviderMachine | undefined> {
  const machine = await findServer(lease.cloudID);
  if (!machine) return undefined;
  if (!providerMachineOwnedByLease(machine, lease, provider, options.labelValue)) {
    throw new Error(
      `refusing to delete ${provider} resource ${lease.cloudID}: ownership does not match lease ${lease.id}`,
    );
  }
  return machine;
}

function checkpointStrategy(value: string | undefined): "image" | "disk-snapshot" | undefined {
  switch ((value ?? "").trim().toLowerCase()) {
    case "image":
    case "ami":
    case "machine-image":
    case "managed-image":
      return "image";
    case "":
    case "auto":
    case "snapshot":
    case "disk":
    case "disk-snapshot":
    case "disk_snapshot":
      return "disk-snapshot";
    default:
      return undefined;
  }
}

function providerFromQuery(value: string | null): Provider | undefined {
  const provider = (value ?? "").trim().toLowerCase();
  if (!provider) return "aws";
  if (provider === "azure" || provider === "gcp" || provider === "aws") {
    return provider;
  }
  return undefined;
}

function providerRegionForConfig(config: LeaseConfig): string | undefined {
  if (config.provider === "gcp") return config.gcpZone;
  if (config.provider === "azure") return config.azureLocation;
  return config.provider === "aws" ? config.awsRegion : undefined;
}

function providerProjectForConfig(config: LeaseConfig): string | undefined {
  return config.provider === "gcp" ? config.gcpProject : undefined;
}

function providerImageResourceName(provider: Provider, name: string, leaseID: string): string {
  if (provider === "aws") {
    return name;
  }
  const allowed = provider === "gcp" ? /[^a-z0-9-]/g : /[^a-z0-9_.-]/g;
  const normalized = name.trim().toLowerCase().replaceAll(allowed, "-");
  const trimmed =
    provider === "gcp"
      ? normalized
          .replaceAll(/^[^a-z]+/g, "")
          .replaceAll(/-+/g, "-")
          .replaceAll(/-+$/g, "")
      : normalized
          .replaceAll(/^[^a-z]+/g, "")
          .replaceAll(/-+/g, "-")
          .replaceAll(/[-.]+$/g, "");
  const fallback = leaseID.toLowerCase().replaceAll(/[^a-z0-9-]/g, "-");
  const maxLength = provider === "gcp" ? 63 : 80;
  const truncated = (trimmed || `checkpoint-${fallback}`).slice(0, maxLength);
  return provider === "gcp"
    ? truncated.replaceAll(/-+$/g, "")
    : truncated.replaceAll(/[-.]+$/g, "");
}

function unsupportedProviderImageLifecycle(provider: Provider) {
  return () => Promise.reject(new Error(`${provider} images are not supported`));
}

function noStoredImageMetadata(): Promise<ProviderImage | undefined> {
  return Promise.resolve(undefined);
}

function passthroughProviderImage(image: ProviderImage): ProviderImage {
  return image;
}

function allowProviderImageDelete(): Promise<undefined> {
  return Promise.resolve(undefined);
}

function azureDeferredCleanupKey(request: AzureDeferredCleanupRequest): string {
  return `${azureDeferredCleanupPrefix}${encodeURIComponent(request.subscription)}:${encodeURIComponent(request.resourceGroup)}:${encodeURIComponent(request.location)}:${encodeURIComponent(request.name)}`;
}

export function recordAzureDeferredCleanup(
  state: Pick<CoordinatorRuntime, "storage" | "runExclusive">,
  scheduleAlarm: () => Promise<void>,
  request: AzureDeferredCleanupRequest,
): Promise<void> {
  return state.runExclusive(async () => {
    const key = azureDeferredCleanupKey(request);
    const current = await state.storage.get<AzureDeferredCleanupRecord>(key);
    const now = new Date().toISOString();
    const record: AzureDeferredCleanupRecord = {
      ...request,
      attempts: current?.attempts ?? 0,
      updatedAt: now,
      retryAt: now,
    };
    if (current?.lastError) {
      record.lastError = current.lastError;
    }
    await state.storage.put(key, record);
    await scheduleAlarm();
  });
}

function leaseUsesCanonicalProviderKey(
  lease: Pick<LeaseRecord, "id" | "providerKey" | "providerKeyCleanupOwned">,
): boolean {
  return (
    lease.providerKeyCleanupOwned === true &&
    validLeaseID(lease.id) &&
    lease.providerKey === providerKeyForLease(lease.id)
  );
}

function validExternalRunnerID(value: string | undefined): value is string {
  return typeof value === "string" && /^[A-Za-z0-9][A-Za-z0-9_.:-]{2,128}$/.test(value);
}

function retainRecentRun(runs: RunRecord[], run: RunRecord, limit: number): void {
  let low = 0;
  let high = runs.length;
  while (low < high) {
    const middle = Math.floor((low + high) / 2);
    if (runs[middle]!.startedAt.localeCompare(run.startedAt) < 0) {
      high = middle;
    } else {
      low = middle + 1;
    }
  }
  runs.splice(low, 0, run);
  if (runs.length > limit) {
    runs.pop();
  }
}

function terminalRunRetentionMs(value: string | undefined): number {
  const parsed = Number(value ?? "");
  const days =
    Number.isFinite(parsed) && parsed >= 1 ? Math.trunc(parsed) : defaultTerminalRunRetentionDays;
  return Math.min(days, 3650) * 24 * 60 * 60 * 1000;
}

function terminalRunTimestamp(run: RunRecord): number | undefined {
  if (run.state === "running") {
    return undefined;
  }
  for (const value of [run.endedAt, run.lastEventAt, run.startedAt]) {
    const timestamp = Date.parse(value ?? "");
    if (Number.isFinite(timestamp)) {
      return timestamp;
    }
  }
  return undefined;
}

function clampLimit(value: string | null, fallback: number): number {
  const parsed = Number(value ?? "");
  if (!Number.isFinite(parsed) || parsed <= 0) {
    return fallback;
  }
  return Math.min(Math.trunc(parsed), 500);
}

function orgFilterKey(url: URL): string | null | undefined {
  const value = url.searchParams.get("org");
  const kind = url.searchParams.get("orgKind");
  if (kind === "missing") {
    return value === null ? MISSING_ORG_KEY : null;
  }
  if (kind === "legacy") {
    return value !== null && isLegacyOrgKey(value) ? value : null;
  }
  if (kind !== null) return null;
  if (!value) return undefined;
  try {
    return orgKeyForLabel(value);
  } catch {
    return null;
  }
}

function tailString(value: string, maxChars: number): string {
  if (value.length <= maxChars) {
    return value;
  }
  return value.slice(value.length - maxChars);
}

function notFound(): Response {
  return json({ error: "not_found" }, { status: 404 });
}

function adminRouteError(request: Request, method: string, parts: string[]): Response | undefined {
  if (!isAdminRoute(method, parts) || isAdminRequest(request)) {
    return undefined;
  }
  return json({ error: "forbidden", message: "admin token required" }, { status: 403 });
}

function isCloudNotFoundError(message: string): boolean {
  const lower = message.toLowerCase();
  return (
    lower.includes("not found") ||
    lower.includes("invalidinstanceid.notfound") ||
    lower.includes("does not exist")
  );
}

function isAWSTerminalInstanceState(state: string): boolean {
  return state === "shutting-down" || state === "terminated";
}

function isAdminRoute(method: string, parts: string[]): boolean {
  if (method === "GET" && parts.join("/") === "v1/pool") {
    return true;
  }
  if (method === "GET" && parts.join("/") === "v1/admin/leases") {
    return true;
  }
  if (method === "GET" && parts.join("/") === "v1/admin/lease-audit") {
    return true;
  }
  if (method === "GET" && parts.join("/") === "v1/admin/aws-identity") {
    return true;
  }
  if (method === "GET" && parts.join("/") === "v1/admin/providers/identity") {
    return true;
  }
  if (method === "POST" && parts.join("/") === "v1/admin/tailscale-preflight") {
    return true;
  }
  if (parts[0] === "v1" && parts[1] === "admin" && parts[2] === "hosts") {
    return true;
  }
  if (parts[0] === "v1" && parts[1] === "admin" && parts[2] === "mac-hosts") {
    return true;
  }
  if ((method === "GET" || method === "POST") && parts.join("/") === "v1/admin/aws-orphan-sweep") {
    return true;
  }
  if (
    (method === "GET" || method === "POST") &&
    parts.join("/") === "v1/admin/azure-orphan-sweep"
  ) {
    return true;
  }
  if (parts[0] === "v1" && parts[1] === "admin" && parts[2] === "leases" && Boolean(parts[3])) {
    return true;
  }
  if (method === "POST" && parts.join("/") === "v1/images") {
    return true;
  }
  return parts[0] === "v1" && parts[1] === "images" && Boolean(parts[2]);
}

function mergeTailscaleMetadata(
  current: TailscaleMetadata | undefined,
  input: Partial<TailscaleMetadata>,
): TailscaleMetadata {
  const tags = Array.isArray(input.tags)
    ? input.tags.map((tag) => tag.trim().toLowerCase()).filter(Boolean)
    : (current?.tags ?? []);
  const merged: TailscaleMetadata = {
    enabled: input.enabled ?? current?.enabled ?? true,
    tags,
    state:
      input.state === "ready" || input.state === "failed" || input.state === "requested"
        ? input.state
        : (current?.state ?? "requested"),
  };
  const hostname = nonSecretString(input.hostname) || current?.hostname;
  const fqdn = nonSecretString(input.fqdn) || current?.fqdn;
  const ipv4 = nonSecretString(input.ipv4) || current?.ipv4;
  const error = nonSecretString(input.error) || current?.error;
  const version = nonSecretString(input.version) || current?.version;
  const deviceID = nonSecretString(input.deviceID) || current?.deviceID;
  const exitNode = nonSecretString(input.exitNode) || current?.exitNode;
  if (hostname) {
    merged.hostname = hostname;
  }
  if (fqdn) {
    merged.fqdn = fqdn;
  }
  if (ipv4) {
    merged.ipv4 = ipv4;
  }
  if (error) {
    merged.error = error;
  }
  if (version) {
    merged.version = version;
  }
  if (deviceID) {
    merged.deviceID = deviceID;
  }
  if (exitNode) {
    merged.exitNode = exitNode;
    merged.exitNodeAllowLanAccess =
      input.exitNodeAllowLanAccess ?? current?.exitNodeAllowLanAccess ?? false;
  }
  if (merged.state !== "failed") {
    delete merged.error;
  }
  return merged;
}

function nonSecretString(value: unknown): string {
  return typeof value === "string" ? value.trim().slice(0, 256) : "";
}

function sanitizeRegisteredHost(value: unknown): string {
  const host = nonSecretString(value);
  return host.length <= 255 && /^[A-Za-z0-9._:[\]%-]+$/.test(host) ? host : "";
}

function sanitizeRegisteredTarget(value: unknown): TargetOS | undefined {
  return value === "linux" || value === "macos" || value === "windows" ? value : undefined;
}

function sanitizeRegisteredPort(value: unknown): string {
  const raw = nonSecretString(value);
  const port = Number(raw);
  return Number.isInteger(port) && port >= 1 && port <= 65_535 ? String(port) : "";
}

function sanitizeRegisteredPorts(value: unknown): string[] | undefined {
  if (!Array.isArray(value)) {
    return undefined;
  }
  const ports = [...new Set(value.map(sanitizeRegisteredPort).filter(Boolean))].slice(0, 10);
  return ports.length > 0 ? ports : undefined;
}

function sanitizeRunnerProvider(value: unknown): string {
  const provider = nonSecretString(value).toLowerCase();
  return /^[a-z0-9][a-z0-9-]{1,63}$/.test(provider) ? provider : "";
}

function sanitizeExternalRunner(
  input: ExternalRunnerInput,
  provider: string,
  now: Date,
):
  | Omit<ExternalRunnerRecord, "owner" | "org" | "firstSeenAt" | "lastSeenAt" | "updatedAt">
  | undefined {
  const id = nonSecretString(input.id);
  if (!validExternalRunnerID(id)) {
    return undefined;
  }
  const createdAt = sanitizeRunnerTimestamp(input.createdAt, now);
  const runner: Omit<
    ExternalRunnerRecord,
    "owner" | "org" | "firstSeenAt" | "lastSeenAt" | "updatedAt"
  > = {
    id,
    provider,
    status: nonSecretString(input.status).toLowerCase() || "unknown",
  };
  for (const key of [
    "repo",
    "workflow",
    "job",
    "ref",
    "actionsRepo",
    "actionsRunID",
    "actionsRunStatus",
    "actionsRunConclusion",
    "actionsWorkflowName",
  ] as const) {
    const value = nonSecretString(input[key]);
    if (value) {
      runner[key] = value;
    }
  }
  for (const key of ["actionsRunURL", "actionsWorkflowURL"] as const) {
    const value = sanitizeGithubURL(input[key]);
    if (value) {
      runner[key] = value;
    }
  }
  if (createdAt) {
    runner.createdAt = createdAt;
  }
  return runner;
}

function sanitizeGithubURL(value: unknown): string {
  const raw = nonSecretString(value);
  if (!raw) {
    return "";
  }
  try {
    const parsed = new URL(raw);
    if (parsed.protocol !== "https:" || parsed.hostname !== "github.com") {
      return "";
    }
    return parsed.toString();
  } catch {
    return "";
  }
}

function sanitizeRunnerTimestamp(value: string | undefined, now: Date): string | undefined {
  const parsed = Date.parse(value ?? "");
  if (!Number.isFinite(parsed)) {
    return undefined;
  }
  const date = new Date(parsed);
  if (date.getTime() > now.getTime() + 5 * 60 * 1000) {
    return undefined;
  }
  return date.toISOString();
}

function runnerSortTime(runner: ExternalRunnerRecord): string {
  return runner.lastSeenAt || runner.updatedAt || runner.createdAt || runner.firstSeenAt;
}

function webVNCLeaseError(lease: LeaseRecord): string {
  if (lease.state !== "active") {
    return "lease is not active";
  }
  if (!lease.desktop && lease.target !== "macos") {
    return "lease was not created with desktop=true";
  }
  if (!lease.host) {
    return "lease has no reachable host yet";
  }
  return "";
}

function codeLeaseError(lease: LeaseRecord): string {
  if (lease.state !== "active") {
    return "lease is not active";
  }
  if (!lease.code) {
    return "lease was not created with code=true";
  }
  if (lease.target && lease.target !== "linux") {
    return "code is currently available for Linux leases only";
  }
  if (!lease.host) {
    return "lease has no reachable host yet";
  }
  return "";
}

export function codeForwardHeaders(headers: Headers): Record<string, string> {
  const out: Record<string, string> = {};
  const allowed = new Set([
    "accept",
    "accept-language",
    "cache-control",
    "content-type",
    "origin",
    "pragma",
    "sec-websocket-protocol",
    "user-agent",
  ]);
  for (const [key, value] of headers) {
    const lower = key.toLowerCase();
    if (lower.startsWith("x-crabbox-")) {
      continue;
    }
    if (allowed.has(lower) || lower.startsWith("x-")) {
      out[lower] = value;
    } else if (lower === "cookie") {
      const cookie = codeForwardCookie(value);
      if (cookie) {
        out["cookie"] = cookie;
      }
    }
  }
  return out;
}

function codeForwardCookie(value: string): string | undefined {
  const tokens = value
    .split(";")
    .map((part) => part.trim())
    .filter((part) => part.startsWith("vscode-tkn="));
  return tokens.length > 0 ? tokens.join("; ") : undefined;
}

const codePortalContentSecurityPolicy = [
  "default-src 'self'",
  "base-uri 'self'",
  "child-src 'self' blob:",
  "connect-src 'self' ws: wss: https:",
  "font-src 'self' data: blob:",
  "frame-ancestors 'self'",
  "frame-src 'self' https://*.vscode-cdn.net data:",
  "img-src 'self' https: data: blob:",
  "manifest-src 'self'",
  "media-src 'self'",
  "object-src 'none'",
  "script-src 'self' 'unsafe-inline' 'unsafe-eval' blob: https://static.cloudflareinsights.com",
  "style-src 'self' 'unsafe-inline'",
  "worker-src 'self' data: blob:",
].join("; ");

export function codeResponseHeaders(
  values: Record<string, string>,
  cookie: { cookiePath: string; secure: boolean } = { cookiePath: "/", secure: true },
): Headers {
  const headers = new Headers();
  for (const [key, value] of Object.entries(values)) {
    const lower = key.toLowerCase();
    if (lower === "set-cookie") {
      const sanitized = codeResponseCookie(value, cookie);
      if (sanitized) {
        headers.set("set-cookie", sanitized);
      }
      continue;
    }
    if (
      lower === "connection" ||
      lower === "content-security-policy" ||
      lower === "content-encoding" ||
      lower === "content-length" ||
      lower === "service-worker-allowed" ||
      lower === "transfer-encoding" ||
      lower === "upgrade"
    ) {
      continue;
    }
    headers.set(key, value);
  }
  if ((headers.get("content-type") || "").toLowerCase().startsWith("text/html")) {
    headers.set("cache-control", "no-store, no-transform");
  }
  headers.set("content-security-policy", codePortalContentSecurityPolicy);
  return headers;
}

export function codeResponseCookiePath(request: Request, leaseID: string): string {
  const match = /^(\/portal\/leases\/[^/]+\/code)(?:\/|$)/.exec(new URL(request.url).pathname);
  return match?.[1] ? `${match[1]}/` : `/portal/leases/${encodeURIComponent(leaseID)}/code/`;
}

function codeResponseCookie(
  value: string,
  options: { cookiePath: string; secure: boolean },
): string | undefined {
  const pair = value.split(";", 1)[0]?.trim();
  if (!pair || !pair.toLowerCase().startsWith("vscode-tkn=")) {
    return undefined;
  }
  return [
    pair,
    `Path=${options.cookiePath}`,
    "HttpOnly",
    ...(options.secure ? ["Secure"] : []),
    "SameSite=Lax",
  ].join("; ");
}

function bridgeAttachment(value: unknown): BridgeAttachment | undefined {
  const attachment = value as BridgeAttachment | undefined;
  if (!attachment || typeof attachment !== "object") {
    return undefined;
  }
  switch (attachment.kind) {
    case "webvnc-viewer":
      return typeof attachment.leaseID === "string" &&
        validWebVNCSessionID(attachment.id) &&
        validWebVNCSessionID(attachment.agentID) &&
        typeof attachment.owner === "string" &&
        completeBridgePrincipal(attachment) &&
        typeof attachment.label === "string"
        ? attachment
        : undefined;
    case "webvnc-agent":
      return typeof attachment.leaseID === "string" &&
        validWebVNCSessionID(attachment.id) &&
        validOptionalBridgePrincipal(attachment)
        ? {
            ...attachment,
            capabilities:
              attachment.capabilities instanceof Set ? attachment.capabilities : new Set<string>(),
          }
        : undefined;
    case "code-agent":
      return typeof attachment.leaseID === "string" && validOptionalBridgePrincipal(attachment)
        ? attachment
        : undefined;
    case "code-viewer":
      const serializedCodeViewer = attachment as Omit<typeof attachment, "auth"> & {
        auth?: AuthContext["auth"];
      };
      if (
        typeof serializedCodeViewer.leaseID !== "string" ||
        typeof serializedCodeViewer.id !== "string"
      ) {
        return undefined;
      }
      if (serializedCodeViewer.auth === undefined) {
        // Old attachments had no auth binding. Treat them as GitHub sessions so the existing
        // portal-session revocation path closes them before any restored traffic is accepted.
        const restored = { ...serializedCodeViewer, auth: "github" as const };
        return completeBridgePrincipal(restored) ? restored : undefined;
      }
      return (serializedCodeViewer.auth === "bearer" ||
        serializedCodeViewer.auth === "github" ||
        serializedCodeViewer.auth === "proxy") &&
        validOptionalBridgePrincipal(serializedCodeViewer) &&
        (serializedCodeViewer.auth !== "github" ||
          validPortalSessionHash(serializedCodeViewer.portalSessionHash))
        ? (serializedCodeViewer as Extract<BridgeAttachment, { kind: "code-viewer" }>)
        : undefined;
    case "egress-host":
    case "egress-client":
      return typeof attachment.leaseID === "string" &&
        typeof attachment.sessionID === "string" &&
        completeBridgePrincipal(attachment)
        ? attachment
        : undefined;
    case "runtime-adapter-agent":
      return validRuntimeAdapterID(attachment.adapterID) &&
        typeof attachment.owner === "string" &&
        typeof attachment.org === "string" &&
        isCurrentOrgKey(attachment.org) &&
        (attachment.desktopTimeoutMs === undefined ||
          validRuntimeAdapterDesktopRelayTimeout(attachment.desktopTimeoutMs))
        ? attachment
        : undefined;
    case "control":
      return typeof attachment.clientID === "string" &&
        typeof attachment.owner === "string" &&
        typeof attachment.org === "string" &&
        (attachment.admin === true || isCurrentOrgKey(attachment.org))
        ? {
            ...attachment,
            subscriptions:
              attachment.subscriptions && typeof attachment.subscriptions === "object"
                ? attachment.subscriptions
                : {},
          }
        : undefined;
    default:
      return undefined;
  }
}

function validOptionalBridgePrincipal(value: {
  owner?: unknown;
  org?: unknown;
  admin?: unknown;
}): boolean {
  const absent = value.org === undefined && value.admin === undefined;
  const complete =
    typeof value.owner === "string" &&
    typeof value.org === "string" &&
    typeof value.admin === "boolean" &&
    (value.admin || isCurrentOrgKey(value.org));
  return absent || complete;
}

function completeBridgePrincipal(value: {
  owner?: unknown;
  org?: unknown;
  admin?: unknown;
}): value is { owner: string; org: string; admin: boolean } {
  return (
    typeof value.owner === "string" &&
    typeof value.org === "string" &&
    typeof value.admin === "boolean" &&
    (value.admin || isCurrentOrgKey(value.org))
  );
}

function revocableUserBridge(attachment: BridgeAttachment): attachment is Extract<
  BridgeAttachment,
  {
    kind:
      | "webvnc-agent"
      | "webvnc-viewer"
      | "code-agent"
      | "code-viewer"
      | "egress-host"
      | "egress-client";
  }
> {
  return (
    ((attachment.kind === "webvnc-agent" || attachment.kind === "code-agent") &&
      completeBridgePrincipal(attachment)) ||
    attachment.kind === "webvnc-viewer" ||
    attachment.kind === "code-viewer" ||
    attachment.kind === "egress-host" ||
    attachment.kind === "egress-client"
  );
}

function leaseBridgeTicketPrincipal(
  ticket: CachedBridgeGrant & { owner: string; org: string; admin?: boolean },
): CachedBridgeGrant & {
  owner: string;
  org: string;
  admin: boolean;
} {
  return {
    owner: ticket.owner,
    org: ticket.org,
    admin: ticket.admin === true,
    ...(ticket.auth ? { auth: ticket.auth } : {}),
    ...(ticket.login ? { login: ticket.login } : {}),
    ...(ticket.sharedTokenHash ? { sharedTokenHash: ticket.sharedTokenHash } : {}),
    ...(ticket.adminTokenHash ? { adminTokenHash: ticket.adminTokenHash } : {}),
    ...(ticket.adminGrantVersion ? { adminGrantVersion: ticket.adminGrantVersion } : {}),
    ...(ticket.portalSessionHash ? { portalSessionHash: ticket.portalSessionHash } : {}),
    ...(ticket.githubGrant ? { githubGrant: ticket.githubGrant } : {}),
  };
}

function bridgeTags(attachment: BridgeAttachment): string[] {
  if (attachment.kind === "control") {
    return [`control:${attachment.clientID}`, `owner:${attachment.owner}`, `org:${attachment.org}`];
  }
  if (attachment.kind === "egress-host" || attachment.kind === "egress-client") {
    return [`lease:${attachment.leaseID}`, `session:${attachment.sessionID}`, attachment.kind];
  }
  if (attachment.kind === "webvnc-agent" || attachment.kind === "webvnc-viewer") {
    return [`lease:${attachment.leaseID}`, `webvnc:${attachment.id}`, attachment.kind];
  }
  if (attachment.kind === "runtime-adapter-agent") {
    return [`adapter:${attachment.adapterID}`, attachment.kind];
  }
  return [`lease:${attachment.leaseID}`, attachment.kind];
}

function restoredBridgeEndpoint(attachment: BridgeAttachment): string {
  switch (attachment.kind) {
    case "webvnc-agent":
      return JSON.stringify([attachment.kind, attachment.leaseID, attachment.id]);
    case "webvnc-viewer":
      return JSON.stringify([attachment.kind, attachment.leaseID, attachment.id]);
    case "code-agent":
      return JSON.stringify([attachment.kind, attachment.leaseID]);
    case "code-viewer":
      return JSON.stringify([attachment.kind, attachment.leaseID, attachment.id]);
    case "egress-host":
    case "egress-client":
      return JSON.stringify([attachment.kind, attachment.leaseID, attachment.sessionID]);
    case "runtime-adapter-agent":
      return JSON.stringify([attachment.kind, attachment.adapterID]);
    case "control":
      return JSON.stringify([attachment.kind, attachment.clientID]);
  }
}

function sendControl(socket: WebSocket, payload: unknown): void {
  try {
    socket.send(JSON.stringify(payload));
  } catch {
    closeSocket(socket, 1011, "control send failed");
  }
}

function bytesToBase64(bytes: Uint8Array): string {
  let binary = "";
  for (const byte of bytes) {
    binary += String.fromCharCode(byte);
  }
  return btoa(binary);
}

function base64ToBytes(value: string): Uint8Array {
  const binary = atob(value);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) {
    bytes[i] = binary.charCodeAt(i);
  }
  return bytes;
}

function identifierMatchesLease(identifier: string, lease: LeaseRecord): boolean {
  return (
    identifier === lease.id || normalizeLeaseSlug(identifier) === normalizeLeaseSlug(lease.slug)
  );
}

function leaseHostID(lease: LeaseRecord): string {
  return lease.hostId || lease.hostID || "";
}

export interface WebVNCBuffer {
  chunks: Array<string | ArrayBuffer>;
  bytes: number;
}

export async function forwardOrBufferWebVNC(
  rawData: unknown,
  socket: WebSocket | undefined,
  buffers: Map<string, WebVNCBuffer>,
  leaseID: string,
): Promise<void> {
  const data = await normalizeWebVNCData(rawData);
  if (socket && socket.readyState === WebSocket.OPEN) {
    socket.send(data);
    return;
  }
  const bytes = webVNCDataBytes(data);
  const buffer = buffers.get(leaseID) ?? { chunks: [], bytes: 0 };
  if (buffer.bytes + bytes > maxPendingWebVNCBytes) {
    buffers.delete(leaseID);
    return;
  }
  buffer.chunks.push(data);
  buffer.bytes += bytes;
  buffers.set(leaseID, buffer);
}

export function flushPendingWebVNC(
  buffers: Map<string, WebVNCBuffer>,
  leaseID: string,
  socket: WebSocket,
): void {
  const buffer = buffers.get(leaseID);
  buffers.delete(leaseID);
  if (!buffer || socket.readyState !== WebSocket.OPEN) {
    return;
  }
  for (const chunk of buffer.chunks) {
    socket.send(chunk);
  }
}

export function resetWebVNCBridge(
  agents: Map<string, WebSocket> | Map<string, Map<string, WebSocket>>,
  buffers: Map<string, WebVNCBuffer>,
  leaseID: string,
  code: number,
  reason: string,
): void {
  const entry = agents.get(leaseID);
  if (entry instanceof Map) {
    for (const socket of entry.values()) {
      closeSocket(socket, code, reason);
    }
  } else {
    closeSocket(entry, code, reason);
  }
  agents.delete(leaseID);
  buffers.delete(leaseID);
  for (const key of buffers.keys()) {
    if (key.startsWith(`${leaseID}:`)) {
      buffers.delete(key);
    }
  }
}

async function forwardWebVNC(rawData: unknown, socket: WebSocket | undefined): Promise<void> {
  if (!socket || socket.readyState !== WebSocket.OPEN) {
    return;
  }
  const data = await normalizeWebVNCData(rawData);
  socket.send(data);
}

async function forwardEgress(rawData: unknown, socket: WebSocket | undefined): Promise<void> {
  if (!socket || socket.readyState !== WebSocket.OPEN) {
    return;
  }
  const data = await normalizeWebVNCData(rawData);
  socket.send(data);
}

async function normalizeWebVNCData(data: unknown): Promise<string | ArrayBuffer> {
  if (typeof data === "string" || data instanceof ArrayBuffer) {
    return data;
  }
  if (data instanceof Blob) {
    return await data.arrayBuffer();
  }
  return String(data);
}

function runtimeAdapterRelayMessageBytes(data: unknown): number | undefined {
  if (typeof data === "string") {
    return data.length > runtimeAdapterRelayFrameLimit
      ? data.length
      : textEncoder.encode(data).byteLength;
  }
  if (data instanceof ArrayBuffer) {
    return data.byteLength;
  }
  if (data instanceof Blob) {
    return data.size;
  }
  return undefined;
}

function webVNCDataBytes(data: string | ArrayBuffer): number {
  return typeof data === "string" ? textEncoder.encode(data).byteLength : data.byteLength;
}

function closeSocket(socket: WebSocket | undefined, code: number, reason: string): void {
  if (
    !socket ||
    socket.readyState === WebSocket.CLOSED ||
    socket.readyState === WebSocket.CLOSING
  ) {
    return;
  }
  try {
    socket.close(code, boundedSocketReason(reason));
  } catch {
    try {
      socket.close(1011, "socket close failed");
    } catch {
      // The socket implementation rejected both close attempts.
    }
  }
}

function workspaceSSHHostKeysFromRequest(
  request: Request,
): { privateKey: string; publicKey: string } | undefined {
  try {
    const privateKey = atob(request.headers.get(workspaceSSHHostPrivateKeyHeader) ?? "");
    const publicKey = atob(request.headers.get(workspaceSSHHostPublicKeyHeader) ?? "");
    return privateKey && publicKey ? { privateKey, publicKey } : undefined;
  } catch {
    return undefined;
  }
}

function generateSSHHostKeyPair(comment: string): { private: string; public: string } {
  return sshUtils.generateKeyPairSync("ed25519", { comment });
}

function withSSHHostKey(config: LeaseConfig, comment: string): LeaseConfig {
  if (config.sshHostPrivateKey || config.sshHostPublicKey) {
    if (!config.sshHostPrivateKey || !config.sshHostPublicKey) {
      throw new Error("SSH host key material must include both private and public halves");
    }
    return config;
  }
  const hostKeys = generateSSHHostKeyPair(comment);
  return {
    ...config,
    sshHostPrivateKey: hostKeys.private,
    sshHostPublicKey: hostKeys.public,
  };
}

async function workspaceSSHHostKeyFingerprint(publicKey: string): Promise<string> {
  const [type, encoded] = publicKey.trim().split(/\s+/u, 3);
  if (type !== "ssh-ed25519" || !encoded) {
    throw new Error("workspace SSH host public key is invalid");
  }
  const raw = Uint8Array.from(atob(encoded), (character) => character.charCodeAt(0));
  const digest = await crypto.subtle.digest("SHA-256", raw);
  return [...new Uint8Array(digest)].map((byte) => byte.toString(16).padStart(2, "0")).join("");
}

function requestSourceCIDRs(request: Request): string[] {
  const sourceIP = request.headers.get("cf-connecting-ip") ?? "";
  if (!sourceIP) {
    return [];
  }
  const cidr = sourceIP.includes(":") ? `${sourceIP}/128` : `${sourceIP}/32`;
  return validCIDRs([cidr]);
}

function providerAccessContext(
  sourceCIDRs: string[],
  activeLeases: LeaseRecord[],
): ProviderAccessContext {
  return {
    requestSourceCIDRs: sourceCIDRs,
    activeLeases,
  };
}

function providerAccessReservation(lease: LeaseRecord, now: Date): LeaseRecord {
  const leaseExpiry = Date.parse(lease.expiresAt);
  const reservationExpiry = now.getTime() + providerAccessReservationTTLMS;
  return {
    ...lease,
    expiresAt: new Date(Math.min(leaseExpiry, reservationExpiry)).toISOString(),
  };
}

function replaceProviderAccessState(leases: LeaseRecord[], lease: LeaseRecord): LeaseRecord[] {
  let replaced = false;
  const next = leases.map((candidate) => {
    if (candidate.id !== lease.id) {
      return candidate;
    }
    replaced = true;
    return lease;
  });
  if (!replaced) {
    next.push(lease);
  }
  return next;
}

function finiteNumber(value: number | undefined): number | undefined {
  return Number.isFinite(value) ? value : undefined;
}

function finiteQueryNumber(value: string | null): number | undefined {
  const parsed = Number(value ?? "");
  return Number.isFinite(parsed) && parsed >= 0 ? Math.trunc(parsed) : undefined;
}

function finiteControlNumber(value: number | undefined): number | undefined {
  return Number.isFinite(value) && value !== undefined && value >= 0
    ? Math.trunc(value)
    : undefined;
}

function boundedEgressString(value: string | undefined): string | undefined {
  const normalized = String(value ?? "").trim();
  if (!normalized) {
    return undefined;
  }
  return normalized.slice(0, 80);
}

function boundedEgressAllowlist(values: string[] | undefined): string[] {
  if (!Array.isArray(values)) {
    return [];
  }
  const out: string[] = [];
  for (const value of values) {
    const normalized = String(value ?? "")
      .trim()
      .toLowerCase();
    if (!normalized || normalized.length > 253 || out.includes(normalized)) {
      continue;
    }
    out.push(normalized);
    if (out.length >= 100) {
      break;
    }
  }
  return out;
}

function normalizeRunLogInput(input: RunFinishRequest): {
  log: string;
  bytes: number;
  truncated: boolean;
} {
  const chunkLog = Array.isArray(input.logChunks)
    ? input.logChunks.map((chunk) => String(chunk)).join("")
    : "";
  const rawLog = chunkLog || input.log || "";
  const bounded = truncateUtf8Tail(rawLog, maxStoredRunLogBytes);
  const rawBytes = textEncoder.encode(rawLog).byteLength;
  return {
    log: bounded,
    bytes: Math.min(rawBytes, maxStoredRunLogBytes),
    truncated: Boolean(input.logTruncated) || rawBytes > maxStoredRunLogBytes,
  };
}

function splitRunLogByBytes(log: string, maxBytes: number): string[] {
  const chunks: string[] = [];
  let current = "";
  let currentBytes = 0;
  for (const char of log) {
    const charBytes = textEncoder.encode(char).byteLength;
    if (current && currentBytes + charBytes > maxBytes) {
      chunks.push(current);
      current = "";
      currentBytes = 0;
    }
    current += char;
    currentBytes += charBytes;
  }
  if (current) {
    chunks.push(current);
  }
  return chunks;
}

function truncateUtf8Tail(value: string, maxBytes: number): string {
  const encoded = textEncoder.encode(value);
  if (encoded.byteLength <= maxBytes) {
    return value;
  }
  return textDecoder.decode(encoded.slice(encoded.byteLength - maxBytes));
}

const MAX_RESULT_FILES = 50;
const MAX_RESULT_FAILURES = 100;
const MAX_RESULT_STRING_BYTES = 4096;
const MAX_EVENT_STRING_BYTES = 16 * 1024;

function boundedRunEvent(
  runID: string,
  seq: number,
  createdAt: string,
  input: RunEventRequest,
): RunEventRecord {
  const type = input.type && input.type.trim() ? input.type.trim() : "event";
  const event: RunEventRecord = {
    runID,
    seq,
    type: truncateString(type, 128),
    createdAt,
  };
  if (input.phase) {
    event.phase = truncateString(input.phase, 128);
  }
  if (input.stream === "stdout" || input.stream === "stderr") {
    event.stream = input.stream;
  }
  if (input.message) {
    event.message = truncateString(input.message, MAX_EVENT_STRING_BYTES);
  }
  if (input.data) {
    event.data = truncateString(input.data, MAX_EVENT_STRING_BYTES);
  }
  if (input.leaseID && validLeaseID(input.leaseID)) {
    event.leaseID = input.leaseID;
  }
  if (input.slug) {
    event.slug = truncateString(input.slug, 128);
  }
  if (
    input.provider === "aws" ||
    input.provider === "hetzner" ||
    input.provider === "azure" ||
    input.provider === "gcp" ||
    input.provider === "daytona"
  ) {
    event.provider = input.provider;
  }
  if (input.target === "linux" || input.target === "macos" || input.target === "windows") {
    event.target = input.target;
  }
  if (input.windowsMode === "normal" || input.windowsMode === "wsl2") {
    event.windowsMode = input.windowsMode;
  }
  if (input.class) {
    event.class = truncateString(input.class, 128);
  }
  if (input.serverType) {
    event.serverType = truncateString(input.serverType, 128);
  }
  const exitCode = input.exitCode;
  if (typeof exitCode === "number" && Number.isFinite(exitCode)) {
    event.exitCode = exitCode;
  }
  return event;
}

function applyRunEventSummary(run: RunRecord, event: RunEventRecord): void {
  if (event.phase) {
    run.phase = event.phase;
  } else {
    const phase = phaseForRunEvent(event);
    if (phase) {
      run.phase = phase;
    }
  }
  if (event.leaseID) {
    run.leaseID = event.leaseID;
  }
  if (event.slug) {
    run.slug = event.slug;
  }
  if (event.provider) {
    run.provider = event.provider;
  }
  if (event.target) {
    run.target = event.target;
  }
  if (event.windowsMode) {
    run.windowsMode = event.windowsMode;
  }
  if (event.class) {
    run.class = event.class;
  }
  if (event.serverType) {
    run.serverType = event.serverType;
  }
  if (event.type === "run.failed") {
    run.state = "failed";
    run.phase = "failed";
    run.endedAt = event.createdAt;
  }
}

function sanitizeRunLabel(value: unknown): string | undefined {
  if (typeof value !== "string") {
    return undefined;
  }
  const label = value.trim().replace(/\s+/g, " ");
  return label ? label.slice(0, 120) : undefined;
}

function sanitizeRunClassification(value: unknown): string | undefined {
  if (typeof value !== "string") {
    return undefined;
  }
  const text = value.trim();
  return /^[a-z0-9][a-z0-9_-]{0,63}$/.test(text) ? text : undefined;
}

function phaseForRunEvent(event: RunEventRecord): string {
  switch (event.type) {
    case "leasing.started":
      return "leasing";
    case "lease.created":
      return "leased";
    case "bootstrap.waiting":
      return "bootstrap";
    case "sync.started":
      return "sync";
    case "sync.finished":
      return "synced";
    case "command.started":
    case "stdout":
    case "stderr":
      return "command";
    case "lease.released":
      return "released";
    default:
      return "";
  }
}

function boundedTestResults(results: TestResultSummary): TestResultSummary {
  const files = Array.isArray(results.files) ? results.files : [];
  const failed = Array.isArray(results.failed) ? results.failed : [];
  return {
    ...results,
    files: files
      .slice(0, MAX_RESULT_FILES)
      .map((file) => truncateString(file, MAX_RESULT_STRING_BYTES)),
    failed: failed.slice(0, MAX_RESULT_FAILURES).map(boundedTestFailure),
  };
}

function boundedTestFailure(failure: TestFailure): TestFailure {
  const out: TestFailure = {
    suite: truncateString(failure.suite, MAX_RESULT_STRING_BYTES),
    name: truncateString(failure.name, MAX_RESULT_STRING_BYTES),
    kind: failure.kind,
  };
  if (failure.classname) {
    out.classname = truncateString(failure.classname, MAX_RESULT_STRING_BYTES);
  }
  if (failure.file) {
    out.file = truncateString(failure.file, MAX_RESULT_STRING_BYTES);
  }
  if (failure.message) {
    out.message = truncateString(failure.message, MAX_RESULT_STRING_BYTES);
  }
  if (failure.type) {
    out.type = truncateString(failure.type, MAX_RESULT_STRING_BYTES);
  }
  return out;
}

function truncateString(value: string, maxBytes: number): string {
  const encoder = new TextEncoder();
  const bytes = encoder.encode(value);
  if (bytes.byteLength <= maxBytes) {
    return value;
  }
  const decoder = new TextDecoder();
  let out = decoder.decode(bytes.slice(0, maxBytes));
  while (encoder.encode(out).byteLength > maxBytes) {
    out = out.slice(0, -1);
  }
  return out;
}

function leaseTTLSeconds(lease: LeaseRecord): number {
  if (Number.isFinite(lease.ttlSeconds) && lease.ttlSeconds > 0) {
    return lease.ttlSeconds;
  }
  const createdAt = Date.parse(lease.createdAt);
  const expiresAt = Date.parse(lease.expiresAt);
  if (Number.isFinite(createdAt) && Number.isFinite(expiresAt) && expiresAt > createdAt) {
    return Math.min(Math.trunc((expiresAt - createdAt) / 1000), 86_400);
  }
  return 5_400;
}

function leaseIdleTimeoutSeconds(lease: LeaseRecord): number {
  if (
    Number.isFinite(lease.idleTimeoutSeconds) &&
    lease.idleTimeoutSeconds &&
    lease.idleTimeoutSeconds > 0
  ) {
    return lease.idleTimeoutSeconds;
  }
  return leaseTTLSeconds(lease);
}

function recomputeLeaseExpiresAt(lease: LeaseRecord, fallbackNow: Date): Date {
  if (isRegisteredLease(lease)) {
    const touchedAt = parseLeaseDate(lease.lastTouchedAt, fallbackNow);
    return registeredLeaseExpiresAt(
      touchedAt,
      leaseTTLSeconds(lease),
      leaseIdleTimeoutSeconds(lease),
    );
  }
  const createdAt = parseLeaseDate(lease.createdAt, fallbackNow);
  const touchedAt = parseLeaseDate(lease.lastTouchedAt, createdAt);
  return leaseExpiresAt(
    createdAt,
    touchedAt,
    leaseTTLSeconds(lease),
    leaseIdleTimeoutSeconds(lease),
  );
}

function registeredLeaseExpiresAt(
  touchedAt: Date,
  ttlSeconds: number,
  idleTimeoutSeconds: number,
): Date {
  return new Date(
    touchedAt.getTime() + Math.min(Math.max(1, ttlSeconds), Math.max(1, idleTimeoutSeconds)) * 1000,
  );
}

function leaseExpiresAt(
  createdAt: Date,
  lastTouchedAt: Date,
  ttlSeconds: number,
  idleTimeoutSeconds: number,
): Date {
  const maxLifetime = createdAt.getTime() + Math.max(1, ttlSeconds) * 1000;
  const idleExpiry = lastTouchedAt.getTime() + Math.max(1, idleTimeoutSeconds) * 1000;
  return new Date(Math.min(maxLifetime, idleExpiry));
}

function parseLeaseDate(value: string | undefined, fallback: Date): Date {
  const parsed = Date.parse(value ?? "");
  return Number.isFinite(parsed) ? new Date(parsed) : fallback;
}

function clampLeaseSeconds(value: number | undefined, max: number): number {
  if (!Number.isFinite(value) || value === undefined || value <= 0) {
    return max;
  }
  return Math.min(Math.trunc(value), max);
}

function sanitizeLeaseTelemetry(
  input: Partial<LeaseTelemetry> | undefined,
  now: Date,
): LeaseTelemetry | undefined {
  if (!input || typeof input !== "object") {
    return undefined;
  }
  const telemetry: LeaseTelemetry = {
    capturedAt: sanitizeTelemetryTimestamp(input.capturedAt, now),
  };
  const source = typeof input.source === "string" ? input.source.trim() : "";
  if (source) {
    telemetry.source = source.slice(0, 32);
  }
  let hasMetric = false;
  const cpuCount = sanitizeTelemetryNumber(input.cpuCount, 1_000_000);
  if (cpuCount !== undefined && cpuCount >= 1) {
    telemetry.cpuCount = Math.trunc(cpuCount);
    hasMetric = true;
  }
  for (const [key, max] of [
    ["load1", 10_000],
    ["load5", 10_000],
    ["load15", 10_000],
    ["memoryPercent", 100],
    ["diskPercent", 100],
  ] as const) {
    const value = sanitizeTelemetryNumber(input[key], max);
    if (value !== undefined) {
      telemetry[key] = value;
      hasMetric = true;
    }
  }
  for (const key of [
    "memoryUsedBytes",
    "memoryTotalBytes",
    "diskUsedBytes",
    "diskTotalBytes",
    "uptimeSeconds",
  ] as const) {
    const value = sanitizeTelemetryNumber(input[key], Number.MAX_SAFE_INTEGER);
    if (value !== undefined) {
      telemetry[key] = Math.trunc(value);
      hasMetric = true;
    }
  }
  return hasMetric ? telemetry : undefined;
}

function sanitizeRunTelemetry(
  input: RunTelemetrySummary | undefined,
  now: Date,
): RunTelemetrySummary | undefined {
  if (!input || typeof input !== "object") {
    return undefined;
  }
  const start = sanitizeLeaseTelemetry(input.start, now);
  const end = sanitizeLeaseTelemetry(input.end, now);
  const samples = Array.isArray(input.samples)
    ? input.samples
        .map((sample) => sanitizeLeaseTelemetry(sample, now))
        .filter((sample): sample is LeaseTelemetry => sample !== undefined)
    : [];
  if (!start && !end && samples.length === 0) {
    return undefined;
  }
  const telemetry: RunTelemetrySummary = {};
  if (start) {
    telemetry.start = start;
  }
  if (end) {
    telemetry.end = end;
  }
  if (samples.length > 0) {
    telemetry.samples = boundedTelemetrySamples(samples, maxRunTelemetrySamples);
  }
  return telemetry;
}

function mergeRunTelemetry(
  existing: RunTelemetrySummary | undefined,
  incoming: RunTelemetrySummary,
): RunTelemetrySummary {
  const telemetry: RunTelemetrySummary = {
    ...existing,
    ...incoming,
  };
  telemetry.samples = boundedTelemetrySamples(
    [
      ...((existing?.samples ?? []).filter(Boolean) as LeaseTelemetry[]),
      ...((incoming.samples ?? []).filter(Boolean) as LeaseTelemetry[]),
    ],
    maxRunTelemetrySamples,
  );
  if (telemetry.samples.length === 0) {
    delete telemetry.samples;
  }
  return telemetry;
}

function appendRunTelemetrySample(
  telemetry: RunTelemetrySummary | undefined,
  sample: LeaseTelemetry,
): RunTelemetrySummary {
  const next: RunTelemetrySummary = { ...telemetry };
  next.samples = boundedTelemetrySamples([...(next.samples ?? []), sample], maxRunTelemetrySamples);
  if (!next.start) {
    next.start = sample;
  }
  return next;
}

function appendLeaseTelemetryHistory(
  history: LeaseTelemetry[] | undefined,
  telemetry: LeaseTelemetry,
): LeaseTelemetry[] {
  return boundedTelemetrySamples(
    [...(Array.isArray(history) ? history : []), telemetry],
    maxLeaseTelemetryHistory,
  );
}

function boundedTelemetrySamples(samples: LeaseTelemetry[], max: number): LeaseTelemetry[] {
  const byTime = new Map<string, LeaseTelemetry>();
  for (const sample of samples) {
    if (sample?.capturedAt) {
      byTime.set(sample.capturedAt, sample);
    }
  }
  return [...byTime.values()]
    .toSorted((left, right) => left.capturedAt.localeCompare(right.capturedAt))
    .slice(-max);
}

function sanitizeTelemetryTimestamp(value: string | undefined, now: Date): string {
  const parsed = Date.parse(value ?? "");
  if (!Number.isFinite(parsed)) {
    return now.toISOString();
  }
  return new Date(parsed).toISOString();
}

function sanitizeTelemetryNumber(value: unknown, max: number): number | undefined {
  if (typeof value !== "number" || !Number.isFinite(value) || value < 0) {
    return undefined;
  }
  return Math.min(value, max);
}

function allocateLeaseSlug(
  requested: string,
  leaseID: string,
  owner: string,
  org: string,
  leases: LeaseRecord[],
): string {
  let slug = normalizeLeaseSlug(requested) || leaseSlugFromID(leaseID);
  for (let attempt = 0; attempt < 20; attempt += 1) {
    if (!activeSlugCollision(slug, owner, org, leases)) {
      return slug;
    }
    slug = slugWithCollisionSuffix(requested, `${leaseID}-${attempt}`);
  }
  throw new Error(`could not allocate slug for ${leaseID}`);
}

function activeSlugCollision(
  slug: string,
  owner: string,
  org: string,
  leases: LeaseRecord[],
): boolean {
  const now = Date.now();
  return leases.some(
    (lease) =>
      leaseIsLive(lease) &&
      Date.parse(lease.expiresAt) > now &&
      lease.owner === owner &&
      lease.org === org &&
      normalizeLeaseSlug(lease.slug) === slug,
  );
}

function leaseIsLive(lease: LeaseRecord): boolean {
  return lease.state === "active" || lease.state === "provisioning";
}

function leaseHeartbeatStateError(
  lease: LeaseRecord,
  now = Date.now(),
): "lease_ended" | "lease_expired" | undefined {
  if (!leaseIsLive(lease)) {
    return "lease_ended";
  }
  const expiresAt = Date.parse(lease.expiresAt);
  if (!Number.isFinite(expiresAt) || expiresAt <= now) {
    return "lease_expired";
  }
  return undefined;
}

function isRegisteredLease(lease: LeaseRecord): boolean {
  return lease.lifecycle === "registered";
}

function managedLeaseProvider(lease: LeaseRecord): Provider | undefined {
  // A registered record never grants provider authority, even when its name is aws/azure/etc.
  return !isRegisteredLease(lease) && isCoordinatorProvider(lease.provider)
    ? lease.provider
    : undefined;
}

function leaseNeedsCleanup(lease: LeaseRecord, now: number): boolean {
  if (leaseIsLive(lease) && Date.parse(lease.expiresAt) <= now) {
    return true;
  }
  if (lease.state === "released" && lease.releaseDeletesServer === false) {
    return false;
  }
  return Boolean(
    !leaseIsLive(lease) &&
    ((lease.cloudID && (lease.cleanupError || lease.cleanupStartedAt)) ||
      lease.providerKeyCleanupPending),
  );
}

function applyLeaseRecordChanges(
  latest: LeaseRecord,
  baseline: LeaseRecord,
  updated: LeaseRecord,
): LeaseRecord {
  const merged = structuredClone(latest) as unknown as Record<string, unknown>;
  const before = baseline as unknown as Record<string, unknown>;
  const after = updated as unknown as Record<string, unknown>;
  for (const key of new Set([...Object.keys(before), ...Object.keys(after)])) {
    const beforeHasKey = Object.hasOwn(before, key);
    const afterHasKey = Object.hasOwn(after, key);
    if (
      beforeHasKey === afterHasKey &&
      JSON.stringify(before[key]) === JSON.stringify(after[key])
    ) {
      continue;
    }
    if (afterHasKey) {
      merged[key] = structuredClone(after[key]);
    } else {
      delete merged[key];
    }
  }
  return merged as unknown as LeaseRecord;
}

function withRequestedTailscaleMetadata(lease: LeaseRecord, config: LeaseConfig): LeaseRecord {
  if (!config.tailscale) return lease;
  const tailscale: TailscaleMetadata = {
    enabled: true,
    hostname: config.tailscaleHostname,
    tags: config.tailscaleTags,
    state: "requested",
  };
  if (config.tailscaleExitNode) {
    tailscale.exitNode = config.tailscaleExitNode;
    tailscale.exitNodeAllowLanAccess = config.tailscaleExitNodeAllowLanAccess;
  }
  return { ...lease, tailscale };
}

function provisionedLeaseRecord(
  lease: LeaseRecord,
  config: LeaseConfig,
  server: ProviderMachine,
  serverType: string,
): LeaseRecord {
  const providerProject = lease.providerProject ?? providerProjectForConfig(config);
  const providerKey = server.providerKey?.trim() || config.providerKey;
  const providerKeyCleanupOwned =
    (config.provider === "aws" || config.provider === "hetzner") &&
    providerKey === providerKeyForLease(lease.id);
  return {
    ...lease,
    state: "active",
    cloudID: server.cloudID,
    serverID: server.id,
    serverName: server.name,
    serverType,
    providerKey,
    providerKeyCleanupOwned,
    host: server.host,
    region: server.region ?? lease.region ?? providerRegionForConfig(config) ?? "",
    ...(providerProject ? { providerProject } : {}),
    ...(server.hostID ? { hostId: server.hostID } : {}),
  };
}

function retainProvisioningCleanupClaim(
  lease: LeaseRecord,
  claim: ProviderProvisioningCleanupClaim,
  error: string,
  failedAt: string,
): void {
  const retainResource = lease.state === "released" && lease.releaseDeletesServer === false;
  lease.cloudID = claim.cloudID;
  lease.serverName = claim.cloudID;
  lease.serverID = claim.serverID ?? 0;
  lease.updatedAt = failedAt;
  if (claim.region) lease.region = claim.region;
  if (claim.providerProject) lease.providerProject = claim.providerProject;
  if (claim.providerScope) lease.providerScope = claim.providerScope;
  if (retainResource) {
    lease.keep = true;
    clearLeaseCleanupMetadata(lease);
    delete lease.provisioningResourceMayExist;
    delete lease.provisioningFailureRetryable;
    return;
  }
  lease.releaseDeletesServer = true;
  lease.provisioningResourceMayExist = true;
  lease.provisioningFailureRetryable = false;
  lease.cleanupError = error;
  lease.cleanupFailedAt = failedAt;
  lease.cleanupRetryAt = new Date(Date.parse(failedAt) + leaseCleanupRetryDelayMs).toISOString();
}

function nextLeaseAlarmTime(lease: LeaseRecord): number {
  const now = Date.now();
  const expiresAt = Date.parse(lease.expiresAt);
  const runtimeAdapterDeleteRetryAt = Date.parse(lease.runtimeAdapterDeleteRetryAt ?? "");
  const runtimeAdapterDeleteDispatchUntil = Date.parse(
    lease.runtimeAdapterDeleteDispatchUntil ?? "",
  );
  if (lease.runtimeAdapterDeleteRequestedAt) {
    let deleteAlarm = Number.isFinite(runtimeAdapterDeleteRetryAt)
      ? Math.max(now + 1000, runtimeAdapterDeleteRetryAt)
      : now + 1000;
    if (
      Number.isFinite(runtimeAdapterDeleteDispatchUntil) &&
      runtimeAdapterDeleteDispatchUntil > now
    ) {
      deleteAlarm = Math.max(deleteAlarm, runtimeAdapterDeleteDispatchUntil);
    }
    return leaseIsLive(lease) && Number.isFinite(expiresAt)
      ? Math.min(expiresAt, deleteAlarm)
      : deleteAlarm;
  }
  if (lease.providerKeyCleanupPending && !lease.cleanupStartedAt) {
    const retryAt = Date.parse(lease.cleanupRetryAt ?? "");
    return Number.isFinite(retryAt) && retryAt > now ? retryAt : now + 1;
  }
  const claimDeadline = cleanupClaimDeadline(lease);
  if (lease.cleanupStartedAt && Number.isFinite(claimDeadline)) {
    return claimDeadline;
  }
  const cleanupRetryAt = Date.parse(lease.cleanupRetryAt ?? "");
  if (Number.isFinite(cleanupRetryAt) && cleanupRetryAt <= now) {
    return now + 1;
  }
  if (Number.isFinite(cleanupRetryAt)) {
    if (Number.isFinite(expiresAt) && expiresAt <= now) {
      return cleanupRetryAt;
    }
    return Math.min(expiresAt, cleanupRetryAt);
  }
  return expiresAt;
}

function cleanupClaimDeadline(lease: LeaseRecord): number {
  const explicit = Date.parse(lease.cleanupClaimExpiresAt ?? "");
  if (Number.isFinite(explicit)) {
    return explicit;
  }
  const startedAt = Date.parse(lease.cleanupStartedAt ?? "");
  return Number.isFinite(startedAt) ? startedAt + leaseCleanupClaimStaleMs : Number.NaN;
}

function clearLeaseCleanupMetadata(lease: LeaseRecord): void {
  delete lease.cleanupAttempts;
  delete lease.cleanupError;
  delete lease.cleanupFailedAt;
  delete lease.cleanupRetryAt;
}

function terminalizeManualProviderCleanup(
  lease: LeaseRecord,
  error: string,
  terminalAt: string,
): void {
  if (leaseIsLive(lease)) {
    lease.state = "expired";
  }
  lease.keep = true;
  lease.releaseDeletesServer = false;
  lease.failureError = error;
  lease.updatedAt = terminalAt;
  lease.endedAt = terminalAt;
  clearLeaseCleanupMetadata(lease);
  delete lease.cleanupStartedAt;
  delete lease.cleanupClaimExpiresAt;
  delete lease.provisioningResourceMayExist;
  delete lease.provisioningFailureRetryable;
}

function clearRuntimeAdapterDeleteMetadata(lease: LeaseRecord): void {
  delete lease.runtimeAdapterDeleteRequestedAt;
  delete lease.runtimeAdapterDeleteClaimID;
  delete lease.runtimeAdapterDeleteRetryAt;
  delete lease.runtimeAdapterDeleteDispatchUntil;
  delete lease.runtimeAdapterDeleteAttempts;
  delete lease.runtimeAdapterDeleteError;
}

function runtimeAdapterDeleteVersionMatches(
  current: LeaseRecord,
  anchor: LeaseRecord,
  claim: RuntimeAdapterDeleteVersion,
): boolean {
  return (
    (leaseIsLive(current) || current.state === "expired") &&
    isRegisteredLease(current) &&
    current.runtimeAdapterDeleteRequestedAt === claim.requestedAt &&
    current.runtimeAdapterDeleteClaimID === claim.claimID &&
    current.runtimeAdapterID === anchor.runtimeAdapterID &&
    current.runtimeAdapterWorkspaceID === anchor.runtimeAdapterWorkspaceID &&
    current.runtimeAdapterRegistrationID === anchor.runtimeAdapterRegistrationID
  );
}

function finalizedRuntimeAdapterDeleteLease(current: LeaseRecord): LeaseRecord {
  if (current.state !== "expired") {
    return finalizedReleasedLease(current, false, true);
  }
  const lease = structuredClone(current);
  lease.updatedAt = new Date().toISOString();
  clearRuntimeAdapterDeleteMetadata(lease);
  return lease;
}

function finalizedReleasedLease(
  current: LeaseRecord,
  deleteServer: boolean,
  keep?: boolean,
): LeaseRecord {
  const lease = structuredClone(current);
  const wasUnprovisionedRelease =
    !lease.cloudID && (lease.state === "provisioning" || lease.state === "released");
  const now = new Date().toISOString();
  lease.state = "released";
  lease.updatedAt = now;
  lease.releasedAt = now;
  lease.endedAt = now;
  if (wasUnprovisionedRelease) {
    lease.releaseDeletesServer = deleteServer;
  } else if (
    !deleteServer &&
    !isRegisteredLease(lease) &&
    (lease.cloudID || lease.providerKeyCleanupPending)
  ) {
    lease.releaseDeletesServer = false;
  } else {
    delete lease.releaseDeletesServer;
  }
  clearLeaseCleanupMetadata(lease);
  clearRuntimeAdapterDeleteMetadata(lease);
  delete lease.cleanupStartedAt;
  delete lease.cleanupClaimExpiresAt;
  if (keep !== undefined) {
    lease.keep = keep;
  }
  return lease;
}

function normalizeShareUser(value: string | undefined): string {
  return (value ?? "").trim().toLowerCase();
}

function sanitizeShareRole(value: string | undefined): LeaseShareRole | undefined {
  return value === "manage" || value === "use" ? value : undefined;
}

type NormalizedLeaseShare = {
  users: Record<string, LeaseShareRole>;
  org?: LeaseShareRole;
  updatedAt?: string;
  updatedBy?: string;
};

function normalizedLeaseShare(share: LeaseShare | undefined): NormalizedLeaseShare {
  const users: Record<string, LeaseShareRole> = {};
  for (const [rawUser, rawRole] of Object.entries(share?.users ?? {})) {
    const user = normalizeShareUser(rawUser);
    const role = sanitizeShareRole(rawRole);
    if (user && role) {
      users[user] = role;
    }
  }
  const role = sanitizeShareRole(share?.org);
  const normalized: NormalizedLeaseShare = { users };
  if (role) {
    normalized.org = role;
  }
  if (share?.updatedAt) {
    normalized.updatedAt = share.updatedAt;
  }
  if (share?.updatedBy) {
    normalized.updatedBy = share.updatedBy;
  }
  return normalized;
}

function leaseShareAccessShrank(
  previous: NormalizedLeaseShare,
  current: NormalizedLeaseShare,
): boolean {
  if (leaseShareRoleRank(current.org) < leaseShareRoleRank(previous.org)) {
    return true;
  }
  return Object.entries(previous.users).some(
    ([user, role]) => leaseShareRoleRank(current.users[user]) < leaseShareRoleRank(role),
  );
}

function leaseShareRoleRank(role: LeaseShareRole | undefined): number {
  if (role === "manage") {
    return 2;
  }
  return role === "use" ? 1 : 0;
}

function sanitizeLeaseShare(input: Partial<LeaseShare>, updatedBy: string): LeaseShare | undefined {
  const share = normalizedLeaseShare(input);
  const hasUsers = Object.keys(share.users).length > 0;
  if (!hasUsers && !share.org) {
    return undefined;
  }
  return {
    users: hasUsers ? share.users : undefined,
    org: share.org,
    updatedAt: new Date().toISOString(),
    updatedBy,
  };
}

async function optionalJson<T>(request: Request): Promise<T> {
  if (!request.headers.get("content-type")?.includes("application/json")) {
    return {} as T;
  }
  return readJson<T>(request);
}

function capacityHints(
  env: Env,
  config: ReturnType<typeof leaseConfig>,
  lease: LeaseRecord,
  attempts: ProvisioningAttempt[],
): CapacityHint[] {
  if (!config.capacityHints || envFlagDisabled(env.CRABBOX_CAPACITY_HINTS)) {
    return [];
  }
  const hints: CapacityHint[] = [];
  const provider = lease.provider === "azure" ? "azure" : "aws";
  const providerName = provider === "azure" ? "Azure" : "AWS";
  const selectedRegion =
    lease.region || (provider === "azure" ? config.azureLocation : config.awsRegion);
  const selectedMarket = lease.market || config.capacityMarket;
  const attemptedRegions = uniqueNonEmpty(attempts.map((attempt) => attempt.region));
  const failedRegions = attemptedRegions.filter((region) => region !== selectedRegion);
  if (selectedRegion && failedRegions.length > 0) {
    hints.push({
      code: `${provider}_capacity_routed`,
      message: `${providerName} launch routed to ${selectedRegion} after failed attempts in ${failedRegions.join(", ")}`,
      action: `Keep multiple capacity regions configured and avoid pinning a single ${providerName} region during capacity pressure.`,
      region: selectedRegion,
      market: selectedMarket,
      class: config.class,
      serverType: lease.serverType,
      regionsTried: uniqueNonEmpty([...attemptedRegions, selectedRegion]),
    });
  }
  if (attempts.some((attempt) => attempt.category === "quota")) {
    hints.push({
      code: `${provider}_quota_pressure`,
      message: `${providerName} quota rejected at least one ${config.class} candidate before selecting ${lease.serverType}`,
      action:
        provider === "azure"
          ? "Use a smaller class or request more Azure vCPU quota for the affected regions."
          : "Use a smaller class or request more EC2 Standard Spot/On-Demand vCPU quota for the affected regions.",
      region: selectedRegion,
      market: selectedMarket,
      class: config.class,
      serverType: lease.serverType,
      regionsTried: uniqueNonEmpty([...attemptedRegions, selectedRegion]),
    });
  }
  if (
    selectedMarket === "on-demand" &&
    attempts.some((attempt) => (attempt.market || "spot") === "spot")
  ) {
    hints.push({
      code: `${provider}_on_demand_fallback`,
      message: `${providerName} launch used on-demand after spot capacity attempts for ${config.class}`,
      action:
        "Keep on-demand fallback for reliability, or switch back to spot when cost matters more than launch success.",
      region: selectedRegion,
      market: selectedMarket,
      class: config.class,
      serverType: lease.serverType,
      regionsTried: uniqueNonEmpty([...attemptedRegions, selectedRegion]),
    });
  }
  if (capacityLargeClasses(env).includes(config.class)) {
    hints.push({
      code: "capacity_large_class",
      message: `class=${config.class} is configured as a high-pressure capacity class`,
      action:
        "Use a smaller class unless the workload is explicitly CPU-bound or this large class was requested intentionally.",
      region: selectedRegion,
      market: selectedMarket,
      class: config.class,
      serverType: lease.serverType,
    });
  }
  return hints;
}

function capacityLargeClasses(env: Env): string[] {
  return uniqueNonEmpty((env.CRABBOX_CAPACITY_LARGE_CLASSES || "beast").split(","));
}

function envFlagDisabled(value: string | undefined): boolean {
  return ["0", "false", "no", "off"].includes((value || "").trim().toLowerCase());
}

function envFlagEnabled(value: string | undefined): boolean {
  return ["1", "true", "yes", "on"].includes((value || "").trim().toLowerCase());
}

function positiveEnvInt(value: string | undefined, fallback: number): number {
  const parsed = Number.parseInt((value ?? "").trim(), 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : fallback;
}

function uniqueNonEmpty(values: Array<string | undefined>): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const value of values) {
    const normalized = (value || "").trim();
    if (normalized && !seen.has(normalized)) {
      seen.add(normalized);
      out.push(normalized);
    }
  }
  return out;
}

function awsLeaseSSHSourceCIDRs(
  config: Pick<ReturnType<typeof leaseConfig>, "awsSSHCIDRs">,
  context: ProviderAccessContext,
): string[] {
  return config.awsSSHCIDRs.length > 0 ? config.awsSSHCIDRs : context.requestSourceCIDRs;
}

function awsGlobalSSHSourceCIDRs(env: Env): string[] {
  return uniqueNonEmpty(validCIDRs((env.CRABBOX_AWS_SSH_CIDRS ?? "").split(",")));
}

function withLeaseSSHSourceCIDRs(
  lease: LeaseRecord,
  cidrs: string[],
  complete: boolean,
): LeaseRecord {
  if (cidrs.length === 0 && !complete) {
    return lease;
  }
  return {
    ...lease,
    network: {
      ...lease.network,
      sshSourceCIDRs: uniqueNonEmpty(cidrs),
      sshSourceCIDRsComplete: complete,
    },
  };
}

function leaseOwnsAWSSSHAccess(lease: LeaseRecord): boolean {
  return (
    lease.provider === "aws" &&
    !isRegisteredLease(lease) &&
    !lease.network?.awsPrivate &&
    (leaseIsLive(lease) || lease.releaseDeletesServer === false)
  );
}

function leaseHasPublishedAWSAccess(lease: LeaseRecord): boolean {
  return Boolean(
    lease.provider === "aws" &&
    !lease.network?.awsPrivate &&
    (lease.cloudID ||
      lease.network?.sshSourceCIDRsComplete !== undefined ||
      (lease.network?.sshSourceCIDRs?.length ?? 0) > 0 ||
      lease.network?.awsSecurityGroupID ||
      lease.network?.awsSubnetID),
  );
}

function awsIngressReconcileTargetKey(lease: LeaseRecord): string {
  return [
    lease.region ?? "",
    lease.network?.awsSecurityGroupID ?? "",
    lease.network?.awsSecurityGroupName ?? "",
    lease.network?.awsSubnetID ?? "",
    lease.sshPort,
    ...(lease.sshFallbackPorts ?? []).toSorted(),
  ].join("\u0000");
}

function awsIngressAccessTargetKey(
  lease: LeaseRecord,
  region: string,
  ports: string[],
  env: Env,
): string {
  const workspaceManaged = lease.providerKey.startsWith(workspaceProviderKeyPrefix);
  const securityGroupID =
    lease.network?.awsSecurityGroupID ||
    (workspaceManaged ? "" : env.CRABBOX_AWS_SECURITY_GROUP_ID || "");
  const subnetID = lease.network?.awsSubnetID || env.CRABBOX_AWS_SUBNET_ID || "";
  const securityGroupName = lease.network?.awsSecurityGroupName;
  const group = securityGroupID
    ? `sg:${securityGroupID}`
    : securityGroupName
      ? `managed:${subnetID}:${securityGroupName}`
      : `auto:${subnetID}`;
  return [region, group, ...ports.toSorted()].join("\u0000");
}

function awsIngressGroupMetadataUnknown(lease: LeaseRecord, env: Env): boolean {
  return (
    !lease.network?.awsSecurityGroupID &&
    !lease.network?.awsSecurityGroupName &&
    (lease.providerKey.startsWith(workspaceProviderKeyPrefix) || !env.CRABBOX_AWS_SECURITY_GROUP_ID)
  );
}

function awsIngressPortScopeKey(region: string, port: string): string {
  return [region, port].join("\u0000");
}

function awsLeaseSSHPorts(lease: LeaseRecord): string[] {
  return uniqueNonEmpty([lease.sshPort, ...(lease.sshFallbackPorts ?? [])]);
}

function awsIngressReconcileTargets(
  record: StoredAWSIngressReconcileRecord | undefined,
): AWSIngressReconcileTarget[] {
  if (!record) {
    return [];
  }
  if ("targets" in record) {
    return record.targets.map((target) => structuredClone(target));
  }
  return [
    {
      anchor: structuredClone(record.anchor),
      attempts: record.attempts,
      generation: `legacy:${record.updatedAt}:${awsIngressReconcileTargetKey(record.anchor)}`,
      updatedAt: record.updatedAt,
      retryAt: record.retryAt,
      ...(record.lastError ? { lastError: record.lastError } : {}),
    },
  ];
}

function activeAWSSSHSourceCIDRs(leases: LeaseRecord[], cidrs: string[]): string[] {
  return uniqueNonEmpty([
    ...leases.flatMap((lease) =>
      leaseOwnsAWSSSHAccess(lease) ? (lease.network?.sshSourceCIDRs ?? []) : [],
    ),
    ...cidrs,
  ]);
}

function hasUnknownActiveAWSSSHSource(leases: LeaseRecord[]): boolean {
  return leases.some(
    (lease) =>
      leaseOwnsAWSSSHAccess(lease) &&
      (lease.network?.sshSourceCIDRs?.length ?? 0) === 0 &&
      !lease.network?.sshSourceCIDRsComplete,
  );
}

function awsOrphanSweepCandidate(
  machine: ProviderMachine,
  activeLeases: Map<string, LeaseRecord>,
  activeCloudIDs: Set<string>,
  region: string,
  graceSeconds: number,
): AWSOrphanSweepCandidate | undefined {
  return cloudOrphanSweepCandidate(machine, activeLeases, activeCloudIDs, region, graceSeconds);
}

function cloudOrphanSweepCandidate(
  machine: ProviderMachine,
  activeLeases: Map<string, LeaseRecord>,
  activeCloudIDs: Set<string>,
  region: string,
  graceSeconds: number,
): CloudOrphanSweepCandidate | undefined {
  const cloudID = machine.cloudID || String(machine.id);
  if (activeCloudIDs.has(cloudID)) {
    return undefined;
  }
  const labels = machine.labels ?? {};
  if (envFlagEnabled(labels["keep"])) {
    return undefined;
  }
  const leaseID = (labels["lease"] ?? "").trim();
  const activeLease = leaseID ? activeLeases.get(leaseID) : undefined;
  if (
    activeLease?.state === "provisioning" ||
    activeLease?.cloudID === cloudID ||
    (activeLease?.releaseDeletesServer === false && !activeLease.cloudID)
  ) {
    return undefined;
  }
  const now = Date.now();
  const graceMs = Math.max(0, graceSeconds) * 1000;
  const createdAt = parseProviderLabelTime(labels["created_at"]);
  const expiresAt = parseProviderLabelTime(labels["expires_at"]);
  const oldEnough = Number.isFinite(createdAt) && createdAt + graceMs <= now;
  const expired = Number.isFinite(expiresAt) && expiresAt + graceMs <= now;
  let reason = "";
  if (activeLease && oldEnough) {
    reason = "lease-cloud-mismatch";
  } else if (expired) {
    reason = "expired-provider-tag";
  } else if (!leaseID && oldEnough) {
    reason = "missing-lease-label";
  } else if (leaseID && !activeLease && oldEnough) {
    reason = "no-active-lease";
  }
  if (!reason) {
    return undefined;
  }
  const candidate: CloudOrphanSweepCandidate = {
    region,
    cloudID,
    name: machine.name,
    status: machine.status,
    serverType: machine.serverType,
    reason,
    ownership: "provider-tags-only",
    action: "reported",
  };
  if (machine.host) {
    candidate.host = machine.host;
  }
  if (leaseID) {
    candidate.leaseID = leaseID;
  }
  if (labels["slug"]) {
    candidate.slug = labels["slug"];
  }
  if (labels["owner"]) {
    candidate.owner = labels["owner"];
  }
  if (Number.isFinite(createdAt)) {
    candidate.createdAt = new Date(createdAt).toISOString();
  }
  if (Number.isFinite(expiresAt)) {
    candidate.expiresAt = new Date(expiresAt).toISOString();
  }
  if (activeLease?.cloudID) {
    candidate.activeCloudID = activeLease.cloudID;
  }
  return candidate;
}

function cloudOrphanSweepResourceKey(cloudID: string, region: string): string {
  return JSON.stringify([region, cloudID]);
}

function recordCloudOrphanSweepOwnership(
  candidate: CloudOrphanSweepCandidate,
  lease: LeaseRecord | undefined,
): void {
  if (!lease) {
    return;
  }
  candidate.ownership = "coordinator-lease";
  candidate.ownershipLeaseID = lease.id;
}

function leaseOwnsCloudResourceDuringSweep(lease: LeaseRecord, now: number): boolean {
  if (leaseIsLive(lease)) {
    return true;
  }
  if (lease.state === "released" && lease.releaseDeletesServer === false) {
    return true;
  }
  return Boolean(lease.cleanupStartedAt && cleanupClaimDeadline(lease) > now);
}

function awsMacHostSweepCandidate(
  host: AWSMacHost,
  activeLeases: LeaseRecord[],
  region: string,
  graceSeconds: number,
): AWSMacHostSweepCandidate | undefined {
  if (host.tags["crabbox"] !== "true" && host.tags["created_by"] !== "crabbox") {
    return undefined;
  }
  const activeLease = activeLeases.find((lease) => leaseHostID(lease) === host.id);
  if (activeLease) {
    return undefined;
  }
  if (host.state !== "pending") {
    return undefined;
  }
  const allocationTime = Date.parse(host.allocationTime ?? "");
  if (!Number.isFinite(allocationTime)) {
    return undefined;
  }
  const graceMs = Math.max(0, graceSeconds) * 1000;
  if (allocationTime + graceMs > Date.now()) {
    return undefined;
  }
  return {
    region,
    hostID: host.id,
    state: host.state,
    instanceType: host.instanceType,
    availabilityZone: host.availabilityZone,
    allocationTime: new Date(allocationTime).toISOString(),
    reason: "stale-pending-mac-host",
    ownership: "provider-tags-only",
    action: "reported",
  };
}

function recordAWSMacHostSweepOwnership(
  candidate: AWSMacHostSweepCandidate,
  lease: LeaseRecord | undefined,
): void {
  if (!lease) {
    return;
  }
  candidate.ownership = "coordinator-lease";
  candidate.ownershipLeaseID = lease.id;
}

function parseProviderLabelTime(value: string | undefined): number {
  const raw = (value ?? "").trim();
  if (!raw) {
    return Number.NaN;
  }
  if (/^\d+$/.test(raw)) {
    const seconds = Number.parseInt(raw, 10);
    return Number.isFinite(seconds) ? seconds * 1000 : Number.NaN;
  }
  return Date.parse(raw);
}

interface CloudProvider {
  listCrabboxServers(): Promise<ProviderMachine[]>;
  workspaceCapability?(
    lease?: LeaseRecord,
    purpose?: "operate" | "observe",
  ): ProviderWorkspaceCapability | undefined;
  supportsSSHHostKeyInjection(config: ReturnType<typeof leaseConfig>): boolean;
  restrictedLeaseRequestFields?(input: LeaseRequest): string[];
  recoverServer?(lease: LeaseRecord): Promise<ProviderMachine | undefined>;
  resumeRecoveredServer?(
    config: ReturnType<typeof leaseConfig>,
    lease: LeaseRecord,
    server: ProviderMachine,
  ): Promise<ProviderMachine>;
  findServerByLease?(leaseID: string): Promise<ProviderMachine | undefined>;
  getServer?(id: string): Promise<ProviderMachine>;
  prepareLeaseConfig?(
    config: ReturnType<typeof leaseConfig>,
  ): Promise<ReturnType<typeof leaseConfig>>;
  prepareLeaseCreate?(
    config: ReturnType<typeof leaseConfig>,
    lease: LeaseRecord,
    context: ProviderAccessContext,
  ): Promise<ProviderLeaseCreatePreparation>;
  refreshLeaseAccess?(
    lease: LeaseRecord,
    context: ProviderAccessContext,
  ): Promise<LeaseRecord | void>;
  refreshLeaseAccessForResolution?(lease: LeaseRecord): Promise<LeaseRecord | void>;
  reconcileLeaseAccess?(lease: LeaseRecord, context: ProviderAccessContext): Promise<void>;
  createServerWithFallback(
    config: ReturnType<typeof leaseConfig>,
    leaseID: string,
    slug: string,
    owner: string,
    provisioning?: ProviderProvisioningContext,
  ): Promise<{
    server: ProviderMachine;
    serverType: string;
    market?: string;
    attempts?: ProvisioningAttempt[];
  }>;
  finalizeLeaseCreate?(
    config: ReturnType<typeof leaseConfig>,
    lease: LeaseRecord,
    server: ProviderMachine,
    attempts: ProvisioningAttempt[],
  ): Promise<ProviderLeaseCreateFinalization>;
  releaseLease(lease: LeaseRecord): Promise<void>;
  deleteServer(id: string): Promise<void>;
  deleteOwnedServer?(lease: LeaseRecord): Promise<void>;
  supportsNativeImages(): boolean;
  nativeImagesUnsupportedMessage(): string;
  defaultImageStrategy(lease: LeaseRecord): "image" | "disk-snapshot";
  validateLeaseImageStrategy(
    lease: LeaseRecord,
    strategy: "image" | "disk-snapshot",
  ): string | undefined;
  createLeaseImage(
    lease: LeaseRecord,
    name: string,
    noReboot: boolean,
    strategy: "image" | "disk-snapshot",
  ): Promise<ProviderImage>;
  getImage(imageID: string, kind?: string): Promise<ProviderImage>;
  deleteImage(imageID: string, kind?: string): Promise<void>;
  storedImageMetadata(imageID: string): Promise<ProviderImage | undefined>;
  decorateImage(image: ProviderImage, metadata?: Partial<ProviderImage>): ProviderImage;
  validateDeleteImage(
    imageID: string,
    metadata?: Partial<ProviderImage>,
  ): Promise<{ status: number; body: Record<string, unknown> } | undefined>;
  promoteImage?(
    imageID: string,
    metadata: ProviderImage | undefined,
    request: Request,
    url: URL,
  ): Promise<Response | { image: ProviderImage }>;
  fastSnapshotRestoreForImage?(
    imageID: string,
    metadata: ProviderImage | undefined,
    url: URL,
  ): Promise<
    Response | { image: ProviderImage; fastSnapshotRestores: ProviderFastSnapshotRestore[] }
  >;
  enableFastSnapshotRestore?(
    snapshotIDs: string[],
    availabilityZones: string[],
  ): Promise<ProviderFastSnapshotRestore[]>;
  fastSnapshotRestoreStatus?(
    snapshotIDs: string[],
    availabilityZones?: string[],
  ): Promise<ProviderFastSnapshotRestore[]>;
  deleteSSHKey(name: string, leaseID: string): Promise<void>;
  hourlyPriceUSD(
    serverType: string,
    config: ReturnType<typeof leaseConfig>,
  ): Promise<number | undefined>;
}

type ProviderWorkspaceLifecycleEvent =
  | "create_accepted"
  | "ready"
  | "recovered_ready"
  | "delete_requested"
  | "terminated";

interface ProviderWorkspaceCapability {
  requiresCommand: boolean;
  supportsDesktop: boolean;
  supportsPrewarm: boolean;
  leaseRequestFields(workspace: WorkspaceRecord): Partial<LeaseRequest>;
  recoveryLeaseRequestFields(
    workspace: WorkspaceRecord,
    server: ProviderMachine,
  ): Partial<LeaseRequest>;
  recoveredReady(server: ProviderMachine): boolean;
  recoveredHost(server: ProviderMachine): string;
  applyRecoveredEvidence(
    lease: LeaseRecord,
    config: ReturnType<typeof leaseConfig>,
    server: ProviderMachine,
  ): void;
  bootstrapEvidence(lease: LeaseRecord | undefined, status: string): Record<string, unknown>;
  log(event: ProviderWorkspaceLifecycleEvent, fields: Record<string, string | undefined>): void;
}

interface ProviderStateStorage {
  get<T>(key: string): Promise<T | undefined>;
  put<T>(key: string, value: T): Promise<void>;
  list<T>(options?: { prefix?: string }): Promise<Map<string, T>>;
  delete(key: string): Promise<unknown>;
}

interface ProviderAccessContext {
  requestSourceCIDRs: string[];
  activeLeases: LeaseRecord[];
}

interface ProviderLeaseCreatePreparation {
  config: ReturnType<typeof leaseConfig>;
  lease: LeaseRecord;
  provisioning?: ProviderProvisioningContext;
}

interface ProviderLeaseCreateFinalization {
  config: ReturnType<typeof leaseConfig>;
  lease: LeaseRecord;
}

interface ProviderProvisioningContext {
  sshIngressReconcile?: "authoritative" | "additive";
  allowEmptySSHIngress?: boolean;
  publishAccessBeforeProvisioning?: boolean;
  onTargetAttempt?: (target: ProviderProvisioningTarget) => Promise<void>;
}

interface ProviderProvisioningTarget {
  region?: string;
}

export class HetznerProvider implements CloudProvider {
  private clientValue?: HetznerClient;

  constructor(private readonly env: Env) {}

  private get client(): HetznerClient {
    this.clientValue ??= new HetznerClient(this.env);
    return this.clientValue;
  }

  async listCrabboxServers(): Promise<ProviderMachine[]> {
    const servers = await this.client.listCrabboxServers();
    return servers.map((server) => this.client.toMachine(server));
  }

  supportsSSHHostKeyInjection(config: ReturnType<typeof leaseConfig>): boolean {
    return config.target === "linux";
  }

  async findServerByLease(leaseID: string): Promise<ProviderMachine | undefined> {
    const server = await this.client.findServerByLease(leaseID);
    return server ? this.client.toMachine(server) : undefined;
  }

  async createServerWithFallback(
    config: ReturnType<typeof leaseConfig>,
    leaseID: string,
    slug: string,
    owner: string,
  ): Promise<{
    server: ProviderMachine;
    serverType: string;
    market?: string;
    attempts?: ProvisioningAttempt[];
  }> {
    const { server, serverType, providerKey } = await this.client.createServerWithFallback(
      config,
      leaseID,
      slug,
      owner,
    );
    return { server: { ...this.client.toMachine(server), providerKey }, serverType };
  }

  async deleteServer(id: string): Promise<void> {
    await this.client.deleteServer(Number(id));
  }

  async finalizeLeaseCreate(
    config: ReturnType<typeof leaseConfig>,
    lease: LeaseRecord,
    server: ProviderMachine,
  ): Promise<ProviderLeaseCreateFinalization> {
    const providerKey = server.providerKey?.trim() || config.providerKey;
    const providerKeyCleanupOwned = providerKey === providerKeyForLease(lease.id);
    return {
      config: { ...config, providerKey },
      lease: { ...lease, providerKey, providerKeyCleanupOwned },
    };
  }

  async releaseLease(lease: LeaseRecord): Promise<void> {
    let serverID = Number(lease.serverID);
    let server: HetznerServer | undefined;
    if (
      (!Number.isSafeInteger(serverID) || serverID <= 0) &&
      lease.providerKeyCleanupPending &&
      lease.provisioningResourceMayExist
    ) {
      server = await this.client.findServerByLease(lease.id);
      serverID = server?.id ?? Number.NaN;
    }
    if (Number.isSafeInteger(serverID) && serverID > 0) {
      try {
        server ??= await this.client.getServer(serverID);
      } catch (error) {
        if (!providerResourceNotFound(error)) {
          throw error;
        }
      }
      if (server) {
        if (
          server.id !== serverID ||
          !hetznerServerOwnedByLease(server, lease.id, lease.slug, lease.serverName)
        ) {
          throw new Error(
            `refusing to delete Hetzner server ${serverID}: ownership does not match lease ${lease.id}`,
          );
        }
        try {
          await this.deleteServer(String(serverID));
        } catch (error) {
          if (!providerResourceNotFound(error)) {
            throw error;
          }
        }
      }
    }
    if (lease.providerKeyCleanupPending) {
      const providerKeyID = Number(lease.providerKeyCleanupID);
      if (!Number.isSafeInteger(providerKeyID) || providerKeyID <= 0) {
        throw new Error("invalid pending Hetzner SSH key cleanup id");
      }
      await this.client.deleteSSHKeyByID(providerKeyID);
      return;
    }
    if (leaseUsesCanonicalProviderKey(lease)) {
      await this.deleteSSHKey(lease.providerKey, lease.id);
    }
  }

  supportsNativeImages(): boolean {
    return false;
  }

  nativeImagesUnsupportedMessage(): string {
    return "native images are supported for AWS, Azure, and GCP leases";
  }

  defaultImageStrategy(): "image" | "disk-snapshot" {
    return "disk-snapshot";
  }

  validateLeaseImageStrategy(): string | undefined {
    return undefined;
  }

  createLeaseImage = unsupportedProviderImageLifecycle("hetzner");
  getImage = unsupportedProviderImageLifecycle("hetzner");
  deleteImage = unsupportedProviderImageLifecycle("hetzner");
  storedImageMetadata = noStoredImageMetadata;
  decorateImage = passthroughProviderImage;
  validateDeleteImage = allowProviderImageDelete;

  async deleteSSHKey(name: string, leaseID: string): Promise<void> {
    await this.client.deleteSSHKey(name, leaseID);
  }

  hourlyPriceUSD(
    serverType: string,
    config: ReturnType<typeof leaseConfig>,
  ): Promise<number | undefined> {
    return this.client.hourlyPriceUSD(serverType, config.location);
  }
}

export class AzureProvider implements CloudProvider {
  private clientValue?: AzureClient;

  constructor(
    private readonly env: Env,
    private readonly deferredCleanup?: (request: AzureDeferredCleanupRequest) => Promise<void>,
    private readonly storage?: ProviderStateStorage,
    private readonly location?: string,
  ) {}

  private get client(): AzureClient {
    this.clientValue ??= new AzureClient(this.env, {
      ...(this.location ? { location: this.location } : {}),
      ...(this.deferredCleanup ? { deferredCleanup: this.deferredCleanup } : {}),
      ...(this.storage ? { ownedDeleteClaimStorage: this.storage } : {}),
    });
    return this.clientValue;
  }

  restrictedLeaseRequestFields(input: LeaseRequest): string[] {
    return [input.azureImage ? "azureImage" : "", input.azureOSDisk ? "azureOSDisk" : ""].filter(
      Boolean,
    );
  }

  listCrabboxServers(): Promise<ProviderMachine[]> {
    return this.client.listCrabboxServers();
  }

  supportsSSHHostKeyInjection(config: ReturnType<typeof leaseConfig>): boolean {
    return config.target === "linux" && !config.azureSnapshot;
  }

  getServer(id: string): Promise<ProviderMachine> {
    return this.client.getServer(id);
  }

  findServer(id: string): Promise<ProviderMachine | undefined> {
    return this.client.findServer(id);
  }

  async prepareLeaseConfig(
    config: ReturnType<typeof leaseConfig>,
  ): Promise<ReturnType<typeof leaseConfig>> {
    return config.azureLocation
      ? config
      : { ...config, azureLocation: azureLocationFor(this.env, "") };
  }

  prepareLeaseCreate(
    config: ReturnType<typeof leaseConfig>,
    lease: LeaseRecord,
  ): Promise<ProviderLeaseCreatePreparation> {
    return Promise.resolve({
      config,
      lease: { ...lease, providerScope: this.client.providerScope() },
    });
  }

  createServerWithFallback(
    config: ReturnType<typeof leaseConfig>,
    leaseID: string,
    slug: string,
    owner: string,
  ): Promise<{
    server: ProviderMachine;
    serverType: string;
    market?: string;
    attempts?: ProvisioningAttempt[];
  }> {
    return this.client.createServerWithFallback(config, leaseID, slug, owner);
  }

  deleteServer(id: string): Promise<void> {
    return this.client.deleteServer(id);
  }

  deleteOwnedServer(lease: LeaseRecord): Promise<void> {
    const scope = azureProviderScope(lease.providerScope);
    if (!scope) {
      return Promise.reject(
        new ProviderCleanupManualResolutionError(
          `refusing to delete Azure lease ${lease.id}: canonical provider scope was not persisted`,
        ),
      );
    }
    return new AzureClient(this.env, {
      ...(this.location ? { location: this.location } : {}),
      ...(this.deferredCleanup ? { deferredCleanup: this.deferredCleanup } : {}),
      subscription: scope.subscription,
      resourceGroup: scope.resourceGroup,
      ...(this.storage ? { ownedDeleteClaimStorage: this.storage } : {}),
    }).deleteOwnedServer(lease);
  }

  async finalizeLeaseCreate(
    config: ReturnType<typeof leaseConfig>,
    lease: LeaseRecord,
    server: ProviderMachine,
    attempts: ProvisioningAttempt[],
  ): Promise<ProviderLeaseCreateFinalization> {
    const region = server.region || config.azureLocation;
    const nextConfig = region ? { ...config, azureLocation: region } : config;
    const nextLease: LeaseRecord = {
      ...lease,
      region,
      providerScope: this.client.providerScope(),
    };
    const hints = capacityHints(this.env, nextConfig, nextLease, attempts);
    if (hints.length > 0) {
      nextLease.capacityHints = hints;
    }
    return { config: nextConfig, lease: nextLease };
  }

  async releaseLease(lease: LeaseRecord): Promise<void> {
    await this.deleteOwnedServer(lease);
  }

  supportsNativeImages(): boolean {
    return true;
  }

  nativeImagesUnsupportedMessage(): string {
    return "native images are supported for AWS, Azure, and GCP leases";
  }

  defaultImageStrategy(): "image" | "disk-snapshot" {
    return "disk-snapshot";
  }

  validateLeaseImageStrategy(
    _lease: LeaseRecord,
    strategy: "image" | "disk-snapshot",
  ): string | undefined {
    return strategy === "image"
      ? "Azure managed images require a stopped/generalized source VM; use disk-snapshot checkpoints for active Azure leases"
      : undefined;
  }

  async createLeaseImage(
    lease: LeaseRecord,
    name: string,
    _noReboot: boolean,
    _strategy: "image" | "disk-snapshot",
  ): Promise<ProviderImage> {
    const image = await this.client.createDiskSnapshot(
      lease.cloudID,
      providerImageResourceName("azure", name, lease.id),
    );
    const region = image.region ?? lease.region;
    const enriched: ProviderImage = {
      ...image,
      provider: "azure",
    };
    if (region) {
      enriched.region = region;
    }
    if (this.storage) {
      await storeCreatedProviderImage(this.storage, "azure", enriched);
    }
    return enriched;
  }

  getImage(imageID: string, kind?: string): Promise<ProviderImage> {
    return this.client.getImage(imageID, kind);
  }

  deleteImage(imageID: string, kind?: string): Promise<void> {
    return this.client.deleteImage(imageID, kind);
  }

  storedImageMetadata(imageID: string): Promise<ProviderImage | undefined> {
    return (
      this.storage?.get<ProviderImage>(createdProviderImageKey("azure", imageID)) ??
      Promise.resolve(undefined)
    );
  }
  decorateImage = passthroughProviderImage;
  validateDeleteImage = allowProviderImageDelete;

  async deleteSSHKey(): Promise<void> {
    // Azure stores the SSH public key inline on the VM; nothing to clean up.
  }

  hourlyPriceUSD(): Promise<number | undefined> {
    return Promise.resolve(undefined);
  }
}

export class GCPProvider implements CloudProvider {
  private clientValue?: GCPClient;

  constructor(
    private readonly env: Env,
    private readonly storage?: ProviderStateStorage,
    private readonly zone?: string,
    private readonly project?: string,
  ) {}

  private get client(): GCPClient {
    this.clientValue ??= new GCPClient(this.env, this.zone, this.project);
    return this.clientValue;
  }

  restrictedLeaseRequestFields(input: LeaseRequest): string[] {
    return [
      input.gcpProject ? "gcpProject" : "",
      input.gcpImage ? "gcpImage" : "",
      input.gcpNetwork ? "gcpNetwork" : "",
      input.gcpSubnet ? "gcpSubnet" : "",
      input.gcpTags?.length ? "gcpTags" : "",
      input.gcpServiceAccount ? "gcpServiceAccount" : "",
    ].filter(Boolean);
  }

  listCrabboxServers(): Promise<ProviderMachine[]> {
    return this.client.listCrabboxServers();
  }

  supportsSSHHostKeyInjection(config: ReturnType<typeof leaseConfig>): boolean {
    return config.target === "linux";
  }

  getServer(id: string): Promise<ProviderMachine> {
    return this.client.getServer(id);
  }

  findServer(id: string): Promise<ProviderMachine | undefined> {
    return this.client.findServer(id);
  }

  recoverServer(lease: LeaseRecord): Promise<ProviderMachine | undefined> {
    return this.client.recoverServerForLease(lease.id, lease.slug);
  }

  async prepareLeaseConfig(
    config: ReturnType<typeof leaseConfig>,
  ): Promise<ReturnType<typeof leaseConfig>> {
    if (config.gcpProject) {
      return config;
    }
    return {
      ...config,
      gcpProject: this.env.CRABBOX_GCP_PROJECT?.trim() || this.env.GCP_PROJECT_ID?.trim() || "",
    };
  }

  prepareLeaseCreate(
    config: ReturnType<typeof leaseConfig>,
    lease: LeaseRecord,
  ): Promise<ProviderLeaseCreatePreparation> {
    return Promise.resolve({ config, lease, provisioning: {} });
  }

  createServerWithFallback(
    config: ReturnType<typeof leaseConfig>,
    leaseID: string,
    slug: string,
    owner: string,
    provisioning?: ProviderProvisioningContext,
  ): Promise<{
    server: ProviderMachine;
    serverType: string;
    market?: string;
    attempts?: ProvisioningAttempt[];
  }> {
    return this.client.createServerWithFallback(config, leaseID, slug, owner, provisioning);
  }

  deleteServer(id: string): Promise<void> {
    return this.client.deleteServer(id);
  }

  async finalizeLeaseCreate(
    config: ReturnType<typeof leaseConfig>,
    lease: LeaseRecord,
    server: ProviderMachine,
  ): Promise<ProviderLeaseCreateFinalization> {
    return {
      config,
      lease: {
        ...lease,
        region: server.region ?? config.gcpZone,
        providerProject: config.gcpProject,
      },
    };
  }

  async releaseLease(lease: LeaseRecord): Promise<void> {
    if (
      !(await ownedProviderMachineForRelease("gcp", lease, (id) => this.findServer(id), {
        labelValue: gcpProviderLabelValue,
      }))
    ) {
      return;
    }
    await this.deleteServer(lease.cloudID);
  }

  supportsNativeImages(): boolean {
    return true;
  }

  nativeImagesUnsupportedMessage(): string {
    return "native images are supported for AWS, Azure, and GCP leases";
  }

  defaultImageStrategy(): "image" | "disk-snapshot" {
    return "disk-snapshot";
  }

  validateLeaseImageStrategy(): string | undefined {
    return undefined;
  }

  async createLeaseImage(
    lease: LeaseRecord,
    name: string,
    _noReboot: boolean,
    strategy: "image" | "disk-snapshot",
  ): Promise<ProviderImage> {
    const image =
      strategy === "image"
        ? await this.client.createImage(
            lease.cloudID,
            providerImageResourceName("gcp", name, lease.id),
          )
        : await this.client.createDiskSnapshot(
            lease.cloudID,
            providerImageResourceName("gcp", name, lease.id),
          );
    const region = image.region ?? lease.region;
    const project = image.project ?? lease.providerProject ?? this.project;
    const enriched: ProviderImage = {
      ...image,
      provider: "gcp",
    };
    if (region) {
      enriched.region = region;
    }
    if (project) {
      enriched.project = project;
    }
    if (this.storage) {
      await storeCreatedProviderImage(this.storage, "gcp", enriched);
    }
    return enriched;
  }

  getImage(imageID: string, kind?: string): Promise<ProviderImage> {
    return this.client.getImage(imageID, kind);
  }

  deleteImage(imageID: string, kind?: string): Promise<void> {
    return this.client.deleteImage(imageID, kind);
  }

  storedImageMetadata(imageID: string): Promise<ProviderImage | undefined> {
    return (
      this.storage?.get<ProviderImage>(createdProviderImageKey("gcp", imageID)) ??
      Promise.resolve(undefined)
    );
  }
  decorateImage = passthroughProviderImage;
  validateDeleteImage = allowProviderImageDelete;

  deleteSSHKey(): Promise<void> {
    return this.client.deleteSSHKey();
  }

  hourlyPriceUSD(): Promise<number | undefined> {
    return this.client.hourlyPriceUSD();
  }
}

export class DaytonaProvider implements CloudProvider {
  private clientValue?: DaytonaClient;
  private readonly pendingAccess = new Map<string, DaytonaSSHEndpoint>();

  constructor(private readonly env: Env) {}

  private get client(): DaytonaClient {
    this.clientValue ??= new DaytonaClient(this.env);
    return this.clientValue;
  }

  listCrabboxServers(): Promise<ProviderMachine[]> {
    return this.client.listCrabboxServers();
  }

  supportsSSHHostKeyInjection(): boolean {
    return false;
  }

  getServer(id: string): Promise<ProviderMachine> {
    return this.client.getServer(id);
  }

  findServerByLease(leaseID: string): Promise<ProviderMachine | undefined> {
    return this.client.findServerByLease(leaseID);
  }

  async recoverServer(lease: LeaseRecord): Promise<ProviderMachine | undefined> {
    const server = await this.findServerByLease(lease.id);
    return server && providerMachineOwnedByLease(server, lease, "daytona") ? server : undefined;
  }

  prepareLeaseConfig(
    config: ReturnType<typeof leaseConfig>,
  ): Promise<ReturnType<typeof leaseConfig>> {
    return Promise.resolve({
      ...config,
      serverType: this.client.snapshot || "default",
      sshUser: this.client.user,
      sshPort: "22",
      sshFallbackPorts: [],
      workRoot: this.client.workRoot,
    });
  }

  async createServerWithFallback(
    config: ReturnType<typeof leaseConfig>,
    leaseID: string,
    slug: string,
    owner: string,
  ): Promise<{
    server: ProviderMachine;
    serverType: string;
    market?: string;
    attempts?: ProvisioningAttempt[];
  }> {
    let server: ProviderMachine;
    try {
      server = await this.client.createServer(config, leaseID, slug, owner);
    } catch (error) {
      const recovered = await this.client.findServerByLease(leaseID).catch(() => undefined);
      if (
        !recovered ||
        !providerMachineOwnedByLease(
          recovered,
          {
            id: leaseID,
            slug,
            provider: "daytona",
            owner,
            cloudID: recovered.cloudID,
          },
          "daytona",
        )
      ) {
        throw error;
      }
      server = recovered;
    }
    try {
      const ready = await this.client.waitForStarted(server.cloudID);
      const access = await this.client.createSSHAccess(ready.cloudID, {
        expiresAt: new Date(Date.now() + config.ttlSeconds * 1_000).toISOString(),
      });
      this.pendingAccess.set(ready.cloudID, access);
      return {
        server: { ...ready, host: access.host },
        serverType: this.client.snapshot || ready.serverType || "default",
      };
    } catch (error) {
      try {
        const current = await this.client.getServer(server.cloudID);
        const owned = providerMachineOwnedByLease(
          current,
          {
            id: leaseID,
            slug,
            provider: "daytona",
            owner,
            cloudID: server.cloudID,
          },
          "daytona",
        );
        if (!owned) {
          throw new Error(
            `refusing to clean Daytona sandbox ${server.cloudID}: ownership does not match lease ${leaseID}`,
            { cause: error },
          );
        }
        await this.client.deleteServer(server.cloudID);
      } catch (cleanupError) {
        if (!isDaytonaNotFound(cleanupError)) {
          throw new ProviderProvisioningCleanupError(
            `${errorMessage(error)}; cleanup failed for Daytona sandbox ${server.cloudID}: ${errorMessage(cleanupError)}`,
            { provider: "daytona", cloudID: server.cloudID, serverID: server.id },
            cleanupError,
          );
        }
      }
      throw new Error(
        `${errorMessage(error)}; deleted Daytona sandbox ${server.cloudID} after readiness failure`,
        { cause: error },
      );
    }
  }

  async finalizeLeaseCreate(
    config: ReturnType<typeof leaseConfig>,
    lease: LeaseRecord,
    server: ProviderMachine,
  ): Promise<ProviderLeaseCreateFinalization> {
    const access =
      this.pendingAccess.get(server.cloudID) ??
      (await this.client.createSSHAccess(server.cloudID, lease));
    this.pendingAccess.delete(server.cloudID);
    return {
      config,
      lease: {
        ...lease,
        host: access.host,
        sshUser: access.user,
        sshPort: access.port,
        sshFallbackPorts: [],
        providerAccessExpiresAt: access.expiresAt,
        workRoot: this.client.workRoot,
        ...(server.region ? { region: server.region } : {}),
      },
    };
  }

  async refreshLeaseAccessForResolution(lease: LeaseRecord): Promise<LeaseRecord | void> {
    if (!daytonaAccessNeedsRefresh(lease)) return;
    const access = await this.client.createSSHAccess(lease.cloudID, lease);
    return {
      ...lease,
      host: access.host,
      sshUser: access.user,
      sshPort: access.port,
      sshFallbackPorts: [],
      providerAccessExpiresAt: access.expiresAt,
    };
  }

  async releaseLease(lease: LeaseRecord): Promise<void> {
    this.pendingAccess.delete(lease.cloudID);
    let server: ProviderMachine;
    try {
      server = await this.client.getServer(lease.cloudID);
    } catch (error) {
      if (isDaytonaNotFound(error)) return;
      throw error;
    }
    if (!providerMachineOwnedByLease(server, lease, "daytona")) {
      throw new Error(
        `refusing to delete Daytona sandbox ${lease.cloudID}: ownership does not match lease ${lease.id}`,
      );
    }
    await this.client.deleteServer(lease.cloudID);
  }

  deleteServer(id: string): Promise<void> {
    this.pendingAccess.delete(id);
    return this.client.deleteServer(id);
  }

  supportsNativeImages(): boolean {
    return false;
  }

  nativeImagesUnsupportedMessage(): string {
    return "Daytona sandboxes are selected through the coordinator snapshot configuration";
  }

  defaultImageStrategy(): "image" | "disk-snapshot" {
    return "disk-snapshot";
  }

  validateLeaseImageStrategy(): string | undefined {
    return undefined;
  }

  createLeaseImage = unsupportedProviderImageLifecycle("daytona");
  getImage = unsupportedProviderImageLifecycle("daytona");
  deleteImage = unsupportedProviderImageLifecycle("daytona");
  storedImageMetadata = noStoredImageMetadata;
  decorateImage = passthroughProviderImage;
  validateDeleteImage = allowProviderImageDelete;

  deleteSSHKey(): Promise<void> {
    return Promise.resolve();
  }

  hourlyPriceUSD(): Promise<number | undefined> {
    return Promise.resolve(undefined);
  }
}

export class AWSProvider implements CloudProvider {
  private clientValue?: EC2SpotClient;
  private readonly region: string;

  constructor(
    private readonly env: Env,
    region: string,
    private readonly storage: ProviderStateStorage,
  ) {
    this.region = region;
  }

  private get client(): EC2SpotClient {
    this.clientValue ??= new EC2SpotClient(this.env, this.region);
    return this.clientValue;
  }

  workspaceCapability(
    lease?: LeaseRecord,
    purpose: "operate" | "observe" = "operate",
  ): ProviderWorkspaceCapability | undefined {
    if (lease && lease.network?.awsPrivate !== true) {
      return undefined;
    }
    let policy: AWSPrivateWorkspaceConfig | undefined;
    try {
      policy = awsPrivateWorkspaceConfig(this.env);
    } catch (error) {
      if (purpose !== "observe" || !lease?.network?.awsPrivate) {
        throw error;
      }
    }
    if (!policy) {
      if (lease?.network?.awsPrivate) {
        if (purpose === "observe") {
          return privateAWSWorkspaceCapability(undefined, this.env);
        }
        throw new Error("private AWS workspace recovery policy is unavailable");
      }
      return undefined;
    }
    return privateAWSWorkspaceCapability(policy, this.env);
  }

  restrictedLeaseRequestFields(input: LeaseRequest): string[] {
    return [
      input.awsAMI ? "awsAMI" : "",
      input.awsSGID ? "awsSGID" : "",
      input.awsSubnetID ? "awsSubnetID" : "",
      input.awsProfile ? "awsProfile" : "",
      input.awsInstanceTypes?.length ? "awsInstanceTypes" : "",
      input.awsPrivate ? "awsPrivate" : "",
      input.awsRequireSSM ? "awsRequireSSM" : "",
      input.awsSSMBootstrapCommand ? "awsSSMBootstrapCommand" : "",
      input.awsSSMLogGroup ? "awsSSMLogGroup" : "",
    ].filter(Boolean);
  }

  listCrabboxServers(): Promise<ProviderMachine[]> {
    return this.client.listCrabboxServers();
  }

  supportsSSHHostKeyInjection(config: ReturnType<typeof leaseConfig>): boolean {
    return config.target === "linux" && !config.awsPrivate;
  }

  getServer(id: string): Promise<ProviderMachine> {
    return this.client.getServer(id);
  }

  findServer(id: string): Promise<ProviderMachine | undefined> {
    return this.client.findServer(id);
  }

  async recoverServer(lease: LeaseRecord): Promise<ProviderMachine | undefined> {
    if (lease.network?.awsPrivate) {
      if (lease.cloudID) {
        const server = await this.findServer(lease.cloudID);
        return server && providerMachineOwnedByLease(server, lease, "aws") ? server : undefined;
      }
      const server = await this.client.findWorkspaceServerByLease(lease.id);
      return server && providerLabelsOwnedByLease(server.labels, lease, "aws") ? server : undefined;
    }
    if (!lease.cloudID) return undefined;
    const server = await this.findServer(lease.cloudID);
    return server && providerMachineOwnedByLease(server, lease, "aws") ? server : undefined;
  }

  async resumeRecoveredServer(
    config: ReturnType<typeof leaseConfig>,
    lease: LeaseRecord,
    server: ProviderMachine,
  ): Promise<ProviderMachine> {
    if (!config.awsPrivate) return server;
    const region = server.region || config.awsRegion;
    const client = region === this.region ? this.client : new EC2SpotClient(this.env, region);
    try {
      const expectedGroups = [config.awsSGID].filter(Boolean).toSorted();
      const profileSuffix = `:instance-profile/${config.awsProfile}`;
      if (
        !["pending", "running"].includes(server.status) ||
        region !== config.awsRegion ||
        !config.awsInstanceTypes.includes(server.serverType) ||
        Boolean(server.host) ||
        (server.awsIPv6Addresses?.length ?? 0) > 0 ||
        Boolean(server.awsKeyName) ||
        server.awsSubnetID !== config.awsSubnetID ||
        JSON.stringify(server.awsSecurityGroupIDs ?? []) !== JSON.stringify(expectedGroups) ||
        !server.awsInstanceProfileARN?.endsWith(profileSuffix) ||
        server.awsMetadataHttpEndpoint !== "enabled" ||
        server.awsMetadataHttpTokens !== "required" ||
        server.awsMetadataHttpPutResponseHopLimit !== 1 ||
        server.awsMetadataInstanceTags !== "disabled"
      ) {
        throw new Error("recovered AWS private workspace is outside deployment policy");
      }
      await client.assertPrivateWorkspaceRootVolume(server, lease.id, config.awsRootGB);
      await client.waitForSSMOnline(server.cloudID);
      const bootstrap = await client.runSSMBootstrap(
        server.cloudID,
        lease.id,
        config.awsSSMBootstrapCommand,
        config.awsSSMLogGroup,
      );
      return {
        ...server,
        region,
        host: "",
        awsSSMCommandID: bootstrap.commandID,
        awsSSMCommandStatus: bootstrap.status,
      };
    } catch (error) {
      const resumeMessage = error instanceof Error ? error.message : String(error);
      try {
        await client.terminateServerAndWait(server.cloudID);
      } catch (cleanupError) {
        const cleanupMessage =
          cleanupError instanceof Error ? cleanupError.message : String(cleanupError);
        throw new ProviderProvisioningCleanupError(
          `${resumeMessage}; cleanup failed for recovered AWS instance ${server.cloudID}: ${cleanupMessage}`,
          {
            provider: "aws",
            cloudID: server.cloudID,
            region,
            serverID: server.id,
          },
          cleanupError,
        );
      }
      throw new Error(
        `${resumeMessage}; crabbox_aws_stale_instance_cleaned; deleted recovered AWS instance ${server.cloudID}`,
        { cause: error },
      );
    }
  }

  async prepareLeaseConfig(
    config: ReturnType<typeof leaseConfig>,
  ): Promise<ReturnType<typeof leaseConfig>> {
    if (
      config.awsAMI ||
      this.env.CRABBOX_AWS_AMI?.trim() ||
      config.awsSnapshot ||
      config.awsUseStockImage ||
      config.providerKey.startsWith(workspaceProviderKeyPrefix)
    ) {
      if (hasImageRequirements(config.imageRequirements)) {
        throw new ImageCapabilityMismatchError(
          "image capability requirements cannot be verified for an explicit or stock image source",
        );
      }
      return config;
    }
    if (config.target === "macos") {
      const awsPromotedAMIs = await this.promotedImagesForFallback(config);
      if (
        hasImageRequirements(config.imageRequirements) &&
        Object.keys(awsPromotedAMIs).length === 0
      ) {
        throw new ImageCapabilityMismatchError(
          "no promoted AWS macOS image satisfies the requested image capabilities",
        );
      }
      return { ...config, awsPromotedAMIs };
    }
    if (hasImageRequirements(config.imageRequirements)) {
      const awsPromotedAMIs = await this.promotedImagesForFallback(config);
      if (Object.keys(awsPromotedAMIs).length === 0) {
        throw new ImageCapabilityMismatchError(
          `no promoted AWS ${config.target} image satisfies the requested image capabilities`,
        );
      }
      return { ...config, awsAMI: "", awsPromotedAMIs };
    }
    const promoted = await this.promotedImage(config);
    return {
      ...config,
      awsAMI: promoted?.id ?? "",
      ...(promoted?.region ? { awsRegion: promoted.region } : {}),
    };
  }

  async prepareLeaseCreate(
    config: ReturnType<typeof leaseConfig>,
    lease: LeaseRecord,
    context: ProviderAccessContext,
  ): Promise<ProviderLeaseCreatePreparation> {
    if (config.awsPrivate) {
      const policy = awsPrivateWorkspaceConfig(this.env);
      if (
        !policy ||
        !config.awsSubnetID ||
        !config.awsSGID ||
        !config.awsProfile ||
        config.awsInstanceTypes.length === 0 ||
        !config.awsSSMBootstrapCommand ||
        !config.awsSSMLogGroup ||
        !config.awsRequireSSM ||
        !config.awsUseStockImage ||
        config.awsRegion !== policy.region ||
        config.awsSubnetID !== policy.subnetID ||
        config.awsSGID !== policy.securityGroupID ||
        config.awsProfile !== policy.instanceProfile ||
        config.awsRootGB !== policy.rootGB ||
        config.awsSSMLogGroup !== policy.ssmLogGroup ||
        config.capacityMarket !== policy.market ||
        config.capacityFallback !== "none" ||
        config.capacityRegions.length !== 1 ||
        config.capacityRegions[0] !== policy.region ||
        !policy.instanceTypes.includes(config.serverType) ||
        config.awsInstanceTypes.some((instanceType) => !policy.instanceTypes.includes(instanceType))
      ) {
        throw new Error("private AWS workspace policy is incomplete or outside deployment limits");
      }
      const nextLease: LeaseRecord = {
        ...lease,
        awsSSMLogGroup: config.awsSSMLogGroup,
        network: {
          ...lease.network,
          awsPrivate: true,
          awsSecurityGroupID: config.awsSGID,
          awsSubnetID: config.awsSubnetID,
        },
      };
      return {
        config: { ...config, awsSSHCIDRs: [] },
        lease: nextLease,
        provisioning: {
          allowEmptySSHIngress: true,
          publishAccessBeforeProvisioning: false,
        },
      };
    }
    const sourceCIDRs = awsLeaseSSHSourceCIDRs(config, context);
    const globalCIDRs = awsGlobalSSHSourceCIDRs(this.env);
    const nextLeaseWithSources = withLeaseSSHSourceCIDRs(
      lease,
      sourceCIDRs,
      sourceCIDRs.length > 0 || globalCIDRs.length > 0,
    );
    const configuredSecurityGroupID = awsConfiguredSecurityGroupID(config, this.env);
    const managedSecurityGroupName = `${awsManagedSecurityGroupName(config)}-${(
      await sha256Hex(`${lease.org}\0${lease.owner}`)
    ).slice(0, 12)}`;
    const nextLease: LeaseRecord = {
      ...nextLeaseWithSources,
      network: {
        ...nextLeaseWithSources.network,
        ...(config.awsSSHCIDRsPinned ? { sshPinnedSourceCIDRs: config.awsSSHCIDRs } : {}),
        ...(configuredSecurityGroupID
          ? { awsSecurityGroupID: configuredSecurityGroupID }
          : { awsSecurityGroupName: managedSecurityGroupName }),
        ...(config.awsSubnetID ? { awsSubnetID: config.awsSubnetID } : {}),
      },
    };
    const activeLeases = replaceProviderAccessState(context.activeLeases, nextLease);
    const targetKey = awsIngressAccessTargetKey(
      nextLease,
      nextLease.region || config.awsRegion,
      awsLeaseSSHPorts(nextLease),
      this.env,
    );
    const targetLeases = activeLeases.filter(
      (candidate) =>
        leaseOwnsAWSSSHAccess(candidate) &&
        awsIngressAccessTargetKey(
          candidate,
          candidate.region || this.region,
          awsLeaseSSHPorts(candidate),
          this.env,
        ) === targetKey,
    );
    return {
      config: {
        ...config,
        awsSGName: configuredSecurityGroupID ? "" : managedSecurityGroupName,
        awsSSHCIDRs: activeAWSSSHSourceCIDRs(targetLeases, [...sourceCIDRs, ...globalCIDRs]),
      },
      lease: nextLease,
      provisioning: {
        // Creates overlap outside the coordinator queue. Only serialized refreshes may prune.
        sshIngressReconcile: "additive",
        publishAccessBeforeProvisioning: true,
      },
    };
  }

  async refreshLeaseAccess(
    lease: LeaseRecord,
    context: ProviderAccessContext,
  ): Promise<LeaseRecord | void> {
    if (lease.network?.awsPrivate) return;
    if (lease.state !== "active") {
      return;
    }
    const sourceCIDRs = context.requestSourceCIDRs;
    const nextLease =
      sourceCIDRs.length > 0
        ? withLeaseSSHSourceCIDRs(
            lease,
            uniqueNonEmpty([...(lease.network?.sshPinnedSourceCIDRs ?? []), ...sourceCIDRs]),
            true,
          )
        : lease;
    const activeLeases = replaceProviderAccessState(context.activeLeases, nextLease);
    try {
      await this.reconcileLeaseAccess(nextLease, { ...context, activeLeases });
    } catch (error) {
      console.warn(
        `refresh AWS SSH ingress failed for ${lease.id}: ${coordinatorErrorMessage(this.env, error)}`,
      );
    }
    return nextLease;
  }

  async reconcileLeaseAccess(lease: LeaseRecord, context: ProviderAccessContext): Promise<void> {
    if (lease.network?.awsPrivate) return;
    const globalCIDRs = awsGlobalSSHSourceCIDRs(this.env);
    const accessLeases = context.activeLeases.filter(leaseOwnsAWSSSHAccess);
    const targets = new Map<string, { lease: LeaseRecord; port: string; region: string }>();
    const targetScopes = new Map<string, { identities: Set<string>; hasUnknownGroup: boolean }>();
    for (const candidate of [lease, ...accessLeases]) {
      const region = candidate.region || this.region;
      for (const port of awsLeaseSSHPorts(candidate)) {
        const key = awsIngressAccessTargetKey(candidate, region, [port], this.env);
        if (!targets.has(key)) {
          targets.set(key, { lease: candidate, port, region });
        }
        const scopeKey = awsIngressPortScopeKey(region, port);
        const scope = targetScopes.get(scopeKey) ?? {
          identities: new Set<string>(),
          hasUnknownGroup: false,
        };
        scope.identities.add(key);
        scope.hasUnknownGroup ||= awsIngressGroupMetadataUnknown(candidate, this.env);
        targetScopes.set(scopeKey, scope);
      }
    }
    const ambiguousTargetScopes = new Set(
      [...targetScopes]
        .filter(([, scope]) => scope.hasUnknownGroup && scope.identities.size > 1)
        .map(([scopeKey]) => scopeKey),
    );
    for (const [targetKey, target] of targets) {
      const targetLease = target.lease;
      const targetLeases = accessLeases.filter((candidate) => {
        const region = candidate.region || this.region;
        return (
          awsLeaseSSHPorts(candidate).includes(target.port) &&
          awsIngressAccessTargetKey(candidate, region, [target.port], this.env) === targetKey
        );
      });
      const cidrs = activeAWSSSHSourceCIDRs(targetLeases, globalCIDRs);
      const reconcile =
        ambiguousTargetScopes.has(awsIngressPortScopeKey(target.region, target.port)) ||
        hasUnknownActiveAWSSSHSource(targetLeases)
          ? "additive"
          : "authoritative";
      const config = {
        ...leaseConfig({
          provider: "aws",
          target: targetLease.target,
          windowsMode: targetLease.windowsMode ?? "normal",
          class: targetLease.class,
          serverType: targetLease.serverType,
          awsSSHCIDRs: cidrs,
          ...(targetLease.network?.awsSecurityGroupID
            ? { awsSGID: targetLease.network.awsSecurityGroupID }
            : {}),
          ...(targetLease.network?.awsSubnetID
            ? { awsSubnetID: targetLease.network.awsSubnetID }
            : {}),
          capacity: { market: targetLease.market === "spot" ? "spot" : "on-demand" },
          providerKey: targetLease.providerKey,
          sshUser: targetLease.sshUser,
          sshPort: target.port,
          sshFallbackPorts: [],
          sshPublicKey: "ssh-ed25519 ingress-reconcile",
          workRoot: targetLease.workRoot,
          ...(targetLease.hostId || targetLease.hostID
            ? { hostId: targetLease.hostId || targetLease.hostID }
            : {}),
        }),
        awsSGName: targetLease.network?.awsSecurityGroupName ?? "",
      };
      const { region } = target;
      const client = region === this.region ? this.client : new EC2SpotClient(this.env, region);
      // oxlint-disable-next-line eslint/no-await-in-loop -- each regional shared group is distinct.
      await client.refreshSSHIngress(
        { ...config, awsRegion: region },
        { reconcile, allowEmpty: true },
      );
    }
  }

  async createServerWithFallback(
    config: ReturnType<typeof leaseConfig>,
    leaseID: string,
    slug: string,
    owner: string,
    provisioning?: ProviderProvisioningContext,
  ): Promise<{
    server: ProviderMachine;
    serverType: string;
    market?: string;
    attempts?: ProvisioningAttempt[];
  }> {
    const regions = awsRegionCandidates(config, this.env, this.region);
    const failures: string[] = [];
    const regionAttempts: ProvisioningAttempt[] = [];
    const ingressOptions =
      provisioning?.sshIngressReconcile === undefined && !provisioning?.allowEmptySSHIngress
        ? undefined
        : {
            ...(provisioning?.sshIngressReconcile
              ? { reconcile: provisioning.sshIngressReconcile }
              : {}),
            ...(provisioning?.allowEmptySSHIngress ? { allowEmpty: true } : {}),
          };
    for (const region of regions) {
      const client = region === this.region ? this.client : new EC2SpotClient(this.env, region);
      try {
        // Record only regions whose provisioning path is about to mutate provider state.
        // oxlint-disable-next-line eslint/no-await-in-loop -- region fallback is intentionally ordered.
        await provisioning?.onTargetAttempt?.({ region });
        // oxlint-disable-next-line eslint/no-await-in-loop -- region fallback must preserve ordered capacity preference.
        const { server, serverType, market, attempts } = await client.createServerWithFallback(
          { ...config, awsRegion: region },
          leaseID,
          slug,
          owner,
          ingressOptions,
        );
        let readyServer: ProviderMachine;
        try {
          // oxlint-disable-next-line eslint/no-await-in-loop -- wait on the region that created the instance.
          readyServer = await client.waitForServerIP(server.cloudID, config.awsPrivate);
          if (config.awsRequireSSM) {
            // oxlint-disable-next-line eslint/no-await-in-loop -- private readiness belongs to the selected region.
            await client.waitForSSMOnline(server.cloudID);
            // oxlint-disable-next-line eslint/no-await-in-loop -- bootstrap must finish before the lease becomes active.
            const bootstrap = await client.runSSMBootstrap(
              server.cloudID,
              leaseID,
              config.awsSSMBootstrapCommand,
              config.awsSSMLogGroup,
            );
            readyServer = {
              ...readyServer,
              host: "",
              awsSSMCommandID: bootstrap.commandID,
              awsSSMCommandStatus: bootstrap.status,
            };
          }
        } catch (error) {
          const waitMessage = error instanceof Error ? error.message : String(error);
          try {
            if (config.awsPrivate) {
              // oxlint-disable-next-line eslint/no-await-in-loop -- clean up the exact instance before any fallback.
              await client.terminateServerAndWait(server.cloudID);
            } else {
              // oxlint-disable-next-line eslint/no-await-in-loop -- clean up the exact instance before any fallback.
              await client.deleteServer(server.cloudID);
            }
          } catch (deleteError) {
            const deleteMessage =
              deleteError instanceof Error ? deleteError.message : String(deleteError);
            if (!isAWSInstanceCleanedAfterReadinessFailure(waitMessage, deleteMessage)) {
              throw new ProviderProvisioningCleanupError(
                `${waitMessage}; cleanup failed for AWS instance ${server.cloudID}: ${deleteMessage}`,
                {
                  provider: "aws",
                  cloudID: server.cloudID,
                  region,
                  serverID: server.id,
                },
                deleteError,
              );
            }
          }
          throw new Error(
            `${waitMessage}; crabbox_aws_stale_instance_cleaned; deleted AWS instance ${server.cloudID} after readiness failure`,
            { cause: error },
          );
        }
        const result: {
          server: ProviderMachine;
          serverType: string;
          market?: string;
          attempts?: ProvisioningAttempt[];
        } = { server: { ...readyServer, region }, serverType };
        if (market) {
          result.market = market;
        }
        const allAttempts = [...regionAttempts, ...(attempts ?? [])];
        if (allAttempts.length > 0) {
          result.attempts = allAttempts;
        }
        return result;
      } catch (error) {
        if (providerProvisioningCleanupClaim(error)) throw error;
        const message = error instanceof Error ? error.message : String(error);
        regionAttempts.push({
          region,
          serverType: config.serverType,
          market: config.capacityMarket,
          category: awsProvisioningErrorCategory(message) || "region",
          message: `region ${region}: ${message}`,
        });
        failures.push(`${region}: ${message}`);
        if (!isRetryableAWSRegionProvisioningError(message)) {
          break;
        }
      }
    }
    throw new Error(failures.join("; "));
  }

  async deleteServer(id: string): Promise<void> {
    await this.client.deleteServer(id);
  }

  async finalizeLeaseCreate(
    config: ReturnType<typeof leaseConfig>,
    lease: LeaseRecord,
    server: ProviderMachine,
    attempts: ProvisioningAttempt[],
  ): Promise<ProviderLeaseCreateFinalization> {
    const nextConfig = server.region ? { ...config, awsRegion: server.region } : config;
    const nextLease: LeaseRecord = {
      ...lease,
      region: server.region ?? nextConfig.awsRegion,
      providerKeyCleanupOwned: lease.providerKey === providerKeyForLease(lease.id),
      ...(server.awsSSMCommandID ? { awsSSMCommandID: server.awsSSMCommandID } : {}),
      ...(server.awsSSMCommandStatus ? { awsSSMCommandStatus: server.awsSSMCommandStatus } : {}),
      ...(config.awsPrivate ? { awsSSMLogGroup: config.awsSSMLogGroup } : {}),
    };
    const hints = capacityHints(this.env, nextConfig, nextLease, attempts);
    if (hints.length > 0) {
      nextLease.capacityHints = hints;
    }
    return { config: nextConfig, lease: nextLease };
  }

  async releaseLease(lease: LeaseRecord): Promise<void> {
    const server = await ownedProviderMachineForRelease("aws", lease, (id) => this.findServer(id));
    try {
      if (server) {
        if (lease.network?.awsPrivate) {
          await this.client.terminateServerAndWait(lease.cloudID);
        } else {
          await this.deleteServer(lease.cloudID);
        }
      }
    } catch (error) {
      const message = coordinatorErrorMessage(this.env, error);
      if (!isAWSInstanceNotFoundError(message)) {
        throw error;
      }
      console.warn(
        `AWS lease cleanup found missing instance lease=${lease.id} cloud=${lease.cloudID}: ${message}`,
      );
    }
    if (lease.network?.awsPrivate) {
      privateAWSWorkspaceLifecycleLog("terminated", {
        lease_id: lease.id,
        cloud_id: lease.cloudID,
        region: lease.region,
      });
    }
    if (leaseUsesCanonicalProviderKey(lease)) {
      await this.deleteSSHKey(lease.providerKey, lease.id);
    }
  }

  supportsNativeImages(): boolean {
    return true;
  }

  nativeImagesUnsupportedMessage(): string {
    return "native images are supported for AWS, Azure, and GCP leases";
  }

  defaultImageStrategy(): "image" | "disk-snapshot" {
    return "image";
  }

  validateLeaseImageStrategy(): string | undefined {
    return undefined;
  }

  async createLeaseImage(
    lease: LeaseRecord,
    name: string,
    noReboot: boolean,
    strategy: "image" | "disk-snapshot",
  ): Promise<ProviderImage> {
    const image =
      strategy === "image"
        ? await this.client.createImage(lease.cloudID, name, noReboot)
        : await this.client.createDiskSnapshot(lease.cloudID, name);
    const enriched = enrichAWSImage(image, lease);
    await this.storage.put(createdAWSImageKey(enriched.id), enriched);
    await storeCreatedProviderImage(this.storage, "aws", enriched);
    return enriched;
  }

  getImage(imageID: string): Promise<ProviderImage> {
    return this.client.getImage(imageID);
  }

  enableFastSnapshotRestore(
    snapshotIDs: string[],
    availabilityZones: string[],
  ): Promise<ProviderFastSnapshotRestore[]> {
    return this.client.enableFastSnapshotRestore(snapshotIDs, availabilityZones);
  }

  fastSnapshotRestoreStatus(
    snapshotIDs: string[],
    availabilityZones?: string[],
  ): Promise<ProviderFastSnapshotRestore[]> {
    return this.client.fastSnapshotRestoreStatus(snapshotIDs, availabilityZones);
  }

  async deleteImage(imageID: string): Promise<void> {
    await this.client.deleteImage(imageID);
    const catalog = await this.storage.list<PromotedImageRecord>({ prefix: "image:aws:catalog:" });
    await Promise.all(
      [...catalog.entries()]
        .filter(([, image]) => image.id === imageID)
        .map(([key]) => this.storage.delete(key)),
    );
  }

  async storedImageMetadata(imageID: string): Promise<ProviderImage | undefined> {
    return (
      (await this.promotedImageByID(imageID)) ??
      (await this.storage.get<ProviderImage>(createdAWSImageKey(imageID))) ??
      (await this.storage.get<ProviderImage>(createdProviderImageKey("aws", imageID)))
    );
  }

  decorateImage(image: ProviderImage, metadata?: Partial<ProviderImage>): ProviderImage {
    return mergeAWSImageMetadata(image, metadata);
  }

  async validateDeleteImage(
    imageID: string,
    metadata?: Partial<ProviderImage>,
  ): Promise<{ status: number; body: Record<string, unknown> } | undefined> {
    if (metadata?.id === imageID && "promotedAt" in metadata) {
      return {
        status: 409,
        body: {
          error: "image_promoted",
          message: `image ${imageID} is the promoted AWS image; promote another image before deleting it`,
        },
      };
    }
    return undefined;
  }

  async fastSnapshotRestoreForImage(
    imageID: string,
    metadata: ProviderImage | undefined,
    url: URL,
  ): Promise<
    Response | { image: ProviderImage; fastSnapshotRestores: ProviderFastSnapshotRestore[] }
  > {
    const rawRegion = url.searchParams.get("region") ?? metadata?.region ?? "";
    const imageRegion = rawRegion ? sanitizeAWSRegion(rawRegion) : "";
    if (rawRegion && !imageRegion) {
      return json(
        { error: "invalid_region", message: "region must be an AWS region name" },
        { status: 400 },
      );
    }
    const region = imageRegion || this.region;
    const provider =
      region === this.region ? this : new AWSProvider(this.env, region, this.storage);
    const image = mergeAWSImageMetadata(await provider.getImage(imageID), metadata);
    const snapshots = image.snapshots ?? [];
    if (snapshots.length === 0) {
      return json(
        {
          error: "image_snapshots_missing",
          message: `image ${imageID} has no EBS snapshots to describe for Fast Snapshot Restore`,
        },
        { status: 409 },
      );
    }
    const availabilityZones = fastSnapshotRestoreStatusAZs(url, image.region ?? imageRegion);
    const fastSnapshotRestores = await provider.fastSnapshotRestoreStatus(
      snapshots,
      availabilityZones,
    );
    const imageWithStatus = { ...image, fastSnapshotRestores };
    return {
      image: imageWithStatus,
      fastSnapshotRestores: imageWithStatus.fastSnapshotRestores ?? [],
    };
  }

  async promoteImage(
    imageID: string,
    known: ProviderImage | undefined,
    request: Request,
    url: URL,
  ): Promise<Response | { image: ProviderImage }> {
    const input: {
      target?: string;
      os?: string;
      region?: string;
      serverType?: string;
      architecture?: string;
      capabilities?: ImageCapabilities;
      fastSnapshotRestore?: unknown;
      fastSnapshotRestoreAvailabilityZones?: string[];
    } = await readJson<{
      target?: string;
      os?: string;
      region?: string;
      serverType?: string;
      architecture?: string;
      capabilities?: ImageCapabilities;
      fastSnapshotRestore?: unknown;
      fastSnapshotRestoreAvailabilityZones?: string[];
    }>(request).catch(() => ({}));
    const requestedRegion = input.region ?? url.searchParams.get("region") ?? "";
    const cataloged = await this.promotedCatalogImageByID(imageID, requestedRegion);
    const priorCapabilities = known?.capabilities ?? cataloged?.capabilities;
    const prior =
      known || cataloged
        ? {
            ...cataloged,
            ...known,
            ...(priorCapabilities ? { capabilities: priorCapabilities } : {}),
          }
        : undefined;
    const target = normalizeAWSImageTarget(
      input.target ?? url.searchParams.get("target") ?? prior?.target ?? "linux",
    );
    if (!target) {
      return json(
        { error: "invalid_target", message: "target must be linux, macos, or windows" },
        { status: 400 },
      );
    }
    let imageOS: string | undefined;
    if (target === "linux") {
      const requestedOS = input.os ?? url.searchParams.get("os");
      const fallbackOS = prior ? (prior.os ?? "ubuntu:24.04") : defaultOSImage;
      try {
        imageOS = normalizeOSImage(requestedOS ?? fallbackOS);
      } catch (error) {
        return json(
          { error: "invalid_os", message: coordinatorErrorMessage(this.env, error) },
          { status: 400 },
        );
      }
    }
    const rawRegion = requestedRegion || prior?.region || "";
    const imageRegion = sanitizeAWSRegion(rawRegion);
    if (rawRegion && !imageRegion) {
      return json(
        { error: "invalid_region", message: "region must be an AWS region name" },
        { status: 400 },
      );
    }
    const metadata: Partial<ProviderImage> = { ...prior, target, region: imageRegion };
    const serverType = input.serverType ?? url.searchParams.get("serverType") ?? prior?.serverType;
    if (serverType) {
      metadata.serverType = serverType;
    }
    const architecture =
      input.architecture ?? url.searchParams.get("architecture") ?? prior?.architecture;
    if (architecture) {
      metadata.architecture = architecture;
    }
    let capabilities: ImageCapabilities | undefined;
    try {
      capabilities = normalizeImageCapabilities({
        ...prior?.capabilities,
        ...input.capabilities,
        osVersion:
          input.capabilities?.osVersion ??
          url.searchParams.get("osVersion") ??
          prior?.capabilities?.osVersion,
        sdks:
          input.capabilities?.sdks ??
          promotionVersionMap(url.searchParams.getAll("sdk")) ??
          prior?.capabilities?.sdks,
        runtimes:
          input.capabilities?.runtimes ??
          promotionVersionMap(url.searchParams.getAll("runtime")) ??
          prior?.capabilities?.runtimes,
        browser:
          input.capabilities?.browser ??
          (url.searchParams.has("browser")
            ? boolFromUnknown(url.searchParams.get("browser"))
            : undefined) ??
          prior?.capabilities?.browser,
        webview2:
          input.capabilities?.webview2 ??
          (url.searchParams.has("webview2")
            ? boolFromUnknown(url.searchParams.get("webview2"))
            : undefined) ??
          prior?.capabilities?.webview2,
        desktop:
          input.capabilities?.desktop ??
          (url.searchParams.has("desktop")
            ? boolFromUnknown(url.searchParams.get("desktop"))
            : undefined) ??
          prior?.capabilities?.desktop,
      });
    } catch (error) {
      return json(
        { error: "invalid_image_capabilities", message: coordinatorErrorMessage(this.env, error) },
        { status: 400 },
      );
    }
    const fastSnapshotRestore = boolFromUnknown(
      input.fastSnapshotRestore ?? url.searchParams.get("fastSnapshotRestore"),
    );
    const fastSnapshotRestoreAvailabilityZones = fastSnapshotRestore
      ? fastSnapshotRestoreAZs(
          input.fastSnapshotRestoreAvailabilityZones,
          url,
          imageRegion,
          this.env,
        )
      : [];
    if (fastSnapshotRestore && fastSnapshotRestoreAvailabilityZones.length === 0) {
      return json(
        {
          error: "invalid_fast_snapshot_restore_zones",
          message:
            "Fast Snapshot Restore promotion requires at least one availability zone via fsrAz, fastSnapshotRestoreAvailabilityZones, CRABBOX_AWS_FAST_SNAPSHOT_RESTORE_AZS, or CRABBOX_CAPACITY_AVAILABILITY_ZONES",
        },
        { status: 400 },
      );
    }
    const region = imageRegion || this.region;
    const provider =
      region === this.region ? this : new AWSProvider(this.env, region, this.storage);
    const image = mergeAWSImageMetadata(await provider.getImage(imageID), metadata);
    if (image.state !== "available") {
      return json(
        { error: "image_not_available", message: `image ${imageID} is ${image.state}` },
        { status: 409 },
      );
    }
    if (target === "macos" && !image.serverType) {
      return json(
        { error: "invalid_server_type", message: "macOS AWS image promotion requires serverType" },
        { status: 400 },
      );
    }
    if (fastSnapshotRestoreAvailabilityZones.length > 0 && (image.snapshots ?? []).length === 0) {
      return json(
        {
          error: "image_snapshots_missing",
          message: `image ${imageID} has no EBS snapshots to enable for Fast Snapshot Restore`,
        },
        { status: 409 },
      );
    }
    const fastSnapshotRestores =
      fastSnapshotRestoreAvailabilityZones.length > 0
        ? await provider.enableFastSnapshotRestore(
            image.snapshots ?? [],
            fastSnapshotRestoreAvailabilityZones,
          )
        : undefined;
    const promoted: PromotedImageRecord = {
      ...image,
      ...(fastSnapshotRestores ? { fastSnapshotRestores } : {}),
      target,
      ...(imageOS ? { os: imageOS } : {}),
      region: image.region ?? imageRegion,
      architecture:
        image.architecture ?? awsImageArchitectureForTarget(target, image.serverType ?? ""),
      promotedAt: new Date().toISOString(),
      ...(capabilities ? { capabilities } : {}),
    };
    await this.storage.put(promotedAWSImageKey(promoted), promoted);
    await this.storage.put(promotedAWSImageCatalogKey(promoted), promoted);
    if (target === "linux" && promoted.os) {
      await this.storage.put(promotedAWSLinuxOSImageKey(promoted), promoted);
    }
    if (
      target === "linux" &&
      (!promoted.os || promoted.os === "ubuntu:24.04") &&
      legacyPromotedAWSImageCompatible(promoted)
    ) {
      await this.storage.put(legacyPromotedAWSImageKey(), promoted);
    }
    return { image: promoted };
  }

  async deleteSSHKey(name: string, leaseID: string): Promise<void> {
    await this.client.deleteSSHKey(name, leaseID);
  }

  hourlyPriceUSD(
    serverType: string,
    config: ReturnType<typeof leaseConfig>,
  ): Promise<number | undefined> {
    // EC2 spot history is not an on-demand quote. Let cost accounting use an
    // explicit override or its conservative AWS fallback for on-demand leases.
    if (config.capacityMarket === "on-demand") return Promise.resolve(undefined);
    const region = config.awsRegion || this.region;
    const client = region === this.region ? this.client : new EC2SpotClient(this.env, region);
    return client.hourlySpotPriceUSD(serverType);
  }

  private async promotedImage(config: {
    target: TargetOS;
    architecture?: string;
    os?: string;
    serverType: string;
    awsRegion: string;
    imageRequirements: LeaseConfig["imageRequirements"];
  }): Promise<PromotedImageRecord | undefined> {
    const architecture = awsImageArchitectureForLease(
      config.target,
      config.serverType,
      config.architecture,
    );
    if (hasImageRequirements(config.imageRequirements)) {
      const imageScope = {
        target: config.target,
        ...(config.os ? { os: config.os } : {}),
        architecture,
        serverType: config.serverType,
        region: config.awsRegion,
      };
      const [selected, catalog] = await Promise.all([
        this.storage.get<PromotedImageRecord>(promotedAWSImageKey(imageScope)),
        this.storage.list<PromotedImageRecord>({
          prefix: promotedAWSImageCatalogPrefix(imageScope),
        }),
      ]);
      const candidates = new Map(
        [selected, ...catalog.values()]
          .filter((image): image is PromotedImageRecord => Boolean(image))
          .map((image) => [image.id, image]),
      );
      return [...candidates.values()]
        .filter((image) => imageSatisfiesRequirements(image.capabilities, config.imageRequirements))
        .toSorted(
          (left, right) =>
            right.promotedAt.localeCompare(left.promotedAt) || left.id.localeCompare(right.id),
        )[0];
    }
    const scoped = await this.storage.get<PromotedImageRecord>(
      promotedAWSImageKey({
        target: config.target,
        ...(config.os ? { os: config.os } : {}),
        architecture,
        serverType: config.serverType,
        region: config.awsRegion,
      }),
    );
    if (scoped) {
      return scoped;
    }
    if (config.target === "macos") {
      return this.storage.get<PromotedImageRecord>(
        legacyScopedPromotedAWSImageKey({
          target: config.target,
          architecture,
          region: config.awsRegion,
        }),
      );
    }
    if (config.target !== "linux") {
      return scoped;
    }
    if (config.os) {
      const osScoped = await this.storage.get<PromotedImageRecord>(
        promotedAWSLinuxOSImageKey({
          os: config.os,
          architecture,
        }),
      );
      if (osScoped) {
        return osScoped;
      }
    }
    if ((!config.os || config.os === "ubuntu:24.04") && architecture === "x86_64") {
      const legacy = await this.storage.get<PromotedImageRecord>(legacyPromotedAWSImageKey());
      if (legacy && legacyPromotedAWSImageCompatible(legacy)) {
        return legacy;
      }
    }
    return undefined;
  }

  private async promotedImagesForFallback(config: LeaseConfig): Promise<Record<string, string>> {
    const out: Record<string, string> = {};
    for (const region of awsRegionCandidates(config, this.env, config.awsRegion)) {
      for (const serverType of awsLaunchCandidates(config)) {
        // oxlint-disable-next-line eslint/no-await-in-loop -- storage reads preserve deterministic fallback key construction.
        const promoted = await this.promotedImage({
          target: config.target,
          architecture: config.architecture,
          os: config.os,
          serverType,
          awsRegion: region,
          imageRequirements: config.imageRequirements,
        });
        if (promoted?.id) {
          out[awsPromotedAMIConfigKey(region, serverType)] = promoted.id;
        }
      }
    }
    return out;
  }

  private async promotedImageByID(imageID: string): Promise<PromotedImageRecord | undefined> {
    const promoted = await this.storage.list<PromotedImageRecord>({
      prefix: promotedAWSImagePrefix(),
    });
    return [...promoted.values()].find((image) => image.id === imageID);
  }

  private async promotedCatalogImageByID(
    imageID: string,
    preferredRegion: string,
  ): Promise<PromotedImageRecord | undefined> {
    const catalog = await this.storage.list<PromotedImageRecord>({ prefix: "image:aws:catalog:" });
    const matches = [...catalog.values()].filter((image) => image.id === imageID);
    return matches.find((image) => image.region === preferredRegion) ?? matches[0];
  }
}

function isRetryableAWSRegionProvisioningError(message: string): boolean {
  return (
    isRetryableAWSProvisioningError(message) ||
    message.includes("quota ") ||
    message.includes("capacity")
  );
}

function redactReadyPoolEntry(entry: ReadyPoolEntry): ReadyPoolEntry {
  const { borrowToken: _borrowToken, ...redacted } = entry;
  void _borrowToken;
  return redacted;
}
