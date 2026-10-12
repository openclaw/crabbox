import { afterEach, describe, expect, it, vi } from "vitest";

import { sha256Hex } from "../src/encoding";
import { FleetCoordinator } from "../src/fleet";
import { putLeaseRecord } from "../src/lease-record-store";
import { requestOrg } from "../src/org-identity";
import { ServeCoordinator, type ServeMachine } from "../src/serve";
import type { Env, LeaseRecord } from "../src/types";
import { ProvisioningTestRuntime, ProvisioningTestStorage } from "./provisioning-fixtures";

const owner = "alice@example.com";
const machineID = "11111111-1111-4111-8111-111111111111";
const leaseID = "cbx_000000000001";
const sshKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA";

function fixture(allowed = owner) {
  const env = { CRABBOX_SERVE_ALLOWED_OWNERS: allowed } as Env;
  const storage = new ProvisioningTestStorage();
  const runtime = new ProvisioningTestRuntime(storage);
  const fleet = new FleetCoordinator(runtime, env);
  const serve = new ServeCoordinator(runtime, env);
  const request = (who = owner, org = "example-org", admin = false, path = "/portal") =>
    new Request(`https://crabbox.example${path}`, {
      headers: {
        "x-crabbox-owner": who,
        "x-crabbox-org": org,
        ...(admin ? { "x-crabbox-admin": "true" } : {}),
      },
    });
  const machine = (overrides: Partial<ServeMachine> = {}): ServeMachine => ({
    machineID,
    owner,
    org: requestOrg(request(), env),
    name: "linux-builder",
    os: "linux",
    arch: "amd64",
    cpus: 16,
    memoryMB: 32768,
    maxLeases: 4,
    address: "100.64.1.2",
    frontDoor: { port: 2222, hostKey: sshKey },
    runtime: "docker",
    runtimeVersion: "28.0",
    agentVersion: "0.75.0",
    image: "example-image",
    labels: {},
    load: 0,
    freeMemoryMB: 32768,
    draining: false,
    lastHeartbeat: Date.now() - 5000,
    leases: [],
    ...overrides,
  });
  const ownerPrefix = async (m = machine()) =>
    `serve-owner:${await sha256Hex(JSON.stringify([m.owner, m.org]))}:`;
  const seed = async (m: ServeMachine) => {
    await storage.put(`serve-machine:${m.machineID}`, m);
    await storage.put((await ownerPrefix(m)) + m.machineID, m);
  };
  const home = async (r = request()) => {
    const response = await fleet.fetch(r);
    expect(response.status).toBe(200);
    return response.text();
  };
  return { storage, serve, request, machine, ownerPrefix, seed, home };
}

afterEach(() => vi.useRealTimers());

describe("portal donated machines", () => {
  it("renders online, offline and draining capacity with allocated lease links", async () => {
    vi.useFakeTimers();
    const f = fixture();
    await f.seed(f.machine());
    const created = await f.serve.create(f.request(), {
      provider: "serve",
      leaseID,
      sshPublicKey: sshKey,
      class: "standard",
    });
    expect(created.status).toBe(202);
    const lease = (await f.storage.get<LeaseRecord>(`lease:${leaseID}`))!;
    await putLeaseRecord(f.storage, { ...lease, slug: "build-tests", state: "active" });
    await f.seed(
      f.machine({
        machineID: "22222222-2222-4222-8222-222222222222",
        name: "offline-builder",
        lastHeartbeat: Date.now() - 30_000,
      }),
    );
    await f.seed(
      f.machine({
        machineID: "33333333-3333-4333-8333-333333333333",
        name: "draining-builder",
        draining: true,
        os: "darwin",
        arch: "arm64",
      }),
    );

    const body = await f.home();
    const section = body.slice(
      body.indexOf('<section class="panel table-panel fleet-panel" id="machines"'),
      body.indexOf("</main>"),
    );
    expect(body).toContain('href="#machines"');
    expect(body).toContain('class="fleet-tile-value">2/3</strong>');
    expect(section).toContain('id="machines-heading">Machines</h2>');
    expect(section).toContain("11111111 · linux/amd64");
    expect(section).toContain("darwin/arm64");
    expect(section).toContain(">online</span>");
    expect(section).toContain(">offline</span>");
    expect(section).toContain(">draining</span>");
    expect(section).toContain("seen 5s ago");
    expect(section).toContain("seen 30s ago");
    expect(section).toContain("8/16 CPUs · 16384/32768 MiB");
    expect(section).toContain("8 CPUs · 16384 MiB free");
    expect(section).toContain("1/4 allocated");
    expect(section).toContain(`/portal/leases/${leaseID}"><strong>build-tests</strong>`);
    expect(section).toContain("No allocated leases.");
    expect(section).toContain("docker 28.0");
    expect(section).toContain("agent 0.75.0");
    expect(section).toContain("100.64.1.2:2222");

    const api = await f.serve.route(f.request(owner, "example-org", false, "/v1/serve/machines"), [
      "v1",
      "serve",
      "machines",
    ]);
    expect(await api.json()).toMatchObject({
      machines: expect.arrayContaining([
        expect.objectContaining({
          machineID,
          online: true,
          freeCPUs: 8,
          allocatedLeaseRefs: [{ id: leaseID, slug: "build-tests", state: "active" }],
        }),
      ]),
    });
  });

  it.each(["", "bob@example.com"])(
    "omits disabled serve without reading machines (%s)",
    async (allowed) => {
      const f = fixture(allowed);
      await f.seed(f.machine());
      f.storage.listOptions.length = 0;
      const reads: string[] = [];
      f.storage.beforeGet = async (key) => {
        reads.push(key);
      };
      const body = await f.home();
      expect(body).not.toContain('id="machines"');
      expect(body).not.toContain("machines online");
      expect(body).not.toContain("serve_not_enabled");
      expect(
        f.storage.listOptions.filter((options) => options?.prefix?.startsWith("serve-")),
      ).toEqual([]);
      expect(reads.filter((key) => key.startsWith("serve-"))).toEqual([]);
    },
  );

  it("omits machines and their readout for an enabled owner with no machines", async () => {
    const f = fixture("*");
    await f.seed(f.machine({ owner: "bob@example.com", name: "private-builder" }));
    const body = await f.home();
    expect(body).not.toContain('id="machines"');
    expect(body).not.toContain("machines online");
    expect(body).not.toContain("private-builder");
  });

  it.each([false, true])("keeps owner and organization isolation (admin=%s)", async (admin) => {
    const f = fixture("*");
    await f.seed(f.machine());
    const foreign = f.machine({
      machineID: "22222222-2222-4222-8222-222222222222",
      owner: "bob@example.com",
      name: "private-owner-builder",
    });
    await f.seed(foreign);
    await f.seed(
      f.machine({
        machineID: "33333333-3333-4333-8333-333333333333",
        org: requestOrg(f.request(owner, "other-org"), {} as Env),
        name: "private-org-builder",
      }),
    );
    // Even a misplaced index row must pass the same ownership check as the API.
    await f.storage.put((await f.ownerPrefix()) + foreign.machineID, foreign);
    const body = await f.home(f.request(owner, "example-org", admin));
    expect(body).toContain("linux-builder");
    expect(body).not.toContain("private-owner-builder");
    expect(body).not.toContain("private-org-builder");
    expect(body).toContain('class="fleet-tile-value">1/1</strong>');
  });

  it("escapes machine metadata and lease slugs, and hides foreign lease references", async () => {
    const f = fixture();
    const attack = '<img src=x onerror="alert(1)">&';
    await f.seed(f.machine());
    expect(
      (
        await f.serve.create(f.request(), {
          provider: "serve",
          leaseID,
          sshPublicKey: sshKey,
          class: "standard",
        })
      ).status,
    ).toBe(202);
    const original = (await f.storage.get<LeaseRecord>(`lease:${leaseID}`))!;
    await f.seed(
      f.machine({
        name: attack,
        labels: { [attack]: attack },
        runtimeVersion: attack,
        agentVersion: attack,
        leases: [leaseID, "cbx_000000000002"],
      }),
    );
    const lease: LeaseRecord = { ...original, slug: attack, state: "active" };
    await putLeaseRecord(f.storage, lease);
    await putLeaseRecord(f.storage, {
      ...lease,
      id: "cbx_000000000002",
      owner: "bob@example.com",
      slug: "private-lease-slug",
    });
    const body = await f.home();
    expect(body).toContain("&lt;img src=x onerror=&quot;alert(1)&quot;&gt;&amp;");
    expect(body).not.toContain(attack);
    expect(body).not.toContain("private-lease-slug");
    expect(body).not.toContain('href="/portal/leases/cbx_000000000002"');
  });

  it("caps the owner index at 64 machines and reads only their allocated lease IDs", async () => {
    const f = fixture();
    const machines = Array.from({ length: 65 }, (_, index) =>
      f.machine({
        machineID: `00000000-0000-4000-8000-${index.toString().padStart(12, "0")}`,
        leases: [`cbx_${index.toString().padStart(12, "0")}`],
      }),
    );
    await Promise.all(machines.map(f.seed));
    f.storage.listOptions.length = 0;
    f.storage.writes.length = 0;
    const reads: string[] = [];
    f.storage.beforeGet = async (key) => {
      reads.push(key);
    };
    const body = await f.home();
    expect(body).toContain('class="fleet-tile-value">64/64</strong>');
    expect(
      f.storage.listOptions.filter((options) => options?.prefix?.startsWith("serve-")),
    ).toEqual([{ prefix: await f.ownerPrefix(), limit: 64 }]);
    expect(reads.filter((key) => key.startsWith("lease:"))).toEqual(
      machines.slice(0, 64).flatMap((machine) => machine.leases.map((id) => `lease:${id}`)),
    );
    expect(
      f.storage.writes.filter((key) => key.startsWith("serve-") || key.startsWith("lease:")),
    ).toEqual([]);
  });
});
