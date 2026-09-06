package superserve

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

func (b *backend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
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
				return exit(2, "provider=superserve is delegated-run only and does not support Tailscale options")
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
				TempPattern: "crabbox-superserve-sync-*.tgz", Stderr: b.rt.Stderr, Now: b.now,
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
			claim, err := readLeaseClaim(leaseID)
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
				printEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, commandEnv)
			}
			return shared.DelegatedSandboxCommand{
				Text: commandText,
				Run: func(ctx context.Context) (int, error) {
					res, err := api.Exec(ctx, &access, execRequest{
						Command: commandText, WorkingDir: workdir, Env: commandEnv, TimeoutSecs: b.execTimeoutSecs(),
					}, b.rt.Stdout, b.rt.Stderr)
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
			removeLeaseClaim(leaseID)
			return nil
		},
	})
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
	createdID := sb.ID
	if strings.TrimSpace(createdID) == "" {
		return "", "", "", nil, exit(5, "superserve create response omitted sandbox identity")
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
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return leaseID, createdID, "", nil, b.cleanupCreateFailure(ctx, api, createdID, err)
	}
	metadata := b.ownershipMetadata(api.BaseURL(), providerScope, leaseID, slug, repo)
	sb, err = api.UpdateSandboxMetadata(ctx, createdID, metadata)
	if err != nil {
		return leaseID, createdID, slug, nil, b.cleanupCreateFailure(ctx, api, createdID, err)
	}
	if sb.ID != createdID {
		return leaseID, createdID, slug, nil, b.cleanupCreateFailure(ctx, api, createdID, exit(5, "superserve metadata response changed sandbox identity"))
	}
	if err := validateSuperserveSandboxOwnership(LeaseClaim{LeaseID: leaseID, Provider: providerName, ProviderScope: providerScope}, sb); err != nil {
		return leaseID, createdID, slug, nil, b.cleanupCreateFailure(ctx, api, createdID, err)
	}
	if err := claimLeaseForRepoProviderScopePond(leaseID, slug, providerName, providerScope, b.cfg.Pond, repo.Root, b.cfg.IdleTimeout, reclaim); err != nil {
		return leaseID, createdID, slug, nil, b.cleanupCreateFailure(ctx, api, createdID, err)
	}
	keepLock = true
	return leaseID, createdID, slug, unlockOperation, nil
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
	return shared.ResolveScopedLeaseID(id, shared.ScopedLeaseResolver{
		Provider:      providerName,
		LeasePrefix:   leasePrefix,
		ReadClaim:     readLeaseClaim,
		ListClaims:    listSuperserveLeaseClaims,
		ValidateClaim: func(claim LeaseClaim) error { return validateSuperserveClaimScope(claim, baseURL) },
		FinishClaim: func(claim LeaseClaim) (string, string, string, error) {
			return finishResolvedLease(claim, repoRoot, reclaim, idleTimeout, baseURL)
		},
		EmptyIdentifierError: func() error {
			return exit(2, "provider=superserve requires a Crabbox-created sandbox slug or lease id")
		},
		UnclaimedIdentifierError: func(identifier string) error {
			return exit(4, "superserve sandbox %q is not claimed by Crabbox; use a Crabbox slug or %s<sandbox-id>", identifier, leasePrefix)
		},
	})
}

func resolveSuperserveLeaseClaim(identifier, baseURL string) (LeaseClaim, bool, error) {
	return shared.ResolveScopedLeaseClaim(identifier, providerName, listSuperserveLeaseClaims, func(claim LeaseClaim) error {
		return validateSuperserveClaimScope(claim, baseURL)
	})
}

func finishResolvedLease(claim LeaseClaim, repoRoot string, reclaim bool, idleTimeout time.Duration, baseURL string) (string, string, string, error) {
	return shared.FinishScopedLease(claim, shared.ScopedLeaseFinishOptions{
		Provider:      providerName,
		LeasePrefix:   leasePrefix,
		RepoRoot:      repoRoot,
		Reclaim:       reclaim,
		IdleTimeout:   idleTimeout,
		ValidateClaim: func(claim LeaseClaim) error { return validateSuperserveClaimScope(claim, baseURL) },
	})
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
	return shared.RandomSuffix()
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
