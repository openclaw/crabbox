const pricingTimeoutMs = 5_000;

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
// dimensions; failures keep the existing fallback instead of rejecting admission.
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
  let hourlyUSD: number | undefined;
  try {
    hourlyUSD = await quote();
  } catch {
    return undefined;
  }
  if (hourlyUSD === undefined || !Number.isFinite(hourlyUSD) || hourlyUSD <= 0) return undefined;
  if (cache.size >= maxCachedPrices) cache.delete(cache.keys().next().value!);
  cache.set(key, { hourlyUSD, expiresAt: Date.now() + priceCacheTTL });
  return hourlyUSD;
}
