import { readFileSync } from "node:fs";

import { describe, expect, it } from "vitest";

import { timingSafeEqual } from "../src/timing-safe";

describe("timing-safe authentication", () => {
  it("preserves exact string equality semantics", () => {
    expect(timingSafeEqual("", "")).toBe(true);
    expect(timingSafeEqual("shared-secret", "shared-secret")).toBe(true);
    expect(timingSafeEqual("Shared-secret", "shared-secret")).toBe(false);
    expect(timingSafeEqual("shared-secreu", "shared-secret")).toBe(false);
    expect(timingSafeEqual("shared-secret-extra", "shared-secret")).toBe(false);
    expect(timingSafeEqual("shared", "shared-secret")).toBe(false);
  });

  it("keeps static coordinator secrets off direct string comparisons", () => {
    const authSource = readFileSync(new URL("../src/auth.ts", import.meta.url), "utf8");
    const coordinatorSource = readFileSync(
      new URL("../src/coordinator-entry.ts", import.meta.url),
      "utf8",
    );

    expect(authSource).not.toMatch(/token\s*={2,3}\s*env\.CRABBOX_(?:ADMIN|SHARED)_TOKEN/);
    expect(coordinatorSource).not.toMatch(/bearerToken\([^)]*\)\s*!={1,2}\s*expected/);
    expect(`${authSource}\n${coordinatorSource}`).not.toMatch(
      /env\.CRABBOX_[A-Z_]*(?:TOKEN|SECRET)\s*(?:={2,3}|!={1,2})/,
    );
  });
});
