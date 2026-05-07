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
	FeatureCleanup     Feature = "cleanup"
	FeatureDesktop     Feature = "desktop"
	FeatureBrowser     Feature = "browser"
	FeatureCode        Feature = "code"
	FeatureTailscale   Feature = "tailscale"
)

type FeatureSet []Feature

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
	Repo    Repo
	Options LeaseOptions
	Keep    bool
	Reclaim bool
}

type ResolveRequest struct {
	Repo    Repo
	Options LeaseOptions
	ID      string
	Reclaim bool
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
}

type RunRequest struct {
	Repo           Repo
	ID             string
	Options        LeaseOptions
	Keep           bool
	Reclaim        bool
	NoSync         bool
	SyncOnly       bool
	DebugSync      bool
	ShellMode      bool
	ChecksumSync   bool
	ForceSyncLarge bool
	Command        []string
	TimingJSON     bool
}

type WarmupRequest struct {
	Repo          Repo
	Options       LeaseOptions
	Keep          bool
	Reclaim       bool
	ActionsRunner bool
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
	return "provider: hetzner, aws, azure, ssh, blacksmith-testbox, daytona, or islo"
}

func providerHelpSSH() string {
	return "provider: hetzner, aws, azure, ssh, or daytona"
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
	if cfg.TargetOS != targetLinux {
		return exit(2, "--actions-runner requires target=linux")
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

func rejectDelegatedSyncOptions(provider string, req RunRequest) error {
	if req.SyncOnly {
		return exit(2, "%s delegates sync; --sync-only is not supported", provider)
	}
	if req.ChecksumSync {
		return exit(2, "%s delegates sync; --checksum is not supported", provider)
	}
	if req.ForceSyncLarge {
		return exit(2, "%s delegates sync; --force-sync-large is not supported", provider)
	}
	return nil
}

func RejectDelegatedSyncOptions(provider string, req RunRequest) error {
	return rejectDelegatedSyncOptions(provider, req)
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
