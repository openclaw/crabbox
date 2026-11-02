# WarpBuild

WarpBuild provides standalone macOS 26 sandboxes through its `wb` CLI. This
provider works independently of Boat and Blacksmith. It does not dispatch a
GitHub Actions workflow.

## Setup

Install the official [WarpBuild CLI](https://www.warpbuild.com/docs/ci/sandboxes)
and provide a CI-scoped API key through `WB_TOKEN` or
`CRABBOX_WARPBUILD_TOKEN`. Never put the key in repository configuration.

```yaml
provider: warpbuild
warpbuild:
  runnerLabel: warp-macos-26-arm64-6x
  workdir: /Users/runner/crabbox
```

`warpbuild.cliPath` selects the local executable. It is accepted only from
trusted user configuration, `CRABBOX_WARPBUILD_CLI`, or `--warpbuild-cli`.
Repository configuration cannot replace the executable that receives the token.
The adapter fixes the API origin and guest user, ignoring native `WB_API_URL`,
`WB_SBX_RUNNER_LABEL`, and `WB_SBX_USER` environment overrides.

## Commands

```sh
crabbox doctor --provider warpbuild
crabbox run --provider warpbuild -- make test
crabbox warmup --provider warpbuild --keep
crabbox run --provider warpbuild --id <lease-id> -- make test
crabbox pause --provider warpbuild <lease-id>
crabbox resume --provider warpbuild <lease-id>
crabbox stop --provider warpbuild <lease-id>
```

Use `--warpbuild-runner-label warp-macos-26-arm64-12x` for the larger sandbox.
Generic `--class` and `--type` are unsupported. The smaller native shape is the
default; size labels describe requested resources, not measured capacity.

Crabbox uploads an archive of the selected workspace before each run. `--no-sync`
reuses the existing remote workspace. `--sync-only` updates it without running
a command. Native `wb` execution carries the command output and exit status.

Only Crabbox-owned local claims can be reused or deleted. Before an operation,
the adapter compares the sandbox's immutable ID and creation time with the
claim. A matching name alone does not establish ownership.

The native CLI addresses effects by sandbox name, without an atomic immutable-ID
condition. Do not delete and recreate an active Crabbox sandbox under the same
name. Crabbox's identity read detects existing replacements, but cannot fence a
replacement made afterward by another trusted operator in the WarpBuild account.

## Lifetime and limitations

WarpBuild's documented native lifetime is seven days, including paused time.
Its CLI does not expose configurable server-side TTL or idle timeout. Crabbox's
generic TTL and idle settings do not schedule WarpBuild deletion. Stop owned
sandboxes explicitly when finished; normal one-shot runs delete their sandbox.

Pausing saves selected filesystem paths and stops compute. It does not preserve
running processes. The home directory and Homebrew installation persist, but
`~/Library`, `~/.warpbuild`, `/tmp`, `/usr/local`, and `/Applications` do not.
Saved disks have separate storage charges. See the current
[WarpBuild sandbox contract and pricing](https://www.warpbuild.com/docs/ci/sandboxes).

Disconnecting from a native foreground command does not confirm remote process
termination. Crabbox reports this gap after cancellation. Delete the sandbox
with the printed stop command when termination is required.

This provider does not advertise SSH, desktop access, port forwarding, Actions
hydration, or automatic orphan cleanup. Linux and Windows WarpBuild Actions
runners are separate products and are not targets of this sandbox adapter.

If creation times out before Crabbox can record the immutable identity, follow
the reported native `wb sbx info` instruction. Do not retry creation blindly or
delete an unrelated sandbox with a similar name.

## Live qualification

Build the candidate CLI and run the opt-in smoke with a CI-scoped test token:

```sh
go build -o bin/crabbox ./cmd/crabbox
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=warpbuild scripts/live-smoke.sh
```

The smoke allocates one 6x sandbox, checks archive replacement, environment
forwarding, reuse, pause/resume, command exit status, and deletion. It incurs
provider charges. A failed cleanup retains its isolated local claim directory
and prints the exact recovery command. If allocation fails before a claim is
written, use the native sandbox name printed by the adapter to reconcile it.

The adapter was developed against `wb` v0.1.2. Local fixture tests do not replace
this live qualification against the configured account.
