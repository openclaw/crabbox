import { base64ToBytes, bytesToBase64 } from "./encoding";
import type { Env, LeaseRecord } from "./types";

export const leaseListPagination = "keyset-v1";
export const leaseListPageSize = 100;

// Project before redaction clones the record, so histories never enter the response graph.
export function leaseListSummary(lease: LeaseRecord): LeaseRecord {
  const {
    creationEvents: _events,
    telemetryHistory: _telemetry,
    provisioningAttempts: _attempts,
    ...summary
  } = lease;
  return summary;
}

const encoder = new TextEncoder();
const cursorAAD = encoder.encode("crabbox/lease-list/keyset-v1");

export function leaseListCursorSecret(env: Env): string | undefined {
  return (
    env.CRABBOX_SESSION_SECRET ||
    env.CRABBOX_ADMIN_TOKEN ||
    env.CRABBOX_SHARED_TOKEN ||
    env.CRABBOX_TRUSTED_PROXY_SECRET
  );
}

async function cursorKey(secret: string): Promise<CryptoKey> {
  const bytes = await crypto.subtle.digest(
    "SHA-256",
    encoder.encode(`crabbox/lease-list/keyset-v1\0${secret}`),
  );
  return crypto.subtle.importKey("raw", bytes, "AES-GCM", false, ["encrypt", "decrypt"]);
}

// A scan may end on an invisible row. Never disclose its ID in a plaintext cursor.
export async function sealLeaseListCursor(key: string, secret: string): Promise<string> {
  const iv = crypto.getRandomValues(new Uint8Array(12));
  const ciphertext = await crypto.subtle.encrypt(
    { name: "AES-GCM", iv, additionalData: cursorAAD },
    await cursorKey(secret),
    encoder.encode(key),
  );
  return bytesToBase64(new Uint8Array([...iv, ...new Uint8Array(ciphertext)]));
}

export async function openLeaseListCursor(
  cursor: string,
  secret: string,
): Promise<string | undefined> {
  try {
    if (cursor.length > 512) return undefined;
    const bytes = base64ToBytes(cursor);
    const plaintext = await crypto.subtle.decrypt(
      { name: "AES-GCM", iv: bytes.slice(0, 12), additionalData: cursorAAD },
      await cursorKey(secret),
      bytes.slice(12),
    );
    const key = new TextDecoder().decode(plaintext);
    return /^lease:cbx_[a-f0-9]{12}$/.test(key) ? key : undefined;
  } catch {
    return undefined;
  }
}
