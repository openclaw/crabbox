# doctor

`crabbox doctor` runs the local preflight before you commit to a long
workflow. It is fast on a healthy machine, non-destructive, and does not create
or mutate provider resources.

```sh
crabbox doctor
crabbox doctor --provider aws
crabbox doctor --profile live-qa --id blue-lobster
crabbox doctor --provider hetzner --target linux
crabbox doctor --provider ssh --target windows --windows-mode normal --static-host win-dev.local
crabbox doctor --json
```

## What It Checks

```text
config       config files load and parse, required keys are present
auth         broker token is set, signed token is valid, identity resolves
network      coordinator URL reachable, DNS works, SSH transport probes work
ssh          SSH key path readable, key permissions sane, ssh-keygen on PATH
tools        provider-applicable local tools are present and executable
```

For `--provider ssh`, doctor validates that `static.host` is configured. Add
`--doctor-probe-ssh` to run a short SSH reachability probe without creating a
lease. Use `crabbox doctor --id <lease>` for a remote SSH/tool probe against a
resolved target.

When `CRABBOX_SSH_KEY` is explicitly set, doctor validates the private key
and the matching `.pub` file. When unset, it skips that check because
per-lease keys do not need a global key.

When a coordinator is configured, doctor also asks the broker for secret
readiness for managed brokered providers such as AWS, Azure, GCP, and Hetzner. It
reports missing Worker secret names such as `AZURE_TENANT_ID` without exposing
secret values. Static, Proxmox, and delegated providers skip this broker-secret check.
Delegated providers can still run their own direct readiness checks; for example,
Cloudflare validates the configured runner URL and bearer token against the
authenticated runner readiness API. Direct provider checks print stable fields
such as `timeout=10s`, `api=list`, and `mutation=false` so scripts can tell
what was checked. GCP uses an aggregated Compute Engine inventory query across
zones, while the other built-in providers use their cheapest non-mutating list
or readiness API. Blacksmith Testbox reports runtime as provider-hydrated
because GitHub Actions hydration is owned by Testbox.

When `--profile <name> --id <lease>` selects a profile with `doctor.enabled:
true`, doctor runs that profile's remote prerequisite contract instead of the
generic remote probe. Profiles can require exact tool availability, Node major
version, Docker daemon usability, Docker Compose v2, and minimum free disk. A
failing profile doctor reports `failed` lines for missing prerequisites and
exits nonzero without installing or changing anything.

For the full list of checks, including how each one decides between
`fail`, `skip`, and `ok`, see
[Doctor checks](../features/doctor.md).

## Output

```text
config:
  ok    user config: ~/.config/crabbox/config.yaml
  ok    repo config: ./.crabbox.yaml
  ok    provider: aws
  ok    target: linux
auth:
  ok    broker: https://crabbox.openclaw.ai
  ok    owner: alex@example.com
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
  ok    tar
```

Failures swap the leading `ok` for `failed` and add a class and remediation
hint:

```text
failed  provider provider=gcp class=auth hint=check_gcp_project_credentials_and_compute_instances_list ...
```

`--json` prints the same checks as structured JSON with `ok`, `provider`, and
`checks` fields. Each check includes `status`, `check`, `message`, and parsed
`details` when available.

Exit code is `0` on full success, `1` on any failure. Skips never change
the exit code.

## Flags

```text
--provider hetzner|aws|azure|gcp|proxmox|ssh   provider to validate
--profile <name>             configured profile for remote prereq checks
--json                       print JSON
--doctor-probe-ssh           probe static SSH reachability
--target linux|macos|windows target OS for ssh provider checks
--windows-mode normal|wsl2   when target=windows
--static-host <host>         static SSH host
--static-user <user>         static SSH user override
--static-port <port>         static SSH port override
--static-work-root <path>    static target work root
```

## When To Run

- before the first `crabbox run` on a new machine;
- after rotating the broker token;
- after editing `~/.crabbox.yaml` or repo config;
- in agent boot sequences as a sanity check;
- when triaging "Crabbox is broken" reports - doctor often catches the
  problem before the user has to describe it.

Doctor is safe to run from `pre-commit`, scheduled jobs, and CI smoke
because it never provisions, never costs money, and never modifies state.

Related docs:

- [Doctor checks](../features/doctor.md)
- [Configuration](../features/configuration.md)
- [Auth and admin](../features/auth-admin.md)
- [Network and reachability](../features/network.md)
- [Troubleshooting](../troubleshooting.md)
