package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
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

type ProviderRouter interface {
	RouteConfig(cfg *Config, fs *flag.FlagSet, values any) error
}

type ProviderConfigValidator interface {
	ValidateConfig(cfg Config) error
}

// ControllerProviderContract binds controller lifecycle retries to one opaque
// provider configuration scope and an explicitly idempotent fixed-ID adapter.
type ControllerProviderContract interface {
	ControllerProviderScope(Config) (string, error)
	SupportsControllerFixedLeaseID(Config) bool
}

type ProviderRoutingFlagProvider interface {
	RoutingFlagNames() []string
}

type ProviderCreationOnlyFlagProvider interface {
	CreationOnlyFlagNames() []string
}

type LeaseClaimEndpointPreparer interface {
	PrepareLeaseClaimEndpoint(existing LeaseClaim, provider, slug string, server Server, allowProviderMetadata bool) (Server, error)
}

type ProviderCommandRoutingArgs interface {
	CommandRoutingArgs(cfg Config, leaseID string) []string
}

type DesktopCredentials struct {
	Username string
	Password string
}

type DesktopCredentialProvider interface {
	DesktopCredentials(cfg Config, target SSHTarget) (DesktopCredentials, bool)
}

type ProviderServerTypeProvider interface {
	ServerTypeForConfig(cfg Config) string
	ServerTypeForClass(class string) string
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

type SSHLoginBackend interface {
	Backend
	Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error)
}

type LeaseTouchBackend interface {
	Backend
	Touch(ctx context.Context, req TouchRequest) (Server, error)
}

type SSHLeaseBackend interface {
	SSHLoginBackend
	LeaseTouchBackend
	Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error)
	List(ctx context.Context, req ListRequest) ([]LeaseView, error)
	ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error
}

// StatusTouchClaimValidator lets a provider require identity labels that core
// cannot interpret before status --wait extends a remotely visible lease.
type StatusTouchClaimValidator interface {
	StatusTouchClaimMatches(LeaseTarget, LeaseClaim) bool
}

type ResolvedLeaseTargetRebinder interface {
	RebindResolvedLeaseTarget(target *LeaseTarget, leaseID string) error
}

type TailscaleMetadataBackend interface {
	Backend
	UpdateTailscaleMetadata(ctx context.Context, lease LeaseTarget, meta TailscaleMetadata) (Server, error)
}

type DelegatedRunBackend interface {
	Backend
	Warmup(ctx context.Context, req WarmupRequest) error
	Run(ctx context.Context, req RunRequest) (RunResult, error)
	List(ctx context.Context, req ListRequest) ([]LeaseView, error)
	Status(ctx context.Context, req StatusRequest) (StatusView, error)
	Stop(ctx context.Context, req StopRequest) error
}

// StopReclaimBackend supports an explicit one-shot adoption before a delegated
// provider stop. Providers without a strong adoption contract do not implement it.
type StopReclaimBackend interface {
	Backend
	ReclaimAndStop(ctx context.Context, req StopRequest) error
}

type DelegatedRunArtifactBackend interface {
	Backend
	CollectRunArtifacts(ctx context.Context, req DelegatedRunArtifactRequest) (DelegatedRunArtifactResult, error)
}

type DelegatedRunDownloadBackend interface {
	Backend
	FetchRunFile(ctx context.Context, req DelegatedRunDownloadRequest) ([]byte, error)
}

type PortsRequest struct {
	Options   LeaseOptions
	ID        string
	Publish   []string
	Unpublish []string
	JSON      bool
}

type CopyRequest struct {
	Options     LeaseOptions
	ID          string
	Source      string
	Destination string
	FollowLink  bool
}

type PortsBackend interface {
	Backend
	Ports(ctx context.Context, req PortsRequest) (string, error)
}

type CopyBackend interface {
	Backend
	Copy(ctx context.Context, req CopyRequest) error
}

type CleanupBackend interface {
	Backend
	Cleanup(ctx context.Context, req CleanupRequest) error
}

// PausableBackend is implemented by providers that can pause a lease, freeing
// remote compute while preserving its state, and resume it later. It is
// optional: the `pause`/`resume` commands report a clear error for providers
// that do not implement it.
type PausableBackend interface {
	Backend
	Pause(ctx context.Context, req PauseRequest) error
	Resume(ctx context.Context, req ResumeRequest) error
}

type ReleaseLeaseReporter interface {
	ReleaseLeaseMessage(lease LeaseTarget) string
}

type CheckpointForkWorkdirValidator interface {
	ValidateCheckpointForkWorkdir(ctx context.Context, lease LeaseTarget, workdir string) error
}

type ReleaseLeaseClaimRetainer interface {
	// Retained releases must persist terminal state and clear live endpoints
	// before ReleaseLease returns.
	RetainLeaseClaimAfterRelease(lease LeaseTarget) bool
}

type NativeCheckpointCapability struct {
	Kind              string
	Direct            bool
	CreateUnsupported string
}

type NativeCheckpointRequest struct {
	Config           Config
	Server           Server
	Target           SSHTarget
	Strategy         string
	StrategyExplicit bool
}

type NativeCheckpointProvider interface {
	NativeCheckpointCapability(req NativeCheckpointRequest) (NativeCheckpointCapability, bool)
}

type NativeCheckpointImage struct {
	ID         string
	Name       string
	State      string
	Provider   string
	Kind       string
	Region     string
	ResourceID string
	Direct     bool
}

type NativeCheckpointCreateRequest struct {
	Config      Config
	Server      Server
	Target      SSHTarget
	LeaseID     string
	Name        string
	RepoName    string
	Workdir     string
	Strategy    string
	NoReboot    bool
	Wait        bool
	WaitTimeout time.Duration
	Stderr      io.Writer
}

type NativeCheckpointCreateResult struct {
	Image    NativeCheckpointImage
	Metadata map[string]string
}

type NativeCheckpointWorkdirRequest struct {
	Config   Config
	Server   Server
	LeaseID  string
	RepoName string
	Override string
}

type NativeCheckpointResourceRequest struct {
	Config   Config
	Image    NativeCheckpointImage
	Metadata map[string]string
}

type NativeCheckpointVerifyResult struct {
	ProviderState string
	NextAction    string
	Error         string
}

type NativeCheckpointLifecycleProvider interface {
	NativeCheckpointWorkdir(req NativeCheckpointWorkdirRequest) string
	CreateNativeCheckpoint(ctx context.Context, req NativeCheckpointCreateRequest) (NativeCheckpointCreateResult, error)
	VerifyNativeCheckpoint(ctx context.Context, req NativeCheckpointResourceRequest) (NativeCheckpointVerifyResult, error)
	DeleteNativeCheckpoint(ctx context.Context, req NativeCheckpointResourceRequest) error
}

type NativeCheckpointForkRecord struct {
	Kind        string
	ImageID     string
	Name        string
	Resource    string
	Region      string
	Project     string
	Direct      bool
	HostID      string
	TargetOS    string
	WindowsMode string
	Desktop     bool
	ServerType  string
	Metadata    map[string]string
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

type NativeCheckpointForkFlagProvider interface {
	ApplyNativeCheckpointForkFlags(cfg *Config, fs *flag.FlagSet, values any) error
}

type JSONListBackend interface {
	Backend
	ListJSON(ctx context.Context, req ListRequest) (any, error)
}

type IdempotentLeaseIDBackend interface {
	SupportsRequestedLeaseID() bool
}

type ProviderSpec struct {
	Name        string
	Family      string
	Kind        ProviderKind
	Targets     []TargetSpec
	Features    FeatureSet
	Coordinator CoordinatorMode
	// TailscaleEgressOnly marks FeatureTailscale as outbound userspace access,
	// not a bidirectional peer endpoint.
	TailscaleEgressOnly bool
}

type ProviderKind string

const (
	ProviderKindSSHLease       ProviderKind = "ssh-lease"
	ProviderKindDelegatedRun   ProviderKind = "delegated-run"
	ProviderKindServiceControl ProviderKind = "service-control"
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
	FeatureSSH          Feature = "ssh"
	FeatureCrabboxSync  Feature = "crabbox-sync"
	FeatureArchiveSync  Feature = "archive-sync"
	FeatureCleanup      Feature = "cleanup"
	FeatureDesktop      Feature = "desktop"
	FeatureBrowser      Feature = "browser"
	FeatureCode         Feature = "code"
	FeatureTailscale    Feature = "tailscale"
	FeatureURLBridge    Feature = "url-bridge"
	FeatureCheckpoint   Feature = "workspace-checkpoint"
	FeatureFork         Feature = "workspace-fork"
	FeatureRestore      Feature = "workspace-restore"
	FeatureSnapshot     Feature = "provider-snapshot"
	FeatureCacheVolume  Feature = "cache-volume"
	FeatureRunProof     Feature = "run-proof"
	FeatureRunSession   Feature = "run-session"
	FeatureRunArtifacts Feature = "run-artifacts"
	FeatureRunDownloads Feature = "run-downloads"
	FeatureModuleRun    Feature = "module-run"
	FeaturePauseResume  Feature = "pause-resume"
	FeatureMCP          Feature = "mcp-attachments"
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
	Name                 string
	Args                 []string
	Env                  []string
	Dir                  string
	Stdin                io.Reader
	Stdout               io.Writer
	Stderr               io.Writer
	DisableOutputCapture bool
	// MaxCapturedOutputBytes bounds each internally captured output stream.
	// On overflow the command context is canceled so a continuously emitting
	// child cannot block forever after the capture buffer fills.
	MaxCapturedOutputBytes int
	CancelGracePeriod      time.Duration
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
	commandCtx := ctx
	var cancel context.CancelFunc
	if req.MaxCapturedOutputBytes > 0 && !req.DisableOutputCapture {
		commandCtx, cancel = context.WithCancel(ctx)
		defer cancel()
	}
	cmd := exec.CommandContext(commandCtx, req.Name, req.Args...)
	if req.CancelGracePeriod > 0 {
		cmd.Cancel = func() error {
			err := cmd.Process.Signal(os.Interrupt)
			if errors.Is(err, os.ErrProcessDone) {
				return os.ErrProcessDone
			}
			return err
		}
		cmd.WaitDelay = req.CancelGracePeriod
	}
	if req.MaxCapturedOutputBytes > 0 && !req.DisableOutputCapture {
		// Provider commands are untrusted process trees. Once bounded capture
		// overflows, terminate descendants too: a grandchild retaining stdout or
		// stderr would otherwise keep os/exec's pipe drain blocked indefinitely.
		// A controller child already belongs to a durable outer process group;
		// nesting a new group here would let provider descendants escape recovery.
		controllerOwnsTree := os.Getenv(controllerProcessTreeOwnedEnv) == "1"
		if !controllerOwnsTree {
			configureDaemonCommand(cmd)
		}
		cmd.Cancel = func() error {
			if cmd.Process == nil {
				return os.ErrProcessDone
			}
			if controllerOwnsTree {
				return cmd.Process.Kill()
			}
			return stopDaemonProcess(cmd.Process, cmd.Process.Pid)
		}
		cmd.WaitDelay = controllerChildWaitDelay
	}
	env := req.Env
	if env == nil {
		env = os.Environ()
	}
	// Controller acquire acknowledgment and process-tree ownership are parent /
	// child controls. Provider adapters must never inherit them.
	cmd.Env = stripControllerAcquireIdentityEnv(env)
	cmd.Dir = req.Dir
	cmd.Stdin = req.Stdin
	stdout := commandCaptureBuffer{limit: req.MaxCapturedOutputBytes, cancel: cancel}
	stderr := commandCaptureBuffer{limit: req.MaxCapturedOutputBytes, cancel: cancel}
	if req.Stdout != nil {
		if req.DisableOutputCapture {
			cmd.Stdout = req.Stdout
		} else {
			cmd.Stdout = io.MultiWriter(req.Stdout, &stdout)
		}
	} else {
		if req.DisableOutputCapture {
			cmd.Stdout = io.Discard
		} else {
			cmd.Stdout = &stdout
		}
	}
	if req.Stderr != nil {
		if req.DisableOutputCapture {
			cmd.Stderr = req.Stderr
		} else {
			cmd.Stderr = io.MultiWriter(req.Stderr, &stderr)
		}
	} else {
		if req.DisableOutputCapture {
			cmd.Stderr = io.Discard
		} else {
			cmd.Stderr = &stderr
		}
	}
	err := cmd.Run()
	result := LocalCommandResult{ExitCode: exitCode(err), Stdout: stdout.String(), Stderr: stderr.String()}
	if stdout.overflow || stderr.overflow {
		err = fmt.Errorf("captured command output exceeded %d-byte limit", req.MaxCapturedOutputBytes)
		result.ExitCode = 5
	}
	if err == nil {
		result.ExitCode = 0
	}
	return result, err
}

type commandCaptureBuffer struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
	cancel   context.CancelFunc
}

func (b *commandCaptureBuffer) Write(data []byte) (int, error) {
	if b.limit <= 0 {
		return b.buffer.Write(data)
	}
	original := len(data)
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		if len(data) > remaining {
			b.overflow = true
			data = data[:remaining]
		}
		_, _ = b.buffer.Write(data)
	} else if original > 0 {
		b.overflow = true
	}
	if b.overflow && b.cancel != nil {
		b.cancel()
	}
	return original, nil
}

func (b *commandCaptureBuffer) String() string {
	return b.buffer.String()
}

type LeaseOptions struct {
	TargetOS      string
	WindowsMode   string
	Class         string
	Pond          string
	ProviderScope string
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
	Repo             Repo
	Options          LeaseOptions
	Keep             bool
	Reclaim          bool
	RequestedLeaseID string
	RequestedSlug    string
	// OnAcquired observes a fully validated raw provider identity before local
	// routing, readiness, or claim side effects. Returning an error requires the
	// provider adapter to roll back the acquired resource.
	OnAcquired func(LeaseTarget) error
}

type ResolveRequest struct {
	Repo        Repo
	Options     LeaseOptions
	ID          string
	Reclaim     bool
	ReleaseOnly bool
	StatusOnly  bool
	ReadyProbe  bool
	Prepare     bool
	// RejectAuthSecret prevents interactive callers from launching SSH probes
	// for token-as-username targets they cannot safely execute.
	RejectAuthSecret bool
	// NoLocalStateMutations is reserved for controller-owned identity-bound
	// lookups that must not rewrite claims or provider routing before the
	// resolved identity has been accepted by the caller.
	NoLocalStateMutations    bool
	ExpectedProviderIdentity ProviderIdentityExpectation
}

// IsReadOnlyStatus reports whether resolution may inspect provider inventory
// without trusting or rewriting local claim state.
func (r ResolveRequest) IsReadOnlyStatus() bool {
	return r.StatusOnly && r.NoLocalStateMutations && !r.ReleaseOnly && !r.Reclaim
}

type ReleaseLeaseRequest struct {
	Lease                    LeaseTarget
	Force                    bool
	ExpectedProviderIdentity ProviderIdentityExpectation
}

// ConfirmedAbsentLocalCleanupRequest carries the immutable provider identity
// proven absent by a complete refreshed inventory. Implementations must only
// remove local sidecars; they must not invoke provider lifecycle operations.
type ConfirmedAbsentLocalCleanupRequest struct {
	ExpectedProviderIdentity ProviderIdentityExpectation
	ProviderScope            string
}

type ConfirmedAbsentLocalStateCleaner interface {
	Backend
	CleanupConfirmedAbsentLocalState(context.Context, ConfirmedAbsentLocalCleanupRequest) error
}

// ProviderIdentityExpectation is the complete immutable identity known by a
// lifecycle caller before resolving a resource for destructive release.
type ProviderIdentityExpectation struct {
	LeaseID        string
	AttemptLeaseID string
	Slug           string
	ResourceID     string
}

func (i ProviderIdentityExpectation) empty() bool {
	return i.LeaseID == "" && i.AttemptLeaseID == "" && i.Slug == "" && i.ResourceID == ""
}

func ValidateProviderIdentityExpectation(i ProviderIdentityExpectation) error {
	for _, identity := range []struct {
		name  string
		value string
	}{{"lease ID", i.LeaseID}, {"attempt lease ID", i.AttemptLeaseID}} {
		name, value := identity.name, identity.value
		if value != "" && (value != strings.TrimSpace(value) || !validLeaseClaimID(value)) {
			return exit(2, "invalid expected provider %s", name)
		}
	}
	if i.Slug != "" && (i.Slug != strings.TrimSpace(i.Slug) || normalizeLeaseSlug(i.Slug) != i.Slug) {
		return exit(2, "invalid expected provider slug")
	}
	if i.ResourceID != "" && (i.ResourceID != strings.TrimSpace(i.ResourceID) || !validControllerInventoryIdentity(i.ResourceID)) {
		return exit(2, "invalid expected provider resource ID")
	}
	if i.LeaseID == "" && i.AttemptLeaseID == "" {
		return exit(2, "expected provider identity requires a lease or attempt lease ID")
	}
	return nil
}

// ValidateLeaseTargetProviderIdentity rejects any resolved release target that
// does not satisfy every non-empty identity persisted by the controller.
func ValidateLeaseTargetProviderIdentity(lease LeaseTarget, expected ProviderIdentityExpectation) error {
	if expected.empty() {
		return nil
	}
	if err := ValidateProviderIdentityExpectation(expected); err != nil {
		return err
	}
	actualLeaseID := lease.LeaseID
	for _, identity := range []struct {
		name  string
		value string
	}{{"lease ID", expected.LeaseID}, {"attempt lease ID", expected.AttemptLeaseID}} {
		name, value := identity.name, identity.value
		if value != "" && actualLeaseID != value {
			return exit(4, "provider %s mismatch before release: expected %s, found %s", name, value, blank(actualLeaseID, "<empty>"))
		}
	}
	if expected.Slug != "" {
		actualSlug := serverSlug(lease.Server)
		if actualSlug != expected.Slug {
			return exit(4, "provider slug mismatch before release: expected %s, found %s", expected.Slug, blank(actualSlug, "<empty>"))
		}
	}
	if expected.ResourceID != "" {
		actualResourceID := lease.Server.DisplayID()
		if actualResourceID != expected.ResourceID {
			return exit(4, "provider resource ID mismatch before release: expected %s, found %s", expected.ResourceID, blank(actualResourceID, "<empty>"))
		}
	}
	return nil
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
	Repo                  Repo
	ID                    string
	Options               LeaseOptions
	Keep                  bool
	Reclaim               bool
	NoSync                bool
	SyncOnly              bool
	DebugSync             bool
	ShellMode             bool
	ChecksumSync          bool
	ForceSyncLarge        bool
	FullResync            bool
	EnvHelper             string
	CaptureStdout         string
	CaptureStderr         string
	CaptureOnFail         bool
	KeepOnFailure         bool
	Preflight             bool
	Downloads             []string
	Env                   map[string]string
	EnvSummary            bool
	ScriptRequested       bool
	Script                *RunScriptSpec
	FreshPR               FreshPRSpec
	ApplyLocalPatch       bool
	Command               []string
	Label                 string
	RequestedSlug         string
	TimingJSON            bool
	ArtifactGlobs         []string
	RequiredArtifactGlobs []string
	EmitProof             string
	ProofTemplate         string
	ProfileVariables      map[string]string
	StopAfter             string
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

type PauseRequest struct {
	Options LeaseOptions
	ID      string
}

type ResumeRequest struct {
	Options LeaseOptions
	ID      string
}

type CleanupRequest struct {
	Options LeaseOptions
	DryRun  bool
}

type RunResult struct {
	ExitCode      int
	Status        RunStatus
	ErrorKind     RunErrorKind
	Command       time.Duration
	Total         time.Duration
	SyncDelegated bool
	Session       *RunSessionHandle
	Provider      string
	LeaseID       string
	Slug          string
	CommandText   string
	LogExcerpt    string
	ActionsURL    string
	Artifacts     []RunArtifact
}

type RunStatus string

const (
	RunStatusSucceeded RunStatus = "succeeded"
	RunStatusFailed    RunStatus = "failed"
	RunStatusTimedOut  RunStatus = "timed-out"
	RunStatusCanceled  RunStatus = "canceled"
)

type RunErrorKind string

const (
	RunErrorNone        RunErrorKind = ""
	RunErrorCommandExit RunErrorKind = "command-exit"
	RunErrorTimeout     RunErrorKind = "timeout"
	RunErrorCanceled    RunErrorKind = "canceled"
	RunErrorProvider    RunErrorKind = "provider-error"
)

func FinalizeRunResult(result RunResult, err error) RunResult {
	if result.Status == "" {
		result.Status = RunStatusForResult(result, err)
	}
	if result.ErrorKind == "" {
		result.ErrorKind = RunErrorKindForResult(result, err)
	}
	return result
}

func RunStatusForResult(result RunResult, err error) RunStatus {
	if errors.Is(err, context.DeadlineExceeded) {
		return RunStatusTimedOut
	}
	if errors.Is(err, context.Canceled) {
		return RunStatusCanceled
	}
	if result.ExitCode != 0 || err != nil {
		return RunStatusFailed
	}
	return RunStatusSucceeded
}

func RunErrorKindForResult(result RunResult, err error) RunErrorKind {
	if errors.Is(err, context.DeadlineExceeded) {
		return RunErrorTimeout
	}
	if errors.Is(err, context.Canceled) {
		return RunErrorCanceled
	}
	if result.ExitCode != 0 {
		return RunErrorCommandExit
	}
	if err != nil {
		return RunErrorProvider
	}
	return RunErrorNone
}

type DelegatedRunArtifactRequest struct {
	RunReq   RunRequest
	Result   RunResult
	MaxFiles int
	MaxBytes int64
}

type DelegatedRunArtifactResult struct {
	Artifacts []RunArtifact
	Output    string
}

type RunSessionHandle struct {
	Provider       string `json:"provider"`
	LeaseID        string `json:"leaseId"`
	Slug           string `json:"slug,omitempty"`
	Reused         bool   `json:"reused"`
	Kept           bool   `json:"kept"`
	ActionsURL     string `json:"actionsUrl,omitempty"`
	RunID          string `json:"runId,omitempty"`
	CleanupCommand string `json:"cleanupCommand"`
}

func ValidateRunSessionForSpec(spec ProviderSpec, result RunResult) error {
	session := result.Session
	if session == nil {
		return nil
	}
	provider := blank(strings.TrimSpace(spec.Name), "provider")
	if !featureSetHas(spec.Features, FeatureRunSession) {
		return exit(2, "%s returned a run session but does not advertise %s", provider, FeatureRunSession)
	}
	if strings.TrimSpace(session.Provider) == "" {
		return exit(2, "%s returned a run session without provider", provider)
	}
	if strings.TrimSpace(session.LeaseID) == "" {
		return exit(2, "%s returned a run session without lease id", provider)
	}
	if strings.TrimSpace(session.CleanupCommand) == "" {
		return exit(2, "%s returned a run session without cleanup command", provider)
	}
	return nil
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

// RegisteredProviderNames returns the canonical names of every registered provider.
func RegisteredProviderNames() []string {
	providers := registeredProviders()
	names := make([]string, 0, len(providers))
	for _, provider := range providers {
		names = append(names, provider.Name())
	}
	return names
}

func normalizeProviderName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func providerHelpAll() string {
	return "provider: " + strings.Join(providerNamesForHelp(nil), ", ")
}

func providerHelpEnvValues() string {
	return joinProviderNames(providerNamesForHelp(nil))
}

func joinProviderNames(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	default:
		return strings.Join(names[:len(names)-1], ", ") + ", or " + names[len(names)-1]
	}
}

func providerHelpSSH() string {
	return "provider: " + strings.Join(providerNamesForHelp(func(spec ProviderSpec) bool {
		return spec.Features.Has(FeatureSSH)
	}), ", ")
}

func providerHelpCleanup() string {
	return "provider: " + joinProviderNames(providerNamesForHelp(func(spec ProviderSpec) bool {
		return spec.Features.Has(FeatureCleanup)
	}))
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

func providerNamesForHelp(include func(ProviderSpec) bool) []string {
	providers := registeredProviders()
	names := make([]string, 0, len(providers))
	for _, provider := range providers {
		spec := provider.Spec()
		if include != nil && !include(spec) {
			continue
		}
		names = append(names, provider.Name())
	}
	return names
}

func applyProviderRoutingFlags(cfg *Config, fs *flag.FlagSet, values providerFlagValues) error {
	if routed, err := routeProviderFlagOverride(cfg, fs, values); routed || err != nil {
		return err
	}
	provider, err := ProviderFor(cfg.Provider)
	if err != nil {
		return err
	}
	if router, ok := provider.(ProviderRouter); ok {
		cfg.Provider = provider.Name()
		if err := router.RouteConfig(cfg, fs, values[provider.Name()]); err != nil {
			return err
		}
		if resolved, err := ProviderFor(cfg.Provider); err == nil {
			cfg.Provider = resolved.Name()
		}
	}
	return nil
}

func applyProviderFlags(cfg *Config, fs *flag.FlagSet, values providerFlagValues) error {
	if flagWasSet(fs, "provider") {
		cfg.providerExplicit = true
	}
	if _, err := routeProviderFlagOverride(cfg, fs, values); err != nil {
		return err
	}
	provider, err := ProviderFor(cfg.Provider)
	if err != nil {
		return err
	}
	before := provider.Name()
	if err := provider.ApplyFlags(cfg, fs, values[provider.Name()]); err != nil {
		return err
	}
	after, err := ProviderFor(cfg.Provider)
	if err != nil || after.Name() == before {
		if err == nil {
			markCredentialDestinationFlagSources(cfg, fs)
			applyCloudflareDynamicWorkersRepositoryCaps(cfg)
		}
		return err
	}
	cfg.Provider = after.Name()
	if err := after.ApplyFlags(cfg, fs, values[after.Name()]); err != nil {
		return err
	}
	markCredentialDestinationFlagSources(cfg, fs)
	applyCloudflareDynamicWorkersRepositoryCaps(cfg)
	return nil
}

func validateProviderConfig(cfg Config) error {
	if err := validateProviderCredentialDestination(cfg); err != nil {
		return err
	}
	provider, err := ProviderFor(cfg.Provider)
	if err != nil {
		return err
	}
	if validator, ok := provider.(ProviderConfigValidator); ok {
		return validator.ValidateConfig(cfg)
	}
	return nil
}

func providerCommandRoutingArgs(cfg Config, leaseID string) []string {
	provider, err := ProviderFor(cfg.Provider)
	if err != nil {
		return nil
	}
	router, ok := provider.(ProviderCommandRoutingArgs)
	if !ok {
		return nil
	}
	return router.CommandRoutingArgs(cfg, leaseID)
}

func routeProviderFlagOverride(cfg *Config, fs *flag.FlagSet, values providerFlagValues) (bool, error) {
	if fs == nil {
		return false, nil
	}
	current, err := ProviderFor(cfg.Provider)
	if err != nil {
		return false, err
	}
	currentFamily := providerFamily(current)
	for _, candidate := range registeredProviders() {
		flagger, ok := candidate.(ProviderRoutingFlagProvider)
		if !ok || providerFamily(candidate) != currentFamily || !anyFlagWasSet(fs, flagger.RoutingFlagNames()) {
			continue
		}
		router, ok := candidate.(ProviderRouter)
		if !ok {
			continue
		}
		cfg.Provider = candidate.Name()
		if err := router.RouteConfig(cfg, fs, values[candidate.Name()]); err != nil {
			return true, err
		}
		if resolved, err := ProviderFor(cfg.Provider); err == nil {
			cfg.Provider = resolved.Name()
		}
		return true, nil
	}
	return false, nil
}

func providerFamily(provider Provider) string {
	spec := provider.Spec()
	return firstNonBlank(spec.Family, provider.Name())
}

func anyFlagWasSet(fs *flag.FlagSet, names []string) bool {
	for _, name := range names {
		if flagWasSet(fs, name) {
			return true
		}
	}
	return false
}

func routeConfiguredProvider(cfg *Config) error {
	provider, err := ProviderFor(cfg.Provider)
	if err != nil {
		return err
	}
	cfg.Provider = provider.Name()
	if router, ok := provider.(ProviderRouter); ok {
		if err := router.RouteConfig(cfg, nil, nil); err != nil {
			return err
		}
	}
	if resolved, err := ProviderFor(cfg.Provider); err == nil {
		cfg.Provider = resolved.Name()
	}
	return nil
}

func runtimeForApp(a App) Runtime {
	return Runtime{Stdout: a.Stdout, Stderr: a.Stderr, Clock: realClock{}, Exec: execCommandRunner{}}
}

const (
	controllerProviderScopeEnv                   = "CRABBOX_ADAPTER_PROVIDER_SCOPE"
	controllerCoordinatorRegistrationExpectedEnv = "CRABBOX_ADAPTER_COORDINATOR_REGISTRATION_EXPECTED"
	controllerCoordinatorRegistrationURLEnv      = "CRABBOX_ADAPTER_COORDINATOR_REGISTRATION_URL"
	controllerWorkspaceIDEnv                     = "CRABBOX_ADAPTER_WORKSPACE_ID"
)

func controllerProviderIdentityForConfig(cfg Config) (string, string, bool, error) {
	provider, err := ProviderFor(cfg.Provider)
	if err != nil {
		return "", "", false, err
	}
	cfg.Provider = provider.Name()
	contract, ok := provider.(ControllerProviderContract)
	if !ok {
		return "", "", false, fmt.Errorf("provider=%s does not expose a controller routing scope", provider.Name())
	}
	scope, err := contract.ControllerProviderScope(cfg)
	if err != nil {
		return "", "", false, err
	}
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return "", "", false, fmt.Errorf("provider=%s returned an empty controller routing scope", provider.Name())
	}
	return provider.Name(), scope, contract.SupportsControllerFixedLeaseID(cfg), nil
}

func validateControllerProviderScope(cfg Config) error {
	expected := strings.TrimSpace(os.Getenv(controllerProviderScopeEnv))
	if expected == "" {
		return nil
	}
	provider, actual, _, err := controllerProviderIdentityForConfig(cfg)
	if err != nil {
		return err
	}
	if actual != expected {
		return exit(2, "provider=%s controller routing scope changed; refusing lifecycle operation", provider)
	}
	return nil
}

func validateControllerCoordinatorRegistrationBinding(cfg Config) error {
	if os.Getenv(controllerCoordinatorRegistrationExpectedEnv) != "1" {
		return nil
	}
	expected := os.Getenv(controllerCoordinatorRegistrationURLEnv)
	if err := validateControllerCoordinatorRegistrationURL(expected); err != nil {
		return err
	}
	actual, err := coordinatorRegistrationURLForConfig(cfg)
	if err != nil {
		return err
	}
	if actual != expected {
		return exit(4, "coordinator registration binding changed before provider acquisition")
	}
	return nil
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
	if err := validateControllerProviderScope(cfg); err != nil {
		return nil, err
	}
	if err := validateControllerCoordinatorRegistrationBinding(cfg); err != nil {
		return nil, err
	}
	backend, err := configureProviderBackend(provider, &cfg, rt)
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

func configureProviderBackend(provider Provider, cfg *Config, rt Runtime) (Backend, error) {
	cfg.Provider = provider.Name()
	applySingleProviderTargetDefault(cfg)
	if err := validateProviderConfig(*cfg); err != nil {
		return nil, err
	}
	return provider.Configure(*cfg, rt)
}

func shouldUseCoordinator(cfg Config, spec ProviderSpec) bool {
	return cfg.BrokerMode != BrokerModeRegistered &&
		spec.Coordinator == CoordinatorSupported && strings.TrimSpace(cfg.Coordinator) != ""
}

func shouldRegisterCoordinatorLease(cfg Config) bool {
	return cfg.BrokerMode == BrokerModeRegistered && strings.TrimSpace(cfg.Coordinator) != ""
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
		Pond:          normalizePondName(cfg.Pond),
		ProviderScope: providerClaimScope(canonicalClaimProvider(cfg.Provider), cfg),
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
	if name := backend.Spec().Name; name == "local-container" || name == "apple-container" || name == "multipass" {
		return exit(2, "--actions-runner is not supported for provider=%s; use normal crabbox run or a remote SSH provider", name)
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
	moduleRun := featureSetHas(spec.Features, FeatureModuleRun)
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
	runArtifacts := featureSetHas(spec.Features, FeatureRunArtifacts)
	runDownloads := featureSetHas(spec.Features, FeatureRunDownloads)
	if len(req.Downloads) > 0 && !runDownloads {
		return exit(2, "%s delegates run execution; --download is not supported", provider)
	}
	if len(req.ArtifactGlobs) > 0 && !runArtifacts {
		return exit(2, "%s delegates run execution; --artifact-glob is not supported", provider)
	}
	if len(req.RequiredArtifactGlobs) > 0 && !runArtifacts && !runDownloads {
		return exit(2, "%s delegates run execution; --require-artifact is not supported", provider)
	}
	if runDownloads {
		if err := validateDelegatedDownloads(req.Downloads); err != nil {
			return err
		}
	}
	if runDownloads && !runArtifacts {
		if err := validateDelegatedRequiredArtifacts(req.RequiredArtifactGlobs); err != nil {
			return err
		}
	}
	if req.EmitProof != "" && !featureSetHas(spec.Features, FeatureRunProof) {
		return exit(2, "%s delegates run execution; --emit-proof is not supported", provider)
	}
	if req.StopAfter != "" {
		return exit(2, "%s delegates run execution; --stop-after is not supported", provider)
	}
	if (req.Script != nil || req.ScriptRequested) && !moduleRun {
		return exit(2, "%s delegates run execution; --script is not supported", provider)
	}
	if moduleRun && len(req.Command) > 0 {
		return exit(2, "%s executes module source; trailing shell commands are not supported", provider)
	}
	if moduleRun && req.ShellMode {
		return exit(2, "%s executes module source; --shell is not supported", provider)
	}
	if !req.FreshPR.Empty() {
		return exit(2, "%s delegates sync; --fresh-pr is not supported", provider)
	}
	return nil
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
	sshBackend, ok := backend.(LeaseTouchBackend)
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
