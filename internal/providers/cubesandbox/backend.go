package cubesandbox

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type cubesandboxFlagValues struct {
	APIURL        *string
	Domain        *string
	Template      *string
	Workdir       *string
	User          *string
	ProxyNodeIP   *string
	ProxyPortHTTP *int
	ProxyScheme   *string
}

const cubesandboxCleanupTimeout = 30 * time.Second

var sandboxViews = shared.EnvdSandboxViews{Provider: providerName, LeasePrefix: "cubesandbox_"}

func RegisterCubeSandboxProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return cubesandboxFlagValues{
		APIURL:        fs.String("cubesandbox-api-url", defaults.CubeSandbox.APIURL, "CubeSandbox API URL"),
		Domain:        fs.String("cubesandbox-domain", defaults.CubeSandbox.Domain, "CubeSandbox sandbox domain"),
		Template:      fs.String("cubesandbox-template", defaults.CubeSandbox.Template, "CubeSandbox sandbox template ID"),
		Workdir:       fs.String("cubesandbox-workdir", defaults.CubeSandbox.Workdir, "CubeSandbox sandbox working directory"),
		User:          fs.String("cubesandbox-user", defaults.CubeSandbox.User, "CubeSandbox sandbox user for command and file ownership"),
		ProxyNodeIP:   fs.String("cubesandbox-proxy-node-ip", defaults.CubeSandbox.ProxyNodeIP, "CubeSandbox CubeProxy node IP or host for data-plane requests"),
		ProxyPortHTTP: fs.Int("cubesandbox-proxy-port-http", defaults.CubeSandbox.ProxyPortHTTP, "CubeSandbox CubeProxy HTTP/HTTPS port"),
		ProxyScheme:   fs.String("cubesandbox-proxy-scheme", defaults.CubeSandbox.ProxyScheme, "CubeSandbox CubeProxy scheme (http or https)"),
	}
}

func ApplyCubeSandboxProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == providerName {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "", ""); err != nil {
			return err
		}
	}
	v, ok := values.(cubesandboxFlagValues)
	if !ok {
		return nil
	}
	if core.FlagWasSet(fs, "cubesandbox-api-url") {
		cfg.CubeSandbox.APIURL = *v.APIURL
		core.RecordProviderFlagInputs(cfg, true, "cubesandbox")
	}
	if core.FlagWasSet(fs, "cubesandbox-domain") {
		cfg.CubeSandbox.Domain = *v.Domain
		core.RecordProviderFlagInputs(cfg, true, "cubesandbox")
	}
	if core.FlagWasSet(fs, "cubesandbox-template") {
		cfg.CubeSandbox.Template = *v.Template
		core.RecordProviderFlagInputs(cfg, true, "cubesandbox")
	}
	if core.FlagWasSet(fs, "cubesandbox-workdir") {
		cfg.CubeSandbox.Workdir = *v.Workdir
		core.RecordProviderFlagInputs(cfg, true, "cubesandbox")
	}
	if core.FlagWasSet(fs, "cubesandbox-user") {
		cfg.CubeSandbox.User = *v.User
		core.RecordProviderFlagInputs(cfg, true, "cubesandbox")
	}
	if core.FlagWasSet(fs, "cubesandbox-proxy-node-ip") {
		cfg.CubeSandbox.ProxyNodeIP = *v.ProxyNodeIP
		core.RecordProviderFlagInputs(cfg, true, "cubesandbox")
	}
	if core.FlagWasSet(fs, "cubesandbox-proxy-port-http") {
		cfg.CubeSandbox.ProxyPortHTTP = *v.ProxyPortHTTP
		core.RecordProviderFlagInputs(cfg, true, "cubesandbox")
	}
	if core.FlagWasSet(fs, "cubesandbox-proxy-scheme") {
		cfg.CubeSandbox.ProxyScheme = *v.ProxyScheme
		core.RecordProviderFlagInputs(cfg, true, "cubesandbox")
	}
	return nil
}

func NewCubeSandboxBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	cfg.Provider = providerName
	return &cubesandboxBackend{spec: spec, cfg: cfg, rt: rt}
}

type cubesandboxBackend struct {
	spec core.ProviderSpec
	cfg  core.Config
	rt   core.Runtime
}

func (b *cubesandboxBackend) Spec() core.ProviderSpec { return b.spec }

func (b *cubesandboxBackend) Warmup(ctx context.Context, req core.WarmupRequest) error {
	if _, err := workspaceForConfig(b.cfg, b.rt).ProcessUser(); err != nil {
		return err
	}
	started := core.ClockNow(b.rt.Clock)
	client, err := newCubeSandboxClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, sandbox, slug, err := b.createSandbox(ctx, client, req.Repo, req.Keep, req.Reclaim, req.RequestedSlug)
	if err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=cubesandbox sandbox=%s\n", leaseID, slug, sandbox.SandboxID)
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: cubesandbox warmup keeps the sandbox until explicit stop\n")
	}
	total := core.ClockNow(b.rt.Clock).Sub(started)
	return shared.CompleteWarmup(b.rt, req.TimingJSON, shared.WarmupCompletion{
		Provider: providerName,
		LeaseID:  leaseID,
		Slug:     slug,
		Total:    total,
	})
}

func (b *cubesandboxBackend) Run(ctx context.Context, req core.RunRequest) (core.RunResult, error) {
	var client shared.EnvdSandboxAPI
	var processUser, leaseID, sandboxID, slug string
	var session shared.EnvdSandboxSession
	workspace := workspaceForConfig(b.cfg, b.rt).Path()
	boundSandbox := func() shared.DelegatedSandbox {
		return shared.DelegatedSandbox{LeaseID: leaseID, Slug: slug, CleanupCommand: cubesandboxCleanupCommand(leaseID)}
	}
	return shared.RunDelegatedSandbox(ctx, req, shared.DelegatedSandboxLifecycle{
		Provider: providerName, Runtime: b.rt, Workdir: workspace,
		IdleTimeout: b.cfg.IdleTimeout, TTL: b.cfg.TTL, CleanupTimeout: cubesandboxCleanupTimeout,
		Preflight: func(context.Context) error {
			if err := rejectCubeSandboxSyncOptions(req); err != nil {
				return err
			}
			var err error
			processUser, err = workspaceForConfig(b.cfg, b.rt).ProcessUser()
			if err != nil {
				return err
			}
			client, err = newCubeSandboxClient(b.cfg, b.rt)
			return err
		},
		PrepareArchive: func(ctx context.Context) (*core.PreparedArchive, error) {
			return core.PrepareDelegatedArchive(ctx, core.DelegatedArchivePreparationRequest{
				Config: b.cfg, Repo: req.Repo, ForceSyncLarge: req.ForceSyncLarge,
				TempPattern: "crabbox-cubesandbox-sync-*.tgz", Stderr: b.rt.Stderr, Now: func() time.Time { return core.ClockNow(b.rt.Clock) },
			})
		},
		Acquire: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			var sandbox shared.EnvdSandbox
			var err error
			leaseID, sandbox, slug, err = b.createSandbox(ctx, client, req.Repo, req.Keep, req.Reclaim, req.RequestedSlug)
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			sandboxID = sandbox.SandboxID
			fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=cubesandbox sandbox=%s\n", leaseID, slug, sandboxID)
			return boundSandbox(), nil
		},
		Resolve: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			var err error
			leaseID, sandboxID, slug, err = b.resolveSandboxID(ctx, client, req.ID, req.Repo.Root, req.Reclaim)
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			return boundSandbox(), nil
		},
		Setup: func(ctx context.Context) error {
			var err error
			session, err = client.ConnectSandbox(ctx, sandboxID, cubesandboxTimeoutSeconds(b.cfg.TTL))
			if err != nil {
				return cubesandboxError("connect sandbox", err)
			}
			return nil
		},
		Sync: func(ctx context.Context, prepared *core.PreparedArchive) ([]core.TimingPhase, time.Duration, error) {
			return workspaceForConfig(b.cfg, b.rt).Sync(ctx, client, session, req, workspace, prepared)
		},
		NoSync: func(ctx context.Context) error {
			return workspaceForConfig(b.cfg, b.rt).Prepare(ctx, client, session, workspace)
		},
		Command: func(context.Context) (shared.DelegatedSandboxCommand, error) {
			intent, err := core.ParseCommandIntent(req.Command, req.ShellMode, req.CommandLiteralArgs)
			if err != nil {
				return shared.DelegatedSandboxCommand{}, core.Exit(2, "%v", err)
			}
			command := intent.ShellSource()
			fmt.Fprintf(b.rt.Stderr, "running on cubesandbox %s\n", strings.Join(req.Command, " "))
			commandEnv, strippedAuthEnv := cubeSandboxCommandEnv(req.Env)
			if len(strippedAuthEnv) > 0 {
				fmt.Fprintf(b.rt.Stderr, "warning: provider=%s did not forward provider authentication variables: %s\n", providerName, strings.Join(strippedAuthEnv, ","))
			}
			return shared.DelegatedSandboxCommand{
				Text: strings.Join(req.Command, " "),
				Run: func(ctx context.Context, stdout, stderr io.Writer) (int, error) {
					return client.StartProcess(ctx, session, shared.EnvdSandboxProcessRequest{
						Command: command, CWD: workspace, Env: commandEnv, User: processUser,
						Timeout: cubesandboxTimeoutDuration(b.cfg.TTL), Stdout: stdout, Stderr: stderr,
					})
				},
			}, nil
		},
		Cleanup: func(ctx context.Context) error {
			return b.deleteClaimedSandbox(ctx, client, leaseID, sandboxID)
		},
	})
}

func cubeSandboxCommandEnv(env map[string]string) (map[string]string, []string) {
	if len(env) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(env))
	var stripped []string
	for name, value := range env {
		switch name {
		case "CRABBOX_CUBESANDBOX_API_KEY", "CUBE_API_KEY", "E2B_API_KEY":
			stripped = append(stripped, name)
		default:
			out[name] = value
		}
	}
	slices.Sort(stripped)
	return out, stripped
}

func (b *cubesandboxBackend) List(ctx context.Context, req core.ListRequest) ([]core.LeaseView, error) {
	_ = req
	client, err := newCubeSandboxClient(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	sandboxes, err := client.ListSandboxes(ctx, map[string]string{"crabbox": "true", "provider": providerName})
	if err != nil {
		return nil, cubesandboxError("list sandboxes", err)
	}
	servers := make([]core.Server, 0, len(sandboxes))
	for _, sandbox := range sandboxes {
		servers = append(servers, sandboxViews.Server(sandbox))
	}
	return servers, nil
}

func (b *cubesandboxBackend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	servers, err := b.List(ctx, core.ListRequest{})
	if err != nil {
		return core.DoctorResult{}, err
	}
	return core.InventoryDoctorResult(providerName, len(servers)), nil
}

func (b *cubesandboxBackend) Status(ctx context.Context, req core.StatusRequest) (core.StatusView, error) {
	client, err := newCubeSandboxClient(b.cfg, b.rt)
	if err != nil {
		return core.StatusView{}, err
	}
	leaseID, sandboxID, _, err := b.resolveSandboxID(ctx, client, req.ID, "", false)
	if err != nil {
		return core.StatusView{}, err
	}
	deadline := core.ClockNow(b.rt.Clock).Add(req.WaitTimeout)
	if req.WaitTimeout <= 0 {
		deadline = core.ClockNow(b.rt.Clock).Add(5 * time.Minute)
	}
	for {
		sandbox, err := client.GetSandbox(ctx, sandboxID)
		if err != nil {
			return core.StatusView{}, cubesandboxError("get sandbox", err)
		}
		view := sandboxViews.Status(leaseID, sandbox)
		if !req.Wait || view.Ready {
			return view, nil
		}
		if core.ClockNow(b.rt.Clock).After(deadline) {
			return core.StatusView{}, core.Exit(5, "timed out waiting for sandbox %s to become ready", sandboxID)
		}
		select {
		case <-ctx.Done():
			return core.StatusView{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *cubesandboxBackend) Stop(ctx context.Context, req core.StopRequest) error {
	client, err := newCubeSandboxClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, sandboxID, _, err := b.resolveSandboxID(ctx, client, req.ID, "", false)
	if err != nil {
		var missing *cubesandboxClaimedSandboxMissingError
		if errors.As(err, &missing) {
			if removeErr := core.RemoveLeaseClaimIfUnchangedAfter(missing.claim.LeaseID, missing.claim, nil); removeErr != nil {
				return removeErr
			}
			fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s (already absent)\n", missing.claim.LeaseID, missing.claim.CloudID)
			return nil
		}
		return err
	}
	if err := b.deleteClaimedSandbox(ctx, client, leaseID, sandboxID); err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s\n", leaseID, sandboxID)
	return nil
}

func (b *cubesandboxBackend) ReclaimAndStop(ctx context.Context, req core.StopRequest) error {
	if req.ID == "" {
		return core.Exit(2, "provider=cubesandbox stop --reclaim requires an exact CubeSandbox sandbox id")
	}
	sandboxID := req.ID
	if isCubeSandboxSyntheticID(sandboxID) {
		sandboxID = strings.TrimPrefix(sandboxID, "cubesandbox_")
	} else if strings.HasPrefix(sandboxID, "cbx_") {
		return core.Exit(2, "provider=cubesandbox stop --reclaim requires an exact CubeSandbox sandbox id, not lease %q", req.ID)
	}
	client, err := newCubeSandboxClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	sandbox, err := client.GetSandbox(ctx, sandboxID)
	if err != nil {
		return cubesandboxError("get sandbox", err)
	}
	if !isCrabboxCubeSandboxSandbox(sandbox) {
		return core.Exit(4, "cubesandbox sandbox %q is not claimed by Crabbox", req.ID)
	}
	leaseID := strings.TrimSpace(sandbox.Metadata["lease"])
	if !core.IsCanonicalLeaseID(leaseID) {
		return core.Exit(4, "cubesandbox sandbox %q lacks a canonical Crabbox lease id", req.ID)
	}
	cfg := cubesandboxClaimConfig(b.cfg)
	if existing, ok, err := resolveLeaseClaimForProviderCloudIDScope(sandbox.SandboxID, providerClaimScope(cfg)); err != nil {
		return err
	} else if ok && existing.LeaseID != leaseID {
		return core.Exit(4, "cubesandbox sandbox %q is already bound to lease %q", sandbox.SandboxID, existing.LeaseID)
	}
	previous, previousExists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		return err
	}
	if err := validateCubeSandboxReclaimCollision(leaseID, sandbox.SandboxID, previous, previousExists); err != nil {
		return err
	}
	var claim core.LeaseClaim
	if previousExists && previous.RepoRoot != "" {
		claim, err = claimLeaseTargetForRepoConfigIfUnchanged(
			leaseID,
			shared.EnvdSandboxSlug(leaseID, sandbox),
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
			shared.EnvdSandboxSlug(leaseID, sandbox),
			cfg,
			sandboxViews.Server(sandbox), core.SSHTarget{}, cfg.IdleTimeout,
			previous,
			previousExists,
		)
	}
	if err != nil {
		return err
	}
	if err := validateCubeSandboxClaim(cfg, claim, sandbox); err != nil {
		return err
	}
	if err := b.deleteClaimedSandbox(ctx, client, leaseID, sandbox.SandboxID); err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s\n", leaseID, sandbox.SandboxID)
	return nil
}

func (b *cubesandboxBackend) createSandbox(ctx context.Context, client shared.EnvdSandboxAPI, repo core.Repo, keep, reclaim bool, requestedSlug string) (string, shared.EnvdSandbox, string, error) {
	leaseID := core.NewLeaseID()
	slug, err := core.AllocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return "", shared.EnvdSandbox{}, "", err
	}
	cfg := b.cfg
	workspace, err := shared.CleanPOSIXWorkspacePath("cubesandbox workspace path", workspaceForConfig(cfg, b.rt).Path())
	if err != nil {
		return "", shared.EnvdSandbox{}, "", err
	}
	template := strings.TrimSpace(b.cfg.CubeSandbox.Template)
	if template == "" {
		return "", shared.EnvdSandbox{}, "", core.Exit(2, "provider=cubesandbox requires a template; set --cubesandbox-template, CUBE_TEMPLATE_ID, or cubeSandbox.template")
	}
	cfg.TTL = cubesandboxTimeoutDuration(cfg.TTL)
	cfg.ServerType = template
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", keep, core.ClockNow(b.rt.Clock).UTC())
	labels["state"] = "ready"
	labels["workdir"] = workspace
	labels["template"] = template
	if repo.Name != "" {
		labels["repo"] = repo.Name
	}
	timeoutSeconds := cubesandboxTimeoutSeconds(cfg.TTL)
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=cubesandbox lease=%s slug=%s template=%s timeout=%ds\n", leaseID, slug, template, timeoutSeconds)
	sandbox, err := client.CreateSandbox(ctx, shared.EnvdSandboxCreateRequest{
		TemplateID:          template,
		TimeoutSeconds:      timeoutSeconds,
		Metadata:            labels,
		AllowInternetAccess: true,
	})
	if err != nil {
		return "", shared.EnvdSandbox{}, "", cubesandboxError("create sandbox", err)
	}
	if sandbox.SandboxID == "" {
		return "", shared.EnvdSandbox{}, "", core.Exit(5, "cubesandbox create sandbox returned no sandbox id")
	}
	cfg = cubesandboxClaimConfig(cfg)
	server := sandboxViews.Server(sandbox)
	if err := claimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, core.SSHTarget{}, repo.Root, cfg.IdleTimeout, reclaim); err != nil {
		if cleanupErr := b.deleteSandboxForCleanup(client, sandbox.SandboxID); cleanupErr != nil {
			leakErr := fmt.Errorf("cleanup cubesandbox sandbox %s after claim failure: %w; run `crabbox stop --provider cubesandbox --id %s --reclaim` to retry cleanup", sandbox.SandboxID, cleanupErr, sandbox.SandboxID)
			fmt.Fprintf(b.rt.Stderr, "warning: %v\n", leakErr)
			return "", shared.EnvdSandbox{}, "", errors.Join(err, leakErr)
		}
		return "", shared.EnvdSandbox{}, "", err
	}
	return leaseID, sandbox, slug, nil
}

func (b *cubesandboxBackend) deleteSandboxForCleanup(client shared.EnvdSandboxAPI, sandboxID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), cubesandboxCleanupTimeout)
	defer cancel()
	return client.DeleteSandbox(ctx, sandboxID)
}

func cubesandboxCleanupCommand(leaseID string) string {
	return fmt.Sprintf("crabbox stop --provider %s --id %s", providerName, core.ShellQuote(leaseID))
}

func (b *cubesandboxBackend) resolveSandboxID(ctx context.Context, client shared.EnvdSandboxAPI, id, repoRoot string, reclaim bool) (string, string, string, error) {
	if id == "" {
		return "", "", "", core.Exit(2, "provider=cubesandbox requires a Crabbox lease id, slug, or CubeSandbox sandbox id")
	}
	cfg := cubesandboxClaimConfig(b.cfg)
	claim, ok, exact, err := resolveLeaseClaimForProviderScopeWithExact(id, providerClaimScope(cfg))
	if err != nil {
		return "", "", "", err
	}
	if exact && !ok {
		if claim.Provider != providerName {
			return "", "", "", core.Exit(4, "cubesandbox identifier %q is claimed by a different provider", id)
		}
		return "", "", "", core.Exit(4, "cubesandbox identifier %q is claimed for a different API endpoint", id)
	}
	if ok {
		return b.resolveClaimedSandbox(ctx, client, claim, repoRoot, reclaim)
	}
	if strings.HasPrefix(id, "cbx_") {
		return "", "", "", core.Exit(4, "cubesandbox lease %q has no exact local claim", id)
	}

	sandboxID := id
	if isCubeSandboxSyntheticID(id) {
		sandboxID = strings.TrimPrefix(id, "cubesandbox_")
	}
	if existing, ok, err := resolveLeaseClaimForProviderCloudIDScope(sandboxID, providerClaimScope(cfg)); err != nil {
		return "", "", "", err
	} else if ok {
		return b.resolveClaimedSandbox(ctx, client, existing, repoRoot, reclaim)
	}
	sandbox, err := client.GetSandbox(ctx, sandboxID)
	if err != nil {
		if isNotFoundError(err) {
			return "", "", "", core.Exit(4, "cubesandbox sandbox or claim %q was not found", id)
		}
		return "", "", "", cubesandboxError("get sandbox", err)
	}
	if !isCrabboxCubeSandboxSandbox(sandbox) {
		return "", "", "", core.Exit(4, "cubesandbox sandbox %q is not claimed by Crabbox", id)
	}
	leaseID := strings.TrimSpace(sandbox.Metadata["lease"])
	if !core.IsCanonicalLeaseID(leaseID) {
		return "", "", "", core.Exit(4, "cubesandbox sandbox %q lacks a canonical Crabbox lease id", id)
	}
	if repoRoot == "" || !reclaim {
		return "", "", "", core.Exit(4, "cubesandbox sandbox %q has no exact local claim; use --reclaim to adopt it explicitly", id)
	}
	if existing, ok, err := resolveLeaseClaimForProviderCloudIDScope(sandbox.SandboxID, providerClaimScope(cfg)); err != nil {
		return "", "", "", err
	} else if ok && existing.LeaseID != leaseID {
		return "", "", "", core.Exit(4, "cubesandbox sandbox %q is already bound to lease %q", sandbox.SandboxID, existing.LeaseID)
	}
	previous, previousExists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		return "", "", "", err
	}
	if err := validateCubeSandboxReclaimCollision(leaseID, sandbox.SandboxID, previous, previousExists); err != nil {
		return "", "", "", err
	}
	claim, err = claimLeaseTargetForRepoConfigIfUnchanged(
		leaseID,
		shared.EnvdSandboxSlug(leaseID, sandbox),
		cfg,
		sandboxViews.Server(sandbox), core.SSHTarget{}, repoRoot,
		cfg.IdleTimeout,
		true,
		previous,
		previousExists,
	)
	if err != nil {
		return "", "", "", err
	}
	if err := validateCubeSandboxClaim(cfg, claim, sandbox); err != nil {
		return "", "", "", err
	}
	return claim.LeaseID, sandbox.SandboxID, claim.Slug, nil
}

func validateCubeSandboxReclaimCollision(leaseID, sandboxID string, previous core.LeaseClaim, previousExists bool) error {
	if !previousExists {
		return nil
	}
	if previous.Provider != providerName {
		return core.Exit(4, "cubesandbox lease %q is already claimed by provider %q; refusing reclaim", leaseID, previous.Provider)
	}
	if previous.CloudID != "" && previous.CloudID != sandboxID {
		return core.Exit(4, "cubesandbox lease %q is already bound to sandbox %q; refusing retarget to %q", leaseID, previous.CloudID, sandboxID)
	}
	return nil
}

func (b *cubesandboxBackend) resolveClaimedSandbox(ctx context.Context, client shared.EnvdSandboxAPI, claim core.LeaseClaim, repoRoot string, reclaim bool) (string, string, string, error) {
	cfg := cubesandboxClaimConfig(b.cfg)
	if claim.ProviderScope != providerClaimScope(cfg) {
		return "", "", "", core.Exit(4, "cubesandbox lease %q belongs to a different API endpoint; use --reclaim with the exact sandbox id to adopt it", claim.LeaseID)
	}
	if strings.TrimSpace(claim.CloudID) == "" {
		return "", "", "", core.Exit(4, "cubesandbox lease %q has a legacy claim not bound to an exact sandbox; use --reclaim with the exact sandbox id to adopt it", claim.LeaseID)
	}
	sandbox, err := client.GetSandbox(ctx, claim.CloudID)
	if err != nil {
		if isNotFoundError(err) {
			return "", "", "", &cubesandboxClaimedSandboxMissingError{claim: claim}
		}
		return "", "", "", cubesandboxError("get sandbox", err)
	}
	if err := validateCubeSandboxClaim(cfg, claim, sandbox); err != nil {
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

func (b *cubesandboxBackend) deleteClaimedSandbox(ctx context.Context, client shared.EnvdSandboxAPI, leaseID, sandboxID string) error {
	cfg := cubesandboxClaimConfig(b.cfg)
	claim, ok, exact, err := resolveLeaseClaimForProviderScopeWithExact(leaseID, providerClaimScope(cfg))
	if err != nil {
		return err
	}
	if !ok || !exact {
		return core.Exit(4, "cubesandbox lease %q has no exact local claim; refusing deletion", leaseID)
	}
	if claim.ProviderScope != providerClaimScope(cfg) || claim.CloudID != sandboxID {
		return core.Exit(4, "cubesandbox lease %q is not bound to sandbox %q on this API endpoint; refusing deletion", leaseID, sandboxID)
	}
	return core.RemoveLeaseClaimIfUnchangedAfter(leaseID, claim, func() error {
		sandbox, err := client.GetSandbox(ctx, sandboxID)
		if err != nil {
			if isNotFoundError(err) {
				return nil
			}
			return cubesandboxError("get sandbox before delete", err)
		}
		if err := validateCubeSandboxClaim(cfg, claim, sandbox); err != nil {
			return err
		}
		if err := client.DeleteSandbox(ctx, sandboxID); err != nil {
			if isNotFoundError(err) {
				return nil
			}
			return cubesandboxError("delete sandbox", err)
		}
		return nil
	})
}

type cubesandboxClaimedSandboxMissingError struct {
	claim core.LeaseClaim
}

func (e *cubesandboxClaimedSandboxMissingError) Error() string {
	return fmt.Sprintf("cubesandbox sandbox %q for lease %q no longer exists", e.claim.CloudID, e.claim.LeaseID)
}

func validateCubeSandboxClaim(cfg core.Config, claim core.LeaseClaim, sandbox shared.EnvdSandbox) error {
	if claim.Provider != providerName || claim.ProviderScope != providerClaimScope(cubesandboxClaimConfig(cfg)) {
		return core.Exit(4, "cubesandbox lease %q belongs to a different provider or API endpoint", claim.LeaseID)
	}
	if claim.CloudID == "" || claim.CloudID != sandbox.SandboxID {
		return core.Exit(4, "cubesandbox lease %q is not bound to sandbox %q", claim.LeaseID, sandbox.SandboxID)
	}
	if !isCrabboxCubeSandboxSandbox(sandbox) || strings.TrimSpace(sandbox.Metadata["lease"]) != claim.LeaseID {
		return core.Exit(4, "cubesandbox sandbox %q no longer has canonical ownership metadata for lease %q", sandbox.SandboxID, claim.LeaseID)
	}
	return nil
}

func cubesandboxClaimConfig(cfg core.Config) core.Config {
	cfg.Provider = providerName
	if strings.TrimSpace(cfg.CubeSandbox.APIURL) == "" {
		cfg.CubeSandbox.APIURL = "http://127.0.0.1:3000"
	}
	return cfg
}

func isCubeSandboxSyntheticID(id string) bool {
	return strings.HasPrefix(id, "cubesandbox_") && len(id) > len("cubesandbox_")
}

func isCrabboxCubeSandboxSandbox(sandbox shared.EnvdSandbox) bool {
	return sandbox.Metadata["provider"] == providerName && sandbox.Metadata["crabbox"] == "true"
}

func cubesandboxTimeoutDuration(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return 5 * time.Minute
	}
	return ttl
}

func cubesandboxTimeoutSeconds(ttl time.Duration) int {
	return durationSecondsCeil(cubesandboxTimeoutDuration(ttl))
}

func rejectCubeSandboxSyncOptions(req core.RunRequest) error {
	if req.ChecksumSync {
		return core.Exit(2, "%s uses CubeSandbox archive sync; --checksum is not supported", providerName)
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
	var apiErr *cubesandboxAPIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == 404
}

func cubesandboxError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("cubesandbox %s: %w", action, err)
}
