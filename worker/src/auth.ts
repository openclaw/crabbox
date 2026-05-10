import { bearerToken } from "./http";
import type { Env } from "./types";

const tokenPrefix = "cbxu_";
const encoder = new TextEncoder();
const decoder = new TextDecoder();
const accessKeyCache = new Map<string, CryptoKey>();

export interface AuthContext {
  authorized: boolean;
  admin: boolean;
  auth: "bearer" | "github";
  owner: string;
  org: string;
  login?: string;
}

interface UserTokenPayload {
  typ: "crabbox-user";
  owner: string;
  org: string;
  login: string;
  name?: string;
  exp: number;
  iat: number;
}

export async function authenticateRequest(
  request: Request,
  env: Pick<
    Env,
    | "CRABBOX_SHARED_TOKEN"
    | "CRABBOX_SHARED_OWNER"
    | "CRABBOX_ADMIN_TOKEN"
    | "CRABBOX_SESSION_SECRET"
    | "CRABBOX_DEFAULT_ORG"
    | "CRABBOX_ACCESS_TEAM_DOMAIN"
    | "CRABBOX_ACCESS_AUD"
  >,
): Promise<AuthContext | undefined> {
  const token = bearerToken(request);
  if (!token) {
    return undefined;
  }
  const accessIdentity = await verifiedAccessIdentity(request, env).catch(() => undefined);
  if (env.CRABBOX_ADMIN_TOKEN && token === env.CRABBOX_ADMIN_TOKEN) {
    return {
      authorized: true,
      admin: true,
      auth: "bearer",
      owner: accessIdentity?.email ?? request.headers.get("x-crabbox-owner") ?? "unknown",
      org: request.headers.get("x-crabbox-org") ?? env.CRABBOX_DEFAULT_ORG ?? "unknown",
    };
  }
  if (env.CRABBOX_SHARED_TOKEN && token === env.CRABBOX_SHARED_TOKEN) {
    return {
      authorized: true,
      admin: false,
      auth: "bearer",
      owner: accessIdentity?.email ?? env.CRABBOX_SHARED_OWNER?.trim() ?? "unknown",
      org: env.CRABBOX_DEFAULT_ORG ?? "unknown",
    };
  }
  const payload = await verifyUserToken(token, env).catch(() => undefined);
  if (!payload) {
    return undefined;
  }
  return {
    authorized: true,
    admin: false,
    auth: "github",
    owner: payload.owner,
    org: payload.org,
    login: payload.login,
  };
}

export function requestWithAuthContext(request: Request, auth: AuthContext): Request {
  const headers = new Headers(request.headers);
  headers.delete("cf-access-authenticated-user-email");
  headers.delete("cf-access-jwt-assertion");
  headers.set("x-crabbox-auth", auth.auth);
  headers.set("x-crabbox-admin", auth.admin ? "true" : "false");
  headers.set("x-crabbox-owner", auth.owner);
  headers.set("x-crabbox-org", auth.org);
  if (auth.login) {
    headers.set("x-crabbox-github-login", auth.login);
  } else {
    headers.delete("x-crabbox-github-login");
  }
  return new Request(request, { headers });
}

export function isAdminRequest(request: Request): boolean {
  return request.headers.get("x-crabbox-admin") === "true";
}

export async function issueUserToken(
  env: Pick<Env, "CRABBOX_SHARED_TOKEN" | "CRABBOX_SESSION_SECRET">,
  input: {
    owner: string;
    org: string;
    login: string;
    name?: string;
    ttlSeconds?: number;
  },
): Promise<string> {
  const now = Math.floor(Date.now() / 1000);
  const payload: UserTokenPayload = {
    typ: "crabbox-user",
    owner: input.owner,
    org: input.org,
    login: input.login,
    iat: now,
    exp: now + (input.ttlSeconds ?? 30 * 24 * 60 * 60),
  };
  if (input.name) {
    payload.name = input.name;
  }
  const encodedPayload = base64URL(encoder.encode(JSON.stringify(payload)));
  const sig = await sign(encodedPayload, sessionSecret(env));
  return `${tokenPrefix}${encodedPayload}.${sig}`;
}

async function verifyUserToken(
  token: string,
  env: Pick<Env, "CRABBOX_SHARED_TOKEN" | "CRABBOX_SESSION_SECRET">,
): Promise<UserTokenPayload | undefined> {
  if (!token.startsWith(tokenPrefix)) {
    return undefined;
  }
  const raw = token.slice(tokenPrefix.length);
  const [encodedPayload, signature] = raw.split(".", 2);
  if (!encodedPayload || !signature) {
    return undefined;
  }
  const expected = await sign(encodedPayload, sessionSecret(env));
  if (!constantTimeEqual(signature, expected)) {
    return undefined;
  }
  const payload = JSON.parse(
    new TextDecoder().decode(base64URLDecode(encodedPayload)),
  ) as Partial<UserTokenPayload>;
  if (
    payload.typ !== "crabbox-user" ||
    typeof payload.owner !== "string" ||
    typeof payload.org !== "string" ||
    typeof payload.login !== "string" ||
    typeof payload.exp !== "number" ||
    payload.exp <= Math.floor(Date.now() / 1000) ||
    "admin" in payload
  ) {
    return undefined;
  }
  return payload as UserTokenPayload;
}

function sessionSecret(env: Pick<Env, "CRABBOX_SHARED_TOKEN" | "CRABBOX_SESSION_SECRET">): string {
  const secret = env.CRABBOX_SESSION_SECRET || env.CRABBOX_SHARED_TOKEN;
  if (!secret) {
    throw new Error("CRABBOX_SESSION_SECRET or CRABBOX_SHARED_TOKEN is required");
  }
  return secret;
}

interface AccessIdentity {
  email?: string;
  subject?: string;
}

interface AccessJwtPayload {
  aud?: string | string[];
  email?: string;
  exp?: number;
  iat?: number;
  iss?: string;
  nbf?: number;
  sub?: string;
}

interface AccessJwtHeader {
  alg?: string;
  kid?: string;
}

interface AccessCerts {
  keys?: AccessPublicJwk[];
}

interface AccessPublicJwk extends JsonWebKey {
  kid?: string;
}

async function verifiedAccessIdentity(
  request: Request,
  env: Pick<Env, "CRABBOX_ACCESS_TEAM_DOMAIN" | "CRABBOX_ACCESS_AUD">,
): Promise<AccessIdentity | undefined> {
  const jwt = request.headers.get("cf-access-jwt-assertion");
  const teamDomain = normalizedAccessTeamDomain(env.CRABBOX_ACCESS_TEAM_DOMAIN);
  const expectedAud = env.CRABBOX_ACCESS_AUD?.trim();
  if (!jwt || !teamDomain || !expectedAud) {
    return undefined;
  }
  const [encodedHeader, encodedPayload, encodedSignature] = jwt.split(".");
  if (!encodedHeader || !encodedPayload || !encodedSignature) {
    return undefined;
  }
  const header = JSON.parse(decoder.decode(base64URLDecode(encodedHeader))) as AccessJwtHeader;
  if (header.alg !== "RS256" || !header.kid) {
    return undefined;
  }
  const key = await accessPublicKey(teamDomain, header.kid);
  if (!key) {
    return undefined;
  }
  const verified = await crypto.subtle.verify(
    "RSASSA-PKCS1-v1_5",
    key,
    base64URLDecode(encodedSignature),
    encoder.encode(`${encodedHeader}.${encodedPayload}`),
  );
  if (!verified) {
    return undefined;
  }
  const payload = JSON.parse(decoder.decode(base64URLDecode(encodedPayload))) as AccessJwtPayload;
  if (!validAccessPayload(payload, teamDomain, expectedAud)) {
    return undefined;
  }
  const identity: AccessIdentity = {};
  if (typeof payload.email === "string" && payload.email !== "") {
    identity.email = payload.email;
  }
  if (typeof payload.sub === "string" && payload.sub !== "") {
    identity.subject = payload.sub;
  }
  return identity;
}

async function accessPublicKey(teamDomain: string, kid: string): Promise<CryptoKey | undefined> {
  const cacheKey = `${teamDomain}:${kid}`;
  const cached = accessKeyCache.get(cacheKey);
  if (cached) {
    return cached;
  }
  const response = await fetch(`https://${teamDomain}/cdn-cgi/access/certs`);
  if (!response.ok) {
    return undefined;
  }
  const certs = (await response.json()) as AccessCerts;
  const jwk = certs.keys?.find((key) => key.kid === kid);
  if (!jwk) {
    return undefined;
  }
  const key = await crypto.subtle.importKey(
    "jwk",
    jwk,
    { name: "RSASSA-PKCS1-v1_5", hash: "SHA-256" },
    false,
    ["verify"],
  );
  accessKeyCache.set(cacheKey, key);
  return key;
}

function normalizedAccessTeamDomain(value: string | undefined): string {
  const trimmed = value?.trim() ?? "";
  if (!trimmed) {
    return "";
  }
  return trimmed.replace(/^https?:\/\//, "").replace(/\/.*$/, "");
}

function validAccessPayload(
  payload: AccessJwtPayload,
  teamDomain: string,
  expectedAud: string,
): boolean {
  const now = Math.floor(Date.now() / 1000);
  const audiences = Array.isArray(payload.aud) ? payload.aud : payload.aud ? [payload.aud] : [];
  return (
    audiences.includes(expectedAud) &&
    payload.iss === `https://${teamDomain}` &&
    typeof payload.exp === "number" &&
    payload.exp > now &&
    (typeof payload.nbf !== "number" || payload.nbf <= now)
  );
}

async function sign(value: string, secret: string): Promise<string> {
  const key = await crypto.subtle.importKey(
    "raw",
    encoder.encode(secret),
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign"],
  );
  const signature = await crypto.subtle.sign("HMAC", key, encoder.encode(value));
  return base64URL(new Uint8Array(signature));
}

export async function sha256Hex(value: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", encoder.encode(value));
  return [...new Uint8Array(digest)].map((byte) => byte.toString(16).padStart(2, "0")).join("");
}

export function base64URL(data: Uint8Array): string {
  let binary = "";
  for (const byte of data) {
    binary += String.fromCharCode(byte);
  }
  return btoa(binary).replaceAll("+", "-").replaceAll("/", "_").replaceAll("=", "");
}

function base64URLDecode(value: string): Uint8Array {
  const padded = value
    .replaceAll("-", "+")
    .replaceAll("_", "/")
    .padEnd(Math.ceil(value.length / 4) * 4, "=");
  const binary = atob(padded);
  const out = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) {
    out[i] = binary.charCodeAt(i);
  }
  return out;
}

function constantTimeEqual(a: string, b: string): boolean {
  if (a.length !== b.length) {
    return false;
  }
  let diff = 0;
  for (let i = 0; i < a.length; i += 1) {
    diff |= a.charCodeAt(i) ^ b.charCodeAt(i);
  }
  return diff === 0;
}
