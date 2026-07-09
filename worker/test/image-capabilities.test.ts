import { describe, expect, it } from "vitest";

import {
  imageSatisfiesRequirements,
  missingImageCapabilities,
  normalizeImageCapabilities,
  normalizeImageRequirements,
} from "../src/image-capabilities";

describe("image capabilities", () => {
  it("compares numeric OS, SDK, and runtime versions", () => {
    const capabilities = normalizeImageCapabilities({
      osVersion: "15.5",
      sdks: { xcode: "16.4" },
      runtimes: { node: "24.2.0" },
      browser: true,
      desktop: true,
    });

    expect(
      imageSatisfiesRequirements(
        capabilities,
        normalizeImageRequirements({
          minOS: "15.4",
          sdks: { XCODE: "16.3" },
          runtimes: { node: "24.2" },
          browser: true,
          desktop: true,
        }),
      ),
    ).toBe(true);
    expect(missingImageCapabilities(capabilities, { webview2: true })).toEqual(["WebView2"]);
  });

  it("rejects malformed versions and capability names", () => {
    expect(() => normalizeImageRequirements({ minOS: "macOS 15" })).toThrow(
      "dot-separated numeric version",
    );
    expect(() => normalizeImageCapabilities({ runtimes: { "node/npm": "24" } })).toThrow("name");
    expect(() => normalizeImageCapabilities({ runtimes: { node: "" } })).toThrow(
      "requires a version",
    );
    for (const value of [false, "node=24", [], 24]) {
      expect(() => normalizeImageRequirements(value)).toThrow("must be an object");
    }
    expect(() => normalizeImageRequirements({ runtime: { node: "24" } })).toThrow(
      "is not supported",
    );
  });

  it("compares versions without numeric precision loss", () => {
    expect(
      imageSatisfiesRequirements(
        { runtimes: { node: "999999999999999999999999.1" } },
        { runtimes: { node: "999999999999999999999998.9" } },
      ),
    ).toBe(true);
  });
});
