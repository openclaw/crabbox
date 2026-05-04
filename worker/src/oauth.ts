import { issueUserToken, sha256Hex } from "./auth";
import { errorMessage, json, readJson } from "./http";
import type { Env, Provider } from "./types";
import { requestOrg } from "./usage";

const githubAuthorizeURL = "https://github.com/login/oauth/authorize";
const githubTokenURL = "https://github.com/login/oauth/access_token";
const githubAPIURL = "https://api.github.com";
const maxPendingOAuthLogins = 100;
const maxGitHubTeamPages = 10;

interface OAuthPending {
  id: string;
  state: string;
  pollSecretHash?: string;
  mode?: "cli" | "portal";
  provider?: Provider;
  returnTo?: string;
  createdAt: string;
  expiresAt: string;
  token?: string;
  owner?: string;
  org?: string;
  login?: string;
  error?: string;
}

interface GitHubUser {
  login?: string;
  name?: string | null;
  email?: string | null;
}

interface GitHubEmail {
  email?: string;
  primary?: boolean;
  verified?: boolean;
}

interface GitHubTeam {
  slug?: string;
  organization?: {
    login?: string;
  };
}

interface AllowedGitHubTeam {
  org: string;
  slug: string;
}

export async function githubAuthRoute(
  request: Request,
  action: string | undefined,
  storage: DurableObjectStorage,
  env: Env,
): Promise<Response> {
  const method = request.method.toUpperCase();
  if (method === "POST" && action === "start") {
    return await githubAuthStart(request, storage, env);
  }
  if (method === "GET" && action === "callback") {
    return await githubAuthCallback(request, storage, env);
  }
  if (method === "POST" && action === "poll") {
    return await githubAuthPoll(request, storage);
  }
  return json({ error: "not_found" }, { status: 404 });
}

export async function githubPortalLogin(
  request: Request,
  storage: DurableObjectStorage,
  env: Env,
): Promise<Response> {
  const clientID = env.CRABBOX_GITHUB_CLIENT_ID;
  if (!clientID || !env.CRABBOX_GITHUB_CLIENT_SECRET) {
    return html("Crabbox login unavailable", "GitHub OAuth is not configured.", 503);
  }
  const pendingCount = await cleanupExpiredPendingOAuth(storage);
  if (pendingCount >= maxPendingOAuthLogins) {
    return html("Crabbox login busy", "Too many pending GitHub logins. Try again shortly.", 429);
  }
  const url = new URL(request.url);
  const id = randomID("login");
  const state = randomID("state");
  const now = new Date();
  const expiresAt = new Date(now.getTime() + 10 * 60 * 1000);
  const pending: OAuthPending = {
    id,
    state,
    mode: "portal",
    returnTo: safePortalReturnTo(url.searchParams.get("returnTo")),
    createdAt: now.toISOString(),
    expiresAt: expiresAt.toISOString(),
  };
  await storage.put(oauthKey(id), pending);
  await storage.put(oauthStateKey(state), id);

  const authorize = new URL(githubAuthorizeURL);
  authorize.searchParams.set("client_id", clientID);
  authorize.searchParams.set("redirect_uri", githubRedirectURI(request, env));
  authorize.searchParams.set("scope", "read:user user:email read:org");
  authorize.searchParams.set("state", state);
  return redirect(authorize.toString(), 302);
}

export function githubPortalLogout(): Response {
  return html(
    "Crabbox logged out",
    "Your Crabbox portal session has ended.",
    200,
    {
      "set-cookie": portalSessionCookie("", 0),
    },
    `<p><a href="/portal/login">Log in again</a></p>`,
  );
}

async function githubAuthStart(
  request: Request,
  storage: DurableObjectStorage,
  env: Env,
): Promise<Response> {
  const clientID = env.CRABBOX_GITHUB_CLIENT_ID;
  if (!clientID || !env.CRABBOX_GITHUB_CLIENT_SECRET) {
    return json(
      { error: "github_oauth_not_configured", message: "GitHub OAuth is not configured" },
      { status: 503 },
    );
  }
  const input = await readJson<{
    pollSecretHash?: string;
    provider?: Provider;
  }>(request);
  if (!input.pollSecretHash || !/^[a-f0-9]{64}$/.test(input.pollSecretHash)) {
    return json({ error: "invalid_poll_secret_hash" }, { status: 400 });
  }
  const pendingCount = await cleanupExpiredPendingOAuth(storage);
  if (pendingCount >= maxPendingOAuthLogins) {
    return json(
      {
        error: "login_rate_limited",
        message: "Too many pending GitHub logins. Try again shortly.",
      },
      { status: 429 },
    );
  }
  const id = randomID("login");
  const state = randomID("state");
  const now = new Date();
  const expiresAt = new Date(now.getTime() + 10 * 60 * 1000);
  const pending: OAuthPending = {
    id,
    state,
    mode: "cli",
    pollSecretHash: input.pollSecretHash,
    createdAt: now.toISOString(),
    expiresAt: expiresAt.toISOString(),
  };
  if (input.provider === "aws" || input.provider === "hetzner") {
    pending.provider = input.provider;
  }
  await storage.put(oauthKey(id), pending);
  await storage.put(oauthStateKey(state), id);

  const authorize = new URL(githubAuthorizeURL);
  authorize.searchParams.set("client_id", clientID);
  authorize.searchParams.set("redirect_uri", githubRedirectURI(request, env));
  authorize.searchParams.set("scope", "read:user user:email read:org");
  authorize.searchParams.set("state", state);
  return json({
    loginID: id,
    url: authorize.toString(),
    expiresAt: expiresAt.toISOString(),
  });
}

async function githubAuthCallback(
  request: Request,
  storage: DurableObjectStorage,
  env: Env,
): Promise<Response> {
  const url = new URL(request.url);
  const code = url.searchParams.get("code") ?? "";
  const state = url.searchParams.get("state") ?? "";
  const error = url.searchParams.get("error") ?? "";
  const id = state ? await storage.get<string>(oauthStateKey(state)) : undefined;
  const pending = id ? await storage.get<OAuthPending>(oauthKey(id)) : undefined;
  if (!pending || pending.state !== state || Date.parse(pending.expiresAt) <= Date.now()) {
    return html(
      "Crabbox login expired",
      "The login request expired. Run crabbox login again.",
      400,
    );
  }
  if (error || !code) {
    pending.error = error || "missing_code";
    await storage.put(oauthKey(pending.id), pending);
    return html("Crabbox login failed", "GitHub did not authorize the login.", 400);
  }
  try {
    const accessToken = await exchangeGitHubCode(code, githubRedirectURI(request, env), env);
    const identity = await githubIdentity(accessToken);
    const requestedOrg = requestOrg(
      new Request(request.url, {
        headers: { "x-crabbox-org": env.CRABBOX_DEFAULT_ORG ?? "openclaw" },
      }),
      env,
    );
    const org = await requireAllowedOrgMembership(accessToken, identity.login, requestedOrg, env);
    await requireAllowedTeamMembership(accessToken, identity.login, org, env);
    const tokenInput = {
      owner: identity.owner,
      org,
      login: identity.login,
    } as { owner: string; org: string; login: string; name?: string };
    if (identity.name) {
      tokenInput.name = identity.name;
    }
    pending.token = await issueUserToken(env, tokenInput);
    pending.owner = identity.owner;
    pending.org = org;
    pending.login = identity.login;
    await storage.put(oauthKey(pending.id), pending);
    if (pending.mode === "portal") {
      await deletePendingOAuth(storage, pending);
      return redirect(pending.returnTo || "/portal", 302, {
        "set-cookie": portalSessionCookie(pending.token, 30 * 24 * 60 * 60),
      });
    }
    return html(
      "Crabbox login complete",
      "GitHub authorized Crabbox. You can close this tab and return to the terminal.",
    );
  } catch (err) {
    pending.error = errorMessage(err);
    await storage.put(oauthKey(pending.id), pending);
    if (err instanceof GitHubAuthorizationError) {
      return html("Crabbox login denied", err.message, 403);
    }
    return html("Crabbox login failed", "The coordinator could not finish GitHub login.", 500);
  }
}

async function githubAuthPoll(request: Request, storage: DurableObjectStorage): Promise<Response> {
  const input = await readJson<{ loginID?: string; pollSecret?: string }>(request);
  if (!input.loginID || !input.pollSecret) {
    return json({ error: "invalid_poll" }, { status: 400 });
  }
  const pending = await storage.get<OAuthPending>(oauthKey(input.loginID));
  if (!pending || Date.parse(pending.expiresAt) <= Date.now()) {
    return json({ status: "expired" }, { status: 410 });
  }
  if (pending.mode === "portal") {
    return json({ error: "forbidden" }, { status: 403 });
  }
  if (!pending.pollSecretHash) {
    return json({ error: "invalid_poll" }, { status: 400 });
  }
  if ((await sha256Hex(input.pollSecret)) !== pending.pollSecretHash) {
    return json({ error: "forbidden" }, { status: 403 });
  }
  if (pending.error) {
    await deletePendingOAuth(storage, pending);
    return json({ status: "failed", error: pending.error }, { status: 400 });
  }
  if (!pending.token) {
    return json({ status: "pending", expiresAt: pending.expiresAt });
  }
  const response = {
    status: "complete",
    token: pending.token,
    owner: pending.owner,
    org: pending.org,
    login: pending.login,
    provider: pending.provider,
  };
  await deletePendingOAuth(storage, pending);
  return json(response);
}

function githubRedirectURI(request: Request, env: Env): string {
  const base = env.CRABBOX_PUBLIC_URL || new URL(request.url).origin;
  return `${base.replace(/\/+$/, "")}/v1/auth/github/callback`;
}

async function exchangeGitHubCode(code: string, redirectURI: string, env: Env): Promise<string> {
  const body = new URLSearchParams({
    client_id: env.CRABBOX_GITHUB_CLIENT_ID ?? "",
    client_secret: env.CRABBOX_GITHUB_CLIENT_SECRET ?? "",
    code,
    redirect_uri: redirectURI,
  });
  const response = await fetch(githubTokenURL, {
    method: "POST",
    headers: {
      accept: "application/json",
      "content-type": "application/x-www-form-urlencoded",
      "user-agent": "crabbox-coordinator",
    },
    body,
  });
  const data = (await response.json()) as { access_token?: string; error?: string };
  if (!response.ok || !data.access_token) {
    throw new Error(data.error || `github token exchange failed: ${response.status}`);
  }
  return data.access_token;
}

async function githubIdentity(accessToken: string): Promise<{
  owner: string;
  login: string;
  name?: string;
}> {
  const headers = {
    accept: "application/vnd.github+json",
    authorization: `Bearer ${accessToken}`,
    "user-agent": "crabbox-coordinator",
    "x-github-api-version": "2022-11-28",
  };
  const userResponse = await fetch(`${githubAPIURL}/user`, { headers });
  if (!userResponse.ok) {
    throw new Error(`github user lookup failed: ${userResponse.status}`);
  }
  const user = (await userResponse.json()) as GitHubUser;
  const login = user.login || "unknown";
  let owner = user.email || "";
  const emailResponse = await fetch(`${githubAPIURL}/user/emails`, { headers });
  if (emailResponse.ok) {
    const emails = (await emailResponse.json()) as GitHubEmail[];
    owner =
      emails.find((email) => email.primary && email.verified)?.email ||
      emails.find((email) => email.verified)?.email ||
      owner;
  }
  if (!owner) {
    owner = `${login}@users.noreply.github.com`;
  }
  const identity = {
    owner,
    login,
  } as { owner: string; login: string; name?: string };
  if (user.name) {
    identity.name = user.name;
  }
  return identity;
}

async function requireAllowedOrgMembership(
  accessToken: string,
  login: string,
  requestedOrg: string,
  env: Env,
): Promise<string> {
  const allowed = allowedGitHubOrgs(env);
  const requested = requestedOrg.toLowerCase();
  const org = allowed.includes(requested) ? requested : allowed[0];
  if (!org) {
    throw new GitHubAuthorizationError("GitHub login is not configured with an allowed org.");
  }
  const response = await fetch(`${githubAPIURL}/user/memberships/orgs/${encodeURIComponent(org)}`, {
    headers: githubHeaders(accessToken),
  });
  if (!response.ok) {
    throw new GitHubAuthorizationError(`GitHub user ${login} is not an active member of ${org}.`);
  }
  const membership = (await response.json()) as {
    state?: string;
    organization?: { login?: string };
  };
  if (
    membership.state !== "active" ||
    membership.organization?.login?.toLowerCase() !== org.toLowerCase()
  ) {
    throw new GitHubAuthorizationError(`GitHub user ${login} is not an active member of ${org}.`);
  }
  return membership.organization.login || org;
}

async function requireAllowedTeamMembership(
  accessToken: string,
  login: string,
  org: string,
  env: Env,
): Promise<void> {
  const allowed = allowedGitHubTeams(env, org);
  if (allowed.length === 0) {
    return;
  }
  const allowedKeys = new Set(allowed.map((team) => teamKey(team.org, team.slug)));
  for (const team of await userGitHubTeams(accessToken)) {
    const teamOrg = team.organization?.login?.toLowerCase() ?? "";
    const teamSlug = team.slug?.toLowerCase() ?? "";
    if (teamOrg && teamSlug && allowedKeys.has(teamKey(teamOrg, teamSlug))) {
      return;
    }
  }
  throw new GitHubAuthorizationError(
    `GitHub user ${login} is not a member of an allowed team in ${org}.`,
  );
}

function allowedGitHubOrgs(env: Env): string[] {
  const raw = env.CRABBOX_GITHUB_ALLOWED_ORGS || env.CRABBOX_GITHUB_ALLOWED_ORG;
  const values = raw ? raw.split(",") : [env.CRABBOX_DEFAULT_ORG || "openclaw"];
  return values.map((value) => value.trim().toLowerCase()).filter(Boolean);
}

function allowedGitHubTeams(env: Env, defaultOrg: string): AllowedGitHubTeam[] {
  const raw = env.CRABBOX_GITHUB_ALLOWED_TEAMS || env.CRABBOX_GITHUB_ALLOWED_TEAM;
  if (!raw) {
    return [];
  }
  return raw
    .split(",")
    .map((value) => parseAllowedGitHubTeam(value, defaultOrg))
    .filter((team): team is AllowedGitHubTeam => team !== undefined);
}

function parseAllowedGitHubTeam(value: string, defaultOrg: string): AllowedGitHubTeam | undefined {
  const trimmed = value.trim().toLowerCase();
  if (!trimmed) {
    return undefined;
  }
  const [org, slug] = trimmed.includes("/") ? trimmed.split("/", 2) : [defaultOrg, trimmed];
  if (!org || !slug) {
    return undefined;
  }
  return { org, slug };
}

async function userGitHubTeams(accessToken: string): Promise<GitHubTeam[]> {
  return userGitHubTeamsPage(accessToken, 1, []);
}

async function userGitHubTeamsPage(
  accessToken: string,
  page: number,
  teams: GitHubTeam[],
): Promise<GitHubTeam[]> {
  const url = `${githubAPIURL}/user/teams?per_page=100&page=${page}`;
  const response = await fetch(url, { headers: githubHeaders(accessToken) });
  if (!response.ok) {
    throw new GitHubAuthorizationError(
      `Could not verify GitHub team membership: GitHub returned ${response.status}.`,
    );
  }
  const pageTeams = (await response.json()) as GitHubTeam[];
  const next = [...teams, ...pageTeams];
  if (pageTeams.length < 100 || page >= maxGitHubTeamPages) {
    return next;
  }
  return userGitHubTeamsPage(accessToken, page + 1, next);
}

function teamKey(org: string, slug: string): string {
  return `${org.toLowerCase()}/${slug.toLowerCase()}`;
}

function githubHeaders(accessToken: string): Record<string, string> {
  return {
    accept: "application/vnd.github+json",
    authorization: `Bearer ${accessToken}`,
    "user-agent": "crabbox-coordinator",
    "x-github-api-version": "2022-11-28",
  };
}

class GitHubAuthorizationError extends Error {}

async function deletePendingOAuth(
  storage: DurableObjectStorage,
  pending: OAuthPending,
): Promise<void> {
  await storage.delete(oauthKey(pending.id));
  await storage.delete(oauthStateKey(pending.state));
}

async function cleanupExpiredPendingOAuth(storage: DurableObjectStorage): Promise<number> {
  const entries = await storage.list<OAuthPending>({ prefix: "oauth:" });
  let active = 0;
  const now = Date.now();
  await Promise.all(
    [...entries.values()].map(async (pending) => {
      if (Date.parse(pending.expiresAt) <= now) {
        await deletePendingOAuth(storage, pending);
        return;
      }
      active += 1;
    }),
  );
  return active;
}

function oauthKey(id: string): string {
  return `oauth:${id}`;
}

function oauthStateKey(state: string): string {
  return `oauth_state:${state}`;
}

function randomID(prefix: string): string {
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  return `${prefix}_${[...bytes].map((byte) => byte.toString(16).padStart(2, "0")).join("")}`;
}

function html(
  title: string,
  message: string,
  status = 200,
  headers: Record<string, string> = {},
  extraBody = "",
): Response {
  const escapedTitle = escapeHTML(title);
  const escapedMessage = escapeHTML(message);
  return new Response(
    `<!doctype html><html><head><meta charset="utf-8"><title>${escapedTitle}</title><style>body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;max-width:42rem;margin:5rem auto;padding:0 1rem;line-height:1.5;color:#111}code{background:#f4f4f5;padding:.15rem .3rem;border-radius:4px}</style></head><body><h1>${escapedTitle}</h1><p>${escapedMessage}</p>${extraBody}</body></html>`,
    { status, headers: { "content-type": "text/html; charset=utf-8", ...headers } },
  );
}

function redirect(location: string, status = 302, headers: Record<string, string> = {}): Response {
  return new Response(null, {
    status,
    headers: {
      location,
      ...headers,
    },
  });
}

function portalSessionCookie(value: string, maxAgeSeconds: number): string {
  const attrs = [
    `crabbox_session=${encodeURIComponent(value)}`,
    "Path=/",
    "HttpOnly",
    "Secure",
    "SameSite=Lax",
    `Max-Age=${Math.max(0, Math.trunc(maxAgeSeconds))}`,
  ];
  return attrs.join("; ");
}

function safePortalReturnTo(value: string | null): string {
  if (!value || !value.startsWith("/portal")) {
    return "/portal";
  }
  if (value.startsWith("//") || value.includes("://")) {
    return "/portal";
  }
  return value;
}

function escapeHTML(value: string): string {
  return value
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;");
}
