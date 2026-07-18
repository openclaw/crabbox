# Editors and Control Surfaces

Editor and terminal integrations run on the operator machine and delegate
remote execution to the installed `crabbox` CLI. They are control surfaces,
not provider adapters and not alternate Crabbox clients.

## Zed control surface

The package under `integrations/zed` adds Crabbox YAML support, snippets, and
lifecycle tasks. Its tasks invoke the local CLI from the current worktree for
doctor, warmup, run, list, status, connect, inspect, and stop.

The package does not store provider credentials or lease state, call provider
APIs, or use MCP. Until the package is accepted by Zed's extension registry,
use the development-extension installation described in the
`integrations/zed/README.md` package guide.

## Remote editor handoff

`crabbox open` prepares an existing SSH-capable lease for a remote editor and
keeps lease activity alive in the foreground:

```sh
crabbox run --id swift-crab --sync-only
crabbox open --editor=zed --id swift-crab
```

For automation, `--json` writes one versioned
`crabbox/editor-handoff/v1` object before waiting. It includes the complete SSH
command, remote folder, lease activity semantics, and, when the target has a
lease ID, a release command.

```sh
crabbox open --editor=zed --id swift-crab --json
```

The handoff intentionally does not modify SSH configuration, editor settings,
or launch an editor process. See the [`open` command](../commands/open.md) for
target restrictions and foreground lifecycle behavior.

Crabbox sync is local to remote. Commit and push editor changes, or explicitly
copy them back, before releasing an ephemeral lease.

## Herdr

The package under `plugins/herdr` adds a live lease overlay and Herdr actions
for warmup, prewarm, connect, jobs, and doctor. Installation and local-link
commands live in `plugins/herdr/README.md`.

The historical directory name reflects Herdr's package format. In Crabbox
product language it is an integration. Herdr owns panes and actions; Crabbox
owns credentials and lifecycle. The package supports Linux and macOS with
Herdr 0.7.0 or newer.

## Contract for another editor

A new editor package should:

- invoke the local CLI with an argument array and the repository root as its
  working directory;
- consume documented `--json` output rather than scrape human tables when a
  machine-readable form exists;
- forward cancellation to the child process and preserve foreground keepalive
  processes when the CLI contract requires it;
- leave credentials, provider API calls, lease claims, cost policy, sync,
  evidence, and cleanup inside Crabbox;
- make provisioning cost and destructive stop actions explicit in its UI;
- declare target OS, shell, and remote-edit sync-back limitations;
- test calls through a controlled CLI boundary and add real host proof before
  publication.

Do not add a hidden core command for each editor. Extend a public,
provider-neutral CLI or JSON contract only when the editor cannot use an
existing one.

See [Integration Authoring](authoring.md) for the shared review checklist.
