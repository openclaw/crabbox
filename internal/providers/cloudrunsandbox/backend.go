package cloudrunsandbox

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"reflect"
	"regexp"
	"strings"
	"time"
)

func NewBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	return &backend{spec: spec, cfg: cfg, rt: rt}
}

type backend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) Warmup(ctx context.Context, req WarmupRequest) error {
	if req.ActionsRunner {
		return exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	if req.Options.Desktop || req.Options.Browser || req.Options.Code {
		return exit(2, "provider=%s does not support desktop, browser, or code-server options", providerName)
	}
	if req.Options.Tailscale.Enabled {
		return exit(2, "provider=%s is delegated-run only and does not support Tailscale options", providerName)
	}
	started := b.now()
	transport, err := newTransport(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, sandboxID, slug, _, err := b.createSandbox(ctx, transport, req.Repo, req.Reclaim, req.RequestedSlug)
	if err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s sandbox=%s mode=%s\n", leaseID, slug, providerName, sandboxID, transport.Mode())
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: cloud-run-sandbox warmup keeps the sandbox until explicit stop\n")
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

func (b *backend) Run(ctx context.Context, req RunRequest) (finalResult RunResult, finalErr error) {
	if err := delegatedSyncOptionsError(b.spec, req); err != nil {
		return RunResult{}, err
	}
	if req.Options.Desktop || req.Options.Browser || req.Options.Code {
		return RunResult{}, exit(2, "provider=%s does not support desktop, browser, or code-server options", providerName)
	}
	if req.Options.Tailscale.Enabled {
		return RunResult{}, exit(2, "provider=%s is delegated-run only and does not support Tailscale options", providerName)
	}
	var command []string
	if !req.SyncOnly {
		var err error
		command, err = buildCommand(req.Command, req.ShellMode)
		if err != nil {
			return RunResult{}, err
		}
	}
	workdir, err := cloudRunSandboxWorkdir(b.cfg)
	if err != nil {
		return RunResult{}, err
	}
	started := b.now()
	transport, err := newTransport(b.cfg, b.rt)
	if err != nil {
		return RunResult{}, err
	}
	leaseID, sandboxID, slug := "", "", ""
	var claim LeaseClaim
	acquired := false
	if req.ID == "" {
		leaseID, sandboxID, slug, claim, err = b.createSandbox(ctx, transport, req.Repo, req.Reclaim, req.RequestedSlug)
		if err != nil {
			return RunResult{}, err
		}
		fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s sandbox=%s mode=%s\n", leaseID, slug, providerName, sandboxID, transport.Mode())
		acquired = true
	} else {
		leaseID, sandboxID, slug, claim, err = b.resolveLeaseID(req.ID, req.Repo.Root, req.Reclaim)
		if err != nil {
			return RunResult{}, err
		}
		if state, ready := b.claimStatus(claim); !ready {
			return RunResult{}, exit(4, "cloud-run-sandbox lease %q is not ready for execution (state=%s)", leaseID, state)
		}
	}
	shouldStop := acquired && !req.Keep
	session := &RunSessionHandle{
		Provider:       providerName,
		LeaseID:        leaseID,
		Slug:           slug,
		Reused:         !acquired,
		Kept:           !shouldStop,
		CleanupCommand: cleanupCommand(b.cfg, leaseID),
	}
	pendingTiming := timingReport{
		Provider:      providerName,
		LeaseID:       leaseID,
		Slug:          slug,
		SyncDelegated: true,
		SyncSkipped:   req.NoSync,
		Label:         strings.TrimSpace(req.Label),
	}
	timingReady := req.TimingJSON
	if req.TimingJSON {
		defer func() {
			if !timingReady {
				return
			}
			report := timingReportWithRunResult(pendingTiming, finalResult, finalErr)
			if timingErr := writeTimingJSON(b.rt.Stderr, report); timingErr != nil {
				finalErr = errors.Join(finalErr, timingErr)
			}
		}()
	}
	if shouldStop {
		defer func() {
			if !shouldStop {
				session.Kept = true
				return
			}
			cleanupCtx, cancel := b.cleanupContext(ctx)
			defer cancel()
			if killErr := b.releaseClaimedSandboxIfUnchanged(cleanupCtx, transport, sandboxID, claim); killErr != nil {
				fmt.Fprintf(b.rt.Stderr, "warning: cloud-run-sandbox destroy failed for %s: %v\n", sandboxID, killErr)
				session.Kept = true
				finalErr = errors.Join(finalErr, fmt.Errorf("cloud-run-sandbox automatic teardown failed; recovery claim retained lease=%s: %w", leaseID, killErr))
				return
			}
			session.Kept = false
		}()
	}
	activityTimeout, err := claimOperationTimeout(claim, leaseActivityTimeout, b.now().UTC())
	if err != nil {
		return RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, Session: session}, err
	}
	claim, err = b.markClaimActivity(claim, "running", activityTimeout)
	if err != nil {
		return RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, Session: session}, err
	}
	ctx, cancel := context.WithTimeout(ctx, activityTimeout)
	defer cancel()
	var guardedResult RunResult
	var guardedErr error
	guardErr := withLeaseClaimUnchanged(leaseID, claim, func() error {
		guardedResult, guardedErr = func() (RunResult, error) {
			fmt.Fprintf(b.rt.Stderr, "provider=%s lease=%s sandbox=%s workdir=%s mode=%s\n", providerName, leaseID, sandboxID, workdir, transport.Mode())

			syncDuration := time.Duration(0)
			syncPhases := []timingPhase{{Name: "sync", Skipped: true, Reason: "--no-sync"}}
			if req.TimingJSON {
				pendingTiming.SyncPhases = syncPhases
			}
			if !req.NoSync {
				syncPhases, syncDuration, err = b.syncWorkspace(ctx, transport, sandboxID, req, workdir)
				if req.TimingJSON {
					pendingTiming.SyncMs = syncDuration.Milliseconds()
					pendingTiming.SyncPhases = syncPhases
				}
				if err != nil {
					handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
					return RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, Total: b.now().Sub(started), SyncDelegated: true, Session: session}, err
				}
				fmt.Fprintf(b.rt.Stderr, "sync complete in %s\n", syncDuration.Round(time.Millisecond))
			} else if err := b.ensureWorkspace(ctx, transport, sandboxID, workdir); err != nil {
				handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
				return RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, Total: b.now().Sub(started), SyncDelegated: true, Session: session}, err
			}

			if req.SyncOnly {
				result := RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, Total: b.now().Sub(started), SyncDelegated: true, Session: session}
				fmt.Fprintf(b.rt.Stdout, "synced %s\n", workdir)
				if req.TimingJSON {
					pendingTiming = timingReport{
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
					}
					timingReady = true
				}
				return result, nil
			}

			if req.EnvSummary || strings.TrimSpace(os.Getenv("CRABBOX_ENV_ALLOW")) != "" {
				printEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
			}
			commandStart := b.now()
			exitCode, runErr := b.execCommand(ctx, transport, sandboxID, workdir, command, req.Env, b.rt.Stdout, b.rt.Stderr)
			commandDuration := b.now().Sub(commandStart)
			result := RunResult{
				ExitCode:      exitCode,
				Command:       commandDuration,
				Total:         b.now().Sub(started),
				SyncDelegated: true,
				Provider:      providerName,
				LeaseID:       leaseID,
				Slug:          slug,
				CommandText:   strings.Join(req.Command, " "),
				Session:       session,
			}
			if req.NoSync {
				fmt.Fprintf(b.rt.Stderr, "cloud-run-sandbox run summary sync_skipped=true command=%s total=%s exit=%d\n",
					result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), exitCode)
			} else {
				fmt.Fprintf(b.rt.Stderr, "cloud-run-sandbox run summary sync=%s command=%s total=%s exit=%d\n",
					syncDuration.Round(time.Millisecond), result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), exitCode)
			}
			if req.TimingJSON {
				pendingTiming = timingReport{
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
				}
				timingReady = true
			}
			if runErr != nil {
				handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
				errorCode := 1
				if errors.Is(runErr, context.Canceled) {
					errorCode = 130
				} else if errors.Is(runErr, context.DeadlineExceeded) {
					errorCode = 124
				}
				return result, errors.Join(ExitError{Code: errorCode, Message: fmt.Sprintf("cloud-run-sandbox run failed: %v", runErr)}, runErr)
			}
			if exitCode != 0 {
				handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
				return result, ExitError{Code: exitCode, Message: fmt.Sprintf("cloud-run-sandbox run exited %d", exitCode)}
			}
			return result, nil
		}()
		return nil
	})
	clearedClaim, clearErr := b.clearClaimActivity(claim)
	if clearErr == nil {
		claim = clearedClaim
	}
	if guardErr != nil {
		return RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, Session: session}, errors.Join(guardErr, clearErr)
	}
	if clearErr != nil {
		return guardedResult, errors.Join(guardedErr, fmt.Errorf("clear cloud-run-sandbox active claim: %w", clearErr))
	}
	return guardedResult, guardedErr
}

func cleanupCommand(cfg Config, leaseID string) string {
	command := "crabbox stop --provider " + providerName
	if gatewayURL := strings.TrimSpace(cfg.CloudRunSandbox.GatewayURL); gatewayURL != "" {
		command += " --cloud-run-sandbox-gateway-url " + shellQuote(gatewayURL)
	} else if cliPath := strings.TrimSpace(cfg.CloudRunSandbox.CLIPath); cliPath != "" && cliPath != defaultCLIPath {
		command += " --cloud-run-sandbox-cli " + shellQuote(cliPath)
	}
	return command + " --id " + shellQuote(leaseID)
}

func (b *backend) List(ctx context.Context, _ ListRequest) ([]LeaseView, error) {
	scope, err := b.claimScope()
	if err != nil {
		return nil, err
	}
	claims, err := listCloudRunSandboxLeaseClaims()
	if err != nil {
		return nil, err
	}
	servers := make([]Server, 0, len(claims))
	var transport sandboxTransport
	for _, claim := range claims {
		if claim.Provider != providerName || !strings.HasPrefix(claim.LeaseID, leasePrefix) {
			continue
		}
		if claim.ProviderScope != scope {
			continue
		}
		sandboxID := strings.TrimPrefix(claim.LeaseID, leasePrefix)
		if sandboxID == "" {
			continue
		}
		state, ready := b.claimStatus(claim)
		if ready {
			if transport == nil {
				transport, err = newTransport(b.cfg, b.rt)
				if err != nil {
					return nil, err
				}
			}
			ownershipToken := strings.TrimSpace(claim.Labels[claimOwnershipLabel])
			switch probeErr := transport.Probe(ctx, sandboxID, ownershipToken); {
			case errors.Is(probeErr, errSandboxNotFound):
				state = "missing"
			case probeErr != nil:
				state = "unknown"
			}
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

func (b *backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	transport, err := newTransport(b.cfg, b.rt)
	if err != nil {
		return DoctorResult{}, err
	}
	result := DoctorResult{Provider: providerName}
	healthErr := transport.Health(ctx)
	details := map[string]string{
		"mode":    transport.Mode(),
		"cli":     blank(strings.TrimSpace(b.cfg.CloudRunSandbox.CLIPath), defaultCLIPath),
		"workdir": blank(strings.TrimSpace(b.cfg.CloudRunSandbox.Workdir), defaultWorkdir),
	}
	if transport.Mode() == "remote" {
		details["gateway"] = strings.TrimSpace(b.cfg.CloudRunSandbox.GatewayURL)
	}
	result.Checks = append(result.Checks, doctorCheck("control_plane", healthErr, details))
	servers, listErr := b.List(ctx, ListRequest{})
	if listErr != nil {
		result.Checks = append(result.Checks, doctorCheck("local_claims", listErr, nil))
	} else {
		result.Checks = append(result.Checks, DoctorCheck{
			Status:  "ok",
			Check:   "local_claims",
			Message: fmt.Sprintf("%d claimed sandboxes", len(servers)),
			Details: map[string]string{"count": fmt.Sprint(len(servers))},
		})
	}
	if healthErr != nil {
		result.Status = "error"
		result.Message = fmt.Sprintf("mode=%s control_plane=blocked mutation=false", transport.Mode())
		return result, errors.Join(healthErr, listErr)
	}
	if listErr != nil {
		result.Status = "error"
		result.Message = fmt.Sprintf("mode=%s control_plane=ready mutation=false local_claims=blocked", transport.Mode())
		return result, listErr
	}
	result.Status = "ok"
	result.Message = fmt.Sprintf("mode=%s control_plane=ready mutation=false claims=%d", transport.Mode(), len(servers))
	return result, nil
}

func (b *backend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	leaseID, sandboxID, slug, claim, err := b.resolveLeaseID(req.ID, "", false)
	if err != nil {
		return StatusView{}, err
	}
	state, ready := b.claimStatus(claim)
	if ready {
		transport, transportErr := newTransport(b.cfg, b.rt)
		if transportErr != nil {
			return StatusView{}, transportErr
		}
		ownershipToken := strings.TrimSpace(claim.Labels[claimOwnershipLabel])
		if probeErr := transport.Probe(ctx, sandboxID, ownershipToken); probeErr != nil {
			if errors.Is(probeErr, errSandboxNotFound) {
				state = "missing"
				ready = false
			} else {
				return StatusView{}, fmt.Errorf("probe cloud-run-sandbox %q: %w", sandboxID, probeErr)
			}
		}
	}
	return StatusView{
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
	}, nil
}

func (b *backend) claimStatus(claim LeaseClaim) (string, bool) {
	state := statusViewReady
	ready := true
	claimState := strings.TrimSpace(claim.Labels[claimStateLabel])
	if claimState == "creating" || claimState == "recovery" || claimState == "conflict" {
		state = claimState
		ready = false
	}
	if expiresAt := strings.TrimSpace(claim.Labels[claimExpiresAtLabel]); expiresAt != "" {
		if expires, parseErr := time.Parse(time.RFC3339Nano, expiresAt); parseErr != nil || !b.now().UTC().Before(expires) {
			state = "expired"
			ready = false
		}
	}
	return state, ready
}

func (b *backend) Stop(ctx context.Context, req StopRequest) error {
	transport, err := newTransport(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, sandboxID, _, claim, err := b.resolveLeaseID(req.ID, "", false)
	if err != nil {
		return err
	}
	if state := strings.TrimSpace(claim.Labels[claimStateLabel]); state == "conflict" {
		return exit(4, "cloud-run-sandbox lease %q has no destructive ownership (state=%s)", leaseID, state)
	}
	if err := b.releaseClaimedSandboxIfUnchanged(ctx, transport, sandboxID, claim); err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s\n", leaseID, sandboxID)
	return nil
}

func (b *backend) Cleanup(ctx context.Context, req CleanupRequest) error {
	scope, err := b.claimScope()
	if err != nil {
		return err
	}
	// Snapshot candidates before any cleanup mutation. A concurrent reuse updates
	// the claim before touching the sandbox, and the guarded destroy below must see
	// that update rather than deleting a newly reclaimed sandbox from a stale view.
	claims, err := listCloudRunSandboxLeaseClaims()
	if err != nil {
		return err
	}
	now := b.now().UTC()
	checked, removed, claimsRemoved := 0, 0, 0
	var cleanupErrs []error
	var transport sandboxTransport
	for _, claim := range claims {
		if claim.Provider != providerName {
			continue
		}
		if claim.ProviderScope != scope {
			continue
		}
		checked++
		sandboxID := strings.TrimPrefix(claim.LeaseID, leasePrefix)
		if sandboxID == "" || sandboxID == claim.LeaseID {
			continue
		}
		due, reason := claimCleanupDue(claim, now)
		if !due {
			fmt.Fprintf(b.rt.Stderr, "skip sandbox=%s lease=%s reason=%s\n", sandboxID, claim.LeaseID, reason)
			continue
		}
		if req.DryRun {
			if err := verifyLeaseClaimUnchanged(claim.LeaseID, claim); err != nil {
				changed, inspectErr := claimChangedSinceSnapshot(claim)
				if inspectErr != nil {
					return fmt.Errorf("inspect cloud-run-sandbox claim after cleanup guard failed: %v: %w", err, inspectErr)
				}
				if changed {
					fmt.Fprintf(b.rt.Stderr, "skip sandbox=%s lease=%s reason=changed-during-cleanup err=%v\n", sandboxID, claim.LeaseID, err)
					continue
				}
				return err
			}
			fmt.Fprintf(b.rt.Stdout, "would delete sandbox=%s lease=%s reason=%s\n", sandboxID, claim.LeaseID, reason)
			continue
		}
		if transport == nil {
			transport, err = newTransport(b.cfg, b.rt)
			if err != nil {
				return err
			}
		}
		wasRemoved, err := b.destroyClaimedSandboxIfUnchanged(ctx, transport, sandboxID, claim)
		if err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("destroy sandbox=%s lease=%s: %w", sandboxID, claim.LeaseID, err))
			continue
		}
		if !wasRemoved {
			continue
		}
		fmt.Fprintf(b.rt.Stdout, "delete sandbox=%s lease=%s reason=%s\n", sandboxID, claim.LeaseID, reason)
		removed++
		claimsRemoved++
	}
	if !req.DryRun {
		fmt.Fprintf(b.rt.Stdout, "%s cleanup removed=%d claims_removed=%d checked=%d\n", providerName, removed, claimsRemoved, checked)
	}
	return errors.Join(cleanupErrs...)
}

func (b *backend) destroyClaimedSandboxIfUnchanged(ctx context.Context, transport sandboxTransport, sandboxID string, claim LeaseClaim) (bool, error) {
	ownershipToken := strings.TrimSpace(claim.Labels[claimOwnershipLabel])
	if ownershipToken == "" {
		return false, exit(4, "cloud-run-sandbox lease %q has no ownership token", claim.LeaseID)
	}
	cleanupStarted := false
	var destroyErr error
	cleanupSandbox := func() error {
		cleanupStarted = true
		destroyErr = transport.Destroy(ctx, sandboxID, ownershipToken)
		if errors.Is(destroyErr, errSandboxNotFound) {
			destroyErr = nil
		}
		return destroyErr
	}
	// Hold the claim lock across the final comparison and remote destroy. This
	// closes the compare/destroy race while removing the claim only after a
	// confirmed destroy (or confirmed absence), so failed cleanup stays tracked.
	if err := removeLeaseClaimIfUnchangedAfter(claim.LeaseID, claim, cleanupSandbox); err != nil {
		if destroyErr != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: destroy sandbox=%s failed; claim retained: %v\n", sandboxID, err)
			return false, destroyErr
		}
		if cleanupStarted {
			// The remote destroy succeeded, so this is a local claim-removal or
			// durability failure and must remain visible to cleanup automation.
			return false, err
		}
		changed, inspectErr := claimChangedSinceSnapshot(claim)
		if inspectErr != nil {
			return false, fmt.Errorf("inspect cloud-run-sandbox claim after cleanup guard failed: %v: %w", err, inspectErr)
		}
		if changed {
			fmt.Fprintf(b.rt.Stderr, "skip sandbox=%s lease=%s reason=changed-during-cleanup err=%v\n", sandboxID, claim.LeaseID, err)
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (b *backend) releaseClaimedSandboxIfUnchanged(ctx context.Context, transport sandboxTransport, sandboxID string, claim LeaseClaim) error {
	ownershipToken := strings.TrimSpace(claim.Labels[claimOwnershipLabel])
	if ownershipToken == "" {
		return exit(4, "cloud-run-sandbox lease %q has no ownership token", claim.LeaseID)
	}
	return removeLeaseClaimIfUnchangedAfter(claim.LeaseID, claim, func() error {
		err := transport.Destroy(ctx, sandboxID, ownershipToken)
		if errors.Is(err, errSandboxNotFound) {
			return nil
		}
		return err
	})
}

func (b *backend) markClaimActivity(claim LeaseClaim, state string, timeout time.Duration) (LeaseClaim, error) {
	labels := cloneLabels(claim.Labels)
	labels[claimStateLabel] = state
	activeUntil := b.now().UTC().Add(timeout)
	if expires, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(labels[claimExpiresAtLabel])); err == nil && expires.Before(activeUntil) {
		activeUntil = expires
	}
	labels[claimActiveUntilLabel] = activeUntil.Format(time.RFC3339Nano)
	return updateLeaseClaimLabelsIfUnchanged(claim.LeaseID, claim, labels)
}

func (b *backend) clearClaimActivity(claim LeaseClaim) (LeaseClaim, error) {
	labels := cloneLabels(claim.Labels)
	delete(labels, claimStateLabel)
	delete(labels, claimActiveUntilLabel)
	return updateLeaseClaimLabelsAndLastUsedIfUnchanged(claim.LeaseID, claim, labels, b.now().UTC())
}

func cloneLabels(labels map[string]string) map[string]string {
	cloned := make(map[string]string, len(labels)+3)
	for key, value := range labels {
		cloned[key] = value
	}
	return cloned
}

func claimOperationTimeout(claim LeaseClaim, maximum time.Duration, now time.Time) (time.Duration, error) {
	expiresAt := strings.TrimSpace(claim.Labels[claimExpiresAtLabel])
	if expiresAt == "" {
		return maximum, nil
	}
	expires, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		return 0, fmt.Errorf("cloud-run-sandbox claim has invalid ttl %q", expiresAt)
	}
	remaining := expires.Sub(now)
	if remaining <= 0 {
		return 0, exit(4, "cloud-run-sandbox lease %q has expired", claim.LeaseID)
	}
	if remaining < maximum {
		return remaining, nil
	}
	return maximum, nil
}

func claimChangedSinceSnapshot(expected LeaseClaim) (bool, error) {
	current, exists, err := readLeaseClaimWithPresence(expected.LeaseID)
	if err != nil {
		return false, err
	}
	return !exists || !reflect.DeepEqual(current, expected), nil
}

func claimCleanupDue(claim LeaseClaim, now time.Time) (bool, string) {
	state := strings.TrimSpace(claim.Labels[claimStateLabel])
	if state == "conflict" {
		return false, "ownership-" + state
	}
	if activeUntil := strings.TrimSpace(claim.Labels[claimActiveUntilLabel]); activeUntil != "" {
		parsed, err := time.Parse(time.RFC3339Nano, activeUntil)
		if err == nil && now.Before(parsed) {
			state := strings.TrimSpace(claim.Labels[claimStateLabel])
			if state == "" {
				state = "active"
			}
			return false, "in-flight-" + state
		}
	}
	if state == "creating" {
		return true, "stale-creating"
	}
	if expiresAt := strings.TrimSpace(claim.Labels[claimExpiresAtLabel]); expiresAt != "" {
		parsed, err := time.Parse(time.RFC3339Nano, expiresAt)
		if err != nil {
			return true, "unparseable-ttl"
		}
		if !now.Before(parsed) {
			return true, "ttl-expired"
		}
	}
	if claim.IdleTimeoutSeconds <= 0 {
		return false, "no-idle-timeout"
	}
	lastUsed := strings.TrimSpace(claim.LastUsedAt)
	if lastUsed == "" {
		lastUsed = strings.TrimSpace(claim.ClaimedAt)
	}
	if lastUsed == "" {
		return true, "missing-timestamps"
	}
	parsed, err := time.Parse(time.RFC3339, lastUsed)
	if err != nil {
		// Accept common claim timestamp formats.
		parsed, err = time.Parse(time.RFC3339Nano, lastUsed)
		if err != nil {
			return true, "unparseable-timestamp"
		}
	}
	deadline := parsed.Add(time.Duration(claim.IdleTimeoutSeconds) * time.Second)
	if now.Before(deadline) {
		return false, "idle-timeout-remaining"
	}
	return true, "idle-timeout-expired"
}

func (b *backend) createSandbox(ctx context.Context, transport sandboxTransport, repo Repo, reclaim bool, requestedSlug string) (string, string, string, LeaseClaim, error) {
	sandboxID, err := newSandboxName(repo)
	if err != nil {
		return "", "", "", LeaseClaim{}, err
	}
	leaseID := leasePrefix + sandboxID
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return "", "", "", LeaseClaim{}, err
	}
	scope, err := b.claimScope()
	if err != nil {
		return "", "", "", LeaseClaim{}, err
	}
	labels := map[string]string{}
	labels[claimStateLabel] = "creating"
	labels[claimOwnershipLabel] = sandboxID
	now := b.now().UTC()
	activeUntil := now.Add(defaultExecTimeout)
	if b.cfg.TTL > 0 {
		expiresAt := now.Add(b.cfg.TTL)
		labels[claimExpiresAtLabel] = expiresAt.Format(time.RFC3339Nano)
		if expiresAt.Before(activeUntil) {
			activeUntil = expiresAt
		}
	}
	labels[claimActiveUntilLabel] = activeUntil.Format(time.RFC3339Nano)
	claim, err := claimLeaseForRepoProviderScopePondWithLabels(leaseID, slug, providerName, scope, b.cfg.Pond, repo.Root, b.cfg.IdleTimeout, labels)
	if err != nil {
		return "", "", "", LeaseClaim{}, err
	}
	claimed := true
	defer func() {
		if claimed {
			_ = removeLeaseClaimIfUnchangedAfter(leaseID, claim, nil)
		}
	}()
	workdir, err := cloudRunSandboxWorkdir(b.cfg)
	if err != nil {
		return "", "", "", LeaseClaim{}, err
	}
	createTimeout, err := claimOperationTimeout(claim, defaultExecTimeout, b.now().UTC())
	if err != nil {
		return "", "", "", LeaseClaim{}, err
	}
	createCtx, cancel := context.WithTimeout(ctx, createTimeout)
	defer cancel()
	recoveryLabels := cloneLabels(claim.Labels)
	recoveryLabels[claimStateLabel] = "recovery"
	delete(recoveryLabels, claimActiveUntilLabel)
	conflictLabels := cloneLabels(claim.Labels)
	conflictLabels[claimStateLabel] = "conflict"
	delete(conflictLabels, claimActiveUntilLabel)
	readyLabels := cloneLabels(claim.Labels)
	delete(readyLabels, claimStateLabel)
	delete(readyLabels, claimActiveUntilLabel)
	resolvedClaim, conflictClaimRemoved, actionSucceeded, createErr := resolveLeaseClaimAfterActionIfUnchanged(leaseID, claim, func() error {
		return transport.Create(createCtx, sandboxID, runOptions{
			AllowEgress:    b.cfg.CloudRunSandbox.AllowEgress,
			Write:          b.cfg.CloudRunSandbox.Write,
			Rootfs:         b.cfg.CloudRunSandbox.Rootfs,
			Workdir:        workdir,
			OwnershipToken: sandboxID,
		})
	}, func(actionErr error) (map[string]string, bool) {
		if actionErr == nil {
			return readyLabels, false
		}
		if errors.Is(actionErr, errSandboxAlreadyExists) {
			return conflictLabels, true
		}
		return recoveryLabels, false
	})
	if actionSucceeded && createErr != nil {
		rollbackClaim := claim
		if resolvedClaim.LeaseID != "" {
			rollbackClaim = resolvedClaim
		}
		rollbackCtx, rollbackCancel := b.cleanupContext(ctx)
		defer rollbackCancel()
		rollbackErr := b.releaseClaimedSandboxIfUnchanged(rollbackCtx, transport, sandboxID, rollbackClaim)
		claimed = false
		if rollbackErr == nil {
			return "", "", "", LeaseClaim{}, fmt.Errorf("cloud-run-sandbox create succeeded but publishing ready ownership failed; sandbox rolled back lease=%s: %w", leaseID, createErr)
		}
		fallbackLabels := cloneLabels(rollbackClaim.Labels)
		fallbackLabels[claimStateLabel] = "recovery"
		delete(fallbackLabels, claimActiveUntilLabel)
		_, recoveryErr := updateLeaseClaimLabelsIfUnchanged(leaseID, rollbackClaim, fallbackLabels)
		return "", "", "", LeaseClaim{}, errors.Join(
			fmt.Errorf("cloud-run-sandbox create succeeded but publishing ready ownership failed; recovery claim retained lease=%s: %w", leaseID, createErr),
			rollbackErr,
			recoveryErr,
		)
	}
	if createErr != nil {
		if errors.Is(createErr, errSandboxAlreadyExists) {
			if !conflictClaimRemoved {
				return "", "", "", LeaseClaim{}, fmt.Errorf("cloud-run-sandbox create conflict quarantined but provisional claim removal failed lease=%s: %w", leaseID, createErr)
			}
			claimed = false
			return "", "", "", LeaseClaim{}, fmt.Errorf("cloud-run-sandbox create rejected without taking ownership lease=%s: %w", leaseID, createErr)
		}
		claimed = false
		// Creation is not transactional: a timeout or lost response can arrive
		// after the deterministically named sandbox was created. Keep the exact,
		// scoped claim so stop/cleanup can recover it instead of leaving untracked
		// billable infrastructure.
		return "", "", "", LeaseClaim{}, fmt.Errorf("cloud-run-sandbox create remains indeterminate; recovery claim retained lease=%s: %w", leaseID, createErr)
	}
	claim = resolvedClaim
	claimed = false
	return leaseID, sandboxID, slug, claim, nil
}

func (b *backend) resolveLeaseID(id, repoRoot string, reclaim bool) (string, string, string, LeaseClaim, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", "", "", LeaseClaim{}, exit(2, "missing lease id")
	}
	scope, err := b.claimScope()
	if err != nil {
		return "", "", "", LeaseClaim{}, err
	}
	if claim, ok, err := resolveLeaseClaim(id); err != nil {
		return "", "", "", LeaseClaim{}, err
	} else if ok && claim.Provider == providerName {
		if claim.ProviderScope != scope {
			return "", "", "", LeaseClaim{}, exit(4, "cloud-run-sandbox lease %q belongs to a different gateway/cli scope", id)
		}
		if repoRoot != "" {
			claim, err = claimLeaseForRepoProviderScopePondIfUnchanged(claim.LeaseID, claim.Slug, providerName, scope, claim.Pond, repoRoot, time.Duration(claim.IdleTimeoutSeconds)*time.Second, reclaim, claim)
			if err != nil {
				return "", "", "", LeaseClaim{}, err
			}
		}
		return claim.LeaseID, strings.TrimPrefix(claim.LeaseID, leasePrefix), claim.Slug, claim, nil
	}
	// Accept raw sandbox id when an exact claim exists.
	leaseID := id
	if !strings.HasPrefix(leaseID, leasePrefix) {
		leaseID = leasePrefix + id
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		return "", "", "", LeaseClaim{}, exit(4, "cloud-run-sandbox sandbox %q is not claimed by Crabbox", id)
	}
	if claim.Provider != providerName {
		return "", "", "", LeaseClaim{}, exit(4, "cloud-run-sandbox sandbox %q is not claimed by Crabbox", id)
	}
	if claim.ProviderScope != scope {
		return "", "", "", LeaseClaim{}, exit(4, "cloud-run-sandbox lease %q belongs to a different gateway/cli scope", id)
	}
	if repoRoot != "" {
		claim, err = claimLeaseForRepoProviderScopePondIfUnchanged(claim.LeaseID, claim.Slug, providerName, scope, claim.Pond, repoRoot, time.Duration(claim.IdleTimeoutSeconds)*time.Second, reclaim, claim)
		if err != nil {
			return "", "", "", LeaseClaim{}, err
		}
	}
	return claim.LeaseID, strings.TrimPrefix(claim.LeaseID, leasePrefix), claim.Slug, claim, nil
}

func (b *backend) claimScope() (string, error) {
	if gateway := strings.TrimSpace(b.cfg.CloudRunSandbox.GatewayURL); gateway != "" {
		validated, err := validateGatewayURL(gateway)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256([]byte(validated))
		return "gateway:" + hex.EncodeToString(sum[:8]), nil
	}
	cli := blank(strings.TrimSpace(b.cfg.CloudRunSandbox.CLIPath), defaultCLIPath)
	sum := sha256.Sum256([]byte("direct:" + cli))
	return "direct:" + hex.EncodeToString(sum[:8]), nil
}

func (b *backend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}

func (b *backend) cleanupContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), cleanupTimeout)
}

func cloudRunSandboxWorkdir(cfg Config) (string, error) {
	workdir := blank(strings.TrimSpace(cfg.CloudRunSandbox.Workdir), defaultWorkdir)
	if !path.IsAbs(workdir) {
		return "", exit(2, "cloudRunSandbox.workdir must be an absolute path")
	}
	return workdir, nil
}

func newSandboxName(repo Repo) (string, error) {
	base := namePrefix
	if name := sanitizeName(path.Base(repo.Root)); name != "" {
		base = namePrefix + name + "-"
	}
	suffix, err := randomSuffix()
	if err != nil {
		return "", err
	}
	maxBase := maxSandboxNameLen - len(suffix) - 1
	if maxBase < len(namePrefix) {
		maxBase = len(namePrefix)
	}
	if len(base) > maxBase {
		base = base[:maxBase]
	}
	if !strings.HasSuffix(base, "-") {
		base += "-"
	}
	return base + suffix, nil
}

func sanitizeName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	re := regexp.MustCompile(`[^a-z0-9-]+`)
	value = re.ReplaceAllString(value, "-")
	value = strings.Trim(value, "-")
	return value
}

func randomSuffix() (string, error) {
	var buf [sandboxNameSuffix]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate cloud-run-sandbox ownership token: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

func doctorCheck(name string, err error, details map[string]string) DoctorCheck {
	if err != nil {
		return DoctorCheck{Status: "error", Check: name, Message: err.Error(), Details: details}
	}
	return DoctorCheck{Status: "ok", Check: name, Message: "ready", Details: details}
}
