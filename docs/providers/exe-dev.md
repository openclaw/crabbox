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

The control host calls (`new`, `ls`, `rm`) speak the exe.dev API over SSH. Your
own shell commands run on the VM SSH target, not through the control host.

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
  cpus: 2                # default
  memory: 4GB            # default
  disk: 10GB             # default
  command: ""            # optional container command
  user: ""               # SSH login user (defaults to ssh_dest / local user)
  workRoot: /tmp/crabbox # default remote work root
  noEmail: true          # default; suppress exe.dev notification email
```

### Provider flags

```text
--exe-dev-control-host <host>
--exe-dev-image <image>
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
CRABBOX_EXE_DEV_CPUS
CRABBOX_EXE_DEV_MEMORY       / EXE_DEV_MEMORY
CRABBOX_EXE_DEV_DISK         / EXE_DEV_DISK
CRABBOX_EXE_DEV_COMMAND
CRABBOX_EXE_DEV_USER
CRABBOX_EXE_DEV_WORK_ROOT
CRABBOX_EXE_DEV_NO_EMAIL
```

`exeDev.user` is empty by default; Crabbox uses the user embedded in the VM's
`ssh_dest` (falling back to your local SSH identity), so set it only when your
image expects a different login user. The SSH port comes from `ssh_dest` as
well. `exeDev.workRoot` defaults to `/tmp/crabbox`; setting a non-default
top-level `workRoot` propagates to the VM when `exeDev.workRoot` is unset.

## Behavior

1. `warmup` lists existing exe.dev VMs, allocates a Crabbox slug, then creates a
   VM with the control-host call `new --name <crabbox-name> --json`, adding the
   tags `crabbox`, `crabbox-lease-<id>`, and `crabbox-slug-<slug>`, plus
   `--no-email`, `--image`, `--cpu`, `--memory`, `--disk`, and `--command` as
   configured.
2. Crabbox waits for SSH readiness on the returned `ssh_dest`, then uses its
   standard rsync + remote command execution.
3. `list --provider exe-dev` calls `ls --l --json` and shows the Crabbox-tagged
   VMs.
4. `stop --provider exe-dev <id>` calls `rm <vm-name> --json` and removes the
   local claim.

## Limits

- Linux only; non-Linux targets are rejected.
- `--tailscale` is rejected — exe.dev VMs expose public SSH only.
- No Crabbox coordinator support; auth and billing stay with your local exe.dev
  SSH account.
- Features are limited to SSH access and Crabbox sync. No managed desktop/VNC,
  browser, code-server, or native provider checkpoints.
- Actions hydration works only if the chosen VM image ships the expected Linux
  SSH tooling and GitHub runner prerequisites.

## Live smoke

```sh
ssh -o BatchMode=yes exe.dev whoami --json
ssh -o BatchMode=yes exe.dev ls --json
crabbox run --provider exe-dev --slug exe-smoke --no-sync -- uname -a
crabbox stop --provider exe-dev exe-smoke
```

If VM creation returns an active-plan error, resolve billing at
`https://exe.dev/user` before rerunning the smoke.

## Related

- [Provider reference](README.md)
- [Static SSH](ssh.md)
- [Provider backends](../provider-backends.md)
