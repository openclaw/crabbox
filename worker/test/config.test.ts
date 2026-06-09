import { readFileSync } from "node:fs";

import { describe, expect, it } from "vitest";

import {
  awsMacOSInstanceTypeCandidates,
  awsARM64InstanceTypeCandidatesForClass,
  awsInstanceTypeCandidatesForClass,
  awsInstanceTypeCandidatesForTargetClass,
  azureARM64VMSizeCandidatesForClass,
  azureWindowsVMSizeCandidatesForClass,
  azureVMSizeCandidatesForClass,
  azureVMSizeCandidatesForTargetClass,
  gcpMachineTypeCandidatesForClass,
  leaseConfig,
  serverTypeCandidatesForClass,
  serverTypeForClass,
  serverTypeForProviderClass,
  sshPorts,
  validCIDRs,
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
    expect(azureVMSizeCandidatesForTargetClass("linux", "standard", "normal", "arm64")).toEqual(
      azureARM64VMSizeCandidatesForClass("standard"),
    );
    expect(azureVMSizeCandidatesForTargetClass("windows", "standard")).toEqual(
      azureWindowsVMSizeCandidatesForClass("standard"),
    );
    expect(azureVMSizeCandidatesForTargetClass("windows", "standard", "wsl2")).toEqual(
      azureWindowsVMSizeCandidatesForClass("standard"),
    );
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
    expect(awsInstanceTypeCandidatesForTargetClass("linux", "standard", "normal", "arm64")).toEqual(
      awsARM64InstanceTypeCandidatesForClass("standard"),
    );
    expect(awsInstanceTypeCandidatesForTargetClass("macos", "standard")).toEqual([
      ...awsMacOSInstanceTypeCandidates,
    ]);
  });

  it("matches the Go CLI machine class tables", () => {
    const go = readFileSync(new URL("../../internal/cli/config.go", import.meta.url), "utf8");
    const goAzure = readFileSync(new URL("../../internal/cli/azure.go", import.meta.url), "utf8");
    const goGCP = readFileSync(new URL("../../internal/cli/gcp.go", import.meta.url), "utf8");
    const classes = ["standard", "fast", "large", "beast"];
    const hetzner = parseGoStringArrayCases(goFunctionBody(go, "serverTypeCandidatesForClass"));
    const awsLinux = parseGoStringArrayCases(
      goFunctionBody(go, "awsInstanceTypeCandidatesForArchitectureClass"),
    );
    const azureLinux = parseGoStringArrayCases(
      goFunctionBody(goAzure, "azureVMSizeCandidatesForArchitectureClass"),
    );
    const azureLinuxARM64 = parseGoStringArrayCases(
      goFunctionBody(goAzure, "azureARM64VMSizeCandidatesForClass"),
    );
    const azureWindows = parseGoStringArrayCases(
      goFunctionBody(goAzure, "azureWindowsVMSizeCandidatesForClass"),
    );
    const awsLinuxARM64 = parseGoStringArrayCases(
      goFunctionBody(go, "awsARM64InstanceTypeCandidatesForClass"),
    );
    const gcp = parseGoStringArrayCases(goFunctionBody(goGCP, "gcpMachineTypeCandidatesForClass"));
    const awsTarget = goFunctionBody(go, "awsInstanceTypeCandidatesForTargetModeArchitectureClass");
    const awsWSL2 = parseGoStringArrayCases(
      goSwitchAfter(awsTarget, "if windowsMode == windowsModeWSL2"),
    );
    const awsWindows = parseGoStringArrayCases(goSwitchAfter(awsTarget, "switch class", 1));

    for (const name of classes) {
      expect(serverTypeCandidatesForClass(name)).toEqual(hetzner[name]);
      expect(awsInstanceTypeCandidatesForClass(name)).toEqual(awsLinux[name]);
      expect(awsARM64InstanceTypeCandidatesForClass(name)).toEqual(awsLinuxARM64[name]);
      expect(azureVMSizeCandidatesForClass(name)).toEqual(azureLinux[name]);
      expect(azureARM64VMSizeCandidatesForClass(name)).toEqual(azureLinuxARM64[name]);
      expect(azureWindowsVMSizeCandidatesForClass(name)).toEqual(azureWindows[name]);
      expect(azureVMSizeCandidatesForTargetClass("windows", name)).toEqual(azureWindows[name]);
      expect(azureVMSizeCandidatesForTargetClass("windows", name, "wsl2")).toEqual(
        azureWindows[name],
      );
      expect(azureVMSizeCandidatesForTargetClass("windows", name, "normal", "arm64")).toEqual(
        azureLinuxARM64[name],
      );
      expect(azureVMSizeCandidatesForTargetClass("windows", name, "wsl2", "arm64")).toEqual([name]);
      expect(awsInstanceTypeCandidatesForTargetClass("windows", name)).toEqual(awsWindows[name]);
      expect(awsInstanceTypeCandidatesForTargetClass("windows", name, "wsl2")).toEqual(
        awsWSL2[name],
      );
      expect(gcpMachineTypeCandidatesForClass(name)).toEqual(gcp[name]);
    }
    expect(awsInstanceTypeCandidatesForTargetClass("macos", "standard")).toEqual(
      awsMacOSInstanceTypeCandidates,
    );
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
    expect(config.desktopEnv).toBe("xfce");
    expect(config.browser).toBe(false);
    expect(config.code).toBe(false);
    expect(config.ttlSeconds).toBe(86_400);
    expect(leaseConfig({ sshPublicKey: "ssh-ed25519 test", ttlSeconds: 0 }).ttlSeconds).toBe(5400);
    expect(leaseConfig({ sshPublicKey: "ssh-ed25519 test", ttlSeconds: -1 }).ttlSeconds).toBe(5400);
    expect(leaseConfig({ sshPublicKey: "ssh-ed25519 test", ttlSeconds: 42.9 }).ttlSeconds).toBe(42);
    expect(
      leaseConfig({ sshPublicKey: "ssh-ed25519 test", idleTimeoutSeconds: 0 }).idleTimeoutSeconds,
    ).toBe(1800);
    expect(
      leaseConfig({ sshPublicKey: "ssh-ed25519 test", idleTimeoutSeconds: -1 }).idleTimeoutSeconds,
    ).toBe(1800);
    expect(
      leaseConfig({ sshPublicKey: "ssh-ed25519 test", idleTimeoutSeconds: 999_999 })
        .idleTimeoutSeconds,
    ).toBe(86_400);
    expect(
      leaseConfig({ sshPublicKey: "ssh-ed25519 test", idleTimeoutSeconds: 42.9 })
        .idleTimeoutSeconds,
    ).toBe(42);
  });

  it("filters invalid SSH CIDR values before provider config", () => {
    expect(
      validCIDRs([
        " 203.0.113.7/32 ",
        "0.0.0.0/0",
        "255.255.255.255/32",
        "2001:db8::1/128",
        "::/0",
        "999.999.999.999/32",
        "256.0.0.1/32",
        "1.2.3/24",
        "1.2.3.4.5/24",
        "203.0.113.7/33",
        "203.0.113.7/-1",
        "203.0.113.7/x",
        "203.0.113.7",
        "203.0.113.7/32/extra",
        "::::/128",
        "::1]#junk/128",
        "::1]@[::2/128",
        "::\n1/128",
        "2001:db8::\t1/128",
        "2001:db8::1/129",
        "2001:db8::1/-1",
        "2001:db8::1/x",
        "not-a-cidr",
      ]),
    ).toEqual(["203.0.113.7/32", "0.0.0.0/0", "255.255.255.255/32", "2001:db8::1/128", "::/0"]);

    const config = leaseConfig({
      provider: "aws",
      sshPublicKey: "ssh-ed25519 test",
      awsSSHCIDRs: ["198.51.100.77/32"],
      gcpSSHCIDRs: ["2001:db8::2/128"],
    });
    expect(config.awsSSHCIDRs).toEqual(["198.51.100.77/32"]);
    expect(config.gcpSSHCIDRs).toEqual(["2001:db8::2/128"]);
    expect(() =>
      leaseConfig({
        provider: "aws",
        sshPublicKey: "ssh-ed25519 test",
        awsSSHCIDRs: ["198.51.100.77/32", "999.999.999.999/32"],
      }),
    ).toThrow("awsSSHCIDRs entries must be valid");
    expect(() =>
      leaseConfig({
        provider: "gcp",
        sshPublicKey: "ssh-ed25519 test",
        gcpSSHCIDRs: ["::::/128"],
      }),
    ).toThrow("gcpSSHCIDRs entries must be valid");
    expect(
      leaseConfig({
        provider: "gcp",
        sshPublicKey: "ssh-ed25519 test",
        awsSSHCIDRs: ["999.999.999.999/32", "198.51.100.77/32"],
        gcpSSHCIDRs: ["2001:db8::2/128"],
      }).awsSSHCIDRs,
    ).toEqual(["198.51.100.77/32"]);
    expect(
      leaseConfig({
        provider: "aws",
        sshPublicKey: "ssh-ed25519 test",
        awsSSHCIDRs: ["198.51.100.77/32"],
        gcpSSHCIDRs: ["::::/128", "2001:db8::2/128"],
      }).gcpSSHCIDRs,
    ).toEqual(["2001:db8::2/128"]);
    expect(
      leaseConfig({
        provider: "hetzner",
        sshPublicKey: "ssh-ed25519 test",
        awsSSHCIDRs: ["999.999.999.999/32"],
        gcpSSHCIDRs: ["::::/128"],
      }),
    ).toMatchObject({ awsSSHCIDRs: [], gcpSSHCIDRs: [] });
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

  it("normalizes the optional pond label and defaults it to empty", () => {
    const empty = leaseConfig({ sshPublicKey: "ssh-ed25519 test" });
    expect(empty.pond).toBe("");
    const tagged = leaseConfig({ sshPublicKey: "ssh-ed25519 test", pond: " Alpha Pond " });
    expect(tagged.pond).toBe("alpha-pond");
    const symbolRun = leaseConfig({
      sshPublicKey: "ssh-ed25519 test",
      pond: `alpha${"_".repeat(10_000)}pond`,
    });
    expect(symbolRun.pond).toBe("alpha-pond");
    expect(() => leaseConfig({ sshPublicKey: "ssh-ed25519 test", pond: " --- " })).toThrow(
      "pond must contain at least one letter or digit",
    );
    expect(() => leaseConfig({ sshPublicKey: "ssh-ed25519 test", pond: "a".repeat(42) })).toThrow(
      "pond must be 41 characters or fewer",
    );
  });

  it("validates and normalizes exposed ports", () => {
    const config = leaseConfig({
      sshPublicKey: "ssh-ed25519 test",
      exposedPorts: ["9090", "8080,443", "8080"],
    });
    expect(config.exposedPorts).toEqual(["443", "8080", "9090"]);
    expect(() => leaseConfig({ sshPublicKey: "ssh-ed25519 test", exposedPorts: ["abc"] })).toThrow(
      "must be a TCP port",
    );
    expect(() =>
      leaseConfig({ sshPublicKey: "ssh-ed25519 test", exposedPorts: ["70000"] }),
    ).toThrow("must be a TCP port");
    expect(() =>
      leaseConfig({
        sshPublicKey: "ssh-ed25519 test",
        exposedPorts: Array.from({ length: 11 }, (_, index) => String(8000 + index)),
      }),
    ).toThrow("at most 10 distinct ports");
  });

  it("validates desktop environment", () => {
    const config = leaseConfig({
      sshPublicKey: "ssh-ed25519 test",
      desktop: true,
      desktopEnv: "wayland",
    });
    expect(config.desktopEnv).toBe("wayland");
    const gnome = leaseConfig({
      sshPublicKey: "ssh-ed25519 test",
      desktop: true,
      desktopEnv: "gnome",
    });
    expect(gnome.desktopEnv).toBe("gnome");
    expect(() => leaseConfig({ sshPublicKey: "ssh-ed25519 test", desktopEnv: "kde" })).toThrow(
      "desktopEnv must be xfce, wayland, or gnome",
    );
    expect(() =>
      leaseConfig({
        sshPublicKey: "ssh-ed25519 test",
        provider: "aws",
        target: "windows",
        desktop: true,
        desktopEnv: "gnome",
      }),
    ).toThrow("desktopEnv=gnome requires target=linux");
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
    expect(config.os).toBe("ubuntu:26.04");
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
    expect(config.azureOSDisk).toBe("managed");
  });

  it("uses Azure ARM defaults when requested", () => {
    const config = leaseConfig({
      provider: "azure",
      architecture: "arm64",
      os: "ubuntu:26.04",
      sshPublicKey: "ssh-ed25519 test",
    });
    expect(config.architecture).toBe("arm64");
    expect(config.serverType).toBe("Standard_D96pds_v6");
    expect(config.azureImage).toBe("Canonical:ubuntu-26_04-lts:server-arm64:latest");
  });

  it("filters Azure defaults for ephemeral-preview full caching", () => {
    const arm = leaseConfig({
      provider: "azure",
      architecture: "arm64",
      class: "standard",
      azureOSDisk: "ephemeral-preview",
      sshPublicKey: "ssh-ed25519 test",
    });
    expect(arm.serverType).toBe("Standard_D32pds_v6");
    const windows = leaseConfig({
      provider: "azure",
      target: "windows",
      class: "standard",
      azureOSDisk: "ephemeral-preview",
      sshPublicKey: "ssh-ed25519 test",
    });
    expect(windows.serverType).toBe("Standard_D8ads_v6");
    const snapshot = leaseConfig({
      provider: "azure",
      target: "windows",
      class: "standard",
      azureSnapshot:
        "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/snapshots/checkpoint-azure",
      azureOSDisk: "ephemeral-preview",
      sshPublicKey: "ssh-ed25519 test",
    });
    expect(snapshot.serverType).toBe("Standard_D2ads_v6");
  });

  it("uses AWS ARM defaults when requested", () => {
    const config = leaseConfig({
      provider: "aws",
      architecture: "arm64",
      sshPublicKey: "ssh-ed25519 test",
    });
    expect(config.serverType).toBe("c7g.16xlarge");
  });

  it("infers Azure ARM architecture from explicit ARM VM sizes", () => {
    const config = leaseConfig({
      provider: "azure",
      serverType: "Standard_D32ps_v6",
      os: "ubuntu:24.04",
      sshPublicKey: "ssh-ed25519 test",
    });
    expect(config.architecture).toBe("arm64");
    expect(config.serverType).toBe("Standard_D32ps_v6");
    expect(config.azureImage).toBe("Canonical:ubuntu-24_04-lts:server-arm64:latest");
  });

  it("infers AWS ARM architecture from explicit Graviton instance types", () => {
    for (const serverType of [
      "a1.large",
      "c7g.16xlarge",
      "c7gd.16xlarge",
      "c7gn.16xlarge",
      "g5g.xlarge",
      "hpc7g.16xlarge",
      "im4gn.16xlarge",
      "is4gen.16xlarge",
    ]) {
      const config = leaseConfig({
        provider: "aws",
        serverType,
        sshPublicKey: "ssh-ed25519 test",
      });
      expect(config.architecture).toBe("arm64");
      expect(config.serverType).toBe(serverType);
    }
  });

  it("allows Azure Windows ARM64 leases", () => {
    const config = leaseConfig({
      provider: "azure",
      target: "windows",
      architecture: "arm64",
      serverType: "Standard_D32pds_v6",
      azureImage: "Contoso:windows-arm64:server:latest",
      sshPublicKey: "ssh-ed25519 test",
    });
    expect(config.architecture).toBe("arm64");
    expect(config.serverType).toBe("Standard_D32pds_v6");
    expect(config.azureImage).toBe("Contoso:windows-arm64:server:latest");

    const marketplace = leaseConfig({
      provider: "azure",
      target: "windows",
      architecture: "arm64",
      serverType: "Standard_D32pds_v6",
      azureImage: "Canonical:windows-arm64:server:latest",
      sshPublicKey: "ssh-ed25519 test",
    });
    expect(marketplace.azureImage).toBe("Canonical:windows-arm64:server:latest");

    const defaultImage = leaseConfig(
      {
        provider: "azure",
        target: "windows",
        architecture: "arm64",
        serverType: "Standard_D32pds_v6",
        sshPublicKey: "ssh-ed25519 test",
      },
      { azureImage: "Contoso:windows-arm64:server:latest" },
    );
    expect(defaultImage.azureImage).toBe("Contoso:windows-arm64:server:latest");
  });

  it("rejects Azure Windows ARM64 leases without an explicit ARM64 image", () => {
    expect(() =>
      leaseConfig({
        provider: "azure",
        target: "windows",
        architecture: "arm64",
        serverType: "Standard_D32pds_v6",
        sshPublicKey: "ssh-ed25519 test",
      }),
    ).toThrow("requires azureImage");
  });

  it("rejects Azure Windows ARM64 WSL2 leases", () => {
    expect(() =>
      leaseConfig({
        provider: "azure",
        target: "windows",
        windowsMode: "wsl2",
        architecture: "arm64",
        serverType: "Standard_D32pds_v6",
        azureImage: "Contoso:windows-arm64:server:latest",
        sshPublicKey: "ssh-ed25519 test",
      }),
    ).toThrow("supports windowsMode=normal only");
  });

  it("rejects ARM leases outside supported providers and targets", () => {
    expect(() =>
      leaseConfig({
        provider: "aws",
        target: "windows",
        architecture: "arm64",
        sshPublicKey: "ssh-ed25519 test",
      }),
    ).toThrow(
      "architecture=arm64 currently supports target=linux or provider=azure target=windows only",
    );
    expect(() =>
      leaseConfig({
        provider: "hetzner",
        architecture: "arm64",
        sshPublicKey: "ssh-ed25519 test",
      }),
    ).toThrow("architecture=arm64 currently supports provider=azure or provider=aws");
  });

  it("rejects explicit server types that do not match the requested architecture", () => {
    expect(() =>
      leaseConfig({
        provider: "aws",
        architecture: "arm64",
        serverType: "c7a.48xlarge",
        sshPublicKey: "ssh-ed25519 test",
      }),
    ).toThrow("architecture=arm64 requires an ARM64 AWS instance type");
    expect(() =>
      leaseConfig({
        provider: "aws",
        architecture: "amd64",
        serverType: "c7g.16xlarge",
        sshPublicKey: "ssh-ed25519 test",
      }),
    ).toThrow("architecture=amd64 requires an amd64 AWS instance type");
    expect(() =>
      leaseConfig({
        provider: "azure",
        architecture: "arm64",
        serverType: "Standard_D96ds_v6",
        sshPublicKey: "ssh-ed25519 test",
      }),
    ).toThrow("architecture=arm64 requires an ARM64 Azure VM size");
    expect(() =>
      leaseConfig({
        provider: "azure",
        architecture: "amd64",
        serverType: "Standard_D96pds_v6",
        sshPublicKey: "ssh-ed25519 test",
      }),
    ).toThrow("architecture=amd64 requires an amd64 Azure VM size");
  });

  it("normalizes Azure OS disk requests", () => {
    expect(
      leaseConfig({
        provider: "azure",
        azureOSDisk: "MANAGED",
        sshPublicKey: "ssh-ed25519 test",
      }).azureOSDisk,
    ).toBe("managed");
    expect(
      leaseConfig({
        provider: "azure",
        azureOSDisk: "ephemeral-preview",
        sshPublicKey: "ssh-ed25519 test",
      }).azureOSDisk,
    ).toBe("ephemeral-preview");
    expect(
      leaseConfig({
        provider: "azure",
        azureOSDisk: "auto",
        sshPublicKey: "ssh-ed25519 test",
      }).azureOSDisk,
    ).toBe("managed");
    expect(() =>
      leaseConfig({
        provider: "azure",
        azureOSDisk: "premium",
        sshPublicKey: "ssh-ed25519 test",
      }),
    ).toThrow("azureOSDisk must be auto, managed, ephemeral, or ephemeral-preview");
  });

  it("uses Worker Azure OS disk defaults when the request omits one", () => {
    expect(
      leaseConfig(
        {
          provider: "azure",
          sshPublicKey: "ssh-ed25519 test",
        },
        { azureOSDisk: "ephemeral" },
      ).azureOSDisk,
    ).toBe("ephemeral");
    expect(
      leaseConfig(
        {
          provider: "azure",
          azureOSDisk: "managed",
          sshPublicKey: "ssh-ed25519 test",
        },
        { azureOSDisk: "ephemeral" },
      ).azureOSDisk,
    ).toBe("managed");
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

  it("maps explicit portable OS selectors", () => {
    const config = leaseConfig({
      provider: "aws",
      os: "ubuntu-24.04",
      sshPublicKey: "ssh-ed25519 test",
    });
    expect(config.os).toBe("ubuntu:24.04");
    expect(config.image).toBe("ubuntu-24.04");
    expect(config.azureImage).toBe("Canonical:ubuntu-24_04-lts:server:latest");
    expect(config.gcpImage).toBe(
      "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64",
    );
  });

  it("does not apply portable Linux OS images to Azure Windows", () => {
    const config = leaseConfig({
      provider: "azure",
      target: "windows",
      os: "ubuntu:24.04",
      sshPublicKey: "ssh-ed25519 test",
    });
    expect(config.os).toBe("ubuntu:24.04");
    expect(config.azureImage).toBe("");
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
      desktop: true,
      sshPublicKey: "ssh-rsa test",
    });
    expect(config.serverType).toBe("Standard_D16ads_v6");
    expect(config.workRoot).toBe("C:\\crabbox");
    expect(config.windowsMode).toBe("normal");
    expect(config.sshUser).toBe("crabbox");
    expect(config.desktop).toBe(true);
    for (const capability of ["browser", "code", "tailscale"] as const) {
      expect(() =>
        leaseConfig({
          provider: "azure",
          target: "windows",
          [capability]: true,
          sshPublicKey: "ssh-rsa test",
        }),
      ).toThrow("browser/code/tailscale");
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
    expect(leaseConfig({ hostId: "h-neutral", sshPublicKey: "ssh-ed25519 test" }).hostID).toBe(
      "h-neutral",
    );
    expect(leaseConfig({ hostID: "h-compat", sshPublicKey: "ssh-ed25519 test" }).hostID).toBe(
      "h-compat",
    );
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
    expect(() =>
      leaseConfig({
        provider: "aws",
        target: "windows",
        windowsMode: "wsl2",
        desktop: true,
        sshPublicKey: "ssh-ed25519 test",
      }),
    ).toThrow("does not support desktop/VNC");
  });

  it("allows Azure Windows WSL2 leases", () => {
    const wsl2 = leaseConfig({
      provider: "azure",
      target: "windows",
      windowsMode: "wsl2",
      sshPublicKey: "ssh-ed25519 test",
    });
    expect(wsl2.serverType).toBe("Standard_D16ads_v6");
    expect(wsl2.workRoot).toBe("/work/crabbox");
    expect(wsl2.windowsMode).toBe("wsl2");
    expect(wsl2.sshUser).toBe("crabbox");
    expect(() =>
      leaseConfig({
        provider: "azure",
        target: "windows",
        windowsMode: "wsl2",
        desktop: true,
        sshPublicKey: "ssh-ed25519 test",
      }),
    ).toThrow("does not support desktop/VNC");
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
