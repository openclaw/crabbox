# XCP-ng Provider

Read this when you:

- choose `provider: xcp-ng`;
- set up an XCP-ng VM template for Crabbox;
- debug XAPI credentials, config-drive cloud-init, guest-tools IP discovery, or
  cleanup;
- change `internal/providers/xcpng`.

XCP-ng is a direct SSH-lease provider for Linux VMs on a self-hosted XCP-ng
pool. For each lease Crabbox talks directly to the XAPI endpoint, clones a
configured template, attaches a temporary config-drive ISO with per-lease
cloud-init SSH access, waits for XCP-ng guest metrics to report the VM IPv4
address, runs the Crabbox Linux bootstrap over SSH, and then uses the normal
SSH sync/run/release path.

The provider is direct-only: it talks to XCP-ng straight from the CLI. Xen
Orchestra is not required, and the Crabbox coordinator does not provision or
broker XCP-ng capacity. XCP-ng supports the `ssh`, `crabbox-sync`, and
`cleanup` features on `target=linux` only.

## When to use

Use XCP-ng when:

- your test capacity lives on a private XCP-ng pool;
- you want reusable lab infrastructure instead of public-cloud VMs;
- your template already supports cloud-init config drives and XCP-ng guest
  tools.

Use [AWS](aws.md), [Azure](azure.md), [Google Cloud](gcp.md), or
[Hetzner](hetzner.md) when you need brokered, cost-capped shared-team leases.
Use the [Static SSH](ssh.md) provider when you already have a running host and
do not want Crabbox to clone or delete VMs.

Provider name:

```text
xcp-ng
```

There are no aliases.

## Template requirements

The configured template must be a Linux VM template with:

- OpenSSH installed and enabled;
- cloud-init enabled with a NoCloud/config-drive datasource;
- XCP-ng guest tools installed and reporting guest metrics;
- DHCP networking, or an equivalent network config that lets guest metrics
  report a non-loopback IPv4 address;
- outbound package access for the Crabbox bootstrap packages;
- a cloud-init user with passwordless `sudo`.

For each lease Crabbox:

1. clones `xcpNg.template` or `xcpNg.templateUuid`;
2. resolves the storage repository and optional network/host placement;
3. writes Crabbox lease labels on the VM;
4. builds a per-lease config-drive ISO containing cloud-init user-data and
   metadata;
5. attaches the config drive to the clone;
6. starts the VM;
7. waits for guest metrics to report an IPv4 address;
8. runs the normal Linux SSH bootstrap.

If the guest tools never report an IPv4 address, or SSH does not come up so the
bootstrap can run, provisioning fails and Crabbox attempts to delete both the
clone and its config drive.

## Quick start

Store credentials in private config or environment variables, then run a
non-mutating doctor check before creating a VM:

```sh
export CRABBOX_XCP_NG_API_URL=https://xcp-pool.example.test
export CRABBOX_XCP_NG_USERNAME=crabbox@example.test
export CRABBOX_XCP_NG_PASSWORD='<api-password>'
export CRABBOX_XCP_NG_SR=default-sr
export CRABBOX_XCP_NG_TEMPLATE=crabbox-ubuntu-2404
export CRABBOX_XCP_NG_NETWORK=pool-network

crabbox doctor --provider xcp-ng --json
crabbox warmup --provider xcp-ng --keep --slug xcp-ng-smoke
crabbox run --provider xcp-ng --id xcp-ng-smoke --no-sync -- echo xcp-ng-ok
crabbox ssh --provider xcp-ng --id xcp-ng-smoke
crabbox stop --provider xcp-ng xcp-ng-smoke
crabbox cleanup --provider xcp-ng --dry-run
```

For self-signed private pools, set `CRABBOX_XCP_NG_INSECURE_TLS=1` or pass
`--xcp-ng-insecure-tls`. That only disables certificate verification; the API
URL must still use HTTPS.

## Configuration

```yaml
provider: xcp-ng
target: linux
xcpNg:
  apiUrl: https://xcp-pool.example.test
  username: crabbox@example.test
  password: <api-password>
  template: crabbox-ubuntu-2404
  templateUuid: ""
  sr: default-sr
  srUuid: ""
  network: pool-network
  networkUuid: ""
  host: ""
  user: crabbox
  workRoot: /work/crabbox
  insecureTLS: false
```

`apiUrl`, `username`, `password`, and either `sr` or `srUuid` are required for
doctor and lifecycle commands. A template name or UUID is required before
`warmup` or `run` can create a lease. `network`, `networkUuid`, and `host` are
optional placement hints. `user` becomes the cloud-init SSH user, and
`workRoot` becomes the remote workspace root.

Keep the password in a private config file with `0600` permissions, in an
environment variable, or in a secret manager. Do not pass it on argv. Crabbox
intentionally has no XCP-ng password command-line flag, so the password does not
appear in shell history or process listings.

Environment variables:

```text
CRABBOX_XCP_NG_API_URL
CRABBOX_XCP_NG_USERNAME
CRABBOX_XCP_NG_PASSWORD
CRABBOX_XCP_NG_TEMPLATE
CRABBOX_XCP_NG_TEMPLATE_UUID
CRABBOX_XCP_NG_SR
CRABBOX_XCP_NG_SR_UUID
CRABBOX_XCP_NG_NETWORK
CRABBOX_XCP_NG_NETWORK_UUID
CRABBOX_XCP_NG_HOST
CRABBOX_XCP_NG_USER
CRABBOX_XCP_NG_WORK_ROOT
CRABBOX_XCP_NG_INSECURE_TLS
```

Provider flags mirror the non-secret config fields:

```text
--xcp-ng-api-url
--xcp-ng-username
--xcp-ng-template
--xcp-ng-template-uuid
--xcp-ng-sr
--xcp-ng-sr-uuid
--xcp-ng-network
--xcp-ng-network-uuid
--xcp-ng-host
--xcp-ng-user
--xcp-ng-work-root
--xcp-ng-insecure-tls
```

## Doctor

`crabbox doctor --provider xcp-ng --json` is non-mutating. It validates local
tools, checks required XCP-ng config, opens a XAPI session, lists
Crabbox-managed leases, and reports `mutation=false`.

When configuration is incomplete, doctor returns a failed provider check with a
message naming the missing config or environment variables. That is the right
classification for CI and local setup probes: `environment_blocked`.

## Lifecycle

1. Allocate a Crabbox lease ID and friendly slug.
2. Resolve the configured template, storage repository, optional network, and
   optional host.
3. Clone the template and label the clone as Crabbox-managed.
4. Generate cloud-init user-data and metadata with the per-lease SSH public key.
5. Build and attach a config-drive ISO on the configured storage repository.
6. Start the VM and wait for XCP-ng guest metrics to report a non-loopback IPv4
   address.
7. Wait for SSH, then run the Crabbox Linux bootstrap.
8. Sync the checkout and run commands over SSH.
9. Touch lease labels during runs; on release, delete the config drive and VM
   unless the lease is kept.

Cleanup lists XCP-ng inventory and only acts on resources labeled as
Crabbox-managed leases. Use `crabbox cleanup --provider xcp-ng --dry-run`
before an actual cleanup sweep.

## Guarded live smoke

The repo includes a guarded smoke helper:

```sh
scripts/xcpng-live-smoke.sh --read-only
```

Read-only mode runs `crabbox doctor --provider xcp-ng --json`, writes redacted
evidence under `.crabbox/xcpng-live-smoke/`, and does not create or delete VMs.

Mutating smoke requires an explicit gate and a configured template:

```sh
CRABBOX_XCP_NG_LIVE_MUTATE=1 scripts/xcpng-live-smoke.sh --mutate
```

The mutating path runs doctor first, creates one kept warmup lease, runs a
minimal command with `--no-sync`, and then stops that exact lease. A shell trap
attempts `crabbox stop --provider xcp-ng <lease>` on failure after a lease ID is
known. If credentials, host access, or template configuration are missing, stop
at the read-only doctor output and classify live validation as
`environment_blocked`; do not claim live provisioning success.

The doctor path is non-mutating: it opens a XAPI session, resolves configured
placement resources, lists Crabbox-managed leases, and reports
`mutation=false`. Template, SR, network, or host typos fail before doctor
reports ready.

## Troubleshooting

`xcp-ng configuration is incomplete`

Set the missing `xcpNg.*` config keys or `CRABBOX_XCP_NG_*` variables. The
password belongs in private config or env, never in a command-line flag.

`xcp-ng api URL must use https`

Use an HTTPS XAPI endpoint. `insecureTLS` permits self-signed certificates but
does not allow plain HTTP.

`xcp-ng template not found by name` / `xcp-ng template name is ambiguous`

Set `xcpNg.templateUuid` or `CRABBOX_XCP_NG_TEMPLATE_UUID` to pin the exact
template, or rename templates so the configured name is unique.

`xcp-ng sr or sr UUID is required`

Set `xcpNg.sr`, `xcpNg.srUuid`, `CRABBOX_XCP_NG_SR`, or
`CRABBOX_XCP_NG_SR_UUID`. The storage repository is needed for both VM
placement and config-drive creation.

`no guest ipv4 address reported by XCP-ng guest metrics`

Confirm XCP-ng guest tools are installed and running in the template, DHCP is
available on the selected network, and the guest reports an IPv4 address after
boot.

## Related docs

- [Configuration](../features/configuration.md)
- [Provider reference](README.md)
- [Doctor](../commands/doctor.md)
- [Warmup](../commands/warmup.md)
- [Run](../commands/run.md)
- [Stop](../commands/stop.md)
- [Cleanup](../commands/cleanup.md)
