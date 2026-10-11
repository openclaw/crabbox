# Getting Started

Crabbox gives coding agents and humans on-demand computers. Keep editing in
your local checkout; Crabbox leases a box, syncs your working tree including
uncommitted changes, runs your command, streams output, and returns its exit
code. A one-shot run releases the box; a warm lease stays available for the next
run.

Remote tests and builds free your laptop's CPU, memory, and ports so multiple
agents can work in parallel. Choose Linux, macOS, native Windows, or WSL2 on a
provider that supports the target. For visible work, see the
[desktop, VNC, browser, Code, and portal guide](features/interactive-desktop-vnc.md).

This walkthrough takes one trusted repo from installation to
`crabbox run -- pnpm test`. Review its config and setup commands before running
them. For a first run with no account, `--provider local-container` executes
against Docker or Podman on your own machine; the
[`crabbox-quickstart` skill](integrations/agents.md#install-through-ecosystem-skill-managers)
walks that credential-free path end to end.

## Step 1. Install

```sh
brew install openclaw/tap/crabbox
```

Verify the install:

```sh
crabbox --version
crabbox doctor
```

`crabbox doctor` prints one line per check. On a fresh install, its
provider-neutral `git` check should report `ok`; provider-specific tool checks
begin after you select a provider. Doctor reports `no provider selected`, keeps
the compatibility metadata as `source=compiled_default selected=false`, and
skips provider credential readiness without failing the command. Select a
provider through config, `CRABBOX_PROVIDER`, or `--provider` before a lifecycle
command. That selection is strict and can fail until its credentials are
configured; run `crabbox providers recommend` to compare options.

If you do not use Homebrew, GitHub Releases ship platform archives for macOS,
Linux, and Windows, with signed and notarized macOS executables. Download the matching archive from
<https://github.com/openclaw/crabbox/releases>.

Keep any included `crabbox-runtime` directory beside the real CLI executable.
Both Linux runtime architectures belong to the matched controller; copying only
the CLI or mixing releases does not preserve the complete runtime pack.

Go users can install only the CLI from an explicit release version. Choose a
tag from [GitHub Releases](https://github.com/openclaw/crabbox/releases); this
channel is supported starting with v0.44.0. For example:

```sh
go install github.com/openclaw/crabbox/cmd/crabbox@v0.70.0
```

The module requires Go 1.26 and declares go1.26.5 as its preferred toolchain;
use Go 1.26.5 or newer, or leave automatic toolchain selection enabled. This
command compiles the Go CLI locally. It does not install release companion
executables or assets, including `crabbox-apple-vm-helper` and the runtime pack, and it is not the
signed/notarized prebuilt distribution. Use Homebrew or a release archive for
the complete platform distribution, notably Apple VM support on Apple Silicon.

Supported WSL2 and x64 no-WSL Windows transfer selection requires a build from
current `main` or Crabbox v0.42.1 and newer. See the
[supported Windows installation](windows-install.md) for the complete setup.

## Step 2. Log In

For a shared fleet, use your team's coordinator URL. It holds provider
credentials and enforces lease limits, spend caps, and expiry. Direct cloud and
local providers skip login; see [Choosing an access path](#choosing-an-access-path).

```sh
crabbox login --url https://broker.example.com
```

`login` opens a browser to the GitHub OAuth flow (pass `--no-browser` to print
the URL for a browser on the same device). The broker exchanges the OAuth code,
verifies your GitHub org membership, and redirects a one-use confirmation to the
CLI's loopback listener before writing the signed token to your user config:

```text
logged in broker=https://broker.example.com provider=hetzner user=github:12345 org=example-org config=/Users/alice/Library/Application Support/crabbox/config.yaml
```

From then on, every `crabbox` command authenticates automatically. Check your
identity any time:

```sh
crabbox whoami
```

```text
user=github:12345 org=example-org auth=user broker=https://broker.example.com
```

### Choosing An Access Path

Broker access is deployment-specific. Use the coordinator URL and GitHub
org/team allowlist from your team. A completed GitHub OAuth flow can still be
rejected when your account is outside that allowlist.

For a personal or third-party installation, pick one path:

- **Direct-provider mode** - bring your own local cloud credentials when you want
  a quick private test lane and can accept local cleanup and state instead of
  broker usage history and shared spend caps.
- **Self-hosted coordinator** - deploy on Cloudflare Workers or run the portable
  Node.js/PostgreSQL service when you want coordinator-owned provider
  credentials, active-lease limits, monthly spend caps, `crabbox usage`,
  durable cleanup, and a shared team endpoint.
- **Request access** - only when the broker operator has a defined onboarding
  path for your org. A team endpoint is not automatically an open broker.

Direct-provider mode skips `login` entirely:

```sh
crabbox doctor --provider hetzner
crabbox run --provider hetzner -- pnpm test
```

Self-hosting starts by choosing a runtime:

- **Cloudflare Workers** - Durable Object state, alarms, scheduled cleanup, and
  optional Cloudflare Access.
- **Node.js/PostgreSQL** - an ordinary HTTP/WebSocket service, PostgreSQL 13+
  state, pg-boss maintenance jobs, and one always-on replica. Treat it as the
  initial portable runtime and complete the deployment proof before production
  cutover.

Both use the same provider secrets, auth config, budget limits, API, and portal.
See [Infrastructure](infrastructure.md#choose-a-coordinator-runtime). Browser
login needs a GitHub OAuth app and at least one allowed org/team; shared-token
automation does not.

For CI environments that cannot open a browser, use shared-token auth:

```sh
printf '%s' "$TOKEN" | crabbox login \
  --url https://broker.example.com \
  --provider aws \
  --token-stdin
```

See [Auth and admin](features/auth-admin.md) for the full identity model.

## Step 3. Onboard A Repo

Inside the repo:

```sh
crabbox init
```

`init` writes three files (override paths with `--config`, `--workflow`, or one
or more `--skill` flags; pass `--force` to overwrite existing files):

```text
.crabbox.yaml                          repo defaults (profile, class, sync, env)
.github/workflows/crabbox.yml          Actions hydration workflow (optional)
.agents/skills/crabbox/SKILL.md        agent-facing skill instructions
```

The generated `.crabbox.yaml` ships sensible defaults. Adjust the parts that
matter for your repo:

- `profile`: a name for this lane (the template uses `<repo>-check`);
- `class`: `tiny`, `small`, `standard`, `fast`, `large`, or `beast` (the template uses `beast`);
- `sync.exclude`: directories that should never be sent to the runner;
- `sync.include`: an optional root-relative whitelist — when set, **only** these
  paths are synced (after excludes), so you can ship a few paths out of a large
  repo instead of blacklisting everything else;
- `env.allow`: environment variables the remote command is allowed to see.

Pass `--detect` to scan the repo for test commands and write a `jobs.detected`
entry you can run with `crabbox job run detected`.

Then preview what a sync would send:

```sh
crabbox sync-plan
```

`sync-plan` prints the file count, total bytes, and the biggest files in the
manifest. If it shows surprises (a `dist/` folder, a forgotten `.cache/`, a
multi-gigabyte asset), tighten `sync.exclude` and re-run. The first sync to a
fresh runner is bound by this size.

## Step 4. Warm A Box

```sh
crabbox warmup --class standard --slug first-tests
```

`warmup` acquires a lease, provisions the runner, waits for SSH and tooling to
come up, keeps the lease (`--keep`, on by default), and prints a lease summary,
ready endpoint, and completion time:

```text
leased cbx_abcdef123456 slug=first-tests provider=hetzner server=... type=... ip=203.0.113.10 idle_timeout=30m0s expires=...
ready ssh=crabbox@203.0.113.10:2222 network=public workroot=/work/crabbox
warmup complete total=...
```

The coordinator now keeps the lease ready for commands. Its idle timeout
(default 30m) and TTL (default 90m) bound its life. Direct providers have their
own expiry enforcement; always stop a box explicitly when done. Machine classes
are provider-specific: inspect `crabbox providers --json` before choosing a size.

Reuse the lease by `slug` (friendly) or `id` (the `cbx_...` handle). Both work
with `--id` on later commands.

## Step 5. Run A Command

```sh
crabbox run --id first-tests -- pnpm test
```

What happens:

1. The CLI verifies SSH readiness on the lease.
2. It seeds the remote Git tree from your origin and base ref, then rsyncs the
   dirty working tree on top (a fingerprint short-circuit skips sync when
   nothing changed).
3. It runs the command over SSH, streaming stdout and stderr.
4. It heartbeats the broker so the lease does not idle out mid-run.
5. It records a `run_...` history entry with sync time, command time, exit code,
   and (on Linux) bounded telemetry samples.

The box supplies execution plumbing; your repo still owns tool and dependency
installation. If needed, run your setup script or
[hydrate from Actions](features/actions-hydration.md) before the tests.

You can omit `--id` for a one-shot run:

```sh
crabbox run -- pnpm test
```

That acquires a fresh lease, runs the command, and releases the lease when the
command exits. Use one-shot for ad-hoc tests; use `warmup` + `--id` for
iterative work on the same box.

## Step 6. Inspect History

```sh
crabbox history
crabbox events run_abcdef123456
crabbox logs run_abcdef123456
crabbox results run_abcdef123456
```

`history` lists recent runs. `events` prints the ordered event stream (lease,
sync, command, output chunks, finish). `logs` returns the retained command
output. `results` parses any JUnit reports the run attached.

If your broker has the portal enabled, `/portal/runs/run_abcdef123456` renders
the same data as a browser page.

## Step 7. Stop The Lease

When you are done:

```sh
crabbox stop first-tests
```

`stop` releases the lease through its provider's cleanup contract and frees
broker-reserved cost. Most disposable providers delete the machine; static SSH
hosts remain, and retained resources follow provider policy. Cleanup requires
verified ownership. Recovery claims or fixed-ID terminal receipts may remain
locally. If you forget a brokered box, the coordinator schedules expiry cleanup.

```sh
crabbox cleanup --dry-run
```

`cleanup` sweeps direct-provider leftovers and local state. It is for the
direct-provider path; brokered cleanup is the broker alarm's job.

## Common Variations

Keep a lease alive across a longer session:

```sh
crabbox warmup --idle-timeout 4h --ttl 8h --slug swift-crab
crabbox run --id swift-crab -- pnpm test
crabbox run --id swift-crab -- pnpm bench
crabbox stop swift-crab
```

Open a desktop session:

```sh
crabbox warmup --desktop --browser --slug ui-check
crabbox webvnc --id ui-check --open --take-control
```

Open a code-server tab:

```sh
crabbox warmup --code --slug code-check
crabbox run --id code-check --sync-only
crabbox code --id code-check --open
```

Keep each bridge command running while using its browser tab. Desktop and Code
support depend on the provider and target; browser Code also needs a configured
coordinator and isolated Code origin. The
[combined access guide](features/interactive-desktop-vnc.md) explains native VNC,
local WebVNC, the portal, sharing, and cleanup.

Open the synced checkout in Zed Remote Projects:

```sh
crabbox run --id swift-crab --sync-only
crabbox open --editor=zed --id swift-crab
```

Paste the printed SSH command into Zed's **Connect New Server** dialog, open the
printed folder, and keep `crabbox open` running while you work.

Use a Mac you already own (static SSH, no provisioning):

```yaml
# .crabbox.yaml
provider: ssh
target: macos
static:
  host: mac-studio.local
  user: alice
  port: "22"
  workRoot: /Users/alice/crabbox
```

```sh
crabbox run -- xcodebuild test
```

Override the configured default provider and class per command:

```sh
crabbox run --provider aws --class beast -- pnpm test
```

## Where To Go Next

- [How Crabbox Works](how-it-works.md) - the mental model.
- [CLI](cli.md) - the full command surface and exit codes.
- [Commands](commands/README.md) - one page per command.
- [Features](features/README.md) - one page per feature.
- [Configuration](features/configuration.md) - YAML schema and precedence.
- [Providers](providers/README.md) - which provider to pick.
- [Provider authoring](features/provider-authoring.md) - add a new provider.
- [Troubleshooting](troubleshooting.md) - what to do when a step fails.
