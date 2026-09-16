import { afterEach, describe, expect, it, vi } from "vitest";

import {
  createTailscaleAuthKey,
  renderTailscaleHostname,
  tailscaleInstallConfig,
  tailscalePreflight,
  tailscaleTagOwnershipErrorMessage,
} from "../src/tailscale";

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("tailscale hostnames", () => {
  it("renders DNS labels from templates", () => {
    expect(
      renderTailscaleHostname(
        " Crabbox/{provider}/{slug}/{id} ",
        "cbx_abcdef123456",
        "Blue Lobster",
        "hetzner",
      ),
    ).toBe("crabbox-hetzner-blue-lobster-cbx-abcdef123456");
  });

  it("falls back to the lease id when the rendered hostname is empty", () => {
    expect(renderTailscaleHostname("!!!", "cbx_abcdef123456", "", "aws")).toBe(
      "crabbox-cbx-abcdef123456",
    );
  });
});

describe("tailscale tag ownership errors", () => {
  it.each([
    {
      name: "OAuth token",
      operation: "oauth token",
      responses: [
        new Response(
          JSON.stringify({
            message:
              "requested tags [tag:ci] are invalid or not permitted; client-secret\n    at providerDiagnostic (provider.js:42:7)",
          }),
          { status: 400 },
        ),
      ],
    },
    {
      name: "auth key",
      operation: "create auth key",
      responses: [
        new Response(JSON.stringify({ access_token: "oauth-token" })),
        new Response(
          "requested tags [tag:ci] are invalid or not permitted; client-secret oauth-token\n    at providerDiagnostic (provider.js:42:7)",
          { status: 400 },
        ),
      ],
    },
  ])("adds safe actionable guidance for $name tag denials", async ({ operation, responses }) => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => responses.shift()!),
    );

    let caught: unknown;
    try {
      await createTailscaleAuthKey(
        {
          CRABBOX_TAILSCALE_CLIENT_ID: "client-id",
          CRABBOX_TAILSCALE_CLIENT_SECRET: "client-secret",
        },
        {
          hostname: "crabbox-ci",
          tags: ["tag:ci"],
          description: "test key",
        },
      );
    } catch (error) {
      caught = error;
    }

    const message = tailscaleTagOwnershipErrorMessage(caught);
    expect(message).toContain("must exactly match the OAuth client's tags");
    expect(message).toContain("dedicated deployment-owner tag");
    expect(message).toContain(`tailscale ${operation} failed: http 400`);
    expect(message).not.toContain("client-secret");
    expect(message).not.toContain("oauth-token");
    expect(message).not.toContain("providerDiagnostic");
    expect(message).not.toContain("provider.js");
    expect(caught).toBeInstanceOf(Error);
    expect((caught as Error).message).toBe(`tailscale ${operation} failed: http 400`);
    expect(JSON.stringify(caught)).not.toContain("client-secret");
    expect(JSON.stringify(caught)).not.toContain("oauth-token");
    expect(JSON.stringify(caught)).not.toContain("providerDiagnostic");
  });

  it("does not classify unrelated OAuth failures as tag ownership errors", async () => {
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        .mockResolvedValue(
          new Response(JSON.stringify({ message: "API token invalid" }), { status: 401 }),
        ),
    );

    let caught: unknown;
    try {
      await createTailscaleAuthKey(
        {
          CRABBOX_TAILSCALE_CLIENT_ID: "client-id",
          CRABBOX_TAILSCALE_CLIENT_SECRET: "client-secret",
        },
        {
          hostname: "crabbox-ci",
          tags: ["tag:ci"],
          description: "test key",
        },
      );
    } catch (error) {
      caught = error;
    }

    expect(tailscaleTagOwnershipErrorMessage(caught)).toBeUndefined();
    expect(caught).toBeInstanceOf(Error);
    expect((caught as Error).message).toBe("tailscale oauth token failed: http 401");
  });
});

describe("tailscale preflight", () => {
  it("reports disabled without touching the Tailscale API", async () => {
    const fetch = vi.fn<(input: RequestInfo | URL) => Promise<Response>>();
    vi.stubGlobal("fetch", fetch);

    const result = await tailscalePreflight({ CRABBOX_TAILSCALE_ENABLED: "0" });

    expect(result.status).toBe("disabled");
    expect(result.enabled).toBe(false);
    expect(result.install.mode).toBe("package");
    expect(fetch).not.toHaveBeenCalled();
  });

  it("reports missing OAuth credentials when explicitly enabled", async () => {
    const result = await tailscalePreflight({ CRABBOX_TAILSCALE_ENABLED: "1" });

    expect(result.status).toBe("missing_oauth_credentials");
    expect(result.enabled).toBe(true);
  });

  describe.each([
    { operation: "oauth token", failureStatus: "oauth_token_failed", calls: 1 },
    { operation: "create auth key", failureStatus: "auth_key_mint_failed", calls: 2 },
  ])("$operation failures", ({ operation, failureStatus, calls }) => {
    it.each(["text response", "JSON response", "fetch rejection"])(
      "keeps diagnostics and credentials out of a %s",
      async (failure) => {
        const clientSecret = "synthetic-client-credential";
        const token = "synthetic-runtime-credential";
        const diagnostic =
          `provider unavailable ${operation === "create auth key" ? `${clientSecret} ${token}` : clientSecret}` +
          "\n    at providerDiagnostic (provider.js:42:7)";
        const fetch = vi.fn<typeof globalThis.fetch>();
        if (operation === "create auth key") {
          fetch.mockResolvedValueOnce(new Response(JSON.stringify({ access_token: token })));
        }
        if (failure === "fetch rejection") {
          fetch.mockRejectedValueOnce(new Error(diagnostic));
        } else {
          fetch.mockResolvedValueOnce(
            new Response(
              failure === "JSON response" ? JSON.stringify({ message: diagnostic }) : diagnostic,
              { status: 503 },
            ),
          );
        }
        vi.stubGlobal("fetch", fetch);

        const result = await tailscalePreflight({
          CRABBOX_TAILSCALE_CLIENT_ID: "client-id",
          CRABBOX_TAILSCALE_CLIENT_SECRET: clientSecret,
          CRABBOX_TAILSCALE_TAGS: "tag:ci",
        });

        // Fetch rejections retain the existing generic key-mint failure category.
        expect(result.status).toBe(
          failure === "fetch rejection" ? "auth_key_mint_failed" : failureStatus,
        );
        expect(result).toMatchObject({ enabled: true, tailnet: "-", tags: ["tag:ci"] });
        expect(result.install.mode).toBe("package");
        const safeDiagnostic =
          operation === "create auth key" ? "[redacted] [redacted]" : "[redacted]";
        const expectedMessage =
          failure === "fetch rejection"
            ? `tailscale ${operation} failed: provider unavailable ${safeDiagnostic}`
            : `tailscale ${operation} failed: http 503`;
        expect(result.message).toBe(expectedMessage);
        const text = JSON.stringify(result);
        expect(text).not.toContain(clientSecret);
        expect(text).not.toContain(token);
        expect(text).not.toContain("providerDiagnostic");
        expect(text).not.toContain("provider.js");
        expect(fetch).toHaveBeenCalledTimes(calls);
      },
    );
  });

  it("mints and redacts the one-off smoke key", async () => {
    const bodies: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        bodies.push(String(init?.body ?? ""));
        const url = String(input);
        if (url === "https://api.tailscale.com/api/v2/oauth/token") {
          return new Response(JSON.stringify({ access_token: "oauth-token" }));
        }
        if (url === "https://api.tailscale.com/api/v2/tailnet/-/keys") {
          return new Response(JSON.stringify({ key: "tskey-secret" }));
        }
        return new Response(JSON.stringify({ message: `unexpected ${url}` }), { status: 500 });
      }),
    );

    const result = await tailscalePreflight({
      CRABBOX_TAILSCALE_CLIENT_ID: "client-id",
      CRABBOX_TAILSCALE_CLIENT_SECRET: "client-secret",
      CRABBOX_TAILSCALE_TAGS: "tag:ci",
    });

    expect(result.status).toBe("ok");
    expect(result.mintedAuthKey).toBe(true);
    expect(JSON.stringify(result)).not.toContain("tskey-secret");
    expect(bodies[0]).toContain("scope=auth_keys");
    expect(bodies[0]).toContain("tags=tag%3Aci");
  });

  it("parses pinned installer overrides", () => {
    expect(
      tailscaleInstallConfig({
        CRABBOX_TAILSCALE_INSTALL_MODE: "pinned",
        CRABBOX_TAILSCALE_VERSION: "1.99.1",
        CRABBOX_TAILSCALE_SHA256_AMD64: "amd",
        CRABBOX_TAILSCALE_SHA256_ARM64: "arm",
      }),
    ).toEqual({
      mode: "pinned",
      version: "1.99.1",
      sha256: { amd64: "amd", arm64: "arm" },
    });
  });

  it("preserves package installer mode and defaults to it", () => {
    expect(
      tailscaleInstallConfig({
        CRABBOX_TAILSCALE_INSTALL_MODE: "package",
      }).mode,
    ).toBe("package");
    expect(tailscaleInstallConfig({}).mode).toBe("package");
  });
});
