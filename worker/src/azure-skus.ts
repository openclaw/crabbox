interface ResourceSKU {
  name?: string;
  resourceType?: string;
  locations?: string[];
  restrictions?: {
    type?: string;
    values?: string[];
    restrictionInfo?: { locations?: string[]; zones?: string[] };
  }[];
}

// Availability is a subscription policy hint, never proof of allocation rejection.
export class AzureSKUAvailability {
  private readonly regions = new Map<
    string,
    { expires: number; available: Map<string, boolean> }
  >();

  async get(
    subscription: string,
    region: string,
    token: () => Promise<string>,
    fetcher: typeof fetch,
  ): Promise<Map<string, boolean>> {
    const key = `${subscription.toLowerCase()}/${region.toLowerCase()}`;
    const cached = this.regions.get(key);
    if (cached && cached.expires > Date.now()) return cached.available;
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;
    const expired = new Promise<never>((_, reject) => {
      timer = setTimeout(() => {
        controller.abort();
        reject(new Error("Azure SKU preflight timed out"));
      }, 5_000);
    });
    try {
      const available = await Promise.race([
        this.load(subscription, region, token, fetcher, controller.signal),
        expired,
      ]);
      // Bound memory when one coordinator serves many region spellings.
      if (this.regions.size >= 64) this.regions.clear();
      this.regions.set(key, { expires: Date.now() + 5 * 60_000, available });
      return available;
    } catch {
      // Missing read permission, incomplete pages, and transport failures are unknown.
      return new Map();
    } finally {
      if (timer !== undefined) clearTimeout(timer);
    }
  }

  private async load(
    subscription: string,
    region: string,
    token: () => Promise<string>,
    fetcher: typeof fetch,
    signal: AbortSignal,
  ): Promise<Map<string, boolean>> {
    const authorization = `Bearer ${await token()}`;
    const root = new URL(
      `https://management.azure.com/subscriptions/${encodeURIComponent(subscription)}/providers/Microsoft.Compute/skus`,
    );
    root.searchParams.set("api-version", "2021-07-01");
    root.searchParams.set("$filter", `location eq '${region.replaceAll("'", "''")}'`);
    let next: string | undefined = root.href;
    const result = new Map<string, boolean>();
    for (let page = 0; next && page < 10; page += 1) {
      const url = new URL(next);
      if (
        url.origin !== root.origin ||
        url.pathname !== root.pathname ||
        url.username ||
        url.password ||
        url.hash
      )
        throw new Error("Invalid Azure SKU page");
      // oxlint-disable-next-line eslint/no-await-in-loop -- ARM pagination is sequential.
      const response = await fetcher(url.href, {
        headers: { authorization },
        signal,
        redirect: "error",
      });
      if (!response.ok) throw new Error("Azure SKU lookup unavailable");
      // oxlint-disable-next-line eslint/no-await-in-loop -- read each page before following its link.
      const body = (await response.json()) as { value?: ResourceSKU[]; nextLink?: string };
      if (!Array.isArray(body.value)) throw new Error("Invalid Azure SKU response");
      for (const sku of body.value) {
        if (!sku.name || sku.resourceType !== "virtualMachines") continue;
        const matches = (value: string) => value.toLowerCase() === region.toLowerCase();
        if (!sku.locations?.some(matches)) continue;
        // These creates are regional: a Zone restriction does not reject an unzoned VM.
        const restricted = sku.restrictions?.some(
          (restriction) =>
            restriction.type?.toLowerCase() === "location" &&
            [...(restriction.values ?? []), ...(restriction.restrictionInfo?.locations ?? [])].some(
              matches,
            ),
        );
        result.set(sku.name.toLowerCase(), !restricted);
      }
      next = body.nextLink;
    }
    if (next) throw new Error("Azure SKU page limit exceeded");
    return result;
  }
}
