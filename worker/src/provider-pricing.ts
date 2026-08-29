const pricingRequestTimeoutMs = 5_000;

export async function withPricingDeadline(
  quote: (signal: AbortSignal) => Promise<number | undefined>,
): Promise<number | undefined> {
  const controller = new AbortController();
  // Pricing is optional metadata. Abort its HTTP request/body and await settlement
  // before admission or activation continues with the existing fallback rate.
  const timeout = setTimeout(
    () => controller.abort(new DOMException("Provider pricing timed out", "TimeoutError")),
    pricingRequestTimeoutMs,
  );
  try {
    return await quote(controller.signal);
  } finally {
    clearTimeout(timeout);
  }
}
