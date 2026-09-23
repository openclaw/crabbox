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
