# Troubleshooting

Use this guide when a lease, run, or deployment misbehaves. Each section lists
the symptoms you will see, the commands that confirm the cause, and the fixes
that resolve it. Read it when:

- a lease fails to create or is rejected before provisioning;
- SSH never becomes ready on a new box;
- tailnet reachability behaves unexpectedly;
- sync transfers the wrong files, stalls, or aborts;
- Actions hydration times out;
- the docs site fails to publish.

## First steps

Most problems show up in one of four places: local config, broker readiness,
the lease list, or usage/cost state. Start here before digging deeper.

```sh
crabbox doctor
crabbox config show
crabbox list --json
crabbox usage --scope all --json
```

`doctor` reports local and broker/provider readiness; pass `--provider <name>`
to scope its checks to one provider. See [doctor](features/doctor.md) and
[cost & usage](features/cost-usage.md) for the data behind these commands.

## Broker auth fails

The CLI talks to the broker (the Cloudflare Worker) over HTTP for every lease
lifecycle call. Auth failures stop a lease before it reaches a provider.

**Symptoms**

- `401` or `403` responses;
- `missing broker token`;
- a GitHub `Invalid redirect_uri` error during browser login;
- a Cloudflare Access challenge page returned instead of JSON.

**Checks**

```sh
crabbox config show
printenv CRABBOX_COORDINATOR
printenv CRABBOX_COORDINATOR_TOKEN
printenv CRABBOX_PUBLIC_URL
```

**Fixes**

- Configure the broker with `crabbox config set-broker --url <worker-url>` (or
  log in with `crabbox login`).
- Point the CLI at the Worker URL, or at the Access-protected route only when
  that is intended.
- Ensure `CRABBOX_COORDINATOR_TOKEN` matches the Worker's `CRABBOX_SHARED_TOKEN`.
- For self-hosted GitHub browser login, create a GitHub OAuth app and set its
  callback URL to `https://<your-coordinator-host>/v1/auth/github/callback`.
- Ensure the Worker's `CRABBOX_PUBLIC_URL` uses the same public origin as that
  GitHub OAuth callback.

See [broker auth & routing](features/broker-auth-routing.md) for the full token
precedence and Access integration.

## SSH host key or control socket fails

**Symptoms**

- SSH warns that the host identification changed after a provider reused an IP;
- a reused warm lease connects to the wrong `ControlMaster` socket;
- config paths under `~/Library/Application Support` appear split at the space.

**Checks**

```sh
crabbox inspect --id swift-crab --json
crabbox ssh --id swift-crab
```

**Fixes**

- Upgrade to a build that quotes SSH config values containing spaces.
- Keep per-lease keys under the Crabbox config `testboxes/<lease>` directory
  (see [SSH keys](features/ssh-keys.md)).
- Do not manually override `UserKnownHostsFile` or `ControlPath` unless you are
  debugging SSH itself.

## Lease rejected by cost control

The broker enforces active-lease counts and monthly reserved-USD budgets before
provisioning. Over-limit requests are rejected with HTTP 429.

**Symptoms**

- `cost_limit_exceeded`;
- the lease request fails before any provider machine is created.

**Checks**

```sh
crabbox usage --scope user --user "$(git config user.email)"
crabbox usage --scope org --org example-org
```

**Fixes**

- Raise the relevant monthly or active-lease limit on the broker.
- Shorten `--idle-timeout` (or `--ttl`) to lower the reserved estimate.
- Choose a smaller `--class`.
- Stop kept leases you no longer need with `crabbox stop <id-or-slug>`.

## Provider not configured, or capacity/quota fails

**Symptoms**

- `provider_not_configured` (HTTP 424 from the broker);
- `crabbox doctor --provider azure` reports `missing=AZURE_TENANT_ID,...`;
- the class falls back from a dedicated machine to a smaller one;
- an AWS Spot request cannot be fulfilled;
- AWS reports `VcpuLimitExceeded` for large On-Demand instances;
- server creation fails before SSH is reachable.

**Checks**

```sh
crabbox doctor --provider aws
crabbox list --json
crabbox usage --scope all
CRABBOX_CAPACITY_REGIONS=eu-west-1,eu-west-2,eu-central-1,us-east-1,us-west-2 \
  crabbox warmup --provider aws --class standard --market on-demand --timing-json
```

**Fixes**

- Set the named Worker provider secrets before retrying brokered leases.
- Choose a smaller `--class`.
- Override the AWS capacity market for a one-off launch with `--market on-demand`
  or `--market spot`.
- Set `CRABBOX_CAPACITY_REGIONS` so brokered and direct AWS launches can try
  several regions.
- Set `CRABBOX_CAPACITY_AVAILABILITY_ZONES` only when you intentionally want
  specific zones within those regions.
- Set `CRABBOX_CAPACITY_STRATEGY=most-available` to prefer regions with capacity.
- Set `CRABBOX_CAPACITY_LARGE_CLASSES` when your installation wants warnings for
  classes beyond `beast`.
- If `doctor` prints `warning capacity`, use its recommended class/type or
  request the printed AWS quota code.
- Raise the AWS `Running On-Demand Standard (A, C, D, H, I, M, R, T, Z) instances`
  quota for the C/M/R/T/Z families, or the matching Spot quota when using Spot.
- Raise the Hetzner dedicated-core quota when dedicated classes are required.

Brokered AWS launches record provisioning attempts and use AWS Service Quotas
when available, reporting the quota code, applied vCPU limit, requested type, and
required vCPUs before trying the next candidate. Brokered responses include
`capacityHints` so callers can surface the selected region/market and the next
operator action instead of parsing raw provider errors. `crabbox doctor
--provider aws` surfaces the same quota pressure before the first warmup.

If AWS reports `InvalidInstanceID.NotFound` during coordinator-backed lease
creation, the backing instance record was stale by the time Crabbox tried to use
it. Crabbox discards that lease record best-effort and retries once with a fresh
lease. See [capacity fallback](features/capacity-fallback.md) for the full
algorithm.

## Provider machine looks orphaned

**Symptoms**

- `crabbox list` shows `orphan=no-active-lease` or `orphan=missing-lease-label`;
- the provider console has a `crabbox-cbx-...` machine, but `crabbox inspect`
  returns not found.

**Checks**

```sh
crabbox list --provider hetzner
crabbox list --provider aws
crabbox admin leases --state active
```

**Fixes**

- Do not delete `keep=true` machines automatically.
- Stop or delete a machine only after confirming no active coordinator lease
  references it.
- Use `crabbox stop <id-or-slug>` for active leases; reserve provider/admin
  cleanup (`crabbox cleanup`, `crabbox admin delete`) for confirmed orphans.

The broker also sweeps untracked AWS instances and idle Mac dedicated hosts on a
schedule. See [lifecycle & cleanup](features/lifecycle-cleanup.md).

## SSH never becomes ready

A box exists but bootstrap has not finished writing the readiness marker, or the
SSH port is unreachable. The default SSH user is `crabbox`, the primary port is
`2222`, and fallback port `22` is tried unless disabled.

**Symptoms**

- the lease exists, but `crabbox run` waits until the SSH timeout;
- the primary port (`2222`) and all fallback ports are unreachable;
- the `crabbox-ready` marker is missing.

**Checks**

```sh
crabbox inspect --id cbx_... --json
ssh -p 2222 crabbox@HOST crabbox-ready
ssh -p 2222 crabbox@HOST test -f /var/lib/crabbox/bootstrapped
ssh -p 22 crabbox@HOST crabbox-ready
```

**Fixes**

- Wait for cloud-init to finish on freshly created machines.
- Verify the security group or firewall allows the primary SSH port and the
  configured fallback ports.
- Set `CRABBOX_SSH_FALLBACK_PORTS=none` when fallback port `22` should not be
  opened or tried.
- Inspect the provider console output for cloud-init failures.
- Retry the lease if bootstrap failed before creating the ready marker.

See [runner bootstrap](features/runner-bootstrap.md) for the readiness contract.

## Tailscale path fails

**Symptoms**

- `--tailscale` lease creation fails with `tailscale_unavailable`,
  `tailscale_disabled`, or `invalid_tailscale_tags`;
- `--network tailscale` reports the lease has no tailnet address;
- `--network tailscale` reports the tailnet host is unreachable over SSH;
- `--network auto` falls back to `public`;
- `tailscale exit node ... joined but remote internet egress failed`.

**Checks**

```sh
crabbox config show
crabbox inspect --id swift-crab --json
crabbox ssh --id swift-crab --network tailscale
tailscale status
tailscale ping <tailscale-fqdn-or-100.x-address>
```

**Fixes**

- For brokered leases, configure the Worker secrets
  `CRABBOX_TAILSCALE_CLIENT_ID` and `CRABBOX_TAILSCALE_CLIENT_SECRET`.
- Keep `CRABBOX_TAILSCALE_ENABLED` unset or `1`; set it to `0` only to disable
  brokered Tailscale intentionally.
- Ensure requested tags are in the Worker's `CRABBOX_TAILSCALE_TAGS` allowlist.
- Ensure the local client is joined to the same tailnet and that ACLs allow SSH
  to the tagged node.
- For exit nodes, ensure the node is approved and that tailnet grants or ACLs
  allow the lease tag (for example `tag:crabbox`) to reach `autogroup:internet`.
- If the exit node is a personal Mac, verify Tailscale still advertises it as an
  exit node and that the Mac can forward internet traffic for clients.
- Use `--network public` to prove the provider SSH path independently.
- Use `--network auto` when fallback to public is acceptable.
- Use `--network tailscale` when a missing or unreachable tailnet path should
  fail the command.

Crabbox uses OpenSSH with per-lease SSH keys over the selected host. Tailscale
SSH, Serve, Funnel, and direct VNC binding are not part of managed lease support.
See [tailscale](features/tailscale.md) and [network](features/network.md).

## Sync looks wrong

`run` syncs your dirty checkout to the box via a git manifest plus rsync, with
fingerprint short-circuiting and guardrails. Wrong-base or unexpected-deletion
problems usually trace to git state or excludes.

**Symptoms**

- changed-test detection picks the wrong base;
- deleted files unexpectedly reappear on the remote;
- sync aborts on a mass tracked-file deletion;
- sync warns or fails before rsync because the candidate tree is too large.

**Checks**

```sh
git status --short
git ls-files --cached --others --exclude-standard | wc -l
crabbox run --id cbx_... -- git status --short
crabbox run --id cbx_... --sync-only --debug
```

**Fixes**

- Commit, stash, or intentionally keep local deletions before syncing.
- Add generated directories to `.gitignore` or to `sync.exclude` in
  `.crabbox.yaml`.
- Keep `.git`, build caches, and package caches out of the repo source tree.
- Use `--force-sync-large` only after verifying the candidate file count and
  byte total are expected.
- Check the repo-local `.crabbox.yaml` sync excludes.
- Rerun without relying on the sync fingerprint after large tree changes.
- Verify base-ref hydration in repo config.

## Sync stalls or times out

**Symptoms**

- rsync prints little output for a long time;
- `rsync timed out after ...`;
- a local cache directory made the first sync unexpectedly huge.

**Checks**

```sh
crabbox config show
crabbox sync-plan
crabbox run --id cbx_... --sync-only --debug
```

**Fixes**

- Inspect the printed sync candidate estimate (`crabbox sync-plan`) before
  retrying.
- Lower `sync.timeout` for quick failure in agent loops, or raise it for
  intentionally large source transfers.
- Tune `sync.warnFiles`, `sync.warnBytes`, `sync.failFiles`, and `sync.failBytes`
  in repo config.
- Stop and warm a fresh lease if the remote workspace looks corrupted.

See [sync](features/sync.md) for the full manifest and hydration flow.

## Actions hydration times out

`crabbox actions hydrate` populates a lease's workspace by driving the repo's
configured GitHub Actions workflow. It waits for the workflow to write a ready
marker before later `run` calls reuse that workspace.

**Symptoms**

- `crabbox actions hydrate` dispatches a run but never sees the ready marker;
- a later `crabbox run --id` does not enter the expected Actions workspace.

**Checks**

```sh
crabbox actions hydrate --id swift-crab
crabbox inspect --id swift-crab --json
```

**Fixes**

- Open the workflow run URL and find the failed setup step.
- Ensure the generated workflow writes the ready marker.
- Confirm the workflow has permission to register or use the runner.
- Keep secrets inside the workflow and write only non-secret handoff data.

See [Actions hydration](features/actions-hydration.md).

## Docs site fails to publish

**Symptoms**

- the Pages workflow fails during Pages setup;
- the local docs build succeeds.

**Checks**

```sh
scripts/check-docs.sh
gh run list --workflow pages.yml
```

**Fixes**

- Enable GitHub Pages for the repository or organization.
- Rerun the Pages workflow after Pages is allowed.
- Keep Markdown links relative so the static builder can rewrite them.
- Fix broken internal Markdown links before assuming Pages itself is down.
