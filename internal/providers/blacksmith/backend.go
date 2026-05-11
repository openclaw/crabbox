package blacksmith

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type ProviderSpec = core.ProviderSpec
type Runtime = core.Runtime
type Backend = core.Backend
type BlacksmithConfig = core.BlacksmithConfig
type WarmupRequest = core.WarmupRequest
type RunRequest = core.RunRequest
type RunResult = core.RunResult
type ListRequest = core.ListRequest
type LeaseView = core.LeaseView
type StatusRequest = core.StatusRequest
type StatusView = core.StatusView
type StopRequest = core.StopRequest
type Server = core.Server
type Repo = core.Repo
type ExitError = core.ExitError
type LocalCommandRequest = core.LocalCommandRequest
type LocalCommandResult = core.LocalCommandResult
type CommandRunner = core.CommandRunner
type timingReport = core.TimingReport
type timingPhase = core.TimingPhase

const targetLinux = core.TargetLinux

func RegisterBlacksmithProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return registerBlacksmithFlags(fs, defaults)
}

func ApplyBlacksmithProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if v, ok := values.(blacksmithFlagValues); ok {
		applyBlacksmithFlagOverrides(cfg, fs, v)
	}
	return nil
}

func NewBlacksmithBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = blacksmithTestboxProvider
	return &blacksmithBackend{spec: spec, cfg: cfg, rt: rt}
}

type blacksmithBackend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func (b *blacksmithBackend) Spec() ProviderSpec { return b.spec }

func (b *blacksmithBackend) Warmup(ctx context.Context, req WarmupRequest) error {
	if req.ActionsRunner {
		return exit(2, "--actions-runner is not supported for provider=%s; Blacksmith owns runner hydration", b.cfg.Provider)
	}
	started := b.rt.Clock.Now()
	leaseID, slug, err := b.warmupLease(ctx, req.Repo, req.Reclaim)
	if err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s idle_timeout=%s\n", leaseID, slug, blacksmithTestboxProvider, blacksmithIdleTimeout(b.cfg))
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: blacksmith warmup keeps the testbox until idle timeout or explicit stop\n")
	}
	fmt.Fprintf(b.rt.Stdout, "warmup complete total=%s\n", b.rt.Clock.Now().Sub(started).Round(time.Millisecond))
	if req.TimingJSON {
		total := b.rt.Clock.Now().Sub(started)
		if err := writeTimingJSON(b.rt.Stderr, timingReport{
			Provider: blacksmithTestboxProvider,
			LeaseID:  leaseID,
			Slug:     slug,
			TotalMs:  total.Milliseconds(),
			ExitCode: 0,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (b *blacksmithBackend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if err := rejectDelegatedSyncOptions(blacksmithTestboxProvider, req); err != nil {
		return RunResult{}, err
	}
	started := b.rt.Clock.Now()
	leaseID := req.ID
	slug := ""
	acquired := false
	var err error
	if leaseID == "" {
		leaseID, slug, err = b.warmupLease(ctx, req.Repo, req.Reclaim)
		if err != nil {
			return RunResult{}, err
		}
		acquired = true
	} else {
		leaseID, err = resolveBlacksmithLeaseID(leaseID, req.Repo.Root, req.Reclaim)
		if err != nil {
			return RunResult{}, err
		}
		slug, err = blacksmithClaimSlug(req.ID, leaseID)
		if err != nil {
			return RunResult{}, err
		}
		if err := claimLeaseForRepoProvider(leaseID, slug, blacksmithTestboxProvider, req.Repo.Root, blacksmithIdleTimeout(b.cfg), req.Reclaim); err != nil {
			return RunResult{}, err
		}
	}
	if acquired && !req.Keep {
		defer func() {
			if err := b.Stop(context.Background(), StopRequest{ID: leaseID}); err != nil {
				fmt.Fprintf(b.rt.Stderr, "warning: blacksmith stop failed for %s: %v\n", leaseID, err)
				return
			}
			removeLeaseClaim(leaseID)
			removeStoredTestboxKey(leaseID)
		}()
	}
	fmt.Fprintf(b.rt.Stderr, "provider=blacksmith-testbox id=%s sync=delegated auth=blacksmith\n", leaseID)
	commandStart := b.rt.Clock.Now()
	code := b.runTestbox(ctx, leaseID, req.Command, req.DebugSync, req.ShellMode)
	commandDuration := b.rt.Clock.Now().Sub(commandStart)
	total := b.rt.Clock.Now().Sub(started)
	fmt.Fprintf(b.rt.Stderr, "blacksmith run summary sync=delegated command=%s total=%s exit=%d\n", commandDuration.Round(time.Millisecond), total.Round(time.Millisecond), code)
	if req.TimingJSON {
		if err := writeTimingJSON(b.rt.Stderr, timingReport{
			Provider:      blacksmithTestboxProvider,
			LeaseID:       leaseID,
			Slug:          slug,
			SyncPhases:    []timingPhase{{Name: "delegated", Skipped: true, Reason: "blacksmith-testbox owns sync"}},
			SyncDelegated: true,
			CommandMs:     commandDuration.Milliseconds(),
			TotalMs:       total.Milliseconds(),
			ExitCode:      code,
		}); err != nil {
			return RunResult{}, err
		}
	}
	result := RunResult{ExitCode: code, Command: commandDuration, Total: total, SyncDelegated: true}
	if code != 0 {
		return result, ExitError{Code: code, Message: fmt.Sprintf("blacksmith testbox run exited %d", code)}
	}
	return result, nil
}

func (b *blacksmithBackend) List(ctx context.Context, req ListRequest) ([]Server, error) {
	out, err := b.commandOutput(ctx, b.listArgs(req))
	if err != nil {
		return nil, err
	}
	items := parseBlacksmithList(out)
	servers := make([]Server, 0, len(items))
	for _, item := range items {
		servers = append(servers, blacksmithItemToServer(item))
	}
	return servers, nil
}

func (b *blacksmithBackend) ListJSON(ctx context.Context, req ListRequest) (any, error) {
	out, err := b.commandOutput(ctx, b.listArgs(req))
	if err != nil {
		return nil, err
	}
	return parseBlacksmithList(out), nil
}

func (b *blacksmithBackend) listArgs(req ListRequest) []string {
	if req.All {
		return blacksmithListAllArgs(b.cfg)
	}
	return blacksmithListArgs(b.cfg)
}

func (b *blacksmithBackend) Status(ctx context.Context, req StatusRequest) (statusView, error) {
	leaseID, err := resolveBlacksmithLeaseID(req.ID, "", false)
	if err != nil {
		return statusView{}, err
	}
	deadline := b.rt.Clock.Now().Add(req.WaitTimeout)
	var lastState statusView
	for {
		state, err := b.blacksmithStatusView(ctx, leaseID)
		if err != nil {
			return statusView{}, err
		}
		lastState = state
		if !req.Wait || state.Ready {
			return state, nil
		}
		if b.rt.Clock.Now().After(deadline) {
			return statusView{}, exit(5, "%s", blacksmithWaitTimeoutMessage(req.ID, lastState.State))
		}
		time.Sleep(5 * time.Second)
	}
}

func (b *blacksmithBackend) Stop(ctx context.Context, req StopRequest) error {
	leaseID, err := resolveBlacksmithLeaseID(req.ID, "", false)
	if err != nil {
		return err
	}
	if _, err := b.runCommand(ctx, blacksmithStopArgs(b.cfg, leaseID), b.rt.Stdout, b.rt.Stderr); err != nil {
		return err
	}
	removeLeaseClaim(leaseID)
	removeStoredTestboxKey(leaseID)
	return nil
}

func (b *blacksmithBackend) warmupLease(ctx context.Context, repo Repo, reclaim bool) (string, string, error) {
	pendingID := "tbx_pending_" + strings.TrimPrefix(newLeaseID(), "cbx_")
	cleanupKeyID := pendingID
	defer func() {
		if cleanupKeyID != "" {
			removeStoredTestboxKey(cleanupKeyID)
		}
	}()
	_, publicKey, err := ensureTestboxKey(pendingID)
	if err != nil {
		return "", "", err
	}
	args, err := blacksmithWarmupArgs(b.cfg, publicKey)
	if err != nil {
		return "", "", err
	}
	beforeWarmup := b.listIDsBestEffort(ctx)
	result, err := b.runCommand(ctx, args, b.rt.Stdout, b.rt.Stderr)
	output := result.Stdout + result.Stderr
	if err != nil {
		b.cleanupFailedWarmup(ctx, beforeWarmup, output)
		return "", "", exit(result.ExitCode, "blacksmith testbox warmup failed: %v", err)
	}
	leaseID := parseBlacksmithID(output)
	if leaseID == "" {
		return "", "", exit(5, "blacksmith testbox warmup did not print a tbx_ id")
	}
	if err := moveStoredTestboxKey(pendingID, leaseID); err != nil {
		_ = b.Stop(ctx, StopRequest{ID: leaseID})
		return "", "", exit(2, "store blacksmith key for %s: %v", leaseID, err)
	}
	cleanupKeyID = leaseID
	slug := newLeaseSlug(leaseID)
	if err := claimLeaseForRepoProvider(leaseID, slug, blacksmithTestboxProvider, repo.Root, blacksmithIdleTimeout(b.cfg), reclaim); err != nil {
		_ = b.Stop(ctx, StopRequest{ID: leaseID})
		return "", "", err
	}
	cleanupKeyID = ""
	return leaseID, slug, nil
}

func (b *blacksmithBackend) runTestbox(ctx context.Context, leaseID string, command []string, debug, shellMode bool) int {
	keyPath, err := testboxKeyPath(leaseID)
	if err != nil {
		fmt.Fprintf(b.rt.Stderr, "blacksmith key path failed: %v\n", err)
		return 2
	}
	args := blacksmithRunArgs(b.cfg, leaseID, keyPath, command, debug || b.cfg.Blacksmith.Debug, shellMode)
	result, timedOut, err := b.runCommandWithSyncGuard(ctx, args, b.rt.Stdout, b.rt.Stderr)
	if timedOut {
		fmt.Fprintf(
			b.rt.Stderr,
			"Blacksmith Testbox sync did not print a completion marker for %s; terminating local runner. "+
				"Rerun with CRABBOX_BLACKSMITH_SYNC_TIMEOUT_MS=0 to disable this guard.\n",
			blacksmithSyncTimeout(os.Getenv),
		)
		return 124
	}
	if err != nil {
		return result.ExitCode
	}
	return 0
}

func (b *blacksmithBackend) commandOutput(ctx context.Context, args []string) (string, error) {
	result, err := b.runCommand(ctx, args, nil, nil)
	if err != nil {
		return "", ExitError{Code: result.ExitCode, Message: fmt.Sprintf("blacksmith failed: %v: %s", err, strings.TrimSpace(result.Stdout+result.Stderr))}
	}
	return result.Stdout + result.Stderr, nil
}

func (b *blacksmithBackend) runCommand(ctx context.Context, args []string, stdout, stderr io.Writer) (LocalCommandResult, error) {
	result, err := b.rt.Exec.Run(ctx, LocalCommandRequest{Name: "blacksmith", Args: args, Stdout: stdout, Stderr: stderr})
	if err != nil {
		return result, ExitError{Code: result.ExitCode, Message: fmt.Sprintf("blacksmith failed: %v", err)}
	}
	return result, nil
}

func (b *blacksmithBackend) runCommandWithSyncGuard(ctx context.Context, args []string, stdout, stderr io.Writer) (LocalCommandResult, bool, error) {
	timeout := blacksmithSyncTimeout(os.Getenv)
	if timeout <= 0 {
		result, err := b.runCommand(ctx, args, stdout, stderr)
		return result, false, err
	}
	guardCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	tracker := &blacksmithSyncTracker{}
	resultCh := make(chan struct {
		result LocalCommandResult
		err    error
	}, 1)
	go func() {
		result, err := b.runCommand(
			guardCtx,
			args,
			blacksmithSyncGuardWriter{w: stdout, tracker: tracker},
			blacksmithSyncGuardWriter{w: stderr, tracker: tracker},
		)
		resultCh <- struct {
			result LocalCommandResult
			err    error
		}{result: result, err: err}
	}()
	ticker := time.NewTicker(minBlacksmithDuration(timeout, time.Second))
	defer ticker.Stop()
	timedOut := false
	for {
		select {
		case result := <-resultCh:
			return result.result, timedOut, result.err
		case <-ticker.C:
			if !tracker.syncStalled(timeout, b.rt.Clock.Now()) {
				continue
			}
			timedOut = true
			cancel()
		}
	}
}

type blacksmithSyncTracker struct {
	mu           sync.Mutex
	syncingSince time.Time
	pending      string
}

func (t *blacksmithSyncTracker) observe(text string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pending += text
	if len(t.pending) > 4096 {
		t.pending = t.pending[len(t.pending)-4096:]
	}
	for {
		i := strings.IndexByte(t.pending, '\n')
		if i < 0 {
			break
		}
		t.observeLineLocked(t.pending[:i+1], now)
		t.pending = t.pending[i+1:]
	}
	if t.pending != "" {
		t.observeLineLocked(t.pending, now)
	}
}

func (t *blacksmithSyncTracker) observeLineLocked(line string, now time.Time) {
	if blacksmithSyncStartPattern.MatchString(line) {
		if t.syncingSince.IsZero() {
			t.syncingSince = now
		}
		return
	}
	if !t.syncingSince.IsZero() && blacksmithSyncDonePattern.MatchString(line) {
		t.syncingSince = time.Time{}
	}
}

func (t *blacksmithSyncTracker) syncStalled(timeout time.Duration, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.syncingSince.IsZero() && now.Sub(t.syncingSince) >= timeout
}

type blacksmithSyncGuardWriter struct {
	w       io.Writer
	tracker *blacksmithSyncTracker
}

func (w blacksmithSyncGuardWriter) Write(chunk []byte) (int, error) {
	if w.tracker != nil {
		w.tracker.observe(string(chunk), time.Now())
	}
	if w.w == nil {
		return len(chunk), nil
	}
	return w.w.Write(chunk)
}

func minBlacksmithDuration(left, right time.Duration) time.Duration {
	if left < right {
		return left
	}
	return right
}

func (b *blacksmithBackend) listIDsBestEffort(ctx context.Context) map[string]bool {
	out, err := b.commandOutput(ctx, blacksmithListAllArgs(b.cfg))
	if err != nil {
		return map[string]bool{}
	}
	ids := map[string]bool{}
	for _, item := range parseBlacksmithList(out) {
		ids[item.ID] = true
	}
	return ids
}

func (b *blacksmithBackend) cleanupFailedWarmup(ctx context.Context, before map[string]bool, output string) {
	if leaseID := parseBlacksmithID(output); leaseID != "" {
		if err := b.Stop(ctx, StopRequest{ID: leaseID}); err == nil {
			before[leaseID] = true
		}
	}
	stoppedAny := false
	quietAttempts := 0
	for attempt := 0; attempt < blacksmithCleanupAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(blacksmithCleanupDelay):
			}
		}
		list, err := b.commandOutput(ctx, blacksmithListAllArgs(b.cfg))
		if err != nil {
			return
		}
		stopped := false
		for _, item := range parseBlacksmithList(list) {
			if before[item.ID] || !blacksmithMatchesConfig(item, b.cfg) {
				continue
			}
			_ = b.Stop(ctx, StopRequest{ID: item.ID})
			before[item.ID] = true
			stopped = true
		}
		if stopped {
			stoppedAny = true
			quietAttempts = 0
			continue
		}
		if stoppedAny {
			quietAttempts++
			if quietAttempts >= blacksmithCleanupQuiet {
				return
			}
		}
	}
}

func (b *blacksmithBackend) blacksmithStatusView(ctx context.Context, leaseID string) (statusView, error) {
	out, err := b.commandOutput(ctx, blacksmithListAllArgs(b.cfg))
	if err != nil {
		return statusView{}, err
	}
	for _, item := range parseBlacksmithList(out) {
		if item.ID != leaseID {
			continue
		}
		server := blacksmithItemToServer(item)
		return statusView{
			ID:          item.ID,
			Provider:    blacksmithTestboxProvider,
			TargetOS:    targetLinux,
			State:       item.Status,
			ServerID:    item.ID,
			ServerType:  "testbox",
			Labels:      server.Labels,
			HasHost:     false,
			Ready:       strings.EqualFold(item.Status, "ready") || strings.EqualFold(item.Status, "running"),
			IdleTimeout: blacksmithIdleTimeout(b.cfg).String(),
		}, nil
	}
	return statusView{}, exit(4, "blacksmith testbox not found: %s", leaseID)
}

func blacksmithItemToServer(item blacksmithListItem) Server {
	labels := map[string]string{
		"lease":    item.ID,
		"provider": blacksmithTestboxProvider,
		"state":    item.Status,
		"repo":     item.Repo,
		"workflow": item.Workflow,
		"job":      item.Job,
		"ref":      item.Ref,
		"created":  item.Created,
	}
	server := Server{
		CloudID:  item.ID,
		Provider: blacksmithTestboxProvider,
		Name:     item.ID,
		Status:   item.Status,
		Labels:   labels,
	}
	server.ServerType.Name = "testbox"
	return server
}

func blacksmithWaitTimeoutMessage(identifier, state string) string {
	state = strings.TrimSpace(state)
	if strings.EqualFold(state, "queued") {
		return fmt.Sprintf("timed out waiting for %s to become ready (last state queued; Blacksmith queue may be stalled, so stop queued ids you created or use another provider)", identifier)
	}
	if state != "" {
		return fmt.Sprintf("timed out waiting for %s to become ready (last state %s)", identifier, state)
	}
	return fmt.Sprintf("timed out waiting for %s to become ready", identifier)
}

type statusView = core.StatusView

func rejectDelegatedSyncOptions(provider string, req RunRequest) error {
	return core.RejectDelegatedSyncOptions(provider, req)
}

func writeTimingJSON(w io.Writer, report timingReport) error {
	return core.WriteTimingJSON(w, report)
}

func newLeaseID() string {
	return core.NewLeaseID()
}

func newLeaseSlug(leaseID string) string {
	return core.NewLeaseSlug(leaseID)
}

func claimLeaseForRepoProvider(leaseID, slug, provider, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return core.ClaimLeaseForRepoProvider(leaseID, slug, provider, repoRoot, idleTimeout, reclaim)
}

func removeLeaseClaim(leaseID string) {
	core.RemoveLeaseClaim(leaseID)
}

func ensureTestboxKey(leaseID string) (string, string, error) {
	return core.EnsureTestboxKey(leaseID)
}

func moveStoredTestboxKey(oldLeaseID, newLeaseID string) error {
	return core.MoveStoredTestboxKey(oldLeaseID, newLeaseID)
}

func removeStoredTestboxKey(leaseID string) {
	core.RemoveStoredTestboxKey(leaseID)
}

func testboxKeyPath(leaseID string) (string, error) {
	return core.TestboxKeyPath(leaseID)
}

func baseConfig() Config {
	return core.BaseConfig()
}

func readLeaseClaim(leaseID string) (core.LeaseClaim, error) {
	return core.ReadLeaseClaim(leaseID)
}
