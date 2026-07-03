import { bearerToken } from "./http";
import { timingSafeEqual } from "./timing-safe";
import type { Env } from "./types";

const tokenPrefix = "cbxu_";
const encoder = new TextEncoder();
const decoder = new TextDecoder();
const accessJwtHeaderMaxChars = 2048;
const accessJwtMaxChars = 32 * 1024;
const accessKidMaxChars = 256;
const accessKeySetTTLMS = 5 * 60 * 1000;
const accessKeySetFailureTTLMS = 30 * 1000;
const accessKeySetCacheMaxEntries = 8;
const userTokenVersion = 2;
const accessKeySetCache = new Map<string, AccessKeySetCacheEntry>();
const accessKeySetLoads = new Map<string, Promise<AccessKeySetCacheEntry>>();

export interface AuthContext {
  authorized: boolean;
  admin: boolean;
  auth: "bearer" | "github" | "proxy";
  owner: string;
  org: string;
  login?: string;
  tokenExpiresAt?: string;
}

export interface AuthRequestContext {
  trustedProxy?: boolean;
}

interface UserTokenPayload {
  typ: "crabbox-user";
  version: typeof userTokenVersion;
  ownerSource: "github-verified-email";
  jti?: string;
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
    | "CRABBOX_GITHUB_ADMIN_OWNERS"
    | "CRABBOX_GITHUB_ADMIN_LOGINS"
    | "CRABBOX_TRUSTED_USER_HEADER"
    | "CRABBOX_TRUSTED_USER_ORG"
    | "CRABBOX_TRUSTED_PROXY_SECRET"
  >,
  context: AuthRequestContext = {},
): Promise<AuthContext | undefined> {
  const token = bearerToken(request);
  const trustedIdentity = context.trustedProxy ? trustedProxyIdentity(request, env) : undefined;
  if (env.CRABBOX_ADMIN_TOKEN && timingSafeEqual(token ?? "", env.CRABBOX_ADMIN_TOKEN)) {
    const accessIdentity = await verifiedAccessIdentity(request, env).catch(() => undefined);
    return {
      authorized: true,
      admin: true,
      auth: "bearer",
      owner:
        accessIdentity?.email ??
        trustedIdentity?.owner ??
        request.headers.get("x-crabbox-owner") ??
        "unknown",
      org: request.headers.get("x-crabbox-org") ?? env.CRABBOX_DEFAULT_ORG ?? "unknown",
    };
  }
  if (env.CRABBOX_SHARED_TOKEN && timingSafeEqual(token ?? "", env.CRABBOX_SHARED_TOKEN)) {
    const accessIdentity = await verifiedAccessIdentity(request, env).catch(() => undefined);
    return {
      authorized: true,
      admin: false,
      auth: "bearer",
      owner:
        accessIdentity?.email ??
        trustedIdentity?.owner ??
        env.CRABBOX_SHARED_OWNER?.trim() ??
        "unknown",
      org: env.CRABBOX_DEFAULT_ORG ?? "unknown",
    };
  }
  if (token) {
    const payload = await verifyUserToken(token, env).catch(() => undefined);
    if (payload) {
      return {
        authorized: true,
        admin: githubUserIsAdmin(payload, env),
        auth: "github",
        owner: payload.owner,
        org: payload.org,
        login: payload.login,
        tokenExpiresAt: new Date(payload.exp * 1000).toISOString(),
      };
    }
  }
  return trustedIdentity;
}

function githubUserIsAdmin(
  payload: Pick<UserTokenPayload, "owner" | "login">,
  env: Pick<Env, "CRABBOX_GITHUB_ADMIN_OWNERS" | "CRABBOX_GITHUB_ADMIN_LOGINS">,
): boolean {
  const owner = payload.owner.trim().toLowerCase();
  const login = payload.login.trim().toLowerCase();
  return (
    envList(env.CRABBOX_GITHUB_ADMIN_OWNERS).includes(owner) ||
    envList(env.CRABBOX_GITHUB_ADMIN_LOGINS).includes(login)
  );
}

function envList(value: string | undefined): string[] {
  return (value ?? "")
    .split(",")
    .map((item) => item.trim().toLowerCase())
    .filter(Boolean);
}

export function requestWithAuthContext(request: Request, auth: AuthContext): Request {
  const headers = new Headers(request.headers);
  headers.delete("cf-access-authenticated-user-email");
  headers.delete("cf-access-jwt-assertion");
  headers.delete("x-crabbox-internal");
  headers.delete("x-crabbox-proxy-secret");
  headers.set("x-crabbox-auth", auth.auth);
  headers.set("x-crabbox-admin", auth.admin ? "true" : "false");
  headers.set("x-crabbox-owner", auth.owner);
  headers.set("x-crabbox-org", auth.org);
  if (auth.login) {
    headers.set("x-crabbox-github-login", auth.login);
  } else {
    headers.delete("x-crabbox-github-login");
  }
  if (auth.tokenExpiresAt) {
    headers.set("x-crabbox-token-expires-at", auth.tokenExpiresAt);
  } else {
    headers.delete("x-crabbox-token-expires-at");
  }
  return new Request(request, { headers });
}

export function requestWithoutTrustedHeaders(request: Request): Request {
  if (
    !request.headers.has("x-crabbox-internal") &&
    !request.headers.has("x-crabbox-proxy-secret")
  ) {
    return request;
  }
  const headers = new Headers(request.headers);
  headers.delete("x-crabbox-internal");
  headers.delete("x-crabbox-proxy-secret");
  return new Request(request, { headers });
}

export function isAdminRequest(request: Request): boolean {
  return request.headers.get("x-crabbox-admin") === "true";
}

export async function issueUserToken(
  env: Pick<Env, "CRABBOX_SHARED_TOKEN" | "CRABBOX_SESSION_SECRET">,
  input: {
    owner: string;
    ownerSource: "github-verified-email";
    org: string;
    login: string;
    name?: string;
    ttlSeconds?: number;
  },
): Promise<string> {
  const now = Math.floor(Date.now() / 1000);
  const payload: UserTokenPayload = {
    typ: "crabbox-user",
    version: userTokenVersion,
    ownerSource: input.ownerSource,
    jti: crypto.randomUUID(),
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

export function userTokenExpiresAt(token: string): string | undefined {
  const payload = decodeUserTokenPayload(token);
  if (typeof payload?.exp !== "number") {
    return undefined;
  }
  return new Date(payload.exp * 1000).toISOString();
}

async function verifyUserToken(
  token: string,
  env: Pick<Env, "CRABBOX_SHARED_TOKEN" | "CRABBOX_SESSION_SECRET">,
): Promise<UserTokenPayload | undefined> {
  const parts = userTokenParts(token);
  if (!parts) {
    return undefined;
  }
  const { encodedPayload, signature } = parts;
  const expected = await sign(encodedPayload, sessionSecret(env));
  if (!timingSafeEqual(signature, expected)) {
    return undefined;
  }
  const payload = decodeUserTokenPayload(token);
  if (
    payload.typ !== "crabbox-user" ||
    payload.version !== userTokenVersion ||
    payload.ownerSource !== "github-verified-email" ||
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

function decodeUserTokenPayload(token: string): Partial<UserTokenPayload> {
  const parts = userTokenParts(token);
  if (!parts) {
    return {};
  }
  try {
    return JSON.parse(
      decoder.decode(base64URLDecode(parts.encodedPayload)),
    ) as Partial<UserTokenPayload>;
  } catch {
    return {};
  }
}

function userTokenParts(token: string): { encodedPayload: string; signature: string } | undefined {
  if (!token.startsWith(tokenPrefix)) {
    return undefined;
  }
  const parts = token.slice(tokenPrefix.length).split(".");
  if (parts.length !== 2 || !parts[0] || !parts[1]) {
    return undefined;
  }
  return { encodedPayload: parts[0], signature: parts[1] };
}

function sessionSecret(env: Pick<Env, "CRABBOX_SHARED_TOKEN" | "CRABBOX_SESSION_SECRET">): string {
  const error = userTokenSigningConfigurationError(env);
  if (error) {
    throw new Error(error);
  }
  return env.CRABBOX_SESSION_SECRET!;
}

export function userTokenSigningConfigurationError(
  env: Pick<Env, "CRABBOX_SHARED_TOKEN" | "CRABBOX_SESSION_SECRET">,
): string | undefined {
  if (!env.CRABBOX_SESSION_SECRET) {
    return "CRABBOX_SESSION_SECRET is required for signed user tokens";
  }
  if (
    env.CRABBOX_SHARED_TOKEN &&
    timingSafeEqual(env.CRABBOX_SESSION_SECRET, env.CRABBOX_SHARED_TOKEN)
  ) {
    return "CRABBOX_SESSION_SECRET must differ from CRABBOX_SHARED_TOKEN";
  }
  return undefined;
}

interface AccessIdentity {
  email?: string;
  subject?: string;
}

function trustedProxyIdentity(
  request: Request,
  env: Pick<
    Env,
    "CRABBOX_TRUSTED_USER_HEADER" | "CRABBOX_TRUSTED_USER_ORG" | "CRABBOX_TRUSTED_PROXY_SECRET"
  >,
): AuthContext | undefined {
  const requiredSecret = env.CRABBOX_TRUSTED_PROXY_SECRET;
  if (
    requiredSecret !== undefined &&
    (!requiredSecret ||
      !timingSafeEqual(request.headers.get("x-crabbox-proxy-secret") ?? "", requiredSecret))
  ) {
    return undefined;
  }
  const header = env.CRABBOX_TRUSTED_USER_HEADER?.trim();
  if (
    !header ||
    header.toLowerCase() === "x-crabbox-proxy-secret" ||
    !/^[!#$%&'*+.^_`|~0-9A-Za-z-]+$/.test(header)
  ) {
    return undefined;
  }
  const owner = request.headers.get(header)?.trim();
  if (!owner || owner.length > 320 || hasControlCharacter(owner)) {
    return undefined;
  }
  return {
    authorized: true,
    admin: false,
    auth: "proxy",
    owner,
    org: env.CRABBOX_TRUSTED_USER_ORG?.trim() || "unknown",
  };
}

function hasControlCharacter(value: string): boolean {
  for (const character of value) {
    const code = character.charCodeAt(0);
    if (code <= 31 || code === 127) {
      return true;
    }
  }
  return false;
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

interface AccessKeySetCacheEntry {
  expiresAt: number;
  jwks: Map<string, AccessPublicJwk>;
  imported: Map<string, CryptoKey>;
  invalid: Set<string>;
  missRefreshUsed: boolean;
}

interface AccessKeySetLookup {
  entry: AccessKeySetCacheEntry;
  fromCache: boolean;
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
  if (jwt.length > accessJwtMaxChars) {
    return undefined;
  }
  const parts = jwt.split(".");
  if (parts.length !== 3) {
    return undefined;
  }
  const [encodedHeader, encodedPayload, encodedSignature] = parts;
  if (
    !encodedHeader ||
    encodedHeader.length > accessJwtHeaderMaxChars ||
    !encodedPayload ||
    !encodedSignature
  ) {
    return undefined;
  }
  const header = JSON.parse(decoder.decode(base64URLDecode(encodedHeader))) as AccessJwtHeader;
  if (
    header.alg !== "RS256" ||
    typeof header.kid !== "string" ||
    header.kid.length === 0 ||
    header.kid.length > accessKidMaxChars
  ) {
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
  const lookup = await accessKeySet(teamDomain);
  let keySet = lookup.entry;
  const cached = keySet.imported.get(kid);
  if (cached) {
    return cached;
  }
  if (keySet.invalid.has(kid)) {
    return undefined;
  }
  let jwk = keySet.jwks.get(kid);
  if (!jwk && lookup.fromCache && !keySet.missRefreshUsed) {
    keySet.missRefreshUsed = true;
    keySet = await refreshAccessKeySet(teamDomain);
    jwk = keySet.jwks.get(kid);
  } else if (!jwk && lookup.fromCache) {
    const refreshing = accessKeySetLoads.get(teamDomain);
    if (refreshing) {
      keySet = await refreshing;
      jwk = keySet.jwks.get(kid);
    }
  }
  if (!jwk) {
    return undefined;
  }
  try {
    const key = await crypto.subtle.importKey(
      "jwk",
      jwk,
      { name: "RSASSA-PKCS1-v1_5", hash: "SHA-256" },
      false,
      ["verify"],
    );
    keySet.imported.set(kid, key);
    return key;
  } catch {
    keySet.invalid.add(kid);
    return undefined;
  }
}

async function accessKeySet(teamDomain: string): Promise<AccessKeySetLookup> {
  const loading = accessKeySetLoads.get(teamDomain);
  if (loading) {
    return { entry: await loading, fromCache: false };
  }
  const now = Date.now();
  const cached = accessKeySetCache.get(teamDomain);
  if (cached && cached.expiresAt > now) {
    accessKeySetCache.delete(teamDomain);
    accessKeySetCache.set(teamDomain, cached);
    return { entry: cached, fromCache: true };
  }
  accessKeySetCache.delete(teamDomain);
  const load = fetchAccessKeySet(teamDomain).finally(() => accessKeySetLoads.delete(teamDomain));
  accessKeySetLoads.set(teamDomain, load);
  return { entry: await load, fromCache: false };
}

async function refreshAccessKeySet(teamDomain: string): Promise<AccessKeySetCacheEntry> {
  const loading = accessKeySetLoads.get(teamDomain);
  if (loading) {
    return loading;
  }
  accessKeySetCache.delete(teamDomain);
  const load = fetchAccessKeySet(teamDomain, true).finally(() =>
    accessKeySetLoads.delete(teamDomain),
  );
  accessKeySetLoads.set(teamDomain, load);
  return load;
}

async function fetchAccessKeySet(
  teamDomain: string,
  missRefreshUsed = false,
): Promise<AccessKeySetCacheEntry> {
  let keys: AccessPublicJwk[] = [];
  let ttl = accessKeySetFailureTTLMS;
  try {
    const response = await fetch(`https://${teamDomain}/cdn-cgi/access/certs`);
    if (response.ok) {
      const certs = (await response.json()) as AccessCerts;
      if (Array.isArray(certs.keys)) {
        keys = certs.keys;
        ttl = accessKeySetTTLMS;
      }
    }
  } catch {
    // Cache fetch failures briefly so an upstream outage cannot amplify request load.
  }
  const entry: AccessKeySetCacheEntry = {
    expiresAt: Date.now() + ttl,
    jwks: new Map(
      keys
        .filter(
          (key): key is AccessPublicJwk & { kid: string } =>
            typeof key.kid === "string" &&
            key.kid.length > 0 &&
            key.kid.length <= accessKidMaxChars,
        )
        .map((key) => [key.kid, key]),
    ),
    imported: new Map(),
    invalid: new Set(),
    missRefreshUsed,
  };
  accessKeySetCache.delete(teamDomain);
  accessKeySetCache.set(teamDomain, entry);
  while (accessKeySetCache.size > accessKeySetCacheMaxEntries) {
    const oldest = accessKeySetCache.keys().next().value;
    if (oldest === undefined) {
      break;
    }
    accessKeySetCache.delete(oldest);
  }
  return entry;
}

function normalizedAccessTeamDomain(value: string | undefined): string {
  let trimmed = value?.trim() ?? "";
  if (!trimmed) {
    return "";
  }
  const lower = trimmed.toLowerCase();
  if (lower.startsWith("https://")) {
    trimmed = trimmed.slice("https://".length);
  } else if (lower.startsWith("http://")) {
    trimmed = trimmed.slice("http://".length);
  }
  for (const separator of ["/", "?", "#"]) {
    const index = trimmed.indexOf(separator);
    if (index >= 0) {
      trimmed = trimmed.slice(0, index);
    }
  }
  return trimmed;
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
