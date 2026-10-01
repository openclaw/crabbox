import { gunzipSync } from "node:zlib";

import { afterEach, expect, it, vi } from "vitest";

import { cloudInit } from "../src/bootstrap";
import { leaseConfig } from "../src/config";
import { HetznerClient } from "../src/hetzner";
import type { Env } from "../src/types";

afterEach(() => vi.unstubAllGlobals());

it.each(["xfce", "wayland", "gnome"] as const)(
  "fits %s desktop/browser bootstrap in Hetzner user_data without changing its contents",
  async (desktopEnv) => {
    const config = leaseConfig({
      provider: "hetzner",
      class: "tiny",
      desktop: true,
      browser: true,
      desktopEnv,
      sshPublicKey: "ssh-ed25519 fixture",
    });
    const key = { id: 7, name: config.providerKey, public_key: config.sshPublicKey };
    let userData = "";
    vi.stubGlobal("fetch", async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input));
      if (url.pathname === "/v1/ssh_keys") return Response.json({ ssh_keys: [key] });
      if (url.pathname === "/v1/servers" && init?.method === "POST") {
        userData = JSON.parse(String(init.body)).user_data;
        return Response.json({
          server: { id: 123, status: "running", public_net: { ipv4: { ip: "192.0.2.1" } } },
        });
      }
      throw new Error(`unexpected request ${init?.method} ${url.pathname}`);
    });
    await new HetznerClient({ HETZNER_TOKEN: "fixture" } as Env).createServerWithFallback(
      config,
      "cbx_abcdef123456",
      "fixture",
      "alice@example.com",
    );
    expect(new TextEncoder().encode(userData).length).toBeLessThanOrEqual(32768);
    const [headers, body] = userData.split("\n\n");
    expect(headers).toContain("Content-Type: application/gzip");
    expect(headers).toContain("Content-Transfer-Encoding: base64");
    expect(gunzipSync(Buffer.from(body, "base64")).toString()).toBe(cloudInit(config));
  },
);
