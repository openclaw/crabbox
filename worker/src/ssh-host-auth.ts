import { sha256Hex } from "./encoding";
import { bearerToken } from "./http";
import type { SSHHost } from "./ssh-host";
import { timingSafeEqual } from "./timing-safe";

export interface HostCleanupBinding {
  leaseID: string;
  generation: number;
  accessGeneration: number;
}
export interface HostCredential {
  purpose: "agent" | "cleanup";
  cleanup?: HostCleanupBinding;
  version: 1;
  generation: number;
  tokenHash: string;
  expiresAt: string;
  revokedAt?: string;
}

const uuid = "[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}";
const tokenPattern = new RegExp(`^cbxh_(${uuid})_([1-9][0-9]{0,8})_[a-f0-9]{64}$`);
export const hostCredentialRequest = (r: Request): boolean =>
  (bearerToken(r) ?? "").startsWith("cbxh_");

export function hostCredentialRouteAllowed(r: Request): boolean {
  const path = new URL(r.url).pathname;
  return (
    (r.method === "POST" && new RegExp(`^/v1/ssh-hosts/${uuid}/heartbeat$`).test(path)) ||
    (r.method === "GET" &&
      r.headers.get("upgrade")?.toLowerCase() === "websocket" &&
      /^\/v1\/serve\/relay\/[a-f0-9]{32}\/agent$/.test(path))
  );
}

export const hostUsesCredential = (h: SSHHost): boolean =>
  h.authMode !== undefined || Boolean(h.credential);
function credentialLive(h: SSHHost, now = Date.now()): boolean {
  return Boolean(
    h.frontDoor &&
    h.credential?.version === 1 &&
    !h.credential.revokedAt &&
    Date.parse(h.credential.expiresAt) > now,
  );
}
export function hostCredentialCurrent(h: SSHHost, now = Date.now()): boolean {
  return !hostUsesCredential(h) || (credentialLive(h, now) && h.credential?.purpose === "agent");
}

export async function authenticateHostCredential(r: Request, h: SSHHost): Promise<boolean> {
  const token = bearerToken(r) ?? "";
  const match = tokenPattern.exec(token);
  return Boolean(
    match &&
    h.credential &&
    credentialLive(h) &&
    (h.credential.purpose === "agent" || h.credential.purpose === "cleanup") &&
    match[1] === h.id &&
    Number(match[2]) === h.credential.generation &&
    timingSafeEqual(await sha256Hex(token), h.credential.tokenHash),
  );
}

export function validHostCredential(
  input: unknown,
  generation: number,
  purpose: "agent" | "cleanup" = "agent",
): input is HostCredential {
  if (!input || typeof input !== "object") return false;
  const c = input as HostCredential;
  const expires = Date.parse(c.expiresAt);
  return (
    c.purpose === purpose &&
    (purpose === "cleanup"
      ? Boolean(
          c.cleanup &&
          /^cbx_[a-f0-9]{12}$/.test(c.cleanup.leaseID) &&
          Number.isSafeInteger(c.cleanup.generation) &&
          c.cleanup.generation > 0 &&
          Number.isSafeInteger(c.cleanup.accessGeneration) &&
          c.cleanup.accessGeneration >= 0,
        )
      : c.cleanup === undefined) &&
    c.version === 1 &&
    c.generation === generation &&
    generation > 0 &&
    generation <= 999999999 &&
    /^[a-f0-9]{64}$/.test(c.tokenHash) &&
    !c.revokedAt &&
    Number.isFinite(expires) &&
    expires > Date.now() &&
    expires <= Date.now() + (purpose === "cleanup" ? 1 : 365) * 86400_000
  );
}

export function sameHostCredential(
  a: HostCredential | undefined,
  b: HostCredential | undefined,
): boolean {
  if (!a || !b) return a === b;
  return (
    a.purpose === b.purpose &&
    a.cleanup?.leaseID === b.cleanup?.leaseID &&
    a.cleanup?.generation === b.cleanup?.generation &&
    a.cleanup?.accessGeneration === b.cleanup?.accessGeneration &&
    a.version === b.version &&
    a.generation === b.generation &&
    a.tokenHash === b.tokenHash &&
    a.expiresAt === b.expiresAt &&
    a.revokedAt === b.revokedAt
  );
}
