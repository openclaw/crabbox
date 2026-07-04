import { readFile } from "node:fs/promises";
import { Script, createContext } from "node:vm";

import { afterEach, describe, expect, it, vi } from "vitest";

import { issueUserToken, sha256Hex } from "../src/auth";
import { EC2SpotClient } from "../src/aws";
import { codeOriginForLease } from "../src/code-origin";
import {
  awsPromotedAMIConfigKey,
  leaseConfig,
  workspaceProviderKeyPrefix,
  type LeaseConfig,
} from "../src/config";
import { routeCoordinatorRequest } from "../src/coordinator-entry";
import {
  AWSProvider,
  FleetDurableObject,
  HetznerProvider,
  bridgeTicketFromRequest,
  boundedSocketReason,
  codeForwardHeaders,
  codeResponseCookiePath,
  codeResponseHeaders,
  flushPendingWebVNC,
  forwardOrBufferWebVNC,
  resetWebVNCBridge,
  recordAzureDeferredCleanup,
  shouldActivateEgressSession,
  workspaceTerminalOriginAllowed,
  type WebVNCBuffer,
} from "../src/fleet";
import { HetznerClient, HetznerProvisioningError } from "../src/hetzner";
import { portalCode } from "../src/portal";
import {
  runtimeAdapterDesktopRelayTimeoutMs,
  runtimeAdapterRelayFrameLimit,
  runtimeAdapterRelayTimeoutMs,
} from "../src/runtime-adapter-relay";
import type {
  Env,
  ExternalRunnerRecord,
  LeaseRecord,
  LeaseRequest,
  ProviderFastSnapshotRestore,
  ProviderImage,
  ProviderMachine,
  ProvisioningAttempt,
  ReadyPoolEntry,
  RunRecord,
} from "../src/types";

afterEach(() => {
  vi.unstubAllGlobals();
});

class MemoryStorage {
  private readonly values = new Map<string, unknown>();
  private alarmTime: number | undefined;

  async get<T>(key: string): Promise<T | undefined> {
    return this.values.get(key) as T | undefined;
  }

  async put<T>(key: string, value: T): Promise<void> {
    this.values.set(key, value);
  }

  async delete(key: string): Promise<void> {
    this.values.delete(key);
  }

  async deleteAlarm(): Promise<void> {
    this.alarmTime = undefined;
  }

  async getAlarm(): Promise<number | null> {
    return this.alarmTime ?? null;
  }

  async setAlarm(time: number): Promise<void> {
    this.alarmTime = time;
  }

  async transaction<T>(callback: (transaction: MemoryStorage) => Promise<T>): Promise<T> {
    return await callback(this);
  }

  async list<T>({ prefix = "" }: { prefix?: string } = {}): Promise<Map<string, T>> {
    const matches = new Map<string, T>();
    for (const [key, value] of this.values) {
      if (key.startsWith(prefix)) {
        matches.set(key, value as T);
      }
    }
    return matches;
  }

  seed<T>(key: string, value: T): void {
    this.values.set(key, value);
  }

  value<T>(key: string): T | undefined {
    return this.values.get(key) as T | undefined;
  }

  alarm(): number | undefined {
    return this.alarmTime;
  }
}

class HookedMemoryStorage extends MemoryStorage {
  beforePut?: (key: string, value: unknown) => Promise<void>;

  override async put<T>(key: string, value: T): Promise<void> {
    await this.beforePut?.(key, value);
    await super.put(key, value);
  }
}

const restrictedBrokerSelectorCases = [
  { provider: "aws" as const, field: "awsAMI", value: "ami-000000000001" },
  { provider: "aws" as const, field: "awsSGID", value: "sg-000000000001" },
  { provider: "aws" as const, field: "awsSubnetID", value: "subnet-000000000001" },
  { provider: "aws" as const, field: "awsProfile", value: "crabbox-runner" },
  { provider: "gcp" as const, field: "gcpProject", value: "other-project" },
  {
    provider: "gcp" as const,
    field: "gcpImage",
    value: "projects/example/global/images/custom-runner",
  },
  { provider: "gcp" as const, field: "gcpNetwork", value: "other-network" },
  { provider: "gcp" as const, field: "gcpSubnet", value: "other-subnet" },
  { provider: "gcp" as const, field: "gcpTags", value: ["other-runner"] },
  {
    provider: "gcp" as const,
    field: "gcpServiceAccount",
    value: "runner@other-project.iam.gserviceaccount.com",
  },
  {
    provider: "azure" as const,
    field: "azureImage",
    value: "Canonical:0001-com-ubuntu-server-noble:24_04-lts-gen2:latest",
  },
  { provider: "azure" as const, field: "azureOSDisk", value: "ephemeral" },
];

class FakeWebSocket {
  readyState = WebSocket.OPEN;
  bufferedAmount = 0;
  closeCode?: number;
  closeReason?: string;
  private attachment: unknown;
  private readonly sent: string[] = [];
  onSend?: (data: string) => void;

  constructor(attachment?: unknown) {
    this.attachment = attachment;
  }

  send(data: string): void {
    this.sent.push(data);
    this.onSend?.(data);
  }

  close(code?: number, reason?: string): void {
    this.closeCode = code;
    this.closeReason = reason;
    this.readyState = WebSocket.CLOSED;
  }

  accept(): void {}

  addEventListener(): void {}

  serializeAttachment(attachment: unknown): void {
    this.attachment = attachment;
  }

  deserializeAttachment(): unknown {
    return this.attachment;
  }

  sentJSON(): unknown[] {
    return this.sent.map((value) => JSON.parse(value) as unknown);
  }
}

describe("runtime adapter relay", () => {
  it("restores unambiguous hibernated bridge sockets after validating lease state", async () => {
    const storage = new MemoryStorage();
    const list = vi.spyOn(storage, "list");
    const lease = testLease({
      id: "cbx_000000000001",
      provider: "external",
      lifecycle: "registered",
      state: "active",
      owner: "alice@example.com",
      org: "example-org",
      expiresAt: new Date(Date.now() + 60_000).toISOString(),
    });
    storage.seed(`lease:${lease.id}`, lease);
    const webVNCAgent = new FakeWebSocket({
      kind: "webvnc-agent",
      leaseID: lease.id,
      id: "agent_restart",
      capabilities: new Set<string>(),
    });
    const restored = [
      new FakeWebSocket({ kind: "code-agent", leaseID: lease.id }),
      new FakeWebSocket({
        kind: "egress-host",
        leaseID: lease.id,
        sessionID: "egress_restart",
        owner: "alice@example.com",
        org: "example-org",
        admin: false,
      }),
      new FakeWebSocket({
        kind: "egress-client",
        leaseID: lease.id,
        sessionID: "egress_restart",
        owner: "alice@example.com",
        org: "example-org",
        admin: false,
      }),
      new FakeWebSocket({
        kind: "runtime-adapter-agent",
        adapterID: "example-adapter",
        owner: "alice@example.com",
        org: "example-org",
      }),
    ];

    const fleet = new FleetDurableObject(
      {
        storage,
        getWebSockets: () => [webVNCAgent, ...restored] as unknown as WebSocket[],
      } as unknown as DurableObjectState,
      { CRABBOX_DEFAULT_ORG: "default-org" } as Env,
    );

    expect(list).not.toHaveBeenCalled();
    for (const socket of [webVNCAgent, ...restored]) {
      expect(socket.closeCode).toBeUndefined();
    }
    expect((await fleet.fetch(request("GET", "/v1/health"))).status).toBe(200);
    expect(list).toHaveBeenCalledTimes(1);
    const relay = fleet as unknown as {
      codeAgents: Map<string, WebSocket>;
      runtimeAdapterAgents: Map<string, WebSocket>;
    };
    expect(relay.codeAgents.get(lease.id)).toBe(restored[0]);
    expect(relay.runtimeAdapterAgents.get("example-adapter")).toBe(restored[3]);
  });

  it("fails closed restored egress sessions without current manage access", async () => {
    const storage = new MemoryStorage();
    const revokedLease = testLease({
      id: "cbx_000000000011",
      provider: "external",
      lifecycle: "registered",
      owner: "owner@example.com",
      org: "example-org",
      expiresAt: new Date(Date.now() + 60_000).toISOString(),
    });
    const legacyLease = testLease({
      ...revokedLease,
      id: "cbx_000000000012",
    });
    const adminLease = testLease({
      ...revokedLease,
      id: "cbx_000000000013",
    });
    for (const lease of [revokedLease, legacyLease, adminLease]) {
      storage.seed(`lease:${lease.id}`, lease);
    }
    const revoked = ["egress-host", "egress-client"].map(
      (kind) =>
        new FakeWebSocket({
          kind,
          leaseID: revokedLease.id,
          sessionID: "egress_revoked",
          owner: "former-manager@example.com",
          org: "example-org",
          admin: false,
        }),
    );
    const legacy = ["egress-host", "egress-client"].map(
      (kind) =>
        new FakeWebSocket({
          kind,
          leaseID: legacyLease.id,
          sessionID: "egress_legacy",
        }),
    );
    const admin = ["egress-host", "egress-client"].map(
      (kind) =>
        new FakeWebSocket({
          kind,
          leaseID: adminLease.id,
          sessionID: "egress_admin",
          owner: "admin@example.com",
          org: "other-org",
          admin: true,
          auth: "github",
          login: "current-admin",
        }),
    );
    const fleet = new FleetDurableObject(
      {
        storage,
        getWebSockets: () => [...revoked, ...legacy, ...admin] as unknown as WebSocket[],
      } as unknown as DurableObjectState,
      {
        CRABBOX_DEFAULT_ORG: "default-org",
        CRABBOX_GITHUB_ADMIN_LOGINS: "current-admin",
      } as Env,
    );

    expect((await fleet.fetch(request("GET", "/v1/health"))).status).toBe(200);
    for (const socket of [...revoked, ...legacy]) {
      expect(socket.closeCode).toBe(1008);
      expect(socket.closeReason).toBe("lease access revoked");
    }
    for (const socket of admin) {
      expect(socket.closeCode).toBeUndefined();
    }
    const relay = fleet as unknown as {
      egressHosts: Map<string, WebSocket>;
      egressClients: Map<string, WebSocket>;
      egressSessions: Map<string, { sessionID: string }>;
    };
    expect(relay.egressHosts.size).toBe(1);
    expect(relay.egressClients.size).toBe(1);
    expect(relay.egressSessions.has(revokedLease.id)).toBe(false);
    expect(relay.egressSessions.has(legacyLease.id)).toBe(false);
    expect(relay.egressSessions.get(adminLease.id)?.sessionID).toBe("egress_admin");
  });

  it("revokes stale admin grants on every restored principal-bearing bridge", async () => {
    const storage = new MemoryStorage();
    const lease = testLease({
      id: "cbx_000000000021",
      provider: "external",
      lifecycle: "registered",
      state: "active",
      owner: "owner@example.com",
      org: "example-org",
      expiresAt: new Date(Date.now() + 60_000).toISOString(),
    });
    storage.seed(`lease:${lease.id}`, lease);
    const revokedGrant = {
      owner: "former-admin@example.com",
      org: "other-org",
      admin: true,
      auth: "github",
      login: "former-admin",
    } as const;
    const codeAgent = new FakeWebSocket({ kind: "code-agent", leaseID: lease.id });
    const revokedCodeViewer = new FakeWebSocket({
      kind: "code-viewer",
      leaseID: lease.id,
      id: "code-revoked-admin",
      portalSessionHash: "a".repeat(64),
      ...revokedGrant,
    });
    const webVNCAgent = new FakeWebSocket({
      kind: "webvnc-agent",
      leaseID: lease.id,
      id: "agent_revoked_admin",
      capabilities: new Set<string>(),
    });
    const revokedWebVNCViewer = new FakeWebSocket({
      kind: "webvnc-viewer",
      leaseID: lease.id,
      id: "viewer_revoked_admin",
      agentID: "agent_revoked_admin",
      label: "former-admin",
      ...revokedGrant,
    });
    const revokedEgress = ["egress-host", "egress-client"].map(
      (kind) =>
        new FakeWebSocket({
          kind,
          leaseID: lease.id,
          sessionID: "egress_revoked_admin",
          ...revokedGrant,
        }),
    );
    const revokedControl = new FakeWebSocket({
      kind: "control",
      clientID: "control-revoked-admin",
      subscriptions: {},
      ...revokedGrant,
    });
    const legacyControl = new FakeWebSocket({
      kind: "control",
      clientID: "control-legacy-admin",
      owner: "legacy-admin@example.com",
      org: "other-org",
      admin: true,
      subscriptions: {},
    });
    const currentGitHubControl = new FakeWebSocket({
      kind: "control",
      clientID: "control-current-github-admin",
      owner: "current-admin@example.com",
      org: "other-org",
      admin: true,
      auth: "github",
      login: "current-admin",
      subscriptions: {},
    });
    const currentAdminToken = "current-admin-token";
    const currentBearerControl = new FakeWebSocket({
      kind: "control",
      clientID: "control-current-bearer-admin",
      owner: "bearer-admin@example.com",
      org: "other-org",
      admin: true,
      auth: "bearer",
      adminTokenHash: await sha256Hex(currentAdminToken),
      subscriptions: {},
    });
    const rotatedBearerControl = new FakeWebSocket({
      kind: "control",
      clientID: "control-rotated-bearer-admin",
      owner: "bearer-admin@example.com",
      org: "other-org",
      admin: true,
      auth: "bearer",
      adminTokenHash: await sha256Hex("old-admin-token"),
      subscriptions: {},
    });
    const sockets = [
      codeAgent,
      revokedCodeViewer,
      webVNCAgent,
      revokedWebVNCViewer,
      ...revokedEgress,
      revokedControl,
      legacyControl,
      currentGitHubControl,
      currentBearerControl,
      rotatedBearerControl,
    ];
    const fleet = new FleetDurableObject(
      {
        storage,
        getWebSockets: () => sockets as unknown as WebSocket[],
      } as unknown as DurableObjectState,
      {
        CRABBOX_ADMIN_TOKEN: currentAdminToken,
        CRABBOX_DEFAULT_ORG: "default-org",
        CRABBOX_GITHUB_ADMIN_LOGINS: "current-admin",
      } as Env,
    );

    expect((await fleet.fetch(request("GET", "/v1/health"))).status).toBe(200);
    for (const socket of [
      revokedCodeViewer,
      revokedWebVNCViewer,
      ...revokedEgress,
      revokedControl,
      legacyControl,
      rotatedBearerControl,
    ]) {
      expect(socket.closeCode).toBe(1008);
      expect(socket.closeReason).toBe("admin access revoked");
    }
    expect(currentGitHubControl.closeCode).toBeUndefined();
    expect(currentBearerControl.closeCode).toBeUndefined();
    expect(codeAgent.sentJSON()).toEqual([
      {
        type: "ws_close",
        id: "code-revoked-admin",
        code: 1008,
        reason: "admin access revoked",
      },
    ]);
  });

  it("rejects hibernated Code viewers whose portal session logged out", async () => {
    const storage = new MemoryStorage();
    const lease = testLease({
      id: "cbx_000000000001",
      provider: "external",
      lifecycle: "registered",
      state: "active",
      owner: "alice@example.com",
      org: "example-org",
      expiresAt: new Date(Date.now() + 60_000).toISOString(),
    });
    storage.seed(`lease:${lease.id}`, lease);
    const portalSessionHash = "a".repeat(64);
    storage.seed(`code-viewer-session-revocation:${portalSessionHash}`, {
      portalSessionHash,
      createdAt: new Date().toISOString(),
      expiresAt: new Date(Date.now() + 60_000).toISOString(),
    });
    const agent = new FakeWebSocket({ kind: "code-agent", leaseID: lease.id });
    const viewer = new FakeWebSocket({
      kind: "code-viewer",
      leaseID: lease.id,
      id: "viewer-revoked",
      auth: "github",
      portalSessionHash,
    });
    const legacyViewer = new FakeWebSocket({
      kind: "code-viewer",
      leaseID: lease.id,
      id: "viewer-legacy",
    });
    const fleet = new FleetDurableObject(
      {
        storage,
        getWebSockets: () => [agent, viewer, legacyViewer] as unknown as WebSocket[],
      } as unknown as DurableObjectState,
      { CRABBOX_DEFAULT_ORG: "default-org" } as Env,
    );

    expect((await fleet.fetch(request("GET", "/v1/health"))).status).toBe(200);
    expect(viewer.closeCode).toBe(1008);
    expect(viewer.closeReason).toBe("portal session ended");
    expect(legacyViewer.closeCode).toBe(1008);
    expect(legacyViewer.closeReason).toBe("portal session ended");
    expect(agent.sentJSON()).toEqual([
      {
        type: "ws_close",
        id: "viewer-revoked",
        code: 1008,
        reason: "portal session ended",
      },
      {
        type: "ws_close",
        id: "viewer-legacy",
        code: 1008,
        reason: "portal session ended",
      },
    ]);
    const relay = fleet as unknown as { codeViewers: Map<string, WebSocket> };
    expect(relay.codeViewers.size).toBe(0);

    await fleet.webSocketMessage(viewer as unknown as WebSocket, "post-logout-frame");
    expect(agent.sentJSON()).toHaveLength(2);
  });

  it("reconciles hibernated viewer principals against the current lease share", async () => {
    const storage = new MemoryStorage();
    const lease = testLease({
      id: "cbx_000000000001",
      provider: "external",
      lifecycle: "registered",
      state: "active",
      owner: "owner@example.com",
      org: "example-org",
      share: { users: { "retained@example.com": "use" } },
      expiresAt: new Date(Date.now() + 60_000).toISOString(),
    });
    storage.seed(`lease:${lease.id}`, lease);
    const codeAgent = new FakeWebSocket({ kind: "code-agent", leaseID: lease.id });
    const revokedCodeViewer = new FakeWebSocket({
      kind: "code-viewer",
      leaseID: lease.id,
      id: "code-revoked",
      auth: "bearer",
      owner: "revoked@example.com",
      org: "other-org",
      admin: false,
    });
    const ownerCodeViewer = new FakeWebSocket({
      kind: "code-viewer",
      leaseID: lease.id,
      id: "code-owner",
      auth: "bearer",
      owner: "owner@example.com",
      org: "example-org",
      admin: false,
    });
    const revokedWebAgent = new FakeWebSocket({
      kind: "webvnc-agent",
      leaseID: lease.id,
      id: "agent_revoked",
      capabilities: new Set<string>(),
    });
    const revokedWebViewer = new FakeWebSocket({
      kind: "webvnc-viewer",
      leaseID: lease.id,
      id: "viewer_revoked",
      agentID: "agent_revoked",
      owner: "revoked@example.com",
      label: "revoked@example.com",
    });
    const retainedWebAgent = new FakeWebSocket({
      kind: "webvnc-agent",
      leaseID: lease.id,
      id: "agent_retained",
      capabilities: new Set<string>(),
    });
    const retainedWebViewer = new FakeWebSocket({
      kind: "webvnc-viewer",
      leaseID: lease.id,
      id: "viewer_retained",
      agentID: "agent_retained",
      owner: "owner@example.com",
      org: "example-org",
      admin: false,
      label: "owner@example.com",
    });
    const legacyOwnerWebAgent = new FakeWebSocket({
      kind: "webvnc-agent",
      leaseID: lease.id,
      id: "agent_legacy_owner",
      capabilities: new Set<string>(),
    });
    const legacyOwnerWebViewer = new FakeWebSocket({
      kind: "webvnc-viewer",
      leaseID: lease.id,
      id: "viewer_legacy_owner",
      agentID: "agent_legacy_owner",
      owner: "owner@example.com",
      label: "owner@example.com",
    });
    const fleet = new FleetDurableObject(
      {
        storage,
        getWebSockets: () =>
          [
            codeAgent,
            revokedCodeViewer,
            ownerCodeViewer,
            revokedWebAgent,
            revokedWebViewer,
            retainedWebAgent,
            retainedWebViewer,
            legacyOwnerWebAgent,
            legacyOwnerWebViewer,
          ] as unknown as WebSocket[],
      } as unknown as DurableObjectState,
      { CRABBOX_DEFAULT_ORG: "default-org" } as Env,
    );

    expect((await fleet.fetch(request("GET", "/v1/health"))).status).toBe(200);
    expect(revokedCodeViewer.closeCode).toBe(1008);
    expect(revokedCodeViewer.closeReason).toBe("lease access revoked");
    expect(ownerCodeViewer.closeCode).toBeUndefined();
    expect(revokedWebViewer.closeCode).toBe(1008);
    expect(revokedWebViewer.closeReason).toBe("lease access revoked");
    expect(revokedWebAgent.closeCode).toBe(1011);
    expect(retainedWebViewer.closeCode).toBeUndefined();
    expect(retainedWebAgent.closeCode).toBeUndefined();
    expect(legacyOwnerWebViewer.closeCode).toBe(1008);
    expect(legacyOwnerWebViewer.closeReason).toBe("lease access revoked");
    expect(legacyOwnerWebAgent.closeCode).toBe(1011);
    expect(codeAgent.sentJSON()).toEqual([
      {
        type: "ws_close",
        id: "code-revoked",
        code: 1008,
        reason: "lease access revoked",
      },
    ]);
  });

  it("fails closed ambiguous and invalid hibernated bridge sockets", () => {
    const storage = new MemoryStorage();
    const list = vi.spyOn(storage, "list");
    const duplicateAdapters = [
      new FakeWebSocket({
        kind: "runtime-adapter-agent",
        adapterID: "example-adapter",
        owner: "alice@example.com",
        org: "example-org",
      }),
      new FakeWebSocket({
        kind: "runtime-adapter-agent",
        adapterID: "example-adapter",
        owner: "alice@example.com",
        org: "example-org",
      }),
    ];
    const competingEgressSessions = ["egress_old", "egress_new"].flatMap((sessionID) => [
      new FakeWebSocket({
        kind: "egress-host",
        leaseID: "cbx_000000000001",
        sessionID,
      }),
      new FakeWebSocket({
        kind: "egress-client",
        leaseID: "cbx_000000000001",
        sessionID,
      }),
    ]);
    const invalid = new FakeWebSocket({ kind: "invalid-restored-attachment" });

    const fleet = new FleetDurableObject(
      {
        storage,
        getWebSockets: () =>
          [...duplicateAdapters, ...competingEgressSessions, invalid] as unknown as WebSocket[],
      } as unknown as DurableObjectState,
      { CRABBOX_DEFAULT_ORG: "default-org" } as Env,
    );

    expect(list).not.toHaveBeenCalled();
    const relay = fleet as unknown as {
      egressHosts: Map<string, WebSocket>;
      runtimeAdapterAgents: Map<string, WebSocket>;
    };
    expect(relay.egressHosts.size).toBe(0);
    expect(relay.runtimeAdapterAgents.size).toBe(0);
    for (const socket of [...duplicateAdapters, ...competingEgressSessions, invalid]) {
      expect(socket.closeCode).toBe(1012);
      expect(socket.closeReason).toBe("coordinator restarted");
    }
  });

  it("retries transient restored-lease reconciliation without dropping sockets", async () => {
    const storage = new MemoryStorage();
    const lease = testLease({
      id: "cbx_000000000002",
      provider: "external",
      lifecycle: "registered",
      state: "active",
      owner: "alice@example.com",
      org: "example-org",
      expiresAt: new Date(Date.now() + 60_000).toISOString(),
    });
    storage.seed(`lease:${lease.id}`, lease);
    const list = vi.spyOn(storage, "list").mockRejectedValueOnce(new Error("storage unavailable"));
    const socket = new FakeWebSocket({ kind: "code-agent", leaseID: lease.id });
    const fleet = new FleetDurableObject(
      {
        storage,
        getWebSockets: () => [socket] as unknown as WebSocket[],
      } as unknown as DurableObjectState,
      { CRABBOX_DEFAULT_ORG: "default-org" } as Env,
    );

    const unavailable = await fleet.fetch(request("GET", "/v1/health"));
    expect(unavailable.status).toBe(503);
    expect(unavailable.headers.get("retry-after")).toBe("1");
    expect(socket.closeCode).toBeUndefined();
    expect((await fleet.fetch(request("GET", "/v1/health"))).status).toBe(200);
    expect(list).toHaveBeenCalledTimes(2);
    expect(socket.closeCode).toBeUndefined();
  });

  it("rejects omitted restored messages and ignores their delayed closes", async () => {
    const fleet = new FleetDurableObject(
      {
        storage: new MemoryStorage(),
        getWebSockets: () => [],
      } as unknown as DurableObjectState,
      { CRABBOX_DEFAULT_ORG: "default-org" } as Env,
    );
    const staleHost = new FakeWebSocket({
      kind: "egress-host",
      leaseID: "cbx_000000000001",
      sessionID: "egress_omitted",
    });
    const client = new FakeWebSocket({
      kind: "egress-client",
      leaseID: "cbx_000000000001",
      sessionID: "egress_omitted",
    });
    const currentAdapter = new FakeWebSocket({
      kind: "runtime-adapter-agent",
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
    });
    const staleAdapter = new FakeWebSocket({
      kind: "runtime-adapter-agent",
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
    });
    const relay = fleet as unknown as {
      egressClients: Map<string, WebSocket>;
      runtimeAdapterAgents: Map<string, WebSocket>;
    };
    relay.egressClients.set("cbx_000000000001\0egress_omitted", client as unknown as WebSocket);
    relay.runtimeAdapterAgents.set("example-adapter", currentAdapter as unknown as WebSocket);

    await fleet.webSocketMessage(staleHost as unknown as WebSocket, "stale frame");
    fleet.webSocketClose(staleAdapter as unknown as WebSocket, 1006, "late close", false);

    expect(staleHost.closeCode).toBe(1012);
    expect(staleHost.closeReason).toBe("coordinator restarted");
    expect(client.sentJSON()).toEqual([]);
    expect(relay.runtimeAdapterAgents.get("example-adapter")).toBe(currentAdapter);
  });

  it("issues owner-scoped tickets and proxies only typed lifecycle requests", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const invalidTicket = await fleet.fetch(
      request("POST", "/v1/adapters/example-adapter/ticket", { headers, body: null }),
    );
    expect(invalidTicket.status).toBe(400);
    await expect(invalidTicket.json()).resolves.toMatchObject({
      error: "invalid_adapter_ticket_request",
    });
    expect(storage.value("runtime-adapter-identity:example-adapter")).toBeUndefined();
    const ticket = await fleet.fetch(
      request("POST", "/v1/adapters/example-adapter/ticket", { headers, body: {} }),
    );
    expect(ticket.status).toBe(200);
    await expect(ticket.json()).resolves.toMatchObject({
      adapterID: "example-adapter",
      ticket: expect.stringMatching(/^adapter_[a-f0-9]{32}$/),
    });
    expect(storage.value("runtime-adapter-identity:example-adapter")).toMatchObject({
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
      claimVersion: 1,
      claimState: "provisional",
      claimExpiresAt: expect.any(String),
    });
    const conflictingTicket = await fleet.fetch(
      request("POST", "/v1/adapters/example-adapter/ticket", {
        headers: {
          "x-crabbox-owner": "mallory@example.com",
          "x-crabbox-org": "example-org",
        },
        body: {},
      }),
    );
    expect(conflictingTicket.status).toBe(409);
    await expect(conflictingTicket.json()).resolves.toMatchObject({
      error: "adapter_id_conflict",
    });

    const socket = new FakeWebSocket({
      kind: "runtime-adapter-agent",
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
    });
    const relayState = fleet as unknown as {
      runtimeAdapterAgents: Map<string, WebSocket>;
    };
    relayState.runtimeAdapterAgents.set("example-adapter", socket as unknown as WebSocket);
    socket.onSend = (data) => {
      const message = JSON.parse(data) as {
        id: string;
        method: string;
        path: string;
        body?: string;
      };
      expect(message).toMatchObject({
        method: "POST",
        path: "/v1/workspaces",
        body: JSON.stringify({ id: "example-workspace-1" }),
      });
      void fleet.webSocketMessage(
        socket as unknown as WebSocket,
        JSON.stringify({
          type: "response",
          id: message.id,
          status: 202,
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ id: "example-workspace-1", status: "provisioning" }),
        }),
      );
    };

    const status = await fleet.fetch(request("GET", "/v1/adapters/example-adapter", { headers }));
    await expect(status.json()).resolves.toEqual({ adapterID: "example-adapter", connected: true });

    const proxied = await fleet.fetch(
      request("POST", "/v1/adapters/example-adapter/proxy/v1/workspaces", {
        headers: { ...headers, "idempotency-key": "example-workspace-1" },
        body: { id: "example-workspace-1" },
      }),
    );
    expect(proxied.status).toBe(202);
    expect(proxied.headers.get("content-type")).toBe("application/json; charset=utf-8");
    await expect(proxied.json()).resolves.toEqual({
      id: "example-workspace-1",
      status: "provisioning",
    });

    const sendsBeforeInvalidDesktopBody = socket.sentJSON().length;
    const invalidDesktopBody = await fleet.fetch(
      request(
        "POST",
        "/v1/adapters/example-adapter/proxy/v1/workspaces/example-workspace-1/connections/desktop",
        { headers, body: {} },
      ),
    );
    expect(invalidDesktopBody.status).toBe(400);
    await expect(invalidDesktopBody.json()).resolves.toMatchObject({
      error: "invalid_request_body",
    });
    expect(socket.sentJSON()).toHaveLength(sendsBeforeInvalidDesktopBody);

    const wrongOwner = await fleet.fetch(
      request("GET", "/v1/adapters/example-adapter/proxy/v1/workspaces/example-workspace-1", {
        headers: { "x-crabbox-owner": "mallory@example.com", "x-crabbox-org": "example-org" },
      }),
    );
    expect(wrongOwner.status).toBe(403);
    await expect(wrongOwner.json()).resolves.toMatchObject({
      error: "runtime_adapter_forbidden",
    });
    const forbiddenPath = await fleet.fetch(
      request(
        "POST",
        "/v1/adapters/example-adapter/proxy/v1/workspaces/example-workspace-1/shell",
        {
          headers,
          body: { command: "id" },
        },
      ),
    );
    expect(forbiddenPath.status).toBe(404);

    const sendsBeforeInvalidKey = socket.sentJSON().length;
    const invalidKey = await fleet.fetch(
      request("POST", "/v1/adapters/example-adapter/proxy/v1/workspaces", {
        headers: { ...headers, "idempotency-key": "x".repeat(129) },
        body: { id: "example-workspace-2" },
      }),
    );
    expect(invalidKey.status).toBe(431);
    await expect(invalidKey.json()).resolves.toMatchObject({
      error: "idempotency_key_too_long",
    });
    expect(socket.sentJSON()).toHaveLength(sendsBeforeInvalidKey);
  });

  it("rejects oversized adapter frames before parsing unknown fields", async () => {
    const fleet = testFleet();
    const socket = new FakeWebSocket({
      kind: "runtime-adapter-agent",
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
    });
    const message = JSON.stringify({
      type: "response",
      id: "unknown-request",
      padding: "x".repeat(runtimeAdapterRelayFrameLimit),
    });
    const relayState = fleet as unknown as {
      runtimeAdapterAgents: Map<string, WebSocket>;
    };
    relayState.runtimeAdapterAgents.set("example-adapter", socket as unknown as WebSocket);

    await fleet.webSocketMessage(socket as unknown as WebSocket, message);

    expect(socket.closeCode).toBe(1009);
    expect(socket.closeReason).toBe("runtime adapter response too large");
  });

  it("bounds pending relay work, applies backpressure, and removes aborted requests", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    storage.seed("runtime-adapter-identity:example-adapter", {
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
      createdAt: "2026-06-01T00:00:00.000Z",
    });
    const socket = new FakeWebSocket({
      kind: "runtime-adapter-agent",
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
    });
    const relayState = fleet as unknown as {
      runtimeAdapterAgents: Map<string, WebSocket>;
      runtimeAdapterPending: Map<string, unknown>;
    };
    relayState.runtimeAdapterAgents.set("example-adapter", socket as unknown as WebSocket);
    const proxyPath = "/v1/adapters/example-adapter/proxy/v1/workspaces/example-workspace-1";
    const pendingRequests = Array.from({ length: 16 }, () =>
      fleet.fetch(request("GET", proxyPath, { headers })),
    );
    await vi.waitFor(() => expect(socket.sentJSON()).toHaveLength(16));

    const limited = await fleet.fetch(request("GET", proxyPath, { headers }));
    expect(limited.status).toBe(429);
    expect(limited.headers.get("retry-after")).toBe("1");
    await expect(limited.json()).resolves.toMatchObject({ error: "runtime_adapter_busy" });

    socket.onSend = (data) => {
      const message = JSON.parse(data) as { id: string; method: string };
      if (message.method !== "DELETE") return;
      void fleet.webSocketMessage(
        socket as unknown as WebSocket,
        JSON.stringify({
          type: "response",
          id: message.id,
          status: 200,
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ id: "example-workspace-1", status: "stopped" }),
        }),
      );
    };
    const successfulDeleteLease = testLease({
      id: "cbx_000000000080",
      provider: "external",
      lifecycle: "registered",
      runtimeAdapterID: "example-adapter",
      runtimeAdapterWorkspaceID: "example-workspace-1",
      runtimeAdapterRegistrationID: "registration-generation-80",
      owner: "alice@example.com",
      org: "example-org",
      keep: true,
      expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
    });
    storage.seed(`lease:${successfulDeleteLease.id}`, successfulDeleteLease);
    const reservedDelete = await fleet.fetch(
      request("POST", `/portal/leases/${successfulDeleteLease.id}/release`, { headers }),
    );
    expect(reservedDelete.status).toBe(303);

    const unfencedDelete = await fleet.fetch(request("DELETE", proxyPath, { headers }));
    expect(unfencedDelete.status).toBe(409);

    socket.onSend = undefined;
    const pendingDeletes = Array.from({ length: 4 }, (_, index) => {
      const suffix = 81 + index;
      const pendingLease = testLease({
        id: `cbx_0000000000${suffix}`,
        provider: "external",
        lifecycle: "registered",
        runtimeAdapterID: "example-adapter",
        runtimeAdapterWorkspaceID: `example-workspace-${suffix}`,
        runtimeAdapterRegistrationID: `registration-generation-${suffix}`,
        owner: "alice@example.com",
        org: "example-org",
        keep: true,
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      });
      storage.seed(`lease:${pendingLease.id}`, pendingLease);
      return fleet.fetch(request("POST", `/portal/leases/${pendingLease.id}/release`, { headers }));
    });
    await vi.waitFor(() => expect(socket.sentJSON()).toHaveLength(21));
    const lease = testLease({
      id: "cbx_000000000085",
      provider: "external",
      lifecycle: "registered",
      runtimeAdapterID: "example-adapter",
      runtimeAdapterWorkspaceID: "example-workspace-85",
      owner: "alice@example.com",
      org: "example-org",
      keep: true,
      expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
    });
    storage.seed(`lease:${lease.id}`, lease);
    const deferredDelete = await fleet.fetch(
      request("POST", `/portal/leases/${lease.id}/release`, { headers }),
    );
    expect(deferredDelete.status).toBe(503);
    expect(storage.value<LeaseRecord>(`lease:${lease.id}`)).toMatchObject({
      runtimeAdapterDeleteRequestedAt: expect.any(String),
    });

    fleet.webSocketClose(socket as unknown as WebSocket, 1006, "gone", false);
    await expect(Promise.all(pendingRequests)).resolves.toHaveLength(16);
    await expect(Promise.all(pendingDeletes)).resolves.toHaveLength(4);
    expect(relayState.runtimeAdapterPending.size).toBe(0);

    const congestedSocket = new FakeWebSocket({
      kind: "runtime-adapter-agent",
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
    });
    congestedSocket.bufferedAmount = 2 * 1024 * 1024;
    relayState.runtimeAdapterAgents.set("example-adapter", congestedSocket as unknown as WebSocket);
    const congested = await fleet.fetch(request("GET", proxyPath, { headers }));
    expect(congested.status).toBe(503);
    expect(congested.headers.get("retry-after")).toBe("1");
    await expect(congested.json()).resolves.toMatchObject({
      error: "runtime_adapter_backpressure",
    });
    expect(congestedSocket.sentJSON()).toHaveLength(0);

    const abortSocket = new FakeWebSocket({
      kind: "runtime-adapter-agent",
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
    });
    relayState.runtimeAdapterAgents.set("example-adapter", abortSocket as unknown as WebSocket);
    const abortController = new AbortController();
    const abortedRequest = fleet.fetch(
      new Request(`https://crabbox.test${proxyPath}`, {
        headers,
        signal: abortController.signal,
      }),
    );
    await vi.waitFor(() => expect(abortSocket.sentJSON()).toHaveLength(1));
    expect(relayState.runtimeAdapterPending.size).toBe(1);
    abortController.abort();

    expect((await abortedRequest).status).toBe(499);
    expect(relayState.runtimeAdapterPending.size).toBe(1);
    const abortedRelayMessage = abortSocket.sentJSON()[0] as { id: string };
    await fleet.webSocketMessage(
      abortSocket as unknown as WebSocket,
      JSON.stringify({
        type: "response",
        id: abortedRelayMessage.id,
        status: 200,
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ id: "example-workspace-1", status: "running" }),
      }),
    );
    expect(relayState.runtimeAdapterPending.size).toBe(0);
  });

  it("recovers only inactive expired provisional runtime adapter claims", async () => {
    const challengerHeaders = {
      "x-crabbox-owner": "mallory@example.com",
      "x-crabbox-org": "example-org",
    };
    const claim = (fleet: FleetDurableObject, adapterID: string) =>
      fleet.fetch(
        request("POST", `/v1/adapters/${adapterID}/ticket`, {
          headers: challengerHeaders,
          body: {},
        }),
      );
    const expectConflict = async (fleet: FleetDurableObject, adapterID: string) => {
      const response = await claim(fleet, adapterID);
      expect(response.status).toBe(409);
      await expect(response.json()).resolves.toMatchObject({ error: "adapter_id_conflict" });
    };

    const recoverableStorage = new MemoryStorage();
    recoverableStorage.seed(
      "runtime-adapter-identity:recoverable-adapter",
      expiredRuntimeAdapterClaim("recoverable-adapter"),
    );
    const recovered = await claim(testFleet(recoverableStorage), "recoverable-adapter");
    expect(recovered.status).toBe(200);
    expect(recoverableStorage.value("runtime-adapter-identity:recoverable-adapter")).toMatchObject({
      owner: "mallory@example.com",
      org: "example-org",
      claimVersion: 1,
      claimState: "provisional",
      claimExpiresAt: expect.any(String),
    });

    const durableStorage = new MemoryStorage();
    durableStorage.seed("runtime-adapter-identity:legacy-adapter", {
      adapterID: "legacy-adapter",
      owner: "alice@example.com",
      org: "example-org",
      createdAt: "2026-01-01T00:00:00.000Z",
    });
    durableStorage.seed("runtime-adapter-identity:confirmed-adapter", {
      ...expiredRuntimeAdapterClaim("confirmed-adapter"),
      claimState: "confirmed",
      confirmedAt: "2026-06-01T00:01:00.000Z",
    });
    durableStorage.seed("runtime-adapter-identity:fresh-adapter", {
      ...expiredRuntimeAdapterClaim("fresh-adapter"),
      claimExpiresAt: new Date(Date.now() + 60_000).toISOString(),
    });
    const durableFleet = testFleet(durableStorage);
    await expectConflict(durableFleet, "legacy-adapter");
    await expectConflict(durableFleet, "confirmed-adapter");
    await expectConflict(durableFleet, "fresh-adapter");

    const ticketStorage = new MemoryStorage();
    ticketStorage.seed(
      "runtime-adapter-identity:ticketed-adapter",
      expiredRuntimeAdapterClaim("ticketed-adapter"),
    );
    ticketStorage.seed("runtime-adapter-ticket:adapter_00000000000000000000000000000000", {
      ticket: "adapter_00000000000000000000000000000000",
      adapterID: "ticketed-adapter",
      owner: "alice@example.com",
      org: "example-org",
      createdAt: new Date().toISOString(),
      expiresAt: new Date(Date.now() + 60_000).toISOString(),
    });
    await expectConflict(testFleet(ticketStorage), "ticketed-adapter");

    const leaseStorage = new MemoryStorage();
    leaseStorage.seed(
      "runtime-adapter-identity:leased-adapter",
      expiredRuntimeAdapterClaim("leased-adapter"),
    );
    const lease = testLease({
      id: "cbx_000000000099",
      provider: "external",
      lifecycle: "registered",
      state: "active",
      owner: "alice@example.com",
      org: "example-org",
      runtimeAdapterID: "leased-adapter",
      runtimeAdapterWorkspaceID: "leased-workspace",
      expiresAt: new Date(Date.now() + 60_000).toISOString(),
    });
    leaseStorage.seed(`lease:${lease.id}`, lease);
    await expectConflict(testFleet(leaseStorage), "leased-adapter");

    const deletingStorage = new MemoryStorage();
    deletingStorage.seed(
      "runtime-adapter-identity:deleting-adapter",
      expiredRuntimeAdapterClaim("deleting-adapter"),
    );
    const deletingLease = testLease({
      id: "cbx_000000000098",
      provider: "external",
      lifecycle: "registered",
      state: "released",
      owner: "alice@example.com",
      org: "example-org",
      runtimeAdapterID: "deleting-adapter",
      runtimeAdapterWorkspaceID: "deleting-workspace",
      runtimeAdapterDeleteRequestedAt: new Date().toISOString(),
      expiresAt: new Date(Date.now() - 60_000).toISOString(),
    });
    deletingStorage.seed(`lease:${deletingLease.id}`, deletingLease);
    await expectConflict(testFleet(deletingStorage), "deleting-adapter");

    const connectedStorage = new MemoryStorage();
    connectedStorage.seed(
      "runtime-adapter-identity:connected-adapter",
      expiredRuntimeAdapterClaim("connected-adapter"),
    );
    const connectedFleet = testFleet(connectedStorage);
    const relayState = connectedFleet as unknown as {
      runtimeAdapterAgents: Map<string, WebSocket>;
    };
    relayState.runtimeAdapterAgents.set(
      "connected-adapter",
      new FakeWebSocket() as unknown as WebSocket,
    );
    await expectConflict(connectedFleet, "connected-adapter");

    const pendingStorage = new MemoryStorage();
    pendingStorage.seed(
      "runtime-adapter-identity:pending-adapter",
      expiredRuntimeAdapterClaim("pending-adapter"),
    );
    const pendingFleet = testFleet(pendingStorage);
    const pendingState = pendingFleet as unknown as {
      runtimeAdapterPending: Map<string, { adapterID: string }>;
    };
    pendingState.runtimeAdapterPending.set("pending-request", { adapterID: "pending-adapter" });
    await expectConflict(pendingFleet, "pending-adapter");
  });

  it("isolates ordinary relay capacity by owner while preserving delete capacity", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const aliceHeaders = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const bobHeaders = {
      "x-crabbox-owner": "bob@example.com",
      "x-crabbox-org": "example-org",
    };
    const relayState = fleet as unknown as {
      runtimeAdapterAgents: Map<string, WebSocket>;
    };
    const connect = (adapterID: string, owner: string): FakeWebSocket => {
      storage.seed(`runtime-adapter-identity:${adapterID}`, {
        adapterID,
        owner,
        org: "example-org",
        createdAt: "2026-06-01T00:00:00.000Z",
      });
      const socket = new FakeWebSocket({
        kind: "runtime-adapter-agent",
        adapterID,
        owner,
        org: "example-org",
      });
      relayState.runtimeAdapterAgents.set(adapterID, socket as unknown as WebSocket);
      return socket;
    };
    const firstAlice = connect("alice-adapter-1", "alice@example.com");
    const secondAlice = connect("alice-adapter-2", "alice@example.com");
    const thirdAlice = connect("alice-adapter-3", "alice@example.com");
    const bob = connect("bob-adapter", "bob@example.com");
    const proxySuffix = "/proxy/v1/workspaces/example-workspace";
    const alicePending = [
      ...Array.from({ length: 16 }, () =>
        fleet.fetch(
          request("GET", `/v1/adapters/alice-adapter-1${proxySuffix}`, {
            headers: aliceHeaders,
          }),
        ),
      ),
      ...Array.from({ length: 16 }, () =>
        fleet.fetch(
          request("GET", `/v1/adapters/alice-adapter-2${proxySuffix}`, {
            headers: aliceHeaders,
          }),
        ),
      ),
    ];
    await vi.waitFor(() => expect(firstAlice.sentJSON()).toHaveLength(16));
    await vi.waitFor(() => expect(secondAlice.sentJSON()).toHaveLength(16));

    const ownerLimited = await fleet.fetch(
      request("GET", `/v1/adapters/alice-adapter-3${proxySuffix}`, { headers: aliceHeaders }),
    );
    expect(ownerLimited.status).toBe(429);
    expect(thirdAlice.sentJSON()).toHaveLength(0);

    thirdAlice.onSend = (data) => {
      const message = JSON.parse(data) as { id: string };
      void fleet.webSocketMessage(
        thirdAlice as unknown as WebSocket,
        JSON.stringify({ type: "response", id: message.id, status: 204 }),
      );
    };
    const deleteLease = testLease({
      id: "cbx_000000000072",
      provider: "external",
      lifecycle: "registered",
      runtimeAdapterID: "alice-adapter-3",
      runtimeAdapterWorkspaceID: "example-workspace",
      runtimeAdapterRegistrationID: "registration-generation-72",
      owner: "alice@example.com",
      org: "example-org",
      keep: true,
    });
    storage.seed(`lease:${deleteLease.id}`, deleteLease);
    const reservedDelete = await fleet.fetch(
      request("POST", `/portal/leases/${deleteLease.id}/release`, {
        headers: aliceHeaders,
      }),
    );
    expect(reservedDelete.status).toBe(303);

    const unfencedDelete = await fleet.fetch(
      request("DELETE", `/v1/adapters/alice-adapter-3${proxySuffix}`, {
        headers: aliceHeaders,
      }),
    );
    expect(unfencedDelete.status).toBe(409);
    await expect(unfencedDelete.json()).resolves.toMatchObject({
      error: "runtime_adapter_delete_requires_lease",
    });
    expect(thirdAlice.sentJSON()).toHaveLength(1);

    bob.onSend = (data) => {
      const message = JSON.parse(data) as { id: string };
      void fleet.webSocketMessage(
        bob as unknown as WebSocket,
        JSON.stringify({
          type: "response",
          id: message.id,
          status: 200,
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ id: "example-workspace", status: "running" }),
        }),
      );
    };
    const otherOwner = await fleet.fetch(
      request("GET", `/v1/adapters/bob-adapter${proxySuffix}`, { headers: bobHeaders }),
    );
    expect(otherOwner.status).toBe(200);

    fleet.webSocketClose(firstAlice as unknown as WebSocket, 1006, "gone", false);
    fleet.webSocketClose(secondAlice as unknown as WebSocket, 1006, "gone", false);
    await expect(Promise.all(alicePending)).resolves.toHaveLength(32);
  });

  it("deletes a generation-bound adapter workspace through the JSON release API", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const lease = testLease({
      id: "cbx_000000000071",
      provider: "external",
      lifecycle: "registered",
      runtimeAdapterID: "example-adapter",
      runtimeAdapterWorkspaceID: "example-workspace-71",
      runtimeAdapterRegistrationID: "registration-generation-71",
      owner: "alice@example.com",
      org: "example-org",
      keep: true,
      expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
    });
    const legacyLease = testLease({
      ...lease,
      id: "cbx_000000000070",
      runtimeAdapterWorkspaceID: "example-workspace-70",
      runtimeAdapterRegistrationID: undefined,
    });
    storage.seed("runtime-adapter-identity:example-adapter", {
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
      createdAt: "2026-06-01T00:00:00.000Z",
    });
    storage.seed(`lease:${lease.id}`, lease);
    storage.seed(`lease:${legacyLease.id}`, legacyLease);

    const unfenced = await fleet.fetch(
      request("POST", `/v1/leases/${legacyLease.id}/release`, {
        headers,
        body: { delete: true },
      }),
    );
    expect(unfenced.status).toBe(409);
    await expect(unfenced.json()).resolves.toMatchObject({
      error: "runtime_adapter_registration_required",
    });
    expect(storage.value<LeaseRecord>(`lease:${legacyLease.id}`)?.state).toBe("active");

    const wrongOwner = await fleet.fetch(
      request("POST", `/v1/leases/${lease.id}/release`, {
        headers: {
          "x-crabbox-owner": "mallory@example.com",
          "x-crabbox-org": "example-org",
        },
        body: { delete: true },
      }),
    );
    expect(wrongOwner.status).toBe(404);

    const socket = new FakeWebSocket({
      kind: "runtime-adapter-agent",
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
    });
    const relayState = fleet as unknown as {
      runtimeAdapterAgents: Map<string, WebSocket>;
    };
    relayState.runtimeAdapterAgents.set("example-adapter", socket as unknown as WebSocket);
    socket.onSend = (data) => {
      const message = JSON.parse(data) as {
        id: string;
        method: string;
        path: string;
        body?: string;
      };
      expect(message).toMatchObject({
        method: "DELETE",
        path: "/v1/workspaces/example-workspace-71",
      });
      expect(message.body).toBeUndefined();
      void fleet.webSocketMessage(
        socket as unknown as WebSocket,
        JSON.stringify({
          type: "response",
          id: message.id,
          status: 202,
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ id: "example-workspace-71", status: "stopping" }),
        }),
      );
    };

    const deleted = await fleet.fetch(
      request("POST", `/v1/leases/${lease.id}/release`, {
        headers,
        body: { delete: true },
      }),
    );
    expect(deleted.status).toBe(202);
    await expect(deleted.json()).resolves.toMatchObject({
      status: "deleting",
      lease: {
        id: lease.id,
        owner: "alice@example.com",
        org: "example-org",
        runtimeAdapterID: "example-adapter",
        runtimeAdapterWorkspaceID: "example-workspace-71",
        runtimeAdapterRegistrationID: "registration-generation-71",
        runtimeAdapterDeleteRequestedAt: expect.any(String),
      },
    });
    expect(socket.sentJSON()).toHaveLength(1);

    const rawDelete = await fleet.fetch(
      request("DELETE", "/v1/adapters/example-adapter/proxy/v1/workspaces/example-workspace-71", {
        headers,
      }),
    );
    expect(rawDelete.status).toBe(409);
    await expect(rawDelete.json()).resolves.toMatchObject({
      error: "runtime_adapter_delete_requires_lease",
    });
    expect(socket.sentJSON()).toHaveLength(1);

    const wrongGeneration = await fleet.fetch(
      request("POST", `/v1/leases/${lease.id}/release`, {
        headers,
        body: {
          runtimeAdapterDeleteCompletion: {
            adapterID: "example-adapter",
            workspaceID: "example-workspace-71",
            registrationID: "registration-generation-other",
            status: "absent",
          },
        },
      }),
    );
    expect(wrongGeneration.status).toBe(409);
    expect(storage.value<LeaseRecord>(`lease:${lease.id}`)?.state).toBe("active");
  });

  it("routes portal Delete through the bound adapter and keeps deregistration as fallback", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const lease = testLease({
      id: "cbx_000000000099",
      slug: "example-box",
      provider: "external",
      lifecycle: "registered",
      runtimeAdapterID: "example-adapter",
      runtimeAdapterWorkspaceID: "example-workspace-99",
      runtimeAdapterRegistrationID: "registration-generation-99",
      owner: "alice@example.com",
      org: "example-org",
      keep: true,
      expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
    });
    storage.seed("runtime-adapter-identity:example-adapter", {
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
      createdAt: "2026-06-01T00:00:00.000Z",
    });
    storage.seed(`lease:${lease.id}`, lease);

    const socket = new FakeWebSocket({
      kind: "runtime-adapter-agent",
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
    });
    const relayState = fleet as unknown as {
      runtimeAdapterAgents: Map<string, WebSocket>;
    };
    relayState.runtimeAdapterAgents.set("example-adapter", socket as unknown as WebSocket);
    let workspaceStatus = "stopping";
    const initialResponse = deferred<void>();
    let holdInitialResponse = true;
    socket.onSend = (data) => {
      const message = JSON.parse(data) as { id: string; method: string; path: string };
      expect(message).toMatchObject({
        method: "DELETE",
        path: "/v1/workspaces/example-workspace-99",
      });
      const respond = () =>
        fleet.webSocketMessage(
          socket as unknown as WebSocket,
          JSON.stringify({
            type: "response",
            id: message.id,
            status:
              workspaceStatus === "stopping"
                ? 202
                : workspaceStatus === "rejected"
                  ? 403
                  : workspaceStatus === "rate-limited"
                    ? 429
                    : 200,
            headers: { "content-type": "application/json" },
            body: JSON.stringify({
              id: "example-workspace-99",
              status: workspaceStatus,
            }),
          }),
        );
      if (holdInitialResponse) {
        holdInitialResponse = false;
        void initialResponse.promise.then(respond);
      } else {
        void respond();
      }
    };

    const page = await fleet.fetch(request("GET", "/portal", { headers }));
    const pageHTML = await page.text();
    expect(pageHTML).toContain('data-release-kind="adapter"');
    expect(pageHTML).toContain('title="Delete example-box workspace"');
    expect(pageHTML).toContain("permanently deletes the external workspace");

    const requestedPromise = fleet.fetch(
      request("POST", `/portal/leases/${lease.id}/release`, { headers }),
    );
    await vi.waitFor(() => expect(socket.sentJSON()).toHaveLength(1));
    const inFlight = storage.value<LeaseRecord>(`lease:${lease.id}`);
    expect(inFlight).toMatchObject({
      state: "active",
      runtimeAdapterDeleteRequestedAt: expect.any(String),
    });
    initialResponse.resolve();
    const requested = await requestedPromise;
    expect(requested.status).toBe(303);
    expect(storage.value<LeaseRecord>(`lease:${lease.id}`)).toMatchObject({
      state: "active",
      runtimeAdapterDeleteRequestedAt: expect.any(String),
      runtimeAdapterDeleteRetryAt: expect.any(String),
    });

    workspaceStatus = "failed";
    const failed = await fleet.fetch(
      request("POST", `/portal/leases/${lease.id}/release`, { headers }),
    );
    expect(failed.status).toBe(502);
    expect(storage.value<LeaseRecord>(`lease:${lease.id}`)?.state).toBe("active");

    workspaceStatus = "rejected";
    const manuallyRejected = await fleet.fetch(
      request("POST", `/portal/leases/${lease.id}/release`, { headers }),
    );
    expect(manuallyRejected.status).toBe(502);
    expect(storage.value<LeaseRecord>(`lease:${lease.id}`)).toMatchObject({
      runtimeAdapterDeleteRequestedAt: expect.any(String),
      runtimeAdapterDeleteRetryAt: expect.any(String),
    });

    workspaceStatus = "stopping";
    const manuallyRearmed = await fleet.fetch(
      request("POST", `/portal/leases/${lease.id}/release`, { headers }),
    );
    expect(manuallyRearmed.status).toBe(303);

    const rateLimitedPending = storage.value<LeaseRecord>(`lease:${lease.id}`);
    expect(rateLimitedPending).toBeDefined();
    if (rateLimitedPending) {
      rateLimitedPending.runtimeAdapterDeleteRetryAt = new Date(0).toISOString();
      storage.seed(`lease:${lease.id}`, rateLimitedPending);
    }
    workspaceStatus = "rate-limited";
    await fleet.alarm();
    expect(storage.value<LeaseRecord>(`lease:${lease.id}`)).toMatchObject({
      state: "active",
      runtimeAdapterDeleteRequestedAt: expect.any(String),
      runtimeAdapterDeleteRetryAt: expect.any(String),
    });

    const rejectedPending = storage.value<LeaseRecord>(`lease:${lease.id}`);
    expect(rejectedPending).toBeDefined();
    if (rejectedPending) {
      rejectedPending.expiresAt = new Date(Date.now() + 60 * 60 * 1000).toISOString();
      rejectedPending.runtimeAdapterDeleteRetryAt = new Date(0).toISOString();
      storage.seed(`lease:${lease.id}`, rejectedPending);
    }
    workspaceStatus = "rejected";
    await fleet.alarm();
    expect(storage.value<LeaseRecord>(`lease:${lease.id}`)).toMatchObject({
      state: "active",
      runtimeAdapterDeleteRequestedAt: expect.any(String),
      runtimeAdapterDeleteRetryAt: expect.any(String),
      runtimeAdapterDeleteError: "runtime_adapter_http_403",
    });

    workspaceStatus = "stopping";
    const rearmed = await fleet.fetch(
      request("POST", `/portal/leases/${lease.id}/release`, { headers }),
    );
    expect(rearmed.status).toBe(303);

    const pending = storage.value<LeaseRecord>(`lease:${lease.id}`);
    expect(pending).toBeDefined();
    if (pending) {
      pending.runtimeAdapterDeleteRetryAt = new Date(0).toISOString();
      storage.seed(`lease:${lease.id}`, pending);
    }
    workspaceStatus = "signed-url-secret";
    await fleet.alarm();
    expect(storage.value<LeaseRecord>(`lease:${lease.id}`)).toMatchObject({
      state: "active",
      runtimeAdapterDeleteError: "runtime_adapter_invalid_response",
    });

    workspaceStatus = "stopped";
    const retry = storage.value<LeaseRecord>(`lease:${lease.id}`);
    expect(retry).toBeDefined();
    if (retry) {
      retry.runtimeAdapterDeleteRetryAt = new Date(0).toISOString();
      storage.seed(`lease:${lease.id}`, retry);
    }
    await fleet.alarm();
    expect(storage.value<LeaseRecord>(`lease:${lease.id}`)).toMatchObject({
      state: "active",
      runtimeAdapterDeleteRequestedAt: expect.any(String),
    });
    expect(
      storage.value<LeaseRecord>(`lease:${lease.id}`)?.runtimeAdapterDeleteDispatchUntil,
    ).toBeUndefined();
    expect((await confirmRuntimeAdapterAbsence(fleet, lease, headers)).status).toBe(200);
    expect(storage.value<LeaseRecord>(`lease:${lease.id}`)).toMatchObject({ state: "released" });
    expect(
      storage.value<LeaseRecord>(`lease:${lease.id}`)?.runtimeAdapterDeleteRequestedAt,
    ).toBeUndefined();
  });

  it("accepts only an owner-scoped exact confirmed-absence completion", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const managerHeaders = {
      "x-crabbox-owner": "manager@example.com",
      "x-crabbox-org": "example-org",
    };
    storage.seed("runtime-adapter-identity:example-adapter", {
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
      createdAt: "2026-06-01T00:00:00.000Z",
    });
    const lease = testLease({
      id: "cbx_000000000078",
      provider: "external",
      lifecycle: "registered",
      runtimeAdapterID: "example-adapter",
      runtimeAdapterWorkspaceID: "example-workspace-78",
      runtimeAdapterRegistrationID: "registration-generation-78",
      runtimeAdapterDeleteRequestedAt: "2026-06-13T00:00:00.000Z",
      runtimeAdapterDeleteClaimID: "delete-claim",
      runtimeAdapterDeleteRetryAt: new Date(Date.now() + 60_000).toISOString(),
      owner: "alice@example.com",
      org: "example-org",
      share: { users: { "manager@example.com": "manage" } },
      keep: true,
    });
    storage.seed(`lease:${lease.id}`, lease);
    const completion = {
      adapterID: "example-adapter",
      workspaceID: "example-workspace-78",
      registrationID: "registration-generation-78",
      status: "absent",
    };

    const generationMissing = await fleet.fetch(
      request("POST", `/v1/leases/${lease.id}/release`, {
        headers,
        body: {
          runtimeAdapterDeleteCompletion: {
            adapterID: completion.adapterID,
            workspaceID: completion.workspaceID,
            status: completion.status,
          },
        },
      }),
    );
    expect(generationMissing.status).toBe(400);

    const manager = await fleet.fetch(
      request("POST", `/v1/leases/${lease.id}/release`, {
        headers: managerHeaders,
        body: { runtimeAdapterDeleteCompletion: completion },
      }),
    );
    expect(manager.status).toBe(403);

    const mismatch = await fleet.fetch(
      request("POST", `/v1/leases/${lease.id}/release`, {
        headers,
        body: {
          runtimeAdapterDeleteCompletion: {
            ...completion,
            workspaceID: "example-workspace-other",
          },
        },
      }),
    );
    expect(mismatch.status).toBe(409);
    expect(storage.value<LeaseRecord>(`lease:${lease.id}`)).toMatchObject({
      state: "active",
      runtimeAdapterDeleteClaimID: "delete-claim",
    });

    const completed = await fleet.fetch(
      request("POST", `/v1/leases/${lease.id}/release`, {
        headers,
        body: { runtimeAdapterDeleteCompletion: completion },
      }),
    );
    expect(completed.status).toBe(200);
    await expect(completed.json()).resolves.toMatchObject({ lease: { state: "released" } });
    expect(
      storage.value<LeaseRecord>(`lease:${lease.id}`)?.runtimeAdapterDeleteRequestedAt,
    ).toBeUndefined();

    const repeated = await fleet.fetch(
      request("POST", `/v1/leases/${lease.id}/release`, {
        headers,
        body: { runtimeAdapterDeleteCompletion: completion },
      }),
    );
    expect(repeated.status).toBe(200);
    await expect(repeated.json()).resolves.toMatchObject({ lease: { state: "released" } });

    const unboundReactivation = await fleet.fetch(
      request("PUT", `/v1/leases/${lease.id}/registration`, {
        headers,
        body: {
          provider: "external",
          target: "linux",
          host: "replacement.example.test",
        },
      }),
    );
    expect(unboundReactivation.status).toBe(200);
    await expect(unboundReactivation.json()).resolves.toMatchObject({
      lease: {
        state: "active",
        runtimeAdapterRegistrationID: "registration-generation-78",
      },
    });

    const replayedRegistration = await fleet.fetch(
      request("PUT", `/v1/leases/${lease.id}/registration`, {
        headers,
        body: {
          provider: "external",
          target: "linux",
          host: "replacement.example.test",
          runtimeAdapterID: "example-adapter",
          runtimeAdapterWorkspaceID: "example-workspace-78",
          runtimeAdapterRegistrationID: "registration-generation-78",
        },
      }),
    );
    expect(replayedRegistration.status).toBe(409);
    await expect(replayedRegistration.json()).resolves.toMatchObject({
      error: "runtime_adapter_registration_replayed",
    });

    const reactivated = await fleet.fetch(
      request("PUT", `/v1/leases/${lease.id}/registration`, {
        headers,
        body: {
          provider: "external",
          target: "linux",
          host: "replacement.example.test",
          runtimeAdapterID: "example-adapter",
          runtimeAdapterWorkspaceID: "example-workspace-78",
          runtimeAdapterRegistrationID: "registration-generation-78-next",
        },
      }),
    );
    expect(reactivated.status).toBe(200);
    await expect(reactivated.json()).resolves.toMatchObject({
      lease: {
        state: "active",
        runtimeAdapterRegistrationID: "registration-generation-78-next",
      },
    });

    const delayedCompletion = await fleet.fetch(
      request("POST", `/v1/leases/${lease.id}/release`, {
        headers,
        body: { runtimeAdapterDeleteCompletion: completion },
      }),
    );
    expect(delayedCompletion.status).toBe(409);
    expect(storage.value<LeaseRecord>(`lease:${lease.id}`)).toMatchObject({
      state: "active",
      runtimeAdapterRegistrationID: "registration-generation-78-next",
    });

    const autonomouslyStopped = testLease({
      id: "cbx_000000000077",
      provider: "external",
      lifecycle: "registered",
      runtimeAdapterID: "example-adapter",
      runtimeAdapterWorkspaceID: "example-workspace-77",
      runtimeAdapterRegistrationID: "registration-generation-77",
      owner: "alice@example.com",
      org: "example-org",
      keep: true,
    });
    storage.seed(`lease:${autonomouslyStopped.id}`, autonomouslyStopped);
    const autonomousCompletion = await fleet.fetch(
      request("POST", `/v1/leases/${autonomouslyStopped.id}/release`, {
        headers,
        body: {
          runtimeAdapterDeleteCompletion: {
            adapterID: "example-adapter",
            workspaceID: "example-workspace-77",
            registrationID: "registration-generation-77",
            status: "absent",
          },
        },
      }),
    );
    expect(autonomousCompletion.status).toBe(200);
    await expect(autonomousCompletion.json()).resolves.toMatchObject({
      lease: { state: "released" },
    });

    const legacyPending = testLease({
      id: "cbx_000000000076",
      provider: "external",
      lifecycle: "registered",
      runtimeAdapterID: "example-adapter",
      runtimeAdapterWorkspaceID: "example-workspace-76",
      runtimeAdapterDeleteRequestedAt: "2026-06-13T00:00:00.000Z",
      owner: "alice@example.com",
      org: "example-org",
      share: { users: { "manager@example.com": "manage" } },
      keep: true,
    });
    storage.seed(`lease:${legacyPending.id}`, legacyPending);
    const legacyCompletionBody = {
      runtimeAdapterLegacyDeleteCompletion: {
        adapterID: "example-adapter",
        workspaceID: "example-workspace-76",
        status: "absent",
      },
    };
    const legacyManagerCompletion = await fleet.fetch(
      request("POST", `/v1/leases/${legacyPending.id}/release`, {
        headers: managerHeaders,
        body: legacyCompletionBody,
      }),
    );
    expect(legacyManagerCompletion.status).toBe(403);
    expect(storage.value<LeaseRecord>(`lease:${legacyPending.id}`)).toMatchObject({
      state: "active",
      runtimeAdapterDeleteRequestedAt: "2026-06-13T00:00:00.000Z",
    });
    const genericLegacyRelease = await fleet.fetch(
      request("POST", `/v1/leases/${legacyPending.id}/release`, {
        headers,
        body: { delete: false },
      }),
    );
    expect(genericLegacyRelease.status).toBe(409);
    await expect(genericLegacyRelease.json()).resolves.toMatchObject({
      error: "runtime_adapter_delete_pending",
    });
    const legacyCompletion = await fleet.fetch(
      request("POST", `/v1/leases/${legacyPending.id}/release`, {
        headers,
        body: legacyCompletionBody,
      }),
    );
    expect(legacyCompletion.status).toBe(200);
    await expect(legacyCompletion.json()).resolves.toMatchObject({
      lease: { state: "released" },
    });

    const expiredLegacyPending = testLease({
      id: "cbx_000000000074",
      provider: "external",
      lifecycle: "registered",
      state: "expired",
      runtimeAdapterID: "example-adapter",
      runtimeAdapterWorkspaceID: "example-workspace-74",
      runtimeAdapterDeleteRequestedAt: "2026-06-13T00:00:00.000Z",
      owner: "alice@example.com",
      org: "example-org",
      keep: true,
    });
    storage.seed(`lease:${expiredLegacyPending.id}`, expiredLegacyPending);
    const expiredLegacyCompletion = await fleet.fetch(
      request("POST", `/v1/leases/${expiredLegacyPending.id}/release`, {
        headers,
        body: {
          runtimeAdapterLegacyDeleteCompletion: {
            adapterID: "example-adapter",
            workspaceID: "example-workspace-74",
            status: "absent",
          },
        },
      }),
    );
    expect(expiredLegacyCompletion.status).toBe(200);
    await expect(expiredLegacyCompletion.json()).resolves.toMatchObject({
      lease: { state: "expired" },
    });
    expect(
      storage.value<LeaseRecord>(`lease:${expiredLegacyPending.id}`)
        ?.runtimeAdapterDeleteRequestedAt,
    ).toBeUndefined();
  });

  it("keeps completion fenced until a generation-scoped delete dispatch expires", async () => {
    vi.useFakeTimers();
    try {
      const storage = new MemoryStorage();
      const fleet = testFleet(storage);
      const headers = {
        "x-crabbox-owner": "alice@example.com",
        "x-crabbox-org": "example-org",
      };
      const lease = testLease({
        id: "cbx_000000000075",
        provider: "external",
        lifecycle: "registered",
        runtimeAdapterID: "example-adapter",
        runtimeAdapterWorkspaceID: "example-workspace-75",
        runtimeAdapterRegistrationID: "registration-generation-75",
        owner: "alice@example.com",
        org: "example-org",
        keep: true,
      });
      storage.seed("runtime-adapter-identity:example-adapter", {
        adapterID: "example-adapter",
        owner: "alice@example.com",
        org: "example-org",
        createdAt: "2026-06-01T00:00:00.000Z",
      });
      storage.seed(`lease:${lease.id}`, lease);
      const socket = new FakeWebSocket({
        kind: "runtime-adapter-agent",
        adapterID: "example-adapter",
        owner: "alice@example.com",
        org: "example-org",
      });
      const relayState = fleet as unknown as {
        runtimeAdapterAgents: Map<string, WebSocket>;
      };
      relayState.runtimeAdapterAgents.set("example-adapter", socket as unknown as WebSocket);
      const sent = deferred<void>();
      socket.onSend = () => sent.resolve();

      const deletion = fleet.fetch(
        request("POST", `/portal/leases/${lease.id}/release`, { headers }),
      );
      await sent.promise;
      let completionSettled = false;
      const completion = fleet
        .fetch(
          request("POST", `/v1/leases/${lease.id}/release`, {
            headers,
            body: {
              runtimeAdapterDeleteCompletion: {
                adapterID: "example-adapter",
                workspaceID: "example-workspace-75",
                registrationID: "registration-generation-75",
                status: "absent",
              },
            },
          }),
        )
        .then((response) => {
          completionSettled = true;
          return response;
        });
      await Promise.resolve();
      expect(completionSettled).toBe(false);

      await vi.advanceTimersByTimeAsync(runtimeAdapterRelayTimeoutMs);
      expect((await deletion).status).toBe(503);
      const fenced = await completion;
      expect(fenced.status).toBe(409);
      expect(fenced.headers.get("retry-after")).toBe("1");
      await expect(fenced.json()).resolves.toMatchObject({
        error: "runtime_adapter_delete_in_flight",
      });
      expect(storage.value<LeaseRecord>(`lease:${lease.id}`)).toMatchObject({
        state: "active",
        runtimeAdapterDeleteDispatchUntil: expect.any(String),
      });

      await vi.advanceTimersByTimeAsync(1_000);
      const retried = await fleet.fetch(
        request("POST", `/v1/leases/${lease.id}/release`, {
          headers,
          body: {
            runtimeAdapterDeleteCompletion: {
              adapterID: "example-adapter",
              workspaceID: "example-workspace-75",
              registrationID: "registration-generation-75",
              status: "absent",
            },
          },
        }),
      );
      expect(retried.status).toBe(200);
      await expect(retried.json()).resolves.toMatchObject({ lease: { state: "released" } });
    } finally {
      vi.useRealTimers();
    }
  });

  it("keeps the delete fence after an ambiguous connector timeout", async () => {
    vi.useFakeTimers();
    try {
      const storage = new MemoryStorage();
      const fleet = testFleet(storage);
      const headers = {
        "x-crabbox-owner": "alice@example.com",
        "x-crabbox-org": "example-org",
      };
      const lease = testLease({
        id: "cbx_000000000073",
        provider: "external",
        lifecycle: "registered",
        runtimeAdapterID: "example-adapter",
        runtimeAdapterWorkspaceID: "example-workspace-73",
        runtimeAdapterRegistrationID: "registration-generation-73",
        owner: "alice@example.com",
        org: "example-org",
        keep: true,
      });
      storage.seed("runtime-adapter-identity:example-adapter", {
        adapterID: "example-adapter",
        owner: "alice@example.com",
        org: "example-org",
        createdAt: "2026-06-01T00:00:00.000Z",
      });
      storage.seed(`lease:${lease.id}`, lease);
      const socket = new FakeWebSocket({
        kind: "runtime-adapter-agent",
        adapterID: "example-adapter",
        owner: "alice@example.com",
        org: "example-org",
      });
      const relayState = fleet as unknown as {
        runtimeAdapterAgents: Map<string, WebSocket>;
      };
      relayState.runtimeAdapterAgents.set("example-adapter", socket as unknown as WebSocket);
      socket.onSend = (data) => {
        const message = JSON.parse(data) as { id: string };
        void fleet.webSocketMessage(
          socket as unknown as WebSocket,
          JSON.stringify({
            type: "response",
            id: message.id,
            status: 504,
            headers: { "content-type": "application/json" },
            body: JSON.stringify({
              error: { code: "adapter_timeout", message: "local adapter request timed out" },
            }),
          }),
        );
      };

      const deletion = await fleet.fetch(
        request("POST", `/portal/leases/${lease.id}/release`, { headers }),
      );
      expect(deletion.status).toBe(503);
      const fenced = await confirmRuntimeAdapterAbsence(fleet, lease, headers);
      expect(fenced.status).toBe(409);
      await expect(fenced.json()).resolves.toMatchObject({
        error: "runtime_adapter_delete_in_flight",
      });

      await vi.advanceTimersByTimeAsync(runtimeAdapterRelayTimeoutMs + 2_000);
      const completed = await confirmRuntimeAdapterAbsence(fleet, lease, headers);
      expect(completed.status).toBe(200);
      await expect(completed.json()).resolves.toMatchObject({ lease: { state: "released" } });
    } finally {
      vi.useRealTimers();
    }
  });

  it("keeps delete intent after a post-send adapter disconnect and retries to confirmation", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const lease = testLease({
      id: "cbx_000000000089",
      provider: "external",
      lifecycle: "registered",
      runtimeAdapterID: "example-adapter",
      runtimeAdapterWorkspaceID: "example-workspace-89",
      runtimeAdapterRegistrationID: "registration-generation-89",
      owner: "alice@example.com",
      org: "example-org",
      keep: true,
      expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
    });
    storage.seed("runtime-adapter-identity:example-adapter", {
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
      createdAt: "2026-06-01T00:00:00.000Z",
    });
    storage.seed(`lease:${lease.id}`, lease);
    const socket = new FakeWebSocket({
      kind: "runtime-adapter-agent",
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
    });
    const relayState = fleet as unknown as {
      runtimeAdapterAgents: Map<string, WebSocket>;
    };
    const offline = await fleet.fetch(
      request("POST", `/portal/leases/${lease.id}/release`, { headers }),
    );
    expect(offline.status).toBe(503);
    expect(
      storage.value<LeaseRecord>(`lease:${lease.id}`)?.runtimeAdapterDeleteRequestedAt,
    ).toBeUndefined();
    relayState.runtimeAdapterAgents.set("example-adapter", socket as unknown as WebSocket);
    const sent = deferred<void>();
    socket.onSend = () => sent.resolve();

    const deletePromise = fleet.fetch(
      request("POST", `/portal/leases/${lease.id}/release`, { headers }),
    );
    await sent.promise;
    fleet.webSocketClose(socket as unknown as WebSocket, 1006, "gone", false);

    expect((await deletePromise).status).toBe(503);
    expect(storage.value<LeaseRecord>(`lease:${lease.id}`)).toMatchObject({
      state: "active",
      runtimeAdapterDeleteRequestedAt: expect.any(String),
      runtimeAdapterDeleteClaimID: expect.any(String),
    });

    const retrySocket = new FakeWebSocket({
      kind: "runtime-adapter-agent",
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
    });
    retrySocket.onSend = (data) => {
      const message = JSON.parse(data) as { id: string };
      void fleet.webSocketMessage(
        retrySocket as unknown as WebSocket,
        JSON.stringify({
          type: "response",
          id: message.id,
          status: 200,
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ id: "example-workspace-89", status: "stopped" }),
        }),
      );
    };
    relayState.runtimeAdapterAgents.set("example-adapter", retrySocket as unknown as WebSocket);
    const pending = storage.value<LeaseRecord>(`lease:${lease.id}`);
    expect(pending).toBeDefined();
    if (pending) {
      pending.runtimeAdapterDeleteRetryAt = new Date(0).toISOString();
      pending.runtimeAdapterDeleteDispatchUntil = new Date(0).toISOString();
      storage.seed(`lease:${lease.id}`, pending);
    }

    await fleet.alarm();
    expect(storage.value<LeaseRecord>(`lease:${lease.id}`)).toMatchObject({
      state: "active",
      runtimeAdapterDeleteRequestedAt: expect.any(String),
    });
    expect(
      storage.value<LeaseRecord>(`lease:${lease.id}`)?.runtimeAdapterDeleteDispatchUntil,
    ).toBeUndefined();
    expect((await confirmRuntimeAdapterAbsence(fleet, lease, headers)).status).toBe(200);
    expect(storage.value<LeaseRecord>(`lease:${lease.id}`)).toMatchObject({ state: "released" });
  });

  it("revokes expired leases while retaining adapter delete retries", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const expiresAt = new Date(Date.now() + 5_000).toISOString();
    const lease = testLease({
      id: "cbx_000000000084",
      provider: "external",
      lifecycle: "registered",
      runtimeAdapterID: "example-adapter",
      runtimeAdapterWorkspaceID: "example-workspace-84",
      runtimeAdapterRegistrationID: "registration-generation-84",
      runtimeAdapterDeleteRequestedAt: "2026-06-13T00:00:00.000Z",
      runtimeAdapterDeleteClaimID: "delete-claim",
      runtimeAdapterDeleteRetryAt: new Date(Date.now() + 60_000).toISOString(),
      owner: "alice@example.com",
      org: "example-org",
      desktop: true,
      keep: true,
      expiresAt,
    });
    storage.seed("runtime-adapter-identity:example-adapter", {
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
      createdAt: "2026-06-01T00:00:00.000Z",
    });
    storage.seed(`lease:${lease.id}`, lease);

    await fleet.alarm();
    expect(storage.value<LeaseRecord>(`lease:${lease.id}`)?.state).toBe("active");
    expect(storage.alarm()).toBe(Date.parse(expiresAt));
    const expiring = storage.value<LeaseRecord>(`lease:${lease.id}`);
    expect(expiring).toBeDefined();
    if (expiring) {
      expiring.expiresAt = new Date(Date.now() - 1_000).toISOString();
      storage.seed(`lease:${lease.id}`, expiring);
    }
    const webVNCAgent = new FakeWebSocket({
      kind: "webvnc-agent",
      leaseID: lease.id,
      id: "agent_expiry",
      capabilities: new Set<string>(),
    });
    const webVNCViewer = new FakeWebSocket({
      kind: "webvnc-viewer",
      leaseID: lease.id,
      id: "viewer_expiry",
      agentID: "agent_expiry",
      owner: "alice@example.com",
      label: "alice",
    });
    const codeAgent = new FakeWebSocket({ kind: "code-agent", leaseID: lease.id });
    const codeViewer = new FakeWebSocket({
      kind: "code-viewer",
      leaseID: lease.id,
      id: "code-viewer-expiry",
      auth: "github",
      portalSessionHash: "f".repeat(64),
    });
    const egressHost = new FakeWebSocket({
      kind: "egress-host",
      leaseID: lease.id,
      sessionID: "egress-expiry",
    });
    const egressClient = new FakeWebSocket({
      kind: "egress-client",
      leaseID: lease.id,
      sessionID: "egress-expiry",
    });
    const relayState = fleet as unknown as {
      webVNCAgents: Map<string, Map<string, WebSocket>>;
      webVNCViewers: Map<
        string,
        Map<
          string,
          {
            id: string;
            agentID: string;
            socket: WebSocket;
            owner: string;
            label: string;
            connectedAt: string;
          }
        >
      >;
      codeAgents: Map<string, WebSocket>;
      codeViewers: Map<string, WebSocket>;
      egressHosts: Map<string, WebSocket>;
      egressClients: Map<string, WebSocket>;
      runtimeAdapterAgents: Map<string, WebSocket>;
    };
    relayState.webVNCAgents.set(
      lease.id,
      new Map([["agent_expiry", webVNCAgent as unknown as WebSocket]]),
    );
    relayState.webVNCViewers.set(
      lease.id,
      new Map([
        [
          "viewer_expiry",
          {
            id: "viewer_expiry",
            agentID: "agent_expiry",
            socket: webVNCViewer as unknown as WebSocket,
            owner: "alice@example.com",
            label: "alice",
            connectedAt: new Date().toISOString(),
          },
        ],
      ]),
    );
    relayState.codeAgents.set(lease.id, codeAgent as unknown as WebSocket);
    relayState.codeViewers.set("code-viewer-expiry", codeViewer as unknown as WebSocket);
    const egressKey = `${lease.id}\0egress-expiry`;
    relayState.egressHosts.set(egressKey, egressHost as unknown as WebSocket);
    relayState.egressClients.set(egressKey, egressClient as unknown as WebSocket);
    await fleet.alarm();
    expect(storage.value<LeaseRecord>(`lease:${lease.id}`)).toMatchObject({
      state: "expired",
      runtimeAdapterDeleteRequestedAt: "2026-06-13T00:00:00.000Z",
      runtimeAdapterDeleteClaimID: "delete-claim",
    });
    for (const socket of [
      webVNCAgent,
      webVNCViewer,
      codeAgent,
      codeViewer,
      egressHost,
      egressClient,
    ]) {
      expect(socket.closeCode).toBe(1008);
      expect(socket.closeReason).toBe("lease expired");
    }
    const revokedTicket = await fleet.fetch(
      request("POST", `/v1/leases/${lease.id}/webvnc/ticket`, { headers, body: {} }),
    );
    expect(revokedTicket.status).toBe(409);
    const pendingRefresh = await fleet.fetch(
      request("PUT", `/v1/leases/${lease.id}/registration`, {
        headers,
        body: {
          provider: "external",
          target: "linux",
          host: "replacement.example.test",
          runtimeAdapterID: "example-adapter",
          runtimeAdapterWorkspaceID: "example-workspace-84",
          runtimeAdapterRegistrationID: "registration-generation-84",
        },
      }),
    );
    expect(pendingRefresh.status).toBe(409);
    await expect(pendingRefresh.json()).resolves.toMatchObject({
      error: "runtime_adapter_delete_pending",
    });
    const pendingRebind = await fleet.fetch(
      request("PUT", "/v1/leases/cbx_000000000083/registration", {
        headers,
        body: {
          provider: "external",
          target: "linux",
          host: "rebound.example.test",
          runtimeAdapterID: "example-adapter",
          runtimeAdapterWorkspaceID: "example-workspace-84",
          runtimeAdapterRegistrationID: "registration-generation-84-rebound",
        },
      }),
    );
    expect(pendingRebind.status).toBe(409);
    await expect(pendingRebind.json()).resolves.toMatchObject({
      error: "runtime_adapter_workspace_conflict",
    });

    const authFailure = new FakeWebSocket({
      kind: "runtime-adapter-agent",
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
    });
    relayState.runtimeAdapterAgents.set("example-adapter", authFailure as unknown as WebSocket);
    authFailure.onSend = (data) => {
      const message = JSON.parse(data) as { id: string };
      void fleet.webSocketMessage(
        authFailure as unknown as WebSocket,
        JSON.stringify({
          type: "response",
          id: message.id,
          status: 401,
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ error: { code: "unauthorized" } }),
        }),
      );
    };
    const authPending = storage.value<LeaseRecord>(`lease:${lease.id}`);
    expect(authPending).toBeDefined();
    if (authPending) {
      authPending.runtimeAdapterDeleteRetryAt = new Date(0).toISOString();
      storage.seed(`lease:${lease.id}`, authPending);
    }
    await fleet.alarm();
    expect(storage.value<LeaseRecord>(`lease:${lease.id}`)).toMatchObject({
      state: "expired",
      runtimeAdapterDeleteRequestedAt: "2026-06-13T00:00:00.000Z",
      runtimeAdapterDeleteError: "runtime_adapter_http_401",
      runtimeAdapterDeleteRetryAt: expect.any(String),
    });

    const pending = storage.value<LeaseRecord>(`lease:${lease.id}`);
    expect(pending).toBeDefined();
    if (pending) {
      pending.runtimeAdapterDeleteRetryAt = new Date(0).toISOString();
      storage.seed(`lease:${lease.id}`, pending);
    }
    const socket = new FakeWebSocket({
      kind: "runtime-adapter-agent",
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
    });
    relayState.runtimeAdapterAgents.set("example-adapter", socket as unknown as WebSocket);
    socket.onSend = (data) => {
      const message = JSON.parse(data) as { id: string };
      void fleet.webSocketMessage(
        socket as unknown as WebSocket,
        JSON.stringify({
          type: "response",
          id: message.id,
          status: 200,
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ id: "example-workspace-84", status: "stopped" }),
        }),
      );
    };

    await fleet.alarm();
    expect(storage.value<LeaseRecord>(`lease:${lease.id}`)).toMatchObject({
      state: "expired",
      runtimeAdapterDeleteRequestedAt: "2026-06-13T00:00:00.000Z",
    });
    expect((await confirmRuntimeAdapterAbsence(fleet, lease, headers)).status).toBe(200);
    expect(storage.value<LeaseRecord>(`lease:${lease.id}`)).toMatchObject({ state: "expired" });
    expect(
      storage.value<LeaseRecord>(`lease:${lease.id}`)?.runtimeAdapterDeleteRequestedAt,
    ).toBeUndefined();
  });

  it("expires leases before waiting for a due adapter delete retry", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const lease = testLease({
      id: "cbx_000000000082",
      provider: "external",
      lifecycle: "registered",
      runtimeAdapterID: "example-adapter",
      runtimeAdapterWorkspaceID: "example-workspace-82",
      runtimeAdapterDeleteRequestedAt: "2026-06-13T00:00:00.000Z",
      runtimeAdapterDeleteClaimID: "delete-claim",
      runtimeAdapterDeleteRetryAt: new Date(0).toISOString(),
      owner: "alice@example.com",
      org: "example-org",
      expiresAt: new Date(Date.now() - 1_000).toISOString(),
    });
    storage.seed("runtime-adapter-identity:example-adapter", {
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
      createdAt: "2026-06-01T00:00:00.000Z",
    });
    storage.seed(`lease:${lease.id}`, lease);
    const socket = new FakeWebSocket({
      kind: "runtime-adapter-agent",
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
    });
    const relayState = fleet as unknown as {
      runtimeAdapterAgents: Map<string, WebSocket>;
    };
    relayState.runtimeAdapterAgents.set("example-adapter", socket as unknown as WebSocket);
    const sent = deferred<void>();
    socket.onSend = () => sent.resolve();

    const alarm = fleet.alarm();
    await sent.promise;
    expect(storage.value<LeaseRecord>(`lease:${lease.id}`)?.state).toBe("expired");
    fleet.webSocketClose(socket as unknown as WebSocket, 1006, "gone", false);
    await alarm;
    expect(storage.value<LeaseRecord>(`lease:${lease.id}`)).toMatchObject({
      state: "expired",
      runtimeAdapterDeleteRequestedAt: "2026-06-13T00:00:00.000Z",
    });
  });

  it("revokes restored bridge sockets from durable terminal lease state", async () => {
    const storage = new MemoryStorage();
    const lease = testLease({
      id: "cbx_000000000071",
      provider: "external",
      lifecycle: "registered",
      state: "expired",
      owner: "alice@example.com",
      org: "example-org",
      expiresAt: new Date(Date.now() - 60_000).toISOString(),
    });
    storage.seed(`lease:${lease.id}`, lease);
    const socket = new FakeWebSocket({
      kind: "egress-host",
      leaseID: lease.id,
      sessionID: "restored-expired-session",
    });
    const fleet = new FleetDurableObject(
      {
        storage,
        getWebSockets: () => [socket as unknown as WebSocket],
      } as unknown as DurableObjectState,
      { CRABBOX_DEFAULT_ORG: "default-org" } as Env,
    );

    expect((await fleet.fetch(request("GET", "/v1/health"))).status).toBe(200);
    await vi.waitFor(() => expect(socket.closeCode).toBe(1008));
    expect(socket.closeReason).toBe("lease ended");
  });

  it("keeps a dispatched portal delete binding fenced until completion", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const lease = testLease({
      id: "cbx_000000000087",
      provider: "external",
      lifecycle: "registered",
      runtimeAdapterID: "example-adapter",
      runtimeAdapterWorkspaceID: "example-workspace-87",
      runtimeAdapterRegistrationID: "registration-generation-87",
      owner: "alice@example.com",
      org: "example-org",
      keep: true,
    });
    storage.seed("runtime-adapter-identity:example-adapter", {
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
      createdAt: "2026-06-01T00:00:00.000Z",
    });
    storage.seed(`lease:${lease.id}`, lease);
    const socket = new FakeWebSocket({
      kind: "runtime-adapter-agent",
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
    });
    const relayState = fleet as unknown as {
      runtimeAdapterAgents: Map<string, WebSocket>;
    };
    relayState.runtimeAdapterAgents.set("example-adapter", socket as unknown as WebSocket);
    const sent = deferred<{ id: string }>();
    socket.onSend = (data) => sent.resolve(JSON.parse(data) as { id: string });

    const deleting = fleet.fetch(
      request("POST", `/portal/leases/${lease.id}/release`, { headers }),
    );
    const message = await sent.promise;
    const racedRelease = await (
      fleet as unknown as {
        releaseResolvedLeaseOperation: (
          lease: LeaseRecord,
          options: { deleteServer: boolean; keep?: boolean },
        ) => Promise<LeaseRecord>;
      }
    ).releaseResolvedLeaseOperation(lease, { deleteServer: false });
    expect(racedRelease).toMatchObject({
      state: "active",
      runtimeAdapterDeleteRequestedAt: expect.any(String),
    });
    const released = await fleet.fetch(
      request("POST", `/v1/leases/${lease.id}/release`, { headers }),
    );
    expect(released.status).toBe(409);
    await expect(released.json()).resolves.toMatchObject({
      error: "runtime_adapter_delete_pending",
    });
    const registered = await fleet.fetch(
      request("PUT", "/v1/leases/cbx_000000000081/registration", {
        headers,
        body: {
          provider: "external",
          target: "linux",
          host: "replacement.example.test",
          runtimeAdapterID: "example-adapter",
          runtimeAdapterWorkspaceID: "example-workspace-87",
          runtimeAdapterRegistrationID: "registration-generation-87-replacement",
        },
      }),
    );
    expect(registered.status).toBe(409);
    await expect(registered.json()).resolves.toMatchObject({
      error: "runtime_adapter_workspace_conflict",
    });
    await fleet.webSocketMessage(
      socket as unknown as WebSocket,
      JSON.stringify({
        type: "response",
        id: message.id,
        status: 200,
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ id: "example-workspace-87", status: "stopped" }),
      }),
    );

    expect((await deleting).status).toBe(303);
    expect(storage.value<LeaseRecord>(`lease:${lease.id}`)).toMatchObject({
      state: "active",
      runtimeAdapterDeleteRequestedAt: expect.any(String),
    });
    expect((await confirmRuntimeAdapterAbsence(fleet, lease, headers)).status).toBe(200);
    expect(storage.value<LeaseRecord>(`lease:${lease.id}`)).toMatchObject({ state: "released" });
    expect(
      storage.value<LeaseRecord>(`lease:${lease.id}`)?.runtimeAdapterDeleteRequestedAt,
    ).toBeUndefined();
  });

  it("keeps a reconciling delete binding fenced until completion", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const lease = testLease({
      id: "cbx_000000000086",
      provider: "external",
      lifecycle: "registered",
      runtimeAdapterID: "example-adapter",
      runtimeAdapterWorkspaceID: "example-workspace-86",
      runtimeAdapterRegistrationID: "registration-generation-86",
      runtimeAdapterDeleteRequestedAt: "2026-06-13T00:00:00.000Z",
      runtimeAdapterDeleteClaimID: "old-delete-claim",
      runtimeAdapterDeleteRetryAt: new Date(0).toISOString(),
      owner: "alice@example.com",
      org: "example-org",
      keep: true,
    });
    storage.seed("runtime-adapter-identity:example-adapter", {
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
      createdAt: "2026-06-01T00:00:00.000Z",
    });
    storage.seed(`lease:${lease.id}`, lease);
    const socket = new FakeWebSocket({
      kind: "runtime-adapter-agent",
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
    });
    const relayState = fleet as unknown as {
      runtimeAdapterAgents: Map<string, WebSocket>;
    };
    relayState.runtimeAdapterAgents.set("example-adapter", socket as unknown as WebSocket);
    const sent = deferred<{ id: string }>();
    socket.onSend = (data) => sent.resolve(JSON.parse(data) as { id: string });

    const reconciling = fleet.alarm();
    const message = await sent.promise;
    const released = await fleet.fetch(
      request("POST", `/v1/leases/${lease.id}/release`, { headers }),
    );
    expect(released.status).toBe(409);
    await expect(released.json()).resolves.toMatchObject({
      error: "runtime_adapter_delete_pending",
    });
    const registered = await fleet.fetch(
      request("PUT", "/v1/leases/cbx_000000000080/registration", {
        headers,
        body: {
          provider: "external",
          target: "linux",
          host: "reconciled-replacement.example.test",
          runtimeAdapterID: "example-adapter",
          runtimeAdapterWorkspaceID: "example-workspace-86",
          runtimeAdapterRegistrationID: "registration-generation-86-replacement",
        },
      }),
    );
    expect(registered.status).toBe(409);
    await expect(registered.json()).resolves.toMatchObject({
      error: "runtime_adapter_workspace_conflict",
    });
    await fleet.webSocketMessage(
      socket as unknown as WebSocket,
      JSON.stringify({
        type: "response",
        id: message.id,
        status: 200,
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ id: "example-workspace-86", status: "stopped" }),
      }),
    );
    await reconciling;

    expect(storage.value<LeaseRecord>(`lease:${lease.id}`)).toMatchObject({
      state: "expired",
      runtimeAdapterDeleteRequestedAt: "2026-06-13T00:00:00.000Z",
    });
    expect((await confirmRuntimeAdapterAbsence(fleet, lease, headers)).status).toBe(200);
    expect(
      storage.value<LeaseRecord>(`lease:${lease.id}`)?.runtimeAdapterDeleteRequestedAt,
    ).toBeUndefined();
  });

  it("serializes concurrent upstream rejections without abandoning delete intent", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const lease = testLease({
      id: "cbx_000000000088",
      provider: "external",
      lifecycle: "registered",
      runtimeAdapterID: "example-adapter",
      runtimeAdapterWorkspaceID: "example-workspace-88",
      owner: "alice@example.com",
      org: "example-org",
      keep: true,
    });
    storage.seed("runtime-adapter-identity:example-adapter", {
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
      createdAt: "2026-06-01T00:00:00.000Z",
    });
    storage.seed(`lease:${lease.id}`, lease);
    const socket = new FakeWebSocket({
      kind: "runtime-adapter-agent",
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
    });
    const relayState = fleet as unknown as {
      runtimeAdapterAgents: Map<string, WebSocket>;
    };
    relayState.runtimeAdapterAgents.set("example-adapter", socket as unknown as WebSocket);
    const sent = [deferred<void>(), deferred<void>()];
    const messages: { id: string }[] = [];
    socket.onSend = (data) => {
      messages.push(JSON.parse(data) as { id: string });
      sent[messages.length - 1]?.resolve();
    };

    const firstDelete = fleet.fetch(
      request("POST", `/portal/leases/${lease.id}/release`, { headers }),
    );
    await sent[0].promise;
    const secondDelete = fleet.fetch(
      request("POST", `/portal/leases/${lease.id}/release`, { headers }),
    );
    await Promise.resolve();
    expect(messages).toHaveLength(1);

    await fleet.webSocketMessage(
      socket as unknown as WebSocket,
      JSON.stringify({
        type: "response",
        id: messages[0]?.id,
        status: 404,
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ error: { code: "not_found" } }),
      }),
    );
    expect((await firstDelete).status).toBe(502);
    await sent[1].promise;
    await fleet.webSocketMessage(
      socket as unknown as WebSocket,
      JSON.stringify({
        type: "response",
        id: messages[1]?.id,
        status: 404,
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ error: { code: "not_found" } }),
      }),
    );

    expect((await secondDelete).status).toBe(502);
    expect(storage.value<LeaseRecord>(`lease:${lease.id}`)).toMatchObject({
      state: "active",
      runtimeAdapterDeleteRequestedAt: expect.any(String),
      runtimeAdapterDeleteRetryAt: expect.any(String),
    });
  });

  it("keeps the registration when the relay rejects local adapter ownership", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const lease = testLease({
      id: "cbx_000000000098",
      slug: "legacy-misbound-box",
      provider: "external",
      lifecycle: "registered",
      runtimeAdapterID: "other-adapter",
      runtimeAdapterWorkspaceID: "example-workspace-98",
      owner: "alice@example.com",
      org: "example-org",
      keep: true,
    });
    storage.seed("runtime-adapter-identity:other-adapter", {
      adapterID: "other-adapter",
      owner: "mallory@example.com",
      org: "example-org",
      createdAt: "2026-06-01T00:00:00.000Z",
    });
    storage.seed(`lease:${lease.id}`, lease);

    const response = await fleet.fetch(
      request("POST", `/portal/leases/${lease.id}/release`, {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );

    expect(response.status).toBe(502);
    expect(storage.value<LeaseRecord>(`lease:${lease.id}`)?.state).toBe("active");
  });

  it("keeps adapter 404s pending until an exact generation confirms absence", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const lease = testLease({
      id: "cbx_000000000097",
      slug: "already-gone-box",
      provider: "external",
      lifecycle: "registered",
      runtimeAdapterID: "example-adapter",
      runtimeAdapterWorkspaceID: "example-workspace-97",
      runtimeAdapterRegistrationID: "registration-generation-97",
      owner: "alice@example.com",
      org: "example-org",
      keep: true,
    });
    storage.seed("runtime-adapter-identity:example-adapter", {
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
      createdAt: "2026-06-01T00:00:00.000Z",
    });
    storage.seed(`lease:${lease.id}`, lease);
    const socket = new FakeWebSocket({
      kind: "runtime-adapter-agent",
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
    });
    const relayState = fleet as unknown as {
      runtimeAdapterAgents: Map<string, WebSocket>;
    };
    relayState.runtimeAdapterAgents.set("example-adapter", socket as unknown as WebSocket);
    let errorCode = "not_found";
    socket.onSend = (data) => {
      const message = JSON.parse(data) as { id: string };
      void fleet.webSocketMessage(
        socket as unknown as WebSocket,
        JSON.stringify({
          type: "response",
          id: message.id,
          status: 404,
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ error: { code: errorCode, message: "not found" } }),
        }),
      );
    };

    const genericMissing = await fleet.fetch(
      request("POST", `/portal/leases/${lease.id}/release`, { headers }),
    );
    expect(genericMissing.status).toBe(502);
    expect(storage.value<LeaseRecord>(`lease:${lease.id}`)).toMatchObject({
      state: "active",
      runtimeAdapterDeleteRequestedAt: expect.any(String),
    });

    errorCode = "workspace_not_found";
    const confirmedMissing = await fleet.fetch(
      request("POST", `/portal/leases/${lease.id}/release`, { headers }),
    );
    expect(confirmedMissing.status).toBe(502);
    expect(storage.value<LeaseRecord>(`lease:${lease.id}`)).toMatchObject({
      state: "active",
      runtimeAdapterDeleteRequestedAt: expect.any(String),
    });
    expect((await confirmRuntimeAdapterAbsence(fleet, lease, headers)).status).toBe(200);
    expect(storage.value<LeaseRecord>(`lease:${lease.id}`)?.state).toBe("released");

    const sendsBeforeStaleRequest = socket.sentJSON().length;
    const staleRequest = await fleet.fetch(
      request("POST", `/portal/leases/${lease.id}/release`, { headers }),
    );
    expect(staleRequest.status).toBe(409);
    expect(socket.sentJSON()).toHaveLength(sendsBeforeStaleRequest);
  });

  it("lets manage-share users delete through the lease owner's adapter", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const managerHeaders = {
      "x-crabbox-owner": "manager@example.com",
      "x-crabbox-org": "example-org",
    };
    const ownerHeaders = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const lease = testLease({
      id: "cbx_000000000096",
      slug: "shared-adapter-box",
      provider: "external",
      lifecycle: "registered",
      runtimeAdapterID: "example-adapter",
      runtimeAdapterWorkspaceID: "example-workspace-96",
      runtimeAdapterRegistrationID: "registration-generation-96",
      owner: "alice@example.com",
      org: "example-org",
      share: { users: { "manager@example.com": "manage" } },
      keep: true,
    });
    storage.seed("runtime-adapter-identity:example-adapter", {
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
      createdAt: "2026-06-01T00:00:00.000Z",
    });
    storage.seed(`lease:${lease.id}`, lease);
    const socket = new FakeWebSocket({
      kind: "runtime-adapter-agent",
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
    });
    const relayState = fleet as unknown as {
      runtimeAdapterAgents: Map<string, WebSocket>;
    };
    relayState.runtimeAdapterAgents.set("example-adapter", socket as unknown as WebSocket);
    socket.onSend = (data) => {
      const message = JSON.parse(data) as { id: string };
      void fleet.webSocketMessage(
        socket as unknown as WebSocket,
        JSON.stringify({
          type: "response",
          id: message.id,
          status: 200,
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ id: "example-workspace-96", status: "stopped" }),
        }),
      );
    };

    const response = await fleet.fetch(
      request("POST", `/portal/leases/${lease.id}/release`, { headers: managerHeaders }),
    );

    expect(response.status).toBe(303);
    expect(storage.value<LeaseRecord>(`lease:${lease.id}`)).toMatchObject({
      state: "active",
      runtimeAdapterDeleteRequestedAt: expect.any(String),
    });
    expect((await confirmRuntimeAdapterAbsence(fleet, lease, ownerHeaders)).status).toBe(200);
    expect(storage.value<LeaseRecord>(`lease:${lease.id}`)?.state).toBe("released");
  });

  it("returns JSON for relay-generated timeout and disconnect responses", async () => {
    vi.useFakeTimers();
    try {
      const storage = new MemoryStorage();
      const fleet = testFleet(storage);
      const headers = {
        "x-crabbox-owner": "alice@example.com",
        "x-crabbox-org": "example-org",
      };
      storage.seed("runtime-adapter-identity:example-adapter", {
        adapterID: "example-adapter",
        owner: "alice@example.com",
        org: "example-org",
        createdAt: "2026-06-01T00:00:00.000Z",
      });
      const relayState = fleet as unknown as {
        runtimeAdapterAgents: Map<string, WebSocket>;
      };
      const timeoutSocket = new FakeWebSocket({
        kind: "runtime-adapter-agent",
        adapterID: "example-adapter",
        owner: "alice@example.com",
        org: "example-org",
      });
      const sent = deferred<void>();
      let timedOutRequestID = "";
      timeoutSocket.onSend = (data) => {
        const message = JSON.parse(data) as { id: string; deadlineMs: number };
        timedOutRequestID = message.id;
        expect(message.deadlineMs).toBe(Date.now() + runtimeAdapterRelayTimeoutMs);
        sent.resolve();
      };
      relayState.runtimeAdapterAgents.set("example-adapter", timeoutSocket as unknown as WebSocket);
      let timedOutSettled = false;
      const timedOutPromise = fleet
        .fetch(
          request("GET", "/v1/adapters/example-adapter/proxy/v1/workspaces/example-workspace-1", {
            headers,
          }),
        )
        .then((response) => {
          timedOutSettled = true;
          return response;
        });
      await sent.promise;
      const malformedText = JSON.stringify({
        type: "response",
        id: timedOutRequestID,
        status: 200,
        headers: { "content-type": "application/json" },
        body: "~",
      });
      const malformedBytes = new TextEncoder().encode(malformedText);
      malformedBytes[malformedText.indexOf('"body":"~"') + 8] = 0xff;
      await fleet.webSocketMessage(timeoutSocket as unknown as WebSocket, malformedBytes.buffer);
      expect(timedOutSettled).toBe(false);
      await vi.advanceTimersByTimeAsync(runtimeAdapterRelayTimeoutMs);
      const timedOut = await timedOutPromise;
      expect(timedOut.status).toBe(504);
      expect(timedOut.headers.get("content-type")).toBe("application/json; charset=utf-8");
      await expect(timedOut.json()).resolves.toMatchObject({ error: "runtime_adapter_timeout" });

      const desktopSent = deferred<void>();
      timeoutSocket.onSend = (data) => {
        const message = JSON.parse(data) as { deadlineMs: number };
        expect(message.deadlineMs).toBe(Date.now() + runtimeAdapterDesktopRelayTimeoutMs);
        desktopSent.resolve();
      };
      let desktopSettled = false;
      const desktopTimedOutPromise = fleet
        .fetch(
          request(
            "POST",
            "/v1/adapters/example-adapter/proxy/v1/workspaces/example-workspace-1/connections/desktop",
            { headers },
          ),
        )
        .then((response) => {
          desktopSettled = true;
          return response;
        });
      await desktopSent.promise;
      await vi.advanceTimersByTimeAsync(runtimeAdapterRelayTimeoutMs);
      expect(desktopSettled).toBe(false);
      await vi.advanceTimersByTimeAsync(
        runtimeAdapterDesktopRelayTimeoutMs - runtimeAdapterRelayTimeoutMs,
      );
      const desktopTimedOut = await desktopTimedOutPromise;
      expect(desktopTimedOut.status).toBe(504);
      await expect(desktopTimedOut.json()).resolves.toMatchObject({
        error: "runtime_adapter_timeout",
      });

      const disconnectedSocket = new FakeWebSocket({
        kind: "runtime-adapter-agent",
        adapterID: "example-adapter",
        owner: "alice@example.com",
        org: "example-org",
      });
      relayState.runtimeAdapterAgents.set(
        "example-adapter",
        disconnectedSocket as unknown as WebSocket,
      );
      disconnectedSocket.onSend = () => {
        fleet.webSocketClose(disconnectedSocket as unknown as WebSocket, 1006, "gone", false);
      };
      const disconnected = await fleet.fetch(
        request("GET", "/v1/adapters/example-adapter/proxy/v1/workspaces/example-workspace-1", {
          headers,
        }),
      );
      expect(disconnected.status).toBe(503);
      expect(disconnected.headers.get("content-type")).toBe("application/json; charset=utf-8");
      await expect(disconnected.json()).resolves.toMatchObject({
        error: "runtime_adapter_unavailable",
      });
    } finally {
      vi.useRealTimers();
    }
  });

  it("sends a streamed request only to the adapter connected after body consumption", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const headers = {
      "content-type": "application/json",
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    storage.seed("runtime-adapter-identity:example-adapter", {
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
      createdAt: "2026-06-01T00:00:00.000Z",
    });
    const oldSocket = new FakeWebSocket({
      kind: "runtime-adapter-agent",
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
    });
    oldSocket.onSend = () => {
      throw new Error("stale runtime adapter received request");
    };
    const newSocket = new FakeWebSocket({
      kind: "runtime-adapter-agent",
      adapterID: "example-adapter",
      owner: "alice@example.com",
      org: "example-org",
    });
    newSocket.onSend = (data) => {
      const message = JSON.parse(data) as { id: string };
      void fleet.webSocketMessage(
        newSocket as unknown as WebSocket,
        JSON.stringify({
          type: "response",
          id: message.id,
          status: 202,
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ id: "example-workspace-1", status: "provisioning" }),
        }),
      );
    };
    const relayState = fleet as unknown as {
      runtimeAdapterAgents: Map<string, WebSocket>;
    };
    relayState.runtimeAdapterAgents.set("example-adapter", oldSocket as unknown as WebSocket);
    const bodyReadStarted = deferred<void>();
    const finishBody = deferred<void>();
    const body = new ReadableStream<Uint8Array>({
      async pull(controller) {
        bodyReadStarted.resolve();
        await finishBody.promise;
        controller.enqueue(new TextEncoder().encode('{"id":"example-workspace-1"}'));
        controller.close();
      },
    });
    const proxiedPromise = fleet.fetch(
      new Request("https://crabbox.test/v1/adapters/example-adapter/proxy/v1/workspaces", {
        method: "POST",
        headers,
        body,
        duplex: "half",
      } as RequestInit & { duplex: "half" }),
    );

    await bodyReadStarted.promise;
    fleet.webSocketClose(oldSocket as unknown as WebSocket, 1012, "replaced", false);
    oldSocket.close();
    relayState.runtimeAdapterAgents.set("example-adapter", newSocket as unknown as WebSocket);
    finishBody.resolve();
    const proxied = await proxiedPromise;

    expect(proxied.status).toBe(202);
    expect(oldSocket.sentJSON()).toEqual([]);
    expect(newSocket.sentJSON()).toHaveLength(1);
    await expect(proxied.json()).resolves.toEqual({
      id: "example-workspace-1",
      status: "provisioning",
    });
  });
});

describe("fleet lease identity and idle", () => {
  it("keeps transient Hetzner create failures recoverable", async () => {
    const responses = [
      jsonResponse({ ssh_keys: [] }),
      jsonResponse({ ssh_keys: [] }),
      jsonResponse({
        ssh_key: {
          id: 1,
          name: "crabbox-workspace-test",
          public_key: "ssh-ed25519 workspace-test",
        },
      }),
      jsonResponse({ error: { code: "server_error" } }, 503),
    ];
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => responses.shift() ?? jsonResponse({}, 500)),
    );
    const client = new HetznerClient({ HETZNER_TOKEN: "test-token" } as Env);
    const config = leaseConfig({
      provider: "hetzner",
      providerKey: "crabbox-workspace-test",
      sshPublicKey: "ssh-ed25519 workspace-test",
    });

    const error = await client
      .createServerWithFallback(config, "cbx_abcdef123456", "fleet-is-100", "alice@example.com")
      .catch((caught: unknown) => caught);

    expect(error).toBeInstanceOf(HetznerProvisioningError);
    expect((error as HetznerProvisioningError).resourceMayExist).toBe(true);
    expect((error as HetznerProvisioningError).providerKeyCleanupID).toBeUndefined();
  });

  it("retains a newly created lease key when the server ID is unknown", async () => {
    const requests: Array<{ method: string; path: string }> = [];
    const responses = [
      jsonResponse({ ssh_keys: [] }),
      jsonResponse({ ssh_keys: [] }),
      jsonResponse({
        ssh_key: {
          id: 7,
          name: "crabbox-cbx-abcdef123456",
          public_key: "ssh-ed25519 rollback-test",
          labels: { crabbox: "true", created_by: "crabbox", lease: "cbx_abcdef123456" },
        },
      }),
      jsonResponse({ error: { code: "server_error" } }, 503),
    ];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = new URL(String(input));
        requests.push({ method: init?.method || "GET", path: url.pathname });
        return responses.shift() ?? jsonResponse({}, 500);
      }),
    );
    const client = new HetznerClient({ HETZNER_TOKEN: "test-token" } as Env);
    const config = leaseConfig({
      provider: "hetzner",
      providerKey: "crabbox-cbx-abcdef123456",
      sshPublicKey: "ssh-ed25519 rollback-test",
    });

    const error = await client
      .createServerWithFallback(config, "cbx_abcdef123456", "rollback", "alice@example.com")
      .catch((caught: unknown) => caught);

    expect(error).toBeInstanceOf(HetznerProvisioningError);
    expect((error as HetznerProvisioningError).resourceMayExist).toBe(true);
    expect((error as HetznerProvisioningError).serverID).toBeUndefined();
    expect((error as HetznerProvisioningError).providerKeyCleanupID).toBe(7);
    expect(requests.filter((entry) => entry.method === "DELETE")).toEqual([]);
  });

  it("retains newly created shared Hetzner workspace keys after readiness rollback", async () => {
    const requests: Array<{ method: string; path: string }> = [];
    const responses = [
      jsonResponse({ ssh_keys: [] }),
      jsonResponse({ ssh_keys: [] }),
      jsonResponse({
        ssh_key: {
          id: 7,
          name: "crabbox-workspace-test",
          public_key: "ssh-ed25519 workspace-test",
          labels: { crabbox: "true", created_by: "crabbox" },
        },
      }),
      jsonResponse({
        server: {
          id: 123,
          name: "crabbox-workspace",
          status: "initializing",
          server_type: { name: "cpx62" },
          public_net: { ipv4: { ip: "" } },
          labels: { crabbox: "true", lease: "cbx_abcdef123456" },
        },
      }),
      new Response(null, { status: 204 }),
    ];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = new URL(String(input));
        requests.push({ method: init?.method || "GET", path: url.pathname });
        return responses.shift() ?? jsonResponse({}, 500);
      }),
    );
    const client = new HetznerClient({ HETZNER_TOKEN: "test-token" } as Env);
    vi.spyOn(client, "waitForServerIP").mockRejectedValue(
      new Error("timed out waiting for server IP: 123"),
    );
    const config = leaseConfig({
      provider: "hetzner",
      class: "cpx62",
      serverType: "cpx62",
      providerKey: "crabbox-workspace-test",
      sshPublicKey: "ssh-ed25519 workspace-test",
    });

    const error = await client
      .createServerWithFallback(config, "cbx_abcdef123456", "workspace", "alice@example.com")
      .catch((caught: unknown) => caught);

    expect(error).toBeInstanceOf(HetznerProvisioningError);
    expect((error as HetznerProvisioningError).resourceMayExist).toBe(false);
    expect((error as HetznerProvisioningError).providerKeyCleanupID).toBeUndefined();
    expect(requests.filter((entry) => entry.method === "DELETE")).toEqual([
      { method: "DELETE", path: "/v1/servers/123" },
    ]);
  });

  it("keeps ordinary Hetzner leases IP-ready while the server is still initializing", async () => {
    const responses = [
      jsonResponse({ ssh_keys: [] }),
      jsonResponse({ ssh_keys: [] }),
      jsonResponse({
        ssh_key: {
          id: 1,
          name: "ordinary-key",
          public_key: "ssh-ed25519 ordinary-test",
        },
      }),
      jsonResponse({
        server: {
          id: 123,
          name: "crabbox-ordinary",
          status: "initializing",
          server_type: { name: "cpx62" },
          public_net: { ipv4: { ip: "192.0.2.123" } },
          labels: {},
        },
      }),
    ];
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => responses.shift() ?? jsonResponse({}, 500)),
    );
    const client = new HetznerClient({ HETZNER_TOKEN: "test-token" } as Env);
    const config = leaseConfig({
      provider: "hetzner",
      providerKey: "ordinary-key",
      sshPublicKey: "ssh-ed25519 ordinary-test",
    });

    await expect(
      client.createServerWithFallback(config, "cbx_abcdef123456", "ordinary", "alice@example.com"),
    ).resolves.toMatchObject({
      server: { id: 123, status: "initializing" },
    });
  });

  it("treats already-absent partial Hetzner server and SSH key cleanup as complete", async () => {
    const requests: Array<{ method: string; path: string }> = [];
    const responses = [
      jsonResponse({ ssh_keys: [] }),
      jsonResponse({ ssh_keys: [] }),
      jsonResponse({
        ssh_key: {
          id: 7,
          name: "crabbox-cbx-abcdef123456",
          public_key: "ssh-ed25519 rollback-test",
          labels: { crabbox: "true", created_by: "crabbox", lease: "cbx_abcdef123456" },
        },
      }),
      jsonResponse({
        server: {
          id: 123,
          name: "crabbox-rollback",
          status: "initializing",
          server_type: { name: "cpx62" },
          public_net: { ipv4: { ip: "" } },
          labels: { crabbox: "true", lease: "cbx_abcdef123456" },
        },
      }),
      jsonResponse({ error: { code: "not_found" } }, 404),
      jsonResponse({ error: { code: "not_found" } }, 404),
    ];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = new URL(String(input));
        requests.push({ method: init?.method || "GET", path: url.pathname });
        return responses.shift() ?? jsonResponse({}, 500);
      }),
    );
    const client = new HetznerClient({ HETZNER_TOKEN: "test-token" } as Env);
    vi.spyOn(client, "waitForServerIP").mockRejectedValue(
      new Error("timed out waiting for server IP: 123"),
    );
    const config = leaseConfig({
      provider: "hetzner",
      class: "cpx62",
      serverType: "cpx62",
      providerKey: "crabbox-cbx-abcdef123456",
      sshPublicKey: "ssh-ed25519 rollback-test",
    });

    const error = await client
      .createServerWithFallback(config, "cbx_abcdef123456", "rollback", "alice@example.com")
      .catch((caught: unknown) => caught);

    expect(error).toBeInstanceOf(HetznerProvisioningError);
    expect((error as HetznerProvisioningError).resourceMayExist).toBe(false);
    expect((error as HetznerProvisioningError).providerKeyCleanupID).toBeUndefined();
    expect(requests.filter((entry) => entry.method === "DELETE")).toEqual([
      { method: "DELETE", path: "/v1/servers/123" },
      { method: "DELETE", path: "/v1/ssh_keys/7" },
    ]);
  });

  it("continues Hetzner server-type fallback after rolling back a readiness timeout", async () => {
    const serverTypes: string[] = [];
    const deletes: string[] = [];
    let serverID = 122;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = new URL(String(input));
        const method = init?.method || "GET";
        if (method === "GET" && url.pathname === "/v1/ssh_keys") {
          return jsonResponse({ ssh_keys: [] });
        }
        if (method === "POST" && url.pathname === "/v1/ssh_keys") {
          return jsonResponse({
            ssh_key: {
              id: 7,
              name: "crabbox-cbx-abcdef123456",
              public_key: "ssh-ed25519 rollback-test",
              labels: { crabbox: "true", created_by: "crabbox", lease: "cbx_abcdef123456" },
            },
          });
        }
        if (method === "POST" && url.pathname === "/v1/servers") {
          const body = JSON.parse(String(init?.body)) as { server_type: string };
          serverTypes.push(body.server_type);
          serverID += 1;
          return jsonResponse({
            server: {
              id: serverID,
              name: "crabbox-rollback",
              status: "initializing",
              server_type: { name: body.server_type },
              public_net: { ipv4: { ip: "" } },
              labels: { crabbox: "true", lease: "cbx_abcdef123456" },
            },
          });
        }
        if (method === "DELETE" && url.pathname.startsWith("/v1/servers/")) {
          deletes.push(url.pathname);
          return new Response(null, { status: 204 });
        }
        return jsonResponse({}, 500);
      }),
    );
    const client = new HetznerClient({ HETZNER_TOKEN: "test-token" } as Env);
    vi.spyOn(client, "waitForServerIP")
      .mockRejectedValueOnce(new Error("timed out waiting for server IP: 123"))
      .mockResolvedValueOnce({
        id: 124,
        name: "crabbox-rollback",
        status: "running",
        server_type: { name: "cpx62" },
        public_net: { ipv4: { ip: "192.0.2.10" } },
        labels: { crabbox: "true", lease: "cbx_abcdef123456" },
      });
    const config = leaseConfig({
      provider: "hetzner",
      class: "standard",
      providerKey: "crabbox-cbx-abcdef123456",
      sshPublicKey: "ssh-ed25519 rollback-test",
    });

    await expect(
      client.createServerWithFallback(config, "cbx_abcdef123456", "rollback", "alice@example.com"),
    ).resolves.toMatchObject({ server: { id: 124 }, serverType: "cpx62" });
    expect(serverTypes).toEqual(["ccx33", "cpx62"]);
    expect(deletes).toEqual(["/v1/servers/123"]);
  });

  it("retains the exact newly created Hetzner SSH key id when immediate cleanup fails", async () => {
    const requests: Array<{ method: string; path: string }> = [];
    let keyDeleteAttempts = 0;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = new URL(String(input));
        const method = init?.method || "GET";
        requests.push({ method, path: url.pathname });
        if (method === "GET" && url.pathname === "/v1/ssh_keys") {
          return jsonResponse({ ssh_keys: [] });
        }
        if (method === "POST" && url.pathname === "/v1/ssh_keys") {
          return jsonResponse({
            ssh_key: {
              id: 7,
              name: "crabbox-cbx-abcdef123456",
              public_key: "ssh-ed25519 rollback-test",
              labels: { crabbox: "true", created_by: "crabbox", lease: "cbx_abcdef123456" },
            },
          });
        }
        if (method === "POST" && url.pathname === "/v1/servers") {
          return jsonResponse({
            server: {
              id: 123,
              name: "crabbox-rollback",
              status: "initializing",
              server_type: { name: "cpx62" },
              public_net: { ipv4: { ip: "" } },
              labels: { crabbox: "true", lease: "cbx_abcdef123456" },
            },
          });
        }
        if (method === "DELETE" && url.pathname === "/v1/servers/123") {
          return new Response(null, { status: 204 });
        }
        if (method === "DELETE" && url.pathname === "/v1/ssh_keys/7") {
          keyDeleteAttempts += 1;
          return keyDeleteAttempts === 1
            ? jsonResponse({ error: { code: "server_error" } }, 503)
            : jsonResponse({ error: { code: "not_found" } }, 404);
        }
        return jsonResponse({}, 500);
      }),
    );
    const client = new HetznerClient({ HETZNER_TOKEN: "test-token" } as Env);
    vi.spyOn(client, "waitForServerIP").mockRejectedValue(new Error("IP wait failed"));
    const config = leaseConfig({
      provider: "hetzner",
      class: "cpx62",
      serverType: "cpx62",
      providerKey: "crabbox-cbx-abcdef123456",
      sshPublicKey: "ssh-ed25519 rollback-test",
    });

    const error = await client
      .createServerWithFallback(config, "cbx_abcdef123456", "rollback", "alice@example.com")
      .catch((caught: unknown) => caught);

    expect(error).toBeInstanceOf(HetznerProvisioningError);
    expect((error as HetznerProvisioningError).resourceMayExist).toBe(false);
    expect((error as HetznerProvisioningError).providerKeyCleanupID).toBe(7);
    await client.deleteSSHKeyByID((error as HetznerProvisioningError).providerKeyCleanupID!);
    expect(
      requests.filter((entry) => entry.method === "DELETE" && entry.path === "/v1/ssh_keys/7"),
    ).toHaveLength(2);
  });

  it("retries persisted Hetzner SSH key cleanup before forgetting the failed lease", async () => {
    const storage = new MemoryStorage();
    const cleanupLeases: LeaseRecord[] = [];
    let providerCreates = 0;
    const fleet = testFleet(
      storage,
      {
        hetzner: fakeProvider(
          async () => {
            providerCreates += 1;
            if (providerCreates === 1) {
              throw new HetznerProvisioningError(
                "cleanup ssh key 7: http 503",
                false,
                true,
                undefined,
                7,
              );
            }
          },
          {
            onReleaseLease: (lease) => {
              cleanupLeases.push(structuredClone(lease));
              if (cleanupLeases.length === 1) {
                throw new Error("temporary cleanup failure");
              }
            },
          },
        ),
      },
      { CRABBOX_WORKSPACE_SSH_PUBLIC_KEY: "ssh-ed25519 workspace-test" },
    );

    const response = await fleet.fetch(
      request("POST", "/v1/workspaces", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: {
          id: "fleet-is-761",
          runtime: "crabbox",
          ttlSeconds: 1800,
          idleTimeoutSeconds: 300,
          capabilities: { desktop: false },
        },
      }),
    );
    expect(response.status).toBe(202);

    await fleet.alarm();

    const failedLease = [...(await storage.list<LeaseRecord>({ prefix: "lease:" })).values()][0];
    expect(failedLease).toMatchObject({
      state: "failed",
      provisioningResourceMayExist: false,
      providerKeyCleanupPending: true,
      providerKeyCleanupID: "7",
    });
    expect(cleanupLeases).toHaveLength(0);
    expect(providerCreates).toBe(1);
    expect(Date.parse(failedLease.cleanupRetryAt ?? "")).toBeGreaterThan(Date.now());
    expect(storage.alarm()).toBe(Date.parse(failedLease.cleanupRetryAt!));

    await fleet.alarm();

    expect(cleanupLeases).toHaveLength(0);
    storage.seed(`lease:${failedLease.id}`, {
      ...failedLease,
      cleanupRetryAt: new Date(Date.now() - 1_000).toISOString(),
    });
    await fleet.alarm();

    expect(cleanupLeases).toHaveLength(1);
    expect(cleanupLeases[0]).toMatchObject({
      providerKeyCleanupPending: true,
      providerKeyCleanupID: "7",
    });
    const cleanupFailedLease = storage.value<LeaseRecord>(`lease:${failedLease.id}`)!;
    expect(cleanupFailedLease).toMatchObject({
      providerKeyCleanupPending: true,
      cleanupError: "temporary cleanup failure",
    });
    expect(storage.alarm()).toBe(Date.parse(cleanupFailedLease.cleanupRetryAt!));

    await fleet.alarm();

    expect(cleanupLeases).toHaveLength(1);
    storage.seed(`lease:${failedLease.id}`, {
      ...cleanupFailedLease,
      cleanupRetryAt: new Date(Date.now() - 1_000).toISOString(),
    });

    await fleet.alarm();

    expect(cleanupLeases).toHaveLength(2);
    const remainingLeases = [...(await storage.list<LeaseRecord>({ prefix: "lease:" })).values()];
    expect(remainingLeases).not.toContainEqual(
      expect.objectContaining({ providerKeyCleanupPending: true }),
    );
    expect(providerCreates).toBe(1);
  });

  it("retains the SSH key and marks the resource uncertain when Hetzner rollback fails", async () => {
    const requests: Array<{ method: string; path: string }> = [];
    const responses = [
      jsonResponse({ ssh_keys: [] }),
      jsonResponse({ ssh_keys: [] }),
      jsonResponse({
        ssh_key: {
          id: 7,
          name: "crabbox-cbx-abcdef123456",
          public_key: "ssh-ed25519 rollback-test",
          labels: { crabbox: "true", created_by: "crabbox", lease: "cbx_abcdef123456" },
        },
      }),
      jsonResponse({
        server: {
          id: 123,
          name: "crabbox-rollback",
          status: "initializing",
          server_type: { name: "cpx62" },
          public_net: { ipv4: { ip: "" } },
          labels: { crabbox: "true", lease: "cbx_abcdef123456" },
        },
      }),
      jsonResponse({ error: { code: "server_error" } }, 503),
    ];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = new URL(String(input));
        requests.push({ method: init?.method || "GET", path: url.pathname });
        return responses.shift() ?? jsonResponse({}, 500);
      }),
    );
    const client = new HetznerClient({ HETZNER_TOKEN: "test-token" } as Env);
    vi.spyOn(client, "waitForServerIP").mockRejectedValue(new Error("IP wait failed"));
    const config = leaseConfig({
      provider: "hetzner",
      providerKey: "crabbox-cbx-abcdef123456",
      sshPublicKey: "ssh-ed25519 rollback-test",
    });

    const error = await client
      .createServerWithFallback(config, "cbx_abcdef123456", "rollback", "alice@example.com")
      .catch((caught: unknown) => caught);

    expect(error).toBeInstanceOf(HetznerProvisioningError);
    expect((error as HetznerProvisioningError).resourceMayExist).toBe(true);
    expect((error as HetznerProvisioningError).serverID).toBe(123);
    expect((error as HetznerProvisioningError).providerKeyCleanupID).toBe(7);
    expect(error).toHaveProperty("message", expect.stringContaining("cleanup server 123"));
    expect(requests.filter((entry) => entry.method === "DELETE")).toEqual([
      { method: "DELETE", path: "/v1/servers/123" },
    ]);
  });

  it("does not delete a reused Hetzner SSH key after partial-server rollback", async () => {
    const requests: Array<{ method: string; path: string }> = [];
    const responses = [
      jsonResponse({
        ssh_keys: [
          {
            id: 7,
            name: "crabbox-cbx-abcdef123456",
            public_key: "ssh-ed25519 rollback-test",
            labels: { crabbox: "true", created_by: "crabbox", lease: "cbx_abcdef123456" },
          },
        ],
      }),
      jsonResponse({
        server: {
          id: 123,
          name: "crabbox-rollback",
          status: "initializing",
          server_type: { name: "cpx62" },
          public_net: { ipv4: { ip: "" } },
          labels: { crabbox: "true", lease: "cbx_abcdef123456" },
        },
      }),
      new Response(null, { status: 204 }),
    ];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = new URL(String(input));
        requests.push({ method: init?.method || "GET", path: url.pathname });
        return responses.shift() ?? jsonResponse({}, 500);
      }),
    );
    const client = new HetznerClient({ HETZNER_TOKEN: "test-token" } as Env);
    vi.spyOn(client, "waitForServerIP").mockRejectedValue(new Error("IP wait failed"));
    const config = leaseConfig({
      provider: "hetzner",
      class: "cpx62",
      serverType: "cpx62",
      providerKey: "crabbox-cbx-abcdef123456",
      sshPublicKey: "ssh-ed25519 rollback-test",
    });

    const error = await client
      .createServerWithFallback(config, "cbx_abcdef123456", "rollback", "alice@example.com")
      .catch((caught: unknown) => caught);

    expect(error).toBeInstanceOf(HetznerProvisioningError);
    expect((error as HetznerProvisioningError).resourceMayExist).toBe(false);
    expect(requests.filter((entry) => entry.method === "DELETE")).toEqual([
      { method: "DELETE", path: "/v1/servers/123" },
    ]);
  });

  it("uses the terminating Hetzner fallback error to decide retryability", async () => {
    const responses = [
      jsonResponse({ ssh_keys: [] }),
      jsonResponse({ ssh_keys: [] }),
      jsonResponse({
        ssh_key: {
          id: 1,
          name: "crabbox-workspace-test",
          public_key: "ssh-ed25519 workspace-test",
        },
      }),
      jsonResponse({ error: { code: "server_type_not_available" } }, 400),
      jsonResponse({ error: { code: "invalid_input" } }, 400),
    ];
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => responses.shift() ?? jsonResponse({}, 500)),
    );
    const client = new HetznerClient({ HETZNER_TOKEN: "test-token" } as Env);
    const config = leaseConfig({
      provider: "hetzner",
      providerKey: "crabbox-workspace-test",
      sshPublicKey: "ssh-ed25519 workspace-test",
    });

    const error = await client
      .createServerWithFallback(config, "cbx_abcdef123456", "fleet-is-100", "alice@example.com")
      .catch((caught: unknown) => caught);

    expect(error).toBeInstanceOf(HetznerProvisioningError);
    expect((error as HetznerProvisioningError).retryable).toBe(false);
  });

  it("treats a conflicting shared Hetzner SSH key as terminal", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        jsonResponse({
          ssh_keys: [
            {
              id: 1,
              name: "crabbox-workspace-test",
              public_key: "ssh-ed25519 different-key",
            },
          ],
        }),
      ),
    );
    const client = new HetznerClient({ HETZNER_TOKEN: "test-token" } as Env);
    const config = leaseConfig({
      provider: "hetzner",
      providerKey: "crabbox-workspace-test",
      sshPublicKey: "ssh-ed25519 workspace-test",
    });

    const error = await client
      .createServerWithFallback(config, "cbx_abcdef123456", "fleet-is-100", "alice@example.com")
      .catch((caught: unknown) => caught);

    expect(error).toBeInstanceOf(HetznerProvisioningError);
    expect((error as HetznerProvisioningError).resourceMayExist).toBe(false);
    expect((error as HetznerProvisioningError).retryable).toBe(false);
  });

  it("recovers Hetzner SSH key uniqueness races by key identity", async () => {
    const existing = {
      id: 42,
      name: "legacy-workspace-key",
      fingerprint: "fingerprint",
      public_key: "ssh-ed25519 workspace-test old-comment",
    };
    const responses = [
      jsonResponse({ ssh_keys: [] }),
      jsonResponse({ ssh_keys: [] }),
      jsonResponse({ error: { code: "uniqueness_error" } }, 409),
      jsonResponse({ ssh_keys: [existing] }),
    ];
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => responses.shift() ?? jsonResponse({}, 500)),
    );
    const client = new HetznerClient({ HETZNER_TOKEN: "test-token" } as Env);

    await expect(
      client.ensureSSHKey("crabbox-workspace-new", "ssh-ed25519 workspace-test new-comment"),
    ).resolves.toEqual(existing);
  });

  it("rejects a canonical Hetzner uniqueness race without lease ownership", async () => {
    const responses = [
      jsonResponse({ ssh_keys: [] }),
      jsonResponse({ ssh_keys: [] }),
      jsonResponse({ error: { code: "uniqueness_error" } }, 409),
      jsonResponse({
        ssh_keys: [
          {
            id: 41,
            name: "shared-existing-key",
            fingerprint: "shared-fingerprint",
            public_key: "ssh-ed25519 lease-test old-comment",
            labels: { crabbox: "true", created_by: "crabbox" },
          },
          {
            id: 42,
            name: "crabbox-cbx-abcdef123456",
            fingerprint: "fingerprint",
            public_key: "ssh-ed25519 lease-test old-comment",
            labels: {
              crabbox: "true",
              created_by: "crabbox",
              lease: "cbx_000000000001",
            },
          },
        ],
      }),
    ];
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => responses.shift() ?? jsonResponse({}, 500)),
    );
    const client = new HetznerClient({ HETZNER_TOKEN: "test-token" } as Env);

    await expect(
      client.ensureSSHKey(
        "crabbox-cbx-abcdef123456",
        "ssh-ed25519 lease-test new-comment",
        "cbx_abcdef123456",
      ),
    ).rejects.toThrow("is not owned by lease cbx_abcdef123456");
  });

  it("rejects a canonical Hetzner key name reserved for another lease", async () => {
    const fetchMock = vi.fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>();
    vi.stubGlobal("fetch", fetchMock);
    const client = new HetznerClient({ HETZNER_TOKEN: "test-token" } as Env);

    await expect(
      client.ensureSSHKey(
        "crabbox-cbx-000000000001",
        "ssh-ed25519 requested-key",
        "cbx_abcdef123456",
      ),
    ).rejects.toThrow("is reserved for lease cbx_000000000001");
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("binds canonical Hetzner SSH key reuse, creation, and deletion to lease labels", async () => {
    const requests: Array<{ method: string; path: string; body?: Record<string, unknown> }> = [];
    let mode: "wrong" | "missing" | "owned" = "wrong";
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const httpRequest = input instanceof Request ? input : new Request(input, init);
        const path = new URL(httpRequest.url).pathname;
        const body = httpRequest.method === "POST" ? await httpRequest.clone().json() : undefined;
        requests.push({ method: httpRequest.method, path, ...(body ? { body } : {}) });
        if (httpRequest.method === "DELETE") return jsonResponse({});
        if (httpRequest.method === "POST") {
          return jsonResponse({
            ssh_key: {
              id: 2,
              name: "crabbox-cbx-abcdef123456",
              public_key: "ssh-ed25519 requested-key",
              labels: { crabbox: "true", lease: "cbx_abcdef123456" },
            },
          });
        }
        if (mode === "missing") return jsonResponse({ ssh_keys: [] });
        return jsonResponse({
          ssh_keys: [
            {
              id: 1,
              name: "crabbox-cbx-abcdef123456",
              public_key: "ssh-ed25519 requested-key old-comment",
              labels: {
                crabbox: "true",
                created_by: "crabbox",
                lease: mode === "owned" ? "cbx_abcdef123456" : "cbx_000000000001",
              },
            },
          ],
        });
      }),
    );
    const client = new HetznerClient({ HETZNER_TOKEN: "test-token" } as Env);

    await expect(
      client.ensureSSHKey(
        "crabbox-cbx-abcdef123456",
        "ssh-ed25519 requested-key new-comment",
        "cbx_abcdef123456",
      ),
    ).rejects.toThrow("is not owned by lease cbx_abcdef123456");
    mode = "owned";
    await expect(
      client.ensureSSHKey(
        "crabbox-cbx-abcdef123456",
        "ssh-ed25519 requested-key new-comment",
        "cbx_abcdef123456",
      ),
    ).resolves.toMatchObject({ id: 1 });
    mode = "wrong";
    await client.deleteSSHKey("crabbox-cbx-abcdef123456", "cbx_abcdef123456");
    mode = "owned";
    await client.deleteSSHKey("crabbox-cbx-abcdef123456", "cbx_abcdef123456");
    expect(requests.filter((entry) => entry.method === "DELETE")).toEqual([
      { method: "DELETE", path: "/v1/ssh_keys/1" },
    ]);

    mode = "missing";
    await client.ensureSSHKey(
      "crabbox-cbx-abcdef123456",
      "ssh-ed25519 requested-key",
      "cbx_abcdef123456",
    );
    expect(requests.findLast((entry) => entry.method === "POST")?.body).toMatchObject({
      labels: { crabbox: "true", created_by: "crabbox", lease: "cbx_abcdef123456" },
    });
  });

  it("reuses a differently named Hetzner SSH key with the same identity", async () => {
    const existing = {
      id: 99,
      name: "older-workspace-key",
      fingerprint: "fingerprint",
      public_key: "ssh-ed25519 workspace-test old-comment",
    };
    const firstPage = Array.from({ length: 50 }, (_, index) => ({
      id: index + 1,
      name: `unrelated-${index}`,
      fingerprint: `fingerprint-${index}`,
      public_key: `ssh-ed25519 unrelated-${index}`,
    }));
    const responses = [
      jsonResponse({ ssh_keys: [] }),
      jsonResponse({ ssh_keys: firstPage }),
      jsonResponse({ ssh_keys: [existing] }),
    ];
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => responses.shift() ?? jsonResponse({}, 500)),
    );
    const client = new HetznerClient({ HETZNER_TOKEN: "test-token" } as Env);

    await expect(
      client.ensureSSHKey(
        "crabbox-cbx-abcdef123456",
        "ssh-ed25519 workspace-test new-comment",
        "cbx_abcdef123456",
      ),
    ).resolves.toEqual(existing);
  });

  it("preserves workspace readiness when its SSH key is reused under another name", async () => {
    const existingKey = {
      id: 42,
      name: "legacy-key",
      fingerprint: "fingerprint",
      public_key: "ssh-ed25519 workspace-test",
    };
    const initializingServer = {
      id: 123,
      name: "crabbox-workspace",
      status: "initializing",
      server_type: { name: "cpx62" },
      public_net: { ipv4: { ip: "192.0.2.123" } },
      labels: {},
    };
    const responses = [
      jsonResponse({ ssh_keys: [] }),
      jsonResponse({ ssh_keys: [existingKey] }),
      jsonResponse({ server: initializingServer }),
      jsonResponse({
        server: {
          ...initializingServer,
          status: "running",
        },
      }),
    ];
    const fetchMock = vi.fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>(
      async () => responses.shift() ?? jsonResponse({}, 500),
    );
    vi.stubGlobal("fetch", fetchMock);
    const client = new HetznerClient({ HETZNER_TOKEN: "test-token" } as Env);
    const config = leaseConfig({
      provider: "hetzner",
      providerKey: "crabbox-workspace-deadbeef0000",
      sshPublicKey: "ssh-ed25519 workspace-test",
    });

    await expect(
      client.createServerWithFallback(config, "cbx_abcdef123456", "workspace", "alice@example.com"),
    ).resolves.toMatchObject({
      server: { id: 123, status: "running" },
    });
    expect(fetchMock).toHaveBeenCalledTimes(4);
  });

  it("treats an already-absent Hetzner server as successful cleanup", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => jsonResponse({ error: { code: "not_found" } }, 404)),
    );
    const provider = new HetznerProvider({ HETZNER_TOKEN: "test-token" } as Env);
    const lease = testLease({
      provider: "hetzner",
      serverID: 123,
      providerKey: "crabbox-workspace-deadbeef0000",
    });

    await expect(provider.releaseLease(lease)).resolves.toBeUndefined();
  });

  it("deletes a persisted Hetzner server before its retained SSH key", async () => {
    const requests: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = new URL(String(input));
        requests.push(`${init?.method ?? "GET"} ${url.pathname}`);
        return new Response(null, { status: 204 });
      }),
    );
    const provider = new HetznerProvider({ HETZNER_TOKEN: "test-token" } as Env);
    const lease = testLease({
      provider: "hetzner",
      serverID: 123,
      providerKeyCleanupPending: true,
      providerKeyCleanupID: "7",
    });

    await expect(provider.releaseLease(lease)).resolves.toBeUndefined();

    expect(requests).toEqual(["DELETE /v1/servers/123", "DELETE /v1/ssh_keys/7"]);
  });

  it("recovers an unknown Hetzner server before deleting its retained SSH key", async () => {
    const requests: string[] = [];
    let labelSelector: string | null = null;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = new URL(String(input));
        requests.push(`${init?.method ?? "GET"} ${url.pathname}`);
        if ((init?.method ?? "GET") === "GET") {
          labelSelector = url.searchParams.get("label_selector");
          return jsonResponse({
            servers: [
              {
                id: 123,
                name: "crabbox-rollback",
                status: "running",
                server_type: { name: "cx23" },
                public_net: { ipv4: { ip: "192.0.2.1" } },
                labels: { crabbox: "true", lease: "cbx_abcdef123456" },
              },
            ],
          });
        }
        return new Response(null, { status: 204 });
      }),
    );
    const provider = new HetznerProvider({ HETZNER_TOKEN: "test-token" } as Env);
    const lease = testLease({
      id: "cbx_abcdef123456",
      provider: "hetzner",
      serverID: 0,
      provisioningResourceMayExist: true,
      providerKeyCleanupPending: true,
      providerKeyCleanupID: "7",
    });

    await expect(provider.releaseLease(lease)).resolves.toBeUndefined();

    expect(labelSelector).toBe("crabbox=true,lease=cbx_abcdef123456");
    expect(requests).toEqual([
      "GET /v1/servers",
      "DELETE /v1/servers/123",
      "DELETE /v1/ssh_keys/7",
    ]);
  });

  it("retains a Hetzner SSH key when persisted server cleanup fails", async () => {
    const requests: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = new URL(String(input));
        requests.push(`${init?.method ?? "GET"} ${url.pathname}`);
        return jsonResponse({ error: { code: "server_error" } }, 503);
      }),
    );
    const provider = new HetznerProvider({ HETZNER_TOKEN: "test-token" } as Env);
    const lease = testLease({
      provider: "hetzner",
      serverID: 123,
      providerKeyCleanupPending: true,
      providerKeyCleanupID: "7",
    });

    await expect(provider.releaseLease(lease)).rejects.toThrow("http 503");

    expect(requests).toEqual(["DELETE /v1/servers/123"]);
  });

  it("records a shared Hetzner key's actual name so cleanup retains it", async () => {
    const provider = new HetznerProvider({} as Env);
    const config = leaseConfig({
      provider: "hetzner",
      providerKey: "crabbox-cbx-abcdef123456",
      sshPublicKey: "ssh-ed25519 shared-test",
    });
    const lease = testLease({
      id: "cbx_abcdef123456",
      provider: "hetzner",
      providerKey: config.providerKey,
    });

    const finalized = await provider.finalizeLeaseCreate(
      config,
      lease,
      testMachine({
        provider: "hetzner",
        labels: { provider_key: "sanitized-wrong-value" },
        providerKey: "shared-existing-key!with-unsafe-label-chars",
      }),
    );

    expect(finalized.config.providerKey).toBe("shared-existing-key!with-unsafe-label-chars");
    expect(finalized.lease.providerKey).toBe("shared-existing-key!with-unsafe-label-chars");
    expect(finalized.lease.providerKeyCleanupOwned).toBe(false);
  });

  it("reserves the shared workspace provider key from ordinary leases", async () => {
    await Promise.all(
      (["hetzner", "aws", "azure", "gcp"] as const).map(async (provider) => {
        const fleet = testFleet(undefined, { [provider]: fakeProvider(undefined, { provider }) });
        const response = await fleet.fetch(
          request("POST", "/v1/leases", {
            headers: {
              "x-crabbox-owner": "alice@example.com",
              "x-crabbox-org": "example-org",
            },
            body: {
              provider,
              providerKey: "crabbox-workspace-deadbeef0000",
              sshPublicKey: "ssh-ed25519 ordinary-test",
            },
          }),
        );

        expect(response.status).toBe(400);
        await expect(response.json()).resolves.toMatchObject({ error: "reserved_provider_key" });
      }),
    );
  });

  it("derives a lease-scoped provider key for direct leases that omit one", async () => {
    const storage = new MemoryStorage();
    let preparedProviderKey = "";
    let createdProviderKey = "";
    const fleet = testFleet(storage, {
      aws: fakeProvider(
        (config) => {
          createdProviderKey = config.providerKey;
        },
        {
          provider: "aws",
          onPrepareLeaseConfig: (config) => {
            preparedProviderKey = config.providerKey;
            return config;
          },
        },
      ),
    });

    const response = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "aws",
          serverType: "c7a.8xlarge",
          sshPublicKey: "ssh-ed25519 direct-test",
        },
      }),
    );

    expect(response.status).toBe(201);
    const expectedProviderKey = "crabbox-cbx-abcdef123456";
    expect(preparedProviderKey).toBe(expectedProviderKey);
    expect(createdProviderKey).toBe(expectedProviderKey);
    expect(storage.value<LeaseRecord>("lease:cbx_abcdef123456")?.providerKey).toBe(
      expectedProviderKey,
    );
  });

  it.each(restrictedBrokerSelectorCases)(
    "requires admin auth for brokered $provider selector $field",
    async ({ provider, field, value }) => {
      const storage = new MemoryStorage();
      const providerFetch = vi.fn<typeof fetch>();
      vi.stubGlobal("fetch", providerFetch);
      const fleet = testFleet(storage);

      const response = await fleet.fetch(
        request("POST", "/v1/leases", {
          headers: {
            "x-crabbox-owner": "alice@example.com",
            "x-crabbox-org": "example-org",
          },
          body: {
            leaseID: "cbx_abcdef123456",
            provider,
            sshPublicKey: "ssh-ed25519 restricted-selector-test",
            [field]: value,
          },
        }),
      );

      expect(response.status).toBe(403);
      await expect(response.json()).resolves.toMatchObject({
        error: "admin_required",
        fields: [field],
      });
      expect(providerFetch).not.toHaveBeenCalled();
      expect((await storage.list()).size).toBe(0);
      expect(storage.alarm()).toBeUndefined();
    },
  );

  it.each(restrictedBrokerSelectorCases)(
    "preserves brokered $provider selector $field for admins",
    async ({ provider, field, value }) => {
      let preparedConfig: LeaseConfig | undefined;
      let restrictionChecks = 0;
      const fleet = testFleet(new MemoryStorage(), {
        [provider]: fakeProvider(
          (config) => {
            preparedConfig = config;
          },
          {
            provider,
            onRestrictedLeaseRequestFields: () => {
              restrictionChecks += 1;
              return [field];
            },
          },
        ),
      });

      const response = await fleet.fetch(
        request("POST", "/v1/leases", {
          headers: {
            "x-crabbox-admin": "true",
            "x-crabbox-owner": "admin@example.com",
            "x-crabbox-org": "example-org",
          },
          body: {
            leaseID: "cbx_abcdef123456",
            provider,
            sshPublicKey: "ssh-ed25519 admin-selector-test",
            [field]: value,
          },
        }),
      );

      expect(response.status).toBe(201);
      expect(restrictionChecks).toBe(0);
      expect(preparedConfig?.[field as keyof LeaseConfig]).toEqual(value);
    },
  );

  it.each([
    {
      provider: "aws" as const,
      env: {
        CRABBOX_AWS_AMI: "ami-operator-default",
        CRABBOX_AWS_SECURITY_GROUP_ID: "sg-operator-default",
        CRABBOX_AWS_SUBNET_ID: "subnet-operator-default",
        CRABBOX_AWS_INSTANCE_PROFILE: "operator-default",
      },
      body: {},
    },
    {
      provider: "gcp" as const,
      env: {
        CRABBOX_GCP_PROJECT: "operator-project",
        CRABBOX_GCP_IMAGE: "projects/example/global/images/operator-default",
        CRABBOX_GCP_NETWORK: "operator-network",
        CRABBOX_GCP_SUBNET: "operator-subnet",
        CRABBOX_GCP_TAGS: "operator-runner",
        CRABBOX_GCP_SERVICE_ACCOUNT: "runner@operator-project.iam.gserviceaccount.com",
      },
      body: { os: "ubuntu:24.04" },
    },
    {
      provider: "azure" as const,
      env: {
        CRABBOX_AZURE_IMAGE: "Canonical:0001-com-ubuntu-server-noble:24_04-lts-gen2:latest",
        CRABBOX_AZURE_OS_DISK: "ephemeral",
      },
      body: {},
    },
  ])(
    "does not treat coordinator $provider defaults as caller-supplied selectors",
    async ({ provider, env, body }) => {
      const storage = new MemoryStorage();
      const providerFetch = vi.fn<typeof fetch>();
      vi.stubGlobal("fetch", providerFetch);
      const fleet = testFleet(storage, {}, env);

      const response = await fleet.fetch(
        request("POST", "/v1/leases", {
          body: {
            leaseID: "cbx_abcdef123456",
            provider,
            sshPublicKey: "ssh-ed25519 operator-default-test",
            ...body,
          },
        }),
      );

      expect(response.status).toBe(424);
      await expect(response.json()).resolves.toMatchObject({
        error: "provider_not_configured",
        provider,
      });
      expect(providerFetch).not.toHaveBeenCalled();
      expect((await storage.list()).size).toBe(0);
    },
  );

  it("allows brokered Azure location selection without admin auth", async () => {
    const storage = new MemoryStorage();
    const providerFetch = vi.fn<typeof fetch>();
    vi.stubGlobal("fetch", providerFetch);
    const fleet = testFleet(storage);

    const response = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "azure",
          azureLocation: "westus3",
          sshPublicKey: "ssh-ed25519 azure-location-test",
        },
      }),
    );

    expect(response.status).toBe(424);
    await expect(response.json()).resolves.toMatchObject({
      error: "provider_not_configured",
      provider: "azure",
    });
    expect(providerFetch).not.toHaveBeenCalled();
    expect((await storage.list()).size).toBe(0);
    expect(storage.alarm()).toBeUndefined();
  });

  it("skips broker selector restrictions for internal workspace provisioning", async () => {
    let created = false;
    let restrictionChecks = 0;
    const fleet = testFleet(
      new MemoryStorage(),
      {
        aws: fakeProvider(
          () => {
            created = true;
          },
          {
            provider: "aws",
            onRestrictedLeaseRequestFields: () => {
              restrictionChecks += 1;
              return ["awsAMI"];
            },
          },
        ),
      },
      {
        CRABBOX_WORKSPACE_PROVIDER: "aws",
        CRABBOX_WORKSPACE_SSH_PUBLIC_KEY: "ssh-ed25519 workspace-selector-test",
        CRABBOX_WORKSPACE_SSH_PRIVATE_KEY: "workspace-private-key",
      },
    );

    const response = await fleet.fetch(
      request("POST", "/v1/workspaces", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: {
          id: "selector-policy-workspace",
          runtime: "crabbox",
          ttlSeconds: 1800,
          idleTimeoutSeconds: 360,
        },
      }),
    );

    expect(response.status).toBe(202);
    await fleet.alarm();
    expect(restrictionChecks).toBe(0);
    expect(created).toBe(true);
  });

  it("requires admin auth for custom provider key names before provider preparation", async () => {
    const createdProviderKeys: string[] = [];
    let prepareCalls = 0;
    const fleet = testFleet(undefined, {
      aws: fakeProvider(
        (config) => {
          createdProviderKeys.push(config.providerKey);
        },
        {
          provider: "aws",
          onPrepareLeaseConfig: (config) => {
            prepareCalls += 1;
            return config;
          },
        },
      ),
    });
    const create = (leaseID: string, providerKey: string, admin = false) =>
      fleet.fetch(
        request("POST", "/v1/leases", {
          headers: admin ? { "x-crabbox-admin": "true" } : undefined,
          body: {
            leaseID,
            provider: "aws",
            providerKey,
            sshPublicKey: "ssh-ed25519 direct-test",
          },
        }),
      );

    const denied = await create("cbx_abcdef123456", "foreign-key");
    expect(denied.status).toBe(403);
    await expect(denied.json()).resolves.toMatchObject({ error: "admin_required" });
    expect(prepareCalls).toBe(0);
    expect(createdProviderKeys).toEqual([]);

    const reserved = await create("cbx_abcdef123456", "crabbox-cbx-000000000001", true);
    expect(reserved.status).toBe(400);
    await expect(reserved.json()).resolves.toMatchObject({ error: "reserved_provider_key" });
    expect(prepareCalls).toBe(0);
    expect(createdProviderKeys).toEqual([]);

    const canonical = "crabbox-cbx-abcdef123456";
    expect((await create("cbx_abcdef123456", canonical)).status).toBe(201);
    expect((await create("cbx_abcdef123457", "shared-admin-key", true)).status).toBe(201);
    expect(prepareCalls).toBe(2);
    expect(createdProviderKeys).toEqual([canonical, "shared-admin-key"]);
  });

  it("only deletes provider keys canonically bound to the released lease", async () => {
    const providers = [
      new HetznerProvider({} as Env),
      new AWSProvider({} as Env, "eu-west-1", new MemoryStorage()),
    ];
    await Promise.all(
      providers.map(async (provider) => {
        vi.spyOn(provider, "deleteServer").mockResolvedValue();
        const deleteSSHKey = vi.spyOn(provider, "deleteSSHKey").mockResolvedValue();
        await provider.releaseLease(
          testLease({
            id: "cbx_abcdef123456",
            providerKey: "crabbox-cbx-000000000001",
          }),
        );
        expect(deleteSSHKey).not.toHaveBeenCalled();

        await provider.releaseLease(
          testLease({
            id: "cbx_abcdef123456",
            providerKey: "crabbox-cbx-abcdef123456",
            providerKeyCleanupOwned: true,
          }),
        );
        expect(deleteSSHKey).toHaveBeenCalledOnce();
        expect(deleteSSHKey).toHaveBeenCalledWith("crabbox-cbx-abcdef123456", "cbx_abcdef123456");
      }),
    );
  });

  it("adapts workspaces onto owner-scoped lease lifecycle", async () => {
    const storage = new MemoryStorage();
    let createdClass = "";
    let createdProviderKey = "";
    let createdHostPrivateKey = "";
    let createdHostPublicKey = "";
    let providerReleases = 0;
    const fleet = testFleet(
      storage,
      {
        hetzner: fakeProvider(
          (config) => {
            createdClass = config.class;
            createdProviderKey = config.providerKey;
            createdHostPrivateKey = config.sshHostPrivateKey;
            createdHostPublicKey = config.sshHostPublicKey;
          },
          {},
          async () => {
            providerReleases += 1;
          },
        ),
      },
      {
        CRABBOX_PUBLIC_URL: "https://crabbox.example.com",
        CRABBOX_WORKSPACE_SSH_PUBLIC_KEY: "ssh-ed25519 workspace-test",
        CRABBOX_WORKSPACE_SSH_PRIVATE_KEY: "workspace-private-key",
      },
    );
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const profile = "profile-".padEnd(100, "x");
    const body = {
      id: "fleet-is-101",
      repo: "openclaw/crabbox",
      branch: "main",
      command: 'bash -lc "echo workspace-ready; sleep 300"',
      runtime: "crabbox",
      profile,
      ttlSeconds: 1800,
      idleTimeoutSeconds: 360,
      capabilities: { desktop: true },
    };
    const create = await fleet.fetch(
      request("POST", "/v1/workspaces", {
        headers: { ...headers, "idempotency-key": body.id },
        body,
      }),
    );
    expect(create.status).toBe(202);
    const created = (await create.json()) as { providerResourceId: string };
    expect(created.providerResourceId).toMatch(/^cbx_[a-f0-9]{12}$/);
    expect(created).toMatchObject({
      status: "provisioning",
      profile,
      capabilities: { terminal: false, desktop: false, vnc: false },
    });
    const shortTTL = await fleet.fetch(
      request("POST", "/v1/workspaces", {
        headers,
        body: {
          id: "fleet-too-short",
          runtime: "crabbox",
          ttlSeconds: 1799,
          idleTimeoutSeconds: 360,
          capabilities: { desktop: false },
        },
      }),
    );
    expect(shortTTL.status).toBe(400);
    await expect(shortTTL.json()).resolves.toMatchObject({
      error: "invalid_duration",
      message: "workspace ttlSeconds must be at least 1800",
    });
    await fleet.alarm();
    expect(createdClass).toBe("standard");
    expect(createdProviderKey).toBe(
      `crabbox-workspace-${(await sha256HexForTest("ssh-ed25519 workspace-test")).slice(0, 12)}`,
    );
    expect(createdHostPrivateKey).toContain("BEGIN OPENSSH PRIVATE KEY");
    expect(createdHostPublicKey).toMatch(/^ssh-ed25519 /);
    expect(
      storage.value<{ sshHostKeySha256?: string }>(
        "workspace:example-org:alice%40example.com:fleet-is-101",
      )?.sshHostKeySha256,
    ).toMatch(/^[a-f0-9]{64}$/);
    const ready = await fleet.fetch(request("GET", `/v1/workspaces/${body.id}`, { headers }));
    const readyBody = (await ready.json()) as {
      attachUrl: string;
      capabilities: {
        terminal: boolean;
        desktop: boolean;
        vnc: boolean;
        nativeVnc: boolean;
      };
      providerResourceId: string;
      status: string;
    };
    expect(readyBody).toMatchObject({
      providerResourceId: created.providerResourceId,
      status: "ready",
      capabilities: { terminal: true, desktop: false, vnc: false, nativeVnc: true },
    });
    const terminalURL = new URL(readyBody.attachUrl);
    expect(terminalURL.origin).toBe("wss://crabbox.example.com");
    expect(terminalURL.pathname).toBe("/v1/workspaces/fleet-is-101/terminal");
    expect(terminalURL.search).toBe("?flow=ack-v1");

    const loopbackFleet = testFleet(
      storage,
      {},
      {
        CRABBOX_PUBLIC_URL: "http://127.0.0.1:8787",
        CRABBOX_WORKSPACE_SSH_PRIVATE_KEY: "workspace-private-key",
      },
    );
    const loopbackReady = await loopbackFleet.fetch(
      request("GET", `/v1/workspaces/${body.id}`, { headers }),
    );
    await expect(loopbackReady.json()).resolves.toMatchObject({
      capabilities: { terminal: false, nativeVnc: true },
    });

    const oversizedRepo = await fleet.fetch(
      request("POST", "/v1/workspaces", {
        headers,
        body: {
          ...body,
          id: "fleet-oversized-repo",
          repo: `openclaw/${"x".repeat(101)}`,
        },
      }),
    );
    expect(oversizedRepo.status).toBe(400);
    await expect(oversizedRepo.json()).resolves.toMatchObject({
      error: "invalid_workspace_request",
    });

    const leaseKey = `lease:${created.providerResourceId}`;
    const activeLease = storage.value<LeaseRecord>(leaseKey)!;
    expect(activeLease.idleTimeoutSeconds).toBe(body.ttlSeconds);
    const staleTouch = new Date(Date.now() - 60_000).toISOString();
    activeLease.lastTouchedAt = staleTouch;
    activeLease.updatedAt = staleTouch;
    storage.seed(leaseKey, activeLease);
    const inspect = await fleet.fetch(request("GET", `/v1/workspaces/${body.id}`, { headers }));
    await expect(inspect.json()).resolves.toMatchObject({
      id: body.id,
      providerResourceId: created.providerResourceId,
      status: "ready",
    });
    expect(storage.value<LeaseRecord>(leaseKey)?.lastTouchedAt).toBe(staleTouch);

    const desktop = await fleet.fetch(
      request("POST", `/v1/workspaces/${body.id}/connections/desktop`, { headers }),
    );
    expect(desktop.status).toBe(409);
    await expect(desktop.json()).resolves.toEqual({
      error: "desktop_unavailable",
      message: "workspace desktop handoff is not configured",
    });
    const missingDesktop = await fleet.fetch(
      request("POST", "/v1/workspaces/missing-workspace/connections/desktop", { headers }),
    );
    expect(missingDesktop.status).toBe(404);
    const otherOwnerDesktop = await fleet.fetch(
      request("POST", `/v1/workspaces/${body.id}/connections/desktop`, {
        headers: {
          "x-crabbox-owner": "bob@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    expect(otherOwnerDesktop.status).toBe(404);

    const nativeVNC = await fleet.fetch(
      request("POST", `/v1/workspaces/${body.id}/connections/native-vnc`, { headers }),
    );
    expect(nativeVNC.status).toBe(200);
    expect(nativeVNC.headers.get("cache-control")).toBe("no-store");
    const nativeVNCGrant = (await nativeVNC.json()) as {
      schema: string;
      brokerUrl: string;
      leaseId: string;
      ticket: string;
      expiresAt: string;
    };
    expect(nativeVNCGrant).toMatchObject({
      schema: "crabbox/native-vnc-grant/v1",
      brokerUrl: "https://crabbox.example.com",
      leaseId: created.providerResourceId,
      ticket: expect.stringMatching(/^native_vnc_[a-f0-9]{32}$/),
      expiresAt: expect.any(String),
    });
    expect(Date.parse(nativeVNCGrant.expiresAt)).toBeGreaterThan(Date.now());
    expect(storage.value(`native-vnc-ticket:${nativeVNCGrant.ticket}`)).toMatchObject({
      workspaceID: body.id,
      leaseID: created.providerResourceId,
    });
    const nativeVNCConnectionKey = "workspace:example-org:alice%40example.com:fleet-is-101";
    const nativeVNCInternals = fleet as unknown as {
      workspaceTerminals: Map<string, Set<WebSocket>>;
    };
    nativeVNCInternals.workspaceTerminals.set(
      nativeVNCConnectionKey,
      new Set(
        Array.from(
          { length: 4 },
          () =>
            ({ readyState: WebSocket.OPEN, close: vi.fn<() => void>() }) as unknown as WebSocket,
        ),
      ),
    );
    const limitedNativeVNC = await fleet.fetch(
      request("GET", "/v1/native-vnc/handoff", {
        headers: {
          authorization: `Bearer ${nativeVNCGrant.ticket}`,
          upgrade: "websocket",
        },
      }),
    );
    expect(limitedNativeVNC.status).toBe(429);
    await expect(limitedNativeVNC.json()).resolves.toMatchObject({
      error: "native_vnc_connection_limit",
    });
    nativeVNCInternals.workspaceTerminals.delete(nativeVNCConnectionKey);
    const stoppingGrantResponse = await fleet.fetch(
      request("POST", `/v1/workspaces/${body.id}/connections/native-vnc`, { headers }),
    );
    const stoppingGrant = (await stoppingGrantResponse.json()) as { ticket: string };
    const activeWorkspace = storage.value<WorkspaceRecord>(nativeVNCConnectionKey)!;
    storage.seed(nativeVNCConnectionKey, {
      ...activeWorkspace,
      releaseRequestedAt: new Date().toISOString(),
    });
    const stoppingNativeVNC = await fleet.fetch(
      request("GET", "/v1/native-vnc/handoff", {
        headers: {
          authorization: `Bearer ${stoppingGrant.ticket}`,
          upgrade: "websocket",
        },
      }),
    );
    expect(stoppingNativeVNC.status).toBe(409);
    await expect(stoppingNativeVNC.json()).resolves.toMatchObject({
      error: "native_vnc_unavailable",
    });
    storage.seed(nativeVNCConnectionKey, activeWorkspace);
    const invalidNativeVNCTicket = await fleet.fetch(
      request("GET", "/v1/native-vnc/handoff", {
        headers: {
          authorization: "Bearer native_vnc_00000000000000000000000000000000",
          upgrade: "websocket",
        },
      }),
    );
    expect(invalidNativeVNCTicket.status).toBe(401);
    await expect(invalidNativeVNCTicket.json()).resolves.toEqual({
      error: "native_vnc_ticket_invalid",
    });
    const otherOwnerNativeVNC = await fleet.fetch(
      request("POST", `/v1/workspaces/${body.id}/connections/native-vnc`, {
        headers: {
          "x-crabbox-owner": "bob@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    expect(otherOwnerNativeVNC.status).toBe(404);

    const duplicate = await fleet.fetch(request("POST", "/v1/workspaces", { headers, body }));
    await expect(duplicate.json()).resolves.toMatchObject({
      providerResourceId: created.providerResourceId,
      status: "ready",
    });
    expect(
      storage.value<{ desktopCapabilityVersion?: number }>(
        "workspace:example-org:alice%40example.com:fleet-is-101",
      )?.desktopCapabilityVersion,
    ).toBe(1);

    const legacyID = "fleet-legacy-desktop";
    storage.seed(`workspace:example-org:alice%40example.com:${legacyID}`, {
      ...storage.value<Record<string, unknown>>(
        "workspace:example-org:alice%40example.com:fleet-is-101",
      ),
      id: legacyID,
      leaseID: "cbx_legacydesktop",
      desktop: false,
      desktopCapabilityVersion: undefined,
    });
    const legacyDuplicate = await fleet.fetch(
      request("POST", "/v1/workspaces", {
        headers,
        body: { ...body, id: legacyID },
      }),
    );
    expect(legacyDuplicate.status).toBe(202);
    await expect(legacyDuplicate.json()).resolves.toMatchObject({
      providerResourceId: "cbx_legacydesktop",
      capabilities: { desktop: false, vnc: false, nativeVnc: false },
    });

    const otherOwner = await fleet.fetch(
      request("POST", "/v1/workspaces", {
        headers: {
          "x-crabbox-owner": "bob@example.com",
          "x-crabbox-org": "example-org",
        },
        body,
      }),
    );
    const other = (await otherOwner.json()) as { providerResourceId: string };
    expect(other.providerResourceId).not.toBe(created.providerResourceId);

    const released = await fleet.fetch(request("DELETE", `/v1/workspaces/${body.id}`, { headers }));
    await expect(released.json()).resolves.toMatchObject({ status: "stopped" });
    expect(providerReleases).toBe(1);
  });

  it("keeps accepted workspace creates successful when post-persist maintenance fails", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(
      storage,
      {},
      {
        CRABBOX_WORKSPACE_SSH_PUBLIC_KEY: "ssh-ed25519 workspace-test",
      },
    );
    const maintenance = fleet as unknown as {
      maintainWorkspacePrewarm: () => Promise<void>;
    };
    vi.spyOn(maintenance, "maintainWorkspacePrewarm").mockRejectedValueOnce(
      new Error("maintenance error code=10000"),
    );

    const created = await fleet.fetch(
      request("POST", "/v1/workspaces", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: {
          id: "fleet-maintenance-failure",
          runtime: "crabbox",
          ttlSeconds: 1800,
          idleTimeoutSeconds: 360,
          capabilities: { desktop: false },
        },
      }),
    );

    expect(created.status).toBe(202);
    await expect(created.json()).resolves.toMatchObject({
      id: "fleet-maintenance-failure",
      status: "provisioning",
    });
    expect(
      storage.value("workspace:example-org:alice%40example.com:fleet-maintenance-failure"),
    ).toBeDefined();
  });

  it("keeps an organization-wide workspace spare while demand is active", async () => {
    type StoredWorkspace = {
      id: string;
      leaseID: string;
      prewarm?: boolean;
      releaseRequestedAt?: string;
    };
    const storage = new MemoryStorage();
    let providerCreates = 0;
    let providerReleases = 0;
    const fleet = testFleet(
      storage,
      {
        hetzner: fakeProvider(
          () => {
            providerCreates += 1;
          },
          {},
          async () => {
            providerReleases += 1;
          },
        ),
      },
      {
        CRABBOX_WORKSPACE_PREWARM_COUNT: "1",
        CRABBOX_WORKSPACE_SSH_PUBLIC_KEY: "ssh-ed25519 workspace-test",
      },
    );
    const aliceHeaders = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const bobHeaders = {
      "x-crabbox-owner": "bob@example.com",
      "x-crabbox-org": "example-org",
    };
    const workspaceBody = {
      runtime: "crabbox",
      ttlSeconds: 1800,
      idleTimeoutSeconds: 360,
      capabilities: { desktop: false },
    };

    const aliceCreate = await fleet.fetch(
      request("POST", "/v1/workspaces", {
        headers: aliceHeaders,
        body: { ...workspaceBody, id: "fleet-prewarm-alice" },
      }),
    );
    expect(aliceCreate.status).toBe(202);
    await fleet.alarm();
    expect(providerCreates).toBe(2);

    const workspacesAfterWarmup = await storage.list<StoredWorkspace>({
      prefix: "workspace:",
    });
    const warm = [...workspacesAfterWarmup.values()].find((workspace) => workspace.prewarm);
    expect(warm).toBeDefined();
    const warmLeaseID = warm!.leaseID;
    const warmLease = storage.value<LeaseRecord>(`lease:${warmLeaseID}`)!;
    expect(storage.alarm()).toBe(Date.parse(warmLease.expiresAt) - 5 * 60_000);
    const hidden = await fleet.fetch(
      request("GET", `/v1/workspaces/${warm!.id}`, {
        headers: {
          "x-crabbox-owner": "crabbox-internal-prewarm",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    expect(hidden.status).toBe(404);

    const bobCreate = await fleet.fetch(
      request("POST", "/v1/workspaces", {
        headers: bobHeaders,
        body: { ...workspaceBody, id: "fleet-prewarm-bob" },
      }),
    );
    expect(bobCreate.status).toBe(200);
    await expect(bobCreate.json()).resolves.toMatchObject({
      providerResourceId: warmLeaseID,
      status: "ready",
    });
    expect(providerCreates).toBe(2);
    expect(storage.value<LeaseRecord>(`lease:${warmLeaseID}`)).toMatchObject({
      owner: "bob@example.com",
      org: "example-org",
      workspaceID: "fleet-prewarm-bob",
    });

    await fleet.alarm();
    expect(providerCreates).toBe(3);
    const replenished = [
      ...(await storage.list<StoredWorkspace>({ prefix: "workspace:" })).values(),
    ].filter((workspace) => workspace.prewarm && !workspace.releaseRequestedAt);
    expect(replenished).toHaveLength(1);

    await fleet.fetch(
      request("DELETE", "/v1/workspaces/fleet-prewarm-alice", { headers: aliceHeaders }),
    );
    await fleet.fetch(
      request("DELETE", "/v1/workspaces/fleet-prewarm-bob", { headers: bobHeaders }),
    );
    await fleet.alarm();
    expect(providerReleases).toBe(3);
    const activeSpares = [
      ...(await storage.list<StoredWorkspace>({ prefix: "workspace:" })).values(),
    ].filter((workspace) => workspace.prewarm && !workspace.releaseRequestedAt);
    expect(activeSpares).toHaveLength(0);
  });

  it("keeps separate workspace spares for active profiles in one organization", async () => {
    type StoredWorkspace = {
      id: string;
      profile: string;
      prewarm?: boolean;
      releaseRequestedAt?: string;
    };
    const storage = new MemoryStorage();
    let providerCreates = 0;
    const fleet = testFleet(
      storage,
      {
        hetzner: fakeProvider(() => {
          providerCreates += 1;
        }),
      },
      {
        CRABBOX_WORKSPACE_PREWARM_COUNT: "1",
        CRABBOX_WORKSPACE_SSH_PUBLIC_KEY: "ssh-ed25519 workspace-test",
      },
    );
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const body = {
      runtime: "crabbox",
      ttlSeconds: 1800,
      idleTimeoutSeconds: 360,
      capabilities: { desktop: false },
    };

    const defaultCreate = await fleet.fetch(
      request("POST", "/v1/workspaces", {
        headers,
        body: { ...body, id: "fleet-prewarm-default", profile: "default" },
      }),
    );
    expect(defaultCreate.status).toBe(202);
    await fleet.alarm();

    const buildCreate = await fleet.fetch(
      request("POST", "/v1/workspaces", {
        headers,
        body: { ...body, id: "fleet-prewarm-build", profile: "build" },
      }),
    );
    expect(buildCreate.status).toBe(202);
    await fleet.alarm();
    expect(providerCreates).toBe(4);

    const activeSpares = [
      ...(await storage.list<StoredWorkspace>({ prefix: "workspace:" })).values(),
    ].filter((workspace) => workspace.prewarm && !workspace.releaseRequestedAt);
    expect(activeSpares).toHaveLength(2);
    expect(activeSpares.map((workspace) => workspace.profile).toSorted()).toEqual([
      "build",
      "default",
    ]);
  });

  it("routes workspaces through each configured brokered provider", async () => {
    await Promise.all(
      (["hetzner", "aws", "azure", "gcp"] as const).map(async (provider) => {
        const storage = new MemoryStorage();
        if (provider === "aws") {
          storage.seed("image:aws:promoted:linux:x86_64:ubuntu26.04:eu-west-1", {
            id: "ami-promoted",
            name: "crabbox-promoted",
            state: "available",
            region: "eu-west-1",
            target: "linux",
            architecture: "x86_64",
            os: "ubuntu:26.04",
            promotedAt: "2026-06-13T00:00:00Z",
          });
        }
        let createdProvider = "";
        let createdAWSSSHCIDRs: string[] = [];
        let createdAWSAMI = "";
        const fleet = testFleet(
          storage,
          {
            [provider]: fakeProvider(
              (config) => {
                createdProvider = config.provider;
                createdAWSSSHCIDRs = config.awsSSHCIDRs;
                createdAWSAMI = config.awsAMI;
              },
              { provider },
            ),
          },
          {
            CRABBOX_WORKSPACE_PROVIDER: provider,
            CRABBOX_WORKSPACE_SSH_PUBLIC_KEY: "ssh-ed25519 workspace-test",
          },
        );
        const id = `fleet-${provider}`;
        const headers = {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        };
        const create = await fleet.fetch(
          request("POST", "/v1/workspaces", {
            headers,
            body: {
              id,
              runtime: "crabbox",
              ttlSeconds: 1800,
              idleTimeoutSeconds: 360,
            },
          }),
        );
        expect(create.status).toBe(202);
        const created = (await create.json()) as { providerResourceId: string };
        await fleet.alarm();
        expect(createdProvider).toBe(provider);
        expect(createdAWSSSHCIDRs).toEqual(provider === "aws" ? ["0.0.0.0/0"] : []);
        expect(createdAWSAMI).toBe("");
        expect(storage.value<LeaseRecord>(`lease:${created.providerResourceId}`)?.provider).toBe(
          provider,
        );
        const released = await fleet.fetch(request("DELETE", `/v1/workspaces/${id}`, { headers }));
        await expect(released.json()).resolves.toMatchObject({ status: "stopped" });
      }),
    );
  });

  it("rejects malformed workspace request bodies", async () => {
    const fleet = testFleet();
    const headers = {
      "content-type": "application/json",
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };

    await Promise.all(
      [null, [], { id: 42 }, { id: "fleet-invalid", profile: 42 }].map(async (body) => {
        const response = await fleet.fetch(
          new Request("https://coordinator.test/v1/workspaces", {
            method: "POST",
            headers,
            body: JSON.stringify(body),
          }),
        );
        expect(response.status).toBe(400);
        await expect(response.json()).resolves.toMatchObject({
          error: "invalid_workspace_request",
        });
      }),
    );
    const invalidJSON = await fleet.fetch(
      new Request("https://coordinator.test/v1/workspaces", {
        method: "POST",
        headers,
        body: "{",
      }),
    );
    expect(invalidJSON.status).toBe(400);
    await expect(invalidJSON.json()).resolves.toMatchObject({
      error: "invalid_workspace_request",
    });
    const oversizedProfile = await fleet.fetch(
      request("POST", "/v1/workspaces", {
        headers,
        body: {
          id: "fleet-invalid-profile",
          profile: "x".repeat(121),
        },
      }),
    );
    expect(oversizedProfile.status).toBe(400);
    await expect(oversizedProfile.json()).resolves.toMatchObject({
      error: "invalid_profile",
    });
    await Promise.all(
      [
        ["fleet-lock-component", "foo.lock/bar"],
        ["fleet-dot-component", "foo/.bar"],
      ].map(async ([id, branch]) => {
        const invalidBranch = await fleet.fetch(
          request("POST", "/v1/workspaces", {
            headers,
            body: { id, branch },
          }),
        );
        expect(invalidBranch.status).toBe(400);
        await expect(invalidBranch.json()).resolves.toMatchObject({
          error: "invalid_workspace_request",
        });
      }),
    );
    const oversizedCommand = await fleet.fetch(
      request("POST", "/v1/workspaces", {
        headers,
        body: {
          id: "fleet-command-too-large",
          command: "x".repeat(3_001),
          runtime: "crabbox",
        },
      }),
    );
    expect(oversizedCommand.status).toBe(400);
    await expect(oversizedCommand.json()).resolves.toMatchObject({
      error: "invalid_workspace_request",
    });
    const oversizedQuotedCommand = await fleet.fetch(
      request("POST", "/v1/workspaces", {
        headers,
        body: {
          id: "fleet-quoted-command-too-large",
          command: "'".repeat(200),
          runtime: "crabbox",
        },
      }),
    );
    expect(oversizedQuotedCommand.status).toBe(400);
  });

  it("keeps delete pending until concurrent workspace provisioning settles", async () => {
    const storage = new MemoryStorage();
    const started = deferred<void>();
    const unblock = deferred<void>();
    let providerCreates = 0;
    let providerReleases = 0;
    const fleet = testFleet(
      storage,
      {
        hetzner: fakeProvider(
          async () => {
            providerCreates += 1;
            started.resolve();
            await unblock.promise;
          },
          {},
          async () => {
            providerReleases += 1;
          },
        ),
      },
      { CRABBOX_WORKSPACE_SSH_PUBLIC_KEY: "ssh-ed25519 workspace-test" },
    );
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const body = {
      id: "fleet-is-102",
      runtime: "crabbox",
      ttlSeconds: 1800,
      idleTimeoutSeconds: 360,
      capabilities: { desktop: false },
    };
    const creating = await fleet.fetch(request("POST", "/v1/workspaces", { headers, body }));
    expect(creating.status).toBe(202);
    const provisioning = fleet.alarm();
    await started.promise;

    const duplicate = await fleet.fetch(request("POST", "/v1/workspaces", { headers, body }));
    expect(duplicate.status).toBe(202);
    const pending = (await duplicate.json()) as { providerResourceId: string };
    expect(pending).toMatchObject({ status: "provisioning" });

    const deleting = await fleet.fetch(request("DELETE", `/v1/workspaces/${body.id}`, { headers }));
    await expect(deleting.json()).resolves.toMatchObject({ status: "stopping" });

    unblock.resolve();
    await provisioning;
    const stopped = await fleet.fetch(request("GET", `/v1/workspaces/${body.id}`, { headers }));
    await expect(stopped.json()).resolves.toMatchObject({
      providerResourceId: pending.providerResourceId,
      status: "stopped",
    });
    expect(providerCreates).toBe(1);
    expect(providerReleases).toBe(1);
  });

  it("blocks provisioning when delete wins before lease reservation", async () => {
    const storage = new MemoryStorage();
    const preparing = deferred<void>();
    const unblock = deferred<void>();
    let providerCreates = 0;
    const fleet = testFleet(
      storage,
      {
        hetzner: fakeProvider(
          () => {
            providerCreates += 1;
          },
          {
            onPrepareLeaseConfig: async (config) => {
              preparing.resolve();
              await unblock.promise;
              return config;
            },
          },
        ),
      },
      { CRABBOX_WORKSPACE_SSH_PUBLIC_KEY: "ssh-ed25519 workspace-test" },
    );
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const body = {
      id: "fleet-is-105",
      runtime: "crabbox",
      ttlSeconds: 1800,
      idleTimeoutSeconds: 360,
      capabilities: { desktop: false },
    };
    await fleet.fetch(request("POST", "/v1/workspaces", { headers, body }));
    const provisioning = fleet.alarm();
    await preparing.promise;

    const deleted = await fleet.fetch(request("DELETE", `/v1/workspaces/${body.id}`, { headers }));
    await expect(deleted.json()).resolves.toMatchObject({ status: "stopped" });

    unblock.resolve();
    await provisioning;
    expect(providerCreates).toBe(0);
    expect((await storage.list({ prefix: "lease:" })).size).toBe(0);
    const stopped = await fleet.fetch(request("GET", `/v1/workspaces/${body.id}`, { headers }));
    await expect(stopped.json()).resolves.toMatchObject({ status: "stopped" });
  });

  it("provisions a persisted workspace reservation from the durable alarm", async () => {
    const storage = new MemoryStorage();
    let providerCreates = 0;
    const fleet = testFleet(
      storage,
      {
        hetzner: fakeProvider(async () => {
          providerCreates += 1;
        }),
      },
      { CRABBOX_WORKSPACE_SSH_PUBLIC_KEY: "ssh-ed25519 workspace-test" },
    );
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const now = new Date().toISOString();
    storage.seed("workspace:example-org:alice%40example.com:fleet-is-103", {
      id: "fleet-is-103",
      leaseID: "cbx_abcdef123456",
      owner: "alice@example.com",
      org: "example-org",
      profile: "default",
      provider: "hetzner",
      class: "standard",
      desktop: false,
      ttlSeconds: 1800,
      idleTimeoutSeconds: 360,
      createdAt: now,
      updatedAt: now,
    });

    const resumed = await fleet.fetch(
      request("POST", "/v1/workspaces", {
        headers,
        body: {
          id: "fleet-is-103",
          runtime: "crabbox",
          ttlSeconds: 1800,
          idleTimeoutSeconds: 360,
          capabilities: { desktop: false },
        },
      }),
    );
    await expect(resumed.json()).resolves.toMatchObject({
      providerResourceId: "cbx_abcdef123456",
      status: "provisioning",
    });
    await fleet.alarm();
    const ready = await fleet.fetch(request("GET", "/v1/workspaces/fleet-is-103", { headers }));
    await expect(ready.json()).resolves.toMatchObject({
      providerResourceId: "cbx_abcdef123456",
      status: "ready",
    });
    expect(providerCreates).toBe(1);
  });

  it("waits for an unexpired provisioning claim when no lease was written", async () => {
    const storage = new MemoryStorage();
    let providerCreates = 0;
    const fleet = testFleet(
      storage,
      {
        hetzner: fakeProvider(async () => {
          providerCreates += 1;
        }),
      },
      { CRABBOX_WORKSPACE_SSH_PUBLIC_KEY: "ssh-ed25519 workspace-test" },
    );
    const claimExpiresAt = new Date(Date.now() + 60_000).toISOString();
    storage.seed("workspace:example-org:alice%40example.com:fleet-is-121", {
      id: "fleet-is-121",
      leaseID: "cbx_abcdef123456",
      owner: "alice@example.com",
      org: "example-org",
      profile: "default",
      provider: "hetzner",
      class: "standard",
      desktop: false,
      ttlSeconds: 1800,
      idleTimeoutSeconds: 360,
      provisionClaim: "claim-121",
      provisionClaimExpiresAt: claimExpiresAt,
      createdAt: new Date().toISOString(),
      updatedAt: new Date().toISOString(),
    });

    await fleet.alarm();

    expect(providerCreates).toBe(0);
    expect(storage.alarm()).toBe(Date.parse(claimExpiresAt));
  });

  it("retries a persisted lease when the provider request never started", async () => {
    const storage = new MemoryStorage();
    let providerCreates = 0;
    const fleet = testFleet(
      storage,
      {
        hetzner: fakeProvider(async () => {
          providerCreates += 1;
        }),
      },
      { CRABBOX_WORKSPACE_SSH_PUBLIC_KEY: "ssh-ed25519 workspace-test" },
    );
    const now = new Date().toISOString();
    const workspaceKey = "workspace:example-org:alice%40example.com:fleet-is-123";
    storage.seed(workspaceKey, {
      id: "fleet-is-123",
      leaseID: "cbx_abcdef123456",
      owner: "alice@example.com",
      org: "example-org",
      profile: "default",
      provider: "hetzner",
      class: "standard",
      desktop: false,
      ttlSeconds: 1800,
      idleTimeoutSeconds: 360,
      provisionClaim: "expired-claim",
      provisionClaimExpiresAt: new Date(Date.now() - 1_000).toISOString(),
      createdAt: now,
      updatedAt: now,
    });
    storage.seed(
      "lease:cbx_abcdef123456",
      testLease({
        id: "cbx_abcdef123456",
        slug: "fleet-is-123",
        workspaceID: "fleet-is-123",
        owner: "alice@example.com",
        org: "example-org",
        cloudID: "",
        serverID: 0,
        serverName: "",
        host: "",
        keep: false,
        state: "provisioning",
        createdAt: now,
        updatedAt: now,
        lastTouchedAt: now,
        expiresAt: new Date(Date.now() + 1200_000).toISOString(),
      }),
    );

    await fleet.alarm();
    expect(providerCreates).toBe(0);
    expect(storage.value<LeaseRecord>("lease:cbx_abcdef123456")).toBeUndefined();

    const workspace = storage.value<Record<string, unknown>>(workspaceKey)!;
    storage.seed(workspaceKey, {
      ...workspace,
      reconcileAfter: new Date(Date.now() - 1_000).toISOString(),
    });
    await fleet.alarm();

    expect(providerCreates).toBe(1);
    const ready = await fleet.fetch(
      request("GET", "/v1/workspaces/fleet-is-123", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    await expect(ready.json()).resolves.toMatchObject({ status: "ready" });
  });

  it("backs off provider recovery errors after the ambiguity deadline", async () => {
    const storage = new MemoryStorage();
    let providerLookups = 0;
    const fleet = testFleet(storage, {
      hetzner: fakeProvider(undefined, {
        onList: () => {
          providerLookups += 1;
          throw new Error("provider unavailable");
        },
      }),
    });
    const old = new Date(Date.now() - 20 * 60_000).toISOString();
    const workspaceKey = "workspace:example-org:alice%40example.com:fleet-is-124";
    storage.seed(workspaceKey, {
      id: "fleet-is-124",
      leaseID: "cbx_abcdef123456",
      owner: "alice@example.com",
      org: "example-org",
      profile: "default",
      provider: "hetzner",
      class: "standard",
      desktop: false,
      ttlSeconds: 1,
      idleTimeoutSeconds: 1,
      createdAt: old,
      updatedAt: old,
    });
    storage.seed(
      "lease:cbx_abcdef123456",
      testLease({
        id: "cbx_abcdef123456",
        slug: "fleet-is-124",
        workspaceID: "fleet-is-124",
        owner: "alice@example.com",
        org: "example-org",
        cloudID: "",
        serverID: 0,
        serverName: "",
        host: "",
        keep: false,
        state: "failed",
        provisioningResourceMayExist: true,
        provisioningRequestStartedAt: old,
        createdAt: old,
        updatedAt: old,
        lastTouchedAt: old,
        expiresAt: new Date(Date.now() - 1_000).toISOString(),
      }),
    );

    await fleet.alarm();

    expect(providerLookups).toBe(1);
    expect(storage.alarm()).toBeGreaterThan(Date.now() + 5_000);
  });

  it.each([
    { id: "fleet-is-127", state: "failed" as const, retryable: true },
    { id: "fleet-is-128", state: "provisioning" as const, retryable: undefined },
  ])("retries $state provisioning after confirmed provider absence", async (testCase) => {
    const storage = new MemoryStorage();
    let providerCreates = 0;
    const fleet = testFleet(
      storage,
      {
        hetzner: fakeProvider(async () => {
          providerCreates += 1;
        }),
      },
      { CRABBOX_WORKSPACE_SSH_PUBLIC_KEY: "ssh-ed25519 workspace-test" },
    );
    const old = new Date(Date.now() - 20 * 60_000).toISOString();
    const workspaceKey = `workspace:example-org:alice%40example.com:${testCase.id}`;
    storage.seed(workspaceKey, {
      id: testCase.id,
      leaseID: "cbx_abcdef123456",
      owner: "alice@example.com",
      org: "example-org",
      profile: "default",
      provider: "hetzner",
      class: "standard",
      desktop: false,
      ttlSeconds: 3600,
      idleTimeoutSeconds: 360,
      createdAt: old,
      updatedAt: old,
    });
    storage.seed(
      "lease:cbx_abcdef123456",
      testLease({
        id: "cbx_abcdef123456",
        slug: testCase.id,
        workspaceID: testCase.id,
        owner: "alice@example.com",
        org: "example-org",
        cloudID: "",
        serverID: 0,
        serverName: "",
        host: "",
        keep: false,
        state: testCase.state,
        provisioningResourceMayExist: true,
        provisioningFailureRetryable: testCase.retryable,
        provisioningRequestStartedAt: old,
        createdAt: old,
        updatedAt: old,
        lastTouchedAt: old,
        expiresAt: new Date(Date.now() + 3600_000).toISOString(),
      }),
    );

    await fleet.alarm();

    expect(providerCreates).toBe(0);
    expect(storage.value<LeaseRecord>("lease:cbx_abcdef123456")).toBeUndefined();
    const workspace = storage.value<WorkspaceRecord>(workspaceKey)!;
    expect(Date.parse(workspace.reconcileAfter ?? "")).toBeGreaterThan(Date.now());

    storage.seed(workspaceKey, {
      ...workspace,
      reconcileAfter: new Date(Date.now() - 1_000).toISOString(),
    });
    await fleet.alarm();

    expect(providerCreates).toBe(1);
    const ready = await fleet.fetch(
      request("GET", `/v1/workspaces/${testCase.id}`, {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    await expect(ready.json()).resolves.toMatchObject({ status: "ready" });
  });

  it("does not recover an expired lease while its provider request claim is active", async () => {
    const storage = new MemoryStorage();
    let providerLookups = 0;
    const fleet = testFleet(storage, {
      hetzner: fakeProvider(undefined, {
        onList: () => {
          providerLookups += 1;
        },
      }),
    });
    const now = new Date().toISOString();
    const claimExpiresAt = new Date(Date.now() + 60_000).toISOString();
    storage.seed("workspace:example-org:alice%40example.com:fleet-is-125", {
      id: "fleet-is-125",
      leaseID: "cbx_abcdef123456",
      owner: "alice@example.com",
      org: "example-org",
      profile: "default",
      provider: "hetzner",
      class: "standard",
      desktop: false,
      ttlSeconds: 1,
      idleTimeoutSeconds: 1,
      provisionClaim: "claim-125",
      provisionClaimExpiresAt: claimExpiresAt,
      createdAt: now,
      updatedAt: now,
    });
    storage.seed(
      "lease:cbx_abcdef123456",
      testLease({
        id: "cbx_abcdef123456",
        slug: "fleet-is-125",
        workspaceID: "fleet-is-125",
        owner: "alice@example.com",
        org: "example-org",
        cloudID: "",
        serverID: 0,
        serverName: "",
        host: "",
        keep: false,
        state: "provisioning",
        provisioningRequestStartedAt: now,
        createdAt: now,
        updatedAt: now,
        lastTouchedAt: now,
        expiresAt: new Date(Date.now() - 1_000).toISOString(),
      }),
    );

    await fleet.alarm();

    expect(providerLookups).toBe(0);
    expect(storage.value<LeaseRecord>("lease:cbx_abcdef123456")?.state).toBe("provisioning");
    expect(storage.alarm()).toBe(Date.parse(claimExpiresAt));
  });

  it("does not start provider provisioning after workspace release wins", async () => {
    const storage = new MemoryStorage();
    let providerCreates = 0;
    const workspaceKey = "workspace:example-org:alice%40example.com:fleet-is-126";
    const fleet = testFleet(
      storage,
      {
        hetzner: fakeProvider(
          async () => {
            providerCreates += 1;
          },
          {
            onPrepareLeaseCreate: () => {
              const workspace = storage.value<WorkspaceRecord>(workspaceKey)!;
              const releasedAt = new Date().toISOString();
              storage.seed(workspaceKey, {
                ...workspace,
                releaseRequestedAt: releasedAt,
                updatedAt: releasedAt,
              });
              return undefined;
            },
          },
        ),
      },
      { CRABBOX_WORKSPACE_SSH_PUBLIC_KEY: "ssh-ed25519 workspace-test" },
    );
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    await fleet.fetch(
      request("POST", "/v1/workspaces", {
        headers,
        body: {
          id: "fleet-is-126",
          runtime: "crabbox",
          ttlSeconds: 1800,
          idleTimeoutSeconds: 360,
          capabilities: { desktop: false },
        },
      }),
    );

    await fleet.alarm();

    expect(providerCreates).toBe(0);
    const stopped = await fleet.fetch(request("GET", "/v1/workspaces/fleet-is-126", { headers }));
    await expect(stopped.json()).resolves.toMatchObject({ status: "stopped" });
  });

  it("revokes workspace terminals from the shared release transition", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const key = "workspace:example-org:alice%40example.com:fleet-is-127";
    const now = new Date().toISOString();
    const lease = testLease({
      id: "cbx_abcdef123457",
      slug: "fleet-is-127",
      workspaceID: "fleet-is-127",
      owner: "alice@example.com",
      org: "example-org",
      state: "active",
      createdAt: now,
      updatedAt: now,
      lastTouchedAt: now,
      expiresAt: new Date(Date.now() + 60_000).toISOString(),
    });
    storage.seed(key, {
      id: "fleet-is-127",
      leaseID: lease.id,
      owner: lease.owner,
      org: lease.org,
      profile: "default",
      repo: "openclaw/crabbox",
      branch: "main",
      command: "exec bash -l",
      provider: lease.provider,
      class: lease.class,
      desktop: false,
      ttlSeconds: 1800,
      idleTimeoutSeconds: 360,
      createdAt: now,
      updatedAt: now,
    });
    const close = vi.fn<(code?: number, reason?: string) => void>();
    const socket = { readyState: WebSocket.OPEN, close } as unknown as WebSocket;
    const internals = fleet as unknown as {
      workspaceTerminals: Map<string, Set<WebSocket>>;
      markWorkspaceReleaseRequested(lease: LeaseRecord): Promise<void>;
    };
    internals.workspaceTerminals.set(key, new Set([socket]));

    await internals.markWorkspaceReleaseRequested(lease);

    expect(close).toHaveBeenCalledWith(1008, "workspace stopping");
    expect(internals.workspaceTerminals.has(key)).toBe(false);
  });

  it("caps concurrent workspace terminal attachments before upgrading", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(
      storage,
      {},
      {
        CRABBOX_PUBLIC_URL: "https://crabbox.example.com",
        CRABBOX_WORKSPACE_SSH_PRIVATE_KEY: "workspace-private-key",
      },
    );
    const key = "workspace:example-org:alice%40example.com:fleet-is-128";
    const now = new Date().toISOString();
    const lease = testLease({
      id: "cbx_abcdef123458",
      slug: "fleet-is-128",
      workspaceID: "fleet-is-128",
      owner: "alice@example.com",
      org: "example-org",
      state: "active",
      host: "192.0.2.10",
      createdAt: now,
      updatedAt: now,
      lastTouchedAt: now,
      expiresAt: new Date(Date.now() + 60_000).toISOString(),
    });
    storage.seed(`lease:${lease.id}`, lease);
    storage.seed(key, {
      id: "fleet-is-128",
      leaseID: lease.id,
      owner: lease.owner,
      org: lease.org,
      profile: "default",
      repo: "",
      branch: "main",
      command: "exec bash -l",
      provider: lease.provider,
      class: lease.class,
      desktop: false,
      ttlSeconds: 1800,
      idleTimeoutSeconds: 360,
      createdAt: now,
      updatedAt: now,
      sshHostKeySha256: "a".repeat(64),
    });
    const internals = fleet as unknown as {
      workspaceTerminals: Map<string, Set<WebSocket>>;
    };
    internals.workspaceTerminals.set(
      key,
      new Set(
        Array.from({ length: 4 }, () => ({ readyState: WebSocket.OPEN }) as unknown as WebSocket),
      ),
    );

    const response = await fleet.fetch(
      request("GET", "/v1/workspaces/fleet-is-128/terminal", {
        headers: {
          connection: "upgrade",
          upgrade: "websocket",
          "x-crabbox-owner": lease.owner,
          "x-crabbox-org": lease.org,
        },
      }),
    );

    expect(response.status).toBe(429);
    await expect(response.json()).resolves.toMatchObject({
      error: "terminal_connection_limit",
    });
  });

  it("rejects cross-origin browser terminal handshakes", () => {
    const env = { CRABBOX_PUBLIC_URL: "https://crabbox.example.com" } as Env;

    expect(
      workspaceTerminalOriginAllowed(
        new Request("https://crabbox.example.com/v1/workspaces/fleet-is-128/terminal"),
        env,
      ),
    ).toBe(true);
    expect(
      workspaceTerminalOriginAllowed(
        new Request("https://crabbox.example.com/v1/workspaces/fleet-is-128/terminal", {
          headers: { origin: "https://crabbox.example.com" },
        }),
        env,
      ),
    ).toBe(true);
    expect(
      workspaceTerminalOriginAllowed(
        new Request("https://crabbox.example.com/v1/workspaces/fleet-is-128/terminal", {
          headers: { origin: "https://attacker.example" },
        }),
        env,
      ),
    ).toBe(false);
  });

  it("bounds WebSocket close reasons by UTF-8 bytes", () => {
    const reason = boundedSocketReason("🔥".repeat(100));

    expect(new TextEncoder().encode(reason).byteLength).toBeLessThanOrEqual(120);
    expect(reason.endsWith("\uFFFD")).toBe(false);
  });

  it("bounds terminal output until the client acknowledges forwarded bytes", async () => {
    const source = await readFile(new URL("../src/fleet.ts", import.meta.url), "utf8");
    const start = source.indexOf("private async connectWorkspaceTerminal");
    const end = source.indexOf("private trackWorkspaceTerminal", start);
    const terminal = source.slice(start, end);
    const sshStart = source.indexOf("async function connectWorkspaceSSH");
    const sshEnd = source.indexOf("async function readWorkspaceVNCPassword", sshStart);
    const ssh = source.slice(sshStart, sshEnd);

    expect(terminal).toContain("queuedOutputFrames >= workspaceTerminalMaxBufferedFrames");
    expect(terminal).toContain("queuedOutputBytes + unacknowledgedOutputBytes + data.byteLength");
    expect(terminal).toContain("unacknowledgedOutputBytes += data.byteLength");
    expect(terminal).toContain("workspaceTerminalAcknowledgement(value)");
    expect(terminal).toContain("bytes > unacknowledgedOutputBytes");
    expect(terminal).toContain("workspaceTerminalSocketBufferedBytes(socket)");
    expect(ssh).toContain('serverHostKey: ["ssh-ed25519"]');
    expect(ssh).toContain('cipher: ["aes128-ctr", "aes192-ctr", "aes256-ctr"]');
    expect(ssh).toContain('"hmac-sha2-256-etm@openssh.com"');
    expect(ssh).toContain('"hmac-sha2-512"');
    expect(ssh).toContain("workspace SSH host key mismatch expected=");
    expect(source).toContain("value.length > workspaceTerminalMaxBufferedBytes");
  });

  it("starts the native VNC viewer timeout after readiness and accepts loopback broker URLs", async () => {
    const source = await readFile(new URL("../src/fleet.ts", import.meta.url), "utf8");
    const start = source.indexOf("private async connectWorkspaceNativeVNC");
    const end = source.indexOf("private async cleanupExpiredNativeVNCTickets", start);
    const nativeVNC = source.slice(start, end);
    const errorStart = source.indexOf("function workspaceNativeVNCError");
    const errorEnd = source.indexOf("function workspacePublicURL", errorStart);
    const nativeVNCError = source.slice(errorStart, errorEnd);

    expect(nativeVNC.indexOf("socket.send(")).toBeLessThan(
      nativeVNC.indexOf("native VNC viewer did not connect"),
    );
    expect(nativeVNCError).toContain("workspaceSSHError(workspace, lease, env)");
    expect(nativeVNCError).toContain("workspacePublicURL(env)");
    expect(nativeVNCError).not.toContain("workspaceTerminalError");
  });

  it("tries every configured SSH port before delaying terminal readiness", async () => {
    const source = await readFile(new URL("../src/fleet.ts", import.meta.url), "utf8");
    const start = source.indexOf("async function connectWorkspaceSSH");
    const end = source.indexOf("async function readWorkspaceVNCPassword", start);
    const ssh = source.slice(start, end);

    expect(ssh).toContain("...(lease.sshFallbackPorts ?? [])");
    expect(ssh).toContain("for (const port of ports)");
    expect(ssh).toContain('Number.parseInt(port || "22", 10)');
    expect(ssh).toContain("candidate.end()");
    expect(ssh.indexOf("for (const port of ports)")).toBeLessThan(
      ssh.indexOf("setTimeout(resolve, 2_000)"),
    );
  });

  it("recovers incomplete clones in a neutral Crabbox workspace", async () => {
    const source = await readFile(new URL("../src/fleet.ts", import.meta.url), "utf8");
    const start = source.indexOf("function workspaceTerminalBootstrapCommand");
    const end = source.indexOf("function shellQuote", start);
    const bootstrap = source.slice(start, end);

    expect(bootstrap).toContain("/workspaces");
    expect(bootstrap).toContain("crabbox-workspace-");
    expect(bootstrap).toContain("systemctl cat crabbox-workspace-ready.service");
    expect(bootstrap).toContain("/run/crabbox/workspace-ready");
    expect(bootstrap).toContain("until crabbox-ready");
    expect(bootstrap).toContain("timeout 20m");
    expect(bootstrap.match(/\|\| exit \$\?/gu)).toHaveLength(2);
    expect(bootstrap.indexOf("/run/crabbox/workspace-ready")).toBeLessThan(
      bootstrap.indexOf("command -v tmux"),
    );
    expect(bootstrap).toContain("rev-parse --verify 'HEAD^{commit}'");
    expect(bootstrap).toContain(".clone.XXXXXX");
    expect(bootstrap).toContain("if ! git clone");
    expect(bootstrap).toContain("printf '\\\\033[2J\\\\033[H'");
    expect(bootstrap).toContain("set +e");
    expect(bootstrap).toContain("Workspace command exited with status %s");
    expect(bootstrap).toContain("exec bash -l");
    expect(bootstrap).not.toContain("exec bash -lc ${shellQuote(command)}");
    expect(bootstrap).not.toContain("/crabfleet/");
  });

  it("retries transient workspace admission failures without poisoning the workspace ID", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(
      storage,
      {},
      { CRABBOX_WORKSPACE_SSH_PUBLIC_KEY: "ssh-ed25519 workspace-test" },
    );
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const body = {
      id: "fleet-is-116",
      runtime: "crabbox",
      ttlSeconds: 1800,
      idleTimeoutSeconds: 360,
      capabilities: { desktop: false },
    };
    await fleet.fetch(request("POST", "/v1/workspaces", { headers, body }));

    await fleet.alarm();

    const pending = await fleet.fetch(request("GET", `/v1/workspaces/${body.id}`, { headers }));
    await expect(pending.json()).resolves.toMatchObject({ status: "provisioning" });
    const workspace = storage.value<Record<string, unknown>>(
      "workspace:example-org:alice%40example.com:fleet-is-116",
    )!;
    expect(workspace["error"]).toBeUndefined();
    expect(Date.parse(String(workspace["reconcileAfter"]))).toBeGreaterThan(Date.now());
    expect(storage.alarm()).toBe(Date.parse(String(workspace["reconcileAfter"])));
  });

  it("refuses late workspace provisioning that cannot recover before hard TTL", async () => {
    const storage = new MemoryStorage();
    let providerCreates = 0;
    const fleet = testFleet(
      storage,
      {
        hetzner: fakeProvider(async () => {
          providerCreates += 1;
        }),
      },
      { CRABBOX_WORKSPACE_SSH_PUBLIC_KEY: "ssh-ed25519 workspace-test" },
    );
    const createdAt = new Date(Date.now() - 16 * 60_000).toISOString();
    storage.seed("workspace:example-org:alice%40example.com:fleet-is-130", {
      id: "fleet-is-130",
      leaseID: "cbx_abcdef123456",
      owner: "alice@example.com",
      org: "example-org",
      profile: "default",
      provider: "hetzner",
      class: "standard",
      desktop: false,
      ttlSeconds: 1800,
      idleTimeoutSeconds: 360,
      createdAt,
      updatedAt: createdAt,
    });

    await fleet.alarm();

    expect(providerCreates).toBe(0);
    const failed = await fleet.fetch(
      request("GET", "/v1/workspaces/fleet-is-130", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    await expect(failed.json()).resolves.toMatchObject({
      status: "failed",
      message: "workspace provisioning recovery window no longer fits before hard TTL",
    });
  });

  it("rejects a conflicting non-workspace lease using the reserved workspace lease ID", async () => {
    const storage = new MemoryStorage();
    let providerReleases = 0;
    const fleet = testFleet(
      storage,
      {
        hetzner: fakeProvider(undefined, {}, async () => {
          providerReleases += 1;
        }),
      },
      { CRABBOX_WORKSPACE_SSH_PUBLIC_KEY: "ssh-ed25519 workspace-test" },
    );
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const body = {
      id: "fleet-is-110",
      runtime: "crabbox",
      ttlSeconds: 1800,
      idleTimeoutSeconds: 360,
      capabilities: { desktop: false },
    };
    const created = await fleet.fetch(request("POST", "/v1/workspaces", { headers, body }));
    const pending = (await created.json()) as { providerResourceId: string };
    const now = new Date().toISOString();
    storage.seed(`lease:${pending.providerResourceId}`, {
      id: pending.providerResourceId,
      slug: body.id,
      provider: "hetzner",
      target: "linux",
      desktop: false,
      browser: false,
      code: false,
      cloudID: "999",
      owner: "alice@example.com",
      org: "example-org",
      profile: "default",
      class: "standard",
      serverType: "cpx62",
      requestedServerType: "cpx62",
      serverID: 999,
      serverName: "unrelated",
      providerKey: "unrelated",
      host: "192.0.2.110",
      sshUser: "root",
      sshPort: "22",
      workRoot: "/workspaces/crabbox",
      keep: false,
      ttlSeconds: 1800,
      idleTimeoutSeconds: 360,
      estimatedHourlyUSD: 0,
      maxEstimatedUSD: 0,
      state: "provisioning",
      createdAt: now,
      updatedAt: now,
      lastTouchedAt: now,
      expiresAt: new Date(Date.now() - 1_000).toISOString(),
    } as LeaseRecord);

    await fleet.alarm();

    const failed = await fleet.fetch(request("GET", `/v1/workspaces/${body.id}`, { headers }));
    await expect(failed.json()).resolves.toMatchObject({
      status: "failed",
      message: "workspace lease reservation conflicts with another lifecycle",
    });
    expect(storage.value<LeaseRecord>(`lease:${pending.providerResourceId}`)?.state).toBe(
      "expired",
    );
    expect(providerReleases).toBe(1);
  });

  it("reserves workspace lease IDs before asynchronous provisioning starts", async () => {
    const fleet = testFleet(
      undefined,
      { hetzner: fakeProvider() },
      { CRABBOX_WORKSPACE_SSH_PUBLIC_KEY: "ssh-ed25519 workspace-test" },
    );
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const created = await fleet.fetch(
      request("POST", "/v1/workspaces", {
        headers,
        body: {
          id: "fleet-is-120",
          runtime: "crabbox",
          ttlSeconds: 1800,
          idleTimeoutSeconds: 360,
          capabilities: { desktop: false },
        },
      }),
    );
    const pending = (await created.json()) as { providerResourceId: string };

    const genericCreate = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers,
        body: {
          leaseID: pending.providerResourceId,
          provider: "hetzner",
          sshPublicKey: "ssh-ed25519 ordinary-test",
        },
      }),
    );
    expect(genericCreate.status).toBe(409);
    await expect(genericCreate.json()).resolves.toMatchObject({
      error: "workspace_managed_lease",
    });

    const registration = await fleet.fetch(
      request("PUT", `/v1/leases/${pending.providerResourceId}/registration`, {
        headers,
        body: {
          provider: "external",
          target: "linux",
          host: "host.example.test",
          ttlSeconds: 1800,
          idleTimeoutSeconds: 360,
        },
      }),
    );
    expect(registration.status).toBe(409);
    await expect(registration.json()).resolves.toMatchObject({
      error: "workspace_managed_lease",
    });
  });

  it("does not advertise an expired active workspace as ready during cleanup", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(
      storage,
      { hetzner: fakeProvider() },
      { CRABBOX_WORKSPACE_SSH_PUBLIC_KEY: "ssh-ed25519 workspace-test" },
    );
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const body = {
      id: "fleet-is-111",
      runtime: "crabbox",
      ttlSeconds: 1800,
      idleTimeoutSeconds: 360,
      capabilities: { desktop: false },
    };
    const created = await fleet.fetch(request("POST", "/v1/workspaces", { headers, body }));
    const pending = (await created.json()) as { providerResourceId: string };
    await fleet.alarm();
    const leaseKey = `lease:${pending.providerResourceId}`;
    const lease = storage.value<LeaseRecord>(leaseKey)!;
    lease.expiresAt = new Date(Date.now() - 1_000).toISOString();
    lease.cleanupStartedAt = new Date().toISOString();
    storage.seed(leaseKey, lease);

    const expired = await fleet.fetch(request("GET", `/v1/workspaces/${body.id}`, { headers }));

    await expect(expired.json()).resolves.toMatchObject({ status: "expired" });
  });

  it("recovers and releases an interrupted provider resource after release intent persisted", async () => {
    const storage = new MemoryStorage();
    const leaseID = "cbx_abcdef123456";
    let providerReleases = 0;
    const fleet = testFleet(
      storage,
      {
        hetzner: fakeProvider(
          undefined,
          {
            servers: [
              {
                provider: "hetzner",
                id: 321,
                cloudID: "321",
                name: "crabbox-fleet-is-106",
                status: "running",
                serverType: "cpx62",
                host: "192.0.2.106",
                labels: { lease: leaseID },
              },
            ],
          },
          async () => {
            providerReleases += 1;
          },
        ),
      },
      { CRABBOX_WORKSPACE_SSH_PUBLIC_KEY: "ssh-ed25519 workspace-test" },
    );
    const now = new Date().toISOString();
    const expiresAt = new Date(Date.now() - 1_000).toISOString();
    storage.seed("workspace:example-org:alice%40example.com:fleet-is-106", {
      id: "fleet-is-106",
      leaseID,
      owner: "alice@example.com",
      org: "example-org",
      profile: "default",
      provider: "hetzner",
      class: "standard",
      desktop: false,
      ttlSeconds: 1800,
      idleTimeoutSeconds: 360,
      createdAt: now,
      updatedAt: now,
      releaseRequestedAt: now,
    });
    storage.seed(`lease:${leaseID}`, {
      id: leaseID,
      slug: "fleet-is-106",
      workspaceID: "fleet-is-106",
      provider: "hetzner",
      target: "linux",
      desktop: false,
      browser: false,
      code: false,
      cloudID: "",
      owner: "alice@example.com",
      org: "example-org",
      profile: "default",
      class: "standard",
      serverType: "cpx62",
      requestedServerType: "cpx62",
      serverID: 0,
      serverName: "",
      providerKey: "workspace-test",
      host: "",
      sshUser: "root",
      sshPort: "22",
      workRoot: "/workspaces/crabbox",
      keep: false,
      ttlSeconds: 1800,
      idleTimeoutSeconds: 360,
      estimatedHourlyUSD: 0,
      maxEstimatedUSD: 0,
      state: "released",
      releaseDeletesServer: true,
      createdAt: now,
      updatedAt: now,
      lastTouchedAt: now,
      expiresAt,
    } as LeaseRecord);

    await fleet.alarm();
    expect(providerReleases).toBe(1);
    const stopped = await fleet.fetch(
      request("GET", "/v1/workspaces/fleet-is-106", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    await expect(stopped.json()).resolves.toMatchObject({
      providerResourceId: leaseID,
      status: "stopped",
    });
  });

  it("keeps failed workspace cleanup retryable until provider deletion succeeds", async () => {
    const storage = new MemoryStorage();
    let providerReleases = 0;
    const fleet = testFleet(
      storage,
      {
        hetzner: fakeProvider(undefined, {}, async () => {
          providerReleases += 1;
          if (providerReleases === 1) {
            throw new Error("provider cleanup throttled");
          }
        }),
      },
      { CRABBOX_WORKSPACE_SSH_PUBLIC_KEY: "ssh-ed25519 workspace-test" },
    );
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const body = {
      id: "fleet-is-104",
      runtime: "crabbox",
      ttlSeconds: 1800,
      idleTimeoutSeconds: 360,
      capabilities: { desktop: false },
    };
    await fleet.fetch(request("POST", "/v1/workspaces", { headers, body }));
    await fleet.alarm();

    const pending = await fleet.fetch(request("DELETE", `/v1/workspaces/${body.id}`, { headers }));
    await expect(pending.json()).resolves.toMatchObject({
      status: "stopping",
      message: "provider cleanup throttled",
    });
    const nextBody = { ...body, id: "fleet-is-107" };
    await fleet.fetch(request("POST", "/v1/workspaces", { headers, body: nextBody }));
    await fleet.alarm();
    const nextReady = await fleet.fetch(
      request("GET", `/v1/workspaces/${nextBody.id}`, { headers }),
    );
    await expect(nextReady.json()).resolves.toMatchObject({ status: "ready" });
    expect(providerReleases).toBe(1);

    const stopped = await fleet.fetch(request("DELETE", `/v1/workspaces/${body.id}`, { headers }));
    await expect(stopped.json()).resolves.toMatchObject({
      status: "stopped",
      message: "workspace stopped",
    });
    expect(providerReleases).toBe(2);
  });

  it("keeps deletion pending while recovering a provider resource after provisioning failure", async () => {
    const storage = new MemoryStorage();
    let leaseID = "";
    let providerReleases = 0;
    let providerLookups = 0;
    const fleet = testFleet(
      storage,
      {
        hetzner: fakeProvider(
          async () => {
            throw new Error("timed out waiting for server IP");
          },
          {
            onList: () => {
              providerLookups += 1;
              return providerLookups === 1
                ? []
                : [
                    {
                      provider: "hetzner",
                      id: 777,
                      cloudID: "777",
                      name: "crabbox-fleet-is-112",
                      status: "running",
                      serverType: "cpx62",
                      host: "192.0.2.112",
                      labels: { lease: leaseID },
                    },
                  ];
            },
          },
          async () => {
            providerReleases += 1;
          },
        ),
      },
      { CRABBOX_WORKSPACE_SSH_PUBLIC_KEY: "ssh-ed25519 workspace-test" },
    );
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const body = {
      id: "fleet-is-112",
      runtime: "crabbox",
      ttlSeconds: 1800,
      idleTimeoutSeconds: 360,
      capabilities: { desktop: false },
    };
    const created = await fleet.fetch(request("POST", "/v1/workspaces", { headers, body }));
    const pending = (await created.json()) as { providerResourceId: string };
    leaseID = pending.providerResourceId;

    await fleet.alarm();
    const workspaceKey = "workspace:example-org:alice%40example.com:fleet-is-112";
    const recovering = await fleet.fetch(request("GET", `/v1/workspaces/${body.id}`, { headers }));
    await expect(recovering.json()).resolves.toMatchObject({
      status: "provisioning",
      message: "workspace provisioning recovery pending",
    });
    const workspace = storage.value<Record<string, unknown>>(workspaceKey)!;
    expect(storage.alarm()).toBe(Date.parse(String(workspace["reconcileAfter"])));
    expect(storage.alarm()).toBeGreaterThan(Date.now());
    const deleting = await fleet.fetch(request("DELETE", `/v1/workspaces/${body.id}`, { headers }));
    await expect(deleting.json()).resolves.toMatchObject({ status: "stopping" });
    expect(providerReleases).toBe(0);
    const deletingWorkspace = storage.value<Record<string, unknown>>(workspaceKey)!;
    storage.seed(workspaceKey, {
      ...deletingWorkspace,
      reconcileAfter: new Date(Date.now() - 1_000).toISOString(),
    });
    await fleet.alarm();
    const retrying = await fleet.fetch(request("GET", `/v1/workspaces/${body.id}`, { headers }));
    await expect(retrying.json()).resolves.toMatchObject({ status: "stopping" });
    const retryingWorkspace = storage.value<Record<string, unknown>>(workspaceKey)!;
    expect(retryingWorkspace["error"]).toBeUndefined();
    storage.seed(workspaceKey, {
      ...retryingWorkspace,
      reconcileAfter: new Date(Date.now() - 1_000).toISOString(),
    });
    await fleet.alarm();

    const stopped = await fleet.fetch(request("GET", `/v1/workspaces/${body.id}`, { headers }));
    await expect(stopped.json()).resolves.toMatchObject({
      providerResourceId: pending.providerResourceId,
      status: "stopped",
    });
    expect(providerReleases).toBe(1);
  });

  it("uses provider-owned GCP recovery instead of project-wide label inventory", async () => {
    const storage = new MemoryStorage();
    const leaseID = "cbx_abcdef123456";
    let inventoryCalls = 0;
    let recoveryCalls = 0;
    const fleet = testFleet(storage, {
      gcp: fakeProvider(undefined, {
        provider: "gcp",
        onList: () => {
          inventoryCalls += 1;
          return [
            {
              provider: "gcp",
              id: 999,
              cloudID: "foreign-instance",
              name: "foreign-instance",
              status: "running",
              serverType: "e2-micro",
              host: "192.0.2.99",
              labels: { crabbox: "true", lease: leaseID },
            },
          ];
        },
        onRecoverServer: (lease) => {
          recoveryCalls += 1;
          expect(lease.id).toBe(leaseID);
          expect(lease.slug).toBe("fleet-is-gcp-recovery");
          return {
            provider: "gcp",
            id: 123,
            cloudID: "crabbox-fleet-is-gcp-recovery-c80c2195",
            name: "crabbox-fleet-is-gcp-recovery-c80c2195",
            status: "running",
            serverType: "e2-micro",
            region: "us-central1-a",
            host: "192.0.2.10",
            labels: {
              crabbox: "true",
              created_by: "crabbox",
              provider: "gcp",
              lease: leaseID,
            },
          };
        },
      }),
    });
    const now = new Date().toISOString();
    storage.seed("workspace:example-org:alice%40example.com:fleet-is-gcp-recovery", {
      id: "fleet-is-gcp-recovery",
      leaseID,
      owner: "alice@example.com",
      org: "example-org",
      profile: "default",
      provider: "gcp",
      class: "standard",
      desktop: false,
      ttlSeconds: 1800,
      idleTimeoutSeconds: 360,
      provisionClaim: "expired-claim",
      provisionClaimExpiresAt: new Date(Date.now() - 1_000).toISOString(),
      createdAt: now,
      updatedAt: now,
    });
    storage.seed(`lease:${leaseID}`, {
      id: leaseID,
      slug: "fleet-is-gcp-recovery",
      workspaceID: "fleet-is-gcp-recovery",
      provider: "gcp",
      providerProject: "proj",
      region: "us-central1-a",
      target: "linux",
      desktop: false,
      browser: false,
      code: false,
      cloudID: "stale-cloud-id",
      owner: "alice@example.com",
      org: "example-org",
      profile: "default",
      class: "standard",
      serverType: "e2-micro",
      requestedServerType: "e2-micro",
      serverID: 0,
      serverName: "",
      providerKey: "workspace-test",
      host: "",
      sshUser: "root",
      sshPort: "22",
      workRoot: "/workspaces/crabbox",
      keep: false,
      ttlSeconds: 1800,
      idleTimeoutSeconds: 360,
      estimatedHourlyUSD: 0,
      maxEstimatedUSD: 0,
      state: "provisioning",
      provisioningRequestStartedAt: now,
      createdAt: now,
      updatedAt: now,
      lastTouchedAt: now,
      expiresAt: new Date(Date.now() + 30 * 60 * 1000).toISOString(),
    } as LeaseRecord);

    await fleet.alarm();

    expect(recoveryCalls).toBe(1);
    expect(inventoryCalls).toBe(0);
    expect(storage.value<LeaseRecord>(`lease:${leaseID}`)).toMatchObject({
      state: "active",
      cloudID: "crabbox-fleet-is-gcp-recovery-c80c2195",
      host: "192.0.2.10",
    });
  });

  it("persists the active GCP fallback zone before provider mutation", async () => {
    const storage = new MemoryStorage();
    const leaseID = "cbx_abcdef123456";
    const fleet = testFleet(storage, {
      gcp: fakeProvider(undefined, {
        provider: "gcp",
        onPrepareLeaseCreate(config, lease) {
          return { config, lease, provisioning: {} };
        },
        async onCreateProvisioning(provisioning) {
          await provisioning?.onTargetAttempt?.({ region: "us-central1-a" });
          await provisioning?.onTargetAttempt?.({ region: "us-central1-b" });
          throw new Error("capacity unavailable");
        },
      }),
    });

    const response = await fleet.fetch(
      request("POST", "/v1/leases", {
        body: {
          leaseID,
          provider: "gcp",
          target: "linux",
          gcpProject: "proj",
          gcpZone: "us-central1-a",
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );

    expect(response.status).toBe(500);
    expect(storage.value<LeaseRecord>(`lease:${leaseID}`)).toMatchObject({
      state: "failed",
      region: "us-central1-b",
    });
  });

  it("backs off durable Hetzner server and SSH key cleanup after rollback failure", async () => {
    const storage = new MemoryStorage();
    const leaseID = "cbx_abcdef123456";
    const cleanupLeases: LeaseRecord[] = [];
    const fleet = testFleet(storage, {
      hetzner: fakeProvider(
        async () => {
          throw new HetznerProvisioningError(
            "IP wait failed; cleanup server 123: http 503",
            true,
            true,
            123,
            7,
          );
        },
        {
          provider: "hetzner",
          onReleaseLease(lease) {
            cleanupLeases.push(structuredClone(lease));
            if (cleanupLeases.length === 1) {
              throw new Error("temporary cleanup failure");
            }
          },
        },
      ),
    });

    const response = await fleet.fetch(
      request("POST", "/v1/leases", {
        body: {
          leaseID,
          provider: "hetzner",
          target: "linux",
          sshPublicKey: "ssh-ed25519 rollback-test",
        },
      }),
    );

    expect(response.status).toBe(500);
    const failedLease = storage.value<LeaseRecord>(`lease:${leaseID}`)!;
    expect(failedLease).toMatchObject({
      state: "failed",
      cloudID: "123",
      serverID: 123,
      releaseDeletesServer: true,
      providerKeyCleanupPending: true,
      providerKeyCleanupID: "7",
    });
    expect(Date.parse(failedLease.cleanupRetryAt ?? "")).toBeGreaterThan(Date.now());
    expect(storage.alarm()).toBe(Date.parse(failedLease.cleanupRetryAt!));

    await fleet.alarm();
    expect(cleanupLeases).toHaveLength(0);

    storage.seed(`lease:${leaseID}`, {
      ...failedLease,
      cleanupRetryAt: new Date(Date.now() - 1_000).toISOString(),
    });
    await fleet.alarm();

    expect(cleanupLeases).toHaveLength(1);
    expect(cleanupLeases[0]).toMatchObject({
      serverID: 123,
      providerKeyCleanupPending: true,
      providerKeyCleanupID: "7",
    });
    const retryLease = storage.value<LeaseRecord>(`lease:${leaseID}`)!;
    expect(retryLease).toMatchObject({
      cleanupAttempts: 1,
      cleanupError: "temporary cleanup failure",
      providerKeyCleanupPending: true,
    });
    expect(Date.parse(retryLease.cleanupRetryAt ?? "")).toBeGreaterThan(Date.now());
    expect(storage.alarm()).toBe(Date.parse(retryLease.cleanupRetryAt!));

    await fleet.alarm();
    expect(cleanupLeases).toHaveLength(1);

    storage.seed(`lease:${leaseID}`, {
      ...retryLease,
      cleanupRetryAt: new Date(Date.now() - 1_000).toISOString(),
    });
    await fleet.alarm();

    expect(cleanupLeases).toHaveLength(2);
    const cleanedLease = storage.value<LeaseRecord>(`lease:${leaseID}`)!;
    expect(cleanedLease.cleanupRetryAt).toBeUndefined();
    expect(cleanedLease.providerKeyCleanupPending).toBeUndefined();
    expect(cleanedLease.providerKeyCleanupID).toBeUndefined();
  });

  it.each([
    { case: "server and key", cloudID: "123", serverID: 123 },
    { case: "key only", cloudID: "", serverID: 0 },
  ])(
    "preserves pending Hetzner $case cleanup after a no-delete release until explicit deletion",
    async ({ cloudID, serverID }) => {
      const storage = new MemoryStorage();
      const cleanupLeases: LeaseRecord[] = [];
      const fleet = testFleet(storage, {
        hetzner: fakeProvider(undefined, {
          provider: "hetzner",
          onReleaseLease(lease) {
            cleanupLeases.push(structuredClone(lease));
          },
        }),
      });
      const lease = testLease({
        id: "cbx_abcdef123456",
        owner: "alice@example.com",
        org: "example-org",
        state: "failed",
        cloudID,
        serverID,
        releaseDeletesServer: true,
        providerKeyCleanupPending: true,
        providerKeyCleanupID: "7",
        cleanupAttempts: 1,
        cleanupError: "temporary cleanup failure",
        cleanupRetryAt: new Date(Date.now() - 1_000).toISOString(),
      });
      storage.seed(`lease:${lease.id}`, lease);
      const headers = {
        "x-crabbox-owner": lease.owner,
        "x-crabbox-org": lease.org,
      };

      const retain = await fleet.fetch(
        request("POST", `/v1/leases/${lease.id}/release`, {
          headers,
          body: { delete: false },
        }),
      );
      expect(retain.status).toBe(200);
      await expect(retain.json()).resolves.toMatchObject({
        lease: {
          state: "released",
          releaseDeletesServer: false,
          providerKeyCleanupPending: true,
          providerKeyCleanupID: "7",
        },
      });
      expect(storage.value<LeaseRecord>(`lease:${lease.id}`)?.cleanupRetryAt).toBeUndefined();
      expect(storage.alarm()).toBeUndefined();

      await fleet.alarm();
      expect(cleanupLeases).toHaveLength(0);
      expect(storage.alarm()).toBeUndefined();

      const remove = await fleet.fetch(
        request("POST", `/v1/leases/${lease.id}/release`, {
          headers,
          body: { delete: true },
        }),
      );
      expect(remove.status).toBe(200);
      expect(cleanupLeases).toHaveLength(1);
      expect(cleanupLeases[0]).toMatchObject({
        cloudID,
        serverID,
        providerKeyCleanupPending: true,
        providerKeyCleanupID: "7",
      });
      expect(storage.value<LeaseRecord>(`lease:${lease.id}`)).toMatchObject({
        state: "released",
      });
      expect(
        storage.value<LeaseRecord>(`lease:${lease.id}`)?.providerKeyCleanupPending,
      ).toBeUndefined();
      expect(storage.value<LeaseRecord>(`lease:${lease.id}`)?.providerKeyCleanupID).toBeUndefined();
    },
  );

  it("does not reschedule workspace alarms for retained pending key cleanup", async () => {
    const storage = new MemoryStorage();
    const now = new Date().toISOString();
    const lease = testLease({
      id: "cbx_abcdef123456",
      workspaceID: "fleet-is-retained",
      owner: "alice@example.com",
      org: "example-org",
      state: "released",
      releaseDeletesServer: false,
      providerKeyCleanupPending: true,
      providerKeyCleanupID: "7",
      releasedAt: now,
      endedAt: now,
      updatedAt: now,
    });
    storage.seed(`lease:${lease.id}`, lease);
    storage.seed("workspace:example-org:alice%40example.com:fleet-is-retained", {
      id: "fleet-is-retained",
      leaseID: lease.id,
      owner: lease.owner,
      org: lease.org,
      profile: lease.profile,
      provider: lease.provider,
      class: lease.class,
      desktop: false,
      ttlSeconds: lease.ttlSeconds,
      idleTimeoutSeconds: 360,
      createdAt: now,
      updatedAt: now,
      releaseRequestedAt: now,
    });
    const fleet = testFleet(storage);

    await fleet.alarm();

    expect(storage.alarm()).toBeUndefined();
    expect(storage.value<LeaseRecord>(`lease:${lease.id}`)).toMatchObject({
      state: "released",
      releaseDeletesServer: false,
      providerKeyCleanupPending: true,
      providerKeyCleanupID: "7",
    });
  });

  it("persists failed Hetzner workspace resources for durable rollback cleanup", async () => {
    const storage = new MemoryStorage();
    const cleanupLeases: LeaseRecord[] = [];
    const fleet = testFleet(
      storage,
      {
        hetzner: fakeProvider(
          async () => {
            throw new HetznerProvisioningError(
              "IP wait failed; cleanup server 123: http 503",
              true,
              true,
              123,
              7,
            );
          },
          {
            provider: "hetzner",
            onReleaseLease(lease) {
              cleanupLeases.push(structuredClone(lease));
            },
          },
        ),
      },
      { CRABBOX_WORKSPACE_SSH_PUBLIC_KEY: "ssh-ed25519 workspace-test" },
    );
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const body = {
      id: "fleet-is-rollback",
      runtime: "crabbox",
      ttlSeconds: 1800,
      idleTimeoutSeconds: 360,
      capabilities: { desktop: false },
    };
    await fleet.fetch(request("POST", "/v1/workspaces", { headers, body }));

    await fleet.alarm();

    const failedLease = [...(await storage.list<LeaseRecord>({ prefix: "lease:" })).values()][0]!;
    expect(failedLease).toMatchObject({
      workspaceID: body.id,
      state: "failed",
      cloudID: "123",
      serverID: 123,
      releaseDeletesServer: true,
      providerKeyCleanupPending: true,
      providerKeyCleanupID: "7",
    });
    expect(Date.parse(failedLease.cleanupRetryAt ?? "")).toBeGreaterThan(Date.now());
    expect(cleanupLeases).toHaveLength(0);

    storage.seed(`lease:${failedLease.id}`, {
      ...failedLease,
      cleanupRetryAt: new Date(Date.now() - 1_000).toISOString(),
    });
    await fleet.alarm();

    expect(cleanupLeases).toHaveLength(1);
    expect(cleanupLeases[0]).toMatchObject({
      cloudID: "123",
      serverID: 123,
      providerKeyCleanupPending: true,
      providerKeyCleanupID: "7",
    });
    expect(
      storage.value<LeaseRecord>(`lease:${failedLease.id}`)?.providerKeyCleanupPending,
    ).toBeUndefined();
  });

  it("fails deterministic workspace provisioning errors without waiting for the lease TTL", async () => {
    const storage = new MemoryStorage();
    let providerLookups = 0;
    const fleet = testFleet(
      storage,
      {
        hetzner: fakeProvider(
          async () => {
            throw new Error("hetzner POST /servers: http 400: invalid_input");
          },
          {
            onList: () => {
              providerLookups += 1;
              return [];
            },
          },
        ),
      },
      { CRABBOX_WORKSPACE_SSH_PUBLIC_KEY: "ssh-ed25519 workspace-test" },
    );
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const body = {
      id: "fleet-is-117",
      runtime: "crabbox",
      ttlSeconds: 1800,
      idleTimeoutSeconds: 360,
      capabilities: { desktop: false },
    };
    await fleet.fetch(request("POST", "/v1/workspaces", { headers, body }));

    await fleet.alarm();

    const failed = await fleet.fetch(request("GET", `/v1/workspaces/${body.id}`, { headers }));
    await expect(failed.json()).resolves.toMatchObject({
      status: "failed",
      message: "hetzner POST /servers: http 400: invalid_input",
    });
    const lease = [...(await storage.list<LeaseRecord>({ prefix: "lease:" })).values()][0];
    expect(lease?.provisioningResourceMayExist).toBe(false);

    const stopping = await fleet.fetch(request("DELETE", `/v1/workspaces/${body.id}`, { headers }));
    await expect(stopping.json()).resolves.toMatchObject({ status: "stopping" });
    await fleet.alarm();
    const stopped = await fleet.fetch(request("GET", `/v1/workspaces/${body.id}`, { headers }));
    await expect(stopped.json()).resolves.toMatchObject({ status: "stopped" });
    expect(providerLookups).toBe(0);
  });

  it("retries transient workspace provisioning failures when no resource can exist", async () => {
    const storage = new MemoryStorage();
    let providerCreates = 0;
    const fleet = testFleet(
      storage,
      {
        hetzner: fakeProvider(async () => {
          providerCreates += 1;
          if (providerCreates === 1) {
            throw new HetznerProvisioningError(
              "hetzner POST /servers: http 429: rate_limit_exceeded",
              false,
              true,
            );
          }
        }),
      },
      { CRABBOX_WORKSPACE_SSH_PUBLIC_KEY: "ssh-ed25519 workspace-test" },
    );
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const body = {
      id: "fleet-is-119",
      runtime: "crabbox",
      ttlSeconds: 1800,
      idleTimeoutSeconds: 360,
      capabilities: { desktop: false },
    };
    await fleet.fetch(request("POST", "/v1/workspaces", { headers, body }));
    const workspaceKey = "workspace:example-org:alice%40example.com:fleet-is-119";

    await fleet.alarm();
    const recovering = await fleet.fetch(request("GET", `/v1/workspaces/${body.id}`, { headers }));
    await expect(recovering.json()).resolves.toMatchObject({ status: "provisioning" });

    for (let attempt = 0; attempt < 2; attempt += 1) {
      const workspace = storage.value<Record<string, unknown>>(workspaceKey)!;
      storage.seed(workspaceKey, {
        ...workspace,
        reconcileAfter: new Date(Date.now() - 1_000).toISOString(),
      });
      // oxlint-disable-next-line eslint/no-await-in-loop -- retry phases depend on persisted state.
      await fleet.alarm();
    }

    const ready = await fleet.fetch(request("GET", `/v1/workspaces/${body.id}`, { headers }));
    await expect(ready.json()).resolves.toMatchObject({ status: "ready" });
    expect(providerCreates).toBe(2);
  });

  it("does not retry workspace cost-limit failures", async () => {
    const storage = new MemoryStorage();
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        expiresAt: new Date(Date.now() + 60_000).toISOString(),
      }),
    );
    const fleet = testFleet(
      storage,
      { hetzner: fakeProvider() },
      {
        CRABBOX_MAX_ACTIVE_LEASES: "1",
        CRABBOX_WORKSPACE_SSH_PUBLIC_KEY: "ssh-ed25519 workspace-test",
      },
    );
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const body = {
      id: "fleet-is-118",
      runtime: "crabbox",
      ttlSeconds: 1800,
      idleTimeoutSeconds: 360,
      capabilities: { desktop: false },
    };
    await fleet.fetch(request("POST", "/v1/workspaces", { headers, body }));

    await fleet.alarm();

    const failed = await fleet.fetch(request("GET", `/v1/workspaces/${body.id}`, { headers }));
    await expect(failed.json()).resolves.toMatchObject({
      status: "failed",
      message: expect.stringContaining("active lease limit exceeded"),
    });
    const workspace = storage.value<Record<string, unknown>>(
      "workspace:example-org:alice%40example.com:fleet-is-118",
    );
    expect(workspace?.["reconcileAfter"]).toBeUndefined();
  });

  it("bounds workspace records per owner and prunes old terminal reservations", async () => {
    const storage = new MemoryStorage();
    const old = new Date(Date.now() - 25 * 60 * 60_000).toISOString();
    for (let index = 0; index < 100; index += 1) {
      const id = `fleet-cap-${index}`;
      const leaseID = `cbx_${index.toString(16).padStart(12, "0")}`;
      storage.seed(`workspace:example-org:alice%40example.com:${id}`, {
        id,
        leaseID,
        owner: "alice@example.com",
        org: "example-org",
        profile: "default",
        provider: "hetzner",
        class: "standard",
        desktop: false,
        ttlSeconds: 1800,
        idleTimeoutSeconds: 360,
        createdAt: old,
        updatedAt: old,
        releaseRequestedAt: old,
      });
      storage.seed(`workspace-lease:${leaseID}`, true);
    }
    const fleet = testFleet(
      storage,
      { hetzner: fakeProvider() },
      { CRABBOX_WORKSPACE_SSH_PUBLIC_KEY: "ssh-ed25519 workspace-test" },
    );
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const limited = await fleet.fetch(
      request("POST", "/v1/workspaces", {
        headers,
        body: {
          id: "fleet-over-limit",
          runtime: "crabbox",
          ttlSeconds: 1800,
          idleTimeoutSeconds: 360,
          capabilities: { desktop: false },
        },
      }),
    );
    expect(limited.status).toBe(429);
    await expect(limited.json()).resolves.toMatchObject({
      error: "workspace_limit_exceeded",
    });

    await fleet.alarm();

    expect((await storage.list({ prefix: "workspace:" })).size).toBe(0);
    expect((await storage.list({ prefix: "workspace-lease:" })).size).toBe(0);
  });

  it("schedules terminal workspace retention cleanup", async () => {
    const storage = new MemoryStorage();
    const terminalAt = new Date().toISOString();
    storage.seed("workspace:example-org:alice%40example.com:fleet-is-122", {
      id: "fleet-is-122",
      leaseID: "cbx_abcdef123456",
      owner: "alice@example.com",
      org: "example-org",
      profile: "default",
      provider: "hetzner",
      class: "standard",
      desktop: false,
      ttlSeconds: 1800,
      idleTimeoutSeconds: 360,
      createdAt: terminalAt,
      updatedAt: terminalAt,
      releaseRequestedAt: terminalAt,
    });
    const fleet = testFleet(storage);

    await fleet.alarm();

    expect(storage.alarm()).toBe(Date.parse(terminalAt) + 24 * 60 * 60_000);
  });

  it("detaches a retained lease when its terminal workspace record is pruned", async () => {
    const storage = new MemoryStorage();
    const terminalAt = new Date(Date.now() - 25 * 60 * 60_000).toISOString();
    const workspaceKey = "workspace:example-org:alice%40example.com:fleet-is-131";
    storage.seed(workspaceKey, {
      id: "fleet-is-131",
      leaseID: "cbx_abcdef123456",
      owner: "alice@example.com",
      org: "example-org",
      profile: "default",
      provider: "hetzner",
      class: "standard",
      desktop: false,
      ttlSeconds: 1800,
      idleTimeoutSeconds: 360,
      createdAt: terminalAt,
      updatedAt: terminalAt,
      error: "workspace provisioning failed",
    });
    storage.seed(
      "lease:cbx_abcdef123456",
      testLease({
        id: "cbx_abcdef123456",
        slug: "fleet-is-131",
        workspaceID: "fleet-is-131",
        owner: "alice@example.com",
        org: "example-org",
        state: "failed",
        failureError: "workspace provisioning failed",
        provisioningResourceMayExist: false,
        createdAt: terminalAt,
        updatedAt: terminalAt,
        endedAt: terminalAt,
      }),
    );
    const fleet = testFleet(storage);

    await fleet.alarm();

    expect(storage.value(workspaceKey)).toBeUndefined();
    expect(storage.value<LeaseRecord>("lease:cbx_abcdef123456")?.workspaceID).toBeUndefined();
  });

  it("keeps workspace-owned leases behind workspace deletion and cleans retained resources", async () => {
    const storage = new MemoryStorage();
    let providerReleases = 0;
    const fleet = testFleet(
      storage,
      {
        hetzner: fakeProvider(undefined, {}, async () => {
          providerReleases += 1;
        }),
      },
      { CRABBOX_WORKSPACE_SSH_PUBLIC_KEY: "ssh-ed25519 workspace-test" },
    );
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const body = {
      id: "fleet-is-113",
      runtime: "crabbox",
      ttlSeconds: 1800,
      idleTimeoutSeconds: 360,
      capabilities: { desktop: false },
    };
    const created = await fleet.fetch(request("POST", "/v1/workspaces", { headers, body }));
    const pending = (await created.json()) as { providerResourceId: string };
    await fleet.alarm();

    const leaseKey = `lease:${pending.providerResourceId}`;
    const lastTouchedAt = storage.value<LeaseRecord>(leaseKey)?.lastTouchedAt;
    const genericHeartbeat = await fleet.fetch(
      request("POST", `/v1/leases/${pending.providerResourceId}/heartbeat`, {
        headers,
        body: { idleTimeoutSeconds: 3_600 },
      }),
    );
    expect(genericHeartbeat.status).toBe(409);
    await expect(genericHeartbeat.json()).resolves.toMatchObject({
      error: "workspace_managed_lease",
    });
    expect(storage.value<LeaseRecord>(leaseKey)?.lastTouchedAt).toBe(lastTouchedAt);

    const genericRelease = await fleet.fetch(
      request("POST", `/v1/leases/${pending.providerResourceId}/release`, {
        headers,
        body: { delete: false },
      }),
    );
    expect(genericRelease.status).toBe(409);
    await expect(genericRelease.json()).resolves.toMatchObject({
      error: "workspace_managed_lease",
    });

    const retained = storage.value<LeaseRecord>(leaseKey)!;
    retained.state = "released";
    retained.releaseDeletesServer = false;
    storage.seed(leaseKey, retained);

    const stopped = await fleet.fetch(request("DELETE", `/v1/workspaces/${body.id}`, { headers }));
    await expect(stopped.json()).resolves.toMatchObject({ status: "stopped" });
    expect(providerReleases).toBe(1);
  });

  it("routes portal release of a workspace lease through workspace cleanup state", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(
      storage,
      {
        hetzner: fakeProvider(undefined, {}, async () => {
          throw new Error("provider cleanup throttled");
        }),
      },
      { CRABBOX_WORKSPACE_SSH_PUBLIC_KEY: "ssh-ed25519 workspace-test" },
    );
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const body = {
      id: "fleet-is-115",
      runtime: "crabbox",
      ttlSeconds: 1800,
      idleTimeoutSeconds: 360,
      capabilities: { desktop: false },
    };
    const created = await fleet.fetch(request("POST", "/v1/workspaces", { headers, body }));
    const pending = (await created.json()) as { providerResourceId: string };
    await fleet.alarm();

    const release = await fleet.fetch(
      request("POST", `/portal/leases/${pending.providerResourceId}/release`, { headers }),
    );
    expect(release.status).toBe(500);

    const stopping = await fleet.fetch(request("GET", `/v1/workspaces/${body.id}`, { headers }));
    await expect(stopping.json()).resolves.toMatchObject({
      status: "stopping",
      message: "provider cleanup throttled",
    });
  });

  it("schedules claimed workspace provisioning from the claim deadline", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const leaseID = "cbx_abcdef123456";
    const now = new Date().toISOString();
    const claimExpiresAt = new Date(Date.now() + 60_000).toISOString();
    const expiresAt = new Date(Date.now() + 5_000).toISOString();
    storage.seed("workspace:example-org:alice%40example.com:fleet-is-108", {
      id: "fleet-is-108",
      leaseID,
      owner: "alice@example.com",
      org: "example-org",
      profile: "default",
      provider: "hetzner",
      class: "standard",
      desktop: false,
      ttlSeconds: 1,
      idleTimeoutSeconds: 1,
      provisionClaim: "claim-108",
      provisionClaimExpiresAt: claimExpiresAt,
      createdAt: now,
      updatedAt: now,
    });
    storage.seed(`lease:${leaseID}`, {
      id: leaseID,
      slug: "fleet-is-108",
      workspaceID: "fleet-is-108",
      provider: "hetzner",
      target: "linux",
      desktop: false,
      browser: false,
      code: false,
      cloudID: "",
      owner: "alice@example.com",
      org: "example-org",
      profile: "default",
      class: "standard",
      serverType: "cpx62",
      requestedServerType: "cpx62",
      serverID: 0,
      serverName: "",
      providerKey: "workspace-test",
      host: "",
      sshUser: "root",
      sshPort: "22",
      workRoot: "/workspaces/crabbox",
      keep: false,
      ttlSeconds: 1,
      idleTimeoutSeconds: 1,
      estimatedHourlyUSD: 0,
      maxEstimatedUSD: 0,
      state: "provisioning",
      createdAt: now,
      updatedAt: now,
      lastTouchedAt: now,
      expiresAt,
    } as LeaseRecord);

    await fleet.alarm();

    expect(storage.alarm()).toBe(Date.parse(claimExpiresAt));
  });

  it("expires an interrupted workspace resource after identity is recovered without an IP", async () => {
    const storage = new MemoryStorage();
    const leaseID = "cbx_abcdef123456";
    let providerReleases = 0;
    const fleet = testFleet(storage, {
      hetzner: fakeProvider(
        undefined,
        {
          servers: [
            {
              provider: "hetzner",
              id: 654,
              cloudID: "654",
              name: "crabbox-fleet-is-109",
              status: "initializing",
              serverType: "cpx62",
              host: "",
              labels: { lease: leaseID },
            },
          ],
        },
        async () => {
          providerReleases += 1;
        },
      ),
    });
    const now = new Date().toISOString();
    const workspaceKey = "workspace:example-org:alice%40example.com:fleet-is-109";
    storage.seed(workspaceKey, {
      id: "fleet-is-109",
      leaseID,
      owner: "alice@example.com",
      org: "example-org",
      profile: "default",
      provider: "hetzner",
      class: "standard",
      desktop: false,
      ttlSeconds: 1800,
      idleTimeoutSeconds: 360,
      provisionClaim: "expired-claim",
      provisionClaimExpiresAt: new Date(Date.now() - 1_000).toISOString(),
      createdAt: now,
      updatedAt: now,
    });
    storage.seed(`lease:${leaseID}`, {
      id: leaseID,
      slug: "fleet-is-109",
      workspaceID: "fleet-is-109",
      provider: "hetzner",
      target: "linux",
      desktop: false,
      browser: false,
      code: false,
      cloudID: "",
      owner: "alice@example.com",
      org: "example-org",
      profile: "default",
      class: "standard",
      serverType: "cpx62",
      requestedServerType: "cpx62",
      serverID: 0,
      serverName: "",
      providerKey: "workspace-test",
      host: "",
      sshUser: "root",
      sshPort: "22",
      workRoot: "/workspaces/crabbox",
      keep: false,
      ttlSeconds: 1800,
      idleTimeoutSeconds: 360,
      estimatedHourlyUSD: 0,
      maxEstimatedUSD: 0,
      state: "provisioning",
      provisioningRequestStartedAt: now,
      createdAt: now,
      updatedAt: now,
      lastTouchedAt: now,
      endedAt: now,
      releasedAt: now,
      expiresAt: new Date(Date.now() + 360_000).toISOString(),
    } as LeaseRecord);

    await fleet.alarm();
    expect(storage.value<LeaseRecord>(`lease:${leaseID}`)).toMatchObject({
      cloudID: "654",
      serverID: 654,
      state: "provisioning",
      host: "",
    });
    expect(storage.value<LeaseRecord>(`lease:${leaseID}`)?.endedAt).toBeUndefined();
    expect(storage.value<LeaseRecord>(`lease:${leaseID}`)?.releasedAt).toBeUndefined();

    const recovered = storage.value<LeaseRecord>(`lease:${leaseID}`)!;
    recovered.expiresAt = new Date(Date.now() - 1_000).toISOString();
    storage.seed(`lease:${leaseID}`, recovered);
    await fleet.alarm();

    const expired = await fleet.fetch(
      request("GET", "/v1/workspaces/fleet-is-109", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    await expect(expired.json()).resolves.toMatchObject({
      providerResourceId: leaseID,
      status: "expired",
    });
    expect(providerReleases).toBe(1);
  });

  it("does not report a recovered initializing server as ready when it already has an IP", async () => {
    const storage = new MemoryStorage();
    const leaseID = "cbx_abcdef123456";
    const fleet = testFleet(storage, {
      hetzner: fakeProvider(undefined, {
        servers: [
          {
            provider: "hetzner",
            id: 655,
            cloudID: "655",
            name: "crabbox-fleet-is-129",
            status: "initializing",
            serverType: "cx53",
            host: "192.0.2.129",
            labels: { lease: leaseID },
          },
        ],
      }),
    });
    const old = new Date(Date.now() - 20 * 60_000).toISOString();
    storage.seed("workspace:example-org:alice%40example.com:fleet-is-129", {
      id: "fleet-is-129",
      leaseID,
      owner: "alice@example.com",
      org: "example-org",
      profile: "default",
      provider: "hetzner",
      class: "standard",
      desktop: false,
      ttlSeconds: 3600,
      idleTimeoutSeconds: 360,
      createdAt: old,
      updatedAt: old,
    });
    storage.seed(
      `lease:${leaseID}`,
      testLease({
        id: leaseID,
        slug: "fleet-is-129",
        workspaceID: "fleet-is-129",
        owner: "alice@example.com",
        org: "example-org",
        cloudID: "",
        serverID: 0,
        serverName: "",
        host: "",
        keep: false,
        ttlSeconds: 3600,
        idleTimeoutSeconds: 3600,
        state: "provisioning",
        provisioningRequestStartedAt: old,
        createdAt: old,
        updatedAt: old,
        lastTouchedAt: old,
        expiresAt: new Date(Date.now() + 3600_000).toISOString(),
      }),
    );

    await fleet.alarm();

    const pending = await fleet.fetch(
      request("GET", "/v1/workspaces/fleet-is-129", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    await expect(pending.json()).resolves.toMatchObject({ status: "provisioning" });
    expect(storage.value<LeaseRecord>(`lease:${leaseID}`)).toMatchObject({
      cloudID: "655",
      state: "provisioning",
      serverType: "cx53",
      estimatedHourlyUSD: 0.1,
      maxEstimatedUSD: 0.1,
    });
  });

  it("expires interrupted workspace recovery through normal lease cleanup", async () => {
    const storage = new MemoryStorage();
    const leaseID = "cbx_abcdef123456";
    const fleet = testFleet(storage, {
      hetzner: fakeProvider(undefined, {}, undefined, async () => {
        throw new Error("hetzner GET /servers/654: http 404: not found");
      }),
    });
    const now = new Date().toISOString();
    const workspaceKey = "workspace:example-org:alice%40example.com:fleet-is-114";
    storage.seed(workspaceKey, {
      id: "fleet-is-114",
      leaseID,
      owner: "alice@example.com",
      org: "example-org",
      profile: "default",
      provider: "hetzner",
      class: "standard",
      desktop: false,
      ttlSeconds: 1800,
      idleTimeoutSeconds: 360,
      createdAt: now,
      updatedAt: now,
    });
    storage.seed(`lease:${leaseID}`, {
      id: leaseID,
      slug: "fleet-is-114",
      workspaceID: "fleet-is-114",
      provider: "hetzner",
      target: "linux",
      desktop: false,
      browser: false,
      code: false,
      cloudID: "654",
      owner: "alice@example.com",
      org: "example-org",
      profile: "default",
      class: "standard",
      serverType: "cpx62",
      requestedServerType: "cpx62",
      serverID: 654,
      serverName: "crabbox-fleet-is-114",
      providerKey: "workspace-test",
      host: "",
      sshUser: "root",
      sshPort: "22",
      workRoot: "/workspaces/crabbox",
      keep: false,
      ttlSeconds: 1800,
      idleTimeoutSeconds: 360,
      estimatedHourlyUSD: 0,
      maxEstimatedUSD: 0,
      state: "provisioning",
      createdAt: now,
      updatedAt: now,
      lastTouchedAt: now,
      expiresAt: new Date(Date.now() + 360_000).toISOString(),
    } as LeaseRecord);

    for (let attempt = 0; attempt < 3; attempt += 1) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- each retry depends on persisted recovery state.
      await fleet.alarm();
      const workspace = storage.value<Record<string, unknown>>(workspaceKey)!;
      storage.seed(workspaceKey, {
        ...workspace,
        reconcileAfter: new Date(Date.now() - 1_000).toISOString(),
      });
    }

    expect(storage.value<LeaseRecord>(`lease:${leaseID}`)?.failureError).toBeUndefined();
    const expiredLease = storage.value<LeaseRecord>(`lease:${leaseID}`)!;
    expiredLease.expiresAt = new Date(Date.now() - 1_000).toISOString();
    storage.seed(`lease:${leaseID}`, expiredLease);
    const workspace = storage.value<Record<string, unknown>>(workspaceKey)!;
    storage.seed(workspaceKey, {
      ...workspace,
      createdAt: new Date(Date.now() - 40 * 60_000).toISOString(),
      reconcileAfter: new Date(Date.now() - 1_000).toISOString(),
    });
    await fleet.alarm();

    expect(storage.value<LeaseRecord>(`lease:${leaseID}`)?.state).toBe("expired");
    const expired = await fleet.fetch(
      request("GET", "/v1/workspaces/fleet-is-114", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    await expect(expired.json()).resolves.toMatchObject({ status: "expired" });
    expect(storage.value<LeaseRecord>(`lease:${leaseID}`)?.failureError).toBeUndefined();
    expect(storage.value<LeaseRecord>(`lease:${leaseID}`)?.cleanupError).toBeUndefined();
  });

  it("registers client-owned leases without granting the coordinator provider lifecycle", async () => {
    const storage = new MemoryStorage();
    let providerReleases = 0;
    const fleet = testFleet(storage, {
      aws: fakeProvider(undefined, { provider: "aws" }, async () => {
        providerReleases += 1;
      }),
    });
    const ownerHeaders = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };

    const registered = await fleet.fetch(
      request("PUT", "/v1/leases/cbx_000000000099/registration", {
        headers: ownerHeaders,
        body: {
          slug: "test-box",
          provider: "aws",
          target: "linux",
          desktop: true,
          browser: true,
          code: true,
          cloudID: "i-registered-test",
          serverName: "test-box",
          serverType: "cpu16",
          host: "test-box.example.com",
          sshUser: "dev-user",
          sshPort: "22",
          exposedPorts: ["3000", "8080"],
          workRoot: "/var/lib/crabbox/work",
          ttlSeconds: 14_400,
          idleTimeoutSeconds: 1_800,
        },
      }),
    );
    expect(registered.status).toBe(201);
    await expect(registered.json()).resolves.toMatchObject({
      lease: {
        id: "cbx_000000000099",
        slug: "test-box",
        provider: "aws",
        lifecycle: "registered",
        state: "active",
        estimatedHourlyUSD: 0,
        exposedPorts: ["3000", "8080"],
      },
    });

    const portal = await fleet.fetch(
      request("GET", "/portal/leases/test-box", { headers: ownerHeaders }),
    );
    expect(portal.status).toBe(200);
    const portalHTML = await portal.text();
    expect(portalHTML).toContain("client managed");
    expect(portalHTML).toContain("remove registration");

    const portalIndex = await fleet.fetch(request("GET", "/portal", { headers: ownerHeaders }));
    expect(portalIndex.status).toBe(200);
    const portalIndexHTML = await portalIndex.text();
    expect(portalIndexHTML).toContain('data-release-kind="registered"');
    expect(portalIndexHTML).toContain('title="Remove test-box registration"');
    expect(portalIndexHTML).toContain(
      "The external machine will keep running. Use crabbox stop locally to shut it down.",
    );

    const poolRegistration = await fleet.fetch(
      request("POST", "/v1/ready-pools/example/register", {
        headers: ownerHeaders,
        body: { leaseID: "cbx_000000000099" },
      }),
    );
    expect(poolRegistration.status).toBe(400);
    await expect(poolRegistration.json()).resolves.toMatchObject({
      error: "unsupported_lifecycle",
    });

    const shared = await fleet.fetch(
      request("PUT", "/v1/leases/test-box/share", {
        headers: ownerHeaders,
        body: { users: { "friend@example.com": "use" } },
      }),
    );
    expect(shared.status).toBe(200);
    const friend = await fleet.fetch(
      request("GET", "/v1/leases/test-box", {
        headers: {
          "x-crabbox-owner": "friend@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    expect(friend.status).toBe(200);

    const released = await fleet.fetch(
      request("POST", "/portal/leases/test-box/release?return=/portal", {
        headers: ownerHeaders,
      }),
    );
    expect(released.status).toBe(303);
    expect(released.headers.get("location")).toBe("/portal");
    expect(providerReleases).toBe(0);
    expect(storage.value<LeaseRecord>("lease:cbx_000000000099")).toMatchObject({
      lifecycle: "registered",
      state: "released",
    });
  });

  it("expires registered leases without invoking a provider", async () => {
    const storage = new MemoryStorage();
    let providerReleases = 0;
    const fleet = testFleet(storage, {
      aws: fakeProvider(undefined, { provider: "aws" }, async () => {
        providerReleases += 1;
      }),
    });
    storage.seed(
      "lease:cbx_000000000098",
      testLease({
        id: "cbx_000000000098",
        provider: "aws",
        lifecycle: "registered",
        cloudID: "i-registered-expired",
        expiresAt: "2026-05-01T00:00:01.000Z",
      }),
    );

    await fleet.alarm();

    expect(providerReleases).toBe(0);
    expect(storage.value<LeaseRecord>("lease:cbx_000000000098")?.state).toBe("expired");
  });

  it("does not let registration overwrite another owner or managed lease", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    storage.seed(
      "lease:cbx_000000000097",
      testLease({ id: "cbx_000000000097", owner: "other@example.com" }),
    );
    const response = await fleet.fetch(
      request("PUT", "/v1/leases/cbx_000000000097/registration", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: { provider: "external", target: "linux", host: "host.example.test" },
      }),
    );
    expect(response.status).toBe(409);
  });

  it("persists an immutable runtime adapter binding on registered leases", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const path = "/v1/leases/cbx_000000000096/registration";
    const body = {
      provider: "external",
      target: "linux",
      host: "host.example.test",
      runtimeAdapterID: "example-adapter",
      runtimeAdapterWorkspaceID: "example-workspace-96",
      runtimeAdapterRegistrationID: "registration-generation-96",
    };

    const unclaimed = await fleet.fetch(request("PUT", path, { headers, body }));
    expect(unclaimed.status).toBe(409);
    await expect(unclaimed.json()).resolves.toMatchObject({ error: "runtime_adapter_unclaimed" });

    const claimed = await fleet.fetch(
      request("POST", "/v1/adapters/example-adapter/ticket", { headers, body: {} }),
    );
    expect(claimed.status).toBe(200);

    const created = await fleet.fetch(request("PUT", path, { headers, body }));
    expect(created.status).toBe(201);
    await expect(created.json()).resolves.toMatchObject({
      lease: {
        runtimeAdapterID: "example-adapter",
        runtimeAdapterWorkspaceID: "example-workspace-96",
        runtimeAdapterRegistrationID: "registration-generation-96",
      },
    });
    expect(storage.value("runtime-adapter-identity:example-adapter")).toMatchObject({
      claimVersion: 1,
      claimState: "confirmed",
      confirmedAt: expect.any(String),
    });

    const duplicateBinding = await fleet.fetch(
      request("PUT", "/v1/leases/cbx_000000000094/registration", { headers, body }),
    );
    expect(duplicateBinding.status).toBe(409);
    await expect(duplicateBinding.json()).resolves.toMatchObject({
      error: "runtime_adapter_workspace_conflict",
    });

    const refreshed = await fleet.fetch(
      request("PUT", path, {
        headers,
        body: { provider: "external", target: "linux", host: "host.example.test" },
      }),
    );
    expect(refreshed.status).toBe(200);
    await expect(refreshed.json()).resolves.toMatchObject({
      lease: {
        runtimeAdapterID: "example-adapter",
        runtimeAdapterWorkspaceID: "example-workspace-96",
      },
    });

    const changed = await fleet.fetch(
      request("PUT", path, {
        headers,
        body: { ...body, runtimeAdapterWorkspaceID: "example-workspace-other" },
      }),
    );
    expect(changed.status).toBe(409);
    await expect(changed.json()).resolves.toMatchObject({ error: "runtime_adapter_conflict" });

    const otherHeaders = {
      "x-crabbox-owner": "mallory@example.com",
      "x-crabbox-org": "example-org",
    };
    const otherClaim = await fleet.fetch(
      request("POST", "/v1/adapters/other-adapter/ticket", {
        headers: otherHeaders,
        body: {},
      }),
    );
    expect(otherClaim.status).toBe(200);
    const wrongOwner = await fleet.fetch(
      request("PUT", "/v1/leases/cbx_000000000095/registration", {
        headers,
        body: {
          ...body,
          runtimeAdapterID: "other-adapter",
          runtimeAdapterWorkspaceID: "example-workspace-95",
        },
      }),
    );
    expect(wrongOwner.status).toBe(409);
    await expect(wrongOwner.json()).resolves.toMatchObject({ error: "runtime_adapter_conflict" });

    const released = await fleet.fetch(
      request("POST", "/v1/leases/cbx_000000000096/release", { headers }),
    );
    expect(released.status).toBe(200);
    const rebound = await fleet.fetch(
      request("PUT", "/v1/leases/cbx_000000000094/registration", {
        headers,
        body: {
          ...body,
          runtimeAdapterRegistrationID: "registration-generation-94",
        },
      }),
    );
    expect(rebound.status).toBe(201);
    const ordinary = await fleet.fetch(
      request("PUT", path, {
        headers,
        body: { provider: "external", target: "linux", host: "ordinary.example.test" },
      }),
    );
    expect(ordinary.status).toBe(200);
    const ordinaryBody = (await ordinary.json()) as { lease: LeaseRecord };
    expect(ordinaryBody.lease.runtimeAdapterID).toBeUndefined();
    expect(ordinaryBody.lease.runtimeAdapterWorkspaceID).toBeUndefined();
    const ordinaryRelease = await fleet.fetch(
      request("POST", "/portal/leases/cbx_000000000096/release", { headers }),
    );
    expect(ordinaryRelease.status).toBe(303);
    expect(storage.value<LeaseRecord>("lease:cbx_000000000094")?.state).toBe("active");
  });

  it("keeps expired leases active when provider cleanup fails and retries later", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage, {
      aws: fakeProvider(undefined, { provider: "aws" }, async () => {
        throw new Error("aws terminate throttled");
      }),
    });
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        provider: "aws",
        cloudID: "i-000000000001",
        region: "eu-west-1",
        expiresAt: "2026-05-01T00:00:01.000Z",
      }),
    );
    await fleet.alarm();

    const lease = storage.value<LeaseRecord>("lease:cbx_000000000001");
    expect(lease?.state).toBe("active");
    expect(lease?.cleanupAttempts).toBe(1);
    expect(lease?.cleanupError).toContain("aws terminate throttled");
    expect(Date.parse(lease?.cleanupRetryAt ?? "")).toBeGreaterThan(Date.now());
    expect(storage.alarm()).toBe(Date.parse(lease?.cleanupRetryAt ?? ""));
  });

  it("expires leases only after provider cleanup succeeds", async () => {
    const storage = new MemoryStorage();
    let deletes = 0;
    const fleet = testFleet(storage, {
      aws: fakeProvider(undefined, { provider: "aws" }, async () => {
        deletes += 1;
      }),
    });
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        provider: "aws",
        cloudID: "i-000000000001",
        region: "eu-west-1",
        cleanupAttempts: 2,
        cleanupError: "previous failure",
        cleanupFailedAt: "2026-05-01T00:00:10.000Z",
        cleanupRetryAt: "2026-05-01T00:05:10.000Z",
        expiresAt: "2026-05-01T00:00:01.000Z",
      }),
    );

    await fleet.alarm();

    const lease = storage.value<LeaseRecord>("lease:cbx_000000000001");
    expect(deletes).toBe(1);
    expect(lease?.state).toBe("expired");
    expect(lease?.cleanupAttempts).toBeUndefined();
    expect(lease?.cleanupError).toBeUndefined();
    expect(lease?.cleanupRetryAt).toBeUndefined();
    expect(storage.alarm()).toBeUndefined();
  });

  it("keeps heartbeats flowing while expiry cleanup runs and reconciles AWS ingress", async () => {
    const storage = new MemoryStorage();
    const deleteStarted = deferred<void>();
    const finishDelete = deferred<void>();
    const reconciliations: string[][] = [];
    const fleet = testFleet(storage, {
      aws: fakeProvider(
        undefined,
        {
          provider: "aws",
          onReconcileLeaseAccess(_lease, context) {
            reconciliations.push(
              context.activeLeases
                .filter((candidate) => candidate.provider === "aws" && candidate.state === "active")
                .map((candidate) => candidate.id)
                .toSorted(),
            );
          },
        },
        async () => {
          deleteStarted.resolve();
          await finishDelete.promise;
        },
      ),
    });
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        provider: "aws",
        cloudID: "i-expired",
        region: "eu-west-1",
        network: {
          awsSecurityGroupID: "sg-shared",
          sshSourceCIDRs: ["198.51.100.10/32"],
          sshSourceCIDRsComplete: true,
        },
        expiresAt: new Date(Date.now() - 1_000).toISOString(),
      }),
    );
    storage.seed(
      "lease:cbx_000000000002",
      testLease({
        id: "cbx_000000000002",
        provider: "aws",
        owner: "alice@example.com",
        org: "example-org",
        cloudID: "i-active",
        region: "eu-west-1",
        network: {
          awsSecurityGroupID: "sg-shared",
          sshSourceCIDRs: ["198.51.100.20/32"],
          sshSourceCIDRsComplete: true,
        },
        expiresAt: new Date(Date.now() + 60_000).toISOString(),
      }),
    );

    const alarm = fleet.alarm();
    await deleteStarted.promise;
    const heartbeatCompleted = await Promise.race([
      fleet
        .fetch(
          request("POST", "/v1/leases/cbx_000000000002/heartbeat", {
            headers: {
              "x-crabbox-owner": "alice@example.com",
              "x-crabbox-org": "example-org",
            },
            body: { idleTimeoutSeconds: 300 },
          }),
        )
        .then((response) => response.status === 200),
      new Promise<false>((resolve) => setTimeout(() => resolve(false), 250)),
    ]);
    expect(heartbeatCompleted).toBe(true);

    finishDelete.resolve();
    await alarm;
    expect(storage.value<LeaseRecord>("lease:cbx_000000000001")?.state).toBe("expired");
    expect(reconciliations).toEqual([["cbx_000000000002"]]);
    expect(storage.value("aws-ingress-reconcile:pending")).toBeUndefined();
  });

  it("keeps provider cleanup failures active even when the cloud instance is already gone", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage, {
      aws: fakeProvider(undefined, { provider: "aws" }, async () => {
        throw new Error("aws instance not found: i-000000000001");
      }),
    });
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        provider: "aws",
        cloudID: "i-000000000001",
        region: "eu-west-1",
        cleanupAttempts: 2,
        cleanupError: "previous failure",
        cleanupFailedAt: "2026-05-01T00:00:10.000Z",
        cleanupRetryAt: "2026-05-01T00:05:10.000Z",
        expiresAt: "2026-05-01T00:00:01.000Z",
      }),
    );

    await fleet.alarm();

    const lease = storage.value<LeaseRecord>("lease:cbx_000000000001");
    expect(lease?.state).toBe("active");
    expect(lease?.cleanupAttempts).toBe(3);
    expect(lease?.cleanupError).toContain("aws instance not found");
    expect(Date.parse(lease?.cleanupRetryAt ?? "")).toBeGreaterThan(Date.now());
    expect(storage.alarm()).toBe(Date.parse(lease?.cleanupRetryAt ?? ""));
  });

  it("schedules the real expiry before stale cleanup retry metadata", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const expiresAt = new Date(Date.now() + 60_000).toISOString();
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        cleanupAttempts: 1,
        cleanupError: "previous failure",
        cleanupFailedAt: new Date(Date.now() - 60_000).toISOString(),
        cleanupRetryAt: new Date(Date.now() + 300_000).toISOString(),
        expiresAt,
      }),
    );

    await fleet.alarm();

    expect(storage.alarm()).toBe(Date.parse(expiresAt));
  });

  it("clears cleanup retry metadata when an active lease heartbeats", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        owner: "peter@example.com",
        org: "openclaw",
        cleanupAttempts: 1,
        cleanupError: "previous failure",
        cleanupFailedAt: new Date(Date.now() - 60_000).toISOString(),
        cleanupRetryAt: new Date(Date.now() + 300_000).toISOString(),
        expiresAt: new Date(Date.now() + 60_000).toISOString(),
      }),
    );
    const heartbeat = await fleet.fetch(
      request("POST", "/v1/leases/cbx_000000000001/heartbeat", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
        body: { idleTimeoutSeconds: 120 },
      }),
    );

    expect(heartbeat.status).toBe(200);
    const { lease } = (await heartbeat.json()) as { lease: LeaseRecord };
    expect(lease.cleanupAttempts).toBeUndefined();
    expect(lease.cleanupError).toBeUndefined();
    expect(lease.cleanupRetryAt).toBeUndefined();
    expect(storage.alarm()).toBe(Date.parse(lease.expiresAt));
  });

  it("offers providers heartbeat request source and active lease access state", async () => {
    const storage = new MemoryStorage();
    let refreshContext: { requestSourceCIDRs: string[]; activeLeases: LeaseRecord[] } | undefined;
    const fleet = testFleet(storage, {
      aws: fakeProvider(undefined, {
        provider: "aws",
        onRefreshLeaseAccess(lease, context) {
          refreshContext = context;
          return {
            ...lease,
            network: { sshSourceCIDRs: context.requestSourceCIDRs },
          };
        },
      }),
    });
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        provider: "aws",
        target: "macos",
        owner: "alice@example.com",
        org: "example-org",
        region: "eu-west-1",
        serverType: "mac2.metal",
        providerKey: "crabbox-cbx-000000000001",
        sshUser: "ec2-user",
        sshPort: "2222",
        sshFallbackPorts: ["22"],
        workRoot: "/Users/ec2-user/crabbox",
        expiresAt: new Date(Date.now() + 60_000).toISOString(),
      }),
    );
    storage.seed(
      "lease:cbx_000000000002",
      testLease({
        id: "cbx_000000000002",
        provider: "aws",
        network: { sshSourceCIDRs: ["203.0.113.9/32"] },
        expiresAt: new Date(Date.now() + 60_000).toISOString(),
      }),
    );

    const heartbeat = await fleet.fetch(
      request("POST", "/v1/leases/cbx_000000000001/heartbeat", {
        headers: {
          "cf-connecting-ip": "198.51.100.44",
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: {},
      }),
    );

    expect(heartbeat.status).toBe(200);
    const heartbeatBody = (await heartbeat.json()) as { lease: LeaseRecord };
    expect(heartbeatBody.lease.network?.sshSourceCIDRs).toEqual(["198.51.100.44/32"]);
    expect(refreshContext?.requestSourceCIDRs).toEqual(["198.51.100.44/32"]);
    expect(
      refreshContext?.activeLeases.find((lease) => lease.id === "cbx_000000000002")?.network
        ?.sshSourceCIDRs,
    ).toEqual(["203.0.113.9/32"]);
    expect(storage.value<LeaseRecord>("lease:cbx_000000000001")?.network?.sshSourceCIDRs).toEqual([
      "198.51.100.44/32",
    ]);
  });

  it("does not postpone the first AWS orphan sweep alarm on repeated scheduling", async () => {
    const storage = new MemoryStorage();
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        owner: "alice@example.com",
        org: "example-org",
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );
    const fleet = testFleet(
      storage,
      {},
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "secret",
      },
    );

    const first = await fleet.fetch(
      request("POST", "/v1/leases/cbx_000000000001/heartbeat", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: { idleTimeoutSeconds: 3600 },
      }),
    );
    const firstAlarm = storage.alarm();
    const second = await fleet.fetch(
      request("POST", "/v1/leases/cbx_000000000001/heartbeat", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: { idleTimeoutSeconds: 3600 },
      }),
    );

    expect(first.status).toBe(200);
    expect(second.status).toBe(200);
    expect(storage.alarm()).toBe(firstAlarm);
  });

  it("serializes Azure deferred cleanup persistence with alarm reconciliation", async () => {
    const storage = new MemoryStorage();
    let tail = Promise.resolve();
    const state = {
      storage,
      async runExclusive<T>(callback: () => Promise<T>): Promise<T> {
        const predecessor = tail;
        const successor = deferred<void>();
        tail = successor.promise;
        await predecessor;
        try {
          return await callback();
        } finally {
          successor.resolve();
        }
      },
    };
    const staleRead = deferred<void>();
    const staleBlocked = deferred<void>();
    const staleSchedule = state.runExclusive(async () => {
      expect((await storage.list({ prefix: "azure-cleanup:" })).size).toBe(0);
      staleRead.resolve();
      await staleBlocked.promise;
      await storage.setAlarm(Date.now() + 60_000);
    });
    await staleRead.promise;

    const deferredCleanup = recordAzureDeferredCleanup(
      state,
      async () => {
        const records = await storage.list<{ retryAt: string }>({ prefix: "azure-cleanup:" });
        const retryAt = Date.parse([...records.values()][0]?.retryAt ?? "");
        await storage.setAlarm(Math.max(Date.now() + 1000, retryAt));
      },
      {
        name: "crabbox-deferred",
        location: "eastus",
        resourceGroup: "crabbox-leases",
        createdAt: new Date().toISOString(),
      },
    );
    staleBlocked.resolve();
    await Promise.all([staleSchedule, deferredCleanup]);

    expect(storage.value("azure-cleanup:eastus:crabbox-deferred")).toMatchObject({
      attempts: 0,
      location: "eastus",
      name: "crabbox-deferred",
    });
    expect(storage.alarm()).toBeLessThanOrEqual(Date.now() + 1500);
  });

  it("bootstraps AWS orphan sweep maintenance from the Worker cron route", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(
      storage,
      { aws: fakeProvider(undefined, { provider: "aws", servers: [] }) },
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "secret",
        CRABBOX_AWS_ORPHAN_SWEEP_ENABLED: "1",
        CRABBOX_AWS_ORPHAN_SWEEP_INTERVAL_SECONDS: "3600",
      },
    );

    const unauthorized = await fleet.fetch(request("POST", "/v1/internal/scheduled"));
    expect(unauthorized.status).toBe(401);

    const response = await fleet.fetch(
      request("POST", "/v1/internal/scheduled", {
        headers: { "x-crabbox-internal": "scheduled" },
      }),
    );

    expect(response.status).toBe(200);
    expect(storage.value("aws-orphan-sweep:last")).toMatchObject({
      trigger: "alarm",
      scanned: 0,
      candidates: [],
    });
    expect(storage.alarm()).toBeGreaterThan(Date.now());
  });

  it("reports AWS orphan sweep candidates from the coordinator alarm without delete enabled", async () => {
    const storage = new MemoryStorage();
    const deleted: string[] = [];
    const oldSeconds = String(Math.trunc((Date.now() - 60 * 60 * 1000) / 1000));
    const fleet = testFleet(
      storage,
      {
        aws: fakeProvider(
          undefined,
          {
            provider: "aws",
            servers: [
              testMachine({
                cloudID: "i-orphan",
                labels: {
                  crabbox: "true",
                  lease: "cbx_missing",
                  created_at: oldSeconds,
                  expires_at: oldSeconds,
                },
              }),
            ],
          },
          async (id) => {
            deleted.push(id);
          },
        ),
      },
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "secret",
        CRABBOX_AWS_ORPHAN_SWEEP_GRACE_SECONDS: "1",
        CRABBOX_AWS_ORPHAN_SWEEP_INTERVAL_SECONDS: "60",
      },
    );

    await fleet.alarm();

    const sweep = storage.value<{
      mode: string;
      scanned: number;
      terminated: number;
      candidates: Array<Record<string, unknown>>;
    }>("aws-orphan-sweep:last");
    expect(deleted).toEqual([]);
    expect(sweep).toMatchObject({ mode: "report", scanned: 1, terminated: 0 });
    expect(sweep?.candidates).toEqual([
      expect.objectContaining({
        cloudID: "i-orphan",
        leaseID: "cbx_missing",
        reason: "expired-provider-tag",
        action: "reported",
      }),
    ]);
    expect(storage.alarm()).toBeGreaterThan(Date.now());
  });

  it("keeps tag-only AWS orphan sweep candidates report-only in delete mode", async () => {
    const storage = new MemoryStorage();
    const deleted: string[] = [];
    const oldSeconds = String(Math.trunc((Date.now() - 60 * 60 * 1000) / 1000));
    const fleet = testFleet(
      storage,
      {
        aws: fakeProvider(
          undefined,
          {
            provider: "aws",
            servers: [
              testMachine({
                cloudID: "i-orphan",
                labels: {
                  crabbox: "true",
                  lease: "cbx_missing",
                  created_at: oldSeconds,
                  expires_at: oldSeconds,
                },
              }),
            ],
          },
          async (id) => {
            deleted.push(id);
          },
        ),
      },
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "secret",
        CRABBOX_AWS_ORPHAN_SWEEP_DELETE: "1",
        CRABBOX_AWS_ORPHAN_SWEEP_GRACE_SECONDS: "1",
      },
    );

    await fleet.alarm();

    const sweep = storage.value<{
      mode: string;
      terminated: number;
      candidates: Array<Record<string, unknown>>;
    }>("aws-orphan-sweep:last");
    expect(deleted).toEqual([]);
    expect(sweep).toMatchObject({ mode: "delete", terminated: 0 });
    expect(sweep?.candidates[0]).toMatchObject({
      cloudID: "i-orphan",
      ownership: "provider-tags-only",
      action: "reported",
    });
  });

  it("terminates AWS orphan sweep candidates with exact coordinator ownership", async () => {
    const storage = new MemoryStorage();
    const deleted: string[] = [];
    const oldSeconds = String(Math.trunc((Date.now() - 60 * 60 * 1000) / 1000));
    storage.seed(
      "lease:cbx_000000000776",
      testLease({
        id: "cbx_000000000776",
        provider: "aws",
        cloudID: "i-orphan",
        region: "eu-west-1",
        state: "expired",
        keep: false,
      }),
    );
    storage.seed(
      "lease:cbx_000000000774",
      testLease({
        id: "cbx_000000000774",
        provider: "aws",
        cloudID: "i-wrong-region",
        region: "us-east-1",
        state: "expired",
        keep: false,
      }),
    );
    const fleet = testFleet(
      storage,
      {
        aws: fakeProvider(
          undefined,
          {
            provider: "aws",
            servers: [
              testMachine({
                cloudID: "i-orphan",
                labels: {
                  crabbox: "true",
                  lease: "cbx_missing",
                  created_at: oldSeconds,
                  expires_at: oldSeconds,
                },
              }),
              testMachine({
                cloudID: "i-wrong-region",
                labels: {
                  crabbox: "true",
                  lease: "cbx_000000000774",
                  created_at: oldSeconds,
                  expires_at: oldSeconds,
                },
              }),
            ],
          },
          async (id) => {
            deleted.push(id);
          },
        ),
      },
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "secret",
        CRABBOX_AWS_ORPHAN_SWEEP_DELETE: "1",
        CRABBOX_AWS_ORPHAN_SWEEP_GRACE_SECONDS: "1",
      },
    );

    await fleet.alarm();

    const sweep = storage.value<{
      mode: string;
      terminated: number;
      candidates: Array<Record<string, unknown>>;
    }>("aws-orphan-sweep:last");
    expect(deleted).toEqual(["i-orphan"]);
    expect(sweep).toMatchObject({ mode: "delete", terminated: 1 });
    expect(sweep?.candidates[0]).toMatchObject({
      cloudID: "i-orphan",
      ownership: "coordinator-lease",
      ownershipLeaseID: "cbx_000000000776",
      action: "terminated",
    });
    expect(sweep?.candidates[1]).toMatchObject({
      cloudID: "i-wrong-region",
      ownership: "provider-tags-only",
      action: "reported",
    });
  });

  it("deletes only coordinator-owned Azure orphan sweep candidates", async () => {
    const storage = new MemoryStorage();
    const deleted: string[] = [];
    const oldSeconds = String(Math.trunc((Date.now() - 60 * 60 * 1000) / 1000));
    storage.seed(
      "lease:cbx_000000000776",
      testLease({
        id: "cbx_000000000776",
        provider: "azure",
        cloudID: "vm-orphan",
        region: "westus2",
        state: "expired",
        keep: false,
      }),
    );
    storage.seed(
      "lease:cbx_000000000777",
      testLease({
        id: "cbx_000000000777",
        provider: "azure",
        cloudID: "",
        region: "eastus",
        state: "provisioning",
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );
    storage.seed(
      "lease:cbx_000000000778",
      testLease({
        id: "cbx_000000000778",
        provider: "azure",
        cloudID: "vm-cleaning",
        region: "eastus",
        state: "released",
        cleanupStartedAt: new Date().toISOString(),
        cleanupClaimExpiresAt: new Date(Date.now() + 30 * 60 * 1000).toISOString(),
      }),
    );
    storage.seed(
      "lease:cbx_000000000779",
      testLease({
        id: "cbx_000000000779",
        provider: "azure",
        cloudID: "",
        region: "eastus",
        state: "released",
        keep: true,
        releaseDeletesServer: false,
      }),
    );
    storage.seed(
      "lease:cbx_000000000780",
      testLease({
        id: "cbx_000000000780",
        provider: "azure",
        cloudID: "vm-retained",
        region: "eastus",
        state: "released",
        keep: true,
        releaseDeletesServer: false,
      }),
    );
    const fleet = testFleet(
      storage,
      {
        azure: fakeProvider(
          undefined,
          {
            provider: "azure",
            servers: [
              testMachine({
                provider: "azure",
                cloudID: "vm-orphan",
                region: "westus2",
                name: "vm-orphan",
                labels: {
                  crabbox: "true",
                  lease: "cbx_missing",
                  created_at: oldSeconds,
                  expires_at: oldSeconds,
                },
              }),
              testMachine({
                provider: "azure",
                cloudID: "vm-kept",
                name: "vm-kept",
                labels: {
                  crabbox: "true",
                  keep: "true",
                  lease: "cbx_missing",
                  created_at: oldSeconds,
                  expires_at: oldSeconds,
                },
              }),
              testMachine({
                provider: "azure",
                cloudID: "vm-tag-only",
                region: "westus2",
                name: "vm-tag-only",
                labels: {
                  crabbox: "true",
                  lease: "cbx_missing",
                  created_at: oldSeconds,
                  expires_at: oldSeconds,
                },
              }),
              testMachine({
                provider: "azure",
                cloudID: "vm-provisioning",
                name: "vm-provisioning",
                labels: {
                  crabbox: "true",
                  lease: "cbx_000000000777",
                  created_at: oldSeconds,
                  expires_at: oldSeconds,
                },
              }),
              testMachine({
                provider: "azure",
                cloudID: "vm-cleaning",
                name: "vm-cleaning",
                labels: {
                  crabbox: "true",
                  lease: "cbx_000000000778",
                  created_at: oldSeconds,
                  expires_at: oldSeconds,
                },
              }),
              testMachine({
                provider: "azure",
                cloudID: "vm-retaining",
                name: "vm-retaining",
                labels: {
                  crabbox: "true",
                  lease: "cbx_000000000779",
                  created_at: oldSeconds,
                  expires_at: oldSeconds,
                },
              }),
              testMachine({
                provider: "azure",
                cloudID: "vm-retained",
                name: "vm-retained",
                labels: {
                  crabbox: "true",
                  lease: "cbx_000000000780",
                  created_at: oldSeconds,
                  expires_at: oldSeconds,
                },
              }),
            ],
          },
          async (id) => {
            deleted.push(id);
          },
        ),
      },
      {
        AZURE_TENANT_ID: "tenant",
        AZURE_CLIENT_ID: "client",
        AZURE_CLIENT_SECRET: "secret",
        AZURE_SUBSCRIPTION_ID: "subscription",
        CRABBOX_AZURE_LOCATION: "eastus",
        CRABBOX_AZURE_ORPHAN_SWEEP_DELETE: "1",
        CRABBOX_AZURE_ORPHAN_SWEEP_GRACE_SECONDS: "1",
      },
    );

    await fleet.alarm();

    const sweep = storage.value<{
      mode: string;
      terminated: number;
      candidates: Array<Record<string, unknown>>;
    }>("azure-orphan-sweep:last");
    expect(deleted).toEqual(["vm-orphan"]);
    expect(sweep).toMatchObject({ mode: "delete", terminated: 1 });
    expect(sweep?.candidates).toEqual([
      expect.objectContaining({
        cloudID: "vm-orphan",
        region: "westus2",
        leaseID: "cbx_missing",
        reason: "expired-provider-tag",
        ownership: "coordinator-lease",
        ownershipLeaseID: "cbx_000000000776",
        action: "terminated",
      }),
      expect.objectContaining({
        cloudID: "vm-tag-only",
        region: "westus2",
        ownership: "provider-tags-only",
        action: "reported",
      }),
    ]);
  });

  it("releases stale pending EC2 Mac hosts during the AWS orphan sweep", async () => {
    const storage = new MemoryStorage();
    const actions: string[] = [];
    let releaseHostID = "";
    storage.seed(
      "lease:cbx_000000000775",
      testLease({
        id: "cbx_000000000775",
        provider: "aws",
        cloudID: "i-terminated",
        region: "eu-west-1",
        hostID: "h-stale",
        state: "expired",
        keep: false,
      }),
    );
    const fetchMock = vi.fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>(
      async (input, init) => {
        const params = new URLSearchParams(await requestBodyForTest(input, init));
        const action = params.get("Action") ?? "";
        actions.push(action);
        if (action === "DescribeHosts") {
          return ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
          <DescribeHostsResponse>
            <hostSet>
              <item>
                <hostId>h-stale</hostId>
                <hostState>pending</hostState>
                <availabilityZone>eu-west-1a</availabilityZone>
                <autoPlacement>off</autoPlacement>
                <allocationTime>2026-05-01T00:00:00Z</allocationTime>
                <hostProperties><instanceType>mac2.metal</instanceType></hostProperties>
                <tagSet><item><key>crabbox</key><value>true</value></item></tagSet>
              </item>
            </hostSet>
          </DescribeHostsResponse>`);
        }
        if (action === "ReleaseHosts") {
          releaseHostID = params.get("HostId.1") ?? "";
          return ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
          <ReleaseHostsResponse>
            <successful><item>h-stale</item></successful>
          </ReleaseHostsResponse>`);
        }
        return ec2XMLResponse("<ErrorResponse />", 500);
      },
    );
    vi.stubGlobal("fetch", fetchMock);
    const fleet = testFleet(
      storage,
      { aws: fakeProvider(undefined, { provider: "aws", servers: [] }) },
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "secret",
        CRABBOX_AWS_ORPHAN_SWEEP_DELETE: "1",
        CRABBOX_AWS_MAC_HOST_SWEEP_RELEASE: "1",
        CRABBOX_AWS_REGION: "eu-west-1",
      },
    );

    await fleet.alarm();

    const sweep = storage.value<{
      macHostsScanned: number;
      macHostsReleased: number;
      macHostCandidates: Array<Record<string, unknown>>;
    }>("aws-orphan-sweep:last");
    expect(actions).toEqual(["DescribeHosts", "ReleaseHosts"]);
    expect(releaseHostID).toBe("h-stale");
    expect(sweep).toMatchObject({ macHostsScanned: 1, macHostsReleased: 1 });
    expect(sweep?.macHostCandidates[0]).toMatchObject({
      hostID: "h-stale",
      reason: "stale-pending-mac-host",
      ownership: "coordinator-lease",
      ownershipLeaseID: "cbx_000000000775",
      action: "released",
    });
  });

  it("keeps tag-only EC2 Mac hosts report-only in release mode", async () => {
    const storage = new MemoryStorage();
    const actions: string[] = [];
    const fetchMock = vi.fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>(
      async (input, init) => {
        const params = new URLSearchParams(await requestBodyForTest(input, init));
        const action = params.get("Action") ?? "";
        actions.push(action);
        if (action === "DescribeHosts") {
          return ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
          <DescribeHostsResponse>
            <hostSet>
              <item>
                <hostId>h-tag-only</hostId>
                <hostState>pending</hostState>
                <availabilityZone>eu-west-1a</availabilityZone>
                <autoPlacement>off</autoPlacement>
                <allocationTime>2026-05-01T00:00:00Z</allocationTime>
                <hostProperties><instanceType>mac2.metal</instanceType></hostProperties>
                <tagSet><item><key>crabbox</key><value>true</value></item></tagSet>
              </item>
            </hostSet>
          </DescribeHostsResponse>`);
        }
        return ec2XMLResponse("<ErrorResponse />", 500);
      },
    );
    vi.stubGlobal("fetch", fetchMock);
    const fleet = testFleet(
      storage,
      { aws: fakeProvider(undefined, { provider: "aws", servers: [] }) },
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "secret",
        CRABBOX_AWS_ORPHAN_SWEEP_DELETE: "1",
        CRABBOX_AWS_MAC_HOST_SWEEP_RELEASE: "1",
        CRABBOX_AWS_REGION: "eu-west-1",
      },
    );

    await fleet.alarm();

    const sweep = storage.value<{
      macHostsReleased: number;
      macHostCandidates: Array<Record<string, unknown>>;
    }>("aws-orphan-sweep:last");
    expect(actions).toEqual(["DescribeHosts"]);
    expect(sweep?.macHostsReleased).toBe(0);
    expect(sweep?.macHostCandidates[0]).toMatchObject({
      hostID: "h-tag-only",
      ownership: "provider-tags-only",
      action: "reported",
    });
  });

  it("does not terminate an active AWS lease just because launch tags are stale", async () => {
    const storage = new MemoryStorage();
    const deleted: string[] = [];
    const oldSeconds = String(Math.trunc((Date.now() - 60 * 60 * 1000) / 1000));
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        provider: "aws",
        cloudID: "i-active",
        region: "eu-west-1",
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );
    const fleet = testFleet(
      storage,
      {
        aws: fakeProvider(
          undefined,
          {
            provider: "aws",
            servers: [
              testMachine({
                cloudID: "i-active",
                labels: {
                  crabbox: "true",
                  lease: "cbx_000000000001",
                  created_at: oldSeconds,
                  expires_at: oldSeconds,
                },
              }),
            ],
          },
          async (id) => {
            deleted.push(id);
          },
        ),
      },
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "secret",
        CRABBOX_AWS_ORPHAN_SWEEP_DELETE: "1",
        CRABBOX_AWS_ORPHAN_SWEEP_GRACE_SECONDS: "1",
      },
    );

    await fleet.alarm();

    const sweep = storage.value<{ candidates: unknown[] }>("aws-orphan-sweep:last");
    expect(deleted).toEqual([]);
    expect(sweep?.candidates).toEqual([]);
  });

  it("does not terminate an active AWS lease when the provider lease tag is stale", async () => {
    const storage = new MemoryStorage();
    const deleted: string[] = [];
    const oldSeconds = String(Math.trunc((Date.now() - 60 * 60 * 1000) / 1000));
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        provider: "aws",
        cloudID: "i-active",
        region: "eu-west-1",
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );
    const fleet = testFleet(
      storage,
      {
        aws: fakeProvider(
          undefined,
          {
            provider: "aws",
            servers: [
              testMachine({
                cloudID: "i-active",
                labels: {
                  crabbox: "true",
                  lease: "cbx_stale",
                  created_at: oldSeconds,
                  expires_at: oldSeconds,
                },
              }),
            ],
          },
          async (id) => {
            deleted.push(id);
          },
        ),
      },
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "secret",
        CRABBOX_AWS_ORPHAN_SWEEP_DELETE: "1",
        CRABBOX_AWS_ORPHAN_SWEEP_GRACE_SECONDS: "1",
      },
    );

    await fleet.alarm();

    const sweep = storage.value<{ candidates: unknown[] }>("aws-orphan-sweep:last");
    expect(deleted).toEqual([]);
    expect(sweep?.candidates).toEqual([]);
  });

  it("creates leases through the public route with slug and idle metadata", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage, {
      hetzner: fakeProvider(),
    });
    const create = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          slug: "Blue Lobster",
          provider: "hetzner",
          class: "standard",
          serverType: "cpx62",
          pond: "Alpha Pond",
          exposedPorts: ["8080", "9090"],
          ttlSeconds: 1200,
          idleTimeoutSeconds: 360,
          keep: true,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );
    expect(create.status).toBe(201);
    const { lease } = (await create.json()) as { lease: LeaseRecord };
    expect(lease.id).toBe("cbx_abcdef123456");
    expect(lease.slug).toBe("blue-lobster");
    expect(lease.idleTimeoutSeconds).toBe(360);
    expect(lease.ttlSeconds).toBe(1200);
    expect(lease.pond).toBe("alpha-pond");
    expect(lease.exposedPorts).toEqual(["8080", "9090"]);
    expect(lease.lastTouchedAt).toBeTruthy();
    expect(Date.parse(lease.expiresAt)).toBeGreaterThan(Date.parse(lease.createdAt));

    const bySlug = await fleet.fetch(
      request("GET", "/v1/leases/blue-lobster", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
      }),
    );
    expect(bySlug.status).toBe(200);
    const found = (await bySlug.json()) as { lease: LeaseRecord };
    expect(found.lease.id).toBe("cbx_abcdef123456");
    expect(found.lease.slug).toBe("blue-lobster");
  });

  it("registers, borrows, and returns ready-pool leases", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const headers = {
      "x-crabbox-owner": "peter@example.com",
      "x-crabbox-org": "openclaw",
    };
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        provider: "azure",
        host: "192.0.2.22",
        sshPort: "2222",
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );
    storage.seed(
      "lease:cbx_000000000002",
      testLease({
        id: "cbx_000000000002",
        provider: "azure",
        host: "192.0.2.23",
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );

    const register = await fleet.fetch(
      request("POST", "/v1/ready-pools/example/register", {
        headers,
        body: {
          leaseID: "cbx_000000000001",
          repo: "example/app",
          ref: "main",
          commit: "abc123",
          sshUser: "ubuntu",
          sshPort: "22",
          workRoot: "/workspace/app",
        },
      }),
    );
    expect(register.status).toBe(200);
    const registerBody = (await register.json()) as {
      entry: { sshUser: string; workRoot: string };
    };
    expect(registerBody.entry.sshUser).toBe("ubuntu");
    expect(registerBody.entry.workRoot).toBe("/workspace/app");

    const slashRegister = await fleet.fetch(
      request("POST", "/v1/ready-pools/example%2Fapp%2Fmain/register", {
        headers,
        body: { leaseID: "cbx_000000000002" },
      }),
    );
    expect(slashRegister.status).toBe(200);
    const slashBody = (await slashRegister.json()) as { entry: { key: string } };
    expect(slashBody.entry.key).toBe("example/app/main");
    storage.seed(
      "lease:cbx_000000000002",
      testLease({
        id: "cbx_000000000002",
        provider: "azure",
        host: "192.0.2.23",
        expiresAt: "2026-05-01T00:00:00.000Z",
      }),
    );
    const allPools = await fleet.fetch(request("GET", "/v1/ready-pools", { headers }));
    expect(allPools.status).toBe(200);
    const allPoolsBody = (await allPools.json()) as {
      pools: Array<{ key: string; state: string }>;
    };
    expect(allPoolsBody.pools.find((entry) => entry.key === "example/app/main")?.state).toBe(
      "stale",
    );

    const borrow = await fleet.fetch(
      request("POST", "/v1/ready-pools/example/borrow", {
        headers,
        body: { repo: "example/app", ref: "main" },
      }),
    );
    expect(borrow.status).toBe(200);
    const borrowed = (await borrow.json()) as {
      entry: { state: string; sshPort: string; borrowToken: string };
    };
    expect(borrowed.entry.state).toBe("busy");
    expect(borrowed.entry.sshPort).toBe("22");
    expect(borrowed.entry.borrowToken).toBeTruthy();

    const busyStatus = await fleet.fetch(request("GET", "/v1/ready-pools/example", { headers }));
    expect(busyStatus.status).toBe(200);
    const busyStatusBody = (await busyStatus.json()) as {
      pool: Array<{ state: string; borrowToken?: string }>;
    };
    expect(busyStatusBody.pool.find((entry) => entry.state === "busy")?.borrowToken).toBe(
      undefined,
    );

    const empty = await fleet.fetch(
      request("POST", "/v1/ready-pools/example/borrow", {
        headers,
        body: { repo: "example/app", ref: "main" },
      }),
    );
    expect(empty.status).toBe(409);

    const returned = await fleet.fetch(
      request("POST", "/v1/ready-pools/example/return", {
        headers,
        body: {
          leaseID: "cbx_000000000001",
          result: "ready",
          borrowToken: borrowed.entry.borrowToken,
        },
      }),
    );
    expect(returned.status).toBe(200);
    const returnedBody = (await returned.json()) as {
      entry: { state: string; borrowedBy?: string };
    };
    expect(returnedBody.entry.state).toBe("ready");
    expect(returnedBody.entry.borrowedBy).toBeUndefined();

    const destructiveReadyReturn = await fleet.fetch(
      request("POST", "/v1/ready-pools/example/return", {
        headers: { "x-crabbox-owner": "friend@example.com", "x-crabbox-org": "openclaw" },
        body: { leaseID: "cbx_000000000001", result: "drain" },
      }),
    );
    expect(destructiveReadyReturn.status).toBe(404);

    const sparseRegister = await fleet.fetch(
      request("POST", "/v1/ready-pools/sparse/register", {
        headers,
        body: {
          leaseID: "cbx_000000000001",
          repo: "example/app",
          ref: "main",
          sshUser: "-oProxyCommand=bad",
        },
      }),
    );
    expect(sparseRegister.status).toBe(200);
    const sparseBody = (await sparseRegister.json()) as { entry: { sshUser: string } };
    expect(sparseBody.entry.sshUser).toBe("crabbox");

    const staleCommitBorrow = await fleet.fetch(
      request("POST", "/v1/ready-pools/sparse/borrow", {
        headers,
        body: { repo: "example/app", ref: "main", commit: "abc123" },
      }),
    );
    expect(staleCommitBorrow.status).toBe(409);

    const movePool = await fleet.fetch(
      request("POST", "/v1/ready-pools/other/register", {
        headers,
        body: {
          leaseID: "cbx_000000000001",
          repo: "example/app",
          ref: "main",
          commit: "abc123",
        },
      }),
    );
    expect(movePool.status).toBe(200);

    const oldPoolBorrow = await fleet.fetch(
      request("POST", "/v1/ready-pools/example/borrow", {
        headers,
        body: { repo: "example/app", ref: "main" },
      }),
    );
    expect(oldPoolBorrow.status).toBe(409);

    const newPoolBorrow = await fleet.fetch(
      request("POST", "/v1/ready-pools/other/borrow", {
        headers,
        body: { repo: "example/app", ref: "main" },
      }),
    );
    expect(newPoolBorrow.status).toBe(200);
    const newPoolBorrowBody = (await newPoolBorrow.json()) as {
      entry: { borrowToken: string };
    };

    const busyMove = await fleet.fetch(
      request("POST", "/v1/ready-pools/example/register", {
        headers,
        body: { leaseID: "cbx_000000000001" },
      }),
    );
    expect(busyMove.status).toBe(409);

    const staleReturn = await fleet.fetch(
      request("POST", "/v1/ready-pools/other/return", {
        headers,
        body: { leaseID: "cbx_000000000001", result: "ready", borrowToken: "old-token" },
      }),
    );
    expect(staleReturn.status).toBe(409);

    const cleanReturn = await fleet.fetch(
      request("POST", "/v1/ready-pools/other/return", {
        headers,
        body: {
          leaseID: "cbx_000000000001",
          result: "ready",
          borrowToken: newPoolBorrowBody.entry.borrowToken,
        },
      }),
    );
    expect(cleanReturn.status).toBe(200);
  });

  it("does not let a stale ready-pool status write overwrite a newer registration", async () => {
    const storage = new HookedMemoryStorage();
    const fleet = testFleet(storage);
    const leaseID = "cbx_000000000001";
    const key = "example";
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    storage.seed(
      `lease:${leaseID}`,
      testLease({
        id: leaseID,
        owner: "alice@example.com",
        org: "example-org",
        expiresAt: "2026-05-01T00:00:00.000Z",
      }),
    );
    storage.seed<ReadyPoolEntry>(`ready-pool:${key}:${leaseID}`, {
      key,
      leaseID,
      state: "ready",
      owner: "alice@example.com",
      org: "example-org",
      createdAt: "2026-05-01T00:00:00.000Z",
      updatedAt: "2026-05-01T00:00:00.000Z",
      expiresAt: "2026-05-01T00:00:00.000Z",
    });
    const stalePutStarted = deferred<void>();
    const finishStalePut = deferred<void>();
    let blocked = false;
    storage.beforePut = async (storageKey, value) => {
      if (
        !blocked &&
        storageKey === `ready-pool:${key}:${leaseID}` &&
        (value as ReadyPoolEntry).state === "stale"
      ) {
        blocked = true;
        stalePutStarted.resolve();
        await finishStalePut.promise;
      }
    };

    const status = fleet.fetch(request("GET", `/v1/ready-pools/${key}`, { headers }));
    await stalePutStarted.promise;
    storage.seed(
      `lease:${leaseID}`,
      testLease({
        id: leaseID,
        owner: "alice@example.com",
        org: "example-org",
        expiresAt: new Date(Date.now() + 60_000).toISOString(),
      }),
    );
    const register = fleet.fetch(
      request("POST", `/v1/ready-pools/${key}/register`, {
        headers,
        body: { leaseID },
      }),
    );
    expect(
      await Promise.race([
        register.then(() => true),
        new Promise<false>((resolve) => setTimeout(() => resolve(false), 25)),
      ]),
    ).toBe(false);

    finishStalePut.resolve();
    expect((await status).status).toBe(200);
    expect((await register).status).toBe(200);
    expect(storage.value<ReadyPoolEntry>(`ready-pool:${key}:${leaseID}`)?.state).toBe("ready");
  });

  it("requires manage access to borrow and drain ready-pool leases", async () => {
    const storage = new MemoryStorage();
    let deleted = "";
    const fleet = testFleet(storage, {
      hetzner: fakeProvider(undefined, {}, async (id) => {
        deleted = id;
      }),
    });
    const ownerHeaders = {
      "x-crabbox-owner": "peter@example.com",
      "x-crabbox-org": "openclaw",
    };
    const friendHeaders = {
      "x-crabbox-owner": "friend@example.com",
      "x-crabbox-org": "openclaw",
    };
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        share: { users: { "friend@example.com": "use" } },
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );

    const register = await fleet.fetch(
      request("POST", "/v1/ready-pools/example/register", {
        headers: ownerHeaders,
        body: { leaseID: "cbx_000000000001" },
      }),
    );
    expect(register.status).toBe(200);

    const borrow = await fleet.fetch(
      request("POST", "/v1/ready-pools/example/borrow", {
        headers: friendHeaders,
        body: {},
      }),
    );
    expect(borrow.status).toBe(403);

    const ownerBorrow = await fleet.fetch(
      request("POST", "/v1/ready-pools/example/borrow", {
        headers: ownerHeaders,
        body: {},
      }),
    );
    expect(ownerBorrow.status).toBe(200);
    const ownerBorrowBody = (await ownerBorrow.json()) as { entry: { borrowToken: string } };

    const friendDrain = await fleet.fetch(
      request("POST", "/v1/ready-pools/example/return", {
        headers: friendHeaders,
        body: { leaseID: "cbx_000000000001", result: "drain" },
      }),
    );
    expect(friendDrain.status).toBe(403);

    const drain = await fleet.fetch(
      request("POST", "/v1/ready-pools/example/return", {
        headers: ownerHeaders,
        body: {
          leaseID: "cbx_000000000001",
          result: "drain",
          borrowToken: ownerBorrowBody.entry.borrowToken,
        },
      }),
    );
    expect(drain.status).toBe(200);
    const drained = (await drain.json()) as {
      entry: { state: string };
      lease: { state: string };
    };
    expect(drained.entry.state).toBe("draining");
    expect(drained.lease.state).toBe("released");
    expect(deleted).toBe("123");
  });

  it("shares leases with explicit users or the owning org", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const ownerHeaders = {
      "x-crabbox-owner": "peter@example.com",
      "x-crabbox-org": "openclaw",
    };
    const friendHeaders = {
      "x-crabbox-owner": "friend@example.com",
      "x-crabbox-org": "openclaw",
    };
    const strangerHeaders = {
      "x-crabbox-owner": "stranger@example.com",
      "x-crabbox-org": "elsewhere",
    };
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "peter@example.com",
        org: "openclaw",
        desktop: true,
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );
    storage.seed(
      "lease:cbx_000000000002",
      testLease({
        id: "cbx_000000000002",
        slug: "blue-lobster",
        owner: "other@example.com",
        org: "elsewhere",
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );

    const scopedAdminLease = await fleet.fetch(
      request("GET", "/v1/leases/blue-lobster", {
        headers: { ...ownerHeaders, "x-crabbox-admin": "true" },
      }),
    );
    expect(scopedAdminLease.status).toBe(200);
    await expect(scopedAdminLease.json()).resolves.toMatchObject({
      lease: { id: "cbx_000000000001" },
    });

    const hidden = await fleet.fetch(
      request("GET", "/v1/leases/blue-lobster", { headers: friendHeaders }),
    );
    expect(hidden.status).toBe(404);

    const shared = await fleet.fetch(
      request("PUT", "/v1/leases/blue-lobster/share", {
        headers: ownerHeaders,
        body: { users: { "Friend@Example.com": "use" } },
      }),
    );
    expect(shared.status).toBe(200);
    await expect(shared.json()).resolves.toMatchObject({
      leaseID: "cbx_000000000001",
      share: { users: { "friend@example.com": "use" } },
    });

    const friendLease = await fleet.fetch(
      request("GET", "/v1/leases/blue-lobster", { headers: friendHeaders }),
    );
    expect(friendLease.status).toBe(200);
    const friendLeaseBody = (await friendLease.json()) as {
      lease: { id: string; slug: string; share?: unknown };
    };
    expect(friendLeaseBody.lease).toMatchObject({
      id: "cbx_000000000001",
      slug: "blue-lobster",
    });
    expect(friendLeaseBody.lease.share).toBeUndefined();

    const friendLeases = await fleet.fetch(
      request("GET", "/v1/leases", { headers: friendHeaders }),
    );
    expect(friendLeases.status).toBe(200);
    const friendLeaseList = (await friendLeases.json()) as { leases: Array<{ share?: unknown }> };
    expect(friendLeaseList.leases).toHaveLength(1);
    expect(friendLeaseList.leases[0]?.share).toBeUndefined();

    const friendShare = await fleet.fetch(
      request("GET", "/v1/leases/blue-lobster/share", { headers: friendHeaders }),
    );
    expect(friendShare.status).toBe(403);

    const friendTicket = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/webvnc/ticket", {
        headers: friendHeaders,
        body: {},
      }),
    );
    expect(friendTicket.status).toBe(403);

    const ownerTicket = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/webvnc/ticket", {
        headers: ownerHeaders,
        body: {},
      }),
    );
    expect(ownerTicket.status).toBe(200);

    const friendCodeTicket = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/code/ticket", {
        headers: friendHeaders,
        body: {},
      }),
    );
    expect(friendCodeTicket.status).toBe(403);

    const friendEgressTicket = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/egress/ticket", {
        headers: friendHeaders,
        body: { role: "host" },
      }),
    );
    expect(friendEgressTicket.status).toBe(403);

    const friendRelease = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/release", {
        headers: friendHeaders,
        body: {},
      }),
    );
    expect(friendRelease.status).toBe(403);

    const friendUsePortal = await fleet.fetch(
      request("GET", "/portal", { headers: friendHeaders }),
    );
    expect(friendUsePortal.status).toBe(200);
    expect(await friendUsePortal.text()).not.toContain(
      'action="/portal/leases/cbx_000000000001/release?return=%2Fportal"',
    );

    const orgShared = await fleet.fetch(
      request("PUT", "/v1/leases/blue-lobster/share", {
        headers: ownerHeaders,
        body: { users: { "friend@example.com": "use" }, org: "manage" },
      }),
    );
    expect(orgShared.status).toBe(200);
    await expect(orgShared.json()).resolves.toMatchObject({
      share: { users: { "friend@example.com": "use" }, org: "manage" },
    });

    const friendManageShare = await fleet.fetch(
      request("GET", "/v1/leases/blue-lobster/share", { headers: friendHeaders }),
    );
    expect(friendManageShare.status).toBe(200);
    await expect(friendManageShare.json()).resolves.toMatchObject({
      share: { users: { "friend@example.com": "use" }, org: "manage" },
    });

    const friendManagePortal = await fleet.fetch(
      request("GET", "/portal", { headers: friendHeaders }),
    );
    expect(friendManagePortal.status).toBe(200);
    expect(await friendManagePortal.text()).toContain(
      'action="/portal/leases/cbx_000000000001/release?return=%2Fportal"',
    );

    const friendSharePage = await fleet.fetch(
      request("GET", "/portal/leases/blue-lobster/share", { headers: friendHeaders }),
    );
    expect(friendSharePage.status).toBe(200);
    const friendShareBody = await friendSharePage.text();
    expect(friendShareBody).toContain("share blue-lobster");
    expect(friendShareBody).toContain("share-shell");
    expect(friendShareBody).toContain("back to lease");
    expect(friendShareBody).toContain('class="button action" type="submit">save</button>');
    expect(friendShareBody).toContain('class="button action" type="submit">add</button>');

    const embeddedSharePage = await fleet.fetch(
      request("GET", "/portal/leases/blue-lobster/share?embed=1", { headers: friendHeaders }),
    );
    expect(embeddedSharePage.status).toBe(200);
    expect(embeddedSharePage.headers.get("content-security-policy")).toContain(
      "frame-ancestors 'self'",
    );
    const embeddedShareBody = await embeddedSharePage.text();
    expect(embeddedShareBody).toContain("share-shell-embedded");
    expect(embeddedShareBody).toContain("/portal/leases/cbx_000000000001/share?embed=1");
    expect(embeddedShareBody).not.toContain("back to lease");

    const shareJSON = await fleet.fetch(
      request("GET", "/portal/leases/blue-lobster/share?format=json", {
        headers: friendHeaders,
      }),
    );
    expect(shareJSON.status).toBe(200);
    await expect(shareJSON.json()).resolves.toMatchObject({
      leaseID: "cbx_000000000001",
      slug: "blue-lobster",
      owner: "peter@example.com",
      org: "openclaw",
      share: { users: { "friend@example.com": "use" }, org: "manage" },
    });

    const shareJSONUpdate = await fleet.fetch(
      request("POST", "/portal/leases/blue-lobster/share?format=json", {
        headers: friendHeaders,
        body: { users: { "Teammate@Example.com": "manage" }, org: "use" },
      }),
    );
    expect(shareJSONUpdate.status).toBe(200);
    await expect(shareJSONUpdate.json()).resolves.toMatchObject({
      share: { users: { "teammate@example.com": "manage" }, org: "use" },
    });

    const stranger = await fleet.fetch(
      request("GET", "/v1/leases/blue-lobster", { headers: strangerHeaders }),
    );
    expect(stranger.status).toBe(404);
  });

  it("revokes only unauthorized live viewers after an API share removal", async () => {
    const storage = new MemoryStorage();
    const adminToken = "current-admin-token";
    const fleet = testFleet(storage, {}, { CRABBOX_ADMIN_TOKEN: adminToken });
    const leaseID = "cbx_000000000001";
    const ownerHeaders = {
      "x-crabbox-owner": "owner@example.com",
      "x-crabbox-org": "example-org",
    };
    storage.seed(
      `lease:${leaseID}`,
      testLease({
        id: leaseID,
        slug: "shared-live-viewers",
        owner: "owner@example.com",
        org: "example-org",
        desktop: true,
        code: true,
        share: {
          users: {
            "revoked@example.com": "use",
            "retained@example.com": "use",
          },
        },
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );

    const revokedWebAgent = new FakeWebSocket();
    const retainedWebAgent = new FakeWebSocket();
    const ownerWebAgent = new FakeWebSocket();
    const revokedWebViewer = new FakeWebSocket({
      kind: "webvnc-viewer",
      leaseID,
      id: "viewer_revoked",
      agentID: "agent_revoked",
      owner: "revoked@example.com",
      org: "other-org",
      admin: false,
      label: "revoked@example.com",
    });
    const retainedWebViewer = new FakeWebSocket({
      kind: "webvnc-viewer",
      leaseID,
      id: "viewer_retained",
      agentID: "agent_retained",
      owner: "retained@example.com",
      org: "other-org",
      admin: false,
      label: "retained@example.com",
    });
    const ownerWebViewer = new FakeWebSocket({
      kind: "webvnc-viewer",
      leaseID,
      id: "viewer_owner",
      agentID: "agent_owner",
      owner: "owner@example.com",
      org: "example-org",
      admin: false,
      label: "owner@example.com",
    });
    const revokedCodeViewer = new FakeWebSocket({
      kind: "code-viewer",
      leaseID,
      id: "code-revoked",
      auth: "bearer",
      owner: "revoked@example.com",
      org: "other-org",
      admin: false,
    });
    const retainedCodeViewer = new FakeWebSocket({
      kind: "code-viewer",
      leaseID,
      id: "code-retained",
      auth: "bearer",
      owner: "retained@example.com",
      org: "other-org",
      admin: false,
    });
    const ownerCodeViewer = new FakeWebSocket({
      kind: "code-viewer",
      leaseID,
      id: "code-owner",
      auth: "bearer",
      owner: "owner@example.com",
      org: "example-org",
      admin: false,
    });
    const adminCodeViewer = new FakeWebSocket({
      kind: "code-viewer",
      leaseID,
      id: "code-admin",
      auth: "bearer",
      owner: "admin@example.com",
      org: "other-org",
      admin: true,
      adminTokenHash: await sha256Hex(adminToken),
    });
    const legacyCodeViewer = new FakeWebSocket({
      kind: "code-viewer",
      leaseID,
      id: "code-legacy",
      auth: "github",
      portalSessionHash: "a".repeat(64),
    });
    const codeAgent = new FakeWebSocket({ kind: "code-agent", leaseID });
    const relay = fleet as unknown as {
      webVNCAgents: Map<string, Map<string, WebSocket>>;
      webVNCViewers: Map<
        string,
        Map<
          string,
          {
            id: string;
            agentID: string;
            socket: WebSocket;
            owner: string;
            org?: string;
            admin?: boolean;
            label: string;
            connectedAt: string;
          }
        >
      >;
      codeAgents: Map<string, WebSocket>;
      codeViewers: Map<string, WebSocket>;
    };
    relay.webVNCAgents.set(
      leaseID,
      new Map([
        ["agent_revoked", revokedWebAgent as unknown as WebSocket],
        ["agent_retained", retainedWebAgent as unknown as WebSocket],
        ["agent_owner", ownerWebAgent as unknown as WebSocket],
      ]),
    );
    relay.webVNCViewers.set(
      leaseID,
      new Map([
        [
          "viewer_revoked",
          {
            id: "viewer_revoked",
            agentID: "agent_revoked",
            socket: revokedWebViewer as unknown as WebSocket,
            owner: "revoked@example.com",
            org: "other-org",
            admin: false,
            label: "revoked@example.com",
            connectedAt: new Date().toISOString(),
          },
        ],
        [
          "viewer_retained",
          {
            id: "viewer_retained",
            agentID: "agent_retained",
            socket: retainedWebViewer as unknown as WebSocket,
            owner: "retained@example.com",
            org: "other-org",
            admin: false,
            label: "retained@example.com",
            connectedAt: new Date().toISOString(),
          },
        ],
        [
          "viewer_owner",
          {
            id: "viewer_owner",
            agentID: "agent_owner",
            socket: ownerWebViewer as unknown as WebSocket,
            owner: "owner@example.com",
            org: "example-org",
            admin: false,
            label: "owner@example.com",
            connectedAt: new Date().toISOString(),
          },
        ],
      ]),
    );
    relay.codeAgents.set(leaseID, codeAgent as unknown as WebSocket);
    relay.codeViewers.set("code-revoked", revokedCodeViewer as unknown as WebSocket);
    relay.codeViewers.set("code-retained", retainedCodeViewer as unknown as WebSocket);
    relay.codeViewers.set("code-owner", ownerCodeViewer as unknown as WebSocket);
    relay.codeViewers.set("code-admin", adminCodeViewer as unknown as WebSocket);
    relay.codeViewers.set("code-legacy", legacyCodeViewer as unknown as WebSocket);

    const removed = await fleet.fetch(
      request("DELETE", "/v1/leases/shared-live-viewers/share", {
        headers: ownerHeaders,
        body: { user: "revoked@example.com" },
      }),
    );
    expect(removed.status).toBe(200);
    expect(revokedWebViewer.closeCode).toBe(1008);
    expect(revokedWebViewer.closeReason).toBe("lease access revoked");
    expect(revokedWebAgent.closeCode).toBe(1011);
    expect(retainedWebViewer.closeCode).toBeUndefined();
    expect(ownerWebViewer.closeCode).toBeUndefined();
    expect(relay.webVNCViewers.get(leaseID)?.has("viewer_revoked")).toBe(false);
    expect(relay.webVNCViewers.get(leaseID)?.has("viewer_retained")).toBe(true);
    expect(relay.webVNCViewers.get(leaseID)?.has("viewer_owner")).toBe(true);

    expect(revokedCodeViewer.closeCode).toBe(1008);
    expect(revokedCodeViewer.closeReason).toBe("lease access revoked");
    expect(retainedCodeViewer.closeCode).toBeUndefined();
    expect(ownerCodeViewer.closeCode).toBeUndefined();
    expect(adminCodeViewer.closeCode).toBeUndefined();
    expect(legacyCodeViewer.closeCode).toBeUndefined();
    expect(relay.codeViewers.has("code-revoked")).toBe(false);
    expect(relay.codeViewers.has("code-retained")).toBe(true);
    expect(relay.codeViewers.has("code-owner")).toBe(true);
    expect(relay.codeViewers.has("code-admin")).toBe(true);
    expect(relay.codeViewers.has("code-legacy")).toBe(true);
    expect(codeAgent.sentJSON()).toEqual([
      {
        type: "ws_close",
        id: "code-revoked",
        code: 1008,
        reason: "lease access revoked",
      },
    ]);

    await fleet.webSocketMessage(
      retainedCodeViewer as unknown as WebSocket,
      JSON.stringify({ retained: true }),
    );
    expect(codeAgent.sentJSON()).toHaveLength(2);
    expect(codeAgent.sentJSON()[1]).toMatchObject({ type: "ws_data", id: "code-retained" });
    await fleet.webSocketMessage(
      retainedWebViewer as unknown as WebSocket,
      JSON.stringify({ retained: true }),
    );
    expect(retainedWebAgent.sentJSON()).toEqual([{ retained: true }]);
  });

  it("revokes a live egress session when manage access is downgraded", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const leaseID = "cbx_000000000001";
    const ownerHeaders = {
      "x-crabbox-owner": "owner@example.com",
      "x-crabbox-org": "example-org",
    };
    storage.seed(
      `lease:${leaseID}`,
      testLease({
        id: leaseID,
        slug: "shared-live-egress",
        owner: "owner@example.com",
        org: "example-org",
        share: {
          users: {
            "manager@example.com": "manage",
            "other-manager@example.com": "manage",
          },
        },
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );
    const host = new FakeWebSocket({
      kind: "egress-host",
      leaseID,
      sessionID: "egress_shared",
      owner: "manager@example.com",
      org: "example-org",
      admin: false,
    });
    const client = new FakeWebSocket({
      kind: "egress-client",
      leaseID,
      sessionID: "egress_shared",
      owner: "manager@example.com",
      org: "example-org",
      admin: false,
    });
    const relay = fleet as unknown as {
      egressHosts: Map<string, WebSocket>;
      egressClients: Map<string, WebSocket>;
      egressSessions: Map<
        string,
        { sessionID: string; allow: string[]; createdAt: string; updatedAt: string }
      >;
    };
    const key = `${leaseID}\0egress_shared`;
    relay.egressHosts.set(key, host as unknown as WebSocket);
    relay.egressClients.set(key, client as unknown as WebSocket);
    relay.egressSessions.set(leaseID, {
      sessionID: "egress_shared",
      allow: [],
      createdAt: new Date().toISOString(),
      updatedAt: new Date().toISOString(),
    });

    const unrelatedRemoval = await fleet.fetch(
      request("DELETE", "/v1/leases/shared-live-egress/share", {
        headers: ownerHeaders,
        body: { user: "other-manager@example.com" },
      }),
    );
    expect(unrelatedRemoval.status).toBe(200);
    expect(host.closeCode).toBeUndefined();
    expect(client.closeCode).toBeUndefined();

    const downgraded = await fleet.fetch(
      request("PUT", "/v1/leases/shared-live-egress/share", {
        headers: ownerHeaders,
        body: { users: { "manager@example.com": "use" } },
      }),
    );
    expect(downgraded.status).toBe(200);
    expect(host.closeCode).toBe(1008);
    expect(host.closeReason).toBe("lease access revoked");
    expect(client.closeCode).toBe(1008);
    expect(client.closeReason).toBe("lease access revoked");
    expect(relay.egressHosts.has(key)).toBe(false);
    expect(relay.egressClients.has(key)).toBe(false);
    expect(relay.egressSessions.has(leaseID)).toBe(false);
  });

  it("revokes org-only live viewers after a portal share update", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const leaseID = "cbx_000000000001";
    storage.seed(
      `lease:${leaseID}`,
      testLease({
        id: leaseID,
        slug: "portal-share-viewers",
        owner: "owner@example.com",
        org: "example-org",
        code: true,
        share: {
          users: { "explicit@example.com": "use" },
          org: "use",
        },
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );
    const orgViewer = new FakeWebSocket({
      kind: "code-viewer",
      leaseID,
      id: "code-org",
      auth: "bearer",
      owner: "org-viewer@example.com",
      org: "example-org",
      admin: false,
    });
    const explicitViewer = new FakeWebSocket({
      kind: "code-viewer",
      leaseID,
      id: "code-explicit",
      auth: "bearer",
      owner: "explicit@example.com",
      org: "other-org",
      admin: false,
    });
    const codeAgent = new FakeWebSocket({ kind: "code-agent", leaseID });
    const relay = fleet as unknown as {
      codeAgents: Map<string, WebSocket>;
      codeViewers: Map<string, WebSocket>;
    };
    relay.codeAgents.set(leaseID, codeAgent as unknown as WebSocket);
    relay.codeViewers.set("code-org", orgViewer as unknown as WebSocket);
    relay.codeViewers.set("code-explicit", explicitViewer as unknown as WebSocket);

    const updated = await fleet.fetch(
      request("POST", "/portal/leases/portal-share-viewers/share?format=json", {
        headers: {
          "x-crabbox-owner": "owner@example.com",
          "x-crabbox-org": "example-org",
        },
        body: { users: { "explicit@example.com": "use" } },
      }),
    );
    expect(updated.status).toBe(200);
    expect(orgViewer.closeCode).toBe(1008);
    expect(orgViewer.closeReason).toBe("lease access revoked");
    expect(explicitViewer.closeCode).toBeUndefined();
    expect(relay.codeViewers.has("code-org")).toBe(false);
    expect(relay.codeViewers.has("code-explicit")).toBe(true);

    const revokedPage = await fleet.fetch(
      request("GET", "/portal/leases/portal-share-viewers/code/", {
        headers: {
          "x-crabbox-owner": "org-viewer@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    expect(revokedPage.status).toBe(404);
  });

  it("requires manage access for shared lease heartbeat and Tailscale metadata updates", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const useHeaders = {
      "x-crabbox-owner": "viewer@example.com",
      "x-crabbox-org": "openclaw",
    };
    const manageHeaders = {
      "x-crabbox-owner": "manager@example.com",
      "x-crabbox-org": "openclaw",
    };
    const now = new Date();
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "peter@example.com",
        org: "openclaw",
        tailscale: { enabled: true, hostname: "blue-lobster", tags: ["tag:ci"] },
        share: {
          users: {
            "viewer@example.com": "use",
            "manager@example.com": "manage",
          },
        },
        createdAt: now.toISOString(),
        updatedAt: now.toISOString(),
        lastTouchedAt: now.toISOString(),
        expiresAt: new Date(now.getTime() + 60 * 60 * 1000).toISOString(),
      }),
    );

    const visible = await fleet.fetch(
      request("GET", "/v1/leases/blue-lobster", { headers: useHeaders }),
    );
    expect(visible.status).toBe(200);

    const useHeartbeat = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/heartbeat", {
        headers: useHeaders,
        body: { idleTimeoutSeconds: 2400 },
      }),
    );
    expect(useHeartbeat.status).toBe(403);

    const useTailscale = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/tailscale", {
        headers: useHeaders,
        body: { ipv4: "100.64.0.10", state: "ready" },
      }),
    );
    expect(useTailscale.status).toBe(403);

    const manageHeartbeat = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/heartbeat", {
        headers: manageHeaders,
        body: { idleTimeoutSeconds: 2400 },
      }),
    );
    expect(manageHeartbeat.status).toBe(200);
    const heartbeatBody = (await manageHeartbeat.json()) as { lease: LeaseRecord };
    expect(heartbeatBody.lease.idleTimeoutSeconds).toBe(2400);

    const manageTailscale = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/tailscale", {
        headers: manageHeaders,
        body: { ipv4: "100.64.0.10", state: "ready" },
      }),
    );
    expect(manageTailscale.status).toBe(200);
    const tailscaleBody = (await manageTailscale.json()) as { lease: LeaseRecord };
    expect(tailscaleBody.lease.tailscale?.ipv4).toBe("100.64.0.10");
    expect(tailscaleBody.lease.tailscale?.state).toBe("ready");
  });

  it("requires manage access for lease metadata writes", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const viewerHeaders = {
      "x-crabbox-owner": "viewer@example.com",
      "x-crabbox-org": "example-org",
    };
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "shared-run",
        owner: "alice@example.com",
        org: "example-org",
        share: { users: { "viewer@example.com": "use" } },
        tailscale: { enabled: true, state: "requested" },
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );

    const heartbeat = await fleet.fetch(
      request("POST", "/v1/leases/shared-run/heartbeat", {
        headers: viewerHeaders,
        body: { idleTimeoutSeconds: 3600 },
      }),
    );
    expect(heartbeat.status).toBe(403);

    const tailscale = await fleet.fetch(
      request("POST", "/v1/leases/shared-run/tailscale", {
        headers: viewerHeaders,
        body: { enabled: true, ipv4: "100.64.0.99", state: "ready" },
      }),
    );
    expect(tailscale.status).toBe(403);

    const stored = storage.value<LeaseRecord>("lease:cbx_000000000001");
    expect(stored?.idleTimeoutSeconds).not.toBe(3600);
    expect(stored?.tailscale?.ipv4).toBeUndefined();
    expect(stored?.tailscale?.state).toBe("requested");
  });

  it("mints brokered Tailscale keys, records non-secret metadata, and accepts readiness updates", async () => {
    const storage = new MemoryStorage();
    let providerConfig:
      | {
          tailscale?: boolean;
          tailscaleAuthKey?: string;
          tailscaleHostname?: string;
          tailscaleTags?: string[];
          tailscaleExitNode?: string;
          tailscaleExitNodeAllowLanAccess?: boolean;
        }
      | undefined;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        if (url === "https://api.tailscale.com/api/v2/oauth/token") {
          return jsonResponse({ access_token: "oauth-token" });
        }
        if (url === "https://api.tailscale.com/api/v2/tailnet/-/keys") {
          return jsonResponse({ key: "tskey-oneoff" });
        }
        return jsonResponse({ message: `unexpected ${url}` }, 500);
      }),
    );
    const fleet = testFleet(
      storage,
      {
        hetzner: fakeProvider((config) => {
          providerConfig = config;
        }),
      },
      {
        CRABBOX_TAILSCALE_CLIENT_ID: "client-id",
        CRABBOX_TAILSCALE_CLIENT_SECRET: "client-secret",
        CRABBOX_TAILSCALE_TAGS: "tag:crabbox,tag:ci",
      },
    );
    const create = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          slug: "Blue Lobster",
          provider: "hetzner",
          tailscale: true,
          tailscaleTags: ["tag:ci"],
          tailscaleHostname: "crabbox-{slug}",
          tailscaleExitNode: "mac-studio.tailnet.ts.net",
          tailscaleExitNodeAllowLanAccess: true,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );
    expect(create.status).toBe(201);
    const { lease } = (await create.json()) as { lease: LeaseRecord };
    expect(lease.tailscale).toEqual({
      enabled: true,
      hostname: "crabbox-blue-lobster",
      tags: ["tag:ci"],
      state: "requested",
      exitNode: "mac-studio.tailnet.ts.net",
      exitNodeAllowLanAccess: true,
    });
    expect(JSON.stringify(lease)).not.toContain("tskey-oneoff");
    expect(providerConfig).toMatchObject({
      tailscale: true,
      tailscaleAuthKey: "tskey-oneoff",
      tailscaleHostname: "crabbox-blue-lobster",
      tailscaleTags: ["tag:ci"],
      tailscaleExitNode: "mac-studio.tailnet.ts.net",
      tailscaleExitNodeAllowLanAccess: true,
    });

    const update = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/tailscale", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: {
          enabled: true,
          hostname: "crabbox-blue-lobster",
          fqdn: "crabbox-blue-lobster.example.ts.net",
          ipv4: "100.64.0.10",
          exitNode: "mac-studio.tailnet.ts.net",
          exitNodeAllowLanAccess: true,
          state: "ready",
        },
      }),
    );
    expect(update.status).toBe(200);
    const updated = (await update.json()) as { lease: LeaseRecord };
    expect(updated.lease.tailscale?.ipv4).toBe("100.64.0.10");
    expect(updated.lease.tailscale?.exitNode).toBe("mac-studio.tailnet.ts.net");
    expect(updated.lease.tailscale?.state).toBe("ready");
  });

  it("exposes admin Tailscale preflight without leaking minted keys", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        if (url === "https://api.tailscale.com/api/v2/oauth/token") {
          return jsonResponse({ access_token: "oauth-token" });
        }
        if (url === "https://api.tailscale.com/api/v2/tailnet/-/keys") {
          return jsonResponse({ key: "tskey-preflight-secret" });
        }
        return jsonResponse({ message: `unexpected ${url}` }, 500);
      }),
    );
    const fleet = testFleet(
      new MemoryStorage(),
      {},
      {
        CRABBOX_TAILSCALE_CLIENT_ID: "client-id",
        CRABBOX_TAILSCALE_CLIENT_SECRET: "client-secret",
        CRABBOX_TAILSCALE_TAGS: "tag:ci",
      },
    );

    const response = await fleet.fetch(
      request("POST", "/v1/admin/tailscale-preflight", {
        headers: { "x-crabbox-admin": "true" },
      }),
    );

    expect(response.status).toBe(200);
    const text = await response.text();
    expect(text).toContain('"status":"ok"');
    expect(text).not.toContain("tskey-preflight-secret");
  });

  it("does not trust client-posted Tailscale device IDs for privileged cleanup", async () => {
    const storage = new MemoryStorage();
    const cleanupOrder: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        throw new Error(`unexpected Tailscale API request: ${String(input)}`);
      }),
    );
    storage.seed(
      "lease:cbx_tailscale_cleanup",
      testLease({
        id: "cbx_tailscale_cleanup",
        slug: "tailscale-cleanup",
        owner: "alice@example.com",
        org: "example-org",
        provider: "hetzner",
        serverID: 4242,
        state: "active",
        keep: false,
        tailscale: {
          enabled: true,
          state: "ready",
          ipv4: "100.64.0.12",
          deviceID: "node-123",
        },
      }),
    );
    const fleet = testFleet(
      storage,
      {
        hetzner: fakeProvider(undefined, {}, async (id) => {
          cleanupOrder.push(`provider:${id}`);
        }),
      },
      {},
    );

    const response = await fleet.fetch(
      request("POST", "/v1/leases/cbx_tailscale_cleanup/release", {
        headers: { "x-crabbox-owner": "alice@example.com", "x-crabbox-org": "example-org" },
      }),
    );

    expect(response.status).toBe(200);
    expect(cleanupOrder).toEqual(["provider:4242"]);
    const stored = storage.value<LeaseRecord>("lease:cbx_tailscale_cleanup");
    expect(stored?.tailscale?.deviceID).toBe("node-123");
    expect(fetch).not.toHaveBeenCalled();
  });

  it("does not hold the state transition lock while minting a Tailscale key", async () => {
    const storage = new MemoryStorage();
    const accessSnapshots = new Map<string, LeaseRecord[]>();
    let oauthStarted!: () => void;
    let finishOAuth!: () => void;
    let providerCreateStarted!: () => void;
    let finishProviderCreate!: () => void;
    const oauthStartedPromise = new Promise<void>((resolve) => {
      oauthStarted = resolve;
    });
    const finishOAuthPromise = new Promise<void>((resolve) => {
      finishOAuth = resolve;
    });
    const providerCreateStartedPromise = new Promise<void>((resolve) => {
      providerCreateStarted = resolve;
    });
    const finishProviderCreatePromise = new Promise<void>((resolve) => {
      finishProviderCreate = resolve;
    });
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        if (url === "https://api.tailscale.com/api/v2/oauth/token") {
          oauthStarted();
          await finishOAuthPromise;
          return jsonResponse({ access_token: "oauth-token" });
        }
        if (url === "https://api.tailscale.com/api/v2/tailnet/-/keys") {
          return jsonResponse({ key: "tskey-oneoff" });
        }
        return jsonResponse({ message: `unexpected ${url}` }, 500);
      }),
    );
    const fleet = testFleet(
      storage,
      {
        aws: fakeProvider(
          async () => {
            providerCreateStarted();
            await finishProviderCreatePromise;
          },
          {
            provider: "aws",
            onPrepareLeaseCreate(config, lease, context) {
              accessSnapshots.set(lease.id, structuredClone(context.activeLeases));
              return {
                config,
                lease: {
                  ...lease,
                  network: { sshSourceCIDRs: context.requestSourceCIDRs },
                },
                provisioning: {
                  sshIngressReconcile: "authoritative",
                  publishAccessBeforeProvisioning: true,
                },
              };
            },
          },
        ),
      },
      {
        CRABBOX_TAILSCALE_CLIENT_ID: "client-id",
        CRABBOX_TAILSCALE_CLIENT_SECRET: "client-secret",
        CRABBOX_TAILSCALE_TAGS: "tag:ci",
      },
    );
    const firstCreate = fleet.fetch(
      request("POST", "/v1/leases", {
        headers: { "cf-connecting-ip": "198.51.100.10" },
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "aws",
          tailscale: true,
          tailscaleTags: ["tag:ci"],
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );

    await oauthStartedPromise;
    const secondCreate = fleet.fetch(
      request("POST", "/v1/leases", {
        headers: { "cf-connecting-ip": "198.51.100.11" },
        body: {
          leaseID: "cbx_abcdef123457",
          provider: "aws",
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );
    const secondReachedProvider = await Promise.race([
      providerCreateStartedPromise.then(() => true),
      new Promise<false>((resolve) => setTimeout(() => resolve(false), 250)),
    ]);
    expect(secondReachedProvider).toBe(true);
    expect(
      (
        await fleet.fetch(
          request("POST", "/v1/leases/cbx_abcdef123457/release", {
            body: { delete: false },
          }),
        )
      ).status,
    ).toBe(200);

    finishOAuth();
    await vi.waitFor(() => expect(accessSnapshots.has("cbx_abcdef123456")).toBe(true));
    expect(
      accessSnapshots.get("cbx_abcdef123456")?.find((lease) => lease.id === "cbx_abcdef123457"),
    ).toMatchObject({
      state: "provisioning",
      network: { sshSourceCIDRs: ["198.51.100.11/32"] },
    });
    finishProviderCreate();
    expect((await firstCreate).status).toBe(201);
    expect((await secondCreate).status).toBe(409);
  });

  it("removes a retained release canceled before provider preparation", async () => {
    const storage = new MemoryStorage();
    let oauthStarted!: () => void;
    let finishOAuth!: () => void;
    let providerCalled = false;
    const oauthStartedPromise = new Promise<void>((resolve) => {
      oauthStarted = resolve;
    });
    const finishOAuthPromise = new Promise<void>((resolve) => {
      finishOAuth = resolve;
    });
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        if (url === "https://api.tailscale.com/api/v2/oauth/token") {
          oauthStarted();
          await finishOAuthPromise;
          return jsonResponse({ access_token: "oauth-token" });
        }
        if (url === "https://api.tailscale.com/api/v2/tailnet/-/keys") {
          return jsonResponse({ key: "tskey-oneoff" });
        }
        return jsonResponse({ message: `unexpected ${url}` }, 500);
      }),
    );
    const fleet = testFleet(
      storage,
      {
        aws: fakeProvider(
          () => {
            providerCalled = true;
          },
          {
            provider: "aws",
            onPrepareLeaseCreate(config, lease) {
              providerCalled = true;
              return { config, lease };
            },
          },
        ),
      },
      {
        CRABBOX_TAILSCALE_CLIENT_ID: "client-id",
        CRABBOX_TAILSCALE_CLIENT_SECRET: "client-secret",
        CRABBOX_TAILSCALE_TAGS: "tag:ci",
      },
    );
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const createPromise = fleet.fetch(
      request("POST", "/v1/leases", {
        headers,
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "aws",
          tailscale: true,
          tailscaleTags: ["tag:ci"],
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );

    await oauthStartedPromise;
    const release = await fleet.fetch(
      request("POST", "/v1/leases/cbx_abcdef123456/release", {
        headers,
        body: { delete: false },
      }),
    );
    expect(release.status).toBe(200);
    expect(storage.value<LeaseRecord>("lease:cbx_abcdef123456")).toMatchObject({
      state: "released",
      releaseDeletesServer: false,
    });

    finishOAuth();
    expect((await createPromise).status).toBe(409);
    expect(providerCalled).toBe(false);
    expect(storage.value("lease:cbx_abcdef123456")).toBeUndefined();
    expect(storage.value("provider-access:cbx_abcdef123456")).toBeUndefined();
  });

  it("removes a release canceled after provider preparation", async () => {
    const storage = new MemoryStorage();
    const prepareStarted = deferred<void>();
    const finishPrepare = deferred<void>();
    let providerCalled = false;
    const fleet = testFleet(storage, {
      aws: fakeProvider(
        () => {
          providerCalled = true;
        },
        {
          provider: "aws",
          async onPrepareLeaseCreate(config, lease) {
            prepareStarted.resolve();
            await finishPrepare.promise;
            return {
              config,
              lease,
              provisioning: {
                sshIngressReconcile: "authoritative",
                publishAccessBeforeProvisioning: true,
              },
            };
          },
        },
      ),
    });
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const createPromise = fleet.fetch(
      request("POST", "/v1/leases", {
        headers,
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "aws",
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );

    await prepareStarted.promise;
    const releasePromise = fleet.fetch(
      request("POST", "/v1/leases/cbx_abcdef123456/release", {
        headers,
        body: { delete: true },
      }),
    );
    finishPrepare.resolve();

    expect((await releasePromise).status).toBe(200);
    expect((await createPromise).status).toBe(409);
    expect(providerCalled).toBe(false);
    expect(storage.value("lease:cbx_abcdef123456")).toBeUndefined();
    expect(storage.value("provider-access:cbx_abcdef123456")).toBeUndefined();
  });

  it("removes a retained release when Tailscale preparation fails", async () => {
    const storage = new MemoryStorage();
    let oauthStarted!: () => void;
    let finishOAuth!: () => void;
    let providerCalled = false;
    const oauthStartedPromise = new Promise<void>((resolve) => {
      oauthStarted = resolve;
    });
    const finishOAuthPromise = new Promise<void>((resolve) => {
      finishOAuth = resolve;
    });
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        if (url === "https://api.tailscale.com/api/v2/oauth/token") {
          oauthStarted();
          await finishOAuthPromise;
          return jsonResponse({ message: "oauth unavailable" }, 503);
        }
        return jsonResponse({ message: `unexpected ${url}` }, 500);
      }),
    );
    const fleet = testFleet(
      storage,
      {
        aws: fakeProvider(() => {
          providerCalled = true;
        }),
      },
      {
        CRABBOX_TAILSCALE_CLIENT_ID: "client-id",
        CRABBOX_TAILSCALE_CLIENT_SECRET: "client-secret",
        CRABBOX_TAILSCALE_TAGS: "tag:ci",
      },
    );
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const createPromise = fleet.fetch(
      request("POST", "/v1/leases", {
        headers,
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "aws",
          tailscale: true,
          tailscaleTags: ["tag:ci"],
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );

    await oauthStartedPromise;
    const release = await fleet.fetch(
      request("POST", "/v1/leases/cbx_abcdef123456/release", {
        headers,
        body: { delete: false },
      }),
    );
    expect(release.status).toBe(200);
    expect(storage.value<LeaseRecord>("lease:cbx_abcdef123456")).toMatchObject({
      state: "released",
      releaseDeletesServer: false,
    });

    finishOAuth();
    expect((await createPromise).status).toBe(502);
    expect(providerCalled).toBe(false);
    expect(storage.value("lease:cbx_abcdef123456")).toBeUndefined();
    expect(storage.value("provider-access:cbx_abcdef123456")).toBeUndefined();
  });

  it("persists brokered leases as provisioning before provider create returns", async () => {
    const storage = new MemoryStorage();
    let storedDuringCreate: LeaseRecord | undefined;
    const fleet = testFleet(storage, {
      azure: fakeProvider(
        () => {
          storedDuringCreate = structuredClone(storage.value("lease:cbx_abcdef123456"));
        },
        { provider: "azure", cloudID: "vm-cbx-abcdef123456", region: "eastus" },
      ),
    });

    const create = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "azure",
          azureLocation: "eastus",
          ttlSeconds: 1200,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );

    expect(create.status).toBe(201);
    expect(storedDuringCreate).toMatchObject({
      id: "cbx_abcdef123456",
      provider: "azure",
      state: "provisioning",
      cloudID: "",
    });
    const { lease } = (await create.json()) as { lease: LeaseRecord };
    expect(lease).toMatchObject({
      id: "cbx_abcdef123456",
      provider: "azure",
      state: "active",
      cloudID: "vm-cbx-abcdef123456",
    });
    expect(storage.value<LeaseRecord>("lease:cbx_abcdef123456")?.state).toBe("active");
  });

  it("rejects a concurrent create with the same lease ID", async () => {
    const storage = new MemoryStorage();
    let createStarted!: () => void;
    let finishCreate!: () => void;
    const createStartedPromise = new Promise<void>((resolve) => {
      createStarted = resolve;
    });
    const finishCreatePromise = new Promise<void>((resolve) => {
      finishCreate = resolve;
    });
    const fleet = testFleet(storage, {
      azure: fakeProvider(
        async () => {
          createStarted();
          await finishCreatePromise;
        },
        { provider: "azure", cloudID: "vm-cbx-abcdef123456", region: "eastus" },
      ),
    });
    const firstCreate = fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "azure",
          azureLocation: "eastus",
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );

    await createStartedPromise;
    const conflictingCreate = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "bob@example.com",
          "x-crabbox-org": "other-org",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "azure",
          azureLocation: "westus3",
          sshPublicKey: "ssh-ed25519 other",
        },
      }),
    );
    expect(conflictingCreate.status).toBe(409);
    await expect(conflictingCreate.json()).resolves.toMatchObject({ error: "lease_id_conflict" });
    expect(storage.value<LeaseRecord>("lease:cbx_abcdef123456")).toMatchObject({
      owner: "alice@example.com",
      org: "example-org",
      state: "provisioning",
    });

    finishCreate();
    const firstResult = await firstCreate;
    expect(firstResult.status).toBe(201);
    expect(storage.value<LeaseRecord>("lease:cbx_abcdef123456")).toMatchObject({
      owner: "alice@example.com",
      org: "example-org",
      state: "active",
    });
  });

  it("marks provisioning leases failed when provider create fails", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage, {
      azure: fakeProvider(
        () => {
          throw new Error("azure create timed out after VM request");
        },
        { provider: "azure", region: "eastus" },
      ),
    });

    const create = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "azure",
          azureLocation: "eastus",
          ttlSeconds: 1200,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );

    expect(create.status).toBe(500);
    const lease = storage.value<LeaseRecord>("lease:cbx_abcdef123456");
    expect(lease).toMatchObject({
      id: "cbx_abcdef123456",
      provider: "azure",
      state: "failed",
      cleanupError: "azure create timed out after VM request",
    });
    expect(lease?.endedAt).toBeTruthy();
  });

  it("can release a provisioning lease before cloud resources are known", async () => {
    const storage = new MemoryStorage();
    const deleted: string[] = [];
    const fleet = testFleet(storage, {
      azure: fakeProvider(undefined, { provider: "azure" }, async (id) => {
        deleted.push(id);
      }),
    });
    storage.seed(
      "lease:cbx_abcdef123456",
      testLease({
        id: "cbx_abcdef123456",
        slug: "slow-azure",
        provider: "azure",
        cloudID: "",
        region: "eastus",
        state: "provisioning",
        owner: "alice@example.com",
        org: "example-org",
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );

    const release = await fleet.fetch(
      request("POST", "/v1/leases/slow-azure/release", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: { delete: true },
      }),
    );

    expect(release.status).toBe(200);
    expect(deleted).toEqual([]);
    const { lease } = (await release.json()) as { lease: LeaseRecord };
    expect(lease.state).toBe("released");
  });

  it("does not reactivate a provisioning lease released while provider create is pending", async () => {
    const storage = new MemoryStorage();
    const deleted: string[] = [];
    let createStarted!: () => void;
    let finishCreate!: () => void;
    const createStartedPromise = new Promise<void>((resolve) => {
      createStarted = resolve;
    });
    const finishCreatePromise = new Promise<void>((resolve) => {
      finishCreate = resolve;
    });
    const fleet = testFleet(storage, {
      azure: fakeProvider(
        async () => {
          createStarted();
          await finishCreatePromise;
        },
        { provider: "azure", cloudID: "vm-cbx-abcdef123456", region: "eastus" },
        async (id) => {
          deleted.push(id);
        },
      ),
    });

    const createPromise = fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "azure",
          azureLocation: "eastus",
          ttlSeconds: 1200,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );

    await createStartedPromise;
    const release = await fleet.fetch(
      request("POST", "/v1/leases/cbx_abcdef123456/release", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: { delete: true },
      }),
    );
    expect(release.status).toBe(200);

    finishCreate();
    const create = await createPromise;
    expect(create.status).toBe(409);
    expect(deleted).toEqual(["vm-cbx-abcdef123456"]);
    const lease = storage.value<LeaseRecord>("lease:cbx_abcdef123456");
    expect(lease).toMatchObject({
      id: "cbx_abcdef123456",
      provider: "azure",
      state: "released",
      cloudID: "",
    });
  });

  it.each([
    { provider: "aws" as const, providerKey: "crabbox-cbx-abcdef123456", cleanupOwned: true },
    {
      provider: "hetzner" as const,
      providerKey: "crabbox-cbx-abcdef123456",
      cleanupOwned: true,
    },
    { provider: "hetzner" as const, providerKey: "shared-existing-key", cleanupOwned: false },
  ])(
    "preserves $provider provider-key ownership during provisioning cancellation cleanup",
    async ({ provider, providerKey, cleanupOwned }) => {
      const storage = new MemoryStorage();
      const released: LeaseRecord[] = [];
      let createStarted!: () => void;
      let finishCreate!: () => void;
      const createStartedPromise = new Promise<void>((resolve) => {
        createStarted = resolve;
      });
      const finishCreatePromise = new Promise<void>((resolve) => {
        finishCreate = resolve;
      });
      const fleet = testFleet(storage, {
        [provider]: fakeProvider(
          async () => {
            createStarted();
            await finishCreatePromise;
          },
          {
            provider,
            providerKey,
            onReleaseLease: (lease) => {
              released.push(structuredClone(lease));
            },
          },
        ),
      });
      const headers = {
        "x-crabbox-owner": "alice@example.com",
        "x-crabbox-org": "example-org",
      };
      const createPromise = fleet.fetch(
        request("POST", "/v1/leases", {
          headers,
          body: {
            leaseID: "cbx_abcdef123456",
            provider,
            ...(provider === "aws" ? { awsUseStockImage: true } : {}),
            ttlSeconds: 1200,
            sshPublicKey: "ssh-ed25519 cancellation-test",
          },
        }),
      );

      await createStartedPromise;
      const release = await fleet.fetch(
        request("POST", "/v1/leases/cbx_abcdef123456/release", {
          headers,
          body: { delete: true },
        }),
      );
      expect(release.status).toBe(200);

      finishCreate();
      expect((await createPromise).status).toBe(409);
      expect(released).toHaveLength(1);
      expect(released[0]).toMatchObject({
        id: "cbx_abcdef123456",
        provider,
        providerKey,
        providerKeyCleanupOwned: cleanupOwned,
      });
    },
  );

  it("rejects a no-delete release after provisioning cleanup has started", async () => {
    const storage = new MemoryStorage();
    let createStarted!: () => void;
    let finishCreate!: () => void;
    let deleteStarted!: () => void;
    let finishDelete!: () => void;
    const createStartedPromise = new Promise<void>((resolve) => {
      createStarted = resolve;
    });
    const finishCreatePromise = new Promise<void>((resolve) => {
      finishCreate = resolve;
    });
    const deleteStartedPromise = new Promise<void>((resolve) => {
      deleteStarted = resolve;
    });
    const finishDeletePromise = new Promise<void>((resolve) => {
      finishDelete = resolve;
    });
    const fleet = testFleet(storage, {
      azure: fakeProvider(
        async () => {
          createStarted();
          await finishCreatePromise;
        },
        { provider: "azure", cloudID: "vm-cbx-abcdef123456", region: "eastus" },
        async () => {
          deleteStarted();
          await finishDeletePromise;
          throw new Error("azure delete throttled");
        },
      ),
    });
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const createPromise = fleet.fetch(
      request("POST", "/v1/leases", {
        headers,
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "azure",
          azureLocation: "eastus",
          ttlSeconds: 1200,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );

    await createStartedPromise;
    expect(
      (
        await fleet.fetch(
          request("POST", "/v1/leases/cbx_abcdef123456/release", {
            headers,
            body: { delete: true },
          }),
        )
      ).status,
    ).toBe(200);
    finishCreate();
    await deleteStartedPromise;

    const noDelete = await fleet.fetch(
      request("POST", "/v1/leases/cbx_abcdef123456/release", {
        headers,
        body: { delete: false },
      }),
    );
    expect(noDelete.status).toBe(409);
    await expect(noDelete.json()).resolves.toMatchObject({ error: "cleanup_in_progress" });
    const claimed = storage.value<LeaseRecord>("lease:cbx_abcdef123456");
    expect(claimed).toMatchObject({
      state: "released",
      cloudID: "vm-cbx-abcdef123456",
      releaseDeletesServer: true,
      cleanupStartedAt: expect.any(String),
      cleanupClaimExpiresAt: expect.any(String),
    });
    expect(
      Date.parse(claimed?.cleanupClaimExpiresAt ?? "") -
        Date.parse(claimed?.cleanupStartedAt ?? ""),
    ).toBe(30 * 60 * 1000);
    expect(storage.alarm()).toBe(Date.parse(claimed?.cleanupClaimExpiresAt ?? ""));
    const heartbeat = await fleet.fetch(
      request("POST", "/v1/leases/cbx_abcdef123456/heartbeat", {
        headers,
        body: { idleTimeoutSeconds: 300 },
      }),
    );
    expect(heartbeat.status).toBe(409);
    expect(storage.value<LeaseRecord>("lease:cbx_abcdef123456")?.cleanupStartedAt).toBe(
      claimed?.cleanupStartedAt,
    );

    finishDelete();
    expect((await createPromise).status).toBe(500);
    const released = storage.value<LeaseRecord>("lease:cbx_abcdef123456");
    expect(released?.state).toBe("released");
    expect(released?.cleanupStartedAt).toBeUndefined();
    expect(released?.cleanupClaimExpiresAt).toBeUndefined();
    expect(released).toMatchObject({
      releaseDeletesServer: true,
      cleanupError: "azure delete throttled",
      cleanupRetryAt: expect.any(String),
    });
  });

  it("does not reactivate a provisioning lease released while create finalization is pending", async () => {
    const storage = new MemoryStorage();
    const deleted: string[] = [];
    let finalizationStarted!: () => void;
    let finishFinalization!: () => void;
    const finalizationStartedPromise = new Promise<void>((resolve) => {
      finalizationStarted = resolve;
    });
    const finishFinalizationPromise = new Promise<void>((resolve) => {
      finishFinalization = resolve;
    });
    const fleet = testFleet(storage, {
      azure: fakeProvider(
        undefined,
        {
          provider: "azure",
          cloudID: "vm-cbx-abcdef123456",
          region: "eastus",
          onFinalizeLeaseCreate: async (config, lease) => {
            finalizationStarted();
            await finishFinalizationPromise;
            return { config, lease };
          },
        },
        async (id) => {
          deleted.push(id);
        },
      ),
    });

    const createPromise = fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "azure",
          azureLocation: "eastus",
          ttlSeconds: 1200,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );

    await finalizationStartedPromise;
    const release = await fleet.fetch(
      request("POST", "/v1/leases/cbx_abcdef123456/release", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: { delete: true },
      }),
    );
    expect(release.status).toBe(200);

    finishFinalization();
    const create = await createPromise;
    expect(create.status).toBe(409);
    expect(deleted).toEqual(["vm-cbx-abcdef123456"]);
    expect(storage.value<LeaseRecord>("lease:cbx_abcdef123456")).toMatchObject({
      id: "cbx_abcdef123456",
      provider: "azure",
      state: "released",
      cloudID: "",
    });
  });

  it("preserves heartbeat updates while create finalization is pending", async () => {
    const storage = new MemoryStorage();
    let finalizationStarted!: () => void;
    let finishFinalization!: () => void;
    const finalizationStartedPromise = new Promise<void>((resolve) => {
      finalizationStarted = resolve;
    });
    const finishFinalizationPromise = new Promise<void>((resolve) => {
      finishFinalization = resolve;
    });
    const fleet = testFleet(storage, {
      azure: fakeProvider(undefined, {
        provider: "azure",
        cloudID: "vm-cbx-abcdef123456",
        region: "eastus",
        onFinalizeLeaseCreate: async (config, lease) => {
          finalizationStarted();
          await finishFinalizationPromise;
          return { config, lease };
        },
      }),
    });

    const createPromise = fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "azure",
          azureLocation: "eastus",
          ttlSeconds: 1200,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );

    await finalizationStartedPromise;
    const heartbeat = await fleet.fetch(
      request("POST", "/v1/leases/cbx_abcdef123456/heartbeat", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: { idleTimeoutSeconds: 300 },
      }),
    );
    expect(heartbeat.status).toBe(200);
    const heartbeatLease = ((await heartbeat.json()) as { lease: LeaseRecord }).lease;

    finishFinalization();
    const create = await createPromise;
    expect(create.status).toBe(201);
    const createdLease = ((await create.json()) as { lease: LeaseRecord }).lease;
    expect(createdLease).toMatchObject({
      id: "cbx_abcdef123456",
      state: "active",
      cloudID: "vm-cbx-abcdef123456",
      idleTimeoutSeconds: 300,
      lastTouchedAt: heartbeatLease.lastTouchedAt,
      expiresAt: heartbeatLease.expiresAt,
    });
    expect(storage.value<LeaseRecord>("lease:cbx_abcdef123456")).toEqual(createdLease);
  });

  it("buffers Cloudflare heartbeat bodies before serializing state transitions", async () => {
    const storage = new MemoryStorage();
    let finalizationStarted!: () => void;
    let finishFinalization!: () => void;
    let bodyReadStarted!: () => void;
    let finishBody!: () => void;
    const finalizationStartedPromise = new Promise<void>((resolve) => {
      finalizationStarted = resolve;
    });
    const finishFinalizationPromise = new Promise<void>((resolve) => {
      finishFinalization = resolve;
    });
    const bodyReadStartedPromise = new Promise<void>((resolve) => {
      bodyReadStarted = resolve;
    });
    const finishBodyPromise = new Promise<void>((resolve) => {
      finishBody = resolve;
    });
    const fleet = testFleet(storage, {
      azure: fakeProvider(undefined, {
        provider: "azure",
        cloudID: "vm-cbx-abcdef123456",
        region: "eastus",
        onFinalizeLeaseCreate: async (config, lease) => {
          finalizationStarted();
          await finishFinalizationPromise;
          return { config, lease };
        },
      }),
    });
    const headers = {
      "content-type": "application/json",
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const createPromise = fleet.fetch(
      request("POST", "/v1/leases", {
        headers,
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "azure",
          azureLocation: "eastus",
          ttlSeconds: 1200,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );

    await finalizationStartedPromise;
    const body = new ReadableStream<Uint8Array>({
      async pull(controller) {
        bodyReadStarted();
        await finishBodyPromise;
        controller.enqueue(new TextEncoder().encode('{"idleTimeoutSeconds":300}'));
        controller.close();
      },
    });
    const heartbeatPromise = fleet.fetch(
      new Request("https://coordinator.test/v1/leases/cbx_abcdef123456/heartbeat", {
        method: "POST",
        headers,
        body,
        duplex: "half",
      } as RequestInit & { duplex: "half" }),
    );
    await bodyReadStartedPromise;
    finishFinalization();
    expect((await createPromise).status).toBe(201);
    expect(storage.value<LeaseRecord>("lease:cbx_abcdef123456")?.state).toBe("active");

    finishBody();
    expect((await heartbeatPromise).status).toBe(200);
    expect(storage.value<LeaseRecord>("lease:cbx_abcdef123456")).toMatchObject({
      state: "active",
      cloudID: "vm-cbx-abcdef123456",
      idleTimeoutSeconds: 300,
    });
  });

  it("preserves no-delete releases while provider create is pending", async () => {
    const storage = new MemoryStorage();
    const deleted: string[] = [];
    let createStarted!: () => void;
    let finishCreate!: () => void;
    const createStartedPromise = new Promise<void>((resolve) => {
      createStarted = resolve;
    });
    const finishCreatePromise = new Promise<void>((resolve) => {
      finishCreate = resolve;
    });
    const fleet = testFleet(storage, {
      azure: fakeProvider(
        async () => {
          createStarted();
          await finishCreatePromise;
        },
        { provider: "azure", cloudID: "vm-cbx-abcdef123456", region: "eastus" },
        async (id) => {
          deleted.push(id);
        },
      ),
    });

    const createPromise = fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "azure",
          azureLocation: "eastus",
          ttlSeconds: 1200,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );

    await createStartedPromise;
    const release = await fleet.fetch(
      request("POST", "/v1/leases/cbx_abcdef123456/release", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: { delete: false },
      }),
    );
    expect(release.status).toBe(200);
    const retryRelease = await fleet.fetch(
      request("POST", "/v1/leases/cbx_abcdef123456/release", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: { delete: false },
      }),
    );
    expect(retryRelease.status).toBe(200);

    finishCreate();
    const create = await createPromise;
    expect(create.status).toBe(409);
    expect(deleted).toEqual([]);
    expect(storage.value<LeaseRecord>("lease:cbx_abcdef123456")).toMatchObject({
      id: "cbx_abcdef123456",
      provider: "azure",
      state: "released",
      cloudID: "vm-cbx-abcdef123456",
      keep: true,
      releaseDeletesServer: false,
    });

    const defaultRelease = await fleet.fetch(
      request("POST", "/v1/leases/cbx_abcdef123456/release", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    expect(defaultRelease.status).toBe(200);
    expect(deleted).toEqual([]);

    const deleteKept = await fleet.fetch(
      request("POST", "/v1/leases/cbx_abcdef123456/release", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: { delete: true },
      }),
    );
    expect(deleteKept.status).toBe(200);
    expect(deleted).toEqual(["vm-cbx-abcdef123456"]);

    const retryDeleteKept = await fleet.fetch(
      request("POST", "/v1/leases/cbx_abcdef123456/release", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: { delete: true },
      }),
    );
    expect(retryDeleteKept.status).toBe(200);
    expect(deleted).toEqual(["vm-cbx-abcdef123456"]);
  });

  it("does not overwrite a released provisioning lease when provider create later fails", async () => {
    const storage = new MemoryStorage();
    let createStarted!: () => void;
    let finishCreate!: () => void;
    const createStartedPromise = new Promise<void>((resolve) => {
      createStarted = resolve;
    });
    const finishCreatePromise = new Promise<void>((resolve) => {
      finishCreate = resolve;
    });
    const fleet = testFleet(storage, {
      azure: fakeProvider(async () => {
        createStarted();
        await finishCreatePromise;
        throw new Error("azure create failed after release");
      }),
    });

    const createPromise = fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "azure",
          azureLocation: "eastus",
          ttlSeconds: 1200,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );

    await createStartedPromise;
    const release = await fleet.fetch(
      request("POST", "/v1/leases/cbx_abcdef123456/release", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: { delete: false },
      }),
    );
    expect(release.status).toBe(200);

    finishCreate();
    const create = await createPromise;
    expect(create.status).toBe(500);
    expect(storage.value<LeaseRecord>("lease:cbx_abcdef123456")).toMatchObject({
      id: "cbx_abcdef123456",
      provider: "azure",
      state: "released",
      cloudID: "",
      releaseDeletesServer: false,
    });
  });

  it("keeps released state when cleanup after a provisioning release fails", async () => {
    const storage = new MemoryStorage();
    const deleted: string[] = [];
    let failDelete = true;
    let createStarted!: () => void;
    let finishCreate!: () => void;
    const createStartedPromise = new Promise<void>((resolve) => {
      createStarted = resolve;
    });
    const finishCreatePromise = new Promise<void>((resolve) => {
      finishCreate = resolve;
    });
    const fleet = testFleet(storage, {
      azure: fakeProvider(
        async () => {
          createStarted();
          await finishCreatePromise;
        },
        { provider: "azure", cloudID: "vm-cbx-abcdef123456", region: "eastus" },
        async (id) => {
          deleted.push(id);
          if (failDelete) {
            throw new Error("azure delete throttled");
          }
        },
      ),
    });

    const createPromise = fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "azure",
          azureLocation: "eastus",
          ttlSeconds: 1200,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );

    await createStartedPromise;
    const release = await fleet.fetch(
      request("POST", "/v1/leases/cbx_abcdef123456/release", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: { delete: true },
      }),
    );
    expect(release.status).toBe(200);

    finishCreate();
    const create = await createPromise;
    expect(create.status).toBe(500);
    expect(deleted).toEqual(["vm-cbx-abcdef123456"]);
    const failedCleanup = storage.value<LeaseRecord>("lease:cbx_abcdef123456");
    expect(failedCleanup).toMatchObject({
      id: "cbx_abcdef123456",
      provider: "azure",
      state: "released",
      cloudID: "vm-cbx-abcdef123456",
      cleanupError: "azure delete throttled",
      releaseDeletesServer: true,
    });

    failDelete = false;
    const retryDelete = await fleet.fetch(
      request("POST", "/v1/leases/cbx_abcdef123456/release", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: { delete: true },
      }),
    );
    expect(retryDelete.status).toBe(200);
    expect(deleted).toEqual(["vm-cbx-abcdef123456", "vm-cbx-abcdef123456"]);
    const retried = storage.value<LeaseRecord>("lease:cbx_abcdef123456");
    expect(retried?.state).toBe("released");
    expect(retried?.cleanupError).toBeUndefined();
    expect(retried?.releaseDeletesServer).toBeUndefined();
  });

  it("keeps failed state when cleanup after provisioning expiry fails", async () => {
    const storage = new MemoryStorage();
    const deleted: string[] = [];
    let createStarted!: () => void;
    let finishCreate!: () => void;
    const createStartedPromise = new Promise<void>((resolve) => {
      createStarted = resolve;
    });
    const finishCreatePromise = new Promise<void>((resolve) => {
      finishCreate = resolve;
    });
    const fleet = testFleet(storage, {
      azure: fakeProvider(
        async () => {
          createStarted();
          await finishCreatePromise;
        },
        { provider: "azure", cloudID: "vm-cbx-abcdef123456", region: "eastus" },
        async (id) => {
          deleted.push(id);
          throw new Error("azure delete throttled");
        },
      ),
    });

    const createPromise = fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "azure",
          azureLocation: "eastus",
          ttlSeconds: 1200,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );

    await createStartedPromise;
    const provisioning = storage.value<LeaseRecord>("lease:cbx_abcdef123456")!;
    provisioning.state = "failed";
    provisioning.endedAt = new Date().toISOString();
    storage.seed("lease:cbx_abcdef123456", provisioning);

    finishCreate();
    const create = await createPromise;
    expect(create.status).toBe(500);
    expect(deleted).toEqual(["vm-cbx-abcdef123456"]);
    expect(storage.value<LeaseRecord>("lease:cbx_abcdef123456")).toMatchObject({
      id: "cbx_abcdef123456",
      provider: "azure",
      state: "failed",
      cloudID: "vm-cbx-abcdef123456",
      cleanupError: "azure delete throttled",
    });
  });

  it("rejects brokered Tailscale tags outside the coordinator allowlist", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(
      storage,
      { hetzner: fakeProvider() },
      {
        CRABBOX_TAILSCALE_CLIENT_ID: "client-id",
        CRABBOX_TAILSCALE_CLIENT_SECRET: "client-secret",
        CRABBOX_TAILSCALE_TAGS: "tag:crabbox",
      },
    );
    const create = await fleet.fetch(
      request("POST", "/v1/leases", {
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "hetzner",
          tailscale: true,
          tailscaleTags: ["tag:prod"],
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );
    expect(create.status).toBe(400);
    await expect(create.json()).resolves.toMatchObject({
      error: "invalid_tailscale_tags",
      message: "tailscale tags not allowed: tag:prod",
    });
    expect(storage.value("lease:cbx_abcdef123456")).toBeUndefined();
  });

  it("translates brokered Tailscale tag ownership denials", async () => {
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        .mockResolvedValue(
          jsonResponse({ message: "requested tags [tag:ci] are invalid or not permitted" }, 400),
        ),
    );
    const storage = new MemoryStorage();
    const fleet = testFleet(
      storage,
      { hetzner: fakeProvider() },
      {
        CRABBOX_TAILSCALE_CLIENT_ID: "client-id",
        CRABBOX_TAILSCALE_CLIENT_SECRET: "client-secret",
        CRABBOX_TAILSCALE_TAGS: "tag:crabbox,tag:ci",
      },
    );
    const create = await fleet.fetch(
      request("POST", "/v1/leases", {
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "hetzner",
          tailscale: true,
          tailscaleTags: ["tag:ci"],
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );
    expect(create.status).toBe(400);
    const body = (await create.json()) as { error: string; message: string };
    expect(body).toMatchObject({
      error: "invalid_tailscale_tags",
      message: expect.stringContaining("must exactly match the OAuth client's tags"),
    });
    expect(body.message).toContain("requested tags [tag:ci] are invalid or not permitted");
    expect(storage.value("lease:cbx_abcdef123456")).toBeUndefined();
  });

  it("reports brokered Tailscale disabled when OAuth secrets are absent", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage, { hetzner: fakeProvider() });
    const create = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "hetzner",
          tailscale: true,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );
    expect(create.status).toBe(403);
    await expect(create.json()).resolves.toMatchObject({
      error: "tailscale_disabled",
      message: "Tailscale is disabled for this coordinator",
    });
    expect(storage.value("lease:cbx_abcdef123456")).toBeUndefined();
  });

  it("passes the Cloudflare request source IP as AWS SSH ingress CIDR", async () => {
    let awsCIDRs: string[] = [];
    const fleet = testFleet(new MemoryStorage(), {
      aws: fakeProvider(undefined, {
        provider: "aws",
        onPrepareLeaseCreate(config, lease, context) {
          awsCIDRs = context.requestSourceCIDRs;
          return {
            config: { ...config, awsSSHCIDRs: awsCIDRs },
            lease: { ...lease, network: { sshSourceCIDRs: awsCIDRs } },
          };
        },
      }),
    });
    const create = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "cf-connecting-ip": "203.0.113.7",
          "x-crabbox-org": "example-org",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "aws",
          class: "standard",
          serverType: "c7a.8xlarge",
          ttlSeconds: 1200,
          idleTimeoutSeconds: 360,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );
    expect(create.status).toBe(201);
    expect(awsCIDRs).toEqual(["203.0.113.7/32"]);
  });

  it("uses additive AWS ingress reconciliation while creates can overlap", async () => {
    const provider = new AWSProvider({} as Env, "eu-west-1", new MemoryStorage());
    const config = leaseConfig({
      provider: "aws",
      sshPublicKey: "ssh-ed25519 test",
    });
    const lease = testLease({
      id: "cbx_abcdef123456",
      provider: "aws",
      state: "provisioning",
      network: { sshSourceCIDRs: ["203.0.113.7/32"] },
      expiresAt: new Date(Date.now() + 60_000).toISOString(),
    });
    const managedGroupName = `crabbox-runners-${(
      await sha256Hex(`${lease.org}\0${lease.owner}`)
    ).slice(0, 12)}`;
    const active = testLease({
      id: "cbx_000000000001",
      provider: "aws",
      network: {
        awsSecurityGroupName: managedGroupName,
        sshSourceCIDRs: ["198.51.100.44/32"],
      },
      expiresAt: new Date(Date.now() + 60_000).toISOString(),
    });

    const prepared = await provider.prepareLeaseCreate(config, lease, {
      requestSourceCIDRs: ["203.0.113.7/32"],
      activeLeases: [active],
    });

    expect(prepared.config.awsSSHCIDRs).toEqual(["198.51.100.44/32", "203.0.113.7/32"]);
    expect(prepared.lease.network?.awsSecurityGroupName).toBe(managedGroupName);
    expect(prepared.provisioning).toMatchObject({
      sshIngressReconcile: "additive",
      publishAccessBeforeProvisioning: true,
    });

    const otherPrepared = await provider.prepareLeaseCreate(
      config,
      { ...lease, owner: "other@example.com" },
      { requestSourceCIDRs: ["192.0.2.7/32"], activeLeases: [] },
    );
    expect(otherPrepared.lease.network?.awsSecurityGroupName).not.toBe(managedGroupName);
  });

  it("preserves configured AWS SSH CIDRs when refreshing lease access", async () => {
    const provider = new AWSProvider({} as Env, "eu-west-1", new MemoryStorage());
    vi.spyOn(provider, "reconcileLeaseAccess").mockResolvedValue();
    expect(
      leaseConfig({
        provider: "aws",
        awsSSHCIDRs: ["192.0.2.7/32"],
        sshPublicKey: "ssh-ed25519 test",
      }).awsSSHCIDRsPinned,
    ).toBe(true);
    expect(
      leaseConfig({
        provider: "aws",
        awsSSHCIDRs: ["192.0.2.7/32"],
        awsSSHCIDRsPinned: false,
        sshPublicKey: "ssh-ed25519 test",
      }).awsSSHCIDRsPinned,
    ).toBe(false);
    const lease = testLease({
      provider: "aws",
      network: {
        sshSourceCIDRs: ["0.0.0.0/0"],
        sshPinnedSourceCIDRs: ["0.0.0.0/0"],
        sshSourceCIDRsComplete: true,
      },
    });

    const refreshed = await provider.refreshLeaseAccess(lease, {
      requestSourceCIDRs: ["203.0.113.7/32"],
      activeLeases: [lease],
    });

    expect(refreshed?.network?.sshSourceCIDRs).toEqual(["0.0.0.0/0", "203.0.113.7/32"]);

    const dynamic = testLease({
      provider: "aws",
      network: { sshSourceCIDRs: ["198.51.100.7/32"], sshSourceCIDRsComplete: true },
    });
    const refreshedDynamic = await provider.refreshLeaseAccess(dynamic, {
      requestSourceCIDRs: ["203.0.113.8/32"],
      activeLeases: [dynamic],
    });
    expect(refreshedDynamic?.network?.sshSourceCIDRs).toEqual(["203.0.113.8/32"]);
  });

  it("reports each AWS fallback region before provisioning mutates it", async () => {
    const attempts: string[] = [];
    const targets: string[] = [];
    const machine: ProviderMachine = {
      provider: "aws",
      id: 123,
      cloudID: "i-123",
      name: "crabbox-test",
      status: "running",
      serverType: "c7a.8xlarge",
      host: "192.0.2.10",
      labels: {},
    };
    const create = vi
      .spyOn(EC2SpotClient.prototype, "createServerWithFallback")
      .mockImplementation(async (candidateConfig) => {
        attempts.push(candidateConfig.awsRegion);
        if (candidateConfig.awsRegion === "eu-west-1") {
          throw new Error("capacity unavailable");
        }
        return { server: machine, serverType: machine.serverType };
      });
    const wait = vi.spyOn(EC2SpotClient.prototype, "waitForServerIP").mockResolvedValue(machine);
    try {
      const provider = new AWSProvider(
        { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "secret" } as Env,
        "eu-west-1",
        new MemoryStorage(),
      );
      const config = {
        ...leaseConfig({ provider: "aws", sshPublicKey: "ssh-ed25519 test" }),
        capacityRegions: ["us-east-1"],
      };

      const result = await provider.createServerWithFallback(
        config,
        "cbx_abcdef123456",
        "test",
        "alice@example.com",
        {
          onTargetAttempt: async (target) => {
            targets.push(target.region ?? "");
          },
        },
      );

      expect(attempts).toEqual(["eu-west-1", "us-east-1"]);
      expect(targets).toEqual(attempts);
      expect(result.server.region).toBe("us-east-1");
    } finally {
      create.mockRestore();
      wait.mockRestore();
    }
  });

  it("keeps retained unknown AWS access additive and ignores unrelated regions", async () => {
    const requests: Array<{ action: string; cidr: string; groupID: string; hostname: string }> = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const fetchRequest = input instanceof Request ? input : new Request(input, init);
        const params = new URLSearchParams(await fetchRequest.clone().text());
        const action = params.get("Action") ?? "";
        const groupID = params.get("GroupId") ?? params.get("GroupId.1") ?? "";
        requests.push({
          action,
          cidr:
            params.get("IpPermissions.1.IpRanges.1.CidrIp") ??
            params.get("IpPermissions.1.Ipv6Ranges.1.CidrIpv6") ??
            "",
          groupID,
          hostname: new URL(fetchRequest.url).hostname,
        });
        if (action === "DescribeSecurityGroups") {
          return new Response(`<?xml version="1.0" encoding="UTF-8"?>
<DescribeSecurityGroupsResponse>
  <securityGroupInfo>
    <item>
      <groupId>${groupID}</groupId>
      <ipPermissions>
        <item>
          <ipProtocol>tcp</ipProtocol>
          <fromPort>22</fromPort>
          <toPort>22</toPort>
          <ipRanges>
            <item>
              <cidrIp>203.0.113.10/32</cidrIp>
              <description>Crabbox SSH</description>
            </item>
          </ipRanges>
        </item>
      </ipPermissions>
    </item>
  </securityGroupInfo>
</DescribeSecurityGroupsResponse>`);
        }
        return new Response(`<Response />`);
      }),
    );
    const provider = new AWSProvider(
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "secret",
      } as Env,
      "eu-west-1",
      new MemoryStorage(),
    );
    const anchor = testLease({
      id: "cbx_abcdef123456",
      provider: "aws",
      state: "released",
      region: "eu-west-1",
      network: { awsSecurityGroupID: "sg-west" },
    });
    const retained = testLease({
      id: "cbx_abcdef123457",
      provider: "aws",
      state: "released",
      region: "eu-west-1",
      releaseDeletesServer: false,
      network: { awsSecurityGroupID: "sg-west" },
    });
    const activeEast = testLease({
      id: "cbx_abcdef123460",
      provider: "aws",
      state: "active",
      region: "us-east-1",
      network: {
        awsSecurityGroupID: "sg-east",
        sshSourceCIDRs: ["198.51.100.20/32"],
        sshSourceCIDRsComplete: true,
      },
    });
    const unrelated = testLease({
      id: "cbx_abcdef123458",
      provider: "azure",
      state: "active",
      region: "eastus",
    });
    const historical = testLease({
      id: "cbx_abcdef123459",
      provider: "aws",
      state: "released",
      region: "us-invalid-1",
    });

    await provider.reconcileLeaseAccess(anchor, {
      requestSourceCIDRs: [],
      activeLeases: [retained, activeEast, unrelated, historical],
    });

    expect(new Set(requests.map((entry) => `${entry.hostname}:${entry.groupID}`))).toEqual(
      new Set(["ec2.eu-west-1.amazonaws.com:sg-west", "ec2.us-east-1.amazonaws.com:sg-east"]),
    );
    expect(
      requests.filter(
        (entry) =>
          entry.action === "RevokeSecurityGroupIngress" &&
          entry.groupID === "sg-west" &&
          entry.cidr === "203.0.113.10/32",
      ),
    ).toEqual([]);
    expect(
      requests.some(
        (entry) =>
          entry.action === "AuthorizeSecurityGroupIngress" &&
          entry.groupID === "sg-west" &&
          entry.cidr === "198.51.100.20/32",
      ),
    ).toBe(false);
  });

  it("keeps reconciliation additive when legacy metadata may resolve to an explicit AWS group", async () => {
    const revokedCIDRs: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const fetchRequest = input instanceof Request ? input : new Request(input, init);
        const params = new URLSearchParams(await fetchRequest.clone().text());
        const action = params.get("Action") ?? "";
        if (action === "DescribeVpcs") {
          return new Response(
            "<DescribeVpcsResponse><vpcSet><item><vpcId>vpc-default</vpcId></item></vpcSet></DescribeVpcsResponse>",
          );
        }
        if (action === "DescribeSecurityGroups") {
          return new Response(`<?xml version="1.0" encoding="UTF-8"?>
<DescribeSecurityGroupsResponse><securityGroupInfo><item><groupId>sg-shared</groupId><ipPermissions><item><ipProtocol>tcp</ipProtocol><fromPort>22</fromPort><toPort>22</toPort><ipRanges><item><cidrIp>198.51.100.10/32</cidrIp><description>Crabbox SSH</description></item><item><cidrIp>198.51.100.20/32</cidrIp><description>Crabbox SSH</description></item></ipRanges></item></ipPermissions></item></securityGroupInfo></DescribeSecurityGroupsResponse>`);
        }
        if (action === "RevokeSecurityGroupIngress") {
          revokedCIDRs.push(params.get("IpPermissions.1.IpRanges.1.CidrIp") ?? "");
        }
        return new Response("<Response />");
      }),
    );
    const provider = new AWSProvider(
      { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "secret" } as Env,
      "eu-west-1",
      new MemoryStorage(),
    );
    const anchor = testLease({
      id: "cbx_abcdef123456",
      provider: "aws",
      state: "released",
      region: "eu-west-1",
      sshPort: "22",
      network: { awsSecurityGroupID: "sg-shared" },
    });
    const legacy = testLease({
      id: "cbx_abcdef123457",
      provider: "aws",
      region: "eu-west-1",
      sshPort: "22",
      network: {
        sshSourceCIDRs: ["198.51.100.10/32"],
        sshSourceCIDRsComplete: true,
      },
    });
    const explicit = testLease({
      id: "cbx_abcdef123458",
      provider: "aws",
      region: "eu-west-1",
      sshPort: "22",
      network: {
        awsSecurityGroupID: "sg-shared",
        sshSourceCIDRs: ["198.51.100.20/32"],
        sshSourceCIDRsComplete: true,
      },
    });

    await provider.reconcileLeaseAccess(anchor, {
      requestSourceCIDRs: [],
      activeLeases: [legacy, explicit],
    });

    expect(revokedCIDRs.filter((cidr) => cidr !== "0.0.0.0/0")).toEqual([]);
  });

  it("prunes stale CIDRs from a single auto-managed AWS group", async () => {
    const revokedCIDRs: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const fetchRequest = input instanceof Request ? input : new Request(input, init);
        const params = new URLSearchParams(await fetchRequest.clone().text());
        const action = params.get("Action") ?? "";
        if (action === "DescribeVpcs") {
          return new Response(
            "<DescribeVpcsResponse><vpcSet><item><vpcId>vpc-default</vpcId></item></vpcSet></DescribeVpcsResponse>",
          );
        }
        if (action === "DescribeSecurityGroups") {
          return new Response(`<?xml version="1.0" encoding="UTF-8"?>
<DescribeSecurityGroupsResponse><securityGroupInfo><item><groupId>sg-auto</groupId><groupName>crabbox-runners</groupName><ipPermissions><item><ipProtocol>tcp</ipProtocol><fromPort>22</fromPort><toPort>22</toPort><ipRanges><item><cidrIp>198.51.100.10/32</cidrIp><description>Crabbox SSH</description></item><item><cidrIp>198.51.100.20/32</cidrIp><description>Crabbox SSH</description></item></ipRanges></item></ipPermissions></item></securityGroupInfo></DescribeSecurityGroupsResponse>`);
        }
        if (action === "RevokeSecurityGroupIngress") {
          revokedCIDRs.push(params.get("IpPermissions.1.IpRanges.1.CidrIp") ?? "");
        }
        return new Response("<Response />");
      }),
    );
    const provider = new AWSProvider(
      { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "secret" } as Env,
      "eu-west-1",
      new MemoryStorage(),
    );
    const anchor = testLease({
      id: "cbx_abcdef123456",
      provider: "aws",
      state: "released",
      region: "eu-west-1",
      sshPort: "22",
    });
    const active = testLease({
      id: "cbx_abcdef123457",
      provider: "aws",
      region: "eu-west-1",
      sshPort: "22",
      network: {
        sshSourceCIDRs: ["198.51.100.20/32"],
        sshSourceCIDRsComplete: true,
      },
    });

    await provider.reconcileLeaseAccess(anchor, {
      requestSourceCIDRs: [],
      activeLeases: [active],
    });

    expect(revokedCIDRs).toContain("198.51.100.10/32");
    expect(revokedCIDRs).not.toContain("198.51.100.20/32");
  });

  it("prunes runner ingress while a distinct managed workspace group is active", async () => {
    const revokedRules: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const fetchRequest = input instanceof Request ? input : new Request(input, init);
        const params = new URLSearchParams(await fetchRequest.clone().text());
        const action = params.get("Action") ?? "";
        if (action === "DescribeVpcs") {
          return new Response(
            "<DescribeVpcsResponse><vpcSet><item><vpcId>vpc-default</vpcId></item></vpcSet></DescribeVpcsResponse>",
          );
        }
        if (action === "DescribeSecurityGroups") {
          const groupName = params.get("Filter.1.Value.1") ?? "";
          const groupID = groupName === "crabbox-workspaces" ? "sg-workspaces" : "sg-runners";
          const ingress =
            groupName === "crabbox-runners"
              ? "<ipPermissions><item><ipProtocol>tcp</ipProtocol><fromPort>22</fromPort><toPort>22</toPort><ipRanges><item><cidrIp>198.51.100.10/32</cidrIp><description>Crabbox SSH</description></item><item><cidrIp>198.51.100.20/32</cidrIp><description>Crabbox SSH</description></item></ipRanges></item></ipPermissions>"
              : "<ipPermissions />";
          return new Response(
            `<DescribeSecurityGroupsResponse><securityGroupInfo><item><groupId>${groupID}</groupId><groupName>${groupName}</groupName>${ingress}</item></securityGroupInfo></DescribeSecurityGroupsResponse>`,
          );
        }
        if (action === "RevokeSecurityGroupIngress") {
          revokedRules.push(
            `${params.get("GroupId")}:${params.get("IpPermissions.1.IpRanges.1.CidrIp")}`,
          );
        }
        return new Response("<Response />");
      }),
    );
    const provider = new AWSProvider(
      { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "secret" } as Env,
      "eu-west-1",
      new MemoryStorage(),
    );
    const anchor = testLease({
      id: "cbx_abcdef123456",
      provider: "aws",
      state: "released",
      region: "eu-west-1",
      sshPort: "22",
      network: { awsSecurityGroupName: "crabbox-runners" },
    });
    const runner = testLease({
      id: "cbx_abcdef123457",
      provider: "aws",
      region: "eu-west-1",
      sshPort: "22",
      network: {
        awsSecurityGroupName: "crabbox-runners",
        sshSourceCIDRs: ["198.51.100.20/32"],
        sshSourceCIDRsComplete: true,
      },
    });
    const workspace = testLease({
      id: "cbx_abcdef123458",
      provider: "aws",
      providerKey: "crabbox-workspace-0123456789ab",
      region: "eu-west-1",
      sshPort: "22",
      network: {
        awsSecurityGroupName: "crabbox-workspaces",
        sshSourceCIDRs: ["0.0.0.0/0"],
        sshSourceCIDRsComplete: true,
      },
    });

    await provider.reconcileLeaseAccess(anchor, {
      requestSourceCIDRs: [],
      activeLeases: [runner, workspace],
    });

    expect(revokedRules).toContain("sg-runners:198.51.100.10/32");
    expect(revokedRules).not.toContain("sg-runners:198.51.100.20/32");
  });

  it("reconciles distinct SSH port sets that share an AWS security group", async () => {
    const authorizedRules: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const fetchRequest = input instanceof Request ? input : new Request(input, init);
        const params = new URLSearchParams(await fetchRequest.clone().text());
        const action = params.get("Action") ?? "";
        if (action === "DescribeSecurityGroups") {
          return new Response(`<?xml version="1.0" encoding="UTF-8"?>
<DescribeSecurityGroupsResponse><securityGroupInfo><item><groupId>sg-shared</groupId><ipPermissions /></item></securityGroupInfo></DescribeSecurityGroupsResponse>`);
        }
        if (action === "AuthorizeSecurityGroupIngress") {
          authorizedRules.push(
            `${params.get("IpPermissions.1.FromPort")}:${
              params.get("IpPermissions.1.IpRanges.1.CidrIp") ??
              params.get("IpPermissions.1.Ipv6Ranges.1.CidrIpv6")
            }`,
          );
        }
        return new Response("<Response />");
      }),
    );
    const provider = new AWSProvider(
      { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "secret" } as Env,
      "eu-west-1",
      new MemoryStorage(),
    );
    const anchor = testLease({
      id: "cbx_abcdef123456",
      provider: "aws",
      state: "released",
      region: "eu-west-1",
      sshPort: "22",
      network: { awsSecurityGroupID: "sg-shared" },
    });
    const active22 = testLease({
      id: "cbx_abcdef123457",
      provider: "aws",
      region: "eu-west-1",
      sshPort: "22",
      network: {
        awsSecurityGroupID: "sg-shared",
        sshSourceCIDRs: ["198.51.100.20/32"],
        sshSourceCIDRsComplete: true,
      },
    });
    const active2222 = testLease({
      id: "cbx_abcdef123458",
      provider: "aws",
      region: "eu-west-1",
      sshPort: "2222",
      network: {
        awsSecurityGroupID: "sg-shared",
        sshSourceCIDRs: ["198.51.100.21/32"],
        sshSourceCIDRsComplete: true,
      },
    });

    await provider.reconcileLeaseAccess(anchor, {
      requestSourceCIDRs: [],
      activeLeases: [active22, active2222],
    });

    expect(new Set(authorizedRules)).toEqual(
      new Set(["22:198.51.100.20/32", "22:198.51.100.21/32", "2222:198.51.100.21/32"]),
    );
  });

  it("reconciles AWS ingress after the final overlapping create drains", async () => {
    const storage = new MemoryStorage();
    const started = [deferred<void>(), deferred<void>()];
    const finish = [deferred<void>(), deferred<void>()];
    let createIndex = 0;
    const reconciliations: string[][] = [];
    const fleet = testFleet(storage, {
      aws: fakeProvider(
        async () => {
          const index = createIndex++;
          started[index]?.resolve();
          await finish[index]?.promise;
        },
        {
          provider: "aws",
          region: "eu-west-1",
          onPrepareLeaseCreate(config, lease, context) {
            return {
              config,
              lease: {
                ...lease,
                network: { ...lease.network, sshSourceCIDRs: context.requestSourceCIDRs },
              },
              provisioning: {
                sshIngressReconcile: "additive",
                publishAccessBeforeProvisioning: true,
              },
            };
          },
          onReconcileLeaseAccess(_lease, context) {
            reconciliations.push(
              context.activeLeases
                .filter((active) => active.provider === "aws" && active.state === "active")
                .map((active) => active.id)
                .toSorted(),
            );
          },
        },
      ),
    });
    const create = (leaseID: string, sourceIP: string) =>
      fleet.fetch(
        request("POST", "/v1/leases", {
          headers: { "cf-connecting-ip": sourceIP },
          body: { leaseID, provider: "aws", sshPublicKey: "ssh-ed25519 test" },
        }),
      );

    const first = create("cbx_abcdef123456", "198.51.100.10");
    await started[0]?.promise;
    const second = create("cbx_abcdef123457", "198.51.100.11");
    await started[1]?.promise;
    finish[0]?.resolve();
    expect((await first).status).toBe(201);
    expect(reconciliations).toEqual([]);

    finish[1]?.resolve();
    expect((await second).status).toBe(201);
    expect(reconciliations).toEqual([]);

    await fleet.alarm();
    expect(reconciliations).toEqual([["cbx_abcdef123456", "cbx_abcdef123457"]]);
    expect(storage.value("aws-ingress-reconcile:pending")).toBeUndefined();
  });

  it("recovers additive AWS creates from security-group rule limits", async () => {
    const storage = new MemoryStorage();
    let creates = 0;
    const reconciliations: Array<{ cidrs: string[]; region: string; stateRegions: string[] }> = [];
    const fleet = testFleet(storage, {
      aws: fakeProvider(
        () => {
          creates += 1;
          if (creates === 1) {
            throw new Error("RulesPerSecurityGroupLimitExceeded: security group rule quota");
          }
        },
        {
          provider: "aws",
          region: "us-east-1",
          onPrepareLeaseCreate(config, lease, context) {
            return {
              config,
              lease: {
                ...lease,
                network: {
                  ...lease.network,
                  sshSourceCIDRs: context.requestSourceCIDRs,
                  sshSourceCIDRsComplete: true,
                },
              },
              provisioning: {
                sshIngressReconcile: "additive",
                publishAccessBeforeProvisioning: true,
              },
            };
          },
          async onCreateProvisioning(provisioning) {
            await provisioning?.onTargetAttempt?.({ region: "us-east-1" });
          },
          onReconcileLeaseAccess(lease, context) {
            reconciliations.push({
              cidrs: context.activeLeases.flatMap(
                (candidate) => candidate.network?.sshSourceCIDRs ?? [],
              ),
              region: lease.region ?? "",
              stateRegions: context.activeLeases.map((candidate) => candidate.region ?? ""),
            });
          },
        },
      ),
    });

    const response = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: { "cf-connecting-ip": "198.51.100.10" },
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "aws",
          awsRegion: "eu-west-1",
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );

    expect(response.status).toBe(201);
    expect(creates).toBe(2);
    expect(reconciliations).toEqual([
      {
        cidrs: ["198.51.100.10/32"],
        region: "us-east-1",
        stateRegions: ["us-east-1"],
      },
    ]);
  });

  it("preserves distinct pending AWS ingress targets until each reconciles", async () => {
    const storage = new MemoryStorage();
    const reconciled: string[] = [];
    const fleet = testFleet(storage, {
      aws: fakeProvider(
        (config) => {
          if (config.awsRegion === "eu-west-1") {
            throw new Error("first region failed");
          }
        },
        {
          provider: "aws",
          region: "us-east-1",
          onPrepareLeaseCreate(config, lease, context) {
            return {
              config,
              lease: {
                ...lease,
                network: {
                  ...lease.network,
                  awsSecurityGroupID: config.awsSGID,
                  sshSourceCIDRs: context.requestSourceCIDRs,
                },
              },
              provisioning: {
                sshIngressReconcile: "additive",
                publishAccessBeforeProvisioning: true,
              },
            };
          },
          onReconcileLeaseAccess(lease) {
            reconciled.push(`${lease.region}:${lease.network?.awsSecurityGroupID}`);
          },
        },
      ),
    });
    const create = (leaseID: string, awsRegion: string, awsSGID: string) =>
      fleet.fetch(
        request("POST", "/v1/leases", {
          headers: { "cf-connecting-ip": "198.51.100.10" },
          body: {
            leaseID,
            provider: "aws",
            awsRegion,
            awsSGID,
            sshPublicKey: "ssh-ed25519 test",
          },
        }),
      );

    expect((await create("cbx_abcdef123456", "eu-west-1", "sg-west")).status).toBe(500);
    expect((await create("cbx_abcdef123457", "us-east-1", "sg-east")).status).toBe(201);
    expect(
      storage.value<{ targets: Array<{ anchor: LeaseRecord }> }>("aws-ingress-reconcile:pending")
        ?.targets,
    ).toHaveLength(2);

    await fleet.alarm();

    expect(reconciled.toSorted()).toEqual(["eu-west-1:sg-west", "us-east-1:sg-east"]);
    expect(storage.value("aws-ingress-reconcile:pending")).toBeUndefined();
  });

  it("keeps managed AWS group names distinct in pending reconciliation", async () => {
    const storage = new MemoryStorage();
    const reconciled: string[] = [];
    const fleet = testFleet(storage, {
      aws: fakeProvider(
        (config) => {
          if (config.providerKey === "runner-a") {
            throw new Error("first managed group failed");
          }
        },
        {
          provider: "aws",
          region: "eu-west-1",
          onPrepareLeaseCreate(config, lease, context) {
            return {
              config,
              lease: {
                ...lease,
                network: {
                  ...lease.network,
                  awsSecurityGroupName: `managed-${config.providerKey}`,
                  sshSourceCIDRs: context.requestSourceCIDRs,
                },
              },
              provisioning: {
                sshIngressReconcile: "additive",
                publishAccessBeforeProvisioning: true,
              },
            };
          },
          onReconcileLeaseAccess(lease) {
            reconciled.push(lease.network?.awsSecurityGroupName ?? "");
          },
        },
      ),
    });
    const create = (leaseID: string, providerKey: string) =>
      fleet.fetch(
        request("POST", "/v1/leases", {
          headers: {
            "cf-connecting-ip": "198.51.100.10",
            "x-crabbox-admin": "true",
          },
          body: {
            leaseID,
            provider: "aws",
            awsRegion: "eu-west-1",
            providerKey,
            sshPublicKey: "ssh-ed25519 test",
          },
        }),
      );

    expect((await create("cbx_abcdef123456", "runner-a")).status).toBe(500);
    expect((await create("cbx_abcdef123457", "runner-b")).status).toBe(201);
    expect(
      storage
        .value<{ targets: Array<{ anchor: LeaseRecord }> }>("aws-ingress-reconcile:pending")
        ?.targets.map((target) => ({
          id: target.anchor.id,
          name: target.anchor.network?.awsSecurityGroupName,
          region: target.anchor.region,
        })),
    ).toEqual([
      {
        id: "cbx_abcdef123456",
        name: "managed-runner-a",
        region: "eu-west-1",
      },
      {
        id: "cbx_abcdef123457",
        name: "managed-runner-b",
        region: "eu-west-1",
      },
    ]);

    await fleet.alarm();

    expect(reconciled.toSorted()).toEqual(["managed-runner-a", "managed-runner-b"]);
    expect(storage.value("aws-ingress-reconcile:pending")).toBeUndefined();
  });

  it("retains every attempted AWS region for cleanup when provisioning fails", async () => {
    const storage = new MemoryStorage();
    const reconciled: string[] = [];
    const fleet = testFleet(storage, {
      aws: fakeProvider(undefined, {
        provider: "aws",
        onPrepareLeaseCreate(config, lease, context) {
          return {
            config,
            lease: {
              ...lease,
              network: {
                ...lease.network,
                awsSecurityGroupID: config.awsSGID,
                sshSourceCIDRs: context.requestSourceCIDRs,
                sshSourceCIDRsComplete: true,
              },
            },
            provisioning: {
              sshIngressReconcile: "additive",
              publishAccessBeforeProvisioning: true,
            },
          };
        },
        async onCreateProvisioning(provisioning) {
          await provisioning?.onTargetAttempt?.({ region: "eu-west-1" });
          await provisioning?.onTargetAttempt?.({ region: "us-east-1" });
          throw new Error("capacity unavailable");
        },
        onReconcileLeaseAccess(lease) {
          reconciled.push(lease.region ?? "");
        },
      }),
    });

    const response = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: { "cf-connecting-ip": "198.51.100.10" },
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "aws",
          awsRegion: "eu-west-1",
          awsSGID: "sg-shared",
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );

    expect(response.status).toBe(500);
    expect(
      storage
        .value<{ targets: Array<{ anchor: LeaseRecord }> }>("aws-ingress-reconcile:pending")
        ?.targets.map((target) => target.anchor.region)
        .toSorted(),
    ).toEqual(["eu-west-1", "us-east-1"]);

    await fleet.alarm();
    expect(reconciled.toSorted()).toEqual(["eu-west-1", "us-east-1"]);
    expect(storage.value("aws-ingress-reconcile:pending")).toBeUndefined();
  });

  it("fences a release behind AWS ingress reconciliation and preserves the newer work", async () => {
    const storage = new MemoryStorage();
    const reconcileStarted = deferred<void>();
    const finishReconcile = deferred<void>();
    const fleet = testFleet(storage, {
      aws: fakeProvider(undefined, {
        provider: "aws",
        onPrepareLeaseCreate(config, lease, context) {
          return {
            config,
            lease: {
              ...lease,
              network: { ...lease.network, sshSourceCIDRs: context.requestSourceCIDRs },
            },
            provisioning: {
              sshIngressReconcile: "additive",
              publishAccessBeforeProvisioning: true,
            },
          };
        },
        async onReconcileLeaseAccess() {
          reconcileStarted.resolve();
          await finishReconcile.promise;
        },
      }),
    });
    expect(
      (
        await fleet.fetch(
          request("POST", "/v1/leases", {
            headers: { "cf-connecting-ip": "198.51.100.10" },
            body: {
              leaseID: "cbx_abcdef123456",
              provider: "aws",
              sshPublicKey: "ssh-ed25519 test",
            },
          }),
        )
      ).status,
    ).toBe(201);

    const alarm = fleet.alarm();
    await reconcileStarted.promise;
    const release = fleet.fetch(
      request("POST", "/v1/leases/cbx_abcdef123456/release", {
        body: { delete: false },
      }),
    );
    const releaseCompleted = await Promise.race([
      release.then(() => true),
      new Promise<false>((resolve) => setTimeout(() => resolve(false), 250)),
    ]);
    expect(releaseCompleted).toBe(false);
    finishReconcile.resolve();
    expect((await release).status).toBe(200);
    await alarm;
    expect(storage.value("aws-ingress-reconcile:pending")).toBeDefined();

    await fleet.alarm();
    expect(storage.value("aws-ingress-reconcile:pending")).toBeUndefined();
  });

  it("applies a heartbeat after an older AWS ingress reconciliation snapshot", async () => {
    const storage = new MemoryStorage();
    const reconcileStarted = deferred<void>();
    const finishReconcile = deferred<void>();
    const operations: string[] = [];
    let reconciliationCount = 0;
    const fleet = testFleet(storage, {
      aws: fakeProvider(undefined, {
        provider: "aws",
        onPrepareLeaseCreate(config, lease, context) {
          return {
            config,
            lease: {
              ...lease,
              network: { ...lease.network, sshSourceCIDRs: context.requestSourceCIDRs },
            },
            provisioning: {
              sshIngressReconcile: "additive",
              publishAccessBeforeProvisioning: true,
            },
          };
        },
        onRefreshLeaseAccess(lease, context) {
          operations.push(`refresh:${context.requestSourceCIDRs.join(",")}`);
          return {
            ...lease,
            network: { ...lease.network, sshSourceCIDRs: context.requestSourceCIDRs },
          };
        },
        async onReconcileLeaseAccess(_lease, context) {
          reconciliationCount += 1;
          operations.push(
            `reconcile:${context.activeLeases
              .flatMap((candidate) => candidate.network?.sshSourceCIDRs ?? [])
              .join(",")}`,
          );
          if (reconciliationCount === 1) {
            reconcileStarted.resolve();
            await finishReconcile.promise;
          }
        },
      }),
    });
    expect(
      (
        await fleet.fetch(
          request("POST", "/v1/leases", {
            headers: {
              "cf-connecting-ip": "198.51.100.10",
              "x-crabbox-owner": "alice@example.com",
              "x-crabbox-org": "example-org",
            },
            body: {
              leaseID: "cbx_abcdef123456",
              provider: "aws",
              sshPublicKey: "ssh-ed25519 test",
            },
          }),
        )
      ).status,
    ).toBe(201);

    const alarm = fleet.alarm();
    await reconcileStarted.promise;
    const heartbeat = fleet.fetch(
      request("POST", "/v1/leases/cbx_abcdef123456/heartbeat", {
        headers: {
          "cf-connecting-ip": "198.51.100.20",
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: { idleTimeoutSeconds: 300 },
      }),
    );
    await new Promise((resolve) => setTimeout(resolve, 25));
    expect(operations).toEqual(["reconcile:198.51.100.10/32"]);

    finishReconcile.resolve();
    expect((await heartbeat).status).toBe(200);
    await alarm;
    expect(storage.value("aws-ingress-reconcile:pending")).toBeDefined();

    await fleet.alarm();
    expect(operations).toEqual([
      "reconcile:198.51.100.10/32",
      "refresh:198.51.100.20/32",
      "reconcile:198.51.100.20/32",
    ]);
    expect(storage.value("aws-ingress-reconcile:pending")).toBeUndefined();
  });

  it("preserves active AWS lease SSH ingress CIDRs while creating another lease", async () => {
    const storage = new MemoryStorage();
    let awsCIDRs: string[] = [];
    let reconcile: string | undefined;
    let inFlightCIDRs: string[] | undefined;
    let inFlightLeaseVisible = false;
    const fleet = testFleet(storage, {
      aws: fakeProvider(
        () => {
          inFlightLeaseVisible = storage.value<LeaseRecord>("lease:cbx_abcdef123456") !== undefined;
          inFlightCIDRs = storage.value<LeaseRecord>("provider-access:cbx_abcdef123456")?.network
            ?.sshSourceCIDRs;
        },
        {
          provider: "aws",
          onPrepareLeaseCreate(config, lease, context) {
            const sourceCIDRs = context.requestSourceCIDRs;
            awsCIDRs = [
              ...context.activeLeases.flatMap((active) => active.network?.sshSourceCIDRs ?? []),
              ...sourceCIDRs,
            ];
            return {
              config: { ...config, awsSSHCIDRs: awsCIDRs },
              lease: { ...lease, network: { sshSourceCIDRs: sourceCIDRs } },
              provisioning: {
                sshIngressReconcile: "authoritative",
                publishAccessBeforeProvisioning: true,
              },
            };
          },
          onCreateProvisioning(provisioning) {
            reconcile = provisioning?.sshIngressReconcile;
          },
        },
      ),
    });
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        provider: "aws",
        network: { sshSourceCIDRs: ["198.51.100.44/32"] },
        expiresAt: new Date(Date.now() + 60_000).toISOString(),
      }),
    );

    const create = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "cf-connecting-ip": "203.0.113.7",
          "x-crabbox-org": "example-org",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "aws",
          class: "standard",
          serverType: "c7a.8xlarge",
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );

    expect(create.status).toBe(201);
    expect(awsCIDRs).toEqual(["198.51.100.44/32", "203.0.113.7/32"]);
    expect(reconcile).toBe("authoritative");
    expect(inFlightLeaseVisible).toBe(true);
    expect(inFlightCIDRs).toEqual(["203.0.113.7/32"]);
    expect(storage.value<LeaseRecord>("lease:cbx_abcdef123456")?.network?.sshSourceCIDRs).toEqual([
      "203.0.113.7/32",
    ]);
  });

  it("starts provider access TTL when the snapshot is published", async () => {
    vi.useFakeTimers();
    try {
      const storage = new MemoryStorage();
      const reservedAt = new Date("2026-06-01T12:00:00.000Z");
      const publishedAt = new Date("2026-06-01T12:10:00.000Z");
      let publishedExpiry = "";
      vi.setSystemTime(reservedAt);
      const fleet = testFleet(storage, {
        aws: fakeProvider(
          () => {
            publishedExpiry =
              storage.value<LeaseRecord>("provider-access:cbx_abcdef123456")?.expiresAt ?? "";
          },
          {
            provider: "aws",
            onPrepareLeaseCreate(config, lease) {
              vi.setSystemTime(publishedAt);
              return {
                config,
                lease,
                provisioning: { publishAccessBeforeProvisioning: true },
              };
            },
          },
        ),
      });

      const create = await fleet.fetch(
        request("POST", "/v1/leases", {
          body: {
            leaseID: "cbx_abcdef123456",
            provider: "aws",
            sshPublicKey: "ssh-ed25519 test",
          },
        }),
      );

      expect(create.status).toBe(201);
      expect(Date.parse(publishedExpiry)).toBe(publishedAt.getTime() + 15 * 60 * 1000);
    } finally {
      vi.useRealTimers();
    }
  });

  it("lets providers choose additive access reconciliation for unknown active lease state", async () => {
    const storage = new MemoryStorage();
    let reconcile: string | undefined;
    const fleet = testFleet(storage, {
      aws: fakeProvider(undefined, {
        provider: "aws",
        onPrepareLeaseCreate(config, lease, context) {
          const sourceCIDRs = context.requestSourceCIDRs;
          return {
            config: { ...config, awsSSHCIDRs: sourceCIDRs },
            lease: { ...lease, network: { sshSourceCIDRs: sourceCIDRs } },
            provisioning: {
              sshIngressReconcile: context.activeLeases.some(
                (active) =>
                  active.provider === "aws" &&
                  active.state === "active" &&
                  (active.network?.sshSourceCIDRs?.length ?? 0) === 0,
              )
                ? "additive"
                : "authoritative",
            },
          };
        },
        onCreateProvisioning(provisioning) {
          reconcile = provisioning?.sshIngressReconcile;
        },
      }),
    });
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        provider: "aws",
        expiresAt: new Date(Date.now() + 60_000).toISOString(),
      }),
    );

    const create = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "cf-connecting-ip": "203.0.113.7",
          "x-crabbox-org": "example-org",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "aws",
          class: "standard",
          serverType: "c7a.8xlarge",
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );

    expect(create.status).toBe(201);
    expect(reconcile).toBe("additive");
  });

  it("counts in-flight provider access reservations against active lease limits", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(
      storage,
      {
        aws: fakeProvider(undefined, { provider: "aws" }),
      },
      { CRABBOX_MAX_ACTIVE_LEASES: "1" },
    );
    storage.seed(
      "provider-access:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        provider: "aws",
        expiresAt: new Date(Date.now() + 60_000).toISOString(),
      }),
    );

    const create = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "cf-connecting-ip": "203.0.113.7",
          "x-crabbox-org": "example-org",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "aws",
          class: "standard",
          serverType: "c7a.8xlarge",
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );

    expect(create.status).toBe(429);
    await expect(create.json()).resolves.toMatchObject({
      error: "cost_limit_exceeded",
    });
  });

  it("requires admin auth for new AWS macOS host pins but reactivates an owner's retained Mac", async () => {
    let created = false;
    let preparedProviderKey = "";
    const storage = new MemoryStorage();
    const reconciledLeaseIDs: string[][] = [];
    storage.seed(
      "lease:cbx_000000000098",
      testLease({
        id: "cbx_000000000098",
        provider: "aws",
        target: "macos",
        state: "released",
        owner: "admin@example.com",
        org: "example-org",
        hostId: "h-000000000001",
        serverType: "mac2.metal",
        providerKey: "crabbox-steipete",
        updatedAt: "2026-01-01T00:00:00.000Z",
      }),
    );
    const fleet = testFleet(storage, {
      aws: fakeProvider(
        () => {
          created = true;
        },
        {
          provider: "aws",
          serverType: "mac2.metal",
          hostID: "h-000000000001",
          onPrepareLeaseConfig: (config) => {
            preparedProviderKey = config.providerKey;
            return config;
          },
          onReconcileLeaseAccess: (_lease, context) => {
            reconciledLeaseIDs.push(context.activeLeases.map((candidate) => candidate.id));
          },
        },
      ),
    });
    const body = {
      provider: "aws" as const,
      target: "macos" as const,
      class: "standard",
      serverType: "mac2.metal",
      hostId: "h-000000000001",
      capacity: { market: "on-demand" as const },
      keep: true,
      sshPublicKey: "ssh-ed25519 test",
    };
    const denied = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "cf-connecting-ip": "203.0.113.7",
          "x-crabbox-org": "example-org",
        },
        body: { ...body, leaseID: "cbx_abcdef123456" },
      }),
    );
    expect(denied.status).toBe(403);
    expect(created).toBe(false);
    await expect(denied.json()).resolves.toMatchObject({ error: "admin_required" });

    const create = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-admin": "true",
          "x-crabbox-owner": "admin@example.com",
          "x-crabbox-org": "example-org",
        },
        body: { ...body, leaseID: "cbx_abcdef123457" },
      }),
    );

    expect(create.status).toBe(201);
    expect(created).toBe(true);
    const { lease } = (await create.json()) as { lease: LeaseRecord };
    expect(lease.hostId).toBe("h-000000000001");

    const released = await fleet.fetch(
      request("POST", `/v1/leases/${lease.id}/release`, {
        headers: {
          "x-crabbox-admin": "true",
          "x-crabbox-owner": "admin@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    expect(released.status).toBe(200);

    created = false;
    reconciledLeaseIDs.length = 0;
    storage.seed(
      "provider-access:cbx_000000000099",
      testLease({
        id: "cbx_000000000099",
        provider: "aws",
        state: "provisioning",
        expiresAt: new Date(Date.now() + 60_000).toISOString(),
      }),
    );
    const incompatible = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "admin@example.com",
          "x-crabbox-org": "example-org",
        },
        body: { ...body, desktop: true, leaseID: "cbx_abcdef123458" },
      }),
    );
    expect(incompatible.status).toBe(409);
    await expect(incompatible.json()).resolves.toMatchObject({
      error: "retained_instance_capability_mismatch",
    });
    expect(created).toBe(false);

    const reused = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "admin@example.com",
          "x-crabbox-org": "example-org",
        },
        body: {
          ...body,
          providerKey: "crabbox-new-request",
          leaseID: "cbx_abcdef123458",
        },
      }),
    );
    expect(reused.status).toBe(201);
    expect(created).toBe(false);
    expect(preparedProviderKey).toBe(lease.providerKey);
    const { lease: reactivated } = (await reused.json()) as { lease: LeaseRecord };
    expect(reactivated.id).toBe(lease.id);
    expect(reactivated.state).toBe("active");
    expect(reactivated.createdAt).toBe(reactivated.lastTouchedAt);
    expect(reactivated.cloudID).toBe(lease.cloudID);
    expect(reactivated.hostId).toBe("h-000000000001");
    expect(reactivated.providerKey).toBe(lease.providerKey);
    expect(reconciledLeaseIDs.at(-1)).toContain("cbx_000000000099");

    const deleted = await fleet.fetch(
      request("POST", `/v1/leases/${lease.id}/release`, {
        headers: {
          "x-crabbox-owner": "admin@example.com",
          "x-crabbox-org": "example-org",
        },
        body: { delete: true },
      }),
    );
    expect(deleted.status).toBe(200);

    const deletedHostReuse = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "admin@example.com",
          "x-crabbox-org": "example-org",
        },
        body: { ...body, leaseID: "cbx_abcdef123459" },
      }),
    );
    expect(deletedHostReuse.status).toBe(403);
    await expect(deletedHostReuse.json()).resolves.toMatchObject({ error: "admin_required" });
  });

  it("reactivates a retained pinned Mac host family when the request type was defaulted", async () => {
    const storage = new MemoryStorage();
    storage.seed(
      "lease:cbx_000000000100",
      testLease({
        id: "cbx_000000000100",
        provider: "aws",
        target: "macos",
        state: "released",
        owner: "alice@example.com",
        org: "example-org",
        hostId: "h-m4",
        serverType: "mac-m4.metal",
        providerKey: "crabbox-steipete",
        releaseDeletesServer: false,
      }),
    );
    const fleet = testFleet(storage, {
      aws: fakeProvider(
        () => {
          throw new Error("retained instance must not launch a replacement");
        },
        { provider: "aws" },
      ),
    });

    const response = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: {
          provider: "aws",
          target: "macos",
          hostId: "h-m4",
          capacity: { market: "on-demand" },
          keep: true,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );

    expect(response.status).toBe(201);
    const { lease } = (await response.json()) as { lease: LeaseRecord };
    expect(lease.id).toBe("cbx_000000000100");
    expect(lease.serverType).toBe("mac-m4.metal");
    expect(lease.requestedServerType).toBe("mac-m4.metal");
  });

  it("does not launch a replacement when a retained Mac is claimed concurrently", async () => {
    const storage = new MemoryStorage();
    const retained = testLease({
      id: "cbx_000000000101",
      provider: "aws",
      target: "macos",
      state: "released",
      owner: "alice@example.com",
      org: "example-org",
      hostId: "h-mac2",
      serverType: "mac2.metal",
      providerKey: "crabbox-steipete",
      releaseDeletesServer: false,
    });
    storage.seed(`lease:${retained.id}`, retained);
    let created = false;
    const fleet = testFleet(storage, {
      aws: fakeProvider(
        () => {
          created = true;
        },
        {
          provider: "aws",
          onPrepareLeaseConfig: (config, currentStorage) => {
            currentStorage?.seed(`lease:${retained.id}`, { ...retained, state: "active" });
            return config;
          },
        },
      ),
    });

    const response = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: {
          provider: "aws",
          target: "macos",
          hostId: "h-mac2",
          capacity: { market: "on-demand" },
          keep: true,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );

    expect(response.status).toBe(409);
    await expect(response.json()).resolves.toMatchObject({
      error: "retained_instance_unavailable",
    });
    expect(created).toBe(false);
  });

  it("only applies target-matching promoted AWS images", async () => {
    const storage = new MemoryStorage();
    storage.seed("image:aws:promoted:macos:arm64_mac:mac2.metal:eu-west-1", {
      id: "ami-macos",
      name: "crabbox-macos",
      state: "available",
      region: "eu-west-1",
      target: "macos",
      serverType: "mac2.metal",
      architecture: "arm64_mac",
      promotedAt: "2026-05-01T12:46:00Z",
    });
    const seenAMI: string[] = [];
    const seenPromotedAMI: string[] = [];
    const fleet = testFleet(storage, {
      aws: fakeProvider((config) => {
        seenAMI.push(config.awsAMI);
        seenPromotedAMI.push(
          config.awsPromotedAMIs[awsPromotedAMIConfigKey("eu-west-1", "mac2.metal")] ?? "",
        );
      }),
    });

    const linux = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "cf-connecting-ip": "203.0.113.7",
          "x-crabbox-org": "example-org",
        },
        body: {
          provider: "aws",
          class: "standard",
          serverType: "c7a.8xlarge",
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );
    expect(linux.status).toBe(201);

    const macos = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "cf-connecting-ip": "203.0.113.7",
          "x-crabbox-org": "example-org",
        },
        body: {
          provider: "aws",
          target: "macos",
          capacity: { market: "on-demand" },
          serverType: "mac2.metal",
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );
    expect(macos.status).toBe(201);
    expect(seenAMI).toEqual(["", ""]);
    expect(seenPromotedAMI).toEqual(["", "ami-macos"]);
  });

  it("uses server-type scoped promoted AWS macOS images when creating leases", async () => {
    const storage = new MemoryStorage();
    storage.seed("image:aws:promoted:macos:arm64_mac:mac2.metal:eu-west-1", {
      id: "ami-mac2",
      name: "crabbox-mac2",
      state: "available",
      region: "eu-west-1",
      target: "macos",
      serverType: "mac2.metal",
      architecture: "arm64_mac",
      promotedAt: "2026-05-01T12:46:00Z",
    });
    storage.seed("image:aws:promoted:macos:arm64_mac:mac-m4.metal:eu-west-1", {
      id: "ami-m4",
      name: "crabbox-m4",
      state: "available",
      region: "eu-west-1",
      target: "macos",
      serverType: "mac-m4.metal",
      architecture: "arm64_mac",
      promotedAt: "2026-05-01T12:47:00Z",
    });
    const seenPromotedAMI: string[] = [];
    const fleet = testFleet(storage, {
      aws: fakeProvider((config) => {
        seenPromotedAMI.push(
          config.awsPromotedAMIs[awsPromotedAMIConfigKey(config.awsRegion, config.serverType)] ??
            "",
        );
      }),
    });

    const mac2 = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "cf-connecting-ip": "203.0.113.7",
          "x-crabbox-org": "example-org",
        },
        body: {
          provider: "aws",
          target: "macos",
          capacity: { market: "on-demand" },
          serverType: "mac2.metal",
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );
    const m4 = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "cf-connecting-ip": "203.0.113.7",
          "x-crabbox-org": "example-org",
        },
        body: {
          provider: "aws",
          target: "macos",
          capacity: { market: "on-demand" },
          serverType: "mac-m4.metal",
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );

    expect(mac2.status).toBe(201);
    expect(m4.status).toBe(201);
    expect(seenPromotedAMI).toEqual(["ami-mac2", "ami-m4"]);
  });

  it("passes promoted AWS macOS images for fallback candidates", async () => {
    const storage = new MemoryStorage();
    storage.seed("image:aws:promoted:macos:x86_64_mac:mac1.metal:eu-west-1", {
      id: "ami-mac1",
      name: "crabbox-mac1",
      state: "available",
      region: "eu-west-1",
      target: "macos",
      serverType: "mac1.metal",
      architecture: "x86_64_mac",
      promotedAt: "2026-05-01T12:46:00Z",
    });
    let seenConfig: LeaseConfig | undefined;
    const fleet = testFleet(storage, {
      aws: fakeProvider((config) => {
        seenConfig = config;
      }),
    });

    const response = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "cf-connecting-ip": "203.0.113.7",
          "x-crabbox-org": "example-org",
        },
        body: {
          provider: "aws",
          target: "macos",
          capacity: { market: "on-demand" },
          serverType: "mac2.metal",
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );

    expect(response.status).toBe(201);
    expect(seenConfig?.awsAMI).toBe("");
    expect(seenConfig?.awsPromotedAMIs).toMatchObject({
      [awsPromotedAMIConfigKey("eu-west-1", "mac1.metal")]: "ami-mac1",
    });
  });

  it("honors requested AWS SSH ingress CIDRs over request source IP", async () => {
    let awsCIDRs: string[] = [];
    const fleet = testFleet(new MemoryStorage(), {
      aws: fakeProvider(undefined, {
        provider: "aws",
        onPrepareLeaseCreate(config, lease) {
          awsCIDRs = config.awsSSHCIDRs;
          return {
            config,
            lease: { ...lease, network: { sshSourceCIDRs: config.awsSSHCIDRs } },
          };
        },
      }),
    });
    const create = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "cf-connecting-ip": "203.0.113.7",
          "x-crabbox-org": "openclaw",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "aws",
          class: "standard",
          serverType: "c7a.8xlarge",
          awsSSHCIDRs: ["198.51.100.0/24"],
          ttlSeconds: 1200,
          idleTimeoutSeconds: 360,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );
    expect(create.status).toBe(201);
    expect(awsCIDRs).toEqual(["198.51.100.0/24"]);
  });

  it("records per-request GCP project without filling Worker-owned defaults", async () => {
    let seenConfig: LeaseConfig | undefined;
    const fleet = testFleet(new MemoryStorage(), {
      gcp: fakeProvider(
        (config) => {
          seenConfig = config;
        },
        {
          provider: "gcp",
          serverType: "c4-standard-192",
          cloudID: "crabbox-gcp-test",
        },
      ),
    });
    const create = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-admin": "true",
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "gcp",
          gcpProject: "request-project",
          class: "beast",
          serverType: "c4-standard-192",
          ttlSeconds: 1200,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );
    expect(create.status).toBe(201);
    expect(seenConfig?.gcpProject).toBe("request-project");
    expect(seenConfig?.gcpZone).toBe("");
    expect(seenConfig?.gcpImage).toBe("");
    expect(seenConfig?.gcpNetwork).toBe("");
    expect(seenConfig?.gcpTags).toEqual([]);
    expect(seenConfig?.gcpRootGB).toBe(0);
    const { lease } = (await create.json()) as { lease: LeaseRecord };
    expect(lease.providerProject).toBe("request-project");
    expect(lease.region).toBe("us-central1-a");
  });

  it("records requested type and provider fallback attempts on resolved leases", async () => {
    const attempts: ProvisioningAttempt[] = [
      {
        region: "eu-west-1",
        serverType: "c7a.48xlarge",
        market: "spot",
        category: "quota",
        message: "quota L-34B43A08 in eu-west-1 is 64 vCPUs; c7a.48xlarge needs 192 vCPUs",
      },
    ];
    const fleet = testFleet(new MemoryStorage(), {
      aws: fakeProvider(undefined, {
        provider: "aws",
        serverType: "c7i.24xlarge",
        cloudID: "i-123",
        market: "on-demand",
        attempts,
      }),
    });
    const create = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "aws",
          class: "beast",
          serverType: "c7a.48xlarge",
          ttlSeconds: 1200,
          idleTimeoutSeconds: 360,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );
    expect(create.status).toBe(201);
    const { lease } = (await create.json()) as { lease: LeaseRecord };
    expect(lease.requestedServerType).toBe("c7a.48xlarge");
    expect(lease.serverType).toBe("c7i.24xlarge");
    expect(lease.market).toBe("on-demand");
    expect(lease.provisioningAttempts).toEqual(attempts);
    expect(lease.capacityHints?.map((hint) => hint.code)).toEqual([
      "aws_capacity_routed",
      "aws_quota_pressure",
      "aws_on_demand_fallback",
      "capacity_large_class",
    ]);
    expect(lease.capacityHints?.[0]?.regionsTried).toEqual(["eu-west-1", "eu-west-2"]);
  });

  it("scopes non-admin usage to the current owner", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const createdAt = new Date().toISOString();
    const expiresAt = new Date(Date.now() + 60 * 60 * 1000).toISOString();
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        owner: "peter@example.com",
        org: "openclaw",
        estimatedHourlyUSD: 1,
        maxEstimatedUSD: 1,
        createdAt,
        expiresAt,
      }),
    );
    storage.seed(
      "lease:cbx_000000000002",
      testLease({
        id: "cbx_000000000002",
        owner: "friend@example.com",
        org: "openclaw",
        estimatedHourlyUSD: 1,
        maxEstimatedUSD: 1,
        createdAt,
        expiresAt,
      }),
    );
    const usage = await fleet.fetch(
      request("GET", `/v1/usage?scope=all&owner=peter@example.com&month=${createdAt.slice(0, 7)}`, {
        headers: {
          "x-crabbox-owner": "friend@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    expect(usage.status).toBe(200);
    const body = (await usage.json()) as {
      usage: { scope: string; owner: string; leases: number };
    };
    expect(body.usage.scope).toBe("user");
    expect(body.usage.owner).toBe("friend@example.com");
    expect(body.usage.leases).toBe(1);
  });

  it("reports marketplace gateway status with credits disabled by default", async () => {
    const fleet = testFleet(new MemoryStorage());
    const response = await fleet.fetch(
      request("GET", "/v1/marketplace/status", {
        headers: {
          "x-crabbox-owner": "friend@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );

    expect(response.status).toBe(200);
    const body = (await response.json()) as {
      marketplace: {
        enabled: boolean;
        mode: string;
        features: { payments: boolean; creditLedger: boolean; leaseEnforcement: boolean };
      };
      owner: string;
      org: string;
    };
    expect(body.owner).toBe("friend@example.com");
    expect(body.org).toBe("example-org");
    expect(body.marketplace).toMatchObject({
      enabled: false,
      mode: "disabled",
      features: {
        payments: false,
        creditLedger: false,
        leaseEnforcement: false,
      },
    });
  });

  it("returns marketplace preview quotes for smart routing candidates", async () => {
    const fleet = testFleet(
      new MemoryStorage(),
      {},
      {
        CRABBOX_MARKETPLACE_ENABLED: "1",
        CRABBOX_MARKETPLACE_BIDDING_ENABLED: "1",
        CRABBOX_MARKETPLACE_ALLOWED_PROVIDERS: "aws,hetzner",
        CRABBOX_MARKETPLACE_RATE_CARD_JSON: JSON.stringify({
          "aws:beast": { costHourlyUSD: 2, retailHourlyUSD: 3 },
          "hetzner:beast": { costHourlyUSD: 1, retailHourlyUSD: 1.5 },
        }),
      },
    );
    const response = await fleet.fetch(
      request("POST", "/v1/marketplace/quotes", {
        headers: {
          "x-crabbox-owner": "friend@example.com",
          "x-crabbox-org": "example-org",
        },
        body: {
          provider: "auto",
          class: "beast",
          ttlSeconds: 7200,
          strategy: "cheapest",
        },
      }),
    );

    expect(response.status).toBe(200);
    const body = (await response.json()) as {
      quote: {
        selected?: { provider: string; credits: number };
        candidates: Array<{ provider: string; credits: number }>;
        warnings: string[];
      };
      marketplace: { features: { bidding: boolean } };
    };
    expect(body.marketplace.features.bidding).toBe(true);
    expect(body.quote.selected).toMatchObject({ provider: "hetzner", credits: 3 });
    expect(body.quote.candidates.map((candidate) => candidate.provider)).toEqual([
      "hetzner",
      "aws",
    ]);
    expect(body.quote.warnings[0]).toContain("preview quote only");
  });

  it("rejects marketplace quotes with HTTP 409 while the gateway is disabled", async () => {
    const fleet = testFleet(new MemoryStorage());
    const response = await fleet.fetch(
      request("POST", "/v1/marketplace/quotes", {
        body: { provider: "aws", class: "beast" },
      }),
    );

    expect(response.status).toBe(409);
    const body = (await response.json()) as {
      error: string;
      marketplace: { enabled: boolean; features: { payments: boolean } };
    };
    expect(body.error).toBe("marketplace_disabled");
    expect(body.marketplace.enabled).toBe(false);
    expect(body.marketplace.features.payments).toBe(false);
  });

  it("returns HTTP 400 with the validation code for malformed quote input", async () => {
    const fleet = testFleet(
      new MemoryStorage(),
      {},
      {
        CRABBOX_MARKETPLACE_ENABLED: "1",
        CRABBOX_MARKETPLACE_ALLOWED_PROVIDERS: "aws,hetzner",
      },
    );
    const response = await fleet.fetch(
      request("POST", "/v1/marketplace/quotes", {
        body: { providers: "aws", class: "beast" },
      }),
    );

    expect(response.status).toBe(400);
    const body = (await response.json()) as { error: string; message: string };
    expect(body.error).toBe("invalid_providers");
    expect(body.message).toContain("providers must be an array");
  });

  it("resolves owner-scoped slugs and heartbeat extends idle expiry", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const touchedAt = new Date(Date.now() - 10 * 60 * 1000);
    const expiresAt = new Date(touchedAt.getTime() + 1800 * 1000);
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "peter@example.com",
        org: "openclaw",
        createdAt: touchedAt.toISOString(),
        updatedAt: touchedAt.toISOString(),
        lastTouchedAt: touchedAt.toISOString(),
        ttlSeconds: 5400,
        idleTimeoutSeconds: 1800,
        expiresAt: expiresAt.toISOString(),
      }),
    );

    const heartbeat = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/heartbeat", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
        body: {
          idleTimeoutSeconds: 2400,
          telemetry: {
            capturedAt: "2026-05-05T01:02:03Z",
            source: "ssh-linux",
            cpuCount: 16,
            load1: 0.42,
            memoryUsedBytes: 1024,
            memoryTotalBytes: 2048,
            memoryPercent: 50,
          },
        },
      }),
    );
    expect(heartbeat.status).toBe(200);
    const { lease } = (await heartbeat.json()) as { lease: LeaseRecord };
    expect(lease.id).toBe("cbx_000000000001");
    expect(lease.slug).toBe("blue-lobster");
    expect(lease.idleTimeoutSeconds).toBe(2400);
    expect(lease.telemetry).toMatchObject({
      capturedAt: "2026-05-05T01:02:03.000Z",
      source: "ssh-linux",
      cpuCount: 16,
      load1: 0.42,
      memoryUsedBytes: 1024,
      memoryTotalBytes: 2048,
      memoryPercent: 50,
    });
    expect(lease.telemetryHistory).toHaveLength(1);
    expect(lease.telemetryHistory?.[0]).toMatchObject({ load1: 0.42, memoryPercent: 50 });
    expect(Date.parse(lease.expiresAt)).toBeGreaterThan(expiresAt.getTime());

    const secondHeartbeat = await fleet.fetch(
      request("POST", "/v1/leases/cbx_000000000001/heartbeat", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
        body: {
          telemetry: {
            capturedAt: "2026-05-05T01:03:03Z",
            source: "ssh-linux",
            load1: 0.84,
            memoryPercent: 55,
          },
        },
      }),
    );
    expect(secondHeartbeat.status).toBe(200);
    const second = (await secondHeartbeat.json()) as { lease: LeaseRecord };
    expect(second.lease.telemetry).toMatchObject({
      capturedAt: "2026-05-05T01:03:03.000Z",
      load1: 0.84,
      memoryPercent: 55,
    });
    expect(second.lease.telemetryHistory?.map((sample) => sample.load1)).toEqual([0.42, 0.84]);
    expect(second.lease.telemetryHistory?.map((sample) => sample.capturedAt)).toEqual([
      "2026-05-05T01:02:03.000Z",
      "2026-05-05T01:03:03.000Z",
    ]);
  });

  it("keeps lease telemetry history bounded to the latest samples", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "peter@example.com",
        org: "openclaw",
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
        telemetryHistory: Array.from({ length: 60 }, (_, index) => ({
          capturedAt: new Date(Date.UTC(2026, 4, 5, 1, index, 0)).toISOString(),
          source: "ssh-linux",
          load1: index,
        })),
      }),
    );

    const heartbeat = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/heartbeat", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
        body: {
          telemetry: {
            capturedAt: "2026-05-05T02:00:00Z",
            source: "ssh-linux",
            load1: 61,
          },
        },
      }),
    );

    expect(heartbeat.status).toBe(200);
    const { lease } = (await heartbeat.json()) as { lease: LeaseRecord };
    expect(lease.telemetryHistory).toHaveLength(60);
    expect(lease.telemetryHistory?.[0]?.capturedAt).toBe("2026-05-05T01:01:00.000Z");
    expect(lease.telemetryHistory?.at(-1)).toMatchObject({
      capturedAt: "2026-05-05T02:00:00.000Z",
      load1: 61,
    });
  });

  it("hides exact lease IDs and lists from other non-admin users", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "peter@example.com",
        org: "openclaw",
      }),
    );
    storage.seed(
      "lease:cbx_000000000002",
      testLease({
        id: "cbx_000000000002",
        slug: "amber-krill",
        owner: "friend@example.com",
        org: "openclaw",
      }),
    );
    const friendHeaders = {
      "x-crabbox-owner": "friend@example.com",
      "x-crabbox-org": "openclaw",
    };

    const byExactID = await fleet.fetch(
      request("GET", "/v1/leases/cbx_000000000001", { headers: friendHeaders }),
    );
    expect(byExactID.status).toBe(404);

    const heartbeat = await fleet.fetch(
      request("POST", "/v1/leases/cbx_000000000001/heartbeat", {
        headers: friendHeaders,
        body: {},
      }),
    );
    expect(heartbeat.status).toBe(404);

    const list = await fleet.fetch(request("GET", "/v1/leases", { headers: friendHeaders }));
    const body = (await list.json()) as { leases: LeaseRecord[] };
    expect(body.leases.map((lease) => lease.id)).toEqual(["cbx_000000000002"]);
  });

  it("renders the portal with only the current owner leases", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "peter@example.com",
        org: "openclaw",
        desktop: true,
        code: true,
        telemetry: {
          capturedAt: new Date(Date.now() - 15_000).toISOString(),
          source: "ssh-linux",
          load1: 0.42,
          load5: 0.24,
          load15: 0.12,
          memoryUsedBytes: 1024,
          memoryTotalBytes: 2048,
          memoryPercent: 50,
          diskUsedBytes: 1024 * 1024 * 1024,
          diskTotalBytes: 4 * 1024 * 1024 * 1024,
          diskPercent: 25,
          uptimeSeconds: 3600,
        },
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );
    storage.seed(
      "lease:cbx_000000000002",
      testLease({
        id: "cbx_000000000002",
        slug: "amber-krill",
        owner: "friend@example.com",
        org: "openclaw",
        desktop: true,
      }),
    );
    storage.seed(
      "lease:cbx_000000000003",
      testLease({
        id: "cbx_000000000003",
        slug: "old-clam",
        owner: "peter@example.com",
        org: "openclaw",
        desktop: true,
        code: true,
        state: "released",
        releasedAt: "2026-05-01T00:20:00.000Z",
        endedAt: "2026-05-01T00:20:00.000Z",
      }),
    );
    storage.seed(
      "lease:cbx_000000000004",
      testLease({
        id: "cbx_000000000004",
        slug: "silver-window",
        owner: "peter@example.com",
        org: "openclaw",
        provider: "aws",
        target: "windows",
        windowsMode: "normal",
      }),
    );
    storage.seed(
      "lease:cbx_000000000005",
      testLease({
        id: "cbx_000000000005",
        slug: "wsl-window",
        owner: "peter@example.com",
        org: "openclaw",
        provider: "aws",
        target: "windows",
        windowsMode: "wsl2",
      }),
    );
    storage.seed(
      "lease:cbx_000000000006",
      testLease({
        id: "cbx_000000000006",
        slug: "azure-box",
        owner: "peter@example.com",
        org: "openclaw",
        provider: "azure",
        target: "linux",
      }),
    );
    await fleet.fetch(
      request("POST", "/v1/runners/sync", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
        body: {
          provider: "blacksmith-testbox",
          runners: [
            {
              id: "tbx_01testbox",
              status: "ready",
              repo: "openclaw",
              workflow: ".github/workflows/ci-check-testbox.yml",
              job: "lease",
              ref: "lease",
              createdAt: "2026-05-05T10:00:00.000Z",
              actionsRepo: "openclaw/openclaw",
              actionsRunID: "123456",
              actionsRunURL: "https://github.com/openclaw/openclaw/actions/runs/123456",
              actionsRunStatus: "in_progress",
              actionsWorkflowName: "ci-check-testbox",
              actionsWorkflowURL:
                "https://github.com/openclaw/openclaw/actions/workflows/ci-check-testbox.yml",
            },
          ],
        },
      }),
    );
    await fleet.fetch(
      request("POST", "/v1/runners/sync", {
        headers: {
          "x-crabbox-owner": "friend@example.com",
          "x-crabbox-org": "openclaw",
        },
        body: {
          provider: "blacksmith-testbox",
          runners: [
            {
              id: "tbx_friendbox",
              status: "ready",
              repo: "openclaw",
              workflow: ".github/workflows/ci-check-testbox.yml",
              job: "check",
              ref: "main",
            },
          ],
        },
      }),
    );

    const response = await fleet.fetch(
      request("GET", "/portal", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
      }),
    );
    expect(response.status).toBe(200);
    const body = await response.text();
    expect(body).toContain('class="portal-shell"');
    expect(body).toContain("<h1>🦀 crabbox</h1>");
    expect(body).toContain('class="portal-actions"');
    expect(body).toContain(
      ".theme-toggle .theme-icon-moon,.theme-toggle .theme-icon-sun,.theme-toggle .theme-icon-system { display:none; }",
    );
    expect(body).toContain(
      ':root[data-theme-source="system"] .theme-toggle .theme-icon-system { display:block; }',
    );
    expect(body).toContain("table-scroll");
    expect(body).toContain(".lease-table th:nth-child(1)");
    expect(body).toContain(
      'data-filter-groups="state|state|active|all:any state,active:active,ended:ended,stale:stale,stuck:stuck;provider|provider|all|all:any provider,aws:aws,azure:azure,hetzner:hetzner,blacksmith-testbox:blacksmith;target|os|all|all:any os,linux:Linux,macos:macOS,windows:Windows;kind|kind|all|all:any kind,lease:lease,external:external,dedicated:dedicated"',
    );
    expect(body).toContain('data-filter-default="active"');
    expect(body).not.toContain("external runners");
    expect(body).toContain("1 external");
    expect(body).toContain('class="external-row"');
    expect(body).toContain("no box access");
    expect(body).toContain("stuck");
    expect(body).toContain(
      'data-filter-tags="active stuck actions mine external blacksmith-testbox ready in_progress',
    );
    expect(body).toContain(
      'lease lease" data-filter-group-tags="kind:external owner:mine provider:blacksmith-testbox state:active state:stuck"',
    );
    expect(body).toContain("tbx_01testbox");
    expect(body).toContain("/portal/runners/blacksmith-testbox/tbx_01testbox");
    expect(body).toContain("blacksmith-testbox");
    expect(body).toContain("ci-check-testbox.yml");
    expect(body).toContain("https://github.com/openclaw/openclaw/actions/runs/123456");
    expect(body).toContain(
      "https://github.com/openclaw/openclaw/actions/workflows/ci-check-testbox.yml",
    );
    expect(body).toContain('class="row-link"');
    expect(body).toContain(
      'data-copy-value="crabbox stop --provider blacksmith-testbox tbx_01testbox"',
    );
    expect(body).not.toContain("tbx_friendbox");
    expect(body).toContain('data-provider="azure"');
    expect(body).toContain('data-provider="hetzner"');
    expect(body).toContain('data-target="linux"');
    expect(body).toContain('data-target="windows"');
    expect(body).toContain("<span>win</span>");
    expect(body).toContain("<span>win (wsl2)</span>");
    expect(body).toContain(
      'data-filter-tags="lease active mine hetzner linux" data-filter-group-tags="kind:lease owner:mine provider:hetzner state:active target:linux"',
    );
    expect(body).toContain(
      'data-filter-tags="lease active mine azure linux" data-filter-group-tags="kind:lease owner:mine provider:azure state:active target:linux"',
    );
    expect(body).toContain('groupTags.includes(group.key + ":" + value)');
    expect(body).toContain('class="access-cell"');
    expect(body).toContain('title="server"');
    expect(body).toContain('data-access="vscode"');
    expect(body).toContain('data-access="vnc"');
    expect(body).toContain('data-release-kind="managed"');
    expect(body).toContain('title="Stop blue-lobster"');
    expect(body).toContain("This deletes the backing machine.");
    expect(body).toContain('action="/portal/leases/cbx_000000000001/release?return=%2Fportal"');
    expect(body).not.toContain('action="/portal/leases/cbx_000000000003/release?return=%2Fportal"');
    expect(body).toContain("data-sort=");
    expect(body).toContain("<time datetime=");
    expect(body).not.toContain("windows / normal");
    expect(body).toContain("blue-lobster");
    expect(body).toContain("old-clam");
    expect(body).toContain("released");
    expect(body).toContain("/portal/leases/cbx_000000000001");
    expect(body).toContain("/portal/leases/cbx_000000000001/vnc");
    expect(body).toContain("/portal/leases/cbx_000000000001/code/");
    expect(body).not.toContain("amber-krill");
  });

  it("renders AWS mac host capacity on the portal when configured", async () => {
    const fetchMock = vi.fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>(
      async (input, init) => {
        const params = new URLSearchParams(await requestBodyForTest(input, init));
        expect(params.get("Action")).toBe("DescribeHosts");
        return ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
        <DescribeHostsResponse>
          <hostSet>
            <item>
              <hostId>h-000000000001</hostId>
              <state>available</state>
              <availabilityZone>eu-west-1a</availabilityZone>
              <autoPlacement>on</autoPlacement>
              <allocationTime>2026-05-15T00:00:00Z</allocationTime>
              <hostProperties><instanceType>mac2.metal</instanceType></hostProperties>
            </item>
            <item>
              <hostId>h-000000000002</hostId>
              <state>pending</state>
              <availabilityZone>eu-west-1b</availabilityZone>
              <autoPlacement>off</autoPlacement>
              <allocationTime>2026-05-17T00:00:00Z</allocationTime>
              <hostProperties><instanceType>mac2.metal</instanceType></hostProperties>
              <tagSet><item><key>crabbox</key><value>true</value></item></tagSet>
            </item>
            <item>
              <hostId>h-000000000003</hostId>
              <state>available</state>
              <availabilityZone>eu-west-1c</availabilityZone>
              <autoPlacement>on</autoPlacement>
              <allocationTime>2026-05-18T00:00:00Z</allocationTime>
              <hostProperties><instanceType>mac2.metal</instanceType></hostProperties>
            </item>
          </hostSet>
        </DescribeHostsResponse>`);
      },
    );
    vi.stubGlobal("fetch", fetchMock);
    const storage = new MemoryStorage();
    storage.seed(
      "lease:cbx_000000000099",
      testLease({
        id: "cbx_000000000099",
        slug: "mac-mini",
        provider: "aws",
        target: "macos",
        desktop: true,
        class: "mac",
        serverType: "mac2.metal",
        hostId: "h-000000000001",
        cloudID: "i-000000000099",
        region: "eu-west-1",
        owner: "alice@example.com",
        org: "example-org",
        share: { users: { "friend@example.com": "use" } },
        createdAt: "2026-05-17T00:10:00.000Z",
        updatedAt: "2026-05-17T00:20:00.000Z",
        lastTouchedAt: "2026-05-17T00:20:00.000Z",
        expiresAt: "2026-05-17T02:20:00.000Z",
      }),
    );
    storage.seed(
      "lease:cbx_000000000100",
      testLease({
        id: "cbx_000000000100",
        slug: "mac-no-vnc",
        provider: "aws",
        target: "macos",
        desktop: false,
        class: "mac",
        serverType: "mac2.metal",
        hostId: "h-000000000002",
        cloudID: "i-000000000100",
        region: "eu-west-1",
        owner: "alice@example.com",
        org: "example-org",
        createdAt: "2026-05-17T00:30:00.000Z",
        updatedAt: "2026-05-17T00:40:00.000Z",
        lastTouchedAt: "2026-05-17T00:40:00.000Z",
        expiresAt: "2026-05-17T02:40:00.000Z",
      }),
    );
    for (let index = 0; index < 101; index += 1) {
      const id = `cbx_${(index + 1_000).toString(16).padStart(12, "0")}`;
      const createdAt = new Date(Date.UTC(2026, 5, 1, 0, 0, index)).toISOString();
      storage.seed(
        `lease:${id}`,
        testLease({
          id,
          slug: `newer-linux-${index}`,
          owner: "alice@example.com",
          org: "example-org",
          createdAt,
          updatedAt: createdAt,
          lastTouchedAt: createdAt,
          expiresAt: "2026-06-02T00:00:00.000Z",
        }),
      );
    }
    const fleet = testFleet(
      storage,
      {},
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "test",
        CRABBOX_AWS_REGION: "eu-west-1",
      },
    );

    const response = await fleet.fetch(
      request("GET", "/portal", {
        headers: {
          "x-crabbox-admin": "true",
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );

    expect(response.status).toBe(200);
    const body = await response.text();
    expect(body).not.toContain("mac hosts");
    expect(body).not.toContain("1 available / 2 total");
    expect(body).toContain('class="capacity-row"');
    expect(body).toContain("dedicated");
    expect(body).toContain('data-filter-tags="active mine dedicated host aws macos available');
    expect(body).toContain("/portal/hosts/aws/h-000000000001");
    expect(body).toContain("/portal/hosts/aws/h-000000000001/vnc");
    expect(body).toContain("/portal/hosts/aws/h-000000000002/vnc");
    expect(body).toContain("/portal/hosts/aws/h-000000000003/vnc");
    expect(body).toContain("lease mac-mini");
    expect(body).toContain("lease mac-no-vnc");
    expect(body).toContain("mac2.metal");
    expect(body).toContain("eu-west-1a");
    expect(body).toContain("eu-west-1b");
    expect(body).toContain("eu-west-1c");
    expect(body).toContain("available");
    expect(body).toContain("pending");

    const userHome = await fleet.fetch(
      request("GET", "/portal", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    const userHomeBody = await userHome.text();
    expect(userHomeBody).toContain("/portal/hosts/aws/h-000000000001");
    expect(userHomeBody).toContain("/portal/hosts/aws/h-000000000002");
    expect(userHomeBody).not.toContain("h-000000000003");

    const friendHome = await fleet.fetch(
      request("GET", "/portal", {
        headers: {
          "x-crabbox-owner": "friend@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    const friendHomeBody = await friendHome.text();
    expect(friendHomeBody).toContain("/portal/hosts/aws/h-000000000001");
    expect(friendHomeBody).not.toContain("h-000000000002");
    expect(friendHomeBody).not.toContain("h-000000000003");

    const filteredHome = await fleet.fetch(
      request("GET", "/portal?provider=hetzner", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    expect(await filteredHome.text()).not.toContain("/portal/hosts/aws/");

    const detail = await fleet.fetch(
      request("GET", "/portal/hosts/aws/h-000000000001", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    expect(detail.status).toBe(200);
    const detailBody = await detail.text();
    expect(detailBody).toContain("dedicated host");
    expect(detailBody).toContain("attached lease");
    expect(detailBody).toContain("mac-mini / cbx_000000000099");
    expect(detailBody).toContain("/portal/leases/cbx_000000000099/vnc");
    expect(detailBody).toContain(
      "crabbox webvnc --provider aws --target macos --id mac-mini --open",
    );

    const friendDetail = await fleet.fetch(
      request("GET", "/portal/hosts/aws/h-000000000001", {
        headers: {
          "x-crabbox-owner": "friend@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    expect(friendDetail.status).toBe(200);
    const friendDetailBody = await friendDetail.text();
    expect(friendDetailBody).toContain("mac-mini / cbx_000000000099");
    expect(friendDetailBody).not.toContain(
      "crabbox webvnc --provider aws --target macos --id mac-mini --open",
    );

    const hiddenHost = await fleet.fetch(
      request("GET", "/portal/hosts/aws/h-000000000003", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    expect(hiddenHost.status).toBe(404);

    const vnc = await fleet.fetch(
      request("GET", "/portal/hosts/aws/h-000000000001/vnc", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    expect(vnc.status).toBe(303);
    expect(vnc.headers.get("location")).toBe("/portal/leases/cbx_000000000099/vnc");

    const enablePage = await fleet.fetch(
      request("GET", "/portal/hosts/aws/h-000000000002", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    expect(enablePage.status).toBe(200);
    const enableBody = await enablePage.text();
    expect(enableBody).toContain("open VNC");
    expect(enableBody).toContain("desktop</dt><dd>enabled");

    const macVNC = await fleet.fetch(
      request("GET", "/portal/hosts/aws/h-000000000002/vnc", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    expect(macVNC.status).toBe(303);
    expect(macVNC.headers.get("location")).toBe("/portal/leases/cbx_000000000100/vnc");

    const enabled = await fleet.fetch(
      request("POST", "/portal/hosts/aws/h-000000000002/vnc", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    expect(enabled.status).toBe(303);
    expect(enabled.headers.get("location")).toBe("/portal/leases/cbx_000000000100/vnc");
    expect(storage.value<LeaseRecord>("lease:cbx_000000000100")?.desktop).toBe(true);

    const startPage = await fleet.fetch(
      request("GET", "/portal/hosts/aws/h-000000000003/vnc", {
        headers: {
          "x-crabbox-admin": "true",
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    expect(startPage.status).toBe(200);
    const startBody = await startPage.text();
    expect(startBody).toContain("start desktop lease");
    expect(startBody).not.toContain('name="sshPublicKey"');
    expect(startBody).toContain(
      "CRABBOX_HOST_ID=h-000000000003 crabbox warmup --provider aws --target macos --market on-demand --type mac2.metal --desktop",
    );
    expect(startBody).toContain(
      'data-copy-value="CRABBOX_HOST_ID=h-000000000003 crabbox warmup --provider aws --target macos --market on-demand --type mac2.metal --desktop"',
    );
    expect(startBody).toContain("copy start command");
    expect(startBody).toContain("crabbox webvnc --id &lt;lease-id-or-slug&gt; --open");
    expect(startBody).toContain("host-pinned macOS run");

    const idlePost = await fleet.fetch(
      request("POST", "/portal/hosts/aws/h-000000000003/vnc", {
        headers: {
          "x-crabbox-admin": "true",
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
        body: {},
      }),
    );
    expect(idlePost.status).toBe(409);
    await expect(idlePost.text()).resolves.toContain("No active Crabbox lease");
  });

  it("syncs external runner visibility and marks missing runners stale", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const headers = {
      "x-crabbox-owner": "peter@example.com",
      "x-crabbox-org": "openclaw",
    };

    const sync = await fleet.fetch(
      request("POST", "/v1/runners/sync", {
        headers,
        body: {
          provider: "blacksmith-testbox",
          runners: [
            {
              id: "tbx_01kqyahxh67z6qtwtsdkt5xcst",
              status: "ready",
              repo: "openclaw",
              workflow: ".github/workflows/ci-check-testbox.yml",
              job: "check",
              ref: "main",
              createdAt: "2026-05-06T09:45:16.000000Z",
              actionsRunURL: "https://github.com/openclaw/openclaw/actions/runs/123456",
              actionsWorkflowURL:
                "https://github.com/openclaw/openclaw/actions/workflows/ci-check-testbox.yml",
            },
          ],
        },
      }),
    );
    expect(sync.status).toBe(200);
    const synced = (await sync.json()) as { runners: ExternalRunnerRecord[] };
    expect(synced.runners).toHaveLength(1);
    expect(synced.runners[0]).toMatchObject({
      id: "tbx_01kqyahxh67z6qtwtsdkt5xcst",
      provider: "blacksmith-testbox",
      status: "ready",
      repo: "openclaw",
      owner: "peter@example.com",
      org: "openclaw",
      actionsRunURL: "https://github.com/openclaw/openclaw/actions/runs/123456",
    });

    const friendList = await fleet.fetch(
      request("GET", "/v1/runners", {
        headers: {
          "x-crabbox-owner": "friend@example.com",
          "x-crabbox-org": "openclaw",
        },
      }),
    );
    const friendBody = (await friendList.json()) as { runners: ExternalRunnerRecord[] };
    expect(friendBody.runners).toHaveLength(0);

    const staleSync = await fleet.fetch(
      request("POST", "/v1/runners/sync", {
        headers,
        body: {
          provider: "blacksmith-testbox",
          runners: [],
        },
      }),
    );
    expect(staleSync.status).toBe(200);
    const staleBody = (await staleSync.json()) as { stale: ExternalRunnerRecord[] };
    expect(staleBody.stale).toHaveLength(1);
    expect(staleBody.stale[0]).toMatchObject({
      id: "tbx_01kqyahxh67z6qtwtsdkt5xcst",
      status: "missing",
      stale: true,
    });
  });

  it("renders external runner detail pages for visible runners", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const headers = {
      "x-crabbox-owner": "peter@example.com",
      "x-crabbox-org": "openclaw",
    };
    const sync = await fleet.fetch(
      request("POST", "/v1/runners/sync", {
        headers,
        body: {
          provider: "blacksmith-testbox",
          runners: [
            {
              id: "tbx_detail",
              status: "ready",
              repo: "openclaw",
              workflow: ".github/workflows/ci-check-testbox.yml",
              job: "check",
              ref: "main",
              createdAt: "2026-05-06T09:45:16.000000Z",
              actionsRepo: "openclaw/openclaw",
              actionsRunID: "123456",
              actionsRunURL: "https://github.com/openclaw/openclaw/actions/runs/123456",
              actionsRunStatus: "queued",
              actionsWorkflowName: "Blacksmith Testbox",
              actionsWorkflowURL:
                "https://github.com/openclaw/openclaw/actions/workflows/ci-check-testbox.yml",
            },
          ],
        },
      }),
    );
    expect(sync.status).toBe(200);

    const detail = await fleet.fetch(
      request("GET", "/portal/runners/blacksmith-testbox/tbx_detail", { headers }),
    );
    expect(detail.status).toBe(200);
    const body = await detail.text();
    expect(body).toContain("tbx_detail");
    expect(body).toContain("actions owner");
    expect(body).toContain("Blacksmith Testbox");
    expect(body).toContain("https://github.com/openclaw/openclaw/actions/runs/123456");
    expect(body).toContain("crabbox stop --provider blacksmith-testbox tbx_detail");
    expect(body).toContain("visibility only");

    const hidden = await fleet.fetch(
      request("GET", "/portal/runners/blacksmith-testbox/tbx_detail", {
        headers: {
          "x-crabbox-owner": "friend@example.com",
          "x-crabbox-org": "openclaw",
        },
      }),
    );
    expect(hidden.status).toBe(404);
  });

  it("shows non-owned runner leases only in the admin portal", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "peter@example.com",
        org: "openclaw",
      }),
    );
    storage.seed(
      "lease:cbx_000000000002",
      testLease({
        id: "cbx_000000000002",
        slug: "testbox-runner",
        owner: "blacksmith",
        org: "openclaw",
        provider: "aws",
        class: "standard",
      }),
    );

    const userResponse = await fleet.fetch(
      request("GET", "/portal", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
      }),
    );
    const userBody = await userResponse.text();
    expect(userBody).toContain("blue-lobster");
    expect(userBody).not.toContain("testbox-runner");
    expect(userBody).not.toContain("system:system");
    expect(userBody).not.toContain("owner|owner");

    const adminResponse = await fleet.fetch(
      request("GET", "/portal", {
        headers: {
          "x-crabbox-admin": "true",
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
      }),
    );
    expect(adminResponse.status).toBe(200);
    const adminBody = await adminResponse.text();
    expect(adminBody).toContain("blue-lobster");
    expect(adminBody).toContain("testbox-runner");
    expect(adminBody).toContain("1 system");
    expect(adminBody).toContain("owner|owner|all|all:any owner,mine:mine,system:system");
    expect(adminBody).toContain('data-filter-tags="lease active mine hetzner linux"');
    expect(adminBody).toContain('data-filter-tags="lease active system aws linux"');
    expect(adminBody).toContain("cbx_000000000002 · blacksmith");

    const detail = await fleet.fetch(
      request("GET", "/portal/leases/cbx_000000000002", {
        headers: {
          "x-crabbox-admin": "true",
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
      }),
    );
    expect(detail.status).toBe(200);
  });

  it("defaults the portal lease table to all leases when none are active", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    storage.seed(
      "lease:cbx_000000000003",
      testLease({
        id: "cbx_000000000003",
        slug: "old-clam",
        owner: "peter@example.com",
        org: "openclaw",
        state: "expired",
        endedAt: "2026-05-01T00:20:00.000Z",
      }),
    );

    const response = await fleet.fetch(
      request("GET", "/portal", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
      }),
    );
    expect(response.status).toBe(200);
    const body = await response.text();
    expect(body).toContain('data-filter-default="all"');
    expect(body).toContain("old-clam");
    expect(body).toContain("expired");
    expect(body).not.toContain("no leases visible");
  });

  it("renders lease detail pages with run logs and stop controls", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage, { hetzner: fakeProvider() });
    const headers = {
      "x-crabbox-owner": "peter@example.com",
      "x-crabbox-org": "openclaw",
    };
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "peter@example.com",
        org: "openclaw",
        desktop: true,
        code: true,
        telemetry: {
          capturedAt: new Date(Date.now() - 15_000).toISOString(),
          source: "ssh-linux",
          cpuCount: 16,
          load1: 0.42,
          load5: 0.24,
          load15: 0.12,
          memoryUsedBytes: 1024,
          memoryTotalBytes: 2048,
          memoryPercent: 50,
          diskUsedBytes: 1024 * 1024 * 1024,
          diskTotalBytes: 4 * 1024 * 1024 * 1024,
          diskPercent: 25,
          uptimeSeconds: 3600,
        },
        telemetryHistory: [
          {
            capturedAt: new Date(Date.now() - 45_000).toISOString(),
            source: "ssh-linux",
            load1: 0.22,
            memoryPercent: 42,
            diskPercent: 24,
          },
          {
            capturedAt: new Date(Date.now() - 30_000).toISOString(),
            source: "ssh-linux",
            load1: 0.32,
            memoryPercent: 47,
            diskPercent: 25,
          },
        ],
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );
    storage.seed(
      "run:run_000000000001",
      testRun({
        id: "run_000000000001",
        leaseID: "cbx_000000000001",
        owner: "peter@example.com",
        org: "openclaw",
        command: ["go", "test", "./..."],
        state: "failed",
        phase: "failed",
        exitCode: 1,
        blockedStage: "unknown",
        retryLikely: "unknown",
        durationMs: 1234,
        logBytes: 11,
        telemetry: {
          start: {
            capturedAt: "2026-05-01T00:00:00.000Z",
            source: "ssh-linux",
            load1: 0.12,
            memoryUsedBytes: 1024,
            memoryTotalBytes: 2048,
            memoryPercent: 50,
            diskUsedBytes: 1024 * 1024,
            diskTotalBytes: 4 * 1024 * 1024,
            diskPercent: 25,
          },
          end: {
            capturedAt: "2026-05-01T00:00:02.000Z",
            source: "ssh-linux",
            load1: 0.42,
            load5: 0.24,
            load15: 0.12,
            memoryUsedBytes: 1536,
            memoryTotalBytes: 2048,
            memoryPercent: 75,
            diskUsedBytes: 2 * 1024 * 1024,
            diskTotalBytes: 4 * 1024 * 1024,
            diskPercent: 50,
          },
        },
        results: {
          format: "junit",
          files: ["junit.xml"],
          suites: 1,
          tests: 2,
          failures: 1,
          errors: 0,
          skipped: 0,
          timeSeconds: 0.42,
          failed: [
            {
              suite: "portal",
              name: "renders detail",
              message: "expected detail page",
              kind: "failure",
            },
          ],
        },
      }),
    );
    storage.seed(
      "run:run_000000000002",
      testRun({
        id: "run_000000000002",
        leaseID: "cbx_000000000001",
        owner: "friend@example.com",
        org: "openclaw",
      }),
    );
    storage.seed("runlog:run_000000000001", "portal log\n");
    storage.seed("runevent:run_000000000001:000000000001", {
      runID: "run_000000000001",
      seq: 1,
      type: "command.finished",
      phase: "failed",
      createdAt: "2026-05-01T00:00:01.000Z",
    });

    const page = await fleet.fetch(request("GET", "/portal/leases/blue-lobster", { headers }));
    expect(page.status).toBe(200);
    const body = await page.text();
    expect(body).toContain("crabbox ssh --id blue-lobster");
    expect(body).toContain("crabbox run --id blue-lobster -- &lt;command&gt;");
    expect(body).toContain(
      "crabbox webvnc --provider hetzner --target linux --id blue-lobster --open",
    );
    expect(body).toContain("crabbox code --id blue-lobster --open");
    expect(body).toContain("data-copy-command");
    expect(body).toContain('querySelector("code")');
    expect(body).toContain('class="portal-shell lease-shell"');
    expect(body).toContain("<h1>🦀 crabbox</h1>");
    expect(body).toContain("blue-lobster · hetzner linux lease");
    expect(body).toContain('data-search-placeholder="search runs"');
    expect(body).toContain(
      'data-filter-buttons="succeeded:succeeded,failed:failed,running:running,all:all"',
    );
    expect(body).not.toContain("<th>phase</th>");
    expect(body).not.toContain("<th>log</th>");
    expect(body).toContain('title="2026-05-01T00:00:00Z"');
    expect(body).toContain('data-provider="hetzner"');
    expect(body).toContain('data-target="linux"');
    expect(body).toContain("<dt>load</dt><dd>0.42 / 0.24 / 0.12</dd>");
    expect(body).toContain("<dt>cpu</dt><dd>16 vCPUs</dd>");
    expect(body).toContain("<dt>memory</dt><dd>1.0 KiB / 2.0 KiB (50%)</dd>");
    expect(body).toContain("<dt>disk</dt><dd>1.0 GiB / 4.0 GiB (25%)</dd>");
    expect(body).toContain("<dt>uptime</dt><dd>1h</dd>");
    expect(body).toContain("box telemetry");
    expect(body).toContain('class="telemetry-chart"');
    expect(body).toContain("<span>0.42</span>");
    expect(body).toContain("<span>50%</span>");
    expect(body).toContain("load 0.42 · mem 75% · +512 B");
    expect(body).toContain("table-search");
    expect(body).toContain("/portal/runs/run_000000000001");
    expect(body).toContain("/portal/runs/run_000000000001/logs");
    expect(body).toContain("/portal/runs/run_000000000001/events");
    expect(body).toContain("/portal/leases/cbx_000000000001/release");
    expect(body).toContain("run_000000000002");

    const logs = await fleet.fetch(
      request("GET", "/portal/runs/run_000000000001/logs", { headers }),
    );
    expect(logs.status).toBe(200);
    expect(logs.headers.get("content-type")).toBe("text/plain; charset=utf-8");
    expect(await logs.text()).toBe("portal log\n");

    const runPage = await fleet.fetch(request("GET", "/portal/runs/run_000000000001", { headers }));
    expect(runPage.status).toBe(200);
    expect(runPage.headers.get("content-type")).toBe("text/html; charset=utf-8");
    const runBody = await runPage.text();
    expect(runBody).toContain('class="portal-shell run-shell"');
    expect(runBody).toContain('class="panel detail-card run-summary-card"');
    expect(runBody).toContain(
      ".run-shell .meta-grid { grid-template-columns:repeat(3,minmax(0,1fr)); }",
    );
    expect(runBody).toContain("<h1>🦀 crabbox</h1>");
    expect(runBody).toContain(
      ".portal-header-meta { flex:1 1 auto; min-width:0; overflow:hidden; }",
    );
    expect(runBody).toContain(".command-row > div { min-width:0; overflow:hidden; }");
    expect(runBody).toContain("run_000000000001 · cbx_000000000001 · failed");
    expect(runBody).not.toContain('<span class="mono">go test ./...</span>');
    expect(runBody).toContain("run_000000000001");
    expect(runBody).toContain("go test ./...");
    expect(runBody).toContain("data-copy-command");
    expect(runBody).toContain("portal log");
    expect(runBody).toContain('data-copy-target="#run-log-tail"');
    expect(runBody).toContain('data-search-placeholder="search events"');
    expect(runBody).toContain(
      'data-filter-buttons="run:run,command:command,sync:sync,stdout:stdout,stderr:stderr,all:all"',
    );
    expect(runBody).toContain('data-filter-tags="command failed"');
    expect(runBody).toContain("<dt>area</dt><dd>command</dd>");
    expect(runBody).not.toContain("<dt>blocked</dt><dd>unknown</dd>");
    expect(runBody).not.toContain("<dt>retry</dt><dd>unknown</dd>");
    expect(runBody).toContain('class="run-telemetry-grid"');
    expect(runBody).toContain(".run-artifact-card .button { width:100%; }");
    expect(runBody).toContain("@media (max-width: 980px)");
    expect(runBody).toContain(
      ".run-telemetry-grid { grid-template-columns:repeat(2,minmax(0,1fr)); }",
    );
    expect(runBody).toContain("<span>load</span>");
    expect(runBody).toContain("<strong>0.42 / 0.24 / 0.12</strong>");
    expect(runBody).toContain("<strong>1.5 KiB / 2.0 KiB (75%)</strong>");
    expect(runBody).toContain("<small>delta +512 B</small>");
    expect(runBody).toContain("table-search");
    expect(runBody).toContain("renders detail");
    expect(runBody).toContain("/portal/leases/cbx_000000000001");
    expect(runBody).toContain("/portal/runs/run_000000000001/logs");

    const events = await fleet.fetch(
      request("GET", "/portal/runs/run_000000000001/events", { headers }),
    );
    expect(events.status).toBe(200);
    await expect(events.json()).resolves.toMatchObject({
      events: [{ runID: "run_000000000001", type: "command.finished" }],
    });

    const stop = await fleet.fetch(
      request("POST", "/portal/leases/blue-lobster/release", { headers }),
    );
    expect(stop.status).toBe(303);
    expect(stop.headers.get("location")).toBe("/portal");
    expect(storage.value<LeaseRecord>("lease:cbx_000000000001")).toMatchObject({
      state: "released",
      keep: false,
    });
  });

  it("fails closed without an isolated Code origin while retaining health and bridge tickets", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const headers = {
      "x-crabbox-owner": "peter@example.com",
      "x-crabbox-org": "openclaw",
    };
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "peter@example.com",
        org: "openclaw",
        code: true,
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );
    storage.seed(
      "lease:cbx_000000000002",
      testLease({
        id: "cbx_000000000002",
        slug: "plain-lobster",
        owner: "peter@example.com",
        org: "openclaw",
        code: false,
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );

    const page = await fleet.fetch(
      request("GET", "/portal/leases/blue-lobster/code/", { headers }),
    );
    expect(page.status).toBe(409);
    const pageBody = await page.text();
    expect(pageBody).toContain("Code origin required");
    expect(pageBody).toContain("CRABBOX_CODE_ORIGIN_TEMPLATE");

    const health = await fleet.fetch(
      request("GET", "/portal/leases/blue-lobster/code/health", { headers }),
    );
    expect(health.status).toBe(200);
    const healthBody = (await health.json()) as {
      lease: { id: string; code: boolean };
      code: { agentConnected: boolean };
    };
    expect(healthBody.lease).toMatchObject({ id: "cbx_000000000001", code: true });
    expect(healthBody.code.agentConnected).toBe(false);

    const plain = await fleet.fetch(
      request("GET", "/portal/leases/plain-lobster/code/", { headers }),
    );
    expect(plain.status).toBe(409);

    const ticket = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/code/ticket", { headers, body: {} }),
    );
    expect(ticket.status).toBe(200);
    const ticketBody = (await ticket.json()) as { ticket: string; leaseID: string };
    expect(ticketBody.ticket).toMatch(/^code_[a-f0-9]{32}$/);
    expect(ticketBody.leaseID).toBe("cbx_000000000001");

    const agent = await fleet.fetch(
      request("GET", "/v1/leases/blue-lobster/code/agent", { headers }),
    );
    expect(agent.status).toBe(426);

    const missingTicket = await fleet.fetch(
      request("GET", "/v1/leases/blue-lobster/code/agent", {
        headers: { upgrade: "websocket" },
      }),
    );
    expect(missingTicket.status).toBe(401);
  });

  it("blocks Code HTTP and WebSocket proxying for invalid origin templates", async () => {
    await Promise.all(
      [undefined, "https://code.example.test/{lease}"].map(async (template) => {
        const storage = new MemoryStorage();
        const fleet = testFleet(storage, {}, { CRABBOX_CODE_ORIGIN_TEMPLATE: template });
        const leaseID = "cbx_000000000001";
        storage.seed(
          `lease:${leaseID}`,
          testLease({
            id: leaseID,
            slug: "blue-lobster",
            owner: "alice@example.com",
            org: "example-org",
            code: true,
            expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
          }),
        );
        const agent = new FakeWebSocket({ kind: "code-agent", leaseID });
        const relay = fleet as unknown as { codeAgents: Map<string, WebSocket> };
        relay.codeAgents.set(leaseID, agent as unknown as WebSocket);
        const headers = {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        };

        const page = await fleet.fetch(
          request("GET", "/portal/leases/blue-lobster/code/api/status", { headers }),
        );
        expect(page.status).toBe(409);
        const socket = await fleet.fetch(
          request("GET", "/portal/leases/blue-lobster/code/websocket", {
            headers: { ...headers, origin: "https://example.test", upgrade: "websocket" },
          }),
        );
        expect(socket.status).toBe(409);
        expect(agent.sentJSON()).toEqual([]);
      }),
    );
  });

  it("redirects owners, managers, and admins to the isolated Code origin", async () => {
    const storage = new MemoryStorage();
    const env = { CRABBOX_CODE_ORIGIN_TEMPLATE: "https://{lease}.code.example.test" };
    const fleet = testFleet(storage, {}, env);
    const leaseID = "cbx_000000000001";
    storage.seed(
      `lease:${leaseID}`,
      testLease({
        id: leaseID,
        slug: "blue-lobster",
        owner: "owner@example.com",
        org: "example-org",
        code: true,
        share: { users: { "manager@example.com": "manage" } },
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );
    const isolatedOrigin = await codeOriginForLease(env, leaseID);
    expect(isolatedOrigin).toBeDefined();
    const viewers = [
      {
        "x-crabbox-auth": "bearer",
        "x-crabbox-owner": "owner@example.com",
        "x-crabbox-org": "example-org",
      },
      {
        "x-crabbox-auth": "bearer",
        "x-crabbox-owner": "manager@example.com",
        "x-crabbox-org": "other-org",
      },
      {
        "x-crabbox-auth": "bearer",
        "x-crabbox-owner": "admin@example.com",
        "x-crabbox-org": "other-org",
        "x-crabbox-admin": "true",
      },
    ];
    await Promise.all(
      viewers.map(async (headers) => {
        const handoff = await fleet.fetch(
          request("GET", "/portal/leases/blue-lobster/code/", { headers }),
        );
        expect(handoff.status).toBe(200);
        const bootstrap = await readCodeBootstrapHandoff(handoff);
        expect(bootstrap.url.origin).toBe(isolatedOrigin);
      }),
    );
  });

  it("bootstraps a one-time lease-scoped Code session on an isolated origin", async () => {
    const storage = new MemoryStorage();
    const env = {
      CRABBOX_CODE_ORIGIN_TEMPLATE: "https://{lease}.code.example.test",
      CRABBOX_PUBLIC_URL: "https://crabbox.test",
    };
    const fleet = testFleet(storage, {}, env);
    const leaseID = "cbx_000000000001";
    const tokenExpiresAt = new Date(Date.now() + 5 * 60 * 1000).toISOString();
    const headers = {
      authorization: "Bearer portal-session-token",
      "x-crabbox-auth": "github",
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
      "x-crabbox-github-login": "alice",
      "x-crabbox-token-expires-at": tokenExpiresAt,
    };
    storage.seed(
      `lease:${leaseID}`,
      testLease({
        id: leaseID,
        slug: "blue-lobster",
        owner: "alice@example.com",
        org: "example-org",
        code: true,
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );

    const coordinatorHealth = await fleet.fetch(
      request("GET", "/portal/leases/blue-lobster/code/health", { headers }),
    );
    expect(coordinatorHealth.status).toBe(200);
    await expect(coordinatorHealth.json()).resolves.toMatchObject({
      lease: { id: leaseID, code: true },
      code: { agentConnected: false },
    });

    const handoff = await fleet.fetch(
      request("GET", "/portal/leases/blue-lobster/code/?folder=%2Fwork%2Frepo", { headers }),
    );
    expect(handoff.status).toBe(200);
    expect(handoff.headers.get("location")).toBeNull();
    expect(handoff.headers.get("cache-control")).toBe("no-store");
    expect(handoff.headers.get("referrer-policy")).toBe("no-referrer");
    expect(handoff.headers.get("content-security-policy")).toContain("default-src 'none'");
    expect(handoff.headers.get("content-security-policy")).toContain("frame-ancestors 'none'");
    const bootstrapHandoff = await readCodeBootstrapHandoff(handoff);
    const bootstrapURL = bootstrapHandoff.url;
    expect(bootstrapURL.hostname).toMatch(/^cbx-[a-f0-9]{32}\.code\.example\.test$/);
    expect(bootstrapURL.pathname).toBe(`/portal/leases/${leaseID}/code/__crabbox_bootstrap`);
    expect(bootstrapURL.search).toBe("");
    expect(bootstrapHandoff.ticket).toMatch(/^code_view_[a-f0-9]{32}$/);

    const copiedURL = await fleet.fetch(new Request(bootstrapURL));
    expect(copiedURL.status).toBe(405);
    expect(copiedURL.headers.get("set-cookie")).toBeNull();

    const bootstrap = await fleet.fetch(
      codeBootstrapRequest(bootstrapURL, bootstrapHandoff.ticket),
    );
    expect(bootstrap.status).toBe(303);
    expect(bootstrap.headers.get("location")).toBe(
      `/portal/leases/${leaseID}/code/?folder=%2Fwork%2Frepo`,
    );
    const cookie = bootstrap.headers.get("set-cookie") || "";
    expect(cookie).toContain("crabbox_code_session=code_session_");
    expect(cookie).toContain(`Path=/portal/leases/${leaseID}/code/`);
    expect(cookie).toContain("HttpOnly");
    expect(cookie).toContain("Secure");
    const maxAge = Number.parseInt(/Max-Age=(\d+)/.exec(cookie)?.[1] || "", 10);
    expect(maxAge).toBeGreaterThanOrEqual(295);
    expect(maxAge).toBeLessThanOrEqual(300);

    const replay = await fleet.fetch(codeBootstrapRequest(bootstrapURL, bootstrapHandoff.ticket));
    expect(replay.status).toBe(401);

    const page = await fleet.fetch(
      new Request(`${bootstrapURL.origin}${bootstrap.headers.get("location")}`, {
        headers: { cookie: cookie.split(";", 1)[0] || "" },
      }),
    );
    expect(page.status).toBe(200);
    expect(await page.text()).toContain("crabbox code --id blue-lobster --open");

    const sessionCookie = cookie.split(";", 1)[0] || "";
    const crossOriginPost = await fleet.fetch(
      new Request(`${bootstrapURL.origin}/portal/leases/${leaseID}/code/api/save`, {
        method: "POST",
        headers: {
          cookie: sessionCookie,
          origin: "https://other-lease.code.example.test",
        },
      }),
    );
    expect(crossOriginPost.status).toBe(403);

    const crossOriginWebSocket = await fleet.fetch(
      new Request(`${bootstrapURL.origin}/portal/leases/${leaseID}/code/websocket`, {
        headers: {
          cookie: sessionCookie,
          origin: "https://other-lease.code.example.test",
          upgrade: "websocket",
        },
      }),
    );
    expect(crossOriginWebSocket.status).toBe(403);

    const sameOriginPost = await fleet.fetch(
      new Request(`${bootstrapURL.origin}/portal/leases/${leaseID}/code/api/save`, {
        method: "POST",
        headers: { cookie: sessionCookie, origin: bootstrapURL.origin },
      }),
    );
    expect(sameOriginPost.status).not.toBe(403);

    const missingSession = await fleet.fetch(
      new Request(`${bootstrapURL.origin}/portal/leases/${leaseID}/code/health`),
    );
    expect(missingSession.status).toBe(302);
    expect(missingSession.headers.get("location")).toBe(
      `https://crabbox.test/portal/leases/${leaseID}/code/`,
    );

    const malformedSession = await fleet.fetch(
      new Request(`${bootstrapURL.origin}/portal/leases/${leaseID}/code/health`, {
        headers: { cookie: "crabbox_code_session=%ZZ" },
      }),
    );
    expect(malformedSession.status).toBe(302);
  });

  it("downgrades stale admin grants in durable Code viewer tickets and sessions", async () => {
    const storage = new MemoryStorage();
    const env = {
      CRABBOX_CODE_ORIGIN_TEMPLATE: "https://{lease}.code.example.test",
      CRABBOX_GITHUB_ADMIN_LOGINS: "current-admin",
    };
    const fleet = testFleet(storage, {}, env);
    const leaseID = "cbx_000000000001";
    storage.seed(
      `lease:${leaseID}`,
      testLease({
        id: leaseID,
        slug: "blue-lobster",
        owner: "owner@example.com",
        org: "example-org",
        code: true,
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );
    const isolatedOrigin = await codeOriginForLease(env, leaseID);
    expect(isolatedOrigin).toBeDefined();
    const staleGrant = {
      auth: "github",
      admin: true,
      owner: "former-admin@example.com",
      org: "other-org",
      login: "former-admin",
      portalSessionHash: "a".repeat(64),
      createdAt: new Date().toISOString(),
      expiresAt: new Date(Date.now() + 5 * 60 * 1000).toISOString(),
    } as const;
    const session = `code_session_${"a".repeat(32)}`;
    storage.seed(`code-viewer-session:${session}`, {
      session,
      leaseID,
      ...staleGrant,
    });

    const staleSession = await fleet.fetch(
      new Request(`${isolatedOrigin}/portal/leases/${leaseID}/code/health`, {
        headers: { cookie: `crabbox_code_session=${session}` },
      }),
    );
    expect(staleSession.status).toBe(404);
    expect(storage.value<{ admin: boolean }>(`code-viewer-session:${session}`)?.admin).toBe(false);

    const ticket = `code_view_${"b".repeat(32)}`;
    storage.seed(`code-viewer-ticket:${ticket}`, {
      ticket,
      leaseID,
      returnTo: `/portal/leases/${leaseID}/code/`,
      ...staleGrant,
    });
    const bootstrap = await fleet.fetch(
      codeBootstrapRequest(
        `${isolatedOrigin}/portal/leases/${leaseID}/code/__crabbox_bootstrap`,
        ticket,
      ),
    );
    expect(bootstrap.status).toBe(303);
    const issuedSession = /crabbox_code_session=([^;]+)/.exec(
      bootstrap.headers.get("set-cookie") ?? "",
    )?.[1];
    expect(issuedSession).toMatch(/^code_session_[a-f0-9]{32}$/);
    expect(storage.value<{ admin: boolean }>(`code-viewer-session:${issuedSession}`)?.admin).toBe(
      false,
    );
  });

  it("revokes isolated Code viewer access end to end when the portal session logs out", async () => {
    const storage = new MemoryStorage();
    const env = {
      CRABBOX_CODE_ORIGIN_TEMPLATE: "https://{lease}.code.example.test",
      CRABBOX_PUBLIC_URL: "https://crabbox.test",
      CRABBOX_SESSION_SECRET: "session-secret",
    } as Env;
    const fleet = testFleet(storage, {}, env);
    const leaseID = "cbx_000000000001";
    storage.seed(
      `lease:${leaseID}`,
      testLease({
        id: leaseID,
        slug: "blue-lobster",
        owner: "alice@example.com",
        org: "example-org",
        code: true,
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );
    const token = await issueUserToken(env, {
      owner: "alice@example.com",
      ownerSource: "github-verified-email",
      org: "example-org",
      login: "alice",
      ttlSeconds: 60 * 60,
    });
    const portalCookie = `crabbox_session=${encodeURIComponent(token)}`;
    const throughCoordinator = async (input: Request): Promise<Response> =>
      await routeCoordinatorRequest(input, env, async (prepared) => await fleet.fetch(prepared));
    const codeEntry = "https://crabbox.test/portal/leases/blue-lobster/code/";

    const handoff = await throughCoordinator(
      new Request(codeEntry, { headers: { cookie: portalCookie } }),
    );
    expect(handoff.status).toBe(200);
    const bootstrapHandoff = await readCodeBootstrapHandoff(handoff);
    const bootstrapURL = bootstrapHandoff.url;
    expect(bootstrapURL.pathname).toContain("/__crabbox_bootstrap");
    expect(bootstrapURL.search).toBe("");

    const pendingHandoff = await throughCoordinator(
      new Request(codeEntry, { headers: { cookie: portalCookie } }),
    );
    expect(pendingHandoff.status).toBe(200);
    const pendingBootstrap = await readCodeBootstrapHandoff(pendingHandoff);

    const bootstrap = await throughCoordinator(
      codeBootstrapRequest(bootstrapURL, bootstrapHandoff.ticket),
    );
    expect(bootstrap.status).toBe(303);
    const codeCookie = (bootstrap.headers.get("set-cookie") || "").split(";", 1)[0] || "";
    const isolatedPage = new URL(bootstrap.headers.get("location") || "", bootstrapURL).toString();
    const page = await throughCoordinator(
      new Request(isolatedPage, { headers: { cookie: codeCookie } }),
    );
    expect(page.status).toBe(200);

    const activeViewerID = "active-code-viewer";
    const portalSessionHash = await sha256Hex(token);
    const activeAgent = new FakeWebSocket({ kind: "code-agent", leaseID });
    const activeViewer = new FakeWebSocket({
      kind: "code-viewer",
      leaseID,
      id: activeViewerID,
      auth: "github",
      portalSessionHash,
    });
    const codeBridgeState = fleet as unknown as {
      codeAgents: Map<string, WebSocket>;
      codeViewers: Map<string, WebSocket>;
    };
    codeBridgeState.codeAgents.set(leaseID, activeAgent as unknown as WebSocket);
    codeBridgeState.codeViewers.set(activeViewerID, activeViewer as unknown as WebSocket);

    const expiredRevocationKey = `code-viewer-session-revocation:${"f".repeat(64)}`;
    storage.seed(expiredRevocationKey, {
      portalSessionHash: "f".repeat(64),
      createdAt: new Date(Date.now() - 2_000).toISOString(),
      expiresAt: new Date(Date.now() - 1_000).toISOString(),
    });
    const logout = await throughCoordinator(
      new Request("https://crabbox.test/portal/logout", {
        headers: { cookie: portalCookie },
      }),
    );
    expect(logout.status).toBe(200);
    expect(logout.headers.get("set-cookie")).toContain("crabbox_session=");
    expect(logout.headers.get("set-cookie")).toContain("Max-Age=0");
    expect(storage.value(expiredRevocationKey)).toBeUndefined();
    expect(activeViewer.closeCode).toBe(1008);
    expect(activeViewer.closeReason).toBe("portal session ended");
    expect(activeAgent.sentJSON()).toEqual([
      {
        type: "ws_close",
        id: activeViewerID,
        code: 1008,
        reason: "portal session ended",
      },
    ]);
    await fleet.webSocketMessage(activeViewer as unknown as WebSocket, "post-logout-frame");
    expect(activeAgent.sentJSON()).toHaveLength(1);

    const staleViewer = await throughCoordinator(
      new Request(isolatedPage, { headers: { cookie: codeCookie } }),
    );
    expect(staleViewer.status).toBe(302);
    expect(staleViewer.headers.get("location")).toBe(
      `https://crabbox.test/portal/leases/${leaseID}/code/`,
    );

    const staleBootstrap = await throughCoordinator(
      codeBootstrapRequest(pendingBootstrap.url, pendingBootstrap.ticket),
    );
    expect(staleBootstrap.status).toBe(401);
    await expect(staleBootstrap.json()).resolves.toMatchObject({
      error: "code_viewer_session_revoked",
    });

    const stalePortalSession = await throughCoordinator(
      new Request(codeEntry, { headers: { cookie: portalCookie } }),
    );
    expect(stalePortalSession.status).toBe(401);
    expect(await stalePortalSession.text()).toContain("Log in again to open Code.");

    const suffixedPortalSession = await throughCoordinator(
      new Request(codeEntry, {
        headers: { cookie: `crabbox_session=${encodeURIComponent(`${token}.ignored`)}` },
      }),
    );
    expect(suffixedPortalSession.status).toBe(302);
    expect(suffixedPortalSession.headers.get("location")).toContain("/portal/login?");
    expect(suffixedPortalSession.headers.get("location")).not.toContain("__crabbox_bootstrap");
  });

  it("keeps bridge tickets usable after endpoint binding mismatches", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const lease = testLease({
      id: "cbx_000000000001",
      slug: "bound-lease",
      owner: "owner@example.com",
      org: "example-org",
      desktop: true,
      code: true,
      state: "active",
      expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
    });
    storage.seed(`lease:${lease.id}`, lease);

    const createdAt = new Date().toISOString();
    const expiresAt = new Date(Date.now() + 60_000).toISOString();
    const webVNCTicket = `wvnc_${"a".repeat(32)}`;
    const codeTicket = `code_${"b".repeat(32)}`;
    const egressTicket = `egress_${"c".repeat(32)}`;
    const adapterTicket = `adapter_${"d".repeat(32)}`;
    storage.seed(`webvnc-ticket:${webVNCTicket}`, {
      ticket: webVNCTicket,
      leaseID: lease.id,
      owner: lease.owner,
      org: lease.org,
      createdAt,
      expiresAt,
    });
    storage.seed(`code-ticket:${codeTicket}`, {
      ticket: codeTicket,
      leaseID: lease.id,
      owner: lease.owner,
      org: lease.org,
      createdAt,
      expiresAt,
    });
    storage.seed(`egress-ticket:${egressTicket}`, {
      ticket: egressTicket,
      leaseID: lease.id,
      owner: lease.owner,
      org: lease.org,
      role: "host",
      sessionID: "egress_bound1",
      createdAt,
      expiresAt,
    });
    storage.seed(`runtime-adapter-ticket:${adapterTicket}`, {
      ticket: adapterTicket,
      adapterID: "bound-adapter",
      owner: lease.owner,
      org: lease.org,
      createdAt,
      expiresAt,
    });

    const bridgeRequest = (path: string, ticket: string) =>
      request("GET", path, {
        headers: {
          authorization: `Bearer ${ticket}`,
          upgrade: "websocket",
        },
      });

    const wrongWebVNCLease = await fleet.fetch(
      bridgeRequest("/v1/leases/other-lease/webvnc/agent", webVNCTicket),
    );
    expect(wrongWebVNCLease.status).toBe(404);
    expect(storage.value(`webvnc-ticket:${webVNCTicket}`)).toBeDefined();

    const wrongCodeLease = await fleet.fetch(
      bridgeRequest("/v1/leases/other-lease/code/agent", codeTicket),
    );
    expect(wrongCodeLease.status).toBe(404);
    expect(storage.value(`code-ticket:${codeTicket}`)).toBeDefined();

    const wrongEgressRole = await fleet.fetch(
      bridgeRequest(`/v1/leases/${lease.id}/egress/client`, egressTicket),
    );
    expect(wrongEgressRole.status).toBe(401);
    expect(storage.value(`egress-ticket:${egressTicket}`)).toBeDefined();

    const wrongEgressLease = await fleet.fetch(
      bridgeRequest("/v1/leases/other-lease/egress/host", egressTicket),
    );
    expect(wrongEgressLease.status).toBe(404);
    expect(storage.value(`egress-ticket:${egressTicket}`)).toBeDefined();

    const wrongAdapter = await fleet.fetch(
      bridgeRequest("/v1/adapters/other-adapter/agent", adapterTicket),
    );
    expect(wrongAdapter.status).toBe(401);
    expect(storage.value(`runtime-adapter-ticket:${adapterTicket}`)).toBeDefined();

    const consumers = fleet as unknown as {
      consumeWebVNCTicket(request: Request, identifier: string): Promise<{ status: string }>;
      consumeCodeTicket(request: Request, identifier: string): Promise<{ status: string }>;
      consumeEgressTicket(
        request: Request,
        identifier: string,
        role: "host" | "client",
      ): Promise<{ status: string }>;
      consumeRuntimeAdapterTicket(request: Request, adapterID: string): Promise<{ status: string }>;
    };

    const queryOnlyRequest = (path: string, ticket: string) =>
      request("GET", `${path}?ticket=${ticket}`, {
        headers: {
          upgrade: "websocket",
        },
      });

    await expect(
      consumers.consumeWebVNCTicket(
        queryOnlyRequest(`/v1/leases/${lease.id}/webvnc/agent`, webVNCTicket),
        lease.id,
      ),
    ).resolves.toMatchObject({ status: "invalid" });
    expect(storage.value(`webvnc-ticket:${webVNCTicket}`)).toBeDefined();

    await expect(
      consumers.consumeCodeTicket(
        queryOnlyRequest(`/v1/leases/${lease.id}/code/agent`, codeTicket),
        lease.id,
      ),
    ).resolves.toMatchObject({ status: "invalid" });
    expect(storage.value(`code-ticket:${codeTicket}`)).toBeDefined();

    await expect(
      consumers.consumeEgressTicket(
        queryOnlyRequest(`/v1/leases/${lease.id}/egress/host`, egressTicket),
        lease.id,
        "host",
      ),
    ).resolves.toMatchObject({ status: "invalid" });
    expect(storage.value(`egress-ticket:${egressTicket}`)).toBeDefined();

    await expect(
      consumers.consumeWebVNCTicket(
        bridgeRequest(`/v1/leases/${lease.id}/webvnc/agent`, webVNCTicket),
        lease.id,
      ),
    ).resolves.toMatchObject({ status: "accepted" });
    expect(storage.value(`webvnc-ticket:${webVNCTicket}`)).toBeUndefined();

    await expect(
      consumers.consumeCodeTicket(
        bridgeRequest(`/v1/leases/${lease.id}/code/agent`, codeTicket),
        lease.id,
      ),
    ).resolves.toMatchObject({ status: "accepted" });
    expect(storage.value(`code-ticket:${codeTicket}`)).toBeUndefined();

    await expect(
      consumers.consumeEgressTicket(
        bridgeRequest(`/v1/leases/${lease.id}/egress/host`, egressTicket),
        lease.id,
        "host",
      ),
    ).resolves.toMatchObject({ status: "accepted" });
    expect(storage.value(`egress-ticket:${egressTicket}`)).toBeUndefined();

    await expect(
      consumers.consumeRuntimeAdapterTicket(
        bridgeRequest("/v1/adapters/bound-adapter/agent", adapterTicket),
        "bound-adapter",
      ),
    ).resolves.toMatchObject({ status: "accepted" });
    expect(storage.value(`runtime-adapter-ticket:${adapterTicket}`)).toBeUndefined();
  });

  it("accepts query bridge tickets only when the legacy compatibility flag is enabled", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage, undefined, { CRABBOX_ALLOW_QUERY_BRIDGE_TICKETS: "1" });
    const lease = testLease({
      id: "cbx_000000000001",
      slug: "bound-lease",
      owner: "owner@example.com",
      org: "example-org",
      desktop: true,
      code: true,
      state: "active",
      expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
    });
    storage.seed(`lease:${lease.id}`, lease);

    const createdAt = new Date().toISOString();
    const expiresAt = new Date(Date.now() + 60_000).toISOString();
    const webVNCTicket = `wvnc_${"e".repeat(32)}`;
    const codeTicket = `code_${"f".repeat(32)}`;
    const egressTicket = `egress_${"0".repeat(32)}`;
    storage.seed(`webvnc-ticket:${webVNCTicket}`, {
      ticket: webVNCTicket,
      leaseID: lease.id,
      owner: lease.owner,
      org: lease.org,
      createdAt,
      expiresAt,
    });
    storage.seed(`code-ticket:${codeTicket}`, {
      ticket: codeTicket,
      leaseID: lease.id,
      owner: lease.owner,
      org: lease.org,
      createdAt,
      expiresAt,
    });
    storage.seed(`egress-ticket:${egressTicket}`, {
      ticket: egressTicket,
      leaseID: lease.id,
      owner: lease.owner,
      org: lease.org,
      role: "host",
      sessionID: "egress_bound2",
      createdAt,
      expiresAt,
    });

    const consumers = fleet as unknown as {
      consumeWebVNCTicket(request: Request, identifier: string): Promise<{ status: string }>;
      consumeCodeTicket(request: Request, identifier: string): Promise<{ status: string }>;
      consumeEgressTicket(
        request: Request,
        identifier: string,
        role: "host" | "client",
      ): Promise<{ status: string }>;
    };
    const queryOnlyRequest = (path: string, ticket: string) =>
      request("GET", `${path}?ticket=${ticket}`, {
        headers: {
          upgrade: "websocket",
        },
      });

    await expect(
      consumers.consumeWebVNCTicket(
        queryOnlyRequest(`/v1/leases/${lease.id}/webvnc/agent`, webVNCTicket),
        lease.id,
      ),
    ).resolves.toMatchObject({ status: "accepted" });
    expect(storage.value(`webvnc-ticket:${webVNCTicket}`)).toBeUndefined();

    await expect(
      consumers.consumeCodeTicket(
        queryOnlyRequest(`/v1/leases/${lease.id}/code/agent`, codeTicket),
        lease.id,
      ),
    ).resolves.toMatchObject({ status: "accepted" });
    expect(storage.value(`code-ticket:${codeTicket}`)).toBeUndefined();

    await expect(
      consumers.consumeEgressTicket(
        queryOnlyRequest(`/v1/leases/${lease.id}/egress/host`, egressTicket),
        lease.id,
        "host",
      ),
    ).resolves.toMatchObject({ status: "accepted" });
    expect(storage.value(`egress-ticket:${egressTicket}`)).toBeUndefined();
  });

  it("stops code bridge polling after terminal status responses", async () => {
    const page = await portalCode(
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        code: true,
      }),
    ).text();
    const runtime = await runCodePortalScript(page, {
      ok: false,
      status: 409,
      json: async () => ({ error: "code_unavailable", message: "lease is not active" }),
    });

    expect(runtime.fetches).toEqual([
      "https://example.test/portal/leases/cbx_000000000001/code/health",
    ]);
    expect(runtime.elements["code-status"]?.textContent).toBe("bridge unavailable");
    expect(runtime.elements["code-status"]?.dataset.tone).toBe("bad");
    expect(runtime.elements["code-hint"]?.textContent).toBe("lease is not active");
    expect(runtime.timers).toEqual([]);
  });

  it("prefers dedicated upgrade bridge tickets over bearer credentials", () => {
    const upgradeTicket = `code_${"c".repeat(32)}`;
    const queryTicket = `code_${"a".repeat(32)}`;
    expect(
      bridgeTicketFromRequest(
        request("GET", `/v1/leases/blue-lobster/code/agent?ticket=${queryTicket}`, {
          headers: {
            authorization: `Bearer code_${"b".repeat(32)}`,
            "x-crabbox-bridge-ticket": upgradeTicket,
          },
        }),
      ),
    ).toBe(upgradeTicket);
  });

  it("uses bearer bridge tickets when the dedicated header is absent", () => {
    const headerTicket = `code_${"b".repeat(32)}`;
    expect(
      bridgeTicketFromRequest(
        request("GET", `/v1/leases/blue-lobster/code/agent?ticket=code_${"a".repeat(32)}`, {
          headers: { authorization: `Bearer ${headerTicket}` },
        }),
      ),
    ).toBe(headerTicket);
  });

  it("ignores query-string bridge tickets", () => {
    for (const ticket of [
      `wvnc_${"a".repeat(32)}`,
      `code_${"b".repeat(32)}`,
      `egress_${"c".repeat(32)}`,
    ]) {
      expect(
        bridgeTicketFromRequest(
          request("GET", `/v1/leases/blue-lobster/code/agent?ticket=${ticket}`),
        ),
      ).toBe("");
      expect(
        bridgeTicketFromRequest(
          request("GET", `/v1/leases/blue-lobster/code/agent?ticket=${ticket}`, {
            headers: { authorization: "Bearer edge-identity-token" },
          }),
        ),
      ).toBe("edge-identity-token");
    }
  });

  it("allows query-string bridge tickets only in compatibility mode", () => {
    const ticket = `code_${"a".repeat(32)}`;
    expect(
      bridgeTicketFromRequest(
        request("GET", `/v1/leases/blue-lobster/code/agent?ticket=${ticket}`),
        { CRABBOX_ALLOW_QUERY_BRIDGE_TICKETS: "1" },
      ),
    ).toBe(ticket);
    expect(
      bridgeTicketFromRequest(
        request("GET", `/v1/leases/blue-lobster/code/agent?ticket=${ticket}`),
        { CRABBOX_ALLOW_QUERY_BRIDGE_TICKETS: "0" },
      ),
    ).toBe("");
  });

  it("uses a VS Code-compatible CSP for code proxy responses", () => {
    const headers = codeResponseHeaders(
      {
        "content-security-policy": "default-src 'none'; script-src 'self'",
        "content-length": "123",
        "content-type": "text/html",
        "cache-control": "public, max-age=31536000",
        "service-worker-allowed": "/",
        "set-cookie": "vscode-tkn=remote-token; Domain=example.test; Path=/",
      },
      { cookiePath: "/portal/leases/cbx_000000000001/code/", secure: true },
    );

    const csp = headers.get("content-security-policy") || "";
    expect(csp).toContain("script-src 'self' 'unsafe-inline' 'unsafe-eval' blob:");
    expect(csp).toContain("https://static.cloudflareinsights.com");
    expect(csp).toContain("worker-src 'self' data: blob:");
    expect(headers.get("content-length")).toBeNull();
    expect(headers.get("content-type")).toBe("text/html");
    expect(headers.get("cache-control")).toBe("no-store, no-transform");
    expect(headers.get("service-worker-allowed")).toBeNull();
    expect(headers.get("set-cookie")).toBe(
      "vscode-tkn=remote-token; Path=/portal/leases/cbx_000000000001/code/; HttpOnly; Secure; SameSite=Lax",
    );
    expect(
      codeResponseHeaders({ "set-cookie": "crabbox_session=attacker; Path=/" }).get("set-cookie"),
    ).toBeNull();
  });

  it("scopes the VS Code token cookie to the active ID or slug Code path", () => {
    expect(
      codeResponseCookiePath(
        new Request("https://broker.example.test/portal/leases/blue-lobster/code/api"),
        "cbx_000000000001",
      ),
    ).toBe("/portal/leases/blue-lobster/code/");
    expect(
      codeResponseCookiePath(
        new Request("https://broker.example.test/portal/leases/cbx_000000000001/code"),
        "cbx_000000000001",
      ),
    ).toBe("/portal/leases/cbx_000000000001/code/");
  });

  it("forwards browser headers without Crabbox auth context to code-server", () => {
    const headers = codeForwardHeaders(
      new Headers({
        cookie: "crabbox_session=secret; vscode-tkn=remote-token; other=value",
        origin: "https://broker.example.com",
        "sec-websocket-protocol": "vscode",
        "x-client-trace": "trace-1",
        "x-crabbox-auth": "github",
        "X-Crabbox-Admin": "true",
        "x-crabbox-owner": "alice@example.com",
        "x-crabbox-org": "example-org",
        "x-crabbox-github-login": "alice",
        "x-crabbox-token-expires-at": "2026-07-01T12:00:00Z",
      }),
    );

    expect(headers["cookie"]).toBe("vscode-tkn=remote-token");
    expect(headers["cookie"]).not.toContain("crabbox_session");
    expect(headers.origin).toBe("https://broker.example.com");
    expect(headers["sec-websocket-protocol"]).toBe("vscode");
    expect(headers["x-client-trace"]).toBe("trace-1");
    expect(Object.keys(headers).filter((key) => key.startsWith("x-crabbox-"))).toEqual([]);
  });

  it("strips Crabbox auth context from Code proxy bridge messages", async () => {
    const storage = new MemoryStorage();
    const env = { CRABBOX_CODE_ORIGIN_TEMPLATE: "https://{lease}.code.example.test" };
    const fleet = testFleet(storage, {}, env);
    const leaseID = "cbx_000000000001";
    storage.seed(
      `lease:${leaseID}`,
      testLease({
        id: leaseID,
        slug: "blue-lobster",
        owner: "alice@example.com",
        org: "example-org",
        code: true,
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );
    const agent = new FakeWebSocket({ kind: "code-agent", leaseID });
    const relay = fleet as unknown as { codeAgents: Map<string, WebSocket> };
    relay.codeAgents.set(leaseID, agent as unknown as WebSocket);
    const session = `code_session_${"a".repeat(32)}`;
    storage.seed(`code-viewer-session:${session}`, {
      session,
      leaseID,
      auth: "bearer",
      admin: false,
      owner: "alice@example.com",
      org: "example-org",
      createdAt: new Date().toISOString(),
      expiresAt: new Date(Date.now() + 5 * 60 * 1000).toISOString(),
    });
    const isolatedOrigin = await codeOriginForLease(env, leaseID);
    expect(isolatedOrigin).toBeDefined();
    let forwarded: { id: string; headers: Record<string, string> } | undefined;
    agent.onSend = (data) => {
      forwarded = JSON.parse(data) as { id: string; headers: Record<string, string> };
      void fleet.webSocketMessage(
        agent as unknown as WebSocket,
        JSON.stringify({ type: "http", id: forwarded.id, status: 204 }),
      );
    };

    const response = await fleet.fetch(
      new Request(`${isolatedOrigin}/portal/leases/${leaseID}/code/api/status`, {
        headers: {
          cookie: `crabbox_code_session=${session}`,
          "x-client-trace": "trace-1",
          origin: isolatedOrigin || "",
        },
      }),
    );

    expect(response.status).toBe(204);
    expect(forwarded?.headers).toMatchObject({
      origin: isolatedOrigin,
      "x-client-trace": "trace-1",
    });
    expect(
      Object.keys(forwarded?.headers ?? {}).filter((key) => key.startsWith("x-crabbox-")),
    ).toEqual([]);
  });

  it("rejects bridge tickets whose cached admin grant was revoked", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage, {}, { CRABBOX_GITHUB_ADMIN_LOGINS: "current-admin" });
    const leaseID = "cbx_000000000001";
    storage.seed(
      `lease:${leaseID}`,
      testLease({
        id: leaseID,
        slug: "blue-lobster",
        owner: "owner@example.com",
        org: "example-org",
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );
    const ticket = `egress_${"a".repeat(32)}`;
    storage.seed(`egress-ticket:${ticket}`, {
      ticket,
      leaseID,
      owner: "former-admin@example.com",
      org: "other-org",
      admin: true,
      auth: "github",
      login: "former-admin",
      role: "host",
      sessionID: "egress_revoked_ticket",
      createdAt: new Date().toISOString(),
      expiresAt: new Date(Date.now() + 60_000).toISOString(),
    });

    const response = await fleet.fetch(
      request("GET", "/v1/leases/blue-lobster/egress/host", {
        headers: { authorization: `Bearer ${ticket}`, upgrade: "websocket" },
      }),
    );

    expect(response.status).toBe(401);
    expect(storage.value(`egress-ticket:${ticket}`)).toBeUndefined();
  });

  it("creates scoped egress tickets and reports bridge status", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "alice@example.com",
        org: "example-org",
        share: {
          users: {
            "viewer@example.com": "use",
            "manager@example.com": "manage",
          },
          org: "use",
        },
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );

    const invalidRole = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/egress/ticket", {
        headers,
        body: { role: "viewer" },
      }),
    );
    expect(invalidRole.status).toBe(400);

    const ticket = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/egress/ticket", {
        headers,
        body: {
          role: "host",
          sessionID: "egress_test123",
          profile: "discord",
          allow: ["discord.com", "*.discordcdn.com"],
        },
      }),
    );
    expect(ticket.status).toBe(200);
    const ticketBody = (await ticket.json()) as {
      ticket: string;
      leaseID: string;
      role: string;
      sessionID: string;
    };
    expect(ticketBody.ticket).toMatch(/^egress_[a-f0-9]{32}$/);
    expect(ticketBody.leaseID).toBe("cbx_000000000001");
    expect(ticketBody.role).toBe("host");
    expect(ticketBody.sessionID).toBe("egress_test123");

    const camelTicket = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/egress/ticket", {
        headers,
        body: {
          role: "client",
          sessionId: "egress_camel123",
          allow: ["discord.com"],
        },
      }),
    );
    expect(camelTicket.status).toBe(200);
    await expect(camelTicket.json()).resolves.toMatchObject({
      role: "client",
      sessionID: "egress_camel123",
    });

    const status = await fleet.fetch(
      request("GET", "/v1/leases/blue-lobster/egress/status", { headers }),
    );
    expect(status.status).toBe(200);
    await expect(status.json()).resolves.toMatchObject({
      leaseID: "cbx_000000000001",
      sessionID: "egress_camel123",
      profile: "discord",
      allow: ["discord.com"],
      hostConnected: false,
      clientConnected: false,
    });

    const portalPage = await fleet.fetch(
      request("GET", "/portal/leases/blue-lobster", { headers }),
    );
    expect(portalPage.status).toBe(200);
    const portalBody = await portalPage.text();
    expect(portalBody).toContain("<strong>egress</strong><small>waiting for host</small>");
    expect(portalBody).toContain("discord · discord.com");
    expect(portalBody).toContain("crabbox egress status --id blue-lobster");
    expect(portalBody).toContain("crabbox egress stop --id blue-lobster");

    const viewerHeaders = {
      "x-crabbox-owner": "viewer@example.com",
      "x-crabbox-org": "example-org",
    };
    const viewerStatus = await fleet.fetch(
      request("GET", "/v1/leases/blue-lobster/egress/status", {
        headers: viewerHeaders,
      }),
    );
    expect(viewerStatus.status).toBe(200);
    await expect(viewerStatus.json()).resolves.toMatchObject({
      leaseID: "cbx_000000000001",
      sessionID: "",
      profile: "",
      allow: [],
      hostConnected: false,
      clientConnected: false,
      createdAt: "",
      updatedAt: "",
    });

    const viewerPortal = await fleet.fetch(
      request("GET", "/portal/leases/blue-lobster", { headers: viewerHeaders }),
    );
    expect(viewerPortal.status).toBe(200);
    const viewerPortalBody = await viewerPortal.text();
    expect(viewerPortalBody).toContain("<strong>egress</strong><small>waiting for host</small>");
    expect(viewerPortalBody).toContain('<span class="muted">active</span>');
    expect(viewerPortalBody).not.toContain("discord · discord.com");
    expect(viewerPortalBody).not.toContain("*.discordcdn.com");
    expect(viewerPortalBody).not.toContain("crabbox egress status --id blue-lobster");
    expect(viewerPortalBody).not.toContain("crabbox egress stop --id blue-lobster");

    const managerStatus = await fleet.fetch(
      request("GET", "/v1/leases/blue-lobster/egress/status", {
        headers: {
          "x-crabbox-owner": "manager@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    await expect(managerStatus.json()).resolves.toMatchObject({
      sessionID: "egress_camel123",
      profile: "discord",
      allow: ["discord.com"],
    });

    const missingTicket = await fleet.fetch(
      request("GET", "/v1/leases/blue-lobster/egress/host", {
        headers: { upgrade: "websocket" },
      }),
    );
    expect(missingTicket.status).toBe(401);
  });

  it("keeps egress status on the latest ticketed session", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const headers = {
      "x-crabbox-owner": "peter@example.com",
      "x-crabbox-org": "openclaw",
    };
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "peter@example.com",
        org: "openclaw",
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );

    const staleTicket = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/egress/ticket", {
        headers,
        body: { role: "host", sessionID: "egress_old001", allow: ["example.com"] },
      }),
    );
    expect(staleTicket.status).toBe(200);
    const staleTicketBody = (await staleTicket.json()) as { ticket: string };
    await new Promise((resolve) => setTimeout(resolve, 2));

    const currentTicket = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/egress/ticket", {
        headers,
        body: { role: "client", sessionID: "egress_new001", allow: ["example.com"] },
      }),
    );
    expect(currentTicket.status).toBe(200);

    const status = await fleet.fetch(
      request("GET", "/v1/leases/blue-lobster/egress/status", { headers }),
    );
    expect(status.status).toBe(200);
    await expect(status.json()).resolves.toMatchObject({
      sessionID: "egress_new001",
      hostConnected: false,
      clientConnected: false,
    });
    expect(staleTicketBody.ticket).toMatch(/^egress_[a-f0-9]{32}$/);
  });

  it("does not let an older egress session replace a newer current session", () => {
    expect(
      shouldActivateEgressSession(
        { sessionID: "egress_new", createdAt: "2026-05-07T10:00:00.000Z" },
        "egress_old",
        "2026-05-07T09:59:59.000Z",
      ),
    ).toBe(false);
    expect(
      shouldActivateEgressSession(
        { sessionID: "egress_new", createdAt: "2026-05-07T10:00:00.000Z" },
        "egress_new",
        "2026-05-07T09:59:59.000Z",
      ),
    ).toBe(true);
  });

  it("requires manage access to reset a WebVNC bridge", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "shared-desktop",
        owner: "owner@example.com",
        org: "example-org",
        desktop: true,
        share: {
          users: {
            "viewer@example.com": "use",
            "manager@example.com": "manage",
          },
          org: "use",
        },
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );

    const resetAs = (owner: string, org: string, admin = false) =>
      fleet.fetch(
        request("POST", "/v1/leases/shared-desktop/webvnc/reset", {
          headers: {
            "x-crabbox-owner": owner,
            "x-crabbox-org": org,
            ...(admin ? { "x-crabbox-admin": "true" } : {}),
          },
          body: {},
        }),
      );

    const directUse = await resetAs("viewer@example.com", "example-org");
    expect(directUse.status).toBe(403);
    await expect(directUse.json()).resolves.toEqual({
      error: "forbidden",
      message: "lease manage access required",
    });

    const orgUse = await resetAs("org-viewer@example.com", "example-org");
    expect(orgUse.status).toBe(403);
    await expect(orgUse.json()).resolves.toEqual({
      error: "forbidden",
      message: "lease manage access required",
    });

    const status = await fleet.fetch(
      request("GET", "/v1/leases/shared-desktop/webvnc/status", {
        headers: {
          "x-crabbox-owner": "owner@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    await expect(status.json()).resolves.toMatchObject({ events: [] });

    const manager = await resetAs("manager@example.com", "example-org");
    expect(manager.status).toBe(200);

    const owner = await resetAs("owner@example.com", "example-org");
    expect(owner.status).toBe(200);

    const admin = await resetAs("admin@example.com", "other-org", true);
    expect(admin.status).toBe(200);
  });

  it("serves WebVNC pages only for desktop leases and requires an agent upgrade", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "alice@example.com",
        org: "example-org",
        desktop: true,
        share: {
          users: {
            "friend@example.com": "use",
            "teammate@example.com": "manage",
          },
          org: "use",
        },
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );
    storage.seed(
      "lease:cbx_000000000002",
      testLease({
        id: "cbx_000000000002",
        slug: "plain-lobster",
        owner: "alice@example.com",
        org: "example-org",
        desktop: false,
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );

    const page = await fleet.fetch(request("GET", "/portal/leases/blue-lobster/vnc", { headers }));
    expect(page.status).toBe(200);
    expect(page.headers.get("content-security-policy")).toContain("script-src 'self' 'nonce-");
    const pageBody = await page.text();
    expect(pageBody).toContain(
      "crabbox webvnc --provider hetzner --target linux --id blue-lobster --open",
    );
    expect(pageBody).toContain("/portal/assets/novnc/rfb.js");
    expect(pageBody).toContain("<h1>🦀 crabbox</h1>");
    expect(pageBody).toContain("WebVNC blue-lobster");
    expect(pageBody).toContain("function scheduleRetry");
    expect(pageBody).toContain("/portal/leases/cbx_000000000001/vnc/status");
    expect(pageBody).toContain("/portal/leases/cbx_000000000001/share");
    expect(pageBody).toContain("/portal/leases/cbx_000000000001/share?format=json");
    expect(pageBody).toContain("vnc-share-dialog");
    expect(pageBody).toContain("People with access");
    expect(pageBody).toContain("General access");
    expect(pageBody).toContain("copy WebVNC link");
    expect(pageBody).toContain("saveShare");
    expect(pageBody).toContain("teammate@example.com");
    expect(pageBody).toContain("shareableWebVNCURL");
    expect(pageBody).toContain("/portal/leases/cbx_000000000001/vnc/handoff");
    expect(pageBody).toContain('linkFragment.set("handoff", body.ticket)');
    expect(pageBody).not.toContain('linkFragment.set("username", username)');
    expect(pageBody).not.toContain('linkFragment.set("password", password)');
    expect(pageBody).toContain("await refreshShareState()");
    expect(pageBody).toContain("writeClipboardText(await shareableWebVNCURL())");
    expect(pageBody).toContain('document.getElementById("vnc-share")');
    expect(pageBody).toContain("vnc-copy-remote");
    expect(pageBody).toContain("vnc-paste");
    expect(pageBody).toContain("vnc-copy");
    expect(pageBody).toContain('addEventListener("clipboard"');
    expect(pageBody).toContain("remote clipboard ready");
    expect(pageBody).toContain("clipboardPasteFrom");
    expect(pageBody).toContain("rfb.showDotCursor = true");
    expect(pageBody).toContain('target === "macos"');
    expect(pageBody).toContain("rfb.compressionLevel = 1");
    expect(pageBody).toContain("rfb.qualityLevel = 2");
    expect(pageBody).toContain("rfb.compressionLevel = 0");
    expect(pageBody).toContain("rfb.qualityLevel = 6");
    expect(pageBody).toContain("MetaLeft");
    expect(pageBody).toContain("ControlLeft");
    expect(pageBody).toContain("window.crabboxDialog?.prompt(");
    expect(pageBody).toContain("Clipboard access is unavailable.");
    expect(pageBody).toContain('document.body.dataset.portalDialogOpen === "true"');
    expect(pageBody).not.toContain("window.prompt(");
    expect(pageBody).toContain("position:sticky");
    expect(pageBody).toContain('data-provider="hetzner"');
    expect(pageBody).toContain('data-target="linux"');
    expect(pageBody).toContain("WebVNC daemon not running; run the bridge command below");
    expect(pageBody).toContain("waiting for an available WebVNC observer slot");
    expect(pageBody).toContain("/portal/leases/cbx_000000000001/vnc/control");
    expect(pageBody).toContain("/portal/leases/cbx_000000000001/vnc/theme");
    expect(pageBody).toContain("queueDesktopTheme(event.detail?.mode)");
    expect(pageBody).toContain("body: JSON.stringify({ viewerID, theme })");
    expect(pageBody).toContain("vnc-takeover");
    expect(pageBody).toContain("vnc-control");
    expect(pageBody).toContain("take control");
    expect(pageBody).toContain("you control");
    expect(pageBody).toContain('fragment.get("control") === "take"');
    expect(pageBody).toContain("takeControlIfRequested(state)");
    expect(pageBody).toContain("refreshCollaborationStateAndMaybeTakeControl");
    expect(pageBody).toContain(
      "void refreshCollaborationStateAndMaybeTakeControl().catch(() => {})",
    );
    expect(pageBody).toContain('aria-label="WebVNC display" tabindex="0"');
    expect(pageBody).toContain('screen.addEventListener("contextmenu"');
    expect(pageBody).toContain(
      'screen.addEventListener("pointerdown", (event) => captureVNCInput(event)',
    );
    expect(pageBody).toContain(
      'screen.addEventListener("mousedown", (event) => captureVNCInput(event, { preventDefault: true })',
    );
    expect(pageBody).toContain("rfb?.focus?.({ preventScroll: true })");
    expect(pageBody).toContain("window.setTimeout(focusVNC, 0)");
    expect(pageBody).toContain("rfb.focusOnClick = true");
    expect(pageBody).not.toContain("vnc-role");
    expect(pageBody).not.toContain("status-pill vnc-role");
    expect(pageBody).toContain("rfb.viewOnly = !controlling");
    expect(pageBody).toContain("state?.terminal");
    expect(pageBody).toContain("stopPolling(state.message");
    expect(pageBody).toContain('fragment.get("username")');
    expect(pageBody).toContain('types.includes("username")');
    expect(pageBody).toContain("VNC credentials missing; open WebVNC from crabbox webvnc status");
    expect(pageBody).toContain(
      "VNC authentication failed; reopen WebVNC from crabbox webvnc status",
    );
    expect(pageBody).toContain(
      "VNC authentication timed out; reopen WebVNC from crabbox webvnc status",
    );
    expect(pageBody).toContain("credentialsSent = true");
    expect(pageBody).not.toContain('window.prompt("VNC username")');
    expect(pageBody).not.toContain('window.prompt("VNC password")');
    expect(pageBody).not.toContain("cdn.jsdelivr.net");

    const friendPage = await fleet.fetch(
      request("GET", "/portal/leases/blue-lobster/vnc", {
        headers: {
          "x-crabbox-owner": "friend@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    expect(friendPage.status).toBe(200);
    const friendPageBody = await friendPage.text();
    expect(friendPageBody).not.toContain(
      "crabbox webvnc --provider hetzner --target linux --id blue-lobster --open",
    );
    expect(friendPageBody).not.toContain('id="vnc-bridge-cmd"');
    expect(friendPageBody).not.toContain('id="vnc-copy"');
    expect(friendPageBody).not.toContain('<button id="vnc-share"');
    expect(friendPageBody).not.toContain('<dialog id="vnc-share-dialog"');
    expect(friendPageBody).not.toContain("People with access");
    expect(friendPageBody).not.toContain("copy WebVNC link");
    expect(friendPageBody).not.toContain("friend@example.com");
    expect(friendPageBody).not.toContain("teammate@example.com");
    expect(friendPageBody).not.toContain('"org":"use"');

    const managerPage = await fleet.fetch(
      request("GET", "/portal/leases/blue-lobster/vnc", {
        headers: {
          "x-crabbox-owner": "teammate@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    expect(managerPage.status).toBe(200);
    expect(await managerPage.text()).toContain(
      "crabbox webvnc --provider hetzner --target linux --id blue-lobster --open",
    );

    const friendStatus = await fleet.fetch(
      request("GET", "/portal/leases/blue-lobster/vnc/status", {
        headers: {
          "x-crabbox-owner": "friend@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    expect(friendStatus.status).toBe(200);
    await expect(friendStatus.json()).resolves.toMatchObject({
      leaseID: "cbx_000000000001",
      command: "",
      message: "WebVNC daemon not running; ask a lease manager to start or refresh the bridge",
    });

    const friendViewer = await fleet.fetch(
      request("GET", "/portal/leases/blue-lobster/vnc/viewer", {
        headers: {
          "x-crabbox-owner": "friend@example.com",
          "x-crabbox-org": "example-org",
          upgrade: "websocket",
        },
      }),
    );
    expect(friendViewer.status).toBe(409);
    await expect(friendViewer.json()).resolves.toEqual({
      error: "webvnc_bridge_missing",
      message:
        "No WebVNC backend is available yet; ask a lease manager to start or refresh the bridge.",
      command: "",
    });

    const status = await fleet.fetch(
      request("GET", "/portal/leases/blue-lobster/vnc/status", { headers }),
    );
    expect(status.status).toBe(200);
    await expect(status.json()).resolves.toMatchObject({
      leaseID: "cbx_000000000001",
      slug: "blue-lobster",
      bridgeConnected: false,
      viewerConnected: false,
      command: "crabbox webvnc --provider hetzner --target linux --id blue-lobster --open",
      events: [],
      message:
        "WebVNC daemon not running; run: crabbox webvnc --provider hetzner --target linux --id blue-lobster --open",
    });

    const apiStatus = await fleet.fetch(
      request("GET", "/v1/leases/blue-lobster/webvnc/status", { headers }),
    );
    expect(apiStatus.status).toBe(200);
    await expect(apiStatus.json()).resolves.toMatchObject({
      leaseID: "cbx_000000000001",
      bridgeConnected: false,
      viewerConnected: false,
      events: [],
    });

    const issuedHandoff = await fleet.fetch(
      request("POST", "/portal/leases/blue-lobster/vnc/handoff", {
        headers,
        body: { username: "vnc-user", password: "generated-vnc-password" },
      }),
    );
    expect(issuedHandoff.status).toBe(200);
    expect(issuedHandoff.headers.get("cache-control")).toBe("no-store");
    const issuedHandoffBody = (await issuedHandoff.json()) as {
      ticket: string;
      expiresAt: string;
    };
    expect(issuedHandoffBody.ticket).toMatch(/^vnc_handoff_[a-f0-9]{32}$/);
    expect(storage.alarm()).toBe(Date.parse(issuedHandoffBody.expiresAt));
    expect(issuedHandoffBody).not.toHaveProperty("username");
    expect(issuedHandoffBody).not.toHaveProperty("password");

    const friendCannotIssue = await fleet.fetch(
      request("POST", "/portal/leases/blue-lobster/vnc/handoff", {
        headers: {
          "x-crabbox-owner": "friend@example.com",
          "x-crabbox-org": "example-org",
        },
        body: { username: "vnc-user", password: "generated-vnc-password" },
      }),
    );
    expect(friendCannotIssue.status).toBe(403);

    const outsiderCannotRedeem = await fleet.fetch(
      request("POST", "/portal/leases/blue-lobster/vnc/handoff", {
        headers: {
          "x-crabbox-owner": "outsider@example.com",
          "x-crabbox-org": "other-org",
        },
        body: { ticket: issuedHandoffBody.ticket },
      }),
    );
    expect(outsiderCannotRedeem.status).toBe(404);

    const redeemedHandoff = await fleet.fetch(
      request("POST", "/portal/leases/blue-lobster/vnc/handoff", {
        headers: {
          "x-crabbox-owner": "friend@example.com",
          "x-crabbox-org": "example-org",
        },
        body: { ticket: issuedHandoffBody.ticket },
      }),
    );
    expect(redeemedHandoff.status).toBe(200);
    expect(redeemedHandoff.headers.get("cache-control")).toBe("no-store");
    await expect(redeemedHandoff.json()).resolves.toEqual({
      username: "vnc-user",
      password: "generated-vnc-password",
    });

    const replayedHandoff = await fleet.fetch(
      request("POST", "/portal/leases/blue-lobster/vnc/handoff", {
        headers: {
          "x-crabbox-owner": "friend@example.com",
          "x-crabbox-org": "example-org",
        },
        body: { ticket: issuedHandoffBody.ticket },
      }),
    );
    expect(replayedHandoff.status).toBe(401);

    const invalidTheme = await fleet.fetch(
      request("POST", "/portal/leases/blue-lobster/vnc/theme", {
        headers,
        body: { theme: "sepia" },
      }),
    );
    expect(invalidTheme.status).toBe(400);

    const missingThemeBridge = await fleet.fetch(
      request("POST", "/portal/leases/blue-lobster/vnc/theme", {
        headers,
        body: { theme: "light", viewerID: "viewer_missing" },
      }),
    );
    expect(missingThemeBridge.status).toBe(409);

    const reset = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/webvnc/reset", { headers, body: {} }),
    );
    expect(reset.status).toBe(200);
    await expect(reset.json()).resolves.toMatchObject({
      leaseID: "cbx_000000000001",
      bridgeWasConnected: false,
      viewerWasConnected: false,
      command: "crabbox webvnc --provider hetzner --target linux --id blue-lobster --open",
      events: [{ event: "reset", reason: "WebVNC reset requested" }],
    });

    const agentFrames: string[] = [];
    const agentSocket = {
      readyState: WebSocket.OPEN,
      send(data: string) {
        agentFrames.push(data);
      },
      close() {},
    } as WebSocket;
    const viewerSocket = { readyState: WebSocket.OPEN, close() {} } as WebSocket;
    const observerSocket = { readyState: WebSocket.OPEN, close() {} } as WebSocket;
    const webVNCState = fleet as unknown as {
      webVNCAgents: Map<string, Map<string, WebSocket>>;
      webVNCAgentCapabilities: Map<string, Map<string, Set<string>>>;
      webVNCControllers: Map<string, string>;
      webVNCViewers: Map<
        string,
        Map<
          string,
          {
            id: string;
            agentID: string;
            socket: WebSocket;
            owner: string;
            label: string;
            connectedAt: string;
          }
        >
      >;
    };
    webVNCState.webVNCAgents.set("cbx_000000000001", new Map([["agent_theme1", agentSocket]]));
    webVNCState.webVNCViewers.set(
      "cbx_000000000001",
      new Map([
        [
          "viewer_theme1",
          {
            id: "viewer_theme1",
            agentID: "agent_theme1",
            socket: viewerSocket,
            owner: "alice@example.com",
            label: "alice@example.com",
            connectedAt: new Date().toISOString(),
          },
        ],
        [
          "viewer_observer1",
          {
            id: "viewer_observer1",
            agentID: "agent_theme1",
            socket: observerSocket,
            owner: "observer@example.com",
            label: "observer@example.com",
            connectedAt: new Date().toISOString(),
          },
        ],
      ]),
    );
    webVNCState.webVNCControllers.set("cbx_000000000001", "viewer_theme1");
    const oldBridgeTheme = await fleet.fetch(
      request("POST", "/portal/leases/blue-lobster/vnc/theme", {
        headers,
        body: { theme: "dark", viewerID: "viewer_theme1" },
      }),
    );
    expect(oldBridgeTheme.status).toBe(409);
    await expect(oldBridgeTheme.json()).resolves.toMatchObject({
      error: "webvnc_bridge_upgrade_required",
    });
    webVNCState.webVNCAgentCapabilities.set(
      "cbx_000000000001",
      new Map([["agent_theme1", new Set(["desktop_theme"])]]),
    );
    const observerTheme = await fleet.fetch(
      request("POST", "/portal/leases/blue-lobster/vnc/theme", {
        headers: { "x-crabbox-owner": "friend@example.com", "x-crabbox-org": "example-org" },
        body: { theme: "dark", viewerID: "viewer_observer1" },
      }),
    );
    expect(observerTheme.status).toBe(403);

    const theme = await fleet.fetch(
      request("POST", "/portal/leases/blue-lobster/vnc/theme", {
        headers,
        body: { theme: "dark", viewerID: "viewer_theme1" },
      }),
    );
    expect(theme.status).toBe(200);
    await expect(theme.json()).resolves.toMatchObject({
      leaseID: "cbx_000000000001",
      theme: "dark",
    });
    expect(agentFrames).toEqual([JSON.stringify({ type: "desktop_theme", theme: "dark" })]);

    const plain = await fleet.fetch(
      request("GET", "/portal/leases/plain-lobster/vnc", { headers }),
    );
    expect(plain.status).toBe(409);

    const ticket = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/webvnc/ticket", { headers, body: {} }),
    );
    expect(ticket.status).toBe(200);
    const ticketBody = (await ticket.json()) as { ticket: string; leaseID: string };
    expect(ticketBody.ticket).toMatch(/^wvnc_[a-f0-9]{32}$/);
    expect(ticketBody.leaseID).toBe("cbx_000000000001");

    const agent = await fleet.fetch(
      request("GET", "/v1/leases/blue-lobster/webvnc/agent", { headers }),
    );
    expect(agent.status).toBe(426);

    const missingTicket = await fleet.fetch(
      request("GET", "/v1/leases/blue-lobster/webvnc/agent", {
        headers: { upgrade: "websocket" },
      }),
    );
    expect(missingTicket.status).toBe(401);
  });

  it("buffers initial WebVNC bridge bytes until the viewer attaches", async () => {
    const buffers = new Map<string, WebVNCBuffer>();
    const sent: Array<string | ArrayBuffer> = [];
    const viewer = {
      readyState: WebSocket.OPEN,
      send(data: string | ArrayBuffer) {
        sent.push(data);
      },
    } as WebSocket;

    await forwardOrBufferWebVNC("RFB 003.008\n", undefined, buffers, "cbx_000000000001");
    expect(sent).toEqual([]);
    expect(buffers.get("cbx_000000000001")).toMatchObject({
      chunks: ["RFB 003.008\n"],
      bytes: 12,
    });

    flushPendingWebVNC(buffers, "cbx_000000000001", viewer);
    expect(sent).toEqual(["RFB 003.008\n"]);
    expect(buffers.has("cbx_000000000001")).toBe(false);
  });

  it("converts WebVNC Blob frames before forwarding", async () => {
    const buffers = new Map<string, WebVNCBuffer>();
    const sent: Array<string | ArrayBuffer> = [];
    const viewer = {
      readyState: WebSocket.OPEN,
      send(data: string | ArrayBuffer) {
        sent.push(data);
      },
    } as WebSocket;

    await forwardOrBufferWebVNC(new Blob(["RFB 003.008\n"]), viewer, buffers, "cbx_000000000001");

    expect(sent).toHaveLength(1);
    expect(new TextDecoder().decode(sent[0] as ArrayBuffer)).toBe("RFB 003.008\n");
    expect(buffers.has("cbx_000000000001")).toBe(false);
  });

  it("resets the WebVNC bridge when the viewer goes away", () => {
    const buffers = new Map<string, WebVNCBuffer>();
    buffers.set("cbx_000000000001", { chunks: ["RFB 003.008\n"], bytes: 12 });
    buffers.set("cbx_000000000001:agent_a", { chunks: ["RFB 003.008\n"], bytes: 12 });
    const closed: Array<{ code: number; reason: string }> = [];
    const agents = new Map<string, Map<string, WebSocket>>();
    agents.set(
      "cbx_000000000001",
      new Map([
        [
          "agent_a",
          {
            readyState: WebSocket.OPEN,
            close(code: number, reason: string) {
              closed.push({ code, reason });
            },
          } as WebSocket,
        ],
      ]),
    );

    resetWebVNCBridge(agents, buffers, "cbx_000000000001", 1011, "WebVNC viewer disconnected");

    expect(closed).toEqual([{ code: 1011, reason: "WebVNC viewer disconnected" }]);
    expect(agents.has("cbx_000000000001")).toBe(false);
    expect(buffers.has("cbx_000000000001")).toBe(false);
    expect(buffers.has("cbx_000000000001:agent_a")).toBe(false);
  });

  it("keeps pool inventory admin-only", async () => {
    const fleet = testFleet(new MemoryStorage(), {
      aws: fakeProvider(),
      hetzner: fakeProvider(),
    });
    const denied = await fleet.fetch(
      request("GET", "/v1/pool", {
        headers: {
          "x-crabbox-owner": "friend@example.com",
          "x-crabbox-org": "openclaw",
        },
      }),
    );
    expect(denied.status).toBe(403);

    const allowed = await fleet.fetch(
      request("GET", "/v1/pool", {
        headers: { "x-crabbox-admin": "true" },
      }),
    );
    expect(allowed.status).toBe(200);
  });

  it("lists AWS EC2 Mac Dedicated Hosts through an admin route", async () => {
    const fetchMock = vi.fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>(
      async (input, init) => {
        const params = new URLSearchParams(await requestBodyForTest(input, init));
        expect(params.get("Action")).toBe("DescribeHosts");
        expect(params.get("Filter.1.Name")).toBe("instance-type");
        expect(params.get("Filter.1.Value.1")).toBe("mac2.metal");
        expect(params.get("Filter.2.Name")).toBe("state");
        expect(params.get("Filter.2.Value.1")).toBe("available");
        return ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
        <DescribeHostsResponse>
          <hostSet>
            <item>
              <hostId>h-000000000001</hostId>
              <state>available</state>
              <availabilityZone>eu-west-1a</availabilityZone>
              <autoPlacement>off</autoPlacement>
              <allocationTime>2026-05-15T00:00:00Z</allocationTime>
              <hostProperties><instanceType>mac2.metal</instanceType></hostProperties>
              <tagSet><item><key>crabbox</key><value>true</value></item></tagSet>
            </item>
          </hostSet>
        </DescribeHostsResponse>`);
      },
    );
    vi.stubGlobal("fetch", fetchMock);
    const fleet = testFleet(
      new MemoryStorage(),
      {},
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "test",
        CRABBOX_AWS_REGION: "eu-west-1",
      },
    );

    const response = await fleet.fetch(
      request(
        "GET",
        "/v1/admin/hosts?provider=aws&target=macos&region=eu-west-1&type=mac2.metal&state=available",
        {
          headers: { "x-crabbox-admin": "true" },
        },
      ),
    );

    expect(response.status).toBe(200);
    await expect(response.json()).resolves.toMatchObject({
      hosts: [
        {
          id: "h-000000000001",
          state: "available",
          region: "eu-west-1",
          availabilityZone: "eu-west-1a",
          instanceType: "mac2.metal",
          autoPlacement: "off",
          tags: { crabbox: "true" },
        },
      ],
    });
  });

  it("rejects unsupported provider-neutral host scopes", async () => {
    const fleet = testFleet(new MemoryStorage(), {});

    const response = await fleet.fetch(
      request("GET", "/v1/admin/hosts?provider=azure&target=macos", {
        headers: { "x-crabbox-admin": "true" },
      }),
    );

    expect(response.status).toBe(400);
    await expect(response.json()).resolves.toMatchObject({
      error: "unsupported_host_scope",
    });
  });

  it("lists AWS EC2 Mac host offerings through an admin route", async () => {
    const fetchMock = vi.fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>(
      async (input, init) => {
        const params = new URLSearchParams(await requestBodyForTest(input, init));
        expect(params.get("Action")).toBe("DescribeInstanceTypeOfferings");
        expect(params.get("LocationType")).toBe("availability-zone");
        expect(params.get("Filter.1.Name")).toBe("instance-type");
        expect(params.get("Filter.1.Value.1")).toBe("mac2.metal");
        return ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
        <DescribeInstanceTypeOfferingsResponse>
          <instanceTypeOfferingSet>
            <item>
              <instanceType>mac2.metal</instanceType>
              <location>eu-west-1b</location>
              <locationType>availability-zone</locationType>
            </item>
            <item>
              <instanceType>mac2.metal</instanceType>
              <location>eu-west-1a</location>
              <locationType>availability-zone</locationType>
            </item>
          </instanceTypeOfferingSet>
        </DescribeInstanceTypeOfferingsResponse>`);
      },
    );
    vi.stubGlobal("fetch", fetchMock);
    const fleet = testFleet(
      new MemoryStorage(),
      {},
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "test",
        CRABBOX_AWS_REGION: "eu-west-1",
      },
    );

    const response = await fleet.fetch(
      request("GET", "/v1/admin/mac-hosts/offerings?region=eu-west-1&type=mac2.metal", {
        headers: { "x-crabbox-admin": "true" },
      }),
    );

    expect(response.status).toBe(200);
    await expect(response.json()).resolves.toMatchObject({
      offerings: [
        { region: "eu-west-1", availabilityZone: "eu-west-1a", instanceType: "mac2.metal" },
        { region: "eu-west-1", availabilityZone: "eu-west-1b", instanceType: "mac2.metal" },
      ],
    });
  });

  it("lists AWS EC2 Mac Dedicated Host service quotas through an admin route", async () => {
    const fetchMock = vi.fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>(
      async (input, init) => {
        const awsRequest = input instanceof Request ? input : new Request(input, init);
        expect(new URL(awsRequest.url).hostname).toBe("servicequotas.eu-west-1.amazonaws.com");
        expect(awsRequest.headers.get("x-amz-target")).toBe(
          "ServiceQuotasV20190624.GetServiceQuota",
        );
        const body = JSON.parse(await requestBodyForTest(input, init));
        expect(body).toMatchObject({ ServiceCode: "ec2", QuotaCode: "L-5D8DADF5" });
        return new Response(
          JSON.stringify({
            Quota: {
              ServiceCode: "ec2",
              QuotaCode: "L-5D8DADF5",
              QuotaName: "Running Dedicated mac2 Hosts",
              Value: 1,
              Adjustable: true,
              GlobalQuota: false,
              Unit: "None",
            },
          }),
          { headers: { "content-type": "application/json" } },
        );
      },
    );
    vi.stubGlobal("fetch", fetchMock);
    const fleet = testFleet(
      new MemoryStorage(),
      {},
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "test",
        CRABBOX_AWS_REGION: "eu-west-1",
      },
    );

    const response = await fleet.fetch(
      request("GET", "/v1/admin/mac-hosts/quota?region=eu-west-1&type=mac2.metal", {
        headers: { "x-crabbox-admin": "true" },
      }),
    );

    expect(response.status).toBe(200);
    await expect(response.json()).resolves.toMatchObject({
      region: "eu-west-1",
      type: "mac2.metal",
      quotas: [
        {
          serviceCode: "ec2",
          quotaCode: "L-5D8DADF5",
          quotaName: "Running Dedicated mac2 Hosts",
          value: 1,
          adjustable: true,
          globalQuota: false,
          unit: "None",
        },
      ],
    });
  });

  it("falls back to listing EC2 service quotas for unknown Mac host families", async () => {
    const fetchMock = vi.fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>(
      async (input, init) => {
        const awsRequest = input instanceof Request ? input : new Request(input, init);
        expect(new URL(awsRequest.url).hostname).toBe("servicequotas.eu-west-1.amazonaws.com");
        expect(awsRequest.headers.get("x-amz-target")).toBe(
          "ServiceQuotasV20190624.ListServiceQuotas",
        );
        const body = JSON.parse(await requestBodyForTest(input, init));
        expect(body).toMatchObject({ ServiceCode: "ec2", MaxResults: 100 });
        return new Response(
          JSON.stringify({
            Quotas: [
              {
                ServiceCode: "ec2",
                QuotaCode: "L-MAC-NEXT",
                QuotaName: "Running Dedicated mac-next Hosts",
                Value: 2,
                Adjustable: true,
                GlobalQuota: false,
                Unit: "None",
              },
            ],
          }),
          { headers: { "content-type": "application/json" } },
        );
      },
    );
    vi.stubGlobal("fetch", fetchMock);
    const fleet = testFleet(
      new MemoryStorage(),
      {},
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "test",
        CRABBOX_AWS_REGION: "eu-west-1",
      },
    );

    const response = await fleet.fetch(
      request("GET", "/v1/admin/mac-hosts/quota?region=eu-west-1&type=mac-next.metal", {
        headers: { "x-crabbox-admin": "true" },
      }),
    );

    expect(response.status).toBe(200);
    await expect(response.json()).resolves.toMatchObject({
      region: "eu-west-1",
      type: "mac-next.metal",
      quotas: [
        {
          quotaCode: "L-MAC-NEXT",
          quotaName: "Running Dedicated mac-next Hosts",
          value: 2,
        },
      ],
    });
  });

  it("reports no Mac host quota when a known quota code is absent in the region", async () => {
    const fetchMock = vi.fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>(
      async (input, init) => {
        const awsRequest = input instanceof Request ? input : new Request(input, init);
        expect(new URL(awsRequest.url).hostname).toBe("servicequotas.eu-central-1.amazonaws.com");
        expect(awsRequest.headers.get("x-amz-target")).toBe(
          "ServiceQuotasV20190624.GetServiceQuota",
        );
        return new Response(
          JSON.stringify({
            __type: "NoSuchResourceException",
            Message: "The request failed because the specified quota and service do not exist.",
          }),
          { status: 400, headers: { "content-type": "application/json" } },
        );
      },
    );
    vi.stubGlobal("fetch", fetchMock);
    const fleet = testFleet(
      new MemoryStorage(),
      {},
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "test",
        CRABBOX_AWS_REGION: "eu-west-1",
      },
    );

    const response = await fleet.fetch(
      request("GET", "/v1/admin/mac-hosts/quota?region=eu-central-1&type=mac2.metal", {
        headers: { "x-crabbox-admin": "true" },
      }),
    );

    expect(response.status).toBe(200);
    expect(fetchMock).toHaveBeenCalledOnce();
    await expect(response.json()).resolves.toMatchObject({
      region: "eu-central-1",
      type: "mac2.metal",
      quotas: [],
    });
  });

  it("reports the coordinator AWS identity through an admin route", async () => {
    const fetchMock = vi.fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>(
      async (input, init) => {
        const url = input instanceof Request ? input.url : String(input);
        expect(url).toContain("sts.eu-west-1.amazonaws.com");
        const params = new URLSearchParams(await requestBodyForTest(input, init));
        expect(params.get("Action")).toBe("GetCallerIdentity");
        return ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
        <GetCallerIdentityResponse>
          <GetCallerIdentityResult>
            <Arn>arn:aws:iam::123456789012:user/crabbox</Arn>
            <UserId>AIDAEXAMPLE</UserId>
            <Account>123456789012</Account>
          </GetCallerIdentityResult>
        </GetCallerIdentityResponse>`);
      },
    );
    vi.stubGlobal("fetch", fetchMock);
    const fleet = testFleet(
      new MemoryStorage(),
      {},
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "test",
        CRABBOX_AWS_REGION: "eu-west-1",
      },
    );

    const response = await fleet.fetch(
      request("GET", "/v1/admin/providers/identity?provider=aws&region=eu-west-1", {
        headers: { "x-crabbox-admin": "true" },
      }),
    );

    expect(response.status).toBe(200);
    await expect(response.json()).resolves.toMatchObject({
      identity: {
        account: "123456789012",
        arn: "arn:aws:iam::123456789012:user/crabbox",
        userId: "AIDAEXAMPLE",
        region: "eu-west-1",
        policyTarget: {
          type: "user",
          name: "crabbox",
          source: "iam-user",
        },
      },
    });
  });

  it("reports the policy target for an assumed AWS role identity", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
        <GetCallerIdentityResponse>
          <GetCallerIdentityResult>
            <Arn>arn:aws:sts::123456789012:assumed-role/crabbox-worker/session-name</Arn>
            <UserId>AROAEXAMPLE:session-name</UserId>
            <Account>123456789012</Account>
          </GetCallerIdentityResult>
        </GetCallerIdentityResponse>`),
      ),
    );
    const fleet = testFleet(
      new MemoryStorage(),
      {},
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "test",
        CRABBOX_AWS_REGION: "eu-west-1",
      },
    );

    const response = await fleet.fetch(
      request("GET", "/v1/admin/aws-identity?region=eu-west-1", {
        headers: { "x-crabbox-admin": "true" },
      }),
    );

    expect(response.status).toBe(200);
    await expect(response.json()).resolves.toMatchObject({
      identity: {
        arn: "arn:aws:sts::123456789012:assumed-role/crabbox-worker/session-name",
        policyTarget: {
          type: "role",
          name: "crabbox-worker",
          source: "assumed-role",
        },
      },
    });
  });

  it("allocates AWS EC2 Mac Dedicated Hosts through an admin route", async () => {
    const actions: string[] = [];
    const seenParams: Record<string, string>[] = [];
    const fetchMock = vi.fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>(
      async (input, init) => {
        const params = new URLSearchParams(await requestBodyForTest(input, init));
        const action = params.get("Action") ?? "";
        actions.push(action);
        seenParams.push(Object.fromEntries(params));
        if (action === "AllocateHosts") {
          return ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
          <AllocateHostsResponse>
            <hostIdSet><item><hostId>h-000000000001</hostId></item></hostIdSet>
          </AllocateHostsResponse>`);
        }
        if (action === "DescribeHosts") {
          return ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
          <DescribeHostsResponse>
            <hostSet>
              <item>
                <hostId>h-000000000001</hostId>
                <hostState>available</hostState>
                <availabilityZone>eu-west-1a</availabilityZone>
                <autoPlacement>off</autoPlacement>
                <hostProperties><instanceType>mac1.metal</instanceType></hostProperties>
              </item>
            </hostSet>
          </DescribeHostsResponse>`);
        }
        return ec2XMLResponse("<ErrorResponse />", 500);
      },
    );
    vi.stubGlobal("fetch", fetchMock);
    const fleet = testFleet(
      new MemoryStorage(),
      {},
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "test",
        CRABBOX_AWS_REGION: "eu-west-1",
      },
    );

    const response = await fleet.fetch(
      request("POST", "/v1/admin/mac-hosts?region=eu-west-1", {
        headers: { "x-crabbox-admin": "true" },
        body: { type: "mac1.metal", availabilityZone: "eu-west-1a" },
      }),
    );

    expect(response.status).toBe(201);
    expect(actions).toEqual(["AllocateHosts", "DescribeHosts"]);
    expect(seenParams[0]).toMatchObject({
      Action: "AllocateHosts",
      AutoPlacement: "off",
      AvailabilityZone: "eu-west-1a",
      InstanceType: "mac1.metal",
      Quantity: "1",
    });
    expect(seenParams[1]).toMatchObject({
      Action: "DescribeHosts",
      "HostId.1": "h-000000000001",
    });
    await expect(response.json()).resolves.toMatchObject({
      hosts: [{ id: "h-000000000001", instanceType: "mac1.metal" }],
    });
  });

  it("dry-runs AWS EC2 Mac host allocation through an admin route", async () => {
    const actions: string[] = [];
    const seenParams: Record<string, string>[] = [];
    const fetchMock = vi.fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>(
      async (input, init) => {
        const params = new URLSearchParams(await requestBodyForTest(input, init));
        const action = params.get("Action") ?? "";
        actions.push(action);
        seenParams.push(Object.fromEntries(params));
        if (action === "DescribeInstanceTypeOfferings") {
          return ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
          <DescribeInstanceTypeOfferingsResponse>
            <instanceTypeOfferingSet>
              <item>
                <instanceType>mac2.metal</instanceType>
                <location>eu-west-1a</location>
                <locationType>availability-zone</locationType>
              </item>
            </instanceTypeOfferingSet>
          </DescribeInstanceTypeOfferingsResponse>`);
        }
        if (action === "AllocateHosts") {
          return ec2XMLResponse(
            `<?xml version="1.0" encoding="UTF-8"?>
          <Response>
            <Errors>
              <Error>
                <Code>DryRunOperation</Code>
                <Message>Request would have succeeded, but DryRun flag is set.</Message>
              </Error>
            </Errors>
          </Response>`,
            412,
          );
        }
        return ec2XMLResponse("<ErrorResponse />", 500);
      },
    );
    vi.stubGlobal("fetch", fetchMock);
    const fleet = testFleet(
      new MemoryStorage(),
      {},
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "test",
        CRABBOX_AWS_REGION: "eu-west-1",
      },
    );

    const response = await fleet.fetch(
      request("POST", "/v1/admin/mac-hosts/dry-run?region=eu-west-1", {
        headers: { "x-crabbox-admin": "true" },
        body: { type: "mac2.metal" },
      }),
    );

    expect(response.status).toBe(200);
    expect(actions).toEqual(["DescribeInstanceTypeOfferings", "AllocateHosts"]);
    expect(seenParams[1]).toMatchObject({
      Action: "AllocateHosts",
      AvailabilityZone: "eu-west-1a",
      DryRun: "true",
      InstanceType: "mac2.metal",
    });
    await expect(response.json()).resolves.toMatchObject({
      dryRun: true,
      checks: [
        {
          availabilityZone: "eu-west-1a",
          ok: true,
          instanceType: "mac2.metal",
          message: "DryRunOperation: request would have succeeded",
        },
      ],
    });
  });

  it("redacts AWS EC2 Mac host dry-run authorization failures", async () => {
    const fetchMock = vi.fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>(
      async (input, init) => {
        const params = new URLSearchParams(await requestBodyForTest(input, init));
        const action = params.get("Action") ?? "";
        if (action === "DescribeInstanceTypeOfferings") {
          return ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
          <DescribeInstanceTypeOfferingsResponse>
            <instanceTypeOfferingSet>
              <item>
                <instanceType>mac2.metal</instanceType>
                <location>eu-west-1a</location>
                <locationType>availability-zone</locationType>
              </item>
            </instanceTypeOfferingSet>
          </DescribeInstanceTypeOfferingsResponse>`);
        }
        if (action === "AllocateHosts") {
          return ec2XMLResponse(
            `<?xml version="1.0" encoding="UTF-8"?>
          <Response>
            <Errors>
              <Error>
                <Code>UnauthorizedOperation</Code>
                <Message>User: arn:aws:iam::123456789012:user/example is not authorized. Encoded authorization failure message: secret</Message>
              </Error>
            </Errors>
          </Response>`,
            403,
          );
        }
        return ec2XMLResponse("<ErrorResponse />", 500);
      },
    );
    vi.stubGlobal("fetch", fetchMock);
    const fleet = testFleet(
      new MemoryStorage(),
      {},
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "test",
        CRABBOX_AWS_REGION: "eu-west-1",
      },
    );

    const response = await fleet.fetch(
      request("POST", "/v1/admin/mac-hosts/dry-run?region=eu-west-1", {
        headers: { "x-crabbox-admin": "true" },
        body: { type: "mac2.metal" },
      }),
    );

    expect(response.status).toBe(200);
    const body = await response.json();
    expect(body.checks).toMatchObject([
      {
        availabilityZone: "eu-west-1a",
        ok: false,
        instanceType: "mac2.metal",
        message:
          "UnauthorizedOperation: coordinator AWS identity needs EC2 Mac host lifecycle permissions, including ec2:AllocateHosts and ec2:CreateTags",
      },
    ]);
    expect(JSON.stringify(body)).not.toContain("123456789012");
    expect(JSON.stringify(body)).not.toContain("Encoded authorization");
  });

  it("discovers an availability zone before allocating an AWS EC2 Mac Dedicated Host", async () => {
    const actions: string[] = [];
    const seenParams: Record<string, string>[] = [];
    const fetchMock = vi.fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>(
      async (input, init) => {
        const params = new URLSearchParams(await requestBodyForTest(input, init));
        const action = params.get("Action") ?? "";
        actions.push(action);
        seenParams.push(Object.fromEntries(params));
        if (action === "DescribeInstanceTypeOfferings") {
          return ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
          <DescribeInstanceTypeOfferingsResponse>
            <instanceTypeOfferingSet>
              <item>
                <instanceType>mac2.metal</instanceType>
                <location>eu-west-1a</location>
                <locationType>availability-zone</locationType>
              </item>
            </instanceTypeOfferingSet>
          </DescribeInstanceTypeOfferingsResponse>`);
        }
        if (action === "AllocateHosts") {
          return ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
          <AllocateHostsResponse>
            <hostIdSet><item><hostId>h-000000000001</hostId></item></hostIdSet>
          </AllocateHostsResponse>`);
        }
        if (action === "DescribeHosts") {
          return ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
          <DescribeHostsResponse>
            <hostSet>
              <item>
                <hostId>h-000000000001</hostId>
                <hostState>available</hostState>
                <availabilityZone>eu-west-1a</availabilityZone>
                <autoPlacement>off</autoPlacement>
                <hostProperties><instanceType>mac2.metal</instanceType></hostProperties>
              </item>
            </hostSet>
          </DescribeHostsResponse>`);
        }
        return ec2XMLResponse("<ErrorResponse />", 500);
      },
    );
    vi.stubGlobal("fetch", fetchMock);
    const fleet = testFleet(
      new MemoryStorage(),
      {},
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "test",
        CRABBOX_AWS_REGION: "eu-west-1",
      },
    );

    const response = await fleet.fetch(
      request("POST", "/v1/admin/mac-hosts?region=eu-west-1", {
        headers: { "x-crabbox-admin": "true" },
        body: { type: "mac2.metal" },
      }),
    );

    expect(response.status).toBe(201);
    expect(actions).toEqual(["DescribeInstanceTypeOfferings", "AllocateHosts", "DescribeHosts"]);
    expect(seenParams[1]).toMatchObject({
      Action: "AllocateHosts",
      AvailabilityZone: "eu-west-1a",
      InstanceType: "mac2.metal",
    });
    await expect(response.json()).resolves.toMatchObject({
      availabilityZone: "eu-west-1a",
      hosts: [{ id: "h-000000000001", instanceType: "mac2.metal" }],
    });
  });

  it("does not retry paid AWS EC2 Mac host allocation after AllocateHosts returns a host", async () => {
    const actions: string[] = [];
    const seenParams: Record<string, string>[] = [];
    const fetchMock = vi.fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>(
      async (input, init) => {
        const params = new URLSearchParams(await requestBodyForTest(input, init));
        const action = params.get("Action") ?? "";
        actions.push(action);
        seenParams.push(Object.fromEntries(params));
        if (action === "DescribeInstanceTypeOfferings") {
          return ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
          <DescribeInstanceTypeOfferingsResponse>
            <instanceTypeOfferingSet>
              <item>
                <instanceType>mac2.metal</instanceType>
                <location>eu-west-1a</location>
                <locationType>availability-zone</locationType>
              </item>
              <item>
                <instanceType>mac2.metal</instanceType>
                <location>eu-west-1b</location>
                <locationType>availability-zone</locationType>
              </item>
            </instanceTypeOfferingSet>
          </DescribeInstanceTypeOfferingsResponse>`);
        }
        if (action === "AllocateHosts") {
          return ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
          <AllocateHostsResponse>
            <hostIdSet><item><hostId>h-000000000001</hostId></item></hostIdSet>
          </AllocateHostsResponse>`);
        }
        if (action === "DescribeHosts") {
          return ec2XMLResponse(
            "<ErrorResponse><Error><Code>UnauthorizedOperation</Code></Error></ErrorResponse>",
            403,
          );
        }
        return ec2XMLResponse("<ErrorResponse />", 500);
      },
    );
    vi.stubGlobal("fetch", fetchMock);
    const fleet = testFleet(
      new MemoryStorage(),
      {},
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "test",
        CRABBOX_AWS_REGION: "eu-west-1",
      },
    );

    const response = await fleet.fetch(
      request("POST", "/v1/admin/mac-hosts?region=eu-west-1", {
        headers: { "x-crabbox-admin": "true" },
        body: { type: "mac2.metal" },
      }),
    );

    expect(response.status).toBe(201);
    expect(actions).toEqual(["DescribeInstanceTypeOfferings", "AllocateHosts", "DescribeHosts"]);
    expect(seenParams[1]).toMatchObject({
      Action: "AllocateHosts",
      AvailabilityZone: "eu-west-1a",
      InstanceType: "mac2.metal",
    });
    await expect(response.json()).resolves.toMatchObject({
      availabilityZone: "eu-west-1a",
      hosts: [{ id: "h-000000000001", instanceType: "mac2.metal" }],
    });
  });

  it("releases AWS EC2 Mac Dedicated Hosts only when AWS confirms success", async () => {
    const actions: string[] = [];
    const seenParams: Record<string, string>[] = [];
    const fetchMock = vi.fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>(
      async (input, init) => {
        const params = new URLSearchParams(await requestBodyForTest(input, init));
        const action = params.get("Action") ?? "";
        actions.push(action);
        seenParams.push(Object.fromEntries(params));
        if (action === "ReleaseHosts") {
          return ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
          <ReleaseHostsResponse>
            <unsuccessful/>
            <successful><item>h-000000000001</item></successful>
          </ReleaseHostsResponse>`);
        }
        return ec2XMLResponse("<ErrorResponse />", 500);
      },
    );
    vi.stubGlobal("fetch", fetchMock);
    const fleet = testFleet(
      new MemoryStorage(),
      {},
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "test",
        CRABBOX_AWS_REGION: "eu-west-1",
      },
    );

    const response = await fleet.fetch(
      request("DELETE", "/v1/admin/mac-hosts/h-000000000001?region=eu-west-1", {
        headers: { "x-crabbox-admin": "true" },
      }),
    );

    expect(response.status).toBe(200);
    expect(actions).toEqual(["ReleaseHosts"]);
    expect(seenParams[0]).toMatchObject({
      Action: "ReleaseHosts",
      "HostId.1": "h-000000000001",
    });
    await expect(response.json()).resolves.toMatchObject({
      hostId: "h-000000000001",
      released: ["h-000000000001"],
    });
  });

  it("rejects AWS EC2 Mac host release when AWS reports an unsuccessful release", async () => {
    const fetchMock = vi.fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>(
      async () =>
        ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
        <ReleaseHostsResponse>
          <unsuccessful>
            <item>
              <resourceId>h-000000000001</resourceId>
              <error>
                <code>Client.InvalidHost.Occupied</code>
                <message>Dedicated host cannot be released as it is occupied</message>
              </error>
            </item>
          </unsuccessful>
          <successful/>
        </ReleaseHostsResponse>`),
    );
    vi.stubGlobal("fetch", fetchMock);
    const fleet = testFleet(
      new MemoryStorage(),
      {},
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "test",
        CRABBOX_AWS_REGION: "eu-west-1",
      },
    );

    const response = await fleet.fetch(
      request("DELETE", "/v1/admin/mac-hosts/h-000000000001?region=eu-west-1", {
        headers: { "x-crabbox-admin": "true" },
      }),
    );

    expect(response.status).toBe(500);
    await expect(response.json()).resolves.toMatchObject({
      error: expect.stringContaining("Client.InvalidHost.Occupied"),
    });
  });

  it("rejects EC2 Mac host allocation when no availability zones are offered", async () => {
    const fetchMock = vi.fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>(
      async () =>
        ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
        <DescribeInstanceTypeOfferingsResponse>
          <instanceTypeOfferingSet />
        </DescribeInstanceTypeOfferingsResponse>`),
    );
    vi.stubGlobal("fetch", fetchMock);
    const fleet = testFleet(
      new MemoryStorage(),
      {},
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "test",
        CRABBOX_AWS_REGION: "eu-west-1",
      },
    );

    const response = await fleet.fetch(
      request("POST", "/v1/admin/mac-hosts?region=eu-west-1", {
        headers: { "x-crabbox-admin": "true" },
        body: { type: "mac2.metal" },
      }),
    );

    expect(response.status).toBe(400);
    await expect(response.json()).resolves.toMatchObject({ error: "no_mac_host_offerings" });
  });

  it("creates, waits, and promotes AWS images through admin routes", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage, {
      aws: fakeProvider(),
    });
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        provider: "aws",
        cloudID: "i-123",
        region: "eu-west-1",
        os: "ubuntu:26.04",
      }),
    );

    const denied = await fleet.fetch(
      request("POST", "/v1/images", {
        body: { leaseID: "cbx_000000000001", name: "openclaw-crabbox-test" },
      }),
    );
    expect(denied.status).toBe(403);

    const created = await fleet.fetch(
      request("POST", "/v1/images", {
        headers: { "x-crabbox-admin": "true" },
        body: { leaseID: "cbx_000000000001", name: "openclaw-crabbox-test", strategy: "image" },
      }),
    );
    expect(created.status).toBe(201);
    const createdBody = (await created.json()) as { image: { id: string; state: string } };
    expect(createdBody.image).toEqual(
      expect.objectContaining({ id: "ami-000000000001", state: "pending" }),
    );

    const promoted = await fleet.fetch(
      request("POST", "/v1/images/ami-000000000001/promote", {
        headers: { "x-crabbox-admin": "true" },
        body: {},
      }),
    );
    expect(promoted.status).toBe(200);
    expect(storage.value("image:aws:promoted:linux:x86_64:ubuntu26.04:eu-west-1")).toEqual(
      expect.objectContaining({
        id: "ami-000000000001",
        state: "available",
        target: "linux",
        os: "ubuntu:26.04",
      }),
    );
    expect(storage.value("image:aws:promoted")).toBeUndefined();
  });

  it("scopes promoted AWS macOS images by target, architecture, and server type", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage, {
      aws: fakeProvider(),
    });
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        provider: "aws",
        target: "macos",
        cloudID: "i-123",
        region: "eu-west-1",
        serverType: "mac2.metal",
      }),
    );

    const created = await fleet.fetch(
      request("POST", "/v1/images", {
        headers: { "x-crabbox-admin": "true" },
        body: { leaseID: "cbx_000000000001", name: "crabbox-macos-test" },
      }),
    );
    expect(created.status).toBe(201);
    await expect(created.json()).resolves.toMatchObject({
      image: {
        id: "ami-000000000001",
        target: "macos",
        architecture: "arm64_mac",
        serverType: "mac2.metal",
      },
    });

    const promoted = await fleet.fetch(
      request("POST", "/v1/images/ami-000000000001/promote", {
        headers: { "x-crabbox-admin": "true" },
        body: {},
      }),
    );
    expect(promoted.status).toBe(200);
    expect(storage.value("image:aws:promoted:macos:arm64_mac:mac2.metal:eu-west-1")).toEqual(
      expect.objectContaining({ id: "ami-000000000001", target: "macos" }),
    );
    expect(storage.value("image:aws:promoted")).toBeUndefined();
  });

  it("uses described AWS macOS image architecture when promoting external AMIs", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage, {
      aws: fakeProvider(undefined, {
        onGetImage(imageID) {
          return {
            id: imageID,
            name: "external-mac1",
            state: "available",
            provider: "aws",
            kind: "aws-ami",
            region: "us-east-1",
            resourceID: imageID,
            architecture: "x86_64_mac",
            serverType: "mac1.metal",
          };
        },
      }),
    });

    const promoted = await fleet.fetch(
      request("POST", "/v1/images/ami-external/promote?target=macos&region=us-east-1", {
        headers: { "x-crabbox-admin": "true" },
        body: { serverType: "mac1.metal" },
      }),
    );

    expect(promoted.status).toBe(200);
    expect(storage.value("image:aws:promoted:macos:x86_64_mac:mac1.metal:us-east-1")).toEqual(
      expect.objectContaining({
        id: "ami-external",
        architecture: "x86_64_mac",
        serverType: "mac1.metal",
        target: "macos",
      }),
    );
    expect(
      storage.value("image:aws:promoted:macos:arm64_mac:mac1.metal:us-east-1"),
    ).toBeUndefined();
  });

  it("promotes AWS images with query metadata and no request body", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage, {
      aws: fakeProvider(undefined, {
        onGetImage(imageID) {
          return {
            id: imageID,
            name: "external-mac1",
            state: "available",
            provider: "aws",
            kind: "aws-ami",
            region: "us-east-1",
            resourceID: imageID,
            architecture: "x86_64_mac",
          };
        },
      }),
    });

    const promoted = await fleet.fetch(
      request(
        "POST",
        "/v1/images/ami-query/promote?target=macos&region=us-east-1&serverType=mac1.metal",
        { headers: { "x-crabbox-admin": "true" } },
      ),
    );

    expect(promoted.status).toBe(200);
    expect(storage.value("image:aws:promoted:macos:x86_64_mac:mac1.metal:us-east-1")).toEqual(
      expect.objectContaining({ id: "ami-query", serverType: "mac1.metal", target: "macos" }),
    );
  });

  it("enables Fast Snapshot Restore when promoting an AWS image", async () => {
    const storage = new MemoryStorage();
    const fsrCalls: Array<{ snapshots: string[]; zones: string[] }> = [];
    const fleet = testFleet(storage, {
      aws: fakeProvider(undefined, {
        onGetImage(imageID) {
          return {
            id: imageID,
            name: "devtools-windows",
            state: "available",
            provider: "aws",
            kind: "aws-ami",
            region: "us-west-2",
            resourceID: imageID,
            architecture: "x86_64",
            snapshots: ["snap-root", "snap-tools"],
          };
        },
        onEnableFastSnapshotRestore(snapshotIDs, availabilityZones) {
          fsrCalls.push({ snapshots: snapshotIDs, zones: availabilityZones });
        },
      }),
    });

    const promoted = await fleet.fetch(
      request("POST", "/v1/images/ami-devtools/promote?target=windows&region=us-west-2", {
        headers: { "x-crabbox-admin": "true" },
        body: {
          fastSnapshotRestore: true,
          fastSnapshotRestoreAvailabilityZones: ["us-west-2a", "us-west-2b"],
        },
      }),
    );

    expect(promoted.status).toBe(200);
    expect(fsrCalls).toEqual([
      { snapshots: ["snap-root", "snap-tools"], zones: ["us-west-2a", "us-west-2b"] },
    ]);
    expect(storage.value("image:aws:promoted:windows:x86_64:us-west-2")).toEqual(
      expect.objectContaining({
        id: "ami-devtools",
        target: "windows",
        fastSnapshotRestores: [
          { snapshotID: "snap-root", availabilityZone: "us-west-2a", state: "enabling" },
          { snapshotID: "snap-root", availabilityZone: "us-west-2b", state: "enabling" },
          { snapshotID: "snap-tools", availabilityZone: "us-west-2a", state: "enabling" },
          { snapshotID: "snap-tools", availabilityZone: "us-west-2b", state: "enabling" },
        ],
      }),
    );
  });

  it("returns current AWS Fast Snapshot Restore status for promoted images", async () => {
    const storage = new MemoryStorage();
    const statusCalls: Array<{ snapshots: string[]; zones: string[] | undefined }> = [];
    storage.seed("image:aws:promoted:windows:x86_64:us-west-2", {
      id: "ami-devtools",
      name: "devtools-windows",
      state: "available",
      provider: "aws",
      kind: "aws-ami",
      region: "us-west-2",
      target: "windows",
      architecture: "x86_64",
      snapshots: ["snap-root"],
      promotedAt: "2026-05-21T00:00:00Z",
    });
    const fleet = testFleet(storage, {
      aws: fakeProvider(undefined, {
        onGetImage(imageID) {
          return {
            id: imageID,
            name: "devtools-windows",
            state: "available",
            provider: "aws",
            kind: "aws-ami",
            region: "us-west-2",
            resourceID: imageID,
            architecture: "x86_64",
            snapshots: ["snap-root", "snap-tools"],
          };
        },
        onFastSnapshotRestoreStatus(snapshotIDs, availabilityZones) {
          statusCalls.push({ snapshots: snapshotIDs, zones: availabilityZones });
          return [
            {
              snapshotID: "snap-root",
              availabilityZone: "us-west-2a",
              state: "enabled",
            },
          ];
        },
      }),
    });

    const response = await fleet.fetch(
      request(
        "GET",
        "/v1/images/ami-devtools/fast-snapshot-restore?region=us-west-2&fsrAz=us-west-2a",
        {
          headers: { "x-crabbox-admin": "true" },
        },
      ),
    );

    expect(response.status).toBe(200);
    expect(statusCalls).toEqual([
      { snapshots: ["snap-root", "snap-tools"], zones: ["us-west-2a"] },
    ]);
    await expect(response.json()).resolves.toMatchObject({
      image: {
        id: "ami-devtools",
        target: "windows",
        fastSnapshotRestores: [
          { snapshotID: "snap-root", availabilityZone: "us-west-2a", state: "enabled" },
        ],
      },
      fastSnapshotRestores: [
        { snapshotID: "snap-root", availabilityZone: "us-west-2a", state: "enabled" },
      ],
    });
  });

  it("requires Fast Snapshot Restore availability zones when no defaults are configured", async () => {
    const fleet = testFleet(new MemoryStorage(), {
      aws: fakeProvider(),
    });

    const promoted = await fleet.fetch(
      request("POST", "/v1/images/ami-devtools/promote?target=linux&region=eu-west-1", {
        headers: { "x-crabbox-admin": "true" },
        body: { fastSnapshotRestore: true },
      }),
    );

    expect(promoted.status).toBe(400);
    await expect(promoted.json()).resolves.toMatchObject({
      error: "invalid_fast_snapshot_restore_zones",
    });
  });

  it("scopes external Linux AWS promotions to the default portable OS", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage, {
      aws: fakeProvider(),
    });

    const promoted = await fleet.fetch(
      request("POST", "/v1/images/ami-linux/promote?target=linux&region=eu-west-1", {
        headers: { "x-crabbox-admin": "true" },
        body: {},
      }),
    );

    expect(promoted.status).toBe(200);
    expect(storage.value("image:aws:promoted:linux:x86_64:ubuntu26.04:eu-west-1")).toEqual(
      expect.objectContaining({ id: "ami-linux", os: "ubuntu:26.04" }),
    );
    expect(storage.value("image:aws:promoted")).toBeUndefined();
  });

  it("treats legacy created Linux AWS images without OS metadata as Ubuntu 24.04", async () => {
    const storage = new MemoryStorage();
    storage.seed("image:aws:created:ami-legacy", {
      id: "ami-legacy",
      name: "crabbox-legacy",
      state: "available",
      provider: "aws",
      target: "linux",
      region: "eu-west-1",
    });
    const fleet = testFleet(storage, {
      aws: fakeProvider(),
    });

    const promoted = await fleet.fetch(
      request("POST", "/v1/images/ami-legacy/promote", {
        headers: { "x-crabbox-admin": "true" },
        body: {},
      }),
    );

    expect(promoted.status).toBe(200);
    expect(storage.value("image:aws:promoted:linux:x86_64:ubuntu24.04:eu-west-1")).toEqual(
      expect.objectContaining({ id: "ami-legacy", os: "ubuntu:24.04" }),
    );
    expect(storage.value("image:aws:promoted")).toEqual(
      expect.objectContaining({ id: "ami-legacy", os: "ubuntu:24.04" }),
    );
  });

  it("does not write ARM Linux AWS promotions to the legacy x86 key", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage, {
      aws: fakeProvider(),
    });

    const promoted = await fleet.fetch(
      request(
        "POST",
        "/v1/images/ami-arm/promote?target=linux&region=eu-west-1&os=ubuntu:24.04&architecture=arm64",
        { headers: { "x-crabbox-admin": "true" }, body: {} },
      ),
    );

    expect(promoted.status).toBe(200);
    expect(storage.value("image:aws:promoted:linux:arm64:ubuntu24.04:eu-west-1")).toEqual(
      expect.objectContaining({ id: "ami-arm", architecture: "arm64", os: "ubuntu:24.04" }),
    );
    expect(storage.value("image:aws:promoted")).toBeUndefined();
  });

  it("uses promoted AWS image region when creating leases", async () => {
    const storage = new MemoryStorage();
    let createdConfig: LeaseConfig | undefined;
    const fleet = testFleet(
      storage,
      {
        aws: fakeProvider(
          (config) => {
            createdConfig = config;
          },
          { provider: "aws", region: "us-east-2" },
        ),
      },
      { CRABBOX_AWS_REGION: "eu-west-1" },
    );
    storage.seed("image:aws:promoted:linux:x86_64:ubuntu26.04", {
      id: "ami-000000000001",
      name: "crabbox-image-test",
      state: "available",
      region: "us-east-2",
      target: "linux",
      os: "ubuntu:26.04",
      promotedAt: "2026-05-01T12:46:00Z",
    });

    const response = await fleet.fetch(
      request("POST", "/v1/leases", {
        body: {
          provider: "aws",
          sshPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest test@example.com",
        },
      }),
    );

    expect(response.status).toBe(201);
    expect(createdConfig?.awsAMI).toBe("ami-000000000001");
    expect(createdConfig?.awsRegion).toBe("us-east-2");
    const body = (await response.json()) as { lease: LeaseRecord };
    expect(body.lease.region).toBe("us-east-2");
  });

  it("uses ARM64 promoted AWS Linux images for ARM leases", async () => {
    const storage = new MemoryStorage();
    let createdConfig: LeaseConfig | undefined;
    const fleet = testFleet(
      storage,
      {
        aws: fakeProvider(
          (config) => {
            createdConfig = config;
          },
          { provider: "aws", region: "us-east-2" },
        ),
      },
      { CRABBOX_AWS_REGION: "eu-west-1" },
    );
    storage.seed("image:aws:promoted:linux:x86_64:ubuntu26.04", {
      id: "ami-x86",
      name: "crabbox-x86-image",
      state: "available",
      region: "us-east-1",
      target: "linux",
      architecture: "x86_64",
      os: "ubuntu:26.04",
      promotedAt: "2026-05-01T12:46:00Z",
    });
    storage.seed("image:aws:promoted:linux:arm64:ubuntu26.04", {
      id: "ami-arm64",
      name: "crabbox-arm-image",
      state: "available",
      region: "us-east-2",
      target: "linux",
      architecture: "arm64",
      os: "ubuntu:26.04",
      promotedAt: "2026-05-01T12:47:00Z",
    });

    const response = await fleet.fetch(
      request("POST", "/v1/leases", {
        body: {
          provider: "aws",
          architecture: "arm64",
          sshPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest test@example.com",
        },
      }),
    );

    expect(response.status).toBe(201);
    expect(createdConfig?.architecture).toBe("arm64");
    expect(createdConfig?.awsAMI).toBe("ami-arm64");
    expect(createdConfig?.awsRegion).toBe("us-east-2");
  });

  it("does not use the legacy x86 AWS promoted image for ARM leases", async () => {
    const storage = new MemoryStorage();
    let createdConfig: LeaseConfig | undefined;
    const fleet = testFleet(
      storage,
      {
        aws: fakeProvider(
          (config) => {
            createdConfig = config;
          },
          { provider: "aws", region: "eu-west-1" },
        ),
      },
      { CRABBOX_AWS_REGION: "eu-west-1" },
    );
    storage.seed("image:aws:promoted", {
      id: "ami-x86-legacy",
      name: "crabbox-legacy-x86",
      state: "available",
      region: "eu-west-1",
      target: "linux",
      architecture: "x86_64",
      os: "ubuntu:24.04",
      promotedAt: "2026-05-01T12:48:00Z",
    });

    const response = await fleet.fetch(
      request("POST", "/v1/leases", {
        body: {
          provider: "aws",
          architecture: "arm64",
          os: "ubuntu:24.04",
          sshPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest test@example.com",
        },
      }),
    );

    expect(response.status).toBe(201);
    expect(createdConfig?.architecture).toBe("arm64");
    expect(createdConfig?.awsAMI).toBe("");
  });

  it("passes provider-native checkpoint snapshot fields into new leases", async () => {
    let awsConfig: LeaseConfig | undefined;
    let azureConfig: LeaseConfig | undefined;
    let gcpConfig: LeaseConfig | undefined;
    const fleet = testFleet(new MemoryStorage(), {
      aws: fakeProvider(
        (config) => {
          awsConfig = config;
        },
        { provider: "aws", region: "us-east-2" },
      ),
      azure: fakeProvider(
        (config) => {
          azureConfig = config;
        },
        { provider: "azure", region: "eastus" },
      ),
      gcp: fakeProvider(
        (config) => {
          gcpConfig = config;
        },
        { provider: "gcp", region: "us-central1-a" },
      ),
    });

    const aws = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: { "x-crabbox-admin": "true" },
        body: {
          provider: "aws",
          awsRegion: "us-east-2",
          awsSnapshot: "snap-000000000001",
          sshPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest test@example.com",
        },
      }),
    );
    const azure = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: { "x-crabbox-admin": "true" },
        body: {
          provider: "azure",
          azureLocation: "eastus",
          azureSnapshot:
            "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/snapshots/checkpoint-azure",
          sshPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest test@example.com",
        },
      }),
    );
    const gcp = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: { "x-crabbox-admin": "true" },
        body: {
          provider: "gcp",
          gcpProject: "proj",
          gcpZone: "us-central1-a",
          gcpSnapshot: "projects/proj/global/snapshots/checkpoint-gcp",
          sshPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest test@example.com",
        },
      }),
    );

    expect(aws.status).toBe(201);
    expect(azure.status).toBe(201);
    expect(gcp.status).toBe(201);
    expect(awsConfig?.awsSnapshot).toBe("snap-000000000001");
    expect(azureConfig?.azureSnapshot).toBe(
      "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/snapshots/checkpoint-azure",
    );
    expect(gcpConfig?.gcpSnapshot).toBe("projects/proj/global/snapshots/checkpoint-gcp");
  });

  it("rejects snapshot-backed lease fields for non-admin users", async () => {
    let created = false;
    const fleet = testFleet(new MemoryStorage(), {
      aws: fakeProvider(() => {
        created = true;
      }),
    });

    const response = await fleet.fetch(
      request("POST", "/v1/leases", {
        body: {
          provider: "aws",
          awsSnapshot: "snap-000000000001",
          sshPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest test@example.com",
        },
      }),
    );

    expect(response.status).toBe(403);
    expect(created).toBe(false);
    await expect(response.json()).resolves.toMatchObject({ error: "admin_required" });
  });

  it("deletes AWS images through admin routes", async () => {
    let deleted = "";
    const storage = new MemoryStorage();
    storage.seed("image:aws:created:ami-000000000001", {
      id: "ami-000000000001",
      name: "openclaw-crabbox-test",
      state: "available",
      provider: "aws",
      kind: "aws-ami",
      region: "eu-west-1",
    });
    const fleet = testFleet(storage, {
      aws: fakeProvider(undefined, {
        onDeleteImage(imageID) {
          deleted = imageID;
        },
      }),
    });

    const denied = await fleet.fetch(
      request("DELETE", "/v1/images/ami-000000000001", {
        body: {},
      }),
    );
    expect(denied.status).toBe(403);

    const allowed = await fleet.fetch(
      request("DELETE", "/v1/images/ami-000000000001", {
        headers: { "x-crabbox-admin": "true" },
        body: {},
      }),
    );
    expect(allowed.status).toBe(200);
    expect(deleted).toBe("ami-000000000001");
  });

  it("rejects deleting AWS images without Crabbox ownership metadata", async () => {
    let deleted = "";
    const fleet = testFleet(new MemoryStorage(), {
      aws: fakeProvider(undefined, {
        onDeleteImage(imageID) {
          deleted = imageID;
        },
      }),
    });

    const response = await fleet.fetch(
      request("DELETE", "/v1/images/ami-000000000001", {
        headers: { "x-crabbox-admin": "true" },
        body: {},
      }),
    );

    expect(response.status).toBe(409);
    expect(deleted).toBe("");
    await expect(response.json()).resolves.toMatchObject({ error: "image_not_owned" });
  });

  it("creates Azure and GCP native disk snapshots through admin routes by default", async () => {
    const storage = new MemoryStorage();
    const createdNames: string[] = [];
    const fleet = testFleet(storage, {
      azure: fakeProvider(undefined, {
        provider: "azure",
        region: "eastus",
        imageRegion: "eastus",
        onCreateImage: (_instanceID, name, strategy) =>
          createdNames.push(`azure:${strategy}:${name}`),
      }),
      gcp: fakeProvider(undefined, {
        provider: "gcp",
        region: "us-central1-a",
        imageRegion: "us-central1-a",
        onCreateImage: (_instanceID, name, strategy) =>
          createdNames.push(`gcp:${strategy}:${name}`),
      }),
    });
    storage.seed(
      "lease:cbx_000000000002",
      testLease({
        id: "cbx_000000000002",
        provider: "azure",
        cloudID: "crabbox-azure",
        region: "eastus",
      }),
    );
    storage.seed(
      "lease:cbx_000000000003",
      testLease({
        id: "cbx_000000000003",
        provider: "gcp",
        cloudID: "crabbox-gcp",
        region: "us-central1-a",
        providerProject: "proj",
      }),
    );

    const azure = await fleet.fetch(
      request("POST", "/v1/images", {
        headers: { "x-crabbox-admin": "true" },
        body: { leaseID: "cbx_000000000002", name: "After Install" },
      }),
    );
    const longGCPName = `${"a".repeat(62)} b`;
    const gcp = await fleet.fetch(
      request("POST", "/v1/images", {
        headers: { "x-crabbox-admin": "true" },
        body: { leaseID: "cbx_000000000003", name: longGCPName },
      }),
    );
    expect(azure.status).toBe(201);
    expect(gcp.status).toBe(201);
    await expect(azure.json()).resolves.toMatchObject({
      image: { id: "checkpoint-azure", provider: "azure", kind: "azure-os-disk-snapshot" },
    });
    await expect(gcp.json()).resolves.toMatchObject({
      image: { id: "checkpoint-gcp", provider: "gcp", kind: "gcp-disk-snapshot" },
    });
    expect(createdNames).toEqual([
      "azure:disk-snapshot:after-install",
      `gcp:disk-snapshot:${"a".repeat(62)}`,
    ]);
    expect(storage.value("image:azure:created:checkpoint-azure")).toMatchObject({
      id: "checkpoint-azure",
      provider: "azure",
    });
    expect(storage.value("image:gcp:created:checkpoint-gcp")).toMatchObject({
      id: "checkpoint-gcp",
      provider: "gcp",
    });
  });

  it("rejects Azure managed image creation from active leases", async () => {
    let created = false;
    const storage = new MemoryStorage();
    const fleet = testFleet(storage, {
      azure: fakeProvider(undefined, {
        provider: "azure",
        onCreateImage() {
          created = true;
        },
      }),
    });
    storage.seed(
      "lease:cbx_000000000002",
      testLease({
        id: "cbx_000000000002",
        provider: "azure",
        cloudID: "crabbox-azure",
        region: "eastus",
      }),
    );

    const response = await fleet.fetch(
      request("POST", "/v1/images", {
        headers: { "x-crabbox-admin": "true" },
        body: { leaseID: "cbx_000000000002", name: "After Install", strategy: "image" },
      }),
    );

    expect(response.status).toBe(400);
    expect(created).toBe(false);
    await expect(response.json()).resolves.toMatchObject({ error: "unsupported_strategy" });
  });

  it("rejects unsupported native image providers before constructing provider clients", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    storage.seed(
      "lease:cbx_000000000004",
      testLease({
        id: "cbx_000000000004",
        provider: "hetzner",
        cloudID: "123",
      }),
    );

    const response = await fleet.fetch(
      request("POST", "/v1/images", {
        headers: { "x-crabbox-admin": "true" },
        body: { leaseID: "cbx_000000000004", name: "After Install" },
      }),
    );

    expect(response.status).toBe(400);
    await expect(response.json()).resolves.toMatchObject({ error: "unsupported_provider" });
  });

  it("rejects invalid native image strategies", async () => {
    let created = false;
    const storage = new MemoryStorage();
    const fleet = testFleet(storage, {
      aws: fakeProvider(undefined, {
        provider: "aws",
        onCreateImage() {
          created = true;
        },
      }),
    });
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        provider: "aws",
        cloudID: "i-000000000001",
      }),
    );

    const response = await fleet.fetch(
      request("POST", "/v1/images", {
        headers: { "x-crabbox-admin": "true" },
        body: { leaseID: "cbx_000000000001", name: "After Install", strategy: "snapshopt" },
      }),
    );

    expect(response.status).toBe(400);
    expect(created).toBe(false);
    await expect(response.json()).resolves.toMatchObject({ error: "invalid_strategy" });
  });

  it("keeps accepting disk_snapshot as a native image strategy alias", async () => {
    let strategy = "";
    const storage = new MemoryStorage();
    const fleet = testFleet(storage, {
      aws: fakeProvider(undefined, {
        provider: "aws",
        onCreateImage(_sourceID, _name, receivedStrategy) {
          strategy = receivedStrategy;
        },
      }),
    });
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        provider: "aws",
        cloudID: "i-000000000001",
      }),
    );

    const response = await fleet.fetch(
      request("POST", "/v1/images", {
        headers: { "x-crabbox-admin": "true" },
        body: { leaseID: "cbx_000000000001", name: "After Install", strategy: "disk_snapshot" },
      }),
    );

    expect(response.status).toBe(201);
    expect(strategy).toBe("disk-snapshot");
  });

  it("deletes provider-native images using provider query routing", async () => {
    let azureDeleted = "";
    let azureDeletedKind = "";
    let gcpDeleted = "";
    let gcpDeletedKind = "";
    const storage = new MemoryStorage();
    const azureResource =
      "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/snapshots/checkpoint-azure";
    const gcpResource = "projects/proj/global/snapshots/checkpoint-gcp";
    storage.seed(`image:azure:created:${encodeURIComponent(azureResource)}`, {
      id: azureResource,
      name: "checkpoint-azure",
      state: "available",
      provider: "azure",
      kind: "azure-os-disk-snapshot",
      region: "eastus",
    });
    storage.seed(`image:gcp:created:${encodeURIComponent(gcpResource)}`, {
      id: gcpResource,
      name: "checkpoint-gcp",
      state: "available",
      provider: "gcp",
      kind: "gcp-disk-snapshot",
      region: "us-central1-a",
      project: "proj",
    });
    const fleet = testFleet(storage, {
      azure: fakeProvider(undefined, {
        provider: "azure",
        onDeleteImage(imageID, kind) {
          azureDeleted = imageID;
          azureDeletedKind = kind ?? "";
        },
      }),
      gcp: fakeProvider(undefined, {
        provider: "gcp",
        onDeleteImage(imageID, kind) {
          gcpDeleted = imageID;
          gcpDeletedKind = kind ?? "";
        },
      }),
    });

    const azure = await fleet.fetch(
      request(
        "DELETE",
        `/v1/images/${encodeURIComponent(azureResource)}?provider=azure&region=eastus&kind=azure-os-disk-snapshot`,
        {
          headers: { "x-crabbox-admin": "true" },
          body: {},
        },
      ),
    );
    const gcp = await fleet.fetch(
      request(
        "DELETE",
        `/v1/images/${encodeURIComponent(gcpResource)}?provider=gcp&region=us-central1-a&project=proj&kind=gcp-disk-snapshot`,
        {
          headers: { "x-crabbox-admin": "true" },
          body: {},
        },
      ),
    );

    expect(azure.status).toBe(200);
    expect(gcp.status).toBe(200);
    expect(azureDeleted).toBe(azureResource);
    expect(azureDeletedKind).toBe("azure-os-disk-snapshot");
    expect(gcpDeleted).toBe(gcpResource);
    expect(gcpDeletedKind).toBe("gcp-disk-snapshot");
  });

  it("rejects deleting provider-native images without Crabbox ownership metadata", async () => {
    let azureDeleted = "";
    let gcpDeleted = "";
    const fleet = testFleet(new MemoryStorage(), {
      azure: fakeProvider(undefined, {
        provider: "azure",
        onDeleteImage(imageID) {
          azureDeleted = imageID;
        },
      }),
      gcp: fakeProvider(undefined, {
        provider: "gcp",
        onDeleteImage(imageID) {
          gcpDeleted = imageID;
        },
      }),
    });

    const azureResource =
      "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/snapshots/checkpoint-azure";
    const gcpResource = "projects/proj/global/snapshots/checkpoint-gcp";
    const azure = await fleet.fetch(
      request(
        "DELETE",
        `/v1/images/${encodeURIComponent(azureResource)}?provider=azure&region=eastus&kind=azure-os-disk-snapshot`,
        {
          headers: { "x-crabbox-admin": "true" },
          body: {},
        },
      ),
    );
    const gcp = await fleet.fetch(
      request(
        "DELETE",
        `/v1/images/${encodeURIComponent(gcpResource)}?provider=gcp&region=us-central1-a&project=proj&kind=gcp-disk-snapshot`,
        {
          headers: { "x-crabbox-admin": "true" },
          body: {},
        },
      ),
    );

    expect(azure.status).toBe(409);
    expect(gcp.status).toBe(409);
    expect(azureDeleted).toBe("");
    expect(gcpDeleted).toBe("");
    await expect(azure.json()).resolves.toMatchObject({ error: "image_not_owned" });
    await expect(gcp.json()).resolves.toMatchObject({ error: "image_not_owned" });
  });

  it("maps missing provider-native images to 404", async () => {
    const fleet = testFleet(new MemoryStorage(), {
      aws: fakeProvider(undefined, {
        provider: "aws",
        onGetImage() {
          throw new Error("aws DescribeImages: http 400: InvalidAMIID.NotFound");
        },
      }),
    });

    const response = await fleet.fetch(
      request("GET", "/v1/images/ami-000000000001?provider=aws", {
        headers: { "x-crabbox-admin": "true" },
      }),
    );

    expect(response.status).toBe(404);
    await expect(response.json()).resolves.toMatchObject({ error: "not_found" });
  });

  it("rejects invalid image provider query routing", async () => {
    let deleted = "";
    const fleet = testFleet(new MemoryStorage(), {
      aws: fakeProvider(undefined, {
        onDeleteImage(imageID) {
          deleted = imageID;
        },
      }),
    });

    const response = await fleet.fetch(
      request("DELETE", "/v1/images/checkpoint-azure?provider=azuer", {
        headers: { "x-crabbox-admin": "true" },
        body: {},
      }),
    );

    expect(response.status).toBe(400);
    expect(deleted).toBe("");
    await expect(response.json()).resolves.toMatchObject({ error: "unsupported_provider" });
  });

  it("rejects deleting the promoted AWS image", async () => {
    let deleted = "";
    const storage = new MemoryStorage();
    storage.seed("image:aws:promoted", {
      id: "ami-000000000001",
      name: "openclaw-crabbox-test",
      state: "available",
      region: "eu-west-1",
      promotedAt: "2026-05-01T12:46:00Z",
    });
    const fleet = testFleet(storage, {
      aws: fakeProvider(undefined, {
        onDeleteImage(imageID) {
          deleted = imageID;
        },
      }),
    });

    const response = await fleet.fetch(
      request("DELETE", "/v1/images/ami-000000000001", {
        headers: { "x-crabbox-admin": "true" },
        body: {},
      }),
    );

    expect(response.status).toBe(409);
    expect(deleted).toBe("");
    await expect(response.json()).resolves.toMatchObject({ error: "image_promoted" });
  });

  it("rejects deleting a promoted AWS image even when a created-image record also exists", async () => {
    let deleted = "";
    const storage = new MemoryStorage();
    const image = {
      id: "ami-000000000001",
      name: "openclaw-crabbox-test",
      state: "available",
      provider: "aws",
      kind: "aws-ami",
      region: "eu-west-1",
    };
    storage.seed("image:aws:created:ami-000000000001", image);
    storage.seed("image:aws:promoted", {
      ...image,
      promotedAt: "2026-05-01T12:46:00Z",
    });
    const fleet = testFleet(storage, {
      aws: fakeProvider(undefined, {
        onDeleteImage(imageID) {
          deleted = imageID;
        },
      }),
    });

    const response = await fleet.fetch(
      request("DELETE", "/v1/images/ami-000000000001", {
        headers: { "x-crabbox-admin": "true" },
        body: {},
      }),
    );

    expect(response.status).toBe(409);
    expect(deleted).toBe("");
    await expect(response.json()).resolves.toMatchObject({ error: "image_promoted" });
  });

  it("rejects deleting scoped promoted AWS images", async () => {
    let deleted = "";
    const storage = new MemoryStorage();
    storage.seed("image:aws:promoted:macos:arm64_mac:mac2.metal:eu-west-1", {
      id: "ami-000000000001",
      name: "crabbox-macos-test",
      state: "available",
      region: "eu-west-1",
      target: "macos",
      serverType: "mac2.metal",
      architecture: "arm64_mac",
      promotedAt: "2026-05-01T12:46:00Z",
    });
    const fleet = testFleet(storage, {
      aws: fakeProvider(undefined, {
        onDeleteImage(imageID) {
          deleted = imageID;
        },
      }),
    });

    const response = await fleet.fetch(
      request("DELETE", "/v1/images/ami-000000000001", {
        headers: { "x-crabbox-admin": "true" },
        body: {},
      }),
    );

    expect(response.status).toBe(409);
    expect(deleted).toBe("");
    await expect(response.json()).resolves.toMatchObject({ error: "image_promoted" });
  });

  it("mints broker-owned artifact upload URLs without exposing secrets", async () => {
    const fleet = testFleet(
      new MemoryStorage(),
      {},
      {
        CRABBOX_ARTIFACTS_BACKEND: "r2",
        CRABBOX_ARTIFACTS_BUCKET: "qa-artifacts",
        CRABBOX_ARTIFACTS_PREFIX: "qa",
        CRABBOX_ARTIFACTS_BASE_URL: "https://artifacts.example.com",
        CRABBOX_ARTIFACTS_REGION: "auto",
        CRABBOX_ARTIFACTS_ENDPOINT_URL: "https://account.r2.cloudflarestorage.com",
        CRABBOX_ARTIFACTS_ACCESS_KEY_ID: "access-key",
        CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY: "super-secret",
      },
    );

    const response = await fleet.fetch(
      request("POST", "/v1/artifacts/uploads", {
        headers: { "x-crabbox-owner": "alice@example.com" },
        body: {
          prefix: "pr-42",
          files: [
            {
              name: "screenshots/after.png",
              size: 123,
              contentType: "image/png",
              sha256: await sha256HexForTest("after"),
            },
          ],
        },
      }),
    );

    expect(response.status).toBe(201);
    const body = (await response.json()) as {
      backend: string;
      bucket: string;
      prefix: string;
      files: Array<{
        name: string;
        key: string;
        url: string;
        upload: { url: string; headers: Record<string, string> };
      }>;
    };
    expect(body.backend).toBe("r2");
    expect(body.bucket).toBe("qa-artifacts");
    const org = Buffer.from("default-org").toString("base64url");
    const owner = Buffer.from("alice@example.com").toString("base64url");
    expect(body.prefix).toBe(`qa/v2/org/${org}/owner/${owner}/pr-42`);
    expect(body.files[0].key).toBe(`qa/v2/org/${org}/owner/${owner}/pr-42/screenshots/after.png`);
    expect(body.files[0].url).toBe(
      `https://artifacts.example.com/qa/v2/org/${org}/owner/${owner}/pr-42/screenshots/after.png`,
    );
    expect(body.files[0].upload.headers["content-length"]).toBe("123");
    expect(body.files[0].upload.headers["content-type"]).toBe("image/png");
    expect(body.files[0].upload.url).toContain("X-Amz-Signature=");
    expect(new URL(body.files[0].upload.url).searchParams.get("X-Amz-SignedHeaders")).toContain(
      "content-length",
    );
    expect(JSON.stringify(body)).not.toContain("super-secret");
  });

  it("isolates artifact grants by opaque organization and owner identities", async () => {
    const fleet = testFleet(
      new MemoryStorage(),
      {},
      {
        CRABBOX_ARTIFACTS_BACKEND: "r2",
        CRABBOX_ARTIFACTS_BUCKET: "qa-artifacts",
        CRABBOX_ARTIFACTS_PREFIX: "qa",
        CRABBOX_ARTIFACTS_ENDPOINT_URL: "https://account.r2.cloudflarestorage.com",
        CRABBOX_ARTIFACTS_ACCESS_KEY_ID: "access-key",
        CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY: "super-secret",
      },
    );
    const grant = async (org: string, owner: string, prefix: string) => {
      const response = await fleet.fetch(
        request("POST", "/v1/artifacts/uploads", {
          headers: { "x-crabbox-org": org, "x-crabbox-owner": owner },
          body: { prefix, files: [{ name: "proof.txt", size: 1 }] },
        }),
      );
      expect(response.status).toBe(201);
      return (await response.json()) as {
        prefix: string;
        files: Array<{ key: string; url: string; upload: { url: string } }>;
      };
    };

    const orgA = await grant("org-a", "alice/team", "run-1");
    const orgB = await grant("org-b", "alice/team", "run-1");
    const shiftedBoundary = await grant("org-a", "alice", "team/run-1");

    const slashOrg = await grant("org/team", "alice", "run-1");
    const backslashOrg = await grant("org\\team", "alice", "run-1");

    for (const [left, right] of [
      [orgA, orgB],
      [orgA, shiftedBoundary],
      [slashOrg, backslashOrg],
    ]) {
      expect(left.prefix).not.toBe(right.prefix);
      expect(left.files[0].key).not.toBe(right.files[0].key);
      expect(new URL(left.files[0].url).pathname).not.toBe(new URL(right.files[0].url).pathname);
      expect(new URL(left.files[0].upload.url).pathname).not.toBe(
        new URL(right.files[0].upload.url).pathname,
      );
    }
    expect(orgA.prefix).toContain(
      `/org/${Buffer.from("org-a").toString("base64url")}/owner/${Buffer.from("alice/team").toString("base64url")}/`,
    );
  });

  it("rejects empty artifact authorization identities", async () => {
    const fleet = testFleet(
      new MemoryStorage(),
      {},
      {
        CRABBOX_DEFAULT_ORG: "",
        CRABBOX_ARTIFACTS_BACKEND: "r2",
        CRABBOX_ARTIFACTS_BUCKET: "qa-artifacts",
        CRABBOX_ARTIFACTS_ENDPOINT_URL: "https://account.r2.cloudflarestorage.com",
        CRABBOX_ARTIFACTS_ACCESS_KEY_ID: "access-key",
        CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY: "super-secret",
      },
    );
    const grant = async (headers: Record<string, string>) => {
      const response = await fleet.fetch(
        request("POST", "/v1/artifacts/uploads", {
          headers,
          body: { files: [{ name: "proof.txt", size: 1 }] },
        }),
      );
      expect(response.status).toBe(400);
      return (await response.json()) as { message: string };
    };

    await expect(grant({ "x-crabbox-owner": "alice", "x-crabbox-org": "" })).resolves.toMatchObject(
      { message: "artifact upload organization identity is required" },
    );
    await expect(
      grant({ "x-crabbox-owner": "", "x-crabbox-org": "example-org" }),
    ).resolves.toMatchObject({ message: "artifact upload owner identity is required" });
  });

  it("reports artifact broker setup errors without provider-specific local credentials", async () => {
    const fleet = testFleet();
    const response = await fleet.fetch(
      request("POST", "/v1/artifacts/uploads", {
        body: { files: [{ name: "screenshot.png", size: 1 }] },
      }),
    );
    const body = (await response.json()) as { error: string; message: string };
    expect(response.status).toBe(400);
    expect(body.error).toBe("artifact_upload_unavailable");
    expect(body.message).toContain("artifact broker is not configured");
  });

  it("requires an R2 endpoint before minting artifact upload URLs", async () => {
    const fleet = testFleet(
      new MemoryStorage(),
      {},
      {
        CRABBOX_ARTIFACTS_BACKEND: "r2",
        CRABBOX_ARTIFACTS_BUCKET: "qa-artifacts",
        CRABBOX_ARTIFACTS_ACCESS_KEY_ID: "access-key",
        CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY: "super-secret",
      },
    );

    const response = await fleet.fetch(
      request("POST", "/v1/artifacts/uploads", {
        body: { files: [{ name: "screenshot.png", size: 1 }] },
      }),
    );
    const body = (await response.json()) as { error: string; message: string };
    expect(response.status).toBe(400);
    expect(body.error).toBe("artifact_upload_unavailable");
    expect(body.message).toContain("CRABBOX_ARTIFACTS_ENDPOINT_URL");
  });

  it("caps aggregate artifact upload bytes before minting grants", async () => {
    const fleet = testFleet(
      new MemoryStorage(),
      {},
      {
        CRABBOX_ARTIFACTS_BACKEND: "r2",
        CRABBOX_ARTIFACTS_BUCKET: "qa-artifacts",
        CRABBOX_ARTIFACTS_ENDPOINT_URL: "https://account.r2.cloudflarestorage.com",
        CRABBOX_ARTIFACTS_ACCESS_KEY_ID: "access-key",
        CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY: "super-secret",
      },
    );

    const response = await fleet.fetch(
      request("POST", "/v1/artifacts/uploads", {
        body: {
          files: Array.from({ length: 6 }, (_, index) => ({
            name: `video-${index}.mp4`,
            size: 1024 * 1024 * 1024,
          })),
        },
      }),
    );
    const body = (await response.json()) as { error: string; message: string };
    expect(response.status).toBe(400);
    expect(body.error).toBe("artifact_upload_unavailable");
    expect(body.message).toContain("5368709120 bytes");
  });
});

describe("fleet run history", () => {
  it("creates early run sessions and appends durable events", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "peter@example.com",
        org: "openclaw",
        provider: "aws",
        serverType: "t3.small",
      }),
    );
    const ownerHeaders = {
      "x-crabbox-owner": "peter@example.com",
      "x-crabbox-org": "openclaw",
    };
    const create = await fleet.fetch(
      request("POST", "/v1/runs", {
        headers: ownerHeaders,
        body: {
          provider: "aws",
          class: "standard",
          serverType: "t3.small",
          command: ["pnpm", "test"],
        },
      }),
    );
    expect(create.status).toBe(201);
    const { run } = (await create.json()) as { run: { id: string; phase: string } };
    expect(run.phase).toBe("starting");

    const attached = await fleet.fetch(
      request("POST", `/v1/runs/${run.id}/events`, {
        headers: ownerHeaders,
        body: {
          type: "lease.created",
          leaseID: "cbx_000000000001",
          slug: "blue-lobster",
          provider: "aws",
          class: "standard",
          serverType: "t3.small",
        },
      }),
    );
    expect(attached.status).toBe(201);

    const stdout = await fleet.fetch(
      request("POST", `/v1/runs/${run.id}/events`, {
        headers: ownerHeaders,
        body: { type: "stdout", stream: "stdout", data: "ok\n" },
      }),
    );
    expect(stdout.status).toBe(201);

    const read = await fleet.fetch(request("GET", `/v1/runs/${run.id}`, { headers: ownerHeaders }));
    const readBody = (await read.json()) as {
      run: { leaseID: string; slug: string; phase: string; eventCount: number };
    };
    expect(readBody.run.leaseID).toBe("cbx_000000000001");
    expect(readBody.run.slug).toBe("blue-lobster");
    expect(readBody.run.phase).toBe("command");
    expect(readBody.run.eventCount).toBe(3);

    const finish = await fleet.fetch(
      request("POST", `/v1/runs/${run.id}/finish`, {
        headers: ownerHeaders,
        body: { exitCode: 0, log: "ok\n" },
      }),
    );
    expect(finish.status).toBe(200);

    const events = await fleet.fetch(
      request("GET", `/v1/runs/${run.id}/events`, { headers: ownerHeaders }),
    );
    const eventsBody = (await events.json()) as {
      events: Array<{ seq: number; type: string; data?: string }>;
    };
    expect(eventsBody.events.map((event) => event.type)).toEqual([
      "run.started",
      "lease.created",
      "stdout",
      "command.finished",
    ]);
    expect(eventsBody.events.map((event) => event.seq)).toEqual([1, 2, 3, 4]);

    const pagedEvents = await fleet.fetch(
      request("GET", `/v1/runs/${run.id}/events?after=1&limit=2`, {
        headers: ownerHeaders,
      }),
    );
    expect(pagedEvents.status).toBe(200);
    const pagedEventsBody = (await pagedEvents.json()) as {
      events: Array<{ seq: number; type: string }>;
    };
    expect(pagedEventsBody.events.map((event) => [event.seq, event.type])).toEqual([
      [2, "lease.created"],
      [3, "stdout"],
    ]);
  });

  it("gives lease owners read-only audit access to shared-user runs", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const ownerHeaders = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const actorHeaders = {
      "x-crabbox-owner": "bob@example.com",
      "x-crabbox-org": "elsewhere",
    };
    const strangerHeaders = {
      "x-crabbox-owner": "stranger@example.com",
      "x-crabbox-org": "elsewhere",
    };
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "alice@example.com",
        org: "example-org",
        share: { users: { "bob@example.com": "use" } },
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );
    storage.seed(
      "lease:cbx_000000000002",
      testLease({
        id: "cbx_000000000002",
        owner: "charlie@example.com",
        org: "another-org",
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );
    storage.seed(
      "lease:cbx_000000000003",
      testLease({
        id: "cbx_000000000003",
        owner: "bob@example.com",
        org: "elsewhere",
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );

    const create = await fleet.fetch(
      request("POST", "/v1/runs", {
        headers: actorHeaders,
        body: {
          leaseID: "cbx_000000000001",
          command: ["pnpm", "test"],
        },
      }),
    );
    expect(create.status).toBe(201);
    const { run } = (await create.json()) as { run: RunRecord };
    expect(run).toMatchObject({
      owner: "bob@example.com",
      org: "elsewhere",
      leaseIDs: ["cbx_000000000001"],
      leaseOwners: [{ owner: "alice@example.com", org: "example-org" }],
    });

    const reattribute = await fleet.fetch(
      request("POST", `/v1/runs/${run.id}/events`, {
        headers: actorHeaders,
        body: {
          type: "lease.created",
          leaseID: "cbx_000000000002",
        },
      }),
    );
    expect(reattribute.status).toBe(404);

    const listed = await fleet.fetch(
      request("GET", "/v1/runs?leaseID=cbx_000000000001", { headers: ownerHeaders }),
    );
    await expect(listed.json()).resolves.toMatchObject({
      runs: [
        {
          id: run.id,
          owner: "bob@example.com",
          leaseOwners: [{ owner: "alice@example.com", org: "example-org" }],
        },
      ],
    });

    const ownerRead = await fleet.fetch(
      request("GET", `/v1/runs/${run.id}`, { headers: ownerHeaders }),
    );
    expect(ownerRead.status).toBe(200);
    const ownerEvents = await fleet.fetch(
      request("GET", `/v1/runs/${run.id}/events`, { headers: ownerHeaders }),
    );
    expect(ownerEvents.status).toBe(200);

    const deniedMutations = await Promise.all(
      (
        [
          ["events", { type: "stdout", stream: "stdout", data: "forged\n" }],
          [
            "telemetry",
            {
              telemetry: {
                capturedAt: "2026-05-01T00:00:10Z",
                source: "ssh-linux",
                load1: 1,
              },
            },
          ],
          ["finish", { exitCode: 0, log: "forged\n" }],
        ] as const
      ).map(([action, body]) =>
        fleet.fetch(
          request("POST", `/v1/runs/${run.id}/${action}`, {
            headers: ownerHeaders,
            body,
          }),
        ),
      ),
    );
    for (const response of deniedMutations) {
      expect(response.status).toBe(404);
    }

    const socket = new FakeWebSocket({
      kind: "control",
      clientID: "owner-audit",
      owner: "alice@example.com",
      org: "example-org",
      subscriptions: {},
    });
    (
      fleet as unknown as {
        controlSockets: Map<string, WebSocket>;
      }
    ).controlSockets.set("owner-audit", socket as unknown as WebSocket);
    await fleet.webSocketMessage(
      socket as unknown as WebSocket,
      JSON.stringify({ type: "subscribe_run", runID: run.id, after: 0 }),
    );
    expect(socket.sentJSON()[0]).toMatchObject({
      type: "run_events",
      runID: run.id,
      events: [{ type: "run.started" }],
    });

    const replacement = await fleet.fetch(
      request("POST", `/v1/runs/${run.id}/events`, {
        headers: actorHeaders,
        body: {
          type: "lease.created",
          leaseID: "cbx_000000000003",
        },
      }),
    );
    expect(replacement.status).toBe(201);
    expect(socket.sentJSON()[1]).toMatchObject({
      type: "run_events",
      runID: run.id,
      events: [{ type: "lease.created", leaseID: "cbx_000000000003" }],
    });
    const originalLeaseHistory = await fleet.fetch(
      request("GET", "/v1/runs?leaseID=cbx_000000000001", { headers: ownerHeaders }),
    );
    await expect(originalLeaseHistory.json()).resolves.toMatchObject({
      runs: [{ id: run.id, leaseID: "cbx_000000000003" }],
    });
    expect(
      (await fleet.fetch(request("GET", `/v1/runs/${run.id}`, { headers: ownerHeaders }))).status,
    ).toBe(200);

    const stdout = await fleet.fetch(
      request("POST", `/v1/runs/${run.id}/events`, {
        headers: actorHeaders,
        body: { type: "stdout", stream: "stdout", data: "ok\n" },
      }),
    );
    expect(stdout.status).toBe(201);
    expect(socket.sentJSON()[2]).toMatchObject({
      type: "run_events",
      runID: run.id,
      events: [{ type: "stdout", data: "ok\n" }],
    });

    const telemetry = await fleet.fetch(
      request("POST", `/v1/runs/${run.id}/telemetry`, {
        headers: actorHeaders,
        body: {
          telemetry: {
            capturedAt: "2026-05-01T00:00:10Z",
            source: "ssh-linux",
            load1: 0.5,
          },
        },
      }),
    );
    expect(telemetry.status).toBe(200);

    const finish = await fleet.fetch(
      request("POST", `/v1/runs/${run.id}/finish`, {
        headers: actorHeaders,
        body: { exitCode: 0, log: "ok\n" },
      }),
    );
    expect(finish.status).toBe(200);

    const ownerLogs = await fleet.fetch(
      request("GET", `/v1/runs/${run.id}/logs`, { headers: ownerHeaders }),
    );
    expect(ownerLogs.status).toBe(200);
    expect(await ownerLogs.text()).toBe("ok\n");

    const leasePage = await fleet.fetch(
      request("GET", "/portal/leases/blue-lobster", { headers: ownerHeaders }),
    );
    expect(leasePage.status).toBe(200);
    expect(await leasePage.text()).toContain(run.id);
    expect(
      (await fleet.fetch(request("GET", `/portal/runs/${run.id}`, { headers: ownerHeaders })))
        .status,
    ).toBe(200);

    const strangerList = await fleet.fetch(
      request("GET", "/v1/runs?leaseID=cbx_000000000001", {
        headers: strangerHeaders,
      }),
    );
    await expect(strangerList.json()).resolves.toEqual({ runs: [] });
    expect(
      (await fleet.fetch(request("GET", `/v1/runs/${run.id}`, { headers: strangerHeaders })))
        .status,
    ).toBe(404);
  });

  it("allows use-share users to create validated lease-attributed runs", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const ownerHeaders = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const actorHeaders = {
      "x-crabbox-owner": "bob@example.com",
      "x-crabbox-org": "elsewhere",
    };
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "alice@example.com",
        org: "example-org",
        share: { users: { "bob@example.com": "use" } },
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );

    const create = await fleet.fetch(
      request("POST", "/v1/runs", {
        headers: actorHeaders,
        body: {
          leaseID: "cbx_000000000001",
          command: ["pnpm", "test"],
        },
      }),
    );
    expect(create.status).toBe(201);
    const { run } = (await create.json()) as { run: RunRecord };
    expect(run).toMatchObject({
      owner: "bob@example.com",
      org: "elsewhere",
      leaseID: "cbx_000000000001",
      leaseIDs: ["cbx_000000000001"],
      leaseOwners: [{ owner: "alice@example.com", org: "example-org" }],
    });

    const ownerList = await fleet.fetch(
      request("GET", "/v1/runs?leaseID=cbx_000000000001", { headers: ownerHeaders }),
    );
    await expect(ownerList.json()).resolves.toMatchObject({
      runs: [{ id: run.id, owner: "bob@example.com" }],
    });
  });

  it("blocks use-share users from re-tagging unrelated runs into lease owner audits", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const ownerHeaders = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const actorHeaders = {
      "x-crabbox-owner": "bob@example.com",
      "x-crabbox-org": "elsewhere",
    };
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "alice@example.com",
        org: "example-org",
        share: { users: { "bob@example.com": "use" } },
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );

    const create = await fleet.fetch(
      request("POST", "/v1/runs", {
        headers: actorHeaders,
        body: {
          command: ["pnpm", "test"],
        },
      }),
    );
    expect(create.status).toBe(201);
    const { run } = (await create.json()) as { run: RunRecord };
    expect(run.leaseID).toBe("");

    const reattribute = await fleet.fetch(
      request("POST", `/v1/runs/${run.id}/events`, {
        headers: actorHeaders,
        body: {
          type: "lease.created",
          leaseID: "cbx_000000000001",
          slug: "blue-lobster",
        },
      }),
    );
    expect(reattribute.status).toBe(404);

    expect(storage.value<RunRecord>(`run:${run.id}`)).toMatchObject({
      leaseID: "",
      leaseIDs: [],
      leaseOwners: [],
      eventCount: 1,
    });
    const ownerList = await fleet.fetch(
      request("GET", "/v1/runs?leaseID=cbx_000000000001", { headers: ownerHeaders }),
    );
    await expect(ownerList.json()).resolves.toEqual({ runs: [] });
    expect(
      (await fleet.fetch(request("GET", `/v1/runs/${run.id}`, { headers: ownerHeaders }))).status,
    ).toBe(404);
  });

  it("reconstructs backing lease audit access for legacy runs", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const ownerHeaders = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        owner: "alice@example.com",
        org: "example-org",
      }),
    );
    storage.seed(
      "lease:cbx_000000000002",
      testLease({
        id: "cbx_000000000002",
        owner: "bob@example.com",
        org: "elsewhere",
      }),
    );
    storage.seed(
      "run:run_000000000001",
      testRun({
        id: "run_000000000001",
        leaseID: "cbx_000000000002",
        owner: "bob@example.com",
        org: "elsewhere",
        eventCount: 2,
      }),
    );
    storage.seed("runevent:run_000000000001:000000000001", {
      runID: "run_000000000001",
      seq: 1,
      type: "lease.created",
      leaseID: "cbx_000000000001",
      createdAt: "2026-05-01T00:00:01.000Z",
    });
    storage.seed("runevent:run_000000000001:000000000002", {
      runID: "run_000000000001",
      seq: 2,
      type: "lease.created",
      leaseID: "cbx_000000000002",
      createdAt: "2026-05-01T00:00:02.000Z",
    });

    const listed = await fleet.fetch(
      request("GET", "/v1/runs?leaseID=cbx_000000000001", { headers: ownerHeaders }),
    );
    await expect(listed.json()).resolves.toMatchObject({
      runs: [{ id: "run_000000000001" }],
    });
    expect(
      (await fleet.fetch(request("GET", "/v1/runs/run_000000000001", { headers: ownerHeaders })))
        .status,
    ).toBe(200);
    expect(storage.value<RunRecord>("run:run_000000000001")).toMatchObject({
      leaseIDs: ["cbx_000000000001", "cbx_000000000002"],
      leaseOwners: [
        { owner: "alice@example.com", org: "example-org" },
        { owner: "bob@example.com", org: "elsewhere" },
      ],
    });
  });

  it("lets admins record a replacement lease outside their owner scope", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        owner: "alice@example.com",
        org: "example-org",
      }),
    );
    storage.seed(
      "run:run_000000000001",
      testRun({
        id: "run_000000000001",
        leaseID: "",
        leaseIDs: [],
        owner: "bob@example.com",
        org: "elsewhere",
        leaseOwners: [],
      }),
    );

    const response = await fleet.fetch(
      request("POST", "/v1/runs/run_000000000001/events", {
        headers: { "x-crabbox-admin": "true" },
        body: { type: "lease.created", leaseID: "cbx_000000000001" },
      }),
    );
    expect(response.status).toBe(201);
    expect(storage.value<RunRecord>("run:run_000000000001")).toMatchObject({
      leaseID: "cbx_000000000001",
      leaseIDs: ["cbx_000000000001"],
      leaseOwners: [{ owner: "alice@example.com", org: "example-org" }],
    });
  });

  it("streams run events and lease heartbeats over a control websocket", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const headers = {
      "x-crabbox-owner": "peter@example.com",
      "x-crabbox-org": "openclaw",
    };
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "peter@example.com",
        org: "openclaw",
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );
    storage.seed(
      "lease:cbx_000000000002",
      testLease({
        id: "cbx_000000000002",
        slug: "fleet-is-201",
        workspaceID: "fleet-is-201",
        owner: "peter@example.com",
        org: "openclaw",
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );
    storage.seed(
      "run:run_000000000001",
      testRun({
        id: "run_000000000001",
        leaseID: "cbx_000000000001",
        owner: "peter@example.com",
        org: "openclaw",
        eventCount: 1,
      }),
    );
    storage.seed("runevent:run_000000000001:000000000001", {
      runID: "run_000000000001",
      seq: 1,
      type: "run.started",
      phase: "starting",
      createdAt: "2026-05-01T00:00:00.000Z",
    });
    const socket = new FakeWebSocket({
      kind: "control",
      clientID: "ctrl_1",
      owner: "peter@example.com",
      org: "openclaw",
      subscriptions: {},
    });
    (
      fleet as unknown as {
        controlSockets: Map<string, WebSocket>;
      }
    ).controlSockets.set("ctrl_1", socket as unknown as WebSocket);

    await fleet.webSocketMessage(
      socket as unknown as WebSocket,
      JSON.stringify({ type: "subscribe_run", runID: "run_000000000001", after: 0 }),
    );
    expect(socket.sentJSON()[0]).toMatchObject({
      type: "run_events",
      runID: "run_000000000001",
      nextSeq: 1,
      events: [{ seq: 1, type: "run.started" }],
    });

    await fleet.fetch(
      request("POST", "/v1/runs/run_000000000001/events", {
        headers,
        body: { type: "stdout", stream: "stdout", data: "ok\n" },
      }),
    );
    expect(socket.sentJSON()[1]).toMatchObject({
      type: "run_events",
      runID: "run_000000000001",
      nextSeq: 2,
      events: [{ seq: 2, type: "stdout", data: "ok\n" }],
    });

    await fleet.webSocketMessage(
      socket as unknown as WebSocket,
      JSON.stringify({ type: "heartbeat", leaseID: "blue-lobster", idleTimeoutSeconds: 900 }),
    );
    expect(socket.sentJSON()[2]).toMatchObject({
      type: "heartbeat",
      leaseID: "cbx_000000000001",
      ok: true,
    });
    expect(storage.value<LeaseRecord>("lease:cbx_000000000001")?.idleTimeoutSeconds).toBe(900);

    await fleet.webSocketMessage(
      socket as unknown as WebSocket,
      JSON.stringify({
        type: "heartbeat",
        leaseID: "cbx_000000000002",
        idleTimeoutSeconds: 900,
      }),
    );
    expect(socket.sentJSON()[3]).toMatchObject({
      type: "heartbeat",
      leaseID: "cbx_000000000002",
      ok: false,
      error: "workspace_managed_lease",
    });
  });

  it("records finished runs and serves logs", async () => {
    const fleet = testFleet();
    const ownerHeaders = {
      "x-crabbox-owner": "peter@example.com",
      "x-crabbox-org": "openclaw",
    };
    const create = await fleet.fetch(
      request("POST", "/v1/runs", {
        headers: ownerHeaders,
        body: {
          leaseID: "cbx_000000000001",
          provider: "aws",
          class: "beast",
          serverType: "c7a.48xlarge",
          command: ["go", "test", "./..."],
        },
      }),
    );
    expect(create.status).toBe(201);
    const { run } = (await create.json()) as { run: { id: string } };

    const finish = await fleet.fetch(
      request("POST", `/v1/runs/${run.id}/finish`, {
        headers: ownerHeaders,
        body: {
          exitCode: 0,
          syncMs: 12,
          commandMs: 34,
          log: "ok\n",
          blockedStage: "unknown",
          retryLikely: "unknown",
          telemetry: {
            start: {
              capturedAt: "2026-05-01T00:00:00Z",
              source: "ssh-linux",
              load1: 0.1,
              memoryUsedBytes: 1024,
              memoryTotalBytes: 2048,
              memoryPercent: 50,
            },
            end: {
              capturedAt: "2026-05-01T00:00:02Z",
              source: "ssh-linux",
              load1: 0.2,
              memoryUsedBytes: 1536,
              memoryTotalBytes: 2048,
              memoryPercent: 75,
            },
          },
          results: {
            format: "junit",
            files: ["junit.xml"],
            suites: 1,
            tests: 2,
            failures: 1,
            errors: 0,
            skipped: 0,
            timeSeconds: 1.2,
            failed: [{ suite: "pkg", name: "fails", kind: "failure" }],
          },
        },
      }),
    );
    expect(finish.status).toBe(200);
    const finished = (await finish.json()) as {
      run: {
        state: string;
        logBytes: number;
        blockedStage?: string;
        retryLikely?: string;
        results?: { tests: number };
        telemetry?: { end?: { load1?: number; memoryPercent?: number } };
      };
    };
    expect(finished.run.state).toBe("succeeded");
    expect(finished.run.logBytes).toBe(3);
    expect(finished.run.blockedStage).toBe("unknown");
    expect(finished.run.retryLikely).toBe("unknown");
    expect(finished.run.results?.tests).toBe(2);
    expect(finished.run.telemetry?.end).toMatchObject({ load1: 0.2, memoryPercent: 75 });

    const listed = await fleet.fetch(
      request("GET", "/v1/runs?leaseID=cbx_000000000001", { headers: ownerHeaders }),
    );
    const listBody = (await listed.json()) as { runs: Array<{ id: string; owner: string }> };
    expect(listBody.runs).toHaveLength(1);
    expect(listBody.runs[0]?.id).toBe(run.id);
    expect(listBody.runs[0]?.owner).toBe("peter@example.com");

    const logs = await fleet.fetch(
      request("GET", `/v1/runs/${run.id}/logs`, { headers: ownerHeaders }),
    );
    expect(await logs.text()).toBe("ok\n");
  });

  it("appends live run telemetry samples and preserves them on finish", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const ownerHeaders = {
      "x-crabbox-owner": "peter@example.com",
      "x-crabbox-org": "openclaw",
    };
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "peter@example.com",
        org: "openclaw",
      }),
    );
    const create = await fleet.fetch(
      request("POST", "/v1/runs", {
        headers: ownerHeaders,
        body: { leaseID: "cbx_000000000001", command: ["sleep", "60"] },
      }),
    );
    const { run } = (await create.json()) as { run: RunRecord };

    const firstSample = await fleet.fetch(
      request("POST", `/v1/runs/${run.id}/telemetry`, {
        headers: ownerHeaders,
        body: {
          telemetry: {
            capturedAt: "2026-05-01T00:00:10Z",
            source: "ssh-linux",
            load1: 0.4,
            memoryPercent: 40,
          },
        },
      }),
    );
    expect(firstSample.status).toBe(200);
    const sampled = (await firstSample.json()) as { run: RunRecord };
    expect(sampled.run.telemetry?.start).toMatchObject({ load1: 0.4, memoryPercent: 40 });
    expect(sampled.run.telemetry?.samples).toHaveLength(1);

    await fleet.fetch(
      request("POST", `/v1/runs/${run.id}/telemetry`, {
        headers: ownerHeaders,
        body: {
          telemetry: {
            capturedAt: "2026-05-01T00:00:20Z",
            source: "ssh-linux",
            load1: 0.9,
            memoryPercent: 55,
          },
        },
      }),
    );

    const finish = await fleet.fetch(
      request("POST", `/v1/runs/${run.id}/finish`, {
        headers: ownerHeaders,
        body: {
          exitCode: 0,
          telemetry: {
            end: {
              capturedAt: "2026-05-01T00:00:30Z",
              source: "ssh-linux",
              load1: 1.2,
              memoryPercent: 60,
            },
          },
        },
      }),
    );
    expect(finish.status).toBe(200);
    const finished = (await finish.json()) as { run: RunRecord };
    expect(finished.run.telemetry?.end).toMatchObject({ load1: 1.2, memoryPercent: 60 });
    expect(finished.run.telemetry?.samples?.map((sample) => sample.load1)).toEqual([0.4, 0.9]);
  });

  it("accepts Go nil slices in passing test results", async () => {
    const fleet = testFleet();
    const create = await fleet.fetch(
      request("POST", "/v1/runs", {
        body: {
          leaseID: "cbx_000000000001",
          provider: "aws",
          class: "beast",
          serverType: "c7a.48xlarge",
          command: ["go", "test", "./..."],
        },
      }),
    );
    expect(create.status).toBe(201);
    const { run } = (await create.json()) as { run: { id: string } };

    const finish = await fleet.fetch(
      request("POST", `/v1/runs/${run.id}/finish`, {
        body: {
          exitCode: 0,
          log: "ok\n",
          results: {
            format: "junit",
            files: null,
            suites: 1,
            tests: 1,
            failures: 0,
            errors: 0,
            skipped: 0,
            timeSeconds: 0.001,
            failed: null,
          },
        },
      }),
    );
    expect(finish.status).toBe(200);
    const finished = (await finish.json()) as {
      run: { results?: { files: string[]; failed: unknown[] } };
    };
    expect(finished.run.results?.files).toEqual([]);
    expect(finished.run.results?.failed).toEqual([]);
  });

  it("records chunked run logs so failures do not disappear from long output", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const create = await fleet.fetch(
      request("POST", "/v1/runs", {
        body: {
          leaseID: "cbx_000000000001",
          provider: "aws",
          class: "beast",
          serverType: "c7a.48xlarge",
          command: ["pnpm", "test"],
        },
      }),
    );
    expect(create.status).toBe(201);
    const { run } = (await create.json()) as { run: { id: string } };
    const chunkA = `${"a".repeat(70_000)}\nFAIL src/example.test.ts\n`;
    const chunkB = `${"b".repeat(70_000)}\nELIFECYCLE Test failed\n`;

    const finish = await fleet.fetch(
      request("POST", `/v1/runs/${run.id}/finish`, {
        body: {
          exitCode: 1,
          log: "fallback tail only\n",
          logChunks: [chunkA, chunkB],
        },
      }),
    );
    expect(finish.status).toBe(200);
    const finished = (await finish.json()) as {
      run: { state: string; logBytes: number; logTruncated: boolean };
    };
    expect(finished.run.state).toBe("failed");
    expect(finished.run.logBytes).toBe(chunkA.length + chunkB.length);
    expect(finished.run.logTruncated).toBe(false);
    expect(storage.value<string>(`runlog:${run.id}`)).toBe("");

    const logs = await fleet.fetch(request("GET", `/v1/runs/${run.id}/logs`));
    const logText = await logs.text();
    expect(logText).toContain("FAIL src/example.test.ts");
    expect(logText).toContain("ELIFECYCLE Test failed");
    expect(logText).not.toContain("fallback tail only");
  });

  it("records resolved lease metadata instead of caller-supplied fallback guesses", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        provider: "aws",
        class: "beast",
        serverType: "c7i.24xlarge",
        owner: "peter@example.com",
        org: "openclaw",
      }),
    );
    const create = await fleet.fetch(
      request("POST", "/v1/runs", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
        body: {
          leaseID: "cbx_000000000001",
          provider: "aws",
          class: "beast",
          serverType: "c7a.48xlarge",
          command: ["go", "test", "./..."],
        },
      }),
    );
    expect(create.status).toBe(201);
    const { run } = (await create.json()) as { run: RunRecord };
    expect(run.provider).toBe("aws");
    expect(run.class).toBe("beast");
    expect(run.serverType).toBe("c7i.24xlarge");
  });

  it("hides run records and logs from other non-admin users", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    storage.seed(
      "run:run_000000000001",
      testRun({
        id: "run_000000000001",
        leaseID: "cbx_000000000001",
        owner: "peter@example.com",
        org: "openclaw",
      }),
    );
    storage.seed("runlog:run_000000000001", "secret log\n");
    storage.seed(
      "run:run_000000000002",
      testRun({
        id: "run_000000000002",
        leaseID: "cbx_000000000002",
        owner: "friend@example.com",
        org: "openclaw",
      }),
    );
    const friendHeaders = {
      "x-crabbox-owner": "friend@example.com",
      "x-crabbox-org": "openclaw",
    };

    const list = await fleet.fetch(request("GET", "/v1/runs", { headers: friendHeaders }));
    const listBody = (await list.json()) as { runs: RunRecord[] };
    expect(listBody.runs.map((run) => run.id)).toEqual(["run_000000000002"]);

    const read = await fleet.fetch(
      request("GET", "/v1/runs/run_000000000001", { headers: friendHeaders }),
    );
    expect(read.status).toBe(404);

    const logs = await fleet.fetch(
      request("GET", "/v1/runs/run_000000000001/logs", { headers: friendHeaders }),
    );
    expect(logs.status).toBe(404);

    const finish = await fleet.fetch(
      request("POST", "/v1/runs/run_000000000001/finish", {
        headers: friendHeaders,
        body: { exitCode: 0, log: "overwrite\n" },
      }),
    );
    expect(finish.status).toBe(404);
    expect(storage.value<string>("runlog:run_000000000001")).toBe("secret log\n");
  });

  it("bounds stored result summaries", async () => {
    const fleet = testFleet();
    const create = await fleet.fetch(
      request("POST", "/v1/runs", {
        body: {
          leaseID: "cbx_000000000001",
          provider: "aws",
          class: "beast",
          serverType: "c7a.48xlarge",
          command: ["go", "test", "./..."],
        },
      }),
    );
    expect(create.status).toBe(201);
    const { run } = (await create.json()) as { run: { id: string } };
    const failed = Array.from({ length: 150 }, (_, index) => ({
      suite: "pkg",
      name: `fails-${index}`,
      kind: "failure" as const,
      message: "x".repeat(5000),
    }));

    const finish = await fleet.fetch(
      request("POST", `/v1/runs/${run.id}/finish`, {
        body: {
          exitCode: 1,
          log: "",
          results: {
            format: "junit",
            files: Array.from({ length: 80 }, (_, index) => `junit-${index}.xml`),
            suites: 1,
            tests: 150,
            failures: 150,
            errors: 0,
            skipped: 0,
            timeSeconds: 1.2,
            failed,
          },
        },
      }),
    );
    expect(finish.status).toBe(200);
    const finished = (await finish.json()) as {
      run: { results?: { files: string[]; failed: Array<{ message?: string }> } };
    };
    expect(finished.run.results?.files).toHaveLength(50);
    expect(finished.run.results?.failed).toHaveLength(100);
    expect(
      new TextEncoder().encode(finished.run.results?.failed[0]?.message ?? "").byteLength,
    ).toBe(4096);
  });
});

describe("fleet identity", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("reports owner and org from request context", async () => {
    const fleet = testFleet();
    const response = await fleet.fetch(
      request("GET", "/v1/whoami", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
      }),
    );
    expect(await response.json()).toEqual({
      owner: "peter@example.com",
      org: "openclaw",
      auth: "bearer",
      admin: false,
    });
  });

  it("reports forwarded GitHub auth mode", async () => {
    const fleet = testFleet();
    const response = await fleet.fetch(
      request("GET", "/v1/whoami", {
        headers: {
          "x-crabbox-auth": "github",
          "x-crabbox-owner": "friend@example.com",
          "x-crabbox-org": "openclaw",
        },
      }),
    );
    expect(await response.json()).toEqual({
      owner: "friend@example.com",
      org: "openclaw",
      auth: "github",
      admin: false,
    });
  });

  it("reports forwarded GitHub token expiry when present", async () => {
    const fleet = testFleet();
    const response = await fleet.fetch(
      request("GET", "/v1/whoami", {
        headers: {
          "x-crabbox-auth": "github",
          "x-crabbox-owner": "friend@example.com",
          "x-crabbox-org": "openclaw",
          "x-crabbox-token-expires-at": "2026-12-05T00:00:00.000Z",
        },
      }),
    );
    expect(await response.json()).toEqual({
      owner: "friend@example.com",
      org: "openclaw",
      auth: "github",
      admin: false,
      tokenExpiresAt: "2026-12-05T00:00:00.000Z",
    });
  });

  it("rejects admin routes without an admin token context", async () => {
    const fleet = testFleet();
    const response = await fleet.fetch(request("GET", "/v1/admin/leases"));
    expect(response.status).toBe(403);
    const macHosts = await fleet.fetch(request("GET", "/v1/admin/mac-hosts"));
    expect(macHosts.status).toBe(403);
  });

  it("renders provider traffic lights on the admin portal page", async () => {
    const storage = new MemoryStorage();
    storage.seed(
      "lease:cbx_aws_admin",
      testLease({
        id: "cbx_aws_admin",
        slug: "aws-admin",
        provider: "aws",
        owner: "alice@example.com",
        org: "example-org",
        serverType: "c7a.large",
      }),
    );
    storage.seed(
      "lease:cbx_azure_admin",
      testLease({
        id: "cbx_azure_admin",
        slug: "azure-admin",
        provider: "azure",
        owner: "bob@example.com",
        org: "example-org",
        serverType: "Standard_D2ads_v6",
        state: "failed",
      }),
    );
    const fleet = testFleet(storage, {
      aws: fakeProvider(undefined, {
        provider: "aws",
        servers: [testMachine({ provider: "aws", cloudID: "i-000000000001" })],
      }),
      hetzner: fakeProvider(undefined, {
        provider: "hetzner",
        onList: async () => {
          throw new Error("hetzner GET /ssh_keys: http 401: token invalid");
        },
      }),
    });

    const response = await fleet.fetch(
      request("GET", "/portal/admin", {
        headers: {
          "x-crabbox-admin": "true",
          "x-crabbox-owner": "admin@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    const body = await response.text();

    expect(response.status).toBe(200);
    expect(body).toContain("provider health");
    expect(body).toContain("4 supported");
    expect(body).toContain("users");
    expect(body).toContain('href="/portal/admin/users"');
    expect(body).not.toContain("all leases");
    expect(body).toContain("attention");
    expect(body).toContain("provider-favicon");
    expect(body).toContain("/v1/providers/aws/readiness");
    expect(body).toContain("disabled>machines</button>");
    expect(body).toContain("/portal/admin/leases?provider=aws");
    expect(body).toContain("AWS");
    expect(body).toContain("Azure");
    expect(body).toContain("GCP");
    expect(body).toContain("Hetzner");
    expect(body).not.toContain("Blacksmith");
    expect(body).toContain("GCP_PROJECT_ID");
    expect(body).not.toContain("alice@example.com");
    expect(body).not.toContain("aws-admin");
    expect(body).not.toContain("azure-admin");
    expect(body).toContain('data-tone="ok"');
    expect(body).toContain('data-tone="bad"');
    expect(body).toContain('data-tone="disabled"');
    expect(body).toContain("token invalid");

    const leasesPage = await fleet.fetch(
      request("GET", "/portal/admin/leases?provider=aws", {
        headers: { "x-crabbox-admin": "true" },
      }),
    );
    const leasesBody = await leasesPage.text();
    expect(leasesPage.status).toBe(200);
    expect(leasesBody).toContain("AWS leases");
    expect(leasesBody).toContain("all providers");
    expect(leasesBody).toContain("aws-admin");
    expect(leasesBody).not.toContain("azure-admin");
    expect(leasesBody).toContain("admin-eject");
    expect(leasesBody).toContain("Emergency release aws-admin");
    expect(leasesBody).toContain(
      "/portal/leases/cbx_aws_admin/release?return=%2Fportal%2Fadmin%2Fleases%3Fprovider%3Daws",
    );

    const usersPage = await fleet.fetch(
      request("GET", "/portal/admin/users", {
        headers: { "x-crabbox-admin": "true" },
      }),
    );
    const usersBody = await usersPage.text();
    expect(usersPage.status).toBe(200);
    expect(usersBody).toContain("bob@example.com");
    expect(usersBody).not.toContain("all leases");

    const portal = await fleet.fetch(
      request("GET", "/portal", {
        headers: {
          "x-crabbox-admin": "true",
          "x-crabbox-owner": "vincentkoc@ieee.org",
          "x-crabbox-org": "openclaw",
        },
      }),
    );
    const portalBody = await portal.text();
    expect(portalBody).toContain('href="/portal/admin"');
    expect(portalBody).toContain("<span>admin</span>");

    const filtered = await fleet.fetch(
      request("GET", "/v1/admin/leases?provider=aws", {
        headers: { "x-crabbox-admin": "true" },
      }),
    );
    const filteredBody = (await filtered.json()) as { leases: LeaseRecord[] };
    expect(filteredBody.leases.map((lease) => lease.provider)).toEqual(["aws"]);

    const release = await fleet.fetch(
      request(
        "POST",
        "/portal/leases/cbx_aws_admin/release?return=%2Fportal%2Fadmin%2Fleases%3Fprovider%3Daws",
        { headers: { "x-crabbox-admin": "true" } },
      ),
    );
    expect(release.status).toBe(303);
    expect(release.headers.get("location")).toBe("/portal/admin/leases?provider=aws");
  });

  it("requires admin access for the admin portal page", async () => {
    const fleet = testFleet();
    const response = await fleet.fetch(request("GET", "/portal/admin"));
    expect(response.status).toBe(403);
  });

  it("audits expired AWS leases against cloud state", async () => {
    const storage = new MemoryStorage();
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "live-lobster",
        provider: "aws",
        cloudID: "i-live",
        region: "eu-west-1",
        state: "expired",
      }),
    );
    storage.seed(
      "lease:cbx_000000000002",
      testLease({
        id: "cbx_000000000002",
        slug: "gone-lobster",
        provider: "aws",
        cloudID: "i-gone",
        region: "eu-west-1",
        state: "expired",
        cleanupAttempts: 1,
        cleanupError: "terminated",
        createdAt: "2026-05-01T00:01:00.000Z",
      }),
    );
    storage.seed(
      "lease:cbx_000000000004",
      testLease({
        id: "cbx_000000000004",
        slug: "terminated-runner",
        provider: "aws",
        cloudID: "i-terminated",
        region: "eu-west-1",
        state: "expired",
        createdAt: "2026-05-01T00:02:00.000Z",
      }),
    );
    storage.seed(
      "lease:cbx_000000000003",
      testLease({
        id: "cbx_000000000003",
        slug: "active-lobster",
        provider: "aws",
        cloudID: "i-active",
        region: "eu-west-1",
        state: "active",
      }),
    );
    const fleet = testFleet(storage, {
      aws: fakeProvider(undefined, { provider: "aws" }, undefined, async (id) => {
        if (id === "i-gone") {
          throw new Error("aws instance not found: i-gone");
        }
        return {
          provider: "aws",
          id: 123,
          cloudID: id,
          region: "eu-west-1",
          name: `crabbox-${id}`,
          status: id === "i-terminated" ? "terminated" : "running",
          serverType: "c7i.2xlarge",
          host: "192.0.2.20",
          labels: {},
        };
      }),
    });

    const response = await fleet.fetch(
      request("GET", "/v1/admin/lease-audit?state=expired&provider=aws", {
        headers: { "x-crabbox-admin": "true" },
      }),
    );
    expect(response.status).toBe(200);
    const body = (await response.json()) as {
      audits: Array<{ leaseID: string; cloudStatus: string; cloudState?: string }>;
    };
    expect(body.audits).toMatchObject([
      { leaseID: "cbx_000000000004", cloudStatus: "missing", cloudState: "terminated" },
      { leaseID: "cbx_000000000002", cloudStatus: "missing" },
      { leaseID: "cbx_000000000001", cloudStatus: "found", cloudState: "running" },
    ]);
  });

  it("audits expired Azure leases against cloud state", async () => {
    const storage = new MemoryStorage();
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "live-azure",
        provider: "azure",
        cloudID: "vm-live",
        region: "eastus",
        state: "expired",
      }),
    );
    storage.seed(
      "lease:cbx_000000000002",
      testLease({
        id: "cbx_000000000002",
        slug: "gone-azure",
        provider: "azure",
        cloudID: "vm-gone",
        region: "eastus",
        state: "expired",
        createdAt: "2026-05-01T00:01:00.000Z",
      }),
    );
    const fleet = testFleet(storage, {
      azure: fakeProvider(undefined, {
        provider: "azure",
        servers: [
          testMachine({
            provider: "azure",
            cloudID: "vm-live",
            name: "vm-live",
            status: "running",
            serverType: "Standard_D16ads_v5",
            host: "192.0.2.30",
            labels: { crabbox: "true", lease: "cbx_000000000001" },
          }),
        ],
      }),
    });

    const response = await fleet.fetch(
      request("GET", "/v1/admin/lease-audit?state=expired&provider=azure", {
        headers: { "x-crabbox-admin": "true" },
      }),
    );

    expect(response.status).toBe(200);
    const body = (await response.json()) as {
      audits: Array<{ leaseID: string; cloudStatus: string; cloudState?: string }>;
    };
    expect(body.audits).toMatchObject([
      { leaseID: "cbx_000000000002", cloudStatus: "missing" },
      { leaseID: "cbx_000000000001", cloudStatus: "found", cloudState: "running" },
    ]);
  });

  it("starts GitHub login and keeps polling secret server-side", async () => {
    const storage = new MemoryStorage();
    const fleet = new FleetDurableObject(
      { storage } as unknown as DurableObjectState,
      {
        CRABBOX_DEFAULT_ORG: "openclaw",
        CRABBOX_GITHUB_CLIENT_ID: "github-client",
        CRABBOX_GITHUB_CLIENT_SECRET: "github-secret",
        CRABBOX_SHARED_TOKEN: "shared",
        CRABBOX_SESSION_SECRET: "session-secret",
      } as Env,
    );
    const pollSecret = "local-poll-secret";
    const start = await fleet.fetch(
      request("POST", "/v1/auth/github/start", {
        body: {
          pollSecretHash: await sha256HexForTest(pollSecret),
          provider: "aws",
        },
      }),
    );
    expect(start.status).toBe(200);
    const body = (await start.json()) as { loginID: string; url: string };
    expect(body.loginID).toMatch(/^login_/);
    const url = new URL(body.url);
    expect(url.origin + url.pathname).toBe("https://github.com/login/oauth/authorize");
    expect(url.searchParams.get("client_id")).toBe("github-client");
    expect(url.searchParams.get("scope")).toBe("read:user user:email read:org");

    const poll = await fleet.fetch(
      request("POST", "/v1/auth/github/poll", {
        body: {
          loginID: body.loginID,
          pollSecret,
        },
      }),
    );
    expect(poll.status).toBe(200);
    await expect(poll.json()).resolves.toMatchObject({ status: "pending" });
  });

  it("reports missing signing material before GitHub login starts", async () => {
    const storage = new MemoryStorage();
    const fleet = new FleetDurableObject(
      { storage } as unknown as DurableObjectState,
      {
        CRABBOX_DEFAULT_ORG: "openclaw",
        CRABBOX_GITHUB_CLIENT_ID: "github-client",
        CRABBOX_GITHUB_CLIENT_SECRET: "github-secret",
        CRABBOX_SHARED_TOKEN: "shared",
      } as Env,
    );

    const start = await fleet.fetch(
      request("POST", "/v1/auth/github/start", {
        body: { pollSecretHash: await sha256HexForTest("local-poll-secret") },
      }),
    );
    expect(start.status).toBe(503);
    await expect(start.json()).resolves.toMatchObject({
      error: "github_session_secret_invalid",
      message: "CRABBOX_SESSION_SECRET is required for signed user tokens",
    });

    const portal = await fleet.fetch(request("GET", "/portal/login"));
    expect(portal.status).toBe(503);
    expect(await portal.text()).toContain(
      "CRABBOX_SESSION_SECRET is required for signed user tokens",
    );
  });

  it("rejects a shared token reused as GitHub signing material", async () => {
    const fleet = new FleetDurableObject(
      { storage: new MemoryStorage() } as unknown as DurableObjectState,
      {
        CRABBOX_DEFAULT_ORG: "openclaw",
        CRABBOX_GITHUB_CLIENT_ID: "github-client",
        CRABBOX_GITHUB_CLIENT_SECRET: "github-secret",
        CRABBOX_SHARED_TOKEN: "shared",
        CRABBOX_SESSION_SECRET: "shared",
      } as Env,
    );

    const start = await fleet.fetch(
      request("POST", "/v1/auth/github/start", {
        body: { pollSecretHash: await sha256HexForTest("local-poll-secret") },
      }),
    );
    expect(start.status).toBe(503);
    await expect(start.json()).resolves.toMatchObject({
      error: "github_session_secret_invalid",
      message: "CRABBOX_SESSION_SECRET must differ from CRABBOX_SHARED_TOKEN",
    });
  });

  it("sets a portal session cookie after GitHub login", async () => {
    const storage = new MemoryStorage();
    const fleet = new FleetDurableObject(
      { storage } as unknown as DurableObjectState,
      {
        CRABBOX_DEFAULT_ORG: "openclaw",
        CRABBOX_GITHUB_CLIENT_ID: "github-client",
        CRABBOX_GITHUB_CLIENT_SECRET: "github-secret",
        CRABBOX_SHARED_TOKEN: "shared",
        CRABBOX_SESSION_SECRET: "session-secret",
      } as Env,
    );
    const start = await fleet.fetch(
      request("GET", "/portal/login?returnTo=/portal/leases/cbx_000000000001/vnc"),
    );
    expect(start.status).toBe(302);
    const location = start.headers.get("location") ?? "";
    const state = new URL(location).searchParams.get("state");
    expect(state).toBeTruthy();

    vi.stubGlobal("fetch", githubFetchMock({ member: true }));
    const callback = await fleet.fetch(
      request("GET", `/v1/auth/github/callback?code=ok&state=${state}`),
    );
    expect(callback.status).toBe(302);
    expect(callback.headers.get("location")).toBe("/portal/leases/cbx_000000000001/vnc");
    expect(callback.headers.get("set-cookie")).toContain("crabbox_session=cbxu_");
  });

  it.each([
    ["CRLF", "/portal/leases/cbx_1/vnc%0d%0aSet-Cookie:%20oops"],
    ["CR", "/portal/leases/cbx_1/vnc%0dSet-Cookie:%20oops"],
    ["LF", "/portal/leases/cbx_1/vnc%0aSet-Cookie:%20oops"],
    ["NUL", "/portal/leases/cbx_1/vnc%00oops"],
    ["DEL", "/portal/leases/cbx_1/vnc%7foops"],
  ])("drops portal return targets containing %s", async (_label, returnTo) => {
    const storage = new MemoryStorage();
    const fleet = new FleetDurableObject(
      { storage } as unknown as DurableObjectState,
      {
        CRABBOX_DEFAULT_ORG: "openclaw",
        CRABBOX_GITHUB_CLIENT_ID: "github-client",
        CRABBOX_GITHUB_CLIENT_SECRET: "github-secret",
        CRABBOX_SHARED_TOKEN: "shared",
        CRABBOX_SESSION_SECRET: "session-secret",
      } as Env,
    );
    const start = await fleet.fetch(request("GET", `/portal/login?returnTo=${returnTo}`));
    expect(start.status).toBe(302);
    const location = start.headers.get("location") ?? "";
    const state = new URL(location).searchParams.get("state");
    expect(state).toBeTruthy();

    vi.stubGlobal("fetch", githubFetchMock({ member: true }));
    const callback = await fleet.fetch(
      request("GET", `/v1/auth/github/callback?code=ok&state=${state}`),
    );
    expect(callback.status).toBe(302);
    expect(callback.headers.get("location")).toBe("/portal");
    expect(callback.headers.get("set-cookie")).toContain("crabbox_session=cbxu_");
  });

  it("clears portal session on logout without restarting OAuth", async () => {
    const fleet = testFleet();
    const logout = await fleet.fetch(request("GET", "/portal/logout"));
    expect(logout.status).toBe(200);
    expect(logout.headers.get("location")).toBeNull();
    expect(logout.headers.get("set-cookie")).toContain("crabbox_session=");
    expect(logout.headers.get("set-cookie")).toContain("Max-Age=0");
    const body = await logout.text();
    expect(body).toContain("Crabbox logged out");
    expect(body).toContain("/portal/login");
  });

  it("cleans expired GitHub login attempts before rate limiting", async () => {
    const storage = new MemoryStorage();
    const fleet = new FleetDurableObject(
      { storage } as unknown as DurableObjectState,
      {
        CRABBOX_DEFAULT_ORG: "openclaw",
        CRABBOX_GITHUB_CLIENT_ID: "github-client",
        CRABBOX_GITHUB_CLIENT_SECRET: "github-secret",
        CRABBOX_SHARED_TOKEN: "shared",
        CRABBOX_SESSION_SECRET: "session-secret",
      } as Env,
    );
    storage.seed("oauth:login_old", {
      id: "login_old",
      state: "state_old",
      pollSecretHash: "0".repeat(64),
      createdAt: "2026-05-01T00:00:00.000Z",
      expiresAt: "2026-05-01T00:00:00.000Z",
    });
    storage.seed("oauth_state:state_old", "login_old");

    const start = await fleet.fetch(
      request("POST", "/v1/auth/github/start", {
        body: {
          pollSecretHash: await sha256HexForTest("new-secret"),
          provider: "aws",
        },
      }),
    );
    expect(start.status).toBe(200);
    expect(storage.value("oauth:login_old")).toBeUndefined();
    expect(storage.value("oauth_state:state_old")).toBeUndefined();
  });

  it("requires GitHub org membership before completing login", async () => {
    const { fleet, loginID, state, pollSecret } = await startGitHubLogin();
    vi.stubGlobal("fetch", githubFetchMock({ member: false }));

    const callback = await fleet.fetch(
      request("GET", `/v1/auth/github/callback?code=ok&state=${state}`),
    );
    expect(callback.status).toBe(403);

    const poll = await fleet.fetch(
      request("POST", "/v1/auth/github/poll", {
        body: {
          loginID,
          pollSecret,
        },
      }),
    );
    expect(poll.status).toBe(400);
    await expect(poll.json()).resolves.toMatchObject({
      status: "failed",
      error: "GitHub user friend is not an active member of openclaw.",
    });
  });

  it("mints GitHub login tokens for allowed org members", async () => {
    const { fleet, loginID, state, pollSecret } = await startGitHubLogin();
    vi.stubGlobal("fetch", githubFetchMock({ member: true }));

    const callback = await fleet.fetch(
      request("GET", `/v1/auth/github/callback?code=ok&state=${state}`),
    );
    expect(callback.status).toBe(200);

    const poll = await fleet.fetch(
      request("POST", "/v1/auth/github/poll", {
        body: {
          loginID,
          pollSecret,
        },
      }),
    );
    expect(poll.status).toBe(200);
    const body = (await poll.json()) as {
      status: string;
      token?: string;
      tokenExpiresAt?: string;
      owner?: string;
      org?: string;
      login?: string;
    };
    expect(body).toMatchObject({
      status: "complete",
      owner: "friend@example.com",
      org: "openclaw",
      login: "friend",
    });
    expect(body.token).toMatch(/^cbxu_/);
    expect(body.tokenExpiresAt).toMatch(/^\d{4}-\d{2}-\d{2}T/);
  });

  it("rejects GitHub login when only public or unverified emails are available", async () => {
    const { fleet, loginID, state, pollSecret } = await startGitHubLogin();
    vi.stubGlobal(
      "fetch",
      githubFetchMock({
        member: true,
        profileEmail: "victim@example.com",
        emails: [{ email: "attacker@example.com", primary: true, verified: false }],
      }),
    );

    const callback = await fleet.fetch(
      request("GET", `/v1/auth/github/callback?code=ok&state=${state}`),
    );
    expect(callback.status).toBe(403);

    const poll = await fleet.fetch(
      request("POST", "/v1/auth/github/poll", {
        body: { loginID, pollSecret },
      }),
    );
    expect(poll.status).toBe(400);
    await expect(poll.json()).resolves.toMatchObject({
      status: "failed",
      error: "GitHub account must have a verified email to use Crabbox.",
    });
  });

  it("does not fall back to a public email when GitHub email lookup fails", async () => {
    const { fleet, loginID, state, pollSecret } = await startGitHubLogin();
    vi.stubGlobal(
      "fetch",
      githubFetchMock({
        member: true,
        profileEmail: "victim@example.com",
        emailStatus: 500,
      }),
    );

    const callback = await fleet.fetch(
      request("GET", `/v1/auth/github/callback?code=ok&state=${state}`),
    );
    expect(callback.status).toBe(500);

    const poll = await fleet.fetch(
      request("POST", "/v1/auth/github/poll", {
        body: { loginID, pollSecret },
      }),
    );
    expect(poll.status).toBe(400);
    await expect(poll.json()).resolves.toMatchObject({
      status: "failed",
      error: "github email lookup failed: 500",
    });
  });

  it("honors configured GitHub user token TTL", async () => {
    const { fleet, loginID, state, pollSecret } = await startGitHubLogin({
      CRABBOX_USER_TOKEN_TTL_SECONDS: "7200",
    });
    vi.stubGlobal("fetch", githubFetchMock({ member: true }));

    const callback = await fleet.fetch(
      request("GET", `/v1/auth/github/callback?code=ok&state=${state}`),
    );
    expect(callback.status).toBe(200);

    const poll = await fleet.fetch(
      request("POST", "/v1/auth/github/poll", {
        body: {
          loginID,
          pollSecret,
        },
      }),
    );
    expect(poll.status).toBe(200);
    const body = (await poll.json()) as {
      status: string;
      token?: string;
      tokenExpiresAt?: string;
    };
    expect(body.status).toBe("complete");
    expect(body.tokenExpiresAt).toMatch(/^\d{4}-\d{2}-\d{2}T/);
    const payload = decodeUserTokenPayload(body.token ?? "");
    expect(payload.exp - payload.iat).toBe(7200);
  });

  it("falls back to the default org when allowed org config is empty", async () => {
    const { fleet, loginID, state, pollSecret } = await startGitHubLogin({
      CRABBOX_DEFAULT_ORG: "example-org",
      CRABBOX_GITHUB_ALLOWED_ORG: " , ",
    });
    vi.stubGlobal("fetch", githubFetchMock({ member: true, org: "example-org" }));

    const callback = await fleet.fetch(
      request("GET", `/v1/auth/github/callback?code=ok&state=${state}`),
    );
    expect(callback.status).toBe(200);

    const poll = await fleet.fetch(
      request("POST", "/v1/auth/github/poll", {
        body: {
          loginID,
          pollSecret,
        },
      }),
    );
    expect(poll.status).toBe(200);
    await expect(poll.json()).resolves.toMatchObject({
      status: "complete",
      org: "example-org",
      login: "friend",
    });
  });

  it("requires configured GitHub team membership before completing login", async () => {
    const { fleet, loginID, state, pollSecret } = await startGitHubLogin({
      CRABBOX_GITHUB_ALLOWED_TEAMS: "maintainers",
    });
    vi.stubGlobal(
      "fetch",
      githubFetchMock({
        member: true,
        teams: [{ slug: "contributors", organization: { login: "openclaw" } }],
      }),
    );

    const callback = await fleet.fetch(
      request("GET", `/v1/auth/github/callback?code=ok&state=${state}`),
    );
    expect(callback.status).toBe(403);

    const poll = await fleet.fetch(
      request("POST", "/v1/auth/github/poll", {
        body: {
          loginID,
          pollSecret,
        },
      }),
    );
    expect(poll.status).toBe(400);
    await expect(poll.json()).resolves.toMatchObject({
      status: "failed",
      error: "GitHub user friend is not a member of an allowed team in openclaw.",
    });
  });

  it("mints GitHub login tokens for allowed team members", async () => {
    const { fleet, loginID, state, pollSecret } = await startGitHubLogin({
      CRABBOX_GITHUB_ALLOWED_TEAMS: "openclaw/maintainers,openclaw/release-captains",
    });
    vi.stubGlobal(
      "fetch",
      githubFetchMock({
        member: true,
        teams: [{ slug: "maintainers", organization: { login: "openclaw" } }],
      }),
    );

    const callback = await fleet.fetch(
      request("GET", `/v1/auth/github/callback?code=ok&state=${state}`),
    );
    expect(callback.status).toBe(200);

    const poll = await fleet.fetch(
      request("POST", "/v1/auth/github/poll", {
        body: {
          loginID,
          pollSecret,
        },
      }),
    );
    expect(poll.status).toBe(200);
    await expect(poll.json()).resolves.toMatchObject({
      status: "complete",
      owner: "friend@example.com",
      org: "openclaw",
      login: "friend",
    });
  });

  it("reports provider secret readiness without exposing secret values", async () => {
    const fleet = testFleet(undefined, {}, { HETZNER_TOKEN: "hcloud-token" });
    const hetzner = await fleet.fetch(request("GET", "/v1/providers/hetzner/readiness"));
    expect(hetzner.status).toBe(200);
    await expect(hetzner.json()).resolves.toMatchObject({
      provider: "hetzner",
      configured: true,
      missing: [],
    });

    const azure = await fleet.fetch(request("GET", "/v1/providers/azure/readiness"));
    expect(azure.status).toBe(200);
    await expect(azure.json()).resolves.toMatchObject({
      provider: "azure",
      configured: false,
      missing: [
        "AZURE_TENANT_ID",
        "AZURE_CLIENT_ID",
        "AZURE_CLIENT_SECRET",
        "AZURE_SUBSCRIPTION_ID",
      ],
    });
  });

  it("reports AWS capacity quota readiness before a lease is requested", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const awsRequest = input instanceof Request ? input : new Request(input, init);
        expect(new URL(awsRequest.url).hostname).toBe("servicequotas.eu-west-1.amazonaws.com");
        expect(awsRequest.headers.get("x-amz-target")).toBe(
          "ServiceQuotasV20190624.GetServiceQuota",
        );
        return new Response(JSON.stringify({ Quota: { Value: 32 } }), {
          headers: { "content-type": "application/json" },
        });
      }),
    );
    const fleet = testFleet(
      undefined,
      {},
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "test",
        CRABBOX_AWS_REGION: "eu-west-1",
      },
    );

    const response = await fleet.fetch(
      request(
        "GET",
        "/v1/providers/aws/readiness?target=linux&class=beast&serverType=c7a.48xlarge&market=spot&fallback=on-demand-after-120s&region=eu-west-1",
      ),
    );

    expect(response.status).toBe(200);
    const body = (await response.json()) as {
      checks?: Array<{ status: string; details: Record<string, string> }>;
    };
    expect(body.checks).toEqual(
      expect.arrayContaining([
        expect.objectContaining({
          status: "warning",
          details: expect.objectContaining({
            market: "spot",
            default_needed_vcpus: "192",
            recommended_class: "standard",
            recommended_type: "c7a.8xlarge",
          }),
        }),
      ]),
    );
  });

  it("rejects path-bearing AWS regions before signed requests", async () => {
    const fetchMock = vi.fn<typeof fetch>();
    vi.stubGlobal("fetch", fetchMock);
    const fleet = testFleet(
      undefined,
      {},
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "secret",
      },
    );

    const readiness = await fleet.fetch(
      request("GET", "/v1/providers/aws/readiness?region=evil.example/"),
    );
    expect(readiness.status).toBe(400);
    await expect(readiness.json()).resolves.toMatchObject({ error: "invalid_region" });

    const responses = await Promise.all(
      [
        { awsRegion: "evil.example/" },
        { capacity: { regions: ["eu-west-1", "evil.example/"] } },
      ].map((body) =>
        fleet.fetch(
          request("POST", "/v1/leases", {
            body: {
              provider: "aws",
              sshPublicKey: "ssh-ed25519 test",
              ...body,
            },
          }),
        ),
      ),
    );
    expect(responses.map((response) => response.status)).toEqual([400, 400]);
    const responseBodies = await Promise.all(responses.map((response) => response.json()));
    for (const body of responseBodies) {
      expect(body).toMatchObject({ error: "invalid_region" });
    }
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("fails brokered Azure leases with provider_not_configured before constructing Azure", async () => {
    const fleet = testFleet();
    const response = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "azure",
          target: "windows",
          windowsMode: "normal",
          class: "standard",
          serverType: "Standard_D16ads_v6",
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );
    expect(response.status).toBe(424);
    await expect(response.json()).resolves.toMatchObject({
      error: "provider_not_configured",
      provider: "azure",
      missing: expect.arrayContaining(["AZURE_TENANT_ID"]),
    });
  });
});

async function startGitHubLogin(env: Partial<Env> = {}): Promise<{
  fleet: FleetDurableObject;
  loginID: string;
  pollSecret: string;
  state: string;
}> {
  const storage = new MemoryStorage();
  const fleet = new FleetDurableObject(
    { storage } as unknown as DurableObjectState,
    {
      CRABBOX_DEFAULT_ORG: "openclaw",
      CRABBOX_GITHUB_CLIENT_ID: "github-client",
      CRABBOX_GITHUB_CLIENT_SECRET: "github-secret",
      CRABBOX_SHARED_TOKEN: "shared",
      CRABBOX_SESSION_SECRET: "session-secret",
      ...env,
    } as Env,
  );
  const pollSecret = "local-poll-secret";
  const start = await fleet.fetch(
    request("POST", "/v1/auth/github/start", {
      body: {
        pollSecretHash: await sha256HexForTest(pollSecret),
        provider: "aws",
      },
    }),
  );
  expect(start.status).toBe(200);
  const body = (await start.json()) as { loginID: string; url: string };
  const url = new URL(body.url);
  const state = url.searchParams.get("state");
  expect(state).toBeTruthy();
  return { fleet, loginID: body.loginID, pollSecret, state: state || "" };
}

function githubFetchMock({
  member,
  org = "openclaw",
  teams = [],
  profileEmail = null,
  emails = [{ email: "friend@example.com", primary: true, verified: true }],
  emailStatus = 200,
}: {
  member: boolean;
  org?: string;
  teams?: Array<{ slug: string; organization: { login: string } }>;
  profileEmail?: string | null;
  emails?: Array<{ email?: string; primary?: boolean; verified?: boolean }>;
  emailStatus?: number;
}) {
  return vi.fn<(input: RequestInfo | URL) => Promise<Response>>(async (input) => {
    const url =
      typeof input === "string" ? input : input instanceof URL ? input.toString() : input.url;
    if (url === "https://github.com/login/oauth/access_token") {
      return jsonResponse({ access_token: "github-access-token" });
    }
    if (url === "https://api.github.com/user") {
      return jsonResponse({ login: "friend", name: "Friendly User", email: profileEmail });
    }
    if (url === "https://api.github.com/user/emails") {
      return jsonResponse(
        emailStatus === 200 ? emails : { message: "email lookup failed" },
        emailStatus,
      );
    }
    if (url === `https://api.github.com/user/memberships/orgs/${encodeURIComponent(org)}`) {
      return member
        ? jsonResponse({ state: "active", organization: { login: org } })
        : jsonResponse({ message: "Not Found" }, 404);
    }
    if (url === "https://api.github.com/user/teams?per_page=100&page=1") {
      return jsonResponse(teams);
    }
    return jsonResponse({ message: `unexpected ${url}` }, 500);
  });
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void } {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((next) => {
    resolve = next;
  });
  return { promise, resolve };
}

function ec2XMLResponse(body: string, status = 200): Response {
  return new Response(body, { status, headers: { "content-type": "application/xml" } });
}

async function requestBodyForTest(input: RequestInfo | URL, init?: RequestInit): Promise<string> {
  if (init?.body !== undefined) {
    return String(init.body);
  }
  if (input instanceof Request) {
    return await input.clone().text();
  }
  return "";
}

function testFleet(
  storage = new MemoryStorage(),
  providers = {},
  env: Partial<Env> = {},
): FleetDurableObject {
  for (const provider of Object.values(providers)) {
    (
      provider as {
        attachStorage?: (storage: MemoryStorage) => void;
      }
    ).attachStorage?.(storage);
  }
  return new FleetDurableObject(
    { storage } as unknown as DurableObjectState,
    { CRABBOX_DEFAULT_ORG: "default-org", ...env } as Env,
    providers,
  );
}

function expiredRuntimeAdapterClaim(adapterID: string) {
  return {
    adapterID,
    owner: "alice@example.com",
    org: "example-org",
    createdAt: "2026-06-01T00:00:00.000Z",
    claimVersion: 1,
    claimState: "provisional",
    claimExpiresAt: new Date(Date.now() - 1_000).toISOString(),
  };
}

function fakeProvider(
  onCreate?: (config: LeaseConfig) => Promise<void> | void,
  result: {
    provider?: "hetzner" | "aws" | "azure" | "gcp";
    serverType?: string;
    hostID?: string;
    cloudID?: string;
    providerKey?: string;
    region?: string;
    imageRegion?: string;
    market?: string;
    attempts?: ProvisioningAttempt[];
    servers?: ProviderMachine[];
    onList?: () => Promise<ProviderMachine[]> | ProviderMachine[];
    onCreateImage?: (
      instanceID: string,
      name: string,
      strategy?: "image" | "disk-snapshot",
    ) => void;
    onGetImage?: (imageID: string, kind?: string) => Promise<ProviderImage> | ProviderImage;
    onDeleteImage?: (imageID: string, kind?: string) => void;
    onEnableFastSnapshotRestore?: (snapshotIDs: string[], availabilityZones: string[]) => void;
    onFastSnapshotRestoreStatus?: (
      snapshotIDs: string[],
      availabilityZones: string[] | undefined,
    ) => ProviderFastSnapshotRestore[];
    onPrepareLeaseCreate?: (
      config: LeaseConfig,
      lease: LeaseRecord,
      context: { requestSourceCIDRs: string[]; activeLeases: LeaseRecord[] },
    ) =>
      | {
          config: LeaseConfig;
          lease: LeaseRecord;
          provisioning?: {
            sshIngressReconcile?: "authoritative" | "additive";
            publishAccessBeforeProvisioning?: boolean;
          };
        }
      | Promise<
          | {
              config: LeaseConfig;
              lease: LeaseRecord;
              provisioning?: {
                sshIngressReconcile?: "authoritative" | "additive";
                publishAccessBeforeProvisioning?: boolean;
              };
            }
          | undefined
        >
      | undefined;
    onRefreshLeaseAccess?: (
      lease: LeaseRecord,
      context: { requestSourceCIDRs: string[]; activeLeases: LeaseRecord[] },
    ) => LeaseRecord | undefined;
    onReconcileLeaseAccess?: (
      lease: LeaseRecord,
      context: { requestSourceCIDRs: string[]; activeLeases: LeaseRecord[] },
    ) => Promise<void> | void;
    onCreateProvisioning?: (provisioning?: {
      sshIngressReconcile?: "authoritative" | "additive";
      publishAccessBeforeProvisioning?: boolean;
      onTargetAttempt?: (target: { region?: string }) => Promise<void>;
    }) => Promise<void> | void;
    onPrepareLeaseConfig?: (
      config: LeaseConfig,
      storage: MemoryStorage | undefined,
    ) => Promise<LeaseConfig> | LeaseConfig;
    onRestrictedLeaseRequestFields?: (input: LeaseRequest) => string[];
    onFinalizeLeaseCreate?: (
      config: LeaseConfig,
      lease: LeaseRecord,
      server: ProviderMachine,
      attempts: ProvisioningAttempt[],
    ) =>
      | Promise<{ config: LeaseConfig; lease: LeaseRecord } | undefined>
      | {
          config: LeaseConfig;
          lease: LeaseRecord;
        }
      | undefined;
    onReleaseLease?: (lease: LeaseRecord) => Promise<void> | void;
    onRecoverServer?: (
      lease: LeaseRecord,
    ) => Promise<ProviderMachine | undefined> | ProviderMachine | undefined;
  } = {},
  onDelete?: (id: string) => Promise<void>,
  onGet?: (id: string) => Promise<ProviderMachine> | ProviderMachine,
) {
  let storage: MemoryStorage | undefined;
  return {
    attachStorage(nextStorage: MemoryStorage) {
      storage = nextStorage;
    },
    async listCrabboxServers() {
      if (result.onList) {
        return await result.onList();
      }
      return result.servers ?? [];
    },
    restrictedLeaseRequestFields(input: LeaseRequest) {
      return result.onRestrictedLeaseRequestFields?.(input) ?? [];
    },
    ...(result.onRecoverServer
      ? {
          async recoverServer(lease: LeaseRecord) {
            return await result.onRecoverServer?.(lease);
          },
        }
      : {}),
    async findServerByLease(leaseID: string) {
      const servers = result.onList ? await result.onList() : (result.servers ?? []);
      return servers.find((server) => server.labels?.["lease"] === leaseID);
    },
    async getServer(id: string) {
      if (onGet) {
        return await onGet(id);
      }
      return {
        provider: result.provider ?? "hetzner",
        id: 123,
        cloudID: id,
        name: `crabbox-${id}`,
        status: "running",
        serverType: result.serverType ?? "cpx62",
        ...(result.hostID ? { hostID: result.hostID } : {}),
        host: "192.0.2.10",
        labels: {},
      };
    },
    async prepareLeaseCreate(
      config: LeaseConfig,
      lease: LeaseRecord,
      context: { requestSourceCIDRs: string[]; activeLeases: LeaseRecord[] },
    ) {
      return result.onPrepareLeaseCreate?.(config, lease, context);
    },
    async prepareLeaseConfig(config: LeaseConfig) {
      const prepared = await result.onPrepareLeaseConfig?.(config, storage);
      if (prepared) {
        return prepared;
      }
      if (
        (result.provider ?? config.provider) !== "aws" ||
        config.awsAMI ||
        config.awsSnapshot ||
        config.awsUseStockImage ||
        config.providerKey.startsWith(workspaceProviderKeyPrefix)
      ) {
        return config;
      }
      if (config.target === "macos") {
        const awsPromotedAMIs: Record<string, string> = {};
        const promoted = await storage?.list<ProviderImage>({ prefix: "image:aws:promoted" });
        for (const image of promoted?.values() ?? []) {
          if (image.target !== "macos" || !image.region || !image.serverType) {
            continue;
          }
          awsPromotedAMIs[awsPromotedAMIConfigKey(image.region, image.serverType)] = image.id;
        }
        return { ...config, awsPromotedAMIs };
      }
      const promoted =
        (await storage?.get<ProviderImage>(
          fakePromotedAWSImageKey({
            target: config.target,
            os: config.os,
            architecture: fakeAWSImageArchitectureForLease(
              config.target,
              config.serverType,
              config.architecture,
            ),
            region: config.awsRegion,
          }),
        )) ??
        (config.os
          ? await storage?.get<ProviderImage>(
              `image:aws:promoted:linux:${fakeAWSImageArchitectureForLease(config.target, config.serverType, config.architecture)}:${config.os.replaceAll(/[^a-z0-9._-]/g, "")}`,
            )
          : undefined) ??
        ((!config.os || config.os === "ubuntu:24.04") &&
        fakeAWSImageArchitectureForLease(config.target, config.serverType, config.architecture) ===
          "x86_64"
          ? await storage?.get<ProviderImage>("image:aws:promoted")
          : undefined);
      return {
        ...config,
        awsAMI: promoted?.id ?? "",
        ...(promoted?.region ? { awsRegion: promoted.region } : {}),
      };
    },
    async refreshLeaseAccess(
      lease: LeaseRecord,
      context: { requestSourceCIDRs: string[]; activeLeases: LeaseRecord[] },
    ) {
      return result.onRefreshLeaseAccess?.(lease, context);
    },
    async reconcileLeaseAccess(
      lease: LeaseRecord,
      context: { requestSourceCIDRs: string[]; activeLeases: LeaseRecord[] },
    ) {
      await result.onReconcileLeaseAccess?.(lease, context);
    },
    async createServerWithFallback(
      config: LeaseConfig,
      _leaseID: string,
      slug: string,
      _owner: string,
      provisioning?: {
        sshIngressReconcile?: "authoritative" | "additive";
        publishAccessBeforeProvisioning?: boolean;
        onTargetAttempt?: (target: { region?: string }) => Promise<void>;
      },
    ) {
      await result.onCreateProvisioning?.(provisioning);
      await onCreate?.(config);
      return {
        server: {
          provider: result.provider ?? "hetzner",
          id: 123,
          cloudID: result.cloudID ?? "123",
          ...(result.providerKey ? { providerKey: result.providerKey } : {}),
          name: `crabbox-${slug}`,
          status: "running",
          serverType: result.serverType ?? "cpx62",
          ...(result.hostID ? { hostID: result.hostID } : {}),
          host: "192.0.2.10",
          region:
            result.provider === "aws"
              ? (result.region ?? "eu-west-2")
              : result.provider === "gcp"
                ? (result.region ?? "us-central1-a")
                : result.provider === "azure"
                  ? (result.region ?? "eastus")
                  : undefined,
          labels: {},
        },
        serverType: result.serverType ?? "cpx62",
        market: result.market,
        attempts: result.attempts,
      };
    },
    async deleteServer(id: string) {
      await onDelete?.(id);
    },
    async releaseLease(lease: LeaseRecord) {
      await result.onReleaseLease?.(lease);
      await onDelete?.(
        (lease.provider ?? result.provider) === "hetzner" ? String(lease.serverID) : lease.cloudID,
      );
    },
    async finalizeLeaseCreate(
      config: LeaseConfig,
      lease: LeaseRecord,
      server: ProviderMachine,
      attempts: ProvisioningAttempt[],
    ) {
      const finalized = await result.onFinalizeLeaseCreate?.(config, lease, server, attempts);
      if (finalized) {
        return finalized;
      }
      const provider = result.provider ?? lease.provider;
      const nextLease = { ...lease };
      if (provider === "aws") {
        nextLease.region = server.region ?? config.awsRegion;
        const codes = new Set(attempts.map((attempt) => attempt.category));
        if (codes.has("capacity") || codes.has("quota") || result.market === "on-demand") {
          const regionsTried = [
            ...new Set(
              [...attempts.map((attempt) => attempt.region), server.region].filter(Boolean),
            ),
          ];
          nextLease.capacityHints = [
            {
              code: "aws_capacity_routed",
              message: "AWS capacity fallback selected a working launch path",
              regionsTried,
            },
            { code: "aws_quota_pressure", message: "AWS quota or capacity pressure was observed" },
            { code: "aws_on_demand_fallback", message: "AWS on-demand fallback was used" },
            { code: "capacity_large_class", message: "Large class capacity can be constrained" },
          ];
        }
      }
      if (provider === "azure") {
        nextLease.region = config.azureLocation;
      }
      if (provider === "gcp") {
        nextLease.region = server.region ?? config.gcpZone;
        nextLease.providerProject = config.gcpProject;
      }
      return { config, lease: nextLease };
    },
    supportsNativeImages() {
      return (result.provider ?? "aws") !== "hetzner";
    },
    nativeImagesUnsupportedMessage() {
      return "native images are supported for AWS, Azure, and GCP leases";
    },
    defaultImageStrategy() {
      return (result.provider ?? "aws") === "aws" ? "image" : "disk-snapshot";
    },
    validateLeaseImageStrategy(_lease: LeaseRecord, strategy: "image" | "disk-snapshot") {
      return (result.provider ?? "aws") === "azure" && strategy === "image"
        ? "Azure managed images require a stopped/generalized source VM; use disk-snapshot checkpoints for active Azure leases"
        : undefined;
    },
    async createLeaseImage(
      lease: LeaseRecord,
      name: string,
      noReboot = true,
      strategy: "image" | "disk-snapshot" = "disk-snapshot",
    ) {
      const image = await this.createImage(
        lease.cloudID,
        fakeProviderImageResourceName(lease.provider, name, lease.id),
        noReboot,
        strategy,
      );
      const enriched =
        lease.provider === "aws"
          ? fakeMergeAWSImageMetadata(image, {
              target: lease.target,
              os: lease.os,
              windowsMode: lease.windowsMode,
              serverType: lease.serverType,
              region: image.region ?? lease.region,
            })
          : image;
      await storage?.put(
        `image:${lease.provider}:created:${encodeURIComponent(enriched.id)}`,
        enriched,
      );
      if (enriched.resourceID && enriched.resourceID !== enriched.id) {
        await storage?.put(
          `image:${lease.provider}:created:${encodeURIComponent(enriched.resourceID)}`,
          enriched,
        );
      }
      if (lease.provider === "aws") {
        await storage?.put(`image:aws:created:${enriched.id}`, enriched);
      }
      return enriched;
    },
    async createImage(
      instanceID: string,
      name: string,
      _noReboot = true,
      strategy: "image" | "disk-snapshot" = "disk-snapshot",
    ) {
      result.onCreateImage?.(instanceID, name, strategy);
      const provider = result.provider ?? "aws";
      const imageID =
        provider === "azure"
          ? "checkpoint-azure"
          : provider === "gcp"
            ? "checkpoint-gcp"
            : strategy === "disk-snapshot"
              ? "snap-000000000001"
              : "ami-000000000001";
      return {
        id: imageID,
        name,
        state: "pending",
        provider,
        kind:
          strategy === "disk-snapshot"
            ? provider === "azure"
              ? "azure-os-disk-snapshot"
              : provider === "gcp"
                ? "gcp-disk-snapshot"
                : "aws-ebs-snapshot"
            : provider === "azure"
              ? "azure-managed-image"
              : provider === "gcp"
                ? "gcp-machine-image"
                : "aws-ami",
        region: result.imageRegion ?? "eu-west-1",
        project: provider === "gcp" ? "proj" : undefined,
        resourceID:
          strategy === "disk-snapshot"
            ? provider === "azure"
              ? `/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/snapshots/${imageID}`
              : provider === "gcp"
                ? `projects/proj/global/snapshots/${imageID}`
                : imageID
            : provider === "azure"
              ? `/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/images/${imageID}`
              : provider === "gcp"
                ? `projects/proj/global/machineImages/${imageID}`
                : imageID,
        snapshots: [imageID],
      };
    },
    async getImage(imageID: string, kind?: string) {
      if (result.onGetImage) {
        return await result.onGetImage(imageID, kind);
      }
      const provider = result.provider ?? "aws";
      return {
        id: imageID,
        name: "crabbox-runner-test",
        state: "available",
        provider,
        kind:
          kind ??
          (provider === "azure"
            ? "azure-managed-image"
            : provider === "gcp"
              ? "gcp-machine-image"
              : "aws-ami"),
        region: result.imageRegion ?? "eu-west-1",
        project: provider === "gcp" ? "proj" : undefined,
        snapshots: ["snap-000000000001"],
      };
    },
    async deleteImage(imageID: string, kind?: string) {
      result.onDeleteImage?.(imageID, kind);
    },
    async storedImageMetadata(imageID: string) {
      const provider = result.provider ?? "aws";
      const promoted = await storage?.list<ProviderImage>({ prefix: "image:aws:promoted" });
      return (
        (provider === "aws"
          ? [...(promoted?.values() ?? [])].find((image) => image.id === imageID)
          : undefined) ??
        (await storage?.get<ProviderImage>(
          `image:${provider}:created:${encodeURIComponent(imageID)}`,
        )) ??
        (provider === "aws"
          ? await storage?.get<ProviderImage>(`image:aws:created:${imageID}`)
          : undefined)
      );
    },
    decorateImage(image: ProviderImage, metadata?: Partial<ProviderImage>) {
      return (result.provider ?? "aws") === "aws"
        ? fakeMergeAWSImageMetadata(image, metadata)
        : image;
    },
    async validateDeleteImage(imageID: string, metadata?: Partial<ProviderImage>) {
      if (metadata?.id === imageID && "promotedAt" in metadata) {
        return {
          status: 409,
          body: {
            error: "image_promoted",
            message: `image ${imageID} is the promoted AWS image; promote another image before deleting it`,
          },
        };
      }
      if (metadata?.id !== imageID && metadata?.resourceID !== imageID) {
        return {
          status: 409,
          body: {
            error: "image_not_owned",
            message: `refusing to delete ${result.provider ?? "aws"} image ${imageID}: no Crabbox-created image metadata found`,
          },
        };
      }
      return undefined;
    },
    async fastSnapshotRestoreForImage(
      imageID: string,
      metadata: ProviderImage | undefined,
      url: URL,
    ) {
      const image = fakeMergeAWSImageMetadata(await this.getImage(imageID), metadata);
      const snapshots = image.snapshots ?? [];
      if (snapshots.length === 0) {
        return jsonResponse(
          {
            error: "image_snapshots_missing",
            message: `image ${imageID} has no EBS snapshots to describe for Fast Snapshot Restore`,
          },
          409,
        );
      }
      const zones = fakeFastSnapshotRestoreStatusAZs(url, image.region ?? "");
      const fastSnapshotRestores = await this.fastSnapshotRestoreStatus(snapshots, zones);
      return { image: { ...image, fastSnapshotRestores }, fastSnapshotRestores };
    },
    async promoteImage(imageID: string, known: ProviderImage | undefined, req: Request, url: URL) {
      const input = (await req
        .clone()
        .json()
        .catch(() => ({}))) as {
        target?: string;
        os?: string;
        region?: string;
        serverType?: string;
        architecture?: string;
        fastSnapshotRestore?: unknown;
        fastSnapshotRestoreAvailabilityZones?: string[];
      };
      const target = fakeNormalizeAWSImageTarget(
        input.target ?? url.searchParams.get("target") ?? known?.target ?? "linux",
      );
      if (!target) {
        return jsonResponse(
          { error: "invalid_target", message: "target must be linux, macos, or windows" },
          400,
        );
      }
      const requestedOS = input.os ?? url.searchParams.get("os");
      const os =
        target === "linux"
          ? fakeNormalizeOSImage(requestedOS ?? (known ? (known.os ?? "ubuntu:24.04") : undefined))
          : undefined;
      const region = input.region ?? url.searchParams.get("region") ?? known?.region ?? "";
      const metadata: Partial<ProviderImage> = { ...known, target, ...(os ? { os } : {}), region };
      const serverType =
        input.serverType ?? url.searchParams.get("serverType") ?? known?.serverType;
      if (serverType) {
        metadata.serverType = serverType;
      }
      const architecture =
        input.architecture ?? url.searchParams.get("architecture") ?? known?.architecture;
      if (architecture) {
        metadata.architecture = architecture;
      }
      const fastSnapshotRestore = fakeBoolFromUnknown(
        input.fastSnapshotRestore ?? url.searchParams.get("fastSnapshotRestore"),
      );
      const zones = fastSnapshotRestore
        ? fakeFastSnapshotRestoreAZs(input.fastSnapshotRestoreAvailabilityZones, url, region)
        : [];
      if (fastSnapshotRestore && zones.length === 0) {
        return jsonResponse({ error: "invalid_fast_snapshot_restore_zones" }, 400);
      }
      const image = fakeMergeAWSImageMetadata(await this.getImage(imageID), metadata);
      if (image.state !== "available") {
        return jsonResponse({ error: "image_not_available" }, 409);
      }
      if (target === "macos" && !image.serverType) {
        return jsonResponse({ error: "invalid_server_type" }, 400);
      }
      if (zones.length > 0 && (image.snapshots ?? []).length === 0) {
        return jsonResponse({ error: "image_snapshots_missing" }, 409);
      }
      const fastSnapshotRestores =
        zones.length > 0
          ? await this.enableFastSnapshotRestore(image.snapshots ?? [], zones)
          : undefined;
      const promoted = {
        ...image,
        ...(fastSnapshotRestores ? { fastSnapshotRestores } : {}),
        target,
        region: image.region ?? region,
        architecture:
          image.architecture ?? fakeAWSImageArchitectureForTarget(target, image.serverType ?? ""),
        promotedAt: "2026-05-25T00:00:00.000Z",
      };
      await storage?.put(fakePromotedAWSImageKey(promoted), promoted);
      if (target === "linux" && promoted.os) {
        await storage?.put(
          `image:aws:promoted:linux:${promoted.architecture}:${promoted.os.replaceAll(/[^a-z0-9._-]/g, "")}`,
          promoted,
        );
      }
      if (
        target === "linux" &&
        (!promoted.os || promoted.os === "ubuntu:24.04") &&
        fakeLegacyPromotedAWSImageCompatible(promoted)
      ) {
        await storage?.put("image:aws:promoted", promoted);
      }
      return { image: promoted };
    },
    async enableFastSnapshotRestore(snapshotIDs: string[], availabilityZones: string[]) {
      result.onEnableFastSnapshotRestore?.(snapshotIDs, availabilityZones);
      return snapshotIDs.flatMap((snapshotID) =>
        availabilityZones.map((availabilityZone) => ({
          snapshotID,
          availabilityZone,
          state: "enabling",
        })),
      );
    },
    async fastSnapshotRestoreStatus(snapshotIDs: string[], availabilityZones?: string[]) {
      if (result.onFastSnapshotRestoreStatus) {
        return result.onFastSnapshotRestoreStatus(snapshotIDs, availabilityZones);
      }
      return [];
    },
    async deleteSSHKey() {},
    async hourlyPriceUSD() {
      return 0.1;
    },
  };
}

function fakeProviderImageResourceName(provider: string, name: string, leaseID: string): string {
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

function fakeMergeAWSImageMetadata(
  image: ProviderImage,
  metadata?: Partial<ProviderImage>,
): ProviderImage {
  const target =
    fakeNormalizeAWSImageTarget(metadata?.target ?? image.target ?? "linux") ?? "linux";
  const serverType = metadata?.serverType ?? image.serverType ?? "";
  const result: ProviderImage = {
    ...metadata,
    ...image,
    target,
    architecture:
      metadata?.architecture ??
      image.architecture ??
      fakeAWSImageArchitectureForTarget(target, serverType),
  };
  const windowsMode = metadata?.windowsMode ?? image.windowsMode;
  if (windowsMode !== undefined) {
    result.windowsMode = windowsMode;
  }
  if (serverType) {
    result.serverType = serverType;
  }
  if (image.region) {
    result.region = image.region;
  } else if (metadata?.region) {
    result.region = metadata.region;
  }
  return result;
}

function fakeNormalizeAWSImageTarget(
  value: string | undefined,
): "linux" | "macos" | "windows" | undefined {
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

function fakeAWSImageArchitectureForTarget(
  target: "linux" | "macos" | "windows",
  serverType: string,
): string {
  if (target === "macos") {
    return serverType.startsWith("mac1.") ? "x86_64_mac" : "arm64_mac";
  }
  return "x86_64";
}

function fakeAWSImageArchitectureForLease(
  target: "linux" | "macos" | "windows",
  serverType: string,
  architecture?: string,
): string {
  if (target === "linux" && architecture === "arm64") {
    return "arm64";
  }
  return fakeAWSImageArchitectureForTarget(target, serverType);
}

function fakeLegacyPromotedAWSImageCompatible(image: Pick<ProviderImage, "architecture">): boolean {
  return !image.architecture || image.architecture === "x86_64";
}

function fakeNormalizeOSImage(value: string | undefined | null): string {
  let normalized = (value ?? "").trim().toLowerCase();
  if (!normalized) {
    normalized = "ubuntu:26.04";
  }
  normalized = normalized.replaceAll("_", ".").replaceAll("-", ":");
  if (normalized === "ubuntu2604" || normalized === "ubuntu:2604") {
    return "ubuntu:26.04";
  }
  if (normalized === "ubuntu2404" || normalized === "ubuntu:2404") {
    return "ubuntu:24.04";
  }
  return normalized;
}

function fakePromotedAWSImageKey(
  image: Pick<ProviderImage, "target" | "architecture" | "region" | "serverType" | "os">,
): string {
  const target = image.target ?? "linux";
  const architecture = image.architecture ?? fakeAWSImageArchitectureForTarget(target, "");
  const region = image.region ?? "";
  if (target === "macos") {
    return `image:aws:promoted:${target}:${architecture}:${(image.serverType ?? "").trim().toLowerCase()}:${region}`;
  }
  if (target === "linux" && image.os) {
    return `image:aws:promoted:${target}:${architecture}:${(image.os ?? "").replaceAll(/[^a-z0-9._-]/g, "")}:${region}`;
  }
  return `image:aws:promoted:${target}:${architecture}:${region}`;
}

function fakeBoolFromUnknown(value: unknown): boolean {
  if (value === true) return true;
  if (value === false || value === undefined || value === null) return false;
  return ["1", "true", "yes", "on"].includes(String(value).trim().toLowerCase());
}

function fakeFastSnapshotRestoreAZs(
  inputZones: string[] | undefined,
  url: URL,
  region: string,
): string[] {
  return [
    ...new Set(
      [...(inputZones ?? []), ...url.searchParams.getAll("fsrAz")]
        .map((zone) => zone.trim())
        .filter((zone) => !region || zone.startsWith(region)),
    ),
  ];
}

function fakeFastSnapshotRestoreStatusAZs(url: URL, region: string): string[] {
  return [
    ...new Set(
      [...url.searchParams.getAll("fsrAz"), ...url.searchParams.getAll("az")]
        .map((zone) => zone.trim())
        .filter((zone) => !region || zone.startsWith(region)),
    ),
  ];
}

function testMachine(overrides: Partial<ProviderMachine>): ProviderMachine {
  return {
    provider: "aws",
    id: 123,
    cloudID: "i-000000000000",
    name: "crabbox-test",
    status: "running",
    serverType: "t3.small",
    host: "192.0.2.10",
    labels: { crabbox: "true" },
    ...overrides,
  };
}

function testLease(overrides: Partial<LeaseRecord>): LeaseRecord {
  return {
    id: "cbx_000000000000",
    provider: "hetzner",
    cloudID: "123",
    owner: "peter@example.com",
    org: "openclaw",
    profile: "default",
    class: "beast",
    serverType: "ccx63",
    serverID: 123,
    serverName: "crabbox-blue-lobster",
    providerKey: "crabbox-cbx-000000000000",
    host: "192.0.2.1",
    sshUser: "crabbox",
    sshPort: "2222",
    sshFallbackPorts: ["22"],
    workRoot: "/work/crabbox",
    keep: true,
    ttlSeconds: 5400,
    estimatedHourlyUSD: 1,
    maxEstimatedUSD: 1.5,
    state: "active",
    createdAt: "2026-05-01T00:00:00.000Z",
    updatedAt: "2026-05-01T00:00:00.000Z",
    expiresAt: "2026-05-01T01:30:00.000Z",
    ...overrides,
  };
}

type CodePortalRuntime = {
  elements: Record<string, DOMElementStub>;
  fetches: string[];
  timers: Array<{ delay: number }>;
};

type DOMElementStub = {
  dataset: Record<string, string>;
  textContent: string;
  disabled: boolean;
  addEventListener: ReturnType<typeof vi.fn>;
};

function elementStub(textContent = ""): DOMElementStub {
  return {
    dataset: {},
    textContent,
    disabled: false,
    addEventListener: vi.fn<() => void>(),
  };
}

async function runCodePortalScript(
  page: string,
  response: { ok: boolean; status: number; json: () => Promise<unknown> },
): Promise<CodePortalRuntime> {
  const script = inlineScript(page, 'const status = document.getElementById("code-status")');
  const elements: Record<string, DOMElementStub> = {
    "code-status": elementStub("checking bridge"),
    "code-hint": elementStub("Run the command below."),
    "code-reload": elementStub(),
    "code-copy": elementStub(),
    "code-bridge-cmd": elementStub("crabbox code --id blue-lobster --open"),
  };
  const fetches: string[] = [];
  const timers: Array<{ delay: number }> = [];
  const context = createContext({
    URL,
    document: {
      getElementById: (id: string) => elements[id] ?? null,
      createRange: () => ({ selectNodeContents: vi.fn<(element: unknown) => void>() }),
    },
    fetch: vi.fn<(url: URL) => Promise<typeof response>>(async (url) => {
      fetches.push(url.toString());
      return response;
    }),
    navigator: { clipboard: { writeText: vi.fn<(text: string) => Promise<void>>() } },
    window: {
      location: { href: "https://example.test/portal/leases/blue-lobster/code/" },
      clearTimeout: vi.fn<(timer?: unknown) => void>(),
      setTimeout: (_callback: () => void, delay: number) => {
        timers.push({ delay });
        return timers.length;
      },
      addEventListener: vi.fn<() => void>(),
      getSelection: () => ({
        removeAllRanges: vi.fn<() => void>(),
        addRange: vi.fn<(range: unknown) => void>(),
      }),
    },
  });

  new Script(script).runInContext(context);
  await new Promise((resolve) => setTimeout(resolve, 0));
  await Promise.resolve();
  return { elements, fetches, timers };
}

function inlineScript(page: string, marker: string): string {
  let cursor = 0;
  while (cursor < page.length) {
    const open = page.indexOf("<script", cursor);
    if (open < 0) {
      break;
    }
    const bodyStart = page.indexOf(">", open);
    if (bodyStart < 0) {
      break;
    }
    const close = page.indexOf("</script>", bodyStart + 1);
    if (close < 0) {
      break;
    }
    const body = page.slice(bodyStart + 1, close);
    if (body.includes(marker)) {
      return body;
    }
    cursor = close + "</script>".length;
  }
  throw new Error(`script marker not found: ${marker}`);
}

function testRun(overrides: Partial<RunRecord>): RunRecord {
  return {
    id: "run_000000000000",
    leaseID: "cbx_000000000000",
    owner: "peter@example.com",
    org: "openclaw",
    provider: "hetzner",
    class: "standard",
    serverType: "cpx62",
    command: ["echo", "ok"],
    state: "running",
    logBytes: 0,
    logTruncated: false,
    startedAt: "2026-05-01T00:00:00.000Z",
    ...overrides,
  };
}

function request(
  method: string,
  path: string,
  init: { headers?: Record<string, string>; body?: unknown } = {},
): Request {
  return new Request(`https://crabbox.test${path}`, {
    method,
    headers: {
      ...(init.body === undefined ? {} : { "content-type": "application/json" }),
      ...init.headers,
    },
    body: init.body === undefined ? undefined : JSON.stringify(init.body),
  });
}

async function readCodeBootstrapHandoff(response: Response): Promise<{ url: URL; ticket: string }> {
  const body = await response.text();
  const action = /<form\b[^>]*\baction="([^"]+)"/.exec(body)?.[1];
  const ticket = /<input\b[^>]*\bname="ticket"[^>]*\bvalue="([^"]+)"/.exec(body)?.[1];
  if (!action || !ticket) {
    throw new Error("Code bootstrap handoff form is incomplete");
  }
  return { url: new URL(action), ticket };
}

function codeBootstrapRequest(url: URL | string, ticket: string): Request {
  return new Request(url, {
    method: "POST",
    headers: { "content-type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({ ticket }),
  });
}

function confirmRuntimeAdapterAbsence(
  fleet: FleetDurableObject,
  lease: LeaseRecord,
  headers: Record<string, string>,
): Promise<Response> {
  if (
    !lease.runtimeAdapterID ||
    !lease.runtimeAdapterWorkspaceID ||
    !lease.runtimeAdapterRegistrationID
  ) {
    throw new Error("runtime adapter completion test requires an exact registration generation");
  }
  return fleet.fetch(
    request("POST", `/v1/leases/${lease.id}/release`, {
      headers,
      body: {
        runtimeAdapterDeleteCompletion: {
          adapterID: lease.runtimeAdapterID,
          workspaceID: lease.runtimeAdapterWorkspaceID,
          registrationID: lease.runtimeAdapterRegistrationID,
          status: "absent",
        },
      },
    }),
  );
}

async function sha256HexForTest(value: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(value));
  return [...new Uint8Array(digest)].map((byte) => byte.toString(16).padStart(2, "0")).join("");
}

function decodeUserTokenPayload(token: string): { exp: number; iat: number } {
  expect(token).toMatch(/^cbxu_/);
  const [payload] = token.slice("cbxu_".length).split(".");
  const decoded = Buffer.from(payload, "base64url").toString("utf8");
  return JSON.parse(decoded) as { exp: number; iat: number };
}
