# XCP-ng Provider

Read this when you:

- choose `provider: xcp-ng`;
- set up an XCP-ng VM template for Crabbox;
- debug XAPI credentials, config-drive cloud-init, guest-tools IP discovery, or
  cleanup;
- change `internal/providers/xcpng`.

Crabbox's `xcp-ng` provider is a direct SSH-lease provider for Linux VMs on a
self-hosted XCP-ng pool. For each lease Crabbox talks directly to the XAPI
endpoint, copies the configured template to the selected SR with `VM.copy`,
calls `VM.provision`, attaches a temporary FAT16 `CIDATA` config-drive image
with per-lease cloud-init SSH access, waits for XCP-ng guest metrics to report
the VM IPv4 address, runs the Crabbox Linux bootstrap over SSH, and then uses
the normal SSH sync/run/release path.

The provider is direct-only: it talks to XCP-ng straight from the CLI. Xen
Orchestra is not required, and the Crabbox coordinator does not provision or
broker XCP-ng capacity. XCP-ng supports the `ssh`, `crabbox-sync`, and
`cleanup` features on `target=linux` only.

Crabbox expects a self-hosted XCP-ng pool on dedicated 64-bit x86 server-class
hardware for VM workloads. XCP-ng itself can host many guest OS families,
including Linux, Windows, and BSD. Crabbox's current `xcp-ng` adapter is
narrower: the normal `doctor` / `warmup` / `run` / `ssh` / `stop` / `cleanup`
lease surface provisions Linux templates only. The separate ISO E2E harness
documented below extends validation coverage to fresh Linux installers and
Windows x86_64/x64 installer media. macOS guests are out of scope on this
path; use the Tart provider on Apple hardware when you need macOS VM
workflows.

## When to use

Use XCP-ng when:

- your test capacity lives on a private XCP-ng pool on dedicated 64-bit x86
  server-class hardware;
- you want reusable lab infrastructure instead of public-cloud VMs;
- your template already supports cloud-init config drives and XCP-ng guest
  tools.

Use [AWS](aws.md), [Azure](azure.md), [Google Cloud](gcp.md), or
[Hetzner](hetzner.md) when you need brokered, cost-capped shared-team leases.
Use the [Static SSH](ssh.md) provider when you already have a running host and
do not want Crabbox to clone or delete VMs. Use the Tart provider on Apple
hardware when you need macOS guest virtualization.

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

1. copies `xcpNg.template` or `xcpNg.templateUuid` to the configured SR with
   `VM.copy`;
2. resolves optional network and host placement, then writes Crabbox lease
   labels on the copied VM;
3. if `xcpNg.network` or `networkUuid` is set, moves all VIFs on the copied VM
   to that network;
4. calls `VM.provision`;
5. builds a per-lease FAT16 `CIDATA` config-drive image containing cloud-init
   user-data and metadata;
6. attaches the config drive to the VM;
7. starts the VM;
8. waits for guest metrics to report an IPv4 address;
9. runs the normal Linux SSH bootstrap.

If the guest tools never report an IPv4 address, or SSH does not come up so the
bootstrap can run, provisioning fails and Crabbox attempts to delete both the
VM and its config drive.

Because `VM.copy` creates full disk copies on the selected SR, template disk
size and SR placement directly affect warmup and run latency. Crabbox is
happiest with a single-NIC template. If you need a custom multi-NIC topology,
leave `xcpNg.network` and `networkUuid` unset so Crabbox preserves the
template's existing network layout.

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

Keep `CRABBOX_XCP_NG_API_URL` on an administrator-only management network or
VPN, and prefer trusted certificates. Set `CRABBOX_XCP_NG_INSECURE_TLS=1` or
pass `--xcp-ng-insecure-tls` only on a trusted management network in a fully
trusted private lab. Keep TLS verification enabled, as it is by default, in all
other environments. Disabling verification does not permit plain HTTP; the API
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

When `network` or `networkUuid` is set, Crabbox moves all VIFs on the copied VM
to the selected network. Use a single-NIC template for Crabbox-managed VMs, or
leave the setting unset when the template's network topology should be
preserved.

Point `apiUrl` at the pool master on an administrator-only management network
or VPN when possible. Prefer trusted certificates. `insecureTLS` is a
private-lab escape hatch, not a general deployment mode. If `apiUrl` points at
a pool member that returns XAPI `HOST_IS_SLAVE` during login, Crabbox retries
login once against the master address reported by XAPI.

**Security consideration:** Following the master address and re-authenticating
there is required for legitimate pool-master failover. The re-authentication
includes the configured XAPI password. When `--xcp-ng-insecure-tls` is enabled,
an on-path attacker who can tamper with the XAPI response could replace the
reported master address and redirect that password-bearing re-authentication to
a host of their choosing. Keep TLS verification enabled in any environment that
is not a fully trusted private lab, and use insecure TLS only on a trusted
management network. This Low/P3 residual risk is accepted to preserve
pool-master failover.

Repository-local `crabbox.yaml` and `.crabbox.yaml` files cannot override
`apiUrl` or `insecureTLS`, so a checkout cannot redirect inherited credentials.
Configure those connection-trust settings in user config, an explicit
`CRABBOX_CONFIG` file outside the active repository, or environment variables.

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
CRABBOX_XCP_NG_GUEST_CIDR
CRABBOX_XCP_NG_HOST
CRABBOX_XCP_NG_USER
CRABBOX_XCP_NG_WORK_ROOT
CRABBOX_XCP_NG_INSECURE_TLS
```

`CRABBOX_XCP_NG_GUEST_CIDR` enables bounded active MAC discovery for fresh
guests that do not yet report XAPI guest metrics. It must be an IPv4 `/24` or
narrower range attached to the local runner. Without it, Crabbox may use an
existing neighbor-table entry but does not sweep local subnets.

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
3. Copy the template to the selected SR with `VM.copy`, label the copied VM as
   Crabbox-managed, and apply optional host affinity.
4. If `xcpNg.network` or `networkUuid` is set, move all VIFs on the copied VM
   to that network.
5. Call `VM.provision` on the copied VM.
6. Generate cloud-init user-data and metadata with the per-lease SSH public key.
7. Build and attach a FAT16 `CIDATA` config-drive image on the configured
   storage repository.
8. Start the VM and wait for XCP-ng guest metrics to report a non-loopback IPv4
   address.
9. Wait for SSH, then run the Crabbox Linux bootstrap.
10. Sync the checkout and run commands over SSH.
11. Touch lease labels during runs; on release, delete the config drive and VM
    unless the lease is kept.

This is full-copy provisioning, not a cheap CoW clone path, so SR throughput
and template disk size are part of the user-visible provisioning contract.

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

The read-only path can also be selected through the guarded top-level provider
matrix:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=xcp-ng CRABBOX_LIVE_COORDINATOR=0 CRABBOX_BIN=./bin/crabbox scripts/live-smoke.sh
```

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

## Guarded ISO E2E harness

The repo also includes a separate guarded harness for fresh-installer ISO
validation:

```sh
scripts/xcpng-iso-e2e-smoke.sh --read-only --os linux --iso ~/Desktop/xcp/ISOs/ubuntu-26.04-live-server-amd64.iso
scripts/xcpng-iso-e2e-smoke.sh --read-only --os windows --iso ~/Desktop/xcp/ISOs/Win11_25H2_English_x64_v2.iso
```

This harness is intentionally separate from `scripts/xcpng-live-smoke.sh`.
`xcpng-live-smoke.sh` preserves the normal Linux template-clone contract.
`xcpng-iso-e2e-smoke.sh` is a quarantined acceptance helper for fresh blank VM
plus installer media lifecycle. It extends validation coverage for fresh Linux and
Windows x86_64/x64 installs on XCP-ng itself, but it does not change the normal
`provider: xcp-ng` lease contract, which remains Linux-only.

Read-only mode:

- loads the same private env file pattern as `xcpng-live-smoke.sh`;
- validates XCP-ng auth plus placement prerequisites needed for ISO boot;
- resolves the installer ISO as a local file path, VDI name, UUID, or
  `OpaqueRef`;
- writes redacted local evidence under `.crabbox/xcpng-iso-e2e/`; and
- creates, changes, and deletes no XCP-ng resources.

Mutating mode requires an explicit gate:

```sh
CRABBOX_XCP_NG_ISO_E2E_MUTATE=1 scripts/xcpng-iso-e2e-smoke.sh --mutate --os linux --iso ~/Desktop/xcp/ISOs/ubuntu-26.04-live-server-amd64.iso
```

Fresh ISO VMs do not inherit template VIFs, so mutating ISO runs also require
`xcpNg.network` or `xcpNg.networkUuid`. These fields remain optional for normal
template-based leases, where leaving them unset preserves the template network.

Linux mutating mode now performs the guarded Ubuntu Server acceptance flow:

- generates a per-run NoCloud seed ISO with Ubuntu autoinstall user-data;
- requires a local Ubuntu Server ISO path and remasters it under
  `.crabbox/xcpng-iso-e2e/` to add the
  required `autoinstall` kernel argument;
- imports temporary installer and answer media into the configured SR when local
  file paths are used;
- creates a fresh blank VM plus writable install disk, boots the installer,
  waits for the installed guest to reboot, and then waits for SSH on first boot;
- proves first boot by running `printf linux-iso-e2e-ok` over SSH; and
- removes the VM, install disk, and temporary imported media unless cleanup is
  blocked by the environment.

The wrapper now defaults to `--timeout 90m`, which is intended for the full
Linux install flow. Override it explicitly only when the lab is known to be
faster or slower.

Windows mutating mode now extends the shared harness for x86_64/x64 installer
media:

- blocks ARM-labelled Windows installer media with
  `windows_requirements_blocked` because the current XCP-ng ISO E2E lab is
  x86_64/x64;
- generates a temporary `Autounattend.xml` ISO when `--answer-iso` is not
  supplied and includes a Crabbox bootstrap PowerShell script that installs
  OpenSSH with the per-run Crabbox SSH key; the generated answer media selects
  Windows image index `1` by default, and `--answer-iso` remains the override
  path when a different edition or custom answer file is required;
- imports local installer and answer ISO paths into the configured SR when
  needed;
- creates a fresh secure-boot VM plus writable install disk, boots from the
  Windows installer media first, then switches the next boot to the installed
  disk; the fresh VM uses a 64 GiB install disk, and Windows 11-labelled media
  also gets a unique vTPM for the Windows 11 hardware requirements (requiring
  XCP-ng 8.3 or newer);
- waits for first-boot guest metrics and then attempts SSH proof with
  `Write-Output windows-iso-e2e-ok`; and
- falls back to `source_uncovered` after first-boot guest metrics when the
  supplied answer media does not guarantee Crabbox SSH bootstrap.

Current classifications:

- `read_only_passed`: config and ISO references resolved without mutation.
- `linux_install_passed`: the Linux installer completed, first boot reported a
  guest IPv4, and SSH proof succeeded.
- `windows_install_passed`: the Windows installer completed, first boot
  reported a guest IPv4, and SSH proof succeeded.
- `windows_requirements_blocked`: the selected Windows installer media or lab
  prerequisites do not match the current x86_64/x64 XCP-ng validation surface.
- `source_uncovered`: the Windows guest reached first boot and reported guest
  metrics, but only guest-metrics readiness was available; Crabbox command
  proof remains uncovered for that answer-media path.
- `environment_blocked`: lab prerequisites, ISO identity, auth, placement, or
  mutation gate blocked the run. For Linux this also includes exact phase
  blockers such as installer boot arg/remaster failure, guest-metrics timeout,
  or SSH readiness failure. For Windows this also includes answer-media
  generation, installer-media import, guest-metrics timeout, and SSH readiness
  failures after the bootstrap path is enabled.
- `resource_cleanup_failed`: the shared lifecycle reached its target phase but
  cleanup left resources behind.
- `test_failed`: local harness contract failure such as invalid arguments or
  malformed helper output.

Linux mutating summaries use these phase values:

- `linux_seed_generation`
- `linux_install_disk`
- `linux_iso_attached`
- `linux_installer_booted`
- `linux_install_complete`
- `linux_first_boot`
- `linux_ssh_ok`

Windows mutating summaries use these phase values:

- `windows_answer_generation`
- `windows_install_disk`
- `windows_iso_attached`
- `windows_setup_started`
- `windows_install_complete`
- `windows_first_boot`
- `windows_command_ok`

Every run writes a JSON summary with these stable keys:

- `classification`
- `mutation`
- `os`
- `iso`
- `phase`
- `cleanup`
- `evidence`

Linux mutating mode requires a local Ubuntu Server ISO path and handles the
temporary remaster/import steps itself. Existing SR-hosted Ubuntu installer ISOs
are read-only validation inputs only unless they were already prepared with the
required `autoinstall` boot argument outside this harness. Keep all
`.crabbox/xcpng-iso-e2e/` artifacts local; do not commit them.

Windows mutating mode expects x86_64/x64 installer media. `ISOs-ARM` style
Windows media is a valid reference set for other local virtualization surfaces,
but this XCP-ng harness treats ARM-labelled Windows installers as
`windows_requirements_blocked`.

## Troubleshooting

`xcp-ng configuration is incomplete`

Set the missing `xcpNg.*` config keys or `CRABBOX_XCP_NG_*` variables. The
password belongs in private config or env, never in a command-line flag.

`xcp-ng api URL must use https`

Use an HTTPS XAPI endpoint on a private management network or VPN. Prefer
trusted certificates. `insecureTLS` permits self-signed certificates but should
stay limited to private lab use; it does not allow plain HTTP.

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
