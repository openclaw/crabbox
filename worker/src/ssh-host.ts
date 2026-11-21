import { isAdminRequest } from "./auth";
import { clampLeaseDurations, resolveLeasePolicy } from "./config";
import type { CoordinatorRuntime, CoordinatorStorageView } from "./coordinator-runtime";
import { sha256Hex } from "./encoding";
import { json, readJson, requestOwner } from "./http";
import { completeLeaseProviderCleanup, leaseProviderCleanupConfirmed } from "./lease-cleanup";
import { putLeaseRecord } from "./lease-record-store";
import { requestOrg } from "./org-identity";
import { publicLeaseRecord } from "./org-records";
import { poolPublicKey } from "./ready-pool-access";
import { hostKeyFingerprint } from "./serve";
import {
  authenticateHostCredential,
  hostCredentialCurrent,
  hostUsesCredential,
  hostCredentialRequest,
  hostCredentialRouteAllowed,
  sameHostCredential,
  validHostCredential,
  type HostCredential,
  type HostCleanupBinding,
} from "./ssh-host-auth";
import { pendingHostRelays, closeHostRelays } from "./ssh-host-relay";
import type { Env, LeaseRecord, LeaseRequest, CreateAttemptRecord, TargetOS } from "./types";

export const hostPrefix = "ssh-host:";
export const hostOnlineMS = 30_000;
const idPattern = /^[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$/;
const leasePattern = /^cbx_[a-f0-9]{12}$/;
const terminal = (l: LeaseRecord) => l.state !== "active" && l.state !== "provisioning";
export interface HostAllocation {
  hostID: string;
  generation: number;
  intent: string;
  operator: string;
  template: string;
  publicKey: string;
  operation: "prepare" | "clean" | "idle";
  accessGeneration: number;
  accessDeadline: string;
  readyGeneration?: number;
  cleanGeneration?: number;
}
export interface SSHHost {
  id: string;
  name: string;
  owner: string;
  org: string;
  target: TargetOS;
  architecture: "amd64" | "arm64";
  capabilities: string[];
  template: string;
  runtime: "native-user" | "mxc-isolation-session";
  enabled: boolean;
  draining: boolean;
  lastHeartbeat: number;
  generation: number;
  leaseID?: string;
  quarantine?: string;
  frontDoor?: { hostKey: string; fingerprint: string };
  authMode?: "host";
  credential?: HostCredential;
}
function completedHostCleanup(
  h: SSHHost,
  l: LeaseRecord | undefined,
  binding: HostCleanupBinding,
): boolean {
  const a = l?.sshHostAllocation;
  return Boolean(
    !h.leaseID &&
    l &&
    terminal(l) &&
    leaseProviderCleanupConfirmed(l) &&
    l.org === h.org &&
    l.id === binding.leaseID &&
    a?.hostID === h.id &&
    a.operator === h.owner &&
    h.generation === binding.generation &&
    a.generation === binding.generation &&
    a.accessGeneration === binding.accessGeneration &&
    a.cleanGeneration === binding.accessGeneration &&
    a.operation === "idle",
  );
}
interface HostReport {
  leaseID: string;
  generation: number;
  accessGeneration: number;
  state: "ready" | "clean" | "failed";
  sshHostKey?: string;
  sshUser?: string;
  workRoot?: string;
}
const textValue = (v: unknown, max = 128): v is string =>
  // oxlint-disable-next-line eslint/no-control-regex -- reject control characters in public metadata.
  typeof v === "string" && v.length > 0 && v.length <= max && !/[\x00-\x1f\x7f]/u.test(v);
function publicHost(h: SSHHost) {
  return {
    id: h.id,
    name: h.name,
    target: h.target,
    architecture: h.architecture,
    capabilities: h.capabilities,
    template: h.template,
    runtime: h.runtime,
    authMode: hostUsesCredential(h) ? "host" : "operator",
    enabled: h.enabled,
    draining: h.draining,
    online: hostCredentialCurrent(h) && Date.now() - h.lastHeartbeat < hostOnlineMS,
    ...(h.credential
      ? {
          credential: {
            mode: "host",
            purpose: h.credential.purpose,
            version: h.credential.version,
            generation: h.credential.generation,
            expiresAt: h.credential.expiresAt,
            revoked: Boolean(h.credential.revokedAt),
          },
        }
      : {}),
    state: h.quarantine ? "quarantined" : h.leaseID ? "allocated" : "available",
    ...(h.quarantine ? { reason: h.quarantine } : {}),
  };
}

// An inventory/provider adapter inside Fleet, not a second scheduler. Allocation
// and cleanup receipts use the same storage transaction and normal lease records.
export class SSHHostCoordinator {
  constructor(
    private readonly runtime: CoordinatorRuntime,
    private readonly env: Env,
  ) {}
  private commit<T>(fn: (s: CoordinatorStorageView) => Promise<T>): Promise<T> {
    return this.runtime.runExclusive(() =>
      this.runtime.provisioning
        ? this.runtime.provisioning.commitAndWake(fn)
        : this.runtime.storage.transaction(fn),
    );
  }
  async route(r: Request, parts: string[]): Promise<Response> {
    if (hostCredentialRequest(r)) {
      if (!hostCredentialRouteAllowed(r) || parts.length !== 4 || parts[3] !== "heartbeat")
        return json({ error: "host_scope_forbidden" }, { status: 403 });
      return this.heartbeat(r, parts[2]!, await readJson(r));
    }
    if (!requestOwner(r) || requestOwner(r) === "unknown")
      return json({ error: "identity_required" }, { status: 403 });
    if (parts.length === 2 && r.method === "GET") {
      const hosts = await this.runtime.storage.list<SSHHost>({ prefix: hostPrefix, limit: 1024 });
      return json({
        hosts: [...hosts.values()].filter((h) => h.org === requestOrg(r, this.env)).map(publicHost),
      });
    }
    if (parts.length === 2 && r.method === "POST") return this.enroll(r, await readJson(r));
    if (!idPattern.test(parts[2] ?? "")) return json({ error: "not_found" }, { status: 404 });
    if (parts.length === 4 && parts[3] === "heartbeat" && r.method === "POST")
      return this.heartbeat(r, parts[2]!, await readJson(r));
    if (
      parts.length === 5 &&
      parts[3] === "credential" &&
      r.method === "POST" &&
      ["revoke", "reissue", "recover"].includes(parts[4]!)
    )
      return this.changeCredential(r, parts[2]!, parts[4]!, await readJson(r));
    if (parts.length === 3 && (r.method === "PATCH" || r.method === "DELETE")) {
      if (!isAdminRequest(r)) return json({ error: "forbidden" }, { status: 403 });
      const input =
        r.method === "DELETE" ? {} : await readJson<{ enabled?: boolean; draining?: boolean }>(r);
      return this.commit(async (s) => {
        const h = await s.get<SSHHost>(hostPrefix + parts[2]);
        if (!h || h.org !== requestOrg(r, this.env))
          return json({ error: "not_found" }, { status: 404 });
        if (r.method === "DELETE") {
          if (h.leaseID || h.quarantine)
            return json({ error: "host_cleanup_required" }, { status: 409 });
          await s.delete(hostPrefix + h.id);
          return json({ removed: h.id });
        }
        if (
          (input.enabled !== undefined && typeof input.enabled !== "boolean") ||
          (input.draining !== undefined && typeof input.draining !== "boolean")
        )
          return json({ error: "invalid_host_policy" }, { status: 400 });
        if (input.enabled && !hostCredentialCurrent(h))
          return json({ error: "host_credential_inactive" }, { status: 409 });
        if (input.enabled !== undefined) h.enabled = input.enabled;
        if (input.draining !== undefined) h.draining = input.draining;
        await s.put(hostPrefix + h.id, h);
        return json({ host: publicHost(h) });
      });
    }
    return json({ error: "not_found" }, { status: 404 });
  }
  private async enroll(
    r: Request,
    input: Partial<SSHHost> & { hostKey?: string },
  ): Promise<Response> {
    if (!isAdminRequest(r)) return json({ error: "forbidden" }, { status: 403 });
    if (
      !idPattern.test(input.id ?? "") ||
      !textValue(input.name) ||
      !["linux", "windows"].includes(input.target ?? "") ||
      !["amd64", "arm64"].includes(input.architecture ?? "") ||
      !textValue(input.template, 256) ||
      !Array.isArray(input.capabilities) ||
      input.capabilities.length > 32 ||
      !input.capabilities.every((c) => textValue(c, 64)) ||
      (input.target === "windows"
        ? input.runtime !== "mxc-isolation-session"
        : input.runtime !== "native-user")
    )
      return json({ error: "invalid_host" }, { status: 400 });
    let frontDoor: SSHHost["frontDoor"];
    if (input.credential !== undefined) {
      if (!validHostCredential(input.credential, 1) || !textValue(input.hostKey, 1024))
        return json({ error: "invalid_host_credential" }, { status: 400 });
      try {
        frontDoor = {
          hostKey: input.hostKey,
          fingerprint: await hostKeyFingerprint(input.hostKey),
        };
      } catch {
        return json({ error: "invalid_host_key" }, { status: 400 });
      }
    } else if (input.hostKey !== undefined)
      return json({ error: "invalid_host_credential" }, { status: 400 });
    return this.commit(async (s) => {
      const key = hostPrefix + input.id;
      const prior = await s.get<SSHHost>(key);
      if (prior) {
        if (
          !sameHostCredential(prior.credential, input.credential) ||
          (input.credential !== undefined && prior.frontDoor?.hostKey !== input.hostKey) ||
          prior.owner !== requestOwner(r) ||
          prior.org !== requestOrg(r, this.env) ||
          prior.name !== input.name ||
          prior.target !== input.target ||
          prior.architecture !== input.architecture ||
          prior.template !== input.template ||
          prior.runtime !== input.runtime ||
          JSON.stringify(prior.capabilities) !== JSON.stringify(input.capabilities)
        )
          return json({ error: "host_enrollment_conflict" }, { status: 409 });
        return json({
          host: publicHost(prior),
          ...(prior.credential ? { identity: this.identity(prior) } : {}),
        });
      }
      const inventory = await s.list({ prefix: hostPrefix, limit: 1024 });
      if (inventory.size >= 1024) return json({ error: "host_inventory_limit" }, { status: 409 });
      const host: SSHHost = {
        id: input.id!,
        name: input.name!,
        owner: requestOwner(r),
        org: requestOrg(r, this.env),
        target: input.target!,
        architecture: input.architecture!,
        capabilities: input.capabilities!,
        template: input.template!,
        runtime: input.runtime!,
        enabled: false,
        draining: false,
        lastHeartbeat: 0,
        generation: 0,
        ...(input.credential
          ? {
              authMode: "host",
              credential: {
                purpose: "agent",
                version: 1,
                generation: 1,
                tokenHash: input.credential.tokenHash,
                expiresAt: input.credential.expiresAt,
              },
              frontDoor: frontDoor!,
            }
          : {}),
      };
      await s.put(key, host);
      return json(
        { host: publicHost(host), ...(host.credential ? { identity: this.identity(host) } : {}) },
        { status: 201 },
      );
    });
  }
  private identity(h: SSHHost) {
    return {
      id: h.id,
      owner: h.owner,
      org: h.org,
      hostKey: h.frontDoor!.hostKey,
      purpose: h.credential!.purpose,
      ...(h.credential!.cleanup ? { cleanup: h.credential!.cleanup } : {}),
      generation: h.credential!.generation,
      expiresAt: h.credential!.expiresAt,
    };
  }
  private async changeCredential(
    r: Request,
    id: string,
    action: string,
    input: { expectedGeneration?: number; credential?: HostCredential; hostKey?: string },
  ): Promise<Response> {
    if (!isAdminRequest(r)) return json({ error: "forbidden" }, { status: 403 });
    const response = await this.commit(async (s) => {
      const h = await s.get<SSHHost>(hostPrefix + id);
      if (!h || h.org !== requestOrg(r, this.env))
        return json({ error: "not_found" }, { status: 404 });
      if (!h.credential) return json({ error: "host_credential_not_scoped" }, { status: 409 });
      const recoveryLeaseID = action === "recover" ? input.credential?.cleanup?.leaseID : undefined;
      const leaseID = h.leaseID ?? recoveryLeaseID;
      const l = leaseID ? await s.get<LeaseRecord>(`lease:${leaseID}`) : undefined;
      if (action === "reissue" || action === "recover") {
        if (!h.draining || h.enabled)
          return json({ error: "host_cleanup_required" }, { status: 409 });
        const purpose = action === "recover" ? "cleanup" : "agent";
        if (
          !Number.isSafeInteger(input.expectedGeneration) ||
          !validHostCredential(input.credential, input.expectedGeneration! + 1, purpose) ||
          input.hostKey !== h.frontDoor?.hostKey
        )
          return json({ error: "invalid_host_credential" }, { status: 400 });
        if (purpose === "cleanup") {
          const bound = input.credential.cleanup!;
          const a = l?.sshHostAllocation;
          if (
            !completedHostCleanup(h, l, bound) &&
            (!l ||
              !terminal(l) ||
              !a ||
              a.operation !== "clean" ||
              l.id !== bound.leaseID ||
              a.hostID !== h.id ||
              h.generation !== bound.generation ||
              a.generation !== bound.generation ||
              a.accessGeneration !== bound.accessGeneration)
          )
            return json({ error: "host_recovery_custody_mismatch" }, { status: 409 });
        } else if (h.leaseID || h.quarantine)
          return json({ error: "host_cleanup_required" }, { status: 409 });
        if (
          h.credential.generation === input.expectedGeneration! + 1 &&
          sameHostCredential(h.credential, input.credential)
        )
          return json({ host: publicHost(h), identity: this.identity(h) });
        if (h.credential.generation !== input.expectedGeneration)
          return json({ error: "host_credential_conflict" }, { status: 409 });
        h.credential = {
          purpose,
          version: 1,
          generation: input.credential.generation,
          tokenHash: input.credential.tokenHash,
          expiresAt: input.credential.expiresAt,
          ...(input.credential.cleanup ? { cleanup: input.credential.cleanup } : {}),
        };
      } else {
        h.credential.revokedAt ??= new Date().toISOString();
      }
      h.authMode = "host";
      h.enabled = false;
      h.lastHeartbeat = 0;
      // Commit invalidation before contacting relay backends. Retain exact closure
      // targets until every close succeeds, including when a Durable Object is down.
      await s.put(hostPrefix + h.id, h);
      return json({
        host: publicHost(h),
        identity: this.identity(h),
        ...(l?.sshHostAllocation
          ? {
              custody: {
                leaseID: l.id,
                generation: l.sshHostAllocation.generation,
                accessGeneration: l.sshHostAllocation.accessGeneration,
              },
            }
          : {}),
      });
    });
    if (!response.ok) return response;
    try {
      await this.commit(async (s) => {
        const h = await s.get<SSHHost>(hostPrefix + id);
        const l = h?.leaseID ? await s.get<LeaseRecord>(`lease:${h.leaseID}`) : undefined;
        if (l) {
          await closeHostRelays(s, this.env, l);
          await putLeaseRecord(s, l);
        }
      });
    } catch {
      return json(
        {
          error: "host_relay_close_pending",
          message:
            "credential change applied; relay closure incomplete; retry the same admin operation",
        },
        { status: 503 },
      );
    }
    return response;
  }
  private async heartbeat(
    r: Request,
    id: string,
    input: { hostKey?: string; report?: HostReport },
  ): Promise<Response> {
    if (!textValue(input.hostKey, 1024))
      return json({ error: "host_key_required" }, { status: 400 });
    let fingerprint: string;
    try {
      fingerprint = await hostKeyFingerprint(input.hostKey);
    } catch {
      return json({ error: "invalid_host_key" }, { status: 400 });
    }
    return this.commit(async (s) => {
      const h = await s.get<SSHHost>(hostPrefix + id);
      if (!h) return json({ error: "not_found" }, { status: 404 });
      if (hostUsesCredential(h) || hostCredentialRequest(r)) {
        if (!(await authenticateHostCredential(r, h)))
          return json({ error: "host_credential_invalid" }, { status: 401 });
      } else if (h.owner !== requestOwner(r) || h.org !== requestOrg(r, this.env))
        return json({ error: "not_found" }, { status: 404 });
      if (h.frontDoor && h.frontDoor.hostKey !== input.hostKey)
        return json({ error: "host_identity_changed" }, { status: 409 });
      h.frontDoor = { hostKey: input.hostKey!, fingerprint };
      h.lastHeartbeat = Date.now();
      const cleanup = h.credential?.purpose === "cleanup" ? h.credential.cleanup : undefined;
      const leaseID = h.leaseID ?? cleanup?.leaseID;
      let l = leaseID ? await s.get<LeaseRecord>(`lease:${leaseID}`) : undefined;
      if (
        h.leaseID &&
        (!l || !l.sshHostAllocation || l.sshHostAllocation.generation !== h.generation)
      ) {
        h.quarantine = "allocation record missing or inconsistent";
        await s.put(hostPrefix + id, h);
        return json({ error: "host_quarantined" }, { status: 409 });
      }
      if (h.credential?.purpose === "cleanup") {
        const a = l?.sshHostAllocation;
        if (
          input.report &&
          (!cleanup ||
            input.report.leaseID !== cleanup.leaseID ||
            input.report.generation !== cleanup.generation ||
            input.report.accessGeneration !== cleanup.accessGeneration ||
            !["clean", "failed"].includes(input.report.state))
        )
          return json({ error: "host_recovery_report_invalid" }, { status: 409 });
        // Replay only the exact completed receipt after a lost reply or local save.
        if (cleanup && completedHostCleanup(h, l, cleanup))
          return json({
            assignment: null,
            cleanupComplete: true,
            relays: [],
            identity: this.identity(h),
            serverTime: Date.now(),
          });
        if (
          !cleanup ||
          !l ||
          !terminal(l) ||
          !a ||
          a.operation !== "clean" ||
          l.id !== cleanup.leaseID ||
          a.hostID !== h.id ||
          a.generation !== cleanup.generation ||
          a.accessGeneration !== cleanup.accessGeneration
        )
          return json({ error: "host_recovery_custody_mismatch" }, { status: 409 });
      }
      if (l) {
        const a = l.sshHostAllocation!;
        if (!l.portablePoolAccess) a.accessDeadline = l.expiresAt;
        if (
          terminal(l) ||
          Date.parse(l.expiresAt) <= Date.now() ||
          Date.parse(l.lastTouchedAt ?? l.createdAt) + (l.idleTimeoutSeconds ?? 1800) * 1000 <=
            Date.now()
        ) {
          if (!terminal(l)) {
            l.state = "expired";
            l.endedAt = new Date().toISOString();
          }
          a.operation = "clean";
        }
        // Obsolete receipts cannot settle custody, but must not prevent delivery
        // of the next generation (including pool access rotation).
        const candidate = input.report;
        const report =
          candidate?.leaseID === l.id &&
          candidate.generation === a.generation &&
          candidate.accessGeneration === a.accessGeneration
            ? candidate
            : undefined;
        if (report?.state === "failed") {
          h.quarantine = "agent operation failed; cleanup required";
          l.state = "failed";
          l.endedAt = new Date().toISOString();
          a.operation = "clean";
        }
        if (Date.parse(a.accessDeadline) <= Date.now()) a.operation = "clean";
        if (report?.state === "clean" && a.operation === "clean") {
          a.cleanGeneration = a.accessGeneration;
          a.operation = "idle";
          delete h.quarantine;
          if (terminal(l)) {
            delete l.cleanupStartedAt;
            delete l.cleanupError;
            delete l.cleanupRetryAt;
            delete l.cleanupFailedAt;
            delete l.cleanupAttempts;
            delete l.cleanupClaimExpiresAt;
            completeLeaseProviderCleanup(l, new Date().toISOString());
            delete l.sshJump;
            delete h.leaseID;
          }
        }
        if (
          report?.state === "ready" &&
          a.operation === "prepare" &&
          !terminal(l) &&
          Date.parse(a.accessDeadline) > Date.now()
        ) {
          if (
            !textValue(report.sshHostKey, 1024) ||
            !textValue(report.sshUser, 64) ||
            !/^[a-zA-Z0-9_-]+$/.test(report.sshUser) ||
            !textValue(report.workRoot, 512)
          )
            return json({ error: "invalid_host_endpoint" }, { status: 400 });
          let keyFingerprint: string;
          try {
            keyFingerprint = await hostKeyFingerprint(report.sshHostKey);
          } catch {
            return json({ error: "invalid_host_endpoint" }, { status: 400 });
          }
          a.readyGeneration = a.accessGeneration;
          l.sshHostKey = report.sshHostKey;
          l.sshHostKeyFingerprint = keyFingerprint;
          l.sshUser = report.sshUser;
          l.workRoot = report.workRoot;
          l.host = l.id;
          l.sshJump = {
            host: "127.0.0.1",
            port: 1,
            ...h.frontDoor,
            relay: true,
            relayOnly: true,
            candidates: [],
          };
          l.state = "active";
          delete h.quarantine;
        }
        l.updatedAt = new Date().toISOString();
        await putLeaseRecord(s, l);
      }
      if (l && l.sshHostAllocation?.operation !== "prepare") {
        await closeHostRelays(s, this.env, l);
        await putLeaseRecord(s, l);
      }
      await s.put(hostPrefix + id, h);
      return json({
        relays: l ? await pendingHostRelays(s, this.env, l) : [],
        assignment:
          l && h.leaseID
            ? {
                leaseID: l.id,
                ...l.sshHostAllocation,
                deadline: Math.min(
                  Date.parse(l.expiresAt),
                  Date.parse(l.lastTouchedAt ?? l.createdAt) +
                    (l.idleTimeoutSeconds ?? 1800) * 1000,
                  Date.parse(l.sshHostAllocation!.accessDeadline),
                  h.credential ? Date.parse(h.credential.expiresAt) : Infinity,
                ),
                target: l.target,
              }
            : null,
        serverTime: Date.now(),
        ...(cleanup && !h.leaseID ? { cleanupComplete: true } : {}),
        ...(h.credential ? { identity: this.identity(h) } : {}),
      });
    });
  }
  async create(
    r: Request,
    input: LeaseRequest,
    fixedID?: string,
    validate?: (l: LeaseRecord, s: CoordinatorStorageView) => Promise<Response | undefined>,
  ): Promise<Response> {
    if (!requestOwner(r) || requestOwner(r) === "unknown")
      return json({ error: "identity_required" }, { status: 403 });
    const publicKey =
      typeof input.sshPublicKey === "string"
        ? input.sshPublicKey.trim().split(/\s+/).slice(0, 2).join(" ")
        : "";
    const id =
      fixedID ?? input.leaseID ?? `cbx_${crypto.randomUUID().replaceAll("-", "").slice(0, 12)}`;
    if (
      !leasePattern.test(id) ||
      (fixedID && input.leaseID && input.leaseID !== fixedID) ||
      !poolPublicKey(publicKey) ||
      (input.createAttemptID && !/^cat_[a-f0-9]{32}$/.test(input.createAttemptID))
    )
      return json({ error: "invalid_host_lease_request" }, { status: 400 });
    if (
      input.desktop ||
      input.browser ||
      input.code ||
      input.tailscale ||
      input.lifetimePolicy ||
      input.os ||
      input.imageRequirements ||
      input.capacity
    )
      return json({ error: "host_feature_unsupported" }, { status: 400 });
    if (
      input.hostCapabilities !== undefined &&
      (!Array.isArray(input.hostCapabilities) ||
        input.hostCapabilities.length > 32 ||
        !input.hostCapabilities.every((c) => textValue(c, 64)))
    )
      return json({ error: "invalid_host_capabilities" }, { status: 400 });
    const intent = await sha256Hex(JSON.stringify(input));
    return this.commit(async (s) => {
      const prior = await s.get<LeaseRecord>(`lease:${id}`);
      const attempt = await s.get<CreateAttemptRecord>(`create-attempt:${id}`);
      if (
        attempt &&
        (attempt.state === "canceled" ||
          attempt.owner !== requestOwner(r) ||
          attempt.org !== requestOrg(r, this.env) ||
          attempt.token !== input.createAttemptID)
      )
        return json({ error: "create_attempt_conflict" }, { status: 409 });
      if (prior)
        return prior.provider === "ssh-host" &&
          prior.owner === requestOwner(r) &&
          prior.org === requestOrg(r, this.env) &&
          prior.sshHostAllocation?.intent === intent
          ? json({ lease: publicLeaseRecord(prior) }, { status: 202 })
          : json({ error: "create_attempt_conflict" }, { status: 409 });
      if (await s.get(`workspace-lease:${id}`))
        return json({ error: "lease_id_conflict" }, { status: 409 });
      const hosts = await s.list<SSHHost>({ prefix: hostPrefix, limit: 1024 });
      const selected = [...hosts.values()]
        .filter(
          (h) =>
            h.org === requestOrg(r, this.env) &&
            h.enabled &&
            hostCredentialCurrent(h) &&
            !h.draining &&
            !h.quarantine &&
            !h.leaseID &&
            h.frontDoor &&
            Date.now() - h.lastHeartbeat < hostOnlineMS &&
            h.target === (input.target ?? "linux") &&
            (!input.architecture || input.architecture === h.architecture) &&
            (!input.sshHost || input.sshHost === h.id || input.sshHost === h.name) &&
            (input.hostCapabilities ?? []).every((c) => h.capabilities.includes(c)),
        )
        .toSorted((a, b) => a.id.localeCompare(b.id))[0];
      const h = selected;
      if (!h) return json({ error: "ssh_host_capacity_unavailable" }, { status: 409 });
      const now = new Date().toISOString();
      const policy = resolveLeasePolicy(this.env, requestOwner(r), requestOrg(r, this.env));
      const { ttlSeconds, idleTimeoutSeconds, policyNotice } = clampLeaseDurations(input, policy);
      const expiresAt = new Date(Date.now() + ttlSeconds * 1000).toISOString();
      h.generation++;
      h.leaseID = id;
      const l: LeaseRecord = {
        id,
        provider: "ssh-host",
        lifecycle: "managed",
        owner: requestOwner(r),
        org: h.org,
        target: h.target,
        architecture: h.architecture,
        profile: input.profile ?? "default",
        class: input.class ?? "standard",
        serverType: "exclusive-host",
        cloudID: h.id,
        serverID: 0,
        serverName: h.name,
        providerKey: "",
        host: "",
        sshUser: "crabbox",
        sshPort: "22",
        sshFallbackPorts: [],
        workRoot: h.target === "windows" ? "C:\\crabbox" : "/work/crabbox",
        keep: input.keep ?? false,
        leasePolicy: policy,
        ...(policyNotice ? { policyNotice } : {}),
        ttlSeconds,
        idleTimeoutSeconds,
        estimatedHourlyUSD: 0,
        maxEstimatedUSD: 0,
        state: "provisioning",
        createdAt: now,
        updatedAt: now,
        lastTouchedAt: now,
        expiresAt,
        image: {
          id: h.template,
          source: "explicit",
          provider: "ssh-host",
          kind: "host-template",
          region: h.org,
        },
        slug: input.slug ?? id,
        releaseDeletesServer: true,
        provisioningResourceMayExist: true,
        region: h.org,
        ...(input.createAttemptID
          ? { createAttemptID: input.createAttemptID, createAttemptGeneration: crypto.randomUUID() }
          : {}),
        sshHostAllocation: {
          hostID: h.id,
          generation: h.generation,
          intent,
          operator: h.owner,
          template: h.template,
          publicKey,
          operation: "prepare",
          accessGeneration: 0,
          accessDeadline: expiresAt,
        },
      };
      const blocked = await validate?.(l, s);
      if (blocked) return blocked;
      if (input.createAttemptID)
        await s.put(`create-attempt:${id}`, {
          version: 1,
          requestedLeaseID: id,
          token: input.createAttemptID,
          owner: l.owner,
          org: l.org,
          state: "pending",
          canonicalLeaseID: id,
          generation: l.createAttemptGeneration!,
          cloudID: h.id,
          createdAt: now,
          updatedAt: now,
        } satisfies CreateAttemptRecord);
      await putLeaseRecord(s, l);
      await s.put(hostPrefix + h.id, h);
      return json({ lease: publicLeaseRecord(l) }, { status: 202 });
    });
  }
  async release(lease: LeaseRecord): Promise<void> {
    await this.commit(async (s) => {
      const l = await s.get<LeaseRecord>(`lease:${lease.id}`);
      if (!l || !l.sshHostAllocation || leaseProviderCleanupConfirmed(l)) return;
      l.state = "released";
      l.releaseDeletesServer = true;
      l.endedAt ??= new Date().toISOString();
      l.updatedAt = new Date().toISOString();
      l.sshHostAllocation.operation = "clean";
      await putLeaseRecord(s, l);
    });
  }
}
