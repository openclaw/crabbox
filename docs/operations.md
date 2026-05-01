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
node scripts/build-docs-site.mjs
bin/crabbox doctor
bin/crabbox whoami
bin/crabbox status --json
bin/crabbox usage --scope all --json
bin/crabbox history --limit 5
```

`crabbox doctor` checks local prerequisites and coordinator reachability. `crabbox whoami` verifies identity. `crabbox status` confirms the broker can answer lease state. `crabbox usage` proves the cost accounting path is reachable. `crabbox history` proves run history is reachable.

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

Required Worker secrets:

```text
CRABBOX_SHARED_TOKEN
HETZNER_TOKEN
AWS_ACCESS_KEY_ID
AWS_SECRET_ACCESS_KEY
```

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

The `crabbox.openclaw.ai/*` route is attached to the coordinator Worker. Bearer-token CLI automation talks to the Worker with `CRABBOX_SHARED_TOKEN`/`CRABBOX_COORDINATOR_TOKEN`; GitHub browser login stores a user-scoped signed token. Access-protected fallback routes can also use `CRABBOX_ACCESS_CLIENT_ID` plus `CRABBOX_ACCESS_CLIENT_SECRET`, or `CRABBOX_ACCESS_TOKEN` for an already minted Access JWT.

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

Before handing off:

- `go test ./...`
- Worker format, lint, typecheck, tests, and build.
- `node scripts/build-docs-site.mjs`
- docs link check, when a link checker is available.
- `git diff --check`
- live `crabbox doctor` if broker credentials are available.
