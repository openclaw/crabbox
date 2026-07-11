import { afterEach, describe, expect, it, vi } from "vitest";

import type { LeaseConfig } from "../src/config";
import { prepareCoordinatorRequest } from "../src/coordinator-entry";
import type {
  CoordinatorRuntime,
  CoordinatorSocketHandlers,
  CoordinatorStorage,
  CoordinatorWebSocketUpgradeOptions,
} from "../src/coordinator-runtime";
import { AWSProvider, FleetCoordinator } from "../src/fleet";
import type { Env, LeaseRecord, ProviderMachine } from "../src/types";

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

class MemoryStorage implements CoordinatorStorage {
  private readonly values = new Map<string, unknown>();

  async get<T>(key: string): Promise<T | undefined> {
    return this.values.get(key) as T | undefined;
  }

  async put<T>(key: string, value: T): Promise<void> {
    this.values.set(key, value);
  }

  async delete(key: string): Promise<void> {
    this.values.delete(key);
  }

  async list<T>({ prefix = "" }: { prefix?: string } = {}): Promise<Map<string, T>> {
    return new Map(
      [...this.values]
        .filter(([key]) => key.startsWith(prefix))
        .map(([key, value]) => [key, value as T]),
    );
  }
}

class MemoryRuntime implements CoordinatorRuntime {
  readonly storage = new MemoryStorage();
  readonly ephemeralWebSocketMaxPayloadBytes = 1024 * 1024;
  private alarmTime?: number;

  async runExclusive<T>(callback: () => Promise<T>): Promise<T> {
    return await callback();
  }

  createWebSocketUpgrade(_options?: CoordinatorWebSocketUpgradeOptions): {
    socket: WebSocket;
    response: Response;
  } {
    throw new Error("websocket upgrades are not used by this test");
  }

  getWebSockets(): Iterable<WebSocket> {
    return [];
  }

  socketAttachment<T>(_socket: WebSocket): T | undefined {
    return undefined;
  }

  setSocketAttachment(_socket: WebSocket, _attachment: unknown): void {}

  acceptWebSocket(
    _socket: WebSocket,
    _attachment: unknown,
    _tags: string[],
    _handlers: CoordinatorSocketHandlers,
  ): void {}

  acceptEphemeralWebSocket(_socket: WebSocket, _handlers: CoordinatorSocketHandlers): void {}

  async take<T>(key: string): Promise<T | undefined> {
    const value = await this.storage.get<T>(key);
    await this.storage.delete(key);
    return value;
  }

  async getAlarm(): Promise<number | undefined> {
    return this.alarmTime;
  }

  async scheduleAlarm(time: number): Promise<void> {
    this.alarmTime = time;
  }

  async clearAlarm(): Promise<void> {
    this.alarmTime = undefined;
  }
}

function privateAWSEnv(): Env {
  return {
    CRABBOX_DEFAULT_ORG: "example-org",
    CRABBOX_WORKSPACE_PROVIDER: "aws",
    CRABBOX_WORKSPACE_AWS_PRIVATE: "1",
    CRABBOX_AWS_EXPECTED_ACCOUNT_ID: "123456789012",
    CRABBOX_AWS_EXPECTED_REGION: "us-west-2",
    CRABBOX_WORKSPACE_AWS_INSTANCE_TYPES: "t3a.small,t3.small",
    CRABBOX_WORKSPACE_AWS_MAX_VCPUS: "2",
    CRABBOX_WORKSPACE_AWS_MAX_MEMORY_MIB: "4096",
    CRABBOX_WORKSPACE_AWS_ROOT_GB: "20",
    CRABBOX_WORKSPACE_AWS_SUBNET_ID: "subnet-abc123",
    CRABBOX_WORKSPACE_AWS_SECURITY_GROUP_ID: "sg-abc123",
    CRABBOX_WORKSPACE_AWS_CONTROLLER_SECURITY_GROUP_ID: "sg-def456",
    CRABBOX_WORKSPACE_AWS_INSTANCE_PROFILE: "crabbox-private-workspace",
    CRABBOX_WORKSPACE_AWS_MARKET: "on-demand",
    CRABBOX_WORKSPACE_AWS_SSM_LOG_GROUP: "/crabbox/private-workspaces/ssm",
  } as Env;
}

function workspaceRequest(command?: string): Request {
  return new Request("https://coordinator.test/v1/workspaces", {
    method: "POST",
    headers: {
      "content-type": "application/json",
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    },
    body: JSON.stringify({
      id: "private-gateway",
      runtime: "crabbox",
      repo: "example-org/gateway",
      branch: "main",
      ...(command ? { command } : {}),
      ttlSeconds: 1800,
      idleTimeoutSeconds: 300,
    }),
  });
}

describe("private AWS workspaces", () => {
  it("fails closed when private AWS mode selects another workspace provider", async () => {
    const env = privateAWSEnv();
    env.CRABBOX_WORKSPACE_PROVIDER = "hetzner";
    const fleet = new FleetCoordinator(new MemoryRuntime(), env);

    const response = await fleet.fetch(workspaceRequest("node server.js"));

    expect(response.status).toBe(424);
    await expect(response.json()).resolves.toMatchObject({
      error: "workspace_not_configured",
      message: "private AWS workspace mode requires CRABBOX_WORKSPACE_PROVIDER=aws",
    });
  });

  it("requires an explicit command before reserving a workspace", async () => {
    const fleet = new FleetCoordinator(new MemoryRuntime(), privateAWSEnv());

    const response = await fleet.fetch(workspaceRequest());

    expect(response.status).toBe(400);
    await expect(response.json()).resolves.toMatchObject({
      error: "invalid_workspace_request",
      message: "private AWS workspaces require an explicit command",
    });
  });

  it("rejects desktop capability before reserving a private workspace", async () => {
    const fleet = new FleetCoordinator(new MemoryRuntime(), privateAWSEnv());
    const request = workspaceRequest("node server.js");
    const body = (await request.json()) as Record<string, unknown>;
    const response = await fleet.fetch(
      new Request(request.url, {
        method: "POST",
        headers: request.headers,
        body: JSON.stringify({ ...body, capabilities: { desktop: true } }),
      }),
    );

    expect(response.status).toBe(400);
    await expect(response.json()).resolves.toMatchObject({
      error: "unsupported_workspace_capability",
    });
  });

  it("provisions only the configured private shape and reports SSM evidence", async () => {
    const lifecycleLog = vi.spyOn(console, "info").mockImplementation(() => {});
    let requested: LeaseConfig | undefined;
    let releases = 0;
    const provider = {
      workspaceCapability: privateWorkspaceCapability(),
      async listCrabboxServers(): Promise<ProviderMachine[]> {
        return [];
      },
      supportsSSHHostKeyInjection(): boolean {
        // Capability-owned workspaces must bypass generic provider host-key injection.
        return true;
      },
      async findServerByLease(): Promise<ProviderMachine | undefined> {
        return undefined;
      },
      async getServer(id: string): Promise<ProviderMachine> {
        return privateMachine(id);
      },
      async prepareLeaseConfig(config: LeaseConfig): Promise<LeaseConfig> {
        return config;
      },
      async prepareLeaseCreate(config: LeaseConfig, lease: LeaseRecord) {
        return {
          config,
          lease: {
            ...lease,
            network: {
              awsPrivate: true,
              awsSecurityGroupID: config.awsSGID,
              awsSubnetID: config.awsSubnetID,
            },
          },
          provisioning: { allowEmptySSHIngress: true },
        };
      },
      async hourlyPriceUSD(): Promise<number> {
        return 0.02;
      },
      async createServerWithFallback(config: LeaseConfig) {
        requested = structuredClone(config);
        return { server: privateMachine("i-abc123"), serverType: config.serverType };
      },
      async finalizeLeaseCreate(config: LeaseConfig, lease: LeaseRecord) {
        return {
          config,
          lease: {
            ...lease,
            region: config.awsRegion,
            awsSSMCommandID: "command-abc123",
            awsSSMCommandStatus: "Success",
          },
        };
      },
      async releaseLease(): Promise<void> {
        releases += 1;
      },
      async deleteServer(): Promise<void> {},
      supportsNativeImages(): boolean {
        return true;
      },
      nativeImagesUnsupportedMessage(): string {
        return "native images are supported";
      },
      defaultImageStrategy(): "image" {
        return "image";
      },
      validateLeaseImageStrategy(): string | undefined {
        return undefined;
      },
    };
    const runtime = new MemoryRuntime();
    const fleet = new FleetCoordinator(runtime, privateAWSEnv(), {
      aws: provider,
    } as never);

    const created = await fleet.fetch(workspaceRequest("node server.js"));
    expect(created.status).toBe(202);
    await fleet.alarm();

    expect(requested).toMatchObject({
      provider: "aws",
      target: "linux",
      serverType: "t3a.small",
      serverTypeExplicit: true,
      awsRegion: "us-west-2",
      awsInstanceTypes: ["t3a.small", "t3.small"],
      awsPrivate: true,
      awsRequireSSM: true,
      awsRootGB: 20,
      awsSubnetID: "subnet-abc123",
      awsSGID: "sg-abc123",
      awsProfile: "crabbox-private-workspace",
      awsSSMLogGroup: "/crabbox/private-workspaces/ssm",
      awsSSHCIDRs: [],
      sshUser: "crabbox",
      sshPublicKey: "",
      sshHostPrivateKey: "",
      sshHostPublicKey: "",
      workRoot: "/work/crabbox",
      capacityMarket: "on-demand",
      capacityFallback: "none",
      capacityRegions: ["us-west-2"],
      capacityHints: false,
    });
    const leases = await runtime.storage.list<LeaseRecord>({ prefix: "lease:" });
    const lease = [...leases.values()][0];
    expect(lease?.sshHostKey).toBeUndefined();
    expect(JSON.stringify(lease)).not.toContain("OPENSSH PRIVATE KEY");
    expect(requested?.awsSSMBootstrapCommand).toContain("crabbox-workspace.service");
    expect(requested?.awsSSMBootstrapCommand).toContain("node server.js");
    expect(requested?.awsSSMBootstrapCommand).toContain(
      "systemctl show --no-pager --property=ActiveState,SubState,Result,ExecMainStatus",
    );
    expect(requested?.awsSSMBootstrapCommand).not.toContain("journalctl");
    expect(requested?.awsSSMBootstrapCommand).toContain("flock -x -w 600 9");
    expect(requested?.awsSSMBootstrapCommand).toContain(
      'test "$(stat -c %U:%G "$workspace_ancestor")" = root:root',
    );
    expect(requested?.awsSSMBootstrapCommand).toContain(
      "test ! -L '/work/crabbox/workspaces/private-gateway'",
    );
    expect(requested?.awsSSMBootstrapCommand).not.toContain(
      "install -d -m 0755 -o crabbox -g crabbox /work/crabbox/workspaces",
    );
    expect(requested?.awsSSMBootstrapCommand).not.toContain("authorized_keys");
    expect(requested?.awsSSMBootstrapCommand).not.toContain("/run/crabbox/workspace-ready");

    const status = await fleet.fetch(
      new Request("https://coordinator.test/v1/workspaces/private-gateway", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    expect(status.status).toBe(200);
    await expect(status.json()).resolves.toMatchObject({
      id: "private-gateway",
      provider: "aws",
      status: "ready",
      leaseId: expect.stringMatching(/^cbx_/),
      cloudResourceId: "i-abc123",
      region: "us-west-2",
      serverType: "t3a.small",
      bootstrap: {
        transport: "ssm",
        status: "Success",
        commandId: "command-abc123",
        logGroup: "/crabbox/private-workspaces/ssm",
      },
      capabilities: { terminal: false, nativeVnc: false },
    });

    const stopped = await fleet.fetch(
      new Request("https://coordinator.test/v1/workspaces/private-gateway", {
        method: "DELETE",
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    expect(stopped.status).toBe(200);
    await expect(stopped.json()).resolves.toMatchObject({ status: "stopped" });
    expect(releases).toBe(1);

    const repeated = await fleet.fetch(
      new Request("https://coordinator.test/v1/workspaces/private-gateway", {
        method: "DELETE",
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    expect(repeated.status).toBe(200);
    await expect(repeated.json()).resolves.toMatchObject({ status: "stopped" });
    expect(releases).toBe(1);
    const logs = lifecycleLog.mock.calls.flat().join("\n");
    expect(logs).toContain('"event":"create_accepted"');
    expect(logs).toContain('"event":"ready"');
    expect(logs).toContain('"event":"delete_requested"');
    expect(logs).not.toContain("node server.js");
    expect(logs).not.toContain("example-org/gateway");
    expect(logs).not.toContain("alice@example.com");
  });

  it("recovers an ambiguous private launch and resumes idempotent SSM bootstrap", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2030-01-01T00:00:00.000Z"));
    const runtime = new MemoryRuntime();
    const state: RecoveryProviderState = {
      creates: 0,
      recovers: 0,
      resumes: 0,
      releases: 0,
    };
    const fleet = new FleetCoordinator(runtime, privateAWSEnv(), {
      aws: privateRecoveryProvider(state),
    } as never);

    expect((await fleet.fetch(workspaceRequest("node server.js"))).status).toBe(202);
    await fleet.alarm();
    expect(state.creates).toBe(1);
    expect(state.resumes).toBe(0);

    const retryAt = await runtime.getAlarm();
    expect(retryAt).toBeDefined();
    vi.setSystemTime(new Date((retryAt ?? Date.now()) + 1));
    await fleet.alarm();

    expect(state.recovers).toBe(1);
    expect(state.resumes).toBe(1);
    expect(state.creates).toBe(1);
    const status = await fleet.fetch(
      new Request("https://coordinator.test/v1/workspaces/private-gateway", {
        headers: {
          "x-crabbox-owner": "alice@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    await expect(status.json()).resolves.toMatchObject({
      status: "ready",
      cloudResourceId: "i-recovered123",
      bootstrap: { transport: "ssm", status: "Success", commandId: "command-recovered123" },
    });
  });

  it("recovers only to retire an ambiguous launch after delete", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2030-01-01T00:00:00.000Z"));
    const runtime = new MemoryRuntime();
    const state: RecoveryProviderState = {
      creates: 0,
      recovers: 0,
      resumes: 0,
      releases: 0,
    };
    const fleet = new FleetCoordinator(runtime, privateAWSEnv(), {
      aws: privateRecoveryProvider(state),
    } as never);
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };

    await fleet.fetch(workspaceRequest("node server.js"));
    await fleet.alarm();
    await fleet.fetch(
      new Request("https://coordinator.test/v1/workspaces/private-gateway", {
        method: "DELETE",
        headers,
      }),
    );
    const retryAt = await runtime.getAlarm();
    vi.setSystemTime(new Date((retryAt ?? Date.now()) + 1));
    await fleet.alarm();

    expect(state.recovers).toBe(1);
    expect(state.resumes).toBe(0);
    expect(state.releases).toBe(1);
    const status = await fleet.fetch(
      new Request("https://coordinator.test/v1/workspaces/private-gateway", { headers }),
    );
    await expect(status.json()).resolves.toMatchObject({ status: "stopped" });
  });

  it("does not reactivate when delete arrives during recovered bootstrap", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2030-01-01T00:00:00.000Z"));
    const runtime = new MemoryRuntime();
    const state: RecoveryProviderState = {
      creates: 0,
      recovers: 0,
      resumes: 0,
      releases: 0,
    };
    const resumeStarted = deferred<void>();
    const finishResume = deferred<void>();
    const fleet = new FleetCoordinator(runtime, privateAWSEnv(), {
      aws: {
        ...privateRecoveryProvider(state),
        async resumeRecoveredServer(
          _config: LeaseConfig,
          _lease: LeaseRecord,
          server: ProviderMachine,
        ): Promise<ProviderMachine> {
          state.resumes += 1;
          resumeStarted.resolve();
          await finishResume.promise;
          return {
            ...server,
            host: "",
            awsSSMCommandID: "command-recovered123",
            awsSSMCommandStatus: "Success",
          };
        },
      },
    } as never);
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };

    await fleet.fetch(workspaceRequest("node server.js"));
    await fleet.alarm();
    const retryAt = await runtime.getAlarm();
    vi.setSystemTime(new Date((retryAt ?? Date.now()) + 1));
    const recovery = fleet.alarm();
    await resumeStarted.promise;

    const deletion = await fleet.fetch(
      new Request("https://coordinator.test/v1/workspaces/private-gateway", {
        method: "DELETE",
        headers,
      }),
    );
    expect(deletion.status).toBe(200);
    finishResume.resolve();
    await recovery;

    expect(state.resumes).toBe(1);
    expect(state.releases).toBe(1);
    const status = await fleet.fetch(
      new Request("https://coordinator.test/v1/workspaces/private-gateway", { headers }),
    );
    await expect(status.json()).resolves.toMatchObject({ status: "stopped" });
    const leases = await runtime.storage.list<LeaseRecord>({ prefix: "lease:" });
    expect([...leases.values()].some((lease) => lease.state === "active")).toBe(false);
  });

  it("uses configured runtime-adapter ownership without changing legacy defaults", async () => {
    const custom = await prepareCoordinatorRequest(
      new Request("https://coordinator.test/v1/workspaces/private-gateway", {
        headers: { authorization: "Bearer runtime" },
      }),
      {
        CRABBOX_RUNTIME_ADAPTER_TOKEN: "runtime",
        CRABBOX_RUNTIME_ADAPTER_OWNER: "controller@example.com",
        CRABBOX_RUNTIME_ADAPTER_ORG: "example-org",
      } as Env,
    );
    if ("response" in custom) throw new Error("configured runtime adapter token was rejected");
    expect(custom.request.headers.get("x-crabbox-owner")).toBe("controller@example.com");
    expect(custom.request.headers.get("x-crabbox-org")).toBe("example-org");

    const legacy = await prepareCoordinatorRequest(
      new Request("https://coordinator.test/v1/workspaces/private-gateway", {
        headers: { authorization: "Bearer runtime" },
      }),
      { CRABBOX_RUNTIME_ADAPTER_TOKEN: "runtime" } as Env,
    );
    if ("response" in legacy) throw new Error("legacy runtime adapter token was rejected");
    expect(legacy.request.headers.get("x-crabbox-owner")).toBe("service@openclaw.org");
    expect(legacy.request.headers.get("x-crabbox-org")).toBe("openclaw");
  });
});

function privateMachine(id: string): ProviderMachine {
  return {
    provider: "aws",
    id: 123,
    cloudID: id,
    name: "crabbox-private-gateway",
    status: "running",
    serverType: "t3a.small",
    host: "",
    region: "us-west-2",
    labels: {},
    awsSSMCommandID: "command-abc123",
    awsSSMCommandStatus: "Success",
  };
}

interface RecoveryProviderState {
  creates: number;
  recovers: number;
  resumes: number;
  releases: number;
}

function privateRecoveryProvider(state: RecoveryProviderState): Record<string, unknown> {
  return {
    workspaceCapability: privateWorkspaceCapability(),
    async listCrabboxServers(): Promise<ProviderMachine[]> {
      return [];
    },
    supportsSSHHostKeyInjection(): boolean {
      return false;
    },
    async recoverServer(): Promise<ProviderMachine> {
      state.recovers += 1;
      return { ...privateMachine("i-recovered123"), status: "running" };
    },
    async getServer(id: string): Promise<ProviderMachine> {
      return privateMachine(id);
    },
    async prepareLeaseConfig(config: LeaseConfig): Promise<LeaseConfig> {
      return config;
    },
    async prepareLeaseCreate(config: LeaseConfig, lease: LeaseRecord) {
      return {
        config,
        lease: {
          ...lease,
          awsSSMLogGroup: config.awsSSMLogGroup,
          network: {
            awsPrivate: true,
            awsSecurityGroupID: config.awsSGID,
            awsSubnetID: config.awsSubnetID,
          },
        },
        provisioning: { allowEmptySSHIngress: true },
      };
    },
    async hourlyPriceUSD(): Promise<number> {
      return 0.02;
    },
    async createServerWithFallback(): Promise<never> {
      state.creates += 1;
      throw new Error("crabbox_aws_run_instances_outcome_uncertain: response interrupted");
    },
    async resumeRecoveredServer(
      _config: LeaseConfig,
      _lease: LeaseRecord,
      server: ProviderMachine,
    ): Promise<ProviderMachine> {
      state.resumes += 1;
      return {
        ...server,
        host: "",
        awsSSMCommandID: "command-recovered123",
        awsSSMCommandStatus: "Success",
      };
    },
    async releaseLease(): Promise<void> {
      state.releases += 1;
    },
    async deleteServer(): Promise<void> {},
    supportsNativeImages(): boolean {
      return true;
    },
    nativeImagesUnsupportedMessage(): string {
      return "native images are supported";
    },
    defaultImageStrategy(): "image" {
      return "image";
    },
    validateLeaseImageStrategy(): string | undefined {
      return undefined;
    },
  };
}

function privateWorkspaceCapability(): AWSProvider["workspaceCapability"] {
  const provider = new AWSProvider(privateAWSEnv(), "us-west-2", {} as never);
  return provider.workspaceCapability.bind(provider);
}

function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void } {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}
