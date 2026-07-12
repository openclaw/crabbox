import { randomBytes } from "node:crypto";

import { describe, expect, it } from "vitest";

import { leaseConfig } from "../src/config";
import { GCPProvider } from "../src/fleet";
import { GCPClient } from "../src/gcp";
import type { Env, LeaseRecord } from "../src/types";

const live = process.env.CRABBOX_GCP_RELEASE_LIVE === "1" ? describe : describe.skip;

live("GCP release ownership live", () => {
  it("creates, reads, denies a foreign claim, releases the owned instance, and leaves no disk", async () => {
    const project = requiredEnv("CRABBOX_GCP_PROJECT");
    const zone = process.env.CRABBOX_GCP_ZONE || "europe-west2-a";
    const credentialSource = process.env.CRABBOX_GCP_CREDENTIAL_SOURCE?.trim();
    const env: Env = {
      FLEET: {} as DurableObjectNamespace,
      CRABBOX_GCP_PROJECT: project,
      CRABBOX_GCP_ZONE: zone,
      CRABBOX_GCP_ROOT_GB: "10",
    };
    if (credentialSource) env.CRABBOX_GCP_CREDENTIAL_SOURCE = credentialSource;
    if (credentialSource !== "metadata") {
      env.GCP_CLIENT_EMAIL = requiredEnv("GCP_CLIENT_EMAIL");
      env.GCP_PRIVATE_KEY = requiredEnv("GCP_PRIVATE_KEY");
    }
    const client = new GCPClient(env, zone, project);
    const provider = new GCPProvider(env, undefined, zone, project);
    const leaseID =
      process.env.CRABBOX_GCP_RELEASE_LIVE_LEASE_ID || `cbx_${randomBytes(6).toString("hex")}`;
    const slug = "live-gcp-release";
    const owner = "live-test@example.com";
    const config = leaseConfig({
      provider: "gcp",
      target: "linux",
      class: "standard",
      serverType: "e2-micro",
      serverTypeExplicit: true,
      gcpProject: project,
      gcpZone: zone,
      gcpRootGB: 10,
      capacity: { market: "on-demand", fallback: "none" },
      sshPublicKey: requiredEnv("LIVE_SSH_PUBLIC_KEY"),
    });
    let cloudID = "";
    let denied = false;
    let released = false;
    try {
      const machine = await client.createServer(config, leaseID, slug, owner);
      cloudID = machine.cloudID;
      const readBack = await client.getServer(cloudID);
      expect(readBack.host).not.toBe("");
      expect(readBack.labels).toMatchObject({ lease: leaseID, provider: "gcp" });
      const now = new Date().toISOString();
      const lease = {
        id: leaseID,
        slug,
        provider: "gcp",
        target: "linux",
        cloudID,
        region: zone,
        providerProject: project,
        owner,
        org: "live-proof",
        profile: "default",
        class: "standard",
        serverType: "e2-micro",
        serverID: 0,
        serverName: cloudID,
        providerKey: "",
        host: readBack.host,
        sshUser: "crabbox",
        sshPort: "22",
        workRoot: "/workspace",
        keep: false,
        ttlSeconds: 900,
        estimatedHourlyUSD: 0,
        maxEstimatedUSD: 0,
        state: "active",
        createdAt: now,
        updatedAt: now,
        expiresAt: new Date(Date.now() + 900_000).toISOString(),
      } satisfies LeaseRecord;

      await expect(
        provider.releaseLease({ ...lease, owner: "foreign@example.com" }),
      ).rejects.toThrow("ownership does not match");
      denied = true;
      await expect(client.getServer(cloudID)).resolves.toMatchObject({ cloudID });

      await provider.releaseLease(lease);
      released = true;
      await expect(client.findServer(cloudID)).resolves.toBeUndefined();
      const disks = await (
        client as unknown as {
          gcp<T>(method: string, path: string): Promise<T>;
        }
      ).gcp<{ items?: { name?: string }[] }>(
        "GET",
        `/zones/${zone}/disks?filter=${encodeURIComponent(`name = ${cloudID}`)}`,
      );
      expect(disks.items ?? []).toEqual([]);
      console.log(JSON.stringify({ provider: "gcp", leaseID, denied, released, residue: 0 }));
    } finally {
      if (cloudID) {
        await client.deleteServer(cloudID);
      }
    }
  }, 600_000);
});

function requiredEnv(name: string): string {
  const value = process.env[name]?.trim();
  if (!value) throw new Error(`${name} is required`);
  return value;
}
