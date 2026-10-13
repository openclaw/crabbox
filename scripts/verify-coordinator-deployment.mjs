// Keep the API token on Cloudflare's API origin; the public health request is anonymous.
export async function verifyCoordinatorDeployment({ accountId, apiToken, sha, fetchImpl = fetch }) {
  if (!/^[a-f0-9]{32}$/.test(accountId ?? "") || !apiToken) {
    throw new Error("Set CLOUDFLARE_ACCOUNT_ID and CLOUDFLARE_API_TOKEN before verification.");
  }
  if (!/^[a-f0-9]{40}$/.test(sha ?? "")) {
    throw new Error("GITHUB_SHA must be the full commit SHA deployed to the coordinator.");
  }
  const deploymentsURL = `https://api.cloudflare.com/client/v4/accounts/${accountId}/workers/scripts/crabbox-coordinator/deployments`;
  async function readJSON(url, label, headers = {}) {
    let response;
    try {
      response = await fetchImpl(url, {
        headers,
        redirect: "error",
        cache: "no-store",
        signal: AbortSignal.timeout(10_000),
      });
    } catch {
      throw new Error(`${label} request failed; check connectivity and retry verification.`);
    }
    if (!response.ok) {
      throw new Error(`${label} request failed (HTTP ${response.status}).`);
    }
    try {
      return await response.json();
    } catch {
      throw new Error(`${label} returned invalid JSON.`);
    }
  }
  async function activeDeployment() {
    const data = await readJSON(deploymentsURL, "Cloudflare deployments", {
      authorization: `Bearer ${apiToken}`,
    });
    // The deployments API returns the latest deployment actively serving traffic first.
    const deployment = data?.success === true ? data.result?.deployments?.[0] : undefined;
    const versions = deployment?.versions;
    if (
      typeof deployment?.id !== "string" ||
      !deployment.id ||
      !Array.isArray(versions) ||
      versions.length !== 1 ||
      versions[0]?.percentage !== 100 ||
      typeof versions[0]?.version_id !== "string" ||
      !versions[0].version_id
    ) {
      throw new Error(
        "Expected one active coordinator version at 100%; inspect Cloudflare deployments.",
      );
    }
    return { deploymentId: deployment.id, versionId: versions[0].version_id };
  }

  const before = await activeDeployment();
  const health = await readJSON(
    `https://crabbox.openclaw.ai/v1/health?deployment=${sha}`,
    "Coordinator health",
  );
  if (
    health?.ok !== true ||
    health.service !== "crabbox-coordinator" ||
    health.version?.id !== before.versionId ||
    health.version?.tag !== sha
  ) {
    throw new Error(
      "Coordinator health did not confirm the active version and requested commit; inspect the route and deployment.",
    );
  }
  // Reject a concurrent rollout instead of reporting success for a superseded version.
  const after = await activeDeployment();
  if (after.deploymentId !== before.deploymentId || after.versionId !== before.versionId) {
    throw new Error(
      "Coordinator deployment changed during verification; verify the current deployment again.",
    );
  }
  return { sha, ...after };
}

if (import.meta.main) {
  try {
    const proof = await verifyCoordinatorDeployment({
      accountId: process.env.CLOUDFLARE_ACCOUNT_ID,
      apiToken: process.env.CLOUDFLARE_API_TOKEN,
      sha: process.env.GITHUB_SHA,
    });
    console.log(`Verified coordinator deployment: ${JSON.stringify(proof)}`);
  } catch (error) {
    console.error(`::error::${error.message}`);
    process.exitCode = 1;
  }
}
