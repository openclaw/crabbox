# Crabbox for Herdr

Run Crabbox workflows from [Herdr](https://herdr.dev) without leaving the
current project workspace. The plugin keeps all provisioning, credentials,
repository sync, and lease ownership inside the installed `crabbox` CLI; Herdr
only provides the actions and terminal panes.

## Actions

| Action | Behavior |
| --- | --- |
| `crabbox.boxes` | Opens a live, read-only overlay of leases for the configured provider. |
| `crabbox.warmup` | Runs `crabbox warmup` for the focused workspace. |
| `crabbox.prewarm` | Runs `crabbox prewarm` for the focused workspace. |
| `crabbox.connect` | Lists leases, asks which one to use, then opens `crabbox connect` in a new tab. |
| `crabbox.run-job` | Lists repository jobs, asks which one to run, then executes `crabbox job run`. |
| `crabbox.doctor` | Runs the normal Crabbox readiness checks in a split pane. |

The plugin does not install lifecycle hooks, stop leases when panes close, or
install keybindings. `run-job` follows the job's normal Crabbox stop policy.
Provisioning actions have the same cost and credential implications as running
the corresponding Crabbox commands directly. Result panes stay open until you
press Enter so final lease identifiers, diagnostics, and failures remain visible.

## Install

Install Herdr and a Crabbox build that already contains this integration first.
The plugin installation rejects older Crabbox binaries before registering the
plugin:

```sh
brew install openclaw/tap/crabbox
herdr plugin install openclaw/crabbox/plugins/herdr
```

Herdr runs the plugin's build step during a GitHub install. The build records
the absolute path of the current Crabbox executable so the integration keeps
working when the Herdr server has an older or minimal `PATH`.

For local development, build the shim and link the working directory:

```sh
cd plugins/herdr
sh build.sh
herdr plugin link .
```

## Optional keybindings

Actions appear in Herdr's plugin action list without extra configuration. To
bind frequently used actions, add any free keys to
`~/.config/herdr/config.toml`:

```toml
[[keys.command]]
key = "prefix+shift+b"
type = "plugin_action"
command = "crabbox.boxes"
description = "Crabbox boxes"

[[keys.command]]
key = "prefix+shift+r"
type = "plugin_action"
command = "crabbox.run-job"
description = "Run a Crabbox job"
```

Reload Herdr after changing the file:

```sh
herdr server reload-config
```

## Configuration

The plugin uses the normal Crabbox configuration and credentials discovered
from the focused workspace. Configure providers, brokers, jobs, and defaults in
the same user or repository files used by the CLI. Set
`CRABBOX_HERDR_REFRESH_INTERVAL` to a duration such as `5s` to change the boxes
overlay refresh rate; the default is `3s` and the minimum is `1s`.

The plugin supports Linux and macOS with Herdr 0.7.0 or newer. The Crabbox CLI
must be new enough to contain the bundled Herdr adapter; if an older binary is
selected during installation, upgrade Crabbox and reinstall the plugin.

## Uninstall

```sh
herdr plugin uninstall crabbox
```

Uninstalling the plugin does not remove Crabbox, its configuration, existing
leases, or recorded run data.
