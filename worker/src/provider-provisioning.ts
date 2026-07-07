import type { Provider } from "./types";

export interface ProviderProvisioningCleanupClaim {
  provider: Extract<Provider, "aws" | "azure" | "gcp" | "daytona">;
  cloudID: string;
  region?: string;
  providerProject?: string;
  providerScope?: string;
  serverID?: number;
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
  const claim = providerProvisioningCleanupClaim(error);
  if (
    !claim ||
    claim.provider !== provider ||
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
      return claim;
  }
}

function nonEmptyClaimValue(value: string | undefined): boolean {
  return Boolean(value && value === value.trim());
}

function validAzureProviderScope(value: string | undefined): boolean {
  return /^\/subscriptions\/[^/]+\/resourceGroups\/[^/]+$/i.test(value ?? "");
}
