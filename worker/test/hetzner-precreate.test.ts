import { afterEach, expect, it, vi } from "vitest";

import { HetznerClient } from "../src/hetzner";
import { providerKeyForLease, providerKeyOwnershipLabels } from "../src/provider-key";
import type { Env } from "../src/types";

afterEach(() => vi.unstubAllGlobals());

it.each([false, true])(
  "defers the full key inventory until an owned key collides (collision=%s)",
  async (collision) => {
    const leaseID = "cbx_abcdef123456";
    const name = providerKeyForLease(leaseID);
    const key = {
      id: 7,
      name: collision ? "shared-key" : name,
      public_key: "ssh-ed25519 test",
      labels: collision ? {} : providerKeyOwnershipLabels(leaseID),
    };
    const calls: string[] = [];
    vi.stubGlobal("fetch", async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input));
      const method = init?.method ?? "GET";
      calls.push(`${method} ${url.searchParams.has("name") ? "named" : "keys"}`);
      if (method === "POST")
        return collision
          ? Response.json({ error: { code: "uniqueness_error" } }, { status: 409 })
          : Response.json({ ssh_key: key });
      return Response.json({ ssh_keys: url.searchParams.has("name") || !collision ? [] : [key] });
    });
    const client = new HetznerClient({ HETZNER_TOKEN: "synthetic-token" } as Env);
    expect(await client.ensureSSHKey(name, "ssh-ed25519 test", leaseID)).toEqual(key);
    expect(calls).toEqual(
      collision ? ["GET named", "POST keys", "GET keys"] : ["GET named", "POST keys"],
    );
  },
);
