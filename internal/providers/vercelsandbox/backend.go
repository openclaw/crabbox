package vercelsandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
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
	vercelSandboxCleanupTimeout = 15 * time.Second

	metadataProviderKey = "crabbox.provider"
	metadataScopeKey    = "crabbox.scope"
	metadataClaimKey    = "crabbox.claim"
	metadataRepoKey     = "crabbox.repo"
	metadataSlugKey     = "crabbox.slug"
)

func (b *backend) Warmup(ctx context.Context, req core.WarmupRequest) error {
	if req.ActionsRunner {
		return core.Exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	if req.Options.Tailscale.Enabled {
		return core.Exit(2, "provider=%s is delegated-run only and does not support Tailscale options", providerName)
	}
	if _, err := vercelSandboxWorkdir(b.cfg); err != nil {
		return err
	}
	started := core.ClockNow(b.rt.Clock)
	api, err := b.client()
	if err != nil {
		return err
	}
	if err := b.bindProviderScope(ctx, api, false); err != nil {
		return err
	}
	leaseID, sandboxID, slug, unlockOperation, err := b.createSandbox(ctx, api, req.Repo, req.Reclaim, req.RequestedSlug, true)
	if err != nil {
		return err
	}
	defer unlockOperation()
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s sandbox=%s runtime=%s\n", leaseID, slug, providerName, sandboxID, vercelSandboxRuntime(b.cfg))
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: vercel-sandbox warmup keeps the sandbox until explicit stop\n")
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
	workdir, workdirErr := vercelSandboxWorkdir(b.cfg)
	var api vercelSandboxClient
	var leaseID, sandboxID, slug string
	session := func(unlock func()) shared.DelegatedSandbox {
		return shared.DelegatedSandbox{LeaseID: leaseID, Slug: slug, Unlock: unlock, CleanupCommand: vercelSandboxCleanupCommand(leaseID)}
	}
	return shared.RunDelegatedSandbox(ctx, req, shared.DelegatedSandboxLifecycle{
		Provider: providerName, Runtime: b.rt, Workdir: workdir,
		IdleTimeout: b.cfg.IdleTimeout, TTL: b.cfg.TTL, CleanupTimeout: vercelSandboxCleanupTimeout,
		Preflight: func(context.Context) error {
			if req.Options.Tailscale.Enabled {
				return core.Exit(2, "provider=%s is delegated-run only and does not support Tailscale options", providerName)
			}
			if workdirErr != nil {
				return workdirErr
			}
			var err error
			api, err = b.client()
			return err
		},
		Workspace: func() shared.SandboxWorkspace { return b.workspace(api, sandboxID, req, workdir) },
		Acquire: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			if err := b.bindProviderScope(ctx, api, false); err != nil {
				return shared.DelegatedSandbox{}, err
			}
			var unlock func()
			var err error
			leaseID, sandboxID, slug, unlock, err = b.createSandbox(ctx, api, req.Repo, req.Reclaim, req.RequestedSlug, req.Keep || req.KeepOnFailure)
			if err != nil {
				return shared.DelegatedSandbox{Unlock: unlock}, err
			}
			fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s sandbox=%s runtime=%s\n", leaseID, slug, providerName, sandboxID, vercelSandboxRuntime(b.cfg))
			return session(unlock), nil
		},
		Resolve: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			if err := b.bindProviderScope(ctx, api, true); err != nil {
				return shared.DelegatedSandbox{}, err
			}
			var err error
			leaseID, sandboxID, _, err = b.resolveLeaseID(req.ID, "", false, 0)
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			unlock, err := lockVercelSandboxLeaseOperation(ctx, leaseID)
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			unbound := shared.DelegatedSandbox{Unlock: unlock}
			leaseID, sandboxID, _, err = b.resolveLeaseID(leaseID, "", false, 0)
			if err != nil {
				return unbound, err
			}
			if _, err := shared.VerifySandboxClaim(ctx, leaseID, sandboxID, b.validateClaimScope, api.GetSandbox, validateSandboxOwnership); err != nil {
				return unbound, err
			}
			claim, err := core.ReadLeaseClaim(leaseID)
			if err != nil {
				return unbound, err
			}
			_, _, slug, err = b.finishResolvedLease(claim, req.Repo.Root, req.Reclaim, b.cfg.IdleTimeout)
			if err != nil {
				return unbound, err
			}
			return session(unlock), nil
		},
		Setup: func(context.Context) error {
			fmt.Fprintf(b.rt.Stderr, "provider=%s lease=%s sandbox=%s workdir=%s\n", providerName, leaseID, sandboxID, workdir)
			return nil
		},
		Command: func(context.Context) (shared.DelegatedSandboxCommand, error) {
			intent, err := core.ParseCommandIntent(req.Command, req.ShellMode, req.CommandLiteralArgs)
			if err != nil {
				return shared.DelegatedSandboxCommand{}, err
			}
			commandText := intent.ShellCommand("bash", "-lc")
			commandEnv, strippedAuthEnv := vercelSandboxCommandEnv(req.Env)
			if len(strippedAuthEnv) > 0 {
				fmt.Fprintf(b.rt.Stderr, "warning: provider=%s did not forward provider authentication variables: %s\n", providerName, strings.Join(strippedAuthEnv, ","))
			}
			if req.EnvSummary || strings.TrimSpace(os.Getenv("CRABBOX_ENV_ALLOW")) != "" {
				core.PrintEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, commandEnv)
			}
			return shared.DelegatedSandboxCommand{Text: commandText, Run: func(ctx context.Context, stdout, stderr io.Writer) (int, error) {
				result, err := api.Exec(ctx, sandboxID, execRequest{Command: commandText, WorkingDir: workdir, Env: commandEnv, TimeoutSecs: b.execTimeoutSecs()}, stdout, stderr)
				if err != nil {
					return result.ExitCode, shared.ExitErrorWithCause(1, redactSecrets(err.Error()), err)
				}
				return result.ExitCode, err
			}}, nil
		},
		Retained: func(context.Context) error {
			err := shared.RefreshRetainedLeaseActivity(leaseID, providerName, b.cfg.IdleTimeout)
			if err != nil {
				fmt.Fprintf(b.rt.Stderr, "warning: refresh vercel-sandbox lease activity failed lease=%s: %v\n", leaseID, err)
			}
			return err
		},
		Cleanup: func(ctx context.Context) error { return b.cleanupCreatedRun(ctx, api, leaseID, sandboxID) },
	})
}

func (b *backend) List(ctx context.Context, _ core.ListRequest) ([]core.LeaseView, error) {
	api, err := b.client()
	if err != nil {
		return nil, err
	}
	sandboxes, err := api.ListSandboxes(ctx)
	if err != nil {
		return nil, err
	}
	if len(sandboxes) == 0 {
		return []core.LeaseView{}, nil
	}
	if err := b.bindProviderScope(ctx, api, true); err != nil {
		return nil, err
	}
	views := make([]core.LeaseView, 0, len(sandboxes))
	for _, sb := range sandboxes {
		if len(sb.Metadata) == 0 && strings.HasPrefix(sb.ID, leasePrefix) {
			claim, err := core.ReadLeaseClaim(sb.ID)
			if err != nil {
				return nil, err
			}
			if claim.LeaseID == "" || claim.Provider != providerName || !b.claimMatchesActiveScope(claim) {
				continue
			}
			remote, err := api.GetSandbox(ctx, strings.TrimPrefix(claim.LeaseID, leasePrefix))
			if err != nil {
				if isVercelSandboxNotFound(err) {
					continue
				}
				return nil, err
			}
			sb = remote
		}
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

func (b *backend) Status(ctx context.Context, req core.StatusRequest) (core.StatusView, error) {
	api, err := b.client()
	if err != nil {
		return core.StatusView{}, err
	}
	if err := b.bindProviderScope(ctx, api, true); err != nil {
		return core.StatusView{}, err
	}
	leaseID, sandboxID, slug, err := b.resolveLeaseID(req.ID, "", false, 0)
	if err != nil {
		return core.StatusView{}, err
	}
	claim, ok, err := b.resolveVercelSandboxLeaseClaim(leaseID)
	if err != nil {
		return core.StatusView{}, err
	}
	if !ok {
		return core.StatusView{}, core.Exit(4, "vercel-sandbox sandbox %q is not claimed by Crabbox", req.ID)
	}
	wait := shared.NewStatusWait(ctx, req, b.rt.Clock, func(id string) error {
		return core.Exit(5, "timed out waiting for vercel-sandbox sandbox %s to become ready", id)
	})
	return shared.ObserveSandboxStatus(wait, sandboxID, 2*time.Second, api.GetSandbox,
		func(sb sandboxSummary) error { return validateSandboxOwnership(claim, sb) },
		func(_ context.Context, sb sandboxSummary) (core.StatusView, error) {
			state := normalizedSandboxState(sb)
			view := shared.SandboxStatusView(providerName, leaseID, slug, sandboxID, claim.Pond, state, isReadyState(state))
			view.Labels["slug"] = slug
			return view, nil
		}, isTerminalState,
		func(id, state string) error {
			return core.Exit(5, "vercel-sandbox sandbox %s entered terminal state %q before becoming ready", id, state)
		})
}

func (b *backend) Stop(ctx context.Context, req core.StopRequest) error {
	api, err := b.client()
	if err != nil {
		return err
	}
	if err := b.bindProviderScope(ctx, api, true); err != nil {
		return err
	}
	leaseID, _, _, err := b.resolveLeaseID(req.ID, "", false, 0)
	if err != nil {
		return err
	}
	unlockOperation, err := lockVercelSandboxLeaseOperation(ctx, leaseID)
	if err != nil {
		return err
	}
	defer unlockOperation()
	leaseID, sandboxID, _, err := b.resolveLeaseID(leaseID, "", false, 0)
	if err != nil {
		return err
	}
	if _, err := shared.VerifySandboxClaim(ctx, leaseID, sandboxID, b.validateClaimScope, api.GetSandbox, validateSandboxOwnership); err != nil {
		if !isVercelSandboxNotFound(err) || !b.cfg.VercelSandbox.ForgetMissing {
			return err
		}
		fmt.Fprintf(b.rt.Stderr, "warning: forgetting missing vercel-sandbox sandbox=%s after explicit request\n", sandboxID)
		core.RemoveLeaseClaim(leaseID)
		return nil
	}
	if err := api.DeleteSandbox(ctx, sandboxID); err != nil {
		if !isVercelSandboxNotFound(err) || !b.cfg.VercelSandbox.ForgetMissing {
			return err
		}
		fmt.Fprintf(b.rt.Stderr, "warning: forgetting missing vercel-sandbox sandbox=%s after explicit request\n", sandboxID)
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
	claims, err := listVercelSandboxLeaseClaims()
	if err != nil {
		return err
	}
	hasProviderClaims := slices.ContainsFunc(claims, func(claim core.LeaseClaim) bool {
		return claim.Provider == providerName
	})
	if !hasProviderClaims {
		if !req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "%s cleanup removed=0 claims_removed=0 checked=0\n", providerName)
		}
		return nil
	}
	if err := b.bindProviderScope(ctx, api, true); err != nil {
		return err
	}
	now := core.ClockNow(b.rt.Clock).UTC()
	return shared.CleanupSandboxClaims(ctx, req, claims, shared.SandboxClaimCleanup[sandboxSummary]{
		Provider:          providerName,
		Runtime:           b.rt,
		Now:               now,
		MatchesScope:      b.claimMatchesActiveScope,
		Lock:              lockVercelSandboxLeaseOperation,
		SandboxID:         func(claim core.LeaseClaim) string { return strings.TrimPrefix(claim.LeaseID, leasePrefix) },
		Get:               api.GetSandbox,
		Delete:            api.DeleteSandbox,
		IsNotFound:        isVercelSandboxNotFound,
		ForgetMissing:     b.cfg.VercelSandbox.ForgetMissing,
		ForgetMissingHint: "vercelSandbox.forgetMissing",
		Due:               shared.ClaimIdleCleanupDue,
		Validate:          validateSandboxOwnership,
	})
}

func (b *backend) createSandbox(ctx context.Context, api vercelSandboxClient, repo core.Repo, reclaim bool, requestedSlug string, retained bool) (string, string, string, func(), error) {
	if err := validateVercelSandboxConfig(b.cfg); err != nil {
		return "", "", "", nil, err
	}
	providerScope, err := b.newClaimScope()
	if err != nil {
		return "", "", "", nil, err
	}
	if _, err := vercelSandboxWorkdir(b.cfg); err != nil {
		return "", "", "", nil, err
	}
	name := newSandboxName(repo)
	tentativeLeaseID := leasePrefix + name
	slug, err := core.AllocateClaimLeaseSlug(tentativeLeaseID, requestedSlug)
	if err != nil {
		return "", "", "", nil, err
	}
	initialMetadata := b.ownershipMetadata(providerScope, tentativeLeaseID, slug, repo)
	sb, err := api.CreateSandbox(ctx, createSandboxRequest{
		Name:       name,
		Persistent: retained || b.cfg.VercelSandbox.Persistent,
		Metadata:   initialMetadata,
	})
	if err != nil {
		return "", "", "", nil, err
	}
	leaseID := leasePrefix + sb.ID
	unlockOperation, err := lockVercelSandboxLeaseOperation(ctx, leaseID)
	if err != nil {
		return leaseID, sb.ID, "", nil, b.cleanupCreateFailure(ctx, api, sb.ID, err)
	}
	keepLock := false
	defer func() {
		if !keepLock {
			unlockOperation()
		}
	}()
	if leaseID != tentativeLeaseID {
		slug, err = core.AllocateClaimLeaseSlug(leaseID, requestedSlug)
		if err != nil {
			return leaseID, sb.ID, "", nil, b.cleanupCreateFailure(ctx, api, sb.ID, err)
		}
	}
	metadata := b.ownershipMetadata(providerScope, leaseID, slug, repo)
	if leaseID != tentativeLeaseID {
		sb, err = api.UpdateSandboxMetadata(ctx, sb.ID, metadata)
		if err != nil {
			return leaseID, sb.ID, slug, nil, b.cleanupCreateFailure(ctx, api, sb.ID, err)
		}
	} else if sb.Metadata == nil || sb.Metadata[metadataClaimKey] == "" {
		sb.Metadata = metadata
	}
	if err := validateSandboxOwnership(core.LeaseClaim{LeaseID: leaseID, Provider: providerName, ProviderScope: providerScope}, sb); err != nil {
		return leaseID, sb.ID, slug, nil, b.cleanupCreateFailure(ctx, api, sb.ID, err)
	}
	if err := core.ClaimLeaseForRepoProviderScopePond(leaseID, slug, providerName, providerScope, b.cfg.Pond, repo.Root, b.cfg.IdleTimeout, reclaim); err != nil {
		return leaseID, sb.ID, slug, nil, b.cleanupCreateFailure(ctx, api, sb.ID, err)
	}
	keepLock = true
	return leaseID, sb.ID, slug, unlockOperation, nil
}

func (b *backend) ownershipMetadata(providerScope, leaseID, slug string, repo core.Repo) map[string]string {
	out := map[string]string{
		metadataProviderKey: providerName,
		metadataScopeKey:    providerScope,
		metadataRepoKey:     shared.SandboxRepositoryMetadataScope(repo),
	}
	if leaseID != "" {
		out[metadataClaimKey] = leaseID
	}
	if slug != "" {
		out[metadataSlugKey] = slug
	}
	return out
}

func (b *backend) serverFromSandbox(claim core.LeaseClaim, sb sandboxSummary) core.Server {
	state := normalizedSandboxState(sb)
	return shared.SandboxLeaseView(providerName, targetLinux, claim, sb.ID, sb.ID, state)
}

func (b *backend) resolveLeaseID(id, repoRoot string, reclaim bool, idleTimeout time.Duration) (string, string, string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", "", "", core.Exit(2, "provider=vercel-sandbox requires a Crabbox-created sandbox slug or lease id")
	}
	exactLeaseID := id
	if !strings.HasPrefix(exactLeaseID, leasePrefix) {
		exactLeaseID = leasePrefix + exactLeaseID
	}
	if claim, err := core.ReadLeaseClaim(exactLeaseID); err != nil {
		return "", "", "", err
	} else if claim.LeaseID == exactLeaseID && claim.Provider == providerName {
		return b.finishResolvedLease(claim, repoRoot, reclaim, idleTimeout)
	}
	claim, ok, err := b.resolveVercelSandboxLeaseClaim(id)
	if err != nil {
		return "", "", "", err
	}
	if ok {
		return b.finishResolvedLease(claim, repoRoot, reclaim, idleTimeout)
	}
	return "", "", "", core.Exit(4, "vercel-sandbox sandbox %q is not claimed by Crabbox; use a Crabbox slug or %s<sandbox-id>", id, leasePrefix)
}

func (b *backend) resolveVercelSandboxLeaseClaim(identifier string) (core.LeaseClaim, bool, error) {
	claims, err := listVercelSandboxLeaseClaims()
	if err != nil {
		return core.LeaseClaim{}, false, err
	}
	for _, claim := range claims {
		if claim.Provider == providerName && claim.LeaseID == identifier {
			if err := b.validateClaimScope(claim); err != nil {
				return core.LeaseClaim{}, false, err
			}
			return claim, true, nil
		}
	}
	slug := core.NormalizeLeaseSlug(identifier)
	if slug != "" {
		for _, legacy := range []bool{false, true} {
			for _, claim := range claims {
				if claim.Provider != providerName || core.NormalizeLeaseSlug(claim.Slug) != slug {
					continue
				}
				isLegacy := !claimMatchesScope(claim, b.providerScopeBase())
				if isLegacy != legacy || !b.claimMatchesActiveScope(claim) {
					continue
				}
				return claim, true, nil
			}
		}
	}
	return core.LeaseClaim{}, false, nil
}

func (b *backend) finishResolvedLease(claim core.LeaseClaim, repoRoot string, reclaim bool, idleTimeout time.Duration) (string, string, string, error) {
	return shared.FinishScopedLease(claim, shared.ScopedLeaseFinishOptions{
		Provider: providerName, LeasePrefix: leasePrefix, RepoRoot: repoRoot,
		Reclaim: reclaim, IdleTimeout: idleTimeout, ValidateClaim: b.validateClaimScope,
	})
}

func (b *backend) newClaimScope() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", core.Exit(5, "generate vercel-sandbox ownership token: %v", err)
	}
	return b.providerScopeBase() + "/ownership:" + hex.EncodeToString(token[:]), nil
}

func (b *backend) bindProviderScope(ctx context.Context, api vercelSandboxClient, readOnly bool) error {
	previousScopeBase := b.providerScopeBase()
	scope, err := api.ResolveProjectScope(ctx, readOnly)
	if err != nil {
		return err
	}
	b.resolvedProject = strings.TrimSpace(scope.ProjectID)
	b.resolvedTeam = strings.TrimSpace(scope.TeamID)
	if resolvedScopeBase := b.providerScopeBase(); previousScopeBase != resolvedScopeBase && b.legacyScopeBase == "" {
		b.legacyScopeBase = previousScopeBase
	}
	return nil
}

func (b *backend) providerScopeBase() string {
	projectID := core.Blank(strings.TrimSpace(b.resolvedProject), strings.TrimSpace(b.cfg.VercelSandbox.ProjectID))
	teamID := core.Blank(strings.TrimSpace(b.resolvedTeam), strings.TrimSpace(b.cfg.VercelSandbox.TeamID))
	parts := []string{
		"scope:" + core.Blank(strings.TrimSpace(b.cfg.VercelSandbox.Scope), "-"),
		"team:" + core.Blank(teamID, "-"),
		"project:" + core.Blank(projectID, "-"),
	}
	return strings.Join(parts, "/")
}

func (b *backend) validateClaimScope(claim core.LeaseClaim) error {
	if !b.claimMatchesActiveScope(claim) {
		return core.Exit(4, "vercel-sandbox lease %q belongs to a different project/team/scope; restore the configuration used to create it", claim.LeaseID)
	}
	return nil
}

func (b *backend) claimMatchesActiveScope(claim core.LeaseClaim) bool {
	if claimMatchesScope(claim, b.providerScopeBase()) {
		return true
	}
	// Legacy claims are candidates only. Callers verify their remote ownership
	// tags through the currently authenticated project before use.
	return b.legacyScopeBase != "" && claimMatchesScope(claim, b.legacyScopeBase)
}

func claimMatchesScope(claim core.LeaseClaim, scopeBase string) bool {
	return strings.HasPrefix(strings.TrimSpace(claim.ProviderScope), scopeBase+"/ownership:")
}

func validateSandboxOwnership(claim core.LeaseClaim, sb sandboxSummary) error {
	return shared.ValidateSandboxOwnershipMetadata(providerName, sb.ID, sb.Metadata, claim)
}

func (b *backend) cleanupCreateFailure(ctx context.Context, api vercelSandboxClient, sandboxID string, cause error) error {
	cleanupCtx, cancel := b.cleanupContext(ctx)
	defer cancel()
	if err := api.DeleteSandbox(cleanupCtx, sandboxID); err != nil {
		if isVercelSandboxNotFound(err) {
			return cause
		}
		return errors.Join(cause, fmt.Errorf("vercel-sandbox cleanup failed for sandbox %s; delete it in the Vercel dashboard: %w", sandboxID, err))
	}
	return cause
}

func (b *backend) cleanupCreatedRun(ctx context.Context, api vercelSandboxClient, leaseID, sandboxID string) error {
	if err := api.DeleteSandbox(ctx, sandboxID); err != nil && !isVercelSandboxNotFound(err) {
		return fmt.Errorf("vercel-sandbox delete failed for %s: %w", sandboxID, err)
	}
	core.RemoveLeaseClaim(leaseID)
	return nil
}

func (b *backend) cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), vercelSandboxCleanupTimeout)
}

func (b *backend) execTimeoutSecs() int {
	return b.cfg.VercelSandbox.ExecTimeoutSecs
}

func normalizedSandboxState(sb sandboxSummary) string {
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
	case "terminated", "stopped", "failed", "error", "aborted", "killed", "deleted", "destroyed":
		return true
	default:
		return false
	}
}

func vercelSandboxRuntime(cfg core.Config) string {
	return core.Blank(strings.TrimSpace(cfg.VercelSandbox.Runtime), defaultRuntime)
}

func newSandboxName(repo core.Repo) string {
	base := core.NormalizeLeaseSlug(repo.Name)
	if base == "" {
		base = "crabbox"
	}
	base = strings.TrimPrefix(base, "crabbox-")
	if base == "" {
		base = "crabbox"
	}
	if len(base) > 40 {
		base = strings.Trim(base[:40], "-")
	}
	return "crabbox-" + base + "-" + shared.RandomSuffix()
}

func vercelSandboxCommandEnv(env map[string]string) (map[string]string, []string) {
	if len(env) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(env))
	var stripped []string
	for name, value := range env {
		switch name {
		case "CRABBOX_VERCEL_SANDBOX_AUTH_TOKEN", "CRABBOX_VERCEL_AUTH_TOKEN", "VERCEL_AUTH_TOKEN",
			"CRABBOX_VERCEL_SANDBOX_TOKEN", "CRABBOX_VERCEL_TOKEN", "VERCEL_TOKEN",
			"CRABBOX_VERCEL_SANDBOX_OIDC_TOKEN", "VERCEL_OIDC_TOKEN":
			stripped = append(stripped, name)
		default:
			out[name] = value
		}
	}
	slices.Sort(stripped)
	return out, stripped
}

type vercelSandboxNotFoundError struct {
	err error
}

func (e *vercelSandboxNotFoundError) Error() string { return e.err.Error() }
func (e *vercelSandboxNotFoundError) Unwrap() error { return e.err }

func isVercelSandboxNotFound(err error) bool {
	var notFound *vercelSandboxNotFoundError
	if errors.As(err, &notFound) {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "not found") || strings.Contains(text, "404")
}
