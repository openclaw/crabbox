# Parallels Provider

Read this when you:

- choose `provider: parallels`;
- run Crabbox on local or remote Parallels Desktop VMs;
- clone Linux, macOS, or Windows templates from known-good snapshots;
- change `internal/providers/parallels` or Parallels checkpoint behavior.

Parallels is a direct SSH-lease provider (it never goes through the broker). To
acquire a box, Crabbox asks `prlctl` to create a clone from a configured source
VM and snapshot, starts the clone, discovers the guest IP, injects the per-lease
SSH key, and then uses the normal Crabbox SSH sync/run/checkpoint path. Parallels
Tools is the default discovery and key-install path. macOS guests can opt into a
host-side DHCP/SSH fallback when Parallels Tools guest execution is unavailable.

The provider is local-first: by default it drives the `prlctl` on the same Mac
that runs Crabbox. Set `parallels.host` to drive a Parallels Desktop install on
another Mac over SSH.

**Targets:** Linux, macOS, and Windows (`--windows-mode normal` or
`--windows-mode wsl2`).

**Capabilities:** SSH, Crabbox sync, cleanup, desktop, browser, code, plus
native checkpoint/fork/restore/snapshot support backed by Parallels snapshots.

## Quick start

The recommended operator UX is a named template alias: a template points at a
source VM plus a known-good snapshot, so day-to-day commands run the normal
Crabbox flows without juggling source/snapshot flags.

```sh
crabbox warmup \
  --provider parallels \
  --target macos \
  --parallels-source "macOS Tahoe" \
  --parallels-source-snapshot fresh \
  --parallels-user alice \
  --ssh-port 22

crabbox run --provider parallels --id blue-lobster -- xcodebuild -version
crabbox checkpoint create --provider parallels --id blue-lobster --mode native --name xcode-ready
crabbox checkpoint fork chk_abc123 --provider parallels
crabbox stop --provider parallels blue-lobster
```

With a template alias:

```sh
crabbox checkpoint list --provider parallels --parallels-template tahoe-latest
crabbox checkpoint fork --provider parallels --parallels-template tahoe-latest --slug tahoe-test
crabbox run --provider parallels --parallels-template tahoe-latest -- xcodebuild -version
```

Use `--target linux`, `--target macos`, or `--target windows`. For Windows,
choose `--windows-mode normal` for PowerShell/OpenSSH or `--windows-mode wsl2`
when the template already exposes a working WSL2 environment through Windows
OpenSSH.

## Template requirements

Each source VM should already include:

- Parallels Tools (used to discover the guest IP and inject the per-lease key);
- a stable guest SSH user;
- an OpenSSH server listening on `ssh.port`;
- Crabbox sync tools for the target OS (`git`, `rsync` or archive sync tools,
  and a shell/PowerShell);
- a known-good power-off snapshot for fast linked clones.

Linked clones require an explicit power-off snapshot. Crabbox rejects linked
clone requests without `parallels.sourceSnapshot`/`sourceSnapshotId`, because
otherwise `prlctl` would create a source-side "Snapshot for linked clone" on the
template VM. Use `cloneMode: full` or `cloneMode: unlink` only when you
intentionally want to clone the current source VM state without a snapshot.
Parallels also refuses to clone from a busy source VM, so keep template VMs shut
down when they serve as local fleet bases.

For macOS templates, use a user with SSH login permission and a writable
`parallels.workRoot`, for example `/Users/<user>/crabbox`. For Windows native
templates, configure OpenSSH Server and PowerShell. For Windows WSL2 templates,
make sure `wsl.exe` works for the SSH user.

### macOS desktop credentials

For `--desktop` leases, configure the real macOS guest account password with
`CRABBOX_PARALLELS_PASSWORD` or `parallels.password` in the mode-0600 user
configuration printed by `crabbox config path`. Crabbox uses that credential
locally for Apple/ARD Screen Sharing authentication. It is never written to the
guest, passed on the command line, or used to reset the macOS account password.
There is intentionally no password flag.

The Screen Sharing username is the resolved lease SSH user, normally
`parallels.user`. Automatic login is optional image configuration: it lets a
desktop lease reach Finder without operator input, while the account password
continues to protect Screen Sharing. Do not put `parallels.password` in
repository `crabbox.yaml` or `.crabbox.yaml`; repository configuration is not
trusted to select this credential.

When no account password is configured, the existing generated legacy VNC
credential remains available for backward compatibility.

### macOS without Parallels Tools guest execution

Some Apple-silicon macOS guests can boot normally while `prlctl` reports no IP
and rejects guest execution. Set `parallels.bootstrapKey` to enable the macOS-only
fallback. Its value is an absolute private-key path **on the Parallels host**.
The source guest must already authorize that identity for `parallels.user`, and
that user must have non-interactive `sudo` permission for the one-time guest
preparation script.

When the normal Tools IP is absent, Crabbox reads
`/Library/Preferences/Parallels/parallels_dhcp_leases` on the same Parallels
host, matches an unexpired lease against the clone's exact normalized NIC MAC,
and confirms that the configured SSH port is reachable. Missing, expired,
malformed, or ambiguous matches fail closed. Crabbox then SSHes from that host
with the bootstrap identity, streams the normal preparation script on stdin,
and adds the generated per-lease key. All later sync/run traffic uses only the
per-lease key.

`bootstrapKey` is accepted only from trusted user configuration or from the
explicit `--parallels-bootstrap-key` / `CRABBOX_PARALLELS_BOOTSTRAP_KEY`
overrides. Repository configuration cannot select a host-side bootstrap
identity. The fallback does not apply to Linux or Windows guests.

## Configuration

```yaml
provider: parallels
target: macos
ssh:
  port: "22"
parallels:
  source: macOS Tahoe
  sourceSnapshot: fresh
  cloneMode: linked
  bootstrapKey: /Users/alice/.ssh/crabbox-bootstrap
  user: alice
  workRoot: /Users/alice/crabbox
  startupTimeout: 15m
```

Set `parallels.vmRoot` or `--parallels-vm-root` to an existing parent directory
on the Parallels host. Crabbox passes this directory to `prlctl clone --dst`;
Parallels names and creates the VM bundle beneath it. With a remote host, the
directory must exist on that Mac. Crabbox does not create the parent directory.
Leave the setting unset to use the Parallels default location.

### Named templates

```yaml
provider: parallels
parallels:
  templates:
    tahoe-latest:
      target: macos
      source: macOS Tahoe
      sourceSnapshot: macOS 26.3.1 LATEST
      user: alice
      workRoot: /Users/alice/crabbox
    ubuntu-fast:
      target: linux
      source: Ubuntu 25.10
      sourceSnapshot: fresh-poweroff-2026-03-17
      user: alice
      workRoot: /work/crabbox
```

A template may set `source`, `sourceId`, `sourceSnapshot`, `sourceSnapshotId`,
`target`, `windowsMode`, `cloneMode`, `host`, `hostUser`, `hostKey`, `vmRoot`,
`user`, and `workRoot`. Explicit command-line flags override the selected
template.

### Remote Mac host

```yaml
provider: parallels
target: linux
parallels:
  host: mac-studio.tailnet
  hostUser: crabbox
  hostKey: ~/.ssh/id_ed25519
  source: Ubuntu 25.10
  sourceSnapshot: fresh-poweroff
  user: crabbox
  workRoot: /work/crabbox
```

When `parallels.host` is set, Crabbox runs `prlctl` over SSH on that Mac and
reaches the guest IP through an SSH `ProxyCommand` via the host. Normal SSH
commands, desktop input helpers, screenshots, and VNC tunnels all use the same
proxy path.

A repository-defined remote host, including one selected through `templates`
or `hosts`, cannot silently inherit a host key or ambient SSH authentication
from a more trusted source. Define the remote host and a relative,
symlink-resolved key file contained by the repository together, or approve the
destination explicitly with `--parallels-host` or `CRABBOX_PARALLELS_HOST`.
Absolute, missing, and repository-escaping key paths require explicit host
approval.

### Fleet hosts

```yaml
provider: parallels
parallels:
  hosts:
    - name: local
      targets: [linux, macos, windows]
      maxVMs: 4
    - name: mac-host
      host: mac-host.example.net
      user: alice
      targets: [linux, macos]
      maxVMs: 3
  templates:
    ubuntu-fast:
      target: linux
      source: Ubuntu 25.10
      sourceSnapshot: fresh-poweroff-2026-03-17
      user: alice
```

When `hosts` is configured, Crabbox checks each host whose `targets` match the
requested target, looks for the requested source VM, and picks the first host
below its `maxVMs` limit. Host selection applies to `warmup`, `run`,
`checkpoint fork`, `status`, `list`, `stop`, and `cleanup`.

A host with a `maxVMs` limit counts its live VMs and clones into it under one
reservation, so concurrent fan-out such as `crabbox shard --count 8` cannot
exceed the limit: forks that arrive when the host is full fail with exit 5 and
`host <name> is at maxVMs capacity` instead of cloning. Clones against a limited
host are therefore serialized against each other, which adds the clone time of
the forks ahead in the queue. A host with no `maxVMs` has no limit to enforce
and its forks stay fully parallel.

The reservation is a local file lock shared by callers using the same Crabbox
state directory and exact configured host/account (with surrounding whitespace
trimmed). Display names and SSH key paths do not change the reservation identity;
all local entries share one identity. SSH/DNS aliases are not resolved, so use the
same host/account spelling for callers that must coordinate. Different state
directories or machines driving the same Parallels host still race against each
other. Advisory queries such as `doctor` and `checkpoint fork --dry-run` neither
take a reservation nor write capacity-lock state.

### Environment variables

```text
CRABBOX_PARALLELS_SOURCE
CRABBOX_PARALLELS_SOURCE_ID
CRABBOX_PARALLELS_SOURCE_SNAPSHOT
CRABBOX_PARALLELS_SOURCE_SNAPSHOT_ID
CRABBOX_PARALLELS_TEMPLATE
CRABBOX_PARALLELS_CLONE_MODE
CRABBOX_PARALLELS_HOST
CRABBOX_PARALLELS_HOST_USER
CRABBOX_PARALLELS_HOST_KEY
CRABBOX_PARALLELS_BOOTSTRAP_KEY
CRABBOX_PARALLELS_VM_ROOT
CRABBOX_PARALLELS_USER
CRABBOX_PARALLELS_PASSWORD
CRABBOX_PARALLELS_WORK_ROOT
CRABBOX_PARALLELS_STARTUP_TIMEOUT
```

Provider flags mirror the same fields (`--parallels-source`,
`--parallels-source-snapshot`, `--parallels-template`, `--parallels-host`, and
so on) and never carry passwords.

## Checkpoints

Native Parallels checkpoints are backed by Parallels snapshots:

```sh
crabbox checkpoint list --provider parallels --id "macOS Tahoe"
crabbox checkpoint list --provider parallels --id "macOS Tahoe" --forkable-only
crabbox checkpoint list --provider parallels --parallels-template tahoe-latest --current
crabbox checkpoint create --provider parallels --id blue-lobster --mode native --name after-xcode-setup
crabbox checkpoint fork chk_abc123 --provider parallels --slug test-a
crabbox checkpoint restore chk_abc123 --provider parallels --id blue-lobster
crabbox checkpoint delete chk_abc123
```

Existing Parallels snapshots do not need to be imported; reference them directly
by source VM and snapshot name:

```sh
crabbox checkpoint fork --provider parallels --target macos --id "macOS Tahoe" --snapshot "macOS 26.4" --slug tahoe-test
crabbox checkpoint fork --provider parallels --parallels-template ubuntu-fast --dry-run
crabbox checkpoint restore --provider parallels --id "macOS Tahoe" --snapshot "macOS 26.3.1 LATEST"
crabbox checkpoint restore --provider parallels --id blue-lobster --snapshot "known-good" --dry-run
crabbox checkpoint delete --provider parallels --id blue-lobster --snapshot "crabbox-test-snap"
```

- `fork` creates a linked clone from the recorded source VM and snapshot.
- `restore` switches an existing Parallels lease back to the recorded snapshot.
- `delete` removes only the recorded snapshot, not the source VM. Direct
  snapshot delete refuses names that do not start with `crabbox-` unless `--yes`
  is supplied, because known-good snapshots are usually hand-managed template
  state.

Linked clones depend on the source VM and snapshot. Keep known-good template VMs
and their base snapshots while any checkpoint or clone depends on them.

## Fixed lease IDs

An external orchestrator can name the lease itself so a repeated dispatch stays
safe to replay:

```sh
crabbox warmup --provider parallels --lease-id cbx_abcdef123456 --parallels-template ubuntu-fast
```

The idempotency key is the VM name. Crabbox derives a host-unique
`crabbox-<lease-id>-<slug>` name from the requested ID and records it, with the
attested host connection identity, the immutable source VM UUID, and a hash of
the create identity, in the durable lease claim **before** it runs
`prlctl clone`. `prlctl` refuses a second VM with that name on the same host, so
a concurrent duplicate create is rejected by Parallels itself even when a clone
reply is lost.

A VM name is host-unique but reusable over time, so it identifies a slot rather
than a resource. Ownership is therefore bound to two provider-issued identities
that configuration cannot forge:

- **The host.** The provider scope is a digest of the Parallels service's own
  server and hardware identifiers, read from `prlsrvctl info`. A fleet entry
  that keeps its `name:` while its `host:` or `user:` is repointed at a
  different machine reaches a different service and is refused. Only attested
  values enter the scope — not the entry's name, its configured address, or the
  account used to reach it — so the same machine answering at a new address
  still owns its leases and can still be stopped, and the hashed scope keeps
  host identifiers out of claims, labels, and error messages. The `host` label
  on a lease is an operator-facing display name and is deliberately not an
  ownership check.
- **The VM incarnation.** `prlctl clone` reports no UUID, and the name it is
  given is mutable, so reading a VM back by name cannot say which incarnation
  the clone produced. Crabbox instead clones into a per-lease directory named
  with a secret nonce, recorded in the claim before the clone runs, and passes
  it as `--dst`. A bundle inside that directory can only have come from this
  attempt's own clone. The VM found there supplies the UUID, and every later
  adoption, guest mutation, and deletion requires the observed VM to carry that
  UUID *and* still live in that directory. The directory is removed after
  release, and only while empty.

The reconciliation contract:

- **Replay.** An identical request returns the same live lease. Crabbox adopts
  only the VM at the exact recorded name, and only after re-confirming that its
  UUID is the incarnation this attempt's clone produced, that its name carries
  the lease, and that the host connection still attests the recorded identity.
- **Drift.** A different source VM, source snapshot ID, clone mode, target OS,
  Windows mode, guest user, work root, VM root, or checkpoint ID is a different
  create identity and fails `lease_id_conflict` instead of provisioning. The
  source is resolved to its immutable UUID before it enters the fingerprint and
  before it is submitted to `prlctl clone`, so replacing a template VM under the
  same name is drift rather than a silent provision from a different template.
- **Host scope.** A fixed lease never re-runs fleet selection. It reconciles
  against the host recorded in its intent. A fleet whose hosts cannot be
  attested keeps custody instead of rebinding; a fleet in which no host attests
  the recorded identity fails `lease_id_conflict`.
- **Absence.** Only a complete inventory listing proves a VM is gone, and only
  the bound UUID being absent from it. A failed `prlctl list` keeps custody
  rather than cloning a second VM; an acquired VM that has been renamed to
  another `crabbox-<lease-id>-<slug>` is found by its UUID and keeps custody
  rather than being reported as deleted; and an acquired lease whose VM has
  disappeared fails closed instead of recreating it.
- **Lost replies.** A lost clone reply leaves the VM, if it was created, inside
  the attempt's own directory, so replay recovers it there rather than cloning a
  second one — even though its UUID was never reported. If instead a VM that
  this attempt did not create occupies the recorded name, `warmup` and `stop`
  both report `lease_id_conflict` and name it: nothing is started, no per-lease
  key is installed, and nothing is deleted. The name alone cannot prove which
  incarnation occupies it, and acting on it would hand credentials and deletion
  authority to whatever is there.
- **Failure.** A failed fixed acquisition keeps its claim, its recorded attempt,
  and its per-lease key. Retry the same lease ID, or stop it. Crabbox does not
  roll the VM back, because that would make a lost reply indistinguishable from
  a plain failure.
- **Release.** `stop` resolves a fixed lease from its durable claim alone. It
  reads no guest and builds no SSH target before the recorded host and VM
  incarnation have been re-attested, so a VM occupying the recorded name is
  never reachable with the lease's credentials. It then deletes only the
  recorded VM, and keeps a terminal tombstone: the ID, slug, connection scope,
  intent hash, timestamps, and terminal state. Stopping a fixed lease whose VM
  is already gone finalizes that tombstone, and stopping an already terminal
  lease is an idempotent no-op — both through `crabbox stop`, not only through
  the provider API. Replaying a released ID never creates another VM. Automatic
  cleanup never prunes tombstones, and there is no reuse window — deleting local
  claim state forfeits the protection, so automation must mint a new ID instead.

No path uses the slug to decide replay ownership. A `crabbox-<slug>` VM, or
another lease's VM carrying the same slug, is never adopted.

A `prepared` intent whose VM was never observed is deliberately inconclusive:
the clone may still be in flight, so `stop` refuses it. Replay the same lease ID
first, then stop the lease it reconciles to.

A fixed lease's VM bundle lives at
`<vmRoot or the host's VM directory>/crabbox-<lease-id>-<slug>-<nonce>/`, one
directory per lease, rather than directly in the VM directory. Ordinary
generated-ID warmup is unaffected.

Fixed Parallels claims are stored under the downgrade-safe
`parallels-fixed-v1` provider marker. Current clients map it back to the
`parallels` runtime provider, so ownership checks, resolution, and cleanup keep
routing it; a released client that does not know the marker cannot mistake the
claim for an ordinary Parallels lease and therefore cannot delete its VM or
prune its tombstone.

## Safety

Crabbox refuses to delete a Parallels VM unless an exact local claim binds the
lease to the VM ID and selected Parallels host. A `crabbox-` name alone is not
ownership proof. `stop` and `cleanup` skip unclaimed or mismatched clones;
intentionally recovered clones must first be adopted through an explicit
`--reclaim` reuse.

Use `--dry-run` on direct fork, restore, and delete when validating a template
or snapshot name. `checkpoint list` prints live Parallels state and marks
whether each snapshot is forkable: power-on snapshots can be restored in place,
while linked-clone forks require a power-off snapshot.

## Related docs

- [Provider overview](README.md)
- [Checkpoints](../features/checkpoints.md)
- [Static SSH](ssh.md)
