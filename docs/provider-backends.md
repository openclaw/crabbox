# Provider Backends

Read when:

- adding a new Crabbox provider;
- deciding between an SSH lease backend and a delegated run backend;
- adding provider-specific flags or config;
- reviewing a provider PR for the right ownership boundary;
- designing a future external provider plugin protocol.

Crabbox providers are built around one rule:

Providers configure backends. Core commands own workflows.

That keeps `crabbox run`, `warmup`, `list`, `status`, `stop`, `cleanup`,
Actions hydration, sync, result collection, rendering, and timing consistent
across providers. A provider should describe what it can do and return a backend
object. It should not fork the command surface.

## Choose The Backend Shape

Start by choosing the execution model.

### SSH Lease Backend

Use `SSHLeaseBackend` when the provider can hand Crabbox an SSH target.

Examples:

- Hetzner Cloud
- AWS EC2
- static SSH hosts

Crabbox core owns the normal workflow after acquisition:

- claim and slug handling;
- SSH readiness checks;
- network target resolution;
- sync and sync guardrails;
- command wrapping and streaming;
- JUnit/result collection;
- Actions runner hydration over SSH;
- heartbeat/touch;
- release.

The backend owns only provider lifecycle:

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

### Delegated Run Backend

Use `DelegatedRunBackend` when the provider owns execution instead of exposing
Crabbox-managed SSH.

Examples:

- Blacksmith Testbox
- Islo sandboxes, where Islo owns workspace setup and command streaming
- Daytona sandboxes for `run`, where Daytona toolbox owns file upload and
  process execution while `crabbox ssh` still uses short-lived SSH tokens
- a future external runner service that accepts a command and streams output

The delegated backend owns warmup, command execution, output streaming, and
stop. Crabbox core still owns provider selection, config loading, local claims,
friendly slugs, timing summaries, and normalized list/status rendering.

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

Delegated backends return normalized `StatusView` values. Rendering remains
core-owned, so provider packages should not print their own `status` or `list`
tables unless a compatibility interface explicitly asks for native output.

A delegated backend must reject run/sync options that Crabbox cannot honor
without a Crabbox-managed SSH target:

```go
if err := cli.RejectDelegatedSyncOptions(providerName, req); err != nil {
	return RunResult{}, err
}
```

That helper rejects sync-only options, checksum sync, forced large sync,
local stdout/stderr captures, failure bundles, downloads, uploaded scripts, and
fresh PR checkouts. Do not pretend a delegated provider is SSH-like unless the
provider has a stable SSH contract. If Crabbox cannot run rsync and remote
commands itself, use `DelegatedRunBackend`.

### Optional Interfaces

Add optional capabilities as small interfaces instead of widening every backend.

Cleanup is already optional:

```go
type CleanupBackend interface {
	Backend

	Cleanup(ctx context.Context, req CleanupRequest) error
}
```

List JSON compatibility is optional:

```go
type JSONListBackend interface {
	Backend

	ListJSON(ctx context.Context, req ListRequest) (any, error)
}
```

`JSONListBackend` is a compatibility escape hatch for script-facing JSON shapes.
Use it only when an existing provider already exposed a different JSON schema
than the normalized `[]LeaseView` shape.

Future provider-specific capability areas should follow the same pattern, for
example pricing or image management.

## Package Layout

Built-in providers live under `internal/providers/<name>`:

```text
internal/providers/all
internal/providers/hetzner
internal/providers/aws
internal/providers/azure
internal/providers/gcp
internal/providers/proxmox
internal/providers/ssh
internal/providers/blacksmith
internal/providers/namespace
internal/providers/sprites
internal/providers/daytona
internal/providers/islo
internal/providers/e2b
internal/providers/semaphore
internal/providers/tensorlake
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

The core provider contract remains in `internal/cli`; built-in implementations
live in their provider folders:

```text
internal/cli/provider_backend.go          # interfaces, registry, request/result types
internal/cli/provider_coordinator.go      # brokered coordinator lease wrapper
internal/cli/provider_labels.go           # shared direct-provider label helpers
internal/providers/shared                 # shared direct SSH retry/touch/cleanup helpers
internal/providers/aws                    # AWS SSH lease backend
internal/providers/azure                  # Azure SSH lease backend
internal/providers/gcp                    # Google Cloud SSH lease backend
internal/providers/hetzner                # Hetzner SSH lease backend
internal/providers/proxmox                # Proxmox VE SSH lease backend
internal/providers/ssh                    # static SSH backend
internal/providers/blacksmith             # Blacksmith delegated backend
internal/providers/namespace              # Namespace Devbox SSH backend
internal/providers/sprites                # Sprites SSH backend
internal/providers/daytona                # Daytona SSH + delegated SDK backend
internal/providers/islo                   # Islo delegated backend
internal/providers/e2b                    # E2B delegated backend
internal/providers/semaphore              # Semaphore SSH lease backend
internal/providers/tensorlake             # Tensorlake delegated CLI backend
```

Provider packages may use small exported core helpers for claims, labels,
sync preflight, timing JSON, and SSH key storage. Keep that helper surface
narrow: if a provider needs broad command orchestration, the behavior probably
belongs in core instead.

## Provider Registration

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

Minimal SSH provider package:

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

func (Provider) Name() string { return "example" }
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

Then add the provider to `internal/providers/all/all.go`:

```go
import _ "github.com/openclaw/crabbox/internal/providers/example"
```

Tests in `internal/cli` do not import `internal/providers/all`, because that
would create an import cycle. Register test providers from a same-package test
file when testing core dispatch.

## Provider Spec

`ProviderSpec` is command-facing metadata:

```go
type ProviderSpec struct {
	Name        string
	Kind        ProviderKind
	Targets     []TargetSpec
	Features    FeatureSet
	Coordinator CoordinatorMode
}
```

Use canonical provider names in docs and config. Aliases are for compatibility.

Pick `Kind` carefully:

- `ProviderKindSSHLease`: provider returns SSH targets and Crabbox owns sync/run.
- `ProviderKindDelegatedRun`: provider owns execution and output streaming.

Targets should describe what the provider can actually satisfy. Do not list
`windows`, `macos`, `desktop`, `browser`, or `code` unless the backend supports
that path end to end.

Feature flags should be concrete:

```go
cli.FeatureSSH
cli.FeatureCrabboxSync
cli.FeatureCleanup
cli.FeatureDesktop
cli.FeatureBrowser
cli.FeatureCode
cli.FeatureTailscale
```

Actions runner hydration is intentionally not a provider feature. It is a core
SSH-over-Linux workflow. It requires:

- an SSH lease backend;
- `target=linux`;
- no delegated execution.

Only set `CoordinatorSupported` when the Crabbox coordinator can provision that
provider. A direct-only SSH provider should use `CoordinatorNever`.

## Flags And Config

Provider flags are registered before parsing because Go's `flag` package rejects
unknown flags. `RegisterFlags` must be cheap and side-effect free. It returns an
opaque values struct that is passed back into `ApplyFlags` only after config and
common flags select the provider.

Pattern, when the provider has an exported flag helper or lives in `internal/cli`:

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

`Config` does not yet have a generic provider config bag. New provider packages
should either:

- add typed config fields and use `cli.FlagWasSet` from the provider package; or
- expose a small provider-specific flag helper from `internal/cli`, as
  Blacksmith does, when the config type is not ready to export cleanly.

If a provider needs durable config, add typed config fields in `Config` and env
overrides in `config.go`. Keep compatibility shims for existing top-level
provider config, but prefer `providers.<name>` for new provider families once
that config bag lands.

Never pass provider secrets as command-line arguments. Use environment variables,
local SDK config, the coordinator, or a credential store outside repo config.

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
`exec.CommandContext` seams.

Use `Runtime.Clock` for timing in backend code. Use `Runtime.Stdout` and
`Runtime.Stderr` for streaming and warnings.

## Implementing An SSH Lease Backend

An SSH lease backend should return a complete `LeaseTarget`:

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

`Resolve` should accept canonical lease IDs, provider IDs, names, and slugs
where the provider can support them. It should return the stored per-lease SSH
key when available.

`List` returns normalized `LeaseView` values. Do not print from `List`; command
rendering belongs to core.

`Touch` should update provider labels/tags with idle and state metadata when the
provider supports it. Static providers can update only the in-memory view.

`ReleaseLease` should be idempotent where practical. Remove local claims after
the provider release succeeds or is known to be unnecessary.

If cleanup is meaningful, implement `CleanupBackend`. Cleanup should honor
`DryRun`, log skip/delete decisions to stderr, and use provider labels to avoid
deleting unrelated machines.

## Implementing A Delegated Run Backend

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

`List` and `Status` should return normalized views. If the provider only offers
a table or lossy native status shape, keep that parsing inside the backend.

`Stop` should stop the provider resource, remove local claims, and remove local
per-resource keys if the backend created them.

Do not make delegated providers support `crabbox ssh`, `vnc`, `webvnc`,
`screenshot`, `code`, or Actions runner hydration unless the provider exposes a
stable connection contract that preserves Crabbox's security boundary.

## Rendering

Backends return values. Core renders output.

`ListRequest` and `StatusRequest` intentionally do not carry JSON flags. The
command handler decides whether to render human output or JSON.

`JSONListBackend` is the exception for compatibility with older script-facing
JSON schemas. It should not be used for new providers.

That rule keeps:

- `crabbox list --json`;
- `crabbox status --json`;
- human tables;
- future UI/plugin consumers;

consistent across backend kinds.

## External Provider Plugins

External process plugins are not implemented yet. Do not add a provider that
depends on an undocumented stdio protocol.

The intended direction is:

- a built-in Go provider package discovers/configures the external process;
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

- sync-only/checksum/force-large options are rejected;
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
npm run docs:check
```

For high-risk provider changes, also run:

```sh
go test -race -count=1 ./internal/cli
go build -trimpath -o bin/crabbox ./cmd/crabbox
```

Add live smoke only when credentials and cost boundaries are explicit.

## Review Checklist

Before landing a new backend:

- The provider has a folder under `internal/providers/<name>`.
- The provider is imported by `internal/providers/all`.
- `Name` is canonical and docs use that name.
- Compatibility aliases are intentional and tested.
- `ProviderSpec.Kind` matches the real execution model.
- Targets and features describe implemented behavior only.
- Coordinator mode is `CoordinatorNever` unless the coordinator can provision it.
- Provider flags are registered before parse and applied only after selection.
- Secrets are not stored in repo config or passed in argv.
- `list` and `status` return normalized values instead of printing.
- Delegated providers reject unsupported sync options.
- SSH providers do not own core sync/run/rendering.
- Tests cover command dispatch and backend behavior without live credentials.
- Docs and source map are updated.
