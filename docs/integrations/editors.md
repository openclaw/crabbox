# Editors and Control Surfaces

Editor and terminal integrations run on the operator machine and delegate
remote execution to the installed `crabbox` CLI. They are control surfaces,
not provider adapters and not alternate Crabbox clients.

## Zed control surface

The package under `integrations/zed` adds Crabbox YAML support, snippets, and
lifecycle tasks. Its tasks invoke the local CLI from the current worktree for
doctor, warmup, run, list, status, connect, inspect, and stop.

The package does not store provider credentials or lease state, call provider
APIs, use MCP, or implement remote-editor handoff. No Crabbox registry
submission exists yet. Until one is opened and accepted, clone the Crabbox
repository, run **zed: extensions**, choose **Install Dev Extension**, and
select `integrations/zed` from the checkout. See the complete
[Zed package guide](https://github.com/openclaw/crabbox/tree/main/integrations/zed#readme).
Registry publication is tracked in https://github.com/openclaw/crabbox/issues/1157.

| Contract field | Zed package |
| --- | --- |
| Type and status | Local editor language/task package; development install available, registry unsubmitted |
| Install owner | Zed development-extension flow now; Zed registry after acceptance |
| Crabbox contract | Public `doctor`, `warmup`, `run`, `job`, `list`, `status`, `connect`, `inspect`, and `stop` commands |
| Platforms and targets | Direct tasks use the local CLI; interactive Bash helpers target macOS and Linux; remote support follows the selected provider |
| Credentials and lifecycle | Credentials remain in Crabbox; the CLI owns acquisition, cancellation, keepalive, and release |
| Return path | Zed task output; repository changes follow normal Crabbox local-to-remote sync rules |
| Validation | Static package check plus the official Zed compiler and controlled task execution in Zed Extension E2E CI |

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
for warmup, prewarm, connect, jobs, and doctor. Install it directly from its
repository path:

```sh
herdr plugin install openclaw/crabbox/plugins/herdr
```

See the complete [Herdr package guide](https://github.com/openclaw/crabbox/tree/main/plugins/herdr#readme).

The historical directory name reflects Herdr's package format. In Crabbox
product language it is an integration. Herdr owns panes and actions; Crabbox
owns credentials and lifecycle. The package supports Linux and macOS with
Herdr 0.7.0 or newer, and the compatible integration shipped in Crabbox 0.39.0.
Direct installation is available, but the package is not yet discoverable in
Herdr's topic-driven marketplace; publication is tracked in
https://github.com/openclaw/crabbox/issues/1156.

| Contract field | Herdr package |
| --- | --- |
| Type and status | Local terminal UI plugin; direct install available, marketplace indexing pending |
| Install owner | Herdr CLI and its managed repository checkout |
| Crabbox contract | First-party `__herdr-plugin` adapter plus public lease and job behavior |
| Platforms and targets | Herdr 0.7.0 or newer on macOS and Linux; remote support follows the selected provider |
| Credentials and lifecycle | Credentials remain in Crabbox; normal Crabbox job and lease policies own stop behavior |
| Return path | Herdr overlays, splits, and tabs backed by Crabbox command output |
| Validation | Manifest/invocation contract tests and live Herdr 0.7.4 proof recorded in https://github.com/openclaw/crabbox/pull/1083 |

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
