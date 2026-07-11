import { sha256Hex } from "./auth";
import type { CoordinatorRuntime } from "./coordinator-runtime";

const ticketPrefix = "vnc_handoff_";
const storagePrefix = "webvnc-credential-handoff:";
const ttlSeconds = 5 * 60;
const recordVersion = 1;
const lookupContext = "crabbox-webvnc-handoff-lookup-v1";
const sealContext = "crabbox-webvnc-handoff-seal-v1";
const encoder = new TextEncoder();
const decoder = new TextDecoder("utf-8", { fatal: true, ignoreBOM: false });

interface WebVNCCredentialHandoffRecord {
  version: typeof recordVersion;
  leaseID: string;
  expiresAt: string;
  iv: string;
  ciphertext: string;
}

export type WebVNCCredentialHandoffResult =
  | { status: "accepted"; username: string; password: string }
  | { status: "expired" | "invalid" };

export class WebVNCCredentialHandoffs {
  constructor(private readonly runtime: CoordinatorRuntime) {}

  async issue(
    leaseID: string,
    credentials: { username: string; password: string },
  ): Promise<{ ticket: string; expiresAt: string }> {
    await this.cleanupExpired();
    const now = new Date();
    const ticket = newTicket();
    const expiresAt = new Date(now.getTime() + ttlSeconds * 1000).toISOString();
    const record: WebVNCCredentialHandoffRecord = {
      version: recordVersion,
      leaseID,
      expiresAt,
      ...(await sealCredentials(ticket, leaseID, expiresAt, credentials)),
    };
    await this.runtime.storage.put(await storageKey(leaseID, ticket), record);
    const alarmAt = Date.parse(record.expiresAt);
    const currentAlarm = await this.runtime.getAlarm();
    if (currentAlarm === undefined || alarmAt < currentAlarm) {
      await this.runtime.scheduleAlarm(alarmAt);
    }
    return { ticket, expiresAt: record.expiresAt };
  }

  async consume(leaseID: string, ticket: string): Promise<WebVNCCredentialHandoffResult> {
    if (!validTicket(ticket)) {
      return { status: "invalid" };
    }
    const record = await this.runtime.take<WebVNCCredentialHandoffRecord>(
      await storageKey(leaseID, ticket),
    );
    if (!validRecord(record, leaseID)) {
      return { status: "invalid" };
    }
    if (Date.parse(record.expiresAt) <= Date.now()) {
      return { status: "expired" };
    }
    const credentials = await openCredentials(ticket, record);
    return credentials ? { status: "accepted", ...credentials } : { status: "invalid" };
  }

  async cleanupExpired(now = Date.now()): Promise<void> {
    const handoffs = await this.records();
    await Promise.all(
      [...handoffs.entries()]
        .filter(([, handoff]) => Date.parse(handoff.expiresAt) <= now)
        .map(([key]) => this.runtime.storage.delete(key)),
    );
  }

  async alarmTimes(now = Date.now()): Promise<number[]> {
    return [...(await this.records()).values()]
      .map((handoff) => Date.parse(handoff.expiresAt))
      .filter((time) => Number.isFinite(time) && time > now);
  }

  private records(): Promise<Map<string, WebVNCCredentialHandoffRecord>> {
    return this.runtime.storage.list<WebVNCCredentialHandoffRecord>({ prefix: storagePrefix });
  }
}

function newTicket(): string {
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  return `${ticketPrefix}${[...bytes].map((byte) => byte.toString(16).padStart(2, "0")).join("")}`;
}

function validTicket(value: string): boolean {
  return /^vnc_handoff_[a-f0-9]{32}$/.test(value);
}

async function storageKey(leaseID: string, ticket: string): Promise<string> {
  // Keep lookup and sealing derivations distinct: storage readers know this digest.
  const digest = await sha256Hex(`${lookupContext}\0${ticket}`);
  return `${storagePrefix}${leaseID}:${digest}`;
}

async function sealCredentials(
  ticket: string,
  leaseID: string,
  expiresAt: string,
  credentials: { username: string; password: string },
): Promise<Pick<WebVNCCredentialHandoffRecord, "iv" | "ciphertext">> {
  const iv = crypto.getRandomValues(new Uint8Array(12));
  const ciphertext = await crypto.subtle.encrypt(
    { name: "AES-GCM", iv, additionalData: recordAAD(leaseID, expiresAt) },
    await credentialKey(ticket, ["encrypt"]),
    encoder.encode(JSON.stringify(credentials)),
  );
  return { iv: encodeHex(iv), ciphertext: encodeHex(new Uint8Array(ciphertext)) };
}

async function openCredentials(
  ticket: string,
  record: WebVNCCredentialHandoffRecord,
): Promise<{ username: string; password: string } | undefined> {
  try {
    const plaintext = await crypto.subtle.decrypt(
      {
        name: "AES-GCM",
        iv: decodeHex(record.iv),
        additionalData: recordAAD(record.leaseID, record.expiresAt),
      },
      await credentialKey(ticket, ["decrypt"]),
      decodeHex(record.ciphertext),
    );
    const credentials = JSON.parse(decoder.decode(plaintext)) as unknown;
    if (!credentials || typeof credentials !== "object" || Array.isArray(credentials)) {
      return undefined;
    }
    const { username, password } = credentials as Record<string, unknown>;
    if (
      typeof username !== "string" ||
      typeof password !== "string" ||
      (!username && !password) ||
      username.length > 256 ||
      password.length > 1024
    ) {
      return undefined;
    }
    return { username, password };
  } catch {
    return undefined;
  }
}

async function credentialKey(
  ticket: string,
  usages: Array<"encrypt" | "decrypt">,
): Promise<CryptoKey> {
  const material = await crypto.subtle.digest(
    "SHA-256",
    encoder.encode(`${sealContext}\0${ticket}`),
  );
  return crypto.subtle.importKey("raw", material, "AES-GCM", false, usages);
}

function recordAAD(leaseID: string, expiresAt: string): Uint8Array {
  return encoder.encode(JSON.stringify([recordVersion, leaseID, expiresAt]));
}

function validRecord(
  record: WebVNCCredentialHandoffRecord | undefined,
  leaseID: string,
): record is WebVNCCredentialHandoffRecord {
  return Boolean(
    record &&
    record.version === recordVersion &&
    record.leaseID === leaseID &&
    typeof record.expiresAt === "string" &&
    Number.isFinite(Date.parse(record.expiresAt)) &&
    validHex(record.iv, 24) &&
    validHex(record.ciphertext) &&
    record.ciphertext.length > 32,
  );
}

function validHex(value: unknown, exactLength?: number): value is string {
  return (
    typeof value === "string" &&
    (exactLength === undefined || value.length === exactLength) &&
    value.length % 2 === 0 &&
    /^[a-f0-9]+$/.test(value)
  );
}

function encodeHex(value: Uint8Array): string {
  return [...value].map((byte) => byte.toString(16).padStart(2, "0")).join("");
}

function decodeHex(value: string): Uint8Array {
  return Uint8Array.from(value.match(/.{2}/g) ?? [], (byte) => Number.parseInt(byte, 16));
}
