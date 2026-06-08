# Incus Local E2E Testbed

Read this when you:

- are validating the planned Crabbox `incus` provider on an Apple Silicon Mac;
- need the local host strategy, Incus access path, or SSH reachability contract
  for that future provider;
- are deciding whether local Incus smoke belongs in repo automation yet.

This page is a local testbed runbook for the future `incus` provider. The
provider is not registered in this branch yet, so the commands here describe the
real environment that `PLAN-01-INCUS-PROVIDER.md` must target rather than a
current built-in `--provider incus` surface.

## Scope And Current Status

- Primary local host route: Tart on Apple Silicon, bootstrapped from
  `~/Desktop/xcp/ISOs-ARM/ubuntu-26.04-desktop-arm64.iso`.
- First acceptable local proof: container-backed Incus instances, not nested
  Incus VMs.
- Required end-to-end contract: Crabbox must be able to reach an Incus-managed
  guest from the Mac over SSH.
- Current state on this branch: local prerequisites are present on the Mac, but
  no host-reachable Incus server remote has been proven from this worktree yet.

Local evidence already verified on this machine:

- `incus version` shows the client is installed, but the local server is still
  unreachable.
- `tart --version` reports `2.32.1`.
- `qemu-system-aarch64 --version` reports `11.0.1`.
- `tart list` shows the historical `ubuntu-26-arm-iso-test` VM shell.
- The canonical ARM installer media exists at
  `~/Desktop/xcp/ISOs-ARM/ubuntu-26.04-desktop-arm64.iso`.

## Why This Route

Tart is the primary path because it already proved the important Apple Silicon
bootstrap step on this Mac: the Ubuntu 26.04 ARM desktop ISO boots to GRUB, the
graphical installer, and the live desktop. That is stronger evidence than the
current UTM gallery baseline.

UTM remains a fallback only. IncusOS documents that the Apple Silicon UTM/QEMU
route cannot provide nested Incus VMs because nested virtualization is not
available there with the required secure-boot and TPM path. That makes a local
container-backed proof the smallest realistic first gate.

Multipass is installed and remains a secondary local Linux-host option, but this
plan keeps Tart plus Ubuntu 26.04 as the preferred route because it matches the
already-tested local evidence.

## Testbed Contract

The future `incus` provider should treat this local testbed as passed only when
all of the following are true:

1. A real Incus daemon is running inside a local Linux environment on this Mac.
2. The Mac-side `incus` client can reach that daemon over the network.
3. At least one Incus-managed guest is reachable from the Mac over SSH.
4. The provider can drive the normal Crabbox lease lifecycle against that guest.

Booting a Linux VM is not enough. Running commands only inside the Linux host is
not enough. Stale `tart ip` output is not enough.

## Local Host Strategy

### Primary route: Tart plus Ubuntu 26.04 ARM Desktop ISO

Use Tart to create a disposable Ubuntu VM shell, install Ubuntu from the local
desktop ISO, then configure Incus inside that Linux guest.

Operator expectations:

- GUI interaction may still be required during the Ubuntu install flow.
- The prior Tart journal proved ISO boot and live desktop entry, but it did not
  prove a durable installed-disk reboot yet.
- Until a reusable installed Ubuntu base is proven, treat the VM bootstrap as a
  local operator runbook, not a repo-owned automation surface.

Suggested bootstrap outline:

```sh
tart create --linux ubuntu-26-arm-iso-testbed
tart run --disk "$HOME/Desktop/xcp/ISOs-ARM/ubuntu-26.04-desktop-arm64.iso:ro" ubuntu-26-arm-iso-testbed
```

After the guest is installed, rerun without the ISO attached and verify the VM
is actually running before trusting any reported guest IP.

### Fallback routes

- UTM/QEMU: fallback only for local Linux hosting experiments. Do not make
  nested Incus VM proof a requirement on this Mac.
- Multipass: acceptable as a secondary Linux-host path if Tart's installed-disk
  story remains unreliable, but not the primary contract for this plan.

## Incus Host Setup

Inside the Ubuntu guest, install and initialize Incus.

Upstream packaging guidance currently supports Ubuntu 24.04 LTS and later with
`apt install incus`, and documents the Zabbly repository for Ubuntu LTS releases
including 26.04. Keep the first local proof simple and prefer the native package
path when it is available in the guest.

Minimal host setup target:

```sh
sudo apt install incus
sudo incus admin init --minimal
```

The minimal setup is acceptable for the first local container-backed smoke even
though it is not the most optimized Incus configuration.

To make the daemon reachable from the Mac, expose the Incus HTTPS API from the
Linux guest:

```sh
sudo incus config set core.https_address :8443
sudo incus config trust add crabbox-mac
```

Then add it from the Mac with either the emitted trust token or a direct remote
add flow:

```sh
incus remote add local-incus-testbed <token>
```

or:

```sh
incus remote add local-incus-testbed <guest-ip-or-hostname>
```

Success here means `incus remote list` shows the remote and a Mac-side command
such as `incus list local-incus-testbed:` reaches the server.

## Guest Reachability Contract

Mac-to-instance SSH is mandatory. The first local proof should start with an
Incus container on a managed bridge rather than a nested VM.

Preferred order:

1. Try direct bridge reachability when the Mac can route to the guest address.
2. If the bridge is not directly reachable from macOS, expose SSH explicitly from
   the Incus host.

Two realistic Incus-supported exposure mechanisms are already documented
upstream.

### Option A: proxy device

Use an Incus `proxy` device to publish a host port to the guest's TCP port 22:

```sh
incus config device add <instance> ssh proxy \
  listen=tcp:0.0.0.0:2222 connect=tcp:0.0.0.0:22
```

This is the simplest first proof when the Linux host itself is already reachable
from the Mac.

### Option B: managed bridge network forward

Use a managed bridge forward when the bridge network owns a reachable listen
address and you want a persistent host-side routing rule:

```sh
incus network forward create incusbr0 <listen-address>
incus network forward port add incusbr0 <listen-address> tcp 2222 <guest-ip> 22
```

Document whichever one you actually use in the local operator notes. Do not use
`macvlan` as the default local testbed answer when host-to-instance communication
is required.

## Manual Local Smoke Contract

This branch does not add an `incus` lane to `scripts/live-smoke.sh` because the
provider is not implemented yet and the local infrastructure path is still too
environment-specific. Keep the smoke manual and explicit for now.

Once the provider exists, the narrow local smoke target is:

```sh
crabbox warmup --provider incus ...
crabbox status --provider incus --id <slug> --wait
crabbox run --provider incus --id <slug> --no-sync -- echo incus-ok
crabbox list --provider incus
crabbox stop --provider incus <slug>
crabbox cleanup --provider incus --dry-run
```

Those commands should only be documented as passed after the Mac reaches a real
Incus-managed guest over SSH. Until then, classify the local path honestly:

- `passed`: the Mac reaches the Incus daemon and a guest over SSH, and the
  future provider smoke succeeds.
- `environment_blocked`: the Mac lacks a real reachable Incus daemon, or the VM
  bootstrap/install path does not yield a usable Linux host.
- `diagnostic_only`: prerequisites and partial reachability checks succeed, but
  the guest SSH contract is still unproven.
- `plan_gap`: the provider surface or required config contract is still missing.

## Current Classification For This Branch

Local smoke classification: `plan_gap`

Infrastructure status: `diagnostic_only`

Why smoke is still `plan_gap`:

- this branch does not yet contain a registered `incus` provider;
- no host-reachable Incus server remote has been proven from this worktree;
- no Mac-to-instance SSH proof has been completed yet.

Why the infrastructure status is `diagnostic_only`:

- host prerequisites are present on the Mac;
- Tart ISO boot evidence exists for Ubuntu 26.04 ARM;
- the preferred ISO path exists locally.

## What PLAN-01 Should Consume

`PLAN-01-INCUS-PROVIDER.md` should implement against these assumptions:

- local Apple Silicon proof may be container-first;
- the local provider contract requires a real Mac-to-guest SSH path;
- `scripts/live-smoke.sh` should not grow an `incus` lane until the adapter is
  implemented and the local route is repeatable enough to maintain;
- provider docs must preserve the distinction between prerequisite detection and
  real local end-to-end proof.
