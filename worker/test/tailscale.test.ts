import { afterEach, describe, expect, it, vi } from "vitest";

import {
  createTailscaleAuthKey,
  renderTailscaleHostname,
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
