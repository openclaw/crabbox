# Troubleshooting

Read when:

- a lease fails to create;
- SSH never becomes ready;
- tailnet reachability behaves unexpectedly;
- sync behaves unexpectedly;
- Actions hydration times out;
- docs deployment fails.

Start with:

```sh
bin/crabbox doctor
bin/crabbox config show
bin/crabbox list --json
bin/crabbox usage --scope all --json
```

## Broker Auth Fails

Symptoms:

- `401`;
- `403`;
- `missing broker token`;
- GitHub `Invalid redirect_uri`;
- Cloudflare Access page instead of JSON.

Checks:

```sh
bin/crabbox config show
printenv CRABBOX_COORDINATOR
printenv CRABBOX_COORDINATOR_TOKEN
printenv CRABBOX_PUBLIC_URL
```

Fixes:

- configure the broker with `crabbox config set-broker`;
- ensure the CLI points at the Worker URL or the Access-protected route intentionally;
- ensure `CRABBOX_COORDINATOR_TOKEN` matches the Worker `CRABBOX_SHARED_TOKEN`.
- for self-hosted GitHub browser login, create your own GitHub OAuth app and set its callback URL to `https://<your-coordinator-host>/v1/auth/github/callback`;
- ensure the Worker's `CRABBOX_PUBLIC_URL` uses the same public origin as that GitHub OAuth callback.

## SSH Host Key Or Control Socket Fails

Symptoms:

- SSH warns that host identification changed after a provider reused an IP;
- a reused warm lease connects to the wrong ControlMaster socket;
- paths under `~/Library/Application Support` appear split at the space.

Checks:

```sh
bin/crabbox inspect --id blue-lobster --json
bin/crabbox ssh --id blue-lobster
```

Fixes:

- upgrade to a build that quotes SSH config values with spaces;
- keep per-lease keys under the Crabbox config `testboxes/<lease>` directory;
- avoid manually overriding `UserKnownHostsFile` or `ControlPath` unless debugging SSH itself.

## Lease Rejected By Cost Control

Symptoms:

- `cost_limit_exceeded`;
- lease request fails before provider creation.

Checks:

```sh
bin/crabbox usage --scope user --user "$(git config user.email)"
bin/crabbox usage --scope org --org openclaw
```

Fixes:

- raise the relevant monthly or active-lease limit;
- shorten `--idle-timeout`;
- choose a smaller `--class`;
- stop kept leases.

## Provider Capacity Or Quota Fails

Symptoms:

- `provider_not_configured`;
- `crabbox doctor --provider azure` reports `missing=AZURE_TENANT_ID,...`;
- class falls back from dedicated machines to smaller machines;
- AWS Spot request cannot be fulfilled;
- AWS reports `VcpuLimitExceeded` for large On-Demand instances;
- server create fails before SSH.

Checks:

```sh
bin/crabbox doctor --provider aws
bin/crabbox list --json
bin/crabbox usage --scope all
CRABBOX_CAPACITY_REGIONS=eu-west-1,eu-west-2,eu-central-1,us-east-1,us-west-2 \
  bin/crabbox warmup --provider aws --class standard --market on-demand --timing-json
```

Fixes:

- set the named Worker provider secrets before retrying brokered leases;
- choose a smaller class;
- use `--market on-demand` or `--market spot` for a one-off AWS capacity-market override;
- set `CRABBOX_CAPACITY_REGIONS` so brokered and direct AWS launches can try multiple regions;
- set `CRABBOX_CAPACITY_AVAILABILITY_ZONES` only when you intentionally want a specific zone in those regions;
- set `CRABBOX_CAPACITY_STRATEGY=most-available`;
- keep capacity hints enabled, or set `CRABBOX_CAPACITY_LARGE_CLASSES` when your installation wants warnings for classes beyond `beast`;
- if `doctor` prints `warning capacity`, use its recommended class/type or request the printed AWS quota code;
- raise the AWS `Running On-Demand Standard (A, C, D, H, I, M, R, T, Z) instances` quota for C/M/R/T/Z families, or the matching Spot quota when using Spot;
- raise Hetzner dedicated-core quota when dedicated classes are required;
- temporarily use AWS fallback capacity.

Brokered AWS launch fallback records provisioning attempts. Quota preflight
uses AWS Service Quotas when available and reports the quota code, applied vCPU
limit, requested type, and required vCPUs before trying the next candidate.
Brokered responses also include `capacityHints` so callers can surface the
selected region/market and next operator action instead of parsing provider
errors. `crabbox doctor --provider aws` can surface the same quota pressure
before the first warmup.

If AWS reports `InvalidInstanceID.NotFound` during coordinator-backed lease
creation, the backing instance record was stale by the time Crabbox tried to use
it. Crabbox discards that lease record best-effort and retries once with a
fresh lease.

## Provider Machine Looks Orphaned

Symptoms:

- `crabbox list` shows `orphan=no-active-lease`;
- provider console has a `crabbox-cbx_...` machine but `crabbox inspect` returns not found.

Checks:

```sh
bin/crabbox list --provider hetzner
bin/crabbox list --provider aws
bin/crabbox admin leases --state active
```

Fixes:

- do not delete `keep=true` machines automatically;
- stop or delete only after checking that no active coordinator lease references the machine;
- use `crabbox stop <lease-id-or-slug>` for active leases, and provider/admin cleanup only for confirmed orphaned machines.

## SSH Never Becomes Ready

Symptoms:

- lease exists but `crabbox run` waits until SSH timeout;
- the primary SSH port, default `2222`, and all fallback ports are unreachable;
- `crabbox-ready` is missing.

Checks:

```sh
bin/crabbox inspect --id cbx_... --json
ssh -p 2222 crabbox@HOST crabbox-ready
ssh -p 2222 crabbox@HOST test -f /var/lib/crabbox/bootstrapped
ssh -p 22 crabbox@HOST crabbox-ready
```

Fixes:

- wait for cloud-init to finish on fresh machines;
- verify security group or firewall allows the primary SSH port and the configured fallback ports;
- set `CRABBOX_SSH_FALLBACK_PORTS=none` when fallback port 22 should not be opened or tried;
- inspect provider console output for cloud-init failures;
- retry the lease if bootstrap failed before creating the ready marker.

## Tailscale Path Fails

Symptoms:

- `--tailscale` lease creation fails with `tailscale_unavailable`,
  `tailscale_disabled`, or `invalid_tailscale_tags`;
- `--network tailscale` says the lease has no tailnet address;
- `--network tailscale` says the tailnet host is unreachable over SSH;
- `--network auto` falls back to `public`;
- `tailscale exit node ... joined but remote internet egress failed`.

Checks:

```sh
bin/crabbox config show
bin/crabbox inspect --id blue-lobster
bin/crabbox inspect --id blue-lobster --json
bin/crabbox ssh --id blue-lobster --network tailscale
tailscale status
tailscale ping <tailscale-fqdn-or-100.x-address>
```

Fixes:

- for brokered leases, configure Worker secrets
  `CRABBOX_TAILSCALE_CLIENT_ID` and `CRABBOX_TAILSCALE_CLIENT_SECRET`;
- keep `CRABBOX_TAILSCALE_ENABLED` unset or `1`; set it to `0` only to disable
  brokered Tailscale intentionally;
- ensure requested tags are in the Worker `CRABBOX_TAILSCALE_TAGS` allowlist;
- ensure the local client is joined to the same tailnet and ACLs allow SSH to
  the tagged node;
- for exit nodes, ensure the exit node is approved and that tailnet grants or
  ACLs allow the lease tag, for example `tag:crabbox`, to reach
  `autogroup:internet`;
- if the exit node is a personal Mac, verify Tailscale still advertises it as
  an exit node and that the Mac can actually forward internet traffic for
  clients;
- use `--network public` to prove the provider SSH path independently;
- use `--network auto` when fallback to public is acceptable;
- use `--network tailscale` when a missing or unreachable tailnet path should
  fail the command.

Crabbox still uses OpenSSH and per-lease SSH keys over the selected host.
Tailscale SSH, Serve, Funnel, and direct VNC binding are not part of managed
lease support.

## Sync Looks Wrong

Symptoms:

- changed-test detection sees the wrong base;
- deleted files unexpectedly appear remotely;
- sync aborts on mass tracked deletions.
- sync warns or fails before rsync because the candidate tree is too large.

Checks:

```sh
git status --short
git ls-files --cached --others --exclude-standard | wc -l
bin/crabbox run --id cbx_... -- git status --short
bin/crabbox run --id cbx_... --sync-only --debug
```

Fixes:

- commit, stash, or intentionally keep local deletions before syncing;
- add generated directories to `.gitignore` or `.crabbox.yaml` `sync.exclude`;
- keep `.git`, build caches, and package caches out of the repo source tree;
- use `--force-sync-large` only after verifying the candidate file count and bytes are expected;
- check repo-local `.crabbox.yaml` sync excludes;
- rerun without relying on the sync fingerprint after large tree changes;
- verify base-ref hydration in repo config.

## Sync Stalls Or Times Out

Symptoms:

- rsync prints little output for a long time;
- `rsync timed out after ...`;
- a local cache directory made the first sync unexpectedly huge.

Checks:

```sh
bin/crabbox config show
bin/crabbox run --id cbx_... --sync-only --debug
```

Fixes:

- inspect the printed sync candidate estimate before retrying;
- lower `sync.timeout` for quick failure in agent loops, or raise it for intentionally large source transfers;
- tune `sync.warnFiles`, `sync.warnBytes`, `sync.failFiles`, and `sync.failBytes` in repo config;
- stop and warm a fresh lease if the remote workspace looks corrupted.

## Actions Hydration Times Out

Symptoms:

- `crabbox actions hydrate` dispatches a run but never sees the ready marker;
- later `crabbox run --id` does not enter the expected Actions workspace.

Checks:

```sh
bin/crabbox actions hydrate --id blue-lobster
bin/crabbox inspect --id blue-lobster --json
```

Fixes:

- open the workflow run URL and find the failed setup step;
- ensure the generated workflow writes the ready marker;
- confirm the workflow has permission to register or use the runner;
- keep secrets inside the workflow and only write non-secret handoff data.

## Docs Site Fails To Publish

Symptoms:

- Pages workflow fails during Pages setup;
- local docs build succeeds.

Checks:

```sh
npm run docs:check
gh run list --workflow pages.yml
```

Fixes:

- enable GitHub Pages for the repository or organization;
- rerun the Pages workflow after Pages is allowed;
- keep Markdown links relative so the static builder can rewrite them.
- fix broken internal Markdown links before checking whether Pages itself is down.
