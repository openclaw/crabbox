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
      responses: [
        new Response(
          JSON.stringify({
            message: "requested tags [tag:ci] are invalid or not permitted",
          }),
          { status: 400 },
        ),
      ],
    },
    {
      name: "auth key",
      responses: [
        new Response(JSON.stringify({ access_token: "oauth-token" })),
        new Response(
          JSON.stringify({
            message: "requested tags [tag:ci] are invalid or not permitted",
          }),
          { status: 400 },
        ),
      ],
    },
  ])("adds actionable guidance for $name tag denials", async ({ responses }) => {
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
    expect(message).toContain("Raw Tailscale error: tailscale");
    expect(message).toContain("requested tags [tag:ci] are invalid or not permitted");
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
    expect((caught as Error).message).toContain('http 401: {"message":"API token invalid"}');
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
});
