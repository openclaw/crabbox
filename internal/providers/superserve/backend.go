package superserve

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"time"
)

const (
	superserveCleanupTimeout = 15 * time.Second
	statusViewReady          = "running"
	NetworkPublic            = "public"

	metadataProviderKey = "crabbox.provider"
	metadataEndpointKey = "crabbox.endpoint"
	metadataScopeKey    = "crabbox.scope"
	metadataClaimKey    = "crabbox.claim"
	metadataRepoKey     = "crabbox.repo"
	metadataPondKey     = "crabbox.pond"
	metadataSlugKey     = "crabbox.slug"
	metadataNameKey     = "crabbox.name"
)

func NewSuperserveBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	return &backend{spec: spec, cfg: cfg, rt: rt, newClient: newSuperserveClient}
}

type backend struct {
	spec                   ProviderSpec
	cfg                    Config
	rt                     Runtime
	newClient              func(Config, Runtime) (superserveClient, error)
	cleanupTimeoutOverride time.Duration
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) client() (superserveClient, error) {
	if b.newClient != nil {
		return b.newClient(b.cfg, b.rt)
	}
	return newSuperserveClient(b.cfg, b.rt)
}

func (b *backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
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

func (b *backend) Warmup(ctx context.Context, req WarmupRequest) error {
	if req.ActionsRunner {
		return exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	if req.Options.Tailscale.Enabled {
		return exit(2, "provider=superserve is delegated-run only and does not support Tailscale options")
	}
	if _, err := superserveWorkdir(b.cfg); err != nil {
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
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s sandbox=%s\n", leaseID, slug, providerName, sandboxID)
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: superserve warmup keeps the sandbox until explicit stop\n")
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
		return RunResult{}, exit(2, "provider=superserve is delegated-run only and does not support Tailscale options")
	}
	workdir, err := superserveWorkdir(b.cfg)
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
		fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s sandbox=%s\n", leaseID, slug, providerName, sandboxID)
		acquired = true
	} else {
		leaseID, sandboxID, _, err = resolveLeaseID(req.ID, "", false, 0, api.BaseURL())
		if err != nil {
			return RunResult{}, err
		}
		unlockOperation, err = lockSuperserveLeaseOperation(ctx, leaseID)
		if err != nil {
			return RunResult{}, err
		}
		leaseID, sandboxID, _, err = resolveLeaseID(leaseID, "", false, 0, api.BaseURL())
		if err != nil {
			return RunResult{}, err
		}
		if _, err := verifySuperserveClaim(ctx, api, leaseID, sandboxID); err != nil {
			return RunResult{}, err
		}
		claim, err := readLeaseClaim(leaseID)
		if err != nil {
			return RunResult{}, err
		}
		_, _, slug, err = finishResolvedLease(claim, req.Repo.Root, req.Reclaim, b.cfg.IdleTimeout, api.BaseURL())
		if err != nil {
			return RunResult{}, err
		}
	}
	shouldStop := acquired && !req.Keep
	access, err := api.ActivateSandbox(ctx, sandboxID)
	if err != nil {
		if acquired {
			handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
			if shouldStop {
				return RunResult{}, b.cleanupClaimedRunFailure(ctx, api, leaseID, sandboxID, err)
			}
		}
		return RunResult{}, err
	}
	if access.Sandbox.ID == "" {
		access.Sandbox.ID = sandboxID
	}
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
		syncPhases, syncDuration, err = b.syncWorkspace(ctx, api, &access, req, workdir)
		if err != nil {
			handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
			return RunResult{Total: b.now().Sub(started), SyncDelegated: true}, err
		}
		fmt.Fprintf(b.rt.Stderr, "sync complete in %s\n", syncDuration.Round(time.Millisecond))
	} else if err := b.ensureWorkspace(ctx, api, &access, workdir); err != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return RunResult{}, err
	}

	if req.SyncOnly {
		result := RunResult{
			Provider:      providerName,
			LeaseID:       leaseID,
			Slug:          slug,
			Total:         b.now().Sub(started),
			SyncDelegated: true,
		}
		fmt.Fprintf(b.rt.Stdout, "synced %s\n", workdir)
		activityErr := b.refreshSuperserveActivityIfRetained(leaseID, shouldStop)
		if activityErr != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: refresh superserve lease activity failed lease=%s: %v\n", leaseID, activityErr)
			result.ExitCode = 1
		}
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
		if activityErr != nil {
			return result, activityErr
		}
		return result, nil
	}

	command, err := buildCommand(req.Command, req.ShellMode)
	if err != nil {
		return RunResult{}, err
	}
	commandText := commandScript(command)
	commandEnv, strippedAuthEnv := superserveCommandEnv(req.Env)
	if len(strippedAuthEnv) > 0 {
		fmt.Fprintf(b.rt.Stderr, "warning: provider=superserve did not forward provider authentication variables: %s\n", strings.Join(strippedAuthEnv, ","))
	}
	if req.EnvSummary || strings.TrimSpace(os.Getenv("CRABBOX_ENV_ALLOW")) != "" {
		printEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, commandEnv)
	}
	commandStart := b.now()
	execRes, runErr := api.Exec(ctx, &access, execRequest{
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
		fmt.Fprintf(b.rt.Stderr, "superserve run summary sync_skipped=true command=%s total=%s exit=%d\n",
			result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), result.ExitCode)
	} else {
		fmt.Fprintf(b.rt.Stderr, "superserve run summary sync=%s command=%s total=%s exit=%d\n",
			syncDuration.Round(time.Millisecond), result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), result.ExitCode)
	}
	var commandErr error
	if runErr != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		if result.ExitCode == 0 {
			result.ExitCode = 1
		}
		commandErr = ExitError{Code: 1, Message: fmt.Sprintf("superserve run failed: %v", runErr)}
	} else if result.ExitCode != 0 {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		commandErr = ExitError{Code: result.ExitCode, Message: fmt.Sprintf("superserve run exited %d", result.ExitCode)}
	}
	activityErr := b.refreshSuperserveActivityIfRetained(leaseID, shouldStop)
	if activityErr != nil {
		fmt.Fprintf(b.rt.Stderr, "warning: refresh superserve lease activity failed lease=%s: %v\n", leaseID, activityErr)
		if commandErr == nil {
			result.ExitCode = 1
		}
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
	if activityErr != nil {
		return result, activityErr
	}
	return result, nil
}

func (b *backend) List(ctx context.Context, _ ListRequest) ([]LeaseView, error) {
	api, err := b.client()
	if err != nil {
		return nil, err
	}
	sandboxes, err := api.ListSandboxes(ctx, b.baseMetadataFilter(api.BaseURL()))
	if err != nil {
		return nil, err
	}
	views := make([]LeaseView, 0, len(sandboxes))
	for _, sb := range sandboxes {
		leaseID := strings.TrimSpace(sb.Metadata[metadataClaimKey])
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
		if err := validateSuperserveClaimScope(claim, api.BaseURL()); err != nil {
			return nil, err
		}
		if err := validateSuperserveSandboxOwnership(claim, sb); err != nil {
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
	leaseID, sandboxID, slug, err := resolveLeaseID(req.ID, "", false, 0, api.BaseURL())
	if err != nil {
		return StatusView{}, err
	}
	claim, ok, err := resolveSuperserveLeaseClaim(leaseID, api.BaseURL())
	if err != nil {
		return StatusView{}, err
	}
	if !ok {
		return StatusView{}, exit(4, "superserve sandbox %q is not claimed by Crabbox", req.ID)
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
				return StatusView{}, exit(5, "timed out waiting for superserve sandbox %s to become ready", sandboxID)
			}
			if ctx.Err() != nil {
				return StatusView{}, ctx.Err()
			}
			return StatusView{}, getErr
		}
		if err := validateSuperserveSandboxOwnership(claim, sb); err != nil {
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
			return StatusView{}, exit(5, "superserve sandbox %s entered terminal state %q before becoming ready", sandboxID, state)
		}
		if b.now().After(deadline) {
			return StatusView{}, exit(5, "timed out waiting for superserve sandbox %s to become ready", sandboxID)
		}
		select {
		case <-pollCtx.Done():
			if ctx.Err() == nil {
				return StatusView{}, exit(5, "timed out waiting for superserve sandbox %s to become ready", sandboxID)
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
	leaseID, _, _, err := resolveLeaseID(req.ID, "", false, 0, api.BaseURL())
	if err != nil {
		return err
	}
	unlockOperation, err := lockSuperserveLeaseOperation(ctx, leaseID)
	if err != nil {
		return err
	}
	defer unlockOperation()
	leaseID, sandboxID, _, err := resolveLeaseID(leaseID, "", false, 0, api.BaseURL())
	if err != nil {
		return err
	}
	if _, err := verifySuperserveClaim(ctx, api, leaseID, sandboxID); err != nil {
		if !isSuperserveNotFound(err) || !b.cfg.Superserve.ForgetMissing {
			return err
		}
		fmt.Fprintf(b.rt.Stderr, "warning: forgetting missing superserve sandbox=%s after explicit request\n", sandboxID)
		removeLeaseClaim(leaseID)
		return nil
	}
	if err := api.DeleteSandbox(ctx, sandboxID); err != nil {
		if !isSuperserveNotFound(err) || !b.cfg.Superserve.ForgetMissing {
			return err
		}
		fmt.Fprintf(b.rt.Stderr, "warning: forgetting missing superserve sandbox=%s after explicit request\n", sandboxID)
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
	claims, err := listSuperserveLeaseClaims()
	if err != nil {
		return err
	}
	now := b.now().UTC()
	checked := 0
	removed := 0
	claimsRemoved := 0
	for _, listed := range claims {
		if listed.Provider != providerName || !superserveClaimMatchesEndpoint(listed, api.BaseURL()) {
			continue
		}
		var removedOne, claimRemovedOne, checkedOne bool
		err := func() error {
			unlockOperation, err := lockSuperserveLeaseOperation(ctx, listed.LeaseID)
			if err != nil {
				return err
			}
			defer unlockOperation()
			claim, err := readLeaseClaim(listed.LeaseID)
			if err != nil {
				return err
			}
			if claim.LeaseID == "" || claim.Provider != providerName || !superserveClaimMatchesEndpoint(claim, api.BaseURL()) {
				return nil
			}
			checkedOne = true
			sandboxID := strings.TrimPrefix(claim.LeaseID, leasePrefix)
			sb, getErr := api.GetSandbox(ctx, sandboxID)
			if getErr != nil {
				if !isSuperserveNotFound(getErr) {
					return getErr
				}
				if !b.cfg.Superserve.ForgetMissing {
					fmt.Fprintf(b.rt.Stderr, "skip sandbox=%s lease=%s reason=missing-or-inaccessible; set superserve forget-missing to remove the claim\n", sandboxID, claim.LeaseID)
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
			due, reason := superserveClaimCleanupDue(claim, now)
			if !due {
				fmt.Fprintf(b.rt.Stderr, "skip sandbox=%s lease=%s reason=%s\n", sandboxID, claim.LeaseID, reason)
				return nil
			}
			if err := validateSuperserveSandboxOwnership(claim, sb); err != nil {
				return err
			}
			if req.DryRun {
				fmt.Fprintf(b.rt.Stdout, "would delete sandbox=%s lease=%s reason=%s\n", sandboxID, claim.LeaseID, reason)
				return nil
			}
			if err := api.DeleteSandbox(ctx, sandboxID); err != nil && !isSuperserveNotFound(err) {
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

func (b *backend) createSandbox(ctx context.Context, api superserveClient, repo Repo, reclaim bool, requestedSlug string) (string, string, string, func(), error) {
	if err := validateSuperserveConfig(b.cfg); err != nil {
		return "", "", "", nil, err
	}
	providerScope, err := newSuperserveClaimScope(api.BaseURL())
	if err != nil {
		return "", "", "", nil, err
	}
	if _, err := superserveWorkdir(b.cfg); err != nil {
		return "", "", "", nil, err
	}
	initialMetadata := b.ownershipMetadata(api.BaseURL(), providerScope, "", "", repo)
	fromTemplate, fromSnapshot := superserveCreateSource(b.cfg)
	sb, err := api.CreateSandbox(ctx, createSandboxRequest{
		Name:           newSandboxName(repo),
		FromTemplate:   fromTemplate,
		FromSnapshot:   fromSnapshot,
		TimeoutSeconds: b.sandboxTimeoutSecs(),
		Metadata:       initialMetadata,
		Network:        superserveNetworkConfig(b.cfg),
	})
	if err != nil {
		return "", "", "", nil, err
	}
	leaseID := leasePrefix + sb.ID
	unlockOperation, err := lockSuperserveLeaseOperation(ctx, leaseID)
	if err != nil {
		return leaseID, sb.ID, "", nil, b.cleanupCreateFailure(ctx, api, sb.ID, err)
	}
	keepLock := false
	defer func() {
		if !keepLock {
			unlockOperation()
		}
	}()
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return leaseID, sb.ID, "", nil, b.cleanupCreateFailure(ctx, api, sb.ID, err)
	}
	metadata := b.ownershipMetadata(api.BaseURL(), providerScope, leaseID, slug, repo)
	sb, err = api.UpdateSandboxMetadata(ctx, sb.ID, metadata)
	if err != nil {
		return leaseID, sb.ID, slug, nil, b.cleanupCreateFailure(ctx, api, sb.ID, err)
	}
	if err := validateSuperserveSandboxOwnership(LeaseClaim{LeaseID: leaseID, Provider: providerName, ProviderScope: providerScope}, sb); err != nil {
		return leaseID, sb.ID, slug, nil, b.cleanupCreateFailure(ctx, api, sb.ID, err)
	}
	if err := claimLeaseForRepoProviderScopePond(leaseID, slug, providerName, providerScope, b.cfg.Pond, repo.Root, b.cfg.IdleTimeout, reclaim); err != nil {
		return leaseID, sb.ID, slug, nil, b.cleanupCreateFailure(ctx, api, sb.ID, err)
	}
	keepLock = true
	return leaseID, sb.ID, slug, unlockOperation, nil
}

func superserveCreateSource(cfg Config) (string, string) {
	snapshot := strings.TrimSpace(cfg.Superserve.Snapshot)
	if snapshot != "" {
		return "", snapshot
	}
	return strings.TrimSpace(cfg.Superserve.Template), ""
}

func superserveNetworkConfig(cfg Config) *createSandboxNetworkCfg {
	if len(cfg.Superserve.NetworkAllowOut) == 0 && len(cfg.Superserve.NetworkDenyOut) == 0 {
		return nil
	}
	return &createSandboxNetworkCfg{
		AllowOut: append([]string(nil), cfg.Superserve.NetworkAllowOut...),
		DenyOut:  append([]string(nil), cfg.Superserve.NetworkDenyOut...),
	}
}

func (b *backend) ownershipMetadata(baseURL, providerScope, leaseID, slug string, repo Repo) map[string]string {
	out := map[string]string{
		metadataProviderKey: providerName,
		metadataEndpointKey: superserveEndpointScope(baseURL),
		metadataScopeKey:    providerScope,
		metadataNameKey:     newSandboxName(repo),
		metadataRepoKey:     repoScope(repo),
	}
	if leaseID != "" {
		out[metadataClaimKey] = leaseID
	}
	if slug != "" {
		out[metadataSlugKey] = slug
	}
	if pond := strings.TrimSpace(b.cfg.Pond); pond != "" {
		out[metadataPondKey] = pond
	}
	return out
}

func (b *backend) baseMetadataFilter(baseURL string) map[string]string {
	return map[string]string{
		metadataProviderKey: providerName,
		metadataEndpointKey: superserveEndpointScope(baseURL),
	}
}

func (b *backend) serverFromSandbox(claim LeaseClaim, sb superserveSandbox) Server {
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

func resolveLeaseID(id, repoRoot string, reclaim bool, idleTimeout time.Duration, baseURL string) (string, string, string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", "", "", exit(2, "provider=superserve requires a Crabbox-created sandbox slug or lease id")
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
	claim, ok, err := resolveSuperserveLeaseClaim(id, baseURL)
	if err != nil {
		return "", "", "", err
	}
	if ok {
		return finishResolvedLease(claim, repoRoot, reclaim, idleTimeout, baseURL)
	}
	return "", "", "", exit(4, "superserve sandbox %q is not claimed by Crabbox; use a Crabbox slug or %s<sandbox-id>", id, leasePrefix)
}

func resolveSuperserveLeaseClaim(identifier, baseURL string) (LeaseClaim, bool, error) {
	claims, err := listSuperserveLeaseClaims()
	if err != nil {
		return LeaseClaim{}, false, err
	}
	for _, claim := range claims {
		if claim.Provider == providerName && claim.LeaseID == identifier {
			if err := validateSuperserveClaimScope(claim, baseURL); err != nil {
				return LeaseClaim{}, false, err
			}
			return claim, true, nil
		}
	}
	slug := normalizeLeaseSlug(identifier)
	if slug != "" {
		for _, claim := range claims {
			if claim.Provider == providerName && normalizeLeaseSlug(claim.Slug) == slug {
				if err := validateSuperserveClaimScope(claim, baseURL); err != nil {
					return LeaseClaim{}, false, err
				}
				return claim, true, nil
			}
		}
	}
	return LeaseClaim{}, false, nil
}

func finishResolvedLease(claim LeaseClaim, repoRoot string, reclaim bool, idleTimeout time.Duration, baseURL string) (string, string, string, error) {
	if err := validateSuperserveClaimScope(claim, baseURL); err != nil {
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

func newSuperserveClaimScope(baseURL string) (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", exit(5, "generate superserve ownership token: %v", err)
	}
	return superserveEndpointScope(baseURL) + "/ownership:" + hex.EncodeToString(token[:]), nil
}

func validateSuperserveClaimScope(claim LeaseClaim, baseURL string) error {
	if !superserveClaimMatchesEndpoint(claim, baseURL) {
		return exit(4, "superserve lease %q belongs to a different API endpoint; restore the endpoint used to create it", claim.LeaseID)
	}
	return nil
}

func superserveClaimMatchesEndpoint(claim LeaseClaim, baseURL string) bool {
	return strings.HasPrefix(strings.TrimSpace(claim.ProviderScope), superserveEndpointScope(baseURL)+"/ownership:")
}

func verifySuperserveClaim(ctx context.Context, api superserveClient, leaseID, sandboxID string) (superserveSandbox, error) {
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		return superserveSandbox{}, err
	}
	if err := validateSuperserveClaimScope(claim, api.BaseURL()); err != nil {
		return superserveSandbox{}, err
	}
	sb, err := api.GetSandbox(ctx, sandboxID)
	if err != nil {
		return superserveSandbox{}, err
	}
	if err := validateSuperserveSandboxOwnership(claim, sb); err != nil {
		return superserveSandbox{}, err
	}
	return sb, nil
}

func validateSuperserveSandboxOwnership(claim LeaseClaim, sb superserveSandbox) error {
	if sb.ID == "" {
		return exit(5, "superserve returned a sandbox without an id")
	}
	if sb.Metadata[metadataProviderKey] != providerName ||
		sb.Metadata[metadataScopeKey] != claim.ProviderScope ||
		sb.Metadata[metadataClaimKey] != claim.LeaseID {
		return exit(4, "superserve sandbox %q ownership metadata does not match its local claim", sb.ID)
	}
	return nil
}

func superserveClaimCleanupDue(claim LeaseClaim, now time.Time) (bool, string) {
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

func (b *backend) refreshSuperserveLeaseActivity(leaseID string) error {
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

func (b *backend) refreshSuperserveActivityIfRetained(leaseID string, shouldStop bool) error {
	if shouldStop {
		return nil
	}
	return b.refreshSuperserveLeaseActivity(leaseID)
}

func (b *backend) cleanupCreateFailure(ctx context.Context, api superserveClient, sandboxID string, cause error) error {
	cleanupCtx, cancel := b.cleanupContext(ctx)
	defer cancel()
	if err := api.DeleteSandbox(cleanupCtx, sandboxID); err != nil {
		if isSuperserveNotFound(err) {
			return cause
		}
		return errorsJoin(cause, fmt.Errorf("superserve cleanup failed for sandbox %s; delete it in the Superserve console: %w", sandboxID, err))
	}
	return cause
}

func (b *backend) cleanupClaimedRunFailure(ctx context.Context, api superserveClient, leaseID, sandboxID string, cause error) error {
	cleanupCtx, cancel := b.cleanupContext(ctx)
	defer cancel()
	if err := api.DeleteSandbox(cleanupCtx, sandboxID); err != nil && !isSuperserveNotFound(err) {
		return errors.Join(cause, fmt.Errorf("superserve cleanup failed for sandbox %s; delete it in the Superserve console: %w", sandboxID, err))
	}
	removeLeaseClaim(leaseID)
	return cause
}

func (b *backend) cleanupCreatedRun(ctx context.Context, api superserveClient, leaseID, sandboxID string, shouldStop *bool) error {
	if !*shouldStop {
		return nil
	}
	*shouldStop = false
	cleanupCtx, cancel := b.cleanupContext(ctx)
	defer cancel()
	if err := api.DeleteSandbox(cleanupCtx, sandboxID); err != nil && !isSuperserveNotFound(err) {
		return fmt.Errorf("superserve delete failed for %s: %w", sandboxID, err)
	}
	removeLeaseClaim(leaseID)
	return nil
}

func (b *backend) cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := superserveCleanupTimeout
	if b.cleanupTimeoutOverride > 0 {
		timeout = b.cleanupTimeoutOverride
	}
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}

func (b *backend) execTimeoutSecs() int {
	return b.cfg.Superserve.ExecTimeoutSecs
}

func (b *backend) sandboxTimeoutSecs() int {
	timeout, _ := superserveSandboxTimeoutSecs(b.cfg)
	return timeout
}

func superserveCommandEnv(env map[string]string) (map[string]string, []string) {
	if len(env) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(env))
	var stripped []string
	for name, value := range env {
		switch name {
		case "CRABBOX_SUPERSERVE_API_KEY", "SUPERSERVE_API_KEY":
			stripped = append(stripped, name)
		default:
			out[name] = value
		}
	}
	slices.Sort(stripped)
	return out, stripped
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

func (b *backend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}

func normalizedSandboxState(sb superserveSandbox) string {
	return strings.ToLower(blank(strings.TrimSpace(sb.Status), blank(strings.TrimSpace(sb.State), "unknown")))
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

func newSandboxName(repo Repo) string {
	base := normalizeLeaseSlug(repo.Name)
	if base == "" {
		base = "crabbox"
	}
	base = strings.TrimPrefix(base, strings.TrimSuffix(namePrefix, "-")+"-")
	if base == "" {
		base = "crabbox"
	}
	if len(base) > 40 {
		base = strings.Trim(base[:40], "-")
	}
	return namePrefix + base + "-" + randomSuffix()
}

func repoScope(repo Repo) string {
	value := strings.TrimSpace(repo.Root)
	if value == "" {
		value = strings.TrimSpace(repo.Name)
	}
	sum := sha256.Sum256([]byte(value))
	return "repo-sha256:" + hex.EncodeToString(sum[:8])
}

func randomSuffix() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())[:6]
	}
	return hex.EncodeToString(b[:])
}

func timeoutOrDefault(primary, fallback time.Duration) time.Duration {
	if primary > 0 {
		return primary
	}
	return fallback
}

func errorsJoin(errs ...error) error {
	var out error
	for _, err := range errs {
		if err == nil {
			continue
		}
		if out == nil {
			out = err
			continue
		}
		out = fmt.Errorf("%v; %w", out, err)
	}
	return out
}

func dataPlaneHostForSandbox(sandboxID, sandboxHost string) string {
	sandboxID = strings.TrimSpace(sandboxID)
	sandboxHost = strings.TrimSpace(sandboxHost)
	if sandboxID == "" || sandboxHost == "" {
		return ""
	}
	if strings.Contains(sandboxHost, "://") {
		return ""
	}
	if _, _, err := net.SplitHostPort(sandboxHost); err == nil {
		host, _, _ := net.SplitHostPort(sandboxHost)
		sandboxHost = host
	}
	return "boxd-" + sandboxID + "." + sandboxHost
}
