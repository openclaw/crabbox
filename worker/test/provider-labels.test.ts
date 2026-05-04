import { describe, expect, it } from "vitest";

import type { LeaseConfig } from "../src/config";
import { leaseProviderLabels } from "../src/provider-labels";

describe("provider labels", () => {
  it("caps expires_at at the shorter of ttl and idle timeout", () => {
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
      browser: true,
      tailscale: true,
      tailscaleTags: ["tag:crabbox"],
      tailscaleHostname: "crabbox-blue-lobster",
      tailscaleAuthKey: "tskey-secret",
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
    expect(labels.browser).toBe("true");
    expect(labels.tailscale).toBe("true");
    expect(labels.tailscale_hostname).toBe("crabbox-blue-lobster");
    expect(labels.tailscale_tags).toBe("tag_crabbox");
    expect(Object.values(labels).join(" ")).not.toContain("tskey-secret");
  });
});
