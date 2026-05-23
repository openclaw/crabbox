import { describe, expect, it } from "vitest";

import { portalHome } from "../src/portal";

describe("portal theme", () => {
  it("defaults to system color scheme and keeps explicit overrides", async () => {
    const response = portalHome([], [], new Request("https://crabbox.example/portal"));
    const body = await response.text();

    expect(body).toContain("data-theme-source");
    expect(body).toContain("prefers-color-scheme: dark");
    expect(body).toContain("crabbox-theme");
    expect(body).toContain('const next = current === "system" ? "dark"');
    expect(body).toContain('Theme: " + source');
  });
});
