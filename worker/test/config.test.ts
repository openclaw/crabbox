import { describe, expect, it } from "vitest";

import {
  awsInstanceTypeCandidatesForClass,
  leaseConfig,
  serverTypeCandidatesForClass,
  serverTypeForClass,
  serverTypeForProviderClass,
  sshPorts,
} from "../src/config";

describe("machine class config", () => {
  it("maps known classes to preferred Hetzner candidates", () => {
    expect(serverTypeForClass("beast")).toBe("ccx63");
    expect(serverTypeCandidatesForClass("beast")).toEqual([
      "ccx63",
      "ccx53",
      "ccx43",
      "cpx62",
      "cx53",
    ]);
  });

  it("treats an unknown class as an explicit server type", () => {
    expect(serverTypeCandidatesForClass("cpx62")).toEqual(["cpx62"]);
  });

  it("maps known classes to preferred AWS candidates", () => {
    expect(serverTypeForProviderClass("aws", "beast")).toBe("c7a.48xlarge");
    expect(awsInstanceTypeCandidatesForClass("beast")).toEqual([
      "c7a.48xlarge",
      "c7i.48xlarge",
      "m7a.48xlarge",
      "m7i.48xlarge",
      "r7a.48xlarge",
      "c7a.32xlarge",
      "c7i.32xlarge",
      "m7a.32xlarge",
      "c7a.24xlarge",
      "c7a.16xlarge",
    ]);
  });
});

describe("lease config", () => {
  it("requires an ssh public key", () => {
    expect(() => leaseConfig({})).toThrow("sshPublicKey is required");
  });

  it("uses strict defaults and clamps ttl", () => {
    const config = leaseConfig({ sshPublicKey: "ssh-ed25519 test", ttlSeconds: 999_999 });
    expect(config.provider).toBe("hetzner");
    expect(config.profile).toBe("default");
    expect(config.sshPort).toBe("2222");
    expect(config.sshFallbackPorts).toEqual(["22"]);
    expect(config.capacityMarket).toBe("spot");
    expect(config.capacityStrategy).toBe("most-available");
    expect(config.desktop).toBe(false);
    expect(config.browser).toBe(false);
    expect(config.ttlSeconds).toBe(86_400);
  });

  it("preserves requested desktop and browser capabilities", () => {
    const config = leaseConfig({
      sshPublicKey: "ssh-ed25519 test",
      desktop: true,
      browser: true,
    });
    expect(config.desktop).toBe(true);
    expect(config.browser).toBe(true);
  });

  it("uses AWS defaults when requested", () => {
    const config = leaseConfig({ provider: "aws", sshPublicKey: "ssh-ed25519 test" });
    expect(config.serverType).toBe("c7a.48xlarge");
    expect(config.serverTypeExplicit).toBe(false);
    expect(config.awsRegion).toBe("eu-west-1");
  });

  it("records linux target defaults and rejects brokered non-linux targets", () => {
    const config = leaseConfig({ sshPublicKey: "ssh-ed25519 test" });
    expect(config.target).toBe("linux");
    expect(config.windowsMode).toBe("normal");
    expect(() => leaseConfig({ target: "macos", sshPublicKey: "ssh-ed25519 test" })).toThrow(
      "unsupported target",
    );
  });

  it("preserves exact server type requests", () => {
    const config = leaseConfig({
      provider: "aws",
      serverType: "t3.small",
      serverTypeExplicit: true,
      sshPublicKey: "ssh-ed25519 test",
    });
    expect(config.serverType).toBe("t3.small");
    expect(config.serverTypeExplicit).toBe(true);
  });

  it("uses configured SSH fallback ports as ordered candidates", () => {
    const config = leaseConfig({
      sshPublicKey: "ssh-ed25519 test",
      sshPort: "2222",
      sshFallbackPorts: ["2022", "22", "2222", "bad"],
    });
    expect(config.sshFallbackPorts).toEqual(["2022", "22", "2222"]);
    expect(sshPorts(config)).toEqual(["2222", "2022", "22"]);
  });

  it("allows disabling SSH fallback ports", () => {
    const config = leaseConfig({ sshPublicKey: "ssh-ed25519 test", sshFallbackPorts: [] });
    expect(config.sshFallbackPorts).toEqual([]);
    expect(sshPorts(config)).toEqual(["2222"]);
  });
});
