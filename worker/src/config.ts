import type { LeaseRequest, Provider, TargetOS, WindowsMode } from "./types";

export interface LeaseConfig {
  provider: Provider;
  target: TargetOS;
  windowsMode: WindowsMode;
  desktop: boolean;
  browser: boolean;
  profile: string;
  class: string;
  serverType: string;
  serverTypeExplicit: boolean;
  location: string;
  image: string;
  awsRegion: string;
  awsAMI: string;
  awsSGID: string;
  awsSubnetID: string;
  awsProfile: string;
  awsRootGB: number;
  awsSSHCIDRs: string[];
  awsMacHostID: string;
  capacityMarket: "spot" | "on-demand";
  capacityStrategy:
    | "most-available"
    | "price-capacity-optimized"
    | "capacity-optimized"
    | "sequential";
  capacityFallback: string;
  capacityRegions: string[];
  capacityAvailabilityZones: string[];
  sshUser: string;
  sshPort: string;
  sshFallbackPorts: string[];
  providerKey: string;
  workRoot: string;
  ttlSeconds: number;
  idleTimeoutSeconds: number;
  keep: boolean;
  sshPublicKey: string;
}

export function leaseConfig(input: LeaseRequest): LeaseConfig {
  const provider = input.provider ?? "hetzner";
  if (provider !== "hetzner" && provider !== "aws") {
    throw new Error(`unsupported provider: ${String(provider)}`);
  }
  const target = normalizeTarget(input.target ?? input.targetOS ?? "linux");
  const windowsMode = normalizeWindowsMode(input.windowsMode ?? "normal");
  if (
    target !== "linux" &&
    !(provider === "aws" && target === "windows" && windowsMode === "normal") &&
    !(provider === "aws" && target === "macos")
  ) {
    if (provider === "aws" && target === "windows") {
      throw new Error("brokered aws target=windows requires windowsMode=normal");
    }
    if (provider === "hetzner") {
      throw new Error(unsupportedManagedTargetMessage(provider, target));
    }
    throw new Error(`unsupported target for brokered ${provider}: ${target}`);
  }
  if (target === "macos") {
    if (provider !== "aws") {
      throw new Error(`unsupported target for brokered ${provider}: ${target}`);
    }
    if ((input.capacity?.market ?? "spot") !== "on-demand") {
      throw new Error("brokered aws target=macos requires capacity.market=on-demand");
    }
  }
  const machineClass = input.class ?? "beast";
  const serverType = input.serverType ?? serverTypeForConfig(provider, target, machineClass);
  const ttlSeconds = clampTTL(input.ttlSeconds ?? 5400);
  const idleTimeoutSeconds = clampIdleTimeout(input.idleTimeoutSeconds ?? 1800);
  const sshPublicKey = input.sshPublicKey?.trim() ?? "";
  if (!sshPublicKey) {
    throw new Error("sshPublicKey is required");
  }
  return {
    provider,
    target,
    windowsMode,
    desktop: input.desktop ?? false,
    browser: input.browser ?? false,
    profile: input.profile ?? "default",
    class: machineClass,
    serverType,
    serverTypeExplicit: input.serverTypeExplicit ?? false,
    location: input.location ?? "fsn1",
    image: input.image ?? "ubuntu-24.04",
    awsRegion: input.awsRegion ?? "eu-west-1",
    awsAMI: input.awsAMI ?? "",
    awsSGID: input.awsSGID ?? "",
    awsSubnetID: input.awsSubnetID ?? "",
    awsProfile: input.awsProfile ?? "",
    awsRootGB: input.awsRootGB ?? 400,
    awsSSHCIDRs: validCIDRs(input.awsSSHCIDRs ?? []),
    awsMacHostID: input.awsMacHostID ?? "",
    capacityMarket: input.capacity?.market ?? "spot",
    capacityStrategy: input.capacity?.strategy ?? "most-available",
    capacityFallback: input.capacity?.fallback ?? "on-demand-after-120s",
    capacityRegions: input.capacity?.regions ?? [],
    capacityAvailabilityZones: input.capacity?.availabilityZones ?? [],
    sshUser: input.sshUser ?? (provider === "aws" && target === "macos" ? "ec2-user" : "crabbox"),
    sshPort: input.sshPort ?? "2222",
    sshFallbackPorts: validPorts(input.sshFallbackPorts ?? ["22"]),
    providerKey: input.providerKey ?? "crabbox-steipete",
    workRoot:
      input.workRoot ??
      (target === "windows" && windowsMode === "normal" ? "C:\\crabbox" : "/work/crabbox"),
    ttlSeconds,
    idleTimeoutSeconds,
    keep: input.keep ?? false,
    sshPublicKey,
  };
}

function unsupportedManagedTargetMessage(provider: Provider, target: TargetOS): string {
  if (target === "windows") {
    return `brokered ${provider} managed provisioning supports target=linux only; use brokered aws for managed Windows or provider=ssh for existing Windows hosts`;
  }
  if (target === "macos") {
    return `brokered ${provider} managed provisioning supports target=linux only; use brokered aws with an EC2 Mac Dedicated Host or provider=ssh for existing macOS hosts`;
  }
  return `brokered ${provider} managed provisioning supports target=linux only`;
}

function normalizeTarget(value: string): TargetOS {
  const normalized = value.trim().toLowerCase();
  if (normalized === "" || normalized === "linux" || normalized === "ubuntu") {
    return "linux";
  }
  if (
    normalized === "mac" ||
    normalized === "macos" ||
    normalized === "darwin" ||
    normalized === "osx"
  ) {
    return "macos";
  }
  if (normalized === "win" || normalized === "windows") {
    return "windows";
  }
  throw new Error(`target must be linux, macos, or windows`);
}

function normalizeWindowsMode(value: string): WindowsMode {
  const normalized = value.trim().toLowerCase();
  if (
    normalized === "" ||
    normalized === "normal" ||
    normalized === "native" ||
    normalized === "powershell"
  ) {
    return "normal";
  }
  if (normalized === "wsl" || normalized === "wsl2") {
    return "wsl2";
  }
  throw new Error(`windowsMode must be normal or wsl2`);
}

export function sshPorts(config: Pick<LeaseConfig, "sshPort" | "sshFallbackPorts">): string[] {
  return uniqueStrings([config.sshPort, ...config.sshFallbackPorts]);
}

export function validCIDRs(values: string[]): string[] {
  const cidrs = values.map((value) => value.trim()).filter(Boolean);
  return cidrs.filter(
    (cidr) =>
      /^(\d{1,3}\.){3}\d{1,3}\/([0-9]|[1-2][0-9]|3[0-2])$/.test(cidr) ||
      /^[0-9a-f:]+\/([0-9]|[1-9][0-9]|1[0-1][0-9]|12[0-8])$/i.test(cidr),
  );
}

function validPorts(values: string[]): string[] {
  return uniqueStrings(
    values
      .map((value) => value.trim())
      .filter((value) => /^[1-9][0-9]{0,4}$/.test(value) && Number(value) <= 65_535),
  );
}

function uniqueStrings(values: string[]): string[] {
  return [...new Set(values.filter(Boolean))];
}

export function serverTypeForClass(machineClass: string): string {
  return serverTypeCandidatesForClass(machineClass)[0] ?? machineClass;
}

export function serverTypeForProviderClass(provider: Provider, machineClass: string): string {
  if (provider === "aws") {
    return awsInstanceTypeCandidatesForClass(machineClass)[0] ?? machineClass;
  }
  return serverTypeForClass(machineClass);
}

export function serverTypeForConfig(
  provider: Provider,
  target: TargetOS,
  machineClass: string,
): string {
  if (provider === "aws") {
    return awsInstanceTypeCandidatesForTargetClass(target, machineClass)[0] ?? machineClass;
  }
  return serverTypeForClass(machineClass);
}

export function awsInstanceTypeCandidatesForTargetClass(
  target: TargetOS,
  machineClass: string,
): string[] {
  if (target === "macos") {
    return ["mac2.metal"];
  }
  if (target === "windows") {
    switch (machineClass) {
      case "standard":
        return ["m7i.large", "m7a.large", "t3.large"];
      case "fast":
        return ["m7i.xlarge", "m7a.xlarge", "t3.xlarge"];
      case "large":
        return ["m7i.2xlarge", "m7a.2xlarge", "t3.2xlarge"];
      case "beast":
        return ["m7i.4xlarge", "m7a.4xlarge", "m7i.2xlarge"];
      default:
        return [machineClass];
    }
  }
  return awsInstanceTypeCandidatesForClass(machineClass);
}

export function serverTypeCandidatesForClass(machineClass: string): string[] {
  switch (machineClass) {
    case "standard":
      return ["ccx33", "cpx62", "cx53"];
    case "fast":
      return ["ccx43", "cpx62", "cx53"];
    case "large":
      return ["ccx53", "ccx43", "cpx62", "cx53"];
    case "beast":
      return ["ccx63", "ccx53", "ccx43", "cpx62", "cx53"];
    default:
      return [machineClass];
  }
}

export function awsInstanceTypeCandidatesForClass(machineClass: string): string[] {
  switch (machineClass) {
    case "standard":
      return ["c7a.8xlarge", "c7i.8xlarge", "m7a.8xlarge", "m7i.8xlarge", "c7a.4xlarge"];
    case "fast":
      return [
        "c7a.16xlarge",
        "c7i.16xlarge",
        "m7a.16xlarge",
        "m7i.16xlarge",
        "c7a.12xlarge",
        "c7a.8xlarge",
      ];
    case "large":
      return [
        "c7a.24xlarge",
        "c7i.24xlarge",
        "m7a.24xlarge",
        "m7i.24xlarge",
        "r7a.24xlarge",
        "c7a.16xlarge",
        "c7a.12xlarge",
      ];
    case "beast":
      return [
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
      ];
    default:
      return [machineClass];
  }
}

function clampTTL(ttlSeconds: number): number {
  if (!Number.isFinite(ttlSeconds) || ttlSeconds <= 0) {
    return 5400;
  }
  return Math.min(Math.trunc(ttlSeconds), 86_400);
}

function clampIdleTimeout(seconds: number): number {
  if (!Number.isFinite(seconds) || seconds <= 0) {
    return 1800;
  }
  return Math.min(Math.trunc(seconds), 86_400);
}
