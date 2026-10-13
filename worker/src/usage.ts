import { lifetimeOwnsCompute, lifetimeRuntimeMs } from "./lease-lifetime";
import { isRegisteredLease, leaseIsLive } from "./lease-state";
import { orgLabelForDisplay, orgMatchesForAccounting, orgMatchesForFilter } from "./org-identity";
import type { Env, LeaseRecord, Provider } from "./types";

export { requestOrg, requestOrgKey, requestOrgLabel } from "./org-identity";

export interface LeaseCost {
  hourlyUSD: number;
  maxUSD: number;
}

export interface CostLimits {
  maxActiveLeases: number;
  maxActiveLeasesPerOwner: number;
  maxActiveLeasesPerOrg: number;
  capacityAdminOwners: string[];
  sharedCapacityOverrideOwners: string[];
  maxActiveLeasesPerCapacityAdmin: number;
  maxMonthlyUSD: number;
  maxMonthlyUSDPerOwner: number;
  maxMonthlyUSDPerOrg: number;
}

export interface CostLimitUsage {
  owner: string;
  org: string;
  month: string;
  activeLeases: number;
  ownerActiveLeases: number;
  orgActiveLeases: number;
  committedUSD: number;
  ownerCommittedUSD: number;
  orgCommittedUSD: number;
}

export interface OwnerCapacity {
  owner: string;
  activeLeases: number;
  effectiveLimit: number;
  observedAt: string;
  fleet: { activeLeases: number; limit: number | null };
  org: { key: string; activeLeases: number; limit: number | null };
  admissible: boolean;
  blockedBy: "fleet" | "owner" | "org" | null;
}

export interface UsageFilter {
  scope: "user" | "org" | "all";
  owner?: string;
  org?: string;
  month: string;
}

export interface UsageSummary {
  month: string;
  scope: "user" | "org" | "all";
  owner?: string;
  org?: string;
  leases: number;
  activeLeases: number;
  runtimeSeconds: number;
  estimatedUSD: number;
  reservedUSD: number;
  committedUSD: number;
  byOwner: UsageGroup[];
  byOrg: UsageGroup[];
  byProvider: UsageGroup[];
  byServerType: UsageGroup[];
}

export interface UsageGroup {
  key: string;
  leases: number;
  activeLeases: number;
  runtimeSeconds: number;
  estimatedUSD: number;
  reservedUSD: number;
  committedUSD: number;
}

interface UsageAccumulator {
  leases: number;
  activeLeases: number;
  runtimeSeconds: number;
  estimatedUSD: number;
  reservedUSD: number;
  committedUSD: number;
}

const defaultHourlyUSD: Record<string, number> = {
  "hetzner:cx53": 0.08,
  "hetzner:cpx62": 0.18,
  "hetzner:ccx33": 0.36,
  "hetzner:ccx43": 0.54,
  "hetzner:ccx53": 0.72,
  "hetzner:ccx63": 1.08,
  "aws:c7a.4xlarge": 0.75,
  "aws:c7a.8xlarge": 1.5,
  "aws:c7a.12xlarge": 2.25,
  "aws:c7a.16xlarge": 3.0,
  "aws:c7a.24xlarge": 4.5,
  "aws:c7a.32xlarge": 6.0,
  "aws:c7a.48xlarge": 9.0,
};

export function leaseCost(
  env: Pick<Env, "CRABBOX_COST_RATES_JSON">,
  provider: Provider,
  serverType: string,
  ttlSeconds: number,
  providerHourlyUSD?: number,
): LeaseCost {
  const hourlyUSD = hourlyRateUSD(env, provider, serverType, providerHourlyUSD);
  return leaseCostForHourlyRate(ttlSeconds, hourlyUSD);
}

function leaseCostForHourlyRate(ttlSeconds: number, hourlyUSD: number): LeaseCost {
  return {
    hourlyUSD,
    maxUSD: roundUSD((Math.max(1, ttlSeconds) / 3600) * hourlyUSD),
  };
}

export function leaseExtensionReservationUSD(
  lease: Pick<LeaseRecord, "estimatedHourlyUSD" | "maxEstimatedUSD">,
  ttlSeconds: number,
): number {
  const cost = leaseCostForHourlyRate(ttlSeconds, lease.estimatedHourlyUSD);
  // Policy tightening must not release an existing reservation during extension.
  return Math.max(lease.maxEstimatedUSD, cost.maxUSD);
}

export function costLimits(env: Env): CostLimits {
  return {
    maxActiveLeases: envInt(env.CRABBOX_MAX_ACTIVE_LEASES),
    maxActiveLeasesPerOwner: envInt(env.CRABBOX_MAX_ACTIVE_LEASES_PER_OWNER),
    maxActiveLeasesPerOrg: envInt(env.CRABBOX_MAX_ACTIVE_LEASES_PER_ORG),
    capacityAdminOwners: [
      ...envList(env.CRABBOX_CAPACITY_ADMIN_OWNERS),
      ...envList(env.CRABBOX_DEPLOYMENT_CAPACITY_ADMIN_OWNERS),
    ].map((owner) => owner.toLowerCase()),
    sharedCapacityOverrideOwners: envList(env.CRABBOX_SHARED_CAPACITY_OVERRIDE_OWNERS).map(
      (owner) => owner.toLowerCase(),
    ),
    maxActiveLeasesPerCapacityAdmin: envInt(env.CRABBOX_MAX_ACTIVE_LEASES_PER_CAPACITY_ADMIN),
    maxMonthlyUSD: envFloat(env.CRABBOX_MAX_MONTHLY_USD),
    maxMonthlyUSDPerOwner: envFloat(env.CRABBOX_MAX_MONTHLY_USD_PER_OWNER),
    maxMonthlyUSDPerOrg: envFloat(env.CRABBOX_MAX_MONTHLY_USD_PER_ORG),
  };
}

export function createCostLimitUsage(
  candidate: Pick<LeaseRecord, "owner" | "org">,
  now: Date,
): CostLimitUsage {
  return {
    owner: candidate.owner,
    org: candidate.org,
    month: monthKey(now),
    activeLeases: 0,
    ownerActiveLeases: 0,
    orgActiveLeases: 0,
    committedUSD: 0,
    ownerCommittedUSD: 0,
    orgCommittedUSD: 0,
  };
}

export function addLeaseToCostLimitUsage(
  usage: CostLimitUsage,
  lease: LeaseRecord,
  now: Date,
): void {
  if (isRegisteredLease(lease)) {
    return;
  }
  // A live record still owns provider capacity after its heartbeat deadline
  // until cleanup commits a terminal state.
  if (leaseIsLive(lease) || lifetimeOwnsCompute(lease)) {
    usage.activeLeases += 1;
    if (lease.owner === usage.owner) {
      usage.ownerActiveLeases += 1;
    }
    if (orgMatchesForAccounting(lease.org, usage.org)) {
      usage.orgActiveLeases += 1;
    }
  }
  // A live lease still reserves provider spend in the active budget window,
  // even when its creation month has rolled over.
  if (!leaseReservesBudget(lease) && monthKey(new Date(lease.createdAt)) !== usage.month) {
    return;
  }
  const committedUSD = leaseUsage(lease, now).committedUSD;
  usage.committedUSD = roundUSD(usage.committedUSD + committedUSD);
  if (lease.owner === usage.owner) {
    usage.ownerCommittedUSD = roundUSD(usage.ownerCommittedUSD + committedUSD);
  }
  if (orgMatchesForAccounting(lease.org, usage.org)) {
    usage.orgCommittedUSD = roundUSD(usage.orgCommittedUSD + committedUSD);
  }
}

export function activeLeaseLimitBlocker(
  usage: CostLimitUsage,
  limits: ReturnType<typeof activeLeaseLimitsForOwner>,
): OwnerCapacity["blockedBy"] {
  if (limits.fleet > 0 && usage.activeLeases + 1 > limits.fleet) {
    return "fleet";
  }
  if (limits.owner > 0 && usage.ownerActiveLeases + 1 > limits.owner) {
    return "owner";
  }
  if (limits.org > 0 && usage.orgActiveLeases + 1 > limits.org) {
    return "org";
  }
  return null;
}

export function enforceCostLimitUsage(
  usage: CostLimitUsage,
  candidate: LeaseRecord,
  limits: CostLimits,
): string {
  const activeLimits = activeLeaseLimitsForOwner(limits, candidate.owner);
  switch (activeLeaseLimitBlocker(usage, activeLimits)) {
    case "fleet":
      return `fleet active lease limit exceeded: ${usage.activeLeases + 1}/${activeLimits.fleet}`;
    case "owner":
      return `active lease limit for owner exceeded: ${usage.ownerActiveLeases + 1}/${activeLimits.owner}`;
    case "org":
      return `active lease limit for org exceeded: ${usage.orgActiveLeases + 1}/${activeLimits.org}`;
  }

  return enforceBudgetLimitUsage(usage, candidate, limits);
}

export function enforceBudgetLimitUsage(
  usage: CostLimitUsage,
  candidate: Pick<LeaseRecord, "maxEstimatedUSD">,
  limits: CostLimits,
  existingReservationUSD?: number,
): string {
  const headroom = (used: number, limit: number): string =>
    existingReservationUSD === undefined
      ? ""
      : `; remaining headroom ${formatUSD(Math.max(0, limit - used - existingReservationUSD))}`;

  if (overBudget(usage.committedUSD + candidate.maxEstimatedUSD, limits.maxMonthlyUSD)) {
    return `monthly budget exceeded: ${formatUSD(usage.committedUSD + candidate.maxEstimatedUSD)}/${formatUSD(limits.maxMonthlyUSD)}${headroom(usage.committedUSD, limits.maxMonthlyUSD)}`;
  }
  if (
    overBudget(usage.ownerCommittedUSD + candidate.maxEstimatedUSD, limits.maxMonthlyUSDPerOwner)
  ) {
    return `monthly budget for owner exceeded: ${formatUSD(usage.ownerCommittedUSD + candidate.maxEstimatedUSD)}/${formatUSD(limits.maxMonthlyUSDPerOwner)}${headroom(usage.ownerCommittedUSD, limits.maxMonthlyUSDPerOwner)}`;
  }
  if (overBudget(usage.orgCommittedUSD + candidate.maxEstimatedUSD, limits.maxMonthlyUSDPerOrg)) {
    return `monthly budget for org exceeded: ${formatUSD(usage.orgCommittedUSD + candidate.maxEstimatedUSD)}/${formatUSD(limits.maxMonthlyUSDPerOrg)}${headroom(usage.orgCommittedUSD, limits.maxMonthlyUSDPerOrg)}`;
  }
  return "";
}

export function usageSummary(
  leases: Iterable<LeaseRecord>,
  filter: UsageFilter,
  now: Date,
): UsageSummary {
  const accumulator = createUsageSummary(filter, now);
  for (const lease of leases) accumulator.add(lease);
  return accumulator.finish();
}

export function createUsageSummary(filter: UsageFilter, now: Date) {
  const total = newAccumulator();
  const byOwner = new Map<string, UsageAccumulator>();
  const byOrg = new Map<string, UsageAccumulator>();
  const byProvider = new Map<string, UsageAccumulator>();
  const byServerType = new Map<string, UsageAccumulator>();
  const add = (lease: LeaseRecord) => {
    if (isRegisteredLease(lease) || !leaseMatchesUsageFilter(lease, filter)) return;
    const item = leaseUsage(lease, now);
    addUsage(total, item);
    addUsage(mapAccumulator(byOwner, lease.owner || "unknown"), item);
    addUsage(mapAccumulator(byOrg, lease.org || "unknown"), item);
    addUsage(mapAccumulator(byProvider, lease.provider), item);
    addUsage(mapAccumulator(byServerType, lease.serverType || "unknown"), item);
  };
  const finish = (): UsageSummary => {
    const summary: UsageSummary = {
      month: filter.month,
      scope: filter.scope,
      ...finalize(total),
      byOwner: finalizeGroups(byOwner),
      byOrg: finalizeGroups(byOrg).map((group) => ({
        ...group,
        key: orgLabelForDisplay(group.key),
      })),
      byProvider: finalizeGroups(byProvider),
      byServerType: finalizeGroups(byServerType),
    };
    if (filter.owner) {
      summary.owner = filter.owner;
    }
    if (filter.org) {
      summary.org = orgLabelForDisplay(filter.org);
    }
    return summary;
  };
  return { add, finish };
}

function hourlyRateUSD(
  env: Pick<Env, "CRABBOX_COST_RATES_JSON">,
  provider: Provider,
  serverType: string,
  providerHourlyUSD?: number,
): number {
  const key = `${provider}:${serverType}`;
  const override = rateOverrides(env)[key];
  if (override !== undefined && Number.isFinite(override) && override > 0) {
    return override;
  }
  if (
    providerHourlyUSD !== undefined &&
    Number.isFinite(providerHourlyUSD) &&
    providerHourlyUSD > 0
  ) {
    return providerHourlyUSD;
  }
  return defaultHourlyUSD[key] ?? (provider === "aws" ? 3 : 0.5);
}

function rateOverrides(env: Pick<Env, "CRABBOX_COST_RATES_JSON">): Record<string, number> {
  if (!env.CRABBOX_COST_RATES_JSON) {
    return {};
  }
  try {
    const parsed = JSON.parse(env.CRABBOX_COST_RATES_JSON) as Record<string, unknown>;
    return Object.fromEntries(
      Object.entries(parsed)
        .map(([key, value]) => [key, typeof value === "number" ? value : Number(value)] as const)
        .filter(([, value]) => Number.isFinite(value) && value > 0),
    );
  } catch {
    return {};
  }
}

function leaseMatchesUsageFilter(lease: LeaseRecord, filter: UsageFilter): boolean {
  if (monthKey(new Date(lease.createdAt)) !== filter.month) {
    return false;
  }
  if (filter.scope === "user") {
    return (
      (!filter.owner || lease.owner === filter.owner) &&
      (!filter.org || orgMatchesForFilter(lease.org, filter.org))
    );
  }
  if (filter.scope === "org") {
    return !filter.org || orgMatchesForFilter(lease.org, filter.org);
  }
  return true;
}

function leaseReservesBudget(lease: LeaseRecord): boolean {
  if (leaseIsLive(lease) || lifetimeOwnsCompute(lease)) return true;
  // Failed or canceled provisioning can still own an unresolved provider resource.
  if (lease.provisioningResourceMayExist && !lease.cleanupCompletedAt) return true;
  // Retained legacy resources may still run without enrollment in a lifetime policy.
  const stopped = lease.lifetimePolicy?.compute
    ? lease.lifetimePolicy.compute.state === "stopped"
    : Boolean(lease.lifetimePolicy?.stoppedAt);
  return Boolean(
    lease.releaseDeletesServer === false &&
    !lease.cleanupCompletedAt &&
    (lease.cloudID || lease.serverID || lease.providerResourceID) &&
    !stopped,
  );
}

function leaseUsage(lease: LeaseRecord, now: Date): UsageAccumulator {
  const live = leaseIsLive(lease) || lifetimeOwnsCompute(lease);
  const created = parseTime(lease.createdAt, now);
  // Legacy terminal rows may lack endedAt. Their last persisted observation,
  // rather than query time, bounds runtime until lifecycle evidence is available.
  const ended = parseTime(
    lease.endedAt || lease.releasedAt || lease.cleanupCompletedAt || lease.updatedAt,
    created,
  );
  const stopped = parseTime(lease.lifetimePolicy?.stoppedAt ?? "", ended);
  const stop = live ? now : stopped;
  const neverAllocated = !live && !lease.cloudID && lease.provisioningResourceMayExist === false;
  const runtimeSeconds = Math.max(
    0,
    Math.trunc(
      (lifetimeRuntimeMs(lease, stop.getTime()) ??
        (neverAllocated ? 0 : stop.getTime() - created.getTime())) / 1000,
    ),
  );
  const estimatedUSD = roundUSD((runtimeSeconds / 3600) * (lease.estimatedHourlyUSD || 0));
  const reservedUSD = roundUSD(lease.maxEstimatedUSD ?? estimatedUSD);
  // Failed admissions have timestamps and a reservation but no billable resource.
  const provisioned = Boolean(
    lease.cloudID || lease.serverID || lease.providerResourceID || lease.lifetimePolicy?.compute,
  );
  const committedUSD = leaseReservesBudget(lease)
    ? reservedUSD
    : provisioned
      ? Math.min(reservedUSD, estimatedUSD)
      : 0;
  return {
    leases: 1,
    activeLeases: live ? 1 : 0,
    runtimeSeconds,
    estimatedUSD,
    reservedUSD,
    committedUSD,
  };
}

function mapAccumulator(map: Map<string, UsageAccumulator>, key: string): UsageAccumulator {
  const existing = map.get(key);
  if (existing) {
    return existing;
  }
  const next = newAccumulator();
  map.set(key, next);
  return next;
}

function newAccumulator(): UsageAccumulator {
  return {
    leases: 0,
    activeLeases: 0,
    runtimeSeconds: 0,
    estimatedUSD: 0,
    reservedUSD: 0,
    committedUSD: 0,
  };
}

function addUsage(total: UsageAccumulator, item: UsageAccumulator): void {
  total.leases += item.leases;
  total.activeLeases += item.activeLeases;
  total.runtimeSeconds += item.runtimeSeconds;
  total.estimatedUSD = roundUSD(total.estimatedUSD + item.estimatedUSD);
  total.reservedUSD = roundUSD(total.reservedUSD + item.reservedUSD);
  total.committedUSD = roundUSD(total.committedUSD + item.committedUSD);
}

function finalize(
  group: UsageAccumulator,
): Omit<UsageSummary, "month" | "scope" | "byOwner" | "byOrg" | "byProvider" | "byServerType"> {
  return {
    leases: group.leases,
    activeLeases: group.activeLeases,
    runtimeSeconds: group.runtimeSeconds,
    estimatedUSD: roundUSD(group.estimatedUSD),
    reservedUSD: roundUSD(group.reservedUSD),
    committedUSD: roundUSD(group.committedUSD),
  };
}

function finalizeGroups(groups: Map<string, UsageAccumulator>): UsageGroup[] {
  return [...groups.entries()]
    .map(([key, value]) => ({ key, ...finalize(value) }))
    .toSorted((a, b) => b.reservedUSD - a.reservedUSD || a.key.localeCompare(b.key));
}

function envInt(value: string | undefined): number {
  const parsed = Number.parseInt(value ?? "", 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : 0;
}

function envFloat(value: string | undefined): number {
  const parsed = Number.parseFloat(value ?? "");
  return Number.isFinite(parsed) && parsed > 0 ? parsed : 0;
}

function envList(value: string | undefined): string[] {
  return (value ?? "")
    .split(",")
    .map((item) => item.trim())
    .filter(Boolean);
}

export function activeLeaseLimitsForOwner(limits: CostLimits, owner: string) {
  const capacityAdmin =
    limits.maxActiveLeasesPerCapacityAdmin > 0 &&
    limits.capacityAdminOwners.includes(owner.toLowerCase());
  const sharedOverride =
    capacityAdmin && limits.sharedCapacityOverrideOwners.includes(owner.toLowerCase());
  return {
    fleet: sharedOverride ? 0 : limits.maxActiveLeases,
    owner: capacityAdmin
      ? Math.max(limits.maxActiveLeasesPerOwner, limits.maxActiveLeasesPerCapacityAdmin)
      : limits.maxActiveLeasesPerOwner,
    org: sharedOverride ? 0 : limits.maxActiveLeasesPerOrg,
  };
}

function overBudget(value: number, limit: number): boolean {
  return limit > 0 && value > limit;
}

function parseTime(value: string, fallback: Date): Date {
  const parsed = Date.parse(value);
  return Number.isFinite(parsed) ? new Date(parsed) : fallback;
}

function monthKey(date: Date): string {
  return date.toISOString().slice(0, 7);
}

function roundUSD(value: number): number {
  return Math.round(value * 100) / 100;
}

function formatUSD(value: number): string {
  return `$${value.toFixed(2)}`;
}
