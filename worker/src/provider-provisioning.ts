import type { Provider } from "./types";

export interface ProviderProvisioningCleanupClaim {
  provider: Extract<Provider, "aws" | "azure" | "gcp" | "daytona">;
  cloudID: string;
  region?: string;
  providerProject?: string;
  providerScope?: string;
  serverID?: number;
}

// Unlike retryable cleanup failure, this requires identity or scope resolution before
// another provider operation can be authorized. It never establishes resource absence.
export class ProviderResourceUnresolvedError extends Error {
  override name = "ProviderResourceUnresolvedError";
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
      return nonEmptyClaimValue(claim.region) && nonEmptyClaimValue(claim.providerProject)
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

function validAzureProviderScope(value: string | undefined): boolean {
  return /^\/subscriptions\/[^/]+\/resourceGroups\/[^/]+$/i.test(value ?? "");
}
