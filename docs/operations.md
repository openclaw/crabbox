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
CRABBOX_LIVE=1 CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
```

To narrow the live matrix while debugging, set `CRABBOX_LIVE_PROVIDERS`:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=aws CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=hetzner CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=blacksmith-testbox CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=e2b CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=modal CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=daytona CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=namespace-devbox CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=semaphore CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=sprites CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=wandb CRABBOX_LIVE_COORDINATOR=0 CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
```

Blacksmith smoke requires a workflow containing a `useblacksmith/testbox`,
`useblacksmith/begin-testbox`, or `useblacksmith/run-testbox` step; set
`CRABBOX_BLACKSMITH_WORKFLOW` when the default path is not correct. E2B smoke
requires `E2B_API_KEY`. Modal smoke requires an authenticated Modal Python
client (`python3 -m modal setup` or Modal token env vars). Semaphore smoke
requires `CRABBOX_SEMAPHORE_HOST`, `CRABBOX_SEMAPHORE_PROJECT`, and
`CRABBOX_SEMAPHORE_TOKEN` or equivalent user config. Daytona needs
`CRABBOX_DAYTONA_SNAPSHOT`, `DAYTONA_SNAPSHOT`, or `daytona.snapshot`.
Namespace needs the authenticated `devbox` CLI on `PATH`. Sprites needs the
authenticated `sprite` CLI on `PATH` plus a Sprites token in the environment.
W&B smoke requires `WANDB_ENTITY_NAME` plus `CRABBOX_WANDB_API_KEY` or
`WANDB_API_KEY` (from `wandb login`).
Use `scripts/wandb-smoke.sh` for a coordinator-free wandb-only gate.

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
health checks for the comma-separated real deployment URLs in
`CRABBOX_DEPLOY_SMOKE_URLS`. To include a short AWS lease smoke after deploy:

```sh
CRABBOX_DEPLOY_SMOKE_URLS="https://$BROKER_HOST/v1/health" CRABBOX_DEPLOY_SMOKE_AWS=1 CRABBOX_LIVE_REPO=/path/to/my-app scripts/deploy-worker-smoke.sh
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
CRABBOX_HOST_ID optional; pins a brokered host such as an EC2 Mac Dedicated Host
CRABBOX_AWS_MAC_HOST_ID optional legacy AWS alias for CRABBOX_HOST_ID
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
CRABBOX_AWS_ORPHAN_SWEEP_ENABLED optional; defaults on when AWS broker credentials exist
CRABBOX_AWS_ORPHAN_SWEEP_DELETE optional; set 1 to terminate confirmed orphan EC2 instances
CRABBOX_AWS_ORPHAN_SWEEP_INTERVAL_SECONDS optional; default 3600
CRABBOX_AWS_ORPHAN_SWEEP_GRACE_SECONDS optional; default 900
CRABBOX_AWS_MAC_HOST_SWEEP_RELEASE optional; set 1 to release stale pending EC2 Mac hosts during orphan sweep
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
CRABBOX_ARTIFACTS_BASE_URL=https://artifacts.example.com
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
https://broker.example.com
```

The Access-protected Worker URL is:

```text
https://broker-access.example.com
```

The `broker.example.com/*` route is attached to the coordinator Worker for normal CLI and browser-login use. `broker-access.example.com/*` is attached to the same Worker behind Cloudflare Access for service-token proof and hardened automation. Bearer-token CLI automation talks to the Worker with `CRABBOX_SHARED_TOKEN`/`CRABBOX_COORDINATOR_TOKEN`; GitHub browser login stores a user-scoped signed token. Access-protected routes also require `CRABBOX_ACCESS_CLIENT_ID` plus `CRABBOX_ACCESS_CLIENT_SECRET`, or `CRABBOX_ACCESS_TOKEN` for an already minted Access JWT.

Use the protected route when testing the Cloudflare Access layer:

```sh
CRABBOX_COORDINATOR=https://broker-access.example.com bin/crabbox doctor
CRABBOX_COORDINATOR=https://broker-access.example.com bin/crabbox whoami
CRABBOX_LIVE=1 CRABBOX_AUTH_SMOKE_ACCESS=1 CRABBOX_COORDINATOR=https://broker-access.example.com CRABBOX_BIN=bin/crabbox scripts/live-auth-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=aws CRABBOX_COORDINATOR=https://broker-access.example.com CRABBOX_BIN=bin/crabbox scripts/live-smoke.sh
```

`doctor` should report `access=service-token`. `scripts/live-auth-smoke.sh`
proves the auth boundary without leasing a machine: no Access headers are denied
at the edge, shared-token user auth works, raw Access identity spoofing is
ignored, shared-token admin calls fail, and admin-token admin calls pass. A raw
request without Access headers to `https://broker-access.example.com/v1/health`
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

After AWS credential or account rotation, scan old provider accounts directly for
Crabbox-tagged EC2 instances that the current coordinator cannot see:

```sh
scripts/aws-crabbox-orphan-audit.sh --profile old-crabbox-account
scripts/aws-crabbox-orphan-audit.sh --profile old-crabbox-account --terminate
```

The audit is read-only by default. It skips `keep=true` instances, protects
active coordinator leases by lease tag or EC2 instance ID, and applies the same
grace window as the broker sweep before reporting stale labels. `--terminate`
refuses to run if active coordinator leases cannot be loaded or may be truncated.

Direct-provider cleanup is only for debug mode without a coordinator:

```sh
bin/crabbox cleanup --dry-run
bin/crabbox cleanup
```

The coordinator also runs an AWS orphan sweep from the Durable Object alarm when
AWS broker credentials are configured. The Worker cron route bootstraps the same
maintenance loop for idle fleets, so cleanup does not depend on new lease
traffic after deploy. It scans `CRABBOX_AWS_REGION` plus
`CRABBOX_CAPACITY_REGIONS` for `crabbox=true` EC2 instances and compares their
lease tags with active coordinator leases. Active matching leases always win,
because provider `expires_at` tags are written at launch and can be older than a
heartbeat-extended lease.

The sweep reports candidates when an instance is past its provider `expires_at`
tag, has no active lease, is missing a lease label, or points at an active lease
whose current cloud ID is different. It skips `keep=true` instances and applies
the grace window before acting on missing or mismatched lease state. Set
`CRABBOX_AWS_ORPHAN_SWEEP_DELETE=1` to terminate confirmed candidates
automatically. When `CRABBOX_AWS_MAC_HOST_SWEEP_RELEASE=1` is also set, the same
sweep releases Crabbox-tagged EC2 Mac Dedicated Hosts that have stayed in
`pending` for at least one hour and are not attached to an active lease.

Trusted admins can inspect or trigger the sweep:

```sh
curl -H "Authorization: Bearer $CRABBOX_COORDINATOR_ADMIN_TOKEN" \
  https://crabbox.example.com/v1/admin/aws-orphan-sweep

curl -X POST -H "Authorization: Bearer $CRABBOX_COORDINATOR_ADMIN_TOKEN" \
  https://crabbox.example.com/v1/admin/aws-orphan-sweep
```

## AWS Security Guardrails

Use the cheap account guardrails before adding heavier audit services:

```sh
account_id="$(aws sts get-caller-identity --query Account --output text)"

aws s3control put-public-access-block \
  --account-id "$account_id" \
  --public-access-block-configuration \
  BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true

aws iam update-account-password-policy \
  --minimum-password-length 14 \
  --require-symbols \
  --require-numbers \
  --require-uppercase-characters \
  --require-lowercase-characters \
  --allow-users-to-change-password \
  --max-password-age 90 \
  --password-reuse-prevention 24
```

S3 account-level Block Public Access and the IAM account password policy are
account-wide controls. IAM Access Analyzer external-access analyzers are
regional, so create one in each AWS capacity region:

```sh
for region in eu-west-1 eu-west-2 eu-central-1 us-east-1 us-west-2; do
  if ! aws accessanalyzer get-analyzer \
    --region "$region" \
    --analyzer-name crabbox-external-access >/dev/null 2>&1; then
    aws accessanalyzer create-analyzer \
      --region "$region" \
      --analyzer-name crabbox-external-access \
      --type ACCOUNT
  fi
done
```

List active external-access findings across the same pool:

```sh
for region in eu-west-1 eu-west-2 eu-central-1 us-east-1 us-west-2; do
  arn="$(aws accessanalyzer get-analyzer \
    --region "$region" \
    --analyzer-name crabbox-external-access \
    --query 'analyzer.arn' \
    --output text)"
  aws accessanalyzer list-findings \
    --region "$region" \
    --analyzer-arn "$arn" \
    --filter '{"status":{"eq":["ACTIVE"]}}'
done
```

Do not treat these as spend caps or compliance audit trails. CloudTrail, AWS
Config, Security Hub, and GuardDuty are separate choices with different cost and
retention tradeoffs.

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
- `scripts/test-go-modules.sh`
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
