# boxd

`provider: boxd` is a direct SSH-lease integration for [boxd](https://boxd.sh)
KVM microVMs. Crabbox asks the local `boxd` CLI to create, inspect, stop, or
destroy Crabbox-named machines, while boxd keeps authentication, the linked SSH
key, and the machine's public SSH endpoint ownership. Crabbox then runs its
usual SSH sync, command execution, status, and cleanup flow against the
machine's per-VM SSH port on the boxd edge proxy.

boxd machines boot in seconds from a full Ubuntu 24.04 image with git, rsync,
Docker, and Docker Compose preinstalled, and idle machines auto-suspend and
wake on SSH ingress. Every machine the provider creates is **isolated** —
boxd's sandbox mode: no east-west reachability to the account's other machines,
no in-VM `boxd` CLI, and no inherited integrations or agent credentials, while
outbound internet keeps working. Isolation is fixed at creation and deliberately
not configurable here.

boxd is direct-only. It never routes through the Crabbox coordinator and it
does not store boxd API tokens in Crabbox config.

## Requirements

- boxd CLI installed and on `PATH` (`curl -fsSL https://boxd.sh/downloads/install.sh | sh`),
  or set `boxd.cli`.
- A boxd login (`boxd auth login`), or a long-lived API key in `BOXD_TOKEN`
  (`boxd auth keys create`).
- Machine quota on the boxd account for at least one more machine.

The CLI manages the SSH credential itself: it generates a per-device key, links
it to the account, and maintains `~/.ssh/config` entries and `known_hosts` pins
for every machine. The provider reads those entries back; it never generates or
registers keys of its own.

Run a non-mutating preflight first:

```sh
crabbox doctor --provider boxd
crabbox doctor --provider boxd --json
```

## Config

```yaml
provider: boxd
boxd:
  cli: boxd
  apiUrl: ""            # empty = the CLI's default control plane
  org: ""               # empty = the CLI's active org
  workRoot: /home/boxd/crabbox
  deleteOnRelease: true # false stops machines on release instead
```

Environment overrides:

```text
CRABBOX_BOXD_CLI
CRABBOX_BOXD_API_URL
CRABBOX_BOXD_ORG
CRABBOX_BOXD_WORK_ROOT
CRABBOX_BOXD_DELETE_ON_RELEASE
```

The boxd token stays in the CLI's own credential store or `BOXD_TOKEN`, not in
Crabbox config and not on Crabbox argv. `boxd.cli`, `boxd.apiUrl`, and
`boxd.org` are honored from trusted config only.

## Flags

```text
--boxd-cli <path>
--boxd-api-url <url>
--boxd-org <name>
--boxd-work-root <path>
--boxd-delete-on-release
```

`--class` and `--type` are not supported for `provider=boxd`; machine sizing
follows the boxd account/org quota.

## Lifecycle

New leases create a machine named `crabbox-<lease-id>` (for example
`crabbox-cbx-a1b2c3d4e5f6`), so the machine's name itself encodes the Crabbox
lease. boxd exposes no server-side labels, so the local Crabbox claim written at
acquire time is the ownership anchor: List surfaces only Crabbox-named machines
that also have a matching local claim, and destructive operations require that
claim — a foreign machine that merely copies the naming convention is never
destroyed or surfaced as a lease.

Crabbox can resolve a boxd lease by Crabbox lease ID, local slug, or machine
name — but only when this install holds the matching local claim. Resolve
never adopts a machine by name: an unclaimed identity is refused before any
CLI call, so resolve can never mint a claim for a machine this install did
not create.

Release enforces the same fence directly: a canonical machine name or lease id
with no matching local claim is refused before any `machine stop` or
`machine remove` is issued, and a claim authorizes exactly the machine it
recorded.

Release destroys the machine by default (`boxd machine remove --confirm`) and
removes the local claim. With `boxd.deleteOnRelease: false` or
`--boxd-delete-on-release=false`, release stops the machine instead: the disk
persists, the claim is retained, and a later resolve restarts it.

Cleanup lists Crabbox-named machines, skips any without a matching local claim,
honors `--dry-run`, and reaps local claims whose machine no longer exists.

## SSH

The boxd edge proxy terminates SSH on a per-machine public port and
authenticates the CLI's linked key; DNS resolves `<machine>.boxd.sh` to the
proxy. The provider reads the CLI-maintained ssh-config entry (host, port,
user `boxd`, IdentityFile) into an explicit SSH target and disables connection
multiplexing — the proxy keeps one interactive session's state per TCP
connection, so ControlMaster-shared connections would clobber each other.

Machines in `standby` (idle auto-suspend) or `hibernated` states wake
automatically on SSH ingress; only explicitly `stopped` machines need a start,
which resolve performs.

## Examples

```sh
crabbox warmup --provider boxd --slug testbox
crabbox run --provider boxd -- pnpm test
crabbox ssh --provider boxd testbox
crabbox status --provider boxd testbox
crabbox stop --provider boxd testbox
```

Keep the machine across releases (stop instead of destroy):

```sh
crabbox run --provider boxd --boxd-delete-on-release=false -- pnpm test
```

## Live smoke

Run live smoke only after `boxd auth` succeeds against the intended control
plane:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=boxd CRABBOX_LIVE_COORDINATOR=0 \
  CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
```

If login or machine quota is unavailable, classify the live smoke as
`environment_blocked` instead of treating deterministic unit tests as live
proof.
