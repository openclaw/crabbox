# Provider Live Smoke

Read when:

- adding or reviewing a provider that needs external credentials, quota, local
  hypervisor access, or a self-hosted control plane;
- deciding whether offline tests are enough for a provider PR;
- writing an opt-in live validation command for a provider doc.

Most provider work must be provable without live credentials. Unit tests should
cover config, command construction, JSON parsing, lifecycle decisions, and error
paths. A live smoke is the extra opt-in proof that the documented real substrate
still matches the offline contract.

## Validation tiers

1. **Hermetic lifecycle** — required CI, no credentials, quota, network access,
   or provider spend. Name full fake-backed lifecycle files
   `lifecycle_test.go` or `*_lifecycle_test.go`; the source-derived Connector
   Lifecycles job discovers their packages and runs them with the race detector.
   Cover acquire, resolve/readiness, use, touch, list, release, cleanup failure,
   and claim retention where the backend supports those stages. The
   `.github/workflows/connector-e2e-smokes.yml` gate additionally drives
   secret-free connector lifecycles end to end on hosted runners: local
   containers, a localhost byo-SSH target, the Docker Sandbox trust-boundary
   proof, and read-only readiness contracts.
2. **Guarded live smoke** — opt-in developer or maintainer proof against the real
   provider. Use `CRABBOX_LIVE=1`, select the exact provider, cap spend and TTL,
   arm cleanup before the first mutation, and prove create/use/destroy with zero
   residue. Funded or remote provider changes require this tier before merge.
3. **Hosted live matrix** — not enabled. A future scheduled or manually
   dispatched secret-backed workflow needs a separate repository policy for
   trusted environments, provider credentials, spend limits, cancellation, and
   orphan auditing. It must never expose secrets to pull-request code.

The hermetic job is a visible merge gate, even though the broader Go job also
runs the same packages. This deliberate overlap makes lifecycle coverage easy to
find and keeps the gate source-derived instead of maintaining another provider
allowlist.

### Lifecycle contract

| Stage | Backend contract | Typical CLI proof |
| --- | --- | --- |
| Acquire | `SSHLeaseBackend.Acquire` or delegated `Warmup` | `crabbox warmup` |
| Resolve / readiness | `Resolve`, delegated `Status` | `crabbox status --wait` |
| Use / sync | SSH execution or delegated `Run` | `crabbox run` |
| Touch | `LeaseTouchBackend.Touch` | status or explicit backend test |
| Inventory | `List`, `ListJSON`, `Inspect` | `crabbox list --json` |
| Optional capabilities | copy, ports, checkpoint, pause/resume | provider-specific commands |
| Release | `ReleaseLease` or delegated `Stop` | `crabbox stop` |
| Orphan cleanup | `CleanupBackend.Cleanup` | `crabbox cleanup --dry-run` |

## Pick Candidates

Start with the checked-in capability matrix:

```sh
crabbox providers recommend live-smoke
crabbox providers recommend offline-validation
crabbox providers recommend cost-control
crabbox providers --json
```

`providers recommend live-smoke` ranks providers that expose enough sync,
cleanup, lifecycle, or evidence metadata to be worth spending real capacity.
It does not prove credentials, quota, regions, templates, Kubernetes contexts, or
provider-side availability. Run `doctor` before creating resources:

```sh
crabbox doctor --provider <name>
```

## Smoke Contract

Every live smoke should prove the narrowest real behavior that offline tests
cannot:

- **SSH lease providers**: acquire or resolve one lease, wait for SSH, sync a
  tiny checkout, run `true` or a small repository command, then release or
  cleanup the lease.
- **Kubernetes-backed SSH lease providers**: also prove the selected context,
  namespace, CRD, RBAC, route configuration, and dry-run cleanup before creating
  a resource. For example, `sealos-devbox` must classify missing kubeconfig,
  context, image, SSHGateway or NodePort route, DevBox RBAC, or
  SSHGate availability as `environment_blocked` instead of claiming live proof
  from unit tests.
- **Delegated run providers**: create or reuse one provider-owned runtime, send a
  tiny command, stream or collect the result, record any session/proof/output
  metadata the provider advertises, then stop or cleanup when the lifecycle
  claims cleanup.
- **Service-control providers**: inspect, start, stop, or redeploy the named
  service without claiming arbitrary command execution.
- **Local runtimes**: prove host prerequisite detection, create one disposable
  runtime, run a tiny command, and delete it. Local smokes are still opt-in when
  they mutate local VM, container, hypervisor, or sandbox state.
- **BYO or external providers**: prove the documented handoff contract only:
  stable host ID, SSH target or external lease metadata, command execution, and
  cleanup semantics if Crabbox owns cleanup.

Do not turn a live smoke into an integration suite. The goal is to prove the
provider boundary, not the provider's whole product.

## Evidence To Keep

A useful smoke leaves enough output to debug a failed adapter without leaking
secrets:

- provider, target, region or local runtime when relevant;
- lease ID, slug, session ID, or service ID when one exists;
- command exit code and timing summary;
- proof, artifact, download, preview URL, or cleanup command when the provider
  advertises that capability;
- exact cleanup outcome.

Scrub tokens, personal paths, private hostnames, and private IP addresses before
copying smoke output into an issue, PR, or fixture.

## Provider Docs

Each provider that needs real credentials should document an opt-in smoke with:

- required CLI or SDK authentication;
- required env/config variables;
- quota, cost, local mutation, or cleanup risk;
- the smallest command that proves the provider contract;
- the cleanup command to run if the smoke is interrupted.

When live credentials are unavailable, land the offline tests and docs first.
Mark the live smoke as opt-in instead of weakening the provider contract or
pretending an untested live path is proven.

## Tagged Go live smokes

Some adapters keep narrow live checks next to their backend. These remain
excluded from normal CI by the `smoke` build tag:

```sh
ISLO_API_KEY=... go test -tags smoke -run TestLiveIsloStatusClassification -v ./internal/providers/islo
CRABBOX_LIVE_ISLO_PAUSE_RESUME=1 ISLO_API_KEY=... CRABBOX_LIVE_ISLO_IMAGE=... go test -tags smoke -run TestLiveIsloPauseResumeLifecycle -v ./internal/providers/islo
CRABBOX_MORPH_API_KEY=... CRABBOX_LIVE_MORPH_SNAPSHOT=... go test -tags smoke -run TestLiveMorphAcquireResolveTouchReleaseLease -v ./internal/providers/morph
CRABBOX_WANDB_API_KEY=... WANDB_ENTITY_NAME=... go test -tags smoke -run TestSmokeVersionAndExec -v ./internal/providers/wandb
```

Use environment injection or an approved credential store; do not put secret
values on command lines, in repository config, fixtures, proof logs, or shell
history.

<!-- BEGIN GENERATED PROVIDER LIFECYCLE COVERAGE -->

## Source-derived coverage matrix

This matrix is generated from the registered provider list, convention-named
hermetic lifecycle tests, `scripts/live-smoke.sh`, dedicated live runners, and
`//go:build smoke` tests. Regenerate it with
`node scripts/generate-provider-matrix.mjs`; docs CI rejects drift.

Current coverage: 76 providers; 4 with convention-named hermetic lifecycle tests, 56 with a live runner, 8 with tagged Go smoke tests, and 19 with none of those lifecycle surfaces.

| Provider | Hermetic lifecycle | Live runner | Tagged Go smoke |
| --- | --- | --- | --- |
| [agent-sandbox](../providers/agent-sandbox.md) | — | dedicated + matrix | — |
| [anthropic-sandbox-runtime](../providers/anthropic-sandbox-runtime.md) | — | dedicated + matrix | — |
| [apple-container](../providers/apple-container.md) | — | matrix | yes |
| [apple-machine](../providers/apple-machine.md) | — | — | — |
| [apple-vm](../providers/apple-vm.md) | — | matrix | yes |
| [ascii-box](../providers/ascii-box.md) | — | — | — |
| [aws](../providers/aws.md) | — | matrix | — |
| [aws-lambda-microvm](../providers/aws-lambda-microvm.md) | — | dedicated + matrix | — |
| [azure](../providers/azure.md) | — | matrix | — |
| [azure-dynamic-sessions](../providers/azure-dynamic-sessions.md) | — | — | — |
| [blacksmith-testbox](../providers/blacksmith-testbox.md) | — | matrix | — |
| [blaxel](../providers/blaxel.md) | — | dedicated | — |
| [cloudflare](../providers/cloudflare.md) | — | dedicated | — |
| [cloudflare-dynamic-workers](../providers/cloudflare-dynamic-workers.md) | — | dedicated | — |
| [cloudflare-sandbox](../providers/cloudflare-sandbox.md) | — | — | — |
| [coder](../providers/coder.md) | — | matrix | — |
| [codesandbox](../providers/codesandbox.md) | — | dedicated | — |
| [crownest](../providers/crownest.md) | — | dedicated | — |
| [cua](../providers/cua.md) | — | dedicated + matrix | — |
| [cubesandbox](../providers/cubesandbox.md) | — | — | — |
| [daytona](../providers/daytona.md) | — | matrix | — |
| [digitalocean](../providers/digitalocean.md) | — | dedicated + matrix | — |
| [docker-sandbox](../providers/docker-sandbox.md) | — | dedicated + matrix | — |
| [e2b](../providers/e2b.md) | — | matrix | — |
| [exe-dev](../providers/exe-dev.md) | — | — | — |
| [external](../providers/external.md) | — | matrix | yes |
| [fastapi-cloud](../providers/fastapi-cloud.md) | — | — | — |
| [firecracker](../providers/firecracker.md) | yes (`firecracker`) | dedicated | — |
| [freestyle](../providers/freestyle.md) | — | — | — |
| [gcp](../providers/gcp.md) | — | — | — |
| [hetzner](../providers/hetzner.md) | — | matrix | — |
| [hostinger](../providers/hostinger.md) | — | — | — |
| [hyperv](../providers/hyperv.md) | — | — | — |
| [incus](../providers/incus.md) | — | matrix | — |
| [islo](../providers/islo.md) | — | — | yes |
| [kubevirt](../providers/kubevirt.md) | — | matrix | — |
| [lambda](../providers/lambda.md) | — | dedicated | — |
| [linode](../providers/linode.md) | — | dedicated + matrix | — |
| [local-container](../providers/local-container.md) | — | matrix | yes |
| [modal](../providers/modal.md) | — | matrix | — |
| [morph](../providers/morph.md) | — | matrix | yes |
| [multipass](../providers/multipass.md) | — | matrix | — |
| [mxc](../providers/mxc.md) | — | — | — |
| [namespace-devbox](../providers/namespace-devbox.md) | — | matrix | — |
| [namespace-instance](../providers/namespace-instance.md) | — | matrix | — |
| [nebius](../providers/nebius.md) | yes (`nebius`) | dedicated + matrix | — |
| [nomad](../providers/nomad.md) | yes (`nomad`) | dedicated + matrix | — |
| [nvidia-brev](../providers/nvidia-brev.md) | — | dedicated + matrix | — |
| [opencomputer](../providers/opencomputer.md) | — | — | — |
| [opensandbox](../providers/opensandbox.md) | — | dedicated + matrix | — |
| [orgo](../providers/orgo.md) | — | matrix | yes |
| [ovh](../providers/ovh.md) | — | dedicated + matrix | — |
| [parallels](../providers/parallels.md) | — | — | — |
| [phala](../providers/phala.md) | — | dedicated + matrix | — |
| [proxmox](../providers/proxmox.md) | — | dedicated + matrix | — |
| [railway](../providers/railway.md) | — | — | — |
| [runpod](../providers/runpod.md) | — | dedicated + matrix | — |
| [scaleway](../providers/scaleway.md) | yes (`scaleway`) | dedicated + matrix | — |
| [sealos-devbox](../providers/sealos-devbox.md) | — | matrix | — |
| [semaphore](../providers/semaphore.md) | — | matrix | — |
| [smolvm](../providers/smolvm.md) | — | dedicated + matrix | — |
| [sprites](../providers/sprites.md) | — | matrix | — |
| [ssh](../providers/ssh.md) | — | — | — |
| [superserve](../providers/superserve.md) | — | dedicated + matrix | — |
| [tart](../providers/tart.md) | — | matrix | — |
| [tencentcloud](../providers/tencentcloud.md) | — | dedicated + matrix | — |
| [tenki](../providers/tenki.md) | — | matrix | — |
| [tensorlake](../providers/tensorlake.md) | — | — | — |
| [unikraft-cloud](../providers/unikraft-cloud.md) | — | dedicated + matrix | — |
| [upstash-box](../providers/upstash-box.md) | — | — | — |
| [vast](../providers/vast.md) | — | dedicated | — |
| [vercel-sandbox](../providers/vercel-sandbox.md) | — | dedicated + matrix | — |
| [vultr](../providers/vultr.md) | — | dedicated + matrix | — |
| [wandb](../providers/wandb.md) | — | matrix | yes |
| [windows-sandbox](../providers/windows-sandbox.md) | — | — | — |
| [xcp-ng](../providers/xcp-ng.md) | — | dedicated + matrix | — |

<!-- END GENERATED PROVIDER LIFECYCLE COVERAGE -->
