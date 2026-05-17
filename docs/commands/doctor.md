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
```

## What It Checks

```text
config       config files load and parse, required keys are present
auth         broker token is set, signed token is valid, identity resolves
network      coordinator URL reachable, DNS works, SSH transport probes work
ssh          SSH key path readable, key permissions sane, ssh-keygen on PATH
tools        rsync, git, ssh, ssh-keygen present and executable
```

For `--provider ssh`, doctor also probes the static host: SSH reachability
on the configured port, target-required tools (`bash`, `git`, `rsync`,
`tar` for POSIX targets; OpenSSH, PowerShell, and `tar` for native
Windows), and `static.workRoot` writability.

When `CRABBOX_SSH_KEY` is explicitly set, doctor validates the private key
and the matching `.pub` file. When unset, it skips that check because
per-lease keys do not need a global key.

When a coordinator is configured, doctor also asks the broker for secret
readiness for managed brokered providers such as AWS, Azure, GCP, and Hetzner. It
reports missing Worker secret names such as `AZURE_TENANT_ID` without exposing
secret values. Static, Proxmox, and delegated providers skip this broker-secret check.
Delegated providers can still run their own direct readiness checks; for example,
Cloudflare validates the configured runner URL and bearer token against the
authenticated runner readiness API.

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
```

Failures swap the leading `ok` for `fail` and add a remediation hint:

```text
auth:
  fail  broker token is missing - run `crabbox login`
```

Exit code is `0` on full success, `2` on any failure. Skips never change
the exit code.

## Flags

```text
--provider hetzner|aws|azure|gcp|proxmox|ssh   provider to validate
--profile <name>             configured profile for remote prereq checks
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
