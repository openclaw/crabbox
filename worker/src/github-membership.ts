import type { Env } from "./types";

const githubAPIURL = "https://api.github.com";
const defaultMembershipCacheSeconds = 5 * 60;
const maxMembershipCacheSeconds = 60 * 60;
const maxGitHubTeamPages = 10;
const membershipCacheMaxEntries = 1024;

interface GitHubTeam {
  slug?: string;
  organization?: {
    login?: string;
  };
}

interface GitHubUser {
  id?: number;
  login?: string;
}

interface AllowedGitHubTeam {
  org: string;
  slug: string;
}

export interface GitHubMembershipIdentity {
  accessToken: string;
  tokenID: string;
  owner: string;
  org: string;
  login: string;
}

export type GitHubMembershipEnv = Pick<
  Env,
  | "CRABBOX_DEFAULT_ORG"
  | "CRABBOX_GITHUB_ALLOWED_ORG"
  | "CRABBOX_GITHUB_ALLOWED_ORGS"
  | "CRABBOX_GITHUB_ALLOWED_TEAM"
  | "CRABBOX_GITHUB_ALLOWED_TEAMS"
  | "CRABBOX_GITHUB_REVOKED_USERS"
  | "CRABBOX_GITHUB_MEMBERSHIP_CACHE_SECONDS"
>;

const membershipCache = new Map<string, number>();
const membershipLoads = new Map<string, Promise<void>>();

export async function requireGitHubLoginMembership(
  accessToken: string,
  identity: Pick<GitHubMembershipIdentity, "owner" | "login">,
  requestedOrg: string,
  env: GitHubMembershipEnv,
): Promise<string> {
  requireSafeGitHubRevocationConfig(env);
  if (githubUserIsRevoked(identity, env)) {
    throw new GitHubAuthorizationError(`GitHub user ${identity.login} has been revoked.`);
  }
  const allowed = allowedGitHubOrgs(env);
  const requested = requestedOrg.toLowerCase();
  const org = allowed.includes(requested) ? requested : allowed[0];
  if (!org) {
    throw new GitHubAuthorizationError("GitHub login is not configured with an allowed org.");
  }
  return requireExactGitHubMembership(accessToken, identity.login, org, env);
}

export async function requireCurrentGitHubMembership(
  identity: GitHubMembershipIdentity,
  env: GitHubMembershipEnv,
): Promise<void> {
  requireSafeGitHubRevocationConfig(env);
  if (githubUserIsRevoked(identity, env)) {
    throw new GitHubAuthorizationError(`GitHub user ${identity.login} has been revoked.`);
  }
  if (!allowedGitHubOrgs(env).includes(identity.org.trim().toLowerCase())) {
    throw new GitHubAuthorizationError(`GitHub organization ${identity.org} is no longer allowed.`);
  }
  const key = membershipCacheKey(identity, env);
  const now = Date.now();
  const cachedUntil = membershipCache.get(key) ?? 0;
  if (cachedUntil > now) {
    return;
  }
  membershipCache.delete(key);
  const loading = membershipLoads.get(key);
  if (loading) {
    return loading;
  }
  const load = requireExactGitHubAccount(identity.accessToken, identity.owner, identity.login)
    .then(() =>
      requireExactGitHubMembership(identity.accessToken, identity.login, identity.org, env),
    )
    .then(() => {
      const ttlSeconds = membershipCacheSeconds(env);
      if (ttlSeconds > 0) {
        membershipCache.set(key, Date.now() + ttlSeconds * 1000);
        trimMembershipCache();
      }
      return undefined;
    })
    .finally(() => membershipLoads.delete(key));
  membershipLoads.set(key, load);
  return load;
}

function githubUserIsRevoked(
  identity: Pick<GitHubMembershipIdentity, "owner">,
  env: Pick<GitHubMembershipEnv, "CRABBOX_GITHUB_REVOKED_USERS">,
): boolean {
  const owner = identity.owner.trim().toLowerCase();
  return envList(env.CRABBOX_GITHUB_REVOKED_USERS).some((entry) => {
    if (entry.startsWith("owner:")) return entry.slice("owner:".length) === owner;
    return entry === owner;
  });
}

function requireSafeGitHubRevocationConfig(
  env: Pick<GitHubMembershipEnv, "CRABBOX_GITHUB_REVOKED_USERS">,
): void {
  const invalid = envList(env.CRABBOX_GITHUB_REVOKED_USERS).find((entry) => {
    if (githubAccountID(entry) !== undefined) return false;
    return (
      !entry.startsWith("owner:") || githubAccountID(entry.slice("owner:".length)) === undefined
    );
  });
  if (invalid) {
    throw new GitHubAuthorizationError(
      "CRABBOX_GITHUB_REVOKED_USERS contains a mutable or invalid selector. Replace email or login selectors with github:<numeric-id>.",
    );
  }
}

async function requireExactGitHubAccount(
  accessToken: string,
  owner: string,
  login: string,
): Promise<void> {
  const expectedID = githubAccountID(owner);
  if (expectedID === undefined) {
    throw new GitHubAuthorizationError(
      "This GitHub session uses a legacy mutable identity. Log in again.",
    );
  }
  const response = await fetch(`${githubAPIURL}/user`, { headers: githubHeaders(accessToken) });
  if (!response.ok) {
    throw new GitHubAuthorizationError(
      `Could not verify GitHub user ${login}: GitHub returned ${response.status}.`,
    );
  }
  const user = (await response.json()) as GitHubUser;
  if (typeof user.id !== "number" || !Number.isSafeInteger(user.id) || user.id !== expectedID) {
    throw new GitHubAuthorizationError("The GitHub credential no longer matches this session.");
  }
}

export function githubAccountID(owner: string): number | undefined {
  const match = /^github:([1-9][0-9]*)$/.exec(owner.trim().toLowerCase());
  if (!match) return undefined;
  const value = Number(match[1]);
  return Number.isSafeInteger(value) ? value : undefined;
}

async function requireExactGitHubMembership(
  accessToken: string,
  login: string,
  org: string,
  env: GitHubMembershipEnv,
): Promise<string> {
  const exactOrg = org.trim().toLowerCase();
  if (!allowedGitHubOrgs(env).includes(exactOrg)) {
    throw new GitHubAuthorizationError(`GitHub organization ${org} is no longer allowed.`);
  }
  const response = await fetch(
    `${githubAPIURL}/user/memberships/orgs/${encodeURIComponent(exactOrg)}`,
    { headers: githubHeaders(accessToken) },
  );
  if (!response.ok) {
    throw new GitHubAuthorizationError(
      `GitHub user ${login} is not an active member of ${exactOrg}.`,
    );
  }
  const membership = (await response.json()) as {
    state?: string;
    organization?: { login?: string };
  };
  if (membership.state !== "active" || membership.organization?.login?.toLowerCase() !== exactOrg) {
    throw new GitHubAuthorizationError(
      `GitHub user ${login} is not an active member of ${exactOrg}.`,
    );
  }
  await requireAllowedTeamMembership(accessToken, login, exactOrg, env);
  return membership.organization.login || exactOrg;
}

async function requireAllowedTeamMembership(
  accessToken: string,
  login: string,
  org: string,
  env: GitHubMembershipEnv,
): Promise<void> {
  const allowed = allowedGitHubTeams(env, org);
  if (allowed.length === 0) return;
  const allowedKeys = new Set(allowed.map((team) => teamKey(team.org, team.slug)));
  for (const team of await userGitHubTeams(accessToken)) {
    const teamOrg = team.organization?.login?.toLowerCase() ?? "";
    const teamSlug = team.slug?.toLowerCase() ?? "";
    if (teamOrg && teamSlug && allowedKeys.has(teamKey(teamOrg, teamSlug))) return;
  }
  throw new GitHubAuthorizationError(
    `GitHub user ${login} is not a member of an allowed team in ${org}.`,
  );
}

function allowedGitHubOrgs(env: GitHubMembershipEnv): string[] {
  const raw = env.CRABBOX_GITHUB_ALLOWED_ORGS || env.CRABBOX_GITHUB_ALLOWED_ORG;
  const configured = envList(raw);
  if (configured.length > 0) return configured;
  return envList(env.CRABBOX_DEFAULT_ORG);
}

function allowedGitHubTeams(env: GitHubMembershipEnv, defaultOrg: string): AllowedGitHubTeam[] {
  const raw = env.CRABBOX_GITHUB_ALLOWED_TEAMS || env.CRABBOX_GITHUB_ALLOWED_TEAM;
  return envList(raw)
    .map((value) => parseAllowedGitHubTeam(value, defaultOrg))
    .filter((team): team is AllowedGitHubTeam => team !== undefined);
}

function parseAllowedGitHubTeam(value: string, defaultOrg: string): AllowedGitHubTeam | undefined {
  const [org, slug] = value.includes("/") ? value.split("/", 2) : [defaultOrg, value];
  if (!org || !slug) return undefined;
  return { org, slug };
}

async function userGitHubTeams(accessToken: string): Promise<GitHubTeam[]> {
  const teams: GitHubTeam[] = [];
  for (let page = 1; page <= maxGitHubTeamPages; page += 1) {
    // oxlint-disable-next-line eslint/no-await-in-loop -- each page determines whether another exists.
    const response = await fetch(`${githubAPIURL}/user/teams?per_page=100&page=${page}`, {
      headers: githubHeaders(accessToken),
    });
    if (!response.ok) {
      throw new GitHubAuthorizationError(
        `Could not verify GitHub team membership: GitHub returned ${response.status}.`,
      );
    }
    // oxlint-disable-next-line eslint/no-await-in-loop -- parse the current page before continuing.
    const pageTeams = (await response.json()) as GitHubTeam[];
    teams.push(...pageTeams);
    if (pageTeams.length < 100) return teams;
  }
  return teams;
}

function membershipCacheKey(
  identity: Pick<GitHubMembershipIdentity, "tokenID" | "owner" | "org">,
  env: GitHubMembershipEnv,
): string {
  return JSON.stringify([
    identity.tokenID,
    identity.owner.toLowerCase(),
    identity.org.toLowerCase(),
    envList(env.CRABBOX_GITHUB_ALLOWED_ORGS || env.CRABBOX_GITHUB_ALLOWED_ORG),
    envList(env.CRABBOX_GITHUB_ALLOWED_TEAMS || env.CRABBOX_GITHUB_ALLOWED_TEAM),
    envList(env.CRABBOX_DEFAULT_ORG),
  ]);
}

function membershipCacheSeconds(
  env: Pick<GitHubMembershipEnv, "CRABBOX_GITHUB_MEMBERSHIP_CACHE_SECONDS">,
): number {
  const value = Number(env.CRABBOX_GITHUB_MEMBERSHIP_CACHE_SECONDS?.trim());
  if (!Number.isFinite(value) || value < 0) return defaultMembershipCacheSeconds;
  return Math.min(maxMembershipCacheSeconds, Math.trunc(value));
}

function trimMembershipCache(): void {
  const now = Date.now();
  for (const [key, expiresAt] of membershipCache) {
    if (expiresAt <= now) membershipCache.delete(key);
  }
  while (membershipCache.size > membershipCacheMaxEntries) {
    const oldest = membershipCache.keys().next().value;
    if (oldest === undefined) break;
    membershipCache.delete(oldest);
  }
}

function envList(value: string | undefined): string[] {
  return (value ?? "")
    .split(",")
    .map((item) => item.trim().toLowerCase())
    .filter(Boolean);
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

export class GitHubAuthorizationError extends Error {}
