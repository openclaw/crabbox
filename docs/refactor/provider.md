# Provider Backend Refactor

Read when:

- refactoring provider dispatch, direct lifecycle, or delegated-run behavior;
- adding a new provider backend;
- changing provider config, provider flags, coordinator routing, list/status/stop,
  cleanup, or capability validation.

For step-by-step implementation guidance, read
[Provider Backends](../provider-backends.md). This document captures the design
intent behind the provider seam and records what has shipped versus what is still
proposed. The authoring guide is the handrail for new code; this file is the
rationale and the migration ledger.

> Status note: the provider seam described here has landed. The 24 built-in
> provider packages under `internal/providers/<name>` all register through
> `internal/providers/all`, and the core interfaces live in
> `internal/cli/provider_backend.go`. Sections below mark what shipped, what
> shipped differently than first sketched, and what remains a future option.

## Context

Crabbox has two real execution models.

The first is **SSH-lease execution**. A provider hands Crabbox a machine
reachable over SSH; Crabbox owns the workflow: claim, sync, command wrapping,
stdout/stderr streaming, result collection, timing, heartbeat, and release.
Hetzner, AWS, Azure, GCP, static SSH, and several managed-sandbox providers
(Daytona, Namespace, RunPod, and others) fit this shape.

The second is **delegated execution**. The provider owns machine setup or
file/workspace transport, command execution, and output streaming. Crabbox keeps
provider selection, config, local claims/slugs, and timing summaries, but it does
not rsync into these providers. Blacksmith Testbox, Islo, Modal, E2B, and the
other sandbox/proof runners fit this shape.

The original problem was provider-name branching spread through command handlers
and helper paths. Adding `isDaytonaProvider`/`isIsloProvider`-style branches would
have worked short term, but every new provider would touch `run`, `warmup`,
`list`, `status`, `stop`, `cleanup`, config, capability validation, and docs.

The refactor makes providers supply small backends while Crabbox core owns the
workflows.

## Design Principle

Providers do not own commands. Providers configure backends. Core commands own
workflow orchestration.

The command flow looks like this:

```go
backend, err := loadBackend(cfg, runtime)
if err != nil {
	return err
}

switch b := backend.(type) {
case DelegatedRunBackend:
	return b.Run(ctx, runReq)

case SSHLeaseBackend:
	lease, acquired, err := acquireOrResolve(ctx, b, runReq)
	if err != nil {
		return err
	}
	return runOverSSHLease(ctx, b, lease, runReq, acquired)

default:
	return exit(2, "provider=%s does not support run", backend.Spec().Name)
}
```

Provider implementations do not receive the CLI `App`. They receive a narrow
`Runtime` and typed request structs.

## Goals

- Keep every provider working without per-command provider branches.
- Make coordinator (broker) routing a wrapper around SSH-lease backends, not a
  conditional baked into each command.
- Register built-in provider flags before parsing so provider-specific flags do
  not fail before provider selection.
- Keep built-in providers compiled into the Go binary; avoid Go dynamic plugins.
- Leave an external-process plugin protocol as a later extension point.
- Keep provider credentials out of repo config and command arguments.

## Current Implementation State

The provider seam has shipped for the full provider set.

- `warmup`, `run`, `list`, `status`, `stop`, `cleanup`, lease resolution, and
  best-effort touch all load a backend through `loadBackend` instead of branching
  on provider names.
- Built-in providers live under `internal/providers/<name>` and are imported by
  `cmd/crabbox` through the blank-import barrel `internal/providers/all`.
- SSH-lease providers (Hetzner, AWS, Azure, GCP, static SSH, Daytona, and others)
  implement `SSHLeaseBackend`. The coordinator wrapper also implements it.
- Delegated providers (Blacksmith Testbox, Islo, Modal, E2B, and others)
  implement `DelegatedRunBackend` and use the injected `CommandRunner` instead of
  package-level `exec.Command`.
- Command rendering for `list` and `status` is core-owned for both backend kinds.

For the up-to-date provider/capability matrix, run `crabbox providers` or
`crabbox providers --json`; the user-facing inventory lives in
[Providers](../features/providers.md).

## Non-Goals

- No runtime-loaded Go `.so` plugins.
- No provider marketplace in this refactor.
- No generic remote filesystem abstraction.
- No attempt to make a delegated provider look like SSH unless it later ships a
  stable SSH contract.
- No VNC/screenshot/desktop/browser/code-portal/Actions-runner support for a
  provider unless its backend explicitly declares the matching feature.

## Provider And Backend Interfaces

`Provider` is the registration and configuration layer
(`internal/cli/provider_backend.go`):

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

`Backend` is the configured runtime object:

```go
type Backend interface {
	Spec() ProviderSpec
}
```

Two backend shapes carry the core lifecycle.

### SSH-Lease Backend

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

For providers that can hand Crabbox an SSH target. Core owns sync and command
execution after acquisition.

### Delegated-Run Backend

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

For providers that own execution. Core does not call SSH, rsync, or remote
command wrapping for these providers. Delegated providers may stream
stdout/stderr during `Run`, but they should not own normal `list` or `status`
rendering when a normalized view can describe the result. If a provider has a
lossy or native-only status shape, keep that loss inside its backend and return
the closest `StatusView` instead of printing directly from command code.

Delegated providers should also return normalized run outcome metadata in
`RunResult`: `Status` uses `succeeded`, `failed`, `timed-out`, or `canceled`;
`ErrorKind` uses `command-exit`, `timeout`, `canceled`, or `provider-error`.
Call `FinalizeRunResult(result, err)` before returning once the provider knows
the command exit code and provider error. `--timing-json` emits the same
normalized outcome fields, deriving them from `exitCode` when the provider did
not set a more specific status. That keeps future recommendation and proof
surfaces provider-neutral instead of parsing provider-specific stderr.

### Optional Backend Interfaces

Several capabilities are opt-in: a provider implements an extra interface only
when it supports that feature. These have shipped alongside the two core shapes:

```go
type CleanupBackend interface {
	Backend
	Cleanup(ctx context.Context, req CleanupRequest) error
}

type DoctorProvider interface {
	Provider
	ConfigureDoctor(cfg Config, rt Runtime) (DoctorBackend, error)
}

type DoctorBackend interface {
	Backend
	Doctor(ctx context.Context, req DoctorRequest) (DoctorResult, error)
}

// JSONListBackend is a narrow compatibility escape hatch for existing
// script-facing JSON schemas (coordinator pool machines, Blacksmith table rows).
type JSONListBackend interface {
	Backend
	ListJSON(ctx context.Context, req ListRequest) (any, error)
}
```

Provider-native checkpoints/images are also expressed through optional hooks
(`NativeCheckpointProvider`, `NativeCheckpointForkProvider`) rather than core
provider-name checks, and config routing/server-type defaults through
`ProviderRouter` and `ProviderServerTypeProvider`. New providers implement only
the interfaces they need.

## Runtime

Backends receive a narrow runtime instead of `App`:

```go
type Runtime struct {
	Stdout io.Writer
	Stderr io.Writer
	Clock  Clock
	HTTP   *http.Client
	Exec   CommandRunner
}

type CommandRunner interface {
	Run(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error)
}

type LocalCommandRequest struct {
	Name   string
	Args   []string
	Env    []string
	Dir    string
	Stdout io.Writer
	Stderr io.Writer
}

type LocalCommandResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}
```

Provider modules do not reach into command state, global command handlers, or
`App` methods. If they need a helper, it is moved into a small shared package or
passed through a request/runtime field.

This lets tests inject writers, clocks, fake HTTP clients, and fake backends
without constructing a full CLI app. `CommandRunner` is the seam for delegated
CLI providers such as Blacksmith, so tests do not depend on package-level
`exec.Command` hooks. `loadBackend` fills in real defaults (`io.Discard` writers,
`realClock`, an `exec`-backed runner) when fields are left nil.

## Provider Spec

Provider capabilities are declarative and typed, not a growing list of
provider-name checks.

```go
type ProviderSpec struct {
	Name        string
	Family      string
	Kind        ProviderKind
	Targets     []TargetSpec
	Features    FeatureSet
	Coordinator CoordinatorMode
}

type ProviderKind string

const (
	ProviderKindSSHLease     ProviderKind = "ssh-lease"
	ProviderKindDelegatedRun ProviderKind = "delegated-run"
)

type CoordinatorMode string

const (
	CoordinatorNever     CoordinatorMode = "never"
	CoordinatorSupported CoordinatorMode = "supported"
)

type TargetSpec struct {
	OS          string
	WindowsMode string
}
```

`Family` groups related adapters (for example `azure` and the Azure dynamic
sessions adapter share `Family: "azure"`).

The feature set has grown beyond the original sketch as new product surfaces
landed:

```go
const (
	FeatureSSH         Feature = "ssh"
	FeatureCrabboxSync Feature = "crabbox-sync"
	FeatureArchiveSync Feature = "archive-sync"
	FeatureCleanup     Feature = "cleanup"
	FeatureDesktop     Feature = "desktop"
	FeatureBrowser     Feature = "browser"
	FeatureCode        Feature = "code"
	FeatureTailscale   Feature = "tailscale"
	FeatureURLBridge   Feature = "url-bridge"
	FeatureCheckpoint  Feature = "workspace-checkpoint"
	FeatureFork        Feature = "workspace-fork"
	FeatureRestore     Feature = "workspace-restore"
	FeatureSnapshot    Feature = "provider-snapshot"
	FeatureRunProof    Feature = "run-proof"
	FeatureRunSession  Feature = "run-session"
	FeatureRunArtifacts Feature = "run-artifacts"
	FeatureRunDownloads Feature = "run-downloads"
	FeatureModuleRun    Feature = "module-run"
	FeaturePauseResume  Feature = "pause-resume"
	FeatureMCP          Feature = "mcp-attachments"
)
```

Actions-runner hydration is **not** modeled as a provider feature. That workflow
is core-over-SSH after a Linux or Windows lease exists, so `--actions-runner`
validates as "requires `SSHLeaseBackend`, target linux or windows, and not
`local-container`" rather than as a provider capability.

The shipped matrix is authoritative in code (each adapter's `Spec()`); a
representative slice:

```text
provider                kind           coordinator  features
hetzner                 ssh-lease      supported    ssh, crabbox-sync, cleanup, desktop, browser, code, tailscale
aws                     ssh-lease      supported    ssh, crabbox-sync, cleanup, desktop, browser, code
azure                   ssh-lease      supported    ssh, crabbox-sync, cleanup, desktop, browser, code, tailscale
gcp                     ssh-lease      supported    ssh, crabbox-sync, cleanup, tailscale
ssh                     ssh-lease      never        ssh, crabbox-sync, desktop, browser, code
daytona                 ssh-lease      supported    ssh, crabbox-sync, archive-sync
blacksmith-testbox      delegated-run  never        run-proof, run-session
islo                    delegated-run  never        url-bridge
```

Target lists are declarative too: Hetzner is Linux-only; AWS, Azure, GCP, and
static SSH declare Linux plus Windows (normal + wsl2) and macOS; the delegated
sandbox providers are Linux-only.

Capability errors come from `ProviderSpec` plus provider-specific validation, for
example:

```text
provider=daytona managed provisioning supports target=linux only
desktop/VNC is not supported for provider=islo
--actions-runner requires an SSH lease provider
```

## Registry

Built-in providers register at init time:

```go
var providerRegistry = map[string]Provider{}

func RegisterProvider(provider Provider) {
	names := append([]string{provider.Name()}, provider.Aliases()...)
	for _, name := range names {
		key := normalizeProviderName(name)
		if key == "" {
			panic("provider name is empty")
		}
		if providerRegistry[key] != nil {
			panic("provider already registered: " + key)
		}
		providerRegistry[key] = provider
	}
}

func ProviderFor(name string) (Provider, error) {
	provider := providerRegistry[normalizeProviderName(name)]
	if provider == nil {
		return nil, exit(2, "unknown provider %q", name)
	}
	return provider, nil
}
```

Each provider package registers itself in its `init`, and
`internal/providers/all` blank-imports every package so a single import of `all`
wires the whole registry. Canonical names and compatibility aliases come from
`Name()`/`Aliases()`, for example:

```text
ssh                 # aliases: static, static-ssh
blacksmith-testbox  # alias: blacksmith
gcp                 # aliases: google, google-cloud
local-container     # aliases: docker, container, local-docker
```

Docs should use canonical names.

## On-Disk Layout

One folder per provider for registration, provider-specific flags, the provider
spec, and backend configuration:

```text
internal/providers/all                 # blank-imports every built-in provider
internal/providers/hetzner             # Hetzner provider + SSH lease backend
internal/providers/aws                 # AWS provider + SSH lease backend
internal/providers/azure               # Azure provider + SSH lease backend
internal/providers/gcp                 # GCP provider + SSH lease backend
internal/providers/ssh                 # static SSH provider + backend
internal/providers/blacksmith          # delegated Blacksmith backend
internal/providers/daytona             # Daytona SSH lease + delegated run backend
internal/providers/islo                # Islo delegated backend
internal/providers/shared              # shared direct SSH backend helpers
...                                     # one folder per remaining adapter
internal/cli/provider_backend.go       # core interfaces, registry, requests
internal/cli/provider_coordinator.go   # brokered coordinator lease backend
```

The exported contract between provider folders and the CLI stays deliberately
small: `Provider`, `ProviderSpec`, the request/result/view types, `Runtime`, and
narrow helpers for claims, labels, sync preflight, SSH-key storage, and timing
output. Command orchestration stays in `internal/cli`; provider lifecycle and
client code lives in the provider folder. Daytona, for example, keeps its
generated API client and toolbox upload entirely inside
`internal/providers/daytona`.

## Flag Parsing

Go's `flag` package rejects unknown flags during parse, so provider-specific
flags must be registered before `flag.Parse`, even though the selected provider
is only known after config and flags are merged.

The first-pass strategy for built-in providers:

1. register common command flags;
2. iterate over all registered built-in providers and call `RegisterFlags`;
3. parse once;
4. load config;
5. apply common flags;
6. select `ProviderFor(cfg.Provider)`;
7. apply only the selected provider's parsed flag values;
8. configure the backend.

Flags for non-selected providers are parsed but ignored. Provider `ApplyFlags`
must mutate config only for flags actually present in argv (using `flagWasSet` or
equivalent): the values returned by `RegisterFlags` exist so the parser and help
text know the flag shape, and they must not overwrite repo config just because
every built-in provider flag was registered up front.

A two-pass parser is reserved for a future where external-process providers
define flags dynamically: pass one would parse only safe global selectors such as
`--provider` and `--config`, load provider metadata, register provider flags, and
pass two would parse the original args.

Provider flags follow a `--<provider>-<field>` convention, for example
`--daytona-snapshot`, `--blacksmith-workflow`, `--islo-image`. Providers should
not expose flags that cannot work; Daytona, for instance, does not expose
CPU/memory/disk overrides while the integration is snapshot-only and Daytona
rejects resource fields on snapshots.

## Backend Loading

All commands use the same loading shape (`loadBackend` in
`internal/cli/provider_backend.go`):

```go
func loadBackend(cfg Config, rt Runtime) (Backend, error) {
	// rt defaults (writers, clock, exec runner) filled in here when nil.
	provider, err := ProviderFor(cfg.Provider)
	if err != nil {
		return nil, err
	}
	cfg.Provider = provider.Name()
	backend, err := provider.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	if ssh, ok := backend.(SSHLeaseBackend); ok && shouldUseCoordinator(cfg, provider.Spec()) {
		coord, _, err := newCoordinatorClient(cfg)
		if err != nil {
			return nil, err
		}
		return &coordinatorLeaseBackend{spec: provider.Spec(), cfg: cfg, direct: ssh, coord: coord, rt: rt}, nil
	}
	return backend, nil
}
```

`Configure` builds direct provider clients and validates provider auth early.
Provider flag registration and `ApplyFlags` happen earlier, during config
assembly; `loadBackend` never sees `flag.FlagSet` or raw argv. Examples of what
`Configure` validates: Hetzner reads `HCLOUD_TOKEN` / `HETZNER_TOKEN`; AWS loads
SDK config; Daytona reads `DAYTONA_API_KEY` or `DAYTONA_JWT_TOKEN`; Islo
validates its API key before SDK use; Blacksmith verifies enough local config to
build CLI args.

## Coordinator Wrapper

Coordinator (broker) routing is a wrapper around `SSHLeaseBackend`, not a special
provider path inside every command.

```go
func shouldUseCoordinator(cfg Config, spec ProviderSpec) bool {
	return spec.Coordinator == CoordinatorSupported && strings.TrimSpace(cfg.Coordinator) != ""
}
```

The wrapper (`coordinatorLeaseBackend`, `internal/cli/provider_coordinator.go`)
implements `SSHLeaseBackend`:

- `Acquire` calls the coordinator lease API and maps the coordinator lease to a
  `LeaseTarget`;
- `Resolve` calls coordinator get/slug lookup;
- `ReleaseLease` calls coordinator release;
- `Touch` calls heartbeat or idle-update paths;
- `List` calls coordinator pool/admin routes.

In brokered mode the wrapper owns key creation, coordinator lease creation, lease
lookup, heartbeat, run-recorder attachment, and lease release. It must not fall
through to the direct provider's acquire/resolve/touch/release/list/cleanup once
the coordinator is selected; the wrapped direct backend exists only to carry the
provider spec and the non-brokered implementation. Brokered list/pool still
enforces the existing admin-token requirement; the wrapper must not silently
downgrade brokered pool/list to a direct provider listing.

Only `hetzner`, `aws`, `azure`, `daytona`, and `gcp` declare `Coordinator: supported`; every
other adapter is `Coordinator: never` and always runs direct-from-CLI. Even the
five brokerable providers run direct unless a broker URL is configured
(`CRABBOX_COORDINATOR` / `config set-broker`). Adding broker support to another
provider means changing its spec and implementing Worker-side support.

## Request And Result Types

`Provider.Configure` is the only place that receives the full `Config`. Provider
modules decode their typed config, create clients, and store them on the backend.
Requests then carry command intent, repo state, and options — not `App` and not
`Config`.

```go
type LeaseOptions struct {
	TargetOS      string
	WindowsMode   string
	Class         string
	Pond          string
	ServerType    string
	IdleTimeout   time.Duration
	TTL           time.Duration
	Desktop       bool
	DesktopEnv    string
	Browser       bool
	Code          bool
	Tailscale     TailscaleConfig
	WorkRoot      string
	SSHUser       string
	SSHPort       string
	SSHKey        string
	Sync          SyncConfig
	Results       ResultsConfig
	EnvAllow      []string
	ActionsRunner bool
}

type AcquireRequest struct {
	Repo          Repo
	Options       LeaseOptions
	Keep          bool
	Reclaim       bool
	RequestedSlug string
}

type ResolveRequest struct {
	Repo        Repo
	Options     LeaseOptions
	ID          string
	Reclaim     bool
	ReleaseOnly bool
}

type ReleaseLeaseRequest struct {
	Lease LeaseTarget
	Force bool
}

type TouchRequest struct {
	Lease       LeaseTarget
	State       string
	IdleTimeout time.Duration
}
```

`RunRequest`, `WarmupRequest`, `StatusRequest`, `StopRequest`, `ListRequest`, and
`RunResult` follow the same pattern (see `provider_backend.go` for the current
fields). Core command code converts CLI/config state into `LeaseOptions` once
(`leaseOptionsFromConfig`); backends do not re-read global command state or decode
raw provider config maps after `Configure`.

`LeaseOptions` is intentionally broad for the migration. Provisioning backends
usually care only about the provisioning subset, while the shared SSH workflow
consumes sync, result, and environment options. Splitting this into
`ProvisionOptions` and `RunOptions` remains a possible cleanup.

`LeaseView` and `StatusView` are command-facing view models. Rendering is
core-owned for both backend kinds: `ListRequest` and `StatusRequest` carry no
JSON/human format flags, because backends return normalized views and core
renders them. `JSONListBackend` is the narrow escape hatch for existing
script-facing JSON schemas; new providers should not need it.

Delegated providers reject irrelevant sync options through a shared helper so the
error wording stays consistent:

```go
func rejectDelegatedSyncOptions(provider string, req RunRequest) error {
	if req.SyncOnly {
		return exit(2, "provider=%s does not sync local files; --sync-only is not supported", provider)
	}
	if req.ChecksumSync {
		return exit(2, "provider=%s does not sync local files; --checksum is not supported", provider)
	}
	if req.ForceSyncLarge {
		return exit(2, "provider=%s does not sync local files; --force-sync-large is not supported", provider)
	}
	return nil
}
```

## Shared SSH Workflow

`runCommand` does not carry provider lifecycle details; it calls one shared SSH
workflow:

```go
func runOverSSHLease(
	ctx context.Context,
	backend SSHLeaseBackend,
	lease LeaseTarget,
	req RunRequest,
	acquired bool,
	rt Runtime,
) error
```

This workflow owns:

- local claim/reclaim checks;
- coordinator recorder attachment when the backend is coordinator-wrapped;
- heartbeat/touch lifecycle through `backend.Touch`;
- Actions-hydration marker detection;
- sync manifest creation, preflight, git seed, rsync/archive transfer, remote
  prune, and sync finalize;
- POSIX / native-Windows / WSL2 command wrapping;
- stdout/stderr streaming and run-log buffering;
- JUnit result collection;
- timing summary and timing JSON;
- release through `backend.ReleaseLease` when `acquired && !req.Keep`.

Providers must not copy this workflow. Daytona, Hetzner, AWS, Azure, GCP, and
static SSH all reuse it.

`ReleaseLease` means "tear down the lease/resource for this specific command or
explicit stop." Background TTL/orphan cleanup is separate and belongs to
`CleanupBackend`. Static SSH implements `ReleaseLease` as a no-op and does not opt
into cleanup.

## Lease Target And SSH Target

Lease backends return:

```go
type LeaseTarget struct {
	Server  Server
	Target  SSHTarget
	LeaseID string
	Options LeaseOptions
}
```

`Server` stays the neutral provider resource. `SSHTarget` carries explicit
metadata for secret-bearing auth:

```go
type SSHTarget struct {
	User          string
	Host          string
	Key           string
	Port          string
	FallbackPorts []string
	TargetOS      string
	WindowsMode   string
	ReadyCheck    string
	AuthSecret    bool
	NetworkKind   NetworkMode
}
```

SSH rendering omits `-i` when `Key == ""`. Human-readable status, list, timing,
and normal JSON output redact `User` when `AuthSecret` is true. The only intended
token-revealing surface is an explicit connect action such as
`crabbox ssh --provider daytona --id <lease>`.

Daytona is the canonical example: its SSH target parses the API-returned SSH
command, uses an empty key with a secret user over a public relay, and supplies a
ready check. Normal output then reads:

```text
ready ssh=<redacted>@<daytona-ssh-host>:<daytona-ssh-port> network=public workroot=/home/daytona/crabbox
```

## Provider State Contract

Direct SSH-lease providers map provider resources into `Server` and use Crabbox
labels/tags when the provider supports metadata. Required labels:

```text
crabbox=true
provider=<provider>
lease=<lease-id>
slug=<friendly-slug>
state=provisioning|leased|ready|running|released|failed
keep=true|false
target=linux|windows|macos
windows_mode=normal|wsl2
server_type=<provider-class-or-instance-type>
created_at=<unix-seconds>
last_touched_at=<unix-seconds>
idle_timeout_secs=<seconds>
ttl_secs=<seconds>
expires_at=<unix-seconds>
```

Direct providers write Unix seconds. The parser also accepts RFC3339 and
RFC3339Nano for compatibility with old or external records; moving labels to
RFC3339 would be a behavior change requiring matching test and doc updates.

Provider-specific labels are documented per provider, for example:

```text
provider_key=<ssh-key-name>   # Hetzner/AWS direct key cleanup
market=spot|on-demand         # AWS
work_root=<remote-work-root>  # Daytona restore/reuse path
```

A provider without labels/tags must implement equivalent lookup and cleanup
semantics before declaring `FeatureCleanup`.

## Config Model

> Shipped differently than first proposed. The original sketch favored a generic
> `Providers map[string]ProviderConfig` bag. In practice each provider ships a
> **typed config section** on the central `Config` (for example `Daytona`,
> `Islo`, `AWS`, `Azure`, `Proxmox`, `Parallels`), populated from a matching YAML
> block and from flags. There is no generic `providers:` map. This keeps config
> decoding type-safe and discoverable at the cost of one field per provider.

Each provider's config lives under its own YAML key:

```yaml
provider: daytona

daytona:
  snapshot: crabbox-ready
  user: daytona
  workRoot: /home/daytona/crabbox

islo:
  image: docker.io/library/ubuntu:24.04
  workdir: /workspace/crabbox
```

Provider credentials stay in environment or native provider auth stores, not repo
YAML. Confirmed env credentials include:

```text
HCLOUD_TOKEN
HETZNER_TOKEN
DAYTONA_API_KEY
DAYTONA_JWT_TOKEN
DAYTONA_ORGANIZATION_ID
ISLO_API_KEY
```

`ISLO_BASE_URL` is also read from the environment, but it is a base-URL override
rather than a credential.

AWS, Azure, and GCP rely on their native SDK credential chains (profile,
`az`/gcloud, ADC) rather than Crabbox-specific env vars.

## Built-In vs External Plugins

The refactor keeps providers compiled into the Go binary.

Do not use Go `plugin.Open`: Go plugins require matching Go versions, module
versions, architecture, and build flags; they cannot be unloaded, run init on
load, and have poor cross-platform support.

If runtime extension is needed later, the chosen direction is an external-process
protocol — a child process speaking JSON over stdio for `spec`/`warmup`/`run`/
`status`/`stop`:

```yaml
provider: my-runner
providers:
  my-runner:
    kind: command
    command: crabbox-provider-my-runner
```

This would let TypeScript or Python SDK adapters exist without making the core
binary load native plugins. It is not implemented today.

## Provider Mapping

The mappings below describe ownership boundaries for the most architecturally
distinct providers. Specs are authoritative in each adapter's `Spec()`.

### Hetzner — `SSHLeaseBackend`

Coordinator supported, Linux only, features ssh/crabbox-sync/cleanup/desktop/
browser/code/tailscale. Owns `HCLOUD_TOKEN`/`HETZNER_TOKEN` auth, SSH key
import/delete, server lifecycle, labels, class fallback, and direct cleanup.
Reuses the coordinator wrapper, the shared SSH sync/run workflow, claims, status
rendering, and cleanup policy.

### AWS — `SSHLeaseBackend`

Coordinator supported, targets Linux + Windows (normal/wsl2) + macOS, features
ssh/crabbox-sync/cleanup/desktop/browser/code. Owns SDK config and region
selection, key-pair import/delete, AMI resolution, security-group setup, EC2
lifecycle, spot/on-demand fallback, Windows/macOS launch options, tags, and
direct cleanup. Also implements native checkpoint hooks (AMI / EBS snapshots).
Reuses the coordinator wrapper, shared SSH workflow (including native-Windows
archive sync and command wrapping), claims, status rendering, and cleanup policy.

### Azure / GCP — `SSHLeaseBackend`

Coordinator supported, multi-target, with native checkpoint/image hooks. Azure
adds tailscale; both reuse the coordinator wrapper and shared workflow. The Azure
dynamic-sessions adapter shares `Family: "azure"` but is a delegated-run sandbox.

### Static SSH — `SSHLeaseBackend`

Coordinator never, multi-target, features ssh/crabbox-sync/desktop/browser/code.
Owns static-config-to-`LeaseTarget` mapping, static claim behavior, and a no-op
release. No provider cleanup, no coordinator.

### Daytona — `SSHLeaseBackend` with delegated `Run`

> Shipped (`internal/providers/daytona`). Daytona registered as a single
> SSH-lease backend whose `Run` uses Daytona's toolbox upload + command execution
> rather than the generic rsync path, while `ssh` access uses the parsed SSH
> command. This is functionally the "hybrid" model from the original plan,
> expressed as one backend implementing both shapes' relevant methods.

Coordinator supported, Linux only, features ssh/crabbox-sync/archive-sync. Owns
the Daytona generated Go API client (auth + organization header), the SDK/toolbox,
sandbox lifecycle, labels and last-activity touch, SSH-access token minting,
toolbox archive upload and command execution for `run`, sandbox-to-`Server`
mapping, and secret SSH-user metadata. Reuses sync guardrails, claims, status
rendering, and explicit release/stop. Constraints: Linux only, no Tailscale, no
desktop/VNC/browser/code portal, no Actions-runner hydration, snapshot mode only;
brokered workspaces and ready pools remain disabled because SSH credentials rotate.

### Blacksmith Testbox — `DelegatedRunBackend`

Coordinator never, Linux only, features run-proof/run-session. Owns Blacksmith
CLI command construction, warmup/run/list/status/stop, Testbox SSH-key storage
through injected filesystem/runtime helpers, provider-specific claim-ID
resolution, and delegated timing summaries. No Crabbox rsync, `--sync-only`, VNC/
screenshot/desktop, or coordinator.

### Islo — `DelegatedRunBackend`

> Shipped (`internal/providers/islo`). Despite the product-fit concerns recorded
> for the earlier PRs, Islo landed as a delegated-run backend.

Coordinator never, Linux only, features url-bridge. Owns SDK auth and token
refresh, sandbox lifecycle, command execution through the provider API, SSE
parsing for live stdout/stderr, and Islo lease-ID/sandbox-name mapping. Keeps the
custom SSE consumer (the SDK stream method has no clean streaming API), validates
the API key before SDK calls, and accepts a base-URL override (`--islo-base-url`,
`CRABBOX_ISLO_BASE_URL`/`ISLO_BASE_URL`, or the `islo.baseUrl` config key;
defaults to `https://api.islo.dev`). No Crabbox rsync, `--sync-only`, `--checksum`, `--force-sync-large`,
desktop/browser, Actions runner, or coordinator.

The remaining sandbox/proof adapters (Modal, E2B, Tensorlake, Upstash, Cloudflare,
Cloudflare Sandbox, Azure dynamic sessions, W&B) follow the Islo pattern:
delegated-run, Linux-only, with a feature subset of archive-sync/url-bridge as
appropriate.

## Migration Ledger

The seam landed in phases. All phases below have shipped:

1. **Registry and specs** — provider registry, `Provider`/`Backend`/
   `ProviderSpec`, feature/target types, built-in registration, flags registered
   before `flag.Parse`. No behavior change.
2. **Backend loading** — `Runtime`, `Provider.Configure`, `loadBackend`
   (intentionally not accepting `flag.FlagSet` or raw args), fake-backend tests.
3. **Coordinator wrapper** — `coordinatorLeaseBackend` wraps brokerable SSH
   backends when a coordinator is configured; direct mode otherwise.
4. **Shared SSH workflow** — `runOverSSHLease` extracted; Hetzner, AWS, Azure,
   GCP, and static SSH route through `SSHLeaseBackend`; fake SSH-backend tests
   cover acquire/resolve/claim/reclaim/sync-only/touch/timing/release/recorder.
5. **Delegated providers** — Blacksmith and the sandbox runners moved to
   `DelegatedRunBackend`, with centralized sync-option rejection.
6. **Provider config** — typed per-provider config sections (not the generic bag
   originally sketched), with `blacksmith:`/`static:` compatibility preserved.
7. **Daytona** — landed as an SSH-lease backend with delegated `run`.
8. **Islo** — landed as a `DelegatedRunBackend`.
9. **Compatibility cleanup** — provider-name branches in commands replaced by
   spec/feature checks; docs and the source map updated.

New provider PRs should add fake-backend tests before any live-only coverage.

## Tests

Representative coverage that any provider change should preserve:

- **Registry/flags** — canonical and alias lookup; duplicate-registration panic;
  unknown-provider error; help string includes built-ins; provider flags accepted
  before selection; non-selected provider flags parse but are ignored.
- **Spec/capability** — target-OS and Windows-mode validation per provider;
  unsupported desktop/browser/code and Actions-runner errors; coordinator wrapper
  selected for brokerable providers only when configured; direct backend selected
  otherwise.
- **SSH workflow** — fake SSH backend enters the shared sync/run path on both
  acquire and resolve; touch transitions go through the backend; release on
  acquired non-keep lease; no provider-specific command branch needed.
- **Delegated** — fake delegated backend receives warmup/run/list/status/stop;
  sync flags rejected; nonzero exit propagates; Blacksmith executes through the
  injected `CommandRunner`, not package-level `exec.Command`.
- **Daytona** — auth/org-header behavior; create/labels body shape; snapshot mode
  omits unusable resource overrides; stopped sandbox starts before SSH-target
  creation; SSH target parses the API `sshCommand` with empty key/secret user/
  public network/ready check; list/status/timing output (including JSON) redacts
  the token-bearing user; release removes the local claim.
- **Islo** — SDK factory rejects a missing API key; create/get/list/delete
  mapping; SSE parser handles stdout/stderr/exit events; run streams output and
  propagates exit code; status wait polls and times out; stop removes the local
  claim.
- **Docs** — provider docs link from [Providers](../features/providers.md);
  [`docs/source-map.md`](../source-map.md) lists provider implementation files.

## Acceptance Criteria

- `go test ./...` passes.
- Existing providers keep working, e.g. `crabbox warmup --provider hetzner`,
  `crabbox run --provider aws`, `crabbox run --provider ssh`,
  `crabbox run --provider blacksmith-testbox`.
- A fake SSH-lease backend and a fake delegated backend can each be tested
  without editing command handlers.
- Brokerable providers still use the coordinator when one is configured.
- No new provider requires touching the main command flow unless it adds a new
  top-level Crabbox feature.
- Normal list/status/timing output, including JSON, never prints secret SSH users
  or provider API credentials.

## Open Questions

- Should `Server` become `Machine` now that not every provider creates a server?
- Should the typed per-provider config sections eventually gain a generic
  `providers.<name>` namespace, or stay one typed field each?
- Should an external command-provider protocol be a small Crabbox JSON protocol
  or MCP? The smaller JSON protocol is preferred for now.
- Should Daytona support image mode and resource overrides, or stay snapshot only?
