const pricingTimeoutMs = 5_000;

export interface PricingFallback {
  reason: "permission_denied" | "timeout" | "quote_unavailable" | "quote_error";
  message: string;
}

export class ProviderPriceUnavailableError extends Error {}

export function pricingFallback(error: unknown): PricingFallback {
  // Do not expose provider response bodies, credential details, or signed URLs.
  const message = error instanceof Error ? error.message : String(error);
  if (
    /AccessDenied|UnauthorizedOperation|Forbidden|permission denied|http (401|403)\b/i.test(message)
  ) {
    return {
      reason: "permission_denied",
      message: "provider pricing permission denied; check the provider IAM policy",
    };
  }
  if (error instanceof Error && (error.name === "TimeoutError" || error.name === "AbortError")) {
    return { reason: "timeout", message: "provider pricing request timed out" };
  }
  if (error instanceof ProviderPriceUnavailableError) {
    return { reason: "quote_unavailable", message: "provider returned no valid price" };
  }
  return { reason: "quote_error", message: "provider pricing request failed" };
}

export async function withPricingDeadline(
  quote: (signal: AbortSignal) => Promise<number | undefined>,
): Promise<number | undefined> {
  const controller = new AbortController();
  let timer: ReturnType<typeof setTimeout> | undefined;
  const expired = new Promise<never>((_, reject) => {
    timer = setTimeout(() => {
      const error = new DOMException("Provider pricing timed out", "TimeoutError");
      reject(error);
      controller.abort(error);
    }, pricingTimeoutMs);
  });
  try {
    // Credential resolution and qualification RPCs may not support cancellation.
    return await Promise.race([quote(controller.signal), expired]);
  } finally {
    if (timer !== undefined) clearTimeout(timer);
  }
}

const priceCacheTTL = 5 * 60_000;
const maxCachedPrices = 256;
const priceCaches = new WeakMap<object, Map<string, { hourlyUSD: number; expiresAt: number }>>();

// Prices are advisory admission estimates. Provider adapters supply all quote
// dimensions. Let the caller record failures before applying a fallback.
export async function cachedProviderPrice(
  scope: object,
  dimensions: readonly string[],
  quote: () => Promise<number | undefined>,
): Promise<number | undefined> {
  let cache = priceCaches.get(scope);
  if (!cache) {
    cache = new Map();
    priceCaches.set(scope, cache);
  }
  const key = JSON.stringify(dimensions);
  const cached = cache.get(key);
  if (cached && cached.expiresAt > Date.now()) return cached.hourlyUSD;
  cache.delete(key);
  const hourlyUSD = await quote();
  if (hourlyUSD === undefined || !Number.isFinite(hourlyUSD) || hourlyUSD <= 0)
    throw new ProviderPriceUnavailableError("provider returned no valid price");
  if (cache.size >= maxCachedPrices) cache.delete(cache.keys().next().value!);
  cache.set(key, { hourlyUSD, expiresAt: Date.now() + priceCacheTTL });
  return hourlyUSD;
}
