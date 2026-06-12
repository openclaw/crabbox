# doctor

`crabbox doctor` runs a preflight before you commit to a long workflow. It is
fast on a healthy machine, non-destructive, and never creates, mutates, or
deletes provider resources. Run it before your first `crabbox run`, after
rotating tokens or editing config, and as a sanity check in agent boot
sequences or CI smoke jobs.

```sh
crabbox doctor
crabbox doctor --provider aws
crabbox doctor --provider hetzner --target linux
crabbox doctor --provider xcp-ng --json
crabbox doctor --provider hostinger
crabbox doctor --provider ssh --target windows --windows-mode normal --static-host win-dev.local
crabbox doctor --id swift-crab
crabbox doctor --profile live-qa --id swift-crab
crabbox doctor --from-run run_abcdef123456
crabbox doctor --pond my-pond
crabbox doctor --all --prepare-check
crabbox doctor --json
```

## What it checks

Doctor walks a sequence of checks, skipping any that do not apply to the
selected provider, target, or context:

```text
config     writable config file exists and has safe permissions (expects 0600)
tools      provider-applicable local tools are present and executable
remote     optional SSH/tool probe against a resolved lease (--id / --from-run)
coord      coordinator URL is reachable and healthy (brokered providers)
broker     signed token is valid and an identity resolves
provider   provider readiness, with no mutation; broker secrets or a direct API probe
admin      admin token can list the machine pool (only when an admin token is set)
capacity   warns when the implicit default machine type is oversized for tests
ssh-key    explicit SSH key path and matching .pub are readable
pond       Tailscale policy row exists for a local pond (--pond)
```

### Local tools

The tool list is derived from the provider's capabilities. `git` is always
checked. Providers that use SSH add `ssh` and `ssh-keygen`; providers that
rsync your checkout add `rsync`; providers that ship a local archive add `tar`.
A missing tool prints `missing` and fails the run.

### Provider readiness

Provider readiness validates the selected provider without creating a lease.

- When a coordinator is configured for a brokered provider (`aws`, `azure`,
  `gcp`, `hetzner`), doctor asks the broker for secret readiness. It reports
  missing Worker secret names such as `AZURE_TENANT_ID` without exposing secret
  values. For AWS, broker readiness can also include non-mutating EC2 vCPU quota
  checks; low quotas print advisory `warning` lines and do not fail the run.
- Without a coordinator, providers that implement a direct doctor run their own
  non-mutating check (cheapest list or readiness API). These print stable fields
  such as `timeout=10s`, `api=list`, and `mutation=false` so scripts can tell
  what was probed. Direct AWS also checks EC2 vCPU quotas. GCP uses an
  aggregated Compute Engine inventory query across zones. XCP-ng opens a XAPI
  session and lists Crabbox-managed leases without creating, changing, or
  deleting VMs. Hostinger lists VPS
  inventory plus priced VPS catalog entries, payment methods, templates, and
  data centers, then reports `purchase=explicit release=stop`; it does not
  purchase, start, stop, delete, or cancel a VPS.
- Delegated providers run their own direct readiness check where available; for
  example Cloudflare validates the configured runner URL and bearer token
  against the runner readiness API. Blacksmith Testbox reports runtime as
  provider-hydrated because GitHub Actions hydration is owned by Testbox.
- Providers with no direct doctor print `skip provider ... direct_doctor=unsupported`.

The provider check is bounded to a 10s timeout. A failure adds a `class`
(`timeout`, `tool`, `config`, `auth`, `network`, or `provider`) and a
remediation `hint`:

```text
failed  provider provider=gcp class=auth hint=check_gcp_project_credentials_and_compute_instances_list ...
```

### SSH key

When `CRABBOX_SSH_KEY` is set, doctor validates the private key and its matching
`.pub` file. When it is unset, doctor reports `ok ssh-key per-lease` because
each lease generates its own key, so a global key is not required.

### Remote probe (`--id`)

`crabbox doctor --id <lease-id-or-slug>` resolves the lease and runs a short
remote probe over SSH against the target host. The default probe reports remote
`git`, `rsync`, `curl`, and `jq` versions. Native Windows targets use a
Windows-specific probe. A failing remote probe exits `7`.

### Profile prerequisites (`--profile` + `--id`)

When `--profile <name> --id <lease>` selects a profile with `doctor.enabled:
true`, doctor runs that profile's remote prerequisite contract instead of the
generic probe. Profiles can require exact tool availability, a Node major
version, a usable Docker daemon, Docker Compose v2, and a minimum free disk. A
failing profile doctor reports `failed` lines for the missing prerequisites and
exits nonzero without installing or changing anything. Profile doctor is not
supported for native Windows targets (exits `2`).

### From a recorded run (`--from-run`)

`crabbox doctor --from-run <run-id>` is for triaging a recorded failure. Doctor
fetches the run record and applies its provider, target, class, server type,
lease, and phase before running diagnostics. This requires a configured
coordinator (exits `2` otherwise). Older run records may omit fields; doctor
prints a `warning run` line with `missing=...` and skips checks that cannot be
tied to the run, such as the remote probe when no lease ID was recorded.

### Pond Tailscale policy (`--pond`)

`crabbox doctor --pond <name>` verifies the Tailscale policy row for an existing
local pond claim set. The check confirms that the concrete
`tag:cbx-pond-<owner>-<pond>` tag is declared in `tagOwners` and is allowed to
reach itself through either `grants` or legacy `acls`. It reads the policy only
when the pond has at least one locally claimed Tailscale-capable member and
`TS_API_KEY` is exported (`TS_TAILNET` selects the tailnet); otherwise it skips
with a reason. Self-hosted control planes that do not expose the Tailscale
policy API are skipped with a pointer to the manual snippet. Plain
`crabbox doctor` never calls the Tailscale API. Verification needs only
`TS_API_KEY`; automatic ACL edits also require `CRABBOX_POND_ACL_BOOTSTRAP=1`.

### Provider matrix (`--all --prepare-check`)

`crabbox doctor --all --prepare-check` checks the default test-runner provider
matrix (`blacksmith-testbox,aws,azure,gcp`) and adds a `prepare` row for each
provider. The prepare row reports the resolved class, machine type, and
configured hydration workflow/job, without creating a lease. Use
`--providers a,b,c` to override the matrix.

For the full per-check breakdown of how each one decides between `ok`, `skip`,
`warning`, and `failed`, see [Doctor checks](../features/doctor.md).

## Output

```text
ok      config   ~/.config/crabbox/config.yaml permissions=0600
ok      git      /usr/bin/git
ok      ssh      /usr/bin/ssh
ok      ssh-keygen /usr/bin/ssh-keygen
ok      rsync    /usr/bin/rsync
ok      coord    https://broker.example.com access=none
ok      broker   auth=user owner=alice@example.com org= default_type=
ok      provider provider=aws coordinator_secrets=ready
ok      ssh-key  per-lease
```

Failures swap the leading `ok` for `failed` (or `missing` for absent tools) and
add a class plus remediation hint. AWS quota warnings are advisory: doctor still
exits `0` unless another check fails.

`--json` prints the same checks as a structured object with `ok`, `provider`,
and `checks` fields. Each check includes `status`, `check`, `message`, and
parsed `details` when available.

Exit codes:

- `0` — no failures (skips and warnings do not change this).
- `1` — at least one check failed.
- `2` — `--from-run` without a coordinator, or profile doctor on a native
  Windows target.
- `7` — the remote SSH probe failed.

## Flags

```text
--provider <name>             provider to validate (defaults to configured provider)
--profile <name>              configured profile for remote prerequisite checks
--id <lease-id-or-slug>       resolve a lease and run a remote SSH/tool probe
--from-run <run-id>           load provider/target/lease/phase context from a recorded run
--pond <name>                 verify Tailscale policy setup for this pond
--all                         check the provider test-runner matrix
--providers <list>            comma-separated providers for --all
--prepare-check               include test-preparation readiness checks
--doctor-probe-ssh            probe static SSH reachability without leasing
--json                        print JSON
--target linux|macos|windows  target OS (affects which checks apply)
--windows-mode normal|wsl2    when target=windows
--static-host <host>          static SSH host (provider ssh)
--static-user <user>          static SSH user override
--static-port <port>          static SSH port override
--static-work-root <path>     static target work root
```

Provider-specific flags (for example Azure Dynamic Sessions endpoint and pool)
are also accepted; see the relevant provider docs.

## Why it is safe to automate

Doctor never provisions, never costs money, and never modifies state, so it is
safe to run from `pre-commit`, scheduled jobs, and CI. Use it when triaging
"Crabbox is broken" reports: it often catches the problem before the user has to
describe it.

Related docs:

- [Doctor checks](../features/doctor.md)
- [Configuration](../features/configuration.md)
- [Auth and admin](../features/auth-admin.md)
- [Network and reachability](../features/network.md)
- [Pond](../features/pond.md)
- [Troubleshooting](../troubleshooting.md)
