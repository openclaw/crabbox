package superserve

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const (
	superserveCleanupTimeout = 15 * time.Second
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

func NewSuperserveBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	cfg.Provider = providerName
	return &backend{spec: spec, cfg: cfg, rt: rt, newClient: newSuperserveClient}
}

type backend struct {
	spec                   core.ProviderSpec
	cfg                    core.Config
	rt                     core.Runtime
	newClient              func(core.Config, core.Runtime) (superserveClient, error)
	cleanupTimeoutOverride time.Duration
}

func (b *backend) Spec() core.ProviderSpec { return b.spec }

func (b *backend) client() (superserveClient, error) {
	if b.newClient != nil {
		return b.newClient(b.cfg, b.rt)
	}
	return newSuperserveClient(b.cfg, b.rt)
}

func (b *backend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	api, err := b.client()
	if err != nil {
		return core.DoctorResult{}, err
	}
	if err := api.Probe(ctx); err != nil {
		return core.DoctorResult{}, err
	}
	servers, err := b.List(ctx, core.ListRequest{})
	if err != nil {
		return core.DoctorResult{}, err
	}
	return core.InventoryDoctorResult(providerName, len(servers)), nil
}

func (b *backend) Warmup(ctx context.Context, req core.WarmupRequest) error {
	if req.ActionsRunner {
		return core.Exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	if req.Options.Tailscale.Enabled {
		return core.Exit(2, "provider=superserve is delegated-run only and does not support Tailscale options")
	}
	if _, err := superserveWorkdir(b.cfg); err != nil {
		return err
	}
	started := core.ClockNow(b.rt.Clock)
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
	total := core.ClockNow(b.rt.Clock).Sub(started)
	return shared.CompleteWarmup(b.rt, req.TimingJSON, shared.WarmupCompletion{
		Provider: providerName,
		LeaseID:  leaseID,
		Slug:     slug,
		Total:    total,
	})
}

func (b *backend) Run(ctx context.Context, req core.RunRequest) (core.RunResult, error) {
	var api superserveClient
	var workdir, leaseID, sandboxID, slug string
	var access sandboxAccess
	activated := false
	activate := func(ctx context.Context) error {
		next, err := api.ActivateSandbox(ctx, sandboxID)
		if err != nil {
			return err
		}
		if next.Sandbox.ID == "" {
			next.Sandbox.ID = sandboxID
		}
		access, activated = next, true
		return nil
	}
	// Resolve and acquire keep the operation lock until the shared owner has
	// finished cleanup and reporting, including errors after lock acquisition.
	handle := func(unlock func()) shared.DelegatedSandbox {
		return shared.DelegatedSandbox{
			LeaseID: leaseID, Slug: slug, CleanupCommand: superserveCleanupCommand(leaseID), Unlock: unlock,
		}
	}
	workdir, workdirErr := superserveWorkdir(b.cfg)
	return shared.RunDelegatedSandbox(ctx, req, shared.DelegatedSandboxLifecycle{
		Provider: providerName, Runtime: b.rt, Workdir: workdir,
		IdleTimeout: b.cfg.IdleTimeout, TTL: b.cfg.TTL, CleanupTimeout: b.cleanupTimeout(),
		Preflight: func(context.Context) error {
			if req.Options.Tailscale.Enabled {
				return core.Exit(2, "provider=superserve is delegated-run only and does not support Tailscale options")
			}
			if workdirErr != nil {
				return workdirErr
			}
			var err error
			api, err = b.client()
			return err
		},
		PrepareArchive: func(ctx context.Context) (*core.PreparedArchive, error) {
			return core.PrepareDelegatedArchive(ctx, core.DelegatedArchivePreparationRequest{
				Config: b.cfg, Repo: req.Repo, ForceSyncLarge: req.ForceSyncLarge,
				TempPattern: "crabbox-superserve-sync-*.tgz", Stderr: b.rt.Stderr, Now: func() time.Time { return core.ClockNow(b.rt.Clock) },
			})
		},
		Acquire: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			var unlock func()
			var err error
			leaseID, sandboxID, slug, unlock, err = b.createSandbox(ctx, api, req.Repo, req.Reclaim, req.RequestedSlug)
			if err == nil {
				fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s sandbox=%s\n", leaseID, slug, providerName, sandboxID)
			}
			return handle(unlock), err
		},
		Resolve: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			var err error
			leaseID, sandboxID, _, err = resolveLeaseID(req.ID, "", false, 0, api.BaseURL())
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			unlock, err := lockSuperserveLeaseOperation(ctx, leaseID)
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			leaseID, sandboxID, _, err = resolveLeaseID(leaseID, "", false, 0, api.BaseURL())
			if err != nil {
				return handle(unlock), err
			}
			if _, err := verifySuperserveClaim(ctx, api, leaseID, sandboxID); err != nil {
				return handle(unlock), err
			}
			claim, err := core.ReadLeaseClaim(leaseID)
			if err != nil {
				return handle(unlock), err
			}
			_, _, slug, err = finishResolvedLease(claim, req.Repo.Root, req.Reclaim, b.cfg.IdleTimeout, api.BaseURL())
			return handle(unlock), err
		},
		AdmitReuse: activate,
		Setup: func(ctx context.Context) error {
			if req.ID == "" {
				if err := activate(ctx); err != nil {
					return err
				}
			}
			fmt.Fprintf(b.rt.Stderr, "provider=%s lease=%s sandbox=%s workdir=%s\n", providerName, leaseID, sandboxID, workdir)
			return nil
		},
		Sync: func(ctx context.Context, prepared *core.PreparedArchive) ([]core.TimingPhase, time.Duration, error) {
			return b.syncWorkspace(ctx, api, &access, req, workdir, prepared)
		},
		NoSync: func(ctx context.Context) error {
			return b.ensureWorkspace(ctx, api, &access, workdir)
		},
		Command: func(context.Context) (shared.DelegatedSandboxCommand, error) {
			intent, err := core.ParseCommandIntent(req.Command, req.ShellMode, req.CommandLiteralArgs)
			if err != nil {
				return shared.DelegatedSandboxCommand{}, err
			}
			commandText := intent.ShellCommand("bash", "-lc")
			commandEnv, strippedAuthEnv := superserveCommandEnv(req.Env)
			if len(strippedAuthEnv) > 0 {
				fmt.Fprintf(b.rt.Stderr, "warning: provider=superserve did not forward provider authentication variables: %s\n", strings.Join(strippedAuthEnv, ","))
			}
			if req.EnvSummary || strings.TrimSpace(os.Getenv("CRABBOX_ENV_ALLOW")) != "" {
				core.PrintEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, commandEnv)
			}
			return shared.DelegatedSandboxCommand{
				Text: commandText,
				Run: func(ctx context.Context, stdout, stderr io.Writer) (int, error) {
					res, err := api.Exec(ctx, &access, execRequest{
						Command: commandText, WorkingDir: workdir, Env: commandEnv, TimeoutSecs: b.execTimeoutSecs(),
					}, stdout, stderr)
					return res.ExitCode, err
				},
			}, nil
		},
		Retained: func(context.Context) error {
			if !activated {
				return nil
			}
			return b.refreshSuperserveLeaseActivity(leaseID)
		},
		Cleanup: func(ctx context.Context) error {
			if err := api.DeleteSandbox(ctx, sandboxID); err != nil && !isSuperserveNotFound(err) {
				return fmt.Errorf("superserve delete failed for %s: %w", sandboxID, err)
			}
			core.RemoveLeaseClaim(leaseID)
			return nil
		},
	})
}
func (b *backend) List(ctx context.Context, _ core.ListRequest) ([]core.LeaseView, error) {
	api, err := b.client()
	if err != nil {
		return nil, err
	}
	sandboxes, err := api.ListSandboxes(ctx, b.baseMetadataFilter(api.BaseURL()))
	if err != nil {
		return nil, err
	}
	views := make([]core.LeaseView, 0, len(sandboxes))
	for _, sb := range sandboxes {
		leaseID := strings.TrimSpace(sb.Metadata[metadataClaimKey])
		if leaseID == "" {
			continue
		}
		claim, err := core.ReadLeaseClaim(leaseID)
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

func (b *backend) Status(ctx context.Context, req core.StatusRequest) (core.StatusView, error) {
	api, err := b.client()
	if err != nil {
		return core.StatusView{}, err
	}
	leaseID, sandboxID, slug, err := resolveLeaseID(req.ID, "", false, 0, api.BaseURL())
	if err != nil {
		return core.StatusView{}, err
	}
	claim, ok, err := resolveSuperserveLeaseClaim(leaseID, api.BaseURL())
	if err != nil {
		return core.StatusView{}, err
	}
	if !ok {
		return core.StatusView{}, core.Exit(4, "superserve sandbox %q is not claimed by Crabbox", req.ID)
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
	for {
		sb, getErr := api.GetSandbox(pollCtx, sandboxID)
		if getErr != nil {
			if req.Wait && ctx.Err() == nil && pollCtx.Err() != nil {
				return core.StatusView{}, core.Exit(5, "timed out waiting for superserve sandbox %s to become ready", sandboxID)
			}
			if ctx.Err() != nil {
				return core.StatusView{}, ctx.Err()
			}
			return core.StatusView{}, getErr
		}
		if err := validateSuperserveSandboxOwnership(claim, sb); err != nil {
			return core.StatusView{}, err
		}
		state := normalizedSandboxState(sb)
		view := core.StatusView{
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
			return core.StatusView{}, core.Exit(5, "superserve sandbox %s entered terminal state %q before becoming ready", sandboxID, state)
		}
		if core.ClockNow(b.rt.Clock).After(deadline) {
			return core.StatusView{}, core.Exit(5, "timed out waiting for superserve sandbox %s to become ready", sandboxID)
		}
		select {
		case <-pollCtx.Done():
			if ctx.Err() == nil {
				return core.StatusView{}, core.Exit(5, "timed out waiting for superserve sandbox %s to become ready", sandboxID)
			}
			return core.StatusView{}, pollCtx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *backend) Stop(ctx context.Context, req core.StopRequest) error {
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
		core.RemoveLeaseClaim(leaseID)
		return nil
	}
	if err := api.DeleteSandbox(ctx, sandboxID); err != nil {
		if !isSuperserveNotFound(err) || !b.cfg.Superserve.ForgetMissing {
			return err
		}
		fmt.Fprintf(b.rt.Stderr, "warning: forgetting missing superserve sandbox=%s after explicit request\n", sandboxID)
	}
	core.RemoveLeaseClaim(leaseID)
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s\n", leaseID, sandboxID)
	return nil
}

func (b *backend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	api, err := b.client()
	if err != nil {
		return err
	}
	claims, err := listSuperserveLeaseClaims()
	if err != nil {
		return err
	}
	now := core.ClockNow(b.rt.Clock).UTC()
	return shared.CleanupSandboxClaims(ctx, req, claims, shared.SandboxClaimCleanup[superserveSandbox]{
		Provider:          providerName,
		Runtime:           b.rt,
		Now:               now,
		MatchesScope:      func(claim core.LeaseClaim) bool { return superserveClaimMatchesEndpoint(claim, api.BaseURL()) },
		Lock:              lockSuperserveLeaseOperation,
		SandboxID:         func(claim core.LeaseClaim) string { return strings.TrimPrefix(claim.LeaseID, leasePrefix) },
		Get:               api.GetSandbox,
		Delete:            api.DeleteSandbox,
		IsNotFound:        isSuperserveNotFound,
		ForgetMissing:     b.cfg.Superserve.ForgetMissing,
		ForgetMissingHint: "superserve forget-missing",
		Due:               shared.ClaimIdleCleanupDue,
		Validate:          validateSuperserveSandboxOwnership,
	})
}

func (b *backend) createSandbox(ctx context.Context, api superserveClient, repo core.Repo, reclaim bool, requestedSlug string) (string, string, string, func(), error) {
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
	createdID := sb.ID
	if strings.TrimSpace(createdID) == "" {
		return "", "", "", nil, core.Exit(5, "superserve create response omitted sandbox identity")
	}
	leaseID := leasePrefix + createdID
	unlockOperation, err := lockSuperserveLeaseOperation(ctx, leaseID)
	if err != nil {
		return leaseID, createdID, "", nil, b.cleanupCreateFailure(ctx, api, createdID, err)
	}
	keepLock := false
	defer func() {
		if !keepLock {
			unlockOperation()
		}
	}()
	slug, err := core.AllocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return leaseID, createdID, "", nil, b.cleanupCreateFailure(ctx, api, createdID, err)
	}
	metadata := b.ownershipMetadata(api.BaseURL(), providerScope, leaseID, slug, repo)
	sb, err = api.UpdateSandboxMetadata(ctx, createdID, metadata)
	if err != nil {
		return leaseID, createdID, slug, nil, b.cleanupCreateFailure(ctx, api, createdID, err)
	}
	if sb.ID != createdID {
		return leaseID, createdID, slug, nil, b.cleanupCreateFailure(ctx, api, createdID, core.Exit(5, "superserve metadata response changed sandbox identity"))
	}
	if err := validateSuperserveSandboxOwnership(core.LeaseClaim{LeaseID: leaseID, Provider: providerName, ProviderScope: providerScope}, sb); err != nil {
		return leaseID, createdID, slug, nil, b.cleanupCreateFailure(ctx, api, createdID, err)
	}
	if err := core.ClaimLeaseForRepoProviderScopePond(leaseID, slug, providerName, providerScope, b.cfg.Pond, repo.Root, b.cfg.IdleTimeout, reclaim); err != nil {
		return leaseID, createdID, slug, nil, b.cleanupCreateFailure(ctx, api, createdID, err)
	}
	keepLock = true
	return leaseID, createdID, slug, unlockOperation, nil
}

func superserveCreateSource(cfg core.Config) (string, string) {
	snapshot := strings.TrimSpace(cfg.Superserve.Snapshot)
	if snapshot != "" {
		return "", snapshot
	}
	return strings.TrimSpace(cfg.Superserve.Template), ""
}

func superserveNetworkConfig(cfg core.Config) *createSandboxNetworkCfg {
	if len(cfg.Superserve.NetworkAllowOut) == 0 && len(cfg.Superserve.NetworkDenyOut) == 0 {
		return nil
	}
	return &createSandboxNetworkCfg{
		AllowOut: append([]string(nil), cfg.Superserve.NetworkAllowOut...),
		DenyOut:  append([]string(nil), cfg.Superserve.NetworkDenyOut...),
	}
}

func (b *backend) ownershipMetadata(baseURL, providerScope, leaseID, slug string, repo core.Repo) map[string]string {
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

func (b *backend) serverFromSandbox(claim core.LeaseClaim, sb superserveSandbox) core.Server {
	state := normalizedSandboxState(sb)
	return core.Server{
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
	return shared.ResolveScopedLeaseID(id, shared.ScopedLeaseResolver{
		Provider:      providerName,
		LeasePrefix:   leasePrefix,
		ReadClaim:     core.ReadLeaseClaim,
		ListClaims:    listSuperserveLeaseClaims,
		ValidateClaim: func(claim core.LeaseClaim) error { return validateSuperserveClaimScope(claim, baseURL) },
		FinishClaim: func(claim core.LeaseClaim) (string, string, string, error) {
			return finishResolvedLease(claim, repoRoot, reclaim, idleTimeout, baseURL)
		},
		EmptyIdentifierError: func() error {
			return core.Exit(2, "provider=superserve requires a Crabbox-created sandbox slug or lease id")
		},
		UnclaimedIdentifierError: func(identifier string) error {
			return core.Exit(4, "superserve sandbox %q is not claimed by Crabbox; use a Crabbox slug or %s<sandbox-id>", identifier, leasePrefix)
		},
	})
}

func resolveSuperserveLeaseClaim(identifier, baseURL string) (core.LeaseClaim, bool, error) {
	return shared.ResolveScopedLeaseClaim(identifier, providerName, listSuperserveLeaseClaims, func(claim core.LeaseClaim) error {
		return validateSuperserveClaimScope(claim, baseURL)
	})
}

func finishResolvedLease(claim core.LeaseClaim, repoRoot string, reclaim bool, idleTimeout time.Duration, baseURL string) (string, string, string, error) {
	return shared.FinishScopedLease(claim, shared.ScopedLeaseFinishOptions{
		Provider:      providerName,
		LeasePrefix:   leasePrefix,
		RepoRoot:      repoRoot,
		Reclaim:       reclaim,
		IdleTimeout:   idleTimeout,
		ValidateClaim: func(claim core.LeaseClaim) error { return validateSuperserveClaimScope(claim, baseURL) },
	})
}

func newSuperserveClaimScope(baseURL string) (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", core.Exit(5, "generate superserve ownership token: %v", err)
	}
	return superserveEndpointScope(baseURL) + "/ownership:" + hex.EncodeToString(token[:]), nil
}

func validateSuperserveClaimScope(claim core.LeaseClaim, baseURL string) error {
	if !superserveClaimMatchesEndpoint(claim, baseURL) {
		return core.Exit(4, "superserve lease %q belongs to a different API endpoint; restore the endpoint used to create it", claim.LeaseID)
	}
	return nil
}

func superserveClaimMatchesEndpoint(claim core.LeaseClaim, baseURL string) bool {
	return strings.HasPrefix(strings.TrimSpace(claim.ProviderScope), superserveEndpointScope(baseURL)+"/ownership:")
}

func verifySuperserveClaim(ctx context.Context, api superserveClient, leaseID, sandboxID string) (superserveSandbox, error) {
	claim, err := core.ReadLeaseClaim(leaseID)
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

func validateSuperserveSandboxOwnership(claim core.LeaseClaim, sb superserveSandbox) error {
	if sb.ID == "" {
		return core.Exit(5, "superserve returned a sandbox without an id")
	}
	if sb.Metadata[metadataProviderKey] != providerName ||
		sb.Metadata[metadataScopeKey] != claim.ProviderScope ||
		sb.Metadata[metadataClaimKey] != claim.LeaseID {
		return core.Exit(4, "superserve sandbox %q ownership metadata does not match its local claim", sb.ID)
	}
	return nil
}

func (b *backend) refreshSuperserveLeaseActivity(leaseID string) error {
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		return err
	}
	if claim.LeaseID == "" {
		return nil
	}
	idleTimeout := timeoutOrDefault(b.cfg.IdleTimeout, time.Duration(claim.IdleTimeoutSeconds)*time.Second)
	return core.ClaimLeaseForRepoProviderScopePond(
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

func (b *backend) cleanupTimeout() time.Duration {
	if b.cleanupTimeoutOverride > 0 {
		return b.cleanupTimeoutOverride
	}
	return superserveCleanupTimeout
}

func (b *backend) cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), b.cleanupTimeout())
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

func normalizedSandboxState(sb superserveSandbox) string {
	return strings.ToLower(core.Blank(strings.TrimSpace(sb.Status), core.Blank(strings.TrimSpace(sb.State), "unknown")))
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

func newSandboxName(repo core.Repo) string {
	base := core.NormalizeLeaseSlug(repo.Name)
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
	return namePrefix + base + "-" + shared.RandomSuffix()
}

func repoScope(repo core.Repo) string {
	value := strings.TrimSpace(repo.Root)
	if value == "" {
		value = strings.TrimSpace(repo.Name)
	}
	sum := sha256.Sum256([]byte(value))
	return "repo-sha256:" + hex.EncodeToString(sum[:8])
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
