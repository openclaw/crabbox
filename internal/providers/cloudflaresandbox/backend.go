package cloudflaresandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
)

const (
	cloudflareSandboxCleanupTimeout = 15 * time.Second

	metadataProviderKey = "crabbox.provider"
	metadataScopeKey    = "crabbox.scope"
	metadataClaimKey    = "crabbox.claim"
	metadataRepoKey     = "crabbox.repo"
	metadataSlugKey     = "crabbox.slug"
)

type backend struct {
	spec      ProviderSpec
	cfg       Config
	rt        Runtime
	newClient func(Config, Runtime) (bridgeClient, error)
}

func NewBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	return &backend{spec: spec, cfg: cfg, rt: rt, newClient: newBridgeClient}
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) client() (bridgeClient, error) {
	if b.newClient != nil {
		return b.newClient(b.cfg, b.rt)
	}
	return newBridgeClient(b.cfg, b.rt)
}

func (b *backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	api, err := b.client()
	if err != nil {
		return DoctorResult{}, err
	}
	checks := []DoctorCheck{}
	health, err := api.Health(ctx)
	if err != nil {
		checks = append(checks, DoctorCheck{Status: "failed", Check: "health", Message: redactSecrets(err.Error()), Details: map[string]string{"mutation": "false"}})
	} else {
		checks = append(checks, DoctorCheck{Status: "ok", Check: "health", Message: fmt.Sprintf("bridge=ready ok=%t mutation=false", health.OK), Details: map[string]string{"mutation": "false"}})
	}
	openapi, err := api.OpenAPI(ctx)
	if err != nil {
		checks = append(checks, DoctorCheck{Status: "warning", Check: "openapi", Message: redactSecrets(err.Error()), Details: map[string]string{"mutation": "false"}})
	} else {
		checks = append(checks, DoctorCheck{Status: "ok", Check: "openapi", Message: fmt.Sprintf("openapi=ready title=%s mutation=false", blank(openapi.Info.Title, "-")), Details: map[string]string{"mutation": "false"}})
	}
	return DoctorResult{
		Provider: providerName,
		Status:   aggregateStatus(checks),
		Message:  "bridge=checked mutation=false",
		Checks:   checks,
	}, nil
}

func (b *backend) Warmup(ctx context.Context, req WarmupRequest) error {
	if req.ActionsRunner {
		return exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	if req.Options.Tailscale.Enabled {
		return exit(2, "provider=%s is delegated-run only and does not support Tailscale options", providerName)
	}
	workdir, err := cloudflareSandboxWorkdir(b.cfg)
	if err != nil {
		return err
	}
	started := b.now()
	api, err := b.client()
	if err != nil {
		return err
	}
	leaseID, sandboxID, slug, unlockOperation, err := b.createSandbox(ctx, api, req.Repo, req.Reclaim, req.RequestedSlug)
	if err != nil {
		return err
	}
	defer unlockOperation()
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s sandbox=%s workdir=%s\n", leaseID, slug, providerName, sandboxID, workdir)
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: cloudflare-sandbox warmup keeps the sandbox until explicit stop\n")
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

func (b *backend) Run(ctx context.Context, req RunRequest) (result RunResult, retErr error) {
	if req.Options.Tailscale.Enabled {
		return RunResult{}, exit(2, "provider=%s is delegated-run only and does not support Tailscale options", providerName)
	}
	workdir, err := cloudflareSandboxWorkdir(b.cfg)
	if err != nil {
		return RunResult{}, err
	}
	started := b.now()
	api, err := b.client()
	if err != nil {
		return RunResult{}, err
	}

	leaseID, sandboxID, slug := "", "", ""
	acquired := false
	var unlockOperation func()
	defer func() {
		if unlockOperation != nil {
			unlockOperation()
		}
	}()
	if req.ID == "" {
		leaseID, sandboxID, slug, unlockOperation, err = b.createSandbox(ctx, api, req.Repo, req.Reclaim, req.RequestedSlug)
		if err != nil {
			return RunResult{}, err
		}
		acquired = true
		fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s sandbox=%s\n", leaseID, slug, providerName, sandboxID)
	} else {
		leaseID, sandboxID, slug, err = b.resolveLeaseID(req.ID, req.Repo.Root, req.Reclaim, b.cfg.IdleTimeout)
		if err != nil {
			return RunResult{}, err
		}
		if _, err := b.verifyClaim(ctx, api, leaseID, sandboxID); err != nil {
			return RunResult{}, err
		}
	}
	shouldStop := acquired && !req.Keep
	if shouldStop {
		defer func() {
			if cleanupErr := b.cleanupCreatedRun(ctx, api, leaseID, sandboxID, &shouldStop); cleanupErr != nil {
				if result.ExitCode == 0 {
					result.ExitCode = 1
				}
				if retErr == nil {
					retErr = exit(1, "%v", cleanupErr)
				} else {
					retErr = errors.Join(retErr, cleanupErr)
				}
			}
		}()
	}
	fmt.Fprintf(b.rt.Stderr, "provider=%s lease=%s sandbox=%s workdir=%s\n", providerName, leaseID, sandboxID, workdir)

	syncDuration := time.Duration(0)
	syncPhases := []timingPhase{{Name: "sync", Skipped: true, Reason: "--no-sync"}}
	if !req.NoSync {
		syncPhases, syncDuration, err = b.syncWorkspace(ctx, api, sandboxID, req, workdir)
		if err != nil {
			handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
			return RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, Total: b.now().Sub(started), SyncDelegated: true}, err
		}
		fmt.Fprintf(b.rt.Stderr, "sync complete in %s\n", syncDuration.Round(time.Millisecond))
	} else if err := b.ensureWorkspace(ctx, api, sandboxID, workdir); err != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return RunResult{}, err
	}

	if req.SyncOnly {
		result = RunResult{
			Provider:      providerName,
			LeaseID:       leaseID,
			Slug:          slug,
			Total:         b.now().Sub(started),
			SyncDelegated: true,
		}
		fmt.Fprintf(b.rt.Stdout, "synced %s\n", workdir)
		activityErr := b.refreshLeaseActivityIfRetained(leaseID, shouldStop)
		if cleanupErr := b.cleanupCreatedRun(ctx, api, leaseID, sandboxID, &shouldStop); cleanupErr != nil {
			result.ExitCode = 1
			return result, cleanupErr
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
				TotalMs:       result.Total.Milliseconds(),
				ExitCode:      result.ExitCode,
				Label:         strings.TrimSpace(req.Label),
			}); err != nil {
				return result, err
			}
		}
		return result, activityErr
	}

	command, err := buildCommand(req.Command, req.ShellMode)
	if err != nil {
		return RunResult{}, err
	}
	commandText := commandScript(command)
	commandEnv, strippedAuthEnv := cloudflareSandboxCommandEnv(req.Env)
	if len(strippedAuthEnv) > 0 {
		fmt.Fprintf(b.rt.Stderr, "warning: provider=%s did not forward provider authentication variables: %s\n", providerName, strings.Join(strippedAuthEnv, ","))
	}
	if req.EnvSummary || strings.TrimSpace(os.Getenv("CRABBOX_ENV_ALLOW")) != "" {
		printEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, commandEnv)
	}
	commandStart := b.now()
	execRes, runErr := api.Exec(ctx, sandboxID, execRequest{
		Command:     commandText,
		WorkingDir:  workdir,
		Env:         commandEnv,
		TimeoutSecs: b.execTimeoutSecs(),
	}, b.rt.Stdout, b.rt.Stderr)
	commandDuration := b.now().Sub(commandStart)
	result = RunResult{
		Provider:      providerName,
		LeaseID:       leaseID,
		Slug:          slug,
		CommandText:   commandText,
		ExitCode:      execRes.ExitCode,
		Command:       commandDuration,
		Total:         b.now().Sub(started),
		SyncDelegated: true,
	}
	if req.NoSync {
		fmt.Fprintf(b.rt.Stderr, "cloudflare-sandbox run summary sync_skipped=true command=%s total=%s exit=%d\n", result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), result.ExitCode)
	} else {
		fmt.Fprintf(b.rt.Stderr, "cloudflare-sandbox run summary sync=%s command=%s total=%s exit=%d\n", syncDuration.Round(time.Millisecond), result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), result.ExitCode)
	}
	var commandErr error
	if runErr != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		if result.ExitCode == 0 {
			result.ExitCode = 1
		}
		commandErr = ExitError{Code: 1, Message: fmt.Sprintf("cloudflare-sandbox run failed: %v", redactSecrets(runErr.Error()))}
	} else if result.ExitCode != 0 {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		commandErr = ExitError{Code: result.ExitCode, Message: fmt.Sprintf("cloudflare-sandbox run exited %d", result.ExitCode)}
	}
	activityErr := b.refreshLeaseActivityIfRetained(leaseID, shouldStop)
	if activityErr != nil && commandErr == nil {
		result.ExitCode = 1
	}
	if cleanupErr := b.cleanupCreatedRun(ctx, api, leaseID, sandboxID, &shouldStop); cleanupErr != nil {
		if result.ExitCode == 0 {
			result.ExitCode = 1
		}
		commandErr = errors.Join(commandErr, cleanupErr)
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
			ExitCode:      result.ExitCode,
			Label:         strings.TrimSpace(req.Label),
		}); err != nil {
			return result, err
		}
	}
	if commandErr != nil {
		return result, commandErr
	}
	return result, activityErr
}

func (b *backend) List(ctx context.Context, _ ListRequest) ([]LeaseView, error) {
	api, err := b.client()
	if err != nil {
		return nil, err
	}
	sandboxes, err := api.ListSandboxes(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]LeaseView, 0, len(sandboxes))
	for _, sb := range sandboxes {
		leaseID := strings.TrimSpace(sb.Metadata[metadataClaimKey])
		if leaseID == "" && strings.HasPrefix(sb.ID, leasePrefix) {
			leaseID = sb.ID
			remote, err := api.GetSandbox(ctx, strings.TrimPrefix(leaseID, leasePrefix))
			if err != nil {
				if isCloudflareSandboxNotFound(err) {
					continue
				}
				return nil, err
			}
			sb = remote
			leaseID = strings.TrimSpace(sb.Metadata[metadataClaimKey])
		}
		if leaseID == "" {
			continue
		}
		claim, err := readLeaseClaim(leaseID)
		if err != nil {
			return nil, err
		}
		if claim.LeaseID == "" || claim.Provider != providerName {
			continue
		}
		if err := b.validateClaimScope(claim); err != nil {
			return nil, err
		}
		if err := validateSandboxOwnership(claim, sb); err != nil {
			return nil, err
		}
		views = append(views, b.serverFromSandbox(claim, sb))
	}
	return views, nil
}

func (b *backend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	api, err := b.client()
	if err != nil {
		return StatusView{}, err
	}
	leaseID, sandboxID, slug, err := b.resolveLeaseID(req.ID, "", false, 0)
	if err != nil {
		return StatusView{}, err
	}
	claim, ok, err := b.resolveCloudflareSandboxLeaseClaim(leaseID)
	if err != nil {
		return StatusView{}, err
	}
	if !ok {
		return StatusView{}, exit(4, "cloudflare-sandbox sandbox %q is not claimed by Crabbox", req.ID)
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
			if req.Wait && ctx.Err() == nil && pollCtx.Err() != nil {
				return StatusView{}, exit(5, "timed out waiting for cloudflare-sandbox sandbox %s to become ready", sandboxID)
			}
			if ctx.Err() != nil {
				return StatusView{}, ctx.Err()
			}
			return StatusView{}, getErr
		}
		if err := validateSandboxOwnership(claim, sb); err != nil {
			return StatusView{}, err
		}
		state := normalizedSandboxState(sb)
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
				"slug":     slug,
				"pond":     claim.Pond,
				"state":    state,
			},
		}
		if !req.Wait || view.Ready {
			return view, nil
		}
		if isTerminalState(state) {
			return StatusView{}, exit(5, "cloudflare-sandbox sandbox %s entered terminal state %q before becoming ready", sandboxID, state)
		}
		if b.now().After(deadline) {
			return StatusView{}, exit(5, "timed out waiting for cloudflare-sandbox sandbox %s to become ready", sandboxID)
		}
		select {
		case <-pollCtx.Done():
			if ctx.Err() == nil {
				return StatusView{}, exit(5, "timed out waiting for cloudflare-sandbox sandbox %s to become ready", sandboxID)
			}
			return StatusView{}, pollCtx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *backend) Stop(ctx context.Context, req StopRequest) error {
	api, err := b.client()
	if err != nil {
		return err
	}
	leaseID, sandboxID, _, err := b.resolveLeaseID(req.ID, "", false, 0)
	if err != nil {
		return err
	}
	if _, err := b.verifyClaim(ctx, api, leaseID, sandboxID); err != nil {
		if !isCloudflareSandboxNotFound(err) || !b.cfg.CloudflareSandbox.ForgetMissing {
			return err
		}
		fmt.Fprintf(b.rt.Stderr, "warning: forgetting missing cloudflare-sandbox sandbox=%s after explicit request\n", sandboxID)
		removeLeaseClaim(leaseID)
		return nil
	}
	if err := api.DeleteSandbox(ctx, sandboxID); err != nil {
		if !isCloudflareSandboxNotFound(err) || !b.cfg.CloudflareSandbox.ForgetMissing {
			return err
		}
		fmt.Fprintf(b.rt.Stderr, "warning: forgetting missing cloudflare-sandbox sandbox=%s after explicit request\n", sandboxID)
	}
	removeLeaseClaim(leaseID)
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s\n", leaseID, sandboxID)
	return nil
}

func (b *backend) Cleanup(ctx context.Context, req CleanupRequest) error {
	api, err := b.client()
	if err != nil {
		return err
	}
	claims, err := listCloudflareSandboxLeaseClaims()
	if err != nil {
		return err
	}
	hasProviderClaims := slices.ContainsFunc(claims, func(claim LeaseClaim) bool {
		return claim.Provider == providerName
	})
	if !hasProviderClaims {
		if !req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "%s cleanup removed=0 claims_removed=0 checked=0\n", providerName)
		}
		return nil
	}
	now := b.now().UTC()
	checked := 0
	removed := 0
	claimsRemoved := 0
	for _, listed := range claims {
		if listed.Provider != providerName || !b.claimMatchesActiveScope(listed) {
			continue
		}
		claim, err := readLeaseClaim(listed.LeaseID)
		if err != nil {
			return err
		}
		if claim.LeaseID == "" || claim.Provider != providerName || !b.claimMatchesActiveScope(claim) {
			continue
		}
		checked++
		sandboxID := strings.TrimPrefix(claim.LeaseID, leasePrefix)
		sb, getErr := api.GetSandbox(ctx, sandboxID)
		if getErr != nil {
			if !isCloudflareSandboxNotFound(getErr) {
				return getErr
			}
			if !b.cfg.CloudflareSandbox.ForgetMissing {
				fmt.Fprintf(b.rt.Stderr, "skip sandbox=%s lease=%s reason=missing-or-inaccessible; set cloudflareSandbox.forgetMissing to remove the claim\n", sandboxID, claim.LeaseID)
				continue
			}
			if req.DryRun {
				fmt.Fprintf(b.rt.Stdout, "would remove claim lease=%s slug=%s reason=missing sandbox\n", claim.LeaseID, blank(claim.Slug, "-"))
				continue
			}
			if err := removeLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
				return err
			}
			fmt.Fprintf(b.rt.Stdout, "remove claim lease=%s slug=%s reason=missing sandbox\n", claim.LeaseID, blank(claim.Slug, "-"))
			claimsRemoved++
			continue
		}
		due, reason := claimCleanupDue(claim, now)
		if !due {
			fmt.Fprintf(b.rt.Stderr, "skip sandbox=%s lease=%s reason=%s\n", sandboxID, claim.LeaseID, reason)
			continue
		}
		if err := validateSandboxOwnership(claim, sb); err != nil {
			return err
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would delete sandbox=%s lease=%s reason=%s\n", sandboxID, claim.LeaseID, reason)
			continue
		}
		if err := api.DeleteSandbox(ctx, sandboxID); err != nil && !isCloudflareSandboxNotFound(err) {
			return err
		}
		if err := removeLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
			return err
		}
		fmt.Fprintf(b.rt.Stdout, "delete sandbox=%s lease=%s reason=%s\n", sandboxID, claim.LeaseID, reason)
		removed++
	}
	if !req.DryRun {
		fmt.Fprintf(b.rt.Stdout, "%s cleanup removed=%d claims_removed=%d checked=%d\n", providerName, removed, claimsRemoved, checked)
	}
	return nil
}

func (b *backend) createSandbox(ctx context.Context, api bridgeClient, repo Repo, reclaim bool, requestedSlug string) (string, string, string, func(), error) {
	if err := validateProviderConfig(b.cfg); err != nil {
		return "", "", "", nil, err
	}
	providerScope, err := b.newClaimScope()
	if err != nil {
		return "", "", "", nil, err
	}
	name := newSandboxName(repo)
	leaseID := leasePrefix + name
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return "", "", "", nil, err
	}
	metadata := b.ownershipMetadata(providerScope, leaseID, slug, repo)
	sb, err := api.CreateSandbox(ctx, createSandboxRequest{
		Name:     name,
		Workdir:  b.cfg.CloudflareSandbox.Workdir,
		Metadata: metadata,
	})
	if err != nil {
		return "", "", "", nil, err
	}
	if sb.ID == "" {
		return "", "", "", nil, b.cleanupCreateFailure(ctx, api, "", exit(5, "cloudflare-sandbox create returned no sandbox id"))
	}
	if sb.Metadata == nil {
		sb.Metadata = metadata
	}
	if sb.ID != name && sb.Metadata[metadataClaimKey] != leasePrefix+sb.ID {
		return "", "", "", nil, b.cleanupCreateFailure(ctx, api, sb.ID, exit(5, "cloudflare-sandbox create returned id %q but bridge did not persist matching ownership metadata", sb.ID))
	}
	if sb.ID != name {
		leaseID = leasePrefix + sb.ID
		slug, err = allocateClaimLeaseSlug(leaseID, requestedSlug)
		if err != nil {
			return "", "", "", nil, b.cleanupCreateFailure(ctx, api, sb.ID, err)
		}
	}
	if err := validateSandboxOwnership(LeaseClaim{LeaseID: leaseID, Provider: providerName, ProviderScope: providerScope}, sb); err != nil {
		return "", "", "", nil, b.cleanupCreateFailure(ctx, api, sb.ID, err)
	}
	if err := claimLeaseForRepoProviderScopePond(leaseID, slug, providerName, providerScope, b.cfg.Pond, repo.Root, b.cfg.IdleTimeout, reclaim); err != nil {
		return "", "", "", nil, b.cleanupCreateFailure(ctx, api, sb.ID, err)
	}
	return leaseID, sb.ID, slug, func() {}, nil
}

func (b *backend) ownershipMetadata(providerScope, leaseID, slug string, repo Repo) map[string]string {
	out := map[string]string{
		metadataProviderKey: providerName,
		metadataScopeKey:    providerScope,
		metadataRepoKey:     repoScope(repo),
	}
	if leaseID != "" {
		out[metadataClaimKey] = leaseID
	}
	if slug != "" {
		out[metadataSlugKey] = slug
	}
	return out
}

func (b *backend) serverFromSandbox(claim LeaseClaim, sb sandboxSummary) Server {
	state := normalizedSandboxState(sb)
	return Server{
		Provider: providerName,
		CloudID:  sb.ID,
		Name:     sb.ID,
		Status:   state,
		Labels: map[string]string{
			"provider": providerName,
			"lease":    claim.LeaseID,
			"slug":     claim.Slug,
			"pond":     claim.Pond,
			"target":   targetLinux,
			"state":    state,
		},
	}
}

func (b *backend) resolveLeaseID(id, repoRoot string, reclaim bool, idleTimeout time.Duration) (string, string, string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", "", "", exit(2, "provider=cloudflare-sandbox requires a Crabbox-created sandbox slug or lease id")
	}
	exactLeaseID := id
	if !strings.HasPrefix(exactLeaseID, leasePrefix) {
		exactLeaseID = leasePrefix + exactLeaseID
	}
	if claim, err := readLeaseClaim(exactLeaseID); err != nil {
		return "", "", "", err
	} else if claim.LeaseID == exactLeaseID && claim.Provider == providerName {
		return b.finishResolvedLease(claim, repoRoot, reclaim, idleTimeout)
	}
	claim, ok, err := b.resolveCloudflareSandboxLeaseClaim(id)
	if err != nil {
		return "", "", "", err
	}
	if ok {
		return b.finishResolvedLease(claim, repoRoot, reclaim, idleTimeout)
	}
	return "", "", "", exit(4, "cloudflare-sandbox sandbox %q is not claimed by Crabbox; use a Crabbox slug or %s<sandbox-id>", id, leasePrefix)
}

func (b *backend) resolveCloudflareSandboxLeaseClaim(identifier string) (LeaseClaim, bool, error) {
	claims, err := listCloudflareSandboxLeaseClaims()
	if err != nil {
		return LeaseClaim{}, false, err
	}
	for _, claim := range claims {
		if claim.Provider == providerName && claim.LeaseID == identifier {
			if err := b.validateClaimScope(claim); err != nil {
				return LeaseClaim{}, false, err
			}
			return claim, true, nil
		}
	}
	slug := normalizeLeaseSlug(identifier)
	if slug != "" {
		for _, claim := range claims {
			if claim.Provider != providerName || normalizeLeaseSlug(claim.Slug) != slug || !b.claimMatchesActiveScope(claim) {
				continue
			}
			return claim, true, nil
		}
	}
	return LeaseClaim{}, false, nil
}

func (b *backend) finishResolvedLease(claim LeaseClaim, repoRoot string, reclaim bool, idleTimeout time.Duration) (string, string, string, error) {
	if err := b.validateClaimScope(claim); err != nil {
		return "", "", "", err
	}
	if repoRoot != "" {
		timeout := idleTimeout
		if timeout <= 0 && claim.IdleTimeoutSeconds > 0 {
			timeout = time.Duration(claim.IdleTimeoutSeconds) * time.Second
		}
		if err := claimLeaseForRepoProviderScopePond(claim.LeaseID, claim.Slug, providerName, claim.ProviderScope, claim.Pond, repoRoot, timeout, reclaim); err != nil {
			return "", "", "", err
		}
	}
	slug := claim.Slug
	if strings.TrimSpace(slug) == "" {
		slug = newLeaseSlug(claim.LeaseID)
	}
	return claim.LeaseID, strings.TrimPrefix(claim.LeaseID, leasePrefix), slug, nil
}

func (b *backend) newClaimScope() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", exit(5, "generate cloudflare-sandbox ownership token: %v", err)
	}
	return b.providerScopeBase() + "/ownership:" + hex.EncodeToString(token[:]), nil
}

func (b *backend) providerScopeBase() string {
	bridge, err := bridgeURL(b.cfg)
	if err != nil {
		bridge = "unconfigured"
	}
	return "bridge:" + bridge
}

func (b *backend) validateClaimScope(claim LeaseClaim) error {
	if !b.claimMatchesActiveScope(claim) {
		return exit(4, "cloudflare-sandbox lease %q belongs to a different bridge scope; restore the configuration used to create it", claim.LeaseID)
	}
	return nil
}

func (b *backend) claimMatchesActiveScope(claim LeaseClaim) bool {
	return strings.HasPrefix(strings.TrimSpace(claim.ProviderScope), b.providerScopeBase()+"/ownership:")
}

func (b *backend) verifyClaim(ctx context.Context, api bridgeClient, leaseID, sandboxID string) (sandboxSummary, error) {
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		return sandboxSummary{}, err
	}
	if err := b.validateClaimScope(claim); err != nil {
		return sandboxSummary{}, err
	}
	sb, err := api.GetSandbox(ctx, sandboxID)
	if err != nil {
		return sandboxSummary{}, err
	}
	if err := validateSandboxOwnership(claim, sb); err != nil {
		return sandboxSummary{}, err
	}
	return sb, nil
}

func validateSandboxOwnership(claim LeaseClaim, sb sandboxSummary) error {
	if sb.ID == "" {
		return exit(5, "cloudflare-sandbox returned a sandbox without an id")
	}
	if sb.Metadata[metadataProviderKey] != providerName ||
		sb.Metadata[metadataScopeKey] != claim.ProviderScope ||
		sb.Metadata[metadataClaimKey] != claim.LeaseID {
		return exit(4, "cloudflare-sandbox sandbox %q ownership metadata does not match its local claim", sb.ID)
	}
	return nil
}

func claimCleanupDue(claim LeaseClaim, now time.Time) (bool, string) {
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

func (b *backend) refreshLeaseActivity(leaseID string) error {
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		return err
	}
	if claim.LeaseID == "" {
		return nil
	}
	idleTimeout := b.cfg.IdleTimeout
	if idleTimeout <= 0 && claim.IdleTimeoutSeconds > 0 {
		idleTimeout = time.Duration(claim.IdleTimeoutSeconds) * time.Second
	}
	return claimLeaseForRepoProviderScopePond(claim.LeaseID, claim.Slug, providerName, claim.ProviderScope, claim.Pond, claim.RepoRoot, idleTimeout, false)
}

func (b *backend) refreshLeaseActivityIfRetained(leaseID string, shouldStop bool) error {
	if shouldStop {
		return nil
	}
	return b.refreshLeaseActivity(leaseID)
}

func (b *backend) cleanupCreateFailure(ctx context.Context, api bridgeClient, sandboxID string, cause error) error {
	if sandboxID == "" {
		return cause
	}
	cleanupCtx, cancel := b.cleanupContext(ctx)
	defer cancel()
	if err := api.DeleteSandbox(cleanupCtx, sandboxID); err != nil {
		if isCloudflareSandboxNotFound(err) {
			return cause
		}
		return errors.Join(cause, fmt.Errorf("cloudflare-sandbox cleanup failed for sandbox %s; delete it in the Cloudflare dashboard: %w", sandboxID, err))
	}
	return cause
}

func (b *backend) cleanupCreatedRun(ctx context.Context, api bridgeClient, leaseID, sandboxID string, shouldStop *bool) error {
	if !*shouldStop {
		return nil
	}
	*shouldStop = false
	cleanupCtx, cancel := b.cleanupContext(ctx)
	defer cancel()
	if err := api.DeleteSandbox(cleanupCtx, sandboxID); err != nil && !isCloudflareSandboxNotFound(err) {
		return fmt.Errorf("cloudflare-sandbox delete failed for %s: %w", sandboxID, err)
	}
	removeLeaseClaim(leaseID)
	return nil
}

func (b *backend) cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), cloudflareSandboxCleanupTimeout)
}

func (b *backend) execTimeoutSecs() int {
	return b.cfg.CloudflareSandbox.ExecTimeoutSecs
}

func (b *backend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}

func normalizedSandboxState(sb sandboxSummary) string {
	return strings.ToLower(blank(strings.TrimSpace(sb.Status), "unknown"))
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
	case "terminated", "stopped", "failed", "error", "aborted", "killed", "deleted", "destroyed":
		return true
	default:
		return false
	}
}

func repoScope(repo Repo) string {
	if strings.TrimSpace(repo.RemoteURL) != "" {
		return strings.TrimSpace(repo.RemoteURL)
	}
	if strings.TrimSpace(repo.Root) != "" {
		return strings.TrimSpace(repo.Root)
	}
	return strings.TrimSpace(repo.Name)
}

func newSandboxName(repo Repo) string {
	source := repo.Name
	if strings.TrimSpace(source) == "" {
		source = "sandbox"
	}
	var b strings.Builder
	for _, r := range strings.ToLower(source) {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-':
			b.WriteRune('-')
		default:
			b.WriteRune('-')
		}
	}
	base := strings.Trim(b.String(), "-")
	if base == "" {
		base = "sandbox"
	}
	if len(base) > 28 {
		base = strings.Trim(base[:28], "-")
	}
	var token [4]byte
	if _, err := rand.Read(token[:]); err == nil {
		return base + "-" + hex.EncodeToString(token[:])
	}
	return fmt.Sprintf("%s-%x", base, time.Now().UnixNano()&0xffffffff)
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

func commandScript(command []string) string {
	return shellScriptFromArgv(command)
}

type cloudflareSandboxNotFoundError struct {
	err error
}

func (e *cloudflareSandboxNotFoundError) Error() string { return e.err.Error() }
func (e *cloudflareSandboxNotFoundError) Unwrap() error { return e.err }

func isCloudflareSandboxNotFound(err error) bool {
	var notFound *cloudflareSandboxNotFoundError
	if errors.As(err, &notFound) {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "not found") || strings.Contains(text, "404")
}

func aggregateStatus(checks []DoctorCheck) string {
	for _, check := range checks {
		if check.Status == "failed" {
			return "failed"
		}
	}
	for _, check := range checks {
		if check.Status == "warning" {
			return "warning"
		}
	}
	return "ok"
}
