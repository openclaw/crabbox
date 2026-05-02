# 🦀 Crabbox

**Warm a box, sync the diff, run the suite.**

Crabbox is an open-source remote testbox runner for maintainers and AI agents. Lease a fast Linux machine on owned cloud capacity, sync your dirty checkout, run a command remotely, stream output, and release. Local edit-save-run loop, cloud-grade compute.

```sh
crabbox run -- pnpm test
```

Behind that single command: a Go CLI on your laptop, a Cloudflare Worker broker that owns provider credentials and lease state, and a vanilla Ubuntu runner on Hetzner Cloud or AWS EC2 Spot. Crabbox can also wrap Blacksmith Testboxes when you choose `provider: blacksmith-testbox`.

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
crabbox CLI    -- HTTPS --> Fleet Durable Object  -->   Hetzner / AWS Spot
   |                         lease + cost state              |
   |                                                         |
   +------------ SSH + rsync to leased runner <--------------+
```

- **CLI** — Go binary. Loads config, mints a per-lease SSH key, asks the broker for a lease, waits for SSH, seeds remote Git, rsyncs the dirty checkout (with fingerprint skip when nothing changed), runs the command, streams output, releases.
- **Broker** — Cloudflare Worker at `crabbox.openclaw.ai` plus a single Durable Object. Owns provider credentials, serializes lease state, enforces active-lease and monthly spend caps, and expires stale leases by alarm. Auth is GitHub login or a shared bearer token.
- **Runner** — vanilla Ubuntu prepared by cloud-init with SSH on the primary port, default `2222`, plus configured fallback ports, Git, rsync, curl, jq, and `/work/crabbox`. No broker credentials live on the box. Project runtimes (Go, Node, Docker, services, secrets) come from your repo's GitHub Actions hydration, devcontainer, Nix, mise/asdf, or setup scripts — not from Crabbox.

A direct-provider mode (`--provider hetzner|aws` with local credentials) exists for debugging the broker itself; the brokered path is the default.

For the full mental model, see [How Crabbox Works](docs/how-it-works.md). For the doc-to-code map, see [Source Map](docs/source-map.md).

## Highlights

- **One-shot or warm.** `crabbox run` for fire-and-forget; `crabbox warmup` + `--id` for repeated runs against the same box.
- **Local-first sync.** No clean-checkout requirement. Tracked + nonignored files only, fingerprint skip on no-op runs, sanity checks against suspicious mass deletions, optional shallow base-ref hydration for changed-test workflows.
- **Brokered cloud.** Maintainers and agents share infra without sharing provider tokens. Hetzner and AWS EC2 Spot are first-class; both fall back across instance families when capacity or quota rejects a request.
- **Blacksmith Testbox wrapper.** Set `provider: blacksmith-testbox` to delegate warmup/run/list/status/stop to the Blacksmith CLI while Crabbox keeps local slugs, repo claims, timing summaries, and config conventions.
- **Cost guardrails.** Per-lease and monthly spend caps. Live pricing from EC2 Spot history or Hetzner server-type prices, with static fallbacks. `crabbox usage` summarizes spend by user, org, provider, and type.
- **GitHub Actions hydration.** `crabbox actions hydrate` registers a leased box as an ephemeral Actions runner, so the repo's own workflow installs runtimes, services, and secrets. Crabbox does not parse Actions YAML.
- **OpenClaw plugin.** The repo root is a native OpenClaw plugin. Agents drive Crabbox through `crabbox_run`, `crabbox_warmup`, `crabbox_status`, `crabbox_list`, `crabbox_events`, and `crabbox_stop` instead of shelling out.
- **Operator surface.** `doctor`, `init`, `status`, `inspect`, `list`, `usage`, `history`, `logs`, `results`, `cache`, `admin`, `cleanup`, plus `--json` output where it matters.

## Machine classes

`beast` is the default. Both providers fall back across an ordered list of instance types.

```text
Hetzner    standard  ccx33, cpx62, cx53
           fast      ccx43, cpx62, cx53
           large     ccx53, ccx43, cpx62, cx53
           beast     ccx63, ccx53, ccx43, cpx62, cx53

AWS Spot   standard  c7a/c7i/m7a/m7i.8xlarge family
           fast      …16xlarge family
           large     …24xlarge family
           beast     …48xlarge family, falling back to 32x/24x/16x
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

Forwarded environment is intentionally narrow: `NODE_OPTIONS` and `CI`. Do not pass secrets as command-line arguments. Full env-var reference and per-command flags are in [docs/cli.md](docs/cli.md) and [docs/commands/](docs/commands/README.md).

## OpenClaw plugin

The repo root is a native OpenClaw plugin package. Once installed, it exposes Crabbox as agent tools:

- `crabbox_run`, `crabbox_warmup`, `crabbox_status`, `crabbox_list`, `crabbox_events`, `crabbox_stop`

The plugin shells out to the configured `crabbox` binary, so local config, broker login, repo claims, and sync behavior stay owned by the CLI. Set `plugins.entries.crabbox.config.binary` if `crabbox` is not on `PATH`.

## Development

```sh
# Go CLI
go build -o bin/crabbox ./cmd/crabbox
go test -race ./...
scripts/check-go-coverage.sh 85.0

# Cloudflare Worker
npm ci --prefix worker
npm test --prefix worker
npm run build --prefix worker

# Docs
npm run docs:check

# Optional live smoke, when broker/provider credentials are available
CRABBOX_LIVE=1 CRABBOX_LIVE_REPO=/path/to/openclaw scripts/live-smoke.sh
```

CI runs the full gate (gofmt, vet, race tests, coverage threshold, docs link/build check, GoReleaser snapshot, Worker lint/typecheck/tests/build) on every push and PR. Tagged pushes matching `v*` publish Go archives via GoReleaser and bump the Homebrew formula at [openclaw/homebrew-tap](https://github.com/openclaw/homebrew-tap).

Worker deployment, required secrets, and DNS routing live in [docs/infrastructure.md](docs/infrastructure.md).

## Docs

- **Get the model:** [How Crabbox Works](docs/how-it-works.md), [Architecture](docs/architecture.md), [Orchestrator](docs/orchestrator.md)
- **Use the CLI:** [CLI](docs/cli.md), [Commands](docs/commands/README.md), [Features](docs/features/README.md)
- **Operate it:** [Operations](docs/operations.md), [Observability](docs/observability.md), [Troubleshooting](docs/troubleshooting.md)
- **Set it up or audit it:** [Infrastructure](docs/infrastructure.md), [Security](docs/security.md), [Source Map](docs/source-map.md), [MVP Plan](docs/mvp-plan.md)
- **Changes:** [CHANGELOG.md](CHANGELOG.md)

The GitHub Pages site at <https://openclaw.github.io/crabbox/> is generated from the `docs/` Markdown:

```sh
npm run docs:check
open dist/docs-site/index.html
```

## Status

Crabbox 0.1.0 (2026-05-01) is the first public release. Working today: brokered Hetzner + AWS Spot provisioning, warm-box reuse, GitHub Actions hydration, cost guardrails, run history, JUnit summaries, OpenClaw plugin, and the full CLI surface listed above. **Not yet:** untrusted multi-tenant isolation — Crabbox today assumes shared trust between operators of a single broker.

## License

MIT — see [LICENSE](LICENSE).
