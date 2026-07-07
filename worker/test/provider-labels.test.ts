import { describe, expect, it } from "vitest";

import type { LeaseConfig } from "../src/config";
import { gcpLabelValue, gcpProviderLabelValue } from "../src/gcp";
import { leaseProviderLabels, providerMachineOwnedByLease } from "../src/provider-labels";
import type { LeaseRecord, ProviderMachine } from "../src/types";

describe("provider labels", () => {
  it("caps expires_at at the shorter of ttl and idle timeout", () => {
    const config: LeaseConfig = {
      provider: "aws",
      target: "linux",
      windowsMode: "normal",
      desktop: false,
      desktopEnv: "xfce",
      browser: false,
      tailscale: false,
      tailscaleTags: ["tag:crabbox"],
      tailscaleHostname: "",
      tailscaleAuthKey: "",
      tailscaleExitNode: "",
      tailscaleExitNodeAllowLanAccess: false,
      profile: "default",
      class: "beast",
      serverType: "c7a.48xlarge",
      image: "ami",
      location: "eu-west-1",
      sshUser: "crabbox",
      sshPort: "2222",
      sshFallbackPorts: ["22"],
      awsRegion: "eu-west-1",
      awsRootGB: 400,
      awsAMI: "",
      awsSGID: "",
      awsSubnetID: "",
      awsProfile: "",
      capacityMarket: "spot",
      capacityStrategy: "most-available",
      capacityFallback: "on-demand-after-120s",
      capacityRegions: [],
      capacityAvailabilityZones: [],
      providerKey: "crabbox-cbx-123",
      workRoot: "/work/crabbox",
      ttlSeconds: 600,
      idleTimeoutSeconds: 7200,
      keep: false,
      sshPublicKey: "ssh-ed25519 test",
    };
    const labels = leaseProviderLabels(
      config,
      "cbx_123",
      "blue-lobster",
      "peter@example.com",
      "aws",
      new Date("2026-05-01T12:00:00Z"),
    );
    expect(labels.expires_at).toBe("1777637400");
    expect(labels.created_at).toBe("1777636800");
    expect(labels.last_touched_at).toBe("1777636800");
    expect(labels.idle_timeout_secs).toBe("7200");
    expect(labels.ttl_secs).toBe("600");
    for (const value of Object.values(labels)) {
      expect(value).toMatch(/^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$/);
    }
  });

  it("labels requested desktop and browser capabilities", () => {
    const config: LeaseConfig = {
      provider: "aws",
      target: "linux",
      windowsMode: "normal",
      desktop: true,
      desktopEnv: "wayland",
      browser: true,
      code: true,
      tailscale: true,
      tailscaleTags: ["tag:crabbox"],
      tailscaleHostname: "crabbox-blue-lobster",
      tailscaleAuthKey: "tskey-secret",
      tailscaleExitNode: "mac-studio.tailnet.ts.net",
      tailscaleExitNodeAllowLanAccess: true,
      profile: "default",
      class: "beast",
      serverType: "c7a.48xlarge",
      image: "ami",
      location: "eu-west-1",
      sshUser: "crabbox",
      sshPort: "2222",
      sshFallbackPorts: ["22"],
      awsRegion: "eu-west-1",
      awsRootGB: 400,
      awsAMI: "",
      awsSGID: "",
      awsSubnetID: "",
      awsProfile: "",
      capacityMarket: "spot",
      capacityStrategy: "most-available",
      capacityFallback: "on-demand-after-120s",
      capacityRegions: [],
      capacityAvailabilityZones: [],
      providerKey: "crabbox-cbx-123",
      workRoot: "/work/crabbox",
      ttlSeconds: 600,
      idleTimeoutSeconds: 7200,
      keep: false,
      sshPublicKey: "ssh-ed25519 test",
    };
    const labels = leaseProviderLabels(
      config,
      "cbx_123",
      "blue-lobster",
      "peter@example.com",
      "aws",
      new Date("2026-05-01T12:00:00Z"),
    );
    expect(labels.desktop).toBe("true");
    expect(labels.desktop_env).toBe("wayland");
    expect(labels.browser).toBe("true");
    expect(labels.code).toBe("true");
    expect(labels.tailscale).toBe("true");
    expect(labels.tailscale_hostname).toBe("crabbox-blue-lobster");
    expect(labels.tailscale_tags).toBe("tag_crabbox");
    expect(labels.tailscale_exit_node).toBe("mac-studio.tailnet.ts.net");
    expect(labels.tailscale_exit_node_allow_lan_access).toBe("true");
    expect(Object.values(labels).join(" ")).not.toContain("tskey-secret");
  });

  it("includes the pond label when the lease is tagged", () => {
    const config: LeaseConfig = {
      provider: "aws",
      target: "linux",
      windowsMode: "normal",
      desktop: false,
      browser: false,
      tailscale: false,
      tailscaleTags: ["tag:crabbox"],
      tailscaleHostname: "",
      tailscaleAuthKey: "",
      tailscaleExitNode: "",
      tailscaleExitNodeAllowLanAccess: false,
      profile: "default",
      class: "beast",
      serverType: "c7a.48xlarge",
      image: "ami",
      location: "eu-west-1",
      sshUser: "crabbox",
      sshPort: "2222",
      sshFallbackPorts: ["22"],
      awsRegion: "eu-west-1",
      awsRootGB: 400,
      awsAMI: "",
      awsSGID: "",
      awsSubnetID: "",
      awsProfile: "",
      capacityMarket: "spot",
      capacityStrategy: "most-available",
      capacityFallback: "on-demand-after-120s",
      capacityRegions: [],
      capacityAvailabilityZones: [],
      providerKey: "crabbox-cbx-123",
      workRoot: "/work/crabbox",
      ttlSeconds: 600,
      idleTimeoutSeconds: 7200,
      keep: false,
      sshPublicKey: "ssh-ed25519 test",
      pond: "alpha",
    } as LeaseConfig;
    const labels = leaseProviderLabels(
      config,
      "cbx_123",
      "blue-lobster",
      "peter@example.com",
      "aws",
      new Date("2026-05-01T12:00:00Z"),
    );
    expect(labels.pond).toBe("alpha");
  });

  it("omits the pond label when the lease has no pond", () => {
    const config: LeaseConfig = {
      provider: "aws",
      target: "linux",
      windowsMode: "normal",
      desktop: false,
      browser: false,
      tailscale: false,
      tailscaleTags: ["tag:crabbox"],
      tailscaleHostname: "",
      tailscaleAuthKey: "",
      tailscaleExitNode: "",
      tailscaleExitNodeAllowLanAccess: false,
      profile: "default",
      class: "beast",
      serverType: "c7a.48xlarge",
      image: "ami",
      location: "eu-west-1",
      sshUser: "crabbox",
      sshPort: "2222",
      sshFallbackPorts: ["22"],
      awsRegion: "eu-west-1",
      awsRootGB: 400,
      awsAMI: "",
      awsSGID: "",
      awsSubnetID: "",
      awsProfile: "",
      capacityMarket: "spot",
      capacityStrategy: "most-available",
      capacityFallback: "on-demand-after-120s",
      capacityRegions: [],
      capacityAvailabilityZones: [],
      providerKey: "crabbox-cbx-123",
      workRoot: "/work/crabbox",
      ttlSeconds: 600,
      idleTimeoutSeconds: 7200,
      keep: false,
      sshPublicKey: "ssh-ed25519 test",
      pond: "",
    } as LeaseConfig;
    const labels = leaseProviderLabels(
      config,
      "cbx_123",
      "blue-lobster",
      "peter@example.com",
      "aws",
      new Date("2026-05-01T12:00:00Z"),
    );
    expect(labels.pond).toBeUndefined();
  });

  it("binds release ownership to the exact provider, resource, lease, owner, and slug", () => {
    const lease = {
      id: "cbx_abcdef123456",
      slug: "blue-lobster",
      provider: "aws",
      cloudID: "i-abcdef123456",
      owner: "alice@example.com",
    } satisfies Pick<LeaseRecord, "id" | "slug" | "provider" | "cloudID" | "owner">;
    const machine = {
      provider: "aws",
      cloudID: lease.cloudID,
      labels: {
        crabbox: "true",
        created_by: "crabbox",
        lease: lease.id,
        owner: "alice_example.com",
        provider: "aws",
        slug: lease.slug,
      },
    } satisfies Pick<ProviderMachine, "provider" | "cloudID" | "labels">;

    expect(providerMachineOwnedByLease(machine, lease, "aws")).toBe(true);
    for (const candidate of [
      { ...machine, provider: "gcp" as const },
      { ...machine, cloudID: "i-foreign" },
      { ...machine, labels: { ...machine.labels, crabbox: "false" } },
      { ...machine, labels: { ...machine.labels, created_by: "external" } },
      { ...machine, labels: { ...machine.labels, lease: "cbx_000000000000" } },
      { ...machine, labels: { ...machine.labels, owner: "bob_example.com" } },
      { ...machine, labels: { ...machine.labels, provider: "azure" } },
      { ...machine, labels: { ...machine.labels, slug: "red-lobster" } },
    ]) {
      expect(providerMachineOwnedByLease(candidate, lease, "aws")).toBe(false);
    }
    expect(providerMachineOwnedByLease(machine, { ...lease, id: "legacy-id" }, "aws")).toBe(false);
    expect(providerMachineOwnedByLease(machine, { ...lease, slug: undefined }, "aws")).toBe(false);
    expect(providerMachineOwnedByLease(machine, { ...lease, owner: "" }, "aws")).toBe(false);
  });

  it("compares GCP ownership using the labels stored by Compute Engine", () => {
    const lease = {
      id: "cbx_abcdef123456",
      slug: "Blue.Lobster",
      provider: "gcp",
      cloudID: "crabbox-blue-lobster",
      owner: "Alice@example.com",
    } satisfies Pick<LeaseRecord, "id" | "slug" | "provider" | "cloudID" | "owner">;
    const machine = {
      provider: "gcp",
      cloudID: lease.cloudID,
      labels: {
        crabbox: "true",
        created_by: "crabbox",
        lease: lease.id,
        owner: "alice_example_com",
        provider: "gcp",
        slug: "blue-lobster",
      },
    } satisfies Pick<ProviderMachine, "provider" | "cloudID" | "labels">;

    expect(providerMachineOwnedByLease(machine, lease, "gcp", gcpLabelValue)).toBe(true);
    expect(providerMachineOwnedByLease(machine, lease, "gcp")).toBe(false);
  });

  it("matches the two-stage GCP label transform used during creation", () => {
    const lease = {
      id: "cbx_abcdef123456",
      slug: "Blue.Lobster",
      provider: "gcp",
      cloudID: "crabbox-blue-lobster",
      owner: "İ@example.com",
    } satisfies Pick<LeaseRecord, "id" | "slug" | "provider" | "cloudID" | "owner">;
    const machine = {
      provider: "gcp",
      cloudID: lease.cloudID,
      labels: {
        crabbox: "true",
        created_by: "crabbox",
        lease: lease.id,
        owner: "example_com",
        provider: "gcp",
        slug: "blue-lobster",
      },
    } satisfies Pick<ProviderMachine, "provider" | "cloudID" | "labels">;

    expect(providerMachineOwnedByLease(machine, lease, "gcp", gcpProviderLabelValue)).toBe(true);
    expect(providerMachineOwnedByLease(machine, lease, "gcp", gcpLabelValue)).toBe(false);
  });

  it("uses the immutable provider owner after a workspace prewarm handoff", () => {
    const lease = {
      id: "cbx_abcdef123456",
      slug: "blue-lobster",
      provider: "aws",
      cloudID: "i-abcdef123456",
      workspaceID: "fleet-blue-lobster",
      owner: "alice@example.com",
      providerOwner: "crabbox-internal-prewarm",
    } satisfies Pick<
      LeaseRecord,
      "id" | "slug" | "provider" | "cloudID" | "workspaceID" | "owner" | "providerOwner"
    >;
    const machine = {
      provider: "aws",
      cloudID: lease.cloudID,
      labels: {
        crabbox: "true",
        created_by: "crabbox",
        lease: lease.id,
        owner: "crabbox-internal-prewarm",
        provider: "aws",
        slug: lease.slug,
      },
    } satisfies Pick<ProviderMachine, "provider" | "cloudID" | "labels">;

    expect(providerMachineOwnedByLease(machine, lease, "aws")).toBe(true);
    expect(
      providerMachineOwnedByLease(machine, { ...lease, providerOwner: undefined }, "aws"),
    ).toBe(true);
    expect(
      providerMachineOwnedByLease(
        machine,
        { ...lease, providerOwner: undefined, workspaceID: undefined },
        "aws",
      ),
    ).toBe(false);
  });
});
