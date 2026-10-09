# exe.dev Provider

Read this when:

- choosing `provider: exe-dev` (aliases `exe`, `exedev`);
- working on `internal/providers/exedev`;
- debugging exe.dev VM lifecycle or the SSH sync/run path on those VMs.

exe.dev is an SSH-lease provider. Crabbox drives the exe.dev SSH API on a control
host (`exe.dev` by default) to create, list, and delete VMs, then treats each
VM's `ssh_dest` as a normal Linux SSH target for sync, run, status, and
`crabbox ssh`. Provisioning runs direct from the CLI; there is no Crabbox
coordinator support for this provider.

The control host calls (`new`, `cp`, `tag`, `ls`, `rm`, `whoami`) speak the exe.dev
API over SSH. Your own shell commands run on the VM SSH target, not through the
control host.

Control-host authentication uses your ambient SSH configuration or agent. A
repository-defined custom `exeDev.controlHost` therefore requires explicit
operator approval through `--exe-dev-control-host` or
`CRABBOX_EXE_DEV_CONTROL_HOST` before Crabbox opens an SSH connection.

## Quick start

```sh
ssh exe.dev whoami
crabbox warmup --provider exe-dev --slug smoke
crabbox run --provider exe-dev --id smoke -- pnpm test
crabbox ssh --provider exe-dev --id smoke
crabbox stop --provider exe-dev smoke
```

The local `ssh exe.dev ...` login must already work, and VM creation requires an
active exe.dev plan.

## Configuration

```yaml
provider: exe-dev
exeDev:
  controlHost: exe.dev   # default
  image: ""              # exe.dev VM image (default image when empty)
  base: ""               # clone a trusted existing VM instead of new
  cpus: 2                # default
  memory: 4GB            # default
  disk: 10GB             # default
  command: ""            # optional container command
  user: ""               # SSH login user (defaults to ssh_dest / local user)
  workRoot: ""          # inherit a non-default top-level root; otherwise /var/tmp/crabbox
  noEmail: true          # default; suppress exe.dev notification email
```

### Provider flags

```text
--exe-dev-control-host <host>
--exe-dev-image <image>
--exe-dev-from <base-vm>
--exe-dev-cpus <n>
--exe-dev-memory <size>      # e.g. 4GB
--exe-dev-disk <size>        # e.g. 10GB
--exe-dev-command <command>
--exe-dev-user <user>
--exe-dev-work-root <path>
--exe-dev-no-email
```

The generic sizing flags do not apply here: `--class` and `--type` are rejected
for this provider. Size VMs with `--exe-dev-cpus`, `--exe-dev-memory`, and
`--exe-dev-disk`, and pick an image with `--exe-dev-image`.

### Environment overrides

```text
CRABBOX_EXE_DEV_CONTROL_HOST / EXE_DEV_CONTROL_HOST
CRABBOX_EXE_DEV_IMAGE        / EXE_DEV_IMAGE
CRABBOX_EXE_DEV_FROM
CRABBOX_EXE_DEV_CPUS
CRABBOX_EXE_DEV_MEMORY       / EXE_DEV_MEMORY
CRABBOX_EXE_DEV_DISK         / EXE_DEV_DISK
CRABBOX_EXE_DEV_COMMAND
CRABBOX_EXE_DEV_USER
CRABBOX_EXE_DEV_WORK_ROOT
CRABBOX_EXE_DEV_NO_EMAIL
```

`exeDev.user` is empty by default; Crabbox uses the user embedded in the VM's
`ssh_dest`. If the destination omits a user, Crabbox uses `exeDev.user`, then a
non-default `sshUser`, then the current OS account name (`USER`, or `root`, only
if the OS account lookup is unavailable). An advertised user takes precedence
over these fallbacks. The SSH port comes from `ssh_dest` as
well. The raw `exeDev.workRoot` setting stays empty until resolution: a
non-default top-level `workRoot` is inherited, otherwise the runtime fallback
is `/var/tmp/crabbox`. Existing leases keep the work root recorded in their
validated local claim when reused, including leases created under `/tmp/crabbox`.
For new leases, a nonempty provider work root takes precedence over the top-level
setting.

The raw image setting also stays empty by default. Native creation omits
`--image` when its trimmed value is empty; the displayed label `default` is not
a configured native image. These runtime and display fallbacks do not populate
base configuration or unselected provider flag defaults.

Nonempty YAML strings replace prior values without trimming; empty, omitted,
or `null` strings keep the prior value. YAML CPU values apply only when positive.
Environment CPU parsing keeps the prior value on malformed input; parsed zero
or negative values reach the existing runtime fallback. Explicit
`noEmail: false` is preserved.

## Behavior

1. `warmup` lists existing exe.dev VMs, allocates a Crabbox slug, then creates a
   VM with the control-host call `new --name <crabbox-name> --json`, adding the
   tags `crabbox`, `crabbox-lease-<id>`, and `crabbox-slug-<slug>`, a random
   `crabbox-claim-<generation>` resource-binding tag, plus
   `--no-email`, `--image`, `--cpu`, `--memory`, `--disk`, and `--command` as
   configured.
   With `--exe-dev-from <base>` (or `exeDev.base`), creation instead calls
   `cp <base> <crabbox-name> --copy-tags=false --json` with the configured
   CPU, memory, and disk sizes, then assigns fresh Crabbox tags. The source
   VM is never tagged, renamed, or deleted. `base` conflicts with nonempty
   image and command settings because those come from the base. `noEmail`
   applies only to `new`; `cp` has no notification option.
2. If creation has not yet returned `ssh_dest`, Crabbox refreshes the exact VM
   until the provider advertises its route. The bootstrap deadline covers both
   inventory requests and polling delays; no hostname is synthesized. Crabbox
   preserves ambient SSH configuration and uses the advertised user and port,
   falling back to the current OS account when no user is advertised. Failures
   after accepted creation use the normal verified rollback unless `--keep` is
   set. Crabbox then waits for SSH readiness on `ssh_dest` and uses its
   standard rsync + remote command execution and persists the exact VM name,
   SSH endpoint, ownership tags, exe.dev control route, and a non-secret hash
   of the authenticated exe.dev account in the local claim.
3. `list --provider exe-dev` calls `ls --l --json` and shows only VMs with the
   complete `crabbox`, canonical lease, and slug tag set. Pass `--all` to inspect
   unowned or incomplete inventory; names that merely start with `crabbox-` do
   not establish ownership.
4. Reusing a completely tagged VM without a local claim, or upgrading a legacy
   claim that lacks a control-route scope or matching remote claim generation,
   requires explicit `--reclaim`. Reclaim rotates the remote generation while
   holding the unchanged local claim lock.
   Crabbox refuses untagged adoption and never retargets a claim already bound
   to another VM or control route.
5. `stop --provider exe-dev <id>` deletes only when the unchanged local claim,
   exact VM name, deterministic lease name, complete remote tags, remote claim
   generation, current control route, and authenticated account fingerprint all
   agree. It rechecks inventory while holding the claim lock,
   calls `rm <vm-name> --json`, and removes the claim only after deletion
   succeeds. Failed or ambiguous deletion keeps the claim for an exact retry;
   if a complete account-bound inventory later confirms the exact VM is absent,
   the retry removes only the still-unchanged local claim.

## Reuse a base VM

Prepare a trusted base with tools and caches on persistent disk, then clone it:

```sh
crabbox warmup --provider exe-dev --exe-dev-from my-base --slug test-copy
crabbox run --provider exe-dev --id test-copy -- go test ./...
crabbox stop --provider exe-dev test-copy
```

Copies use the configured CPU, memory, and disk sizes, including the defaults of
2 vCPU, 4 GB memory, and 10 GB disk. For a larger base, set `--exe-dev-disk` to its
disk size or larger; adjust `--exe-dev-cpus` and `--exe-dev-memory` as needed.

The default `/var/tmp/crabbox` work root is on persistent disk with the default
exe.dev image. A custom image must provide a writable persistent root; set
`exeDev.workRoot` when needed. Copies retain disk files at their original
absolute paths. A checkout under the base's old lease directory does not become
the new lease's workspace automatically: normal sync populates the new lease
path, while machine-wide tools and caches are already present.

Clone only bases whose disk contents you trust, including any stored credentials
and services that start at boot. exe.dev copies disk state into a running VM;
Crabbox does not promise memory or process restoration. The base and clone both
consume account capacity until deleted. Stop the clone normally; its base stays
available for subsequent copies.

Clones must advertise the exact fresh ownership and generation tags before
Crabbox connects over SSH. If copying or tagging has an uncertain outcome,
Crabbox reports the exact destination and a manual-cleanup command. It refuses
to delete an untagged or replaced VM automatically; inspect it before cleanup.

## Workspace checkpoints and forks

exe-dev supports the shared archive checkpoint workflow:

```sh
crabbox checkpoint create --provider exe-dev --id test-copy --mode archive --json
crabbox checkpoint restore <checkpoint-id> --provider exe-dev --id test-copy
crabbox checkpoint fork <checkpoint-id> --provider exe-dev --exe-dev-from my-base
```

A fork creates a separately claimed lease, optionally from the configured base,
then restores the saved workspace archive into the new workspace. Forks are
kept by default; stop each fork when finished. Archives save workspace files,
not installed system tools, memory, or running processes.

The provider capability bits for checkpoints, forks, restores, and snapshots
describe native operations and remain unadvertised. The commands above use the
shared archive fallback. `cp` creates a mutable, running VM rather than an
immutable checkpoint resource, so `--mode native` is unsupported.

## Limits

- Linux only; non-Linux targets are rejected.
- `--tailscale` is rejected — exe.dev VMs expose public SSH only.
- No Crabbox coordinator support; auth and billing stay with your local exe.dev
  SSH account.
- Advertised features are SSH and Crabbox sync. Shared archive checkpoints and
  forks work over SSH. No managed desktop/VNC, browser, code-server, or native
  provider snapshots.
- Actions hydration works only if the chosen VM image ships the expected Linux
  SSH tooling and GitHub runner prerequisites.

## Live smoke

```sh
ssh -o BatchMode=yes exe.dev whoami --json
ssh -o BatchMode=yes exe.dev ls --l --json
crabbox run --provider exe-dev --slug exe-smoke --no-sync -- uname -a
crabbox stop --provider exe-dev exe-smoke
```

To adopt an existing fully tagged VM after inspecting it, run a normal reuse
with explicit ownership intent before stopping it:

```sh
crabbox run --provider exe-dev --id <vm-or-lease> --reclaim --no-sync -- true
crabbox stop --provider exe-dev <vm-or-lease>
```

If VM creation returns an active-plan error, resolve billing at
`https://exe.dev/user` before rerunning the smoke.

## Related

- [Provider reference](README.md)
- [Static SSH](ssh.md)
- [Provider backends](../provider-backends.md)
