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

  it("routes deployed and preview workspaces through the verified AWS backend", () => {
    expect(configStringValues("CRABBOX_WORKSPACE_PROVIDER")).toEqual(["aws", "aws"]);
    expect(configStringValues("CRABBOX_AWS_SSH_CIDRS")).toEqual([]);
    expect(configStringValues("CRABBOX_PUBLIC_URL")).toEqual(["https://crabbox.openclaw.ai"]);
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
