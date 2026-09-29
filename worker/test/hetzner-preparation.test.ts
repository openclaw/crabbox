import { afterEach, expect, it, vi } from "vitest";

import { HetznerClient } from "../src/hetzner";
import type { Env } from "../src/types";

afterEach(() => vi.unstubAllGlobals());

const leaseID = "cbx_abcdef123456";
const name = "crabbox-cbx-abcdef123456";
const key = {
  id: 7,
  name,
  public_key: "ssh-ed25519 fixture",
  labels: { crabbox: "true", created_by: "crabbox", lease: leaseID },
};

it("registers a fresh per-lease key without reading the full inventory", async () => {
  const calls: string[] = [];
  vi.stubGlobal("fetch", async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input));
    const method = init?.method ?? "GET";
    calls.push(`${method} ${url.pathname}${url.search}`);
    if (method === "POST") return Response.json({ ssh_key: key });
    return Response.json({ ssh_keys: [] });
  });
  await expect(
    new HetznerClient({ HETZNER_TOKEN: "fixture" } as Env).ensureSSHKey(
      name,
      key.public_key,
      leaseID,
    ),
  ).resolves.toEqual(key);
  expect(calls).toEqual([`GET /v1/ssh_keys?name=${name}`, "POST /v1/ssh_keys"]);
});

it.each(["owned", "foreign", "unavailable"])(
  "reads inventory after a uniqueness conflict and respects %s ownership evidence",
  async (mode) => {
    const calls: string[] = [];
    vi.stubGlobal("fetch", async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input));
      const method = init?.method ?? "GET";
      calls.push(
        method === "POST" ? "create" : url.searchParams.has("name") ? "name" : "inventory",
      );
      if (method === "POST")
        return Response.json({ error: { code: "uniqueness_error" } }, { status: 409 });
      if (url.searchParams.has("name")) return Response.json({ ssh_keys: [] });
      if (mode === "unavailable") return new Response("unavailable", { status: 503 });
      return Response.json({
        ssh_keys: [
          {
            ...key,
            labels: { ...key.labels, lease: mode === "owned" ? leaseID : "cbx_000000000001" },
          },
        ],
      });
    });
    const result = await new HetznerClient({ HETZNER_TOKEN: "fixture" } as Env)
      .ensureSSHKey(name, key.public_key, leaseID)
      .then(
        (value) => ({ key: value }),
        (error: unknown) => ({ error: String(error) }),
      );
    const expectedError = expect.stringContaining(
      mode === "foreign" ? "not owned by lease" : "503",
    );
    expect(result).toEqual(mode === "owned" ? { key } : { error: expectedError });
    expect(calls).toEqual(["name", "create", "inventory"]);
  },
);
