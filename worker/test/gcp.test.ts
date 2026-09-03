import { afterEach, describe, expect, it, vi } from "vitest";

import { leaseConfig } from "../src/config";
import {
  GCPClient,
  gcpEffectiveTags,
  gcpFirewallNameForPolicy,
  gcpFirewallNameForNetwork,
  gcpMachineImageNotFound,
  gcpSnapshotNotFound,
  isFallbackProvisioningError,
  operationDone,
} from "../src/gcp";
import {
  ProviderProvisioningCleanupError,
  ProviderProvisioningOutcomeUncertainError,
  ProviderResourceUnresolvedError,
  providerProvisioningCleanupClaim,
  providerProvisioningOutcomeUncertain,
  type ProviderProvisioningCleanupClaim,
} from "../src/provider-provisioning";
import { leaseProviderName } from "../src/slug";
import type { Env, LeaseRecord, ProviderMachine } from "../src/types";

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

function metadataResponse(body: BodyInit | null, init: ResponseInit = {}): Response {
  const headers = new Headers(init.headers);
  headers.set("Metadata-Flavor", "Google");
  return new Response(body, { ...init, headers });
}

function metadataJSON(body: unknown, init: ResponseInit = {}): Response {
  const headers = new Headers(init.headers);
  headers.set("Content-Type", "application/json");
  return metadataResponse(JSON.stringify(body), { ...init, headers });
}

function primeAccessToken(client: GCPClient): void {
  (client as unknown as { cache: { token: string; expiresAt: number } }).cache = {
    token: "test-token",
    expiresAt: Math.trunc(Date.now() / 1000) + 3600,
  };
}

function observedGCPLease(overrides: Partial<LeaseRecord> = {}): LeaseRecord {
  return {
    id: "cbx_abcdef123456",
    slug: "blue-lobster",
    provider: "gcp",
    target: "linux",
    architecture: "arm64",
    cloudID: "crabbox-blue-lobster",
    region: "us-central1-a",
    providerProject: "default-project",
    owner: "alice@example.com",
    org: "example-org",
    profile: "default",
    class: "standard",
    serverType: "t2a-standard-1",
    serverID: 123,
    providerResourceID: "9223372036854775807",
    serverName: "crabbox-blue-lobster",
    providerKey: "",
    host: "192.0.2.10",
    sshUser: "ubuntu",
    sshPort: "22",
    workRoot: "/workspace",
    keep: false,
    ttlSeconds: 3600,
    estimatedHourlyUSD: 0,
    maxEstimatedUSD: 0,
    state: "active",
    createdAt: "2026-09-03T00:00:00.000Z",
    updatedAt: "2026-09-03T00:00:00.000Z",
    expiresAt: "2026-09-04T00:00:00.000Z",
    ...overrides,
  };
}

function ownedGCPInstance(
  lease: LeaseRecord,
  overrides: Record<string, unknown> = {},
): Record<string, unknown> {
  return {
    id: lease.providerResourceID,
    name: lease.cloudID,
    status: "RUNNING",
    machineType: `zones/${lease.region}/machineTypes/${lease.serverType}`,
    zone: `projects/${lease.providerProject}/zones/${lease.region}`,
    labels: {
      crabbox: "true",
      created_by: "crabbox",
      lease: lease.id,
      owner: "alice_example_com",
      provider: "gcp",
      slug: lease.slug,
    },
    disks: [
      {
        boot: true,
        type: "PERSISTENT",
        source: `projects/${lease.providerProject}/zones/${lease.region}/disks/boot-disk`,
      },
    ],
    ...overrides,
  };
}

function observedGCPBootDisk(
  lease: LeaseRecord,
  overrides: Record<string, unknown> = {},
): Record<string, unknown> {
  return {
    id: "8223372036854775807",
    selfLink: `projects/${lease.providerProject}/zones/${lease.region}/disks/boot-disk`,
    users: [`projects/${lease.providerProject}/zones/${lease.region}/instances/${lease.cloudID}`],
    sourceImage: "projects/source-project/global/images/runner-v3",
    sourceImageId: "8123456789012345678",
    ...overrides,
  };
}

describe("gcp provider", () => {
  const env: Env = {
    FLEET: {} as DurableObjectNamespace,
    HETZNER_TOKEN: "",
    GCP_CLIENT_EMAIL: "test@example.iam.gserviceaccount.com",
    GCP_PRIVATE_KEY: "test-key",
    CRABBOX_GCP_PROJECT: "default-project",
    CRABBOX_GCP_ZONE: "us-central1-a",
  };

  it("waits until operations report DONE", () => {
    expect(operationDone({ name: "operation-1", status: "RUNNING" })).toBe(false);
    expect(operationDone({ name: "operation-1", status: "PENDING" })).toBe(false);
    expect(operationDone({ name: "operation-1" })).toBe(false);
    expect(operationDone({ name: "operation-1", status: "DONE" })).toBe(true);
  });

  it("prefers per-request project over Worker defaults", () => {
    expect(new GCPClient(env).project).toBe("default-project");
    expect(new GCPClient(env, undefined, "request-project").project).toBe("request-project");
  });

  it("observes exact immutable image and snapshot provenance from the owned boot disk", async () => {
    const fixtures = [
      {
        disk: {
          sourceImage: "projects/source-project/global/images/runner-v3",
          sourceImageId: "8123456789012345678",
        },
        expected: {
          id: "8123456789012345678",
          source: "explicit",
          provider: "gcp",
          kind: "gcp-image",
          region: "us-central1-a",
          sourceID: "projects/source-project/global/images/runner-v3",
        },
      },
      {
        disk: {
          sourceImage: undefined,
          sourceImageId: undefined,
          sourceSnapshot:
            "https://www.googleapis.com/compute/v1/projects/source-project/global/snapshots/runner-v3",
          sourceSnapshotId: "7123456789012345678",
        },
        expected: {
          id: "7123456789012345678",
          source: "snapshot",
          provider: "gcp",
          kind: "gcp-disk-snapshot",
          region: "us-central1-a",
          sourceID: "projects/source-project/global/snapshots/runner-v3",
        },
      },
    ] as const;

    for (const fixture of fixtures) {
      const lease = observedGCPLease();
      const client = new GCPClient(env);
      const requests: string[] = [];
      const internal = client as unknown as {
        gcp<T>(method: string, path: string): Promise<T>;
      };
      vi.spyOn(internal, "gcp").mockImplementation(async (method, path) => {
        requests.push(`${method} ${path}`);
        if (path.includes("/instances/")) return ownedGCPInstance(lease) as never;
        if (path.includes("/disks/")) {
          return observedGCPBootDisk(lease, fixture.disk) as never;
        }
        throw new Error(`unexpected GCP request ${method} ${path}`);
      });

      // oxlint-disable-next-line eslint/no-await-in-loop -- each source owns isolated request evidence.
      await expect(client.observeReadyPoolImageIdentity(lease)).resolves.toEqual(fixture.expected);
      expect(requests).toEqual([
        "GET /zones/us-central1-a/instances/crabbox-blue-lobster",
        "GET /zones/us-central1-a/disks/boot-disk",
        "GET /zones/us-central1-a/instances/crabbox-blue-lobster",
        "GET /zones/us-central1-a/disks/boot-disk",
      ]);
    }
  });

  it("rejects incomplete lease scope before GCP I/O", async () => {
    const fixtures: Array<Partial<LeaseRecord>> = [
      { provider: "aws" },
      { providerProject: undefined },
      { providerProject: " default-project" },
      { region: undefined },
      { region: "us-central1-a " },
      { cloudID: "" },
      { cloudID: " crabbox-blue-lobster" },
      { serverName: "other-instance" },
      { providerResourceID: undefined },
      { providerResourceID: "9223372036854775807 " },
      { providerResourceID: "instance-latest" },
    ];

    for (const fixture of fixtures) {
      const client = new GCPClient(env);
      const internal = client as unknown as {
        gcp<T>(method: string, path: string): Promise<T>;
      };
      const gcp = vi.spyOn(internal, "gcp");

      // oxlint-disable-next-line eslint/no-await-in-loop -- every malformed scope must stop before I/O.
      await expect(
        client.observeReadyPoolImageIdentity(observedGCPLease(fixture)),
      ).resolves.toBeUndefined();
      expect(gcp).not.toHaveBeenCalled();
    }
  });

  it("rejects foreign, ambiguous, malformed, and non-persistent boot provenance", async () => {
    const lease = observedGCPLease();
    const baseDisk = observedGCPBootDisk(lease);
    const baseBoot = {
      boot: true,
      type: "PERSISTENT",
      source: "projects/default-project/zones/us-central1-a/disks/boot-disk",
    };
    const fixtures: Array<{
      name: string;
      instance?: Record<string, unknown>;
      disk?: Record<string, unknown>;
    }> = [
      { name: "machine image", instance: { sourceMachineImage: "machine-images/runner" } },
      { name: "missing instance id", instance: { id: undefined } },
      { name: "replacement instance id", instance: { id: "9223372036854775806" } },
      { name: "foreign instance", instance: { name: "foreign-instance" } },
      {
        name: "foreign ownership",
        instance: {
          labels: { ...(ownedGCPInstance(lease).labels as object), owner: "mallory" },
        },
      },
      { name: "missing boot disk", instance: { disks: [] } },
      { name: "multiple boot disks", instance: { disks: [baseBoot, { ...baseBoot }] } },
      { name: "non-persistent boot disk", instance: { disks: [{ ...baseBoot, type: "SCRATCH" }] } },
      {
        name: "foreign disk project",
        instance: {
          disks: [
            {
              ...baseBoot,
              source: "projects/foreign-project/zones/us-central1-a/disks/boot-disk",
            },
          ],
        },
      },
      {
        name: "foreign disk zone",
        instance: {
          disks: [
            {
              ...baseBoot,
              source: "projects/default-project/zones/us-central1-b/disks/boot-disk",
            },
          ],
        },
      },
      { name: "malformed disk path", instance: { disks: [{ ...baseBoot, source: "boot-disk" }] } },
      { name: "missing disk id", disk: { id: undefined } },
      { name: "non-numeric disk id", disk: { id: "disk-latest" } },
      { name: "missing disk self link", disk: { selfLink: undefined } },
      { name: "malformed disk self link", disk: { selfLink: "boot-disk" } },
      {
        name: "foreign disk self link",
        disk: {
          selfLink: "projects/foreign-project/zones/us-central1-a/disks/boot-disk",
        },
      },
      { name: "missing disk users", disk: { users: undefined } },
      { name: "empty disk users", disk: { users: [] } },
      {
        name: "multiple disk users",
        disk: {
          users: [
            "projects/default-project/zones/us-central1-a/instances/crabbox-blue-lobster",
            "projects/default-project/zones/us-central1-a/instances/other-instance",
          ],
        },
      },
      {
        name: "foreign disk user",
        disk: {
          users: ["projects/default-project/zones/us-central1-a/instances/other-instance"],
        },
      },
      { name: "malformed disk user", disk: { users: ["crabbox-blue-lobster"] } },
      {
        name: "both source kinds",
        disk: {
          ...baseDisk,
          sourceSnapshot: "projects/source-project/global/snapshots/runner-v3",
          sourceSnapshotId: "7123456789012345678",
        },
      },
      {
        name: "neither source kind",
        disk: {
          sourceImage: undefined,
          sourceImageId: undefined,
          sourceSnapshot: undefined,
          sourceSnapshotId: undefined,
        },
      },
      { name: "malformed image source", disk: { ...baseDisk, sourceImage: "runner-v3" } },
      { name: "missing immutable id", disk: { sourceImageId: undefined } },
      { name: "non-numeric immutable id", disk: { ...baseDisk, sourceImageId: "latest" } },
      {
        name: "machine image disk source",
        disk: {
          sourceImage: "projects/source-project/global/machineImages/runner-v3",
          sourceImageId: "8123456789012345678",
        },
      },
    ];

    for (const fixture of fixtures) {
      const client = new GCPClient(env);
      let requests = 0;
      const internal = client as unknown as {
        gcp<T>(method: string, path: string): Promise<T>;
      };
      vi.spyOn(internal, "gcp").mockImplementation(async (_method, path) => {
        requests += 1;
        if (path.includes("/instances/")) {
          return ownedGCPInstance(lease, fixture.instance) as never;
        }
        return { ...baseDisk, ...fixture.disk } as never;
      });

      // oxlint-disable-next-line eslint/no-await-in-loop -- each invalid shape is an isolated fail-closed fixture.
      const observed = await client.observeReadyPoolImageIdentity(lease);
      expect({ name: fixture.name, observed }).toEqual({ name: fixture.name, observed: undefined });
      expect({ name: fixture.name, requests }).toEqual({
        name: fixture.name,
        requests: fixture.instance ? 1 : 2,
      });
    }
  });

  it("rejects instance replacement and attachment or provenance drift across fenced rereads", async () => {
    const lease = observedGCPLease();
    const changedBootDisk = {
      boot: true,
      type: "PERSISTENT",
      source: "projects/default-project/zones/us-central1-a/disks/replacement-disk",
    };
    const fixtures: Array<{
      name: string;
      read: number;
      value: Record<string, unknown>;
    }> = [
      { name: "instance replacement", read: 3, value: { id: "9223372036854775806" } },
      {
        name: "ownership drift",
        read: 3,
        value: {
          labels: { ...(ownedGCPInstance(lease).labels as object), owner: "mallory" },
        },
      },
      { name: "boot attachment drift", read: 3, value: { disks: [changedBootDisk] } },
      { name: "disk replacement", read: 4, value: { id: "8223372036854775806" } },
      {
        name: "disk self link drift",
        read: 4,
        value: {
          selfLink: "projects/default-project/zones/us-central1-a/disks/replacement-disk",
        },
      },
      {
        name: "disk user drift",
        read: 4,
        value: {
          users: ["projects/default-project/zones/us-central1-a/instances/other-instance"],
        },
      },
      {
        name: "disk provenance drift",
        read: 4,
        value: { sourceImageId: "8123456789012345677" },
      },
      {
        name: "disk provenance source drift",
        read: 4,
        value: { sourceImage: "projects/source-project/global/images/runner-v4" },
      },
    ];

    for (const fixture of fixtures) {
      const client = new GCPClient(env);
      let reads = 0;
      const internal = client as unknown as {
        gcp<T>(method: string, path: string): Promise<T>;
      };
      vi.spyOn(internal, "gcp").mockImplementation(async (_method, path) => {
        reads += 1;
        const base = path.includes("/instances/")
          ? ownedGCPInstance(lease)
          : observedGCPBootDisk(lease);
        return { ...base, ...(reads === fixture.read ? fixture.value : {}) } as never;
      });

      // oxlint-disable-next-line eslint/no-await-in-loop -- each drift point owns isolated read evidence.
      await expect(client.observeReadyPoolImageIdentity(lease)).resolves.toBeUndefined();
      expect({ name: fixture.name, reads }).toEqual({ name: fixture.name, reads: fixture.read });
    }
  });

  it("propagates IAM failures at every fenced instance and boot-disk read", async () => {
    const lease = observedGCPLease();
    for (const deniedRead of [1, 2, 3, 4] as const) {
      const client = new GCPClient(env);
      let reads = 0;
      const internal = client as unknown as {
        gcp<T>(method: string, path: string): Promise<T>;
      };
      vi.spyOn(internal, "gcp").mockImplementation(async (_method, path) => {
        reads += 1;
        if (reads === deniedRead) throw new Error(`IAM denied read ${deniedRead}`);
        return path.includes("/instances/")
          ? (ownedGCPInstance(lease) as never)
          : (observedGCPBootDisk(lease) as never);
      });

      // oxlint-disable-next-line eslint/no-await-in-loop -- each fenced read is a distinct IAM boundary.
      await expect(client.observeReadyPoolImageIdentity(lease)).rejects.toThrow(
        `IAM denied read ${deniedRead}`,
      );
    }
  });

  it("preserves the exact raw GCP instance resource id on provider machines", async () => {
    const lease = observedGCPLease();
    const client = new GCPClient(env);
    const internal = client as unknown as {
      gcp<T>(method: string, path: string): Promise<T>;
    };
    vi.spyOn(internal, "gcp").mockResolvedValue(ownedGCPInstance(lease) as never);

    await expect(client.getServer(lease.cloudID)).resolves.toMatchObject({
      id: Number("9223372036854775807"),
      providerResourceID: "9223372036854775807",
    });
  });

  it("uses the owned collision reread instead of an operation error target id", async () => {
    const client = new GCPClient(env);
    primeAccessToken(client);
    const leaseID = "cbx_abcdef123456";
    const slug = "blue-lobster";
    const instanceName = leaseProviderName(leaseID, slug);
    const lease = observedGCPLease({
      id: leaseID,
      slug,
      cloudID: instanceName,
      serverName: instanceName,
    });
    const internal = client as unknown as {
      ensureFirewall(config: ReturnType<typeof leaseConfig>): Promise<void>;
    };
    vi.spyOn(internal, "ensureFirewall").mockResolvedValue();
    const calls: string[] = [];
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      calls.push(`${init?.method ?? "GET"} ${url.pathname}`);
      if (init?.method === "POST" && url.pathname.endsWith("/zones/us-central1-a/instances")) {
        return Response.json({ name: "insert-op", targetId: "1111111111111111111" });
      }
      if (
        init?.method === "POST" &&
        url.pathname.endsWith("/zones/us-central1-a/operations/insert-op/wait")
      ) {
        return Response.json({
          name: "insert-op",
          status: "DONE",
          targetId: "1111111111111111111",
          error: {
            errors: [{ code: "RESOURCE_ALREADY_EXISTS", message: "already exists" }],
          },
        });
      }
      if (
        init?.method === "GET" &&
        url.pathname.endsWith(`/zones/us-central1-a/instances/${instanceName}`)
      ) {
        return Response.json(ownedGCPInstance(lease));
      }
      throw new Error(`unexpected GCP request ${init?.method ?? "GET"} ${url.pathname}`);
    };
    const claims: ProviderProvisioningCleanupClaim[] = [];
    const targets: string[] = [];

    const error = await client
      .createServerWithFallback(
        leaseConfig({
          provider: "gcp",
          gcpZone: "us-central1-a",
          capacity: { availabilityZones: ["us-central1-b"] },
          serverType: "e2-micro",
          serverTypeExplicit: true,
          sshPublicKey: "ssh-ed25519 test",
        }),
        leaseID,
        slug,
        "alice@example.com",
        {
          async onTargetAttempt(target) {
            targets.push(target.region ?? "");
          },
          async onResourceCreated(claim) {
            claims.push(claim);
            return true;
          },
        },
      )
      .catch((cause: unknown) => cause);

    expect(error).toBeInstanceOf(ProviderResourceUnresolvedError);
    expect(error).toMatchObject({
      message: expect.stringContaining("not bound to this create attempt"),
    });
    expect(targets).toEqual(["us-central1-a"]);
    expect(claims).toEqual([
      {
        provider: "gcp",
        cloudID: instanceName,
        region: "us-central1-a",
        providerProject: "default-project",
        providerResourceID: "9223372036854775807",
      },
    ]);
    expect(calls).toEqual([
      "POST /compute/v1/projects/default-project/zones/us-central1-a/instances",
      "POST /compute/v1/projects/default-project/zones/us-central1-a/operations/insert-op/wait",
      `GET /compute/v1/projects/default-project/zones/us-central1-a/instances/${instanceName}`,
    ]);
  });

  it("fails closed without fallback when the instance insert response is lost", async () => {
    const client = new GCPClient(env);
    const internal = client as unknown as {
      ensureFirewall(config: ReturnType<typeof leaseConfig>): Promise<void>;
      gcp<T>(method: string, path: string, body?: unknown): Promise<T>;
    };
    vi.spyOn(internal, "ensureFirewall").mockResolvedValue();
    const gcp = vi.spyOn(internal, "gcp").mockRejectedValue(new TypeError("socket closed"));
    const targets: string[] = [];
    const config = leaseConfig({
      provider: "gcp",
      gcpZone: "us-central1-a",
      capacity: { availabilityZones: ["us-central1-b"] },
      serverType: "e2-micro",
      serverTypeExplicit: true,
      sshPublicKey: "ssh-ed25519 test",
    });

    await expect(
      client.createServerWithFallback(
        config,
        "cbx_abcdef123456",
        "blue-lobster",
        "alice@example.com",
        {
          async onTargetAttempt(target) {
            targets.push(target.region ?? "");
          },
          async onResourceCreated() {
            return true;
          },
        },
      ),
    ).rejects.toBeInstanceOf(ProviderProvisioningOutcomeUncertainError);
    expect(targets).toEqual(["us-central1-a"]);
    expect(gcp).toHaveBeenCalledOnce();
  });

  it.each([408, 503])(
    "fails closed without fallback when the instance insert returns HTTP %i",
    async (status) => {
      const client = new GCPClient(env);
      primeAccessToken(client);
      const internal = client as unknown as {
        ensureFirewall(config: ReturnType<typeof leaseConfig>): Promise<void>;
      };
      vi.spyOn(internal, "ensureFirewall").mockResolvedValue();
      const calls: string[] = [];
      client.fetcher = async (input, init) => {
        calls.push(`${init?.method ?? "GET"} ${String(input)}`);
        return new Response("backend unavailable", { status });
      };
      const targets: string[] = [];
      const config = leaseConfig({
        provider: "gcp",
        gcpZone: "us-central1-a",
        capacity: { availabilityZones: ["us-central1-b"] },
        serverType: "e2-micro",
        serverTypeExplicit: true,
        sshPublicKey: "ssh-ed25519 test",
      });

      await expect(
        client.createServerWithFallback(
          config,
          "cbx_abcdef123456",
          "blue-lobster",
          "alice@example.com",
          {
            async onTargetAttempt(target) {
              targets.push(target.region ?? "");
            },
            async onResourceCreated() {
              return true;
            },
          },
        ),
      ).rejects.toBeInstanceOf(ProviderProvisioningOutcomeUncertainError);
      expect(targets).toEqual(["us-central1-a"]);
      expect(calls).toHaveLength(1);
      expect(calls[0]).toContain(
        "POST https://compute.googleapis.com/compute/v1/projects/default-project/zones/us-central1-a/instances",
      );
    },
  );

  it("fails closed without fallback when an accepted instance operation becomes unobservable", async () => {
    const client = new GCPClient(env);
    const internal = client as unknown as {
      ensureFirewall(config: ReturnType<typeof leaseConfig>): Promise<void>;
      gcp<T>(method: string, path: string, body?: unknown): Promise<T>;
    };
    vi.spyOn(internal, "ensureFirewall").mockResolvedValue();
    const calls: string[] = [];
    vi.spyOn(internal, "gcp").mockImplementation(async (method, path) => {
      calls.push(`${method} ${path}`);
      if (path.endsWith("/instances")) {
        return { name: "insert-op", targetId: "9223372036854775807" } as T;
      }
      throw new TypeError("operation wait disconnected");
    });
    const targets: string[] = [];
    const config = leaseConfig({
      provider: "gcp",
      gcpZone: "us-central1-a",
      capacity: { availabilityZones: ["us-central1-b"] },
      serverType: "e2-micro",
      serverTypeExplicit: true,
      sshPublicKey: "ssh-ed25519 test",
    });

    const error = await client
      .createServerWithFallback(config, "cbx_abcdef123456", "blue-lobster", "alice@example.com", {
        async onTargetAttempt(target) {
          targets.push(target.region ?? "");
        },
        async onResourceCreated() {
          return true;
        },
      })
      .catch((cause: unknown) => cause);
    expect(error).toBeInstanceOf(ProviderProvisioningCleanupError);
    expect(providerProvisioningOutcomeUncertain(error)).toBe(true);
    expect(providerProvisioningCleanupClaim(error)).toEqual({
      provider: "gcp",
      cloudID: leaseProviderName("cbx_abcdef123456", "blue-lobster"),
      region: "us-central1-a",
      providerProject: "default-project",
      providerResourceID: "9223372036854775807",
    });
    expect(targets).toEqual(["us-central1-a"]);
    expect(calls).toEqual([
      "POST /zones/us-central1-a/instances",
      "POST /zones/us-central1-a/operations/insert-op/wait",
    ]);
  });

  it.each([
    {
      name: "malformed initial target id",
      initialTargetID: "instance-latest",
      completedTargetID: "9223372036854775807",
      expectedCalls: 1,
    },
    {
      name: "conflicting completed target id",
      initialTargetID: "9223372036854775807",
      completedTargetID: "9223372036854775806",
      expectedCalls: 2,
    },
  ])("keeps $name in exact interrupted-create recovery", async (fixture) => {
    const client = new GCPClient(env);
    const internal = client as unknown as {
      ensureFirewall(config: ReturnType<typeof leaseConfig>): Promise<void>;
      gcp<T>(method: string, path: string, body?: unknown): Promise<T>;
    };
    vi.spyOn(internal, "ensureFirewall").mockResolvedValue();
    const calls: string[] = [];
    vi.spyOn(internal, "gcp").mockImplementation(async (method, path) => {
      calls.push(`${method} ${path}`);
      if (path.endsWith("/instances")) {
        return { name: "insert-op", targetId: fixture.initialTargetID } as T;
      }
      if (path.endsWith("/operations/insert-op/wait")) {
        return {
          name: "insert-op",
          status: "DONE",
          targetId: fixture.completedTargetID,
        } as T;
      }
      throw new Error(`unexpected GCP request ${method} ${path}`);
    });
    let publications = 0;
    const targets: string[] = [];

    const error = await client
      .createServerWithFallback(
        leaseConfig({
          provider: "gcp",
          gcpZone: "us-central1-a",
          capacity: { availabilityZones: ["us-central1-b"] },
          serverType: "e2-micro",
          serverTypeExplicit: true,
          sshPublicKey: "ssh-ed25519 test",
        }),
        "cbx_abcdef123456",
        "blue-lobster",
        "alice@example.com",
        {
          async onTargetAttempt(target) {
            targets.push(target.region ?? "");
          },
          async onResourceCreated() {
            publications += 1;
            return true;
          },
        },
      )
      .catch((cause: unknown) => cause);

    expect(providerProvisioningOutcomeUncertain(error)).toBe(true);
    expect(providerProvisioningCleanupClaim(error)).toBeUndefined();
    expect(publications).toBe(0);
    expect(targets).toEqual(["us-central1-a"]);
    expect(calls).toHaveLength(fixture.expectedCalls);
  });

  it("never treats an operation error target id as resource ownership", async () => {
    const client = new GCPClient(env);
    const internal = client as unknown as {
      ensureFirewall(config: ReturnType<typeof leaseConfig>): Promise<void>;
      gcp<T>(method: string, path: string, body?: unknown): Promise<T>;
    };
    vi.spyOn(internal, "ensureFirewall").mockResolvedValue();
    vi.spyOn(internal, "gcp").mockImplementation(async (_method, path) => {
      if (path.endsWith("/instances")) {
        return { name: "insert-op", targetId: "9223372036854775807" } as never;
      }
      return {
        name: "insert-op",
        status: "DONE",
        targetId: "9223372036854775807",
        error: { errors: [{ code: "QUOTA_EXCEEDED", message: "quota exceeded" }] },
      } as never;
    });
    let publications = 0;

    const error = await client
      .createServer(
        leaseConfig({
          provider: "gcp",
          serverType: "e2-micro",
          sshPublicKey: "ssh-ed25519 test",
        }),
        "cbx_abcdef123456",
        "blue-lobster",
        "alice@example.com",
        {
          async onResourceCreated() {
            publications += 1;
            return true;
          },
        },
      )
      .catch((cause: unknown) => cause);

    expect(error).toBeInstanceOf(Error);
    expect(providerProvisioningCleanupClaim(error)).toBeUndefined();
    expect(publications).toBe(0);
  });

  it("returns immediately without provenance I/O when publication stops readiness", async () => {
    const client = new GCPClient(env);
    const internal = client as unknown as {
      ensureFirewall(config: ReturnType<typeof leaseConfig>): Promise<void>;
      gcp<T>(method: string, path: string, body?: unknown): Promise<T>;
    };
    vi.spyOn(internal, "ensureFirewall").mockResolvedValue();
    const calls: string[] = [];
    vi.spyOn(internal, "gcp").mockImplementation(async (method, path) => {
      calls.push(`${method} ${path}`);
      if (path.endsWith("/instances")) {
        return { name: "insert-op", targetId: "9223372036854775807" } as T;
      }
      if (path.endsWith("/operations/insert-op/wait")) {
        return {
          name: "insert-op",
          status: "DONE",
          targetId: "9223372036854775807",
        } as T;
      }
      throw new Error(`unexpected GCP request ${method} ${path}`);
    });
    const claims: unknown[] = [];

    const server = await client.createServer(
      leaseConfig({
        provider: "gcp",
        serverType: "e2-micro",
        sshPublicKey: "ssh-ed25519 test",
      }),
      "cbx_abcdef123456",
      "blue-lobster",
      "alice@example.com",
      {
        async onResourceCreated(claim) {
          claims.push(claim);
          return false;
        },
      },
    );

    expect(claims).toEqual([
      {
        provider: "gcp",
        cloudID: leaseProviderName("cbx_abcdef123456", "blue-lobster"),
        region: "us-central1-a",
        providerProject: "default-project",
        providerResourceID: "9223372036854775807",
      },
    ]);
    expect(server).toMatchObject({
      cloudID: leaseProviderName("cbx_abcdef123456", "blue-lobster"),
      status: "provisioning",
      region: "us-central1-a",
    });
    expect(calls).toEqual([
      "POST /zones/us-central1-a/instances",
      "POST /zones/us-central1-a/operations/insert-op/wait",
    ]);
  });

  it("resolves a missing operation target id before publishing cleanup custody", async () => {
    const leaseID = "cbx_abcdef123456";
    const slug = "blue-lobster";
    const instanceName = leaseProviderName(leaseID, slug);
    const lease = observedGCPLease({ id: leaseID, slug, cloudID: instanceName });
    const client = new GCPClient(env);
    const internal = client as unknown as {
      ensureFirewall(config: ReturnType<typeof leaseConfig>): Promise<void>;
      gcp<T>(method: string, path: string, body?: unknown): Promise<T>;
    };
    vi.spyOn(internal, "ensureFirewall").mockResolvedValue();
    const calls: string[] = [];
    vi.spyOn(internal, "gcp").mockImplementation(async (method, path) => {
      calls.push(`${method} ${path}`);
      if (path.endsWith("/instances")) return { name: "insert-op" } as T;
      if (path.endsWith("/operations/insert-op/wait")) {
        return { name: "insert-op", status: "DONE" } as T;
      }
      if (path.endsWith(`/instances/${instanceName}`)) {
        return ownedGCPInstance(lease) as T;
      }
      throw new Error(`unexpected GCP request ${method} ${path}`);
    });
    const claims: ProviderProvisioningCleanupClaim[] = [];

    await expect(
      client.createServer(
        leaseConfig({
          provider: "gcp",
          serverType: "e2-micro",
          sshPublicKey: "ssh-ed25519 test",
        }),
        leaseID,
        slug,
        lease.owner,
        {
          async onResourceCreated(claim) {
            claims.push(claim);
            return false;
          },
        },
      ),
    ).resolves.toMatchObject({ status: "provisioning" });

    expect(claims).toEqual([
      {
        provider: "gcp",
        cloudID: instanceName,
        region: "us-central1-a",
        providerProject: "default-project",
        providerResourceID: "9223372036854775807",
      },
    ]);
    expect(calls).toEqual([
      "POST /zones/us-central1-a/instances",
      "POST /zones/us-central1-a/operations/insert-op/wait",
      `GET /zones/us-central1-a/instances/${instanceName}`,
    ]);
  });

  it("never publishes an id-less GCP cleanup claim", async () => {
    const client = new GCPClient(env);
    const internal = client as unknown as {
      ensureFirewall(config: ReturnType<typeof leaseConfig>): Promise<void>;
      gcp<T>(method: string, path: string, body?: unknown): Promise<T>;
    };
    vi.spyOn(internal, "ensureFirewall").mockResolvedValue();
    vi.spyOn(internal, "gcp").mockImplementation(async (_method, path) => {
      if (path.endsWith("/instances")) return { name: "insert-op" } as T;
      if (path.endsWith("/operations/insert-op/wait")) {
        return { name: "insert-op", status: "DONE" } as T;
      }
      throw new TypeError("instance identity read disconnected");
    });
    let publications = 0;

    const error = await client
      .createServer(
        leaseConfig({
          provider: "gcp",
          serverType: "e2-micro",
          sshPublicKey: "ssh-ed25519 test",
        }),
        "cbx_abcdef123456",
        "blue-lobster",
        "alice@example.com",
        {
          async onResourceCreated() {
            publications += 1;
            return true;
          },
        },
      )
      .catch((cause: unknown) => cause);

    expect(error).toBeInstanceOf(ProviderProvisioningOutcomeUncertainError);
    expect(providerProvisioningCleanupClaim(error)).toBeUndefined();
    expect(publications).toBe(0);
  });

  it("propagates unresolved publication and wraps other post-publication failures", async () => {
    const client = new GCPClient(env);
    const internal = client as unknown as {
      ensureFirewall(config: ReturnType<typeof leaseConfig>): Promise<void>;
      gcp<T>(method: string, path: string, body?: unknown): Promise<T>;
    };
    vi.spyOn(internal, "ensureFirewall").mockResolvedValue();
    const calls: string[] = [];
    vi.spyOn(internal, "gcp").mockImplementation(async (method, path) => {
      calls.push(`${method} ${path}`);
      if (path.endsWith("/instances")) {
        return { name: "insert-op", targetId: "9223372036854775807" } as T;
      }
      if (path.endsWith("/operations/insert-op/wait")) {
        return {
          name: "insert-op",
          status: "DONE",
          targetId: "9223372036854775807",
        } as T;
      }
      if (path.includes("/instances/")) {
        throw new Error("instance read failed");
      }
      throw new Error(`unexpected GCP request ${method} ${path}`);
    });
    const config = leaseConfig({
      provider: "gcp",
      serverType: "e2-micro",
      sshPublicKey: "ssh-ed25519 test",
    });
    const unresolved = new ProviderResourceUnresolvedError("lease incarnation changed");

    const unresolvedResult = await client
      .createServer(config, "cbx_abcdef123456", "blue-lobster", "alice@example.com", {
        async onResourceCreated() {
          throw unresolved;
        },
      })
      .catch((error: unknown) => error);
    expect(unresolvedResult).toBe(unresolved);
    expect(calls).toHaveLength(2);

    calls.length = 0;
    const cleanupError = await client
      .createServer(config, "cbx_abcdef123456", "blue-lobster", "alice@example.com", {
        async onResourceCreated() {
          return true;
        },
      })
      .catch((error: unknown) => error);
    expect(cleanupError).toBeInstanceOf(ProviderProvisioningCleanupError);
    expect(providerProvisioningCleanupClaim(cleanupError)).toEqual({
      provider: "gcp",
      cloudID: leaseProviderName("cbx_abcdef123456", "blue-lobster"),
      region: "us-central1-a",
      providerProject: "default-project",
      providerResourceID: "9223372036854775807",
    });
    expect(calls.some((call) => call.startsWith("DELETE "))).toBe(false);
  });

  it("uses the metadata server when service account key credentials are omitted", async () => {
    const metadataEnv: Env = {
      FLEET: {} as DurableObjectNamespace,
      HETZNER_TOKEN: "",
      CRABBOX_GCP_PROJECT: "default-project",
      CRABBOX_GCP_ZONE: "us-central1-a",
      CRABBOX_GCP_CREDENTIAL_SOURCE: "metadata",
    };
    const client = new GCPClient(metadataEnv);
    const calls: Array<{ url: string; headers: Headers; redirect?: RequestRedirect }> = [];
    client.fetcher = async (input, init) => {
      const url = String(input);
      calls.push({ url, headers: new Headers(init?.headers), redirect: init?.redirect });
      if (url.includes("metadata.google.internal")) {
        return metadataJSON({ access_token: "metadata-token", expires_in: 1200 });
      }
      if (url.includes("/aggregated/instances")) {
        return Response.json({ items: {} });
      }
      throw new Error(`unexpected GCP request ${url}`);
    };

    await expect(client.listCrabboxServers()).resolves.toEqual([]);
    expect(calls[0]?.url).toContain("metadata.google.internal");
    expect(calls[0]?.headers.get("Metadata-Flavor")).toBe("Google");
    expect(calls[0]?.redirect).toBe("error");
    expect(calls[1]?.headers.get("Authorization")).toBe("Bearer metadata-token");
  });

  it("retries transient metadata token failures", async () => {
    vi.useFakeTimers();
    const metadataEnv: Env = {
      FLEET: {} as DurableObjectNamespace,
      HETZNER_TOKEN: "",
      CRABBOX_GCP_PROJECT: "default-project",
      CRABBOX_GCP_CREDENTIAL_SOURCE: "metadata",
    };
    const client = new GCPClient(metadataEnv);
    let metadataCalls = 0;
    client.fetcher = async (input) => {
      const url = String(input);
      if (url.includes("metadata.google.internal")) {
        metadataCalls += 1;
        if (metadataCalls === 1) throw new TypeError("connection refused");
        if (metadataCalls === 2) return metadataResponse("client closed", { status: 499 });
        if (metadataCalls === 3)
          return metadataResponse("control plane unavailable", { status: 500 });
        if (metadataCalls === 4) return metadataResponse("bad gateway", { status: 502 });
        if (metadataCalls === 5) return metadataResponse("busy", { status: 503 });
        if (metadataCalls === 6) return metadataResponse("rate limited", { status: 429 });
        return metadataJSON({ access_token: "metadata-token", expires_in: 1200 });
      }
      if (url.includes("/aggregated/instances")) return Response.json({ items: {} });
      throw new Error(`unexpected GCP request ${url}`);
    };

    const result = client.listCrabboxServers();
    await vi.runAllTimersAsync();
    await expect(result).resolves.toEqual([]);
    expect(metadataCalls).toBe(7);
  });

  it("keeps the HTTP status when metadata errors are not JSON", async () => {
    const metadataEnv: Env = {
      FLEET: {} as DurableObjectNamespace,
      HETZNER_TOKEN: "",
      CRABBOX_GCP_PROJECT: "default-project",
      CRABBOX_GCP_CREDENTIAL_SOURCE: "metadata",
    };
    const client = new GCPClient(metadataEnv);
    client.fetcher = async () =>
      metadataResponse("service account disabled", { status: 401, statusText: "Unauthorized" });

    await expect(client.listCrabboxServers()).rejects.toThrow(
      "gcp metadata token: http 401: Unauthorized",
    );
  });

  it("bounds metadata retries", async () => {
    vi.useFakeTimers();
    const metadataEnv: Env = {
      FLEET: {} as DurableObjectNamespace,
      HETZNER_TOKEN: "",
      CRABBOX_GCP_PROJECT: "default-project",
      CRABBOX_GCP_CREDENTIAL_SOURCE: "metadata",
    };
    const client = new GCPClient(metadataEnv);
    let metadataCalls = 0;
    client.fetcher = async () => {
      metadataCalls += 1;
      return metadataResponse("busy", { status: 503, statusText: "Service Unavailable" });
    };

    const result = client.listCrabboxServers().then(
      () => undefined,
      (error: unknown) => error,
    );
    await vi.runAllTimersAsync();
    const error = await result;
    expect(error).toBeInstanceOf(Error);
    expect((error as Error).message).toBe("gcp metadata token: http 503: Service Unavailable");
    expect(metadataCalls).toBe(7);
  });

  it("bounds metadata connection retries", async () => {
    vi.useFakeTimers();
    const metadataEnv: Env = {
      FLEET: {} as DurableObjectNamespace,
      HETZNER_TOKEN: "",
      CRABBOX_GCP_PROJECT: "default-project",
      CRABBOX_GCP_CREDENTIAL_SOURCE: "metadata",
    };
    const client = new GCPClient(metadataEnv);
    let metadataCalls = 0;
    client.fetcher = async () => {
      metadataCalls += 1;
      throw new TypeError("connection refused");
    };

    const result = client.listCrabboxServers().then(
      () => undefined,
      (error: unknown) => error,
    );
    await vi.runAllTimersAsync();
    const error = await result;
    expect(error).toBeInstanceOf(Error);
    expect((error as Error).message).toBe("gcp metadata token: request failed: connection refused");
    expect(metadataCalls).toBe(7);
  });

  it("rejects token responses without the metadata server response marker", async () => {
    const metadataEnv: Env = {
      FLEET: {} as DurableObjectNamespace,
      HETZNER_TOKEN: "",
      CRABBOX_GCP_PROJECT: "default-project",
      CRABBOX_GCP_CREDENTIAL_SOURCE: "metadata",
    };
    const client = new GCPClient(metadataEnv);
    client.fetcher = async () =>
      Response.json({ access_token: "untrusted-token", expires_in: 1200 });

    await expect(client.listCrabboxServers()).rejects.toThrow(
      "gcp metadata token: response missing Metadata-Flavor: Google",
    );
  });

  it("bounds stalled metadata requests with an overall deadline", async () => {
    vi.useFakeTimers();
    const startedAt = Date.now();
    const metadataEnv: Env = {
      FLEET: {} as DurableObjectNamespace,
      HETZNER_TOKEN: "",
      CRABBOX_GCP_PROJECT: "default-project",
      CRABBOX_GCP_CREDENTIAL_SOURCE: "metadata",
    };
    const client = new GCPClient(metadataEnv);
    let metadataCalls = 0;
    client.fetcher = async (_input, init) => {
      metadataCalls += 1;
      return await new Promise<Response>((_resolve, reject) => {
        init?.signal?.addEventListener(
          "abort",
          () => reject(new DOMException("aborted", "AbortError")),
          { once: true },
        );
      });
    };

    const result = client.listCrabboxServers().then(
      () => undefined,
      (error: unknown) => error,
    );
    await vi.runAllTimersAsync();
    const error = await result;
    expect(error).toBeInstanceOf(Error);
    expect((error as Error).message).toBe("gcp metadata token: request failed: request timed out");
    expect(metadataCalls).toBeGreaterThan(1);
    expect(Date.now() - startedAt).toBeGreaterThanOrEqual(60_000);
  });

  it("keeps the metadata timeout active while reading the response body", async () => {
    vi.useFakeTimers();
    const startedAt = Date.now();
    const metadataEnv: Env = {
      FLEET: {} as DurableObjectNamespace,
      HETZNER_TOKEN: "",
      CRABBOX_GCP_PROJECT: "default-project",
      CRABBOX_GCP_CREDENTIAL_SOURCE: "metadata",
    };
    const client = new GCPClient(metadataEnv);
    let metadataCalls = 0;
    client.fetcher = async (_input, init) => {
      metadataCalls += 1;
      const body = new ReadableStream<Uint8Array>({
        start(controller) {
          init?.signal?.addEventListener(
            "abort",
            () => controller.error(new DOMException("aborted", "AbortError")),
            { once: true },
          );
        },
      });
      return metadataResponse(body);
    };

    const result = client.listCrabboxServers().then(
      () => undefined,
      (error: unknown) => error,
    );
    await vi.runAllTimersAsync();
    const error = await result;
    expect(error).toBeInstanceOf(Error);
    expect((error as Error).message).toBe("gcp metadata token: request failed: request timed out");
    expect(metadataCalls).toBeGreaterThan(1);
    expect(Date.now() - startedAt).toBeGreaterThanOrEqual(60_000);
  });

  it("refreshes metadata tokens at the five-minute cache boundary", async () => {
    const metadataEnv: Env = {
      FLEET: {} as DurableObjectNamespace,
      HETZNER_TOKEN: "",
      CRABBOX_GCP_PROJECT: "default-project",
      CRABBOX_GCP_CREDENTIAL_SOURCE: "metadata",
    };
    const client = new GCPClient(metadataEnv);
    (client as unknown as { cache: { token: string; expiresAt: number } }).cache = {
      token: "expiring-token",
      expiresAt: Math.trunc(Date.now() / 1000) + 300,
    };
    const authorizations: string[] = [];
    let metadataCalls = 0;
    client.fetcher = async (input, init) => {
      if (String(input).includes("metadata.google.internal")) {
        metadataCalls += 1;
        return metadataJSON({ access_token: "fresh-token", expires_in: 3600 });
      }
      authorizations.push(new Headers(init?.headers).get("Authorization") ?? "");
      return Response.json({ items: {} });
    };

    await expect(client.listCrabboxServers()).resolves.toEqual([]);
    expect(metadataCalls).toBe(1);
    expect(authorizations).toEqual(["Bearer fresh-token"]);
  });

  it("keeps service account key tokens until the one-minute cache boundary", async () => {
    const client = new GCPClient(env);
    (client as unknown as { cache: { token: string; expiresAt: number } }).cache = {
      token: "cached-token",
      expiresAt: Math.trunc(Date.now() / 1000) + 120,
    };
    const calls: Array<{ url: string; authorization: string }> = [];
    client.fetcher = async (input, init) => {
      calls.push({
        url: String(input),
        authorization: new Headers(init?.headers).get("Authorization") ?? "",
      });
      return Response.json({ items: {} });
    };

    await expect(client.listCrabboxServers()).resolves.toEqual([]);
    expect(calls).toHaveLength(1);
    expect(calls[0]?.url).toContain("/aggregated/instances");
    expect(calls[0]?.authorization).toBe("Bearer cached-token");
  });

  it("rejects partial service account key credentials", () => {
    expect(() => new GCPClient({ ...env, GCP_PRIVATE_KEY: "" })).toThrow(
      "GCP_CLIENT_EMAIL and GCP_PRIVATE_KEY must be configured together",
    );
    expect(() => new GCPClient({ ...env, GCP_CLIENT_EMAIL: "" })).toThrow(
      "GCP_CLIENT_EMAIL and GCP_PRIVATE_KEY must be configured together",
    );
  });

  it("requires explicit metadata credential source when service account key credentials are omitted", () => {
    const metadataEnv: Env = {
      FLEET: {} as DurableObjectNamespace,
      HETZNER_TOKEN: "",
      CRABBOX_GCP_PROJECT: "default-project",
      CRABBOX_GCP_ZONE: "us-central1-a",
    };
    expect(() => new GCPClient(metadataEnv)).toThrow(
      "GCP_CLIENT_EMAIL and GCP_PRIVATE_KEY are required unless CRABBOX_GCP_CREDENTIAL_SOURCE=metadata",
    );
  });

  it("rejects invalid configured GCP credential sources", () => {
    expect(
      () => new GCPClient({ ...env, CRABBOX_GCP_CREDENTIAL_SOURCE: "workload-identity" }),
    ).toThrow("CRABBOX_GCP_CREDENTIAL_SOURCE must be metadata or service-account-key");
  });

  it("rejects invalid configured GCP SSH CIDRs", () => {
    expect(() => new GCPClient({ ...env, CRABBOX_GCP_SSH_CIDRS: "::::/128" })).toThrow(
      "CRABBOX_GCP_SSH_CIDRS entries must be valid",
    );
  });

  it("treats only the exact zonal instance as absent", async () => {
    const client = new GCPClient(env);
    (client as unknown as { cache: { token: string; expiresAt: number } }).cache = {
      token: "test-token",
      expiresAt: Math.trunc(Date.now() / 1000) + 3600,
    };
    let message =
      "The resource 'projects/default-project/zones/us-central1-a/instances/crabbox-blue-lobster' was not found";
    client.fetcher = async () =>
      new Response(JSON.stringify({ error: { code: 404, message } }), { status: 404 });

    await expect(client.findServer("crabbox-blue-lobster")).resolves.toBeUndefined();
    message = "The resource 'projects/default-project' was not found";
    await expect(client.findServer("crabbox-blue-lobster")).rejects.toThrow(
      "projects/default-project",
    );
  });

  it("treats only an exact missing instance DELETE as complete", async () => {
    const client = new GCPClient(env);
    (client as unknown as { cache: { token: string; expiresAt: number } }).cache = {
      token: "test-token",
      expiresAt: Math.trunc(Date.now() / 1000) + 3600,
    };
    let message =
      "The resource 'projects/default-project/zones/us-central1-a/instances/crabbox-blue-lobster' was not found";
    client.fetcher = async () =>
      new Response(JSON.stringify({ error: { code: 404, message } }), { status: 404 });

    await expect(client.deleteServer("crabbox-blue-lobster")).resolves.toBeUndefined();
    message = "The resource 'projects/default-project' was not found";
    await expect(client.deleteServer("crabbox-blue-lobster")).rejects.toThrow(
      "projects/default-project",
    );
  });

  it("does not delete a foreign same-name instance after a failed create", async () => {
    const client = new GCPClient(env);
    const calls: string[] = [];
    const internal = client as unknown as {
      ensureFirewall(config: ReturnType<typeof leaseConfig>): Promise<void>;
      gcp<T>(method: string, path: string, body?: unknown): Promise<T>;
    };
    vi.spyOn(internal, "ensureFirewall").mockResolvedValue();
    vi.spyOn(internal, "gcp").mockImplementation(async (method, path) => {
      calls.push(`${method} ${path}`);
      if (method === "POST") throw new Error("already exists");
      return {
        id: "1",
        name: leaseProviderName("cbx_abcdef123456", "blue-lobster"),
        zone: "projects/default-project/zones/us-central1-a",
        machineType: "zones/us-central1-a/machineTypes/e2-micro",
        labels: {
          crabbox: "true",
          created_by: "crabbox",
          provider: "gcp",
          lease: "cbx_000000000000",
          owner: "foreign_example_com",
          slug: "blue-lobster",
        },
      } as T;
    });

    const error = await client
      .createServer(
        leaseConfig({
          provider: "gcp",
          serverType: "e2-micro",
          gcpProject: "default-project",
          gcpZone: "us-central1-a",
          sshPublicKey: "ssh-ed25519 test",
        }),
        "cbx_abcdef123456",
        "blue-lobster",
        "alice@example.com",
      )
      .catch((caught: unknown) => caught);
    expect(error).toBeInstanceOf(Error);
    expect(String(error)).toContain("provisioning cleanup failed closed");
    expect(providerProvisioningCleanupClaim(error)).toEqual({
      provider: "gcp",
      cloudID: leaseProviderName("cbx_abcdef123456", "blue-lobster"),
      region: "us-central1-a",
      providerProject: "default-project",
    });
    expect(calls.some((call) => call.startsWith("DELETE "))).toBe(false);
  });

  it("recovers when another create wins the shared firewall race", async () => {
    const client = new GCPClient(env);
    (client as unknown as { cache: { token: string; expiresAt: number } }).cache = {
      token: "test-token",
      expiresAt: Math.trunc(Date.now() / 1000) + 3600,
    };
    const calls: string[] = [];
    let firewallReads = 0;
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      const method = init?.method ?? "GET";
      calls.push(`${method} ${url.pathname}`);
      if (url.pathname.includes("/global/firewalls/") && method === "GET") {
        firewallReads += 1;
        return firewallReads === 1
          ? new Response("not found", { status: 404 })
          : Response.json({ description: "Crabbox-managed SSH ingress" });
      }
      if (url.pathname.endsWith("/global/firewalls") && method === "POST") {
        return new Response("already exists", { status: 409 });
      }
      return Response.json({});
    };

    await (
      client as unknown as { ensureFirewall(config: ReturnType<typeof leaseConfig>): Promise<void> }
    ).ensureFirewall(
      leaseConfig({
        provider: "gcp",
        gcpSSHCIDRs: ["198.51.100.77/32"],
        sshPublicKey: "ssh-ed25519 test",
      }),
    );

    expect(calls.filter((call) => call.startsWith("GET "))).toHaveLength(2);
    expect(calls.filter((call) => call.startsWith("POST "))).toHaveLength(1);
    expect(calls.filter((call) => call.startsWith("PUT "))).toHaveLength(1);
  });

  it("waits for a raced firewall insert before reconciling policy", async () => {
    vi.useFakeTimers();
    const client = new GCPClient(env);
    (client as unknown as { cache: { token: string; expiresAt: number } }).cache = {
      token: "test-token",
      expiresAt: Math.trunc(Date.now() / 1000) + 3600,
    };
    let firewallReads = 0;
    let firewallUpdates = 0;
    let operationWaits = 0;
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      const method = init?.method ?? "GET";
      if (url.pathname.includes("/global/firewalls/") && method === "GET") {
        firewallReads += 1;
        return firewallReads < 3
          ? new Response("not found", { status: 404 })
          : Response.json({ description: "Crabbox-managed SSH ingress" });
      }
      if (url.pathname.endsWith("/global/firewalls") && method === "POST") {
        return new Response("already exists", { status: 409 });
      }
      if (url.pathname.includes("/global/firewalls/") && method === "PUT") {
        firewallUpdates += 1;
        return firewallUpdates === 1
          ? new Response("operation in progress", { status: 409 })
          : Response.json({ name: "op-raced", status: "PENDING" });
      }
      if (url.pathname.endsWith("/global/operations/op-raced/wait") && method === "POST") {
        operationWaits += 1;
        return Response.json({ name: "op-raced", status: "DONE" });
      }
      return Response.json({});
    };

    const ensure = (
      client as unknown as { ensureFirewall(config: ReturnType<typeof leaseConfig>): Promise<void> }
    ).ensureFirewall(
      leaseConfig({
        provider: "gcp",
        gcpSSHCIDRs: ["198.51.100.77/32"],
        sshPublicKey: "ssh-ed25519 test",
      }),
    );
    await vi.runAllTimersAsync();
    await ensure;

    expect(firewallReads).toBe(4);
    expect(firewallUpdates).toBe(2);
    expect(operationWaits).toBe(1);
  });

  it("lists Crabbox machines across aggregated GCP zones", async () => {
    const client = new GCPClient(env);
    (client as unknown as { cache: { token: string; expiresAt: number } }).cache = {
      token: "test-token",
      expiresAt: Math.trunc(Date.now() / 1000) + 3600,
    };
    client.fetcher = async (input) => {
      const url = new URL(String(input));
      expect(url.pathname).toBe("/compute/v1/projects/default-project/aggregated/instances");
      expect(url.searchParams.get("filter")).toBe("labels.crabbox = true");
      expect(url.searchParams.get("returnPartialSuccess")).toBe("true");
      return Response.json({
        items: {
          "zones/us-central1-a": {
            instances: [
              {
                id: "1",
                name: leaseProviderName("cbx_000000000001", "alpha"),
                machineType: "zones/us-central1-a/machineTypes/e2-micro",
                labels: {
                  crabbox: "true",
                  created_by: "crabbox",
                  provider: "gcp",
                  lease: "cbx_000000000001",
                  slug: "alpha",
                },
              },
              {
                id: "forged",
                name: "crabbox-wrong-deterministic-name",
                machineType: "zones/us-central1-a/machineTypes/e2-micro",
                labels: {
                  crabbox: "true",
                  created_by: "crabbox",
                  provider: "gcp",
                  lease: "cbx_000000000099",
                  slug: "forged",
                },
              },
            ],
          },
          "zones/europe-west2-b": {
            instances: [
              {
                id: "2",
                name: leaseProviderName("cbx_000000000002", "bravo"),
                zone: "projects/default-project/zones/europe-west2-b",
                machineType: "zones/europe-west2-b/machineTypes/c4-standard-32",
                labels: {
                  crabbox: "true",
                  created_by: "crabbox",
                  provider: "gcp",
                  lease: "cbx_000000000002",
                  slug: "bravo",
                },
              },
            ],
          },
        },
      });
    };

    const servers = await client.listCrabboxServers();
    expect(servers.map((server) => [server.name, server.region])).toEqual([
      [leaseProviderName("cbx_000000000001", "alpha"), "us-central1-a"],
      [leaseProviderName("cbx_000000000002", "bravo"), "europe-west2-b"],
    ]);
  });

  it("recovers only the exact persisted GCP instance identity with one lookup", async () => {
    const name = leaseProviderName("cbx_abcdef123456", "blue-lobster");
    const lease = observedGCPLease({ cloudID: name, serverName: name });
    const client = new GCPClient(env);
    primeAccessToken(client);
    const requests: string[] = [];
    client.fetcher = async (input) => {
      requests.push(String(input));
      return Response.json(ownedGCPInstance(lease));
    };

    await expect(client.recoverServerForLease(lease)).resolves.toMatchObject({
      cloudID: name,
      providerResourceID: lease.providerResourceID,
    });
    expect(requests).toEqual([
      `https://compute.googleapis.com/compute/v1/projects/default-project/zones/us-central1-a/instances/${name}`,
    ]);
  });

  it("observes one exact owned legacy cleanup identity from a safe numeric anchor", async () => {
    const name = leaseProviderName("cbx_abcdef123456", "blue-lobster");
    const lease = observedGCPLease({
      cloudID: name,
      serverName: name,
      serverID: 123,
      providerResourceID: undefined,
    });
    const client = new GCPClient(env);
    primeAccessToken(client);
    const requests: string[] = [];
    client.fetcher = async (input) => {
      requests.push(String(input));
      return Response.json(ownedGCPInstance(lease, { id: "123" }));
    };

    await expect(client.observeLegacyCleanupIdentity(lease)).resolves.toEqual({
      providerResourceID: "123",
    });
    expect(requests).toEqual([
      `https://compute.googleapis.com/compute/v1/projects/default-project/zones/us-central1-a/instances/${name}`,
    ]);
    expect(requests[0]).not.toContain("aggregated");
  });

  it("prefers a durable legacy cleanup anchor over the lossy numeric server id", async () => {
    const name = leaseProviderName("cbx_abcdef123456", "blue-lobster");
    const lease = observedGCPLease({
      cloudID: name,
      serverName: name,
      serverID: Number("9223372036854775807"),
      providerResourceID: undefined,
    });
    const client = new GCPClient(env);
    primeAccessToken(client);
    client.fetcher = async () =>
      Response.json(ownedGCPInstance(lease, { id: "9223372036854775807" }));

    await expect(
      client.observeLegacyCleanupIdentity(lease, {
        resourceIdentity: "9223372036854775807",
      }),
    ).resolves.toEqual({ providerResourceID: "9223372036854775807" });
  });

  it("treats only exact legacy cleanup absence as authoritative", async () => {
    const name = leaseProviderName("cbx_abcdef123456", "blue-lobster");
    const lease = observedGCPLease({
      cloudID: name,
      serverName: name,
      serverID: 123,
      providerResourceID: undefined,
    });
    const client = new GCPClient(env);
    primeAccessToken(client);
    client.fetcher = async () =>
      new Response(
        JSON.stringify({
          error: {
            code: 404,
            message: `The resource 'projects/default-project/zones/us-central1-a/instances/${name}' was not found`,
          },
        }),
        { status: 404 },
      );

    await expect(client.observeLegacyCleanupIdentity(lease)).resolves.toBeUndefined();
    client.fetcher = async () => new Response("temporarily unavailable", { status: 503 });
    await expect(client.observeLegacyCleanupIdentity(lease)).rejects.toThrow("http 503");
  });

  it("rejects unsafe anchors, malformed scope, replacements, and foreign ownership without inventory", async () => {
    const name = leaseProviderName("cbx_abcdef123456", "blue-lobster");
    const baseLease = observedGCPLease({
      cloudID: name,
      serverName: name,
      serverID: 123,
      providerResourceID: undefined,
    });
    const fixtures: Array<{
      name: string;
      lease: LeaseRecord;
      context?: { resourceIdentity?: string };
      instance: Record<string, unknown>;
      requests: number;
    }> = [
      {
        name: "unsafe numeric anchor",
        lease: { ...baseLease, serverID: Number.MAX_SAFE_INTEGER + 1 },
        instance: ownedGCPInstance(baseLease, { id: "9007199254740992" }),
        requests: 0,
      },
      {
        name: "missing numeric anchor",
        lease: { ...baseLease, serverID: 0 },
        instance: ownedGCPInstance(baseLease, { id: "123" }),
        requests: 0,
      },
      {
        name: "malformed durable anchor",
        lease: baseLease,
        context: { resourceIdentity: "123 " },
        instance: ownedGCPInstance(baseLease, { id: "123" }),
        requests: 0,
      },
      {
        name: "noncanonical project",
        lease: { ...baseLease, providerProject: "proj" },
        instance: ownedGCPInstance(baseLease, { id: "123" }),
        requests: 0,
      },
      {
        name: "wrong canonical name",
        lease: { ...baseLease, serverName: "crabbox-wrong-name" },
        instance: ownedGCPInstance(baseLease, { id: "123" }),
        requests: 0,
      },
      {
        name: "replacement id",
        lease: baseLease,
        instance: ownedGCPInstance(baseLease, { id: "124" }),
        requests: 1,
      },
      {
        name: "foreign owner",
        lease: baseLease,
        instance: ownedGCPInstance(baseLease, {
          id: "123",
          labels: { ...(ownedGCPInstance(baseLease).labels as object), owner: "mallory" },
        }),
        requests: 1,
      },
      {
        name: "missing raw id",
        lease: baseLease,
        instance: ownedGCPInstance(baseLease, { id: undefined }),
        requests: 1,
      },
    ];

    for (const fixture of fixtures) {
      const client = new GCPClient(env);
      primeAccessToken(client);
      const requests: string[] = [];
      client.fetcher = async (input) => {
        requests.push(String(input));
        return Response.json(fixture.instance);
      };

      // oxlint-disable-next-line eslint/no-await-in-loop -- each cleanup refusal is isolated.
      await expect(
        client.observeLegacyCleanupIdentity(fixture.lease, fixture.context),
      ).rejects.toBeInstanceOf(ProviderResourceUnresolvedError);
      expect({ name: fixture.name, requests: requests.length }).toEqual({
        name: fixture.name,
        requests: fixture.requests,
      });
      expect(requests.some((request) => request.includes("aggregated"))).toBe(false);
    }
  });

  it("captures only an exact owned unbound instance with one canonical lookup", async () => {
    const name = leaseProviderName("cbx_abcdef123456", "blue-lobster");
    const ownedLease = observedGCPLease({ cloudID: name, serverName: name });
    const unboundLease = structuredClone(ownedLease);
    unboundLease.cloudID = "";
    unboundLease.serverID = 0;
    delete unboundLease.providerResourceID;
    unboundLease.serverName = "";
    unboundLease.host = "";
    const client = new GCPClient(env);
    primeAccessToken(client);
    const requests: string[] = [];
    client.fetcher = async (input) => {
      requests.push(String(input));
      return Response.json(ownedGCPInstance(ownedLease));
    };

    await expect(client.recoverUnboundProvisioningResource(unboundLease)).resolves.toMatchObject({
      cloudID: name,
      providerResourceID: ownedLease.providerResourceID,
    });
    expect(requests).toEqual([
      `https://compute.googleapis.com/compute/v1/projects/default-project/zones/us-central1-a/instances/${name}`,
    ]);
  });

  it("retries strict unbound recovery only for the same captured raw id", async () => {
    const name = leaseProviderName("cbx_abcdef123456", "blue-lobster");
    const ownedLease = observedGCPLease({ cloudID: name, serverName: name });
    const capturedLease = structuredClone(ownedLease);
    capturedLease.cloudID = "";
    capturedLease.serverID = 0;
    capturedLease.serverName = "";
    capturedLease.host = "";
    const client = new GCPClient(env);
    primeAccessToken(client);
    client.fetcher = async () => Response.json(ownedGCPInstance(ownedLease));

    await expect(client.recoverUnboundProvisioningResource(capturedLease)).resolves.toMatchObject({
      cloudID: name,
      providerResourceID: capturedLease.providerResourceID,
    });

    const replacement = ownedGCPInstance(ownedLease);
    replacement.id = "9223372036854775806";
    client.fetcher = async () => Response.json(replacement);
    await expect(client.recoverUnboundProvisioningResource(capturedLease)).rejects.toBeInstanceOf(
      ProviderResourceUnresolvedError,
    );
  });

  it("rejects malformed scope and foreign unbound recovery evidence without inventory fallback", async () => {
    const name = leaseProviderName("cbx_abcdef123456", "blue-lobster");
    const ownedLease = observedGCPLease({ cloudID: name, serverName: name });
    const unboundLease = structuredClone(ownedLease);
    unboundLease.cloudID = "";
    unboundLease.serverID = 0;
    delete unboundLease.providerResourceID;
    unboundLease.serverName = "";
    unboundLease.host = "";
    const fixtures: Array<{
      name: string;
      lease: LeaseRecord;
      instance: Record<string, unknown>;
      requests: number;
    }> = [
      {
        name: "noncanonical project",
        lease: { ...unboundLease, providerProject: "proj" },
        instance: ownedGCPInstance(ownedLease),
        requests: 0,
      },
      {
        name: "existing identity",
        lease: { ...unboundLease, cloudID: name },
        instance: ownedGCPInstance(ownedLease),
        requests: 0,
      },
      {
        name: "missing raw id",
        lease: unboundLease,
        instance: ownedGCPInstance(ownedLease, { id: undefined }),
        requests: 1,
      },
      {
        name: "foreign owner",
        lease: unboundLease,
        instance: ownedGCPInstance(ownedLease, {
          labels: { ...(ownedGCPInstance(ownedLease).labels as object), owner: "mallory" },
        }),
        requests: 1,
      },
      {
        name: "wrong canonical name",
        lease: unboundLease,
        instance: ownedGCPInstance(ownedLease, { name: "crabbox-wrong-name" }),
        requests: 1,
      },
    ];

    for (const fixture of fixtures) {
      const client = new GCPClient(env);
      primeAccessToken(client);
      const requests: string[] = [];
      client.fetcher = async (input) => {
        requests.push(String(input));
        return Response.json(fixture.instance);
      };

      // oxlint-disable-next-line eslint/no-await-in-loop -- each recovery authority failure is isolated.
      await expect(client.recoverUnboundProvisioningResource(fixture.lease)).rejects.toBeInstanceOf(
        ProviderResourceUnresolvedError,
      );
      expect({ name: fixture.name, requests: requests.length }).toEqual({
        name: fixture.name,
        requests: fixture.requests,
      });
      expect(requests.some((request) => request.includes("aggregated"))).toBe(false);
    }
  });

  it("treats only an exact persisted GCP instance 404 as authoritative absence", async () => {
    const name = leaseProviderName("cbx_abcdef123456", "blue-lobster");
    const lease = observedGCPLease({ cloudID: name, serverName: name });
    const client = new GCPClient(env);
    primeAccessToken(client);
    const requests: string[] = [];
    client.fetcher = async (input) => {
      requests.push(String(input));
      return new Response(`projects/default-project/zones/us-central1-a/instances/${name}`, {
        status: 404,
      });
    };

    await expect(client.recoverServerForLease(lease)).resolves.toBeUndefined();
    expect(requests).toHaveLength(1);
    expect(requests[0]).not.toContain("aggregated");
  });

  it("refuses recovery without the persisted raw ID or when identity and ownership drift", async () => {
    const name = leaseProviderName("cbx_abcdef123456", "blue-lobster");
    const baseLease = observedGCPLease({ cloudID: name, serverName: name });
    const missingIDLease = structuredClone(baseLease);
    delete missingIDLease.providerResourceID;
    const fixtures = [
      {
        name: "missing persisted id",
        lease: missingIDLease,
        instance: ownedGCPInstance(baseLease),
        requests: 0,
      },
      {
        name: "replacement id",
        lease: baseLease,
        instance: ownedGCPInstance(baseLease, { id: "9223372036854775806" }),
        requests: 1,
      },
      {
        name: "foreign ownership",
        lease: baseLease,
        instance: ownedGCPInstance(baseLease, {
          labels: { ...(ownedGCPInstance(baseLease).labels as object), owner: "mallory" },
        }),
        requests: 1,
      },
    ];

    for (const fixture of fixtures) {
      const client = new GCPClient(env);
      primeAccessToken(client);
      let requests = 0;
      client.fetcher = async () => {
        requests += 1;
        return Response.json(fixture.instance);
      };

      // oxlint-disable-next-line eslint/no-await-in-loop -- each recovery authority failure is isolated.
      await expect(client.recoverServerForLease(fixture.lease)).rejects.toBeInstanceOf(
        ProviderResourceUnresolvedError,
      );
      expect({ name: fixture.name, requests }).toEqual({
        name: fixture.name,
        requests: fixture.requests,
      });
    }
  });

  it("publishes each fallback zone before creating an instance", async () => {
    const client = new GCPClient(env);
    const attemptedZones: string[] = [];
    const createCalls: string[] = [];
    vi.spyOn(GCPClient.prototype, "createServer").mockImplementation(async (config) => {
      createCalls.push(config.gcpZone);
      if (config.gcpZone === "us-central1-a") {
        throw new Error("ZONE_RESOURCE_POOL_EXHAUSTED");
      }
      return {
        provider: "gcp",
        id: 1,
        cloudID: "crabbox-blue-lobster-c80c2195",
        name: "crabbox-blue-lobster-c80c2195",
        status: "running",
        serverType: config.serverType,
        region: config.gcpZone,
        host: "192.0.2.10",
        labels: {},
      };
    });

    const config = leaseConfig({
      provider: "gcp",
      gcpZone: "us-central1-a",
      capacity: { availabilityZones: ["us-central1-b"] },
      serverType: "e2-micro",
      serverTypeExplicit: true,
      sshPublicKey: "ssh-ed25519 test",
    });
    await client.createServerWithFallback(
      config,
      "cbx_abcdef123456",
      "blue-lobster",
      "alice@example.com",
      {
        async onTargetAttempt(target) {
          attemptedZones.push(target.region ?? "");
        },
      },
    );

    expect(attemptedZones).toEqual(["us-central1-a", "us-central1-b"]);
    expect(createCalls).toEqual(["us-central1-a", "us-central1-b"]);
  });

  it("creates and deletes machine images through Compute Engine", async () => {
    const client = new GCPClient(env);
    (client as unknown as { cache: { token: string; expiresAt: number } }).cache = {
      token: "test-token",
      expiresAt: Math.trunc(Date.now() / 1000) + 3600,
    };
    const calls: Array<{ method: string; path: string; body: unknown }> = [];
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      const body = init?.body ? JSON.parse(String(init.body)) : undefined;
      calls.push({ method: init?.method ?? "GET", path: url.pathname + url.search, body });
      if (url.pathname.endsWith("/global/operations/op-1/wait")) {
        return Response.json({ name: "op-1", status: "DONE" });
      }
      if (url.pathname.endsWith("/global/machineImages/checkpoint-gcp") && init?.method === "GET") {
        return Response.json({
          name: "checkpoint-gcp",
          selfLink: "projects/default-project/global/machineImages/checkpoint-gcp",
          status: "READY",
        });
      }
      return Response.json({ name: "op-1", status: "PENDING" });
    };

    const image = await client.createImage("crabbox-source", "checkpoint-gcp");
    await client.deleteImage("checkpoint-gcp");

    expect(image).toMatchObject({
      id: "checkpoint-gcp",
      provider: "gcp",
      kind: "gcp-machine-image",
      state: "ready",
    });
    expect(calls.map((call) => `${call.method} ${call.path}`)).toEqual([
      "POST /compute/v1/projects/default-project/global/machineImages",
      "POST /compute/v1/projects/default-project/global/operations/op-1/wait",
      "GET /compute/v1/projects/default-project/global/machineImages/checkpoint-gcp",
      "DELETE /compute/v1/projects/default-project/global/machineImages/checkpoint-gcp",
      "POST /compute/v1/projects/default-project/global/operations/op-1/wait",
    ]);
    expect(calls[0]?.body).toMatchObject({
      name: "checkpoint-gcp",
      sourceInstance: "zones/us-central1-a/instances/crabbox-source",
    });
  });

  it.each(["chk_owned_gcp", `chk_${"a".repeat(124)}`])(
    "encodes bounded GCP ownership labels for checkpoint id %s",
    async (checkpointID) => {
      const client = new GCPClient(env);
      (client as unknown as { cache: { token: string; expiresAt: number } }).cache = {
        token: "test-token",
        expiresAt: Math.trunc(Date.now() / 1000) + 3600,
      };
      const ownership = {
        checkpointID,
        sourceLeaseID: "cbx_000000000001",
        tokenHash: "a".repeat(64),
      };
      let labels: Record<string, string> = {};
      client.fetcher = async (input, init) => {
        const url = new URL(String(input));
        if (url.pathname.endsWith("/global/machineImages") && init?.method === "POST") {
          labels = (JSON.parse(String(init.body)) as { labels: Record<string, string> }).labels;
          return Response.json({ name: "op-managed", status: "PENDING" });
        }
        if (url.pathname.endsWith("/global/operations/op-managed/wait")) {
          return Response.json({ name: "op-managed", status: "DONE" });
        }
        return Response.json({
          id: "987654321012345678",
          name: "checkpoint-managed",
          selfLink: "projects/default-project/global/machineImages/checkpoint-managed",
          status: "READY",
          labels,
        });
      };
      const image = await client.createImage("crabbox-source", "checkpoint-managed", ownership);
      expect(labels).toMatchObject({
        crabbox_checkpoint_id:
          ownership.checkpointID.length <= 63
            ? ownership.checkpointID
            : ownership.tokenHash.slice(0, 63),
        crabbox_checkpoint_token_a: "a".repeat(32),
        crabbox_checkpoint_token_b: "a".repeat(32),
        crabbox_checkpoint_lease: ownership.sourceLeaseID,
      });
      expect(Object.values(labels).every((value) => value.length <= 63)).toBe(true);
      expect(image).toMatchObject({
        immutableID: "987654321012345678",
        checkpointOwnershipHash: ownership.tokenHash,
        checkpointSourceLeaseID: ownership.sourceLeaseID,
      });
    },
  );

  it("accepts only exact project and resource kind in checkpoint not-found responses", async () => {
    const client = new GCPClient(env);
    (client as unknown as { cache: { token: string; expiresAt: number } }).cache = {
      token: "test-token",
      expiresAt: Math.trunc(Date.now() / 1000) + 3600,
    };
    client.fetcher = async (input) => {
      const url = new URL(String(input));
      const resource = url.pathname.slice(url.pathname.indexOf("projects/"));
      return Response.json(
        { error: { code: 404, message: `The resource '${resource}' was not found` } },
        { status: 404 },
      );
    };
    let failure: unknown;
    try {
      await client.getImage("checkpoint-gcp", "gcp-disk-snapshot");
    } catch (error) {
      failure = error;
    }
    expect(gcpSnapshotNotFound(failure, "default-project", "checkpoint-gcp")).toBe(true);
    expect(gcpSnapshotNotFound(failure, "other-project", "checkpoint-gcp")).toBe(false);
    expect(gcpSnapshotNotFound(failure, "default-project", "other")).toBe(false);
    expect(gcpMachineImageNotFound(failure, "default-project", "checkpoint-gcp")).toBe(false);
  });

  it("routes kind-specific snapshot reads and deletes to GCP snapshots", async () => {
    const client = new GCPClient(env);
    (client as unknown as { cache: { token: string; expiresAt: number } }).cache = {
      token: "test-token",
      expiresAt: Math.trunc(Date.now() / 1000) + 3600,
    };
    const calls: Array<{ method: string; path: string }> = [];
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      calls.push({ method: init?.method ?? "GET", path: url.pathname + url.search });
      if (url.pathname.endsWith("/global/operations/op-1/wait")) {
        return Response.json({ name: "op-1", status: "DONE" });
      }
      if (url.pathname.endsWith("/global/snapshots/checkpoint-gcp") && init?.method !== "DELETE") {
        return Response.json({
          name: "checkpoint-gcp",
          selfLink: "projects/default-project/global/snapshots/checkpoint-gcp",
          status: "READY",
        });
      }
      return Response.json({ name: "op-1", status: "PENDING" });
    };

    const image = await client.getImage(
      "projects/default-project/global/snapshots/checkpoint-gcp",
      "gcp-disk-snapshot",
    );
    await client.deleteImage(
      "projects/default-project/global/snapshots/checkpoint-gcp",
      "gcp-disk-snapshot",
    );

    expect(image).toMatchObject({
      id: "checkpoint-gcp",
      provider: "gcp",
      kind: "gcp-disk-snapshot",
    });
    expect(calls.map((call) => `${call.method} ${call.path}`)).toEqual([
      "GET /compute/v1/projects/default-project/global/snapshots/checkpoint-gcp",
      "DELETE /compute/v1/projects/default-project/global/snapshots/checkpoint-gcp",
      "POST /compute/v1/projects/default-project/global/operations/op-1/wait",
    ]);
  });

  it("creates instances from machine images without boot disk initialization", async () => {
    const client = new GCPClient(env);
    (client as unknown as { cache: { token: string; expiresAt: number } }).cache = {
      token: "test-token",
      expiresAt: Math.trunc(Date.now() / 1000) + 3600,
    };
    const calls: Array<{
      method: string;
      path: string;
      body: Record<string, unknown> | undefined;
    }> = [];
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      const method = init?.method ?? "GET";
      const body = init?.body
        ? (JSON.parse(String(init.body)) as Record<string, unknown>)
        : undefined;
      calls.push({ method, path: url.pathname + url.search, body });
      if (url.pathname.endsWith("/global/firewalls/crabbox-ssh") && method === "GET") {
        return new Response("not found", { status: 404 });
      }
      if (url.pathname.endsWith("/global/operations/op-firewall/wait")) {
        return Response.json({ name: "op-firewall", status: "DONE" });
      }
      if (url.pathname.endsWith("/zones/us-central1-a/operations/op-instance/wait")) {
        return Response.json({ name: "op-instance", status: "DONE" });
      }
      if (url.pathname.endsWith("/global/firewalls") && method === "POST") {
        return Response.json({ name: "op-firewall", status: "PENDING" });
      }
      if (url.pathname.endsWith("/zones/us-central1-a/instances") && method === "POST") {
        return Response.json({ name: "op-instance", status: "PENDING" });
      }
      if (url.pathname.includes("/zones/us-central1-a/instances/crabbox-blue-lobster-")) {
        return Response.json({
          id: "123",
          name: url.pathname.split("/").pop(),
          status: "RUNNING",
          machineType: "zones/us-central1-a/machineTypes/e2-micro",
          networkInterfaces: [{ accessConfigs: [{ natIP: "192.0.2.5" }] }],
        });
      }
      return Response.json({});
    };

    const config = leaseConfig({
      provider: "gcp",
      serverType: "e2-micro",
      gcpMachineImage: "checkpoint-gcp",
      sshPublicKey: "ssh-ed25519 test",
    });
    const server = await client.createServer(
      config,
      "cbx_123456789abc",
      "blue-lobster",
      "alice@example.com",
    );

    const createCall = calls.find(
      (call) => call.method === "POST" && call.path.includes("/zones/us-central1-a/instances?"),
    );
    expect(server.host).toBe("192.0.2.5");
    expect(server.providerResourceID).toBe("123");
    expect(createCall?.path).toContain(
      "sourceMachineImage=projects%2Fdefault-project%2Fglobal%2FmachineImages%2Fcheckpoint-gcp",
    );
    expect(createCall?.body).not.toHaveProperty("disks");
    expect(String(createCall?.body?.name)).toMatch(/^crabbox-blue-lobster-/);
  });

  it("keeps exact GCP types eligible for zone fallback", async () => {
    const attempts: string[] = [];
    const original = GCPClient.prototype.createServer;
    GCPClient.prototype.createServer = async function (config): Promise<ProviderMachine> {
      attempts.push(`${config.gcpZone}/${config.serverType}`);
      if (config.gcpZone === "europe-west2-b") {
        return {
          provider: "gcp",
          id: 2,
          cloudID: "crabbox-b",
          name: "crabbox-b",
          status: "RUNNING",
          serverType: config.serverType,
          host: "192.0.2.10",
          region: config.gcpZone,
          labels: {},
        };
      }
      throw new Error("quota exceeded");
    };
    try {
      const client = new GCPClient(env, "us-central1-a");
      const config = leaseConfig({
        provider: "gcp",
        serverType: "c4-standard-32",
        serverTypeExplicit: true,
        gcpZone: "us-central1-a",
        capacity: { market: "spot", availabilityZones: ["europe-west2-b"] },
        sshPublicKey: "ssh-ed25519 test",
      });
      const result = await client.createServerWithFallback(
        config,
        "cbx_123456789abc",
        "blue-lobster",
        "peter@example.com",
      );
      expect(result.server.region).toBe("europe-west2-b");
      expect(attempts).toEqual(["us-central1-a/c4-standard-32", "europe-west2-b/c4-standard-32"]);
    } finally {
      GCPClient.prototype.createServer = original;
    }
  });

  it("uses network-specific firewall names", () => {
    expect(gcpFirewallNameForNetwork("default")).toBe("crabbox-ssh");
    expect(gcpFirewallNameForNetwork("projects/p/global/networks/default")).toBe("crabbox-ssh");
    expect(gcpFirewallNameForNetwork("crabbox-ci")).toBe("crabbox-ssh-crabbox-ci");
    expect(gcpFirewallNameForNetwork("projects/p/global/networks/123_custom")).toBe(
      "crabbox-ssh-net-123-custom",
    );
  });

  it("adds an ingress-policy suffix to non-default firewall names", () => {
    expect(
      gcpFirewallNameForPolicy("default", ["0.0.0.0/0"], ["crabbox-ssh"], ["2222", "22"]),
    ).toBe("crabbox-ssh");
    expect(
      gcpFirewallNameForPolicy("default", ["198.51.100.7/32"], ["crabbox-ssh"], ["2222", "22"]),
    ).not.toBe("crabbox-ssh");
    expect(
      gcpFirewallNameForPolicy("crabbox-ci", ["198.51.100.7/32"], ["crabbox-ssh"], ["2222", "22"]),
    ).toMatch(/^crabbox-ssh-crabbox-ci-[0-9a-f]{8}$/);
    expect(
      gcpFirewallNameForPolicy(
        "this-is-a-very-long-custom-network-name-that-would-fill-the-firewall-name",
        ["198.51.100.7/32"],
        ["crabbox-ssh"],
        ["2222", "22"],
      ).length,
    ).toBeLessThanOrEqual(63);
  });

  it("replaces default GCP tags when request tags are explicit", () => {
    expect(gcpEffectiveTags(["crabbox-ssh"], [])).toEqual(["crabbox-ssh"]);
    expect(gcpEffectiveTags(["crabbox-ssh"], ["crabbox-ci", "crabbox-ci"])).toEqual(["crabbox-ci"]);
    expect(gcpEffectiveTags(["  "], [])).toEqual(["crabbox-ssh"]);
    expect(gcpEffectiveTags(["crabbox-ssh"], ["  "])).toEqual(["crabbox-ssh"]);
  });

  it("treats unavailable machine types as fallback-eligible", () => {
    expect(
      isFallbackProvisioningError(
        "gcp POST /zones/us-central1-a/instances: http 400: Invalid value for field 'resource.machineType': 'zones/us-central1-a/machineTypes/c4-standard-192'. The referenced resource does not exist.",
      ),
    ).toBe(true);
    expect(
      isFallbackProvisioningError(
        "gcp POST /zones/us-central1-a/instances: http 404: The resource 'projects/p/zones/us-central1-a/machineTypes/c4-standard-192' was not found",
      ),
    ).toBe(true);
    expect(
      isFallbackProvisioningError(
        "gcp POST /zones/us-central1-a/instances: http 400: invalid labels",
      ),
    ).toBe(false);
  });
});
