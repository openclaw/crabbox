import { artifactUploadResponse, type ArtifactUploadRequest } from "./artifacts";
import { isAdminRequest } from "./auth";
import {
  EC2SpotClient,
  awsLaunchCandidates,
  awsProvisioningErrorCategory,
  awsRegionCandidates,
  isAWSInstanceCleanedAfterReadinessFailure,
  isRetryableAWSProvisioningError,
  type AWSMacHost,
} from "./aws";
import { AzureClient, azureRegionCandidates, type AzureDeferredCleanupRequest } from "./azure";
import {
  awsPromotedAMIConfigKey,
  azureLocationFor,
  leaseConfig,
  validCIDRs,
  type LeaseConfig,
} from "./config";
import { GCPClient } from "./gcp";
import { HetznerClient } from "./hetzner";
import { errorMessage, json, pathParts, readJson, requestOwner } from "./http";
import { githubAuthRoute, githubPortalLogin, githubPortalLogout } from "./oauth";
import { defaultOSImage, normalizeOSImage } from "./os-image";
import {
  portalCode,
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
import { leaseSlugFromID, normalizeLeaseSlug, slugWithCollisionSuffix } from "./slug";
import {
  createTailscaleAuthKey,
  renderTailscaleHostname,
  tailscaleAllowed,
  tailscaleDefaultTags,
  validateTailscaleTags,
} from "./tailscale";
import type {
  CapacityHint,
  Env,
  ExternalRunnerInput,
  ExternalRunnerRecord,
  ExternalRunnerSyncRequest,
  LeaseRecord,
  LeaseRequest,
  LeaseShare,
  LeaseShareRole,
  LeaseTelemetry,
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
import { coordinatorProviders, coordinatorProviderSpec, isCoordinatorProvider } from "./types";
import { costLimits, enforceCostLimits, leaseCost, requestOrg, usageSummary } from "./usage";

const fleetID = "default";
const maxStoredRunLogBytes = 8 * 1024 * 1024;
const runLogChunkBytes = 64 * 1024;
const maxLeaseTelemetryHistory = 60;
const maxRunTelemetrySamples = 60;
const maxExternalRunnerSyncItems = 200;
const webVNCTicketTTLSeconds = 120;
const codeTicketTTLSeconds = 120;
const egressTicketTTLSeconds = 120;
const leaseCleanupRetryDelayMs = 5 * 60 * 1000;
const awsOrphanSweepInitialDelayMs = 60 * 1000;
const azureOrphanSweepInitialDelayMs = 60 * 1000;
const defaultAWSOrphanSweepIntervalSeconds = 60 * 60;
const defaultAWSOrphanSweepGraceSeconds = 15 * 60;
const defaultAzureOrphanSweepIntervalSeconds = 60 * 60;
const defaultAzureOrphanSweepGraceSeconds = 15 * 60;
const providerAccessReservationTTLMS = 15 * 60 * 1000;
const maxPendingWebVNCBytes = 1024 * 1024;
const maxCodeRequestBytes = 10 * 1024 * 1024;
const maxCodeWebSocketFrameChunkBytes = 15 * 1024;
const textEncoder = new TextEncoder();
const textDecoder = new TextDecoder();
const awsOrphanSweepRecordKey = "aws-orphan-sweep:last";
const awsOrphanSweepFirstAlarmKey = "aws-orphan-sweep:first-alarm";
const azureOrphanSweepRecordKey = "azure-orphan-sweep:last";
const azureOrphanSweepFirstAlarmKey = "azure-orphan-sweep:first-alarm";
const azureDeferredCleanupPrefix = "azure-cleanup:";
const readyPoolPrefix = "ready-pool:";

interface WebVNCTicketRecord {
  ticket: string;
  leaseID: string;
  owner: string;
  org: string;
  createdAt: string;
  expiresAt: string;
}

interface CodeTicketRecord {
  ticket: string;
  leaseID: string;
  owner: string;
  org: string;
  createdAt: string;
  expiresAt: string;
}

type EgressRole = "host" | "client";

interface EgressTicketRecord {
  ticket: string;
  leaseID: string;
  owner: string;
  org: string;
  role: EgressRole;
  sessionID: string;
  profile?: string;
  allow?: string[];
  createdAt: string;
  expiresAt: string;
}

interface EgressSessionStatus {
  leaseID: string;
  sessionID: string;
  profile?: string;
  allow: string[];
  createdAt: string;
  updatedAt: string;
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
  id: string;
  frame: "text" | "binary";
  chunks: string[];
}

interface LeaseCloudAudit {
  leaseID: string;
  slug?: string;
  provider: Provider;
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
}

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
  | { kind: "webvnc-agent"; leaseID: string; id: string; capabilities: Set<string> }
  | {
      kind: "webvnc-viewer";
      leaseID: string;
      id: string;
      agentID: string;
      owner: string;
      label: string;
    }
  | { kind: "code-agent"; leaseID: string }
  | { kind: "code-viewer"; leaseID: string; id: string }
  | { kind: "egress-host"; leaseID: string; sessionID: string }
  | { kind: "egress-client"; leaseID: string; sessionID: string }
  | {
      kind: "control";
      clientID: string;
      owner: string;
      org: string;
      admin?: boolean;
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

interface WebVNCViewerSession {
  id: string;
  agentID: string;
  socket: WebSocket;
  owner: string;
  label: string;
  connectedAt: string;
}

export class FleetDurableObject implements DurableObject {
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
  private readonly controlSockets = new Map<string, WebSocket>();
  private readyPoolBorrowQueue: Promise<void> = Promise.resolve();

  constructor(
    private readonly state: DurableObjectState,
    private readonly env: Env,
    private readonly testProviders: Partial<Record<Provider, CloudProvider>> = {},
  ) {
    this.restoreBridgeWebSockets();
  }

  async fetch(request: Request): Promise<Response> {
    try {
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
        return await githubAuthRoute(request, parts[3], this.state.storage, this.env);
      }
      if (method === "GET" && parts.join("/") === "portal/login") {
        return await githubPortalLogin(request, this.state.storage, this.env);
      }
      if (method === "GET" && parts.join("/") === "portal/logout") {
        return githubPortalLogout();
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
      return json({ error: errorMessage(error) }, { status: 500 });
    }
  }

  async webSocketMessage(socket: WebSocket, message: string | ArrayBuffer): Promise<void> {
    const attachment = bridgeAttachment(socket);
    if (!attachment) {
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
    if (typeof this.state.getWebSockets !== "function") {
      return;
    }
    for (const socket of this.state.getWebSockets()) {
      const attachment = bridgeAttachment(socket);
      if (!attachment || socket.readyState !== WebSocket.OPEN) {
        continue;
      }
      this.trackBridgeSocket(socket, attachment);
    }
  }

  private acceptBridgeWebSocket(socket: WebSocket, attachment: BridgeAttachment): void {
    if (typeof this.state.acceptWebSocket === "function") {
      this.state.acceptWebSocket(socket, bridgeTags(attachment));
      socket.serializeAttachment(attachment);
    } else {
      socket.accept();
      socket.addEventListener("message", (event) => {
        void this.handleBridgeMessage(socket, attachment, event.data);
      });
      socket.addEventListener("close", (event) => {
        this.handleBridgeClose(socket, event.code, event.reason);
      });
      socket.addEventListener("error", () => {
        this.handleBridgeClose(socket, 1011, "bridge socket error");
      });
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
      this.clearEgressSessionSockets(
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
    const pair = new WebSocketPair();
    const client = pair[0];
    const server = pair[1];
    const attachment: BridgeAttachment = {
      kind: "control",
      clientID: crypto.randomUUID(),
      owner: requestOwner(request),
      org: requestOrg(request, this.env),
      admin: isAdminRequest(request),
      subscriptions: {},
    };
    this.controlSockets.set(attachment.clientID, server);
    this.acceptBridgeWebSocket(server, attachment);
    sendControl(server, {
      type: "hello",
      protocol: 1,
      clientID: attachment.clientID,
    });
    return new Response(null, { status: 101, webSocket: client });
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
    if (!run || !this.runVisibleToControl(run, attachment)) {
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
    const heartbeat: { idleTimeoutSeconds?: number; telemetry?: Partial<LeaseTelemetry> } = {};
    if (input.idleTimeoutSeconds !== undefined) {
      heartbeat.idleTimeoutSeconds = input.idleTimeoutSeconds;
    }
    if (input.telemetry !== undefined) {
      heartbeat.telemetry = input.telemetry;
    }
    await this.applyLeaseHeartbeat(lease, heartbeat);
    sendControl(socket, {
      type: "heartbeat",
      leaseID: lease.id,
      ok: true,
      expiresAt: lease.expiresAt,
    });
  }

  private serializeBridgeAttachment(socket: WebSocket, attachment: BridgeAttachment): void {
    if (typeof socket.serializeAttachment === "function") {
      socket.serializeAttachment(attachment);
    }
  }

  private async handleBridgeMessage(
    socket: WebSocket,
    attachment: BridgeAttachment,
    message: string | ArrayBuffer | Blob,
  ): Promise<void> {
    switch (attachment.kind) {
      case "webvnc-agent":
        await forwardOrBufferWebVNC(
          message,
          this.webVNCViewerForAgent(attachment.leaseID, attachment.id)?.socket,
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
          this.webVNCAgents.get(attachment.leaseID)?.get(attachment.agentID),
        );
        break;
      case "code-agent":
        await this.handleCodeAgentMessage(attachment.leaseID, message);
        break;
      case "code-viewer": {
        const agent = this.codeAgents.get(attachment.leaseID);
        if (agent?.readyState !== WebSocket.OPEN) {
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
          this.egressClients.get(egressSocketKey(attachment.leaseID, attachment.sessionID)),
        );
        break;
      case "egress-client":
        await forwardEgress(
          message,
          this.egressHosts.get(egressSocketKey(attachment.leaseID, attachment.sessionID)),
        );
        break;
      case "control":
        await this.handleControlMessage(socket, attachment, message);
        break;
    }
    void socket;
  }

  private handleBridgeClose(socket: WebSocket, code: number, reason: string): void {
    const attachment = bridgeAttachment(socket);
    if (!attachment) {
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
      case "control":
        if (this.controlSockets.get(attachment.clientID) === socket) {
          this.controlSockets.delete(attachment.clientID);
        }
        break;
    }
  }

  async alarm(): Promise<void> {
    await this.runMaintenance();
  }

  private async scheduledMaintenance(request: Request): Promise<Response> {
    if (request.headers.get("x-crabbox-internal") !== "scheduled") {
      return json({ error: "unauthorized" }, { status: 401 });
    }
    await this.runMaintenance();
    return json({ ok: true });
  }

  private async runMaintenance(): Promise<void> {
    await this.expireLeases();
    await this.runAzureDeferredCleanups();
    await this.runAWSOrphanSweepIfDue("alarm");
    await this.runAzureOrphanSweepIfDue("alarm");
    await this.scheduleAlarm();
  }

  private async createLease(request: Request): Promise<Response> {
    const owner = requestOwner(request);
    const org = requestOrg(request, this.env);
    const input = await readJson<LeaseRequest>(request);
    let config = leaseConfig(
      input,
      this.env.CRABBOX_AZURE_OS_DISK ? { azureOSDisk: this.env.CRABBOX_AZURE_OS_DISK } : undefined,
    );
    if (!isAdminRequest(request) && hasNativeLeaseSource(config)) {
      return json(
        {
          error: "admin_required",
          message: "native snapshot/image lease sources require admin-token auth",
        },
        { status: 403 },
      );
    }
    config =
      (await this.provider(
        config.provider,
        providerRegionForConfig(config),
        providerProjectForConfig(config),
      ).prepareLeaseConfig?.(config)) ?? config;
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
    const leaseID = validLeaseID(input.leaseID) ? input.leaseID : newLeaseID();
    const leases = await this.leaseRecords();
    const slug = allocateLeaseSlug(
      normalizeLeaseSlug(input.slug ?? input.requestedSlug) || leaseSlugFromID(leaseID),
      leaseID,
      owner,
      org,
      leases,
    );
    const providerAccessLeases = await this.providerAccessRecords();
    const tailscaleError = await this.prepareTailscaleConfig(config, input, leaseID, slug);
    if (tailscaleError) {
      return tailscaleError;
    }
    const provider = this.provider(
      config.provider,
      providerRegionForConfig(config),
      providerProjectForConfig(config),
    );
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
    const now = new Date();
    const providerProject = providerProjectForConfig(config);
    const providerRegion = ["aws", "azure", "gcp"].includes(config.provider)
      ? providerRegionForConfig(config)
      : undefined;
    let record: LeaseRecord = {
      id: leaseID,
      slug,
      provider: config.provider,
      target: config.target,
      os: config.os,
      desktop: config.desktop,
      desktopEnv: config.desktopEnv,
      browser: config.browser,
      code: config.code,
      cloudID: "",
      owner,
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
    const requestedHostID = config.hostID || config.awsMacHostID;
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
        hostname: config.tailscaleHostname,
        tags: config.tailscaleTags,
        state: "requested",
      };
      if (config.tailscaleExitNode) {
        record.tailscale.exitNode = config.tailscaleExitNode;
        record.tailscale.exitNodeAllowLanAccess = config.tailscaleExitNodeAllowLanAccess;
      }
    }
    const limitError = enforceCostLimits(
      [...leases, ...providerAccessLeases],
      record,
      costLimits(this.env),
      now,
    );
    if (limitError) {
      return json({ error: "cost_limit_exceeded", message: limitError }, { status: 429 });
    }
    const accessContext = providerAccessContext(requestSourceCIDRs(request), [
      ...leases,
      ...providerAccessLeases,
    ]);
    const prepared = await provider.prepareLeaseCreate?.(config, record, accessContext);
    if (prepared) {
      config = prepared.config;
      record = prepared.lease;
    }
    if (prepared?.provisioning?.publishAccessBeforeProvisioning) {
      await this.putProviderAccess(providerAccessReservation(record, now));
    }
    await this.putLease(record);
    await this.scheduleAlarm();
    const { server, serverType, market, attempts } = await provider
      .createServerWithFallback(config, leaseID, slug, owner, prepared?.provisioning)
      .catch(async (error: unknown) => {
        if (prepared?.provisioning?.publishAccessBeforeProvisioning) {
          await this.deleteProviderAccess(record.id);
        }
        const current = await this.getLease(record.id);
        if (!current || current.state !== "provisioning") {
          await this.scheduleAlarm();
          throw error;
        }
        const failedAt = new Date().toISOString();
        record = { ...current };
        record.state = "failed";
        record.updatedAt = failedAt;
        record.endedAt = failedAt;
        record.cleanupFailedAt = failedAt;
        record.cleanupError = errorMessage(error);
        await this.putLease(record);
        await this.scheduleAlarm();
        throw error;
      });
    let current = await this.getLease(record.id);
    if (!current || current.state !== "provisioning") {
      return this.abortProvisionedLeaseAfterStateChange(
        current,
        record,
        config,
        server,
        serverType,
        Boolean(prepared?.provisioning?.publishAccessBeforeProvisioning),
      );
    }
    record = { ...current };
    record.state = "active";
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
    current = await this.getLease(record.id);
    if (!current || current.state !== "provisioning") {
      return this.abortProvisionedLeaseAfterStateChange(
        current,
        record,
        config,
        server,
        serverType,
        Boolean(prepared?.provisioning?.publishAccessBeforeProvisioning),
      );
    }
    record = { ...current, ...record };
    await this.putLease(record);
    if (prepared?.provisioning?.publishAccessBeforeProvisioning) {
      await this.deleteProviderAccess(record.id);
    }
    await this.scheduleAlarm();
    return json({ lease: record }, { status: 201 });
  }

  private async leaseRoute(request: Request, leaseID: string, action?: string): Promise<Response> {
    const method = request.method.toUpperCase();
    if (method === "GET" && action === undefined) {
      const lease = await this.resolveLease(leaseID, request, false);
      return lease ? json({ lease }) : notFound();
    }
    if (method === "POST" && action === "heartbeat") {
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
      const body = await optionalJson<{
        idleTimeoutSeconds?: number;
        telemetry?: Partial<LeaseTelemetry>;
      }>(request);
      const updatedLease = await this.applyLeaseHeartbeat(lease, body, request);
      return json({ lease: updatedLease });
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
      return json({ lease });
    }
    if (method === "POST" && action === "release") {
      return this.releaseLease(request, leaseID, false);
    }
    if (action === "share") {
      return await this.shareLeaseRoute(request, leaseID);
    }
    return json({ error: "not_found" }, { status: 404 });
  }

  private async applyLeaseHeartbeat(
    lease: LeaseRecord,
    input: {
      idleTimeoutSeconds?: number;
      telemetry?: Partial<LeaseTelemetry>;
    },
    request?: Request,
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
    const requestCIDRs = request ? requestSourceCIDRs(request) : [];
    if (requestCIDRs.length > 0) {
      const provider = this.provider(lease.provider, lease.region, lease.providerProject);
      const leases = await this.leaseRecords();
      const accessContext = providerAccessContext(
        requestCIDRs,
        replaceProviderAccessState([...leases, ...(await this.providerAccessRecords())], lease),
      );
      const updatedLease = await provider.refreshLeaseAccess?.(lease, accessContext);
      if (updatedLease) {
        lease = updatedLease;
      }
    }
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
      config.tailscaleAuthKey = await createTailscaleAuthKey(this.env, {
        hostname: config.tailscaleHostname,
        tags: config.tailscaleTags,
        description: `crabbox ${leaseID} ${slug}`,
      });
    } catch (error) {
      const message = errorMessage(error);
      if (message.includes("tags not allowed") || message.includes("requires at least one")) {
        return json({ error: "invalid_tailscale_tags", message }, { status: 400 });
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
    const body = await optionalJson<{ delete?: boolean }>(request);
    const shouldDelete = body.delete ?? !lease.keep;
    return json({ lease: await this.releaseResolvedLease(lease, { deleteServer: shouldDelete }) });
  }

  private async shareLeaseRoute(request: Request, leaseID: string): Promise<Response> {
    const method = request.method.toUpperCase();
    const lease = await this.resolveLease(leaseID, request, isAdminRequest(request));
    if (!lease) {
      return notFound();
    }
    if (method === "GET") {
      return json({ leaseID: lease.id, share: normalizedLeaseShare(lease.share) });
    }
    if (!this.leaseManageableByRequest(lease, request, isAdminRequest(request))) {
      return json({ error: "forbidden", message: "lease manage access required" }, { status: 403 });
    }
    if (method === "PUT") {
      const input = await readJson<Partial<LeaseShare>>(request);
      lease.share = sanitizeLeaseShare(input, requestOwner(request));
      lease.updatedAt = new Date().toISOString();
      await this.putLease(lease);
      return json({ leaseID: lease.id, share: normalizedLeaseShare(lease.share) });
    }
    if (method === "DELETE") {
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
      await this.putLease(lease);
      return json({ leaseID: lease.id, share: normalizedLeaseShare(lease.share) });
    }
    return json({ error: "not_found" }, { status: 404 });
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
      org: requestOrg(request, this.env),
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
      leaseRequest.awsRegion = region;
    }
    const config = leaseConfig(leaseRequest);
    return await new EC2SpotClient(this.env, config.awsRegion).capacityReadinessChecks(config);
  }

  private async portalRoute(request: Request, parts: string[]): Promise<Response> {
    const method = request.method.toUpperCase();
    if (method === "GET" && parts.length === 1) {
      const [leases, runners, macHosts] = await Promise.all([
        this.portalLeases(request),
        this.visibleExternalRunners(request),
        this.portalMacHosts(),
      ]);
      return portalHome(leases, runners, request, this.attachPortalMacHostLeases(macHosts, leases));
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
      return portalVNC(lease, {
        canManage: this.leaseManageableByRequest(lease, request, isAdminRequest(request)),
      });
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
    const leases = await this.leaseRecords();
    return isAdminRequest(request)
      ? this.filterLeases(leases, request)
      : this.filterLeasesForRequest(leases, request);
  }

  private async portalMacHosts(): Promise<PortalMacHostRecord[]> {
    if (!this.env.AWS_ACCESS_KEY_ID || !this.env.AWS_SECRET_ACCESS_KEY) {
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
      console.warn(`portal mac host inventory unavailable: ${errorMessage(error)}`);
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

  private async resolvePortalMacHost(
    request: Request,
    provider: string,
    hostID: string,
  ): Promise<PortalMacHostRecord | undefined> {
    if (provider !== "aws") {
      return undefined;
    }
    const [leases, hosts] = await Promise.all([this.portalLeases(request), this.portalMacHosts()]);
    return this.attachPortalMacHostLeases(hosts, leases).find(
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
      providerSet.add(lease.provider);
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
    const readiness = this.providerConfigurationReadiness(provider);
    const providerLeases = leases.filter((lease) => lease.provider === provider);
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
        error: errorMessage(error),
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
        state: lease.state,
        target: lease.target || "linux",
        owner: lease.owner,
        org: lease.org,
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
    return portalMacHostDetail(host, host.lease ? this.leaseBridgeStatus(host.lease) : undefined);
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
    if (!lease) {
      if (action === "vnc") {
        return portalMacHostDetail(host, undefined);
      }
      return portalError(
        "Code unavailable",
        "No active Crabbox lease is attached to that dedicated host.",
        409,
      );
    }
    const error = action === "vnc" ? webVNCLeaseError(lease) : codeLeaseError(lease);
    if (action === "vnc" && error === "lease was not created with desktop=true") {
      return portalMacHostDetail(host, this.leaseBridgeStatus(lease));
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
    if (!this.leaseManageableByRequest(lease, request, isAdminRequest(request))) {
      return portalError("WebVNC unavailable", "Lease manage access is required.", 403);
    }
    if (lease.target !== "macos") {
      return portalError("WebVNC unavailable", "Only macOS host leases can be enabled here.", 409);
    }
    lease.desktop = true;
    lease.updatedAt = new Date().toISOString();
    await this.putLease(lease);
    return new Response(null, {
      status: 303,
      headers: { location: `/portal/leases/${encodeURIComponent(lease.id)}/vnc` },
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
    const runs = (await this.runRecords())
      .filter((run) => run.leaseID === lease.id && this.runVisibleToRequest(run, request))
      .toSorted((a, b) => b.startedAt.localeCompare(a.startedAt))
      .slice(0, 12);
    return portalLeaseDetail(lease, runs, this.leaseBridgeStatus(lease), {
      canManage: this.leaseManageableByRequest(lease, request, isAdminRequest(request)),
    });
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
    await this.releaseResolvedLease(lease, { deleteServer: true, keep: false });
    return new Response(null, {
      status: 303,
      headers: { location: portalReturnLocation(request) },
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
        org: lease.org,
        share: normalizedLeaseShare(lease.share),
      });
    }
    const embedded = url.searchParams.get("embed") === "1";
    return portalShareLease(lease, { embedded });
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
      lease.share = sanitizeLeaseShare(input, requestOwner(request));
      lease.updatedAt = new Date().toISOString();
      await this.putLease(lease);
      return json({
        leaseID: lease.id,
        slug: lease.slug || lease.id,
        owner: lease.owner,
        org: lease.org,
        share: normalizedLeaseShare(lease.share),
      });
    }
    const form = await request.formData();
    const action = String(form.get("action") || "");
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
    await this.putLease(lease);
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
    if (!run || !this.runVisibleToRequest(run, request)) {
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
      return portalRunDetail(run, events, tailString(log, 12 * 1024));
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
    const ticket = await this.consumeWebVNCTicket(request);
    if (!ticket) {
      return json(
        { error: "webvnc_ticket_required", message: "valid WebVNC bridge ticket required" },
        { status: 401 },
      );
    }
    const lease = await this.getLease(ticket.leaseID);
    if (!lease || !identifierMatchesLease(identifier, lease)) {
      return notFound();
    }
    const error = webVNCLeaseError(lease);
    if (error) {
      return json({ error: "webvnc_unavailable", message: error }, { status: 409 });
    }
    const pair = new WebSocketPair();
    const client = pair[0];
    const agent = pair[1];

    const agentID = newWebVNCSessionID("agent");
    const capabilities = webVNCAgentCapabilities(request);
    this.trackWebVNCAgent(lease.id, agentID, agent, capabilities);
    this.recordWebVNCEvent(lease.id, "bridge_connected");
    this.acceptBridgeWebSocket(agent, {
      kind: "webvnc-agent",
      leaseID: lease.id,
      id: agentID,
      capabilities,
    });
    return new Response(null, { status: 101, webSocket: client });
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
    const ticket = await this.consumeEgressTicket(request);
    if (!ticket || ticket.role !== role) {
      return json(
        { error: "egress_ticket_required", message: "valid egress bridge ticket required" },
        { status: 401 },
      );
    }
    const lease = await this.getLease(ticket.leaseID);
    if (!lease || !identifierMatchesLease(identifier, lease)) {
      return notFound();
    }
    if (lease.state !== "active") {
      return json({ error: "egress_unavailable", message: "lease is not active" }, { status: 409 });
    }
    const pair = new WebSocketPair();
    const client = pair[0];
    const agent = pair[1];
    const attachment: BridgeAttachment = {
      kind: role === "host" ? "egress-host" : "egress-client",
      leaseID: lease.id,
      sessionID: ticket.sessionID,
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
    return new Response(null, { status: 101, webSocket: client });
  }

  private async egressStatus(request: Request, identifier: string): Promise<Response> {
    const lease = await this.resolveLease(identifier, request, false);
    if (!lease) {
      return notFound();
    }
    const session = this.egressSessions.get(lease.id);
    const key = session ? egressSocketKey(lease.id, session.sessionID) : undefined;
    const host = key ? this.egressHosts.get(key) : undefined;
    const client = key ? this.egressClients.get(key) : undefined;
    return json({
      leaseID: lease.id,
      slug: lease.slug,
      sessionID: session?.sessionID ?? "",
      profile: session?.profile ?? "",
      allow: session?.allow ?? [],
      hostConnected: host?.readyState === WebSocket.OPEN,
      clientConnected: client?.readyState === WebSocket.OPEN,
      createdAt: session?.createdAt ?? "",
      updatedAt: session?.updatedAt ?? "",
    });
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
    await this.cleanupExpiredWebVNCTickets();
    const now = new Date();
    const ticket: WebVNCTicketRecord = {
      ticket: newWebVNCTicket(),
      leaseID: lease.id,
      owner: requestOwner(request),
      org: requestOrg(request, this.env),
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
    const command = webVNCBridgeCommand(lease);
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
        : `WebVNC daemon not running; run: ${command}`,
    });
  }

  private async webVNCReset(request: Request, identifier: string): Promise<Response> {
    if (request.method.toUpperCase() !== "POST") {
      return json({ error: "not_found" }, { status: 404 });
    }
    const lease = await this.resolveLease(identifier, request, false);
    if (!lease) {
      return notFound();
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
    const agentSocket = this.webVNCAgents.get(lease.id)?.get(viewer.agentID);
    if (!agentSocket || agentSocket.readyState !== WebSocket.OPEN) {
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
    await this.cleanupExpiredCodeTickets();
    const now = new Date();
    const ticket: CodeTicketRecord = {
      ticket: newCodeTicket(),
      leaseID: lease.id,
      owner: requestOwner(request),
      org: requestOrg(request, this.env),
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
    const ticket = await this.consumeCodeTicket(request);
    if (!ticket) {
      return json(
        { error: "code_ticket_required", message: "valid code bridge ticket required" },
        { status: 401 },
      );
    }
    const lease = await this.getLease(ticket.leaseID);
    if (!lease || !identifierMatchesLease(identifier, lease)) {
      return notFound();
    }
    const error = codeLeaseError(lease);
    if (error) {
      return json({ error: "code_unavailable", message: error }, { status: 409 });
    }
    const pair = new WebSocketPair();
    const client = pair[0];
    const agent = pair[1];

    closeSocket(this.codeAgents.get(lease.id), 1012, "replaced by a newer code bridge");
    this.clearCodeLease(lease.id);
    this.codeAgents.set(lease.id, agent);
    this.acceptBridgeWebSocket(agent, { kind: "code-agent", leaseID: lease.id });
    return new Response(null, { status: 101, webSocket: client });
  }

  private async codePortalProxy(
    request: Request,
    identifier: string,
    _rest: string[],
  ): Promise<Response> {
    const lease = await this.resolvePortalLease(identifier, request);
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
    const agent = this.codeAgents.get(lease.id);
    if (request.method.toUpperCase() === "GET" && _rest.length === 1 && _rest[0] === "health") {
      return this.codePortalHealth(lease, agent);
    }
    if (!agent || agent.readyState !== WebSocket.OPEN) {
      return portalCode(lease);
    }
    if (request.headers.get("upgrade")?.toLowerCase() === "websocket") {
      return this.codeViewerWebSocket(request, lease, agent);
    }
    return await this.codeProxyHTTP(request, lease, agent);
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
    if (bodyBytes.byteLength > maxCodeRequestBytes) {
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
      this.pendingCodeRequests.set(id, { resolve, timeout, chunks: [] });
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
      headers: codeResponseHeaders(response.headers ?? {}),
    });
  }

  private codeViewerWebSocket(request: Request, lease: LeaseRecord, agent: WebSocket): Response {
    const pair = new WebSocketPair();
    const client = pair[0];
    const viewer = pair[1];
    const id = crypto.randomUUID();
    this.codeViewers.set(id, viewer);
    this.acceptBridgeWebSocket(viewer, { kind: "code-viewer", leaseID: lease.id, id });
    const url = new URL(request.url);
    const open: CodeWebSocketOpen = {
      type: "ws_open",
      id,
      path: `${url.pathname}${url.search}`,
      headers: codeForwardHeaders(request.headers),
    };
    agent.send(JSON.stringify(open));
    return new Response(null, { status: 101, webSocket: client });
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

  private sendCodeDataToViewer(message: CodeWebSocketData): void {
    const viewer = this.codeViewers.get(message.id);
    if (viewer?.readyState !== WebSocket.OPEN) {
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
      this.sendCodeDataToViewer(message as CodeWebSocketData);
      return;
    }
    if (message.type === "ws_start" && message.id) {
      const start = message as CodeWebSocketFrameStart;
      this.pendingCodeFrames.set(start.chunkID, {
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
        this.sendCodeDataToViewer({
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

  private clearCodeLease(_leaseID: string): void {
    for (const [id, viewer] of this.codeViewers) {
      this.codeViewers.delete(id);
      closeSocket(viewer, 1011, "code bridge disconnected");
    }
    for (const [id, pending] of this.pendingCodeRequests) {
      clearTimeout(pending.timeout);
      this.pendingCodeRequests.delete(id);
      pending.resolve({ type: "http", id, status: 502, error: "code bridge disconnected" });
    }
    this.pendingCodeFrames.clear();
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

  private clearEgressLease(leaseID: string): void {
    for (const [key, socket] of this.egressHosts) {
      if (egressSocketLeaseID(key) === leaseID) {
        closeSocket(socket, 1011, "lease ended");
        this.egressHosts.delete(key);
      }
    }
    for (const [key, socket] of this.egressClients) {
      if (egressSocketLeaseID(key) === leaseID) {
        closeSocket(socket, 1011, "lease ended");
        this.egressClients.delete(key);
      }
    }
    this.egressSessions.delete(leaseID);
  }

  private clearEgressSessionSockets(
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
  }

  private async webVNCViewer(request: Request, identifier: string): Promise<Response> {
    if (request.headers.get("upgrade")?.toLowerCase() !== "websocket") {
      return json(
        { error: "upgrade_required", message: "WebVNC viewer requires a websocket upgrade" },
        { status: 426 },
      );
    }
    const lease = await this.resolvePortalLease(identifier, request);
    if (!lease) {
      return notFound();
    }
    const error = webVNCLeaseError(lease);
    if (error) {
      return json({ error: "webvnc_unavailable", message: error }, { status: 409 });
    }
    const agent = this.claimIdleWebVNCAgent(lease.id);
    if (!agent) {
      const command = webVNCBridgeCommand(lease);
      return json(
        {
          error: "webvnc_bridge_missing",
          message: `No WebVNC backend is available yet; start or refresh the bridge with: ${command}`,
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

    const pair = new WebSocketPair();
    const client = pair[0];
    const viewer = pair[1];
    const owner = requestOwner(request);
    const label = webVNCViewerLabel(owner);

    this.trackWebVNCViewer(lease.id, {
      id: viewerID,
      agentID: agent.id,
      socket: viewer,
      owner,
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
      label,
    });
    flushPendingWebVNC(this.pendingWebVNCToViewer, webVNCBufferKey(lease.id, agent.id), viewer);
    return new Response(null, { status: 101, webSocket: client });
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

  private claimIdleWebVNCAgent(leaseID: string): { id: string; socket: WebSocket } | undefined {
    const viewers = this.openWebVNCViewers(leaseID);
    for (const [id, socket] of this.openWebVNCAgents(leaseID)) {
      if (!viewers.some((viewer) => viewer.agentID === id)) {
        return { id, socket };
      }
    }
    return undefined;
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

  private async consumeWebVNCTicket(request: Request): Promise<WebVNCTicketRecord | undefined> {
    const value = bridgeTicketFromRequest(request);
    if (!validWebVNCTicket(value)) {
      return undefined;
    }
    const key = webVNCTicketKey(value);
    const ticket = await this.state.storage.get<WebVNCTicketRecord>(key);
    if (!ticket || ticket.ticket !== value) {
      return undefined;
    }
    await this.state.storage.delete(key);
    if (Date.parse(ticket.expiresAt) <= Date.now()) {
      return undefined;
    }
    return ticket;
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

  private async consumeCodeTicket(request: Request): Promise<CodeTicketRecord | undefined> {
    const value = bridgeTicketFromRequest(request);
    if (!validCodeTicket(value)) {
      return undefined;
    }
    const key = codeTicketKey(value);
    const ticket = await this.state.storage.get<CodeTicketRecord>(key);
    if (!ticket || ticket.ticket !== value) {
      return undefined;
    }
    await this.state.storage.delete(key);
    if (Date.parse(ticket.expiresAt) <= Date.now()) {
      return undefined;
    }
    return ticket;
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

  private async consumeEgressTicket(request: Request): Promise<EgressTicketRecord | undefined> {
    const value = bridgeTicketFromRequest(request);
    if (!validEgressTicket(value)) {
      return undefined;
    }
    const key = egressTicketKey(value);
    const ticket = await this.state.storage.get<EgressTicketRecord>(key);
    if (!ticket || ticket.ticket !== value) {
      return undefined;
    }
    await this.state.storage.delete(key);
    if (Date.parse(ticket.expiresAt) <= Date.now()) {
      return undefined;
    }
    return ticket;
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
              : [
                  ...(await this.provider("hetzner").listCrabboxServers()),
                  ...(await this.listProviderMachinesSafe("aws")),
                  ...(await this.listProviderMachinesSafe("azure")),
                  ...(await this.listProviderMachinesSafe("gcp")),
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
      return json({ pools: await this.allReadyPoolStatus(request) });
    }
    const decodedKey = decodeReadyPoolRouteKey(rawKey ?? "");
    const key = decodedKey === undefined ? "" : normalizeReadyPoolKey(decodedKey);
    if (!key) {
      return json({ error: "invalid_pool_key" }, { status: 400 });
    }
    if (method === "GET" && !action) {
      return json({ pool: await this.readyPoolStatus(key, request) });
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
    const entries = (await this.visibleReadyPoolEntries(request)).filter(
      (entry) => entry.key === key,
    );
    await this.markStaleReadyPoolEntries(
      entries,
      new Map((await this.leaseRecords()).map((lease) => [lease.id, lease])),
      Date.now(),
    );
    return (await this.visibleReadyPoolEntries(request)).filter((entry) => entry.key === key);
  }

  private async allReadyPoolStatus(request: Request): Promise<ReadyPoolEntry[]> {
    const entries = await this.visibleReadyPoolEntries(request);
    await this.markStaleReadyPoolEntries(
      entries,
      new Map((await this.leaseRecords()).map((lease) => [lease.id, lease])),
      Date.now(),
    );
    return await this.visibleReadyPoolEntries(request);
  }

  private async visibleReadyPoolEntries(request: Request): Promise<ReadyPoolEntry[]> {
    const entries = await this.readyPoolEntries();
    const leases = new Map((await this.leaseRecords()).map((lease) => [lease.id, lease]));
    return entries
      .filter((entry) =>
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
      return json({ entry, lease });
    });
  }

  private async borrowReadyPoolLease(request: Request, key: string): Promise<Response> {
    const input = await readJson<ReadyPoolBorrowRequest>(request);
    return await this.withReadyPoolBorrowLock(async () => {
      const entries = (await this.readyPoolStatus(key, request)).filter((entry) =>
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
      if (ready.length === 0) {
        await this.markStaleReadyPoolEntries(entries, leases, nowMs);
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
      const first = ready[0];
      if (!first) {
        await this.markStaleReadyPoolEntries(entries, leases, nowMs);
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
      return json({ entry: borrowed, lease });
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
            entry: drained,
            lease: await this.releaseResolvedLease(lease, { deleteServer: true, keep: false }),
          });
        }
        return json({ entry: drained, lease });
      }
      if (!lease || lease.state !== "active" || Date.parse(lease.expiresAt) <= Date.now()) {
        const stale = this.nextReturnedReadyPoolEntry(current, lease, "stale", input.reason);
        await this.putReadyPoolEntry(stale);
        return json({ entry: stale, lease });
      }
      const returned = this.nextReturnedReadyPoolEntry(current, lease, "ready", input.reason);
      await this.putReadyPoolEntry(returned);
      return json({ entry: returned, lease });
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
    const leases = isAdminRequest(request)
      ? this.filterLeases(await this.leaseRecords(), request)
      : this.filterLeasesForRequest(await this.leaseRecords(), request);
    return json({ leases });
  }

  private async adminLeases(request: Request): Promise<Response> {
    return json({ leases: this.filterLeases(await this.leaseRecords(), request) });
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
    const org = url.searchParams.get("org") ?? "";
    const limit = clampLimit(url.searchParams.get("limit"), 100);
    const leases = (await this.leaseRecords())
      .filter((lease) => lease.provider === provider)
      .filter((lease) => !state || lease.state === state)
      .filter((lease) => !owner || lease.owner === owner)
      .filter((lease) => !org || lease.org === org)
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
            message: sanitizeMacHostQuotaError(errorMessage(error)),
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
            const message = errorMessage(error);
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
    const sweep = await this.runAWSOrphanSweep("admin", config);
    await this.scheduleAlarm();
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
    const sweep = await this.runAzureOrphanSweep("admin", config);
    await this.scheduleAlarm();
    return json({ config, sweep });
  }

  private async auditAWSLeaseCloud(lease: LeaseRecord): Promise<LeaseCloudAudit> {
    const audit: LeaseCloudAudit = {
      leaseID: lease.id,
      provider: lease.provider,
      state: lease.state,
      target: lease.target,
      owner: lease.owner,
      org: lease.org,
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
      audit.cleanupError = lease.cleanupError;
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
      const message = errorMessage(error);
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
      org: lease.org,
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
      audit.cleanupError = lease.cleanupError;
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
      const message = errorMessage(error);
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
    return json({
      lease: await this.releaseResolvedLease(lease, { deleteServer: true, keep: false }),
    });
  }

  private filterLeases(leases: LeaseRecord[], request: Request): LeaseRecord[] {
    const url = new URL(request.url);
    const state = url.searchParams.get("state") ?? "";
    const provider = url.searchParams.get("provider") ?? "";
    const owner = url.searchParams.get("owner") ?? "";
    const org = url.searchParams.get("org") ?? "";
    const limit = clampLimit(url.searchParams.get("limit"), 100);
    return leases
      .filter((lease) => !state || lease.state === state)
      .filter((lease) => !provider || lease.provider === provider)
      .filter((lease) => !owner || lease.owner === owner)
      .filter((lease) => !org || lease.org === org)
      .toSorted((a, b) => b.createdAt.localeCompare(a.createdAt))
      .slice(0, limit);
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
      owner,
      org,
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
    return json({ run }, { status: 201 });
  }

  private async createArtifactUploads(request: Request): Promise<Response> {
    try {
      const input = await readJson<ArtifactUploadRequest>(request);
      return json(await artifactUploadResponse(this.env, input, requestOwner(request)), {
        status: 201,
      });
    } catch (error) {
      return json(
        { error: "artifact_upload_unavailable", message: errorMessage(error) },
        { status: 400 },
      );
    }
  }

  private async runRoute(request: Request, runID: string, action?: string): Promise<Response> {
    const method = request.method.toUpperCase();
    if (method === "GET" && action === undefined) {
      const run = await this.getRun(runID);
      return run && this.runVisibleToRequest(run, request) ? json({ run }) : notFound();
    }
    if (method === "GET" && action === "logs") {
      const run = await this.getRun(runID);
      if (!run || !this.runVisibleToRequest(run, request)) {
        return notFound();
      }
      const log = await this.readRunLog(runID);
      return new Response(log, {
        headers: { "content-type": "text/plain; charset=utf-8" },
      });
    }
    if (method === "GET" && action === "events") {
      const run = await this.getRun(runID);
      if (!run || !this.runVisibleToRequest(run, request)) {
        return notFound();
      }
      const url = new URL(request.url);
      const after = finiteQueryNumber(url.searchParams.get("after")) ?? 0;
      const limit = clampLimit(url.searchParams.get("limit"), 500);
      return json({ events: await this.runEvents(runID, after, limit) });
    }
    if (method === "POST" && action === "events") {
      const run = await this.getRun(runID);
      if (!run || !this.runVisibleToRequest(run, request)) {
        return notFound();
      }
      const input = await readJson<RunEventRequest>(request);
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
    if (!run || !this.runVisibleToRequest(run, request)) {
      return notFound();
    }
    const input = await readJson<RunTelemetryRequest>(request);
    const telemetry = sanitizeLeaseTelemetry(input.telemetry, new Date());
    if (!telemetry) {
      return json({ error: "invalid_telemetry" }, { status: 400 });
    }
    run.telemetry = appendRunTelemetrySample(run.telemetry, telemetry);
    await this.putRun(run);
    return json({ run });
  }

  private async finishRun(request: Request, runID: string): Promise<Response> {
    const run = await this.getRun(runID);
    if (!run || !this.runVisibleToRequest(run, request)) {
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
    return json({ run });
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
    const org = url.searchParams.get("org") ?? "";
    const state = url.searchParams.get("state") ?? "";
    const limit = clampLimit(url.searchParams.get("limit"), 50);
    const admin = isAdminRequest(request);
    const runs = await this.runRecords();
    const scopedOwner = admin ? owner : requestOwner(request);
    const scopedOrg = admin ? org : requestOrg(request, this.env);
    return json({
      runs: runs
        .filter((run) => !leaseID || run.leaseID === leaseID)
        .filter((run) => !scopedOwner || run.owner === scopedOwner)
        .filter((run) => !scopedOrg || run.org === scopedOrg)
        .filter((run) => !state || run.state === state)
        .toSorted((a, b) => b.startedAt.localeCompare(a.startedAt))
        .slice(0, limit),
    });
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
        .slice(0, limit),
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
    const org = admin ? url.searchParams.get("org") : requestOrg(request, this.env);
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
    return portalExternalRunnerDetail(runner, { admin });
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
    return json({ runners: synced, stale });
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
    const org = admin
      ? (url.searchParams.get("org") ?? requestOrg(request, this.env))
      : requestOrg(request, this.env);
    const usage = usageSummary(await this.leaseRecords(), { scope, owner, org, month }, new Date());
    return json({ usage, limits: costLimits(this.env) });
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
    const provider = this.provider(lease.provider, lease.region, lease.providerProject);
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
    const leases = await this.state.storage.list<LeaseRecord>({ prefix: "lease:" });
    const now = Date.now();
    const expired = [...leases.values()].filter((lease) => leaseNeedsCleanup(lease, now));
    await Promise.all(
      expired.map(async (lease) => {
        const retryAt = Date.parse(lease.cleanupRetryAt ?? "");
        if (Number.isFinite(retryAt) && retryAt > now) {
          return;
        }
        const nowISO = new Date().toISOString();
        if (lease.state === "provisioning" && !lease.cloudID) {
          lease.state = "failed";
          lease.updatedAt = nowISO;
          lease.endedAt = nowISO;
          lease.cleanupFailedAt = nowISO;
          lease.cleanupError = "lease expired before provider returned a cloud resource";
          await this.putLease(lease);
          return;
        }
        try {
          await this.deleteLeaseServer(lease);
        } catch (error) {
          lease.cleanupAttempts = (lease.cleanupAttempts ?? 0) + 1;
          lease.cleanupError = errorMessage(error);
          lease.cleanupFailedAt = nowISO;
          lease.cleanupRetryAt = new Date(now + leaseCleanupRetryDelayMs).toISOString();
          lease.updatedAt = nowISO;
          await this.putLease(lease);
          console.warn(
            `lease cleanup failed lease=${lease.id} provider=${lease.provider} cloud=${lease.cloudID}: ${lease.cleanupError}`,
          );
          return;
        }
        lease.state = leaseIsLive(lease) ? "expired" : lease.state;
        lease.updatedAt = nowISO;
        lease.endedAt = nowISO;
        delete lease.releaseDeletesServer;
        clearLeaseCleanupMetadata(lease);
        await this.putLease(lease);
      }),
    );
  }

  private async scheduleAlarm(): Promise<void> {
    const leases = await this.state.storage.list<LeaseRecord>({ prefix: "lease:" });
    const now = Date.now();
    const alarmTimes = [...leases.values()]
      .filter((lease) => leaseIsLive(lease) || leaseNeedsCleanup(lease, now))
      .map((lease) => nextLeaseAlarmTime(lease))
      .filter((time) => Number.isFinite(time));
    const orphanSweepAlarm = await this.nextAWSOrphanSweepAlarmTime();
    if (orphanSweepAlarm !== undefined) {
      alarmTimes.push(orphanSweepAlarm);
    }
    const azureOrphanSweepAlarm = await this.nextAzureOrphanSweepAlarmTime();
    if (azureOrphanSweepAlarm !== undefined) {
      alarmTimes.push(azureOrphanSweepAlarm);
    }
    const azureCleanupAlarm = await this.nextAzureDeferredCleanupAlarmTime();
    if (azureCleanupAlarm !== undefined) {
      alarmTimes.push(azureCleanupAlarm);
    }
    if (alarmTimes.length === 0) {
      await this.state.storage.deleteAlarm();
      return;
    }
    await this.state.storage.setAlarm(Math.min(...alarmTimes));
  }

  private async nextAzureDeferredCleanupAlarmTime(): Promise<number | undefined> {
    const records = await this.state.storage.list<AzureDeferredCleanupRecord>({
      prefix: azureDeferredCleanupPrefix,
    });
    const times = [...records.values()]
      .map((record) => Date.parse(record.retryAt))
      .filter((time) => Number.isFinite(time));
    if (times.length === 0) {
      return undefined;
    }
    return Math.max(Date.now() + 1000, Math.min(...times));
  }

  private async runAzureDeferredCleanups(): Promise<void> {
    const records = await this.state.storage.list<AzureDeferredCleanupRecord>({
      prefix: azureDeferredCleanupPrefix,
    });
    const now = Date.now();
    await Promise.all(
      [...records.entries()].map(async ([key, record]) => {
        const retryAt = Date.parse(record.retryAt);
        if (Number.isFinite(retryAt) && retryAt > now) {
          return;
        }
        try {
          await new AzureClient(this.env, { location: record.location }).deleteServer(record.name);
        } catch (error) {
          const nextRecord: AzureDeferredCleanupRecord = {
            ...record,
            attempts: record.attempts + 1,
            updatedAt: new Date().toISOString(),
            retryAt: new Date(now + leaseCleanupRetryDelayMs).toISOString(),
            lastError: errorMessage(error),
          };
          await this.state.storage.put(key, nextRecord);
          console.warn(
            `azure deferred cleanup failed name=${record.name} location=${record.location}: ${nextRecord.lastError}`,
          );
          return;
        }
        await this.state.storage.delete(key);
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

  private async runAWSOrphanSweepIfDue(trigger: "alarm" | "admin"): Promise<void> {
    const config = this.awsOrphanSweepConfig();
    if (!config.enabled) {
      return;
    }
    const lastRun = await this.state.storage.get<AWSOrphanSweepRecord>(awsOrphanSweepRecordKey);
    const lastFinishedAt = Date.parse(lastRun?.finishedAt ?? "");
    if (
      trigger !== "admin" &&
      Number.isFinite(lastFinishedAt) &&
      Date.now() < lastFinishedAt + config.intervalSeconds * 1000
    ) {
      return;
    }
    await this.runAWSOrphanSweep(trigger, config);
  }

  private async runAWSOrphanSweep(
    trigger: "alarm" | "admin",
    config = this.awsOrphanSweepConfig(),
  ): Promise<AWSOrphanSweepRecord> {
    const startedAt = new Date().toISOString();
    const leases = [...(await this.state.storage.list<LeaseRecord>({ prefix: "lease:" })).values()];
    const activeAWSLeases = leases.filter(
      (lease) => lease.provider === "aws" && leaseIsLive(lease),
    );
    const activeLeases = new Map(activeAWSLeases.map((lease) => [lease.id, lease]));
    const activeCloudIDs = new Set(activeAWSLeases.map((lease) => lease.cloudID).filter(Boolean));
    const candidates: AWSOrphanSweepCandidate[] = [];
    const macHostCandidates: AWSMacHostSweepCandidate[] = [];
    const errors: AWSOrphanSweepRecord["errors"] = [];
    let scanned = 0;
    let macHostsScanned = 0;
    for (const region of config.regions) {
      try {
        // oxlint-disable-next-line eslint/no-await-in-loop -- regions are swept independently.
        const machines = await this.provider("aws", region).listCrabboxServers();
        scanned += machines.length;
        for (const machine of machines) {
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
          if (config.deleteEnabled) {
            try {
              // oxlint-disable-next-line eslint/no-await-in-loop -- delete failures must stay attached to the candidate.
              await this.provider("aws", region).deleteServer(
                machine.cloudID || String(machine.id),
              );
              candidate.action = "terminated";
            } catch (error) {
              candidate.action = "terminate_failed";
              candidate.error = errorMessage(error);
              console.warn(
                `aws orphan sweep terminate failed region=${region} cloud=${machine.cloudID}: ${candidate.error}`,
              );
            }
          }
          candidates.push(candidate);
        }
        if (config.macHostReleaseEnabled) {
          const client = new EC2SpotClient(this.env, region);
          // oxlint-disable-next-line eslint/no-await-in-loop -- keep host cleanup attached to its region.
          const macHosts = await client.listMacHosts();
          macHostsScanned += macHosts.length;
          for (const host of macHosts) {
            const candidate = awsMacHostSweepCandidate(
              host,
              activeAWSLeases,
              region,
              Math.max(config.graceSeconds, 3600),
            );
            if (!candidate) {
              continue;
            }
            if (config.deleteEnabled) {
              try {
                // oxlint-disable-next-line eslint/no-await-in-loop -- release failures must stay attached to the host.
                await client.releaseMacHost(host.id);
                candidate.action = "released";
              } catch (error) {
                candidate.action = "release_failed";
                candidate.error = errorMessage(error);
                console.warn(
                  `aws orphan sweep mac host release failed region=${region} host=${host.id}: ${candidate.error}`,
                );
              }
            }
            macHostCandidates.push(candidate);
          }
        }
      } catch (error) {
        const message = errorMessage(error);
        errors.push({ region, message });
        console.warn(`aws orphan sweep failed region=${region}: ${message}`);
      }
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
    await this.state.storage.put(awsOrphanSweepRecordKey, record);
    await this.state.storage.delete(awsOrphanSweepFirstAlarmKey);
    if (candidates.length > 0 || macHostCandidates.length > 0 || errors.length > 0) {
      console.warn(
        `aws orphan sweep mode=${record.mode} scanned=${record.scanned} candidates=${candidates.length} terminated=${record.terminated} mac_hosts=${macHostCandidates.length} mac_hosts_released=${record.macHostsReleased ?? 0} errors=${errors.length}`,
      );
    }
    return record;
  }

  private awsOrphanSweepConfig(): AWSOrphanSweepConfig {
    const hasAWSCredentials = Boolean(this.env.AWS_ACCESS_KEY_ID && this.env.AWS_SECRET_ACCESS_KEY);
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

  private async runAzureOrphanSweepIfDue(trigger: "alarm" | "admin"): Promise<void> {
    const config = this.azureOrphanSweepConfig();
    if (!config.enabled) {
      return;
    }
    const lastRun = await this.state.storage.get<AzureOrphanSweepRecord>(azureOrphanSweepRecordKey);
    const lastFinishedAt = Date.parse(lastRun?.finishedAt ?? "");
    if (
      trigger !== "admin" &&
      Number.isFinite(lastFinishedAt) &&
      Date.now() < lastFinishedAt + config.intervalSeconds * 1000
    ) {
      return;
    }
    await this.runAzureOrphanSweep(trigger, config);
  }

  private async runAzureOrphanSweep(
    trigger: "alarm" | "admin",
    config = this.azureOrphanSweepConfig(),
  ): Promise<AzureOrphanSweepRecord> {
    const startedAt = new Date().toISOString();
    const leases = [...(await this.state.storage.list<LeaseRecord>({ prefix: "lease:" })).values()];
    const liveAzureLeases = leases.filter(
      (lease) => lease.provider === "azure" && leaseIsLive(lease),
    );
    const liveLeases = new Map(liveAzureLeases.map((lease) => [lease.id, lease]));
    const liveCloudIDs = new Set(liveAzureLeases.map((lease) => lease.cloudID).filter(Boolean));
    const candidates: AzureOrphanSweepCandidate[] = [];
    const errors: AzureOrphanSweepRecord["errors"] = [];
    const seenCloudIDs = new Set<string>();
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
          const candidate = cloudOrphanSweepCandidate(
            machine,
            liveLeases,
            liveCloudIDs,
            candidateRegion,
            config.graceSeconds,
          );
          if (!candidate) {
            continue;
          }
          if (config.deleteEnabled) {
            try {
              // oxlint-disable-next-line eslint/no-await-in-loop -- delete failures must stay attached to the candidate.
              await this.provider("azure", candidateRegion).deleteServer(
                machine.cloudID || machine.name,
              );
              candidate.action = "terminated";
            } catch (error) {
              candidate.action = "terminate_failed";
              candidate.error = errorMessage(error);
              console.warn(
                `azure orphan sweep terminate failed region=${region} cloud=${machine.cloudID}: ${candidate.error}`,
              );
            }
          }
          candidates.push(candidate);
        }
      } catch (error) {
        const message = errorMessage(error);
        errors.push({ region, message });
        console.warn(`azure orphan sweep failed region=${region}: ${message}`);
      }
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
    await this.state.storage.put(azureOrphanSweepRecordKey, record);
    await this.state.storage.delete(azureOrphanSweepFirstAlarmKey);
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

  private async getLease(leaseID: string): Promise<LeaseRecord | undefined> {
    return this.state.storage.get<LeaseRecord>(leaseKey(leaseID));
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

  private async providerAccessRecords(): Promise<LeaseRecord[]> {
    const records = await this.state.storage.list<LeaseRecord>({
      prefix: providerAccessPrefix(),
    });
    const now = Date.now();
    const active: LeaseRecord[] = [];
    const staleKeys: string[] = [];
    for (const [key, record] of records) {
      if (record.state === "active" && Date.parse(record.expiresAt) > now) {
        active.push(record);
        continue;
      }
      staleKeys.push(key);
    }
    await Promise.all(staleKeys.map((key) => this.state.storage.delete(key)));
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

  private async runRecords(): Promise<RunRecord[]> {
    const runs = await this.state.storage.list<RunRecord>({ prefix: "run:" });
    return [...runs.values()];
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

  private leaseAccessRole(
    lease: LeaseRecord,
    request: Request,
    admin: boolean,
  ): "owner" | LeaseShareRole | undefined {
    if (
      admin ||
      (lease.owner === requestOwner(request) && lease.org === requestOrg(request, this.env))
    ) {
      return "owner";
    }
    const share = normalizedLeaseShare(lease.share);
    const userRole = share.users[normalizeShareUser(requestOwner(request))];
    const orgRole = lease.org === requestOrg(request, this.env) ? share.org : undefined;
    if (userRole === "manage" || orgRole === "manage") {
      return "manage";
    }
    if (userRole === "use" || orgRole === "use") {
      return "use";
    }
    return undefined;
  }

  private runVisibleToRequest(run: RunRecord, request: Request): boolean {
    return (
      isAdminRequest(request) ||
      (run.owner === requestOwner(request) && run.org === requestOrg(request, this.env))
    );
  }

  private runVisibleToControl(
    run: RunRecord,
    attachment: Extract<BridgeAttachment, { kind: "control" }>,
  ): boolean {
    return Boolean(
      attachment.admin || (run.owner === attachment.owner && run.org === attachment.org),
    );
  }

  private leaseVisibleToControl(
    lease: LeaseRecord,
    attachment: Extract<BridgeAttachment, { kind: "control" }>,
  ): boolean {
    return Boolean(
      attachment.admin || (lease.owner === attachment.owner && lease.org === attachment.org),
    );
  }

  private async putLease(lease: LeaseRecord): Promise<void> {
    await this.state.storage.put(leaseKey(lease.id), lease);
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
    applyRunEventSummary(run, event);
    run.eventCount = seq;
    run.lastEventAt = now;
    await this.state.storage.put(runEventKey(run.id, seq), event);
    await this.putRun(run);
    this.broadcastRunEvent(run, event);
    return event;
  }

  private async listProviderMachinesSafe(provider: Provider): Promise<ProviderMachine[]> {
    try {
      return await this.provider(provider).listCrabboxServers();
    } catch {
      return [];
    }
  }

  private broadcastRunEvent(run: RunRecord, event: RunEventRecord): void {
    for (const socket of this.controlSockets.values()) {
      if (socket.readyState !== WebSocket.OPEN) {
        continue;
      }
      const attachment = bridgeAttachment(socket);
      if (!attachment || attachment.kind !== "control") {
        continue;
      }
      const after = attachment.subscriptions?.[run.id];
      if (after === undefined || after >= event.seq || !this.runVisibleToControl(run, attachment)) {
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
      return new AzureProvider(this.env, this.state.storage, region);
    }
    if (provider === "gcp") {
      return new GCPProvider(this.env, region, project);
    }
    return new HetznerProvider(this.env);
  }

  private async deleteLeaseServer(lease: LeaseRecord): Promise<void> {
    await this.provider(lease.provider, lease.region, lease.providerProject).releaseLease(lease);
  }

  private async abortProvisionedLeaseAfterStateChange(
    current: LeaseRecord | undefined,
    record: LeaseRecord,
    config: LeaseConfig,
    server: ProviderMachine,
    serverType: string,
    deletePublishedAccess: boolean,
  ): Promise<Response> {
    if (deletePublishedAccess) {
      await this.deleteProviderAccess(record.id);
    }
    const cleanupLease = provisionedLeaseRecord(current ?? record, config, server, serverType);
    if (current?.state === "released" && current.releaseDeletesServer === false) {
      cleanupLease.state = "released";
      cleanupLease.keep = true;
      clearLeaseCleanupMetadata(cleanupLease);
      await this.putLease(cleanupLease);
      await this.scheduleAlarm();
      return json(
        {
          error: "lease_state_changed",
          message: "lease changed state while provider provisioning was in progress",
          lease: cleanupLease,
        },
        { status: 409 },
      );
    }
    try {
      await this.deleteLeaseServer(cleanupLease);
    } catch (error) {
      const failedAt = new Date().toISOString();
      cleanupLease.state = current?.state ?? "active";
      if (cleanupLease.state === "released") {
        cleanupLease.releaseDeletesServer = true;
      }
      cleanupLease.cleanupAttempts = (cleanupLease.cleanupAttempts ?? 0) + 1;
      cleanupLease.cleanupFailedAt = failedAt;
      cleanupLease.cleanupError = errorMessage(error);
      cleanupLease.cleanupRetryAt = new Date(Date.now() + leaseCleanupRetryDelayMs).toISOString();
      cleanupLease.expiresAt = failedAt;
      cleanupLease.updatedAt = failedAt;
      await this.putLease(cleanupLease);
      await this.scheduleAlarm();
      throw error;
    }
    await this.scheduleAlarm();
    return json(
      {
        error: "lease_state_changed",
        message: "lease changed state while provider provisioning was in progress",
        lease: current,
      },
      { status: 409 },
    );
  }

  private async releaseResolvedLease(
    lease: LeaseRecord,
    options: { deleteServer: boolean; keep?: boolean },
  ): Promise<LeaseRecord> {
    this.clearEgressLease(lease.id);
    if (
      options.deleteServer &&
      lease.cloudID &&
      (leaseIsLive(lease) || lease.releaseDeletesServer !== undefined || lease.cleanupError)
    ) {
      await this.deleteLeaseServer(lease);
    }
    const wasUnprovisionedRelease =
      !lease.cloudID && (lease.state === "provisioning" || lease.state === "released");
    const now = new Date().toISOString();
    lease.state = "released";
    lease.updatedAt = now;
    lease.releasedAt = now;
    lease.endedAt = now;
    if (wasUnprovisionedRelease) {
      lease.releaseDeletesServer = options.deleteServer;
    } else if (lease.releaseDeletesServer === false && !options.deleteServer) {
      lease.releaseDeletesServer = false;
    } else {
      delete lease.releaseDeletesServer;
    }
    clearLeaseCleanupMetadata(lease);
    if (options.keep !== undefined) {
      lease.keep = options.keep;
    }
    await this.putLease(lease);
    return lease;
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
  if (provider === "gcp") {
    const missing = providerRequiredSecrets(provider).filter((name) => !nonSecretString(env[name]));
    if (
      !nonSecretString(gcpProject) &&
      !nonSecretString(env.GCP_PROJECT_ID) &&
      !nonSecretString(env.CRABBOX_GCP_PROJECT)
    ) {
      missing.unshift("GCP_PROJECT_ID");
    }
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
  return `${externalRunnerPrefix()}${provider}:${runnerID}:${org}:${owner}`;
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

function sanitizeAWSRegion(value: string): string {
  const region = value.trim().toLowerCase();
  return /^[a-z]{2}-[a-z-]+-[0-9]$/.test(region) ? region : "";
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

function egressTicketPrefix(): string {
  return "egress-ticket:";
}

function egressTicketKey(ticket: string): string {
  return `${egressTicketPrefix()}${ticket}`;
}

function newLeaseID(): string {
  const bytes = new Uint8Array(6);
  crypto.getRandomValues(bytes);
  return `cbx_${[...bytes].map((byte) => byte.toString(16).padStart(2, "0")).join("")}`;
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

function validWebVNCTicket(value: string | undefined): value is string {
  return typeof value === "string" && /^wvnc_[a-f0-9]{32}$/.test(value);
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

export function bridgeTicketFromRequest(request: Request): string {
  const auth = request.headers.get("authorization")?.trim() ?? "";
  const match = /^Bearer\s+(.+)$/i.exec(auth);
  if (match?.[1]) {
    return match[1].trim();
  }
  return new URL(request.url).searchParams.get("ticket") ?? "";
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
  return config.awsRegion;
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

function azureDeferredCleanupKey(location: string, name: string): string {
  return `${azureDeferredCleanupPrefix}${location}:${name}`;
}

function validCrabboxProviderKey(value: string | undefined): value is string {
  return typeof value === "string" && /^crabbox-cbx-[a-f0-9]{12}$/.test(value);
}

function validExternalRunnerID(value: string | undefined): value is string {
  return typeof value === "string" && /^[A-Za-z0-9][A-Za-z0-9_.:-]{2,128}$/.test(value);
}

function clampLimit(value: string | null, fallback: number): number {
  const parsed = Number(value ?? "");
  if (!Number.isFinite(parsed) || parsed <= 0) {
    return fallback;
  }
  return Math.min(Math.trunc(parsed), 500);
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
  "frame-src 'self' https://*.vscode-cdn.net data:",
  "img-src 'self' https: data: blob:",
  "manifest-src 'self'",
  "media-src 'self'",
  "object-src 'none'",
  "script-src 'self' 'unsafe-inline' 'unsafe-eval' blob: https://static.cloudflareinsights.com",
  "style-src 'self' 'unsafe-inline'",
  "worker-src 'self' data: blob:",
].join("; ");

export function codeResponseHeaders(values: Record<string, string>): Headers {
  const headers = new Headers();
  for (const [key, value] of Object.entries(values)) {
    const lower = key.toLowerCase();
    if (
      lower === "connection" ||
      lower === "content-security-policy" ||
      lower === "content-encoding" ||
      lower === "content-length" ||
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

function bridgeAttachment(socket: WebSocket): BridgeAttachment | undefined {
  const attachment = socket.deserializeAttachment?.() as BridgeAttachment | undefined;
  if (!attachment || typeof attachment !== "object") {
    return undefined;
  }
  switch (attachment.kind) {
    case "webvnc-viewer":
      return typeof attachment.leaseID === "string" &&
        validWebVNCSessionID(attachment.id) &&
        validWebVNCSessionID(attachment.agentID) &&
        typeof attachment.owner === "string" &&
        typeof attachment.label === "string"
        ? attachment
        : undefined;
    case "webvnc-agent":
      return typeof attachment.leaseID === "string" && validWebVNCSessionID(attachment.id)
        ? {
            ...attachment,
            capabilities:
              attachment.capabilities instanceof Set ? attachment.capabilities : new Set<string>(),
          }
        : undefined;
    case "code-agent":
      return typeof attachment.leaseID === "string" ? attachment : undefined;
    case "code-viewer":
      return typeof attachment.leaseID === "string" && typeof attachment.id === "string"
        ? attachment
        : undefined;
    case "egress-host":
    case "egress-client":
      return typeof attachment.leaseID === "string" && typeof attachment.sessionID === "string"
        ? attachment
        : undefined;
    case "control":
      return typeof attachment.clientID === "string" &&
        typeof attachment.owner === "string" &&
        typeof attachment.org === "string"
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
  return [`lease:${attachment.leaseID}`, attachment.kind];
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
  socket.close(code, reason);
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
    input.provider === "gcp"
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
  const createdAt = parseLeaseDate(lease.createdAt, fallbackNow);
  const touchedAt = parseLeaseDate(lease.lastTouchedAt, createdAt);
  return leaseExpiresAt(
    createdAt,
    touchedAt,
    leaseTTLSeconds(lease),
    leaseIdleTimeoutSeconds(lease),
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

function leaseNeedsCleanup(lease: LeaseRecord, now: number): boolean {
  if (leaseIsLive(lease) && Date.parse(lease.expiresAt) <= now) {
    return true;
  }
  return Boolean(!leaseIsLive(lease) && lease.cloudID && lease.cleanupError);
}

function provisionedLeaseRecord(
  lease: LeaseRecord,
  config: LeaseConfig,
  server: ProviderMachine,
  serverType: string,
): LeaseRecord {
  const providerProject = lease.providerProject ?? providerProjectForConfig(config);
  return {
    ...lease,
    state: "active",
    cloudID: server.cloudID,
    serverID: server.id,
    serverName: server.name,
    serverType,
    host: server.host,
    region: server.region ?? lease.region ?? providerRegionForConfig(config) ?? "",
    ...(providerProject ? { providerProject } : {}),
    ...(server.hostID ? { hostId: server.hostID } : {}),
  };
}

function nextLeaseAlarmTime(lease: LeaseRecord): number {
  const now = Date.now();
  const expiresAt = Date.parse(lease.expiresAt);
  const cleanupRetryAt = Date.parse(lease.cleanupRetryAt ?? "");
  if (Number.isFinite(cleanupRetryAt) && cleanupRetryAt > now) {
    if (Number.isFinite(expiresAt) && expiresAt <= now) {
      return cleanupRetryAt;
    }
    return Math.min(expiresAt, cleanupRetryAt);
  }
  return expiresAt;
}

function clearLeaseCleanupMetadata(lease: LeaseRecord): void {
  delete lease.cleanupAttempts;
  delete lease.cleanupError;
  delete lease.cleanupFailedAt;
  delete lease.cleanupRetryAt;
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

function activeAWSSSHSourceCIDRs(leases: LeaseRecord[], cidrs: string[]): string[] {
  return uniqueNonEmpty([
    ...leases.flatMap((lease) =>
      lease.provider === "aws" && leaseIsLive(lease) ? (lease.network?.sshSourceCIDRs ?? []) : [],
    ),
    ...cidrs,
  ]);
}

function hasUnknownActiveAWSSSHSource(leases: LeaseRecord[]): boolean {
  return leases.some(
    (lease) =>
      lease.provider === "aws" &&
      leaseIsLive(lease) &&
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
  if (activeLease?.cloudID === machine.cloudID) {
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
    action: "reported",
  };
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
  deleteSSHKey(name: string): Promise<void>;
  hourlyPriceUSD(
    serverType: string,
    config: ReturnType<typeof leaseConfig>,
  ): Promise<number | undefined>;
}

interface ProviderStateStorage {
  get<T>(key: string): Promise<T | undefined>;
  put<T>(key: string, value: T): Promise<void>;
  list<T>(options?: { prefix?: string }): Promise<Map<string, T>>;
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
  publishAccessBeforeProvisioning?: boolean;
}

class HetznerProvider implements CloudProvider {
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
    const { server, serverType } = await this.client.createServerWithFallback(
      config,
      leaseID,
      slug,
      owner,
    );
    return { server: this.client.toMachine(server), serverType };
  }

  async deleteServer(id: string): Promise<void> {
    await this.client.deleteServer(Number(id));
  }

  async releaseLease(lease: LeaseRecord): Promise<void> {
    await this.deleteServer(String(lease.serverID));
    if (validCrabboxProviderKey(lease.providerKey)) {
      await this.deleteSSHKey(lease.providerKey);
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

  async deleteSSHKey(name: string): Promise<void> {
    await this.client.deleteSSHKey(name);
  }

  hourlyPriceUSD(
    serverType: string,
    config: ReturnType<typeof leaseConfig>,
  ): Promise<number | undefined> {
    return this.client.hourlyPriceUSD(serverType, config.location);
  }
}

class AzureProvider implements CloudProvider {
  private clientValue?: AzureClient;

  constructor(
    private readonly env: Env,
    private readonly storage?: DurableObjectStorage,
    private readonly location?: string,
  ) {}

  private get client(): AzureClient {
    this.clientValue ??= new AzureClient(this.env, {
      ...(this.location ? { location: this.location } : {}),
      deferredCleanup: (request) => this.recordDeferredCleanup(request),
    });
    return this.clientValue;
  }

  private async recordDeferredCleanup(request: AzureDeferredCleanupRequest): Promise<void> {
    if (!this.storage) return;
    const key = azureDeferredCleanupKey(request.location, request.name);
    const current = await this.storage.get<AzureDeferredCleanupRecord>(key);
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
    await this.storage.put(key, record);
    await this.storage.setAlarm(Date.now() + 1000);
  }

  listCrabboxServers(): Promise<ProviderMachine[]> {
    return this.client.listCrabboxServers();
  }

  async prepareLeaseConfig(
    config: ReturnType<typeof leaseConfig>,
  ): Promise<ReturnType<typeof leaseConfig>> {
    return config.azureLocation
      ? config
      : { ...config, azureLocation: azureLocationFor(this.env, "") };
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

  async finalizeLeaseCreate(
    config: ReturnType<typeof leaseConfig>,
    lease: LeaseRecord,
    server: ProviderMachine,
    attempts: ProvisioningAttempt[],
  ): Promise<ProviderLeaseCreateFinalization> {
    const region = server.region || config.azureLocation;
    const nextConfig = region ? { ...config, azureLocation: region } : config;
    const nextLease: LeaseRecord = { ...lease, region };
    const hints = capacityHints(this.env, nextConfig, nextLease, attempts);
    if (hints.length > 0) {
      nextLease.capacityHints = hints;
    }
    return { config: nextConfig, lease: nextLease };
  }

  async releaseLease(lease: LeaseRecord): Promise<void> {
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

  validateLeaseImageStrategy(
    _lease: LeaseRecord,
    strategy: "image" | "disk-snapshot",
  ): string | undefined {
    return strategy === "image"
      ? "Azure managed images require a stopped/generalized source VM; use disk-snapshot checkpoints for active Azure leases"
      : undefined;
  }

  createLeaseImage(
    lease: LeaseRecord,
    name: string,
    _noReboot: boolean,
    _strategy: "image" | "disk-snapshot",
  ): Promise<ProviderImage> {
    return this.client.createDiskSnapshot(
      lease.cloudID,
      providerImageResourceName(lease.provider, name, lease.id),
    );
  }

  getImage(imageID: string, kind?: string): Promise<ProviderImage> {
    return this.client.getImage(imageID, kind);
  }

  deleteImage(imageID: string, kind?: string): Promise<void> {
    return this.client.deleteImage(imageID, kind);
  }

  storedImageMetadata = noStoredImageMetadata;
  decorateImage = passthroughProviderImage;
  validateDeleteImage = allowProviderImageDelete;

  async deleteSSHKey(): Promise<void> {
    // Azure stores the SSH public key inline on the VM; nothing to clean up.
  }

  hourlyPriceUSD(): Promise<number | undefined> {
    return Promise.resolve(undefined);
  }
}

class GCPProvider implements CloudProvider {
  private clientValue?: GCPClient;

  constructor(
    private readonly env: Env,
    private readonly zone?: string,
    private readonly project?: string,
  ) {}

  private get client(): GCPClient {
    this.clientValue ??= new GCPClient(this.env, this.zone, this.project);
    return this.clientValue;
  }

  listCrabboxServers(): Promise<ProviderMachine[]> {
    return this.client.listCrabboxServers();
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

  createLeaseImage(
    lease: LeaseRecord,
    name: string,
    _noReboot: boolean,
    strategy: "image" | "disk-snapshot",
  ): Promise<ProviderImage> {
    return strategy === "image"
      ? this.client.createImage(
          lease.cloudID,
          providerImageResourceName(lease.provider, name, lease.id),
        )
      : this.client.createDiskSnapshot(
          lease.cloudID,
          providerImageResourceName(lease.provider, name, lease.id),
        );
  }

  getImage(imageID: string, kind?: string): Promise<ProviderImage> {
    return this.client.getImage(imageID, kind);
  }

  deleteImage(imageID: string, kind?: string): Promise<void> {
    return this.client.deleteImage(imageID, kind);
  }

  storedImageMetadata = noStoredImageMetadata;
  decorateImage = passthroughProviderImage;
  validateDeleteImage = allowProviderImageDelete;

  deleteSSHKey(): Promise<void> {
    return this.client.deleteSSHKey();
  }

  hourlyPriceUSD(): Promise<number | undefined> {
    return this.client.hourlyPriceUSD();
  }
}

class AWSProvider implements CloudProvider {
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

  listCrabboxServers(): Promise<ProviderMachine[]> {
    return this.client.listCrabboxServers();
  }

  getServer(id: string): Promise<ProviderMachine> {
    return this.client.getServer(id);
  }

  async prepareLeaseConfig(
    config: ReturnType<typeof leaseConfig>,
  ): Promise<ReturnType<typeof leaseConfig>> {
    if (config.awsAMI || config.awsSnapshot) {
      return config;
    }
    if (config.target === "macos") {
      return { ...config, awsPromotedAMIs: await this.promotedImagesForFallback(config) };
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
    const sourceCIDRs = awsLeaseSSHSourceCIDRs(config, context);
    const globalCIDRs = awsGlobalSSHSourceCIDRs(this.env);
    const nextLease = withLeaseSSHSourceCIDRs(
      lease,
      sourceCIDRs,
      sourceCIDRs.length > 0 || globalCIDRs.length > 0,
    );
    const activeLeases = replaceProviderAccessState(context.activeLeases, nextLease);
    return {
      config: {
        ...config,
        awsSSHCIDRs: activeAWSSSHSourceCIDRs(activeLeases, [...sourceCIDRs, ...globalCIDRs]),
      },
      lease: nextLease,
      provisioning: {
        sshIngressReconcile: hasUnknownActiveAWSSSHSource(activeLeases)
          ? "additive"
          : "authoritative",
        publishAccessBeforeProvisioning: true,
      },
    };
  }

  async refreshLeaseAccess(
    lease: LeaseRecord,
    context: ProviderAccessContext,
  ): Promise<LeaseRecord | void> {
    if (lease.state !== "active") {
      return;
    }
    const sourceCIDRs = context.requestSourceCIDRs;
    if (sourceCIDRs.length === 0) {
      return;
    }
    const nextLease = withLeaseSSHSourceCIDRs(lease, sourceCIDRs, true);
    const activeLeases = replaceProviderAccessState(context.activeLeases, nextLease);
    const cidrs = activeAWSSSHSourceCIDRs(activeLeases, sourceCIDRs);
    if (cidrs.length === 0) {
      return nextLease;
    }
    try {
      const config = leaseConfig({
        provider: "aws",
        target: lease.target,
        windowsMode: lease.windowsMode ?? "normal",
        class: lease.class,
        serverType: lease.serverType,
        awsSSHCIDRs: cidrs,
        capacity: { market: lease.market === "spot" ? "spot" : "on-demand" },
        providerKey: lease.providerKey,
        sshUser: lease.sshUser,
        sshPort: lease.sshPort,
        sshFallbackPorts: lease.sshFallbackPorts ?? [],
        sshPublicKey: "ssh-ed25519 heartbeat-refresh",
        workRoot: lease.workRoot,
        ...(lease.hostId || lease.hostID ? { hostId: lease.hostId || lease.hostID } : {}),
        ...(lease.region ? { awsRegion: lease.region } : {}),
      });
      const region = config.awsRegion || this.region;
      const client = region === this.region ? this.client : new EC2SpotClient(this.env, region);
      await client.refreshSSHIngress(config, {
        reconcile: hasUnknownActiveAWSSSHSource(activeLeases) ? "additive" : "authoritative",
      });
    } catch (error) {
      console.warn(`refresh AWS SSH ingress failed for ${lease.id}: ${errorMessage(error)}`);
    }
    return nextLease;
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
      provisioning?.sshIngressReconcile === undefined
        ? undefined
        : { reconcile: provisioning.sshIngressReconcile };
    for (const region of regions) {
      const client = region === this.region ? this.client : new EC2SpotClient(this.env, region);
      try {
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
          readyServer = await client.waitForServerIP(server.cloudID);
        } catch (error) {
          const waitMessage = error instanceof Error ? error.message : String(error);
          try {
            // oxlint-disable-next-line eslint/no-await-in-loop -- clean up the exact instance before any fallback.
            await client.deleteServer(server.cloudID);
          } catch (deleteError) {
            const deleteMessage =
              deleteError instanceof Error ? deleteError.message : String(deleteError);
            if (!isAWSInstanceCleanedAfterReadinessFailure(waitMessage, deleteMessage)) {
              throw new Error(
                `${waitMessage}; cleanup failed for AWS instance ${server.cloudID}: ${deleteMessage}`,
                { cause: deleteError },
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
    const nextLease: LeaseRecord = { ...lease, region: server.region ?? nextConfig.awsRegion };
    const hints = capacityHints(this.env, nextConfig, nextLease, attempts);
    if (hints.length > 0) {
      nextLease.capacityHints = hints;
    }
    return { config: nextConfig, lease: nextLease };
  }

  async releaseLease(lease: LeaseRecord): Promise<void> {
    try {
      await this.deleteServer(lease.cloudID);
    } catch (error) {
      const message = errorMessage(error);
      if (!isCloudNotFoundError(message)) {
        throw error;
      }
      console.warn(
        `AWS lease cleanup found missing instance lease=${lease.id} cloud=${lease.cloudID}: ${message}`,
      );
    }
    if (validCrabboxProviderKey(lease.providerKey)) {
      await this.deleteSSHKey(lease.providerKey);
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

  deleteImage(imageID: string): Promise<void> {
    return this.client.deleteImage(imageID);
  }

  async storedImageMetadata(imageID: string): Promise<ProviderImage | undefined> {
    return (
      (await this.promotedImageByID(imageID)) ??
      (await this.storage.get<ProviderImage>(createdAWSImageKey(imageID)))
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
      fastSnapshotRestore?: unknown;
      fastSnapshotRestoreAvailabilityZones?: string[];
    } = await readJson<{
      target?: string;
      os?: string;
      region?: string;
      serverType?: string;
      architecture?: string;
      fastSnapshotRestore?: unknown;
      fastSnapshotRestoreAvailabilityZones?: string[];
    }>(request).catch(() => ({}));
    const target = normalizeAWSImageTarget(
      input.target ?? url.searchParams.get("target") ?? known?.target ?? "linux",
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
      const fallbackOS = known ? (known.os ?? "ubuntu:24.04") : defaultOSImage;
      try {
        imageOS = normalizeOSImage(requestedOS ?? fallbackOS);
      } catch (error) {
        return json({ error: "invalid_os", message: errorMessage(error) }, { status: 400 });
      }
    }
    const rawRegion = input.region ?? url.searchParams.get("region") ?? known?.region ?? "";
    const imageRegion = sanitizeAWSRegion(rawRegion);
    if (rawRegion && !imageRegion) {
      return json(
        { error: "invalid_region", message: "region must be an AWS region name" },
        { status: 400 },
      );
    }
    const metadata: Partial<ProviderImage> = { ...known, target, region: imageRegion };
    const serverType = input.serverType ?? url.searchParams.get("serverType") ?? known?.serverType;
    if (serverType) {
      metadata.serverType = serverType;
    }
    const architecture =
      input.architecture ?? url.searchParams.get("architecture") ?? known?.architecture;
    if (architecture) {
      metadata.architecture = architecture;
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
    };
    await this.storage.put(promotedAWSImageKey(promoted), promoted);
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

  async deleteSSHKey(name: string): Promise<void> {
    await this.client.deleteSSHKey(name);
  }

  hourlyPriceUSD(
    serverType: string,
    config: ReturnType<typeof leaseConfig>,
  ): Promise<number | undefined> {
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
  }): Promise<PromotedImageRecord | undefined> {
    const architecture = awsImageArchitectureForLease(
      config.target,
      config.serverType,
      config.architecture,
    );
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
