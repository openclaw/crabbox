# exe.dev Provider

Read when:

- choosing `provider: exe-dev`;
- changing `internal/providers/exedev`;
- debugging exe.dev SSH API lifecycle or VM SSH sync/run behavior.

exe.dev is an SSH lease provider. Crabbox uses the exe.dev SSH API on
`exe.dev` to create, list, and delete VMs, then uses the VM's `ssh_dest` as a
normal Linux SSH target for sync, run, status, and `crabbox ssh`.

Use this provider when you want exe.dev's disposable VM lifecycle while keeping
Crabbox's standard SSH sync/run path. Do not model exe.dev's `/exec` endpoint
as arbitrary shell execution; `/exec` runs exe.dev API commands. Shell commands
belong on the VM SSH target.

## Quick Start

```sh
ssh exe.dev whoami
crabbox warmup --provider exe-dev --slug smoke
crabbox run --provider exe-dev --id smoke -- pnpm test
crabbox ssh --provider exe-dev --id smoke
crabbox stop --provider exe-dev smoke
```

The local `ssh exe.dev ...` login must already work. VM creation also requires
an active exe.dev plan.

## Config

```yaml
provider: exe-dev
exeDev:
  controlHost: exe.dev
  image: ""
  cpus: 2
  memory: 4GB
  disk: 10GB
  command: ""
  user: ""
  workRoot: /tmp/crabbox
  noEmail: true
```

Provider flags:

```text
--exe-dev-control-host <host>
--exe-dev-image <image>
--exe-dev-cpus <n>
--exe-dev-memory <size>
--exe-dev-disk <size>
--exe-dev-command <command>
--exe-dev-user <user>
--exe-dev-work-root <path>
--exe-dev-no-email
```

Environment overrides:

```text
CRABBOX_EXE_DEV_CONTROL_HOST / EXE_DEV_CONTROL_HOST
CRABBOX_EXE_DEV_IMAGE / EXE_DEV_IMAGE
CRABBOX_EXE_DEV_CPUS
CRABBOX_EXE_DEV_MEMORY / EXE_DEV_MEMORY
CRABBOX_EXE_DEV_DISK / EXE_DEV_DISK
CRABBOX_EXE_DEV_COMMAND
CRABBOX_EXE_DEV_USER
CRABBOX_EXE_DEV_WORK_ROOT
CRABBOX_EXE_DEV_NO_EMAIL
```

`exeDev.user` defaults to the local OS user because exe.dev's VM SSH examples
use the caller's SSH identity. Set it when your VM image expects a different
login user. The SSH port is always `22`, and `exeDev.workRoot` defaults to
`/tmp/crabbox`.

## Behavior

1. `warmup` lists existing exe.dev VMs, allocates a Crabbox slug, and runs
   `ssh exe.dev new --name <crabbox-name> --json`.
2. The provider tags VMs with `crabbox`, the Crabbox lease ID, and the slug when
   exe.dev accepts tags.
3. Crabbox waits for SSH readiness on the returned `ssh_dest`, then uses normal
   rsync and remote command execution.
4. `list --provider exe-dev` shows Crabbox-prefixed VMs.
5. `stop --provider exe-dev <id>` runs `ssh exe.dev rm <vm-name> --json` and
   removes the local claim.

## Limits

- Linux only.
- No Crabbox coordinator support; auth and billing stay with the local exe.dev
  SSH account.
- No managed VNC, browser, code-server, Tailscale bootstrap, or native provider
  checkpoint support yet.
- Actions hydration works only if the chosen VM image has the expected Linux
  SSH tools and GitHub runner prerequisites.

## Live Smoke

```sh
ssh -o BatchMode=yes exe.dev whoami --json
ssh -o BatchMode=yes exe.dev ls --json
crabbox run --provider exe-dev --slug exe-smoke --no-sync -- uname -a
crabbox stop --provider exe-dev exe-smoke
```

If `new` returns an active-plan error, fix billing at `https://exe.dev/user`
before rerunning the Crabbox smoke.

## Related

- [Provider reference](README.md)
- [Static SSH](ssh.md)
- [Provider backends](../provider-backends.md)
