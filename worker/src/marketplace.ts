import {
  coordinatorProviders,
  isCoordinatorProvider,
  type Env,
  type Provider,
  type TargetOS,
} from "./types";
import { leaseCost } from "./usage";

export type MarketplaceStrategy = "cheapest" | "balanced" | "provider-default";

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

  const ttlSeconds = positiveInt(input.ttlSeconds, 3600, 30 * 24 * 60 * 60);
  const className = nonEmpty(input.class) || "standard";
  const serverType = nonEmpty(input.serverType) || className;
  const target = quoteTarget(input.target);
  const strategy = quoteStrategy(input.strategy);
  const maxCredits = positiveNumber(input.maxCredits);
  const providers = quoteProviders(status.supportedProviders, input);
  const candidates = providers
    .map((provider) =>
      quoteCandidate(env, provider, target, className, serverType, ttlSeconds, maxCredits),
    )
    .toSorted((left, right) => candidateRank(left, right, strategy));
  const selected = candidates.find((candidate) => candidate.available);
  const warnings = quoteWarnings(input, status, selected);
  const quote: MarketplaceQuote = {
    id: marketplaceQuoteID(providers, className, serverType, ttlSeconds),
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
  const baseHourlyUSD = leaseCost(
    env,
    provider,
    serverType,
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
  if (status.requireCreditsForLeases) {
    warnings.push("credit enforcement is configured but not wired into lease creation yet");
  }
  return warnings;
}

function quoteProviders(supported: Provider[], input: MarketplaceQuoteRequest): Provider[] {
  const raw = input.providers?.length
    ? input.providers
    : input.provider && input.provider !== "auto"
      ? [input.provider]
      : supported;
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
  if (strategy === "cheapest" || strategy === "balanced" || strategy === "provider-default") {
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
  return left.credits - right.credits || left.provider.localeCompare(right.provider);
}

function marketplaceQuoteID(
  providers: Provider[],
  className: string,
  serverType: string,
  ttlSeconds: number,
): string {
  const source = `${providers.join(",")}:${className}:${serverType}:${ttlSeconds}`;
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
