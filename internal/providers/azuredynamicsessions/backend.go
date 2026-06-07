package azuredynamicsessions

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

func NewAzureDynamicSessionsBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	return &azureDynamicSessionsBackend{spec: spec, cfg: cfg, rt: rt}
}

type azureDynamicSessionsBackend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func (b *azureDynamicSessionsBackend) Spec() ProviderSpec { return b.spec }

func (b *azureDynamicSessionsBackend) Warmup(ctx context.Context, req WarmupRequest) error {
	if req.ActionsRunner {
		return exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	started := b.now()
	client, err := newAzureDynamicSessionsClient(ctx, b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, slug, err := b.createSession(ctx, client, req.Repo, req.Reclaim, req.RequestedSlug)
	if err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s session=%s\n", leaseID, slug, providerName, leaseID)
	if !req.Keep {
		if err := client.DeleteSession(ctx, leaseID); err != nil {
			return providerError("delete session", err)
		}
		removeLeaseClaim(leaseID)
		fmt.Fprintf(b.rt.Stderr, "released lease=%s session=%s\n", leaseID, leaseID)
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

func (b *azureDynamicSessionsBackend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if err := delegatedSyncOptionsError(b.spec, req); err != nil {
		return RunResult{}, err
	}
	if !req.SyncOnly && len(req.Command) == 0 {
		return RunResult{}, exit(2, "missing command")
	}
	started := b.now()
	client, err := newAzureDynamicSessionsClient(ctx, b.cfg, b.rt)
	if err != nil {
		return RunResult{}, err
	}
	leaseID, slug := "", ""
	acquired := false
	if req.ID == "" {
		leaseID, slug, err = b.createSession(ctx, client, req.Repo, req.Reclaim, req.RequestedSlug)
		if err != nil {
			return RunResult{}, err
		}
		fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s session=%s\n", leaseID, slug, providerName, leaseID)
		acquired = true
	} else {
		leaseID, slug, err = b.resolveSessionID(ctx, client, req.ID, req.Repo.Root, req.Reclaim)
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
			if err := client.DeleteSession(context.Background(), leaseID); err != nil {
				fmt.Fprintf(b.rt.Stderr, "warning: %s stop failed for %s: %v\n", providerName, leaseID, err)
				return
			}
			removeLeaseClaim(leaseID)
		}()
	}

	workspace, err := azureDynamicSessionsWorkspace(b.cfg)
	if err != nil {
		return RunResult{}, err
	}
	syncDuration := time.Duration(0)
	syncPhases := []timingPhase{{Name: "sync", Skipped: true, Reason: "--no-sync"}}
	if !req.NoSync {
		syncPhases, syncDuration, err = b.syncWorkspace(ctx, client, leaseID, req, workspace)
		if err != nil {
			return RunResult{Total: b.now().Sub(started), SyncDelegated: true, Provider: providerName, LeaseID: leaseID, Slug: slug}, err
		}
		fmt.Fprintf(b.rt.Stderr, "sync complete in %s\n", syncDuration.Round(time.Millisecond))
	} else if err := b.prepareWorkspace(ctx, client, leaseID, workspace, false); err != nil {
		return RunResult{}, err
	}
	if req.SyncOnly {
		result := RunResult{Total: b.now().Sub(started), SyncDelegated: true, Provider: providerName, LeaseID: leaseID, Slug: slug}
		fmt.Fprintf(b.rt.Stdout, "synced %s\n", workspace)
		if req.TimingJSON {
			err := writeTimingJSON(b.rt.Stderr, timingReport{
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
			return result, err
		}
		return result, nil
	}

	command, err := buildAzureDynamicSessionsCommand(req.Command, req.ShellMode)
	if err != nil {
		return RunResult{}, err
	}
	if req.EnvSummary {
		printEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
	}
	commandStarted := b.now()
	fmt.Fprintf(b.rt.Stderr, "running on %s %s\n", providerName, strings.Join(req.Command, " "))
	exitCode, commandErr := client.ExecStream(ctx, leaseID, azureDynamicSessionsExecRequest{
		Command:   command,
		Cwd:       workspace,
		Env:       req.Env,
		TimeoutMS: durationMillisecondsCeil(azureDynamicSessionsTimeout(b.cfg)),
	}, b.rt.Stdout, b.rt.Stderr)
	commandDuration := b.now().Sub(commandStarted)
	if commandErr != nil && exitCode == 0 {
		exitCode = 1
	}

	result := RunResult{
		ExitCode:      exitCode,
		Command:       commandDuration,
		Total:         b.now().Sub(started),
		SyncDelegated: true,
		Provider:      providerName,
		LeaseID:       leaseID,
		Slug:          slug,
		CommandText:   command,
	}
	if req.NoSync {
		fmt.Fprintf(b.rt.Stderr, "%s run summary sync_skipped=true command=%s total=%s exit=%d\n", providerName, result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), result.ExitCode)
	} else {
		fmt.Fprintf(b.rt.Stderr, "%s run summary sync=%s command=%s total=%s exit=%d\n", providerName, syncDuration.Round(time.Millisecond), result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), result.ExitCode)
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
			CommandMs:     commandDuration.Milliseconds(),
			TotalMs:       result.Total.Milliseconds(),
			ExitCode:      result.ExitCode,
			Label:         strings.TrimSpace(req.Label),
		}); err != nil {
			return result, err
		}
	}
	if commandErr != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return result, ExitError{Code: 1, Message: fmt.Sprintf("%s run failed: %v", providerName, commandErr)}
	}
	if result.ExitCode != 0 {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return result, ExitError{Code: result.ExitCode, Message: fmt.Sprintf("%s run exited %d", providerName, result.ExitCode)}
	}
	return result, nil
}

func (b *azureDynamicSessionsBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	client, err := newAzureDynamicSessionsClient(ctx, b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	sessions, err := client.ListSessions(ctx)
	if err != nil {
		return nil, providerError("list sessions", err)
	}
	claims, err := listLeaseClaims()
	if err != nil {
		return nil, err
	}
	scope, err := b.claimScope()
	if err != nil {
		return nil, err
	}
	claimByID := map[string]coreLeaseClaim{}
	for _, claim := range claims {
		if claim.Provider == providerName && strings.TrimSpace(claim.ProviderScope) == scope {
			claimByID[claim.LeaseID] = coreLeaseClaim{LeaseID: claim.LeaseID, Slug: claim.Slug, RepoRoot: claim.RepoRoot}
		}
	}
	servers := make([]Server, 0, len(sessions))
	for _, session := range sessions {
		identifier := session.Identifier
		if identifier == "" {
			continue
		}
		claim, claimed := claimByID[identifier]
		if !req.All && !claimed && !strings.HasPrefix(identifier, "azds-") {
			continue
		}
		servers = append(servers, b.sessionToServer(session, claim))
	}
	return servers, nil
}

func (b *azureDynamicSessionsBackend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	servers, err := b.List(ctx, ListRequest{})
	if err != nil {
		return DoctorResult{}, err
	}
	return inventoryDoctorResult(providerName, len(servers)), nil
}

func (b *azureDynamicSessionsBackend) Status(ctx context.Context, req StatusRequest) (statusView, error) {
	client, err := newAzureDynamicSessionsClient(ctx, b.cfg, b.rt)
	if err != nil {
		return statusView{}, err
	}
	leaseID, slug, err := b.resolveSessionID(ctx, client, req.ID, "", false)
	if err != nil {
		return statusView{}, err
	}
	deadline := b.now().Add(req.WaitTimeout)
	if req.WaitTimeout <= 0 {
		deadline = b.now().Add(5 * time.Minute)
	}
	for {
		session, err := client.GetSession(ctx, leaseID)
		if err == nil {
			view := b.statusView(leaseID, slug, session)
			if !req.Wait || view.Ready {
				return view, nil
			}
			if b.now().After(deadline) {
				return statusView{}, exit(5, "timed out waiting for session %s to become ready", leaseID)
			}
			select {
			case <-ctx.Done():
				return statusView{}, ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}
		if !isNotFoundError(err) || !req.Wait {
			return statusView{}, providerError("get session", err)
		}
		if b.now().After(deadline) {
			return statusView{}, exit(5, "timed out waiting for session %s to become ready", leaseID)
		}
		select {
		case <-ctx.Done():
			return statusView{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *azureDynamicSessionsBackend) Stop(ctx context.Context, req StopRequest) error {
	client, err := newAzureDynamicSessionsClient(ctx, b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, _, err := b.resolveSessionID(ctx, client, req.ID, "", false)
	if err != nil {
		return err
	}
	if err := client.DeleteSession(ctx, leaseID); err != nil {
		if isNotFoundError(err) {
			removeLeaseClaim(leaseID)
			fmt.Fprintf(b.rt.Stderr, "removed stale claim for missing session=%s\n", leaseID)
			return nil
		}
		return providerError("delete session", err)
	}
	removeLeaseClaim(leaseID)
	fmt.Fprintf(b.rt.Stderr, "released lease=%s session=%s\n", leaseID, leaseID)
	return nil
}

func (b *azureDynamicSessionsBackend) createSession(ctx context.Context, client azureDynamicSessionsAPI, repo Repo, reclaim bool, requestedSlug string) (string, string, error) {
	leaseID := newSessionID()
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return "", "", err
	}
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s\n", providerName, leaseID, slug)
	if err := client.CheckRunner(ctx, leaseID); err != nil {
		return "", "", providerError("create session", err)
	}
	scope, err := b.claimScope()
	if err != nil {
		_ = client.DeleteSession(context.Background(), leaseID)
		return "", "", err
	}
	if err := claimLeaseForRepoProviderScope(leaseID, slug, providerName, scope, repo.Root, b.cfg.IdleTimeout, reclaim); err != nil {
		_ = client.DeleteSession(context.Background(), leaseID)
		return "", "", err
	}
	return leaseID, slug, nil
}

func (b *azureDynamicSessionsBackend) resolveSessionID(_ context.Context, _ azureDynamicSessionsAPI, id, repoRoot string, reclaim bool) (string, string, error) {
	if id == "" {
		return "", "", exit(2, "provider=%s requires a kept Crabbox lease id or slug", providerName)
	}
	scope, err := b.claimScope()
	if err != nil {
		return "", "", err
	}
	if claim, ok, err := resolveAzureDynamicSessionsClaim(id, scope); err != nil {
		return "", "", err
	} else if ok {
		if repoRoot != "" {
			if err := claimLeaseForRepoProviderScope(claim.LeaseID, claim.Slug, providerName, scope, repoRoot, time.Duration(claim.IdleTimeoutSeconds)*time.Second, reclaim); err != nil {
				return "", "", err
			}
		}
		return claim.LeaseID, claim.Slug, nil
	}
	return "", "", exit(4, "%s session %q is not claimed by Crabbox; use a kept Crabbox lease id or slug", providerName, id)
}

func (b *azureDynamicSessionsBackend) claimScope() (string, error) {
	endpoint, err := azureDynamicSessionsEndpoint(b.cfg)
	if err != nil {
		return "", err
	}
	return "endpoint:" + endpoint, nil
}

func resolveAzureDynamicSessionsClaim(identifier, scope string) (LeaseClaim, bool, error) {
	claims, err := listLeaseClaims()
	if err != nil {
		return LeaseClaim{}, false, err
	}
	slug := normalizeLeaseSlug(identifier)
	for _, claim := range claims {
		if claim.Provider != providerName || strings.TrimSpace(claim.ProviderScope) != scope {
			continue
		}
		if claim.LeaseID == identifier || (slug != "" && normalizeLeaseSlug(claim.Slug) == slug) {
			return claim, true, nil
		}
	}
	return LeaseClaim{}, false, nil
}

type coreLeaseClaim struct {
	LeaseID  string
	Slug     string
	RepoRoot string
}

func (b *azureDynamicSessionsBackend) sessionToServer(session azureDynamicSessionsSession, claim coreLeaseClaim) Server {
	identifier := session.Identifier
	slug := claim.Slug
	if slug == "" {
		slug = newLeaseSlug(identifier)
	}
	status := azureDynamicSessionsSessionStatus(session)
	labels := map[string]string{
		"crabbox":  "true",
		"provider": providerName,
		"lease":    identifier,
		"slug":     normalizeLeaseSlug(slug),
		"target":   targetLinux,
		"state":    blank(status, "ready"),
	}
	if claim.RepoRoot != "" {
		labels["claimed"] = "true"
	}
	if expires := azureDynamicSessionsSessionExpires(session); expires != "" {
		labels["expires_at"] = expires
	}
	server := Server{
		Provider: providerName,
		CloudID:  identifier,
		Name:     identifier,
		Status:   blank(status, "ready"),
		Labels:   labels,
	}
	server.ServerType.Name = "custom-container"
	return server
}

func (b *azureDynamicSessionsBackend) statusView(leaseID, slug string, session azureDynamicSessionsSession) statusView {
	status := azureDynamicSessionsSessionStatus(session)
	return statusView{
		ID:         leaseID,
		Slug:       slug,
		Provider:   providerName,
		TargetOS:   targetLinux,
		State:      blank(status, "ready"),
		ServerID:   leaseID,
		ServerType: "custom-container",
		Network:    networkPublic,
		Ready:      azureDynamicSessionsStatusReady(status),
		ExpiresAt:  azureDynamicSessionsSessionExpires(session),
		Labels: map[string]string{
			"provider": providerName,
			"lease":    leaseID,
			"slug":     normalizeLeaseSlug(slug),
			"target":   targetLinux,
		},
	}
}

func azureDynamicSessionsSessionStatus(session azureDynamicSessionsSession) string {
	if session.Status != "" {
		return session.Status
	}
	return "ready"
}

func azureDynamicSessionsSessionExpires(session azureDynamicSessionsSession) string {
	if session.ExpiresAt != "" {
		return session.ExpiresAt
	}
	return session.ExpireAt
}

func azureDynamicSessionsStatusReady(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "ready", "running", "succeeded", "active":
		return true
	default:
		return false
	}
}

func isNotFoundError(err error) bool {
	var apiErr *azureDynamicSessionsAPIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.StatusCode == 404 {
		return true
	}
	if apiErr.StatusCode != 400 {
		return false
	}
	return strings.Contains(apiErr.Body, "SessionWithIdentifierNotFound") ||
		strings.Contains(apiErr.Body, "SessionNotFound")
}

func (b *azureDynamicSessionsBackend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}
