import { describe, expect, it } from "vitest";

import { portalHome } from "../src/portal";

describe("portal theme", () => {
  it("defaults to system color scheme and keeps explicit overrides", async () => {
    const response = portalHome([], [], new Request("https://crabbox.example/portal"));
    const body = await response.text();

    expect(body).toContain("data-theme-source");
    expect(body).toContain("prefers-color-scheme: dark");
    expect(body).toContain("crabbox-theme-source");
    expect(body).not.toContain("getItem('crabbox-theme')");
    expect(body).not.toContain('getItem("crabbox-theme")');
    expect(body).toContain('value === "system"');
    expect(body).toContain('const next = current === "system" ? "dark"');
    expect(body).toContain('Theme: " + source');
    expect(body).toContain("crabbox-theme-change");
    expect(body).toContain('querySelectorAll("form[data-confirm]")');
    expect(body).toContain('id="portal-dialog"');
    expect(body).toContain('aria-modal="true"');
    expect(body).toContain("window.crabboxDialog = Object.freeze({");
    expect(body).toContain('showPortalDialog("confirm", message, options)');
    expect(body).toContain("portalDialog.close(returnValue);");
    expect(body).toContain("resolvePortalDialog(returnValue);");
    expect(body).toContain('portalDialogClosing || portalDialog.hasAttribute("open")');
    expect(body).toContain('typeof form.requestSubmit === "function"');
    expect(body).toContain("form.requestSubmit(submitter || undefined)");
    expect(body).toContain("HTMLFormElement.prototype.submit.call(form)");
    expect(body).toContain(".portal-dialog-input[hidden] { display:none; }");
    expect(body).toContain('submitter?.getAttribute("aria-label")');
    expect(body).toContain('portalDialog.dataset.fallbackModal = "true"');
    expect(body).toContain('document.body.dataset.portalDialogOpen = "true"');
    expect(body).toContain("delete document.body.dataset.portalDialogOpen");
    expect(body).toContain('if (event.key === "Escape")');
    expect(body).not.toContain("if (!portalDialog?.showModal)");
    expect(body).not.toContain("window.confirm(");
    expect(body).not.toContain("window.prompt(");
  });

  it("renders an explicit admin summary when the portal is admin scoped", async () => {
    const response = portalHome(
      [],
      [],
      new Request("https://crabbox.example/portal", {
        headers: {
          "x-crabbox-admin": "true",
          "x-crabbox-owner": "admin@example.com",
          "x-crabbox-org": "example-org",
        },
      }),
    );
    const body = await response.text();

    expect(body).toContain("admin mode");
    expect(body).toContain("data-admin-panel");
    expect(body).toContain("leases JSON");
    expect(body).toContain("admin@example.com / example-org");
  });
});
