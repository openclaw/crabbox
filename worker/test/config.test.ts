import { readFileSync } from "node:fs";

import { describe, expect, it } from "vitest";

import {
  awsInstanceTypeCandidatesForClass,
  awsInstanceTypeCandidatesForTargetClass,
  azureWindowsVMSizeCandidatesForClass,
  azureVMSizeCandidatesForClass,
  azureVMSizeCandidatesForTargetClass,
  gcpMachineTypeCandidatesForClass,
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

  it("maps known classes to preferred Azure candidates", () => {
    expect(serverTypeForProviderClass("azure", "standard")).toBe("Standard_D32ads_v6");
    expect(azureVMSizeCandidatesForClass("standard")).toEqual([
      "Standard_D32ads_v6",
      "Standard_D32ds_v6",
      "Standard_F32s_v2",
      "Standard_D32ads_v5",
      "Standard_D32ds_v5",
      "Standard_D16ads_v6",
      "Standard_D16ds_v6",
      "Standard_F16s_v2",
    ]);
    expect(azureVMSizeCandidatesForTargetClass("linux", "standard")).toEqual(
      azureVMSizeCandidatesForClass("standard"),
    );
    expect(azureVMSizeCandidatesForTargetClass("windows", "standard")).toEqual(
      azureWindowsVMSizeCandidatesForClass("standard"),
    );
    expect(azureVMSizeCandidatesForTargetClass("windows", "standard", "wsl2")).toEqual([
      "standard",
    ]);
  });

  it("maps known classes to preferred GCP candidates", () => {
    expect(serverTypeForProviderClass("gcp", "standard")).toBe("c4-standard-32");
    expect(gcpMachineTypeCandidatesForClass("standard")).toEqual([
      "c4-standard-32",
      "c3-standard-22",
      "n2-standard-32",
      "n2d-standard-32",
    ]);
  });

  it("maps AWS Windows and macOS classes to compatible families", () => {
    expect(awsInstanceTypeCandidatesForTargetClass("windows", "standard")).toEqual([
      "m7i.large",
      "m7a.large",
      "t3.large",
    ]);
    expect(awsInstanceTypeCandidatesForTargetClass("macos", "standard")).toEqual(["mac2.metal"]);
  });

  it("matches the Go CLI machine class tables", () => {
    const go = readFileSync(new URL("../../internal/cli/config.go", import.meta.url), "utf8");
    const goAzure = readFileSync(new URL("../../internal/cli/azure.go", import.meta.url), "utf8");
    const goGCP = readFileSync(new URL("../../internal/cli/gcp.go", import.meta.url), "utf8");
    const classes = ["standard", "fast", "large", "beast"];
    const hetzner = parseGoStringArrayCases(goFunctionBody(go, "serverTypeCandidatesForClass"));
    const awsLinux = parseGoStringArrayCases(
      goFunctionBody(go, "awsInstanceTypeCandidatesForClass"),
    );
    const azureLinux = parseGoStringArrayCases(
      goFunctionBody(goAzure, "azureVMSizeCandidatesForClass"),
    );
    const azureWindows = parseGoStringArrayCases(
      goFunctionBody(goAzure, "azureWindowsVMSizeCandidatesForClass"),
    );
    const gcp = parseGoStringArrayCases(goFunctionBody(goGCP, "gcpMachineTypeCandidatesForClass"));
    const awsTarget = goFunctionBody(go, "awsInstanceTypeCandidatesForTargetModeClass");
    const awsWSL2 = parseGoStringArrayCases(
      goSwitchAfter(awsTarget, "if windowsMode == windowsModeWSL2"),
    );
    const awsWindows = parseGoStringArrayCases(goSwitchAfter(awsTarget, "switch class", 1));

    for (const name of classes) {
      expect(serverTypeCandidatesForClass(name)).toEqual(hetzner[name]);
      expect(awsInstanceTypeCandidatesForClass(name)).toEqual(awsLinux[name]);
      expect(azureVMSizeCandidatesForClass(name)).toEqual(azureLinux[name]);
      expect(azureWindowsVMSizeCandidatesForClass(name)).toEqual(azureWindows[name]);
      expect(azureVMSizeCandidatesForTargetClass("windows", name)).toEqual(azureWindows[name]);
      expect(awsInstanceTypeCandidatesForTargetClass("windows", name)).toEqual(awsWindows[name]);
      expect(awsInstanceTypeCandidatesForTargetClass("windows", name, "wsl2")).toEqual(
        awsWSL2[name],
      );
      expect(gcpMachineTypeCandidatesForClass(name)).toEqual(gcp[name]);
    }
    expect(awsInstanceTypeCandidatesForTargetClass("macos", "standard")).toEqual(["mac2.metal"]);
  });
});

function goFunctionBody(source: string, name: string): string {
  const start = source.indexOf(`func ${name}(`);
  expect(start).toBeGreaterThanOrEqual(0);
  const open = source.indexOf("{", start);
  expect(open).toBeGreaterThanOrEqual(0);
  let depth = 0;
  for (let index = open; index < source.length; index += 1) {
    const char = source[index];
    if (char === "{") {
      depth += 1;
    } else if (char === "}") {
      depth -= 1;
      if (depth === 0) {
        return source.slice(open + 1, index);
      }
    }
  }
  throw new Error(`unterminated Go function ${name}`);
}

function goSwitchAfter(source: string, marker: string, occurrence = 0): string {
  let cursor = -1;
  for (let index = 0; index <= occurrence; index += 1) {
    cursor = source.indexOf(marker, cursor + 1);
    expect(cursor).toBeGreaterThanOrEqual(0);
  }
  const open = source.indexOf("{", cursor);
  expect(open).toBeGreaterThanOrEqual(0);
  let depth = 0;
  for (let index = open; index < source.length; index += 1) {
    const char = source[index];
    if (char === "{") {
      depth += 1;
    } else if (char === "}") {
      depth -= 1;
      if (depth === 0) {
        return source.slice(open + 1, index);
      }
    }
  }
  throw new Error(`unterminated Go switch after ${marker}`);
}

function parseGoStringArrayCases(source: string): Record<string, string[]> {
  const out: Record<string, string[]> = {};
  const pattern = /case "([^"]+)":\s*return \[\]string\{([^}]*)\}/g;
  for (const match of source.matchAll(pattern)) {
    const key = match[1];
    const body = match[2];
    if (key === undefined || body === undefined) {
      continue;
    }
    out[key] = [...body.matchAll(/"([^"]+)"/g)].map((item) => item[1]).filter(isString);
  }
  return out;
}

function isString(value: string | undefined): value is string {
  return value !== undefined;
}

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
    expect(config.capacityHints).toBe(true);
    expect(config.desktop).toBe(false);
    expect(config.browser).toBe(false);
    expect(config.code).toBe(false);
    expect(config.ttlSeconds).toBe(86_400);
  });

  it("allows capacity hints to be disabled per lease", () => {
    const config = leaseConfig({
      sshPublicKey: "ssh-ed25519 test",
      capacity: { hints: false },
    });
    expect(config.capacityHints).toBe(false);
  });

  it("preserves requested desktop, browser, and code capabilities", () => {
    const config = leaseConfig({
      sshPublicKey: "ssh-ed25519 test",
      desktop: true,
      browser: true,
      code: true,
    });
    expect(config.desktop).toBe(true);
    expect(config.browser).toBe(true);
    expect(config.code).toBe(true);
  });

  it("preserves Tailscale lease capability requests", () => {
    const config = leaseConfig({
      sshPublicKey: "ssh-ed25519 test",
      tailscale: true,
      tailscaleTags: ["tag:Crabbox", "tag:ci", "invalid"],
      tailscaleHostname: "crabbox-blue-lobster",
      tailscaleExitNode: "mac-studio.tailnet.ts.net",
      tailscaleExitNodeAllowLanAccess: true,
    });
    expect(config.tailscale).toBe(true);
    expect(config.tailscaleTags).toEqual(["tag:crabbox", "tag:ci"]);
    expect(config.tailscaleHostname).toBe("crabbox-blue-lobster");
    expect(config.tailscaleAuthKey).toBe("");
    expect(config.tailscaleExitNode).toBe("mac-studio.tailnet.ts.net");
    expect(config.tailscaleExitNodeAllowLanAccess).toBe(true);
  });

  it("uses AWS defaults when requested", () => {
    const config = leaseConfig({ provider: "aws", sshPublicKey: "ssh-ed25519 test" });
    expect(config.serverType).toBe("c7a.48xlarge");
    expect(config.serverTypeExplicit).toBe(false);
    expect(config.awsRegion).toBe("eu-west-1");
  });

  it("uses Azure defaults when requested", () => {
    const config = leaseConfig({
      provider: "azure",
      azureLocation: "eastus",
      azureImage: "Canonical:offer:sku:latest",
      sshPublicKey: "ssh-ed25519 test",
    });
    expect(config.serverType).toBe("Standard_D192ds_v6");
    expect(config.azureLocation).toBe("eastus");
    expect(config.azureImage).toBe("Canonical:offer:sku:latest");
  });

  it("leaves omitted GCP fields for Worker defaults", () => {
    const config = leaseConfig({ provider: "gcp", sshPublicKey: "ssh-ed25519 test" });
    expect(config.serverType).toBe("c4-standard-192");
    expect(config.gcpProject).toBe("");
    expect(config.gcpZone).toBe("");
    expect(config.gcpImage).toBe("");
    expect(config.gcpNetwork).toBe("");
    expect(config.gcpTags).toEqual([]);
    expect(config.gcpRootGB).toBe(0);
  });

  it("keeps explicit GCP request fields", () => {
    const config = leaseConfig({
      provider: "gcp",
      gcpProject: "request-project",
      gcpZone: "us-central1-a",
      gcpImage: "projects/example/global/images/custom",
      gcpNetwork: "custom-network",
      gcpTags: ["custom-ssh", "custom-ssh", ""],
      gcpRootGB: 128,
      sshPublicKey: "ssh-ed25519 test",
    });
    expect(config.gcpProject).toBe("request-project");
    expect(config.gcpZone).toBe("us-central1-a");
    expect(config.gcpImage).toBe("projects/example/global/images/custom");
    expect(config.gcpNetwork).toBe("custom-network");
    expect(config.gcpTags).toEqual(["custom-ssh"]);
    expect(config.gcpRootGB).toBe(128);
  });

  it("allows Azure native Windows leases", () => {
    const config = leaseConfig({
      provider: "azure",
      target: "windows",
      sshPublicKey: "ssh-rsa test",
    });
    expect(config.serverType).toBe("Standard_D16ads_v6");
    expect(config.workRoot).toBe("C:\\crabbox");
    expect(config.windowsMode).toBe("normal");
    expect(config.sshUser).toBe("crabbox");
    expect(() =>
      leaseConfig({
        provider: "azure",
        target: "windows",
        windowsMode: "wsl2",
        sshPublicKey: "ssh-rsa test",
      }),
    ).toThrow("native Windows only");
    for (const capability of ["desktop", "browser", "code", "tailscale"] as const) {
      expect(() =>
        leaseConfig({
          provider: "azure",
          target: "windows",
          [capability]: true,
          sshPublicKey: "ssh-rsa test",
        }),
      ).toThrow("SSH, sync, and run");
    }
  });

  it("records linux target defaults and rejects unsupported brokered non-linux targets", () => {
    const config = leaseConfig({ sshPublicKey: "ssh-ed25519 test" });
    expect(config.target).toBe("linux");
    expect(config.windowsMode).toBe("normal");
    expect(() =>
      leaseConfig({ provider: "hetzner", target: "windows", sshPublicKey: "ssh-ed25519 test" }),
    ).toThrow("managed provisioning supports target=linux only");
    expect(() =>
      leaseConfig({ provider: "hetzner", target: "macos", sshPublicKey: "ssh-ed25519 test" }),
    ).toThrow("EC2 Mac Dedicated Host");
  });

  it("allows AWS Windows leases", () => {
    const config = leaseConfig({
      provider: "aws",
      target: "windows",
      desktop: true,
      sshPublicKey: "ssh-ed25519 test",
    });
    expect(config.serverType).toBe("m7i.4xlarge");
    expect(config.workRoot).toBe("C:\\crabbox");
    expect(config.desktop).toBe(true);
    const wsl2 = leaseConfig({
      provider: "aws",
      target: "windows",
      windowsMode: "wsl2",
      sshPublicKey: "ssh-ed25519 test",
    });
    expect(wsl2.serverType).toBe("m8i.4xlarge");
    expect(wsl2.workRoot).toBe("/work/crabbox");
    expect(wsl2.windowsMode).toBe("wsl2");
    expect(wsl2.sshUser).toBe("Administrator");
  });

  it("allows AWS macOS leases only with on-demand capacity", () => {
    expect(() =>
      leaseConfig({
        provider: "aws",
        target: "macos",
        sshPublicKey: "ssh-ed25519 test",
      }),
    ).toThrow("capacity.market=on-demand");
    const config = leaseConfig({
      provider: "aws",
      target: "macos",
      capacity: { market: "on-demand" },
      sshPublicKey: "ssh-ed25519 test",
    });
    expect(config.serverType).toBe("mac2.metal");
    expect(config.sshUser).toBe("ec2-user");
    expect(config.workRoot).toBe("/Users/ec2-user/crabbox");
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
