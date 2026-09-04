package opencomputer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const (
	openComputerCleanupTimeout  = 15 * time.Second
	openComputerExecTimeoutSecs = 3600
	openComputerClaimTagKey     = "crabbox.claim"
)

func NewOpenComputerBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	return &openComputerBackend{spec: spec, cfg: cfg, rt: rt}
}

type openComputerBackend struct {
	spec                   ProviderSpec
	cfg                    Config
	rt                     Runtime
	cleanupTimeoutOverride time.Duration
}

func (b *openComputerBackend) Spec() ProviderSpec { return b.spec }

func (b *openComputerBackend) Warmup(ctx context.Context, req WarmupRequest) error {
	if req.ActionsRunner {
		return exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	started := b.now()
	api, err := newOCAPIClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, sandboxID, slug, err := b.createSandbox(ctx, api, req.Repo, req.Reclaim, req.RequestedSlug)
	if err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s sandbox=%s\n", leaseID, slug, providerName, sandboxID)
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: opencomputer warmup keeps the sandbox until explicit stop\n")
	}
	total := b.now().Sub(started)
	fmt.Fprintf(b.rt.Stdout, "warmup complete total=%s\n", total.Round(time.Millisecond))
	if req.TimingJSON {
		return writeTimingJSON(b.rt.Stderr, timingReport{
			Provider: providerName,
			LeaseID:  leaseID,
			Slug:     slug,
			TotalMs:  total.Milliseconds(),
			ExitCode: 0,
		})
	}
	return nil
}

func (b *openComputerBackend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	workdir, err := openComputerWorkdir(b.cfg)
	if err != nil {
		return RunResult{}, err
	}
	var api *ocAPIClient
	var leaseID, sandboxID, slug string
	boundSandbox := func() shared.DelegatedSandbox {
		fmt.Fprintf(b.rt.Stderr, "provider=%s lease=%s sandbox=%s workdir=%s\n", providerName, leaseID, sandboxID, workdir)
		return shared.DelegatedSandbox{LeaseID: leaseID, Slug: slug, CleanupCommand: openComputerCleanupCommand(leaseID)}
	}
	return shared.RunDelegatedSandbox(ctx, req, shared.DelegatedSandboxLifecycle{
		Provider: providerName, Runtime: b.rt, Workdir: workdir,
		IdleTimeout: b.cfg.IdleTimeout, TTL: b.cfg.TTL, CleanupTimeout: b.cleanupTimeout(),
		Preflight: func(context.Context) error {
			var err error
			api, err = newOCAPIClient(b.cfg, b.rt)
			return err
		},
		PrepareArchive: func(ctx context.Context) (*core.PreparedArchive, error) {
			return core.PrepareDelegatedArchive(ctx, core.DelegatedArchivePreparationRequest{
				Config: b.cfg, Repo: req.Repo, ForceSyncLarge: req.ForceSyncLarge,
				TempPattern: "crabbox-opencomputer-sync-*.tgz", Stderr: b.rt.Stderr, Now: b.now,
			})
		},
		Acquire: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			var err error
			leaseID, sandboxID, slug, err = b.createSandbox(ctx, api, req.Repo, req.Reclaim, req.RequestedSlug)
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s sandbox=%s\n", leaseID, slug, providerName, sandboxID)
			return boundSandbox(), nil
		},
		Resolve: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			var err error
			leaseID, sandboxID, _, err = resolveLeaseID(req.ID, "", false, 0, api.baseURL)
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			if _, err := verifyOpenComputerClaim(ctx, api, leaseID, sandboxID); err != nil {
				return shared.DelegatedSandbox{}, err
			}
			claim, err := readLeaseClaim(leaseID)
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			_, _, slug, err = finishResolvedLease(claim, req.Repo.Root, req.Reclaim, b.cfg.IdleTimeout, api.baseURL)
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			return boundSandbox(), nil
		},
		Sync: func(ctx context.Context, prepared *core.PreparedArchive) ([]core.TimingPhase, time.Duration, error) {
			return b.syncWorkspace(ctx, api, sandboxID, req, workdir, prepared)
		},
		NoSync: func(ctx context.Context) error {
			return b.ensureWorkspace(ctx, api, sandboxID, workdir)
		},
		Command: func(context.Context) (shared.DelegatedSandboxCommand, error) {
			intent, err := core.ParseCommandIntent(req.Command, req.ShellMode, req.CommandLiteralArgs)
			if err != nil {
				return shared.DelegatedSandboxCommand{}, err
			}
			command := intent.Argv("bash", "-lc")
			if req.EnvSummary || strings.TrimSpace(os.Getenv("CRABBOX_ENV_ALLOW")) != "" {
				printEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
			}
			return shared.DelegatedSandboxCommand{
				Text: strings.Join(req.Command, " "),
				Run: func(ctx context.Context) (int, error) {
					return b.execCommand(ctx, api, sandboxID, workdir, command, req.Env)
				},
			}, nil
		},
		Cleanup: func(ctx context.Context) error {
			if err := api.killSandbox(ctx, sandboxID); err != nil && !isOCNotFound(err) {
				return err
			}
			removeLeaseClaim(leaseID)
			return nil
		},
	})
}

func openComputerCleanupCommand(leaseID string) string {
	return "crabbox stop --provider " + providerName + " --id " + shellQuote(leaseID)
}

func (b *openComputerBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	api, err := newOCAPIClient(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	claims, err := listOpenComputerLeaseClaims()
	if err != nil {
		return nil, err
	}
	servers := make([]Server, 0, len(claims))
	for _, claim := range claims {
		if claim.Provider != providerName || !strings.HasPrefix(claim.LeaseID, leasePrefix) {
			continue
		}
		if validateOpenComputerClaimScope(claim, api.baseURL) != nil {
			continue
		}
		sandboxID := strings.TrimPrefix(claim.LeaseID, leasePrefix)
		if sandboxID == "" {
			continue
		}
		sb, getErr := api.getSandboxWithTags(ctx, sandboxID)
		state := ""
		if getErr != nil {
			if isOCNotFound(getErr) {
				state = "missing-or-inaccessible"
			} else {
				return nil, getErr
			}
		} else {
			if err := validateOpenComputerSandboxOwnership(claim, sb); err != nil {
				return nil, err
			}
			state = blank(sb.Status, statusViewReady)
		}
		servers = append(servers, Server{
			Provider: providerName,
			CloudID:  sandboxID,
			Name:     sandboxID,
			Status:   state,
			Labels: map[string]string{
				"provider": providerName,
				"lease":    claim.LeaseID,
				"slug":     claim.Slug,
				"pond":     claim.Pond,
				"target":   targetLinux,
				"state":    state,
			},
		})
	}
	return servers, nil
}

func (b *openComputerBackend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	api, err := newOCAPIClient(b.cfg, b.rt)
	if err != nil {
		return DoctorResult{}, err
	}
	if err := api.probeSandboxes(ctx); err != nil {
		return DoctorResult{}, err
	}
	servers, err := b.List(ctx, ListRequest{})
	if err != nil {
		return DoctorResult{}, err
	}
	return inventoryDoctorResult(providerName, len(servers)), nil
}

func (b *openComputerBackend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	api, err := newOCAPIClient(b.cfg, b.rt)
	if err != nil {
		return StatusView{}, err
	}
	leaseID, sandboxID, slug, err := resolveLeaseID(req.ID, "", false, 0, api.baseURL)
	if err != nil {
		return StatusView{}, err
	}
	claim, ok, err := resolveOpenComputerLeaseClaim(leaseID, api.baseURL)
	if err != nil {
		return StatusView{}, err
	}
	if !ok {
		return StatusView{}, exit(4, "opencomputer sandbox %q is not claimed by Crabbox", req.ID)
	}
	waitTimeout := req.WaitTimeout
	if waitTimeout <= 0 {
		waitTimeout = 5 * time.Minute
	}
	deadline := b.now().Add(waitTimeout)
	pollCtx := ctx
	cancel := func() {}
	if req.Wait {
		pollCtx, cancel = context.WithTimeout(ctx, waitTimeout)
	}
	defer cancel()
	for {
		sb, getErr := api.getSandboxWithTags(pollCtx, sandboxID)
		if getErr != nil {
			if req.Wait && errors.Is(pollCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				return StatusView{}, exit(5, "timed out waiting for opencomputer sandbox %s to become ready", sandboxID)
			}
			if ctx.Err() != nil {
				return StatusView{}, ctx.Err()
			}
			// Surface real API failures (auth, 5xx, sandbox gone) instead of
			// masking them as a not-ready status.
			return StatusView{}, getErr
		}
		if err := validateOpenComputerSandboxOwnership(claim, sb); err != nil {
			return StatusView{}, err
		}
		state := strings.ToLower(strings.TrimSpace(sb.Status))
		view := StatusView{
			ID:       leaseID,
			Slug:     slug,
			Provider: providerName,
			TargetOS: targetLinux,
			State:    state,
			ServerID: sandboxID,
			Pond:     claim.Pond,
			Network:  NetworkPublic,
			Ready:    isReadyState(state),
			Labels: map[string]string{
				"provider": providerName,
				"lease":    leaseID,
				"pond":     claim.Pond,
				"state":    state,
			},
		}
		if !req.Wait || view.Ready {
			return view, nil
		}
		if isTerminalState(state) {
			return StatusView{}, exit(5, "opencomputer sandbox %s entered terminal state %q before becoming ready", sandboxID, state)
		}
		if b.now().After(deadline) {
			return StatusView{}, exit(5, "timed out waiting for opencomputer sandbox %s to become ready", sandboxID)
		}
		select {
		case <-pollCtx.Done():
			if errors.Is(pollCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				return StatusView{}, exit(5, "timed out waiting for opencomputer sandbox %s to become ready", sandboxID)
			}
			return StatusView{}, pollCtx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *openComputerBackend) Stop(ctx context.Context, req StopRequest) error {
	api, err := newOCAPIClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, sandboxID, _, err := resolveLeaseID(req.ID, "", false, 0, api.baseURL)
	if err != nil {
		return err
	}
	if _, err := verifyOpenComputerClaim(ctx, api, leaseID, sandboxID); err != nil {
		if !isOCNotFound(err) || !b.cfg.OpenComputer.ForgetMissing {
			return err
		}
		fmt.Fprintf(b.rt.Stderr, "warning: forgetting missing opencomputer sandbox=%s after explicit request\n", sandboxID)
		removeLeaseClaim(leaseID)
		return nil
	}
	if err := api.killSandbox(ctx, sandboxID); err != nil {
		if !isOCNotFound(err) || !b.cfg.OpenComputer.ForgetMissing {
			return err
		}
		fmt.Fprintf(b.rt.Stderr, "warning: forgetting missing opencomputer sandbox=%s after explicit request\n", sandboxID)
	}
	removeLeaseClaim(leaseID)
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s\n", leaseID, sandboxID)
	return nil
}

// execCommand runs the user command via POST /exec/run, forwarding env in the
// request body and streaming the buffered stdout/stderr back to the caller.
func (b *openComputerBackend) execCommand(ctx context.Context, api *ocAPIClient, sandboxID, workdir string, command []string, env map[string]string) (int, error) {
	if len(command) == 0 {
		return 2, errors.New("missing command")
	}
	res, err := api.execRun(ctx, sandboxID, execRunRequest{
		Cmd:     command[0],
		Args:    command[1:],
		Envs:    env,
		Cwd:     workdir,
		Timeout: b.execTimeoutSecs(),
	})
	if err != nil {
		return 1, err
	}
	if res.Stdout != "" {
		_, _ = io.WriteString(b.rt.Stdout, res.Stdout)
	}
	if res.Stderr != "" {
		_, _ = io.WriteString(b.rt.Stderr, res.Stderr)
	}
	return res.ExitCode, nil
}

// createSandbox creates a Crabbox-owned sandbox and records the local lease.
// Returns (leaseID, sandboxID, slug, err).
func (b *openComputerBackend) createSandbox(ctx context.Context, api *ocAPIClient, repo Repo, reclaim bool, requestedSlug string) (string, string, string, error) {
	providerScope, err := newOpenComputerClaimScope(api.baseURL)
	if err != nil {
		return "", "", "", err
	}
	req := createSandboxRequest{
		Timeout: b.cfg.OpenComputer.TimeoutSecs,
		Burst:   b.cfg.OpenComputer.Burst,
		Metadata: map[string]string{
			"crabbox":      "true",
			"crabbox-name": newSandboxName(repo),
		},
	}
	if b.cfg.OpenComputer.CPU > 0 {
		req.CPUCount = b.cfg.OpenComputer.CPU
	}
	if b.cfg.OpenComputer.MemoryMB > 0 {
		req.MemoryMB = b.cfg.OpenComputer.MemoryMB
	}
	sb, err := api.createSandbox(ctx, req)
	if err != nil {
		return "", "", "", err
	}
	if err := api.replaceSandboxTags(ctx, sb.ID, map[string]string{openComputerClaimTagKey: providerScope}); err != nil {
		return leasePrefix + sb.ID, sb.ID, "", b.cleanupCreateFailure(ctx, api, sb.ID, err)
	}
	leaseID := leasePrefix + sb.ID
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return leaseID, sb.ID, "", b.cleanupCreateFailure(ctx, api, sb.ID, err)
	}
	if err := claimLeaseForRepoProviderScopePond(leaseID, slug, providerName, providerScope, b.cfg.Pond, repo.Root, b.cfg.IdleTimeout, reclaim); err != nil {
		return leaseID, sb.ID, slug, b.cleanupCreateFailure(ctx, api, sb.ID, err)
	}
	return leaseID, sb.ID, slug, nil
}

// resolveLeaseID resolves a user-supplied identifier (slug, lease ID, or raw
// OpenComputer sandbox ID) to a (leaseID, sandboxID, slug) tuple. Resolution is
// strict: only locally-claimed Crabbox sandboxes are accepted, mirroring islo
// and tensorlake. Raw IDs are accepted only when a matching `ocbx_<id>` claim
// exists.
func resolveLeaseID(id, repoRoot string, reclaim bool, idleTimeout time.Duration, baseURL string) (string, string, string, error) {
	return shared.ResolveScopedLeaseID(id, shared.ScopedLeaseResolver{
		Provider:      providerName,
		LeasePrefix:   leasePrefix,
		ReadClaim:     readLeaseClaim,
		ListClaims:    listOpenComputerLeaseClaims,
		ValidateClaim: func(claim LeaseClaim) error { return validateOpenComputerClaimScope(claim, baseURL) },
		FinishClaim: func(claim LeaseClaim) (string, string, string, error) {
			return finishResolvedLease(claim, repoRoot, reclaim, idleTimeout, baseURL)
		},
		EmptyIdentifierError: func() error {
			return exit(2, "provider=opencomputer requires a Crabbox-created sandbox slug or lease id")
		},
		UnclaimedIdentifierError: func(identifier string) error {
			return exit(4, "opencomputer sandbox %q is not claimed by Crabbox; use a Crabbox slug or %s<sandbox-id>", identifier, leasePrefix)
		},
	})
}

func resolveOpenComputerLeaseClaim(identifier, baseURL string) (LeaseClaim, bool, error) {
	return shared.ResolveScopedLeaseClaim(identifier, providerName, listOpenComputerLeaseClaims, func(claim LeaseClaim) error {
		return validateOpenComputerClaimScope(claim, baseURL)
	})
}

func finishResolvedLease(claim LeaseClaim, repoRoot string, reclaim bool, idleTimeout time.Duration, baseURL string) (string, string, string, error) {
	return shared.FinishScopedLease(claim, shared.ScopedLeaseFinishOptions{
		Provider:      providerName,
		LeasePrefix:   leasePrefix,
		RepoRoot:      repoRoot,
		Reclaim:       reclaim,
		IdleTimeout:   idleTimeout,
		ValidateClaim: func(claim LeaseClaim) error { return validateOpenComputerClaimScope(claim, baseURL) },
	})
}

func validateOpenComputerClaimScope(claim LeaseClaim, baseURL string) error {
	if !strings.HasPrefix(strings.TrimSpace(claim.ProviderScope), openComputerEndpointScope(baseURL)+"/ownership:") {
		return exit(4, "opencomputer lease %q belongs to a different API endpoint; restore the endpoint used to create it", claim.LeaseID)
	}
	return nil
}

func newOpenComputerClaimScope(baseURL string) (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", exit(5, "generate opencomputer ownership token: %v", err)
	}
	return openComputerEndpointScope(baseURL) + "/ownership:" + hex.EncodeToString(token[:]), nil
}

func openComputerEndpointScope(baseURL string) string {
	digest := sha256.Sum256([]byte(baseURL))
	return "endpoint-sha256:" + hex.EncodeToString(digest[:])
}

func verifyOpenComputerClaim(ctx context.Context, api *ocAPIClient, leaseID, sandboxID string) (sandbox, error) {
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		return sandbox{}, err
	}
	if err := validateOpenComputerClaimScope(claim, api.baseURL); err != nil {
		return sandbox{}, err
	}
	sb, err := api.getSandboxWithTags(ctx, sandboxID)
	if err != nil {
		return sandbox{}, err
	}
	if err := validateOpenComputerSandboxOwnership(claim, sb); err != nil {
		return sandbox{}, err
	}
	return sb, nil
}

func validateOpenComputerSandboxOwnership(claim LeaseClaim, sb sandbox) error {
	if sb.Tags[openComputerClaimTagKey] != claim.ProviderScope {
		return exit(4, "opencomputer sandbox %q ownership tag does not match its local claim", sb.ID)
	}
	return nil
}

func newSandboxName(repo Repo) string {
	base := normalizeLeaseSlug(repo.Name)
	if base == "" {
		base = "crabbox"
	}
	base = strings.TrimPrefix(base, strings.TrimSuffix(namePrefix, "-")+"-")
	maxBase := maxSandboxNameLen - len(namePrefix) - 1 - sandboxNameSuffixLen
	if maxBase < 1 {
		maxBase = 1
	}
	if len(base) > maxBase {
		base = strings.Trim(base[:maxBase], "-")
	}
	if base == "" {
		base = "crabbox"
	}
	return namePrefix + base + "-" + randomSuffix()
}

func isReadyState(state string) bool {
	switch strings.TrimSpace(strings.ToLower(state)) {
	case "running", "ready", "started", "active":
		return true
	default:
		return false
	}
}

// isTerminalState reports whether a sandbox status will never transition to
// ready, so Status can fail fast instead of polling until a deadline.
func isTerminalState(state string) bool {
	switch strings.TrimSpace(strings.ToLower(state)) {
	case "terminated", "stopped", "failed", "error", "killed", "deleted":
		return true
	default:
		return false
	}
}

func randomSuffix() string {
	return shared.RandomSuffix()
}

// openComputerWorkdir returns the configured absolute workspace path inside the
// sandbox, validating that it isn't relative, empty, or a broad system path.
func openComputerWorkdir(cfg Config) (string, error) {
	workdir := strings.TrimSpace(cfg.OpenComputer.Workdir)
	if workdir == "" {
		workdir = defaultWorkdir
	}
	clean := path.Clean(workdir)
	if !strings.HasPrefix(clean, "/") {
		return "", exit(2, "opencomputer workdir %q must be an absolute path", workdir)
	}
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var", "/workspace":
		return "", exit(2, "opencomputer workdir %q is too broad; choose a dedicated subdirectory", clean)
	}
	return clean, nil
}

func (b *openComputerBackend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}

func (b *openComputerBackend) cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), b.cleanupTimeout())
}

func (b *openComputerBackend) cleanupTimeout() time.Duration {
	if b.cleanupTimeoutOverride > 0 {
		return b.cleanupTimeoutOverride
	}
	return openComputerCleanupTimeout
}

func (b *openComputerBackend) cleanupCreateFailure(ctx context.Context, api *ocAPIClient, sandboxID string, cause error) error {
	cleanupCtx, cancel := b.cleanupContext(ctx)
	defer cancel()
	if err := api.killSandbox(cleanupCtx, sandboxID); err != nil {
		if isOCNotFound(err) {
			return cause
		}
		return errors.Join(cause, fmt.Errorf("opencomputer cleanup failed for sandbox %s; delete it in the OpenComputer console: %w", sandboxID, err))
	}
	return cause
}

func (b *openComputerBackend) execTimeoutSecs() int {
	if b.cfg.OpenComputer.ExecTimeoutSecs > 0 {
		return b.cfg.OpenComputer.ExecTimeoutSecs
	}
	return openComputerExecTimeoutSecs
}
