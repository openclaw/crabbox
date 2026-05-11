# Operations

Read when:

- deploying or validating the coordinator;
- changing Worker secrets, routes, or provider credentials;
- checking cost limits or lease cleanup;
- deciding whether a failure is local CLI, broker, provider, or runner state.

Crabbox operations have three layers:

```text
local CLI -> Cloudflare Worker/Fleet Durable Object -> provider VM
```

The CLI owns local config, SSH keys, sync, and remote command execution. The coordinator owns auth, lease state, provider credentials, cost guardrails, and cleanup. Providers own VM creation, network reachability, and deletion.

## Daily Health Check

Run these before a release or after changing secrets:

```sh
go test ./...
npm run check --prefix worker
npm test --prefix worker
npm run docs:check
bin/crabbox doctor
bin/crabbox whoami
bin/crabbox list --json
bin/crabbox usage --scope all --json
bin/crabbox history --limit 5
```

`crabbox doctor` checks local prerequisites and coordinator reachability. `crabbox whoami` verifies identity. `crabbox list` confirms the broker can answer lease state. `crabbox usage` proves the cost accounting path is reachable. `crabbox history` proves run history is reachable.

When broker/provider credentials are available and infra changed, run the live smoke:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_REPO=/path/to/openclaw scripts/live-smoke.sh
```

To narrow the live matrix while debugging, set `CRABBOX_LIVE_PROVIDERS`:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=aws CRABBOX_LIVE_REPO=/path/to/openclaw scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=hetzner CRABBOX_LIVE_REPO=/path/to/openclaw scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=blacksmith-testbox CRABBOX_LIVE_REPO=/path/to/openclaw scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=e2b CRABBOX_LIVE_REPO=/path/to/openclaw scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=daytona CRABBOX_LIVE_REPO=/path/to/openclaw scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=namespace-devbox CRABBOX_LIVE_REPO=/path/to/openclaw scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=semaphore CRABBOX_LIVE_REPO=/path/to/openclaw scripts/live-smoke.sh
```

E2B smoke requires `E2B_API_KEY`. Semaphore smoke requires
`CRABBOX_SEMAPHORE_HOST`, `CRABBOX_SEMAPHORE_PROJECT`, and
`CRABBOX_SEMAPHORE_TOKEN` or equivalent user config.
Daytona needs `CRABBOX_DAYTONA_SNAPSHOT` or `daytona.snapshot`.
Namespace needs the authenticated `devbox` CLI on `PATH`.

For direct-provider smoke, disable the coordinator with a scratch config and run the same commands manually:

```sh
tmp="$(mktemp)"
printf 'provider: hetzner\n' > "$tmp"
CRABBOX_CONFIG="$tmp" CRABBOX_COORDINATOR= bin/crabbox warmup --provider hetzner --class standard --ttl 15m --idle-timeout 4m
CRABBOX_CONFIG="$tmp" CRABBOX_COORDINATOR= bin/crabbox run --provider hetzner --id <slug> --no-sync -- echo direct-hetzner-ok
CRABBOX_CONFIG="$tmp" CRABBOX_COORDINATOR= bin/crabbox stop --provider hetzner <slug>
rm -f "$tmp"
```

Use `--provider aws` with AWS SDK credentials for the direct AWS equivalent.

## Deployment

Worker source lives in `worker/`.

```sh
npm ci --prefix worker
npm run format:check --prefix worker
npm run lint --prefix worker
npm run check --prefix worker
npm test --prefix worker
npm run build --prefix worker
npx wrangler deploy --config worker/wrangler.jsonc
```

The repeatable deploy proof is:

```sh
scripts/deploy-worker-smoke.sh
```

It runs Worker format, lint, typecheck, tests, dry-run build, deploy, and public
health checks for `crabbox.openclaw.ai` plus the workers.dev fallback. To include
a short AWS lease smoke after deploy:

```sh
CRABBOX_DEPLOY_SMOKE_AWS=1 CRABBOX_LIVE_REPO=/path/to/openclaw scripts/deploy-worker-smoke.sh
```

Required Worker secrets:

```text
CRABBOX_SHARED_TOKEN
HETZNER_TOKEN
AWS_ACCESS_KEY_ID
AWS_SECRET_ACCESS_KEY
```

Conditional Worker secrets and settings:

```text
AWS_SESSION_TOKEN optional
CRABBOX_AWS_MAC_HOST_ID required only for brokered target=macos
CRABBOX_SHARED_OWNER optional fixed owner identity for shared-token automation
CRABBOX_ADMIN_TOKEN required for admin routes and image promotion
CRABBOX_GITHUB_CLIENT_ID required for browser login
CRABBOX_GITHUB_CLIENT_SECRET required for browser login
CRABBOX_SESSION_SECRET required for browser login
CRABBOX_GITHUB_ALLOWED_ORG or CRABBOX_GITHUB_ALLOWED_ORGS
CRABBOX_GITHUB_ALLOWED_TEAMS optional
CRABBOX_ACCESS_TEAM_DOMAIN required for Access JWT verification
CRABBOX_ACCESS_AUD required for Access JWT verification
CRABBOX_TAILSCALE_CLIENT_ID required for brokered --tailscale
CRABBOX_TAILSCALE_CLIENT_SECRET required for brokered --tailscale
CRABBOX_TAILSCALE_TAILNET optional
CRABBOX_TAILSCALE_TAGS optional
CRABBOX_TAILSCALE_ENABLED optional; set 0 to disable brokered Tailscale
CRABBOX_ARTIFACTS_BACKEND optional; enables brokered artifact publishing
CRABBOX_ARTIFACTS_BUCKET required when artifact backend is enabled
CRABBOX_ARTIFACTS_PREFIX optional
CRABBOX_ARTIFACTS_BASE_URL optional; public final artifact URL prefix
CRABBOX_ARTIFACTS_REGION optional
CRABBOX_ARTIFACTS_ENDPOINT_URL optional; required for R2/custom S3 endpoints
CRABBOX_ARTIFACTS_ACCESS_KEY_ID required when artifact backend is enabled
CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY required when artifact backend is enabled
CRABBOX_ARTIFACTS_SESSION_TOKEN optional
CRABBOX_ARTIFACTS_UPLOAD_EXPIRES_SECONDS optional
CRABBOX_ARTIFACTS_URL_EXPIRES_SECONDS optional
```

Artifact backend vars are ordinary Worker vars except
`CRABBOX_ARTIFACTS_ACCESS_KEY_ID`, `CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY`, and
optional `CRABBOX_ARTIFACTS_SESSION_TOKEN`, which must be Worker secrets. These
object-store keys let the coordinator sign short-lived artifact upload/read
URLs; they should be scoped to the artifact bucket or prefix and should not have
Cloudflare account, Worker deployment, lease-provider, or VM permissions.

Our current coordinator artifact config is R2-compatible:

```text
CRABBOX_ARTIFACTS_BACKEND=r2
CRABBOX_ARTIFACTS_BUCKET=openclaw-crabbox-artifacts
CRABBOX_ARTIFACTS_PREFIX=crabbox-artifacts
CRABBOX_ARTIFACTS_BASE_URL=https://artifacts.openclaw.ai
CRABBOX_ARTIFACTS_REGION=auto
CRABBOX_ARTIFACTS_ENDPOINT_URL=<account>.r2.cloudflarestorage.com
```

The corresponding R2 access key id and secret access key are deployed as Worker
secrets, not local CLI defaults. Normal users should run
`crabbox artifacts publish` without direct S3/R2 credentials.

Cost-control secrets and settings:

```text
CRABBOX_COST_RATES_JSON
CRABBOX_EUR_TO_USD
CRABBOX_MAX_ACTIVE_LEASES
CRABBOX_MAX_ACTIVE_LEASES_PER_OWNER
CRABBOX_MAX_ACTIVE_LEASES_PER_ORG
CRABBOX_MAX_MONTHLY_USD
CRABBOX_MAX_MONTHLY_USD_PER_OWNER
CRABBOX_MAX_MONTHLY_USD_PER_ORG
CRABBOX_DEFAULT_ORG
```

## Routes And Access

The canonical Worker URL is:

```text
https://crabbox.openclaw.ai
```

The Access-protected Worker URL is:

```text
https://crabbox-access.openclaw.ai
```

The `crabbox.openclaw.ai/*` route is attached to the coordinator Worker for normal CLI and browser-login use. `crabbox-access.openclaw.ai/*` is attached to the same Worker behind Cloudflare Access for service-token proof and hardened automation. Bearer-token CLI automation talks to the Worker with `CRABBOX_SHARED_TOKEN`/`CRABBOX_COORDINATOR_TOKEN`; GitHub browser login stores a user-scoped signed token. Access-protected routes also require `CRABBOX_ACCESS_CLIENT_ID` plus `CRABBOX_ACCESS_CLIENT_SECRET`, or `CRABBOX_ACCESS_TOKEN` for an already minted Access JWT.

Use the protected route when testing the Cloudflare Access layer:

```sh
CRABBOX_COORDINATOR=https://crabbox-access.openclaw.ai bin/crabbox doctor
CRABBOX_COORDINATOR=https://crabbox-access.openclaw.ai bin/crabbox whoami
CRABBOX_LIVE=1 CRABBOX_COORDINATOR=https://crabbox-access.openclaw.ai CRABBOX_BIN=bin/crabbox scripts/live-auth-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=aws CRABBOX_COORDINATOR=https://crabbox-access.openclaw.ai CRABBOX_BIN=bin/crabbox scripts/live-smoke.sh
```

`doctor` should report `access=service-token`. `scripts/live-auth-smoke.sh`
proves the auth boundary without leasing a machine: no Access headers are denied
at the edge, shared-token user auth works, raw Access identity spoofing is
ignored, shared-token admin calls fail, and admin-token admin calls pass. A raw
request without Access headers to `https://crabbox-access.openclaw.ai/v1/health`
should return a Cloudflare Access `403`.

Use `crabbox config show` to confirm which URL and provider the CLI will use:

```sh
bin/crabbox config show
```

## Cleanup

Brokered cleanup belongs to the Durable Object alarm. The CLI refuses provider cleanup when a coordinator is configured, because deleting machines behind the coordinator can remove live leases.

Use:

```sh
bin/crabbox list
bin/crabbox admin leases --state active
bin/crabbox inspect --id blue-lobster --json
bin/crabbox stop blue-lobster
```

Trusted operators can use `crabbox admin release` or `crabbox admin delete --force` for stuck leases.

Direct-provider cleanup is only for debug mode without a coordinator:

```sh
bin/crabbox cleanup --dry-run
bin/crabbox cleanup
```

## Cost Guardrails

The coordinator reserves worst-case lease cost before provisioning. If a request would exceed active-lease or monthly cost limits, it fails before creating a VM.

Use:

```sh
bin/crabbox usage
bin/crabbox usage --scope user --user someone@example.com
bin/crabbox usage --scope org --org openclaw
bin/crabbox usage --scope all --json
```

Cost is an estimate for compute leases, not an invoice. See [Cost And Usage](features/cost-usage.md).

## Release Checklist

Before tagging a release:

- Reorder `CHANGELOG.md` with the user-facing changes first, date the release
  section, and keep contributor thanks/co-author notes intact.
- Update package metadata that carries the project version, including
  `package.json`, `worker/package.json`, and `worker/package-lock.json`.
- `go vet ./...`
- `go test -race ./...`
- `go build -trimpath -o bin/crabbox ./cmd/crabbox`
- `scripts/check-go-coverage.sh 90.0`
- Worker format, lint, typecheck, tests, and build:
  `npm run format:check --prefix worker && npm run lint --prefix worker && npm run check --prefix worker && npm test --prefix worker && npm run build --prefix worker`
- `npm run docs:check`
- `git diff --check`
- Live smoke at least one coordinator-backed `crabbox run`, then verify
  `crabbox attach`, `crabbox events`, `crabbox logs`, and lease cleanup.
- Push, pull, and wait for CI green on the release commit.
- Tag and push `vX.Y.Z`, then wait for the release workflow. The workflow
  publishes GitHub release assets, copies the matching `CHANGELOG.md` section
  into the GitHub release body, and directly pushes the generated
  `Formula/crabbox.rb` update to `openclaw/homebrew-tap` with
  `HOMEBREW_TAP_GITHUB_TOKEN`; missing tap access is a release failure.
- Verify the GitHub release assets and Homebrew formula update.
- `brew update`, install or upgrade `openclaw/tap/crabbox`, run
  `crabbox --version`, and run a short live smoke from the installed binary.
