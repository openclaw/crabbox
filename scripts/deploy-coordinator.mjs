import { spawn } from "node:child_process";
import { randomUUID } from "node:crypto";
import fs from "node:fs";
import { createRequire } from "node:module";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { isLeasePolicyOverrides } from "../worker/src/lease-policy-overrides.mjs";
import {
  bindingName,
  normalizeOverrides,
  policyDigest,
  readCoordinatorPolicySnapshot,
  verifyCoordinatorLeasePolicy,
} from "./verify-coordinator-lease-policy.mjs";

// Actions prints declared step env before running the script. Read private policy
// input from the runner-owned event file instead of interpolating it into env/run.
export function policyReplacementOptions(env = process.env) {
  if (env.GITHUB_ACTIONS !== "true") {
    return {
      acknowledgeReplacement: env.CRABBOX_ACKNOWLEDGE_LEASE_POLICY_REPLACEMENT === "1",
      replacementOverrides: env.CRABBOX_LEASE_POLICY_REPLACEMENT,
      expectedLivePolicySHA256: env.CRABBOX_EXPECTED_LIVE_LEASE_POLICY_SHA256,
    };
  }
  if (env.GITHUB_EVENT_NAME !== "workflow_dispatch") return {};
  let inputs;
  try {
    inputs = JSON.parse(fs.readFileSync(env.GITHUB_EVENT_PATH, "utf8")).inputs;
  } catch {
    throw new Error(
      "Cannot read workflow dispatch policy inputs from GITHUB_EVENT_PATH; no deployment performed.",
    );
  }
  if (
    !inputs ||
    typeof inputs !== "object" ||
    Array.isArray(inputs) ||
    ["lease_policy_replacement", "lease_policy_expected_sha256"].some(
      (key) => inputs[key] !== undefined && typeof inputs[key] !== "string",
    )
  ) {
    throw new Error(
      "Workflow dispatch policy inputs have an invalid shape; no deployment performed.",
    );
  }
  return {
    acknowledgeReplacement:
      inputs.replace_live_lease_policy === true || inputs.replace_live_lease_policy === "true",
    replacementOverrides: inputs.lease_policy_replacement,
    expectedLivePolicySHA256: inputs.lease_policy_expected_sha256,
  };
}

// Child logs can include the operator-owned document or secrets. Preserve safe
// stage/recovery diagnostics without replaying untrusted child output.
async function runCommand(command, args, options) {
  const { stage, ...spawnOptions } = options;
  const recovery =
    stage === "asset preparation"
      ? "In a credential-free checkout, run npm ci --prefix worker and npm run predeploy --prefix worker; repair the reported pinned noVNC assets or dependencies."
      : "Inspect the Cloudflare deployment and audit log, confirm the serving version/policy, and check Wrangler configuration and token permissions before retrying.";
  const failure = (status) =>
    new Error(
      `Coordinator ${stage} ${status}. ${recovery} Raw child output withheld to protect runtime configuration.`,
    );
  await new Promise((resolve, reject) => {
    const child = spawn(command, args, { ...spawnOptions, stdio: "ignore" });
    const interrupt = () => child.kill("SIGINT");
    const terminate = () => child.kill("SIGTERM");
    process.once("SIGINT", interrupt);
    process.once("SIGTERM", terminate);
    child.once("close", () => {
      process.removeListener("SIGINT", interrupt);
      process.removeListener("SIGTERM", terminate);
    });
    child.once("error", () => reject(failure("could not start")));
    child.once("exit", (code) =>
      code === 0 ? resolve() : reject(failure(`failed (exit ${code ?? "signal"})`)),
    );
  });
}

export async function deployCoordinator({
  accountId,
  apiToken,
  sha,
  secretsFile,
  acknowledgeReplacement = false,
  replacementOverrides,
  expectedLivePolicySHA256,
  workerDirectory = fileURLToPath(new URL("../worker/", import.meta.url)),
  fetchImpl = fetch,
  run = runCommand,
  env = process.env,
}) {
  if (!/^[a-f0-9]{32}$/.test(accountId ?? "") || !apiToken || !/^[a-f0-9]{40}$/.test(sha ?? ""))
    throw new Error(
      "Set the coordinator account, API token and full source commit before deployment.",
    );
  const replacing = replacementOverrides !== undefined && replacementOverrides !== "";
  if (replacing !== acknowledgeReplacement)
    throw new Error(
      "Intentional replacement requires both explicit acknowledgment and a complete replacement document ({} removes exceptions).",
    );
  if (
    (replacing && !/^[a-f0-9]{64}$/.test(expectedLivePolicySHA256 ?? "")) ||
    (!replacing && expectedLivePolicySHA256)
  )
    throw new Error(
      "Intentional replacement requires the reviewed live policy SHA-256; ordinary deploys must omit it.",
    );
  let replacement;
  if (replacing) {
    try {
      replacement = JSON.parse(replacementOverrides);
    } catch {
      /* validated below */
    }
    if (!isLeasePolicyOverrides(replacement))
      throw new Error("Replacement lease policy is invalid; no deployment performed.");
  }
  const require = createRequire(path.join(workerDirectory, "package.json"));
  const { experimental_readRawConfig, unstable_readConfig } = require("wrangler");
  const sourcePath = path.join(workerDirectory, "wrangler.jsonc");
  const sourceBytes = fs.readFileSync(sourcePath, "utf8");
  const { rawConfig: source } = experimental_readRawConfig({ config: sourcePath });
  if (
    source.name !== "crabbox-coordinator" ||
    source.keep_vars === true ||
    (source.account_id && source.account_id !== accountId) ||
    Object.hasOwn(source.vars ?? {}, bindingName)
  )
    throw new Error(
      "Production source must target the coordinator without keep_vars or a source-owned override binding.",
    );

  // Complete local asset preparation before capturing live state.
  const childEnv = { ...env, CLOUDFLARE_ACCOUNT_ID: accountId, CLOUDFLARE_API_TOKEN: apiToken };
  await run("npm", ["run", "predeploy"], {
    cwd: workerDirectory,
    env: childEnv,
    stage: "asset preparation",
  });
  const options = { accountId, apiToken, fetchImpl };
  const baseline = await readCoordinatorPolicySnapshot(options);
  if (replacing && policyDigest(baseline.document) !== expectedLivePolicySHA256)
    throw new Error(
      "Live lease policy no longer matches the reviewed replacement baseline; no deployment performed.",
    );
  if (!replacing && !isLeasePolicyOverrides(baseline.document))
    throw new Error(
      "Live lease policy is invalid; ordinary deployment stopped. Review an explicit replacement to repair it.",
    );
  const desired = replacing ? replacement : baseline.document;
  const config = structuredClone(source);
  config.vars ??= {};
  // Preserve the complete document, including empty entries; empty groups/absence are equivalent.
  if (Object.keys(normalizeOverrides(desired)).length)
    config.vars[bindingName] = JSON.stringify(desired);
  const configBytes = JSON.stringify(config, null, 2) + "\n";
  // Same directory as the source keeps every relative path (entrypoint, assets, aliases) intact.
  const configPath = path.join(workerDirectory, `.crabbox-coordinator-deploy-${randomUUID()}.json`);
  const scratch = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-coordinator-deploy-"));
  fs.chmodSync(scratch, 0o700);
  try {
    fs.writeFileSync(configPath, configBytes, { flag: "wx", mode: 0o600 });
    const actual = unstable_readConfig({ config: configPath }, { hideWarnings: true });
    await verifyCoordinatorLeasePolicy({
      ...options,
      desiredOverrides: actual.vars?.[bindingName],
      acknowledgeReplacement,
      expectedSnapshot: baseline,
    });
    if (
      fs.readFileSync(configPath, "utf8") !== configBytes ||
      fs.readFileSync(sourcePath, "utf8") !== sourceBytes
    )
      throw new Error("Deployment configuration changed after preparation; deployment stopped.");
    const outputPath = path.join(scratch, "wrangler-output.jsonl");
    const cliPath = path.join(workerDirectory, "node_modules/wrangler/bin/wrangler.js");
    try {
      await run(
        process.execPath,
        [
          cliPath,
          "deploy",
          "--config",
          configPath,
          "--tag",
          sha,
          ...(secretsFile ? ["--secrets-file", secretsFile] : []),
        ],
        {
          cwd: workerDirectory,
          env: { ...childEnv, WRANGLER_OUTPUT_FILE_PATH: outputPath },
          stage: "Wrangler upload",
        },
      );
      let upload;
      try {
        const events = fs.readFileSync(outputPath, "utf8").trim().split("\n").map(JSON.parse);
        const uploads = events.filter((event) => event.type === "deploy");
        if (uploads.length === 1) upload = uploads[0];
      } catch {
        /* missing/malformed receipt fails closed below */
      }
      if (
        upload?.worker_name !== "crabbox-coordinator" ||
        !/^[0-9a-f-]{36}$/i.test(upload?.version_id ?? "")
      )
        throw new Error(
          "Deployment may have changed production, but Wrangler did not confirm its version; inspect production before retrying.",
        );
      const deployed = await readCoordinatorPolicySnapshot(options);
      if (
        deployed.versionId !== upload.version_id ||
        policyDigest(deployed.document) !== policyDigest(desired)
      )
        throw new Error(
          "Post-deployment version/policy mismatch; production may have changed. Investigate; no automatic rollback performed.",
        );
      return {
        versionId: deployed.versionId,
        deploymentId: deployed.deploymentId,
        policySHA256: policyDigest(desired),
      };
    } catch (error) {
      throw new Error(
        `Deployment was attempted; production may have changed. ${error.message} Inspect production before retrying; no automatic rollback performed.`,
        { cause: error },
      );
    }
  } finally {
    fs.rmSync(configPath, { force: true });
    fs.rmSync(scratch, { recursive: true, force: true });
  }
}

if (import.meta.main) {
  try {
    const args = process.argv.slice(2);
    if (args.length && (args.length !== 2 || args[0] !== "--secrets-file"))
      throw new Error("Coordinator deployment accepts only --secrets-file <file>.");
    const proof = await deployCoordinator({
      accountId: process.env.CLOUDFLARE_ACCOUNT_ID,
      apiToken: process.env.CLOUDFLARE_API_TOKEN,
      sha: process.env.GITHUB_SHA,
      secretsFile: args[1],
      ...policyReplacementOptions(),
    });
    if (process.env.GITHUB_OUTPUT)
      fs.appendFileSync(process.env.GITHUB_OUTPUT, `lease_policy_sha256=${proof.policySHA256}\n`);
    console.log(
      `Verified deployed coordinator version ${proof.versionId} and complete lease policy.`,
    );
  } catch (error) {
    console.error(`::error::${error.message}`);
    process.exitCode = 1;
  }
}
