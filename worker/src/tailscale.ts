import type { Env } from "./types";

export interface TailscaleKeyRequest {
  hostname: string;
  tags: string[];
  description: string;
}

type TailscaleAPIOperation = "oauth token" | "create auth key";

class TailscaleAPIError extends Error {
  constructor(
    readonly operation: TailscaleAPIOperation,
    readonly status: number,
    readonly responseBody: string,
  ) {
    super(`tailscale ${operation}: http ${status}: ${responseBody}`);
    this.name = "TailscaleAPIError";
  }
}

export function tailscaleAllowed(env: Env): boolean {
  if (env.CRABBOX_TAILSCALE_ENABLED === "0") {
    return false;
  }
  if (env.CRABBOX_TAILSCALE_ENABLED === "1") {
    return true;
  }
  return Boolean(env.CRABBOX_TAILSCALE_CLIENT_ID && env.CRABBOX_TAILSCALE_CLIENT_SECRET);
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
  value = sanitizeDNSLabel(value).slice(0, 63);
  return value || sanitizeDNSLabel(`crabbox-${leaseID.replaceAll("_", "-")}`).slice(0, 63);
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
    throw tailscaleAPIError("create auth key", response.status, text);
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
    throw tailscaleAPIError("oauth token", response.status, text);
  }
  const data = JSON.parse(text) as { access_token?: string };
  if (!data.access_token) {
    throw new Error("tailscale oauth token returned no access token");
  }
  return data.access_token;
}

export function tailscaleTagOwnershipErrorMessage(error: unknown): string | undefined {
  if (!(error instanceof TailscaleAPIError) || !isTailscaleTagOwnershipError(error)) {
    return undefined;
  }
  return [
    "Tailscale rejected the requested tags.",
    "The requested tag set must exactly match the OAuth client's tags, or every requested tag must be owned by one of the OAuth client tags in tagOwners.",
    "For multi-tag allowlists, configure self-ownership for subset requests or use a dedicated deployment-owner tag.",
    `Raw Tailscale error: ${error.message}`,
  ].join(" ");
}

function tailscaleAPIError(
  operation: TailscaleAPIOperation,
  status: number,
  responseBody: string,
): TailscaleAPIError {
  return new TailscaleAPIError(operation, status, trimBody(responseBody));
}

function isTailscaleTagOwnershipError(error: TailscaleAPIError): boolean {
  if (error.status !== 400 && error.status !== 403) {
    return false;
  }
  const body = error.responseBody.toLowerCase();
  return (
    (body.includes("requested tags") &&
      (body.includes("invalid or not permitted") ||
        body.includes("invalid or not allowed") ||
        body.includes("not owned"))) ||
    body.includes("tailnet-owned auth key must have tags set")
  );
}

function normalizeTags(values: string[]): string[] {
  return [...new Set(values.map((value) => value.trim().toLowerCase()).filter(Boolean))].filter(
    (tag) => /^tag:[a-z0-9_-]{1,63}$/.test(tag),
  );
}

function trimBody(value: string): string {
  return value.replaceAll(/\s+/g, " ").trim().slice(0, 500);
}

function sanitizeDNSLabel(value: string): string {
  let out = "";
  let lastDash = false;
  for (const char of value.toLowerCase()) {
    const code = char.charCodeAt(0);
    const ok = (code >= 97 && code <= 122) || (code >= 48 && code <= 57);
    if (ok) {
      out += char;
      lastDash = false;
      continue;
    }
    if (!lastDash) {
      out += "-";
      lastDash = true;
    }
  }
  let start = 0;
  let end = out.length;
  while (start < end && out[start] === "-") {
    start += 1;
  }
  while (end > start && out[end - 1] === "-") {
    end -= 1;
  }
  return out.slice(start, end);
}
