package opencomputer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
)

const openComputerCleanupTimeout = 15 * time.Second

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
	started := b.now()
	api, err := newOCAPIClient(b.cfg, b.rt)
	if err != nil {
		return RunResult{}, err
	}
	leaseID, sandboxID, slug := "", "", ""
	acquired := false
	if req.ID == "" {
		leaseID, sandboxID, slug, err = b.createSandbox(ctx, api, req.Repo, req.Reclaim, req.RequestedSlug)
		if err != nil {
			return RunResult{}, err
		}
		fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s sandbox=%s\n", leaseID, slug, providerName, sandboxID)
		acquired = true
	} else {
		leaseID, sandboxID, slug, err = resolveLeaseID(req.ID, req.Repo.Root, req.Reclaim, b.cfg.IdleTimeout)
		if err != nil {
			return RunResult{}, err
		}
	}
	shouldStop := acquired && !req.Keep
	if shouldStop {
		defer func() {
			if !shouldStop {
				return
			}
			cleanupCtx, cancel := b.cleanupContext(ctx)
			defer cancel()
			if killErr := api.killSandbox(cleanupCtx, sandboxID); killErr != nil {
				fmt.Fprintf(b.rt.Stderr, "warning: opencomputer kill failed for %s: %v\n", sandboxID, killErr)
				return
			}
			removeLeaseClaim(leaseID)
		}()
	}
	fmt.Fprintf(b.rt.Stderr, "provider=%s lease=%s sandbox=%s workdir=%s\n", providerName, leaseID, sandboxID, workdir)

	syncDuration := time.Duration(0)
	syncPhases := []timingPhase{{Name: "sync", Skipped: true, Reason: "--no-sync"}}
	if !req.NoSync {
		syncPhases, syncDuration, err = b.syncWorkspace(ctx, api, sandboxID, req, workdir)
		if err != nil {
			return RunResult{Total: b.now().Sub(started), SyncDelegated: true}, err
		}
		fmt.Fprintf(b.rt.Stderr, "sync complete in %s\n", syncDuration.Round(time.Millisecond))
	} else if err := b.prepareWorkspace(ctx, api, sandboxID, workdir); err != nil {
		return RunResult{}, err
	}

	if req.SyncOnly {
		result := RunResult{Total: b.now().Sub(started), SyncDelegated: true}
		fmt.Fprintf(b.rt.Stdout, "synced %s\n", workdir)
		if req.TimingJSON {
			return result, writeTimingJSON(b.rt.Stderr, timingReport{
				Provider:      providerName,
				LeaseID:       leaseID,
				Slug:          slug,
				SyncDelegated: true,
				SyncMs:        syncDuration.Milliseconds(),
				SyncPhases:    syncPhases,
				SyncSkipped:   req.NoSync,
				TotalMs:       result.Total.Milliseconds(),
				ExitCode:      0,
				Label:         strings.TrimSpace(req.Label),
			})
		}
		return result, nil
	}

	command, err := buildCommand(req.Command, req.ShellMode)
	if err != nil {
		return RunResult{}, err
	}
	commandStart := b.now()
	// Env travels in the exec request body (`envs`), never argv. cwd is the
	// synced workspace.
	exitCode, runErr := b.execCommand(ctx, api, sandboxID, workdir, command, req.Env)
	commandDuration := b.now().Sub(commandStart)
	result := RunResult{
		ExitCode:      exitCode,
		Command:       commandDuration,
		Total:         b.now().Sub(started),
		SyncDelegated: true,
	}
	if req.NoSync {
		fmt.Fprintf(b.rt.Stderr, "opencomputer run summary sync_skipped=true command=%s total=%s exit=%d\n",
			result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), exitCode)
	} else {
		fmt.Fprintf(b.rt.Stderr, "opencomputer run summary sync=%s command=%s total=%s exit=%d\n",
			syncDuration.Round(time.Millisecond), result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), exitCode)
	}
	if req.TimingJSON {
		if err := writeTimingJSON(b.rt.Stderr, timingReport{
			Provider:      providerName,
			LeaseID:       leaseID,
			Slug:          slug,
			SyncDelegated: true,
			SyncMs:        syncDuration.Milliseconds(),
			SyncPhases:    syncPhases,
			SyncSkipped:   req.NoSync,
			CommandMs:     result.Command.Milliseconds(),
			TotalMs:       result.Total.Milliseconds(),
			ExitCode:      exitCode,
			Label:         strings.TrimSpace(req.Label),
		}); err != nil {
			return result, err
		}
	}
	if runErr != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return result, ExitError{Code: 1, Message: fmt.Sprintf("opencomputer run failed: %v", runErr)}
	}
	if exitCode != 0 {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return result, ExitError{Code: exitCode, Message: fmt.Sprintf("opencomputer run exited %d", exitCode)}
	}
	return result, nil
}

func (b *openComputerBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	api, err := newOCAPIClient(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	sandboxes, err := api.listSandboxes(ctx)
	if err != nil {
		return nil, err
	}
	servers := make([]Server, 0, len(sandboxes))
	for _, sb := range sandboxes {
		if sb.ID == "" {
			continue
		}
		leaseID := leasePrefix + sb.ID
		claim, ok, err := resolveLeaseClaim(leaseID)
		if err != nil || !ok || claim.Provider != providerName {
			continue
		}
		state := blank(sb.Status, statusViewReady)
		servers = append(servers, Server{
			Provider: providerName,
			CloudID:  sb.ID,
			Name:     sb.ID,
			Status:   state,
			Labels: map[string]string{
				"provider": providerName,
				"lease":    claim.LeaseID,
				"slug":     claim.Slug,
				"target":   targetLinux,
				"state":    state,
			},
		})
	}
	return servers, nil
}

func (b *openComputerBackend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
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
	leaseID, sandboxID, slug, err := resolveLeaseID(req.ID, "", false, 0)
	if err != nil {
		return StatusView{}, err
	}
	deadline := b.now().Add(req.WaitTimeout)
	if req.WaitTimeout <= 0 {
		deadline = b.now().Add(5 * time.Minute)
	}
	for {
		sb, getErr := api.getSandbox(ctx, sandboxID)
		if getErr != nil {
			// Surface real API failures (auth, 5xx, sandbox gone) instead of
			// masking them as a not-ready status.
			return StatusView{}, getErr
		}
		state := strings.ToLower(strings.TrimSpace(sb.Status))
		view := StatusView{
			ID:       leaseID,
			Slug:     slug,
			Provider: providerName,
			TargetOS: targetLinux,
			State:    state,
			ServerID: sandboxID,
			Network:  NetworkPublic,
			Ready:    isReadyState(state),
			Labels: map[string]string{
				"provider": providerName,
				"lease":    leaseID,
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
		case <-ctx.Done():
			return StatusView{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *openComputerBackend) Stop(ctx context.Context, req StopRequest) error {
	api, err := newOCAPIClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, sandboxID, _, err := resolveLeaseID(req.ID, "", false, 0)
	if err != nil {
		return err
	}
	if err := api.killSandbox(ctx, sandboxID); err != nil {
		return err
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
		Cmd:  command[0],
		Args: command[1:],
		Envs: env,
		Cwd:  workdir,
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
	req := createSandboxRequest{
		Timeout:  b.cfg.OpenComputer.TimeoutSecs,
		Metadata: map[string]string{"crabbox": "true", "crabbox-name": newSandboxName(repo)},
	}
	// Only send sizing when both are set; CPU/memory must form an allowed tier,
	// so an unset sizing falls back to the service default rather than risking
	// an invalid 0/0 combination.
	if b.cfg.OpenComputer.CPU > 0 && b.cfg.OpenComputer.MemoryMB > 0 {
		req.CPUCount = b.cfg.OpenComputer.CPU
		req.MemoryMB = b.cfg.OpenComputer.MemoryMB
	}
	sb, err := api.createSandbox(ctx, req)
	if err != nil {
		return "", "", "", err
	}
	leaseID := leasePrefix + sb.ID
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		cleanupCtx, cancel := b.cleanupContext(ctx)
		defer cancel()
		_ = api.killSandbox(cleanupCtx, sb.ID)
		return "", "", "", err
	}
	if err := claimLeaseForRepoProviderPond(leaseID, slug, providerName, b.cfg.Pond, repo.Root, b.cfg.IdleTimeout, reclaim); err != nil {
		cleanupCtx, cancel := b.cleanupContext(ctx)
		defer cancel()
		_ = api.killSandbox(cleanupCtx, sb.ID)
		return "", "", "", err
	}
	return leaseID, sb.ID, slug, nil
}

// resolveLeaseID resolves a user-supplied identifier (slug, lease ID, or raw
// OpenComputer sandbox ID) to a (leaseID, sandboxID, slug) tuple. Resolution is
// strict: only locally-claimed Crabbox sandboxes are accepted, mirroring islo
// and tensorlake. Raw IDs are accepted only when a matching `ocbx_<id>` claim
// exists.
func resolveLeaseID(id, repoRoot string, reclaim bool, idleTimeout time.Duration) (string, string, string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", "", "", exit(2, "provider=opencomputer requires a Crabbox-created sandbox slug or lease id")
	}
	probes := []string{id}
	if !strings.HasPrefix(id, leasePrefix) {
		probes = append(probes, leasePrefix+id)
	}
	for _, probe := range probes {
		claim, ok, err := resolveLeaseClaimForProvider(probe, providerName)
		if err != nil {
			return "", "", "", err
		}
		if !ok {
			continue
		}
		if repoRoot != "" {
			if err := claimLeaseForRepoProvider(claim.LeaseID, claim.Slug, providerName, repoRoot,
				timeoutOrDefault(idleTimeout, time.Duration(claim.IdleTimeoutSeconds)*time.Second), reclaim); err != nil {
				return "", "", "", err
			}
		}
		slug := claim.Slug
		if strings.TrimSpace(slug) == "" {
			slug = newLeaseSlug(claim.LeaseID)
		}
		return claim.LeaseID, strings.TrimPrefix(claim.LeaseID, leasePrefix), slug, nil
	}
	return "", "", "", exit(4, "opencomputer sandbox %q is not claimed by Crabbox; use a Crabbox slug or %s<sandbox-id>", id, leasePrefix)
}

func timeoutOrDefault(primary, fallback time.Duration) time.Duration {
	if primary > 0 {
		return primary
	}
	return fallback
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
	case "terminated", "stopped", "failed", "killed", "deleted":
		return true
	default:
		return false
	}
}

func randomSuffix() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())[:6]
	}
	return hex.EncodeToString(b[:])
}

func buildCommand(command []string, shellMode bool) ([]string, error) {
	if len(command) == 0 {
		return nil, errors.New("missing command")
	}
	if shellMode {
		return []string{"bash", "-lc", strings.Join(command, " ")}, nil
	}
	if shouldUseShell(command) || leadingEnvAssignment(command) {
		return []string{"bash", "-lc", shellScriptFromArgv(command)}, nil
	}
	return command, nil
}

func leadingEnvAssignment(command []string) bool {
	return len(command) > 1 && strings.Contains(command[0], "=") && !strings.HasPrefix(command[0], "-")
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
	timeout := openComputerCleanupTimeout
	if b.cleanupTimeoutOverride > 0 {
		timeout = b.cleanupTimeoutOverride
	}
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}
