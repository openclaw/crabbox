import { execFileSync } from "node:child_process";

import { describe, expect, it, vi } from "vitest";

import {
  AWSPoolAccess,
  poolEnrollmentScript,
  poolInstallScript,
  poolRevokeScript,
} from "../src/aws-pool-access";
import type { PoolAccessGrant } from "../src/ready-pool-access";
import type { LeaseRecord, ProviderMachine } from "../src/types";

const binding = {
  leaseID: "cbx_000000000001",
  provider: "aws",
  resourceID: "i-0123456789abcdef0",
  scope: "us-east-1",
  user: "ubuntu",
};
const grant: PoolAccessGrant = {
  id: "grant",
  key: "builders",
  leaseID: binding.leaseID,
  owner: "alice@example.com",
  org: "example-org",
  binding,
  identity: {
    schema: "crabbox-ready-pool-identity/v1",
    image: { provider: "aws", scope: binding.scope, id: "ami-0123456789abcdef0" },
    architecture: "amd64",
    seedDigest: "sha256:test",
    cacheCompatibility: "node-24",
  },
  generation: 2,
  borrowHash: "borrow-hash",
  receiptHash: "receipt-hash",
  publicKey: "ssh-ed25519 AAAA",
  fingerprint: "fingerprint",
  expiresAt: "2030-01-01T00:30:00.000Z",
  acknowledgementDeadline: "2030-01-01T00:01:00.000Z",
  state: "revoking",
  result: "ready",
  updatedAt: "2030-01-01T00:00:00.000Z",
};
function fixture() {
  const machine = {
    cloudID: binding.resourceID,
    status: "running",
    labels: { lease: binding.leaseID, crabbox: "true", created_by: "crabbox" },
  } as unknown as ProviderMachine;
  const client = {
    findServer: vi.fn<() => Promise<ProviderMachine | undefined>>(async () => machine),
    runPoolAccessCommand: vi.fn<() => Promise<string>>(async () => "FENCED"),
    terminateServerAndWait: vi.fn<() => Promise<void>>(async () => {}),
  };
  return { machine, client, adapter: new AWSPoolAccess(client) };
}

describe("AWS portable pool access", () => {
  it("checks exact resource and lease ownership before every mutation", async () => {
    const { machine, client, adapter } = fixture();
    machine.labels["lease"] = "another-lease";
    await expect(adapter.install(grant)).rejects.toThrow("binding mismatch");
    await expect(adapter.revoke(grant)).rejects.toThrow("binding mismatch");
    expect(client.runPoolAccessCommand).not.toHaveBeenCalled();
    expect(client.terminateServerAndWait).not.toHaveBeenCalled();
  });
  it("does not confuse accepted reboot or uncertain SSM completion with session fencing", async () => {
    const { client, adapter } = fixture();
    client.runPoolAccessCommand.mockResolvedValueOnce("REBOOTING");
    await expect(adapter.revoke(grant)).rejects.toThrow("awaiting observed boot change");
    client.runPoolAccessCommand.mockRejectedValueOnce(new Error("lost SSM response"));
    await expect(adapter.revoke(grant)).rejects.toThrow("lost SSM response");
    await expect(adapter.revoke(grant)).resolves.toEqual({ destroyed: false });
  });
  it("requires observed destruction for drain and never reinstalls on revoke", async () => {
    const { client, adapter } = fixture();
    await expect(adapter.revoke({ ...grant, result: "drain" })).rejects.toThrow(
      "destruction not confirmed",
    );
    client.findServer.mockResolvedValueOnce(undefined);
    await expect(adapter.revoke({ ...grant, result: "drain" })).resolves.toEqual({
      destroyed: true,
    });
    expect(client.runPoolAccessCommand).not.toHaveBeenCalled();
  });
  it("rejects enrollment without Linux SSH, stable instance identity or guest proof", async () => {
    const { client, adapter } = fixture();
    const lease = {
      id: binding.leaseID,
      cloudID: binding.resourceID,
      region: binding.scope,
      sshUser: binding.user,
      target: "linux",
    } as LeaseRecord;
    await expect(adapter.enroll({ ...lease, target: "macos" })).rejects.toThrow("public Linux");
    await expect(adapter.enroll(lease)).rejects.toThrow("enrollment was not confirmed");
    client.runPoolAccessCommand.mockResolvedValueOnce("ENROLLED");
    await expect(adapter.enroll(lease)).resolves.toEqual(binding);
  });
  it("renders valid shell with persistent generation-specific expiry and boot evidence", () => {
    const enrollment = poolEnrollmentScript(binding);
    const install = poolInstallScript(grant);
    const revoke = poolRevokeScript(grant);
    for (const script of [enrollment, install, revoke])
      execFileSync("bash", ["-n"], { input: script });
    expect(install).toContain("Persistent=true");
    expect(install).toContain("expire-2");
    expect(install).toContain('test "$generation" -le 2');
    expect(install).toContain('test "$(cat state)" = active');
    expect(install.indexOf("systemctl enable --now")).toBeLessThan(install.indexOf("expiry-time="));
    expect(install).toContain('expiry-time="20300101003000Z"');
    expect(revoke).toContain(
      'test "$(cat fence-boot)" != "$(cat /proc/sys/kernel/random/boot_id)"',
    );
    expect(revoke.indexOf("printf closed > state")).toBeLessThan(
      revoke.indexOf("systemctl --no-block reboot"),
    );
  });
});
