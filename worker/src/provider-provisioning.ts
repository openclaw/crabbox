import type { LeaseConfig } from "./config";
import type { CoordinatorStorageView } from "./coordinator-runtime";
import type { ProvisioningMaterial } from "./provisioning-material";
import type { Provider } from "./types";
import type { LeaseRecord, ProviderMachine, LeaseImageIdentity } from "./types";

export type ProvisioningPhase =
  | "prepared"
  | "provisioning"
  | "ready-to-publish"
  | "settling"
  | "cleanup"
  | "blocked"
  | "retained"
  | "terminal";

export interface FrozenProvisioningPlan {
  version: 1;
  provider: Provider;
  scope: string;
  // Cleanup readers use these exact identities even before the lease has a selected machine.
  resources: Array<{ cloudID: string; region: string; scope: string }>;
  data: unknown;
}

export interface ProvisioningPublication {
  server: ProviderMachine;
  serverType: string;
  market: string;
  image?: LeaseImageIdentity;
  cost?: { hourlyUSD: number; maxUSD: number };
}

export interface ProvisioningStep {
  phase: ProvisioningPhase;
  attempt: number;
  state: unknown;
  nextWake: number;
  delivery?: number;
  coordinationKey?: string;
  publication?: ProvisioningPublication;
  // Provider diagnostics must be static codes, never remote response text.
  blockedReason?: string;
}

export interface ProviderResumableProvisioning {
  version: 1;
  supports(config: LeaseConfig): boolean;
  validateAdmission?(
    storage: CoordinatorStorageView,
    plan: FrozenProvisioningPlan,
    lease: LeaseRecord,
  ): Promise<void>;
  prepare(
    config: LeaseConfig,
    lease: LeaseRecord,
  ): Promise<{
    plan: FrozenProvisioningPlan;
    material: ProvisioningMaterial;
    step: ProvisioningStep;
  }>;
  advance(
    input: {
      plan: FrozenProvisioningPlan;
      step: ProvisioningStep;
      lease: LeaseRecord;
      deadline: number;
      retain?: boolean;
      recovering: boolean;
    } & (
      | { canceled: false; material: ProvisioningMaterial }
      | { canceled: true; material?: never }
    ),
  ): Promise<ProvisioningStep>;
}

export interface ProviderProvisioningCleanupClaim {
  provider: Extract<Provider, "aws" | "azure" | "gcp" | "daytona">;
  cloudID: string;
  region?: string;
  providerProject?: string;
  providerScope?: string;
  serverID?: number;
  providerResourceID?: string;
}

// Unlike retryable cleanup failure, this requires identity or scope resolution before
// another provider operation can be authorized. It never establishes resource absence.
export class ProviderResourceUnresolvedError extends Error {
  override name = "ProviderResourceUnresolvedError";
}

export class ProviderProvisioningOutcomeUncertainError extends Error {
  override name = "ProviderProvisioningOutcomeUncertainError";
}

export class ProviderProvisioningCleanupError extends Error {
  constructor(
    message: string,
    readonly cleanupClaim: ProviderProvisioningCleanupClaim,
    cause: unknown,
  ) {
    super(message, { cause });
    this.name = "ProviderProvisioningCleanupError";
  }
}

export function providerProvisioningCleanupClaim(
  error: unknown,
): ProviderProvisioningCleanupClaim | undefined {
  let current = error;
  for (let depth = 0; depth < 8 && current instanceof Error; depth += 1) {
    if (current instanceof ProviderProvisioningCleanupError) return current.cleanupClaim;
    current = current.cause;
  }
  return undefined;
}

export function providerProvisioningOutcomeUncertain(error: unknown): boolean {
  let current = error;
  for (let depth = 0; depth < 8 && current instanceof Error; depth += 1) {
    if (current instanceof ProviderProvisioningOutcomeUncertainError) return true;
    current = current.cause;
  }
  return false;
}

export function validatedProviderProvisioningCleanupClaim(
  error: unknown,
  provider: Provider,
): ProviderProvisioningCleanupClaim | undefined {
  return validateProviderProvisioningCleanupClaim(
    providerProvisioningCleanupClaim(error),
    provider,
  );
}

export function validateProviderProvisioningCleanupClaim(
  claim: ProviderProvisioningCleanupClaim | undefined,
  provider: Provider,
): ProviderProvisioningCleanupClaim | undefined {
  if (
    !claim ||
    claim.provider !== provider ||
    typeof claim.cloudID !== "string" ||
    claim.cloudID !== claim.cloudID.trim() ||
    !claim.cloudID
  ) {
    return undefined;
  }
  switch (claim.provider) {
    case "aws":
      return nonEmptyClaimValue(claim.region) ? claim : undefined;
    case "azure":
      return nonEmptyClaimValue(claim.region) && validAzureProviderScope(claim.providerScope)
        ? claim
        : undefined;
    case "gcp":
      return nonEmptyClaimValue(claim.region) &&
        nonEmptyClaimValue(claim.providerProject) &&
        canonicalNumericClaimValue(claim.providerResourceID)
        ? claim
        : undefined;
    case "daytona":
      // Daytona accepts names at its ID route too; only a returned UUID in the
      // original allocation context grants managed cleanup authority.
      return /^[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}$/i.test(claim.cloudID) &&
        /^daytona:context:v1:[a-f0-9]{64}$/.test(claim.providerScope ?? "")
        ? claim
        : undefined;
  }
}

function nonEmptyClaimValue(value: string | undefined): boolean {
  return Boolean(value && value === value.trim());
}

function canonicalNumericClaimValue(value: string | undefined): boolean {
  return Boolean(value && value === value.trim() && /^[0-9]+$/.test(value));
}

function validAzureProviderScope(value: string | undefined): boolean {
  return /^\/subscriptions\/[^/]+\/resourceGroups\/[^/]+$/i.test(value ?? "");
}
