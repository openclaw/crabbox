package neevcloud

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

const cleanupTimeout = 30 * time.Second

var sandboxViews = shared.EnvdSandboxViews{Provider: providerName, LeasePrefix: leasePrefix}

type backend struct {
	spec core.ProviderSpec
	cfg  core.Config
	rt   core.Runtime
}

func (b *backend) Spec() core.ProviderSpec { return b.spec }

// workspace binds archive sync to the sandbox; the archive is staged under /workspace
// because the runtime API only addresses paths there.
func (b *backend) workspace(client shared.EnvdSandboxAPI, session shared.EnvdSandboxSession, req core.RunRequest, dir string) core.ArchiveWorkspace {
	archive := shared.EnvdWorkspace{
		Provider: providerName, Config: b.cfg, Runtime: b.rt,
		Workdir: dir, RemoteArchivePrefix: ".crabbox-sync-",
	}.Bind(client, session, req, dir)
	archive.RemoteArchiveDir = workspaceRoot
	return archive
}

// Warmup creates, claims and waits for a kept sandbox.
func (b *backend) Warmup(ctx context.Context, req core.WarmupRequest) error {
	started := core.ClockNow(b.rt.Clock)
	client, err := newClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	claim, sandbox, err := b.createSandbox(ctx, client, req.Repo, req.Keep, req.Reclaim, req.RequestedSlug)
	if err != nil {
		return err
	}
	leaseID, slug := claim.LeaseID, claim.Slug
	if _, err := client.ConnectSandbox(ctx, sandbox.SandboxID, 0); err != nil {
		return fmt.Errorf("neevcloud wait for sandbox %s (lease %s is kept; stop it with `%s`): %w", sandbox.SandboxID, leaseID, cleanupCommand(leaseID), err)
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s sandbox=%s\n", leaseID, slug, providerName, sandbox.SandboxID)
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: neevcloud warmup keeps the sandbox until explicit stop\n")
	}
	return shared.CompleteWarmup(b.rt, req.TimingJSON, shared.WarmupCompletion{
		Provider: providerName, LeaseID: leaseID, Slug: slug,
		Total: core.ClockNow(b.rt.Clock).Sub(started),
	})
}

// Run acquires or resolves a sandbox, syncs the archive, and streams one command.
func (b *backend) Run(ctx context.Context, req core.RunRequest) (core.RunResult, error) {
	dir, err := workdir(b.cfg)
	if err != nil {
		return core.RunResult{}, err
	}
	var client shared.EnvdSandboxAPI
	var session shared.EnvdSandboxSession
	// claim is the admitted snapshot; every sandbox effect runs fenced on it.
	var claim core.LeaseClaim
	handle := func() shared.DelegatedSandbox {
		return shared.DelegatedSandbox{LeaseID: claim.LeaseID, Slug: claim.Slug, CleanupCommand: cleanupCommand(claim.LeaseID)}
	}
	fenced := func(ctx context.Context, action func() error) error {
		return core.WithLeaseClaimUnchangedShared(ctx, claim.LeaseID, claim, action)
	}
	return shared.RunDelegatedSandbox(ctx, req, shared.DelegatedSandboxLifecycle{
		Provider: providerName, Runtime: b.rt, Workdir: dir,
		IdleTimeout: b.cfg.IdleTimeout, TTL: b.cfg.TTL, CleanupTimeout: cleanupTimeout,
		Preflight: func(context.Context) error {
			if err := core.RejectDelegatedSyncOptionsForSpec(b.spec, req); err != nil {
				return err
			}
			var err error
			client, err = newClient(b.cfg, b.rt)
			return err
		},
		Workspace: func() shared.SandboxWorkspace {
			archive := b.workspace(client, session, req, dir)
			return shared.WorkspaceOperations{
				PrepareArchiveFunc: archive.PrepareArchive,
				SyncFunc: func(ctx context.Context, prepared *core.PreparedArchive) (phases []core.TimingPhase, took time.Duration, err error) {
					err = fenced(ctx, func() error {
						var syncErr error
						if prepared == nil {
							phases, took, syncErr = archive.Sync(ctx)
						} else {
							phases, took, syncErr = archive.Sync(ctx, prepared)
						}
						return syncErr
					})
					return phases, took, err
				},
				EnsureFunc: func(ctx context.Context) error {
					return fenced(ctx, func() error { return archive.Ensure(ctx) })
				},
			}
		},
		Acquire: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			var sandbox shared.EnvdSandbox
			var err error
			if claim, sandbox, err = b.createSandbox(ctx, client, req.Repo, req.Keep, req.Reclaim, req.RequestedSlug); err != nil {
				return shared.DelegatedSandbox{}, err
			}
			fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s sandbox=%s\n", claim.LeaseID, claim.Slug, providerName, sandbox.SandboxID)
			return handle(), nil
		},
		Resolve: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			var err error
			claim, err = b.admitRunClaim(ctx, client, req.ID, req.Repo.Root, req.Reclaim)
			return handle(), err
		},
		Setup: func(ctx context.Context) error {
			var err error
			session, err = client.ConnectSandbox(ctx, claim.CloudID, 0)
			return operationError("wait for sandbox", err)
		},
		Command: func(context.Context) (shared.DelegatedSandboxCommand, error) {
			intent, err := core.ParseCommandIntent(req.Command, req.ShellMode, req.CommandLiteralArgs)
			if err != nil {
				return shared.DelegatedSandboxCommand{}, core.Exit(2, "%v", err)
			}
			command := intent.ShellSource()
			return shared.DelegatedSandboxCommand{Run: func(ctx context.Context, stdout, stderr io.Writer) (int, error) {
				fmt.Fprintf(b.rt.Stderr, "running on neevcloud %s\n", strings.Join(req.Command, " "))
				var code int
				err := fenced(ctx, func() error {
					var runErr error
					code, runErr = client.StartProcess(ctx, session, shared.EnvdSandboxProcessRequest{
						Command: command, CWD: dir, Env: req.Env, Timeout: b.cfg.TTL, Stdout: stdout, Stderr: stderr,
					})
					return runErr
				})
				return code, err
			}}, nil
		},
		Cleanup: func(ctx context.Context) error {
			return b.deleteClaimedSandbox(ctx, client, claim)
		},
	})
}

// List returns Crabbox-owned sandboxes in the configured project.
func (b *backend) List(ctx context.Context, _ core.ListRequest) ([]core.LeaseView, error) {
	client, err := newClient(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	sandboxes, err := client.ListSandboxes(ctx, map[string]string{"crabbox": "true", "provider": providerName})
	if err != nil {
		return nil, operationError("list sandboxes", err)
	}
	servers := make([]core.Server, 0, len(sandboxes))
	for _, sandbox := range sandboxes {
		servers = append(servers, sandboxViews.Server(sandbox))
	}
	return servers, nil
}

// Doctor reports config, endpoint, auth and project access as separate checks; it never mutates.
func (b *backend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	result := core.DoctorResult{Provider: providerName, Status: "ready"}
	add := func(status, check, message string) {
		result.Checks = append(result.Checks, core.DoctorCheck{Status: status, Check: check, Message: message})
	}
	block := func(check string, err error) (core.DoctorResult, error) {
		add("blocked", check, err.Error())
		result.Status, result.Message = "blocked", err.Error()
		return result, err
	}
	client, err := newClient(b.cfg, b.rt)
	if err != nil {
		return block("config", err)
	}
	add("ok", "config", "api key, org and project set")
	sandboxes, err := client.ListSandboxes(ctx, map[string]string{"crabbox": "true", "provider": providerName})
	var apiErr *apiError
	switch {
	case err == nil:
	case !errors.As(err, &apiErr):
		return block("endpoint", operationError("reach API", err))
	case apiErr.StatusCode == 401 || apiErr.StatusCode == 403:
		add("ok", "endpoint", "reachable")
		return block("auth", operationError("authenticate", err))
	default:
		add("ok", "endpoint", "reachable")
		return block("project", operationError("list sandboxes", err))
	}
	add("ok", "endpoint", "reachable")
	add("ok", "auth", "api key accepted")
	add("ok", "project", fmt.Sprintf("%d crabbox sandboxes", len(sandboxes)))
	result.Message = fmt.Sprintf("auth=ready control_plane=ready inventory=ready api=list mutation=false leases=%d runtime=unchecked", len(sandboxes))
	return result, nil
}

func (b *backend) Status(ctx context.Context, req core.StatusRequest) (core.StatusView, error) {
	client, err := newClient(b.cfg, b.rt)
	if err != nil {
		return core.StatusView{}, err
	}
	wait := shared.NewStatusWait(ctx, req, b.rt.Clock, func(id string) error {
		return core.Exit(5, "timed out waiting for sandbox %s to become ready", id)
	})
	defer wait.Close()
	leaseID, sandboxID, _, err := b.resolveSandboxID(wait.Context(), client, req.ID)
	if err != nil {
		if ctxErr := wait.ContextError(req.ID); ctxErr != nil {
			return core.StatusView{}, ctxErr
		}
		return core.StatusView{}, err
	}
	return wait.Poll(sandboxID, 2*time.Second, func(ctx context.Context) (core.StatusView, bool, error) {
		sandbox, err := client.GetSandbox(ctx, sandboxID)
		if err != nil {
			if ctxErr := wait.ContextError(sandboxID); ctxErr != nil {
				return core.StatusView{}, false, ctxErr
			}
			return core.StatusView{}, false, operationError("get sandbox", err)
		}
		return sandboxViews.Status(leaseID, sandbox), false, nil
	})
}

// Stop deletes the claimed sandbox and removes its claim; an already-absent sandbox only drops the claim.
func (b *backend) Stop(ctx context.Context, req core.StopRequest) error {
	client, err := newClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	claim, sandbox, err := b.resolveClaimedSandbox(ctx, client, req.ID)
	if err != nil {
		var missing *claimedSandboxMissingError
		if errors.As(err, &missing) {
			if removeErr := core.RemoveLeaseClaimIfUnchangedAfter(missing.claim.LeaseID, missing.claim, nil); removeErr != nil {
				return removeErr
			}
			fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s (already absent)\n", missing.claim.LeaseID, missing.claim.CloudID)
			return nil
		}
		return err
	}
	if err := b.deleteClaimedSandbox(ctx, client, claim); err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s\n", claim.LeaseID, sandbox.SandboxID)
	return nil
}

// createSandbox provisions a labelled sandbox and claims it, returning the claim it committed so
// later effects fence on that snapshot rather than a re-read; a claim failure deletes the sandbox.
func (b *backend) createSandbox(ctx context.Context, client shared.EnvdSandboxAPI, repo core.Repo, keep, reclaim bool, requestedSlug string) (core.LeaseClaim, shared.EnvdSandbox, error) {
	leaseID := core.NewLeaseID()
	slug, err := core.AllocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return core.LeaseClaim{}, shared.EnvdSandbox{}, err
	}
	cfg := b.claimConfig()
	template := cfg.ServerType
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", keep, core.ClockNow(b.rt.Clock).UTC())
	labels["state"] = "ready"
	timeoutSeconds := sandboxTimeoutSeconds(cfg)
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s template=%s max_lifetime=%ds\n", providerName, leaseID, slug, template, timeoutSeconds)
	sandbox, err := client.CreateSandbox(ctx, shared.EnvdSandboxCreateRequest{
		TemplateID:          template,
		TimeoutSeconds:      timeoutSeconds,
		Metadata:            sandboxLabels(labels),
		AllowInternetAccess: cfg.Neevcloud.AllowInternet,
	})
	if err != nil {
		return core.LeaseClaim{}, shared.EnvdSandbox{}, operationError("create sandbox", err)
	}
	if sandbox.SandboxID == "" {
		return core.LeaseClaim{}, shared.EnvdSandbox{}, core.Exit(5, "neevcloud create sandbox returned no sandbox id")
	}
	// The lease id is new, so expectedExists=false also refuses any claim that appeared meanwhile.
	claim, err := core.ClaimLeaseTargetForRepoConfigIfUnchanged(leaseID, slug, cfg, sandboxViews.Server(sandbox), core.SSHTarget{}, repo.Root, cfg.IdleTimeout, reclaim, core.LeaseClaim{}, false)
	if err != nil {
		if cleanupErr := b.deleteSandboxForCleanup(client, sandbox.SandboxID); cleanupErr != nil {
			leakErr := fmt.Errorf("cleanup neevcloud sandbox %s after claim failure: %w; delete it from the NeevCloud console", sandbox.SandboxID, cleanupErr)
			fmt.Fprintf(b.rt.Stderr, "warning: %v\n", leakErr)
			return core.LeaseClaim{}, shared.EnvdSandbox{}, errors.Join(err, leakErr)
		}
		return core.LeaseClaim{}, shared.EnvdSandbox{}, err
	}
	return claim, sandbox, nil
}

func (b *backend) deleteSandboxForCleanup(client shared.EnvdSandboxAPI, sandboxID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	return client.DeleteSandbox(ctx, sandboxID)
}

func cleanupCommand(leaseID string) string {
	return fmt.Sprintf("crabbox stop --provider %s --id %s", providerName, core.ShellQuote(leaseID))
}

// admitRunClaim binds a reused sandbox to its exact local claim, then applies repository admission.
// A sandbox with no local claim in this scope is never run, whatever its remote labels say.
func (b *backend) admitRunClaim(ctx context.Context, client shared.EnvdSandboxAPI, id, repoRoot string, reclaim bool) (core.LeaseClaim, error) {
	claim, sandbox, err := b.resolveClaimedSandbox(ctx, client, id)
	if err != nil {
		var missing *claimedSandboxMissingError
		if errors.As(err, &missing) {
			return core.LeaseClaim{}, core.Exit(4, "%v; stop it with `%s`", err, cleanupCommand(missing.claim.LeaseID))
		}
		return core.LeaseClaim{}, err
	}
	if repoRoot == "" {
		return claim, nil
	}
	return core.ClaimLeaseTargetForRepoConfigIfUnchanged(
		claim.LeaseID, claim.Slug, b.claimConfig(), sandboxViews.Server(sandbox), core.SSHTarget{}, repoRoot,
		time.Duration(claim.IdleTimeoutSeconds)*time.Second, reclaim, claim, true,
	)
}

// claimConfig is the config claims are written with; ServerType records the template.
func (b *backend) claimConfig() core.Config {
	cfg := b.cfg
	cfg.ServerType = core.Blank(b.cfg.Neevcloud.Template, core.NeevcloudConfigDefaultTemplate)
	return cfg
}

// resolveClaimedSandbox requires an exact local claim in this project scope and live ownership labels.
func (b *backend) resolveClaimedSandbox(ctx context.Context, client shared.EnvdSandboxAPI, id string) (core.LeaseClaim, shared.EnvdSandbox, error) {
	if id == "" {
		return core.LeaseClaim{}, shared.EnvdSandbox{}, core.Exit(2, "provider=neevcloud requires a Crabbox lease id, slug, or NeevCloud sandbox id")
	}
	scope := claimScope(b.cfg)
	claim, ok, exact, err := core.ResolveLeaseClaimForProviderScopeWithExact(id, providerName, scope)
	if err != nil {
		return core.LeaseClaim{}, shared.EnvdSandbox{}, err
	}
	if exact && !ok {
		if claim.Provider != providerName {
			return core.LeaseClaim{}, shared.EnvdSandbox{}, core.Exit(4, "neevcloud identifier %q is claimed by a different provider", id)
		}
		return core.LeaseClaim{}, shared.EnvdSandbox{}, core.Exit(4, "neevcloud identifier %q is claimed for a different endpoint or project", id)
	}
	if !ok {
		if strings.HasPrefix(id, "cbx_") {
			return core.LeaseClaim{}, shared.EnvdSandbox{}, core.Exit(4, "neevcloud lease %q has no exact local claim", id)
		}
		claim, ok, err = core.ResolveLeaseClaimForProviderCloudIDScope(strings.TrimPrefix(id, leasePrefix), providerName, scope)
		if err != nil {
			return core.LeaseClaim{}, shared.EnvdSandbox{}, err
		}
		if !ok {
			return core.LeaseClaim{}, shared.EnvdSandbox{}, core.Exit(4, "neevcloud sandbox %q has no exact local claim", id)
		}
	}
	if claim.ProviderScope != scope || strings.TrimSpace(claim.CloudID) == "" {
		return core.LeaseClaim{}, shared.EnvdSandbox{}, core.Exit(4, "neevcloud lease %q is not bound to a sandbox in this project", claim.LeaseID)
	}
	sandbox, err := client.GetSandbox(ctx, claim.CloudID)
	if err != nil {
		if isNotFoundError(err) {
			return core.LeaseClaim{}, shared.EnvdSandbox{}, &claimedSandboxMissingError{claim: claim}
		}
		return core.LeaseClaim{}, shared.EnvdSandbox{}, operationError("get sandbox", err)
	}
	if err := validateClaim(b.cfg, claim, sandbox); err != nil {
		return core.LeaseClaim{}, shared.EnvdSandbox{}, err
	}
	return claim, sandbox, nil
}

// deleteClaimedSandbox deletes the sandbox fenced on the admitted claim snapshot, then removes the claim;
// a claim changed since admission refuses deletion.
func (b *backend) deleteClaimedSandbox(ctx context.Context, client shared.EnvdSandboxAPI, claim core.LeaseClaim) error {
	if claim.ProviderScope != claimScope(b.cfg) || strings.TrimSpace(claim.CloudID) == "" {
		return core.Exit(4, "neevcloud lease %q is not bound to a sandbox in this project; refusing deletion", claim.LeaseID)
	}
	return shared.DeleteClaimedEnvdSandbox(ctx, client, claim.LeaseID, claim.CloudID, claim,
		func(sandbox shared.EnvdSandbox) error { return validateClaim(b.cfg, claim, sandbox) },
		isNotFoundError, operationError)
}

type claimedSandboxMissingError struct {
	claim core.LeaseClaim
}

func (e *claimedSandboxMissingError) Error() string {
	return fmt.Sprintf("neevcloud sandbox %q for lease %q no longer exists", e.claim.CloudID, e.claim.LeaseID)
}

// validateClaim checks the live sandbox still carries this claim's ownership labels.
func validateClaim(cfg core.Config, claim core.LeaseClaim, sandbox shared.EnvdSandbox) error {
	if claim.Provider != providerName || claim.ProviderScope != claimScope(cfg) {
		return core.Exit(4, "neevcloud lease %q belongs to a different provider, endpoint or project", claim.LeaseID)
	}
	if claim.CloudID == "" || claim.CloudID != sandbox.SandboxID {
		return core.Exit(4, "neevcloud lease %q is not bound to sandbox %q", claim.LeaseID, sandbox.SandboxID)
	}
	if !isCrabboxSandbox(sandbox) || strings.TrimSpace(sandbox.Metadata["lease"]) != claim.LeaseID || strings.TrimSpace(sandbox.Metadata["slug"]) != claim.Slug {
		return core.Exit(4, "neevcloud sandbox %q no longer has canonical ownership labels for lease %q", sandbox.SandboxID, claim.LeaseID)
	}
	return nil
}

// resolveSandboxID maps a claim, lease ID, synthetic ID or raw sandbox ID to an owned sandbox for
// read-only status; runs and deletions go through resolveClaimedSandbox instead.
func (b *backend) resolveSandboxID(ctx context.Context, client shared.EnvdSandboxAPI, id string) (string, string, string, error) {
	if id == "" {
		return "", "", "", core.Exit(2, "provider=neevcloud requires a Crabbox lease id, slug, or NeevCloud sandbox id")
	}
	if claim, ok, err := core.ResolveLeaseClaim(id); err != nil {
		return "", "", "", err
	} else if ok && claim.Provider == providerName && claim.CloudID != "" {
		if claim.ProviderScope != claimScope(b.cfg) {
			return "", "", "", core.Exit(4, "neevcloud lease %q belongs to a different endpoint or project", claim.LeaseID)
		}
		sandbox, err := client.GetSandbox(ctx, claim.CloudID)
		if err != nil {
			return "", "", "", operationError("get sandbox", err)
		}
		if err := validateClaim(b.cfg, claim, sandbox); err != nil {
			return "", "", "", err
		}
		return claim.LeaseID, sandbox.SandboxID, claim.Slug, nil
	}
	if strings.HasPrefix(id, "cbx_") {
		sandboxes, err := client.ListSandboxes(ctx, map[string]string{"lease": id, "provider": providerName})
		if err != nil {
			return "", "", "", operationError("list sandboxes", err)
		}
		for _, sandbox := range sandboxes {
			if isCrabboxSandbox(sandbox) {
				return id, sandbox.SandboxID, shared.EnvdSandboxSlug(id, sandbox), nil
			}
		}
		return "", "", "", core.Exit(4, "neevcloud lease %q was not found", id)
	}
	sandbox, err := client.GetSandbox(ctx, strings.TrimPrefix(id, leasePrefix))
	if err == nil && isCrabboxSandbox(sandbox) {
		leaseID := sandboxViews.LeaseID(sandbox)
		return leaseID, sandbox.SandboxID, shared.EnvdSandboxSlug(leaseID, sandbox), nil
	}
	if err != nil && !isNotFoundError(err) {
		return "", "", "", operationError("get sandbox", err)
	}
	return "", "", "", core.Exit(4, "neevcloud sandbox or claim %q was not found", id)
}

func isCrabboxSandbox(sandbox shared.EnvdSandbox) bool {
	return sandbox.Metadata["provider"] == providerName && sandbox.Metadata["crabbox"] == "true"
}

func isNotFoundError(err error) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && apiErr.StatusCode == 404
}

func operationError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("neevcloud %s: %w", action, err)
}
