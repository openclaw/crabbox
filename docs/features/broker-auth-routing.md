# Broker Auth And Routing

Read when:

- changing coordinator authentication;
- changing Cloudflare routes or Access policy;
- debugging bearer-token automation or GitHub browser login.

The broker is exposed through Cloudflare Workers routes:

```text
https://crabbox.openclaw.ai
https://crabbox-coordinator.steipete.workers.dev
crabbox.clawd.bot/*
```

Normal users run `crabbox login`, which opens GitHub and stores a signed Crabbox user token. The coordinator needs a GitHub OAuth app with callback:

```text
https://crabbox.openclaw.ai/v1/auth/github/callback
```

Worker secrets:

```text
CRABBOX_GITHUB_CLIENT_ID
CRABBOX_GITHUB_CLIENT_SECRET
CRABBOX_GITHUB_ALLOWED_ORG
CRABBOX_GITHUB_ALLOWED_ORGS
CRABBOX_GITHUB_ALLOWED_TEAMS
CRABBOX_SESSION_SECRET
```

GitHub browser login requires active membership in the allowed GitHub org before
the coordinator mints a Crabbox user token. Set `CRABBOX_GITHUB_ALLOWED_ORG` or
comma-separated `CRABBOX_GITHUB_ALLOWED_ORGS`; if unset, the Worker falls back
to `CRABBOX_DEFAULT_ORG`, then `openclaw`. The OAuth app must request
`read:user user:email read:org`.

Set comma-separated `CRABBOX_GITHUB_ALLOWED_TEAMS` to require membership in at
least one team after org membership passes. Entries are GitHub team slugs. Use
`team-slug` for the selected org or `org/team-slug` when multiple orgs are
allowed.

Trusted automation can still use the shared operator bearer token configured in the CLI and Worker. The CLI sends:

```text
Authorization: Bearer <token>
X-Crabbox-Owner: <email>
X-Crabbox-Org: <org>
```

Owner selection for bearer-token requests:

```text
CRABBOX_OWNER
GIT_AUTHOR_EMAIL
GIT_COMMITTER_EMAIL
git config user.email
```

`CRABBOX_ORG` sets the org header. When Cloudflare Access identity is present, Access email wins over the CLI-provided owner.

GitHub user tokens are signed by the Worker and are not admin tokens. Admin routes require the shared operator token. The `crabbox.openclaw.ai/*` route is the canonical CLI and browser-login endpoint. The worker.dev and `crabbox.clawd.bot/*` routes are fallbacks.

Related docs:

- [Coordinator](coordinator.md)
- [Security](../security.md)
- [Infrastructure](../infrastructure.md)
- [config command](../commands/config.md)
