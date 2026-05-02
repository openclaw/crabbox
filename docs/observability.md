# Observability

Read when:

- debugging a failed or slow run;
- checking who used capacity this month;
- finding a remote machine for SSH inspection;
- correlating Actions hydration with the remote workspace.

Crabbox exposes operational visibility through CLI commands, coordinator usage summaries, retained run history/log tails, provider labels, GitHub Actions run links, and Worker logs. The reliable path is to keep the lease ID and run ID together.

## Lease State

Use `status`, `list`, and `inspect`:

```sh
bin/crabbox status --id blue-lobster
bin/crabbox list --json
bin/crabbox inspect --id blue-lobster --json
```

Important fields:

- lease ID and slug;
- owner and org;
- provider and server type;
- state;
- `createdAt`, `lastTouchedAt`, `idleTimeoutSeconds`, `ttlSeconds`, and `expiresAt`;
- public address;
- SSH user and port;
- keep/delete behavior.

Provider machines are labeled with Crabbox metadata so cloud consoles can be correlated back to the lease.

## Usage And Cost

Use `usage` for monthly summaries:

```sh
bin/crabbox usage
bin/crabbox usage --scope user --user steipete@gmail.com
bin/crabbox usage --scope org --org openclaw
bin/crabbox usage --scope all --json
```

Reports include lease count, active lease count, elapsed runtime, estimated elapsed cost, reserved worst-case cost, and breakdowns by owner, org, provider, and server type.

## Run History And Logs

Coordinator-backed `crabbox run` creates a run record before sync starts, appends lifecycle/output events while the CLI is alive, and finishes it with exit code, timing, and the latest retained output tail.

Use:

```sh
bin/crabbox history
bin/crabbox history --lease cbx_...
bin/crabbox history --owner steipete@gmail.com --json
bin/crabbox events run_...
bin/crabbox attach run_...
bin/crabbox logs run_...
bin/crabbox results run_...
```

History is for command debugging, not unlimited log archival. Events are structured phase/output records with sequence numbers; logs are bounded tails of remote stdout/stderr. Test results are stored as structured summaries when `--junit` or `results.junit` is configured.

## Remote Debugging

Use SSH for live process and filesystem inspection:

```sh
bin/crabbox ssh --id blue-lobster
bin/crabbox inspect --id blue-lobster --json
```

Useful remote checks:

```sh
crabbox-ready
test -f /var/lib/crabbox/bootstrapped
df -h
free -h
ps aux --sort=-%cpu | head
```

If a lease was created with `--keep`, SSH remains available until `crabbox stop`, idle expiry, or the TTL cap removes it.

## Actions Hydration

`crabbox actions hydrate` dispatches the configured workflow and waits for a ready marker. The workflow run URL and marker path are the key correlation points.

Use:

```sh
bin/crabbox actions hydrate --id blue-lobster
bin/crabbox inspect --id blue-lobster --json
```

The hydrated run writes non-secret handoff data for later `crabbox run --id blue-lobster` commands. Secrets and OIDC tokens remain workflow-step scoped unless the workflow intentionally writes its own short-lived handoff.

## Worker Logs

When the coordinator path fails before SSH, check Worker logs and Durable Object errors. The symptoms usually group into:

- auth failure;
- cost limit rejection;
- provider quota or capacity rejection;
- provider API failure;
- Durable Object alarm or state transition bug.

Keep the lease ID, owner, org, provider, class, and request time when comparing CLI output to Worker logs.

## Gaps

Current Crabbox observability is enough for maintainer operations, but not yet a full analytics product. Missing pieces:

- alerting on budget or failure-rate thresholds;
- dashboard UI.
