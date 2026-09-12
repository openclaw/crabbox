const pricingRequestTimeoutMs = 5_000;

export async function withPricingDeadline(
  quote: (signal: AbortSignal) => Promise<number | undefined>,
): Promise<number | undefined> {
  const controller = new AbortController();
  let timeout: ReturnType<typeof setTimeout> | undefined;
  const deadline = new Promise<never>((_resolve, reject) => {
    timeout = setTimeout(() => {
      const error = new DOMException("Provider pricing timed out", "TimeoutError");
      // Stop waiting even when SDK credentials or an authority RPC cannot be
      // canceled. The quote's signal prevents subsequent requests and retries.
      reject(error);
      controller.abort(error);
    }, pricingRequestTimeoutMs);
  });
  try {
    return await Promise.race([quote(controller.signal), deadline]);
  } finally {
    if (timeout !== undefined) clearTimeout(timeout);
  }
}
