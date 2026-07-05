import {
  issueUserToken,
  sha256Hex,
  userTokenExpiresAt,
  userTokenSigningConfigurationError,
} from "./auth";
import type { CoordinatorRuntime, CoordinatorStorage } from "./coordinator-runtime";
import { errorMessage, json, readJson } from "./http";
import type { Env, Provider } from "./types";
import { requestOrg } from "./usage";

const githubAuthorizeURL = "https://github.com/login/oauth/authorize";
const githubTokenURL = "https://github.com/login/oauth/access_token";
const githubAPIURL = "https://api.github.com";
const maxPendingOAuthLogins = 100;
const maxGitHubTeamPages = 10;
const defaultUserTokenTTLSeconds = 180 * 24 * 60 * 60;
const minUserTokenTTLSeconds = 60 * 60;
const maxUserTokenTTLSeconds = 365 * 24 * 60 * 60;

interface OAuthPending {
  id: string;
  state: string;
  pollSecretHash?: string;
  browserConfirmationHash?: string;
  mode?: "cli" | "portal";
  provider?: Provider;
  returnTo?: string;
  loopbackRedirectURI?: string;
  redirectURI: string;
  createdAt: string;
  expiresAt: string;
  token?: string;
  tokenExpiresAt?: string;
  owner?: string;
  org?: string;
  login?: string;
  error?: string;
  callbackClaim?: string;
}

interface GitHubUser {
  login?: string;
  name?: string | null;
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
  runtime: Pick<CoordinatorRuntime, "storage" | "runExclusive">,
  env: Env,
): Promise<Response> {
  const method = request.method.toUpperCase();
  if (method === "POST" && action === "start") {
    return await githubAuthStart(request, runtime.storage, env);
  }
  if (method === "GET" && action === "callback") {
    return await githubAuthCallback(request, runtime, env);
  }
  if (method === "POST" && action === "poll") {
    return await githubAuthPoll(request, runtime.storage);
  }
  return json({ error: "not_found" }, { status: 404 });
}

export async function githubPortalLogin(
  request: Request,
  storage: CoordinatorStorage,
  env: Env,
): Promise<Response> {
  const clientID = env.CRABBOX_GITHUB_CLIENT_ID;
  if (!clientID || !env.CRABBOX_GITHUB_CLIENT_SECRET) {
    return html("Crabbox login unavailable", "GitHub OAuth is not configured.", 503);
  }
  const oauthConfig = githubOAuthConfiguration(env);
  if ("error" in oauthConfig) {
    return html("Crabbox login unavailable", oauthConfig.message, 503);
  }
  const signingError = userTokenSigningConfigurationError(env);
  if (signingError) {
    return html("Crabbox login unavailable", signingError, 503);
  }
  const pendingCount = await cleanupExpiredPendingOAuth(storage);
  if (pendingCount >= maxPendingOAuthLogins) {
    return html("Crabbox login busy", "Too many pending GitHub logins. Try again shortly.", 429);
  }
  const url = new URL(request.url);
  const pending = newPendingOAuth({
    mode: "portal",
    returnTo: safePortalReturnTo(url.searchParams.get("returnTo")),
    redirectURI: oauthConfig.redirectURI,
  });
  await storePendingOAuth(storage, pending);

  return redirect(githubAuthorizeURLFor(clientID, pending.state, pending.redirectURI), 302);
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
  storage: CoordinatorStorage,
  env: Env,
): Promise<Response> {
  const clientID = env.CRABBOX_GITHUB_CLIENT_ID;
  if (!clientID || !env.CRABBOX_GITHUB_CLIENT_SECRET) {
    return json(
      { error: "github_oauth_not_configured", message: "GitHub OAuth is not configured" },
      { status: 503 },
    );
  }
  const oauthConfig = githubOAuthConfiguration(env);
  if ("error" in oauthConfig) {
    return json({ error: oauthConfig.error, message: oauthConfig.message }, { status: 503 });
  }
  const signingError = userTokenSigningConfigurationError(env);
  if (signingError) {
    return json({ error: "github_session_secret_invalid", message: signingError }, { status: 503 });
  }
  const input = await readJson<{
    pollSecretHash?: string;
    provider?: Provider;
    loopbackRedirectURI?: string;
  }>(request);
  if (!input.pollSecretHash || !/^[a-f0-9]{64}$/.test(input.pollSecretHash)) {
    return json({ error: "invalid_poll_secret_hash" }, { status: 400 });
  }
  const loopbackRedirectURI = validLoopbackRedirectURI(input.loopbackRedirectURI);
  if (!loopbackRedirectURI) {
    return json(
      {
        error: "loopback_redirect_required",
        message: "Upgrade the Crabbox CLI to complete GitHub login on this device.",
      },
      { status: 400 },
    );
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
  const pending = newPendingOAuth({
    mode: "cli",
    pollSecretHash: input.pollSecretHash,
    loopbackRedirectURI,
    redirectURI: oauthConfig.redirectURI,
  });
  if (input.provider === "aws" || input.provider === "hetzner" || input.provider === "gcp") {
    pending.provider = input.provider;
  }
  await storePendingOAuth(storage, pending);

  return json({
    loginID: pending.id,
    url: githubAuthorizeURLFor(clientID, pending.state, pending.redirectURI),
    expiresAt: pending.expiresAt,
  });
}

function newPendingOAuth(
  input: Omit<OAuthPending, "id" | "state" | "createdAt" | "expiresAt">,
): OAuthPending {
  const now = new Date();
  return {
    ...input,
    id: randomID("login"),
    state: randomID("state"),
    createdAt: now.toISOString(),
    expiresAt: new Date(now.getTime() + 10 * 60 * 1000).toISOString(),
  };
}

async function storePendingOAuth(
  storage: CoordinatorStorage,
  pending: OAuthPending,
): Promise<void> {
  await storage.put(oauthKey(pending.id), pending);
  await storage.put(oauthStateKey(pending.state), pending.id);
}

function githubAuthorizeURLFor(clientID: string, state: string, redirectURI: string): string {
  const authorize = new URL(githubAuthorizeURL);
  authorize.searchParams.set("client_id", clientID);
  authorize.searchParams.set("redirect_uri", redirectURI);
  authorize.searchParams.set("scope", "read:user user:email read:org");
  authorize.searchParams.set("state", state);
  return authorize.toString();
}

async function githubAuthCallback(
  request: Request,
  runtime: Pick<CoordinatorRuntime, "storage" | "runExclusive">,
  env: Env,
): Promise<Response> {
  const url = new URL(request.url);
  const oauthConfig = githubOAuthConfiguration(env);
  if ("error" in oauthConfig) {
    return html("Crabbox login unavailable", oauthConfig.message, 503);
  }
  if (url.origin !== new URL(oauthConfig.redirectURI).origin) {
    return html(
      "Crabbox login denied",
      "The GitHub OAuth callback did not arrive on the configured public origin.",
      403,
    );
  }
  const code = url.searchParams.get("code") ?? "";
  const state = url.searchParams.get("state") ?? "";
  const error = url.searchParams.get("error") ?? "";
  const claimed = await claimPendingOAuth(runtime, state);
  if ("response" in claimed) {
    return claimed.response;
  }
  const { pending, claim } = claimed;
  if (pending.redirectURI !== oauthConfig.redirectURI) {
    const message = "The GitHub OAuth public origin changed. Start a new login.";
    await finishPendingOAuth(runtime, pending.id, claim, { error: message });
    return html("Crabbox login unavailable", message, 503);
  }
  if (error || !code) {
    await finishPendingOAuth(runtime, pending.id, claim, {
      error: error || "missing_code",
    });
    return html("Crabbox login failed", "GitHub did not authorize the login.", 400);
  }
  const signingError = userTokenSigningConfigurationError(env);
  if (signingError) {
    await finishPendingOAuth(runtime, pending.id, claim, { error: signingError });
    return html("Crabbox login unavailable", signingError, 503);
  }
  try {
    const accessToken = await exchangeGitHubCode(code, pending.redirectURI, env);
    const identity = await githubIdentity(accessToken);
    const requestedOrg = requestOrg(
      new Request(request.url, {
        headers: { "x-crabbox-org": env.CRABBOX_DEFAULT_ORG ?? "" },
      }),
      env,
    );
    const org = await requireAllowedOrgMembership(accessToken, identity.login, requestedOrg, env);
    await requireAllowedTeamMembership(accessToken, identity.login, org, env);
    const ttlSeconds = userTokenTTLSeconds(env);
    const tokenInput = {
      owner: identity.owner,
      ownerSource: identity.ownerSource,
      org,
      login: identity.login,
      ttlSeconds,
    } as {
      owner: string;
      ownerSource: "github-verified-email";
      org: string;
      login: string;
      name?: string;
      ttlSeconds: number;
    };
    if (identity.name) {
      tokenInput.name = identity.name;
    }
    const token = await issueUserToken(env, tokenInput);
    const completion: Pick<OAuthPending, "token" | "owner" | "org" | "login"> & {
      tokenExpiresAt?: string;
    } = {
      token,
      owner: identity.owner,
      org,
      login: identity.login,
    };
    const tokenExpiresAt = userTokenExpiresAt(token);
    if (tokenExpiresAt) {
      completion.tokenExpiresAt = tokenExpiresAt;
    }
    const browserConfirmation = randomID("confirm");
    const completed = await finishPendingOAuth(runtime, pending.id, claim, {
      ...completion,
      browserConfirmationHash: await sha256Hex(browserConfirmation),
    });
    if (!completed) {
      return expiredOAuthResponse();
    }
    if (completed.mode === "portal") {
      await runtime.runExclusive(() => deletePendingOAuth(runtime.storage, completed));
      return redirect(completed.returnTo || "/portal", 302, {
        "set-cookie": portalSessionCookie(token, ttlSeconds),
      });
    }
    if (!completed.loopbackRedirectURI) {
      await runtime.runExclusive(() => deletePendingOAuth(runtime.storage, completed));
      return html(
        "Crabbox login unavailable",
        "This login was started by an outdated client. Upgrade Crabbox and try again.",
        400,
      );
    }
    const loopback = new URL(completed.loopbackRedirectURI);
    loopback.searchParams.set("confirmation", browserConfirmation);
    return redirect(loopback.toString(), 303, {
      "cache-control": "no-store",
      "referrer-policy": "no-referrer",
    });
  } catch (err) {
    await finishPendingOAuth(runtime, pending.id, claim, { error: errorMessage(err) });
    if (err instanceof GitHubAuthorizationError) {
      return html("Crabbox login denied", err.message, 403);
    }
    return html("Crabbox login failed", "The coordinator could not finish GitHub login.", 500);
  }
}

async function claimPendingOAuth(
  runtime: Pick<CoordinatorRuntime, "storage" | "runExclusive">,
  state: string,
): Promise<
  | { pending: OAuthPending; claim: string }
  | {
      response: Response;
    }
> {
  return runtime.runExclusive(async () => {
    const id = state ? await runtime.storage.get<string>(oauthStateKey(state)) : undefined;
    const pending = id ? await runtime.storage.get<OAuthPending>(oauthKey(id)) : undefined;
    if (!pending || pending.state !== state || Date.parse(pending.expiresAt) <= Date.now()) {
      return { response: expiredOAuthResponse() };
    }
    if (pending.callbackClaim || pending.error || pending.token) {
      return {
        response: html(
          "Crabbox login already used",
          "This GitHub login callback is already being processed or completed.",
          409,
        ),
      };
    }
    const claim = randomID("callback");
    pending.callbackClaim = claim;
    await runtime.storage.put(oauthKey(pending.id), pending);
    return { pending: structuredClone(pending), claim };
  });
}

async function finishPendingOAuth(
  runtime: Pick<CoordinatorRuntime, "storage" | "runExclusive">,
  id: string,
  claim: string,
  result: Partial<
    Pick<
      OAuthPending,
      "token" | "tokenExpiresAt" | "owner" | "org" | "login" | "error" | "browserConfirmationHash"
    >
  >,
): Promise<OAuthPending | undefined> {
  return runtime.runExclusive(async () => {
    const pending = await runtime.storage.get<OAuthPending>(oauthKey(id));
    if (
      !pending ||
      pending.callbackClaim !== claim ||
      Date.parse(pending.expiresAt) <= Date.now()
    ) {
      return undefined;
    }
    Object.assign(pending, result);
    delete pending.callbackClaim;
    await runtime.storage.put(oauthKey(pending.id), pending);
    return structuredClone(pending);
  });
}

function expiredOAuthResponse(): Response {
  return html(
    "Crabbox login expired",
    "The login request expired. Run crabbox login --url <broker-url> again.",
    400,
  );
}

async function githubAuthPoll(request: Request, storage: CoordinatorStorage): Promise<Response> {
  const input = await readJson<{
    loginID?: string;
    pollSecret?: string;
    browserConfirmation?: string;
  }>(request);
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
  if (!pending.loopbackRedirectURI || !pending.browserConfirmationHash) {
    await deletePendingOAuth(storage, pending);
    return json(
      { status: "failed", error: "This login was started by an outdated client." },
      { status: 400 },
    );
  }
  if (!input.browserConfirmation) {
    return json({ status: "confirmation_required", expiresAt: pending.expiresAt });
  }
  if (
    !/^confirm_[a-f0-9]{32}$/.test(input.browserConfirmation) ||
    (await sha256Hex(input.browserConfirmation)) !== pending.browserConfirmationHash
  ) {
    return json({ error: "forbidden" }, { status: 403 });
  }
  const response = {
    status: "complete",
    token: pending.token,
    owner: pending.owner,
    org: pending.org,
    login: pending.login,
    provider: pending.provider,
    tokenExpiresAt: pending.tokenExpiresAt,
  };
  await deletePendingOAuth(storage, pending);
  return json(response);
}

function userTokenTTLSeconds(env: Pick<Env, "CRABBOX_USER_TOKEN_TTL_SECONDS">): number {
  const configured = env.CRABBOX_USER_TOKEN_TTL_SECONDS?.trim();
  if (!configured) {
    return defaultUserTokenTTLSeconds;
  }
  const value = Number(configured);
  if (!Number.isFinite(value) || value <= 0) {
    return defaultUserTokenTTLSeconds;
  }
  return Math.min(maxUserTokenTTLSeconds, Math.max(minUserTokenTTLSeconds, Math.trunc(value)));
}

function validLoopbackRedirectURI(value: string | undefined): string | undefined {
  if (!value) {
    return undefined;
  }
  let parsed: URL;
  try {
    parsed = new URL(value);
  } catch {
    return undefined;
  }
  const port = Number(parsed.port);
  if (
    parsed.protocol !== "http:" ||
    parsed.hostname !== "127.0.0.1" ||
    !Number.isInteger(port) ||
    port < 1 ||
    port > 65535 ||
    parsed.username !== "" ||
    parsed.password !== "" ||
    parsed.search !== "" ||
    parsed.hash !== "" ||
    !/^\/crabbox\/oauth\/[a-f0-9]{64}$/.test(parsed.pathname)
  ) {
    return undefined;
  }
  return parsed.toString();
}

function githubOAuthConfiguration(
  env: Pick<Env, "CRABBOX_PUBLIC_URL">,
): { redirectURI: string } | { error: string; message: string } {
  const configured = env.CRABBOX_PUBLIC_URL?.trim();
  if (!configured) {
    return {
      error: "github_public_url_required",
      message: "CRABBOX_PUBLIC_URL is required before GitHub OAuth can start.",
    };
  }
  let publicURL: URL;
  try {
    publicURL = new URL(configured);
  } catch {
    return {
      error: "github_public_url_invalid",
      message: "CRABBOX_PUBLIC_URL must be a valid canonical HTTPS origin.",
    };
  }
  const loopbackHTTP =
    publicURL.protocol === "http:" &&
    (publicURL.hostname === "localhost" ||
      publicURL.hostname === "127.0.0.1" ||
      publicURL.hostname === "[::1]");
  if (
    (publicURL.protocol !== "https:" && !loopbackHTTP) ||
    publicURL.username !== "" ||
    publicURL.password !== ""
  ) {
    return {
      error: "github_public_url_invalid",
      message:
        "CRABBOX_PUBLIC_URL must be a canonical HTTPS origin (HTTP is allowed only for loopback development).",
    };
  }
  return { redirectURI: `${publicURL.origin}/v1/auth/github/callback` };
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
  ownerSource: "github-verified-email";
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
  const emailResponse = await fetch(`${githubAPIURL}/user/emails`, { headers });
  if (!emailResponse.ok) {
    throw new Error(`github email lookup failed: ${emailResponse.status}`);
  }
  const emails = (await emailResponse.json()) as GitHubEmail[];
  const verifiedEmails = emails.filter(
    (email) => email.verified && typeof email.email === "string" && email.email.trim(),
  );
  const owner = (
    verifiedEmails.find((email) => email.primary)?.email || verifiedEmails[0]?.email
  )?.trim();
  if (!owner) {
    throw new GitHubAuthorizationError("GitHub account must have a verified email to use Crabbox.");
  }
  const identity = {
    owner,
    ownerSource: "github-verified-email",
    login,
  } as {
    owner: string;
    ownerSource: "github-verified-email";
    login: string;
    name?: string;
  };
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
  const configured = raw
    ? raw
        .split(",")
        .map((value) => value.trim().toLowerCase())
        .filter(Boolean)
    : [];
  if (configured.length > 0) {
    return configured;
  }
  return [(env.CRABBOX_DEFAULT_ORG || "").trim().toLowerCase()].filter(Boolean);
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
  storage: CoordinatorStorage,
  pending: OAuthPending,
): Promise<void> {
  await storage.delete(oauthKey(pending.id));
  await storage.delete(oauthStateKey(pending.state));
}

async function cleanupExpiredPendingOAuth(storage: CoordinatorStorage): Promise<number> {
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
  if (value.startsWith("//") || value.includes("://") || hasHeaderControlCharacter(value)) {
    return "/portal";
  }
  return value;
}

function hasHeaderControlCharacter(value: string): boolean {
  for (let index = 0; index < value.length; index += 1) {
    const code = value.charCodeAt(index);
    if (code <= 0x1f || code === 0x7f) return true;
  }
  return false;
}

function escapeHTML(value: string): string {
  return value
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;");
}
