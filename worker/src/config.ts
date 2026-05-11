import type { LeaseRequest, Provider, TargetOS, WindowsMode } from "./types";

export interface LeaseConfig {
  provider: Provider;
  target: TargetOS;
  windowsMode: WindowsMode;
  desktop: boolean;
  browser: boolean;
  code: boolean;
  tailscale: boolean;
  tailscaleTags: string[];
  tailscaleHostname: string;
  tailscaleAuthKey: string;
  tailscaleExitNode: string;
  tailscaleExitNodeAllowLanAccess: boolean;
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
  azureLocation: string;
  azureImage: string;
  gcpProject: string;
  gcpZone: string;
  gcpImage: string;
  gcpNetwork: string;
  gcpSubnet: string;
  gcpTags: string[];
  gcpSSHCIDRs: string[];
  gcpRootGB: number;
  gcpServiceAccount: string;
  capacityMarket: "spot" | "on-demand";
  capacityStrategy:
    | "most-available"
    | "price-capacity-optimized"
    | "capacity-optimized"
    | "sequential";
  capacityFallback: string;
  capacityRegions: string[];
  capacityAvailabilityZones: string[];
  capacityHints: boolean;
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
  if (provider !== "hetzner" && provider !== "aws" && provider !== "azure" && provider !== "gcp") {
    throw new Error(`unsupported provider: ${String(provider)}`);
  }
  const target = normalizeTarget(input.target ?? input.targetOS ?? "linux");
  const windowsMode = normalizeWindowsMode(input.windowsMode ?? "normal");
  if (
    target !== "linux" &&
    !(provider === "aws" && target === "windows") &&
    !(provider === "aws" && target === "macos") &&
    !(provider === "azure" && target === "windows" && windowsMode === "normal")
  ) {
    if (provider === "hetzner" || provider === "azure" || provider === "gcp") {
      throw new Error(unsupportedManagedTargetMessage(provider, target, windowsMode));
    }
    throw new Error(`unsupported target for brokered ${provider}: ${target}`);
  }
  if (
    provider === "azure" &&
    target === "windows" &&
    (input.desktop || input.browser || input.code || input.tailscale)
  ) {
    throw new Error(
      "brokered azure target=windows currently supports SSH, sync, and run; desktop/browser/code/tailscale require Linux or AWS Windows where supported",
    );
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
  const serverType =
    input.serverType ?? serverTypeForConfig(provider, target, windowsMode, machineClass);
  const ttlSeconds = clampTTL(input.ttlSeconds ?? 5400);
  const idleTimeoutSeconds = clampIdleTimeout(input.idleTimeoutSeconds ?? 1800);
  const sshPublicKey = input.sshPublicKey?.trim() ?? "";
  if (!sshPublicKey) {
    throw new Error("sshPublicKey is required");
  }
  const sshUser = input.sshUser ?? defaultSSHUser(provider, target, windowsMode);
  const tailscaleExitNode = input.tailscaleExitNode?.trim() ?? "";
  const tailscaleExitNodeAllowLanAccess = input.tailscaleExitNodeAllowLanAccess ?? false;
  if (tailscaleExitNodeAllowLanAccess && !tailscaleExitNode) {
    throw new Error("tailscaleExitNodeAllowLanAccess requires tailscaleExitNode");
  }
  return {
    provider,
    target,
    windowsMode,
    desktop: input.desktop ?? false,
    browser: input.browser ?? false,
    code: input.code ?? false,
    tailscale: input.tailscale ?? false,
    tailscaleTags: normalizeTailscaleTags(input.tailscaleTags ?? ["tag:crabbox"]),
    tailscaleHostname: input.tailscaleHostname ?? "",
    tailscaleAuthKey: "",
    tailscaleExitNode,
    tailscaleExitNodeAllowLanAccess,
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
    azureLocation: input.azureLocation ?? "",
    azureImage: input.azureImage ?? "",
    gcpProject: input.gcpProject ?? "",
    gcpZone: input.gcpZone ?? "",
    gcpImage: input.gcpImage ?? "",
    gcpNetwork: input.gcpNetwork ?? "",
    gcpSubnet: input.gcpSubnet ?? "",
    gcpTags: uniqueStrings(input.gcpTags ?? []),
    gcpSSHCIDRs: validCIDRs(input.gcpSSHCIDRs ?? []),
    gcpRootGB: input.gcpRootGB ?? 0,
    gcpServiceAccount: input.gcpServiceAccount ?? "",
    capacityMarket: input.capacity?.market ?? "spot",
    capacityStrategy: input.capacity?.strategy ?? "most-available",
    capacityFallback: input.capacity?.fallback ?? "on-demand-after-120s",
    capacityRegions: input.capacity?.regions ?? [],
    capacityAvailabilityZones: input.capacity?.availabilityZones ?? [],
    capacityHints: input.capacity?.hints ?? true,
    sshUser,
    sshPort: input.sshPort ?? "2222",
    sshFallbackPorts: validPorts(input.sshFallbackPorts ?? ["22"]),
    providerKey: input.providerKey ?? "crabbox-steipete",
    workRoot: input.workRoot ?? defaultWorkRoot(target, windowsMode, sshUser),
    ttlSeconds,
    idleTimeoutSeconds,
    keep: input.keep ?? false,
    sshPublicKey,
  };
}

function defaultWorkRoot(target: TargetOS, windowsMode: WindowsMode, sshUser: string): string {
  if (target === "macos") {
    return `/Users/${sshUser || "ec2-user"}/crabbox`;
  }
  if (target === "windows" && windowsMode === "normal") {
    return "C:\\crabbox";
  }
  return "/work/crabbox";
}

function defaultSSHUser(provider: Provider, target: TargetOS, windowsMode: WindowsMode): string {
  if (provider === "aws" && target === "macos") {
    return "ec2-user";
  }
  if (provider === "aws" && target === "windows" && windowsMode === "wsl2") {
    return "Administrator";
  }
  return "crabbox";
}

function unsupportedManagedTargetMessage(
  provider: Provider,
  target: TargetOS,
  windowsMode: WindowsMode,
): string {
  if (provider === "azure" && target === "windows" && windowsMode === "wsl2") {
    return "brokered azure supports native Windows only; use brokered aws for managed Windows WSL2 or provider=ssh for existing Windows WSL2 hosts";
  }
  if (provider === "azure") {
    if (target === "macos") {
      return "brokered azure managed provisioning supports target=linux and native Windows only; use brokered aws with an EC2 Mac Dedicated Host or provider=ssh for existing macOS hosts";
    }
    return "brokered azure managed provisioning supports target=linux and native Windows only";
  }
  if (provider === "gcp") {
    if (target === "macos") {
      return "brokered gcp managed provisioning supports target=linux only; use brokered aws with an EC2 Mac Dedicated Host or provider=ssh for existing macOS hosts";
    }
    return "brokered gcp managed provisioning supports target=linux only";
  }
  if (target === "windows") {
    return `brokered ${provider} managed provisioning supports target=linux only; use brokered aws for managed Windows or provider=ssh for existing Windows hosts`;
  }
  if (target === "macos") {
    return `brokered ${provider} managed provisioning supports target=linux only; use brokered aws with an EC2 Mac Dedicated Host or provider=ssh for existing macOS hosts`;
  }
  return `brokered ${provider} managed provisioning supports target=linux only`;
}

export function azureLocationFor(
  env: { CRABBOX_AZURE_LOCATION?: string },
  override: string,
): string {
  return override.trim() || env.CRABBOX_AZURE_LOCATION?.trim() || "eastus";
}

export function normalizeTailscaleTags(values: string[]): string[] {
  return uniqueStrings(
    values
      .map((value) => value.trim().toLowerCase())
      .filter((value) => /^tag:[a-z0-9_-]{1,63}$/.test(value)),
  );
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
  if (provider === "azure") {
    return azureVMSizeCandidatesForClass(machineClass)[0] ?? machineClass;
  }
  if (provider === "gcp") {
    return gcpMachineTypeCandidatesForClass(machineClass)[0] ?? machineClass;
  }
  return serverTypeForClass(machineClass);
}

export function serverTypeForConfig(
  provider: Provider,
  target: TargetOS,
  windowsMode: WindowsMode,
  machineClass: string,
): string {
  if (provider === "aws") {
    return (
      awsInstanceTypeCandidatesForTargetClass(target, machineClass, windowsMode)[0] ?? machineClass
    );
  }
  if (provider === "azure") {
    return (
      azureVMSizeCandidatesForTargetClass(target, machineClass, windowsMode)[0] ?? machineClass
    );
  }
  if (provider === "gcp") {
    return gcpMachineTypeCandidatesForClass(machineClass)[0] ?? machineClass;
  }
  return serverTypeForClass(machineClass);
}

export function gcpMachineTypeCandidatesForClass(machineClass: string): string[] {
  switch (machineClass) {
    case "standard":
      return ["c4-standard-32", "c3-standard-22", "n2-standard-32", "n2d-standard-32"];
    case "fast":
      return [
        "c4-standard-64",
        "c3-standard-44",
        "n2-standard-64",
        "n2d-standard-64",
        "c4-standard-32",
      ];
    case "large":
      return [
        "c4-standard-96",
        "c3-standard-88",
        "n2-standard-80",
        "n2d-standard-96",
        "c4-standard-64",
      ];
    case "beast":
      return [
        "c4-standard-192",
        "c4-standard-96",
        "c3-standard-176",
        "c3-standard-88",
        "n2d-standard-224",
        "n2-standard-128",
      ];
    default:
      return [machineClass];
  }
}

export function azureVMSizeCandidatesForTargetClass(
  target: TargetOS,
  machineClass: string,
  windowsMode: WindowsMode = "normal",
): string[] {
  if (target === "linux") {
    return azureVMSizeCandidatesForClass(machineClass);
  }
  if (target === "windows" && windowsMode === "normal") {
    return azureWindowsVMSizeCandidatesForClass(machineClass);
  }
  return [machineClass];
}

export function azureVMSizeCandidatesForClass(machineClass: string): string[] {
  switch (machineClass) {
    case "standard":
      return [
        "Standard_D32ads_v6",
        "Standard_D32ds_v6",
        "Standard_F32s_v2",
        "Standard_D32ads_v5",
        "Standard_D32ds_v5",
        "Standard_D16ads_v6",
        "Standard_D16ds_v6",
        "Standard_F16s_v2",
      ];
    case "fast":
      return [
        "Standard_D64ads_v6",
        "Standard_D64ds_v6",
        "Standard_F64s_v2",
        "Standard_D64ads_v5",
        "Standard_D64ds_v5",
        "Standard_D48ads_v6",
        "Standard_D48ds_v6",
        "Standard_F48s_v2",
        "Standard_D32ads_v6",
        "Standard_D32ds_v6",
        "Standard_F32s_v2",
      ];
    case "large":
      return [
        "Standard_D96ads_v6",
        "Standard_D96ds_v6",
        "Standard_D96ads_v5",
        "Standard_D96ds_v5",
        "Standard_D64ads_v6",
        "Standard_D64ds_v6",
        "Standard_F64s_v2",
        "Standard_D48ads_v6",
        "Standard_D48ds_v6",
        "Standard_F48s_v2",
      ];
    case "beast":
      return [
        "Standard_D192ds_v6",
        "Standard_D128ds_v6",
        "Standard_D96ads_v6",
        "Standard_D96ds_v6",
        "Standard_D96ads_v5",
        "Standard_D96ds_v5",
        "Standard_D64ads_v6",
        "Standard_D64ds_v6",
        "Standard_F64s_v2",
      ];
    default:
      return [machineClass];
  }
}

export function azureWindowsVMSizeCandidatesForClass(machineClass: string): string[] {
  switch (machineClass) {
    case "standard":
      return [
        "Standard_D2ads_v6",
        "Standard_D2ds_v6",
        "Standard_D2ads_v5",
        "Standard_D2ds_v5",
        "Standard_D2as_v6",
      ];
    case "fast":
      return [
        "Standard_D4ads_v6",
        "Standard_D4ds_v6",
        "Standard_D4ads_v5",
        "Standard_D4ds_v5",
        "Standard_D4as_v6",
      ];
    case "large":
      return [
        "Standard_D8ads_v6",
        "Standard_D8ds_v6",
        "Standard_D8ads_v5",
        "Standard_D8ds_v5",
        "Standard_D8as_v6",
      ];
    case "beast":
      return [
        "Standard_D16ads_v6",
        "Standard_D16ds_v6",
        "Standard_D16ads_v5",
        "Standard_D16ds_v5",
        "Standard_D8ads_v6",
      ];
    default:
      return [machineClass];
  }
}

export function awsInstanceTypeCandidatesForTargetClass(
  target: TargetOS,
  machineClass: string,
  windowsMode: WindowsMode = "normal",
): string[] {
  if (target === "macos") {
    return ["mac2.metal"];
  }
  if (target === "windows") {
    if (windowsMode === "wsl2") {
      switch (machineClass) {
        case "standard":
          return ["m8i.large", "m8i-flex.large", "c8i.large", "r8i.large"];
        case "fast":
          return ["m8i.xlarge", "m8i-flex.xlarge", "c8i.xlarge", "r8i.xlarge"];
        case "large":
          return ["m8i.2xlarge", "m8i-flex.2xlarge", "c8i.2xlarge", "r8i.2xlarge"];
        case "beast":
          return ["m8i.4xlarge", "m8i-flex.4xlarge", "c8i.4xlarge", "r8i.4xlarge", "m8i.2xlarge"];
        default:
          return [machineClass];
      }
    }
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
