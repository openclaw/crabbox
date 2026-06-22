# Authoring A Provider

Read this when:

- adding a new Crabbox provider end to end;
- porting a hosted runner or sandbox service into Crabbox;
- learning what core owns versus what your backend owns.

This page is the step-by-step guide. The contract reference for backend
interfaces, registration, and the review checklist lives in
[Provider backends](../provider-backends.md). Read this page first, then keep
that reference open as a checklist while you implement.

## What A Provider Does

A Crabbox provider answers four questions:

1. What execution model does the provider expose?
2. What targets and capabilities can it satisfy?
3. How does it acquire, resolve, list, and release a runner?
4. What flags and config does it own that core does not?

Everything else — command parsing, sync, command streaming, recorded runs,
heartbeats, slugs, claims, list/status rendering, JSON output — belongs to
core. A provider that needs to fork those concerns is fighting the design.

The interfaces below live in `internal/cli/provider_backend.go`. The provider
package imports that package (conventionally aliased `core`) and implements
against it.

## Step 1. Pick The Backend Shape

Two execution models exist, selected by `ProviderSpec.Kind`:

- `ProviderKindSSHLease` — the provider hands Crabbox a real SSH target. Core
  owns sync, command streaming, results, heartbeats, and release. Use this when
  you can populate `SSHTarget` with host, port, user, key, work root, and
  target OS.
- `ProviderKindDelegatedRun` — the provider owns command execution and streams
  output back to Crabbox. Use this when you cannot give Crabbox a stable SSH
  contract (for example Blacksmith Testbox, E2B, Modal, Upstash).

If you can give Crabbox SSH, prefer the SSH lease backend. The CLI has more
invested in the SSH path, including Actions hydration, VNC, code-server,
screenshot, and cache stats/warm/purge. A delegated backend cannot reuse those
without a stable connection contract.

| Capability | SSH lease | Delegated run |
|:-----------|:----------|:--------------|
| `crabbox run` | yes | yes |
| `crabbox warmup` | yes | yes |
| `crabbox ssh` | yes | only if you implement short-lived SSH |
| `crabbox vnc` / `code` | yes (Linux + capability) | no |
| `crabbox webvnc` | coordinator-backed or local-container only | no |
| `crabbox actions hydrate` | yes (Linux) | no |
| `crabbox cache stats` / `purge` / `warm` | yes | no |
| Crabbox-owned sync | yes | no — your backend owns sync |
| Coordinator support | optional | not used |

## Step 2. Lay Out The Package

Built-in providers live under `internal/providers/<name>`:

```text
internal/providers/example/
  provider.go      # Provider type, init() registration, Spec()
  backend.go       # SSH lease or delegated run implementation
  flags.go         # provider-specific flag struct (optional)
  example.go       # API client, helpers, types
  example_test.go  # backend tests, no live calls
```

Then add the side-effect import in `internal/providers/all/all.go`:

```go
import _ "github.com/openclaw/crabbox/internal/providers/example"
```

`cmd/crabbox/main.go` already imports `internal/providers/all`, so nothing else
needs to change for the binary to see the new provider.

Tests inside `internal/cli` cannot import `internal/providers/all` because that
creates an import cycle. If you need a test provider for core dispatch, register
it from a same-package test file.

## Step 3. Register The Provider

A provider is a small struct that satisfies `core.Provider`:

```go
package example

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string      { return "example" }
func (Provider) Aliases() []string { return nil }

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:    "example",
		Family:  "example",
		Kind:    core.ProviderKindSSHLease,
		Targets: []core.TargetSpec{{OS: core.TargetLinux}},
		Features: core.FeatureSet{
			core.FeatureSSH,
			core.FeatureCrabboxSync,
			core.FeatureCleanup,
		},
		Coordinator: core.CoordinatorNever,
	}
}

func (Provider) RegisterFlags(*flag.FlagSet, core.Config) any {
	return core.NoProviderFlags()
}

func (Provider) ApplyFlags(*core.Config, *flag.FlagSet, any) error {
	return nil
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return newBackend(p.Spec(), cfg, rt)
}
```

`Name()` is the canonical name used in docs, config (`provider: example`), and
the `--provider` flag. `RegisterProvider` registers the canonical name plus
every alias and panics on a duplicate, so keep names unique. Aliases are for
compatibility — Blacksmith uses `blacksmith` as an alias for
`blacksmith-testbox`. Do not invent aliases for new providers; pick one
canonical name.

`Spec()` is the source of truth for what the provider can do. Read on.

## Step 4. Be Honest In `Spec`

`ProviderSpec` is command-facing metadata. Help text, target validation, and
feature gating all read from it.

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

Rules:

- `Kind` must match the real execution model. Do not declare
  `ProviderKindSSHLease` if you cannot return a usable `SSHTarget`.
- `Family` groups related providers that share config and flag routing (for
  example `azure` and `azure-dynamic-sessions` share `Family: "azure"`). Set it
  to the canonical name when the provider stands alone.
- `Targets` lists only OS combinations you support end to end, as
  `TargetSpec{OS, WindowsMode}`. OS values are `core.TargetLinux`,
  `core.TargetMacOS`, and `core.TargetWindows`; Windows entries use
  `core.WindowsModeNormal` or `core.WindowsModeWSL2`. Hetzner is `linux` only.
  AWS lists Linux, Windows (normal and `wsl2`), and macOS. Static SSH lists all
  three but does no setup; the host must already match.
- `Features` lists concrete capabilities from the `Feature` constants:
  - `FeatureSSH` — plain SSH access works.
  - `FeatureCrabboxSync` — core can rsync a git manifest into the runner.
  - `FeatureArchiveSync` — delegated backend accepts an archive-based sync
    (relaxes some of the delegated sync-option rejections; see Step 6).
  - `FeatureCleanup` — implement `CleanupBackend` for orphan cleanup.
  - `FeatureDesktop`, `FeatureBrowser`, `FeatureCode` — lease can host a visible
    desktop, a browser, or a code-server instance. `FeatureDesktop` enables
    native `crabbox vnc`; WebVNC also needs the coordinator portal or the
    trusted local-container noVNC path.
  - `FeatureTailscale` — lease can join a tailnet via cloud-init / `--tailscale`.
  - `FeatureURLBridge` — delegated backend can expose a forwarded URL.
  - `FeatureCheckpoint`, `FeatureFork`, `FeatureRestore`, `FeatureSnapshot` —
    provider-native workspace/VM state operations beyond the generic local
    ledger (the constant values are `workspace-checkpoint`, `workspace-fork`,
    `workspace-restore`, `provider-snapshot`).
  - `FeatureRunProof`, `FeatureRunSession` — delegated backend can return
    bounded stream/timing proof metadata or a reusable run session.
- `Coordinator` is `CoordinatorSupported` only when the shared coordinator
  provider adapter can provision your runners. Today that is `aws`, `azure`, `gcp`, and
  `hetzner`. Everything else — all delegated run backends and Static SSH —
  sets `CoordinatorNever`. Even a `CoordinatorSupported` provider runs direct
  from the CLI unless a broker URL is configured (see
  [Coordinator](coordinator.md)).

Actions runner hydration is not a feature flag. Core checks for an SSH lease
backend on a `linux` or `windows` target (`localcontainer` is explicitly
rejected). Set `target=linux` only on a backend that can actually satisfy it.

Versioned workspace features describe provider depth, not the presence of
Crabbox checkpoint commands. Core can always record a generic checkpoint from
repo metadata, logs, and artifacts. Set the checkpoint-related flags only when
you can preserve or recreate state the generic ledger cannot, such as sandbox
filesystem state, VM snapshot IDs, or copy-on-write forks.

The provider matrix normalizes those flags into a `workspace` capability array:
`checkpoint`, `fork`, `restore`, and `snapshot-ref`. New providers should make
that normalized contract true instead of adding provider-specific command
branches. For example, a future forkable microVM runtime should declare the same
capabilities as any other provider that can checkpoint and fork workspace state;
its Firecracker, Kubernetes, or image identifiers belong inside the adapter and
checkpoint metadata.

## Step 5. Own Provider-Specific Flags

Go's `flag` package rejects unknown flags, so provider flags must be registered
before parse and applied only after a provider is selected.

```go
type exampleFlagValues struct {
	Region *string
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return exampleFlagValues{
		Region: fs.String("example-region", defaults.Example.Region, "Example region"),
	}
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(exampleFlagValues)
	if !ok {
		return nil
	}
	if core.FlagWasSet(fs, "example-region") {
		cfg.Example.Region = *v.Region
	}
	return nil
}
```

Conventions:

- Prefix every flag name with the provider name (`--example-region`,
  `--aws-region`). Crabbox does not gate flag visibility per provider, so the
  prefix is the only thing keeping namespaces clean.
- `RegisterFlags` must be cheap and side-effect free. It runs for every
  registered provider on every command, even when that provider is not selected.
- Apply only flags that were explicitly set, using `core.FlagWasSet`. Otherwise
  zero values from one command overwrite intentional config from another.
- For providers that need rich config but no flags, return
  `core.NoProviderFlags()` from `RegisterFlags` and ignore the values in
  `ApplyFlags`.
- Provider families that route between members can implement `ProviderRouter`
  (`RouteConfig`) and `ProviderRoutingFlagProvider` (`RoutingFlagNames`) so a
  family-shared flag selects the right sibling. Most new providers do not need
  this.

Never accept secrets as flag arguments. Pull them from environment variables,
SDK config, the broker, or the operator's credential store. Flags are visible in
shell history, process listings, and recorded run logs.

## Step 6. Implement The Backend

Pick the interface that matches the kind you declared. Both embed `Backend`,
which only requires `Spec() ProviderSpec`.

### SSH Lease Backend

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

`Acquire` is the heavy lifter. A complete implementation:

1. validates direct-mode prerequisites (credentials, region, image);
2. accepts the lease ID from `req` or generates one if the provider needs it;
3. ensures or installs the per-lease SSH key with the provider;
4. provisions the machine or sandbox with Crabbox labels/tags;
5. waits for the provider to assign an address;
6. populates `SSHTarget` with host, port, user, key, work root, target OS, and
   any Windows mode;
7. waits for SSH readiness when the provider owns boot;
8. flips provider labels/tags to `ready`;
9. returns the populated `LeaseTarget`.

`Resolve` handles `crabbox run --id`, `crabbox ssh --id`, and similar reuse
paths. Accept canonical lease IDs; accept slugs and provider-native IDs when you
can. Return the stored per-lease SSH key when available so reuse does not need a
fresh key.

`List` returns `[]LeaseView` (a type alias for `Server`). Do not print from
`List` — core renders the table.

`Touch` updates idle/state metadata on the provider when possible. Use the
`internal/cli/provider_labels.go` helpers for safe label encoding. For static
providers, an in-memory update is enough.

`ReleaseLease` is called when a lease ends or expires. Make it idempotent; treat
"not found" as success. Remove local claims and the per-lease key directory
after the provider release succeeds.

If cleanup is meaningful, also implement:

```go
type CleanupBackend interface {
	Backend
	Cleanup(ctx context.Context, req CleanupRequest) error
}
```

Cleanup must honor `CleanupRequest.DryRun`, log every skip/delete decision to
`rt.Stderr`, and filter by Crabbox labels so it never touches unrelated
machines. When a broker is configured, core refuses to call provider cleanup at
all — brokered cleanup belongs to the coordinator scheduler.

### Delegated Run Backend

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

`Warmup` should validate workflow/config, create or warm the provider resource,
claim it locally with the provider name and slug, and print the standard warmup
summary.

`Run` should:

1. reject Crabbox sync options the provider cannot honor:
   ```go
   if err := core.RejectDelegatedSyncOptionsForSpec(p.Spec(), req); err != nil {
       return core.RunResult{}, err
   }
   ```
   `RejectDelegatedSyncOptionsForSpec` reads `Spec().Features`, so declaring
   `FeatureArchiveSync`, `FeatureRunProof`, etc. relaxes the matching
   rejections. Pass a spec without those features for the strict behavior.
2. acquire a resource or resolve an existing id/slug;
3. claim or reclaim the resource for the calling repo;
4. stream provider output through `rt.Stdout` and `rt.Stderr`;
5. return `RunResult` with command duration, exit code, and
   `SyncDelegated: true`;
6. stop temporary resources when `Keep` is false.

`Status` returns a normalized `StatusView`. If the provider only emits a table,
parse it inside the backend and return structured fields — do not print the
native table.

`Stop` should stop the provider resource, remove local claims, and remove
per-resource keys the backend created.

Delegated backends should refuse `crabbox ssh`, `vnc`, `webvnc`, `screenshot`,
`code`, and Actions hydration unless the provider can keep Crabbox's security
boundary intact across those flows.

### Optional Backends

- `DoctorProvider` / `DoctorBackend` — add `ConfigureDoctor` plus a `Doctor`
  method so `crabbox doctor --provider <name>` returns structured
  `DoctorCheck` items instead of a generic message.
- `JSONListBackend` — add `ListJSON` only when a script-facing JSON shape
  already exists and callers depend on it. This is a compatibility escape hatch;
  new providers should return normalized `[]LeaseView` from `List` and let core
  render JSON.

## Step 7. Use The Runtime

Backends receive a narrow runtime instead of touching package-level state:

```go
type Runtime struct {
	Stdout io.Writer
	Stderr io.Writer
	Clock  Clock
	HTTP   *http.Client
	Exec   CommandRunner
}
```

Rules:

- Use `rt.Exec.Run(ctx, core.LocalCommandRequest{...})` for every subprocess.
  Never call `exec.CommandContext` directly. Tests pass a fake `CommandRunner`
  to assert on argv without spawning real processes.
- Use `rt.Clock.Now()` for timing inside the backend. The default is wall-clock;
  tests can pass a fake clock for deterministic timing assertions.
- Use `rt.Stdout` and `rt.Stderr` for streaming and warnings. Do not write
  directly to `os.Stdout` / `os.Stderr`.
- Use `rt.HTTP` for outbound HTTP when the provider has a JSON API. Tests can
  inject a stubbed transport.

Anything that bypasses the runtime breaks tests and parallel safety.

## Step 8. Hand-Off Boundaries

The most common review feedback on a new provider is "this belongs in core."
Use this map:

| Concern | Owned by |
|:--------|:---------|
| `--provider`, `--target`, `--id`, `--profile` parsing | core |
| Config precedence (flags → env → repo → user → defaults) | core |
| Friendly slug generation, normalization, collisions | core |
| Local claim files and `--reclaim` behavior | core |
| SSH key creation and storage under user config | core |
| `crabbox-ready` readiness wait | core |
| Repo manifest, fingerprints, rsync, sanity checks | core |
| Heartbeats, idle expiry math | core (broker) or core direct labels |
| Recorded runs, retained logs, telemetry samples | core |
| List/status table rendering and JSON output | core |
| Provider lifecycle (create, delete, list, label) | provider |
| Provider-native auth (SDK config, env, CLI tokens) | provider |
| Translating provider state into normalized lease views | provider |
| Rejecting unsupported delegated options | provider (via core helper) |

If your provider needs to own one of the core-owned concerns, raise it in the PR
description. The fix is usually a small core helper, not a fork.

## Step 9. Test Without Live Credentials

Land the provider with tests that prove the contract without hitting a real
account. Cover:

- Provider registration: the canonical name resolves through `ProviderFor`,
  declared aliases resolve, `Spec()` returns the right kind/targets/features,
  and flag values apply only when that provider is selected.
- SSH lease backends: `Acquire` populates a complete `LeaseTarget`, partial
  failures release what they created, `Resolve` accepts the supported lookup
  shapes, `List` returns normalized views, `Touch` updates state/idle, and
  `ReleaseLease` is idempotent. If you implement `Cleanup`, assert dry-run
  prints decisions and does not call destructive APIs.
- Delegated run backends: unsupported sync options are rejected, a fresh `Run`
  acquires/streams/stops, an existing `--id` resolves and reuses, `List` and
  `Status` parse provider output into normalized values, `Stop` removes claims,
  and every subprocess goes through `rt.Exec`.

Use the existing fakes:

- a recording `CommandRunner` for argv assertions;
- a fake `Clock` for timing;
- an `http.RoundTripper` test transport for API calls;
- a per-provider test client where the provider has a typed SDK.

Run at least:

```sh
go test -count=1 ./internal/cli ./internal/providers/...
go test -race ./...
go vet ./...
scripts/check-docs.sh
```

Add a live smoke only when the provider can be exercised cheaply with explicit
credentials. Wire it into `scripts/live-smoke.sh` so it runs in the same place
as the others.

## Step 10. Document The Provider

Three doc surfaces care about a new provider:

- `docs/providers/<name>.md` — one page in the provider reference. Use the
  existing pages as a template: target matrix, config keys, env vars, sync
  behavior, expected failures.
- `docs/features/<name>.md` — a feature page when the provider has interesting
  semantics worth a separate read (capacity fallback, sandbox lifecycle,
  workflow integration). Skip it when the reference page already covers it.
- `docs/source-map.md` — add the new package paths under "Providers And Runner
  Bootstrap" so the source map keeps tracking implementation truth.

Also add the provider to:

- `docs/providers/provider-metadata.json`, including selection, lifecycle,
  cleanup, and caveat metadata;
- the index in `docs/features/README.md` if you added a feature page;
- the related-doc lists at the bottom of any pages you cross-link from.

Run `node scripts/generate-provider-matrix.mjs` to regenerate the provider
decision matrix in `docs/providers/README.md`; do not edit the generated table
by hand. Then run `scripts/check-docs.sh` before pushing — it builds the CLI,
validates the provider metadata and command/help surface, checks every internal
link, and rebuilds the docs site.

## Step 11. Ship The PR

A reviewable provider PR includes:

- a folder under `internal/providers/<name>` with `provider.go`, `backend.go`,
  helpers, and tests;
- registration in `internal/providers/all/all.go`;
- doc pages in `docs/providers/<name>.md` and (optionally)
  `docs/features/<name>.md`;
- index updates in `docs/providers/README.md`, `docs/features/README.md`, and
  `docs/source-map.md`;
- tests that pass without live credentials;
- a CHANGELOG entry under `Unreleased` describing the new provider.

Keep the diff focused. If you find yourself touching `run.go`, `repo.go`,
`coordinator.go`, or `provider_backend.go`, stop and check whether the change is
really provider-specific or whether it should be a shared helper landed in a
separate PR.

## External Process Plugins

External provider plugins are not implemented yet. Do not add a provider that
depends on an undocumented stdio protocol. The intended direction is:

- a built-in Go provider package configures and launches the external process;
- the process speaks JSON over stdio for capabilities, acquire, resolve, list,
  release, touch, run, status, and stop;
- the Go side adapts that to an SSH lease or delegated run backend;
- core commands still own list/status rendering and SSH workflows where the
  provider exposes them.

When that protocol exists, a plugin will look like a normal registered provider
to the rest of Crabbox.

## Related Docs

- [Provider backends](../provider-backends.md): contract reference and review
  checklist.
- [Provider reference](../providers/README.md): one page per built-in backend.
- [Source map](../source-map.md): files behind documented behavior.
- [Architecture](../architecture.md): system overview and lease flow.
- [Coordinator](coordinator.md): brokered lease contract.
