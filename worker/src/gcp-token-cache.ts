import { ExpiringTokenCache } from "./expiring-token-cache";
import type { Env } from "./types";

type Entry = {
  source: string | undefined;
  email: string | undefined;
  key: string | undefined;
  cache: ExpiringTokenCache;
};

// Provider instances are request-scoped. Tokens stay in memory, scoped to one
// coordinator environment and credential generation, never in durable state.
const caches = new WeakMap<Env, Entry>();

export function coordinatorGCPTokenCache(env: Env): ExpiringTokenCache {
  const entry = caches.get(env);
  if (
    entry &&
    entry.source === env.CRABBOX_GCP_CREDENTIAL_SOURCE &&
    entry.email === env.GCP_CLIENT_EMAIL &&
    entry.key === env.GCP_PRIVATE_KEY
  )
    return entry.cache;
  const cache = new ExpiringTokenCache();
  caches.set(env, {
    source: env.CRABBOX_GCP_CREDENTIAL_SOURCE,
    email: env.GCP_CLIENT_EMAIL,
    key: env.GCP_PRIVATE_KEY,
    cache,
  });
  return cache;
}
