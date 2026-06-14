import {
  coordinatorProviders,
  isCoordinatorProvider,
  type Env,
  type Provider,
  type TargetOS,
} from "./types";
import { leaseCost } from "./usage";

export type MarketplaceStrategy = "cheapest" | "balanced" | "weighted" | "provider-default";

export interface MarketplaceStatus {
  enabled: boolean;
  mode: "disabled" | "preview";
  currency: "USD";
  creditUnit: "usd";
  requireCreditsForLeases: boolean;
  supportedProviders: Provider[];
  features: {
    quotes: boolean;
    bidding: boolean;
    payments: boolean;
    creditLedger: boolean;
    leaseEnforcement: boolean;
  };
  settlement: {
    paymentProvider: string;
    ledgerProvider: string;
    providerSettlement: "external";
  };
  decisionsRequired: string[];
}

export interface MarketplaceQuoteRequest {
  provider?: Provider | "auto";
  providers?: string[];
  class?: string;
  serverType?: string;
  target?: TargetOS;
  ttlSeconds?: number;
  maxCredits?: number;
  strategy?: MarketplaceStrategy;
}

export interface MarketplaceQuoteCandidate {
  provider: Provider;
  target: TargetOS;
  class: string;
  serverType: string;
  priority: number;
  weight: number;
  ttlSeconds: number;
  costHourlyUSD: number;
  retailHourlyUSD: number;
  estimatedCostUSD: number;
  credits: number;
  marginUSD: number;
  routeKey: string;
  available: boolean;
  unavailableReason?: string;
  // fraction of traffic (0..1) this candidate would receive under weighted load balancing
  // among available candidates sharing the winning priority tier; only set for the "weighted"
  // strategy. Mirrors the active tier's share in routingPlan. Preview-only: no traffic is routed.
  routeShare?: number;
}

export interface MarketplaceRouteTierMember {
  provider: Provider;
  routeKey: string;
  weight: number;
  // fraction of the tier's traffic (0..1); shares within a tier sum to exactly 1
  routeShare: number;
}

export interface MarketplaceRouteTier {
  priority: number;
  // the single winning failover tier (the one that contains the selected candidate)
  active: boolean;
  members: MarketplaceRouteTierMember[];
}

export interface MarketplaceQuote {
  id: string;
  mode: "preview";
  currency: "USD";
  creditUnit: "usd";
  strategy: MarketplaceStrategy;
  ttlSeconds: number;
  candidates: MarketplaceQuoteCandidate[];
  selected?: MarketplaceQuoteCandidate;
  // ordered failover ladder (highest priority first), each tier with its weighted member split.
  // Preview-only and weighted-strategy only: it routes no traffic and moves no credits.
  routingPlan?: MarketplaceRouteTier[];
  warnings: string[];
}

interface MarketplaceRate {
  costHourlyUSD?: number;
  retailHourlyUSD?: number;
  markupBps?: number;
  priority?: number;
  weight?: number;
  enabled?: boolean;
}

type MarketplaceRateCard = Record<string, number | MarketplaceRate>;

const defaultMarketplaceDecisions = [
  "choose payment processor and customer identity boundary",
  "choose durable credit ledger and refund adjustment model",
  "define provider settlement, taxes, invoices, and chargeback ownership",
  "decide whether credit enforcement is advisory or mandatory for lease creation",
  "define routing groups with priority, weight, active flags, and compatibility guarantees",
  "decide whether unknown pricing data blocks credit-enforced lease creation",
];

export function marketplaceStatus(env: Env): MarketplaceStatus {
  const enabled = envBool(env.CRABBOX_MARKETPLACE_ENABLED);
  return {
    enabled,
    mode: enabled ? "preview" : "disabled",
    currency: "USD",
    creditUnit: "usd",
    requireCreditsForLeases: envBool(env.CRABBOX_MARKETPLACE_REQUIRE_CREDITS),
    supportedProviders: marketplaceProviders(env),
    features: {
      quotes: enabled,
      bidding: enabled && envBool(env.CRABBOX_MARKETPLACE_BIDDING_ENABLED),
      payments: false,
      creditLedger: false,
      leaseEnforcement: false,
    },
    settlement: {
      paymentProvider: nonEmpty(env.CRABBOX_MARKETPLACE_PAYMENT_PROVIDER) || "none",
      ledgerProvider: nonEmpty(env.CRABBOX_MARKETPLACE_LEDGER_PROVIDER) || "none",
      providerSettlement: "external",
    },
    decisionsRequired: defaultMarketplaceDecisions,
  };
}

export class MarketplaceInputError extends Error {
  constructor(
    message: string,
    readonly code: string,
  ) {
    super(message);
    this.name = "MarketplaceInputError";
  }
}

export function marketplaceQuote(env: Env, input: MarketplaceQuoteRequest): MarketplaceQuote {
  const status = marketplaceStatus(env);
  if (!status.enabled) {
    throw new MarketplaceInputError("marketplace preview is disabled", "marketplace_disabled");
  }

  // basic shape validation for untyped JSON input (readJson only casts)
  if (input && typeof input === "object") {
    if (input.providers !== undefined && !Array.isArray(input.providers)) {
      throw new MarketplaceInputError(
        "providers must be an array of provider names",
        "invalid_providers",
      );
    }
    if (input.class !== undefined && typeof input.class !== "string") {
      throw new MarketplaceInputError("class must be a string", "invalid_class");
    }
    if (input.serverType !== undefined && typeof input.serverType !== "string") {
      throw new MarketplaceInputError("serverType must be a string", "invalid_server_type");
    }
  } else if (input !== undefined) {
    throw new MarketplaceInputError("quote request must be an object", "invalid_request");
  }

  let ttlSeconds: number;
  if (input.ttlSeconds !== undefined) {
    if (!Number.isFinite(input.ttlSeconds) || input.ttlSeconds <= 0) {
      throw new MarketplaceInputError(
        "ttlSeconds must be a positive number of seconds",
        "invalid_ttl",
      );
    }
    ttlSeconds = positiveInt(input.ttlSeconds, 3600, 30 * 24 * 60 * 60);
  } else {
    ttlSeconds = 3600;
  }
  const className = nonEmpty(input.class) || "standard";
  const serverType = nonEmpty(input.serverType) || className;
  const target = quoteTarget(input.target);
  const strategy = quoteStrategy(input.strategy);
  const maxCredits = positiveNumber(input.maxCredits);
  if (input.maxCredits !== undefined && maxCredits === undefined) {
    throw new MarketplaceInputError("maxCredits must be a positive number", "invalid_max_credits");
  }
  const providers = quoteProviders(status.supportedProviders, input);
  const candidates = providers
    .map((provider) =>
      quoteCandidate(env, provider, target, className, serverType, ttlSeconds, maxCredits),
    )
    .toSorted((left, right) => candidateRank(left, right, strategy));
  const selected = candidates.find((candidate) => candidate.available);
  // Build the failover ladder once, then mirror the winning tier's shares onto the flat
  // candidate.routeShare so the two views are computed from a single source and cannot diverge.
  let routingPlan: MarketplaceRouteTier[] | undefined;
  if (strategy === "weighted" && selected) {
    routingPlan = buildRoutingPlan(candidates, selected.priority);
    const active = routingPlan.find((tier) => tier.active);
    if (active) {
      const shareByRoute = new Map(active.members.map((m) => [m.routeKey, m.routeShare]));
      for (const candidate of candidates) {
        const share = shareByRoute.get(candidate.routeKey);
        if (share !== undefined) {
          candidate.routeShare = share;
        }
      }
    }
  }
  const warnings = quoteWarnings(input, status, selected, strategy, candidates);
  const quote: MarketplaceQuote = {
    id: marketplaceQuoteID(providers, className, serverType, ttlSeconds, strategy, maxCredits),
    mode: "preview",
    currency: "USD",
    creditUnit: "usd",
    strategy,
    ttlSeconds,
    candidates,
    warnings,
  };
  if (selected) {
    quote.selected = selected;
  }
  if (routingPlan) {
    quote.routingPlan = routingPlan;
  }
  return quote;
}

function quoteCandidate(
  env: Env,
  provider: Provider,
  target: TargetOS,
  className: string,
  serverType: string,
  ttlSeconds: number,
  maxCredits?: number,
): MarketplaceQuoteCandidate {
  const rate = marketplaceRate(env, provider, className, serverType);
  // For leaseCost base fallback (no rate cost entry), never pass a class name as serverType
  // (classes are not concrete machine types in the lease cost table). Class-only quotes rely on
  // rate-card entries (e.g. "aws:beast") for accurate pricing; without them we fall back to generic.
  const leaseServerType = serverType === className ? "standard" : serverType;
  const baseHourlyUSD = leaseCost(
    env,
    provider,
    leaseServerType,
    ttlSeconds,
    rate?.costHourlyUSD,
  ).hourlyUSD;
  const markupBps = rate?.markupBps ?? envInt(env.CRABBOX_MARKETPLACE_MARKUP_BPS);
  const costHourlyUSD = roundUSD(rate?.costHourlyUSD ?? baseHourlyUSD);
  const retailHourlyUSD = roundUSD(
    rate?.retailHourlyUSD ?? Math.max(costHourlyUSD, costHourlyUSD * (1 + markupBps / 10000)),
  );
  const estimatedCostUSD = roundUSD((ttlSeconds / 3600) * costHourlyUSD);
  const credits = roundUSD((ttlSeconds / 3600) * retailHourlyUSD);
  const candidate: MarketplaceQuoteCandidate = {
    provider,
    target,
    class: className,
    serverType,
    priority: rate?.priority ?? 0,
    weight: rate?.weight ?? 1,
    ttlSeconds,
    costHourlyUSD,
    retailHourlyUSD,
    estimatedCostUSD,
    credits,
    marginUSD: roundUSD(credits - estimatedCostUSD),
    routeKey: `${provider}:${target}:${serverType}`,
    available: rate?.enabled !== false,
  };
  if (candidate.available && !providerSupportsTarget(provider, target)) {
    candidate.available = false;
    candidate.unavailableReason = "unsupported_target_for_provider";
  }
  if (candidate.available && maxCredits !== undefined && credits > maxCredits) {
    candidate.available = false;
    candidate.unavailableReason = "above_max_credits";
  } else if (!candidate.available && !candidate.unavailableReason) {
    candidate.unavailableReason = "disabled_by_rate_card";
  }
  return candidate;
}

function quoteWarnings(
  input: MarketplaceQuoteRequest,
  status: MarketplaceStatus,
  selected: MarketplaceQuoteCandidate | undefined,
  strategy: MarketplaceStrategy,
  candidates: MarketplaceQuoteCandidate[],
): string[] {
  const warnings = [
    "preview quote only; no payment is captured, no credits are reserved, and no provider is provisioned",
  ];
  if (
    !status.features.bidding &&
    (input.provider === "auto" || (input.providers?.length ?? 0) > 1)
  ) {
    warnings.push("bidding is not enabled; candidates are ranked for preview only");
  }
  if (!selected) {
    warnings.push("no candidate fits the requested credit ceiling");
  }
  if (strategy === "weighted" && selected) {
    const tier = candidates.filter(
      (candidate) => candidate.available && candidate.priority === selected.priority,
    ).length;
    if (tier > 1) {
      warnings.push(
        `weighted strategy previews the load-balancing split across ${tier} same-priority candidates; no traffic is routed in preview mode`,
      );
    }
  }
  if (status.requireCreditsForLeases) {
    warnings.push("credit enforcement is configured but not wired into lease creation yet");
  }
  return warnings;
}

// Build the failover ladder: available candidates grouped into priority tiers (highest priority
// first = failover order), each tier carrying its members and weighted load-balancing shares.
// This mirrors AI-gateway routing groups (priority = failover, weight = same-tier load balance) as a
// preview-only projection: it documents how traffic would be distributed, but routes nothing.
function buildRoutingPlan(
  candidates: MarketplaceQuoteCandidate[],
  activePriority: number,
): MarketplaceRouteTier[] {
  const byPriority = new Map<number, MarketplaceQuoteCandidate[]>();
  for (const candidate of candidates) {
    if (!candidate.available) {
      continue;
    }
    const tier = byPriority.get(candidate.priority);
    if (tier) {
      tier.push(candidate);
    } else {
      byPriority.set(candidate.priority, [candidate]);
    }
  }
  return [...byPriority.entries()]
    .toSorted(([leftPriority], [rightPriority]) => rightPriority - leftPriority)
    .map(([priority, tier]) => {
      const members = tier.toSorted(
        (left, right) =>
          right.weight - left.weight ||
          left.credits - right.credits ||
          left.provider.localeCompare(right.provider),
      );
      const shares = weightedShares(members.map((candidate) => candidate.weight));
      return {
        priority,
        active: priority === activePriority,
        members: members.map((candidate, index) => ({
          provider: candidate.provider,
          routeKey: candidate.routeKey,
          weight: candidate.weight,
          routeShare: shares[index] ?? 0,
        })),
      };
    });
}

// Largest-remainder allocation: split 1.0 across weights so the rounded 4-decimal shares sum to
// exactly 1 (independent rounding would drift, e.g. three equal weights -> 0.3333 x3 = 0.9999).
function weightedShares(weights: number[]): number[] {
  const total = weights.reduce((sum, weight) => sum + weight, 0);
  if (total <= 0) {
    return weights.map(() => 0);
  }
  const scaled = weights.map((weight) => (weight / total) * 10000);
  const base = scaled.map((value) => Math.floor(value));
  const residual = 10000 - base.reduce((sum, value) => sum + value, 0);
  // hand the leftover units to the largest fractional parts (stable: ties keep input order, so the
  // heavier/cheaper primary route -- already first within the tier -- absorbs the residual)
  const bump = new Set<number>(
    scaled
      .map((value, index) => ({ index, frac: value - Math.floor(value) }))
      .toSorted((left, right) => right.frac - left.frac)
      .slice(0, Math.max(0, residual))
      .map((entry) => entry.index),
  );
  return base.map((value, index) => (value + (bump.has(index) ? 1 : 0)) / 10000);
}

function quoteProviders(supported: Provider[], input: MarketplaceQuoteRequest): Provider[] {
  const raw = input.providers?.length
    ? input.providers
    : input.provider && input.provider !== "auto"
      ? [input.provider]
      : supported;
  const isExplicit = !!(input.providers?.length || (input.provider && input.provider !== "auto"));
  // intersect with the deployment allowlist (CRABBOX_MARKETPLACE_ALLOWED_PROVIDERS or default coordinator)
  // so that explicitly requested providers outside the list are rejected (prevents bypass of routing/billing policy)
  const supportedSet = new Set(supported);
  const filtered = raw.filter((p) => supportedSet.has(p as Provider));
  const providers = uniqueProviders(filtered);
  if (providers.length === 0) {
    throw new MarketplaceInputError(
      "no supported marketplace providers requested",
      "invalid_provider",
    );
  }
  if (isExplicit && providers.length < uniqueProviders(raw).length) {
    // any explicitly requested provider was dropped -> policy violation for the requestor
    throw new MarketplaceInputError(
      "one or more requested providers are not allowed by CRABBOX_MARKETPLACE_ALLOWED_PROVIDERS",
      "invalid_provider",
    );
  }
  return providers;
}

function quoteTarget(target: TargetOS | undefined): TargetOS {
  if (target === undefined) {
    return "linux";
  }
  if (target === "linux" || target === "macos" || target === "windows") {
    return target;
  }
  throw new MarketplaceInputError(`unsupported target ${target}`, "invalid_target");
}

function quoteStrategy(strategy: MarketplaceStrategy | undefined): MarketplaceStrategy {
  if (strategy === undefined) {
    return "cheapest";
  }
  if (
    strategy === "cheapest" ||
    strategy === "balanced" ||
    strategy === "weighted" ||
    strategy === "provider-default"
  ) {
    return strategy;
  }
  throw new MarketplaceInputError(`unsupported strategy ${strategy}`, "invalid_strategy");
}

function providerSupportsTarget(provider: Provider, target: TargetOS): boolean {
  if (target === "linux") return true;
  // Mirror documented/lease target compatibility for the quote preview (see lease config, provider backends, and docs).
  // macOS: only AWS today; Windows: AWS + Azure; Hetzner/GCP and others are Linux-only for brokered use.
  if (target === "macos") return provider === "aws";
  if (target === "windows") return provider === "aws" || provider === "azure";
  return false;
}

function marketplaceProviders(env: Env): Provider[] {
  const configured = splitList(env.CRABBOX_MARKETPLACE_ALLOWED_PROVIDERS);
  if (configured.length === 0) {
    return [...coordinatorProviders];
  }
  return uniqueProviders(configured);
}

function uniqueProviders(values: readonly string[]): Provider[] {
  const seen = new Set<Provider>();
  for (const value of values) {
    const provider = value.trim();
    if (isCoordinatorProvider(provider)) {
      seen.add(provider);
    }
  }
  return [...seen];
}

function marketplaceRate(
  env: Env,
  provider: Provider,
  className: string,
  serverType: string,
): MarketplaceRate | undefined {
  const card = marketplaceRateCard(env);
  const keys = [
    `${provider}:${serverType}`,
    `${provider}:${className}`,
    `${provider}:*`,
    `*:${serverType}`,
    `*:${className}`,
    "*",
  ];
  for (const key of keys) {
    const value = card[key];
    if (value === undefined) {
      continue;
    }
    if (typeof value === "number") {
      return { retailHourlyUSD: value };
    }
    return value;
  }
  return undefined;
}

function marketplaceRateCard(env: Env): MarketplaceRateCard {
  const raw = env.CRABBOX_MARKETPLACE_RATE_CARD_JSON;
  if (!raw) {
    return {};
  }
  try {
    const parsed = JSON.parse(raw) as Record<string, unknown>;
    return Object.fromEntries(
      Object.entries(parsed)
        .map(([key, value]) => [key, normalizeRateValue(value)] as const)
        .filter(
          (entry): entry is readonly [string, number | MarketplaceRate] => entry[1] !== undefined,
        ),
    );
  } catch {
    return {};
  }
}

function normalizeRateValue(value: unknown): number | MarketplaceRate | undefined {
  if (typeof value === "number" && Number.isFinite(value) && value > 0) {
    return value;
  }
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    return undefined;
  }
  return normalizeRate(value as Record<string, unknown>);
}

function normalizeRate(value: Record<string, unknown>): MarketplaceRate {
  const rate: MarketplaceRate = {};
  const costHourlyUSD = positiveNumber(value["costHourlyUSD"]);
  const retailHourlyUSD = positiveNumber(value["retailHourlyUSD"]);
  const markupBps =
    value["markupBps"] === undefined ? undefined : envInt(String(value["markupBps"]));
  const priority = value["priority"] === undefined ? undefined : envInt(String(value["priority"]));
  const weight = positiveNumber(value["weight"]);
  if (costHourlyUSD !== undefined) {
    rate.costHourlyUSD = costHourlyUSD;
  }
  if (retailHourlyUSD !== undefined) {
    rate.retailHourlyUSD = retailHourlyUSD;
  }
  if (markupBps !== undefined) {
    rate.markupBps = markupBps;
  }
  if (priority !== undefined) {
    rate.priority = priority;
  }
  if (weight !== undefined) {
    rate.weight = weight;
  }
  if (typeof value["enabled"] === "boolean") {
    rate.enabled = value["enabled"];
  }
  return rate;
}

function candidateRank(
  left: MarketplaceQuoteCandidate,
  right: MarketplaceQuoteCandidate,
  strategy: MarketplaceStrategy,
): number {
  if (left.available !== right.available) {
    return left.available ? -1 : 1;
  }
  if (left.priority !== right.priority) {
    return right.priority - left.priority;
  }
  if (strategy === "provider-default") {
    return 0;
  }
  if (strategy === "balanced") {
    return right.marginUSD - left.marginUSD || left.credits - right.credits;
  }
  if (strategy === "weighted") {
    // within a priority tier, prefer the heavier route (it absorbs the larger traffic share),
    // then fall back to cheapest for a stable, deterministic ordering
    return (
      right.weight - left.weight ||
      left.credits - right.credits ||
      left.provider.localeCompare(right.provider)
    );
  }
  return left.credits - right.credits || left.provider.localeCompare(right.provider);
}

function marketplaceQuoteID(
  providers: Provider[],
  className: string,
  serverType: string,
  ttlSeconds: number,
  strategy: MarketplaceStrategy,
  maxCredits: number | undefined,
): string {
  // fold in strategy and the credit ceiling so two requests that differ only by those (and thus
  // yield different selection / candidate ordering / routeShare) do not collide on the same id
  const source = `${providers.join(",")}:${className}:${serverType}:${ttlSeconds}:${strategy}:${maxCredits ?? ""}`;
  let hash = 0;
  for (let index = 0; index < source.length; index++) {
    hash = (hash * 31 + source.charCodeAt(index)) >>> 0;
  }
  return `mq_${hash.toString(16).padStart(8, "0")}`;
}

function splitList(value: string | undefined): string[] {
  return (value ?? "")
    .split(",")
    .map((item) => item.trim())
    .filter(Boolean);
}

function envBool(value: string | undefined): boolean {
  return ["1", "true", "yes", "on"].includes((value ?? "").trim().toLowerCase());
}

function envInt(value: string | undefined): number {
  const parsed = Number.parseInt(value ?? "", 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : 0;
}

function positiveInt(value: number | undefined, fallback: number, max: number): number {
  if (!Number.isFinite(value) || value === undefined || value <= 0) {
    return fallback;
  }
  // ceil to ensure fractional inputs (e.g. 0.5 from raw JSON) produce positive integer seconds
  // and never zero-credit quotes; CLI path already produces integers via parseDuration
  return Math.min(Math.max(1, Math.ceil(value)), max);
}

function positiveNumber(value: unknown): number | undefined {
  const parsed = typeof value === "number" ? value : Number(value);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : undefined;
}

function nonEmpty(value: string | undefined): string {
  return value?.trim() ?? "";
}

function roundUSD(value: number): number {
  return Math.round((value + Number.EPSILON) * 100) / 100;
}
