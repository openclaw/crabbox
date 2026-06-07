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
  it("keeps deployed and preview coordinator cost guardrails enabled", () => {
    for (const name of requiredCostGuardrails) {
      const values = configValues(name);
      expect(values).toHaveLength(2);
      expect(values.every((value) => value > 0)).toBe(true);
    }
  });
});

function configValues(name: string): number[] {
  const pattern = new RegExp(`"${name}"\\s*:\\s*"(\\d+)"`, "g");
  return [...wranglerConfig.matchAll(pattern)].map((match) => Number(match[1]));
}
