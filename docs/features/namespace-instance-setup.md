# Namespace Instance Setup

Read when:

- installing and authenticating the Namespace `nsc` CLI for Crabbox;
- preparing a machine so `provider: namespace-instance` works non-interactively;
- running the opt-in live smoke for Namespace Compute Instances.

Crabbox does not talk to Namespace with a stored API token. It shells out to the
upstream `nsc` CLI and lets that CLI own login, account selection, endpoint,
keychain, inventory, create, describe, extend, and destroy. Crabbox then uses
the SSH target returned by `nsc` for normal SSH and rsync.

For provider selection, config keys, and the lifecycle boundary, see
[Namespace Instance](namespace-instance.md).

## Install and Authenticate

Install the upstream Namespace CLI and log in:

```sh
nsc login
nsc auth check-login
```

For automation hosts, complete the login handoff using the flow printed by
`nsc login`. Do not paste login codes, auth tokens, or workspace/account details
into Crabbox config, scripts, docs, or issue comments.

Optional Namespace CLI routing can be configured through Crabbox when needed:

```yaml
provider: namespace-instance
namespaceInstance:
  endpoint: https://api.namespace.example
  keychain: crabbox
  region: us-west
```

Those values become `nsc --endpoint`, `nsc --keychain`, and `nsc --region`
arguments. Leave them unset for the CLI defaults.

## Read-Only Check

Before creating an instance, verify local readiness:

```sh
nsc auth check-login
nsc list -o json
crabbox doctor --provider namespace-instance
```

`crabbox doctor` checks the same non-mutating prerequisites and reports whether
`nsc`, auth, and inventory access are ready.

## Live Smoke

The live smoke creates and destroys a real Namespace Compute Instance. It is
disabled unless all opt-in variables are explicit:

```sh
go build -trimpath -o bin/crabbox ./cmd/crabbox
CRABBOX_LIVE=1 \
CRABBOX_BIN=bin/crabbox \
CRABBOX_LIVE_PROVIDERS=namespace-instance \
CRABBOX_LIVE_REPO=/path/to/my-app \
scripts/live-smoke.sh
```

The smoke runs:

```text
doctor -> warmup -> status --wait -> run --no-sync -> list --json -> stop
```

It also registers a cleanup trap after `warmup` returns a lease, so a later
failure still attempts `crabbox stop --provider namespace-instance`.

Useful smoke overrides:

```text
CRABBOX_LIVE_NAMESPACE_INSTANCE_SLUG
CRABBOX_LIVE_NAMESPACE_INSTANCE_TTL
CRABBOX_LIVE_NAMESPACE_INSTANCE_IDLE_TIMEOUT
CRABBOX_LIVE_NAMESPACE_INSTANCE_DURATION
CRABBOX_LIVE_NAMESPACE_INSTANCE_MACHINE_TYPE
CRABBOX_LIVE_NAMESPACE_INSTANCE_WAIT_TIMEOUT
```

## How Crabbox Drives `nsc`

- **Doctor** — `nsc --help`, `nsc auth check-login`, and `nsc list -o json`.
- **Create** — `nsc create` with machine type, duration, purpose, SSH public key,
  cidfile, JSON output path, optional ephemeral mode, optional unique tag,
  optional volumes, and Crabbox labels.
- **Describe/list** — `nsc describe <id> -o json` and `nsc list -o json --all`.
- **Extend** — `nsc extend <id> --ensure_minimum <duration>` when a retained
  lease needs a longer minimum lifetime.
- **Release** — `nsc destroy <id> --force`.

## Troubleshooting

- `namespace-instance smoke requires CRABBOX_LIVE_REPO` means the destructive
  smoke was not explicitly pointed at a checkout. Set the variable to a local
  repo path.
- `namespace-instance smoke requires the authenticated Namespace nsc CLI on PATH`
  means `nsc` was not found. Install it and make sure the same shell can run
  `nsc auth check-login`.
- `namespace-instance smoke requires an authenticated nsc CLI` means the CLI is
  present but not logged in. Run `nsc login`, then retry `nsc auth check-login`.
- `plan_gap: nsc JSON did not expose a normal SSH target` means the current
  `nsc` response did not include enough SSH host/user/port information for the
  direct SSH path. Do not guess a target; capture redacted CLI behavior and add
  an API-backed SSH resolution path before enabling that environment.

Related docs:

- [Namespace Instance](namespace-instance.md)
- [Provider: Namespace Instance](../providers/namespace-instance.md)
- [Namespace Devbox setup](namespace-devbox-setup.md)
