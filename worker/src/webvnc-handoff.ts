import type { CoordinatorRuntime } from "./coordinator-runtime";

const ticketPrefix = "vnc_handoff_";
const storagePrefix = "webvnc-credential-handoff:";
const ttlSeconds = 5 * 60;

interface WebVNCCredentialHandoffRecord {
  ticket: string;
  leaseID: string;
  username: string;
  password: string;
  createdAt: string;
  expiresAt: string;
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
    const record: WebVNCCredentialHandoffRecord = {
      ticket: newTicket(),
      leaseID,
      ...credentials,
      createdAt: now.toISOString(),
      expiresAt: new Date(now.getTime() + ttlSeconds * 1000).toISOString(),
    };
    await this.runtime.storage.put(storageKey(leaseID, record.ticket), record);
    const expiresAt = Date.parse(record.expiresAt);
    const currentAlarm = await this.runtime.getAlarm();
    if (currentAlarm === undefined || expiresAt < currentAlarm) {
      await this.runtime.scheduleAlarm(expiresAt);
    }
    return { ticket: record.ticket, expiresAt: record.expiresAt };
  }

  async consume(leaseID: string, ticket: string): Promise<WebVNCCredentialHandoffResult> {
    if (!validTicket(ticket)) {
      return { status: "invalid" };
    }
    const record = await this.runtime.take<WebVNCCredentialHandoffRecord>(
      storageKey(leaseID, ticket),
    );
    if (!record || record.ticket !== ticket || record.leaseID !== leaseID) {
      return { status: "invalid" };
    }
    if (Date.parse(record.expiresAt) <= Date.now()) {
      return { status: "expired" };
    }
    return { status: "accepted", username: record.username, password: record.password };
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

function storageKey(leaseID: string, ticket: string): string {
  return `${storagePrefix}${leaseID}:${ticket}`;
}
