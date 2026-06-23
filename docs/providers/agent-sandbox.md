# Agent Sandbox Provider

Read this when:

- choosing `provider: agent-sandbox`;
- configuring a Kubernetes-backed Agent Sandbox warm pool;
- changing `internal/providers/agentsandbox`.

Agent Sandbox is a delegated-run provider. Crabbox invokes `kubectl` against
the configured cluster, creates a `SandboxClaim` from a configured
`SandboxWarmPool`, waits for the resulting `Sandbox` and pod to become ready,
archive-syncs the local repository through `kubectl exec` and `tar`, runs the
command in the sandbox pod, and deletes the claim on release by default.

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

- `kubectl` installed and compatible with the target cluster.
- A Kubernetes kubeconfig that can reach the target cluster. Standard
  `KUBECONFIG` and default kubeconfig resolution apply when
  `agentSandbox.kubeconfig` is empty.
  Configured kubeconfig paths and `KUBECONFIG` entries must be absolute after
  home expansion so repository files cannot become cluster credentials.
- A non-empty Kubernetes context. Crabbox requires the context so local claims
  cannot drift when the kubeconfig current context changes.
- A namespace containing Agent Sandbox resources.
- Agent Sandbox CRDs:
  - `agents.x-k8s.io/v1beta1` `sandboxes`
  - `extensions.agents.x-k8s.io/v1beta1` `sandboxclaims`
  - `extensions.agents.x-k8s.io/v1beta1` `sandboxwarmpools`
- A `SandboxWarmPool` in the configured namespace.
- RBAC allowing:
  - `get`, `create`, and `delete` on `sandboxclaims`
  - `get` on `sandboxwarmpools`
  - `get` on `sandboxes`
  - `get` and `list` on pods
  - `create` on `pods/exec`
- A sandbox image that provides `/bin/sh`, `bash`, `tar`, `cp`, and a writable
  workdir. Crabbox uses `/bin/sh` for transport scripts and `bash -lc` for
  user shell-mode and auto-shell commands.

## Supported Agent Sandbox Version

Crabbox currently targets the Agent Sandbox `v0.5.0rc1` prerelease API:

- `agents.x-k8s.io/v1beta1`
- `extensions.agents.x-k8s.io/v1beta1`

This is intentional because `v0.5.0` is expected to promote the same beta API
soon. Crabbox does not carry `v1alpha1` compatibility. Until stable `v0.5.0`
ships, pin the controller and CRDs to `v0.5.0rc1` or a newer release that still
serves these `v1beta1` resources.

Official project and release references:

- [Agent Sandbox project](https://github.com/kubernetes-sigs/agent-sandbox)
- [Agent Sandbox v0.5.0rc1 release](https://github.com/kubernetes-sigs/agent-sandbox/releases/tag/v0.5.0rc1)

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

## Live Smoke

The provider-specific live smoke is guarded by `CRABBOX_LIVE=1` and an explicit
provider selection. It builds `bin/crabbox` unless `CRABBOX_BIN` points at an
existing binary, verifies `doctor`, creates a short-lived `SandboxClaim`,
proves archive sync and env forwarding with a tiny Git fixture, reuses the
retained claim for a replacement-sync proof, checks status/list, then deletes
the claim.

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=agent-sandbox CRABBOX_LIVE_COORDINATOR=0 scripts/live-smoke.sh
# or, directly:
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=agent-sandbox scripts/live-agent-sandbox-smoke.sh
```

The script emits one machine-readable classification:
`live_agent_sandbox_smoke_passed`, `environment_blocked`, `quota_blocked`, or
`diagnostic_only`. Missing kubeconfig, context, warm pool, RBAC, cluster
connectivity, and API readiness issues are reported without pretending a live
mutation succeeded.

`warmup` keeps the sandbox available until explicit `stop` or the configured
Crabbox TTL expires. At expiry, the controller tears down the sandbox workload
but retains the `SandboxClaim` as an exact cleanup handle.
A `run` without `--id` creates a claim and deletes it after the command unless
`--keep` or `--keep-on-failure` retains it. The provider is Linux-only.
`run --lease-output <path>` writes the Agent Sandbox lease ID, slug,
reuse/retention state, and exact cleanup command for orchestration handoff.

## Config

```yaml
provider: agent-sandbox
target: linux
agentSandbox:
  kubectl: kubectl                 # trusted user config only; binary name or absolute path
  kubeconfig: ~/.kube/config       # empty = KUBECONFIG/default kubeconfig
  context: agent-cluster           # required
  namespace: sandboxes             # default: default
  warmPool: linux-pool             # required SandboxWarmPool name
  container: worker                # empty = Kubernetes default container
  workdir: /workspace/crabbox      # default sync target and exec cwd
  sandboxReadyTimeout: 180s
  podReadyTimeout: 180s
  execTimeoutSecs: 600             # 0 = no provider command deadline
  deleteOnRelease: true
  forgetMissing: false
```

Repository-local `.crabbox.yaml` and `crabbox.yaml` files cannot override
`kubectl`, `kubeconfig`, `context`, `namespace`, `warmPool`, `container`, or
`workdir`.
Kubeconfig files may invoke credential plugins, and workload selection can
redirect forwarded source and environment values to another pod, so these
settings are accepted only from trusted user config, environment variables, or
explicit flags. The `kubectl` value must be a bare executable name resolved
through `PATH` or an absolute path; checkout-relative paths are rejected.

Provider flags:

```text
--agent-sandbox-kubectl
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
CRABBOX_AGENT_SANDBOX_KUBECTL
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

1. `doctor` uses `kubectl` discovery, verifies the exact `v1beta1` Agent
   Sandbox resources, verifies the configured warm pool, and checks the
   required RBAC verbs. It does not create a claim.
2. `warmup` or `run` without `--id` creates one `SandboxClaim` named
   `crabbox-<slug>-<lease-hash>` in the configured namespace. The claim points
   at `agentSandbox.warmPool`, sets `spec.lifecycle.shutdownTime` to the
   Crabbox TTL with `shutdownPolicy: Retain`, and carries Crabbox ownership
   labels plus annotations for provider scope, workdir, and container. If `kubectl create`
   loses its response after the API server accepted the object, Crabbox fetches
   that exact deterministic name and adopts it only after ownership, scope, and
   Kubernetes UID validation.
3. Crabbox waits for `SandboxClaim.status.sandbox.name`, fetches the matching
   `Sandbox`, waits for its Ready condition, then resolves the pod from the
   sandbox pod annotation or selector and waits for the pod Ready condition.
   A `Finished=True` Sandbox or a pod in `Succeeded`/`Failed` phase stops the
   wait immediately with the terminal reason instead of consuming the timeout.
   Every claim lookup must retain the Kubernetes UID returned by creation.
   The live claim must also keep pointing at the configured warm pool.
   Its shutdown time and retain policy must still match the absolute expiry
   pinned in the local claim.
   Before sync or command execution, the Sandbox must carry that claim UID and
   be controller-owned by the exact `SandboxClaim`; the pod must be
   controller-owned by that exact Sandbox UID. Crabbox pins both downstream UIDs
   and revalidates the full chain immediately before each pod exec, rejecting
   observed deletion, recreation, or redirection before sending source or
   forwarded values. When `agentSandbox.container` is empty, Crabbox resolves
   the pod's default container once, pins its name in the local lease, and
   always passes that exact container to `kubectl exec`.
4. Unless `--no-sync` is set, Crabbox builds a portable archive from the local
   Git file manifest and extracts it into the configured workdir with
   `kubectl exec`. With `sync.delete: true`, extraction happens in a
   hidden staging directory inside the workdir and replaces the workdir contents
   only after upload and extraction succeed, without renaming the workdir
   itself or requiring its parent directory to be writable. This remains valid
   when the workdir is a mounted volume.
5. The command runs through Kubernetes exec in the sandbox pod. Forwarded
   environment values are exported inside the streamed shell script, not placed
   on the local command line.
6. On release, Crabbox deletes only the owned `SandboxClaim` whose labels and
   provider-scope annotation and immutable Kubernetes UID match the local
   claim. Deletion sends that UID as an API-server precondition. The Agent
   Sandbox controller owns the resulting sandbox and pod teardown.

Retained-claim `run`, `stop`, and `cleanup` operations share a per-lease
cross-process lock. A concurrent stop or cleanup waits for the active command
to finish, then re-resolves the local claim before mutating Kubernetes.
`status --wait` returns immediately when the root `SandboxClaim` disappears,
while still polling temporary downstream Sandbox or pod readiness gaps.
Retained claims whose pinned TTL elapsed, or whose controller condition reports
`ClaimExpired` or `SandboxExpired`, return the terminal `expired` state without
waiting for missing downstream resources.

## Claim Scope And Cleanup Safety

Local claim IDs use the `asbx_` prefix. The provider scope includes the
kubeconfig identity, context, namespace, warm pool, and container. Reusing,
listing, status-checking, stopping, or cleaning up a retained claim only
matches claims from the same scope. Kubernetes stores only a SHA-256
fingerprint of that scope, not the local kubeconfig path.

Before deleting a live `SandboxClaim`, Crabbox verifies:

- `crabbox.dev/provider=agent-sandbox`
- `crabbox.dev/lease-id=<local lease id>`
- `crabbox.dev/provider-scope=<SHA-256 scope fingerprint>`
- the current `metadata.uid` matches the UID pinned in the local claim
- `spec.warmPoolRef.name` matches the warm pool pinned in the local claim
- `spec.lifecycle.shutdownTime` and `shutdownPolicy: Retain` match the pinned
  Crabbox TTL expiry

Cleanup performs the same live identity validation before idle checks and
dry-run output, so a preview cannot claim that a replaced or redirected object
would be deleted. After the pinned TTL, the controller removes the Sandbox,
pod, and Service while retaining the exact UID-bearing `SandboxClaim`; Crabbox
then deletes that claim and its local lease through `run`, `stop`, or `cleanup`.

Missing Kubernetes claims are preserved locally by default because a 404 can be
ambiguous across clusters or accounts. Set `--agent-sandbox-forget-missing` or
`CRABBOX_AGENT_SANDBOX_FORGET_MISSING=true` only after confirming the claim is
gone in the intended cluster.

If readiness fails and Kubernetes also rejects the UID-preconditioned cleanup,
Crabbox retains a minimal `not-ready` local lease containing the claim name and
UID. Retry the printed `crabbox stop` command after restoring cluster access;
successful cleanup removes that recovery lease.

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
- `kubectl` is a runtime prerequisite, but Crabbox does not embed the
  Kubernetes Go client libraries. This keeps the CLI dependency and binary
  cost bounded and uses the operator's normal kubeconfig and auth plugins.
- Isolation strength is determined by the Agent Sandbox installation and
  `SandboxTemplate`: runtime class, gVisor or Kata configuration, service
  account, network policy, volumes, and node isolation remain cluster-operator
  responsibilities. A plain pod is not automatically a strong security
  boundary.
- `--checksum` and SSH/rsync-specific behavior do not apply to the pod-exec
  archive path.
- `--no-sync` creates the workdir but does not apply `sync.delete`; retained
  workspaces keep whatever was already present.
- The default container is Kubernetes' default container for the pod. Set
  `agentSandbox.container` when the sandbox pod has multiple containers.
- Kubernetes errors may include cluster resource names. Do not paste live
  errors into public issues without a redaction pass.

## Live Smoke

The guarded live smoke is opt-in and creates one short-lived Crabbox-owned
`SandboxClaim` only when explicit live configuration is present. Because the
smoke forces replacement sync and verifies that a remote-only stale file is
removed on retained reuse, it accepts cluster access, workload selectors, and
`workdir` only from environment variables or an explicit `CRABBOX_CONFIG` file,
not repository-local config:

```sh
CRABBOX_LIVE=1 \
CRABBOX_LIVE_COORDINATOR=0 \
CRABBOX_LIVE_PROVIDERS=agent-sandbox \
CRABBOX_AGENT_SANDBOX_KUBECTL=kubectl \
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
- [kubectl reference](https://kubernetes.io/docs/reference/kubectl/)
