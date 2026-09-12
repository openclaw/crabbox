package azuredynamicsessions

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func NewAzureDynamicSessionsBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	cfg.Provider = providerName
	return &azureDynamicSessionsBackend{spec: spec, cfg: cfg, rt: rt}
}

var azureDynamicSessionsDeleteTimeout = 30 * time.Second

type azureDynamicSessionsBackend struct {
	spec core.ProviderSpec
	cfg  core.Config
	rt   core.Runtime
}

func (b *azureDynamicSessionsBackend) Spec() core.ProviderSpec { return b.spec }

func (b *azureDynamicSessionsBackend) Warmup(ctx context.Context, req core.WarmupRequest) error {
	if req.ActionsRunner {
		return core.Exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	started := core.ClockNow(b.rt.Clock)
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
		claim, err := b.sessionClaimForDeletion(leaseID)
		if err != nil {
			return err
		}
		if err := core.RemoveLeaseClaimIfUnchangedAfter(leaseID, claim, func() error {
			return client.DeleteSession(ctx, leaseID)
		}); err != nil {
			return providerError("delete session", err)
		}
		fmt.Fprintf(b.rt.Stderr, "released lease=%s session=%s\n", leaseID, leaseID)
	}
	total := core.ClockNow(b.rt.Clock).Sub(started)
	return shared.CompleteWarmup(b.rt, req.TimingJSON, shared.WarmupCompletion{
		Provider: providerName,
		LeaseID:  leaseID,
		Slug:     slug,
		Total:    total,
	})
}

func (b *azureDynamicSessionsBackend) Run(ctx context.Context, req core.RunRequest) (core.RunResult, error) {
	workspace, workspaceErr := azureDynamicSessionsWorkspace(b.cfg)
	var client azureDynamicSessionsAPI
	var leaseID, slug string
	var cleanupClaim core.LeaseClaim
	handle := func() shared.DelegatedSandbox {
		return shared.DelegatedSandbox{LeaseID: leaseID, Slug: slug, CleanupCommand: azureDynamicSessionsCleanupCommand(leaseID)}
	}
	return shared.RunDelegatedSandbox(ctx, req, shared.DelegatedSandboxLifecycle{
		Provider: providerName, Runtime: b.rt, Workdir: workspace,
		IdleTimeout: b.cfg.IdleTimeout, TTL: b.cfg.TTL, CleanupTimeout: azureDynamicSessionsDeleteTimeout,
		Preflight: func(ctx context.Context) error {
			if err := core.RejectDelegatedSyncOptionsForSpec(b.spec, req); err != nil {
				return err
			}
			if !req.SyncOnly && len(req.Command) == 0 {
				return core.Exit(2, "missing command")
			}
			var err error
			client, err = newAzureDynamicSessionsClient(ctx, b.cfg, b.rt)
			return err
		},
		PrepareArchive: func(ctx context.Context) (*core.PreparedArchive, error) {
			return core.PrepareDelegatedArchive(ctx, core.DelegatedArchivePreparationRequest{
				Config: b.cfg, Repo: req.Repo, ForceSyncLarge: req.ForceSyncLarge,
				TempPattern: "crabbox-azds-sync-*.tgz", Stderr: b.rt.Stderr, Now: func() time.Time { return core.ClockNow(b.rt.Clock) },
			})
		},
		Acquire: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			var err error
			leaseID, slug, err = b.createSession(ctx, client, req.Repo, req.Reclaim, req.RequestedSlug)
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			if !req.Keep {
				cleanupClaim, err = b.sessionClaimForDeletion(leaseID)
				if err != nil {
					return shared.DelegatedSandbox{}, err
				}
			}
			fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s session=%s\n", leaseID, slug, providerName, leaseID)
			return handle(), nil
		},
		Resolve: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			var err error
			leaseID, slug, err = b.resolveSessionID(ctx, client, req.ID, req.Repo.Root, req.Reclaim)
			return handle(), err
		},
		// Keep invalid-workspace handling after acquisition, with normal retention.
		Setup: func(context.Context) error { return workspaceErr },
		Sync: func(ctx context.Context, prepared *core.PreparedArchive) ([]core.TimingPhase, time.Duration, error) {
			return b.syncWorkspace(ctx, client, leaseID, req, workspace, prepared)
		},
		NoSync: func(ctx context.Context) error { return b.prepareWorkspace(ctx, client, leaseID, workspace) },
		Command: func(context.Context) (shared.DelegatedSandboxCommand, error) {
			command, err := buildAzureDynamicSessionsCommand(req.Command, req.ShellMode)
			if err != nil {
				return shared.DelegatedSandboxCommand{}, err
			}
			if req.EnvSummary {
				core.PrintEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
			}
			return shared.DelegatedSandboxCommand{Text: command, Run: func(ctx context.Context, stdout, stderr io.Writer) (int, error) {
				fmt.Fprintf(b.rt.Stderr, "running on %s %s\n", providerName, strings.Join(req.Command, " "))
				return client.ExecStream(ctx, leaseID, azureDynamicSessionsExecRequest{
					Command: command, Cwd: workspace, Env: req.Env,
					TimeoutMS: durationMillisecondsCeil(azureDynamicSessionsTimeout(b.cfg)),
				}, stdout, stderr)
			}}, nil
		},
		Cleanup: func(ctx context.Context) error {
			return core.CleanupLeaseClaimIfUnchangedAfterContext(ctx, leaseID, cleanupClaim, true, func() error {
				return client.DeleteSession(ctx, leaseID)
			})
		},
	})
}

func (b *azureDynamicSessionsBackend) List(ctx context.Context, req core.ListRequest) ([]core.LeaseView, error) {
	client, err := newAzureDynamicSessionsClient(ctx, b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	sessions, err := client.ListSessions(ctx)
	if err != nil {
		return nil, providerError("list sessions", err)
	}
	claims, err := core.ListLeaseClaims()
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
	servers := make([]core.Server, 0, len(sessions))
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

func (b *azureDynamicSessionsBackend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	servers, err := b.List(ctx, core.ListRequest{})
	if err != nil {
		return core.DoctorResult{}, err
	}
	return core.InventoryDoctorResult(providerName, len(servers)), nil
}

func (b *azureDynamicSessionsBackend) Status(ctx context.Context, req core.StatusRequest) (core.StatusView, error) {
	client, err := newAzureDynamicSessionsClient(ctx, b.cfg, b.rt)
	if err != nil {
		return core.StatusView{}, err
	}
	waitTimeout := req.WaitTimeout
	if waitTimeout <= 0 {
		waitTimeout = 5 * time.Minute
	}
	deadline := core.ClockNow(b.rt.Clock).Add(waitTimeout)
	pollCtx := ctx
	cancel := func() {}
	if req.Wait {
		pollCtx, cancel = context.WithTimeout(ctx, waitTimeout)
	}
	defer cancel()
	leaseID, slug, err := b.resolveSessionID(pollCtx, client, req.ID, "", false)
	if err != nil {
		return core.StatusView{}, err
	}
	for {
		session, err := client.GetSession(pollCtx, leaseID)
		if err == nil {
			view := b.statusView(leaseID, slug, session)
			if !req.Wait || view.Ready {
				return view, nil
			}
			if core.ClockNow(b.rt.Clock).After(deadline) {
				return core.StatusView{}, core.Exit(5, "timed out waiting for session %s to become ready", leaseID)
			}
			select {
			case <-pollCtx.Done():
				if errors.Is(pollCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
					return core.StatusView{}, core.Exit(5, "timed out waiting for session %s to become ready", leaseID)
				}
				return core.StatusView{}, pollCtx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}
		if req.Wait && errors.Is(pollCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return core.StatusView{}, core.Exit(5, "timed out waiting for session %s to become ready", leaseID)
		}
		if ctx.Err() != nil {
			return core.StatusView{}, ctx.Err()
		}
		if !isNotFoundError(err) || !req.Wait {
			return core.StatusView{}, providerError("get session", err)
		}
		if core.ClockNow(b.rt.Clock).After(deadline) {
			return core.StatusView{}, core.Exit(5, "timed out waiting for session %s to become ready", leaseID)
		}
		select {
		case <-pollCtx.Done():
			if errors.Is(pollCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				return core.StatusView{}, core.Exit(5, "timed out waiting for session %s to become ready", leaseID)
			}
			return core.StatusView{}, pollCtx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *azureDynamicSessionsBackend) Stop(ctx context.Context, req core.StopRequest) error {
	client, err := newAzureDynamicSessionsClient(ctx, b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, _, err := b.resolveSessionID(ctx, client, req.ID, "", false)
	if err != nil {
		return err
	}
	claim, err := b.sessionClaimForDeletion(leaseID)
	if err != nil {
		return err
	}
	missing := false
	if err := core.RemoveLeaseClaimIfUnchangedAfter(leaseID, claim, func() error {
		if err := client.DeleteSession(ctx, leaseID); err != nil {
			if isNotFoundError(err) {
				missing = true
				return nil
			}
			return providerError("delete session", err)
		}
		return nil
	}); err != nil {
		return err
	}
	if missing {
		fmt.Fprintf(b.rt.Stderr, "removed stale claim for missing session=%s\n", leaseID)
		return nil
	}
	fmt.Fprintf(b.rt.Stderr, "released lease=%s session=%s\n", leaseID, leaseID)
	return nil
}

func (b *azureDynamicSessionsBackend) createSession(ctx context.Context, client azureDynamicSessionsAPI, repo core.Repo, reclaim bool, requestedSlug string) (string, string, error) {
	leaseID := newSessionID()
	slug, err := core.AllocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return "", "", err
	}
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s\n", providerName, leaseID, slug)
	if err := client.CheckRunner(ctx, leaseID); err != nil {
		return "", "", providerError("create session", err)
	}
	scope, err := b.claimScope()
	if err != nil {
		return "", "", b.rollbackCreatedSession(client, leaseID, err)
	}
	if err := core.ClaimLeaseForRepoProviderScope(leaseID, slug, providerName, scope, repo.Root, b.cfg.IdleTimeout, reclaim); err != nil {
		return "", "", b.rollbackCreatedSession(client, leaseID, err)
	}
	return leaseID, slug, nil
}

func (b *azureDynamicSessionsBackend) rollbackCreatedSession(client azureDynamicSessionsAPI, leaseID string, cause error) error {
	if err := b.deleteSessionBounded(client, leaseID); err != nil {
		return errors.Join(cause, fmt.Errorf("cleanup %s session %s: %w", providerName, leaseID, err))
	}
	return cause
}

func (b *azureDynamicSessionsBackend) deleteSessionBounded(client azureDynamicSessionsAPI, leaseID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), azureDynamicSessionsDeleteTimeout)
	defer cancel()
	return client.DeleteSession(ctx, leaseID)
}

func (b *azureDynamicSessionsBackend) sessionClaimForDeletion(leaseID string) (core.LeaseClaim, error) {
	scope, err := b.claimScope()
	if err != nil {
		return core.LeaseClaim{}, err
	}
	claim, ok, err := resolveAzureDynamicSessionsClaim(leaseID, scope)
	if err != nil {
		return core.LeaseClaim{}, err
	}
	if !ok || claim.LeaseID != leaseID {
		return core.LeaseClaim{}, core.Exit(4, "%s session %q is not claimed by Crabbox", providerName, leaseID)
	}
	return claim, nil
}

func (b *azureDynamicSessionsBackend) resolveSessionID(_ context.Context, _ azureDynamicSessionsAPI, id, repoRoot string, reclaim bool) (string, string, error) {
	if id == "" {
		return "", "", core.Exit(2, "provider=%s requires a kept Crabbox lease id or slug", providerName)
	}
	scope, err := b.claimScope()
	if err != nil {
		return "", "", err
	}
	if claim, ok, err := resolveAzureDynamicSessionsClaim(id, scope); err != nil {
		return "", "", err
	} else if ok {
		if repoRoot != "" {
			if err := core.ClaimLeaseForRepoProviderScope(claim.LeaseID, claim.Slug, providerName, scope, repoRoot, time.Duration(claim.IdleTimeoutSeconds)*time.Second, reclaim); err != nil {
				return "", "", err
			}
		}
		return claim.LeaseID, claim.Slug, nil
	}
	return "", "", core.Exit(4, "%s session %q is not claimed by Crabbox; use a kept Crabbox lease id or slug", providerName, id)
}

func (b *azureDynamicSessionsBackend) claimScope() (string, error) {
	endpoint, err := azureDynamicSessionsEndpoint(b.cfg)
	if err != nil {
		return "", err
	}
	return "endpoint:" + endpoint, nil
}

func resolveAzureDynamicSessionsClaim(identifier, scope string) (core.LeaseClaim, bool, error) {
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return core.LeaseClaim{}, false, err
	}
	slug := core.NormalizeLeaseSlug(identifier)
	for _, claim := range claims {
		if claim.Provider != providerName || strings.TrimSpace(claim.ProviderScope) != scope {
			continue
		}
		if claim.LeaseID == identifier || (slug != "" && core.NormalizeLeaseSlug(claim.Slug) == slug) {
			return claim, true, nil
		}
	}
	return core.LeaseClaim{}, false, nil
}

type coreLeaseClaim struct {
	LeaseID  string
	Slug     string
	RepoRoot string
}

func (b *azureDynamicSessionsBackend) sessionToServer(session azureDynamicSessionsSession, claim coreLeaseClaim) core.Server {
	identifier := session.Identifier
	slug := claim.Slug
	if slug == "" {
		slug = core.NewLeaseSlug(identifier)
	}
	status := azureDynamicSessionsSessionStatus(session)
	labels := map[string]string{
		"crabbox":  "true",
		"provider": providerName,
		"lease":    identifier,
		"slug":     core.NormalizeLeaseSlug(slug),
		"target":   targetLinux,
		"state":    core.Blank(status, "ready"),
	}
	if claim.RepoRoot != "" {
		labels["claimed"] = "true"
	}
	if expires := azureDynamicSessionsSessionExpires(session); expires != "" {
		labels["expires_at"] = expires
	}
	server := core.Server{
		Provider: providerName,
		CloudID:  identifier,
		Name:     identifier,
		Status:   core.Blank(status, "ready"),
		Labels:   labels,
	}
	server.ServerType.Name = "custom-container"
	return server
}

func (b *azureDynamicSessionsBackend) statusView(leaseID, slug string, session azureDynamicSessionsSession) core.StatusView {
	status := azureDynamicSessionsSessionStatus(session)
	return core.StatusView{
		ID:         leaseID,
		Slug:       slug,
		Provider:   providerName,
		TargetOS:   targetLinux,
		State:      core.Blank(status, "ready"),
		ServerID:   leaseID,
		ServerType: "custom-container",
		Network:    networkPublic,
		Ready:      azureDynamicSessionsStatusReady(status),
		ExpiresAt:  azureDynamicSessionsSessionExpires(session),
		Labels: map[string]string{
			"provider": providerName,
			"lease":    leaseID,
			"slug":     core.NormalizeLeaseSlug(slug),
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
