# Getting Started

Read this when:

- you are new to Crabbox and want a working `crabbox run` in about ten minutes;
- you are evaluating Crabbox for a repo and want to see the shape of a real workflow;
- you want a reference for what a typical onboarding looks like end to end.

This is a cookbook, not a reference. It walks through one repo from install to
`crabbox run -- pnpm test`. Each step links to deeper docs when you want more.
If you are still deciding whether Crabbox fits your workflow, start with
[What Crabbox is](README.md#what-crabbox-is).

## Step 1. Install

```sh
brew install openclaw/tap/crabbox
```

Verify the install:

```sh
crabbox --version
crabbox doctor
```

`crabbox doctor` prints one line per check. Local tool checks (`git`, `ssh`,
`ssh-keygen`, `rsync`) should report `ok`. It is fine if the broker and provider
checks fail for now - we configure those next.

If you do not use Homebrew, GitHub Releases ship signed archives for macOS,
Linux, and Windows. Download the matching archive from
<https://github.com/openclaw/crabbox/releases>.

## Step 2. Log In

```sh
crabbox login --url https://broker.example.com
```

`login` opens a browser to the GitHub OAuth flow (pass `--no-browser` to print
the URL for a browser on the same device). The broker exchanges the OAuth code,
verifies your GitHub org membership, and redirects a one-use confirmation to the
CLI's loopback listener before writing the signed token to your user config:

```text
logged in broker=https://broker.example.com provider=hetzner user=alice@example.com org=example-org config=/Users/alice/.config/crabbox/config.yaml
```

From then on, every `crabbox` command authenticates automatically. Check your
identity any time:

```sh
crabbox whoami
```

```text
user=alice@example.com org=example-org auth=user broker=https://broker.example.com
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

`init` writes three files (override any path with `--config`, `--workflow`, or
`--skill`; pass `--force` to overwrite existing files):

```text
.crabbox.yaml                          repo defaults (profile, class, sync, env)
.github/workflows/crabbox.yml          Actions hydration workflow (optional)
.agents/skills/crabbox/SKILL.md        agent-facing skill instructions
```

The generated `.crabbox.yaml` ships sensible defaults. Adjust the parts that
matter for your repo:

- `profile`: a name for this lane (the template uses `<repo>-check`);
- `class`: `standard`, `fast`, `large`, or `beast` (the template uses `beast`);
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
crabbox warmup
```

`warmup` acquires a lease, provisions the runner, waits for SSH and tooling to
come up, keeps the lease (`--keep`, on by default), and prints two lines:

```text
leased cbx_abcdef123456 slug=swift-crab provider=hetzner server=cx... type=ccx... ip=203.0.113.10 idle_timeout=30m0s expires=2026-05-29T17:30:00Z
ready ssh=crabbox@203.0.113.10:2222 network=public workroot=/work/crabbox
```

The lease is now waiting for commands. Two timers bound its life: the idle
timeout (default 30m) and the TTL (default 90m). Whichever fires first releases
the box.

Reuse the lease by `slug` (friendly) or `id` (the `cbx_...` handle). Both work
with `--id` on later commands.

## Step 5. Run A Command

```sh
crabbox run --id swift-crab -- pnpm test
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
crabbox stop swift-crab
```

`stop` releases the lease, deletes the provider machine, removes the local
claim, and frees the reserved cost. If you forget, the broker's idle alarm
releases the lease automatically.

```sh
crabbox cleanup --dry-run
```

`cleanup` sweeps direct-provider leftovers and local state. It is for the
direct-provider path; brokered cleanup is the broker alarm's job.

## Common Variations

Keep a lease alive across a longer session:

```sh
crabbox warmup --idle-timeout 4h --ttl 8h
crabbox run --id swift-crab -- pnpm test
crabbox run --id swift-crab -- pnpm bench
crabbox stop swift-crab
```

Open a desktop session:

```sh
crabbox warmup --desktop
crabbox vnc --id swift-crab --open
```

Open a code-server tab:

```sh
crabbox warmup --code
crabbox code --id swift-crab --open
```

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
