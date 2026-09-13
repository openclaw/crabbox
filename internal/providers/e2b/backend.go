package e2b

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const e2bCleanupTimeout = 30 * time.Second

var sandboxViews = shared.EnvdSandboxViews{Provider: e2bProvider, LeasePrefix: "e2b_"}

func RegisterE2BProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterE2BConfigFlags(fs, defaults.E2B)
}

func ApplyE2BProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == e2bProvider {
		if err := shared.RejectExplicitMachineSizingFlags(fs, e2bProvider, "", ""); err != nil {
			return err
		}
	}
	v, ok := values.(core.E2BConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := v.Apply(&cfg.E2B, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, e2bProvider)
	return err
}

func NewE2BBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	cfg.Provider = e2bProvider
	return &e2bBackend{spec: spec, cfg: cfg, rt: rt}
}

type e2bBackend struct {
	spec core.ProviderSpec
	cfg  core.Config
	rt   core.Runtime
}

const e2bMaxSandboxTimeout = time.Hour

func (b *e2bBackend) Spec() core.ProviderSpec { return b.spec }

func (b *e2bBackend) Warmup(ctx context.Context, req core.WarmupRequest) error {
	if err := validateE2BUser(b.cfg.E2B.User); err != nil {
		return err
	}
	started := core.ClockNow(b.rt.Clock)
	client, err := newE2BClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, sandbox, slug, err := b.createSandbox(ctx, client, req.Repo, req.Keep, req.Reclaim, req.RequestedSlug)
	if err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=e2b sandbox=%s\n", leaseID, slug, sandbox.SandboxID)
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: e2b warmup keeps the sandbox until explicit stop\n")
	}
	total := core.ClockNow(b.rt.Clock).Sub(started)
	return shared.CompleteWarmup(b.rt, req.TimingJSON, shared.WarmupCompletion{
		Provider: e2bProvider,
		LeaseID:  leaseID,
		Slug:     slug,
		Total:    total,
	})
}

func (b *e2bBackend) Run(ctx context.Context, req core.RunRequest) (core.RunResult, error) {
	var processUser string
	workspace := e2bWorkspacePath(b.cfg)
	var client shared.EnvdSandboxAPI
	var session shared.EnvdSandboxSession
	var leaseID, sandboxID, slug string
	handle := func() shared.DelegatedSandbox {
		return shared.DelegatedSandbox{LeaseID: leaseID, Slug: slug, CleanupCommand: e2bCleanupCommand(leaseID)}
	}
	return shared.RunDelegatedSandbox(ctx, req, shared.DelegatedSandboxLifecycle{
		Provider: e2bProvider, Runtime: b.rt, Workdir: workspace,
		IdleTimeout: b.cfg.IdleTimeout, TTL: b.cfg.TTL, CleanupTimeout: e2bCleanupTimeout,
		Preflight: func(context.Context) error {
			if err := rejectE2BSyncOptions(req); err != nil {
				return err
			}
			var err error
			processUser, err = e2bProcessUser(b.cfg.E2B.User)
			if err != nil {
				return err
			}
			client, err = newE2BClient(b.cfg, b.rt)
			return err
		},
		PrepareArchive: func(ctx context.Context) (*core.PreparedArchive, error) {
			return core.PrepareDelegatedArchive(ctx, core.DelegatedArchivePreparationRequest{
				Config: b.cfg, Repo: req.Repo, ForceSyncLarge: req.ForceSyncLarge,
				TempPattern: "crabbox-e2b-sync-*.tgz", Stderr: b.rt.Stderr, Now: func() time.Time { return core.ClockNow(b.rt.Clock) },
			})
		},
		Acquire: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			var sandbox shared.EnvdSandbox
			var err error
			leaseID, sandbox, slug, err = b.createSandbox(ctx, client, req.Repo, req.Keep, req.Reclaim, req.RequestedSlug)
			sandboxID = sandbox.SandboxID
			if err == nil {
				fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=e2b sandbox=%s\n", leaseID, slug, sandboxID)
			}
			return handle(), err
		},
		Resolve: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			var err error
			leaseID, sandboxID, slug, err = b.resolveSandboxID(ctx, client, req.ID, req.Repo.Root, req.Reclaim)
			return handle(), err
		},
		Setup: func(ctx context.Context) error {
			var err error
			session, err = client.ConnectSandbox(ctx, sandboxID, e2bTimeoutSeconds(b.cfg.TTL))
			return e2bError("connect sandbox", err)
		},
		Sync: func(ctx context.Context, prepared *core.PreparedArchive) ([]core.TimingPhase, time.Duration, error) {
			return b.syncWorkspace(ctx, client, session, req, workspace, prepared)
		},
		NoSync: func(ctx context.Context) error {
			return b.prepareWorkspace(ctx, client, session, workspace)
		},
		Command: func(context.Context) (shared.DelegatedSandboxCommand, error) {
			intent, err := core.ParseCommandIntent(req.Command, req.ShellMode, req.CommandLiteralArgs)
			if err != nil {
				return shared.DelegatedSandboxCommand{}, core.Exit(2, "%v", err)
			}
			command := intent.ShellSource()
			return shared.DelegatedSandboxCommand{Run: func(ctx context.Context, stdout, stderr io.Writer) (int, error) {
				fmt.Fprintf(b.rt.Stderr, "running on e2b %s\n", strings.Join(req.Command, " "))
				return client.StartProcess(ctx, session, shared.EnvdSandboxProcessRequest{
					Command: command, CWD: workspace, Env: req.Env, User: processUser,
					Timeout: e2bTimeoutDuration(b.cfg.TTL), Stdout: stdout, Stderr: stderr,
				})
			}}, nil
		},
		Cleanup: func(ctx context.Context) error {
			return b.deleteClaimedSandbox(ctx, client, leaseID, sandboxID)
		},
	})
}

func (b *e2bBackend) List(ctx context.Context, req core.ListRequest) ([]core.LeaseView, error) {
	_ = req
	client, err := newE2BClient(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	sandboxes, err := client.ListSandboxes(ctx, map[string]string{"crabbox": "true", "provider": e2bProvider})
	if err != nil {
		return nil, e2bError("list sandboxes", err)
	}
	servers := make([]core.Server, 0, len(sandboxes))
	for _, sandbox := range sandboxes {
		servers = append(servers, sandboxViews.Server(sandbox))
	}
	return servers, nil
}

func (b *e2bBackend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	servers, err := b.List(ctx, core.ListRequest{})
	if err != nil {
		return core.DoctorResult{}, err
	}
	return core.InventoryDoctorResult(e2bProvider, len(servers)), nil
}

func (b *e2bBackend) Status(ctx context.Context, req core.StatusRequest) (core.StatusView, error) {
	client, err := newE2BClient(b.cfg, b.rt)
	if err != nil {
		return core.StatusView{}, err
	}
	wait := shared.NewStatusWait(ctx, req, b.rt.Clock, func(id string) error {
		return core.Exit(5, "timed out waiting for sandbox %s to become ready", id)
	})
	defer wait.Close()
	leaseID, sandboxID, _, err := b.resolveSandboxID(wait.Context(), client, req.ID, "", false)
	if err != nil {
		if ctxErr := wait.ContextError(req.ID); ctxErr != nil {
			return core.StatusView{}, ctxErr
		}
		return core.StatusView{}, err
	}
	for {
		sandbox, err := client.GetSandbox(wait.Context(), sandboxID)
		if err != nil {
			if ctxErr := wait.ContextError(sandboxID); ctxErr != nil {
				return core.StatusView{}, ctxErr
			}
			return core.StatusView{}, e2bError("get sandbox", err)
		}
		view := sandboxViews.Status(leaseID, sandbox)
		if !req.Wait || view.Ready {
			return view, nil
		}
		if err := wait.Next(sandboxID, 2*time.Second); err != nil {
			return core.StatusView{}, err
		}
	}
}

func (b *e2bBackend) Stop(ctx context.Context, req core.StopRequest) error {
	client, err := newE2BClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	claim, sandbox, err := b.resolveStopTarget(ctx, client, req.ID)
	if err != nil {
		var missing *e2bClaimedSandboxMissingError
		if errors.As(err, &missing) {
			if removeErr := core.RemoveLeaseClaimIfUnchangedAfter(missing.claim.LeaseID, missing.claim, nil); removeErr != nil {
				return removeErr
			}
			fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s (already absent)\n", missing.claim.LeaseID, missing.claim.CloudID)
			return nil
		}
		return err
	}
	if err := b.deleteClaimedSandbox(ctx, client, claim.LeaseID, sandbox.SandboxID); err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s\n", claim.LeaseID, sandbox.SandboxID)
	return nil
}

func (b *e2bBackend) ReclaimAndStop(ctx context.Context, req core.StopRequest) error {
	if req.ID == "" {
		return core.Exit(2, "provider=e2b stop --reclaim requires an exact E2B sandbox id")
	}
	sandboxID := req.ID
	if isE2BSyntheticID(sandboxID) {
		sandboxID = strings.TrimPrefix(sandboxID, "e2b_")
	} else if strings.HasPrefix(sandboxID, "cbx_") {
		return core.Exit(2, "provider=e2b stop --reclaim requires an exact E2B sandbox id, not lease %q", req.ID)
	}
	client, err := newE2BClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	sandbox, err := client.GetSandbox(ctx, sandboxID)
	if err != nil {
		return e2bError("get sandbox", err)
	}
	if sandbox.SandboxID != sandboxID {
		return core.Exit(4, "e2b sandbox lookup for %q returned a different sandbox %q", sandboxID, sandbox.SandboxID)
	}
	if !isCrabboxE2BSandbox(sandbox) {
		return core.Exit(4, "e2b sandbox %q is not claimed by Crabbox", req.ID)
	}
	leaseID := strings.TrimSpace(sandbox.Metadata["lease"])
	if !core.IsCanonicalLeaseID(leaseID) {
		return core.Exit(4, "e2b sandbox %q lacks a canonical Crabbox lease id", req.ID)
	}
	slug := strings.TrimSpace(sandbox.Metadata["slug"])
	if slug == "" {
		return core.Exit(4, "e2b sandbox %q lacks a canonical Crabbox slug", req.ID)
	}
	cfg := e2bClaimConfig(b.cfg)
	if existing, ok, err := resolveLeaseClaimForProviderCloudIDScope(sandbox.SandboxID, providerClaimScope(cfg)); err != nil {
		return err
	} else if ok && existing.LeaseID != leaseID {
		return core.Exit(4, "e2b sandbox %q is already bound to lease %q", sandbox.SandboxID, existing.LeaseID)
	}
	previous, previousExists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		return err
	}
	if err := validateE2BReclaimCollision(leaseID, sandbox.SandboxID, previous, previousExists); err != nil {
		return err
	}
	var claim core.LeaseClaim
	if previousExists && previous.RepoRoot != "" {
		claim, err = claimLeaseTargetForRepoConfigIfUnchanged(
			leaseID,
			slug,
			cfg,
			sandboxViews.Server(sandbox), core.SSHTarget{}, previous.RepoRoot,
			cfg.IdleTimeout,
			true,
			previous,
			true,
		)
	} else {
		claim, err = claimLeaseTargetForConfigIfUnchanged(
			leaseID,
			slug,
			cfg,
			sandboxViews.Server(sandbox), core.SSHTarget{}, cfg.IdleTimeout,
			previous,
			previousExists,
		)
	}
	if err != nil {
		return err
	}
	if err := validateE2BClaim(cfg, claim, sandbox); err != nil {
		return err
	}
	if err := b.deleteClaimedSandbox(ctx, client, leaseID, sandbox.SandboxID); err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s\n", leaseID, sandbox.SandboxID)
	return nil
}

func (b *e2bBackend) createSandbox(ctx context.Context, client shared.EnvdSandboxAPI, repo core.Repo, keep, reclaim bool, requestedSlug string) (string, shared.EnvdSandbox, string, error) {
	leaseID := core.NewLeaseID()
	slug, err := core.AllocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return "", shared.EnvdSandbox{}, "", err
	}
	template := core.Blank(b.cfg.E2B.Template, core.E2BConfigDefaultTemplate)
	cfg := b.cfg
	workspace, err := cleanE2BWorkspacePath(e2bWorkspacePath(cfg))
	if err != nil {
		return "", shared.EnvdSandbox{}, "", err
	}
	cfg.TTL = e2bTimeoutDuration(cfg.TTL)
	cfg.ServerType = template
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, e2bProvider, "", keep, core.ClockNow(b.rt.Clock).UTC())
	labels["state"] = "ready"
	labels["workdir"] = workspace
	labels["template"] = template
	if repo.Name != "" {
		labels["repo"] = repo.Name
	}
	timeoutSeconds := e2bTimeoutSeconds(cfg.TTL)
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=e2b lease=%s slug=%s template=%s timeout=%ds\n", leaseID, slug, template, timeoutSeconds)
	sandbox, err := client.CreateSandbox(ctx, shared.EnvdSandboxCreateRequest{
		TemplateID:          template,
		TimeoutSeconds:      timeoutSeconds,
		Metadata:            labels,
		AllowInternetAccess: true,
	})
	if err != nil {
		return "", shared.EnvdSandbox{}, "", e2bError("create sandbox", err)
	}
	if sandbox.SandboxID == "" {
		return "", shared.EnvdSandbox{}, "", core.Exit(5, "e2b create sandbox returned no sandbox id")
	}
	cfg = e2bClaimConfig(cfg)
	if err := claimLeaseTargetForRepoConfig(leaseID, slug, cfg, sandboxViews.Server(sandbox), core.SSHTarget{}, repo.Root, cfg.IdleTimeout, reclaim); err != nil {
		if cleanupErr := b.deleteSandboxForCleanup(client, sandbox.SandboxID); cleanupErr != nil {
			leakErr := fmt.Errorf("cleanup e2b sandbox %s after claim failure: %w; run `crabbox stop --provider e2b --id %s --reclaim` to retry cleanup", sandbox.SandboxID, cleanupErr, sandbox.SandboxID)
			fmt.Fprintf(b.rt.Stderr, "warning: %v\n", leakErr)
			return "", shared.EnvdSandbox{}, "", errors.Join(err, leakErr)
		}
		return "", shared.EnvdSandbox{}, "", err
	}
	return leaseID, sandbox, slug, nil
}

func (b *e2bBackend) deleteSandboxForCleanup(client shared.EnvdSandboxAPI, sandboxID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), e2bCleanupTimeout)
	defer cancel()
	return client.DeleteSandbox(ctx, sandboxID)
}

func e2bCleanupCommand(leaseID string) string {
	return fmt.Sprintf("crabbox stop --provider %s --id %s", e2bProvider, core.ShellQuote(leaseID))
}

func (b *e2bBackend) resolveStopTarget(ctx context.Context, client shared.EnvdSandboxAPI, id string) (core.LeaseClaim, shared.EnvdSandbox, error) {
	if id == "" {
		return core.LeaseClaim{}, shared.EnvdSandbox{}, core.Exit(2, "provider=e2b requires a Crabbox lease id, slug, or E2B sandbox id")
	}
	cfg := e2bClaimConfig(b.cfg)
	claim, ok, exact, err := resolveLeaseClaimForProviderScopeWithExact(id, providerClaimScope(cfg))
	if err != nil {
		return core.LeaseClaim{}, shared.EnvdSandbox{}, err
	}
	if exact && !ok {
		if claim.Provider != e2bProvider {
			return core.LeaseClaim{}, shared.EnvdSandbox{}, core.Exit(4, "e2b identifier %q is claimed by a different provider", id)
		}
		return core.LeaseClaim{}, shared.EnvdSandbox{}, core.Exit(4, "e2b identifier %q is claimed for a different API endpoint", id)
	}
	if !ok {
		if strings.HasPrefix(id, "cbx_") {
			return core.LeaseClaim{}, shared.EnvdSandbox{}, core.Exit(4, "e2b lease %q has no exact local claim", id)
		}
		sandboxID := id
		if isE2BSyntheticID(id) {
			sandboxID = strings.TrimPrefix(id, "e2b_")
		}
		claim, ok, err = resolveLeaseClaimForProviderCloudIDScope(sandboxID, providerClaimScope(cfg))
		if err != nil {
			return core.LeaseClaim{}, shared.EnvdSandbox{}, err
		}
		if !ok {
			return core.LeaseClaim{}, shared.EnvdSandbox{}, core.Exit(4, "e2b sandbox %q has no exact local claim; use --reclaim to adopt it explicitly", id)
		}
	}
	if claim.ProviderScope != providerClaimScope(cfg) {
		return core.LeaseClaim{}, shared.EnvdSandbox{}, core.Exit(4, "e2b lease %q belongs to a different API endpoint; use --reclaim with the exact sandbox id to adopt it", claim.LeaseID)
	}
	if strings.TrimSpace(claim.CloudID) == "" {
		return core.LeaseClaim{}, shared.EnvdSandbox{}, core.Exit(4, "e2b lease %q has a legacy claim not bound to an exact sandbox; use --reclaim with the exact sandbox id to adopt it", claim.LeaseID)
	}
	sandbox, err := client.GetSandbox(ctx, claim.CloudID)
	if err != nil {
		if isNotFoundError(err) {
			return core.LeaseClaim{}, shared.EnvdSandbox{}, &e2bClaimedSandboxMissingError{claim: claim}
		}
		return core.LeaseClaim{}, shared.EnvdSandbox{}, e2bError("get sandbox", err)
	}
	if err := validateE2BClaim(cfg, claim, sandbox); err != nil {
		return core.LeaseClaim{}, shared.EnvdSandbox{}, err
	}
	return claim, sandbox, nil
}

func (b *e2bBackend) deleteClaimedSandbox(ctx context.Context, client shared.EnvdSandboxAPI, leaseID, sandboxID string) error {
	cfg := e2bClaimConfig(b.cfg)
	claim, ok, exact, err := resolveLeaseClaimForProviderScopeWithExact(leaseID, providerClaimScope(cfg))
	if err != nil {
		return err
	}
	if !ok || !exact {
		return core.Exit(4, "e2b lease %q has no exact local claim; refusing deletion", leaseID)
	}
	if claim.ProviderScope != providerClaimScope(cfg) || claim.CloudID != sandboxID {
		return core.Exit(4, "e2b lease %q is not bound to sandbox %q on this API endpoint; refusing deletion", leaseID, sandboxID)
	}
	return core.RemoveLeaseClaimIfUnchangedAfter(leaseID, claim, func() error {
		sandbox, err := client.GetSandbox(ctx, sandboxID)
		if err != nil {
			if isNotFoundError(err) {
				return nil
			}
			return e2bError("get sandbox before delete", err)
		}
		if err := validateE2BClaim(cfg, claim, sandbox); err != nil {
			return err
		}
		if err := client.DeleteSandbox(ctx, sandboxID); err != nil {
			if isNotFoundError(err) {
				return nil
			}
			return e2bError("delete sandbox", err)
		}
		return nil
	})
}

func validateE2BReclaimCollision(leaseID, sandboxID string, previous core.LeaseClaim, previousExists bool) error {
	if !previousExists {
		return nil
	}
	if previous.Provider != e2bProvider {
		return core.Exit(4, "e2b lease %q is already claimed by provider %q; refusing reclaim", leaseID, previous.Provider)
	}
	if previous.CloudID != "" && previous.CloudID != sandboxID {
		return core.Exit(4, "e2b lease %q is already bound to sandbox %q; refusing retarget to %q", leaseID, previous.CloudID, sandboxID)
	}
	return nil
}

type e2bClaimedSandboxMissingError struct {
	claim core.LeaseClaim
}

func (e *e2bClaimedSandboxMissingError) Error() string {
	return fmt.Sprintf("e2b sandbox %q for lease %q no longer exists", e.claim.CloudID, e.claim.LeaseID)
}

func validateE2BClaim(cfg core.Config, claim core.LeaseClaim, sandbox shared.EnvdSandbox) error {
	if claim.Provider != e2bProvider || claim.ProviderScope != providerClaimScope(e2bClaimConfig(cfg)) {
		return core.Exit(4, "e2b lease %q belongs to a different provider or API endpoint", claim.LeaseID)
	}
	if claim.CloudID == "" || claim.CloudID != sandbox.SandboxID {
		return core.Exit(4, "e2b lease %q is not bound to sandbox %q", claim.LeaseID, sandbox.SandboxID)
	}
	if !isCrabboxE2BSandbox(sandbox) || strings.TrimSpace(sandbox.Metadata["lease"]) != claim.LeaseID || strings.TrimSpace(sandbox.Metadata["slug"]) != claim.Slug {
		return core.Exit(4, "e2b sandbox %q no longer has canonical ownership metadata for lease %q", sandbox.SandboxID, claim.LeaseID)
	}
	return nil
}

func e2bClaimConfig(cfg core.Config) core.Config {
	cfg.Provider = e2bProvider
	if strings.TrimSpace(cfg.E2B.APIURL) == "" {
		cfg.E2B.APIURL = core.E2BConfigDefaultAPIURL
	}
	return cfg
}

func (b *e2bBackend) resolveSandboxID(ctx context.Context, client shared.EnvdSandboxAPI, id, repoRoot string, reclaim bool) (string, string, string, error) {
	if id == "" {
		return "", "", "", core.Exit(2, "provider=e2b requires a Crabbox lease id, slug, or E2B sandbox id")
	}
	if claim, ok, err := core.ResolveLeaseClaim(id); err != nil {
		return "", "", "", err
	} else if ok && claim.Provider == e2bProvider {
		if claim.CloudID != "" {
			cfg := e2bClaimConfig(b.cfg)
			if claim.ProviderScope != providerClaimScope(cfg) {
				return "", "", "", core.Exit(4, "e2b lease %q belongs to a different API endpoint", claim.LeaseID)
			}
			sandbox, err := client.GetSandbox(ctx, claim.CloudID)
			if err != nil {
				return "", "", "", e2bError("get sandbox", err)
			}
			if err := validateE2BClaim(cfg, claim, sandbox); err != nil {
				return "", "", "", err
			}
			if repoRoot != "" {
				claim, err = claimLeaseTargetForRepoConfigIfUnchanged(
					claim.LeaseID,
					claim.Slug,
					cfg,
					sandboxViews.Server(sandbox), core.SSHTarget{}, repoRoot,
					time.Duration(claim.IdleTimeoutSeconds)*time.Second,
					reclaim,
					claim,
					true,
				)
				if err != nil {
					return "", "", "", err
				}
			}
			return claim.LeaseID, sandbox.SandboxID, claim.Slug, nil
		}
		if repoRoot != "" {
			if err := claimLeaseForRepoProvider(claim.LeaseID, claim.Slug, e2bProvider, repoRoot, time.Duration(claim.IdleTimeoutSeconds)*time.Second, reclaim); err != nil {
				return "", "", "", err
			}
		}
		sandbox, err := resolveE2BSandboxByLease(ctx, client, claim.LeaseID)
		if err != nil {
			return "", "", "", err
		}
		return claim.LeaseID, sandbox.SandboxID, claim.Slug, nil
	}
	if isE2BSyntheticID(id) {
		sandboxID := strings.TrimPrefix(id, "e2b_")
		sandbox, err := client.GetSandbox(ctx, sandboxID)
		if err != nil {
			return "", "", "", e2bError("get sandbox", err)
		}
		if !isCrabboxE2BSandbox(sandbox) {
			return "", "", "", core.Exit(4, "e2b sandbox %q is not claimed by Crabbox", id)
		}
		leaseID := sandboxViews.LeaseID(sandbox)
		return leaseID, sandbox.SandboxID, shared.EnvdSandboxSlug(leaseID, sandbox), nil
	}
	if strings.HasPrefix(id, "cbx_") {
		sandbox, err := resolveE2BSandboxByLease(ctx, client, id)
		if err != nil {
			return "", "", "", err
		}
		return id, sandbox.SandboxID, shared.EnvdSandboxSlug(id, sandbox), nil
	}
	sandbox, err := client.GetSandbox(ctx, id)
	if err == nil && isCrabboxE2BSandbox(sandbox) {
		leaseID := sandboxViews.LeaseID(sandbox)
		return leaseID, sandbox.SandboxID, shared.EnvdSandboxSlug(leaseID, sandbox), nil
	}
	if err != nil && !isNotFoundError(err) {
		return "", "", "", e2bError("get sandbox", err)
	}
	return "", "", "", core.Exit(4, "e2b sandbox or claim %q was not found", id)
}

func resolveE2BSandboxByLease(ctx context.Context, client shared.EnvdSandboxAPI, leaseID string) (shared.EnvdSandbox, error) {
	sandboxes, err := client.ListSandboxes(ctx, map[string]string{"lease": leaseID, "provider": e2bProvider})
	if err != nil {
		return shared.EnvdSandbox{}, e2bError("list sandboxes", err)
	}
	for _, sandbox := range sandboxes {
		if isCrabboxE2BSandbox(sandbox) {
			return sandbox, nil
		}
	}
	return shared.EnvdSandbox{}, core.Exit(4, "e2b lease %q was not found", leaseID)
}

func isE2BSyntheticID(id string) bool {
	return strings.HasPrefix(id, "e2b_") && len(id) > len("e2b_")
}

func isCrabboxE2BSandbox(sandbox shared.EnvdSandbox) bool {
	return sandbox.Metadata["provider"] == e2bProvider && sandbox.Metadata["crabbox"] == "true"
}

func e2bTimeoutDuration(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return 5 * time.Minute
	}
	if ttl > e2bMaxSandboxTimeout {
		return e2bMaxSandboxTimeout
	}
	return ttl
}

func e2bTimeoutSeconds(ttl time.Duration) int {
	return durationSecondsCeil(e2bTimeoutDuration(ttl))
}

func e2bWorkspacePath(cfg core.Config) string {
	workdir := strings.TrimSpace(cfg.E2B.Workdir)
	if workdir == "" {
		workdir = core.E2BConfigDefaultWorkdir
	}
	if strings.HasPrefix(workdir, "/") {
		return path.Clean(workdir)
	}
	return path.Join(e2bUserHome(cfg.E2B.User), workdir)
}

func e2bUserHome(user string) string {
	user = e2bWorkspaceUser(user)
	if user == "" {
		user = "user"
	}
	if user == "root" {
		return "/root"
	}
	return path.Join("/home", user)
}

func e2bWorkspaceUser(user string) string {
	clean, err := e2bProcessUser(user)
	if err != nil || clean == "" {
		return "user"
	}
	return clean
}

func validateE2BUser(user string) error {
	_, err := e2bProcessUser(user)
	return err
}

func e2bProcessUser(user string) (string, error) {
	clean := strings.TrimSpace(user)
	if clean == "" {
		return "", nil
	}
	if clean == "." || clean == ".." || strings.ContainsAny(clean, `/\`) || strings.ContainsRune(clean, 0) {
		return "", core.Exit(2, "invalid e2b.user %q: use a login name, not a path", user)
	}
	return clean, nil
}

func rejectE2BSyncOptions(req core.RunRequest) error {
	if req.ChecksumSync {
		return core.Exit(2, "%s uses E2B archive sync; --checksum is not supported", e2bProvider)
	}
	return nil
}

func durationSecondsCeil(duration time.Duration) int {
	if duration <= 0 {
		return 0
	}
	return int((duration + time.Second - 1) / time.Second)
}

func isNotFoundError(err error) bool {
	var apiErr *e2bAPIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == 404
}

func e2bError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("e2b %s: %w", action, err)
}
