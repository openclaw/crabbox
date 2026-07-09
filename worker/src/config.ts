import { requireAWSRegion } from "./aws-region";
import { normalizeOSImage, osImageSpec } from "./os-image";
import type { LeaseRequest, Provider, TargetOS, WindowsMode } from "./types";

export const awsMacOSInstanceTypeCandidates = [
  "mac2.metal",
  "mac2-m2.metal",
  "mac2-m2pro.metal",
  "mac-m4.metal",
  "mac-m4pro.metal",
  "mac-m4max.metal",
  "mac2-m1ultra.metal",
  "mac-m3ultra.metal",
  "mac1.metal",
];

export const workspaceProviderKeyPrefix = "crabbox-workspace-";

export interface LeaseConfig {
  provider: Provider;
  target: TargetOS;
  architecture: Architecture;
  os: string;
  windowsMode: WindowsMode;
  desktop: boolean;
  desktopEnv: "xfce" | "wayland" | "gnome";
  browser: boolean;
  code: boolean;
  tailscale: boolean;
  tailscaleTags: string[];
  tailscaleHostname: string;
  tailscaleAuthKey: string;
  tailscaleInstallMode?: "package" | "pinned";
  tailscaleVersion?: string;
  tailscaleSHA256?: {
    amd64: string;
    arm64: string;
  };
  tailscaleExitNode: string;
  tailscaleExitNodeAllowLanAccess: boolean;
  profile: string;
  class: string;
  serverType: string;
  serverTypeExplicit: boolean;
  hostID: string;
  location: string;
  image: string;
  awsRegion: string;
  awsAMI: string;
  awsUseStockImage?: boolean;
  awsPromotedAMIs: Record<string, string>;
  awsSnapshot: string;
  awsSGID: string;
  awsSGName: string;
  awsSubnetID: string;
  awsProfile: string;
  awsRootGB: number;
  awsSSHCIDRs: string[];
  awsSSHCIDRsPinned: boolean;
  awsMacHostID: string;
  azureLocation: string;
  azureImage: string;
  azureSnapshot: string;
  azureOSDisk: AzureOSDiskMode;
  gcpProject: string;
  gcpZone: string;
  gcpImage: string;
  gcpMachineImage: string;
  gcpSnapshot: string;
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
  sshHostPrivateKey: string;
  sshHostPublicKey: string;
  pond: string;
  exposedPorts: string[];
}

export type AzureOSDiskMode = "managed" | "ephemeral" | "ephemeral-preview";
export type Architecture = "amd64" | "arm64";

export interface LeaseConfigDefaults {
  azureOSDisk?: string;
  azureImage?: string;
  azureWindowsARM64Image?: string;
}

const maxRequestedPondNameLength = 41;
const maxExposedPort = 65_535;
const maxExposedPortsPerLease = 10;

export function leaseConfig(input: LeaseRequest, defaults: LeaseConfigDefaults = {}): LeaseConfig {
  const provider = input.provider ?? "hetzner";
  if (
    provider !== "hetzner" &&
    provider !== "aws" &&
    provider !== "azure" &&
    provider !== "gcp" &&
    provider !== "daytona"
  ) {
    throw new Error(`unsupported provider: ${String(provider)}`);
  }
  const target = normalizeTarget(input.target ?? input.targetOS ?? "linux");
  const requestedArchitecture = normalizeArchitecture(input.architecture);
  const architectureExplicit = Boolean(input.architecture?.trim());
  const os = normalizeOSImage(input.os);
  const osExplicit = Boolean(input.os?.trim());
  const linuxOSImage = target === "linux" ? osImageSpec(os) : undefined;
  const windowsMode = normalizeWindowsMode(input.windowsMode ?? "normal");
  const architecture = architectureExplicit
    ? requestedArchitecture
    : inferArchitectureForServerType(provider, target, input.serverType, requestedArchitecture);
  const defaultAzureImage =
    provider === "azure" && target === "windows" && architecture === "arm64"
      ? defaults.azureWindowsARM64Image
      : undefined;
  const inputAzureImage = input.azureImage?.trim() || undefined;
  const azureImage =
    inputAzureImage ??
    defaultAzureImage ??
    (target === "linux" && osExplicit
      ? architecture === "arm64"
        ? (linuxOSImage?.azureArm64Image ?? "")
        : (linuxOSImage?.azureImage ?? "")
      : "");
  if (
    target !== "linux" &&
    !(provider === "aws" && target === "windows") &&
    !(provider === "aws" && target === "macos") &&
    !(provider === "azure" && target === "windows")
  ) {
    if (
      provider === "hetzner" ||
      provider === "azure" ||
      provider === "gcp" ||
      provider === "daytona"
    ) {
      throw new Error(unsupportedManagedTargetMessage(provider, target));
    }
    throw new Error(`unsupported target for brokered ${provider}: ${target}`);
  }
  if (architecture === "arm64") {
    if (provider !== "azure" && provider !== "aws") {
      throw new Error("architecture=arm64 currently supports provider=azure or provider=aws");
    }
    if (target !== "linux" && !(provider === "azure" && target === "windows")) {
      throw new Error(
        "architecture=arm64 currently supports target=linux or provider=azure target=windows only",
      );
    }
    if (provider === "azure" && target === "windows" && windowsMode === "wsl2") {
      throw new Error(
        "brokered provider=azure target=windows architecture=arm64 supports windowsMode=normal only; windowsMode=wsl2 requires nested virtualization, which Azure Cobalt ARM64 VM sizes do not support",
      );
    }
    if (
      provider === "azure" &&
      target === "windows" &&
      !azureWindowsARM64HasExplicitImage(azureImage)
    ) {
      throw new Error(
        "brokered provider=azure target=windows architecture=arm64 requires azureImage with an ARM64 Windows image; the built-in Windows default is x64",
      );
    }
  }
  if (provider === "daytona" && architectureExplicit) {
    throw new Error(
      "brokered provider=daytona takes architecture from the configured Daytona snapshot",
    );
  }
  if (provider === "daytona" && (input.desktop || input.browser || input.code || input.tailscale)) {
    throw new Error(
      "brokered provider=daytona currently supports SSH, sync, and run only; desktop/browser/code/tailscale are unavailable",
    );
  }
  if (provider === "daytona" && input.serverTypeExplicit) {
    throw new Error(
      "brokered provider=daytona takes CPU, memory, and disk from the configured Daytona snapshot",
    );
  }
  if (
    provider === "azure" &&
    target === "windows" &&
    (input.browser || input.code || input.tailscale)
  ) {
    throw new Error(
      "brokered azure target=windows currently supports SSH, sync, run, and desktop/VNC; browser/code/tailscale require Linux or AWS Windows where supported",
    );
  }
  if (target === "windows" && windowsMode === "wsl2" && input.desktop) {
    throw new Error(
      "brokered target=windows windowsMode=wsl2 does not support desktop/VNC; use windowsMode=normal for desktop/VNC or omit desktop for WSL2",
    );
  }
  const desktopEnv = normalizeDesktopEnv(input.desktopEnv);
  if ((desktopEnv === "wayland" || desktopEnv === "gnome") && input.desktop && target !== "linux") {
    throw new Error(`desktopEnv=${desktopEnv} requires target=linux`);
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
  const azureOSDisk = normalizeAzureOSDiskMode(input.azureOSDisk ?? defaults.azureOSDisk);
  const azureSnapshot = input.azureSnapshot ?? "";
  const serverTypeAzureOSDisk = azureSnapshot ? "managed" : azureOSDisk;
  const serverType =
    provider === "daytona"
      ? "snapshot"
      : (input.serverType ??
        serverTypeForConfig(
          provider,
          target,
          windowsMode,
          machineClass,
          architecture,
          serverTypeAzureOSDisk,
        ));
  if (input.serverType && provider !== "daytona") {
    validateArchitectureServerType(
      provider,
      target,
      architecture,
      architectureExplicit,
      serverType,
    );
  }
  const ttlSeconds = clampTTL(input.ttlSeconds ?? 5400);
  const idleTimeoutSeconds = clampIdleTimeout(input.idleTimeoutSeconds ?? 1800);
  const awsSSHCIDRs =
    provider === "aws"
      ? validatedCIDRs(input.awsSSHCIDRs ?? [], "awsSSHCIDRs")
      : validCIDRs(input.awsSSHCIDRs ?? []);
  const gcpSSHCIDRs =
    provider === "gcp"
      ? validatedCIDRs(input.gcpSSHCIDRs ?? [], "gcpSSHCIDRs")
      : validCIDRs(input.gcpSSHCIDRs ?? []);
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
  const requestedAWSRegion =
    typeof input.awsRegion === "string" ? input.awsRegion.trim() : input.awsRegion;
  const awsRegion = requireAWSRegion(requestedAWSRegion || "eu-west-1", "awsRegion");
  const capacityRegions =
    provider === "aws"
      ? uniqueStrings(
          (input.capacity?.regions ?? [])
            .filter((region) => typeof region !== "string" || region.trim() !== "")
            .map((region) => requireAWSRegion(region, "capacity.regions entry")),
        )
      : (input.capacity?.regions ?? []);
  return {
    provider,
    target,
    architecture,
    os,
    windowsMode,
    desktop: input.desktop ?? false,
    desktopEnv,
    browser: input.browser ?? false,
    code: input.code ?? false,
    tailscale: input.tailscale ?? false,
    tailscaleTags: normalizeTailscaleTags(input.tailscaleTags ?? ["tag:crabbox"]),
    tailscaleHostname: input.tailscaleHostname ?? "",
    tailscaleAuthKey: "",
    tailscaleInstallMode: "package",
    tailscaleVersion: "",
    tailscaleSHA256: { amd64: "", arm64: "" },
    tailscaleExitNode,
    tailscaleExitNodeAllowLanAccess,
    profile: input.profile ?? "default",
    class: machineClass,
    serverType,
    serverTypeExplicit: input.serverTypeExplicit ?? false,
    hostID: input.hostId ?? input.hostID ?? "",
    location: input.location ?? "fsn1",
    image: input.image ?? linuxOSImage?.hetznerImage ?? "ubuntu-24.04",
    awsRegion,
    awsAMI: input.awsAMI ?? "",
    awsUseStockImage: false,
    awsPromotedAMIs: {},
    awsSnapshot: input.awsSnapshot ?? "",
    awsSGID: input.awsSGID ?? "",
    awsSGName: "",
    awsSubnetID: input.awsSubnetID ?? "",
    awsProfile: input.awsProfile ?? "",
    awsRootGB: input.awsRootGB ?? 400,
    awsSSHCIDRs,
    awsSSHCIDRsPinned: input.awsSSHCIDRsPinned ?? (input.awsSSHCIDRs?.length ?? 0) > 0,
    awsMacHostID: input.awsMacHostID ?? "",
    azureLocation: input.azureLocation ?? "",
    azureImage,
    azureSnapshot,
    azureOSDisk,
    gcpProject: input.gcpProject ?? "",
    gcpZone: input.gcpZone ?? "",
    gcpImage: input.gcpImage ?? (osExplicit ? (linuxOSImage?.gcpImage ?? "") : ""),
    gcpMachineImage: input.gcpMachineImage ?? "",
    gcpSnapshot: input.gcpSnapshot ?? "",
    gcpNetwork: input.gcpNetwork ?? "",
    gcpSubnet: input.gcpSubnet ?? "",
    gcpTags: uniqueStrings(input.gcpTags ?? []),
    gcpSSHCIDRs,
    gcpRootGB: input.gcpRootGB ?? 0,
    gcpServiceAccount: input.gcpServiceAccount ?? "",
    capacityMarket: input.capacity?.market ?? "spot",
    capacityStrategy: input.capacity?.strategy ?? "most-available",
    capacityFallback: input.capacity?.fallback ?? "on-demand-after-120s",
    capacityRegions,
    capacityAvailabilityZones: input.capacity?.availabilityZones ?? [],
    capacityHints: input.capacity?.hints ?? true,
    sshUser,
    sshPort: provider === "daytona" ? "22" : (input.sshPort ?? "2222"),
    sshFallbackPorts: provider === "daytona" ? [] : validPorts(input.sshFallbackPorts ?? ["22"]),
    providerKey: input.providerKey?.trim() ?? "",
    workRoot:
      input.workRoot ??
      (provider === "daytona"
        ? "/home/daytona/crabbox"
        : defaultWorkRoot(target, windowsMode, sshUser)),
    ttlSeconds,
    idleTimeoutSeconds,
    keep: input.keep ?? false,
    sshPublicKey,
    sshHostPrivateKey: "",
    sshHostPublicKey: "",
    pond: requestedPondName(input.pond ?? ""),
    exposedPorts: normalizeExposedPorts(input.exposedPorts ?? []),
  };
}

// normalizePondName mirrors the Go-side helper in internal/cli/pond.go. The
// `pond` label is a reserved provider-label key that groups leases for
// peer discovery and lifecycle commands.
export function normalizePondName(value: string): string {
  const normalized = value.toLowerCase().trim();
  const output: string[] = [];
  let lastDash = false;
  for (const char of normalized) {
    const code = char.charCodeAt(0);
    const isLetter = code >= 97 && code <= 122;
    const isDigit = code >= 48 && code <= 57;
    if (isLetter || isDigit) {
      output.push(char);
      lastDash = false;
      continue;
    }
    if (!lastDash) {
      output.push("-");
      lastDash = true;
    }
  }
  let start = 0;
  let end = output.length;
  while (output[start] === "-") {
    start += 1;
  }
  while (end > start && output[end - 1] === "-") {
    end -= 1;
  }
  return output.slice(start, end).join("");
}

function requestedPondName(value: string): string {
  if (value.trim() === "") {
    return "";
  }
  const name = normalizePondName(value);
  if (!name) {
    throw new Error("pond must contain at least one letter or digit");
  }
  if (name.length > maxRequestedPondNameLength) {
    throw new Error(
      `pond must be ${maxRequestedPondNameLength} characters or fewer after normalization`,
    );
  }
  return name;
}

export function normalizeDesktopEnv(value: string | undefined): "xfce" | "wayland" | "gnome" {
  const normalized = (value ?? "").trim().toLowerCase();
  switch (normalized) {
    case "":
    case "xfce":
      return "xfce";
    case "wayland":
      return "wayland";
    case "gnome":
      return "gnome";
    default:
      throw new Error("desktopEnv must be xfce, wayland, or gnome");
  }
}

export function normalizeArchitecture(value: string | undefined): Architecture {
  const normalized = (value ?? "").trim().toLowerCase();
  switch (normalized) {
    case "":
    case "amd64":
    case "x86_64":
    case "x64":
      return "amd64";
    case "arm64":
    case "aarch64":
      return "arm64";
    default:
      throw new Error("architecture must be amd64 or arm64");
  }
}

function inferArchitectureForServerType(
  provider: Provider,
  target: TargetOS,
  serverType: string | undefined,
  fallback: Architecture,
): Architecture {
  if (!serverType) {
    return fallback;
  }
  if (
    provider === "azure" &&
    (target === "linux" || target === "windows") &&
    azureVMSizeIsARM64(serverType)
  ) {
    return "arm64";
  }
  if (provider === "aws" && target === "linux" && awsInstanceTypeIsARM64(serverType)) {
    return "arm64";
  }
  return fallback;
}

function validateArchitectureServerType(
  provider: Provider,
  target: TargetOS,
  architecture: Architecture,
  architectureExplicit: boolean,
  serverType: string,
): void {
  if (target !== "linux" && !(provider === "azure" && target === "windows")) {
    return;
  }
  const serverTypeARM64 =
    (provider === "azure" && azureVMSizeIsARM64(serverType)) ||
    (provider === "aws" && awsInstanceTypeIsARM64(serverType));
  if (architecture === "arm64" && !serverTypeARM64) {
    throw new Error(
      `architecture=arm64 requires an ARM64 ${providerServerTypeName(provider)}; ${serverType} is not ARM64`,
    );
  }
  if (architectureExplicit && architecture === "amd64" && serverTypeARM64) {
    throw new Error(
      `architecture=amd64 requires an amd64 ${providerServerTypeName(provider)}; ${serverType} is ARM64`,
    );
  }
}

function providerServerTypeName(provider: Provider): string {
  if (provider === "azure") {
    return "Azure VM size";
  }
  if (provider === "aws") {
    return "AWS instance type";
  }
  return "server type";
}

function azureWindowsARM64HasExplicitImage(image: string | undefined): boolean {
  const normalized = image?.trim() ?? "";
  if (!normalized) return false;
  const defaultImages = new Set([
    "MicrosoftWindowsServer:windowsserver2022:2022-datacenter-smalldisk-g2:latest",
    "Canonical:0001-com-ubuntu-server-jammy:22_04-lts-gen2:latest",
    "Canonical:0001-com-ubuntu-server-noble:24_04-lts-gen2:latest",
    osImageSpec("ubuntu:24.04").azureImage,
    osImageSpec("ubuntu:24.04").azureArm64Image,
    osImageSpec("ubuntu:26.04").azureImage,
    osImageSpec("ubuntu:26.04").azureArm64Image,
  ]);
  return !defaultImages.has(normalized);
}

export function awsPromotedAMIConfigKey(region: string, serverType: string): string {
  return `${region.trim().toLowerCase()}\0${serverType.trim().toLowerCase()}`;
}

export function normalizeAzureOSDiskMode(value: string | undefined): AzureOSDiskMode {
  const normalized = (value ?? "").trim().toLowerCase();
  switch (normalized) {
    case "":
    case "managed":
      return "managed";
    case "auto":
      return "managed";
    case "ephemeral":
      return "ephemeral";
    case "ephemeral-preview":
      return "ephemeral-preview";
    default:
      throw new Error("azureOSDisk must be auto, managed, ephemeral, or ephemeral-preview");
  }
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
  if (provider === "daytona") {
    return "daytona";
  }
  if (provider === "aws" && target === "macos") {
    return "ec2-user";
  }
  if (provider === "aws" && target === "windows" && windowsMode === "wsl2") {
    return "Administrator";
  }
  return "crabbox";
}

function unsupportedManagedTargetMessage(provider: Provider, target: TargetOS): string {
  if (provider === "azure") {
    if (target === "macos") {
      return "brokered azure managed provisioning supports target=linux and Windows only; use brokered aws with an EC2 Mac Dedicated Host or provider=ssh for existing macOS hosts";
    }
    return "brokered azure managed provisioning supports target=linux and Windows only";
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
  return cidrs.filter(isValidCIDR);
}

export function validatedCIDRs(values: string[], fieldName: string): string[] {
  const cidrs = values.map((value) => value.trim()).filter(Boolean);
  const valid = cidrs.filter(isValidCIDR);
  if (valid.length !== cidrs.length) {
    throw new Error(`${fieldName} entries must be valid IPv4 or IPv6 CIDR ranges`);
  }
  return valid;
}

function isValidCIDR(cidr: string): boolean {
  const firstSlash = cidr.indexOf("/");
  if (firstSlash <= 0 || firstSlash !== cidr.lastIndexOf("/")) {
    return false;
  }
  const address = cidr.slice(0, firstSlash);
  const prefixText = cidr.slice(firstSlash + 1);
  if (!/^[0-9]+$/.test(prefixText)) {
    return false;
  }
  const prefix = Number(prefixText);
  if (address.includes(":")) {
    return prefix >= 0 && prefix <= 128 && isValidIPv6Address(address);
  }
  return prefix >= 0 && prefix <= 32 && isValidIPv4Address(address);
}

function isValidIPv4Address(address: string): boolean {
  const parts = address.split(".");
  return (
    parts.length === 4 &&
    parts.every((part) => {
      if (!/^[0-9]{1,3}$/.test(part)) {
        return false;
      }
      const octet = Number(part);
      return Number.isInteger(octet) && octet >= 0 && octet <= 255;
    })
  );
}

function isValidIPv6Address(address: string): boolean {
  if (!/^[0-9A-Fa-f:.]+$/.test(address)) {
    return false;
  }
  if (["[", "]", "@", "/", "?", "#"].some((delimiter) => address.includes(delimiter))) {
    return false;
  }
  try {
    const parsed = new URL(`http://[${address}]/`);
    return (
      parsed.hostname !== "" &&
      parsed.username === "" &&
      parsed.password === "" &&
      parsed.search === "" &&
      parsed.hash === "" &&
      parsed.pathname === "/"
    );
  } catch {
    return false;
  }
}

function validPorts(values: string[]): string[] {
  return uniqueStrings(
    values
      .map((value) => value.trim())
      .filter((value) => /^[1-9][0-9]{0,4}$/.test(value) && Number(value) <= 65_535),
  );
}

function normalizeExposedPorts(values: string[]): string[] {
  const seen = new Set<number>();
  for (const raw of values) {
    if (raw.trim() === "") {
      throw new Error("exposedPorts values must not be empty");
    }
    const parts = raw
      .split(",")
      .map((part) => part.trim())
      .filter(Boolean);
    if (parts.length === 0) {
      throw new Error("exposedPorts values must not be empty");
    }
    for (const part of parts) {
      if (!/^[1-9][0-9]*$/.test(part)) {
        throw new Error(`exposedPorts value ${part} must be a TCP port in 1..${maxExposedPort}`);
      }
      const port = Number(part);
      if (!Number.isSafeInteger(port) || port <= 0 || port > maxExposedPort) {
        throw new Error(`exposedPorts value ${part} must be a TCP port in 1..${maxExposedPort}`);
      }
      seen.add(port);
    }
  }
  if (seen.size > maxExposedPortsPerLease) {
    throw new Error(`exposedPorts accepts at most ${maxExposedPortsPerLease} distinct ports`);
  }
  return [...seen].toSorted((left, right) => left - right).map((port) => String(port));
}

function uniqueStrings(values: string[]): string[] {
  return [...new Set(values.filter(Boolean))];
}

export function serverTypeForClass(machineClass: string): string {
  return serverTypeCandidatesForClass(machineClass)[0] ?? machineClass;
}

export function serverTypeForProviderClass(provider: Provider, machineClass: string): string {
  if (provider === "daytona") {
    return "snapshot";
  }
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
  architecture: Architecture = "amd64",
  azureOSDisk: AzureOSDiskMode = "managed",
): string {
  if (provider === "daytona") {
    return "snapshot";
  }
  if (provider === "aws") {
    return (
      awsInstanceTypeCandidatesForTargetClass(target, machineClass, windowsMode, architecture)[0] ??
      machineClass
    );
  }
  if (provider === "azure") {
    return (
      azureVMSizeCandidatesForTargetClass(
        target,
        machineClass,
        windowsMode,
        architecture,
        azureOSDisk,
      )[0] ?? machineClass
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
  architecture: Architecture = "amd64",
  azureOSDisk: AzureOSDiskMode = "managed",
): string[] {
  let candidates: string[];
  if (target === "linux") {
    candidates = azureVMSizeCandidatesForArchitectureClass(architecture, machineClass);
  } else if (target === "windows" && (windowsMode === "normal" || windowsMode === "wsl2")) {
    candidates =
      architecture === "arm64"
        ? windowsMode === "wsl2"
          ? [machineClass]
          : azureARM64VMSizeCandidatesForClass(machineClass)
        : azureWindowsVMSizeCandidatesForClass(machineClass);
  } else {
    candidates = [machineClass];
  }
  if (azureOSDisk === "ephemeral-preview") {
    return azureEphemeralFullCachingCandidates(target, candidates, architecture, windowsMode);
  }
  return candidates;
}

export function azureVMSizeCandidatesForClass(machineClass: string): string[] {
  return azureVMSizeCandidatesForArchitectureClass("amd64", machineClass);
}

export function azureVMSizeCandidatesForArchitectureClass(
  architecture: Architecture,
  machineClass: string,
): string[] {
  if (architecture === "arm64") {
    return azureARM64VMSizeCandidatesForClass(machineClass);
  }
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

export function azureARM64VMSizeCandidatesForClass(machineClass: string): string[] {
  switch (machineClass) {
    case "standard":
      return ["Standard_D32pds_v6", "Standard_D32ps_v6", "Standard_D16pds_v6", "Standard_D16ps_v6"];
    case "fast":
      return [
        "Standard_D64pds_v6",
        "Standard_D64ps_v6",
        "Standard_D48pds_v6",
        "Standard_D48ps_v6",
        "Standard_D32pds_v6",
        "Standard_D32ps_v6",
      ];
    case "large":
      return [
        "Standard_D96pds_v6",
        "Standard_D96ps_v6",
        "Standard_D64pds_v6",
        "Standard_D64ps_v6",
        "Standard_D48pds_v6",
        "Standard_D48ps_v6",
      ];
    case "beast":
      return ["Standard_D96pds_v6", "Standard_D96ps_v6", "Standard_D64pds_v6", "Standard_D64ps_v6"];
    default:
      return [machineClass];
  }
}

export function azureVMSizeIsARM64(vmSize: string): boolean {
  const normalized = vmSize.trim().toLowerCase();
  return (
    normalized.includes("ps_v6") ||
    normalized.includes("pds_v6") ||
    normalized.includes("pls_v6") ||
    normalized.includes("plds_v6")
  );
}

export function azureSupportsEphemeralOS(vmSize: string): boolean {
  const normalized = vmSize.toLowerCase();
  if (normalized.startsWith("standard_f") && normalized.endsWith("s_v2")) {
    return true;
  }
  if (normalized.includes("pds_v6") || normalized.includes("plds_v6")) {
    return true;
  }
  if (
    (normalized.startsWith("standard_d") || normalized.startsWith("standard_e")) &&
    (normalized.includes("ds_v5") || normalized.includes("ds_v6"))
  ) {
    return true;
  }
  return false;
}

export function azureSupportsEphemeralFullCaching(vmSize: string): boolean {
  if (!azureSupportsEphemeralOS(vmSize)) return false;
  const cores = azureVMSizeVCPUCount(vmSize);
  return cores !== undefined && cores > 4;
}

function azureVMSizeVCPUCount(vmSize: string): number | undefined {
  const match = vmSize
    .trim()
    .toLowerCase()
    .match(/^standard_[a-z]+(\d+)/);
  if (!match?.[1]) return undefined;
  return Number.parseInt(match[1], 10);
}

function azureEphemeralFullCachingCandidates(
  target: TargetOS,
  candidates: string[],
  architecture: Architecture = "amd64",
  windowsMode: WindowsMode = "normal",
): string[] {
  const filtered = candidates.filter(azureSupportsEphemeralFullCaching);
  if (filtered.length > 0) return filtered;
  if (target === "windows") {
    return [
      ...azureVMSizeCandidatesForTargetClass(target, "large", windowsMode, architecture),
      ...azureVMSizeCandidatesForTargetClass(target, "beast", windowsMode, architecture),
    ]
      .filter((value, index, values) => values.indexOf(value) === index)
      .filter(azureSupportsEphemeralFullCaching);
  }
  return candidates;
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
  architecture: Architecture = "amd64",
): string[] {
  if (target === "macos") {
    return awsMacOSInstanceTypeCandidates;
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
  return awsInstanceTypeCandidatesForArchitectureClass(architecture, machineClass);
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
  return awsInstanceTypeCandidatesForArchitectureClass("amd64", machineClass);
}

export function awsInstanceTypeCandidatesForArchitectureClass(
  architecture: Architecture,
  machineClass: string,
): string[] {
  if (architecture === "arm64") {
    return awsARM64InstanceTypeCandidatesForClass(machineClass);
  }
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

export function awsARM64InstanceTypeCandidatesForClass(machineClass: string): string[] {
  switch (machineClass) {
    case "standard":
      return ["c7g.8xlarge", "m7g.8xlarge", "r7g.8xlarge", "c7g.4xlarge"];
    case "fast":
      return ["c7g.16xlarge", "m7g.16xlarge", "r7g.16xlarge", "c7g.12xlarge", "c7g.8xlarge"];
    case "large":
      return ["c7g.16xlarge", "m7g.16xlarge", "r7g.16xlarge", "c7g.12xlarge"];
    case "beast":
      return ["c7g.16xlarge", "m7g.16xlarge", "r7g.16xlarge", "c7g.12xlarge"];
    default:
      return [machineClass];
  }
}

export function awsInstanceTypeIsARM64(instanceType: string): boolean {
  const family = instanceType.trim().toLowerCase().split(".")[0] ?? "";
  if (["a1", "g5g", "hpc7g", "i4g", "im4gn", "is4gen", "t4g", "x2gd"].includes(family)) {
    return true;
  }
  return /^[cmr][0-9]+g[dn]?$/.test(family);
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
