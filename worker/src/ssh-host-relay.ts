// oxlint-disable eslint/no-await-in-loop -- bounded relay operations are serialized with custody updates.
import type { CoordinatorStorageView } from "./coordinator-runtime";
import { sha256Hex } from "./encoding";
import { json, requestOwner } from "./http";
import { putLeaseRecord } from "./lease-record-store";
import { requestOrg } from "./org-identity";
import { portablePoolGrantKey, type PoolAccessGrant } from "./ready-pool-access";
import { serveRelayBackend, type ServeRelayDescriptor } from "./serve-relay";
import { hostPrefix, hostOnlineMS, type SSHHost } from "./ssh-host";
import { timingSafeEqual } from "./timing-safe";
import type { Env, LeaseRecord } from "./types";
interface HostRelay extends ServeRelayDescriptor {
  owner: string;
  org: string;
  operator: string;
  leaseID: string;
  hostID: string;
  generation: number;
  accessGeneration: number;
  tokenHash: string;
  clientAttached?: boolean;
  agentAttached?: boolean;
}
const prefix = "ssh-host-relay:";
const denied = () => json({ error: "serve_relay_not_allowed" }, { status: 403 });
async function borrower(s: CoordinatorStorageView, l: LeaseRecord): Promise<string | undefined> {
  if (!l.portablePoolAccess) return l.owner;
  const g = await s.get<PoolAccessGrant>(portablePoolGrantKey(l.id));
  return g?.state === "active" &&
    g.generation === l.sshHostAllocation?.accessGeneration &&
    Date.parse(g.expiresAt) > Date.now()
    ? g.owner
    : undefined;
}
async function active(
  s: CoordinatorStorageView,
  l: LeaseRecord | undefined,
): Promise<SSHHost | undefined> {
  const a = l?.sshHostAllocation;
  if (
    !l ||
    l.provider !== "ssh-host" ||
    l.state !== "active" ||
    !a ||
    a.operation !== "prepare" ||
    a.readyGeneration !== a.accessGeneration ||
    Date.parse(l.expiresAt) <= Date.now() ||
    Date.parse(a.accessDeadline) <= Date.now() ||
    Date.parse(l.lastTouchedAt ?? l.createdAt) + (l.idleTimeoutSeconds ?? 1800) * 1000 <= Date.now()
  )
    return;
  const h = await s.get<SSHHost>(hostPrefix + a.hostID);
  if (
    h &&
    h.owner === a.operator &&
    h.org === l.org &&
    h.leaseID === l.id &&
    h.generation === a.generation &&
    !h.quarantine &&
    Date.now() - h.lastHeartbeat < hostOnlineMS
  )
    return h;
  return undefined;
}
export async function mintHostRelay(
  s: CoordinatorStorageView,
  env: Env,
  r: Request,
  l: LeaseRecord,
): Promise<Response> {
  const backend = serveRelayBackend(env),
    h = await active(s, l);
  if (!backend) return json({ error: "serve_relay_unavailable" }, { status: 501 });
  if (!h || l.org !== requestOrg(r, env) || (await borrower(s, l)) !== requestOwner(r))
    return denied();
  const sessions: string[] = [];
  for (const id of (l.serveRelaySessions ?? []).slice(0, 4)) {
    if (await backend.alive(id)) sessions.push(id);
    else await s.delete(prefix + id);
  }
  if (sessions.length >= 4) return json({ error: "serve_relay_capacity" }, { status: 429 });
  const token = crypto.randomUUID().replaceAll("-", "");
  const a = l.sshHostAllocation!;
  const deadline = Math.min(Date.parse(l.expiresAt), Date.parse(a.accessDeadline));
  const record: HostRelay = {
    sessionID: crypto.randomUUID().replaceAll("-", ""),
    deadline,
    connectBy: Math.min(deadline, Date.now() + 60_000),
    owner: requestOwner(r),
    org: l.org,
    operator: h.owner,
    leaseID: l.id,
    hostID: h.id,
    generation: a.generation,
    accessGeneration: a.accessGeneration,
    tokenHash: await sha256Hex(token),
  };
  await backend.prepare(record, token);
  await s.put(prefix + record.sessionID, record);
  l.serveRelaySessions = [...sessions, record.sessionID];
  await putLeaseRecord(s, l);
  return json({ sessionID: record.sessionID, connectBy: record.connectBy });
}
export async function authorizeHostRelay(
  s: CoordinatorStorageView,
  env: Env,
  r: Request,
  id: string,
  role: string,
): Promise<Response | undefined> {
  const row = await s.get<HostRelay>(prefix + id);
  if (!row) return;
  const backend = serveRelayBackend(env);
  if (
    !backend ||
    r.headers.get("upgrade")?.toLowerCase() !== "websocket" ||
    !["agent", "client"].includes(role) ||
    row.org !== requestOrg(r, env) ||
    requestOwner(r) !== (role === "agent" ? row.operator : row.owner) ||
    Date.now() >= row.connectBy ||
    Date.now() >= row.deadline ||
    !(await backend.alive(id))
  )
    return denied();
  const l = await s.get<LeaseRecord>(`lease:${row.leaseID}`),
    h = await active(s, l);
  if (
    !l ||
    !h ||
    h.id !== row.hostID ||
    h.owner !== row.operator ||
    h.generation !== row.generation ||
    l.sshHostAllocation?.accessGeneration !== row.accessGeneration ||
    (await borrower(s, l)) !== row.owner
  )
    return denied();
  if (role === "agent") {
    const token = r.headers.get("x-crabbox-relay-token") ?? "";
    if (
      row.agentAttached ||
      r.headers.get("x-crabbox-serve-machine") !== row.hostID ||
      !timingSafeEqual(await sha256Hex(token), row.tokenHash)
    )
      return denied();
    row.agentAttached = true;
  } else {
    if (row.clientAttached) return denied();
    row.clientAttached = true;
  }
  await s.put(prefix + id, row);
  return json({ ok: true });
}
export async function pendingHostRelays(s: CoordinatorStorageView, env: Env, l: LeaseRecord) {
  const backend = serveRelayBackend(env);
  const pending: { sessionID: string; leaseID: string; agentToken: string }[] = [];
  if (!backend || !(await active(s, l))) return pending;
  for (const id of (l.serveRelaySessions ?? []).slice(0, 4)) {
    const row = await s.get<HostRelay>(prefix + id);
    if (
      !row ||
      row.generation !== l.sshHostAllocation?.generation ||
      row.accessGeneration !== l.sshHostAllocation.accessGeneration ||
      row.agentAttached ||
      row.connectBy <= Date.now()
    )
      continue;
    const token = await backend.pendingToken(id);
    if (token) pending.push({ sessionID: id, leaseID: l.id, agentToken: token });
  }
  return pending;
}
export async function closeHostRelays(s: CoordinatorStorageView, env: Env, l: LeaseRecord) {
  const backend = serveRelayBackend(env);
  for (const id of (l.serveRelaySessions ?? []).slice(0, 4)) {
    await backend?.close(id);
    await s.delete(prefix + id);
  }
  delete l.serveRelaySessions;
}

export async function isHostRelayRequest(
  s: CoordinatorStorageView,
  parts: string[],
): Promise<boolean> {
  if (parts.length !== 5) return false;
  if (parts[2] === "relay") return Boolean(await s.get(prefix + parts[3]));
  if (parts[2] === "leases" && parts[4] === "relay")
    return (await s.get<LeaseRecord>(`lease:${parts[3]}`))?.provider === "ssh-host";
  return false;
}
