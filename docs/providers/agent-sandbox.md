# Agent Sandbox Provider

Read this when:

- choosing `provider: agent-sandbox`;
- configuring a Kubernetes-backed Agent Sandbox warm pool;
- changing `internal/providers/agentsandbox`.

Agent Sandbox is a delegated-run provider. Crabbox talks directly to the
Kubernetes API, creates a `SandboxClaim` from a configured `SandboxWarmPool`,
waits for the resulting `Sandbox` and pod to become ready, archive-syncs the
local repository through Kubernetes exec and `tar`, runs the command in the
sandbox pod, and deletes the claim on release by default.

There is no Crabbox SSH lease. Kubernetes and Agent Sandbox own the runtime,
sandbox pod, and command transport. Crabbox owns local config, repo claims,
slug allocation, claim ownership labels and annotations, sync guardrails,
timing summaries, and normalized `list` / `status` output.

## When To Use

Use Agent Sandbox when your team already runs Agent Sandbox in Kubernetes and
wants Crabbox's local workflow, archive sync, repo claims, and command
streaming against a warm pool. It fits short Linux command execution where a
`SandboxClaim` is the durable unit of ownership.

Use an SSH-lease provider such as AWS, Hetzner, KubeVirt, Incus, Static SSH, or
Local Container when you need `crabbox ssh`, VNC, code-server, Actions runner
hydration, Tailscale, or the normal SSH/rsync data plane.

## Prerequisites

- A Kubernetes kubeconfig or in-cluster config that can reach the target
  cluster.
- A non-empty Kubernetes context. Crabbox requires the context so local claims
  cannot drift when the kubeconfig current context changes.
- A namespace containing Agent Sandbox resources.
- Agent Sandbox CRDs:
  - `agents.x-k8s.io/v1beta1` `sandboxes`
  - `extensions.agents.x-k8s.io/v1beta1` `sandboxclaims`
  - `extensions.agents.x-k8s.io/v1beta1` `sandboxwarmpools`
- A `SandboxWarmPool` in the configured namespace.
- RBAC allowing:
  - `get`, `list`, `watch`, `create`, and `delete` on `sandboxclaims`
  - `get` on `sandboxwarmpools`
  - `get`, `list`, and `watch` on `sandboxes`
  - `get`, `list`, and `watch` on pods
  - `create` on `pods/exec`
- A sandbox image that provides `/bin/sh`, `tar`, and a writable workdir.

## Commands

```sh
crabbox doctor --provider agent-sandbox
crabbox warmup --provider agent-sandbox --slug linux-pool-smoke
crabbox run --provider agent-sandbox -- go test ./...
crabbox run --provider agent-sandbox --id linux-pool-smoke --no-sync -- echo reused
crabbox run --provider agent-sandbox --id linux-pool-smoke --sync-only
crabbox status --provider agent-sandbox --id linux-pool-smoke --wait
crabbox list --provider agent-sandbox --json
crabbox stop --provider agent-sandbox linux-pool-smoke
crabbox cleanup --provider agent-sandbox --dry-run
```

`warmup` always leaves the `SandboxClaim` retained until explicit `stop`.
A `run` without `--id` creates a claim and deletes it after the command unless
`--keep` or `--keep-on-failure` retains it. The provider is Linux-only.

## Config

```yaml
provider: agent-sandbox
target: linux
agentSandbox:
  kubeconfig: ~/.kube/config       # empty = KUBECONFIG/default/in-cluster
  context: agent-cluster           # required
  namespace: sandboxes             # default: default
  warmPool: linux-pool             # required SandboxWarmPool name
  container: worker                # empty = Kubernetes default container
  workdir: /workspace/crabbox      # default sync target and exec cwd
  sandboxReadyTimeout: 180s
  podReadyTimeout: 180s
  execTimeoutSecs: 600
  deleteOnRelease: true
  forgetMissing: false
```

Provider flags:

```text
--agent-sandbox-kubeconfig
--agent-sandbox-context
--agent-sandbox-namespace
--agent-sandbox-warm-pool
--agent-sandbox-container
--agent-sandbox-workdir
--agent-sandbox-sandbox-ready-timeout
--agent-sandbox-pod-ready-timeout
--agent-sandbox-exec-timeout-secs
--agent-sandbox-delete-on-release
--agent-sandbox-forget-missing
```

Environment overrides use the `CRABBOX_AGENT_SANDBOX_*` prefix:

```text
CRABBOX_AGENT_SANDBOX_KUBECONFIG
CRABBOX_AGENT_SANDBOX_CONTEXT
CRABBOX_AGENT_SANDBOX_NAMESPACE
CRABBOX_AGENT_SANDBOX_WARM_POOL
CRABBOX_AGENT_SANDBOX_CONTAINER
CRABBOX_AGENT_SANDBOX_WORKDIR
CRABBOX_AGENT_SANDBOX_SANDBOX_READY_TIMEOUT
CRABBOX_AGENT_SANDBOX_POD_READY_TIMEOUT
CRABBOX_AGENT_SANDBOX_EXEC_TIMEOUT_SECS
CRABBOX_AGENT_SANDBOX_DELETE_ON_RELEASE
CRABBOX_AGENT_SANDBOX_FORGET_MISSING
```

`agentSandbox.workdir` must be absolute and cannot be a broad system directory
such as `/`, `/tmp`, `/usr`, `/var`, or `/home`. `namespace`, `warmPool`, and
`container` cannot contain whitespace or `/`.

## Lifecycle

1. `doctor` loads Kubernetes config, verifies the Agent Sandbox CRDs, verifies
   the configured warm pool, and checks the required RBAC verbs. It does not
   create a claim.
2. `warmup` or `run` without `--id` creates one `SandboxClaim` named
   `crabbox-<slug>-<lease-hash>` in the configured namespace. The claim points
   at `agentSandbox.warmPool` and carries Crabbox ownership labels plus
   annotations for provider scope, workdir, and container.
3. Crabbox waits for `SandboxClaim.status.sandbox.name`, fetches the matching
   `Sandbox`, waits for its Ready condition, then resolves the pod from the
   sandbox pod annotation or selector and waits for the pod Ready condition.
4. Unless `--no-sync` is set, Crabbox builds a portable archive from the local
   Git file manifest and extracts it into the configured workdir with
   Kubernetes `pods/exec`. With `sync.delete: true`, extraction happens in a
   sibling staging directory and replaces the workdir only after upload and
   extraction succeed.
5. The command runs through Kubernetes exec in the sandbox pod. Forwarded
   environment values are exported inside the streamed shell script, not placed
   on the local command line.
6. On release, Crabbox deletes only the owned `SandboxClaim` whose labels and
   provider-scope annotation match the local claim. The Agent Sandbox
   controller owns the resulting sandbox and pod teardown.

## Claim Scope And Cleanup Safety

Local claim IDs use the `asbx_` prefix. The provider scope includes the
kubeconfig identity, context, namespace, warm pool, and container. Reusing,
listing, status-checking, stopping, or cleaning up a retained claim only
matches claims from the same scope.

Before deleting a live `SandboxClaim`, Crabbox verifies:

- `crabbox.openclaw.dev/provider=agent-sandbox`
- `crabbox.openclaw.dev/lease-id=<local lease id>`
- `crabbox.openclaw.dev/provider-scope=<local scope>`

Missing Kubernetes claims are preserved locally by default because a 404 can be
ambiguous across clusters or accounts. Set `--agent-sandbox-forget-missing` or
`CRABBOX_AGENT_SANDBOX_FORGET_MISSING=true` only after confirming the claim is
gone in the intended cluster.

`cleanup` uses Crabbox's local idle-time policy. It deletes due live claims
after ownership validation, skips missing claims unless `forgetMissing` is
enabled, and reports every skipped or removed claim.

## Capabilities

- SSH: no.
- Crabbox sync: yes, delegated archive upload and `tar` extraction through pod
  exec.
- Provider sync: no separate provider-native copy command.
- Env forwarding: yes, inside the remote shell script.
- Desktop / browser / code / VNC: no.
- Tailscale: no.
- Actions runner hydration: no.
- Coordinator broker: no. Agent Sandbox always runs direct from the CLI.
- Aliases: none.

## Gotchas

- `--actions-runner` and Tailscale options are rejected because the provider is
  delegated-run only.
- `--checksum` and SSH/rsync-specific behavior do not apply to the pod-exec
  archive path.
- `--no-sync` creates the workdir but does not apply `sync.delete`; retained
  workspaces keep whatever was already present.
- The default container is Kubernetes' default container for the pod. Set
  `agentSandbox.container` when the sandbox pod has multiple containers.
- `podReadyTimeout` is part of config today, while readiness polling uses the
  sandbox readiness timeout around the claim/sandbox/pod resolution path.
- Kubernetes errors may include cluster resource names. Do not paste live
  errors into public issues without a redaction pass.

## Live Smoke

The guarded live smoke is opt-in and creates one short-lived Crabbox-owned
`SandboxClaim` only when explicit live configuration is present:

```sh
CRABBOX_LIVE=1 \
CRABBOX_LIVE_COORDINATOR=0 \
CRABBOX_LIVE_PROVIDERS=agent-sandbox \
CRABBOX_AGENT_SANDBOX_KUBECONFIG=~/.kube/config \
CRABBOX_AGENT_SANDBOX_CONTEXT=agent-cluster \
CRABBOX_AGENT_SANDBOX_NAMESPACE=sandboxes \
CRABBOX_AGENT_SANDBOX_WARM_POOL=linux-pool \
scripts/live-agent-sandbox-smoke.sh
```

The smoke builds `bin/crabbox` unless `CRABBOX_BIN` points at an executable,
runs `doctor`, creates one claim with a unique or configured slug, proves
archive sync and environment forwarding, checks `status --wait` and `list
--json`, then stops only the slug it created using the Agent Sandbox provider.

It prints exactly one classification:

- `live_agent_sandbox_smoke_passed`
- `environment_blocked`
- `quota_blocked`
- `diagnostic_only`

`environment_blocked` is the expected safe result when live mode, provider
selection, kubeconfig, context, warm pool, RBAC, CRDs, or cluster connectivity
are missing. Unit tests cover those guardrails without touching Kubernetes.

The general dispatcher can run the same smoke:

```sh
CRABBOX_LIVE=1 \
CRABBOX_LIVE_COORDINATOR=0 \
CRABBOX_LIVE_PROVIDERS=agent-sandbox \
scripts/live-smoke.sh
```

## Related Docs

- [Provider backends](../provider-backends.md)
- [Provider authoring](../features/provider-authoring.md)
- [Provider decision matrix](README.md)
