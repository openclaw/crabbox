# 🦀 📦 Crabbox

[![CI](https://github.com/openclaw/crabbox/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/openclaw/crabbox/actions/workflows/ci.yml)
[![Release](https://github.com/openclaw/crabbox/actions/workflows/release.yml/badge.svg)](https://github.com/openclaw/crabbox/actions/workflows/release.yml)
[![Latest release](https://img.shields.io/github/v/release/openclaw/crabbox?sort=semver)](https://github.com/openclaw/crabbox/releases/latest)

**Warm a box, sync the diff, run the suite.**

Crabbox is an open-source agent workspace control plane for maintainers and AI
agents. Lease fast managed cloud capacity, point at an existing SSH host, or use
an agent sandbox provider, then sync your dirty checkout, run commands remotely,
stream output, collect evidence, and release. Local edit-save-run loop,
cloud-grade compute, agent-ready observability.

```sh
crabbox run -- pnpm test
```

Behind that single command: a Go CLI on your laptop, a Cloudflare Worker broker
that owns provider credentials and lease state, and a managed or delegated
runner.

Supported providers:

- [AWS EC2](docs/providers/aws.md) (`provider: aws`): brokered or direct Linux,
  native Windows, Windows WSL2, and EC2 Mac.
- [Azure](docs/providers/azure.md) (`provider: azure`): brokered or direct
  Linux and native Windows VMs.
- [Google Cloud](docs/providers/gcp.md) (`provider: gcp`): brokered or direct
  Linux Compute Engine VMs.
- [Hetzner Cloud](docs/providers/hetzner.md) (`provider: hetzner`): brokered or
  direct Linux VMs.
- [Proxmox](docs/providers/proxmox.md) (`provider: proxmox`): direct Linux QEMU
  VM clones from private Proxmox VE templates.
- [Static SSH](docs/providers/ssh.md) (`provider: ssh`): existing Linux, macOS,
  Windows, or WSL2 hosts.
- [Blacksmith Testbox](docs/providers/blacksmith-testbox.md)
  (`provider: blacksmith-testbox`): delegated Testbox lifecycle and execution.
- [Namespace Devbox](docs/providers/namespace-devbox.md)
  (`provider: namespace-devbox`): Namespace-managed Devboxes over SSH.
- [Semaphore CI testbox](docs/providers/semaphore.md) (`provider: semaphore`):
  Semaphore jobs leased as SSH testboxes.
- [Sprites](docs/providers/sprites.md) (`provider: sprites`): Sprites
  microVMs exposed as SSH leases through `sprite proxy`.
- [Daytona](docs/providers/daytona.md) (`provider: daytona`): Daytona
  SDK/toolbox sandbox execution.
- [Islo](docs/providers/islo.md) (`provider: islo`): delegated Islo sandbox
  execution.
- [E2B](docs/providers/e2b.md) (`provider: e2b`): delegated E2B sandbox
  execution.

---

## Install

```sh
brew install openclaw/tap/crabbox
crabbox --version
```

No Homebrew? Grab a [GoReleaser archive](https://github.com/openclaw/crabbox/releases) for macOS, Linux, or Windows.

Prerequisites on the laptop: `git`, `ssh`, `ssh-keygen`, `rsync`, `curl`.

## Quick start

```sh
# log in once per machine (stores a broker token in user config)
crabbox login

# verify local prerequisites and broker reachability
crabbox doctor

# one-shot: lease, sync, run, release
crabbox run -- pnpm test

# named repo workflow from .crabbox.yaml
crabbox job run full-ci

# or warm a box once, then reuse it
crabbox warmup                                       # prints cbx_... + a slug
crabbox run --id blue-lobster -- pnpm test:changed
crabbox ssh --id blue-lobster
crabbox stop blue-lobster
```

Every lease has a stable `cbx_...` ID and a friendly crustacean slug (`blue-lobster`, `swift-hermit`, …). Either works wherever an `--id` is accepted.

## How it works

```text
your laptop                Cloudflare Worker            cloud provider
-------------              ------------------           --------------
crabbox CLI    -- HTTPS --> Fleet Durable Object  -->   Hetzner / AWS / Azure / GCP
   |                         lease + cost state              |
   |                                                         |
   +------------ SSH + rsync to leased runner <--------------+
```

- **CLI** — Go binary. Loads config, mints a per-lease SSH key, asks the broker for a lease, waits for SSH, seeds remote Git, rsyncs the dirty checkout (with fingerprint skip when nothing changed), runs the command, streams output, releases.
- **Broker** — Cloudflare Worker at `crabbox.openclaw.ai` plus a single Durable Object. Owns provider credentials, serializes lease state, enforces active-lease and monthly spend caps, and expires stale leases by alarm. Auth is GitHub login or a shared bearer token.
- **Runner** — a throwaway SSH machine prepared with SSH on the primary port, default `2222`, plus configured fallback ports and Crabbox's sync/run prerequisites. Linux uses Ubuntu with cloud-init and `/work/crabbox`; native Windows uses OpenSSH, Git for Windows, and `C:\crabbox`. No broker credentials live on the box. Project runtimes (Go, Node, Docker, services, secrets) come from your repo's GitHub Actions hydration, devcontainer, Nix, mise/asdf, or setup scripts — not from Crabbox.

A direct-provider mode (`--provider hetzner|aws|azure|gcp|proxmox` with local credentials) exists for debugging the broker itself or using private infrastructure; the brokered path is the default where supported.

For the full mental model, see [How Crabbox Works](docs/how-it-works.md). For the doc-to-code map, see [Source Map](docs/source-map.md).

## Highlights

- **One-shot or warm workspaces.** `crabbox run` for fire-and-forget; `crabbox warmup` + `--id` for repeated runs against the same box.
- **Named repo jobs.** `crabbox job run <name>` lets repos define warmup, optional Actions hydration, run command, and cleanup policy in `.crabbox.yaml`.
- **Run observability.** Every coordinator-backed run gets an early `run_...` handle. Use `crabbox attach <run-id>` while it is active, `crabbox events <run-id> --after <seq> --limit <n>` for durable lifecycle/output events, and `crabbox logs <run-id>` for retained output after completion.
- **Stable timing records.** `--timing-json` on `run`, `warmup`, and `actions hydrate` gives scripts one machine-readable sync/command/total timing schema across AWS, Hetzner, and Blacksmith Testboxes.
- **Local-first workspace sync.** No clean-checkout requirement. Tracked + nonignored files only, fingerprint skip on no-op runs, sanity checks against suspicious mass deletions, optional shallow base-ref hydration for changed-test workflows.
- **Brokered cloud.** Maintainers and agents share infra without sharing provider tokens. Hetzner, AWS EC2, Azure, and Google Cloud are managed providers; AWS also owns Windows WSL2 and EC2 Mac targets. Linux defaults to Spot unless capacity config says otherwise. Providers fall back across compatible instance families when capacity or quota rejects a request.
- **Azure Linux and native Windows.** `provider: azure` provisions Linux and native Windows VMs in a configurable Azure subscription using `DefaultAzureCredential` in direct mode or service-principal secrets in the broker. Crabbox creates a shared resource group, vnet, subnet, and NSG on first use, then per-lease public IPs, NICs, and VMs. Linux uses cloud-init; Windows uses VM Agent Custom Script Extension to install OpenSSH/Git and configure the Crabbox user.
- **macOS and Windows static hosts.** `provider: ssh` reuses existing machines; it does not create macOS or Windows Crabbox boxes. macOS and Windows WSL2 use the POSIX rsync path; native Windows uses PowerShell plus tar archive sync.
- **Blacksmith Testbox wrapper.** Set `provider: blacksmith-testbox` to delegate warmup/run/list/status/stop to the Blacksmith CLI while Crabbox keeps local slugs, repo claims, timing summaries, config conventions, and portal visibility for active external runners.
- **Namespace Devbox SSH leases.** Set `provider: namespace-devbox` to create or reuse Namespace Devboxes through the `devbox` CLI, then let Crabbox sync the dirty checkout and run commands over SSH.
- **Semaphore CI testbox.** Set `provider: semaphore` to lease a Semaphore CI job as a testbox. Same environment as your real pipelines.
- **Proxmox VM clones.** Set `provider: proxmox` to clone Linux QEMU templates on a private Proxmox VE cluster, bootstrap them through the QEMU guest agent, and use normal Crabbox SSH sync/run/cleanup.
- **Sprites SSH leases.** Set `provider: sprites` to create a Sprites microVM, bootstrap OpenSSH inside it, and let Crabbox sync/run through `sprite proxy` with `crabbox ssh` support.
- **Daytona, Islo, and E2B sandboxes.** Set `provider: daytona` for Daytona SDK/toolbox execution from a snapshot with explicit SSH access when needed, `provider: islo` for delegated Islo sandbox execution through the Islo Go SDK, or `provider: e2b` for delegated E2B sandbox execution through E2B sandbox APIs.
- **Trusted AWS images.** Operators can create AMIs from active brokered AWS leases and promote a known-good image as the coordinator default.
- **Cost guardrails.** Per-lease and monthly spend caps. Live pricing from EC2 Spot history or Hetzner server-type prices, with static fallbacks. `crabbox usage` summarizes spend by user, org, provider, and type.
- **GitHub Actions hydration.** `crabbox actions hydrate` registers a leased box as an ephemeral Actions runner, so the repo's own workflow installs runtimes, services, and secrets. Crabbox does not parse Actions YAML.
- **Interactive desktop and browser leases.** `--browser` provisions Chrome or Chromium for headless automation, `--desktop` provisions visible UI with tunnel-only VNC takeover on managed Linux, AWS native Windows, and AWS EC2 Mac targets. `crabbox desktop doctor` checks session, VNC, input tooling, browser, ffmpeg, screen size, screenshot capture, and WebVNC portal state; `desktop click/paste/type/key` provide first-class input helpers so agents do not hand-roll brittle `xdotool` snippets. `desktop proof` launches a terminal smoke and captures metadata, screenshot, diagnostics, MP4, and a contact-sheet PNG in one bundle that can be published to a PR; MP4 capture is Linux/native Windows only for now. QA systems such as Mantis own scenario logic, screenshots, and PR evidence. Azure native Windows is SSH/sync/run only; use AWS for managed Windows desktop/WSL2 or `provider: ssh` for an existing Windows host.
- **Authenticated web portal.** Browser login opens owner-scoped and explicitly shared lease/run views with searchable, paginated tables, muted external-runner rows, compact provider/OS/access icons, relative sortable times, recent run logs/events, WebVNC, code-server, and Linux lease/run telemetry charts. `crabbox share` can grant a lease to one user or the owning org, and the lease page exposes the same sharing controls for owners/managers. WebVNC is preferred for human demos because it preloads the VNC password; `webvnc status` reports local daemon, tunnel, target reachability, bridge/viewer state, recent events, URL/password, and native VNC fallback, while `webvnc reset` restarts only the selected lease's WebVNC/input stack. Admin sessions can also see non-owned runner leases behind `mine`/`system` filters.
- **Agent workspace evidence.** History, logs, events, telemetry, JUnit summaries, screenshots, recordings, artifacts, and PR publishing make autonomous work reviewable instead of only ephemeral terminal output.
- **Hardened coordinator auth.** GitHub browser login, owner-scoped leases, admin-only routes, optional GitHub team allowlists, Cloudflare Access JWT verification, and service-token support keep normal use and operator automation separate.
- **OpenClaw plugin.** The repo root is a native OpenClaw plugin for box lifecycle operations: `crabbox_run`, `crabbox_warmup`, `crabbox_status`, `crabbox_list`, and `crabbox_stop`. Run inspection stays in the CLI and Crabbox skill.
- **Operator surface.** `doctor`, `init`, `status`, `inspect`, `list`, `usage`, `history`, `logs`, `results`, `cache`, `admin`, `cleanup`, plus `--json` output where it matters. Brokered `doctor` checks provider secret readiness before users discover missing Worker config through a failed lease.

## Machine classes

`beast` is the default. Both providers fall back across an ordered list of instance types.

```text
Hetzner    standard  ccx33, cpx62, cx53
           fast      ccx43, cpx62, cx53
           large     ccx53, ccx43, cpx62, cx53
           beast     ccx63, ccx53, ccx43, cpx62, cx53

AWS Linux  standard  c7a/c7i/m7a/m7i.8xlarge family
           fast      …16xlarge family
           large     …24xlarge family
           beast     …48xlarge family, falling back to 32x/24x/16x

AWS Win    standard  m7i.large, m7a.large, t3.large
           fast      m7i.xlarge, m7a.xlarge, t3.xlarge
           large     m7i.2xlarge, m7a.2xlarge, t3.2xlarge
           beast     m7i.4xlarge, m7a.4xlarge, m7i.2xlarge

AWS WSL2   standard  m8i.large, m8i-flex.large, c8i.large, r8i.large
           fast      m8i.xlarge, m8i-flex.xlarge, c8i.xlarge, r8i.xlarge
           large     m8i.2xlarge, m8i-flex.2xlarge, c8i.2xlarge, r8i.2xlarge
           beast     m8i.4xlarge, m8i-flex.4xlarge, c8i.4xlarge, r8i.4xlarge, m8i.2xlarge

AWS macOS  all       mac2.metal unless --type is set

Azure      standard  Standard_D32ads_v6, Standard_D32ds_v6, Standard_F32s_v2, then 16-vCPU fallbacks
           fast      Standard_D64ads_v6, Standard_D64ds_v6, Standard_F64s_v2, then 48/32-vCPU fallbacks
           large     Standard_D96ads_v6, Standard_D96ds_v6, then 64/48-vCPU fallbacks
           beast     Standard_D192ds_v6, Standard_D128ds_v6, then 96/64-vCPU fallbacks

Azure Win  standard  Standard_D2ads_v6, Standard_D2ds_v6, Standard_D2ads_v5, Standard_D2ds_v5, Standard_D2as_v6
           fast      Standard_D4ads_v6, Standard_D4ds_v6, Standard_D4ads_v5, Standard_D4ds_v5, Standard_D4as_v6
           large     Standard_D8ads_v6, Standard_D8ds_v6, Standard_D8ads_v5, Standard_D8ds_v5, Standard_D8as_v6
           beast     Standard_D16ads_v6, Standard_D16ds_v6, Standard_D16ads_v5, Standard_D16ds_v5, Standard_D8ads_v6

Namespace  standard  S
           fast      M
           large     L
           beast     XL
```

Override with `--type` or `CRABBOX_SERVER_TYPE` for a specific instance.

## Configuration

Config resolves in order: flags → env → repo `.crabbox.yaml` → user `~/.config/crabbox/config.yaml` → defaults.

```yaml
broker:
  url: https://crabbox.openclaw.ai
  provider: aws
  token: ...
class: beast
capacity:
  market: spot
  strategy: most-available
  fallback: on-demand-after-120s
  hints: true
aws:
  region: eu-west-1
  rootGB: 400
lease:
  idleTimeout: 30m
  ttl: 90m
ssh:
  key: ~/.ssh/id_ed25519
  user: crabbox
  port: "2222"
  # Ordered fallback ports tried after ssh.port; use [] to disable fallback.
  fallbackPorts:
    - "22"
```

Optional Blacksmith Testbox wrapper:

```yaml
provider: blacksmith-testbox
blacksmith:
  org: openclaw
  workflow: .github/workflows/ci-check-testbox.yml
  job: test
  ref: main
  idleTimeout: 90m
```

`crabbox list --provider blacksmith-testbox` also refreshes muted external
runner rows in the portal lease table from the current all-status Testbox list
when coordinator auth is configured. When GitHub is reachable, Crabbox also
links those rows back to the inferred Actions run and workflow, surfaces the
Actions status/conclusion, flags long-queued or long-running rows as `stuck`,
and exposes a copyable local `crabbox stop --provider blacksmith-testbox ...`
command. Clicking an external row opens a visibility-only runner detail page
with owner, workflow, timestamps, boundary notes, and the same stop command.
Those rows are visibility-only records for Blacksmith-owned Testboxes, not
Crabbox leases.

Optional Namespace Devbox:

```yaml
provider: namespace-devbox
namespace:
  image: builtin:base
  size: M
  workRoot: /workspaces/crabbox
```

Optional Daytona sandbox:

```yaml
provider: daytona
daytona:
  snapshot: crabbox-ready
  workRoot: /home/daytona/crabbox
```

Optional Islo sandbox:

```yaml
provider: islo
islo:
  image: docker.io/library/ubuntu:24.04
  workdir: crabbox
```

Optional E2B sandbox:

```yaml
provider: e2b
e2b:
  template: base
  workdir: crabbox
```

Optional Semaphore CI testbox:

```yaml
provider: semaphore
semaphore:
  host: myorg.semaphoreci.com
  project: my-app
  machine: f1-standard-2
  osImage: ubuntu2204
  idleTimeout: 30m
```

Keep the token in `CRABBOX_SEMAPHORE_TOKEN` or `SEMAPHORE_API_TOKEN`, not in
repo config.

Optional Sprites microVM:

```yaml
provider: sprites
sprites:
  workRoot: /home/sprite/crabbox
```

Keep the token in `CRABBOX_SPRITES_TOKEN`, `SPRITES_TOKEN`, `SPRITE_TOKEN`, or
`SETUP_SPRITE_TOKEN`; the authenticated `sprite` CLI must also be on `PATH`.

Optional static macOS or Windows target:

```yaml
provider: ssh
target: windows
windows:
  mode: normal # or wsl2
static:
  host: win-dev.local
  user: Peter
  port: "22"
  workRoot: C:\crabbox
```

OpenClaw WSL2 test helper:

```sh
CRABBOX_LIVE=1 scripts/openclaw-wsl2-tests.sh
CRABBOX_LIVE=1 CRABBOX_OPENCLAW_WSL2_ID=blue-lobster scripts/openclaw-wsl2-tests.sh
```

Optional Tailscale reachability for managed Linux leases:

```yaml
tailscale:
  enabled: true
  network: auto
  tags:
    - tag:crabbox
  hostnameTemplate: crabbox-{slug}
  authKeyEnv: CRABBOX_TAILSCALE_AUTH_KEY
  exitNode: mac-studio.example.ts.net
  exitNodeAllowLanAccess: true
```

Tailscale is a network plane, not a provider. `--tailscale` joins new managed
Linux leases to the tailnet; `--network auto|tailscale|public` chooses how SSH
and VNC tunnel commands resolve the host. Brokered mode uses Worker OAuth
secrets to mint one-off keys; direct-provider mode reads the auth key from the
configured env var. `exitNode` is opt-in per lease for routing outbound internet
through an approved tailnet exit node. See [Tailscale](docs/features/tailscale.md).

Forwarded environment is intentionally narrow: `NODE_OPTIONS` and `CI`. Do not pass secrets as command-line arguments. Full env-var reference and per-command flags are in [docs/cli.md](docs/cli.md) and [docs/commands/](docs/commands/README.md).

For live-secret smoke tests, use `crabbox run --env-from-profile <file>
--allow-env NAME` so Crabbox forwards only selected names and prints redacted
presence/length metadata. For larger commands, use `--script <file>` or
`--script-stdin` so the remote runner executes an uploaded file instead of a
giant quoted shell string.
Delegated providers may own their command transport. Blacksmith Testbox cannot
forward CLI-side env values; Crabbox prints an explicit unsupported warning and
the workflow should provide required secrets.

For binary or terminal-hostile output, use `crabbox run --capture-stdout <path>`
or `--capture-stderr <path>` so remote streams are written directly to local
files and omitted from retained run-log previews. Add `--preflight` for a
remote capability snapshot, `--keep-on-failure` to SSH into the exact failed
one-shot lease, or `--download remote=local` to copy a successful-run artifact
back. Failed SSH-backed and Blacksmith delegated runs save local
`.crabbox/captures/*.tar.gz` bundles by default. Captured files are not redacted
by Crabbox.

## OpenClaw plugin

The repo root is a native OpenClaw plugin package. Once installed, it exposes Crabbox as agent tools:

- `crabbox_run`, `crabbox_warmup`, `crabbox_status`, `crabbox_list`, `crabbox_stop`

The plugin shells out to the configured `crabbox` binary, so local config, broker login, repo claims, and sync behavior stay owned by the CLI. Set `plugins.entries.crabbox.config.binary` if `crabbox` is not on `PATH`.

Durable run inspection is intentionally CLI/skill-led instead of additional plugin tools: use `crabbox history`, `crabbox events --after --limit`, `crabbox attach`, `crabbox logs`, `crabbox results`, and `crabbox usage` from a shell-capable agent.

## Development

```sh
# Go CLI
go build -o bin/crabbox ./cmd/crabbox
go test -race ./...
scripts/check-go-coverage.sh 85.0

# Cloudflare Worker
# Use Node 22+ for local Worker checks; CI currently runs Node 24.
npm ci --prefix worker
npm test --prefix worker
npm run build --prefix worker

# Docs
npm run docs:check

# Optional live smoke, when broker/provider credentials are available
CRABBOX_LIVE=1 CRABBOX_LIVE_REPO=/path/to/openclaw scripts/live-smoke.sh
# Add Blacksmith only for repos with a Testbox workflow.
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=blacksmith-testbox scripts/live-smoke.sh
```

CI runs the full gate (gofmt, vet, race tests, coverage threshold, docs link/build check, GoReleaser snapshot, Worker lint/typecheck/tests/build) on every push and PR. Tagged pushes matching `v*` publish Go archives via GoReleaser and bump the Homebrew formula at [openclaw/homebrew-tap](https://github.com/openclaw/homebrew-tap).

Worker deployment, required secrets, and DNS routing live in [docs/infrastructure.md](docs/infrastructure.md).

## Docs

- **Get the model:** [How Crabbox Works](docs/how-it-works.md), [Architecture](docs/architecture.md), [Orchestrator](docs/orchestrator.md)
- **Use the CLI:** [CLI](docs/cli.md), [Commands](docs/commands/README.md), [Features](docs/features/README.md)
- **Interactive QA:** [Interactive Desktop and VNC](docs/features/interactive-desktop-vnc.md)
- **Operate it:** [Operations](docs/operations.md), [Observability](docs/observability.md), [Troubleshooting](docs/troubleshooting.md)
- **Set it up or audit it:** [Infrastructure](docs/infrastructure.md), [Security](docs/security.md), [Source Map](docs/source-map.md), [MVP Plan](docs/mvp-plan.md)
- **Changes:** [CHANGELOG.md](CHANGELOG.md)

The GitHub Pages site at <https://openclaw.github.io/crabbox/> is generated from the `docs/` Markdown:

```sh
npm run docs:check
open dist/docs-site/index.html
```

## License

MIT — see [LICENSE](LICENSE).
