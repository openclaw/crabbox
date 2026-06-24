# Nomad Provider

Read when:

- choosing `provider: nomad`;
- configuring a HashiCorp Nomad cluster for delegated Linux command execution;
- changing `internal/providers/nomad`.

Nomad is a delegated-run provider. Crabbox registers one Nomad job, waits for a
running allocation, uploads the current checkout as a portable archive through
Nomad allocation exec, extracts it into the configured workdir, and runs the
requested command through allocation exec. Nomad owns the client runtime,
drivers, scheduling, allocation placement, and exec transport. Crabbox owns
local config, repo claims, slugs, ownership metadata, archive sync guardrails,
command timing summaries, and normalized `list`, `status`, and `stop` output.

There is no Crabbox-managed SSH lease. Use AWS, Hetzner, Static SSH, Incus,
KubeVirt, Firecracker, Local Container, or another SSH-lease provider when you
need `crabbox ssh`, rsync, VNC, browser/code capability flags, Actions runner
hydration, Tailscale, checkpoints, forks, restores, or provider-native SSH.

## When To Use

Use Nomad when your team already operates a Nomad cluster and wants Crabbox's
local `run` workflow, archive sync, repo claims, and cleanup safety against
short-lived Linux allocations.

This first Nomad adapter is direct-only and Linux-only. It does not use the
Crabbox coordinator, does not expose aliases, and does not create a brokered
fleet. Select it as `nomad`.

## Prerequisites

- A reachable Nomad HTTP API endpoint.
- A Nomad ACL token available in the environment. The default variable is
  `NOMAD_TOKEN`; configure `nomad.tokenEnv` or `--nomad-token-env` to point at
  a different variable name.
- A Nomad namespace and region if your cluster requires them.
- A client pool that can run the configured task driver. The default driver is
  `docker` with image `ubuntu:24.04`.
- A task image or template that provides `/bin/sh`, `bash`, `tar`, `cat`,
  `mkdir`, and a writable absolute workdir.
- ACL privileges for job registration, job reads, allocation reads, evaluation
  reads, allocation exec, and job deregistration in the selected namespace.

Keep Nomad tokens in environment variables or your existing shell secret
manager. Do not store token values in repo-local config, committed docs, shell
history, or command-line arguments.

## Commands

```sh
crabbox config show --json | jq '.nomad'
crabbox doctor --provider nomad --json
crabbox warmup --provider nomad --slug nomad-smoke
crabbox run --provider nomad -- go test ./...
crabbox run --provider nomad --id nomad-smoke --no-sync -- echo reused
crabbox run --provider nomad --id nomad-smoke --sync-only
crabbox status --provider nomad --id nomad-smoke --wait --json
crabbox list --provider nomad --json
crabbox stop --provider nomad nomad-smoke
crabbox cleanup --provider nomad --dry-run
```

`doctor` is read-only. It verifies that the address is configured, that the
token environment variable contains a value, that `agent.self` is reachable, and
that configured region and namespace values resolve. It prints
`mutation=false` in its checks.

`warmup` creates a Nomad job and local Crabbox claim. The job stays running
until explicit `stop` or `cleanup`, even if `--keep` is omitted. A `run` without
`--id` creates a fresh job and deletes it after the command unless `--keep` or
`--keep-on-failure` retains it. A reused `--id` run leaves the job running.

## Config

```yaml
provider: nomad
target: linux
nomad:
  address: https://nomad.example.com:4646
  region: global
  namespace: default
  tokenEnv: NOMAD_TOKEN
  task: crabbox
  driver: docker
  image: ubuntu:24.04
  workdir: /workspace/crabbox
  datacenters: [dc1]
  cpu: 1000
  memoryMB: 2048
  diskMB: 1024
  allocReadyTimeout: 5m
  evalTimeout: 5m
  execTimeoutSecs: 600
```

| Setting | Config key | Environment variable | Flag |
| --- | --- | --- | --- |
| Nomad address | `address` | `NOMAD_ADDR` | `--nomad-address` |
| Region | `region` | `NOMAD_REGION` | `--nomad-region` |
| Namespace | `namespace` | `NOMAD_NAMESPACE` | `--nomad-namespace` |
| ACL token env name | `tokenEnv` | _(name only)_ | `--nomad-token-env` |
| CA certificate | `caCert` | `NOMAD_CACERT` | `--nomad-ca-cert` |
| CA path | `caPath` | `NOMAD_CAPATH` | `--nomad-ca-path` |
| Client certificate | `clientCert` | _(none)_ | `--nomad-client-cert` |
| Client key | `clientKey` | _(none)_ | `--nomad-client-key` |
| TLS server name | `tlsServerName` | _(none)_ | `--nomad-tls-server-name` |
| Skip TLS verification | `skipVerify` | _(none)_ | `--nomad-skip-verify` |
| Task name | `task` | _(none)_ | `--nomad-task` |
| Task driver | `driver` | _(none)_ | `--nomad-driver` |
| Task image | `image` | _(none)_ | `--nomad-image` |
| Workdir | `workdir` | _(none)_ | `--nomad-workdir` |
| Job spec template | `jobspecTemplate` | _(none)_ | `--nomad-jobspec-template` |
| Node pool | `nodePool` | _(none)_ | `--nomad-node-pool` |
| Datacenters | `datacenters` | _(none)_ | `--nomad-datacenters` |
| CPU MHz | `cpu` | _(none)_ | `--nomad-cpu` |
| Memory MB | `memoryMB` | _(none)_ | `--nomad-memory-mb` |
| Ephemeral disk MB | `diskMB` | _(none)_ | `--nomad-disk-mb` |
| Allocation ready timeout | `allocReadyTimeout` | _(none)_ | `--nomad-alloc-ready-timeout` |
| Evaluation timeout | `evalTimeout` | _(none)_ | `--nomad-eval-timeout` |
| Exec timeout seconds | `execTimeoutSecs` | _(none)_ | `--nomad-exec-timeout-secs` |

Defaults: `tokenEnv` `NOMAD_TOKEN`, task `crabbox`, driver `docker`, image
`ubuntu:24.04`, workdir `/workspace/crabbox`, datacenter `dc1`, CPU `1000`,
memory `2048` MB, disk `1024` MB, allocation/evaluation timeouts `5m`, and exec
timeout `600` seconds.

`address` must be an absolute `http`, `https`, or `unix` URL and must not
include credentials, query parameters, or fragments. `workdir` must be
absolute. `tokenEnv` must be an environment variable name; the token value is
read only from that variable at runtime. `--class` and `--type` are rejected for
Nomad; use `--nomad-cpu`, `--nomad-memory-mb`, `--nomad-disk-mb`,
`--nomad-image`, and `--nomad-jobspec-template` instead.

## ACL Policy

The exact policy depends on your Nomad version, namespace layout, and task
driver. Start with the narrowest namespace-scoped policy that lets Crabbox
register and deregister its own jobs, read evaluations and allocations, and
execute into the selected task:

```hcl
namespace "default" {
  policy = "write"
  capabilities = [
    "list-jobs",
    "read-job",
    "submit-job",
    "dispatch-job",
    "alloc-lifecycle",
    "read-logs",
    "read-fs",
    "alloc-exec"
  ]
}

agent {
  policy = "read"
}

node {
  policy = "read"
}
```

If you use a custom `raw_exec` template or node-level exec behavior, audit the
additional node and driver privileges separately. The default Docker job path
does not require storing Nomad tokens in the allocation.

## Job Spec

Without `jobspecTemplate`, Crabbox registers a service job named
`crabbox-<lease-suffix>` with one task group and one task. The default task
runs:

```text
/bin/sh -lc 'mkdir -p /workspace/crabbox && sleep infinity'
```

Crabbox adds ownership metadata to the job, task group, and task:

```text
crabbox.managed=true
crabbox.lease_id=<cbx_...>
crabbox.slug=<slug>
crabbox.provider=nomad
crabbox.scope=<sha256 namespace/region/task scope>
crabbox.namespace=<namespace or default>
crabbox.region=<region or default>
crabbox.job_id=<job id>
crabbox.task=<task>
crabbox.workdir=<workdir>
crabbox.expires_at=<ttl timestamp when configured>
```

Custom job spec templates must currently be JSON files. They may use these
placeholders:

```text
{{.LeaseID}}
{{.Slug}}
{{.JobID}}
{{.Task}}
{{.Workdir}}
{{.Namespace}}
{{.Region}}
{{.Scope}}
{{.ExpiresAt}}
```

Template validation requires the rendered job ID, matching Crabbox ownership
metadata, and a task whose name equals `nomad.task`.

## Lifecycle

1. `warmup` or `run` without `--id` creates a lease ID (`cbx_...`), allocates a
   friendly slug, renders the Nomad job, registers it, waits for evaluation
   completion, waits for a running allocation, and writes a local Crabbox claim.
2. `status` and `list` start from local `cbx_...` claims scoped to the selected
   namespace, region, and task. They verify remote job ownership before
   reporting readiness.
3. Unless `--no-sync` is set, `run` creates a portable archive of the checkout,
   uploads it through allocation exec, and extracts it inside `nomad.workdir`.
   With `sync.delete: true`, archive sync replaces stale remote files according
   to the delegated archive-sync contract.
4. Commands run through Nomad allocation exec against the configured task. Exit
   codes are mirrored by `crabbox run`.
5. `--sync-only` performs only archive sync, prints the remote workdir, and
   follows normal keep/cleanup behavior.
6. `stop` deregisters only a job with matching Crabbox ownership metadata and
   removes the local claim. If the job is already missing, it removes the stale
   local claim for that lease.
7. `cleanup` sweeps only local Nomad claims in the active provider scope. It
   deregisters TTL-expired or idle-expired Crabbox-owned jobs, removes missing
   stale claims, and skips active claims. `--dry-run` prints the planned action
   without mutating Nomad or local claim state.

## Capabilities

- Provider ID: `nomad`.
- Family: `nomad`.
- Kind: `delegated-run`.
- Targets: Linux.
- Coordinator: never.
- Aliases: none.
- Sync: delegated archive sync through allocation exec.
- Cleanup: yes, for Crabbox-owned Nomad jobs and local claims.
- Env forwarding: yes, for command env values allowed through normal Crabbox
  env-forwarding flags.
- Config show: yes; `crabbox config show --json` reports the token env name and
  auth source as `env` or `missing`, never the token value.
- Unsupported: run artifacts, artifact downloads, run session, interactive TTY,
  SSH, VNC, desktop, browser, code, Tailscale, URL bridge, MCP attachments, run
  proof, checkpoints, forks, restores, provider-managed coordinator routing, and
  mandatory live CI.

## Live Smoke

Nomad live proof is opt-in because it creates and deregisters a real job:

```sh
export CRABBOX_LIVE=1
export CRABBOX_LIVE_PROVIDERS=nomad
export NOMAD_ADDR=https://nomad.example.com:4646
export NOMAD_TOKEN=...
scripts/live-nomad-smoke.sh
```

or through the matrix dispatcher:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=nomad scripts/live-smoke.sh
```

The script builds `bin/crabbox` unless `CRABBOX_BIN` points at an existing
binary, verifies `doctor`, creates a small Git fixture, runs `warmup`, proves
archive sync and env forwarding through a retained Nomad job, checks
`status`/`list`, reuses the allocation with `--no-sync`, and then calls `stop`.

It emits one classification:

- `live_nomad_smoke_passed`
- `environment_blocked`
- `quota_blocked`
- `diagnostic_only`

Missing `CRABBOX_LIVE=1`, missing `CRABBOX_LIVE_PROVIDERS=nomad`, missing
`NOMAD_ADDR` or `nomad.address`, and a missing token env value are classified as
`environment_blocked` before any mutation. If the smoke is interrupted after a
job is created, rerun:

```sh
crabbox stop --provider nomad <slug-or-cbx-id>
crabbox cleanup --provider nomad --dry-run
```

## Troubleshooting

- `nomad address is required`: set `NOMAD_ADDR`, `nomad.address`, or
  `--nomad-address`.
- `missing_token`: export the environment variable named by `tokenEnv`
  (`NOMAD_TOKEN` by default). Do not place the token on argv.
- TLS or `x509` errors: configure `caCert`, `caPath`, `clientCert`,
  `clientKey`, or `tlsServerName`. Use `skipVerify` only for disposable local
  clusters where trust is handled another way.
- `missing_region` or namespace failures: set the same region and namespace
  used by the token policy and Nomad jobs.
- Allocation readiness timeouts: check Nomad client availability, datacenters,
  node pool, image pull access, task driver availability, task name, and
  resource sizing.
- `alloc-exec` or permission denied failures: add the allocation exec and job
  read/write capabilities to the token policy in the selected namespace.
- Stale local claims: `crabbox list --provider nomad --json` shows
  `missing-or-inaccessible`; `crabbox stop --provider nomad <id>` removes a
  claim only after resolving the active provider scope.
- Cleanup surprises: `crabbox cleanup --provider nomad --dry-run` prints
  `would deregister`, `would remove`, or `skip` decisions. Cleanup never
  enumerates arbitrary Nomad jobs and never mutates jobs without matching
  Crabbox ownership metadata.
