import { base64URL, sha256Hex, type GitHubUserGrant } from "./auth";
import { bearerToken, pathParts } from "./http";
import { timingSafeEqual } from "./timing-safe";
import type { Env } from "./types";

export const pairingGrantTTLSeconds = 5 * 60;
export const deviceTokenTTLSeconds = 90 * 24 * 60 * 60;
export const pairingRequestBodyBytes = 4 * 1024;
export const maxDeviceTokensPerOwner = 10;
export const maxPairingGrantsPerOwner = 10;

const pairingGrantPrefix = "cbxp_";
export const deviceTokenPrefix = "cbxd_";
const deviceAudience = "crabbox-device";
const pairingAudience = "crabbox-device-pairing";
const leaseReadScope = "leases:read";
const deviceIDPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const sha256Pattern = /^[a-f0-9]{64}$/;
const encoder = new TextEncoder();

export interface PairingGrantRecord {
  version: 1;
  audience: typeof pairingAudience;
  scope: typeof leaseReadScope;
  grantHash: string;
  owner: string;
  org: string;
  ownerLogin: string;
  ownerGrant: GitHubUserGrant;
  name: string;
  createdAt: string;
  expiresAt: string;
}

export interface DeviceTokenRecord {
  version: 1;
  id: string;
  audience: typeof deviceAudience;
  scope: [typeof leaseReadScope];
  tokenHash: string;
  owner: string;
  org: string;
  ownerLogin: string;
  ownerGrant: GitHubUserGrant;
  name: string;
  createdAt: string;
  expiresAt: string;
}

export interface PublicDeviceRecord {
  id: string;
  audience: typeof deviceAudience;
  scope: [typeof leaseReadScope];
  name: string;
  createdAt: string;
  expiresAt: string;
}

export interface PairingGrantOwnerIndexRecord {
  version: 1;
  grantHash: string;
  expiresAt: string;
}

export interface DeviceOwnerIndexRecord {
  version: 1;
  deviceID: string;
  expiresAt: string;
}

export type CoordinatorOriginStatus = "allowed" | "forbidden" | "unavailable";

export function coordinatorOriginStatus(
  request: Request,
  env: Pick<Env, "CRABBOX_PUBLIC_URL">,
  requireOriginHeader: boolean,
): CoordinatorOriginStatus {
  const configured = canonicalCoordinatorOrigin(env.CRABBOX_PUBLIC_URL);
  if (!configured) return "unavailable";
  if (new URL(request.url).origin !== configured) return "forbidden";
  const origin = request.headers.get("origin")?.trim();
  if (!origin) return requireOriginHeader ? "forbidden" : "allowed";
  try {
    return new URL(origin).origin === configured && origin === configured ? "allowed" : "forbidden";
  } catch {
    return "forbidden";
  }
}

export function isPairingExchangeRequest(request: Request): boolean {
  return (
    request.method.toUpperCase() === "POST" &&
    pathParts(request).join("/") === "v1/pairing/exchange"
  );
}

export function isPairingGrantRequest(request: Request): boolean {
  return (
    request.method.toUpperCase() === "POST" && pathParts(request).join("/") === "v1/pairing/grants"
  );
}

export function isDeviceManagementRequest(request: Request): boolean {
  const parts = pathParts(request);
  return (
    (request.method.toUpperCase() === "GET" || request.method.toUpperCase() === "DELETE") &&
    parts[0] === "v1" &&
    parts[1] === "devices" &&
    (parts.length === 2 || parts.length === 3)
  );
}

export function isDeviceTokenRequest(request: Request): boolean {
  return bearerToken(request).startsWith(deviceTokenPrefix);
}

export function deviceTokenRouteAllowed(request: Request): boolean {
  if (request.method.toUpperCase() !== "GET") return false;
  const parts = pathParts(request);
  if (parts.join("/") === "v1/leases") return true;
  return (
    parts.length === 3 &&
    parts[0] === "v1" &&
    parts[1] === "leases" &&
    Boolean(parts[2]) &&
    !new URL(request.url).searchParams.has("providerMetadata")
  );
}

export async function newPairingGrant(): Promise<{ grant: string; grantHash: string }> {
  const grant = `${pairingGrantPrefix}${randomSecret()}`;
  return { grant, grantHash: await sha256Hex(grant) };
}

export async function pairingGrantHash(grant: string): Promise<string | undefined> {
  return validPairingGrant(grant) ? await sha256Hex(grant) : undefined;
}

export function pairingGrantKey(grantHash: string): string {
  return `pairing-grant:${grantHash}`;
}

export function pairingGrantOwnerIndexKey(owner: string, org: string, grantHash: string): string {
  return `${pairingGrantOwnerIndexPrefix(owner, org)}${grantHash}`;
}

export function pairingGrantOwnerIndexPrefix(owner: string, org: string): string {
  return `pairing-grant-owner:${ownerScope(owner, org)}:`;
}

export async function newDeviceToken(): Promise<{
  id: string;
  token: string;
  tokenHash: string;
}> {
  const id = crypto.randomUUID().toLowerCase();
  const token = `${deviceTokenPrefix}${id}.${randomSecret()}`;
  return { id, token, tokenHash: await sha256Hex(token) };
}

export async function parsedDeviceToken(
  token: string,
): Promise<{ id: string; tokenHash: string } | undefined> {
  const match = /^cbxd_([0-9a-f-]{36})\.([A-Za-z0-9_-]{43})$/.exec(token);
  if (!match?.[1] || !deviceIDPattern.test(match[1])) return undefined;
  return { id: match[1], tokenHash: await sha256Hex(token) };
}

export function deviceTokenKey(id: string): string {
  return `device-token:${id}`;
}

export function deviceOwnerIndexKey(owner: string, org: string, deviceID: string): string {
  return `${deviceOwnerIndexPrefix(owner, org)}${deviceID}`;
}

export function deviceOwnerIndexPrefix(owner: string, org: string): string {
  return `device-owner:${ownerScope(owner, org)}:`;
}

export function validDeviceID(id: string): boolean {
  return deviceIDPattern.test(id);
}

export function validPairingGrantRecord(record: PairingGrantRecord, hash: string): boolean {
  return (
    record.version === 1 &&
    record.audience === pairingAudience &&
    record.scope === leaseReadScope &&
    typeof record.owner === "string" &&
    record.owner.length > 0 &&
    typeof record.org === "string" &&
    typeof record.ownerLogin === "string" &&
    record.ownerLogin.length > 0 &&
    validGitHubUserGrant(record.ownerGrant) &&
    typeof record.name === "string" &&
    Number.isFinite(Date.parse(record.createdAt)) &&
    Number.isFinite(Date.parse(record.expiresAt)) &&
    typeof record.grantHash === "string" &&
    timingSafeEqual(record.grantHash, hash)
  );
}

export function validDeviceTokenRecord(
  record: DeviceTokenRecord,
  id: string,
  tokenHash: string,
): boolean {
  return validStoredDeviceTokenRecord(record, id) && timingSafeEqual(record.tokenHash, tokenHash);
}

export function validStoredDeviceTokenRecord(record: DeviceTokenRecord, id: string): boolean {
  return (
    record.version === 1 &&
    record.id === id &&
    record.audience === deviceAudience &&
    Array.isArray(record.scope) &&
    record.scope.length === 1 &&
    record.scope[0] === leaseReadScope &&
    typeof record.owner === "string" &&
    record.owner.length > 0 &&
    typeof record.org === "string" &&
    typeof record.ownerLogin === "string" &&
    record.ownerLogin.length > 0 &&
    validGitHubUserGrant(record.ownerGrant) &&
    typeof record.name === "string" &&
    Number.isFinite(Date.parse(record.createdAt)) &&
    Number.isFinite(Date.parse(record.expiresAt)) &&
    typeof record.tokenHash === "string" &&
    sha256Pattern.test(record.tokenHash)
  );
}

export function validPairingGrantOwnerIndexRecord(
  record: PairingGrantOwnerIndexRecord,
  grantHash: string,
): boolean {
  return (
    record.version === 1 &&
    record.grantHash === grantHash &&
    sha256Pattern.test(record.grantHash) &&
    Number.isFinite(Date.parse(record.expiresAt))
  );
}

export function validDeviceOwnerIndexRecord(
  record: DeviceOwnerIndexRecord,
  deviceID: string,
): boolean {
  return (
    record.version === 1 &&
    record.deviceID === deviceID &&
    validDeviceID(record.deviceID) &&
    Number.isFinite(Date.parse(record.expiresAt))
  );
}

export function publicDeviceRecord(record: DeviceTokenRecord): PublicDeviceRecord {
  return {
    id: record.id,
    audience: record.audience,
    scope: record.scope,
    name: record.name,
    createdAt: record.createdAt,
    expiresAt: record.expiresAt,
  };
}

export function pairingDeviceName(value: unknown): string | undefined {
  if (value === undefined || value === null || value === "") return "Mobile device";
  if (typeof value !== "string") return undefined;
  const name = value.trim();
  if (!name || name.length > 80) return undefined;
  for (const character of name) {
    const code = character.charCodeAt(0);
    if (code <= 31 || code === 127) return undefined;
  }
  return name;
}

function canonicalCoordinatorOrigin(value: string | undefined): string | undefined {
  try {
    const url = new URL(value?.trim() ?? "");
    if (url.username || url.password) return undefined;
    if (url.protocol === "https:") return url.origin;
    if (url.protocol === "http:" && loopbackHostname(url.hostname)) return url.origin;
  } catch {
    return undefined;
  }
  return undefined;
}

function loopbackHostname(hostname: string): boolean {
  return hostname === "localhost" || hostname === "127.0.0.1" || hostname === "[::1]";
}

function validPairingGrant(grant: string): boolean {
  return new RegExp(`^${pairingGrantPrefix}[A-Za-z0-9_-]{43}$`).test(grant);
}

function randomSecret(): string {
  return base64URL(crypto.getRandomValues(new Uint8Array(32)));
}

function ownerScope(owner: string, org: string): string {
  return base64URL(encoder.encode(JSON.stringify([owner, org])));
}

function validGitHubUserGrant(grant: GitHubUserGrant): boolean {
  return (
    grant !== null &&
    typeof grant === "object" &&
    typeof grant.tokenID === "string" &&
    /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(grant.tokenID) &&
    typeof grant.sealedCredential === "string" &&
    grant.sealedCredential.length > 0 &&
    typeof grant.expiresAt === "string" &&
    Number.isFinite(Date.parse(grant.expiresAt))
  );
}
