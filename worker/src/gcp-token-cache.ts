import { ExpiringTokenCache } from "./expiring-token-cache";
import { GCPFirewallCache } from "./gcp-firewall-cache";
import { GCPImageMinimumCache } from "./gcp-image-minimum-cache";
import type { Env } from "./types";

type Entry = {
  source: string | undefined;
  email: string | undefined;
  key: string | undefined;
  cache: ExpiringTokenCache;
  firewall: GCPFirewallCache;
  imageMinimum: GCPImageMinimumCache;
};

// Provider instances are request-scoped. Preparation stays in memory, scoped to one
// coordinator environment and credential generation, never in durable state.
const caches = new WeakMap<Env, Entry>();

export function coordinatorGCPTokenCache(env: Env): ExpiringTokenCache {
  return coordinatorCaches(env).cache;
}

export function coordinatorGCPFirewallCache(env: Env): GCPFirewallCache {
  return coordinatorCaches(env).firewall;
}

export function coordinatorGCPImageMinimumCache(env: Env): GCPImageMinimumCache {
  return coordinatorCaches(env).imageMinimum;
}

function coordinatorCaches(env: Env): Entry {
  const entry = caches.get(env);
  if (
    entry &&
    entry.source === env.CRABBOX_GCP_CREDENTIAL_SOURCE &&
    entry.email === env.GCP_CLIENT_EMAIL &&
    entry.key === env.GCP_PRIVATE_KEY
  )
    return entry;
  entry?.firewall.invalidate();
  entry?.imageMinimum.invalidate();
  const next = {
    source: env.CRABBOX_GCP_CREDENTIAL_SOURCE,
    email: env.GCP_CLIENT_EMAIL,
    key: env.GCP_PRIVATE_KEY,
    cache: new ExpiringTokenCache(),
    firewall: new GCPFirewallCache(),
    imageMinimum: new GCPImageMinimumCache(),
  };
  caches.set(env, next);
  return next;
}
