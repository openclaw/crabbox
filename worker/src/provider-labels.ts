import type { LeaseConfig } from "./config";
import { normalizeLeaseSlug } from "./slug";

export function leaseProviderLabels(
  config: LeaseConfig,
  leaseID: string,
  slug: string,
  owner: string,
  provider: string,
  now: Date,
  extra: Record<string, string> = {},
): Record<string, string> {
  const labels: Record<string, string> = {
    class: config.class,
    crabbox: "true",
    created_by: "crabbox",
    keep: String(config.keep),
    lease: leaseID,
    slug: normalizeLeaseSlug(slug),
    owner: sanitizeLabel(owner),
    profile: config.profile,
    provider_key: config.providerKey,
    provider,
    target: config.target ?? "linux",
    server_type: config.serverType,
    state: "leased",
    created_at: labelTime(now),
    last_touched_at: labelTime(now),
    ttl_secs: String(config.ttlSeconds),
    idle_timeout_secs: String(config.idleTimeoutSeconds),
    expires_at: labelTime(
      new Date(now.getTime() + Math.min(config.ttlSeconds, config.idleTimeoutSeconds) * 1000),
    ),
  };
  if (config.target === "windows") {
    labels["windows_mode"] = config.windowsMode;
  }
  if (config.desktop) {
    labels["desktop"] = "true";
  }
  if (config.browser) {
    labels["browser"] = "true";
  }
  return sanitizeLabels({ ...labels, ...extra });
}

function sanitizeLabel(value: string): string {
  const cleaned = value
    .trim()
    .replaceAll(/[^a-zA-Z0-9_.-]/g, "_")
    .slice(0, 63)
    .replaceAll(/^[_.-]+|[_.-]+$/g, "");
  return cleaned || "unknown";
}

function sanitizeLabels(labels: Record<string, string>): Record<string, string> {
  return Object.fromEntries(
    Object.entries(labels).map(([key, value]) => [key, sanitizeLabel(value)]),
  );
}

function labelTime(value: Date): string {
  return String(Math.trunc(value.getTime() / 1000));
}
