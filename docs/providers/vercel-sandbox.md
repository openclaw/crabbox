# Vercel Sandbox Provider

Read when:

- choosing `provider: vercel-sandbox`;
- configuring Vercel project scope, runtime, workdir, resources, network policy,
  snapshots, or cleanup behavior;
- changing `internal/providers/vercelsandbox`.

Vercel Sandbox is a delegated run provider. Crabbox creates a Vercel-managed
Linux microVM through the Vercel Sandbox SDK bridge, uploads a portable archive
of the checkout, and runs commands through the sandbox command API. Vercel owns
the microVM runtime, file API, command transport, and sandbox deletion. Crabbox
owns local config, repo claims, slugs, sync guardrails, ownership metadata,
timing summaries, and normalized `list` / `status` rendering.

The provider does not expose a Crabbox-managed SSH lease. Choose AWS, Hetzner,
Static SSH, Local Container, or another SSH-lease provider when you need
`crabbox ssh`, VNC, browser/code capability flags, Actions runner hydration,
raw rsync behavior, or provider-native SSH access.

## Setup

Install the Vercel Sandbox SDK where the Crabbox binary can load it, and install
the `sandbox` CLI when you want `doctor` and the live smoke to prove local auth:

```sh
npm install @vercel/sandbox
npm install -g sandbox
sandbox login
```

For project-linked local development, Vercel's recommended path is to link the
project and pull environment so the SDK can use OIDC:

```sh
vercel link
vercel env pull
```

External CI or non-Vercel environments can provide tokens through environment
variables. Crabbox never accepts Vercel tokens as command-line flags and never
persists them in `crabbox.yaml`, `.crabbox.yaml`, or trusted user config.

## Auth

Crabbox forwards these credential variables to the SDK bridge and `sandbox`
readiness probe:

```text
CRABBOX_VERCEL_SANDBOX_OIDC_TOKEN -> VERCEL_OIDC_TOKEN
VERCEL_OIDC_TOKEN
CRABBOX_VERCEL_SANDBOX_TOKEN -> VERCEL_TOKEN
CRABBOX_VERCEL_TOKEN -> VERCEL_TOKEN
VERCEL_TOKEN
CRABBOX_VERCEL_SANDBOX_AUTH_TOKEN -> VERCEL_AUTH_TOKEN
CRABBOX_VERCEL_AUTH_TOKEN -> VERCEL_AUTH_TOKEN
VERCEL_AUTH_TOKEN
```

`sandbox login` satisfies both the CLI readiness check and the default SDK
bridge. The bridge reads the official Vercel auth store through the SDK's
exported auth helpers, refreshes expired stored OAuth tokens, and resolves the
project/team scope before lifecycle operations. Explicit OIDC or access-token
environment credentials take precedence. OIDC scope comes from the token's
project/team claims, so Crabbox rejects explicit `projectId`, `teamId`, or
`scope` configuration when `VERCEL_OIDC_TOKEN` is set. For access-token and
stored-login auth, explicit scope configuration takes precedence over a
`.vercel/project.json` link in the current checkout.

Set at least one project scoping value when your token or local project link
does not imply it. An explicit `projectId` must be paired with `teamId` or
`scope`, matching the official SDK credential contract:

```sh
export CRABBOX_VERCEL_SANDBOX_PROJECT_ID=prj_example
export CRABBOX_VERCEL_SANDBOX_TEAM_ID=team_example
# or
export CRABBOX_VERCEL_SANDBOX_SCOPE=example-org
```

`doctor` reports project scope as a warning, not a mutation. Authentication,
tool, SDK, and inventory failures are classified as environment readiness
problems and do not create sandboxes.

Provider authentication variables are stripped from forwarded command
environments. If a run uses `--allow-env VERCEL_TOKEN`, Crabbox warns and does
not send that value into the sandbox command environment.

## Commands

```sh
crabbox doctor --provider vercel-sandbox --json
crabbox warmup --provider vercel-sandbox --slug vsbx-smoke
crabbox run --provider vercel-sandbox -- go test ./...
crabbox run --provider vercel-sandbox --id vsbx-smoke --shell 'pnpm install && pnpm test'
crabbox run --provider vercel-sandbox --id vsbx-smoke --sync-only
crabbox status --provider vercel-sandbox --id vsbx-smoke --wait --json
crabbox list --provider vercel-sandbox --json
crabbox stop --provider vercel-sandbox vsbx-smoke
crabbox cleanup --provider vercel-sandbox --dry-run
```

`warmup` keeps the sandbox until explicit `stop`. A `run` without `--id`
creates a sandbox and deletes it after the command unless `--keep` or
`--keep-on-failure` retains it.

## Config

```yaml
provider: vercel-sandbox
target: linux
vercelSandbox:
  runtime: node24
  workdir: /vercel/sandbox/crabbox
  projectId: prj_example
  teamId: team_example
  scope: example-org
  vcpus: 1
  timeoutSecs: 0
  execTimeoutSecs: 600
  persistent: false
  snapshot: ""
  snapshotMode: ""
  networkPolicy: default
  networkAllow:
    - api.example.com
  networkDeny:
    - 169.254.169.254/32
  ports:
    - "3000"
  forgetMissing: false
```

Defaults: runtime `node24`, workdir `/vercel/sandbox/crabbox`, command timeout
`600` seconds, network policy `default`, no explicit vCPU count, and provider
service defaults for sandbox lifetime.

Provider flags, each overriding the matching config key:

```text
--vercel-sandbox-runtime
--vercel-sandbox-workdir
--vercel-sandbox-project-id
--vercel-sandbox-team-id
--vercel-sandbox-scope
--vercel-sandbox-vcpus
--vercel-sandbox-timeout-secs
--vercel-sandbox-exec-timeout-secs
--vercel-sandbox-persistent
--vercel-sandbox-snapshot
--vercel-sandbox-snapshot-mode
--vercel-sandbox-network-policy
--vercel-sandbox-network-allow
--vercel-sandbox-network-deny
--vercel-sandbox-ports
--vercel-sandbox-forget-missing
```

Environment overrides:

```text
CRABBOX_VERCEL_SANDBOX_RUNTIME
CRABBOX_VERCEL_SANDBOX_WORKDIR
CRABBOX_VERCEL_SANDBOX_PROJECT_ID
CRABBOX_VERCEL_SANDBOX_TEAM_ID
CRABBOX_VERCEL_SANDBOX_SCOPE
CRABBOX_VERCEL_SANDBOX_VCPUS
CRABBOX_VERCEL_SANDBOX_TIMEOUT_SECS
CRABBOX_VERCEL_SANDBOX_EXEC_TIMEOUT_SECS
CRABBOX_VERCEL_SANDBOX_PERSISTENT
CRABBOX_VERCEL_SANDBOX_SNAPSHOT
CRABBOX_VERCEL_SANDBOX_SNAPSHOT_MODE
CRABBOX_VERCEL_SANDBOX_NETWORK_POLICY
CRABBOX_VERCEL_SANDBOX_NETWORK_ALLOW
CRABBOX_VERCEL_SANDBOX_NETWORK_DENY
CRABBOX_VERCEL_SANDBOX_PORTS
CRABBOX_VERCEL_SANDBOX_FORGET_MISSING
```

`runtime` must be `node26`, `node24`, `node22`, or `python3.13`. Vercel may
expose newer runtimes, but Crabbox validates this v1 surface explicitly. `workdir` must be an
absolute dedicated directory and cannot be `/`, `/tmp`, `/usr`, `/var`,
`/home`, `/vercel`, or `/vercel/sandbox`. `networkPolicy` must be `default`,
`public`, `private`, `restricted`, or `none`; allow and deny entries must be
valid network entries. Allows accept domains, IP addresses, or CIDRs. Denies
accept only IP addresses or CIDRs because the Vercel SDK does not support
domain deny rules. Raw IPs are normalized to host CIDRs. `none` means deny all
egress and cannot be combined with allow/deny entries. `ports` accepts ports or
`start-end` ranges, with at most 15 unique exposed ports.

## Lifecycle

1. `warmup` or `run` without `--id` creates a Vercel Sandbox with the configured
   runtime, project/team/scope, vCPU count, lifetime cap, persistence flag,
   snapshot, network policy, ports, and Crabbox ownership metadata. Snapshot
   creation uses the official `{type: "snapshot", snapshotId: ...}` source
   contract.
2. The local lease is stored as `vsbx_<sandbox-name>` with a friendly slug and a
   repo claim. The claim scope includes configured project, team, and scope plus
   a random ownership marker. Crabbox verifies that scope before reusing,
   stopping, or cleaning up a sandbox.
3. Unless `--no-sync` is set, Crabbox builds a portable gzipped archive from the
   working tree and uploads it through the SDK bridge. With `sync.delete: true`,
   Crabbox extracts into a staging directory and replaces the configured workdir
   only after extraction succeeds.
4. Uploads and commands automatically resume a retained sandbox session when
   needed. Commands run through Vercel Sandbox `runCommand` with `cwd` set to
   the configured workdir and forwarded non-auth environment values sent in the
   SDK request body. Stdout and stderr stream through the bridge as they arrive,
   with a 4 MiB limit per stream, and command timeouts are enforced by the
   sandbox service.
5. One-shot sandboxes are deleted after successful `run` unless `--keep` is set.
   `--keep-on-failure` retains a newly created sandbox after sync, workspace, or
   command failures and prints reuse/stop guidance.
6. `stop` deletes the Vercel Sandbox and removes the local claim. If the remote
   sandbox is already missing, Crabbox preserves the claim unless
   `--vercel-sandbox-forget-missing` or `vercelSandbox.forgetMissing: true` is
   set.
7. `cleanup` only acts on local `vsbx_...` claims in the current
   project/team/scope. It deletes idle-expired Crabbox-owned sandboxes, skips
   still-active claims, and treats missing-or-inaccessible sandboxes
   conservatively unless forget-missing is explicit.

## Capabilities

- SSH: no.
- Crabbox sync: yes, via portable archive upload and in-sandbox extraction.
- Env forwarding: yes, off-argv in the SDK bridge request body; provider auth
  variables are stripped.
- Provider sync: no separate provider-side copy command is required.
- Desktop / browser / code / VNC: no.
- Actions hydration: no.
- Coordinator broker: no, Vercel Sandbox runs direct from the CLI.
- Run session: yes, `run` returns a reusable Crabbox lease/session handle for
  `--lease-output`, retained failure inspection, and later `run --id`,
  `status`, or `stop`.
- Pause/resume: retained sessions resume automatically for sync and execution;
  explicit pause/resume commands are not advertised in v1.
- Ports, snapshots, and persistence: configuration is accepted and passed to
  creation, but Crabbox does not expose post-create port, checkpoint, fork,
  pause, or resume commands for this provider in v1.

## Doctor

`crabbox doctor --provider vercel-sandbox --json` is non-mutating. It checks:

- SDK bridge contract (`mutation=false`);
- local `sandbox` CLI availability;
- read-only CLI auth with `sandbox list --all --limit 1`;
- project scoping readiness;
- local Crabbox inventory for `vsbx_...` claims.

Missing SDK, missing CLI, missing auth, network errors, and inventory failures
are environment blockers. Project scope is a warning because an OIDC token or
linked Vercel project may supply it at runtime.

## Live Smoke

Run the guarded hosted lifecycle smoke only when you intend to create a real
short-lived Vercel Sandbox:

```sh
CRABBOX_LIVE=1 \
CRABBOX_LIVE_PROVIDERS=vercel-sandbox \
CRABBOX_VERCEL_SANDBOX_PROJECT_ID=prj_example \
CRABBOX_VERCEL_SANDBOX_TEAM_ID=team_example \
scripts/live-smoke.sh
```

The top-level smoke dispatches to the provider-specific script:

```sh
CRABBOX_LIVE=1 \
CRABBOX_LIVE_PROVIDERS=vercel-sandbox \
CRABBOX_VERCEL_SANDBOX_PROJECT_ID=prj_example \
CRABBOX_VERCEL_SANDBOX_TEAM_ID=team_example \
scripts/live-vercel-sandbox-smoke.sh
```

The smoke builds `bin/crabbox` unless `CRABBOX_BIN` points at an existing
binary, preflights `sandbox --help` and read-only
`sandbox list --all --limit 1`, creates one uniquely named Crabbox-owned
sandbox, verifies archive sync and off-argv environment forwarding, checks
`doctor`, `status`, and `list`, stops the underlying session through the
official CLI, proves that sync and execution resume it, verifies stdout arrives
before command completion, reuses the same sandbox for a second sync that adds,
updates, and deletes files, proves nonzero exit-code propagation, then stops the
sandbox and confirms that no matching Crabbox-owned inventory remains.
Cleanup proof also polls the official `sandbox list` inventory for remote
absence; a still-visible sandbox is targeted with `sandbox rm` and the smoke
fails instead of reporting success.

The script prints exactly one classification:

- `classification=live_vercel_sandbox_smoke_passed`
- `classification=environment_blocked`
- `classification=quota_blocked`
- `classification=diagnostic_only`

Authentication, missing tools, missing SDK, DNS, TLS, and connectivity failures
are `environment_blocked`. Quota, capacity, plan-limit, admission, and
rate-limit failures are `quota_blocked`. Incomplete proof or unconfirmed cleanup
is `diagnostic_only`.

## Gotchas

- `vercel-sandbox` has no aliases in v1.
- `--class` and `--type` are rejected for this provider; use
  `--vercel-sandbox-vcpus` and `--vercel-sandbox-runtime`.
- `--checksum` and SSH/rsync-only options are rejected. `--sync-only` and
  `--force-sync-large` are supported because the provider declares
  `archive-sync`.
- `--no-sync` only ensures the workdir exists. It does not apply `sync.delete`.
- Raw Vercel CLI `--env key=value` places values on argv. Prefer Crabbox
  `--allow-env` / `--env-from-profile`, which forwards non-auth values through
  the SDK bridge request body and redacts summaries.
- The default bridge cannot update metadata after creation, so ownership
  metadata must be present at sandbox creation time.
- The `sandbox` CLI does not provide a stable JSON contract for all lifecycle
  operations used by Crabbox. Normal provider behavior uses the SDK bridge;
  the CLI is limited to login/readiness checks and manual debugging.

## Related Docs

- [Provider backends](../provider-backends.md)
- [Provider feature overview](../features/providers.md)
