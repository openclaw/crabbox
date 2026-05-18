# Doctor Checks

Read when:

- adding a new precheck before users run long workflows;
- debugging an unexpected `doctor` failure;
- deciding whether a check belongs in `doctor` or somewhere else.

`crabbox doctor` is the preflight. It validates the things that have
silently broken commands in the past so users get an answer before they
spend ten minutes on a failed lease.

The command is fast (under a second on a healthy machine), non-destructive,
and never calls billable provider create APIs. When a coordinator is configured
it performs cheap broker checks such as health, identity, and provider secret
readiness.

## Categories

Doctor groups checks under five categories:

```text
config       config files load and parse, required keys are present
auth         broker token is set, signed token is valid, identity resolves
network      coordinator URL reachable, DNS works, SSH transport probes work
ssh          SSH key path readable, key type acceptable, ssh-keygen on PATH
tools        provider-applicable local tools are present and executable
```

Each category emits one or more pass/fail/skip lines. Failures are listed
first; passes and skips follow in deterministic order so the output is
diffable across runs.

## What `config` Checks

- The user config file parses without error.
- The repo config (when present) parses without error.
- Provider name resolves through `ProviderFor`.
- Target OS is one of `linux`, `macos`, `windows`.
- Network mode is one of `auto`, `tailscale`, `public`.
- Tailscale config validates when `tailscale.enabled: true` (tags non-empty,
  hostname template non-empty, exit-node-allow-lan-access requires an
  exit node, target is `linux`, provider is not Blacksmith or static).
- Class is one of `standard`, `fast`, `large`, `beast` when set; explicit
  `type:` values are accepted as-is.

## What `auth` Checks

- A broker URL is configured if the user expects coordinator mode.
- A broker token is present when the URL is configured.
- The signed token (when GitHub login was used) decodes and is not expired.
- Owner can be resolved from `CRABBOX_OWNER`, Git env, or
  `git config user.email`.
- `whoami` succeeds against the configured coordinator with the stored
  token.
- Provider readiness succeeds for managed brokered providers that need Worker
  secrets. Missing names are reported without exposing values, for example
  `AZURE_TENANT_ID` or `AZURE_SUBSCRIPTION_ID`. Delegated and static providers
  skip this check.
- Direct provider readiness succeeds for delegated providers that expose a cheap
  non-mutating check. Cloudflare validates its runner URL and bearer token
  directly instead of treating a healthy coordinator as proof of runner
  readiness, using the runner's authenticated readiness endpoint. All built-in
  providers now expose provider-owned direct doctor checks; providers with list
  APIs use non-mutating inventory checks when running in direct mode. Direct
  checks include stable `timeout`, `api`, and `mutation` fields; for example
  GCP reports an aggregated Compute Engine list query, and Blacksmith Testbox
  reports provider-owned runtime hydration.

When auth is missing, doctor prints `crabbox login` as the next step.

## What `network` Checks

- The coordinator URL resolves via DNS.
- The coordinator is reachable over HTTPS within a small timeout.
- When `--network tailscale` is configured, `tailscale status` reports a
  joined client.
- `crabbox doctor --id <lease>` runs an SSH transport/tool probe against the
  resolved target. The static provider check without `--id` only verifies that
  `static.host` is configured unless `--doctor-probe-ssh` is set.

DNS is checked before HTTPS so a broken DNS responder does not look like a
broker outage.

## What `ssh` Checks

- The configured SSH key path (`ssh.key` or `CRABBOX_SSH_KEY`) is readable
  when set.
- The key file has a sensible permissions mode (warn on group/world
  readable).
- `ssh-keygen` is on PATH so per-lease key generation works.
- The user's `~/.ssh/known_hosts` is writable (if it exists).

When `ssh.key` is unset, doctor skips the path validation - per-lease keys
do not need a global key.

## What `tools` Checks

- `git` is on PATH.
- `ssh` and `ssh-keygen` are on PATH for SSH-backed providers.
- `rsync` is on PATH for providers that use local rsync-based workspace sync.
- `tar` is on PATH for providers that use local archive-based workspace sync.

The check is path-based, not version-based. Crabbox tolerates any reasonably
modern version of these tools.

## What Doctor Does Not Do

Doctor avoids mutating provider state on purpose. It does not:

- start a real lease or provision a server;
- create, delete, or mutate cloud or Proxmox resources;
- call delegated provider APIs except for explicit, cheap readiness or inventory
  probes such as Cloudflare runner auth checks or provider list commands;
- run `git ls-files` against the repo (that belongs in `crabbox sync-plan`);
- estimate costs;
- modify config or rotate keys.

Anything that costs money or has side effects belongs in a different command.
Doctor is for "before I run anything, is my machine and configured control
plane sane?" and should be safe to run from preflight hooks, agent boot, or CI
smoke.

## Output Shape

```text
config:
  ok    user config: ~/.config/crabbox/config.yaml
  ok    repo config: ./.crabbox.yaml
  ok    provider: aws
  ok    target: linux
  ok    network: auto
auth:
  ok    broker: https://crabbox.openclaw.ai
  ok    owner: alex@example.com
  ok    org:   openclaw
network:
  ok    coordinator dns
  ok    coordinator https
ssh:
  ok    ssh-keygen present
  skip  ssh.key unset (per-lease keys will be used)
tools:
  ok    git
  ok    rsync
  ok    ssh
  ok    ssh-keygen
```

Failures swap the leading `ok` for `fail` and add a remediation hint:

```text
failed  provider provider=gcp class=auth hint=check_gcp_project_credentials_and_compute_instances_list ...
```

Skips swap `ok` for `skip` and explain why the check did not run:

```text
network:
  skip  coordinator unconfigured (direct provider mode)
```

Exit code is `0` on full success, `1` on any failure. Skips do not change
the exit code.

Use `--json` for automation. The JSON view contains the overall `ok` boolean,
selected `provider`, and a `checks` array with each check's status, category,
message, and parsed details.

## Adding A Check

Doctor orchestration lives in `internal/cli/doctor.go`. Prefer provider-owned
`DoctorProvider` implementations for direct provider checks. Keep each check
explicit and cheap, and print stable `ok`, `failed`, `missing`, `skip`, or
`warning` lines that remain easy to scan in terminal logs. Maintainers can run
`scripts/live-doctor-smoke.sh` after building `bin/crabbox` to exercise every
built-in provider against the local machine and configured credentials.

Rules for new checks:

- they must run in under ~100ms;
- they must not call paid create/delete APIs or write any state;
- they must name the concrete missing tool, config key, or provider secret;
- they should `skip` (not `fail`) when the configuration genuinely does
  not apply (e.g. SSH key check when `ssh.key` is unset).

Add focused tests for new helpers or response parsing so future refactors do
not silently regress the user-facing output.

Related docs:

- [doctor command](../commands/doctor.md)
- [Configuration](configuration.md)
- [Network and reachability](network.md)
- [SSH keys](ssh-keys.md)
- [Source map](../source-map.md)
