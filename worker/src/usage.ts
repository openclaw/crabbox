import type { Env, LeaseRecord, Provider } from "./types";

export interface LeaseCost {
  hourlyUSD: number;
  maxUSD: number;
}

export interface CostLimits {
  maxActiveLeases: number;
  maxActiveLeasesPerOwner: number;
  maxActiveLeasesPerOrg: number;
  capacityAdminOwners: string[];
  maxActiveLeasesPerCapacityAdmin: number;
  maxMonthlyUSD: number;
  maxMonthlyUSDPerOwner: number;
  maxMonthlyUSDPerOrg: number;
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
}

interface UsageAccumulator {
  leases: number;
  activeLeases: number;
  runtimeSeconds: number;
  estimatedUSD: number;
  reservedUSD: number;
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

export function requestOrg(request: Request, env: Pick<Env, "CRABBOX_DEFAULT_ORG">): string {
  const header = request.headers.get("x-crabbox-org")?.trim();
  if (header) {
    return sanitizeOrg(header);
  }
  if (env.CRABBOX_DEFAULT_ORG) {
    return sanitizeOrg(env.CRABBOX_DEFAULT_ORG);
  }
  return "unknown";
}

export function leaseCost(
  env: Pick<Env, "CRABBOX_COST_RATES_JSON">,
  provider: Provider,
  serverType: string,
  ttlSeconds: number,
  providerHourlyUSD?: number,
): LeaseCost {
  const hourlyUSD = hourlyRateUSD(env, provider, serverType, providerHourlyUSD);
  return {
    hourlyUSD,
    maxUSD: roundUSD((Math.max(1, ttlSeconds) / 3600) * hourlyUSD),
  };
}

export function costLimits(env: Env): CostLimits {
  return {
    maxActiveLeases: envInt(env.CRABBOX_MAX_ACTIVE_LEASES),
    maxActiveLeasesPerOwner: envInt(env.CRABBOX_MAX_ACTIVE_LEASES_PER_OWNER),
    maxActiveLeasesPerOrg: envInt(env.CRABBOX_MAX_ACTIVE_LEASES_PER_ORG),
    capacityAdminOwners: envList(env.CRABBOX_CAPACITY_ADMIN_OWNERS).map((owner) =>
      owner.toLowerCase(),
    ),
    maxActiveLeasesPerCapacityAdmin: envInt(env.CRABBOX_MAX_ACTIVE_LEASES_PER_CAPACITY_ADMIN),
    maxMonthlyUSD: envFloat(env.CRABBOX_MAX_MONTHLY_USD),
    maxMonthlyUSDPerOwner: envFloat(env.CRABBOX_MAX_MONTHLY_USD_PER_OWNER),
    maxMonthlyUSDPerOrg: envFloat(env.CRABBOX_MAX_MONTHLY_USD_PER_ORG),
  };
}

export function enforceCostLimits(
  leases: LeaseRecord[],
  candidate: LeaseRecord,
  limits: CostLimits,
  now: Date,
): string {
  const managedLeases = leases.filter(isManagedLease);
  const active = managedLeases.filter((lease) => isActiveLease(lease, now));
  const ownerActive = active.filter((lease) => lease.owner === candidate.owner);
  const orgActive = active.filter((lease) => lease.org === candidate.org);
  if (limits.maxActiveLeases > 0 && active.length + 1 > limits.maxActiveLeases) {
    return `active lease limit exceeded: ${active.length + 1}/${limits.maxActiveLeases}`;
  }
  const ownerLimit = activeLeaseLimitForOwner(limits, candidate.owner);
  if (ownerLimit > 0 && ownerActive.length + 1 > ownerLimit) {
    return `active lease limit for owner exceeded: ${ownerActive.length + 1}/${ownerLimit}`;
  }
  if (limits.maxActiveLeasesPerOrg > 0 && orgActive.length + 1 > limits.maxActiveLeasesPerOrg) {
    return `active lease limit for org exceeded: ${orgActive.length + 1}/${limits.maxActiveLeasesPerOrg}`;
  }

  const month = monthKey(now);
  const allUsage = usageSummary(managedLeases, { scope: "all", month }, now);
  const ownerUsage = usageSummary(
    managedLeases,
    { scope: "user", owner: candidate.owner, month },
    now,
  );
  const orgUsage = usageSummary(managedLeases, { scope: "org", org: candidate.org, month }, now);
  if (overBudget(allUsage.reservedUSD + candidate.maxEstimatedUSD, limits.maxMonthlyUSD)) {
    return `monthly budget exceeded: ${formatUSD(allUsage.reservedUSD + candidate.maxEstimatedUSD)}/${formatUSD(limits.maxMonthlyUSD)}`;
  }
  if (
    overBudget(ownerUsage.reservedUSD + candidate.maxEstimatedUSD, limits.maxMonthlyUSDPerOwner)
  ) {
    return `monthly budget for owner exceeded: ${formatUSD(ownerUsage.reservedUSD + candidate.maxEstimatedUSD)}/${formatUSD(limits.maxMonthlyUSDPerOwner)}`;
  }
  if (overBudget(orgUsage.reservedUSD + candidate.maxEstimatedUSD, limits.maxMonthlyUSDPerOrg)) {
    return `monthly budget for org exceeded: ${formatUSD(orgUsage.reservedUSD + candidate.maxEstimatedUSD)}/${formatUSD(limits.maxMonthlyUSDPerOrg)}`;
  }
  return "";
}

export function usageSummary(leases: LeaseRecord[], filter: UsageFilter, now: Date): UsageSummary {
  const selected = leases.filter(
    (lease) => isManagedLease(lease) && leaseMatchesUsageFilter(lease, filter),
  );
  const total = newAccumulator();
  const byOwner = new Map<string, UsageAccumulator>();
  const byOrg = new Map<string, UsageAccumulator>();
  const byProvider = new Map<string, UsageAccumulator>();
  const byServerType = new Map<string, UsageAccumulator>();
  for (const lease of selected) {
    const item = leaseUsage(lease, now);
    addUsage(total, item);
    addUsage(mapAccumulator(byOwner, lease.owner || "unknown"), item);
    addUsage(mapAccumulator(byOrg, lease.org || "unknown"), item);
    addUsage(mapAccumulator(byProvider, lease.provider), item);
    addUsage(mapAccumulator(byServerType, lease.serverType || "unknown"), item);
  }
  const summary: UsageSummary = {
    month: filter.month,
    scope: filter.scope,
    ...finalize(total),
    byOwner: finalizeGroups(byOwner),
    byOrg: finalizeGroups(byOrg),
    byProvider: finalizeGroups(byProvider),
    byServerType: finalizeGroups(byServerType),
  };
  if (filter.owner) {
    summary.owner = filter.owner;
  }
  if (filter.org) {
    summary.org = filter.org;
  }
  return summary;
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
    return !filter.owner || lease.owner === filter.owner;
  }
  if (filter.scope === "org") {
    return !filter.org || lease.org === filter.org;
  }
  return true;
}

function leaseUsage(lease: LeaseRecord, now: Date): UsageAccumulator {
  const created = parseTime(lease.createdAt, now);
  const ended = parseTime(lease.endedAt || lease.releasedAt || "", now);
  const stop = isLiveLease(lease) ? now : ended;
  const runtimeSeconds = Math.max(0, Math.trunc((stop.getTime() - created.getTime()) / 1000));
  const estimatedUSD = roundUSD((runtimeSeconds / 3600) * (lease.estimatedHourlyUSD || 0));
  return {
    leases: 1,
    activeLeases: isActiveLease(lease, now) ? 1 : 0,
    runtimeSeconds,
    estimatedUSD,
    reservedUSD: roundUSD(lease.maxEstimatedUSD || estimatedUSD),
  };
}

function isActiveLease(lease: LeaseRecord, now: Date): boolean {
  return isLiveLease(lease) && Date.parse(lease.expiresAt) > now.getTime();
}

function isLiveLease(lease: LeaseRecord): boolean {
  return lease.state === "active" || lease.state === "provisioning";
}

function isManagedLease(lease: LeaseRecord): boolean {
  return lease.lifecycle !== "registered";
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
  };
}

function addUsage(total: UsageAccumulator, item: UsageAccumulator): void {
  total.leases += item.leases;
  total.activeLeases += item.activeLeases;
  total.runtimeSeconds += item.runtimeSeconds;
  total.estimatedUSD = roundUSD(total.estimatedUSD + item.estimatedUSD);
  total.reservedUSD = roundUSD(total.reservedUSD + item.reservedUSD);
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

function activeLeaseLimitForOwner(limits: CostLimits, owner: string): number {
  if (
    limits.maxActiveLeasesPerCapacityAdmin > 0 &&
    limits.capacityAdminOwners.includes(owner.toLowerCase())
  ) {
    return Math.max(limits.maxActiveLeasesPerOwner, limits.maxActiveLeasesPerCapacityAdmin);
  }
  return limits.maxActiveLeasesPerOwner;
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

function sanitizeOrg(value: string): string {
  return value.replaceAll(/[^a-zA-Z0-9_.@-]/g, "_").slice(0, 63) || "unknown";
}

function roundUSD(value: number): number {
  return Math.round(value * 100) / 100;
}

function formatUSD(value: number): string {
  return `$${value.toFixed(2)}`;
}
