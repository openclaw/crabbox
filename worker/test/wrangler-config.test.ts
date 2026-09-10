import { readFileSync } from "node:fs";

import { describe, expect, it } from "vitest";

const wranglerConfig = readFileSync(new URL("../wrangler.jsonc", import.meta.url), "utf8");

const requiredCostGuardrails = [
  "CRABBOX_MAX_ACTIVE_LEASES",
  "CRABBOX_MAX_ACTIVE_LEASES_PER_OWNER",
  "CRABBOX_MAX_ACTIVE_LEASES_PER_ORG",
  "CRABBOX_MAX_ACTIVE_LEASES_PER_CAPACITY_ADMIN",
  "CRABBOX_MAX_MONTHLY_USD",
  "CRABBOX_MAX_MONTHLY_USD_PER_OWNER",
  "CRABBOX_MAX_MONTHLY_USD_PER_ORG",
];

describe("wrangler config", () => {
  it("uses the portable ssh2 crypto implementation", () => {
    expect(wranglerConfig).toContain('"./agent.js": "./src/ssh2-agent.cjs"');
    expect(wranglerConfig).toContain(
      '"./crypto/build/Release/sshcrypto.node": "./src/ssh2-native.cjs"',
    );
    expect(wranglerConfig).toContain('"./crypto/poly1305.js": "./src/ssh2-poly1305.cjs"');
  });

  it("keeps deployed and preview coordinator cost guardrails enabled", () => {
    for (const name of requiredCostGuardrails) {
      const values = configValues(name);
      expect(values).toHaveLength(2);
      expect(values.every((value) => value > 0)).toBe(true);
    }
  });

  it("keeps deployed and preview checkpoint and use-claim admission bounded", () => {
    expect(configValues("CRABBOX_MAX_CHECKPOINTS")).toEqual([20, 20]);
    expect(configValues("CRABBOX_MAX_CHECKPOINTS_PER_OWNER")).toEqual([10, 10]);
    expect(configValues("CRABBOX_MAX_CHECKPOINTS_PER_ORG")).toEqual([20, 20]);
    expect(configValues("CRABBOX_MAX_CHECKPOINT_USE_CLAIMS")).toEqual([16, 16]);
    expect(configValues("CRABBOX_MAX_CHECKPOINT_USE_CLAIMS_PER_OWNER")).toEqual([64, 64]);
    expect(configValues("CRABBOX_MAX_CHECKPOINT_USE_CLAIMS_TOTAL")).toEqual([256, 256]);
  });

  it("routes deployed and preview workspaces through the verified AWS backend", () => {
    expect(configStringValues("CRABBOX_WORKSPACE_PROVIDER")).toEqual(["aws", "aws"]);
    expect(configStringValues("CRABBOX_AWS_SSH_CIDRS")).toEqual([]);
    expect(configStringValues("CRABBOX_PUBLIC_URL")).toEqual(["https://crabbox.openclaw.ai"]);
  });

  it("configures the production-only R2 artifact backend without credentials or public reads", () => {
    const productionVars = wranglerConfig.match(/"vars"\s*:\s*\{([^}]*)\}/)?.[1] ?? "";
    const artifactVars = {
      CRABBOX_ARTIFACTS_BACKEND: "r2",
      CRABBOX_ARTIFACTS_BUCKET: "openclaw-crabbox-artifacts",
      CRABBOX_ARTIFACTS_PREFIX: "crabbox-artifacts",
      CRABBOX_ARTIFACTS_REGION: "auto",
      CRABBOX_ARTIFACTS_ENDPOINT_URL:
        "https://91b59577e757131d68d55a471fe32aca.r2.cloudflarestorage.com",
    };
    for (const [name, value] of Object.entries(artifactVars)) {
      expect(productionVars).toContain(`"${name}": "${value}"`);
      expect(configStringValues(name)).toEqual([value]);
    }

    const previewConfig = wranglerConfig.split(/"previews"\s*:/)[1];
    expect(previewConfig).toBeDefined();
    expect(previewConfig).not.toContain("CRABBOX_ARTIFACTS_");
    for (const name of [
      "CRABBOX_ARTIFACTS_ACCESS_KEY_ID",
      "CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY",
      "CRABBOX_ARTIFACTS_SESSION_TOKEN",
      "CRABBOX_ARTIFACTS_BASE_URL",
      "CRABBOX_ARTIFACTS_PUBLIC_READS",
    ]) {
      expect(wranglerConfig).not.toContain(name);
    }
    expect(wranglerConfig).not.toContain('"r2_buckets"');
  });

  it("binds deployment version metadata for interrupted provisioning recovery", () => {
    expect(wranglerConfig).toContain('"version_metadata"');
    expect(wranglerConfig).toContain('"binding": "CF_VERSION_METADATA"');
  });
});

function configValues(name: string): number[] {
  const pattern = new RegExp(`"${name}"\\s*:\\s*"(\\d+)"`, "g");
  return [...wranglerConfig.matchAll(pattern)].map((match) => Number(match[1]));
}

function configStringValues(name: string): string[] {
  const pattern = new RegExp(`"${name}"\\s*:\\s*"([^"]+)"`, "g");
  return [...wranglerConfig.matchAll(pattern)].map((match) => String(match[1]));
}
