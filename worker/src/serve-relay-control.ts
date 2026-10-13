import type { CoordinatorStorageView } from "./coordinator-runtime";
import { sha256Hex } from "./encoding";
import { json, requestOwner } from "./http";
import { putLeaseRecord } from "./lease-record-store";
import { requestOrg } from "./org-identity";
import type { ServeMachine } from "./serve";
import { serveRelayBackend, serveRelayID, type ServeRelayDescriptor } from "./serve-relay";
import { timingSafeEqual } from "./timing-safe";
import type { Env, LeaseRecord } from "./types";

interface RelayRecord extends ServeRelayDescriptor {
  leaseID: string;
  machineID: string;
  owner: string;
  org: string;
  tokenHash: string;
  clientAttached?: boolean;
  agentAttached?: boolean;
}
function activeRelayLease(l: LeaseRecord | undefined, now = Date.now()): l is LeaseRecord {
  return Boolean(
    l &&
    l.provider === "serve" &&
    l.state === "active" &&
    Date.parse(l.expiresAt) > now &&
    Date.parse(l.lastTouchedAt ?? l.createdAt) + (l.idleTimeoutSeconds ?? 1800) * 1000 > now,
  );
}
const notAllowed = () => json({ error: "serve_relay_not_allowed" }, { status: 403 });
const expired = () => json({ error: "serve_relay_expired" }, { status: 410 });
export class ServeRelayControl {
  constructor(private readonly env: Env) {}
  private async key(owner: string, _org: string): Promise<string> {
    return `serve-relay-owner:${await sha256Hex(JSON.stringify([owner]))}`;
  }
  private async rows(
    s: CoordinatorStorageView,
    owner: string,
    org: string,
  ): Promise<RelayRecord[]> {
    const rows = (await s.get<RelayRecord[]>(await this.key(owner, org))) ?? [];
    if (rows.length > 8) throw new Error("invalid serve relay index");
    return rows.filter((row) => row.owner === owner);
  }
  async mint(s: CoordinatorStorageView, r: Request, leaseID: string): Promise<Response> {
    const backend = serveRelayBackend(this.env);
    if (!backend) return json({ error: "serve_relay_unavailable" }, { status: 501 });
    const owner = requestOwner(r),
      org = requestOrg(r, this.env),
      now = Date.now();
    const l = await s.get<LeaseRecord>(`lease:${leaseID}`);
    if (
      !activeRelayLease(l, now) ||
      l.owner !== owner ||
      l.org !== org ||
      l.state !== "active" ||
      !l.serve ||
      Date.parse(l.expiresAt) <= now
    )
      return notAllowed();
    const m = await s.get<ServeMachine>(`serve-machine:${l.serve.machineID}`);
    if (
      !m ||
      m.owner !== owner ||
      m.org !== org ||
      now - m.lastHeartbeat >= 30_000 ||
      !m.frontDoor.relay
    )
      return notAllowed();
    const rows = (
      await Promise.all(
        (await this.rows(s, owner, org)).map(async (row) => {
          if (row.deadline <= now || !(await backend.alive(row.sessionID))) {
            await backend.close(row.sessionID);
            return undefined;
          }
          return row;
        }),
      )
    ).filter((row): row is RelayRecord => Boolean(row));
    if (rows.length >= 8 || rows.filter((row) => row.leaseID === leaseID).length >= 4)
      return json({ error: "serve_relay_capacity" }, { status: 429 });
    const token = crypto.randomUUID().replaceAll("-", "");
    const descriptor: ServeRelayDescriptor = {
      sessionID: crypto.randomUUID().replaceAll("-", ""),
      deadline: Date.parse(l.expiresAt),
      connectBy: Math.min(now + 60_000, Date.parse(l.expiresAt)),
    };
    try {
      await backend.prepare(descriptor, token);
    } catch (error) {
      if (error instanceof Error && error.message === "serve_relay_capacity")
        return json({ error: "serve_relay_capacity" }, { status: 429 });
      throw error;
    }
    const record: RelayRecord = {
      ...descriptor,
      owner,
      org,
      leaseID,
      machineID: m.machineID,
      tokenHash: await sha256Hex(token),
    };
    await s.put(await this.key(owner, org), [...rows, record]);
    l.serveRelaySessions = [
      ...rows.filter((row) => row.leaseID === leaseID).map((row) => row.sessionID),
      record.sessionID,
    ];
    await putLeaseRecord(s, l);
    return json({ sessionID: record.sessionID, connectBy: record.connectBy });
  }
  async authorize(
    s: CoordinatorStorageView,
    r: Request,
    id: string,
    role: string,
  ): Promise<Response> {
    const backend = serveRelayBackend(this.env);
    if (
      !backend ||
      !serveRelayID.test(id) ||
      !["agent", "client"].includes(role) ||
      r.headers.get("upgrade")?.toLowerCase() !== "websocket"
    )
      return notAllowed();
    const owner = requestOwner(r),
      org = requestOrg(r, this.env);
    const rows = await this.rows(s, owner, org),
      row = rows.find((candidate) => candidate.sessionID === id);
    if (!row || row.org !== org) return notAllowed();
    if (Date.now() >= row.connectBy || Date.now() >= row.deadline || !(await backend.alive(id)))
      return expired();
    const l = await s.get<LeaseRecord>(`lease:${row.leaseID}`);
    const m = await s.get<ServeMachine>(`serve-machine:${row.machineID}`);
    if (
      !activeRelayLease(l) ||
      l.owner !== owner ||
      l.org !== org ||
      l.serve?.machineID !== row.machineID ||
      !m ||
      m.owner !== owner ||
      m.org !== org ||
      Date.now() - m.lastHeartbeat >= 30_000
    )
      return notAllowed();
    if (role === "agent") {
      const token = r.headers.get("x-crabbox-relay-token") ?? "";
      if (
        row.agentAttached ||
        r.headers.get("x-crabbox-serve-machine") !== row.machineID ||
        !serveRelayID.test(token) ||
        !timingSafeEqual(await sha256Hex(token), row.tokenHash)
      )
        return notAllowed();
      row.agentAttached = true;
    } else {
      if (row.clientAttached) return notAllowed();
      row.clientAttached = true;
    }
    await s.put(await this.key(owner, org), rows);
    return json({ ok: true });
  }
  async pending(
    s: CoordinatorStorageView,
    owner: string,
    org: string,
    machineID: string,
  ): Promise<{ sessionID: string; leaseID: string; agentToken: string }[]> {
    const backend = serveRelayBackend(this.env);
    if (!backend) return [];
    const rows = await this.rows(s, owner, org);
    const pending = [];
    // Eight owner-scoped sessions is the bound, independent of fleet size.
    // oxlint-disable eslint/no-await-in-loop
    for (const row of rows) {
      if (row.org !== org || row.machineID !== machineID) continue;
      const l = await s.get<LeaseRecord>(`lease:${row.leaseID}`);
      if (
        !activeRelayLease(l) ||
        l.owner !== owner ||
        l.org !== org ||
        l.serve?.machineID !== machineID ||
        row.deadline <= Date.now()
      ) {
        await backend.close(row.sessionID);
        continue;
      }
      if (!row.agentAttached && row.connectBy > Date.now()) {
        const token = await backend.pendingToken(row.sessionID);
        if (token)
          pending.push({ sessionID: row.sessionID, leaseID: row.leaseID, agentToken: token });
      }
    }
    // oxlint-enable eslint/no-await-in-loop
    return pending;
  }
  async closeLease(l: LeaseRecord): Promise<void> {
    const backend = serveRelayBackend(this.env);
    await Promise.all((l.serveRelaySessions ?? []).slice(0, 4).map((id) => backend?.close(id)));
    delete l.serveRelaySessions;
  }
}
