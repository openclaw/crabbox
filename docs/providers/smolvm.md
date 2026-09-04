# SmolVM Provider

Read this when:
- choosing `provider: smolvm`;
- configuring the SmolVM (smol machines) image, workdir, CPU count, memory, or related options;
- changing `internal/providers/smolvm`.

SmolVM (smol machines) provides fast microVM sandboxes via the hosted smolfleet API (`api.smolmachines.com`). It is a **delegated-run** provider. Crabbox calls the smolfleet REST API directly for sandbox lifecycle (create/start, status, exec, list, delete) and performs archive sync + file writes via direct `/exec` calls (heredoc + base64 payload for the tar). No guest Python or other interpreters are required for Crabbox operations — only standard shell tools (`sh` + `base64` + `tar`). Smol machines own the sandbox state and process transport; Crabbox owns local config, repo claims, archive sync manifests and guardrails, slugs, timing summaries, and the normalized `list`/`status` output. There is no direct SSH target.

## When to use

Use SmolVM when commands should run in fast microVM sandboxes and you do not need a Crabbox SSH box. It is good for isolated test runs and agent sandboxes. Reach for a provisioned SSH provider (AWS, Hetzner, Azure, GCP, static SSH) or another SSH-capable delegated provider instead when you need `crabbox ssh`, VNC, code-server, or Actions hydration — none of those surfaces exist on SmolVM.

SmolVM is Linux-only. Desktop, browser, code, and SSH-based run options are not available.

## Setup

Create a smol machines API key at https://smolmachines.com/console/keys. Keys are prefixed `smk_...`.

Export the key through the environment using one of the accepted variable names. Crabbox never accepts the key as a command-line flag.

## Auth

```sh
export SMOLMACHINES_API_KEY=smk_...
# or
export CRABBOX_SMOLVM_API_KEY=smk_...
# or
export SMK_API_KEY=smk_...
```

`CRABBOX_SMOLVM_API_KEY` takes precedence when multiple variables are present.

Rotate the key if it was ever pasted into a chat, shell history, issue, or log.

## Commands

```sh
crabbox warmup --provider smolvm --smolvm-image alpine
crabbox run --provider smolvm -- pnpm test
crabbox run --provider smolvm --id swift-crab --shell 'pnpm install && pnpm test'
crabbox status --provider smolvm --id swift-crab
crabbox stop --provider smolvm swift-crab
crabbox list --provider smolvm --json
```

`warmup` always keeps the sandbox until an explicit `stop`. The lease ID, slug, or SmolVM sandbox identifier printed by `warmup`/`run` can be passed to later commands via `--id`.

## Config

```yaml
provider: smolvm
target: linux
smolvm:
  image: alpine
  workdir: /workspace
  cpus: 1
  memoryMB: 512
```

Provider flags (each overrides the matching `smolvm.*` config key):

```text
--smolvm-image
--smolvm-workdir
--smolvm-cpus
--smolvm-memory-mb
```

(Additional `--smolvm-*` flags exist for other smolfleet options; run `crabbox run --help` with `--provider smolvm` for the current set.)

Environment overrides:

```text
SMOLMACHINES_API_KEY
CRABBOX_SMOLVM_API_KEY
SMK_API_KEY
CRABBOX_SMOLVM_BASE_URL
CRABBOX_SMOLVM_ALLOW_CUSTOM_BASE_URL
CRABBOX_SMOLVM_IMAGE
CRABBOX_SMOLVM_WORKDIR
CRABBOX_SMOLVM_CPUS
CRABBOX_SMOLVM_MEMORY_MB
CRABBOX_SMOLVM_NETWORK
CRABBOX_SMOLVM_KEEP
```

The base URL must use `https` (plain `http` is allowed only for localhost
endpoints) and must not contain userinfo, query, or fragment components; the
API key is sent as a bearer token only to official `smolmachines.com` hosts or
loopback by default. Set `CRABBOX_SMOLVM_ALLOW_CUSTOM_BASE_URL=1` only when you
intend to trust a custom control plane with the API key.

Defaults: image `alpine` (lightweight; provides the standard shell tools needed for direct archive sync via the API), workdir `/workspace`, network open by default.

## Lifecycle

1. `warmup` / `run` without `--id` creates a microVM sandbox from the configured `--smolvm-image`.
2. Before startup, Crabbox durably binds a local claim to the returned machine ID, name, creation timestamp, full API endpoint, normal `cbx_...` lease ID, and friendly slug.
3. By default `run` archive-syncs the working tree using a direct API call to `/exec` (the tarball is base64-encoded and sent in a shell heredoc; the guest completes checked base64 decoding before extracting with `tar`).
4. The user command executes inside the microVM. Because the smolfleet API does not stream live output, command output appears after the command completes.
5. One-shot sandboxes are deleted after a `run` that did not pass `--keep`. `--keep` and `--keep-on-failure` retain the sandbox until `crabbox stop`.
6. `run --lease-output <path>` writes the SmolVM lease ID, slug, reuse/retention state, and exact cleanup command for orchestration handoff.

Note: `warmup` always keeps the sandbox until an explicit `crabbox stop`. If you pass `--keep=false` to `warmup`, Crabbox prints a warning and still keeps it.

Deletion and reuse require that exact local claim and a fresh matching machine response. Crabbox holds the unchanged claim through deletion and confirmed absence; run teardown and failed-start rollback use the same ownership checks with a fresh 60-second cleanup budget. Explicit stop preserves caller cancellation within that budget. A concurrent claim change, changed machine identity, failed delete, or uncertain confirmation retains the claim and reports the cleanup problem.

Older claims without the machine ID, endpoint, and creation timestamp do not authorize stop or reuse. Name-matched machines remain discoverable through `list` and `status`, which do not create or upgrade claims. `--reclaim` transfers repository ownership of an already proven binding; it never adopts an unclaimed or legacy machine. Review those machines in the provider console before any manual cleanup, or create a new lease for reuse.

The [hosted API](https://smolmachines.com/docs/cloud/api-reference) uses 404 both for missing machines and machines not owned by the caller. An initial 404 therefore retains the claim, including after credentials change. Crabbox only accepts 404 as deletion confirmation after freshly matching and successfully deleting the bound machine using the same client. No API key or key fingerprint is stored in the claim.

## Capabilities

- SSH: no.
- Crabbox sync: yes — archive sync via direct `/exec` (heredoc + base64 tar; pure shell on the guest, no Python).
- Provider sync: no separate SmolVM sync step.
- Desktop / browser / code: no.
- Actions hydration: no.
- Run-session output: yes — `--lease-output` records the SmolVM lease, reuse/retention state, and cleanup command.
- Coordinator (broker): no — SmolVM always runs direct from the CLI.

## Notes

- Defaults to the lightweight `alpine` image (provides `sh` + `base64` + `tar` for direct API-driven archive sync; user commands can `apk add` additional packages as needed).
- Network is open by default.
- Archive sync and small file writes use direct calls to the smolfleet `/exec` API (heredoc payload).
- Archive and small-file uploads share a checked byte-transfer path: an invocation-private directory holds a fully decoded file before extraction or publication. Decoder, write, extraction and temporary-cleanup failures return nonzero status; an earlier failure remains primary. Small files are published from a private same-filesystem staging file, so failed decoding does not truncate an existing destination. Archive extraction retains the incoming file-mode mask.
- Remote traps clean temporary upload files on ordinary completion and handled signals. They do not guarantee cleanup after SIGKILL or an ambiguous HTTP failure while the remote command may still run. This does not make workspace replacement transactional. Final environment profiles use the separate run-scoped owner described below.
- No direct `crabbox ssh` / `crabbox vnc` (delegated execution model).
- Supports `warmup`, `run`, `status`, `stop`, `list`, and `doctor`.

## Live smoke

Run the guarded hosted lifecycle smoke with an exported API key:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=smolvm scripts/live-smoke.sh
```

That top-level smoke dispatches to the provider-specific script:

```sh
CRABBOX_SMOLVM_LIVE_SMOKE=1 scripts/live-smolvm-smoke.sh
```

The smoke creates one uniquely named sandbox, verifies initial archive sync and
environment forwarding, reuses it for a second sync that adds, updates, and
deletes files, checks nonzero command exit propagation, runs status/list/doctor,
then stops the sandbox and verifies that it disappeared from inventory. An exit
trap retries targeted cleanup by the unique slug up to three times if any
intermediate step fails, treats a confirmed absent slug as already clean, and
returns failure if cleanup remains blocked. The smoke leaves SmolVM's default
open network enabled because a cold worker may need to pull the configured image
during the first exec.

## Limitations

- Output from runs appears after the command completes (the smolfleet API provides no live stream for delegated exec).
- Workspace sync is performed via a direct API call (`/exec` with base64 tar in a heredoc).
- Delegated-run restrictions apply (no SSH surface, no rsync, certain advanced sync/run flags are rejected).

## Gotchas

- `--class` and `--type` are rejected; sizing and image are controlled via `--smolvm-*` flags (and the base image).
- `--checksum` is rejected because SmolVM has no SSH/rsync target. Large-sync guardrails still apply; `--force-sync-large` may be honored where supported for intentional large archive syncs.
- Use `--sync-only` to pre-upload the archive into a kept sandbox before a later command (subject to delegated guardrails).
- Delegated run/sync options that need an SSH target or proof surface are rejected: `--script` / `--script-stdin`, `--fresh-pr`, `--full-resync`, `--env-helper`, `--capture-stdout` / `--capture-stderr`, `--capture-on-fail`, `--download`, `--artifact-glob`, `--emit-proof`, and `--stop-after`.
- IDs can be a Crabbox slug, a `cbx_...` lease ID, or a raw SmolVM identifier/name; stop and reuse require an unambiguous exact local claim, not just a Crabbox-looking name.
- Forwarded environment values use an independently named profile in the validated workdir. The shared profile owner retains cleanup responsibility after a failed upload; cleanup runs before one-shot teardown with a fresh bounded context, checking the original local claim and native machine identity. Changed ownership retains the remote file and warns rather than authorizing stale cleanup. The source check uses the existing POSIX shell and does not require Bash or change user-command errexit behavior.
- The direct archive sync sends the (base64) tar inside the `/exec` command body. Very large repos may hit request size limits (the usual preflight checks still apply).

## Related docs

- [Provider backends](../provider-backends.md)
