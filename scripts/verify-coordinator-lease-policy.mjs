import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";
import { isDeepStrictEqual } from "node:util";

const bindingName = "CRABBOX_LEASE_POLICY_OVERRIDES";
const isRecord = (value) => value !== null && typeof value === "object" && !Array.isArray(value);

function parseOverrides(text, label) {
  let value;
  try {
    value = JSON.parse(text);
  } catch {
    throw new Error(`${label} lease policy is not readable JSON; deployment stopped.`);
  }
  if (
    !isRecord(value) ||
    ["owners", "orgs"].some(
      (group) =>
        Object.hasOwn(value, group) &&
        (!isRecord(value[group]) || Object.values(value[group]).some((entry) => !isRecord(entry))),
    )
  ) {
    throw new Error(`${label} lease policy has an invalid document shape; deployment stopped.`);
  }
  return value;
}

function normalizeOverrides(policy) {
  const copy = structuredClone(policy);
  for (const group of ["owners", "orgs"]) {
    if (copy[group] && Object.keys(copy[group]).length === 0) delete copy[group];
  }
  return copy;
}

function differenceSummary(live, desired) {
  const differences = [];
  for (const group of [...new Set([...Object.keys(live), ...Object.keys(desired)])].sort()) {
    const liveGroup = Object.hasOwn(live, group) ? live[group] : undefined;
    const desiredGroup = Object.hasOwn(desired, group) ? desired[group] : undefined;
    if (isDeepStrictEqual(liveGroup, desiredGroup)) continue;
    const start = differences.length;
    if (isRecord(liveGroup ?? {}) && isRecord(desiredGroup ?? {})) {
      for (const key of [
        ...new Set([...Object.keys(liveGroup ?? {}), ...Object.keys(desiredGroup ?? {})]),
      ].sort()) {
        const liveValue = Object.hasOwn(liveGroup ?? {}, key) ? liveGroup[key] : undefined;
        const desiredValue = Object.hasOwn(desiredGroup ?? {}, key) ? desiredGroup[key] : undefined;
        if (!isDeepStrictEqual(liveValue, desiredValue)) {
          differences.push(`${JSON.stringify(group)}[${JSON.stringify(key)}]`);
        }
      }
    }
    if (differences.length === start) differences.push(JSON.stringify(group));
  }
  return `Live and source lease policy differ at: ${differences.join(", ")}.`;
}

export async function verifyCoordinatorLeasePolicy({
  accountId,
  apiToken,
  desiredOverrides,
  acknowledgeReplacement = false,
  fetchImpl = fetch,
}) {
  if (!/^[a-f0-9]{32}$/.test(accountId ?? "") || !apiToken) {
    throw new Error("Set the Cloudflare account and API token before verification.");
  }
  const desired = normalizeOverrides(
    parseOverrides(desiredOverrides === undefined ? "{}" : desiredOverrides, "Source"),
  );

  let response;
  try {
    response = await fetchImpl(
      `https://api.cloudflare.com/client/v4/accounts/${accountId}/workers/scripts/crabbox-coordinator/settings`,
      {
        headers: { authorization: `Bearer ${apiToken}` },
        redirect: "error",
        cache: "no-store",
        signal: AbortSignal.timeout(10_000),
      },
    );
  } catch {
    throw new Error("Cloudflare Worker settings request failed; deployment stopped.");
  }
  if (!response.ok) {
    throw new Error(`Cloudflare Worker settings request failed (HTTP ${response.status}).`);
  }
  let data;
  try {
    data = await response.json();
  } catch {
    throw new Error("Cloudflare Worker settings returned invalid JSON; deployment stopped.");
  }
  if (data?.success !== true || !Array.isArray(data.result?.bindings)) {
    throw new Error(
      "Cloudflare Worker settings did not confirm binding metadata; deployment stopped.",
    );
  }
  const bindings = data.result.bindings.filter((binding) => binding?.name === bindingName);
  if (
    bindings.length > 1 ||
    (bindings.length === 1 &&
      (bindings[0].type !== "plain_text" || typeof bindings[0].text !== "string"))
  ) {
    throw new Error("Live lease policy is not one readable plaintext binding; deployment stopped.");
  }
  const live = normalizeOverrides(parseOverrides(bindings[0]?.text ?? "{}", "Live"));
  if (!isDeepStrictEqual(live, desired)) {
    const summary = differenceSummary(live, desired);
    if (!acknowledgeReplacement) {
      throw new Error(
        `${summary} Reconcile the complete override document or set CRABBOX_ACKNOWLEDGE_LEASE_POLICY_REPLACEMENT=1 for an intentional replacement; deployment stopped.`,
      );
    }
    return summary;
  }
}

if (import.meta.main) {
  try {
    if (process.argv.length > 2) {
      throw new Error("Lease policy verification takes no arguments; deployment stopped.");
    }
    let config;
    try {
      const require = createRequire(new URL("../worker/package.json", import.meta.url));
      const { unstable_readConfig } = require("wrangler");
      config = unstable_readConfig(
        { config: fileURLToPath(new URL("../worker/wrangler.jsonc", import.meta.url)) },
        { hideWarnings: true },
      );
    } catch {
      throw new Error("Unable to read the production Wrangler configuration; deployment stopped.");
    }
    if (config.name !== "crabbox-coordinator") {
      throw new Error(
        "Production Wrangler configuration targets another Worker; deployment stopped.",
      );
    }
    const summary = await verifyCoordinatorLeasePolicy({
      accountId: process.env.CLOUDFLARE_ACCOUNT_ID,
      apiToken: process.env.CLOUDFLARE_API_TOKEN,
      desiredOverrides: config.vars?.[bindingName],
      acknowledgeReplacement: process.env.CRABBOX_ACKNOWLEDGE_LEASE_POLICY_REPLACEMENT === "1",
    });
    if (summary) console.log(`${summary} Intentional replacement acknowledged.`);
    console.log("Verified coordinator lease policy preservation.");
  } catch (error) {
    console.error(`::error::${error.message}`);
    process.exitCode = 1;
  }
}
