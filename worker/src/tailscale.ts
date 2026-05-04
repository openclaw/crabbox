import type { Env } from "./types";

export interface TailscaleKeyRequest {
  hostname: string;
  tags: string[];
  description: string;
}

export function tailscaleAllowed(env: Env): boolean {
  return env.CRABBOX_TAILSCALE_ENABLED !== "0";
}

export function tailscaleDefaultTags(env: Env): string[] {
  return normalizeTags((env.CRABBOX_TAILSCALE_TAGS ?? "tag:crabbox").split(","));
}

export function validateTailscaleTags(requested: string[], allowed: string[]): string[] {
  const tags = normalizeTags(requested.length > 0 ? requested : allowed);
  const allowedSet = new Set(allowed);
  const denied = tags.filter((tag) => !allowedSet.has(tag));
  if (denied.length > 0) {
    throw new Error(`tailscale tags not allowed: ${denied.join(",")}`);
  }
  if (tags.length === 0) {
    throw new Error("tailscale requires at least one allowed tag");
  }
  return tags;
}

export function renderTailscaleHostname(
  template: string,
  leaseID: string,
  slug: string,
  provider: string,
): string {
  const replacements: Record<string, string> = {
    "{id}": leaseID.replaceAll("_", "-"),
    "{slug}": slug,
    "{provider}": provider,
  };
  let value = template.trim() || "crabbox-{slug}";
  for (const [key, replacement] of Object.entries(replacements)) {
    value = value.replaceAll(key, replacement);
  }
  value = value
    .toLowerCase()
    .replaceAll(/[^a-z0-9-]/g, "-")
    .replaceAll(/-+/g, "-")
    .replaceAll(/^-+|-+$/g, "")
    .slice(0, 63);
  return value || `crabbox-${leaseID.replaceAll("_", "-")}`.slice(0, 63);
}

export async function createTailscaleAuthKey(
  env: Env,
  request: TailscaleKeyRequest,
): Promise<string> {
  const clientID = env.CRABBOX_TAILSCALE_CLIENT_ID;
  const clientSecret = env.CRABBOX_TAILSCALE_CLIENT_SECRET;
  if (!clientID || !clientSecret) {
    throw new Error("tailscale OAuth secrets are not configured");
  }
  const token = await tailscaleOAuthToken(clientID, clientSecret, request.tags);
  const tailnet = env.CRABBOX_TAILSCALE_TAILNET?.trim() || "-";
  const response = await fetch(
    `https://api.tailscale.com/api/v2/tailnet/${encodeURIComponent(tailnet)}/keys`,
    {
      method: "POST",
      headers: {
        authorization: `Bearer ${token}`,
        "content-type": "application/json",
      },
      body: JSON.stringify({
        capabilities: {
          devices: {
            create: {
              reusable: false,
              ephemeral: true,
              preauthorized: true,
              tags: request.tags,
            },
          },
        },
        expirySeconds: 600,
        description: request.description,
      }),
    },
  );
  const text = await response.text();
  if (!response.ok) {
    throw new Error(`tailscale create auth key: http ${response.status}: ${trimBody(text)}`);
  }
  const data = JSON.parse(text) as { key?: string };
  if (!data.key) {
    throw new Error("tailscale create auth key returned no key");
  }
  return data.key;
}

async function tailscaleOAuthToken(
  clientID: string,
  clientSecret: string,
  tags: string[],
): Promise<string> {
  const body = new URLSearchParams();
  body.set("client_id", clientID);
  body.set("client_secret", clientSecret);
  body.set("scope", "auth_keys");
  body.set("tags", tags.join(" "));
  const response = await fetch("https://api.tailscale.com/api/v2/oauth/token", {
    method: "POST",
    headers: { "content-type": "application/x-www-form-urlencoded" },
    body,
  });
  const text = await response.text();
  if (!response.ok) {
    throw new Error(`tailscale oauth token: http ${response.status}: ${trimBody(text)}`);
  }
  const data = JSON.parse(text) as { access_token?: string };
  if (!data.access_token) {
    throw new Error("tailscale oauth token returned no access token");
  }
  return data.access_token;
}

function normalizeTags(values: string[]): string[] {
  return [...new Set(values.map((value) => value.trim().toLowerCase()).filter(Boolean))].filter(
    (tag) => /^tag:[a-z0-9_-]{1,63}$/.test(tag),
  );
}

function trimBody(value: string): string {
  return value.replaceAll(/\s+/g, " ").trim().slice(0, 500);
}
