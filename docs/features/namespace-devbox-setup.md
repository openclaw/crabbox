# Namespace Devbox Setup

Read when:

- installing and authenticating the Namespace `devbox` CLI for use with Crabbox;
- preparing a machine so `provider: namespace-devbox` works non-interactively;
- live-checking that Namespace auth, Devbox SSH, and Crabbox agree.

Crabbox does not talk to the Namespace API directly. It shells out to the
upstream `devbox` CLI for the whole Devbox lifecycle (create, prepare SSH,
list, shutdown, delete) and lets that CLI own login and credentials. Crabbox
then reads the SSH config the CLI generates and drives normal SSH and rsync.
That means `devbox` must be installed, on `PATH`, and authenticated wherever
Crabbox runs.

For provider selection, config keys, and the lifecycle boundary, see
[Namespace Devbox](namespace-devbox.md).

## Install and Authenticate

Install the upstream Devbox CLI and log in:

```sh
curl -fsSL https://get.namespace.so/devbox/install.sh | bash
devbox login
devbox auth check-login
```

`devbox login` opens a browser. For headless or automation hosts, request a
workspace handoff URL and approve the login from an authenticated browser:

```sh
devbox login --browser=false
```

Open the printed URL, verify its code matches the terminal, and select the
workspace to authorize. The CLI stores the resulting session in its platform
user config directory. Do not copy the browser token-page value into a profile
or the CLI's credential file.

```sh
devbox auth check-login
```

Crabbox does not read or store Namespace credentials; it relies entirely on
the auth state the `devbox` CLI resolves at invocation time.

## Live Check

First confirm the CLI itself is authenticated and can reach the account:

```sh
devbox auth check-login
devbox image list -o json
devbox list -o json
```

Then verify the full Crabbox path against a throwaway Devbox. Let Crabbox
provision it so the generated SSH config and the managed naming/cleanup all
match what production runs use:

```sh
crabbox warmup --provider namespace-devbox --slug smoke --namespace-size S
crabbox run --provider namespace-devbox --id smoke --shell 'echo crabbox-live-ok'
crabbox stop --provider namespace-devbox smoke
```

`warmup` prints a `provisioned … state=ready` line once the Devbox is up and
Crabbox has confirmed `git`, `rsync`, and `tar` over SSH; `run --shell` proves
command transport; `stop` shuts the Devbox down (or deletes it when
`namespace.deleteOnRelease` is set) and removes the SSH config it created.

## How Crabbox Drives `devbox`

- **Create** — Crabbox writes a temporary YAML spec and runs
  `devbox create --from <spec>.yaml`. The spec carries `name`, `image`,
  `size`, and (when configured) `checkout`, `site`, `volume_size_gb`, and
  `auto_stop_idle_timeout`. The auto-stop timeout defaults to the
  `namespace.autoStopIdleTimeout` config value (30m by default), falling back
  to the lease idle timeout, and is rounded up to whole minutes.
- **Prepare SSH** — Crabbox runs `devbox configure-ssh` (or `devbox prepare`)
  and reads the resulting host entry, then connects with plain SSH.
- **List** — `devbox list -o json` (falling back to `--json`).
- **Release** — `devbox shutdown <name> --force` by default, or
  `devbox delete <name> --force` when `namespace.deleteOnRelease` is true.

## Notes

- Default image is `builtin:base`; default size is `M` (override per lease with
  `--namespace-size S|M|L|XL`); default work root is `/workspaces/crabbox`.
- Crabbox-managed Devboxes are named `crabbox-<slug>-<8hex>`, and the generated
  SSH host is `<name>.devbox.namespace`.
- The CLI writes per-host SSH config under `~/.namespace/ssh/`
  (`<host>.ssh` plus a `<host>.key`) and includes it from `~/.ssh/config`.
- `crabbox stop --provider namespace-devbox` keeps that lease's claim and
  `crabbox-*.devbox.namespace.{ssh,key}` files when it shuts the Devbox down;
  delete-on-release removes them;
  `crabbox cleanup --provider namespace-devbox` sweeps all Crabbox-owned
  entries under `~/.namespace/ssh/` (use `--dry-run` to preview).
- `devbox list -o json` prints a non-JSON notice when no Devboxes exist;
  Crabbox treats that as an empty list.

Related docs:

- [Namespace Devbox](namespace-devbox.md)
- [Provider: Namespace Devbox](../providers/namespace-devbox.md)
- [Providers](providers.md)
