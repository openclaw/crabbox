import {
  defaultTailscaleVersion,
  defaultTailscaleAMD64SHA256,
  defaultTailscaleARM64SHA256,
} from "./bootstrap.generated";
import { errorMessage } from "./http";
import type { Env } from "./types";

export interface TailscaleKeyRequest {
  hostname: string;
  tags: string[];
  description: string;
}

export type TailscaleInstallMode = "package" | "pinned";

export const defaultPinnedTailscaleVersion = defaultTailscaleVersion;
export const defaultPinnedTailscaleSHA256 = {
  amd64: defaultTailscaleAMD64SHA256,
  arm64: defaultTailscaleARM64SHA256,
} as const;

export interface TailscaleInstallConfig {
  mode: TailscaleInstallMode;
  version: string;
  sha256: {
    amd64: string;
    arm64: string;
  };
}

export type TailscalePreflightStatus =
  | "disabled"
  | "missing_oauth_credentials"
  | "invalid_tags"
  | "oauth_token_failed"
  | "auth_key_mint_failed"
  | "ok";

export interface TailscalePreflightResult {
  status: TailscalePreflightStatus;
  enabled: boolean;
  tailnet: string;
  tags: string[];
  install: TailscaleInstallConfig;
  mintedAuthKey?: boolean;
  message?: string;
}

type TailscaleAPIOperation = "oauth token" | "create auth key";

class TailscaleAPIError extends Error {
  // Provider diagnostics are classification-only, never public error fields.
  readonly #responseBody: string;

  constructor(
    readonly operation: TailscaleAPIOperation,
    readonly status: number,
    responseBody: string,
  ) {
    super(`tailscale ${operation} failed: http ${status}`);
    this.name = "TailscaleAPIError";
    this.#responseBody = trimBody(responseBody);
  }

  isTagOwnershipError(): boolean {
    if (this.status !== 400 && this.status !== 403) {
      return false;
    }
    const body = this.#responseBody.toLowerCase();
    return (
      (body.includes("requested tags") &&
        (body.includes("invalid or not permitted") ||
          body.includes("invalid or not allowed") ||
          body.includes("not owned"))) ||
      body.includes("tailnet-owned auth key must have tags set")
    );
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

export function tailscaleInstallConfig(env: Env): TailscaleInstallConfig {
  const requested = (env.CRABBOX_TAILSCALE_INSTALL_MODE ?? "package").trim().toLowerCase();
  return {
    mode: requested === "pinned" ? "pinned" : "package",
    version: env.CRABBOX_TAILSCALE_VERSION?.trim() || defaultPinnedTailscaleVersion,
    sha256: {
      amd64: env.CRABBOX_TAILSCALE_SHA256_AMD64?.trim() || defaultPinnedTailscaleSHA256.amd64,
      arm64: env.CRABBOX_TAILSCALE_SHA256_ARM64?.trim() || defaultPinnedTailscaleSHA256.arm64,
    },
  };
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
  const token = await tailscaleOAuthToken(clientID, clientSecret, {
    scope: "auth_keys",
    tags: request.tags,
  });
  const tailnet = env.CRABBOX_TAILSCALE_TAILNET?.trim() || "-";
  let data: { key?: string };
  try {
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
      throw new TailscaleAPIError("create auth key", response.status, text);
    }
    data = JSON.parse(text) as { key?: string };
  } catch (error) {
    if (error instanceof TailscaleAPIError) {
      throw error;
    }
    // oxlint-disable-next-line eslint/preserve-caught-error -- Upstream causes can expose OAuth credentials.
    throw new Error(
      `tailscale create auth key failed: ${errorMessage(error, [clientSecret, token])}`,
    );
  }
  if (!data.key) {
    throw new Error("tailscale create auth key returned no key");
  }
  return data.key;
}

export async function tailscalePreflight(env: Env): Promise<TailscalePreflightResult> {
  const install = tailscaleInstallConfig(env);
  const tailnet = env.CRABBOX_TAILSCALE_TAILNET?.trim() || "-";
  if (!tailscaleAllowed(env)) {
    return {
      status: "disabled",
      enabled: false,
      tailnet,
      tags: tailscaleDefaultTags(env),
      install,
      message: "Tailscale is disabled for this coordinator",
    };
  }
  if (!env.CRABBOX_TAILSCALE_CLIENT_ID || !env.CRABBOX_TAILSCALE_CLIENT_SECRET) {
    return {
      status: "missing_oauth_credentials",
      enabled: true,
      tailnet,
      tags: tailscaleDefaultTags(env),
      install,
      message: "Tailscale OAuth client id/secret are required",
    };
  }
  let tags: string[];
  try {
    tags = validateTailscaleTags(tailscaleDefaultTags(env), tailscaleDefaultTags(env));
  } catch (error) {
    return {
      status: "invalid_tags",
      enabled: true,
      tailnet,
      tags: [],
      install,
      message: errorMessage(error, [env.CRABBOX_TAILSCALE_CLIENT_SECRET]),
    };
  }
  try {
    await createTailscaleAuthKey(env, {
      hostname: "crabbox-preflight",
      tags,
      description: "crabbox tailscale preflight",
    });
  } catch (error) {
    const operation =
      error instanceof TailscaleAPIError && error.operation === "oauth token"
        ? "oauth_token_failed"
        : "auth_key_mint_failed";
    return {
      status: operation,
      enabled: true,
      tailnet,
      tags,
      install,
      message: errorMessage(error, [env.CRABBOX_TAILSCALE_CLIENT_SECRET]),
    };
  }
  return {
    status: "ok",
    enabled: true,
    tailnet,
    tags,
    install,
    mintedAuthKey: true,
  };
}

async function tailscaleOAuthToken(
  clientID: string,
  clientSecret: string,
  request: { scope: string; tags?: string[] },
): Promise<string> {
  const body = new URLSearchParams();
  body.set("client_id", clientID);
  body.set("client_secret", clientSecret);
  body.set("scope", request.scope);
  if (request.tags && request.tags.length > 0) {
    body.set("tags", request.tags.join(" "));
  }
  let data: { access_token?: string };
  try {
    const response = await fetch("https://api.tailscale.com/api/v2/oauth/token", {
      method: "POST",
      headers: { "content-type": "application/x-www-form-urlencoded" },
      body,
    });
    const text = await response.text();
    if (!response.ok) {
      throw new TailscaleAPIError("oauth token", response.status, text);
    }
    data = JSON.parse(text) as { access_token?: string };
  } catch (error) {
    if (error instanceof TailscaleAPIError) {
      throw error;
    }
    // oxlint-disable-next-line eslint/preserve-caught-error -- Upstream causes can expose OAuth credentials.
    throw new Error(`tailscale oauth token failed: ${errorMessage(error, [clientSecret])}`);
  }
  if (!data.access_token) {
    throw new Error("tailscale oauth token returned no access token");
  }
  return data.access_token;
}

export function tailscaleTagOwnershipErrorMessage(error: unknown): string | undefined {
  if (!(error instanceof TailscaleAPIError) || !error.isTagOwnershipError()) {
    return undefined;
  }
  return [
    "Tailscale rejected the requested tags.",
    "The requested tag set must exactly match the OAuth client's tags, or every requested tag must be owned by one of the OAuth client tags in tagOwners.",
    "For multi-tag allowlists, configure self-ownership for subset requests or use a dedicated deployment-owner tag.",
    error.message,
  ].join(" ");
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
