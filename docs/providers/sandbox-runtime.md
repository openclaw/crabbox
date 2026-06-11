# Sandbox Runtime Provider

Read when:

- choosing `provider: sandbox-runtime` or alias `srt`;
- running local commands through Anthropic Sandbox Runtime;
- changing `internal/providers/sandboxruntime`.

Sandbox Runtime is a local delegated-run provider. Crabbox shells out to the
`srt` CLI for one-shot command execution, while SRT owns the operating-system
sandbox, filesystem policy, and network policy. Crabbox owns provider
selection, config, flags, environment forwarding, output streaming, timing
summaries, and doctor/readiness reporting.

This provider is local sandboxing, not a remote lease. It does not create a VM,
does not SSH anywhere, and does not keep persistent Crabbox resources.

## When To Use

Use `sandbox-runtime` when you want to run the current checkout through SRT's
native macOS or Linux sandboxing primitives:

```sh
crabbox run --provider sandbox-runtime -- pnpm test
crabbox run --provider srt --sandbox-runtime-settings .crabbox/srt-settings.json -- go test ./...
crabbox doctor --provider sandbox-runtime
```

Use an SSH-lease provider such as `aws`, `hetzner`, `local-container`, or
`ssh` when you need Crabbox-managed sync, remote compute, SSH access, desktop,
browser, code-server, ports, copies, persistent IDs, or Actions hydration.

## Prerequisites

- Install Anthropic Sandbox Runtime so `srt` is on `PATH`, or configure
  `--sandbox-runtime-cli` / `sandboxRuntime.cliPath`.
- Configure SRT permissions in `~/.srt-settings.json`, or pass a specific
  settings file with `--sandbox-runtime-settings`.
- The host must satisfy SRT platform prerequisites:
  - macOS uses `sandbox-exec` and generated Seatbelt profiles.
  - Linux uses `bubblewrap` and proxy-based network filtering.
- Windows is deferred for this Crabbox provider. The SRT source contains
  Windows helpers, but the current SRT README still marks Windows support as
  not ready for this acceptance surface.

## Commands

```sh
crabbox doctor --provider sandbox-runtime
crabbox run --provider sandbox-runtime -- echo ok
crabbox run --provider srt --sandbox-runtime-debug -- npm test
crabbox run --provider sandbox-runtime --sandbox-runtime-settings .crabbox/srt-settings.json -- sh -lc 'printf ok'
```

Crabbox invokes SRT as:

```text
srt [--debug] [--settings <path>] -c <command>
```

The command text is built from the Crabbox argv using the same shell quoting
helpers used for other shell-backed run surfaces. Selected `--allow-env` and
`--env-from-profile` values are passed through the subprocess environment, not
embedded in command text or argv.

## Config

```yaml
provider: sandbox-runtime
sandboxRuntime:
  cliPath: srt
  settings: "" # empty means SRT default ~/.srt-settings.json
  debug: false
```

Provider flags:

```text
--sandbox-runtime-cli <path>
--sandbox-runtime-settings <path>
--sandbox-runtime-debug
```

Environment overrides:

```text
CRABBOX_SANDBOX_RUNTIME_CLI
CRABBOX_SANDBOX_RUNTIME_SETTINGS
CRABBOX_SANDBOX_RUNTIME_DEBUG
```

Precedence follows the normal Crabbox order:

```text
flags > env > repo config > user config > defaults
```

Crabbox validates only its own config shape, such as a non-empty `cliPath`.
SRT owns validation of the settings JSON schema and sandbox policy. Keep
trusted, machine-specific settings in user config when they grant broad local
filesystem or network access.

## Lifecycle

`sandbox-runtime` is one-shot:

1. `run` executes one local command with `srt -c <command>`.
2. `doctor` checks the local SRT command surface with a non-mutating `srt --help`
   probe. `srt --version` is reported only as informational compatibility
   context, because SRT can fall back to package metadata defaults.
3. `list` returns no leases because Crabbox owns no persistent SRT resources.
4. `warmup`, `status`, and `stop` return clear one-shot unsupported messages.

The provider omits `run-session`, so `--lease-output` remains unsupported.
Global SSH, sync, desktop, browser, code-server, Tailscale, pool, fresh-PR,
script upload, downloads, capture stdout/stderr, and remote artifact flags are
rejected through delegated-provider validation.

## Capabilities

- Kind: delegated-run.
- Family: `sandbox-runtime`.
- Canonical provider: `sandbox-runtime`.
- Alias: `srt`.
- Targets: macOS and Linux.
- Coordinator: never. SRT always runs locally through the CLI.
- SSH: unsupported.
- Crabbox rsync/archive sync: unsupported.
- Desktop, browser, code-server, VNC, Tailscale, ports, copies, and checkpoints:
  unsupported.
- Persistent lifecycle and reusable lease IDs: unsupported.

## Live Smoke

Run the scripted live proof on a host with SRT installed:

```sh
scripts/live-sandbox-runtime-smoke.sh
```

The script builds `bin/crabbox`, runs `doctor`, verifies a one-shot
`echo ok`, then creates a temporary SRT settings file that:

- allows writing inside one temporary directory;
- denies reading a temporary secret file;
- denies all network access with `network.allowedDomains: []`.

It then proves:

- allowed temp write/read succeeds;
- denied secret read fails;
- denied network access fails.

Expected pass classification:

```text
classification=live_sandbox_runtime_smoke_passed cleanup=complete
```

If `srt`, `curl`, or host sandbox prerequisites are unavailable, the script
prints `classification=environment_blocked` with the failing command and exit
code. Treat that as an honest live-proof blocker, not as a passing isolation
result.
