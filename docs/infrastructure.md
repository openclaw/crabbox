# Infrastructure

## Current Intended Setup

Canonical Worker endpoint:

```text
https://crabbox.openclaw.ai
```

Access-protected Worker endpoint:

```text
https://crabbox-access.openclaw.ai
```

Legacy fallback route:

```text
https://crabbox.clawd.bot
```

Workers.dev fallback endpoint:

```text
https://crabbox-coordinator.services-91b.workers.dev
```

The `crabbox.openclaw.ai/*` Worker route is the stable automation and browser-login endpoint. `crabbox-access.openclaw.ai/*` is the Cloudflare Access-protected route for service-token proof and hardened automation. `crabbox.clawd.bot/*` and the workers.dev URL remain fallback routes.

## Cloudflare

Use Cloudflare for:

- HTTPS coordinator.
- Access auth.
- Worker runtime.
- Durable Object lease state.
- DNS/custom domain routing.

Known setup:

- Access org: `crabbox-openclaw.cloudflareaccess.com`.
- Access enabled.
- Current IdPs: one-time PIN and GitHub.
- GitHub IdP name: `GitHub OpenClaw`.
- GitHub IdP restriction: org `openclaw`.
- Service-token Access app: `Crabbox Coordinator Service Token` on `crabbox-access.openclaw.ai`.
- Service-token Access policy: `CLI service token`, `non_identity`, include the local Crabbox CLI service token.

Required env:

```text
CRABBOX_CLOUDFLARE_API_TOKEN
CRABBOX_CLOUDFLARE_ACCOUNT_ID
CRABBOX_CLOUDFLARE_ZONE_ID
CRABBOX_CLOUDFLARE_ZONE_NAME
CRABBOX_DOMAIN
CRABBOX_FALLBACK_DOMAIN
CRABBOX_GITHUB_ALLOWED_ORG
CRABBOX_GITHUB_ALLOWED_ORGS
CRABBOX_GITHUB_ALLOWED_TEAMS
```

Crabbox browser login needs a GitHub OAuth app owned by the `openclaw` org:

```text
GitHub org: openclaw
App name: Crabbox Access
Homepage URL: https://crabbox.openclaw.ai
Callback URL: https://crabbox.openclaw.ai/v1/auth/github/callback
```

Store resulting values outside the repo:

```text
CRABBOX_GITHUB_OAUTH_CLIENT_ID
CRABBOX_GITHUB_OAUTH_CLIENT_SECRET
CRABBOX_GITHUB_CLIENT_ID
CRABBOX_GITHUB_CLIENT_SECRET
CRABBOX_GITHUB_ALLOWED_ORG
CRABBOX_GITHUB_ALLOWED_TEAMS
CRABBOX_SESSION_SECRET
```

Optional Tailscale brokered reachability uses a Tailscale OAuth client with the
`auth_keys` scope and only the tags Crabbox may assign, usually `tag:crabbox`.
Store OAuth credentials as Worker secrets:

```text
CRABBOX_TAILSCALE_CLIENT_ID
CRABBOX_TAILSCALE_CLIENT_SECRET
```

Optional Worker config:

```text
CRABBOX_TAILSCALE_ENABLED=1
CRABBOX_TAILSCALE_TAILNET=-              # or explicit tailnet/org
CRABBOX_TAILSCALE_TAGS=tag:crabbox       # allowlist/default tags
```

The Worker mints one-off ephemeral pre-approved auth keys per lease and injects
the key only into cloud-init. Lease records and provider labels store only
non-secret Tailscale metadata such as hostname, FQDN, 100.x address, state, and
tags.

Current local status:

- Core Cloudflare, Hetzner, and GitHub tokens are present in local `~/.profile`.
- The Crabbox Cloudflare token is mirrored to MacBook Pro `~/.profile`.
- `CRABBOX_COORDINATOR` and `CRABBOX_COORDINATOR_TOKEN` are present in local and MacBook Pro `~/.profile`.
- The GitHub OAuth client ID and secret may be stored locally as `CRABBOX_GITHUB_OAUTH_*` and deployed to the Worker as `CRABBOX_GITHUB_CLIENT_*`.
- Cloudflare Access service-token CLI credentials can be stored locally as `CRABBOX_ACCESS_CLIENT_ID` and `CRABBOX_ACCESS_CLIENT_SECRET`; `CRABBOX_ACCESS_TOKEN` can carry an already minted Access JWT for protected fallback routes.
- Crabbox browser-login OAuth secrets are deployed as Worker secrets `CRABBOX_GITHUB_CLIENT_ID`, `CRABBOX_GITHUB_CLIENT_SECRET`, and `CRABBOX_SESSION_SECRET`.
- Worker routes are attached for `crabbox.openclaw.ai/*` and `crabbox-access.openclaw.ai/*`.
- `CRABBOX_COORDINATOR`, `CRABBOX_PROFILE`, `CRABBOX_CONFIG`, `CRABBOX_FLEET_CONFIG`, `CRABBOX_SSH_KEY`, `CRABBOX_NO_COLOR`, and `CRABBOX_LOG` are optional CLI defaults and are not required to build the MVP.

The Cloudflare token `crabbox-deploy` is scoped to the OpenClaw Cloudflare account and the Crabbox/OpenClaw routes it manages. It verifies access to Workers scripts, Access applications, Access identity providers, Access keys, DNS records, and zone Worker routes from both the local machine and MacBook Pro.

## DNS State

Current path:

1. Keep the main `openclaw.ai` website on Vercel.
2. Manage `crabbox.openclaw.ai` in the OpenClaw Cloudflare account.
3. Proxy `crabbox.openclaw.ai/*` and `crabbox-access.openclaw.ai/*` to the `crabbox-coordinator` Worker.
4. Set `CRABBOX_PUBLIC_URL=https://crabbox.openclaw.ai`.
5. Configure the GitHub OAuth callback on `https://crabbox.openclaw.ai/v1/auth/github/callback`.

Fallback path:

1. Use the workers.dev URL for health checks if DNS is disrupted.
2. Use `crabbox.clawd.bot` only as a legacy fallback.

## Hetzner

Use Hetzner Cloud for worker machines.

Required env:

```text
HCLOUD_TOKEN
HETZNER_TOKEN
```

Direct Hetzner defaults:

```yaml
provider: hetzner-main
location: fsn1
serverType: ccx63
image: ubuntu-24.04
sshUser: crabbox
sshPort: "2222"
# Ordered fallback ports tried after sshPort; use [] to disable fallback.
sshFallbackPorts:
  - "22"
workdir: /work/crabbox
```

Machine labels:

```text
crabbox=true
profile=openclaw-check
class=ccx33
lease=cbx_...
slug=blue-lobster
owner=<github-login-or-email>
created_at=<unix-seconds>
last_touched_at=<unix-seconds>
ttl_secs=<seconds>
idle_timeout_secs=<seconds>
expires_at=<unix-seconds>
```

Current direct-CLI status:

- `crabbox warmup --profile openclaw-check --class beast --keep` provisions through the Hetzner API without requiring `hcloud`.
- The `beast` class tries `ccx63`, `ccx53`, `ccx43`, `cpx62`, then `cx53`.
- Dedicated-core types currently fail on the available account quota, so the verified runner used `cpx62`.
- Cloud-init installs only Crabbox plumbing: OpenSSH, curl/CA certificates, Git, rsync, jq, and a readiness probe through a retrying bootstrap script. Project runtimes and services are supplied by Actions hydration or repo-owned setup.
- SSH prefers the configured primary port, default `2222`, and then tries `ssh.fallbackPorts`, default `["22"]`. Set `ssh.fallbackPorts: []` or `CRABBOX_SSH_FALLBACK_PORTS=none` to disable fallback dialing/opening.
- The verified kept lease was `cbx_f782c469c9ce` on server `128694755`, `cpx62`, `188.245.91.84`.

## AWS EC2 Spot

Use AWS as the first non-Hetzner burst backend. The Cloudflare coordinator brokers AWS EC2 Spot by default; the CLI direct provider remains available with `--provider aws` when no broker is configured.

Brokered AWS credentials live as Worker secrets:

```text
AWS_ACCESS_KEY_ID
AWS_SECRET_ACCESS_KEY
AWS_SESSION_TOKEN optional
```

Direct fallback env is whatever the AWS SDK can resolve, such as:

```text
AWS_PROFILE
AWS_ACCESS_KEY_ID
AWS_SECRET_ACCESS_KEY
AWS_SESSION_TOKEN
```

AWS-specific Crabbox env:

```text
CRABBOX_AWS_REGION               default eu-west-1
CRABBOX_AWS_AMI                  optional Ubuntu 24.04 x86_64 AMI override
CRABBOX_AWS_SECURITY_GROUP_ID    optional security group override
CRABBOX_AWS_SUBNET_ID            optional subnet override
CRABBOX_AWS_INSTANCE_PROFILE     optional IAM instance profile name
CRABBOX_AWS_ROOT_GB              default 400
CRABBOX_AWS_SSH_CIDRS            optional comma-separated SSH source CIDRs
CRABBOX_SSH_FALLBACK_PORTS       optional comma-separated SSH fallback ports, or none
```

The AWS provider imports the local SSH public key as an EC2 key pair when needed, creates or reuses a `crabbox-runners` security group when no security group is supplied, launches one-time Spot instances, tags instances and volumes with Crabbox lease metadata, and terminates non-kept instances after the command.

Grant the Worker AWS principal EC2 launch/list/tag/terminate permissions plus
`servicequotas:GetServiceQuota`. Service Quotas access is best-effort: when it
is available, Crabbox can skip known quota-impossible instance types before
calling `RunInstances`; when it is missing, EC2 launch errors are still
classified after the failed call.

SSH ingress for AWS security groups is source-scoped. If `CRABBOX_AWS_SSH_CIDRS` is set, Crabbox adds those CIDRs. Otherwise, the CLI sends its detected outbound IPv4 `/32` to the broker; when that is unavailable, the Worker falls back to `CF-Connecting-IP` as `/32` or `/128`. Direct and brokered AWS open the primary SSH port plus configured fallback ports. Crabbox also revokes the old managed `0.0.0.0/0` SSH ingress rule when the broker touches the managed security group. Supplying `CRABBOX_AWS_SECURITY_GROUP_ID` makes network policy your responsibility.

## Machine Classes

Fleet config should define machine classes instead of hardcoding provider types. Current Hetzner direct defaults:

```yaml
classes:
  standard:
    provider: hetzner-main
    serverTypes: [ccx33, cpx62, cx53]
    cpu: 8
    memory: 32gb
  fast:
    provider: hetzner-main
    serverTypes: [ccx43, cpx62, cx53]
    cpu: 16
    memory: 64gb
  large:
    provider: hetzner-main
    serverTypes: [ccx53, ccx43, cpx62, cx53]
    cpu: 32
    memory: 128gb
  beast:
    provider: hetzner-main
    serverTypes: [ccx63, ccx53, ccx43, cpx62, cx53]
    cpu: 48
    memory: 192gb
```

Current AWS defaults:

```yaml
classes:
  standard:
    provider: aws
    serverTypes: [c7a.8xlarge, c7a.4xlarge]
  fast:
    provider: aws
    serverTypes: [c7a.16xlarge, c7a.12xlarge, c7a.8xlarge]
  large:
    provider: aws
    serverTypes: [c7a.24xlarge, c7a.16xlarge, c7a.12xlarge]
  beast:
    provider: aws
    serverTypes: [c7a.48xlarge, c7a.32xlarge, c7a.24xlarge, c7a.16xlarge]
```

Profiles choose a default class, and commands can override with `--class`.

## Deployment

Worker source lives in `worker/`. Build and deploy with the package scripts plus Wrangler:

```sh
npm ci --prefix worker
npm run format:check --prefix worker
npm run lint --prefix worker
npm run check --prefix worker
npm test --prefix worker
npm run build --prefix worker
npx wrangler deploy --config worker/wrangler.jsonc
```

Deployment should:

1. Build Worker.
2. Create/update Durable Object bindings.
3. Set Worker secrets.
4. Deploy Worker.
5. Verify `/v1/health` on `workers.dev`.
6. Configure route/custom domain on `crabbox.openclaw.ai`.
7. Verify `/v1/health` on the canonical and fallback domains.

Use `npx wrangler` from the Worker package unless `wrangler` is installed globally. Do not assume `hcloud` is installed; the implementation can use the Hetzner API directly from Go or from the Worker.

Current deployed coordinator:

```text
https://crabbox.openclaw.ai
https://crabbox-access.openclaw.ai
https://crabbox-coordinator.services-91b.workers.dev
crabbox.clawd.bot/* -> crabbox-coordinator fallback
```

Current Worker secrets and settings:

```text
HETZNER_TOKEN
AWS_ACCESS_KEY_ID
AWS_SECRET_ACCESS_KEY
AWS_SESSION_TOKEN optional
CRABBOX_SHARED_TOKEN
CRABBOX_GITHUB_CLIENT_ID
CRABBOX_GITHUB_CLIENT_SECRET
CRABBOX_GITHUB_ALLOWED_ORG
CRABBOX_GITHUB_ALLOWED_ORGS optional
CRABBOX_GITHUB_ALLOWED_TEAMS optional
CRABBOX_DEFAULT_ORG
CRABBOX_SESSION_SECRET
```

## Verified OpenClaw Run

Historical warm-run command from an OpenClaw checkout through the Cloudflare coordinator:

```sh
CI=1 /usr/bin/time -p /Users/steipete/Projects/crabbox/bin/crabbox run --id cbx_f60f47cbc879 -- pnpm test:changed:max
```

Result:

- 61 Vitest shards completed successfully.
- End-to-end warm wall time: 93.66 seconds.
- Runner class: requested `beast`, actual fallback `cpx62`.
- Sync path: rsync overlay plus remote Git hydrate for shallow checkout merge-base support.

Current live smoke command:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_REPO=/Users/steipete/Projects/clawdbot6 /Users/steipete/Projects/crabbox/scripts/live-smoke.sh
```

The smoke covers brokered AWS, direct Hetzner, Blacksmith Testbox delegation, slug reuse, status/inspect/cache/history/logs, stop, and final active-lease cleanup checks.

## Local, MacBook Pro, And Mac Studio

The same required env should exist on the local machine, MacBook Pro, and Mac Studio. Do not commit these values.
