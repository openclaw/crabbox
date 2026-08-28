import { readFileSync } from "node:fs";

import { describe, expect, it } from "vitest";

import { defaultOSImage, normalizeOSImage, osImageSpec } from "../src/os-image";
import { osImageAliases, osImageSpecs, supportedOSImages } from "../src/os-image.generated";

const fixtures: { input: string; selector: string }[] = JSON.parse(
  readFileSync(new URL("../../testdata/bootstrap/os-images.json", import.meta.url), "utf8"),
);

describe("portable OS catalog", () => {
  it("normalizes shared aliases and rejects unknown selectors", () => {
    expect(normalizeOSImage(undefined)).toBe(defaultOSImage);
    for (const fixture of fixtures.filter((item) => item.selector)) {
      expect(normalizeOSImage(fixture.input)).toBe(fixture.selector);
      expect(osImageSpec(fixture.input)).toBe(osImageSpecs[fixture.selector]);
    }
    for (const fixture of fixtures.filter((item) => !item.selector)) {
      expect(() => osImageSpec(fixture.input)).toThrow(`supported: ${supportedOSImages}`);
    }
    for (const [alias, selector] of Object.entries(osImageAliases)) {
      expect(normalizeOSImage(alias)).toBe(selector);
    }
  });

  it("retains image mappings for both architectures and provider fallbacks", () => {
    for (const selector of ["ubuntu:24.04", "ubuntu:26.04"]) {
      const spec = osImageSpec(selector);
      expect(spec.awsName).toContain("amd64-server");
      expect(spec.awsArm64Name).toContain("arm64-server");
      expect(spec.azureImage).toContain(":server:latest");
      expect(spec.azureArm64Image).toContain(":server-arm64:latest");
      expect(spec.hetznerImage).toBe("ubuntu-24.04");
    }
  });
});
