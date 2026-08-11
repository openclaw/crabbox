import type { Provider, ProviderMachine } from "./types";

export interface ProviderReconciliationQuarantine {
  fingerprint: string;
  firstObservedAt: string;
  lastObservedAt: string;
  eligibleAt: string;
  observations: number;
}

export interface ProviderReconciliationCircuit {
  consecutiveFailures: number;
  retryAt?: string;
  lastError?: string;
}

export type ProviderReconciliationObservation =
  | {
      action: "quarantine";
      quarantine: ProviderReconciliationQuarantine;
    }
  | {
      action: "release";
      quarantine: ProviderReconciliationQuarantine;
    };

export function providerReconciliationFingerprint(
  provider: Provider,
  scope: string,
  machine: Pick<ProviderMachine, "cloudID" | "id" | "name" | "labels" | "resourceIdentity">,
): string {
  const labels = machine.labels ?? {};
  return JSON.stringify({
    provider,
    scope,
    cloudID: machine.cloudID || String(machine.id),
    name: machine.name,
    lease: labels["lease"] ?? "",
    owner: labels["owner"] ?? "",
    providerLabel: labels["provider"] ?? "",
    slug: labels["slug"] ?? "",
    keep: labels["keep"] ?? "",
    createdAt: labels["created_at"] ?? "",
    expiresAt: labels["expires_at"] ?? "",
    ...(machine.resourceIdentity === undefined
      ? {}
      : { resourceIdentity: machine.resourceIdentity }),
  });
}

export function observeProviderReconciliationCandidate(input: {
  fingerprint: string;
  previous?: ProviderReconciliationQuarantine;
  now: number;
  quarantineSeconds: number;
}): ProviderReconciliationObservation {
  const observedAt = new Date(input.now).toISOString();
  const sameIdentity = input.previous?.fingerprint === input.fingerprint;
  const previousEligibleAt = Date.parse(input.previous?.eligibleAt ?? "");
  const observations = sameIdentity ? (input.previous?.observations ?? 0) + 1 : 1;
  const eligibleAt = sameIdentity
    ? input.previous!.eligibleAt
    : new Date(input.now + Math.max(0, input.quarantineSeconds) * 1000).toISOString();
  const quarantine: ProviderReconciliationQuarantine = {
    fingerprint: input.fingerprint,
    firstObservedAt: sameIdentity ? input.previous!.firstObservedAt : observedAt,
    lastObservedAt: observedAt,
    eligibleAt,
    observations,
  };
  const release =
    sameIdentity &&
    observations >= 2 &&
    Number.isFinite(previousEligibleAt) &&
    input.now >= previousEligibleAt;
  return {
    action: release ? "release" : "quarantine",
    quarantine,
  };
}

export function recordProviderReconciliationSuccess(): ProviderReconciliationCircuit {
  return { consecutiveFailures: 0 };
}

export function recordProviderReconciliationFailure(input: {
  previous?: ProviderReconciliationCircuit;
  now: number;
  error: string;
}): ProviderReconciliationCircuit {
  const consecutiveFailures = (input.previous?.consecutiveFailures ?? 0) + 1;
  const delayMs =
    consecutiveFailures < 3 ? 0 : Math.min(60, 5 * 3 ** (consecutiveFailures - 3)) * 60 * 1000;
  return {
    consecutiveFailures,
    ...(delayMs > 0 ? { retryAt: new Date(input.now + delayMs).toISOString() } : {}),
    lastError: input.error,
  };
}

export function providerReconciliationCircuitOpen(
  circuit: ProviderReconciliationCircuit | undefined,
  now: number,
): boolean {
  const retryAt = Date.parse(circuit?.retryAt ?? "");
  return Number.isFinite(retryAt) && now < retryAt;
}
