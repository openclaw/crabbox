package cli

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"time"
)

type Provider interface {
	Name() string
	Aliases() []string
	Spec() ProviderSpec
	RegisterFlags(fs *flag.FlagSet, defaults Config) any
	ApplyFlags(cfg *Config, fs *flag.FlagSet, values any) error
	Configure(cfg Config, rt Runtime) (Backend, error)
}

type Backend interface {
	Spec() ProviderSpec
}

type DoctorProvider interface {
	Provider
	ConfigureDoctor(cfg Config, rt Runtime) (DoctorBackend, error)
}

type DoctorBackend interface {
	Backend
	Doctor(ctx context.Context, req DoctorRequest) (DoctorResult, error)
}

type SSHLeaseBackend interface {
	Backend
	Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error)
	Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error)
	List(ctx context.Context, req ListRequest) ([]LeaseView, error)
	ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error
	Touch(ctx context.Context, req TouchRequest) (Server, error)
}

type DelegatedRunBackend interface {
	Backend
	Warmup(ctx context.Context, req WarmupRequest) error
	Run(ctx context.Context, req RunRequest) (RunResult, error)
	List(ctx context.Context, req ListRequest) ([]LeaseView, error)
	Status(ctx context.Context, req StatusRequest) (StatusView, error)
	Stop(ctx context.Context, req StopRequest) error
}

type CleanupBackend interface {
	Backend
	Cleanup(ctx context.Context, req CleanupRequest) error
}

type ReleaseLeaseReporter interface {
	ReleaseLeaseMessage(lease LeaseTarget) string
}

type NativeCheckpointCapability struct {
	Kind              string
	Direct            bool
	CreateUnsupported string
}

type NativeCheckpointRequest struct {
	Config   Config
	Server   Server
	Target   SSHTarget
	Strategy string
}

type NativeCheckpointProvider interface {
	NativeCheckpointCapability(req NativeCheckpointRequest) (NativeCheckpointCapability, bool)
}

type NativeCheckpointForkRecord struct {
	Kind        string
	ImageID     string
	Resource    string
	Region      string
	Project     string
	Direct      bool
	HostID      string
	TargetOS    string
	WindowsMode string
	ServerType  string
}

type NativeCheckpointForkRequest struct {
	Config              *Config
	Record              NativeCheckpointForkRecord
	MarketExplicit      bool
	AzureOSDisk         string
	AzureOSDiskExplicit bool
}

type NativeCheckpointForkProvider interface {
	ApplyNativeCheckpointForkConfig(req NativeCheckpointForkRequest) error
}

type JSONListBackend interface {
	Backend
	ListJSON(ctx context.Context, req ListRequest) (any, error)
}

type ProviderSpec struct {
	Name        string
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

type Feature string

const (
	FeatureSSH         Feature = "ssh"
	FeatureCrabboxSync Feature = "crabbox-sync"
	FeatureArchiveSync Feature = "archive-sync"
	FeatureCleanup     Feature = "cleanup"
	FeatureDesktop     Feature = "desktop"
	FeatureBrowser     Feature = "browser"
	FeatureCode        Feature = "code"
	FeatureTailscale   Feature = "tailscale"
	FeatureCheckpoint  Feature = "workspace-checkpoint"
	FeatureFork        Feature = "workspace-fork"
	FeatureRestore     Feature = "workspace-restore"
	FeatureSnapshot    Feature = "provider-snapshot"
	FeatureRunProof    Feature = "run-proof"
)

type FeatureSet []Feature

func (s FeatureSet) Has(feature Feature) bool {
	for _, item := range s {
		if item == feature {
			return true
		}
	}
	return false
}

type Runtime struct {
	Stdout io.Writer
	Stderr io.Writer
	Clock  Clock
	HTTP   *http.Client
	Exec   CommandRunner
}

type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

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

type DoctorRequest struct {
	ProbeSSH bool
}

type DoctorResult struct {
	Provider string
	Message  string
	Status   string
	Checks   []DoctorCheck
}

type DoctorCheck struct {
	Status  string            `json:"status"`
	Check   string            `json:"check"`
	Message string            `json:"message,omitempty"`
	Details map[string]string `json:"details,omitempty"`
}

func InventoryDoctorResult(provider string, leases int) DoctorResult {
	return DoctorResult{
		Provider: provider,
		Message:  fmt.Sprintf("auth=ready control_plane=ready inventory=ready api=list mutation=false leases=%d runtime=unchecked", leases),
	}
}

func CLIDoctorResult(provider string, leases int, runtime string) DoctorResult {
	return DoctorResult{
		Provider: provider,
		Message:  fmt.Sprintf("cli=ready control_plane=ready inventory=ready api=list mutation=false leases=%d runtime=%s", leases, runtime),
	}
}

type execCommandRunner struct{}

func (execCommandRunner) Run(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	cmd := exec.CommandContext(ctx, req.Name, req.Args...)
	cmd.Env = req.Env
	cmd.Dir = req.Dir
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if req.Stdout != nil {
		cmd.Stdout = io.MultiWriter(req.Stdout, &stdout)
	} else {
		cmd.Stdout = &stdout
	}
	if req.Stderr != nil {
		cmd.Stderr = io.MultiWriter(req.Stderr, &stderr)
	} else {
		cmd.Stderr = &stderr
	}
	err := cmd.Run()
	result := LocalCommandResult{ExitCode: exitCode(err), Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		result.ExitCode = 0
	}
	return result, err
}

type LeaseOptions struct {
	TargetOS      string
	WindowsMode   string
	Class         string
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

type ListRequest struct {
	Options LeaseOptions
	All     bool
	Refresh bool
}

type RunRequest struct {
	Repo             Repo
	ID               string
	Options          LeaseOptions
	Keep             bool
	Reclaim          bool
	NoSync           bool
	SyncOnly         bool
	DebugSync        bool
	ShellMode        bool
	ChecksumSync     bool
	ForceSyncLarge   bool
	FullResync       bool
	EnvHelper        string
	CaptureStdout    string
	CaptureStderr    string
	CaptureOnFail    bool
	KeepOnFailure    bool
	Preflight        bool
	Downloads        []string
	Env              map[string]string
	EnvSummary       bool
	ScriptRequested  bool
	Script           *RunScriptSpec
	FreshPR          FreshPRSpec
	ApplyLocalPatch  bool
	Command          []string
	Label            string
	RequestedSlug    string
	TimingJSON       bool
	ArtifactGlobs    []string
	EmitProof        string
	ProofTemplate    string
	ProfileVariables map[string]string
	StopAfter        string
}

type WarmupRequest struct {
	Repo          Repo
	Options       LeaseOptions
	Keep          bool
	Reclaim       bool
	ActionsRunner bool
	RequestedSlug string
	TimingJSON    bool
}

type StatusRequest struct {
	Options     LeaseOptions
	ID          string
	Wait        bool
	WaitTimeout time.Duration
}

type StopRequest struct {
	Options LeaseOptions
	ID      string
}

type CleanupRequest struct {
	Options LeaseOptions
	DryRun  bool
}

type RunResult struct {
	ExitCode      int
	Command       time.Duration
	Total         time.Duration
	SyncDelegated bool
	Provider      string
	LeaseID       string
	Slug          string
	CommandText   string
	LogExcerpt    string
	ActionsURL    string
	Artifacts     []RunArtifact
}

type LeaseTarget struct {
	Server      Server
	SSH         SSHTarget
	LeaseID     string
	Coordinator *CoordinatorClient
}

type LeaseView = Server

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

func registeredProviders() []Provider {
	seen := map[string]struct{}{}
	providers := make([]Provider, 0, len(providerRegistry))
	for _, provider := range providerRegistry {
		name := normalizeProviderName(provider.Name())
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		providers = append(providers, provider)
	}
	sort.Slice(providers, func(i, j int) bool {
		return providers[i].Name() < providers[j].Name()
	})
	return providers
}

func normalizeProviderName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func providerHelpAll() string {
	return "provider: hetzner, aws, azure, gcp, proxmox, parallels, local-container, ssh, exe-dev, blacksmith-testbox, namespace-devbox, semaphore, daytona, islo, e2b, modal, sprites, railway, runpod, cloudflare, or wandb"
}

func providerHelpSSH() string {
	return "provider: hetzner, aws, azure, gcp, proxmox, parallels, local-container, ssh, exe-dev, namespace-devbox, semaphore, daytona, runpod, or sprites"
}

func isBlacksmithProvider(provider string) bool {
	return provider == "blacksmith-testbox" || provider == "blacksmith"
}

type providerFlagValues map[string]any

func registerProviderFlags(fs *flag.FlagSet, defaults Config) providerFlagValues {
	values := providerFlagValues{}
	for _, provider := range registeredProviders() {
		values[provider.Name()] = provider.RegisterFlags(fs, defaults)
	}
	return values
}

func applyProviderFlags(cfg *Config, fs *flag.FlagSet, values providerFlagValues) error {
	provider, err := ProviderFor(cfg.Provider)
	if err != nil {
		return err
	}
	return provider.ApplyFlags(cfg, fs, values[provider.Name()])
}

func runtimeForApp(a App) Runtime {
	return Runtime{Stdout: a.Stdout, Stderr: a.Stderr, Clock: realClock{}, Exec: execCommandRunner{}}
}

func loadBackend(cfg Config, rt Runtime) (Backend, error) {
	if rt.Stdout == nil {
		rt.Stdout = io.Discard
	}
	if rt.Stderr == nil {
		rt.Stderr = io.Discard
	}
	if rt.Clock == nil {
		rt.Clock = realClock{}
	}
	if rt.Exec == nil {
		rt.Exec = execCommandRunner{}
	}
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

func shouldUseCoordinator(cfg Config, spec ProviderSpec) bool {
	return spec.Coordinator == CoordinatorSupported && strings.TrimSpace(cfg.Coordinator) != ""
}

func backendCoordinator(backend Backend) *CoordinatorClient {
	if b, ok := backend.(*coordinatorLeaseBackend); ok {
		return b.coord
	}
	return nil
}

func leaseOptionsFromConfig(cfg Config) LeaseOptions {
	return LeaseOptions{
		TargetOS:      cfg.TargetOS,
		WindowsMode:   cfg.WindowsMode,
		Class:         cfg.Class,
		ServerType:    cfg.ServerType,
		IdleTimeout:   cfg.IdleTimeout,
		TTL:           cfg.TTL,
		Desktop:       cfg.Desktop,
		DesktopEnv:    normalizedDesktopEnv(cfg.DesktopEnv),
		Browser:       cfg.Browser,
		Code:          cfg.Code,
		Tailscale:     cfg.Tailscale,
		WorkRoot:      cfg.WorkRoot,
		SSHUser:       cfg.SSHUser,
		SSHPort:       cfg.SSHPort,
		SSHKey:        cfg.SSHKey,
		Sync:          cfg.Sync,
		Results:       cfg.Results,
		EnvAllow:      cfg.EnvAllow,
		ActionsRunner: cfg.Actions.Workflow != "" || len(cfg.Actions.RunnerLabels) > 0,
	}
}

func validateActionsRunnerCapability(backend Backend, cfg Config) error {
	if _, ok := backend.(SSHLeaseBackend); !ok {
		return exit(2, "--actions-runner requires an SSH lease provider")
	}
	if backend.Spec().Name == "local-container" {
		return exit(2, "--actions-runner is not supported for provider=local-container; use normal crabbox run or a remote SSH provider")
	}
	if !supportsGitHubActionsRunnerTarget(SSHTarget{TargetOS: cfg.TargetOS, WindowsMode: cfg.WindowsMode}) {
		return exit(2, "--actions-runner requires target=linux or target=windows")
	}
	return nil
}

func featureSetHas(features FeatureSet, feature Feature) bool {
	for _, candidate := range features {
		if candidate == feature {
			return true
		}
	}
	return false
}

func rejectDelegatedSyncOptionsForSpec(spec ProviderSpec, req RunRequest) error {
	provider := spec.Name
	archiveSync := featureSetHas(spec.Features, FeatureArchiveSync)
	if req.SyncOnly && !archiveSync {
		return exit(2, "%s delegates sync; --sync-only is not supported", provider)
	}
	if req.ChecksumSync {
		return exit(2, "%s delegates sync; --checksum is not supported", provider)
	}
	if req.ForceSyncLarge && !archiveSync {
		return exit(2, "%s delegates sync; --force-sync-large is not supported", provider)
	}
	if req.FullResync {
		return exit(2, "%s delegates sync; --full-resync is not supported", provider)
	}
	if req.EnvHelper != "" {
		return exit(2, "%s delegates run execution; --env-helper is not supported", provider)
	}
	if req.CaptureStdout != "" {
		return exit(2, "%s delegates run execution; --capture-stdout is not supported", provider)
	}
	if req.CaptureStderr != "" {
		return exit(2, "%s delegates run execution; --capture-stderr is not supported", provider)
	}
	if req.CaptureOnFail {
		return exit(2, "%s delegates run execution; --capture-on-fail is not supported", provider)
	}
	if len(req.Downloads) > 0 {
		return exit(2, "%s delegates run execution; --download is not supported", provider)
	}
	if len(req.ArtifactGlobs) > 0 {
		return exit(2, "%s delegates run execution; --artifact-glob is not supported", provider)
	}
	if req.EmitProof != "" && !featureSetHas(spec.Features, FeatureRunProof) {
		return exit(2, "%s delegates run execution; --emit-proof is not supported", provider)
	}
	if req.StopAfter != "" {
		return exit(2, "%s delegates run execution; --stop-after is not supported", provider)
	}
	if req.Script != nil || req.ScriptRequested {
		return exit(2, "%s delegates run execution; --script is not supported", provider)
	}
	if !req.FreshPR.Empty() {
		return exit(2, "%s delegates sync; --fresh-pr is not supported", provider)
	}
	return nil
}

func rejectDelegatedSyncOptions(provider string, req RunRequest) error {
	return rejectDelegatedSyncOptionsForSpec(ProviderSpec{Name: provider}, req)
}

func RejectDelegatedSyncOptions(provider string, req RunRequest) error {
	return rejectDelegatedSyncOptions(provider, req)
}

func RejectDelegatedSyncOptionsForSpec(spec ProviderSpec, req RunRequest) error {
	return rejectDelegatedSyncOptionsForSpec(spec, req)
}

func renderServerList(stdout io.Writer, servers []Server) {
	for _, s := range servers {
		extra := ""
		if orphan := strings.TrimSpace(s.Labels["orphan"]); orphan != "" {
			extra = " " + orphan
		}
		fmt.Fprintf(stdout, "%-20s %-28s %-12s %-14s %-15s lease=%s slug=%s keep=%s target=%s%s\n",
			s.DisplayID(), s.Name, s.Status, s.ServerType.Name, s.PublicNet.IPv4.IP, s.Labels["lease"], blank(serverSlug(s), "-"), s.Labels["keep"], s.Labels["target"], extra)
	}
}

func (a App) touchLeaseTargetBestEffort(ctx context.Context, cfg Config, lease LeaseTarget, state string) Server {
	backend, err := loadBackend(cfg, runtimeForApp(a))
	if err != nil {
		fmt.Fprintf(a.Stderr, "warning: touch failed for %s: %v\n", lease.LeaseID, err)
		return lease.Server
	}
	sshBackend, ok := backend.(SSHLeaseBackend)
	if !ok {
		fmt.Fprintf(a.Stderr, "warning: provider=%s does not support lease touch\n", backend.Spec().Name)
		return lease.Server
	}
	if state == "" {
		state = blank(lease.Server.Labels["state"], "ready")
	}
	server, err := sshBackend.Touch(ctx, TouchRequest{Lease: lease, State: state, IdleTimeout: cfg.IdleTimeout})
	if err != nil {
		fmt.Fprintf(a.Stderr, "warning: touch failed for %s: %v\n", lease.LeaseID, err)
		return lease.Server
	}
	return server
}
