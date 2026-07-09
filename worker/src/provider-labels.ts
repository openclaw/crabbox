import type { LeaseConfig } from "./config";
import { normalizeLeaseSlug } from "./slug";
import type { LeaseRecord, Provider, ProviderMachine } from "./types";

type ProviderOwnershipLease = Pick<
  LeaseRecord,
  "id" | "slug" | "provider" | "owner" | "providerOwner" | "workspaceID"
>;

export const workspacePrewarmProviderOwner = "crabbox-internal-prewarm";

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
    owner: providerLabelValue(owner),
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
  if (config.pond) {
    labels["pond"] = config.pond;
  }
  if (config.exposedPorts && config.exposedPorts.length > 0) {
    labels["crabbox_exposed_ports"] = config.exposedPorts.join("-");
  }
  if (config.desktop) {
    labels["desktop"] = "true";
    labels["desktop_env"] = config.desktopEnv;
  }
  if (config.browser) {
    labels["browser"] = "true";
  }
  if (config.code) {
    labels["code"] = "true";
  }
  if (config.tailscale) {
    labels["tailscale"] = "true";
    labels["tailscale_state"] = "requested";
    labels["tailscale_hostname"] = config.tailscaleHostname;
    labels["tailscale_tags"] = config.tailscaleTags.join(",");
    if (config.tailscaleExitNode) {
      labels["tailscale_exit_node"] = config.tailscaleExitNode;
      labels["tailscale_exit_node_allow_lan_access"] = String(
        config.tailscaleExitNodeAllowLanAccess,
      );
    }
  }
  return sanitizeLabels({ ...labels, ...extra });
}

export function providerLabelValue(value: string): string {
  const cleaned = value
    .trim()
    .replaceAll(/[^a-zA-Z0-9_.-]/g, "_")
    .slice(0, 63)
    .replaceAll(/^[_.-]+|[_.-]+$/g, "");
  return cleaned || "unknown";
}

export function providerMachineOwnedByLease(
  machine: Pick<ProviderMachine, "provider" | "cloudID" | "labels">,
  lease: ProviderOwnershipLease & Pick<LeaseRecord, "cloudID">,
  provider: Provider,
  labelValue: (value: string) => string = providerLabelValue,
): boolean {
  return (
    /^cbx_[a-f0-9]{12}$/.test(lease.id) &&
    lease.provider === provider &&
    machine.provider === provider &&
    lease.cloudID.length > 0 &&
    machine.cloudID === lease.cloudID &&
    providerLabelsOwnedByLease(machine.labels ?? {}, lease, provider, labelValue)
  );
}

export function providerLabelsOwnedByLease(
  labels: Record<string, string>,
  lease: ProviderOwnershipLease,
  provider: Provider,
  labelValue: (value: string) => string = providerLabelValue,
): boolean {
  const slug = normalizeLeaseSlug(lease.slug);
  const providerOwners = lease.providerOwner?.trim()
    ? [lease.providerOwner.trim()]
    : [lease.owner.trim(), ...(lease.workspaceID ? [workspacePrewarmProviderOwner] : [])].filter(
        Boolean,
      );
  return (
    /^cbx_[a-f0-9]{12}$/.test(lease.id) &&
    slug.length > 0 &&
    providerOwners.length > 0 &&
    lease.provider === provider &&
    labels["crabbox"] === "true" &&
    labels["created_by"] === "crabbox" &&
    labels["lease"] === lease.id &&
    providerOwners.some((owner) => labels["owner"] === labelValue(owner)) &&
    labels["provider"] === provider &&
    labels["slug"] === labelValue(slug)
  );
}

function sanitizeLabels(labels: Record<string, string>): Record<string, string> {
  return Object.fromEntries(
    Object.entries(labels).map(([key, value]) => [key, providerLabelValue(value)]),
  );
}

function labelTime(value: Date): string {
  return String(Math.trunc(value.getTime() / 1000));
}
