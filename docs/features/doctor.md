# Doctor Checks

Read when:

- adding a new precheck before users run long workflows;
- debugging an unexpected `doctor` failure;
- deciding whether a check belongs in `doctor` or somewhere else.

`crabbox doctor` is the preflight. It validates the things that have silently
broken commands in the past so users get an answer before they spend ten
minutes on a failed lease. For end-user invocation, flags, and examples, see
the [doctor command reference](../commands/doctor.md); this page documents how
each check decides its status and how to add a new one.

The command is fast (under a second on a healthy machine), non-destructive, and
never calls billable provider create APIs. When a coordinator is configured it
performs cheap broker checks such as health, identity, and provider secret
readiness. The provider readiness probe is bounded to a 10s timeout.

## Output Model

Doctor prints one line per check. Unlike a grouped report, every line is
self-contained: a status, a check name, and a message. Checks run in a fixed
order so the output is diffable across runs.

Statuses:

```text
ok       the check passed
missing  a required local tool is absent from PATH (counts as a failure)
failed   the check ran and did not pass (counts as a failure)
warning  advisory only; does not change the exit code
skip     the check did not apply and was not run
```

Check names you can see, roughly in emission order:

```text
run        --from-run context summary (provider/target/lease/phase)
config     writable config file exists with safe permissions (0600)
git        git is on PATH
ssh        ssh is on PATH (SSH-backed providers)
ssh-keygen ssh-keygen is on PATH (SSH-backed providers)
rsync      rsync is on PATH (rsync-sync providers)
tar        tar is on PATH (archive-sync providers)
remote     SSH/tool probe against a resolved lease (--id / --from-run)
coord      coordinator URL reachable and healthy (brokered + coordinator set)
broker     signed token valid, identity resolves (whoami)
provider   provider readiness without mutation
admin      admin token can list the machine pool (only when admin token set)
ssh-key    explicit SSH key path and matching .pub are readable
pond       Tailscale policy row exists for a local pond (--pond)
```

## Local Tools (`git`, `ssh`, `ssh-keygen`, `rsync`, `tar`)

The tool list is derived from the selected provider's capabilities, not a fixed
set:

- `git` is always checked.
- `ssh` and `ssh-keygen` are added for SSH-lease providers and any provider that
  declares the SSH feature.
- `rsync` is added for providers that use local rsync-based workspace sync
  (`crabbox-sync` feature).
- `tar` is added for providers that use local archive-based workspace sync
  (`archive-sync` feature, or the known archive providers `daytona`, `e2b`,
  `islo`, `tensorlake`).

A tool not on PATH prints `missing` and fails the run. The check is path-based,
not version-based — Crabbox tolerates any reasonably modern version.

## Config (`config`)

Doctor inspects the writable config path. If the file exists, it must have safe
permissions; the check prints `ok config <path> permissions=0600`, or `failed`
if the permission mode is unsafe. If no writable config file exists yet, the
check is silent (there is nothing to validate).

This check does not parse config keys or validate provider/target/network/class
values — invalid combinations surface later through provider readiness or the
relevant command.

## Coordinator and Identity (`coord`, `broker`, `admin`)

These checks run only when the selected provider supports the coordinator and a
coordinator URL is configured (`CRABBOX_COORDINATOR` or config). Without a
coordinator, doctor falls through to the direct provider check below.

- `coord` — the coordinator answers a health probe. The message reports the URL
  and the Cloudflare Access auth state, e.g. `access=none`.
- `broker` — `whoami` succeeds with the stored token. The message reports the
  auth kind, resolved owner/org, and the default server type.
- `admin` — only when an admin token is configured: the admin token can list the
  machine pool. An unauthorized admin token downgrades to a `warning` (user
  broker checks still passed) rather than failing the run.

## Provider Readiness (`provider`)

Provider readiness validates the selected provider without creating a lease.

**Brokered path (coordinator configured).** For providers whose coordinator
support is `supported` (`aws`, `azure`, `gcp`, `hetzner`), doctor asks the broker
for secret readiness. Missing Worker secret names are reported without exposing
values, for example `missing=AZURE_TENANT_ID,AZURE_SUBSCRIPTION_ID`. The broker
may attach additional non-mutating checks; AWS broker readiness can include EC2
vCPU quota checks via Service Quotas. Low quotas emit advisory `warning capacity`
lines (quota code, applied limit, default type, required vCPUs, recommended
class/type); if quotas cannot be inspected the capacity check is skipped rather
than warning about unproven pressure. When the coordinator path runs, doctor
returns immediately after the SSH-key check — it does not also run the direct
provider check.

**Direct path (no coordinator).** Providers that implement `DoctorProvider` run
their own non-mutating check (cheapest list or readiness API), bounded to the 10s
provider timeout. Examples:

- AWS reports EC2 inventory plus advisory vCPU quota checks.
- GCP reports an aggregated Compute Engine list query.
- Cloudflare validates its runner URL and bearer token against the runner's
  authenticated readiness endpoint, rather than treating a healthy coordinator as
  proof of runner readiness.
- Blacksmith Testbox reports runtime as provider-hydrated because GitHub Actions
  hydration is owned by Testbox.
- XCP-ng opens a XAPI session, resolves configured placement resources
  (template, storage repository, network, and host), lists Crabbox-managed
  leases, and reports `mutation=false`; incomplete `xcpNg.*` config fails
  before any inventory call.

Direct checks carry stable detail fields such as `timeout`, `api`, and
`mutation` so scripts can tell what was probed.

Direct Incus doctor follows this model: it resolves the selected
socket/address/remote path through the official Go client, reports
`mode`, `endpoint`, `project`, and `auth`, then performs a read-only list so
operators can distinguish config/auth drift from guest reachability issues
before running a live smoke.

**No direct doctor.** Providers without a `DoctorProvider` implementation print
`skip provider provider=<name> direct_doctor=unsupported`.

Failures add a `class` and a remediation `hint`. The class is one of `timeout`,
`tool`, `config`, `auth`, `network`, or `provider`, inferred from the error:

```text
failed  provider provider=gcp class=auth hint=check_gcp_project_credentials_and_compute_instances_list ...
```

## SSH Key (`ssh-key`)

When `CRABBOX_SSH_KEY` is set, doctor validates that the private key path is
readable and that a matching public key can be derived; either failure prints
`missing ssh-key`. When `CRABBOX_SSH_KEY` is unset, doctor prints
`ok ssh-key per-lease`, because each lease generates its own key and no global
key is required.

## Remote Probe (`remote`)

`crabbox doctor --id <lease-id-or-slug>` resolves the lease and runs a short
remote command over SSH against the target host. The default probe reports the
remote versions of `git`, `rsync`, `curl`, and `jq`. Native Windows targets use
a Windows-specific probe instead.

When a profile with `doctor.enabled: true` is selected via `--profile <name>`
(together with `--id`), the remote probe is replaced by that profile's
prerequisite contract (exact tools, Node major version, usable Docker daemon,
Docker Compose v2, minimum free disk). Profile doctor is rejected for native
Windows targets (exit `2`).

A failing remote probe exits `7`. When the lease came from `--from-run` and is
no longer resolvable, the remote check is downgraded to a `skip` instead of
erroring.

## From a Recorded Run (`run`)

`crabbox doctor --from-run <run-id>` triages a recorded failure. Doctor fetches
the run record and applies its provider, target, class, server type, lease, and
phase before running diagnostics; it requires a configured coordinator (exit `2`
otherwise). Older run records may omit fields — doctor then prints a
`warning run` line listing `missing=...` and skips checks that cannot be tied to
the run, such as the remote probe when no lease ID was recorded.

## Pond Tailscale Policy (`pond`)

`crabbox doctor --pond <name>` verifies the Tailscale policy row for an existing
local pond. The tag is `tag:cbx-pond-<owner>-<pond>`. The check passes only when
that tag is declared under `tagOwners` and is allowed to reach itself through a
`grants` row or a legacy `acls` row.

It reads the live policy only when:

- the pond has at least one locally claimed member (otherwise `skip`);
- a Tailscale-capable provider is present for the pond (otherwise `skip`);
- `TS_API_KEY` is exported (otherwise `skip`); `TS_TAILNET` selects the tailnet.

The policy lookup is bounded to a 4s timeout. Self-hosted control planes that do
not expose the Tailscale policy API (for example Headscale) are skipped with a
pointer to the manual snippet in [Pond](pond.md). A missing-but-expected policy
row is a `failed`. Plain `crabbox doctor` without `--pond` never calls the
Tailscale API. Verification needs only `TS_API_KEY`; automatic ACL installation
additionally requires `CRABBOX_POND_ACL_BOOTSTRAP=1`.

## What Doctor Does Not Do

Doctor avoids mutating provider state on purpose. It does not:

- start a real lease or provision a server;
- create, delete, or mutate cloud or self-hosted hypervisor resources;
- call provider APIs except for explicit, cheap readiness or inventory probes
  such as Cloudflare runner auth checks, Service Quotas reads, or provider list
  commands;
- run `git ls-files` against the repo (that belongs in `crabbox sync-plan`);
- estimate costs;
- modify config or rotate keys.

Anything that costs money or has side effects belongs in a different command.
Doctor is for "before I run anything, is my machine and configured control plane
sane?" and is safe to run from preflight hooks, agent boot, or CI smoke.

## Exit Codes

```text
0   no failures (skips and warnings do not change this)
1   at least one check failed
2   --from-run without a coordinator, or profile doctor on a native Windows target
7   the remote SSH probe failed
```

## JSON Output

`--json` is for automation. The object contains the overall `ok` boolean, the
selected `provider`, and a `checks` array. Each entry has `status`, `check`, an
optional `provider`, a `message`, and a parsed `details` map (key=value fields
lifted from the message, plus provider-supplied detail fields).

## Adding A Check

Doctor orchestration lives in `internal/cli/doctor.go`; the pond check lives in
`internal/cli/doctor_pond.go`. Prefer provider-owned `DoctorProvider`
implementations for direct provider checks rather than provider-specific branches
in core. Keep each check explicit and cheap, and emit stable `ok`, `failed`,
`missing`, `skip`, or `warning` lines that stay easy to scan in terminal logs.
Maintainers can run `scripts/live-doctor-smoke.sh` after building `bin/crabbox`
to exercise built-in providers against the local machine and configured
credentials.

Rules for new checks:

- keep them fast and non-blocking; respect the existing provider (10s) and pond
  (4s) timeouts;
- never call paid create/delete APIs or write any state;
- name the concrete missing tool, config key, or provider secret;
- `skip` (not `fail`) when the configuration genuinely does not apply (e.g. the
  SSH key check when `CRABBOX_SSH_KEY` is unset).

Add focused tests for new helpers or response parsing so future refactors do not
silently regress the user-facing output.

`scripts/live-doctor-smoke.sh` stays conservative by default. Local-only
providers such as Incus participate only when explicitly selected (for example
`CRABBOX_LIVE_DOCTOR_PROVIDERS=incus`) so machines without that local testbed do
not fail the shared smoke matrix.

Related docs:

- [doctor command](../commands/doctor.md)
- [Configuration](configuration.md)
- [Network and reachability](network.md)
- [SSH keys](ssh-keys.md)
- [Pond](pond.md)
- [Source map](../source-map.md)
