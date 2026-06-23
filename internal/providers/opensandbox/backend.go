package opensandbox

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"syscall"
	"time"

	sdk "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
)

type openSandboxBackend struct {
	spec                   ProviderSpec
	cfg                    Config
	rt                     Runtime
	newClient              func(Config, Runtime) (openSandboxClient, error)
	cleanupTimeoutOverride time.Duration
	reconcilePollOverride  time.Duration
	statusPollOverride     time.Duration
	statusProbeOverride    time.Duration
}

func (b *openSandboxBackend) Spec() ProviderSpec { return b.spec }

func (b *openSandboxBackend) client() (openSandboxClient, error) {
	if b.newClient != nil {
		return b.newClient(b.cfg, b.rt)
	}
	return newOpenSandboxClient(b.cfg, b.rt)
}

func (b *openSandboxBackend) Warmup(ctx context.Context, req WarmupRequest) error {
	if req.ActionsRunner {
		return exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	if req.Options.Tailscale.Enabled {
		return exit(2, "provider=opensandbox is delegated-run only and does not support Tailscale options")
	}
	if err := validateOpenSandboxRunConfig(b.cfg); err != nil {
		return err
	}
	if _, err := openSandboxWorkdir(b.cfg); err != nil {
		return err
	}
	started := b.now()
	api, err := b.client()
	if err != nil {
		return err
	}
	leaseID, sandboxID, slug, sb, unlockOperation, err := b.createSandbox(ctx, api, req.Repo, req.Reclaim, req.RequestedSlug)
	if err != nil {
		return err
	}
	defer unlockOperation()
	if sb.ExpiresAt == nil || sb.ExpiresAt.IsZero() {
		sb, err = verifyOpenSandboxClaim(ctx, api, leaseID, sandboxID)
		if err != nil {
			return b.cleanupClaimedSandboxFailure(ctx, api, leaseID, sandboxID, err)
		}
	}
	deadline, err := openSandboxExpiration(sb)
	if err != nil {
		return b.cleanupClaimedSandboxFailure(ctx, api, leaseID, sandboxID, err)
	}
	required := openSandboxRunBudgetForConfig(b.cfg, false, false)
	if remaining := deadline.Sub(b.now()); remaining < required {
		return b.cleanupClaimedSandboxFailure(ctx, api, leaseID, sandboxID,
			exit(5, "opensandbox sandbox %s has %s remaining after warmup, less than the %s default run budget", sandboxID, remaining.Round(time.Second), required))
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s sandbox=%s\n", leaseID, slug, providerName, sandboxID)
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: opensandbox warmup keeps the sandbox until explicit stop\n")
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

func (b *openSandboxBackend) Run(ctx context.Context, req RunRequest) (result RunResult, retErr error) {
	if req.Options.Tailscale.Enabled {
		return RunResult{}, exit(2, "provider=opensandbox is delegated-run only and does not support Tailscale options")
	}
	if req.ID == "" {
		if err := validateOpenSandboxRequestConfig(b.cfg, req); err != nil {
			return RunResult{}, err
		}
	}
	workdir, err := openSandboxWorkdir(b.cfg)
	if err != nil {
		return RunResult{}, err
	}
	started := b.now()
	api, err := b.client()
	if err != nil {
		return RunResult{}, err
	}
	leaseID, sandboxID, slug := "", "", ""
	sb := sandboxInfo{}
	deadline := time.Time{}
	acquired := false
	shouldStop := false
	cleanedUp := false
	var session *RunSessionHandle
	finishResult := func(result RunResult) RunResult {
		if session == nil {
			return result
		}
		if result.Provider == "" {
			result.Provider = providerName
		}
		if result.LeaseID == "" {
			result.LeaseID = leaseID
		}
		if result.Slug == "" {
			result.Slug = slug
		}
		result.Session = session
		if slug != "" {
			result.Session.Slug = slug
		}
		result.Session.Kept = !cleanedUp && !shouldStop
		return result
	}
	defer func() {
		result = finishResult(result)
	}()
	var unlockOperation func()
	defer func() {
		if unlockOperation != nil {
			unlockOperation()
		}
	}()
	if req.ID == "" {
		leaseID, sandboxID, slug, sb, unlockOperation, err = b.createSandbox(ctx, api, req.Repo, req.Reclaim, req.RequestedSlug)
		if err != nil {
			return RunResult{}, err
		}
		fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s sandbox=%s\n", leaseID, slug, providerName, sandboxID)
		acquired = true
	} else {
		leaseID, sandboxID, _, err = resolveLeaseID(req.ID, "", false, 0, api.BaseURL())
		if err != nil {
			return RunResult{}, err
		}
		unlockOperation, err = lockOpenSandboxLeaseOperation(ctx, leaseID)
		if err != nil {
			return RunResult{}, err
		}
		leaseID, sandboxID, _, err = resolveLeaseID(leaseID, "", false, 0, api.BaseURL())
		if err != nil {
			return RunResult{}, err
		}
		sb, err = verifyOpenSandboxClaim(ctx, api, leaseID, sandboxID)
		if err != nil {
			return RunResult{}, err
		}
		claim, err := readLeaseClaim(leaseID)
		if err != nil {
			return RunResult{}, err
		}
		if err := authorizeOpenSandboxRepoClaim(claim, req.Repo.Root, req.Reclaim); err != nil {
			return RunResult{}, err
		}
		slug = blank(claim.Slug, newLeaseSlug(leaseID))
		session = &RunSessionHandle{
			Provider:       providerName,
			LeaseID:        leaseID,
			Slug:           slug,
			Reused:         true,
			Kept:           true,
			CleanupCommand: openSandboxCleanupCommand(leaseID),
		}
		if sb.ExpiresAt == nil || sb.ExpiresAt.IsZero() {
			sb, err = verifyOpenSandboxClaim(ctx, api, leaseID, sandboxID)
			if err != nil {
				return RunResult{}, err
			}
		}
		deadline, err = openSandboxExpiration(sb)
		if err != nil {
			return RunResult{}, err
		}
		if !deadline.After(b.now()) {
			return RunResult{}, exit(5, "opensandbox sandbox %s exceeded its absolute Crabbox TTL", sandboxID)
		}
		if remaining, required := deadline.Sub(b.now()), b.runLifetimeBudget(req); remaining < required {
			runErr := exit(5, "opensandbox sandbox %s has %s remaining before its absolute TTL, less than the %s sync/command budget; create a new sandbox", sandboxID, remaining.Round(time.Second), required)
			return RunResult{Total: b.now().Sub(started), SyncDelegated: true}, runErr
		}
		if err := b.ensureReusableSandbox(ctx, api, sandboxID, sb); err != nil {
			return RunResult{}, err
		}
		if !deadline.After(b.now()) {
			return RunResult{}, exit(5, "opensandbox sandbox %s exceeded its absolute Crabbox TTL while resuming", sandboxID)
		}
		if remaining, required := deadline.Sub(b.now()), b.runLifetimeBudget(req); remaining < required {
			runErr := exit(5, "opensandbox sandbox %s has %s remaining after resume before its absolute TTL, less than the %s sync/command budget; create a new sandbox", sandboxID, remaining.Round(time.Second), required)
			return RunResult{Total: b.now().Sub(started), SyncDelegated: true}, runErr
		}
		_, _, slug, err = finishResolvedLease(claim, req.Repo.Root, req.Reclaim, b.cfg.IdleTimeout, api.BaseURL())
		if err != nil {
			return RunResult{}, err
		}
		if session != nil {
			session.Slug = slug
		}
	}
	shouldStop = acquired && !req.Keep
	if session == nil {
		session = &RunSessionHandle{
			Provider:       providerName,
			LeaseID:        leaseID,
			Slug:           slug,
			Reused:         false,
			Kept:           !shouldStop,
			CleanupCommand: openSandboxCleanupCommand(leaseID),
		}
	}
	cleanupCreated := func() error {
		cleanupPending := shouldStop
		err := b.cleanupCreatedRun(ctx, api, leaseID, sandboxID, &shouldStop)
		if cleanupPending && err == nil {
			cleanedUp = true
		}
		return err
	}
	if shouldStop {
		defer func() {
			if cleanupErr := cleanupCreated(); cleanupErr != nil {
				fmt.Fprintf(b.rt.Stderr, "warning: %v\n", cleanupErr)
			}
		}()
	}
	if acquired {
		if sb.ExpiresAt == nil || sb.ExpiresAt.IsZero() {
			sb, err = verifyOpenSandboxClaim(ctx, api, leaseID, sandboxID)
			if err != nil {
				return RunResult{}, err
			}
		}
		deadline, err = openSandboxExpiration(sb)
		if err != nil {
			return RunResult{}, err
		}
		if !deadline.After(b.now()) {
			return RunResult{}, exit(5, "opensandbox sandbox %s exceeded its absolute Crabbox TTL", sandboxID)
		}
		if remaining, required := deadline.Sub(b.now()), b.runLifetimeBudget(req); remaining < required {
			runErr := exit(5, "opensandbox sandbox %s has %s remaining before its absolute TTL, less than the %s sync/command budget; create a new sandbox", sandboxID, remaining.Round(time.Second), required)
			handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
			return RunResult{Total: b.now().Sub(started), SyncDelegated: true}, runErr
		}
	}
	fmt.Fprintf(b.rt.Stderr, "provider=%s lease=%s sandbox=%s workdir=%s\n", providerName, leaseID, sandboxID, workdir)

	syncDuration := time.Duration(0)
	syncPhases := []timingPhase{{Name: "sync", Skipped: true, Reason: "--no-sync"}}
	if !req.NoSync {
		syncPhases, syncDuration, err = b.syncWorkspace(ctx, api, sandboxID, req, workdir)
		if err != nil {
			handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
			b.warnOpenSandboxActivityRefresh(leaseID, shouldStop)
			return RunResult{Total: b.now().Sub(started), SyncDelegated: true}, err
		}
		fmt.Fprintf(b.rt.Stderr, "sync complete in %s\n", syncDuration.Round(time.Millisecond))
	} else if err := b.ensureWorkspace(ctx, api, sandboxID, workdir); err != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		b.warnOpenSandboxActivityRefresh(leaseID, shouldStop)
		return RunResult{}, err
	}

	if req.SyncOnly {
		result := RunResult{Total: b.now().Sub(started), SyncDelegated: true}
		fmt.Fprintf(b.rt.Stdout, "synced %s\n", workdir)
		activityErr := b.refreshOpenSandboxActivityIfRetained(leaseID, shouldStop)
		if activityErr != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: refresh opensandbox lease activity failed lease=%s: %v\n", leaseID, activityErr)
			result.ExitCode = 1
		}
		if req.TimingJSON {
			if cleanupErr := cleanupCreated(); cleanupErr != nil {
				fmt.Fprintf(b.rt.Stderr, "warning: %v\n", cleanupErr)
			}
			report := timingReportWithRunResult(timingReport{
				Provider:      providerName,
				LeaseID:       leaseID,
				Slug:          slug,
				SyncDelegated: true,
				SyncMs:        syncDuration.Milliseconds(),
				SyncPhases:    syncPhases,
				SyncSkipped:   req.NoSync,
				TotalMs:       result.Total.Milliseconds(),
				ExitCode:      result.ExitCode,
				Label:         strings.TrimSpace(req.Label),
			}, result, activityErr)
			if activityErr != nil {
				report = timingReportWithProviderError(report)
			}
			if err := writeTimingJSON(b.rt.Stderr, report); err != nil {
				return result, err
			}
		}
		if activityErr != nil {
			return result, activityErr
		}
		return result, nil
	}

	command, err := buildCommand(req.Command, req.ShellMode)
	if err != nil {
		b.warnOpenSandboxActivityRefresh(leaseID, shouldStop)
		return RunResult{}, err
	}
	if req.EnvSummary || strings.TrimSpace(os.Getenv("CRABBOX_ENV_ALLOW")) != "" {
		printEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
	}
	if remaining := deadline.Sub(b.now()); remaining < b.commandLifetime() {
		runErr := exit(5, "opensandbox sandbox %s has %s remaining before its absolute TTL, less than the %s command budget; create a new sandbox", sandboxID, remaining.Round(time.Second), b.commandLifetime())
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		b.warnOpenSandboxActivityRefresh(leaseID, shouldStop)
		return RunResult{Total: b.now().Sub(started), SyncDelegated: true}, runErr
	}
	commandStart := b.now()
	exitCode, runErr := api.RunCommand(ctx, sandboxID, runCommandRequest{
		Command:     commandScript(command),
		Workdir:     workdir,
		Env:         req.Env,
		TimeoutSecs: b.execTimeoutSecs(),
	})
	commandDuration := b.now().Sub(commandStart)
	result = RunResult{
		ExitCode:      exitCode,
		Command:       commandDuration,
		Total:         b.now().Sub(started),
		SyncDelegated: true,
	}
	fmt.Fprintf(b.rt.Stderr, "opensandbox run summary sync=%s command=%s total=%s exit=%d\n",
		syncDuration.Round(time.Millisecond), result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), exitCode)
	var commandErr error
	if runErr != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		if result.ExitCode == 0 {
			result.ExitCode = 1
		}
		commandErr = ExitError{Code: 1, Message: fmt.Sprintf("opensandbox run failed: %v", runErr)}
	} else if exitCode != 0 {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		commandErr = ExitError{Code: exitCode, Message: fmt.Sprintf("opensandbox run exited %d", exitCode)}
	}
	activityErr := b.refreshOpenSandboxActivityIfRetained(leaseID, shouldStop)
	if activityErr != nil {
		fmt.Fprintf(b.rt.Stderr, "warning: refresh opensandbox lease activity failed lease=%s: %v\n", leaseID, activityErr)
		if commandErr == nil {
			result.ExitCode = 1
		}
	}
	if req.TimingJSON {
		if cleanupErr := cleanupCreated(); cleanupErr != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: %v\n", cleanupErr)
		}
		timingErr := commandErr
		if timingErr == nil {
			timingErr = activityErr
		}
		report := timingReportWithRunResult(timingReport{
			Provider:      providerName,
			LeaseID:       leaseID,
			Slug:          slug,
			SyncDelegated: true,
			SyncMs:        syncDuration.Milliseconds(),
			SyncPhases:    syncPhases,
			SyncSkipped:   req.NoSync,
			CommandMs:     result.Command.Milliseconds(),
			TotalMs:       result.Total.Milliseconds(),
			ExitCode:      result.ExitCode,
			Label:         strings.TrimSpace(req.Label),
		}, result, timingErr)
		if commandErr == nil && activityErr != nil {
			report = timingReportWithProviderError(report)
		}
		if err := writeTimingJSON(b.rt.Stderr, report); err != nil {
			return result, err
		}
	}
	if commandErr != nil {
		return result, commandErr
	}
	if activityErr != nil {
		return result, activityErr
	}
	return result, nil
}

func (b *openSandboxBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	api, err := b.client()
	if err != nil {
		return nil, err
	}
	claims, err := listOpenSandboxLeaseClaims()
	if err != nil {
		return nil, err
	}
	servers := make([]Server, 0, len(claims))
	for _, claim := range claims {
		if claim.Provider != providerName || !strings.HasPrefix(claim.LeaseID, leasePrefix) {
			continue
		}
		if validateOpenSandboxClaimScope(claim, api.BaseURL()) != nil {
			continue
		}
		sandboxID := strings.TrimPrefix(claim.LeaseID, leasePrefix)
		if sandboxID == "" {
			continue
		}
		sb, getErr := api.GetSandbox(ctx, sandboxID)
		state := ""
		if getErr != nil {
			if isOpenSandboxNotFound(getErr) {
				state = "missing-or-inaccessible"
			} else {
				return nil, getErr
			}
		} else {
			if err := validateOpenSandboxOwnership(claim, sb); err != nil {
				return nil, err
			}
			state = blank(strings.ToLower(sb.State), statusViewReady)
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

func (b *openSandboxBackend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	api, err := b.client()
	if err != nil {
		return DoctorResult{}, err
	}
	if err := api.Probe(ctx); err != nil {
		return DoctorResult{}, err
	}
	servers, err := b.List(ctx, ListRequest{})
	if err != nil {
		return DoctorResult{}, err
	}
	return inventoryDoctorResult(providerName, len(servers)), nil
}

func (b *openSandboxBackend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	api, err := b.client()
	if err != nil {
		return StatusView{}, err
	}
	leaseID, sandboxID, slug, err := resolveLeaseID(req.ID, "", false, 0, api.BaseURL())
	if err != nil {
		return StatusView{}, err
	}
	claim, ok, err := resolveOpenSandboxLeaseClaim(leaseID, api.BaseURL())
	if err != nil {
		return StatusView{}, err
	}
	if !ok {
		return StatusView{}, exit(4, "opensandbox sandbox %q is not claimed by Crabbox", req.ID)
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
		sb, getErr := api.GetSandbox(pollCtx, sandboxID)
		if getErr != nil {
			if req.Wait && errors.Is(pollCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				return StatusView{}, exit(5, "timed out waiting for opensandbox sandbox %s to become ready", sandboxID)
			}
			if ctx.Err() != nil {
				return StatusView{}, ctx.Err()
			}
			return StatusView{}, getErr
		}
		if err := validateOpenSandboxOwnership(claim, sb); err != nil {
			return StatusView{}, err
		}
		state := strings.ToLower(strings.TrimSpace(sb.State))
		ready := false
		if isReadyState(state) {
			probeCtx, probeCancel := context.WithTimeout(pollCtx, b.statusProbeTimeout())
			pingErr := api.PingSandbox(probeCtx, sandboxID)
			probeCancel()
			ready = pingErr == nil
			if pingErr != nil && pollCtx.Err() != nil {
				if errors.Is(pollCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
					return StatusView{}, exit(5, "timed out waiting for opensandbox sandbox %s to become ready", sandboxID)
				}
				return StatusView{}, pollCtx.Err()
			}
			if pingErr != nil && !isOpenSandboxReadinessPending(pingErr) {
				return StatusView{}, fmt.Errorf("opensandbox status execd health: %w", pingErr)
			}
		}
		view := StatusView{
			ID:       leaseID,
			Slug:     slug,
			Provider: providerName,
			TargetOS: targetLinux,
			State:    state,
			ServerID: sandboxID,
			Pond:     claim.Pond,
			Network:  NetworkPublic,
			Ready:    ready,
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
			return StatusView{}, exit(5, "opensandbox sandbox %s entered terminal state %q before becoming ready", sandboxID, state)
		}
		if b.now().After(deadline) {
			return StatusView{}, exit(5, "timed out waiting for opensandbox sandbox %s to become ready", sandboxID)
		}
		select {
		case <-pollCtx.Done():
			if errors.Is(pollCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				return StatusView{}, exit(5, "timed out waiting for opensandbox sandbox %s to become ready", sandboxID)
			}
			return StatusView{}, pollCtx.Err()
		case <-time.After(b.statusPollInterval()):
		}
	}
}

func (b *openSandboxBackend) Stop(ctx context.Context, req StopRequest) error {
	api, err := b.client()
	if err != nil {
		return err
	}
	leaseID, _, _, err := resolveLeaseID(req.ID, "", false, 0, api.BaseURL())
	if err != nil {
		return err
	}
	unlockOperation, err := lockOpenSandboxLeaseOperation(ctx, leaseID)
	if err != nil {
		return err
	}
	defer unlockOperation()
	leaseID, sandboxID, _, err := resolveLeaseID(leaseID, "", false, 0, api.BaseURL())
	if err != nil {
		return err
	}
	if _, err := verifyOpenSandboxClaim(ctx, api, leaseID, sandboxID); err != nil {
		if !isOpenSandboxNotFound(err) || !b.cfg.OpenSandbox.ForgetMissing {
			return err
		}
		fmt.Fprintf(b.rt.Stderr, "warning: forgetting missing opensandbox sandbox=%s after explicit request\n", sandboxID)
		removeLeaseClaim(leaseID)
		return nil
	}
	if err := api.DeleteSandbox(ctx, sandboxID); err != nil {
		if !isOpenSandboxNotFound(err) || !b.cfg.OpenSandbox.ForgetMissing {
			return err
		}
		fmt.Fprintf(b.rt.Stderr, "warning: forgetting missing opensandbox sandbox=%s after explicit request\n", sandboxID)
	}
	removeLeaseClaim(leaseID)
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s\n", leaseID, sandboxID)
	return nil
}

func (b *openSandboxBackend) Cleanup(ctx context.Context, req CleanupRequest) error {
	api, err := b.client()
	if err != nil {
		return err
	}
	claims, err := listOpenSandboxCleanupClaims()
	if err != nil {
		return err
	}
	now := b.now().UTC()
	checked := 0
	removed := 0
	claimsRemoved := 0
	for _, listedClaim := range claims {
		if listedClaim.Provider != providerName || !openSandboxClaimMatchesEndpoint(listedClaim, api.BaseURL()) {
			continue
		}
		var removedOne, claimRemovedOne, checkedOne bool
		err := func() error {
			unlock, err := lockOpenSandboxLeaseOperation(ctx, listedClaim.LeaseID)
			if err != nil {
				return err
			}
			defer unlock()
			claim, err := readLeaseClaim(listedClaim.LeaseID)
			if err != nil {
				return err
			}
			if claim.LeaseID == "" || claim.Provider != providerName || !openSandboxClaimMatchesEndpoint(claim, api.BaseURL()) {
				return nil
			}
			checkedOne = true
			if strings.HasPrefix(claim.LeaseID, recoveryPrefix) {
				removedOne, claimRemovedOne, err = b.cleanupOpenSandboxRecovery(ctx, api, claim, now, req.DryRun)
				return err
			}
			sandboxID := strings.TrimPrefix(claim.LeaseID, leasePrefix)
			sb, getErr := api.GetSandbox(ctx, sandboxID)
			if getErr != nil {
				if !isOpenSandboxNotFound(getErr) {
					return getErr
				}
				if !b.cfg.OpenSandbox.ForgetMissing {
					fmt.Fprintf(b.rt.Stderr, "skip sandbox=%s lease=%s reason=missing-or-inaccessible; set opensandbox forget-missing to remove the claim\n", sandboxID, claim.LeaseID)
					return nil
				}
				if req.DryRun {
					fmt.Fprintf(b.rt.Stdout, "would remove claim lease=%s slug=%s reason=missing sandbox\n", claim.LeaseID, blank(claim.Slug, "-"))
					return nil
				}
				if err := removeLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
					return err
				}
				fmt.Fprintf(b.rt.Stdout, "remove claim lease=%s slug=%s reason=missing sandbox\n", claim.LeaseID, blank(claim.Slug, "-"))
				claimRemovedOne = true
				return nil
			}
			due, reason := openSandboxClaimCleanupDue(claim, now)
			if !due {
				fmt.Fprintf(b.rt.Stderr, "skip sandbox=%s lease=%s reason=%s\n", sandboxID, claim.LeaseID, reason)
				return nil
			}
			if err := validateOpenSandboxOwnership(claim, sb); err != nil {
				return err
			}
			if req.DryRun {
				fmt.Fprintf(b.rt.Stdout, "would delete sandbox=%s lease=%s reason=%s\n", sandboxID, claim.LeaseID, reason)
				return nil
			}
			if err := api.DeleteSandbox(ctx, sandboxID); err != nil && !isOpenSandboxNotFound(err) {
				return err
			}
			if err := removeLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
				return err
			}
			fmt.Fprintf(b.rt.Stdout, "delete sandbox=%s lease=%s reason=%s\n", sandboxID, claim.LeaseID, reason)
			removedOne = true
			return nil
		}()
		if err != nil {
			return err
		}
		if checkedOne {
			checked++
		}
		if removedOne {
			removed++
		}
		if claimRemovedOne {
			claimsRemoved++
		}
	}
	if !req.DryRun {
		fmt.Fprintf(b.rt.Stdout, "%s cleanup removed=%d claims_removed=%d checked=%d\n", providerName, removed, claimsRemoved, checked)
	}
	return nil
}

func (b *openSandboxBackend) cleanupOpenSandboxRecovery(ctx context.Context, api openSandboxClient, claim LeaseClaim, now time.Time, dryRun bool) (bool, bool, error) {
	sandboxes, err := api.ListSandboxes(ctx, map[string]string{openSandboxClaimKey: claim.ProviderScope})
	if err != nil {
		return false, false, err
	}
	matches := make([]sandboxInfo, 0, len(sandboxes))
	for _, sb := range sandboxes {
		if sb.Metadata[openSandboxClaimKey] != claim.ProviderScope {
			continue
		}
		if strings.TrimSpace(sb.ID) == "" {
			return false, false, exit(5, "opensandbox recovery %s matched a sandbox without an id", claim.LeaseID)
		}
		matches = append(matches, sb)
	}
	if len(matches) == 0 {
		expired, err := openSandboxRecoveryExpired(claim, now)
		if err != nil {
			return false, false, err
		}
		if !expired {
			fmt.Fprintf(b.rt.Stderr, "skip recovery=%s reason=awaiting sandbox visibility or expiration\n", claim.LeaseID)
			return false, false, nil
		}
		if dryRun {
			fmt.Fprintf(b.rt.Stdout, "would remove recovery=%s reason=sandbox lifetime elapsed\n", claim.LeaseID)
			return false, false, nil
		}
		if err := removeLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
			return false, false, err
		}
		fmt.Fprintf(b.rt.Stdout, "remove recovery=%s reason=sandbox lifetime elapsed\n", claim.LeaseID)
		return false, true, nil
	}
	if dryRun {
		for _, sb := range matches {
			fmt.Fprintf(b.rt.Stdout, "would delete sandbox=%s recovery=%s reason=ambiguous create\n", sb.ID, claim.LeaseID)
		}
		return false, false, nil
	}
	for _, sb := range matches {
		if err := api.DeleteSandbox(ctx, sb.ID); err != nil && !isOpenSandboxNotFound(err) {
			return false, false, err
		}
	}
	if err := removeLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
		return false, false, err
	}
	for _, sb := range matches {
		fmt.Fprintf(b.rt.Stdout, "delete sandbox=%s recovery=%s reason=ambiguous create\n", sb.ID, claim.LeaseID)
	}
	return true, false, nil
}

func openSandboxRecoveryExpired(claim LeaseClaim, now time.Time) (bool, error) {
	createdAt, err := time.Parse(time.RFC3339, strings.TrimSpace(claim.ClaimedAt))
	if err != nil {
		return false, exit(5, "opensandbox recovery %s has invalid claimed time", claim.LeaseID)
	}
	if claim.IdleTimeoutSeconds <= 0 {
		return false, exit(5, "opensandbox recovery %s has no sandbox lifetime", claim.LeaseID)
	}
	return !now.Before(createdAt.Add(time.Duration(claim.IdleTimeoutSeconds) * time.Second)), nil
}

func openSandboxClaimMatchesEndpoint(claim LeaseClaim, baseURL string) bool {
	return strings.HasPrefix(strings.TrimSpace(claim.ProviderScope), openSandboxEndpointScope(baseURL)+"-own-")
}

func openSandboxClaimCleanupDue(claim LeaseClaim, now time.Time) (bool, string) {
	if claim.IdleTimeoutSeconds <= 0 {
		return false, "idle timeout disabled"
	}
	lastUsed, err := time.Parse(time.RFC3339, strings.TrimSpace(claim.LastUsedAt))
	if err != nil {
		return false, "invalid last-used time"
	}
	deadline := lastUsed.Add(time.Duration(claim.IdleTimeoutSeconds) * time.Second)
	if now.Before(deadline) {
		return false, "idle timeout not reached"
	}
	return true, "idle timeout"
}

func (b *openSandboxBackend) refreshOpenSandboxLeaseActivity(leaseID string) error {
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		return err
	}
	if claim.LeaseID == "" {
		return nil
	}
	idleTimeout := timeoutOrDefault(b.cfg.IdleTimeout, time.Duration(claim.IdleTimeoutSeconds)*time.Second)
	return claimLeaseForRepoProviderScopePond(
		claim.LeaseID,
		claim.Slug,
		providerName,
		claim.ProviderScope,
		claim.Pond,
		claim.RepoRoot,
		idleTimeout,
		false,
	)
}

func (b *openSandboxBackend) refreshOpenSandboxActivityIfRetained(leaseID string, shouldStop bool) error {
	if shouldStop {
		return nil
	}
	return b.refreshOpenSandboxLeaseActivity(leaseID)
}

func (b *openSandboxBackend) warnOpenSandboxActivityRefresh(leaseID string, shouldStop bool) {
	if err := b.refreshOpenSandboxActivityIfRetained(leaseID, shouldStop); err != nil {
		fmt.Fprintf(b.rt.Stderr, "warning: refresh opensandbox lease activity failed lease=%s: %v\n", leaseID, err)
	}
}

func (b *openSandboxBackend) cleanupCreatedRun(ctx context.Context, api openSandboxClient, leaseID, sandboxID string, shouldStop *bool) error {
	if !*shouldStop {
		return nil
	}
	*shouldStop = false
	cleanupCtx, cancel := b.cleanupContext(ctx)
	defer cancel()
	if err := api.DeleteSandbox(cleanupCtx, sandboxID); err != nil && !isOpenSandboxNotFound(err) {
		return fmt.Errorf("opensandbox delete failed for %s: %w", sandboxID, err)
	}
	removeLeaseClaim(leaseID)
	return nil
}

func (b *openSandboxBackend) createSandbox(ctx context.Context, api openSandboxClient, repo Repo, reclaim bool, requestedSlug string) (string, string, string, sandboxInfo, func(), error) {
	providerScope, err := newOpenSandboxClaimScope(api.BaseURL())
	if err != nil {
		return "", "", "", sandboxInfo{}, nil, err
	}
	image := strings.TrimSpace(b.cfg.OpenSandbox.Image)
	if image == "" {
		image = defaultImage
	}
	platformOS, platformArch, err := openSandboxPlatform(b.cfg.OpenSandbox.PlatformOS, b.cfg.OpenSandbox.PlatformArch)
	if err != nil {
		return "", "", "", sandboxInfo{}, nil, err
	}
	sb, err := api.CreateSandbox(ctx, createSandboxOptions{
		Image:          image,
		TimeoutSecs:    durationSecondsCeil(b.sandboxLifetime()),
		CPU:            b.cfg.OpenSandbox.CPU,
		Memory:         b.cfg.OpenSandbox.Memory,
		SecureAccess:   b.cfg.OpenSandbox.SecureAccess,
		UseServerProxy: b.cfg.OpenSandbox.UseServerProxy,
		PlatformOS:     platformOS,
		PlatformArch:   platformArch,
		Metadata: map[string]string{
			openSandboxClaimKey: providerScope,
			openSandboxNameKey:  newSandboxName(repo),
			"crabbox":           "true",
		},
	})
	if err != nil {
		var ambiguous *ambiguousOpenSandboxCreateError
		if !errors.As(err, &ambiguous) {
			return "", "", "", sandboxInfo{}, nil, err
		}
		recoveryLeaseID, recoveryErr := b.recordAmbiguousCreate(providerScope, repo)
		if recoveryErr != nil {
			return "", "", "", sandboxInfo{}, nil, fmt.Errorf("%w; persist opensandbox create recovery scope=%s: %v", err, providerScope, recoveryErr)
		}
		return "", "", "", sandboxInfo{}, nil, b.reconcileAmbiguousCreateFailure(ctx, api, providerScope, recoveryLeaseID, err)
	}
	leaseID := leasePrefix + sb.ID
	unlockOperation, err := lockOpenSandboxLeaseOperation(ctx, leaseID)
	if err != nil {
		return leaseID, sb.ID, "", sandboxInfo{}, nil, b.cleanupCreateFailure(ctx, api, sb.ID, err)
	}
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		cleanupErr := b.cleanupCreateFailure(ctx, api, sb.ID, err)
		unlockOperation()
		return leaseID, sb.ID, "", sandboxInfo{}, nil, cleanupErr
	}
	if err := claimLeaseForRepoProviderScopePond(leaseID, slug, providerName, providerScope, b.cfg.Pond, repo.Root, b.cfg.IdleTimeout, reclaim); err != nil {
		cleanupErr := b.cleanupCreateFailure(ctx, api, sb.ID, err)
		unlockOperation()
		return leaseID, sb.ID, slug, sandboxInfo{}, nil, cleanupErr
	}
	return leaseID, sb.ID, slug, sb, unlockOperation, nil
}

func (b *openSandboxBackend) recordAmbiguousCreate(providerScope string, repo Repo) (string, error) {
	if strings.TrimSpace(repo.Root) == "" {
		return "", errors.New("repository root is required")
	}
	recoveryLeaseID := openSandboxRecoveryLeaseID(providerScope)
	if err := claimLeaseForRepoProviderScopePond(
		recoveryLeaseID,
		"",
		providerName,
		providerScope,
		"",
		repo.Root,
		b.sandboxLifetime(),
		false,
	); err != nil {
		return "", err
	}
	return recoveryLeaseID, nil
}

func (b *openSandboxBackend) reconcileAmbiguousCreateFailure(ctx context.Context, api openSandboxClient, providerScope, recoveryLeaseID string, cause error) error {
	cleanupCtx, cancel := b.cleanupContext(ctx)
	defer cancel()
	unlockOperation, err := lockOpenSandboxLeaseOperation(cleanupCtx, recoveryLeaseID)
	if err != nil {
		return fmt.Errorf("%w; lock opensandbox create recovery=%s: %v", cause, recoveryLeaseID, err)
	}
	defer unlockOperation()
	recoveryClaim, err := readLeaseClaim(recoveryLeaseID)
	if err != nil {
		return fmt.Errorf("%w; read opensandbox create recovery=%s: %v", cause, recoveryLeaseID, err)
	}
	if recoveryClaim.LeaseID == "" {
		return cause
	}
	if recoveryClaim.Provider != providerName || recoveryClaim.ProviderScope != providerScope {
		return fmt.Errorf("%w; opensandbox create recovery=%s changed before reconciliation", cause, recoveryLeaseID)
	}
	pollInterval := 250 * time.Millisecond
	if b.reconcilePollOverride > 0 {
		pollInterval = b.reconcilePollOverride
	}
	var lastTransient error
	for {
		sandboxes, err := api.ListSandboxes(cleanupCtx, map[string]string{openSandboxClaimKey: providerScope})
		if err != nil {
			if !isOpenSandboxAmbiguousCreateError(err) {
				return fmt.Errorf("%w; reconcile ambiguous opensandbox create recovery=%s failed: %v", cause, recoveryLeaseID, err)
			}
			lastTransient = err
		} else {
			lastTransient = nil
			var cleanupErrors []error
			matched := false
			for _, sb := range sandboxes {
				if sb.Metadata[openSandboxClaimKey] != providerScope {
					continue
				}
				matched = true
				if strings.TrimSpace(sb.ID) == "" {
					cleanupErrors = append(cleanupErrors, errors.New("matched sandbox omitted its id"))
					continue
				}
				if err := api.DeleteSandbox(cleanupCtx, sb.ID); err != nil && !isOpenSandboxNotFound(err) {
					if isOpenSandboxAmbiguousCreateError(err) {
						lastTransient = err
						continue
					}
					cleanupErrors = append(cleanupErrors, fmt.Errorf("delete sandbox %s: %w", sb.ID, err))
				}
			}
			if err := errors.Join(cleanupErrors...); err != nil {
				return fmt.Errorf("%w; reconcile ambiguous opensandbox create recovery=%s cleanup failed: %v", cause, recoveryLeaseID, err)
			}
			if matched && lastTransient == nil {
				if err := removeOpenSandboxRecoveryClaim(recoveryLeaseID, providerScope); err != nil {
					return fmt.Errorf("%w; remove opensandbox create recovery=%s: %v", cause, recoveryLeaseID, err)
				}
				return cause
			}
		}
		select {
		case <-cleanupCtx.Done():
			if lastTransient != nil {
				return fmt.Errorf("%w; reconcile ambiguous opensandbox create recovery=%s timed out: %v", cause, recoveryLeaseID, lastTransient)
			}
			return fmt.Errorf("%w; unresolved opensandbox create retained as recovery=%s", cause, recoveryLeaseID)
		case <-time.After(pollInterval):
		}
	}
}

func openSandboxRecoveryLeaseID(providerScope string) string {
	digest := sha256.Sum256([]byte(providerScope))
	return recoveryPrefix + hex.EncodeToString(digest[:8])
}

func removeOpenSandboxRecoveryClaim(leaseID, providerScope string) error {
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		return err
	}
	if claim.LeaseID == "" || claim.Provider != providerName || claim.ProviderScope != providerScope {
		return nil
	}
	return removeLeaseClaimIfUnchanged(leaseID, claim)
}

func resolveLeaseID(id, repoRoot string, reclaim bool, idleTimeout time.Duration, baseURL string) (string, string, string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", "", "", exit(2, "provider=opensandbox requires a Crabbox-created sandbox slug or lease id")
	}
	exactLeaseID := id
	if !strings.HasPrefix(exactLeaseID, leasePrefix) {
		exactLeaseID = leasePrefix + exactLeaseID
	}
	if claim, err := readLeaseClaim(exactLeaseID); err != nil {
		return "", "", "", err
	} else if claim.LeaseID == exactLeaseID && claim.Provider == providerName {
		return finishResolvedLease(claim, repoRoot, reclaim, idleTimeout, baseURL)
	}
	claim, ok, err := resolveOpenSandboxLeaseClaim(id, baseURL)
	if err != nil {
		return "", "", "", err
	}
	if ok {
		return finishResolvedLease(claim, repoRoot, reclaim, idleTimeout, baseURL)
	}
	return "", "", "", exit(4, "opensandbox sandbox %q is not claimed by Crabbox; use a Crabbox slug or %s<sandbox-id>", id, leasePrefix)
}

func resolveOpenSandboxLeaseClaim(identifier, baseURL string) (LeaseClaim, bool, error) {
	claims, err := listOpenSandboxLeaseClaims()
	if err != nil {
		return LeaseClaim{}, false, err
	}
	for _, claim := range claims {
		if claim.Provider == providerName && claim.LeaseID == identifier {
			if err := validateOpenSandboxClaimScope(claim, baseURL); err != nil {
				return LeaseClaim{}, false, err
			}
			return claim, true, nil
		}
	}
	slug := normalizeLeaseSlug(identifier)
	if slug != "" {
		for _, claim := range claims {
			if claim.Provider == providerName && normalizeLeaseSlug(claim.Slug) == slug {
				if err := validateOpenSandboxClaimScope(claim, baseURL); err != nil {
					return LeaseClaim{}, false, err
				}
				return claim, true, nil
			}
		}
	}
	return LeaseClaim{}, false, nil
}

func finishResolvedLease(claim LeaseClaim, repoRoot string, reclaim bool, idleTimeout time.Duration, baseURL string) (string, string, string, error) {
	if err := validateOpenSandboxClaimScope(claim, baseURL); err != nil {
		return "", "", "", err
	}
	if repoRoot != "" {
		if err := claimLeaseForRepoProviderScopePond(claim.LeaseID, claim.Slug, providerName, claim.ProviderScope, claim.Pond, repoRoot,
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

func authorizeOpenSandboxRepoClaim(claim LeaseClaim, repoRoot string, reclaim bool) error {
	if repoRoot == "" || claim.RepoRoot == "" || claim.RepoRoot == repoRoot || reclaim {
		return nil
	}
	return exit(2, "lease %s is claimed by repo %s; use --reclaim to claim it for %s", claim.LeaseID, claim.RepoRoot, repoRoot)
}

func validateOpenSandboxClaimScope(claim LeaseClaim, baseURL string) error {
	if !strings.HasPrefix(strings.TrimSpace(claim.ProviderScope), openSandboxEndpointScope(baseURL)+"-own-") {
		return exit(4, "opensandbox lease %q belongs to a different API endpoint; restore the endpoint used to create it", claim.LeaseID)
	}
	return nil
}

func openSandboxPlatformOS(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if !strings.EqualFold(value, "linux") {
		return "", exit(2, "provider=opensandbox only supports Linux sandboxes; set openSandbox.platformOS to linux or leave it empty")
	}
	return "linux", nil
}

func openSandboxPlatform(osValue, archValue string) (string, string, error) {
	osValue, err := openSandboxPlatformOS(osValue)
	if err != nil {
		return "", "", err
	}
	archValue = strings.TrimSpace(archValue)
	if (osValue == "") != (archValue == "") {
		return "", "", exit(2, "openSandbox.platformOS and openSandbox.platformArch must be set together or both left empty")
	}
	return osValue, archValue, nil
}

func newOpenSandboxClaimScope(baseURL string) (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", exit(5, "generate opensandbox ownership token: %v", err)
	}
	return openSandboxEndpointScope(baseURL) + "-own-" + hex.EncodeToString(token[:]), nil
}

func openSandboxEndpointScope(baseURL string) string {
	digest := sha256.Sum256([]byte(baseURL))
	return "ep-" + hex.EncodeToString(digest[:8])
}

func verifyOpenSandboxClaim(ctx context.Context, api openSandboxClient, leaseID, sandboxID string) (sandboxInfo, error) {
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		return sandboxInfo{}, err
	}
	if err := validateOpenSandboxClaimScope(claim, api.BaseURL()); err != nil {
		return sandboxInfo{}, err
	}
	sb, err := api.GetSandbox(ctx, sandboxID)
	if err != nil {
		return sandboxInfo{}, err
	}
	if err := validateOpenSandboxOwnership(claim, sb); err != nil {
		return sandboxInfo{}, err
	}
	return sb, nil
}

func validateOpenSandboxOwnership(claim LeaseClaim, sb sandboxInfo) error {
	if sb.Metadata[openSandboxClaimKey] != claim.ProviderScope {
		return exit(4, "opensandbox sandbox %q ownership metadata does not match its local claim", sb.ID)
	}
	return nil
}

func (b *openSandboxBackend) ensureReusableSandbox(ctx context.Context, api openSandboxClient, sandboxID string, sb sandboxInfo) error {
	switch strings.ToLower(strings.TrimSpace(sb.State)) {
	case "", "running":
		return nil
	case "paused":
		fmt.Fprintf(b.rt.Stderr, "resuming opensandbox sandbox=%s\n", sandboxID)
		return api.ResumeSandbox(ctx, sandboxID)
	default:
		return exit(4, "opensandbox sandbox %q is %s and cannot be reused until it is running", sandboxID, sb.State)
	}
}

func buildCommand(command []string, shellMode bool) ([]string, error) {
	if len(command) == 0 {
		return nil, errors.New("missing command")
	}
	if shellMode {
		return []string{"bash", "-lc", strings.Join(command, " ")}, nil
	}
	if shouldUseShell(command) || leadingEnvAssignment(command) {
		if len(command) == 1 {
			return []string{"bash", "-lc", command[0]}, nil
		}
		return []string{"bash", "-lc", shellScriptFromArgv(command)}, nil
	}
	return command, nil
}

func leadingEnvAssignment(command []string) bool {
	return len(command) > 1 && strings.Contains(command[0], "=") && !strings.HasPrefix(command[0], "-")
}

func commandScript(command []string) string {
	return shellScriptFromArgv(command)
}

func openSandboxWorkdir(cfg Config) (string, error) {
	workdir := strings.TrimSpace(cfg.OpenSandbox.Workdir)
	if workdir == "" {
		workdir = defaultWorkdir
	}
	clean := path.Clean(workdir)
	if !strings.HasPrefix(clean, "/") {
		return "", exit(2, "opensandbox workdir %q must be an absolute path", workdir)
	}
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var", "/workspace":
		return "", exit(2, "opensandbox workdir %q is too broad; choose a dedicated subdirectory", clean)
	}
	return clean, nil
}

func isReadyState(state string) bool {
	switch strings.TrimSpace(strings.ToLower(state)) {
	case "running", "ready", "started", "active":
		return true
	default:
		return false
	}
}

func isTerminalState(state string) bool {
	switch strings.TrimSpace(strings.ToLower(state)) {
	case "terminated", "stopped", "failed", "error", "killed", "deleted":
		return true
	default:
		return false
	}
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
	maxBase := 63 - len(namePrefix) - 1 - 6
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

func randomSuffix() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())[:6]
	}
	return hex.EncodeToString(b[:])
}

func (b *openSandboxBackend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}

func (b *openSandboxBackend) cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := openSandboxCleanupTimeout
	if b.cleanupTimeoutOverride > 0 {
		timeout = b.cleanupTimeoutOverride
	}
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}

func (b *openSandboxBackend) cleanupCreateFailure(ctx context.Context, api openSandboxClient, sandboxID string, cause error) error {
	cleanupCtx, cancel := b.cleanupContext(ctx)
	defer cancel()
	if err := api.DeleteSandbox(cleanupCtx, sandboxID); err != nil {
		if isOpenSandboxNotFound(err) {
			return cause
		}
		return fmt.Errorf("%w; cleanup opensandbox sandbox %s failed: %v", cause, sandboxID, err)
	}
	return cause
}

func (b *openSandboxBackend) cleanupClaimedSandboxFailure(ctx context.Context, api openSandboxClient, leaseID, sandboxID string, cause error) error {
	cleanupCtx, cancel := b.cleanupContext(ctx)
	defer cancel()
	if err := api.DeleteSandbox(cleanupCtx, sandboxID); err != nil && !isOpenSandboxNotFound(err) {
		return fmt.Errorf("%w; cleanup opensandbox sandbox %s failed: %v", cause, sandboxID, err)
	}
	removeLeaseClaim(leaseID)
	return cause
}

func (b *openSandboxBackend) execTimeoutSecs() int {
	if b.cfg.OpenSandbox.ExecTimeoutSecs > 0 {
		return b.cfg.OpenSandbox.ExecTimeoutSecs
	}
	return openSandboxExecTimeoutSecs
}

func (b *openSandboxBackend) sandboxLifetime() time.Duration {
	return openSandboxLifetimeForConfig(b.cfg)
}

func (b *openSandboxBackend) commandLifetime() time.Duration {
	return openSandboxCommandBudgetForConfig(b.cfg)
}

func (b *openSandboxBackend) runLifetimeBudget(req RunRequest) time.Duration {
	return openSandboxRunBudgetForConfig(b.cfg, req.NoSync, req.SyncOnly)
}

func openSandboxExpiration(sb sandboxInfo) (time.Time, error) {
	if sb.ExpiresAt == nil || sb.ExpiresAt.IsZero() {
		return time.Time{}, exit(5, "opensandbox sandbox %s did not report an expiration", sb.ID)
	}
	return sb.ExpiresAt.UTC(), nil
}

func isOpenSandboxReadinessPending(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var apiErr *sdk.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusNotFound ||
			apiErr.StatusCode == http.StatusConflict ||
			apiErr.StatusCode == http.StatusTooEarly ||
			apiErr.StatusCode == http.StatusTooManyRequests ||
			apiErr.StatusCode >= http.StatusInternalServerError
	}
	var netErr net.Error
	if errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary()) {
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH)
}

func isOpenSandboxAmbiguousCreateError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var apiErr *sdk.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusRequestTimeout ||
			apiErr.StatusCode == http.StatusTooManyRequests ||
			apiErr.StatusCode >= http.StatusInternalServerError
	}
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary()) {
		return true
	}
	return errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EPIPE)
}

func (b *openSandboxBackend) statusPollInterval() time.Duration {
	if b.statusPollOverride > 0 {
		return b.statusPollOverride
	}
	return openSandboxStatusPoll
}

func (b *openSandboxBackend) statusProbeTimeout() time.Duration {
	if b.statusProbeOverride > 0 {
		return b.statusProbeOverride
	}
	return openSandboxStatusProbe
}

func durationSecondsCeil(value time.Duration) int {
	if value <= 0 {
		return 0
	}
	return int((value + time.Second - 1) / time.Second)
}
