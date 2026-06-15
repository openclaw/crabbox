# Phala Cloud Confidential Provider

Use `provider: phala` for short-lived confidential Linux CVMs managed by the
Phala Cloud `phala` CLI. Phala provisions Intel TDX (dstack) confidential VMs,
so this is Crabbox's first **confidential-compute** provider: the lease runs
inside a hardware-isolated trusted execution environment. Aliases are
`phala-cloud` and `dstack`.

## Setup

Install the `phala` CLI, authenticate it, then verify access:

```sh
npm install -g phala        # or: npx phala ...
phala login                 # device flow; or export PHALA_CLOUD_API_KEY
phala status
crabbox doctor --provider phala
```

Crabbox never reads or stores Phala credentials and never passes an API key on
the command line. It invokes the configured `phala` binary, which uses its own
stored auth profile (device-flow login or the `PHALA_CLOUD_API_KEY` environment
variable read by the CLI itself).

## Usage

```sh
crabbox warmup --provider phala --class standard --ttl 15m
crabbox run --provider phala -- go test ./...
crabbox list --provider phala --json
crabbox stop --provider phala <lease-id-or-slug>
```

Crabbox injects a per-lease SSH public key into the CVM at deploy time, connects
over SSH through the Phala gateway, uses the normal SSH/rsync data plane, and
deletes the CVM on release.

## Configuration

```yaml
provider: phala
target: linux
phala:
  cli: phala
  instanceType: tdx.small
  workRoot: /work/crabbox
  nodeId: ""
  compose: ""
```

Class defaults are `standard=tdx.small`, `fast=tdx.medium`, `large=tdx.large`,
and `beast=tdx.xlarge`. Use `--type` or `--phala-instance-type` for an exact
Phala instance shape.

Provider flags:

```text
--phala-cli
--phala-instance-type
--phala-node-id
--phala-work-root
--phala-compose
```

`--phala-node-id` pins deployments (and ownership scope) to a specific Phala
node. `--phala-compose` deploys an optional Docker Compose file alongside the
dev OS image. The CLI path, node id, and compose path are accepted only from
trusted user config, environment variables, or explicit flags, not
repository-local config. The instance type and work root may also come from
repository config. Instance-type OS prefixes must be Linux.

## Lifecycle

- Linux only; coordinator disabled; confidential TDX CVMs.
- `phala deploy --dev-os --ssh-pubkey ... -t <type> -n <name> --wait` provisions
  the CVM. `--dev-os` selects the dstack dev OS image, which runs `sshd` and
  accepts the injected key.
- Ownership is carried by the CVM **name** (`crabbox-<lease-id>`) and
  cross-checked against the local lease claim. Phala's deploy API exposes no
  arbitrary label facility, so the name prefix is the only on-resource owner
  marker; `list`/`cleanup` therefore never touch resources that lack the prefix
  and a matching local claim.
- `cleanup` and `stop` use `phala cvms delete --cvm-id <id> --force`.
- `--keep` keeps the CVM after the current command.

Because Phala has no extend/touch primitive, lease lifetime is enforced entirely
by Crabbox's idle timeout and the cleanup sweep rather than a provider-side
duration deadline.

## Attestation (deferred)

Phala publishes a TDX attestation quote per CVM (`phala cvms attestation`).
Crabbox does not yet model confidential attestation as a Feature, so this
provider does **not** advertise or verify the quote. Treat the TEE guarantee as
best-effort until a dedicated attestation Feature lands; the deferred hook is the
single follow-up needed to make this a fully verifiable confidential provider.

See the [Phala Cloud CLI reference](https://docs.phala.com/phala-cloud/references/phala-cli).
