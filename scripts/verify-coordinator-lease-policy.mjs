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

function withoutOwnerMaximum(policy, owner) {
  const copy = structuredClone(policy);
  if (Object.hasOwn(copy.owners ?? {}, owner)) {
    delete copy.owners[owner].maxTTLSeconds;
    if (Object.keys(copy.owners[owner]).length === 0) delete copy.owners[owner];
  }
  for (const group of ["owners", "orgs"]) {
    if (copy[group] && Object.keys(copy[group]).length === 0) delete copy[group];
  }
  return copy;
}

export async function verifyCoordinatorLeasePolicy({
  accountId,
  apiToken,
  owner,
  desiredOverrides,
  fetchImpl = fetch,
}) {
  if (!/^[a-f0-9]{32}$/.test(accountId ?? "") || !apiToken || !owner) {
    throw new Error("Set the Cloudflare account, API token, and policy owner before verification.");
  }
  const desired = parseOverrides(desiredOverrides, "Source");
  if (
    !Object.hasOwn(desired.owners ?? {}, owner) ||
    !Number.isInteger(desired.owners[owner].maxTTLSeconds)
  ) {
    throw new Error(
      "Source policy must declare the named owner's maximum TTL; deployment stopped.",
    );
  }

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
  if (bindings.length === 0) return;
  if (
    bindings.length !== 1 ||
    bindings[0].type !== "plain_text" ||
    typeof bindings[0].text !== "string"
  ) {
    throw new Error("Live lease policy is not one readable plaintext binding; deployment stopped.");
  }
  const live = parseOverrides(bindings[0].text, "Live");
  if (!isDeepStrictEqual(withoutOwnerMaximum(live, owner), withoutOwnerMaximum(desired, owner))) {
    throw new Error(
      "Live lease policy contains other entries or fields not preserved in source; reconcile the complete override document before deploying.",
    );
  }
}

if (import.meta.main) {
  try {
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
    await verifyCoordinatorLeasePolicy({
      accountId: process.env.CLOUDFLARE_ACCOUNT_ID,
      apiToken: process.env.CLOUDFLARE_API_TOKEN,
      owner: process.argv[2],
      desiredOverrides: config.vars[bindingName],
    });
    console.log("Verified coordinator lease policy preservation.");
  } catch (error) {
    console.error(`::error::${error.message}`);
    process.exitCode = 1;
  }
}
