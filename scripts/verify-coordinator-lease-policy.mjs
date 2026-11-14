import { createHash } from "node:crypto";
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";
import { isDeepStrictEqual } from "node:util";

import { isLeasePolicyOverrides } from "../worker/src/lease-policy-overrides.mjs";

export const bindingName = "CRABBOX_LEASE_POLICY_OVERRIDES";
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

export function normalizeOverrides(policy) {
  const copy = structuredClone(policy);
  for (const group of ["owners", "orgs"]) {
    if (copy[group] && Object.keys(copy[group]).length === 0) delete copy[group];
  }
  return copy;
}

function differenceSummary(live, desired) {
  const differences = [];
  for (const group of [...new Set([...Object.keys(live), ...Object.keys(desired)])].toSorted()) {
    const liveGroup = Object.hasOwn(live, group) ? live[group] : undefined;
    const desiredGroup = Object.hasOwn(desired, group) ? desired[group] : undefined;
    if (isDeepStrictEqual(liveGroup, desiredGroup)) continue;
    const start = differences.length;
    if (isRecord(liveGroup ?? {}) && isRecord(desiredGroup ?? {})) {
      for (const key of [
        ...new Set([...Object.keys(liveGroup ?? {}), ...Object.keys(desiredGroup ?? {})]),
      ].toSorted()) {
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

function assertAuthority({ accountId, apiToken }) {
  if (!/^[a-f0-9]{32}$/.test(accountId ?? "") || !apiToken)
    throw new Error("Set the Cloudflare account and API token before verification.");
}

export async function readCoordinatorJSON({ accountId, apiToken, fetchImpl = fetch }, suffix) {
  assertAuthority({ accountId, apiToken });
  let response;
  try {
    response = await fetchImpl(
      `https://api.cloudflare.com/client/v4/accounts/${accountId}/workers/scripts/crabbox-coordinator${suffix}`,
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
  if (data?.success !== true || !isRecord(data.result)) {
    throw new Error(
      "Cloudflare Worker settings did not confirm binding metadata; deployment stopped.",
    );
  }
  return data.result;
}

function policyBinding(allBindings) {
  if (!Array.isArray(allBindings))
    throw new Error(
      "Cloudflare Worker settings did not confirm binding metadata; deployment stopped.",
    );
  const bindings = allBindings.filter((binding) => binding?.name === bindingName);
  if (
    bindings.length > 1 ||
    (bindings.length === 1 &&
      (bindings[0].type !== "plain_text" || typeof bindings[0].text !== "string"))
  ) {
    throw new Error("Live lease policy is not one readable plaintext binding; deployment stopped.");
  }
  return {
    present: bindings.length === 1,
    document: parseOverrides(bindings[0]?.text ?? "{}", "Live"),
  };
}

export function policyDigest(policy) {
  const canonical = (value) => {
    if (!isRecord(value)) return JSON.stringify(value);
    return `{${Object.keys(value)
      .toSorted()
      .map((key) => `${JSON.stringify(key)}:${canonical(value[key])}`)
      .join(",")}}`;
  };
  return createHash("sha256")
    .update(canonical(normalizeOverrides(policy)))
    .digest("hex");
}

export async function readCoordinatorDeployment(options) {
  const data = await readCoordinatorJSON(options, "/deployments");
  const deployment = data.deployments?.[0];
  const versions = deployment?.versions;
  const uuid = (value) => typeof value === "string" && /^[0-9a-f-]{36}$/i.test(value);
  if (
    !uuid(deployment?.id) ||
    !Array.isArray(versions) ||
    versions.length !== 1 ||
    versions[0]?.percentage !== 100 ||
    !uuid(versions[0]?.version_id)
  )
    throw new Error("Expected one active coordinator version at 100%; deployment stopped.");
  return { deploymentId: deployment.id, versionId: versions[0].version_id };
}

// Bind readable policy to a stable serving deployment, not a staged version.
export async function readCoordinatorPolicySnapshot(options) {
  const deployment = await readCoordinatorDeployment(options);
  const settings = await readCoordinatorJSON(options, "/settings");
  const version = await readCoordinatorJSON(options, `/versions/${deployment.versionId}`);
  const policy = policyBinding(settings.bindings);
  const serving = policyBinding(version.resources?.bindings);
  const after = await readCoordinatorDeployment(options);
  if (!isDeepStrictEqual(deployment, after))
    throw new Error("Coordinator deployment changed during policy capture; deployment stopped.");
  if (!isDeepStrictEqual(normalizeOverrides(policy.document), normalizeOverrides(serving.document)))
    throw new Error("Settings and serving lease policy differ; deployment stopped.");
  return { ...deployment, ...policy };
}

export function assertPolicySnapshotUnchanged(before, after) {
  if (
    before.deploymentId !== after.deploymentId ||
    before.versionId !== after.versionId ||
    !isDeepStrictEqual(normalizeOverrides(before.document), normalizeOverrides(after.document))
  )
    throw new Error(
      "Live policy or coordinator deployment changed; capture and review again before retrying.",
    );
}

export async function verifyCoordinatorLeasePolicy({
  accountId,
  apiToken,
  desiredOverrides,
  acknowledgeReplacement = false,
  expectedSnapshot,
  fetchImpl = fetch,
}) {
  if (!/^[a-f0-9]{32}$/.test(accountId ?? "") || !apiToken) {
    throw new Error("Set the Cloudflare account and API token before verification.");
  }
  const source = parseOverrides(desiredOverrides === undefined ? "{}" : desiredOverrides, "Source");
  if (!isLeasePolicyOverrides(source)) {
    throw new Error(
      `Source lease policy is invalid; deployment stopped. Correct ${bindingName} in the reviewed deployment configuration: ` +
        "use only owners/orgs entries with defaultTTLSeconds, defaultIdleTimeoutSeconds, or maxTTLSeconds " +
        "as integers from 1 to 86400, and extension/autoExtend values of allow or deny. Then retry.",
    );
  }
  const desired = normalizeOverrides(source);

  const livePolicy = expectedSnapshot
    ? await readCoordinatorPolicySnapshot({ accountId, apiToken, fetchImpl })
    : policyBinding(
        (await readCoordinatorJSON({ accountId, apiToken, fetchImpl }, "/settings")).bindings,
      );
  if (expectedSnapshot) assertPolicySnapshotUnchanged(expectedSnapshot, livePolicy);
  // Keep readable, shape-valid live values comparable so an acknowledged replacement
  // can repair invalid semantics. Only the validated source is ever deployed.
  const live = normalizeOverrides(livePolicy.document);
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
    const args = process.argv.slice(2);
    if (args.length === 1 && args[0] === "--snapshot") {
      const snapshot = await readCoordinatorPolicySnapshot({
        accountId: process.env.CLOUDFLARE_ACCOUNT_ID,
        apiToken: process.env.CLOUDFLARE_API_TOKEN,
      });
      console.log(
        JSON.stringify({
          deploymentId: snapshot.deploymentId,
          versionId: snapshot.versionId,
          policySHA256: policyDigest(snapshot.document),
          valid: isLeasePolicyOverrides(snapshot.document),
        }),
      );
    } else {
      if (args.length && (args.length !== 2 || args[0] !== "--config")) {
        throw new Error(
          "Lease policy verification takes no arguments except --config <file>; deployment stopped.",
        );
      }
      let config;
      try {
        const require = createRequire(new URL("../worker/package.json", import.meta.url));
        const { unstable_readConfig } = require("wrangler");
        config = unstable_readConfig(
          {
            config: args[1] ?? fileURLToPath(new URL("../worker/wrangler.jsonc", import.meta.url)),
          },
          { hideWarnings: true },
        );
      } catch {
        throw new Error(
          "Unable to read the production Wrangler configuration; deployment stopped.",
        );
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
    }
  } catch (error) {
    console.error(`::error::${error.message}`);
    process.exitCode = 1;
  }
}
