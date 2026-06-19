# Provider Backends

This is the contract reference for Crabbox provider backends: the interfaces a
provider implements, how it registers, what core owns versus what the backend
owns, and the checklist to land a new one. For the step-by-step walkthrough,
read [Authoring a provider](features/provider-authoring.md) first, then use this
page as the reference and review checklist.

Read this when you are:

- adding a new Crabbox provider;
- choosing between an SSH lease backend and a delegated run backend;
- adding provider-specific flags or config;
- reviewing a provider PR for the right ownership boundary;
- designing a future external provider plugin protocol.

Every provider follows one rule:

**Providers configure backends. Core commands own workflows.**

That keeps `crabbox run`, `warmup`, `list`, `status`, `stop`, `cleanup`, Actions
hydration, sync, result collection, rendering, and timing consistent across
providers. A provider describes what it can do and returns a backend object. It
does not fork the command surface.

## Choose the backend shape

Start by picking the execution model. A provider's `Configure` returns a
`Backend`, and core inspects which interfaces that value implements.

### SSH lease backend

Use `SSHLeaseBackend` when the provider can hand Crabbox a reachable SSH target.

Examples: Hetzner Cloud, AWS EC2, GCP Compute Engine, Azure VMs, Proxmox, a
local Docker container, and static BYO SSH hosts.

Core owns the entire workflow after acquisition:

- claim and slug handling;
- SSH readiness checks;
- network target resolution;
- sync and sync guardrails;
- command wrapping and streaming;
- JUnit/result collection;
- Actions runner hydration over SSH;
- heartbeat/touch;
- release.

The backend owns only the provider lifecycle:

```go
type SSHLeaseBackend interface {
	Backend

	Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error)
	Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error)
	List(ctx context.Context, req ListRequest) ([]LeaseView, error)
	ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error
	Touch(ctx context.Context, req TouchRequest) (Server, error)
}
```

Implement this when `LeaseTarget.SSH` can be populated with host, port, user,
key, work root, target OS, and Windows mode.

When the provider's spec sets `Coordinator: CoordinatorSupported` and a broker
URL is configured (`CRABBOX_COORDINATOR`), core wraps your `SSHLeaseBackend` in
a coordinator lease backend automatically (`loadBackend` in
`internal/cli/provider_backend.go`). Lease lifecycle then flows through the
broker over HTTP, while sync and command execution still happen directly from
the CLI to the SSH host. You do not implement that wrapper; you only provide the
direct backend.

### Delegated run backend

Use `DelegatedRunBackend` when the provider owns execution itself instead of
exposing a Crabbox-managed SSH target.

Examples: Blacksmith Testbox, E2B, Islo, Modal, Tensorlake,
[Upstash Box](https://upstash.com/docs/box/overall/quickstart), Superserve,
Vercel Sandbox, and Azure Container Apps dynamic sessions, where the provider
owns workspace setup and command streaming.

The delegated backend owns warmup, command execution, output streaming, and
stop. Core still owns provider selection, config loading, local claims, friendly
slugs, timing summaries, and normalized list/status rendering.

```go
type DelegatedRunBackend interface {
	Backend

	Warmup(ctx context.Context, req WarmupRequest) error
	Run(ctx context.Context, req RunRequest) (RunResult, error)
	List(ctx context.Context, req ListRequest) ([]LeaseView, error)
	Status(ctx context.Context, req StatusRequest) (StatusView, error)
	Stop(ctx context.Context, req StopRequest) error
}
```

Delegated backends return normalized `StatusView` values. Rendering stays
core-owned, so provider packages should not print their own `status` or `list`
tables unless a compatibility interface explicitly asks for native output.

A delegated backend must reject run/sync options that Crabbox cannot honor
without a Crabbox-managed SSH target:

```go
if err := cli.RejectDelegatedSyncOptionsForSpec(spec, req); err != nil {
	return RunResult{}, err
}
```

Providers that declare `FeatureArchiveSync` (an archive upload of the checkout)
can declare that feature in `spec` so `--sync-only` and `--force-sync-large`
are allowed while the rest stay rejected. The helper rejects checksum sync,
full resync, local stdout/stderr captures, capture-on-fail, downloads, artifact
globs, uploaded scripts, env helpers, `--stop-after`, fresh PR checkouts, and
`--emit-proof` (unless the provider declares `FeatureRunProof`) unless another
explicit feature covers the request. Providers that execute source modules
instead of shell commands may declare `FeatureModuleRun`; then `--script` and
`--script-stdin` are accepted as module source input, while trailing shell
command argv remains rejected. Delegated artifact globs require
`FeatureRunArtifacts` and `DelegatedRunArtifactBackend`. Delegated single-file
downloads require `FeatureRunDownloads` and `DelegatedRunDownloadBackend`;
required artifacts may use either capability, but download-only providers accept
safe relative file paths instead of globs. Do not pretend a delegated provider
is SSH-like unless it has a stable SSH contract. If Crabbox cannot run rsync and
remote commands itself, use `DelegatedRunBackend`.

### Optional interfaces

Add optional capabilities as small interfaces instead of widening every backend.

Cleanup is optional:

```go
type CleanupBackend interface {
	Backend

	Cleanup(ctx context.Context, req CleanupRequest) error
}
```

Pause and resume are optional:

```go
type PausableBackend interface {
	Backend

	Pause(ctx context.Context, req PauseRequest) error
	Resume(ctx context.Context, req ResumeRequest) error
}
```

Declare `FeaturePauseResume` when implementing this interface so
`crabbox providers` exposes the capability.

List JSON compatibility is optional:

```go
type JSONListBackend interface {
	Backend

	ListJSON(ctx context.Context, req ListRequest) (any, error)
}
```

`JSONListBackend` is a compatibility escape hatch for script-facing JSON shapes.
Use it only when an existing provider already exposed a JSON schema different
from the normalized `[]LeaseView` shape. Do not use it for new providers.

Provider doctor checks are optional for direct providers that can prove cheap,
non-mutating readiness:

```go
type DoctorBackend interface {
	Backend

	Doctor(ctx context.Context, req DoctorRequest) (DoctorResult, error)
}
```

Use `DoctorBackend` when a provider owns direct credentials or a delegated runner
outside the coordinator. The check must validate provider-specific readiness
without creating resources, and it must not treat unrelated coordinator health as
proof that the provider itself is configured correctly. Expose it through the
matching provider-level hook so `doctor` does not configure every provider just
to discover the optional capability:

```go
type DoctorProvider interface {
	Provider

	ConfigureDoctor(cfg Config, rt Runtime) (DoctorBackend, error)
}
```

Native checkpoint and fork support follow the same pattern through
`NativeCheckpointProvider` and `NativeCheckpointForkProvider`. Future
provider-specific capability areas, such as pricing or image management, should
add similarly narrow interfaces rather than widening the base backend.

## Package layout

Built-in providers live under `internal/providers/<name>`. The registry is
populated by side-effect `init()` registration, gathered in
`internal/providers/all`:

```text
internal/providers/all                  # side-effect imports of every provider
internal/providers/shared               # shared direct SSH retry/touch/cleanup helpers
internal/providers/aws                  # AWS EC2 SSH lease backend (coordinator)
internal/providers/azure                # Azure VM SSH lease backend (coordinator)
internal/providers/azuredynamicsessions # Azure Container Apps delegated runner
internal/providers/gcp                  # GCP Compute Engine SSH lease backend (coordinator)
internal/providers/hetzner              # Hetzner Cloud SSH lease backend (coordinator)
internal/providers/linode               # Linode SSH lease backend
internal/providers/scaleway             # Scaleway SSH lease backend
internal/providers/proxmox              # Proxmox VE SSH lease backend
internal/providers/parallels            # Parallels macOS VM host SSH lease backend
internal/providers/localcontainer       # local Docker container SSH backend
internal/providers/multipass            # Canonical Multipass local Ubuntu VM SSH backend
internal/providers/ssh                  # static / BYO SSH backend
internal/providers/daytona              # Daytona SSH lease + delegated SDK backend
internal/providers/kubevirt             # generic KubeVirt SSH backend
internal/providers/external             # executable provider protocol
internal/providers/tenki                # Tenki sandbox SSH backend
internal/providers/namespace            # Namespace devbox SSH backend
internal/providers/namespaceinstance    # Namespace Compute instance SSH backend
internal/providers/semaphore            # Semaphore SSH lease backend
internal/providers/sprites              # Sprites SSH backend
internal/providers/exedev               # exe.dev SSH backend
internal/providers/runpod               # RunPod GPU pod SSH backend
internal/providers/nvidiabrev           # NVIDIA Brev GPU workspace SSH backend
internal/providers/railway              # Railway.app delegated backend
internal/providers/blacksmith           # Blacksmith Testbox delegated backend
internal/providers/e2b                  # E2B delegated backend
internal/providers/islo                 # Islo delegated backend
internal/providers/modal                # Modal delegated backend
internal/providers/opencomputer         # OpenComputer delegated backend
internal/providers/opensandbox          # OpenSandbox delegated backend
internal/providers/tensorlake           # Tensorlake delegated backend
internal/providers/upstashbox           # Upstash Box delegated backend
internal/providers/cloudflare           # Cloudflare Containers delegated backend
internal/providers/wandb                # Weights & Biases delegated backend
```

Each provider package owns registration, provider name, aliases, spec,
provider-specific flags, backend configuration, provider clients, provider
lifecycle code, and provider-specific tests. `cmd/crabbox` imports
`internal/providers/all` for side-effect registration:

```go
import (
	"github.com/openclaw/crabbox/internal/cli"
	_ "github.com/openclaw/crabbox/internal/providers/all"
)
```

The core provider contract lives in `internal/cli`:

```text
internal/cli/provider_backend.go      # interfaces, registry, request/result types
internal/cli/provider_coordinator.go  # brokered coordinator lease wrapper
internal/cli/provider_labels.go       # shared direct-provider label helpers
```

Provider packages may use small exported core helpers for claims, labels, sync
preflight, timing JSON, and SSH key storage. Keep that helper surface narrow: if
a provider needs broad command orchestration, the behavior probably belongs in
core instead.

## Provider registration

A provider implements `cli.Provider`:

```go
type Provider interface {
	Name() string
	Aliases() []string
	Spec() ProviderSpec

	RegisterFlags(fs *flag.FlagSet, defaults Config) any
	ApplyFlags(cfg *Config, fs *flag.FlagSet, values any) error

	Configure(cfg Config, rt Runtime) (Backend, error)
}
```

A minimal SSH provider package:

```go
package example

import (
	"flag"

	"github.com/openclaw/crabbox/internal/cli"
)

func init() {
	cli.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string      { return "example" }
func (Provider) Aliases() []string { return nil }

func (Provider) Spec() cli.ProviderSpec {
	return cli.ProviderSpec{
		Name: "example",
		Kind: cli.ProviderKindSSHLease,
		Targets: []cli.TargetSpec{
			{OS: "linux"},
		},
		Features: cli.FeatureSet{
			cli.FeatureSSH,
			cli.FeatureCrabboxSync,
		},
		Coordinator: cli.CoordinatorNever,
	}
}

func (Provider) RegisterFlags(*flag.FlagSet, cli.Config) any {
	return cli.NoProviderFlags()
}

func (Provider) ApplyFlags(*cli.Config, *flag.FlagSet, any) error {
	return nil
}

func (p Provider) Configure(cfg cli.Config, rt cli.Runtime) (cli.Backend, error) {
	return cli.NewExampleLeaseBackend(p.Spec(), cfg, rt), nil
}
```

`NewExampleLeaseBackend` stands in for the backend constructor you add for the
provider. Existing providers use constructors such as `NewAWSLeaseBackend` and
`NewBlacksmithBackend`.

Then add the side-effect import in `internal/providers/all/all.go`:

```go
import _ "github.com/openclaw/crabbox/internal/providers/example"
```

Tests in `internal/cli` do not import `internal/providers/all`, because that
would create an import cycle. Register test providers from a same-package test
file when testing core dispatch.

## Provider spec

`ProviderSpec` is command-facing metadata:

```go
type ProviderSpec struct {
	Name        string
	Family      string
	Kind        ProviderKind
	Targets     []TargetSpec
	Features    FeatureSet
	Coordinator CoordinatorMode
}
```

Use canonical provider names in docs and config. Aliases are for compatibility
only. `Family` groups related providers so a flag set by one can route to a
sibling (for example, the Azure family covers `azure` and
`azure-dynamic-sessions`); leave it empty to default to the provider name.

Pick `Kind` carefully:

- `ProviderKindSSHLease`: provider returns SSH targets and Crabbox owns sync/run.
- `ProviderKindDelegatedRun`: provider owns execution and output streaming.

`Targets` should describe what the provider can actually satisfy. Use `linux`,
`macos`, or `windows` only for real operating-system targets. Use
`worker-runtime` for Worker-isolate or module-runtime providers that execute
source in a hosted runtime without POSIX shell, SSH, filesystem sync, ports, or
desktop semantics. Do not list `windows`, `macos`, `desktop`, `browser`, or
`code` unless the backend supports that path end to end.

Feature flags are concrete capability declarations:

```go
cli.FeatureSSH          // "ssh"
cli.FeatureCrabboxSync  // "crabbox-sync"
cli.FeatureArchiveSync  // "archive-sync"
cli.FeatureCleanup      // "cleanup"
cli.FeatureDesktop      // "desktop"
cli.FeatureBrowser      // "browser"
cli.FeatureCode         // "code"
cli.FeatureTailscale    // "tailscale"
cli.FeatureURLBridge    // "url-bridge"
cli.FeatureCheckpoint   // "workspace-checkpoint"
cli.FeatureFork         // "workspace-fork"
cli.FeatureRestore      // "workspace-restore"
cli.FeatureSnapshot     // "provider-snapshot"
cli.FeatureCacheVolume  // "cache-volume"
cli.FeatureRunProof     // "run-proof"
cli.FeatureRunSession   // "run-session"
cli.FeatureModuleRun    // "module-run"
cli.FeatureRunArtifacts // "run-artifacts"
cli.FeatureRunDownloads // "run-downloads"
```

Actions runner hydration is intentionally not a provider feature. It is a core
SSH-over-Linux/Windows workflow that requires an SSH lease backend, a
`linux`/`windows` target, and no delegated execution.

Set `CoordinatorSupported` only when the Crabbox broker can provision that
provider. Today that is the managed cloud set (`aws`, `azure`, `gcp`,
`hetzner`). A direct-only SSH provider should use `CoordinatorNever`. Even a
`CoordinatorSupported` provider runs direct from the CLI until a broker URL/token
is configured.

Checkpoint-related features are reserved for versioned workspaces:

- `FeatureCheckpoint`: provider can create a provider-aware checkpoint.
- `FeatureFork`: provider can create a new workspace from a checkpoint.
- `FeatureRestore`: provider can restore an existing workspace to a checkpoint.
- `FeatureSnapshot`: provider can expose a native snapshot id for Crabbox
  metadata.
- `FeatureCacheVolume`: provider can mount keyed rebuildable cache volumes on
  warmup/run.
- `FeatureRunProof`: delegated provider can return bounded stream/timing metadata
  for core `crabbox run --emit-proof` rendering.
- `FeatureRunSession`: delegated proof/session runner that exposes a run session
  handle.
- `FeatureRunArtifacts`: delegated provider can validate and collect bounded run
  artifact globs after a successful command, including required artifacts.
- `FeatureRunDownloads`: delegated provider can materialize bounded single-file
  downloads and validate safe relative single-file required artifacts after a
  successful command.
- `FeatureModuleRun`: delegated provider accepts `--script` or `--script-stdin`
  as source module input and does not interpret trailing argv as a shell command.
- `FeatureArchiveSync`: provider syncs the checkout as an uploaded archive rather
  than over rsync.
- `FeatureURLBridge`: delegated provider can expose a lease's port through the
  broker URL bridge.

Do not set the checkpoint flags for plain SSH access alone. Generic
Git/archive/log checkpoints are core-owned and work even when a provider
advertises no native checkpoint features.

## Flags and config

Provider flags are registered before parsing because Go's `flag` package rejects
unknown flags. `RegisterFlags` must be cheap and side-effect free. It returns an
opaque values struct passed back into `ApplyFlags` only after config and common
flags select the provider.

Pattern for a provider with typed config fields:

```go
type exampleFlagValues struct {
	Region *string
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults cli.Config) any {
	return exampleFlagValues{
		Region: fs.String("example-region", defaults.Example.Region, "Example region"),
	}
}

func (Provider) ApplyFlags(cfg *cli.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(exampleFlagValues)
	if !ok {
		return nil
	}
	if cli.FlagWasSet(fs, "example-region") {
		cfg.Example.Region = *v.Region
	}
	return nil
}
```

`Config` does not have a generic provider config bag. New provider packages
should either add typed config fields and use `cli.FlagWasSet` from the provider
package, or expose a small provider-specific flag helper from `internal/cli` (as
Blacksmith does) when the config type is not ready to export cleanly.

If a provider needs durable config, add typed config fields in `Config` and env
overrides in `config.go`.

Never pass provider secrets as command-line arguments. Use environment variables,
local SDK config, the broker, or a credential store outside repo config.

## Runtime

Backends receive a narrow runtime:

```go
type Runtime struct {
	Stdout io.Writer
	Stderr io.Writer
	Clock  Clock
	HTTP   *http.Client
	Exec   CommandRunner
}
```

Use it instead of `App`, global clocks, or package-level command hooks.

Delegated CLI integrations must use `Runtime.Exec`:

```go
result, err := rt.Exec.Run(ctx, cli.LocalCommandRequest{
	Name:   "provider-cli",
	Args:   args,
	Stdout: rt.Stdout,
	Stderr: rt.Stderr,
})
```

This gives tests a fake command runner and avoids package-level
`exec.CommandContext` seams. Use `Runtime.Clock` for timing and `Runtime.Stdout`
/ `Runtime.Stderr` for streaming and warnings.

## Implementing an SSH lease backend

An SSH lease backend returns a complete `LeaseTarget`:

```go
type LeaseTarget struct {
	Server      Server
	SSH         SSHTarget
	LeaseID     string
	Coordinator *CoordinatorClient
}
```

`Acquire` should:

1. validate direct-provider prerequisites;
2. mint or accept the lease id handled by the request path;
3. ensure or install the SSH key;
4. provision the machine or sandbox;
5. wait until an address exists;
6. populate `SSHTarget`;
7. wait for SSH readiness when the provider owns boot;
8. mark provider labels/tags as ready;
9. return `LeaseTarget`.

`Resolve` should accept canonical lease IDs, provider IDs, names, and slugs where
the provider can support them. It should return the stored per-lease SSH key when
available.

`List` returns normalized `LeaseView` values. Do not print from `List`; command
rendering belongs to core.

`Touch` should update provider labels/tags with idle and state metadata when the
provider supports it. Static providers can update only the in-memory view.

`ReleaseLease` should be idempotent where practical. Remove local claims after the
provider release succeeds or is known to be unnecessary.

If cleanup is meaningful, implement `CleanupBackend`. Cleanup should honor
`DryRun`, log skip/delete decisions to stderr, and use provider labels to avoid
deleting unrelated machines.

## Implementing a delegated run backend

A delegated backend should preserve Crabbox ergonomics while letting the provider
own the remote workflow.

`Warmup` should:

1. validate provider-specific workflow config;
2. create or warm the provider resource;
3. claim the resource locally with provider name and slug;
4. print the standard warmup summary;
5. write timing JSON when requested.

`Run` should:

1. reject unsupported Crabbox sync options;
2. acquire a resource or resolve an existing id/slug;
3. claim/reclaim the resource for the repo;
4. stream provider output through `Runtime.Stdout` and `Runtime.Stderr`;
5. return `RunResult`;
6. stop temporary resources when `Keep` is false.

`List` and `Status` should return normalized views. If the provider only offers a
table or lossy native status shape, keep that parsing inside the backend.

`Stop` should stop the provider resource, remove local claims, and remove local
per-resource keys if the backend created them.

Do not make delegated providers support `crabbox ssh`, `vnc`, `webvnc`,
`screenshot`, `code`, or Actions runner hydration unless the provider exposes a
stable connection contract that preserves Crabbox's security boundary.

## Rendering

Backends return values. Core renders output.

`ListRequest` and `StatusRequest` intentionally do not carry JSON flags. The
command handler decides whether to render human output or JSON.

`JSONListBackend` is the only exception, for compatibility with older
script-facing JSON schemas. It should not be used for new providers.

That rule keeps `crabbox list --json`, `crabbox status --json`, human tables, and
future UI/plugin consumers consistent across backend kinds.

## External provider plugins

External process plugins are not implemented yet. Do not add a provider that
depends on an undocumented stdio protocol.

The intended direction is:

- a built-in Go provider package discovers and configures the external process;
- the process speaks JSON over stdio;
- the Go side adapts it to `SSHLeaseBackend` or `DelegatedRunBackend`;
- core commands still render list/status and own SSH workflows where applicable.

Expected rough command shape:

```text
provider-plugin capabilities
provider-plugin acquire
provider-plugin resolve
provider-plugin list
provider-plugin release
provider-plugin touch
provider-plugin run
provider-plugin status
provider-plugin stop
```

The external protocol should not bypass the backend interfaces. It is an
implementation detail behind a normal registered provider.

## Tests

Add tests at the lowest level that proves the contract.

For provider registration:

- canonical name resolves through `ProviderFor`;
- aliases resolve where promised;
- `Spec` has the expected kind, targets, features, and coordinator mode;
- provider-specific flags apply only after selection.

For SSH lease backends:

- acquire success returns a `LeaseTarget` with host, user, port, key, lease id;
- acquire failure releases partial resources when possible;
- resolve supports lease id and supported aliases;
- list returns normalized views without printing;
- touch updates labels/tags and honors state/idle timeout;
- release removes claims and provider resources;
- cleanup honors dry-run.

For delegated run backends:

- sync-only/checksum/force-large options are rejected as the spec dictates;
- new run acquires, claims, streams, and stops when `Keep=false`;
- existing id/slug resolves and claims correctly;
- list/status parse provider output into normalized views;
- stop removes claims and local keys;
- all subprocess calls go through `Runtime.Exec`.

Use fake `CommandRunner`, fake clocks, fake HTTP clients, and provider test
clients. Avoid live provider calls in unit tests.

Run at least:

```sh
go test -count=1 ./internal/cli ./internal/providers/...
go test -count=1 ./...
go vet ./...
scripts/check-docs.sh
```

For high-risk provider changes, also run:

```sh
go test -race -count=1 ./internal/cli
go build -trimpath -o bin/crabbox ./cmd/crabbox
```

Add live smoke only when credentials and cost boundaries are explicit.

## Review checklist

Before landing a new backend:

- The provider has a folder under `internal/providers/<name>`.
- The provider is imported by `internal/providers/all`.
- `Name` is canonical and docs use that name.
- Compatibility aliases are intentional and tested.
- `ProviderSpec.Kind` matches the real execution model.
- `Family` is set when the provider routes flags to a sibling.
- Targets and features describe implemented behavior only.
- Coordinator mode is `CoordinatorNever` unless the broker can provision it.
- Provider flags are registered before parse and applied only after selection.
- Secrets are not stored in repo config or passed in argv.
- `list` and `status` return normalized values instead of printing.
- Delegated providers reject unsupported sync options.
- SSH providers do not own core sync/run/rendering.
- Tests cover command dispatch and backend behavior without live credentials.
- Docs and the [source map](source-map.md) are updated.

## See also

- [Authoring a provider](features/provider-authoring.md): step-by-step guide.
- [Providers overview](features/providers.md): the full provider catalog.
- [Concepts](concepts.md): how providers fit the lease/run model.
