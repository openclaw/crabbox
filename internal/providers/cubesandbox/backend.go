package cubesandbox

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"
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

func RegisterCubeSandboxProviderFlags(fs *flag.FlagSet, defaults Config) any {
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

func ApplyCubeSandboxProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == providerName {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=cubesandbox")
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=cubesandbox")
		}
	}
	v, ok := values.(cubesandboxFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "cubesandbox-api-url") {
		cfg.CubeSandbox.APIURL = *v.APIURL
	}
	if flagWasSet(fs, "cubesandbox-domain") {
		cfg.CubeSandbox.Domain = *v.Domain
	}
	if flagWasSet(fs, "cubesandbox-template") {
		cfg.CubeSandbox.Template = *v.Template
	}
	if flagWasSet(fs, "cubesandbox-workdir") {
		cfg.CubeSandbox.Workdir = *v.Workdir
	}
	if flagWasSet(fs, "cubesandbox-user") {
		cfg.CubeSandbox.User = *v.User
	}
	if flagWasSet(fs, "cubesandbox-proxy-node-ip") {
		cfg.CubeSandbox.ProxyNodeIP = *v.ProxyNodeIP
	}
	if flagWasSet(fs, "cubesandbox-proxy-port-http") {
		cfg.CubeSandbox.ProxyPortHTTP = *v.ProxyPortHTTP
	}
	if flagWasSet(fs, "cubesandbox-proxy-scheme") {
		cfg.CubeSandbox.ProxyScheme = *v.ProxyScheme
	}
	return nil
}

func NewCubeSandboxBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	return &cubesandboxBackend{spec: spec, cfg: cfg, rt: rt}
}

type cubesandboxBackend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func (b *cubesandboxBackend) Spec() ProviderSpec { return b.spec }

func (b *cubesandboxBackend) Warmup(ctx context.Context, req WarmupRequest) error {
	if err := validateCubeSandboxUser(b.cfg.CubeSandbox.User); err != nil {
		return err
	}
	started := b.now()
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

func (b *cubesandboxBackend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if err := rejectCubeSandboxSyncOptions(req); err != nil {
		return RunResult{}, err
	}
	processUser, err := cubesandboxProcessUser(b.cfg.CubeSandbox.User)
	if err != nil {
		return RunResult{}, err
	}
	started := b.now()
	client, err := newCubeSandboxClient(b.cfg, b.rt)
	if err != nil {
		return RunResult{}, err
	}
	leaseID, sandboxID, slug := "", "", ""
	acquired := false
	if req.ID == "" {
		var sandbox cubesandboxSandbox
		leaseID, sandbox, slug, err = b.createSandbox(ctx, client, req.Repo, req.Keep, req.Reclaim, req.RequestedSlug)
		if err != nil {
			return RunResult{}, err
		}
		sandboxID = sandbox.SandboxID
		fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=cubesandbox sandbox=%s\n", leaseID, slug, sandboxID)
		acquired = true
	} else {
		leaseID, sandboxID, slug, err = b.resolveSandboxID(ctx, client, req.ID, req.Repo.Root, req.Reclaim)
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
			cleanupCtx, cancel := context.WithTimeout(context.Background(), cubesandboxCleanupTimeout)
			defer cancel()
			if err := b.deleteClaimedSandbox(cleanupCtx, client, leaseID, sandboxID); err != nil {
				fmt.Fprintf(b.rt.Stderr, "warning: cubesandbox stop failed for %s: %v\n", sandboxID, err)
			}
		}()
	}
	result := RunResult{
		SyncDelegated: true,
		Session: &RunSessionHandle{
			Provider:       providerName,
			LeaseID:        leaseID,
			Slug:           slug,
			Reused:         !acquired,
			Kept:           !shouldStop,
			CleanupCommand: cubesandboxCleanupCommand(leaseID),
		},
	}
	finishResult := func() RunResult {
		result.Total = b.now().Sub(started)
		result.Session.Kept = !shouldStop
		return result
	}

	session, err := client.ConnectSandbox(ctx, sandboxID, cubesandboxTimeoutSeconds(b.cfg.TTL))
	if err != nil {
		return finishResult(), cubesandboxError("connect sandbox", err)
	}
	workspace := cubesandboxWorkspacePath(b.cfg)
	syncDuration := time.Duration(0)
	syncPhases := []timingPhase{{Name: "sync", Skipped: true, Reason: "--no-sync"}}
	if !req.NoSync {
		syncPhases, syncDuration, err = b.syncWorkspace(ctx, client, session, req, workspace)
		if err != nil {
			return finishResult(), err
		}
		fmt.Fprintf(b.rt.Stderr, "sync complete in %s\n", syncDuration.Round(time.Millisecond))
	} else if err := b.prepareWorkspace(ctx, client, session, workspace); err != nil {
		return finishResult(), err
	}
	if req.SyncOnly {
		result := finishResult()
		fmt.Fprintf(b.rt.Stdout, "synced %s\n", workspace)
		if req.TimingJSON {
			err := writeTimingJSON(b.rt.Stderr, timingReportWithRunResult(timingReport{
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
			}, result, nil))
			return result, err
		}
		return result, nil
	}
	command := cubesandboxCommandString(req.Command, req.ShellMode)
	if command == "" {
		return finishResult(), exit(2, "missing command")
	}
	commandStarted := b.now()
	fmt.Fprintf(b.rt.Stderr, "running on cubesandbox %s\n", strings.Join(req.Command, " "))
	commandEnv, strippedAuthEnv := cubeSandboxCommandEnv(req.Env)
	if len(strippedAuthEnv) > 0 {
		fmt.Fprintf(b.rt.Stderr, "warning: provider=%s did not forward provider authentication variables: %s\n", providerName, strings.Join(strippedAuthEnv, ","))
	}
	exitCode, commandErr := client.StartProcess(ctx, session, cubesandboxProcessRequest{
		Command: command,
		CWD:     workspace,
		Env:     commandEnv,
		User:    processUser,
		Timeout: cubesandboxTimeoutDuration(b.cfg.TTL),
		Stdout:  b.rt.Stdout,
		Stderr:  b.rt.Stderr,
	})
	commandDuration := b.now().Sub(commandStarted)
	result.ExitCode = exitCode
	result.Command = commandDuration
	result.Total = b.now().Sub(started)
	result.Session.Kept = !shouldStop
	if req.NoSync {
		fmt.Fprintf(b.rt.Stderr, "cubesandbox run summary sync_skipped=true command=%s total=%s exit=%d\n", result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), result.ExitCode)
	} else {
		fmt.Fprintf(b.rt.Stderr, "cubesandbox run summary sync=%s command=%s total=%s exit=%d\n", syncDuration.Round(time.Millisecond), result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), result.ExitCode)
	}
	if req.TimingJSON {
		if err := writeTimingJSON(b.rt.Stderr, timingReportWithRunResult(timingReport{
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
		}, result, commandErr)); err != nil {
			return result, err
		}
	}
	if commandErr != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return finishResult(), ExitError{Code: 1, Message: fmt.Sprintf("cubesandbox run failed: %v", commandErr)}
	}
	if result.ExitCode != 0 {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return finishResult(), ExitError{Code: result.ExitCode, Message: fmt.Sprintf("cubesandbox run exited %d", result.ExitCode)}
	}
	return finishResult(), nil
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

func (b *cubesandboxBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	client, err := newCubeSandboxClient(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	sandboxes, err := client.ListSandboxes(ctx, map[string]string{"crabbox": "true", "provider": providerName})
	if err != nil {
		return nil, cubesandboxError("list sandboxes", err)
	}
	servers := make([]Server, 0, len(sandboxes))
	for _, sandbox := range sandboxes {
		servers = append(servers, cubesandboxSandboxToServer(sandbox))
	}
	return servers, nil
}

func (b *cubesandboxBackend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	servers, err := b.List(ctx, ListRequest{})
	if err != nil {
		return DoctorResult{}, err
	}
	return inventoryDoctorResult(providerName, len(servers)), nil
}

func (b *cubesandboxBackend) Status(ctx context.Context, req StatusRequest) (statusView, error) {
	client, err := newCubeSandboxClient(b.cfg, b.rt)
	if err != nil {
		return statusView{}, err
	}
	leaseID, sandboxID, _, err := b.resolveSandboxID(ctx, client, req.ID, "", false)
	if err != nil {
		return statusView{}, err
	}
	deadline := b.now().Add(req.WaitTimeout)
	if req.WaitTimeout <= 0 {
		deadline = b.now().Add(5 * time.Minute)
	}
	for {
		sandbox, err := client.GetSandbox(ctx, sandboxID)
		if err != nil {
			return statusView{}, cubesandboxError("get sandbox", err)
		}
		view := cubesandboxStatusView(leaseID, sandbox)
		if !req.Wait || view.Ready {
			return view, nil
		}
		if b.now().After(deadline) {
			return statusView{}, exit(5, "timed out waiting for sandbox %s to become ready", sandboxID)
		}
		select {
		case <-ctx.Done():
			return statusView{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *cubesandboxBackend) Stop(ctx context.Context, req StopRequest) error {
	client, err := newCubeSandboxClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, sandboxID, _, err := b.resolveSandboxID(ctx, client, req.ID, "", false)
	if err != nil {
		return err
	}
	if err := b.deleteClaimedSandbox(ctx, client, leaseID, sandboxID); err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s\n", leaseID, sandboxID)
	return nil
}

func (b *cubesandboxBackend) ReclaimAndStop(ctx context.Context, req StopRequest) error {
	if req.ID == "" {
		return exit(2, "provider=cubesandbox stop --reclaim requires an exact CubeSandbox sandbox id")
	}
	sandboxID := req.ID
	if isCubeSandboxSyntheticID(sandboxID) {
		sandboxID = strings.TrimPrefix(sandboxID, "cubesandbox_")
	} else if strings.HasPrefix(sandboxID, "cbx_") {
		return exit(2, "provider=cubesandbox stop --reclaim requires an exact CubeSandbox sandbox id, not lease %q", req.ID)
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
		return exit(4, "cubesandbox sandbox %q is not claimed by Crabbox", req.ID)
	}
	leaseID := strings.TrimSpace(sandbox.Metadata["lease"])
	if !isCanonicalLeaseID(leaseID) {
		return exit(4, "cubesandbox sandbox %q lacks a canonical Crabbox lease id", req.ID)
	}
	cfg := cubesandboxClaimConfig(b.cfg)
	if existing, ok, err := resolveLeaseClaimForProviderCloudIDScope(sandbox.SandboxID, providerClaimScope(cfg)); err != nil {
		return err
	} else if ok && existing.LeaseID != leaseID {
		return exit(4, "cubesandbox sandbox %q is already bound to lease %q", sandbox.SandboxID, existing.LeaseID)
	}
	previous, previousExists, err := readLeaseClaimWithPresence(leaseID)
	if err != nil {
		return err
	}
	if err := validateCubeSandboxReclaimCollision(leaseID, sandbox.SandboxID, previous, previousExists); err != nil {
		return err
	}
	var claim LeaseClaim
	if previousExists && previous.RepoRoot != "" {
		claim, err = claimLeaseTargetForRepoConfigIfUnchanged(
			leaseID,
			cubesandboxSlug(leaseID, sandbox),
			cfg,
			cubesandboxSandboxToServer(sandbox),
			SSHTarget{},
			previous.RepoRoot,
			cfg.IdleTimeout,
			true,
			previous,
			true,
		)
	} else {
		claim, err = claimLeaseTargetForConfigIfUnchanged(
			leaseID,
			cubesandboxSlug(leaseID, sandbox),
			cfg,
			cubesandboxSandboxToServer(sandbox),
			SSHTarget{},
			cfg.IdleTimeout,
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

func (b *cubesandboxBackend) createSandbox(ctx context.Context, client cubesandboxAPI, repo Repo, keep, reclaim bool, requestedSlug string) (string, cubesandboxSandbox, string, error) {
	leaseID := newLeaseID()
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return "", cubesandboxSandbox{}, "", err
	}
	cfg := b.cfg
	workspace, err := cleanCubeSandboxWorkspacePath(cubesandboxWorkspacePath(cfg))
	if err != nil {
		return "", cubesandboxSandbox{}, "", err
	}
	template := strings.TrimSpace(b.cfg.CubeSandbox.Template)
	if template == "" {
		return "", cubesandboxSandbox{}, "", exit(2, "provider=cubesandbox requires a template; set --cubesandbox-template, CUBE_TEMPLATE_ID, or cubeSandbox.template")
	}
	cfg.TTL = cubesandboxTimeoutDuration(cfg.TTL)
	cfg.ServerType = template
	labels := directLeaseLabels(cfg, leaseID, slug, providerName, "", keep, b.now().UTC())
	labels["state"] = "ready"
	labels["workdir"] = workspace
	labels["template"] = template
	if repo.Name != "" {
		labels["repo"] = repo.Name
	}
	timeoutSeconds := cubesandboxTimeoutSeconds(cfg.TTL)
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=cubesandbox lease=%s slug=%s template=%s timeout=%ds\n", leaseID, slug, template, timeoutSeconds)
	sandbox, err := client.CreateSandbox(ctx, cubesandboxCreateSandboxRequest{
		TemplateID:          template,
		TimeoutSeconds:      timeoutSeconds,
		Metadata:            labels,
		AllowInternetAccess: true,
	})
	if err != nil {
		return "", cubesandboxSandbox{}, "", cubesandboxError("create sandbox", err)
	}
	if sandbox.SandboxID == "" {
		return "", cubesandboxSandbox{}, "", exit(5, "cubesandbox create sandbox returned no sandbox id")
	}
	cfg = cubesandboxClaimConfig(cfg)
	server := cubesandboxSandboxToServer(sandbox)
	if err := claimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, SSHTarget{}, repo.Root, cfg.IdleTimeout, reclaim); err != nil {
		if cleanupErr := b.deleteSandboxForCleanup(client, sandbox.SandboxID); cleanupErr != nil {
			leakErr := fmt.Errorf("cleanup cubesandbox sandbox %s after claim failure: %w; run `crabbox stop --provider cubesandbox --id %s --reclaim` to retry cleanup", sandbox.SandboxID, cleanupErr, sandbox.SandboxID)
			fmt.Fprintf(b.rt.Stderr, "warning: %v\n", leakErr)
			return "", cubesandboxSandbox{}, "", errors.Join(err, leakErr)
		}
		return "", cubesandboxSandbox{}, "", err
	}
	return leaseID, sandbox, slug, nil
}

func (b *cubesandboxBackend) deleteSandboxForCleanup(client cubesandboxAPI, sandboxID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), cubesandboxCleanupTimeout)
	defer cancel()
	return client.DeleteSandbox(ctx, sandboxID)
}

func cubesandboxCleanupCommand(leaseID string) string {
	return fmt.Sprintf("crabbox stop --provider %s --id %s", providerName, shellQuote(leaseID))
}

func (b *cubesandboxBackend) resolveSandboxID(ctx context.Context, client cubesandboxAPI, id, repoRoot string, reclaim bool) (string, string, string, error) {
	if id == "" {
		return "", "", "", exit(2, "provider=cubesandbox requires a Crabbox lease id, slug, or CubeSandbox sandbox id")
	}
	cfg := cubesandboxClaimConfig(b.cfg)
	claim, ok, exact, err := resolveLeaseClaimForProviderScopeWithExact(id, providerClaimScope(cfg))
	if err != nil {
		return "", "", "", err
	}
	if exact && !ok {
		if claim.Provider != providerName {
			return "", "", "", exit(4, "cubesandbox identifier %q is claimed by a different provider", id)
		}
		return "", "", "", exit(4, "cubesandbox identifier %q is claimed for a different API endpoint", id)
	}
	if ok {
		return b.resolveClaimedSandbox(ctx, client, claim, repoRoot, reclaim)
	}
	if strings.HasPrefix(id, "cbx_") {
		return "", "", "", exit(4, "cubesandbox lease %q has no exact local claim", id)
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
			return "", "", "", exit(4, "cubesandbox sandbox or claim %q was not found", id)
		}
		return "", "", "", cubesandboxError("get sandbox", err)
	}
	if !isCrabboxCubeSandboxSandbox(sandbox) {
		return "", "", "", exit(4, "cubesandbox sandbox %q is not claimed by Crabbox", id)
	}
	leaseID := strings.TrimSpace(sandbox.Metadata["lease"])
	if !isCanonicalLeaseID(leaseID) {
		return "", "", "", exit(4, "cubesandbox sandbox %q lacks a canonical Crabbox lease id", id)
	}
	if repoRoot == "" || !reclaim {
		return "", "", "", exit(4, "cubesandbox sandbox %q has no exact local claim; use --reclaim to adopt it explicitly", id)
	}
	if existing, ok, err := resolveLeaseClaimForProviderCloudIDScope(sandbox.SandboxID, providerClaimScope(cfg)); err != nil {
		return "", "", "", err
	} else if ok && existing.LeaseID != leaseID {
		return "", "", "", exit(4, "cubesandbox sandbox %q is already bound to lease %q", sandbox.SandboxID, existing.LeaseID)
	}
	previous, previousExists, err := readLeaseClaimWithPresence(leaseID)
	if err != nil {
		return "", "", "", err
	}
	if err := validateCubeSandboxReclaimCollision(leaseID, sandbox.SandboxID, previous, previousExists); err != nil {
		return "", "", "", err
	}
	claim, err = claimLeaseTargetForRepoConfigIfUnchanged(
		leaseID,
		cubesandboxSlug(leaseID, sandbox),
		cfg,
		cubesandboxSandboxToServer(sandbox),
		SSHTarget{},
		repoRoot,
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

func validateCubeSandboxReclaimCollision(leaseID, sandboxID string, previous LeaseClaim, previousExists bool) error {
	if !previousExists {
		return nil
	}
	if previous.Provider != providerName {
		return exit(4, "cubesandbox lease %q is already claimed by provider %q; refusing reclaim", leaseID, previous.Provider)
	}
	if previous.CloudID != "" && previous.CloudID != sandboxID {
		return exit(4, "cubesandbox lease %q is already bound to sandbox %q; refusing retarget to %q", leaseID, previous.CloudID, sandboxID)
	}
	return nil
}

func (b *cubesandboxBackend) resolveClaimedSandbox(ctx context.Context, client cubesandboxAPI, claim LeaseClaim, repoRoot string, reclaim bool) (string, string, string, error) {
	cfg := cubesandboxClaimConfig(b.cfg)
	if claim.ProviderScope != providerClaimScope(cfg) {
		return "", "", "", exit(4, "cubesandbox lease %q belongs to a different API endpoint; use --reclaim with the exact sandbox id to adopt it", claim.LeaseID)
	}
	if strings.TrimSpace(claim.CloudID) == "" {
		return "", "", "", exit(4, "cubesandbox lease %q has a legacy claim not bound to an exact sandbox; use --reclaim with the exact sandbox id to adopt it", claim.LeaseID)
	}
	sandbox, err := client.GetSandbox(ctx, claim.CloudID)
	if err != nil {
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
			cubesandboxSandboxToServer(sandbox),
			SSHTarget{},
			repoRoot,
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

func (b *cubesandboxBackend) deleteClaimedSandbox(ctx context.Context, client cubesandboxAPI, leaseID, sandboxID string) error {
	cfg := cubesandboxClaimConfig(b.cfg)
	claim, ok, exact, err := resolveLeaseClaimForProviderScopeWithExact(leaseID, providerClaimScope(cfg))
	if err != nil {
		return err
	}
	if !ok || !exact {
		return exit(4, "cubesandbox lease %q has no exact local claim; refusing deletion", leaseID)
	}
	if claim.ProviderScope != providerClaimScope(cfg) || claim.CloudID != sandboxID {
		return exit(4, "cubesandbox lease %q is not bound to sandbox %q on this API endpoint; refusing deletion", leaseID, sandboxID)
	}
	return removeLeaseClaimIfUnchangedAfter(leaseID, claim, func() error {
		sandbox, err := client.GetSandbox(ctx, sandboxID)
		if err != nil {
			return cubesandboxError("get sandbox before delete", err)
		}
		if err := validateCubeSandboxClaim(cfg, claim, sandbox); err != nil {
			return err
		}
		if err := client.DeleteSandbox(ctx, sandboxID); err != nil {
			return cubesandboxError("delete sandbox", err)
		}
		return nil
	})
}

func validateCubeSandboxClaim(cfg Config, claim LeaseClaim, sandbox cubesandboxSandbox) error {
	if claim.Provider != providerName || claim.ProviderScope != providerClaimScope(cubesandboxClaimConfig(cfg)) {
		return exit(4, "cubesandbox lease %q belongs to a different provider or API endpoint", claim.LeaseID)
	}
	if claim.CloudID == "" || claim.CloudID != sandbox.SandboxID {
		return exit(4, "cubesandbox lease %q is not bound to sandbox %q", claim.LeaseID, sandbox.SandboxID)
	}
	if !isCrabboxCubeSandboxSandbox(sandbox) || strings.TrimSpace(sandbox.Metadata["lease"]) != claim.LeaseID {
		return exit(4, "cubesandbox sandbox %q no longer has canonical ownership metadata for lease %q", sandbox.SandboxID, claim.LeaseID)
	}
	return nil
}

func cubesandboxClaimConfig(cfg Config) Config {
	cfg.Provider = providerName
	if strings.TrimSpace(cfg.CubeSandbox.APIURL) == "" {
		cfg.CubeSandbox.APIURL = "http://127.0.0.1:3000"
	}
	return cfg
}

func cubesandboxSandboxToServer(sandbox cubesandboxSandbox) Server {
	labels := map[string]string{}
	for k, v := range sandbox.Metadata {
		labels[k] = v
	}
	labels["provider"] = providerName
	labels["lease"] = cubesandboxLeaseID(sandbox)
	if labels["slug"] == "" {
		labels["slug"] = newLeaseSlug(labels["lease"])
	}
	labels["target"] = targetLinux
	if labels["state"] == "" {
		labels["state"] = sandbox.State
	}
	server := Server{
		Provider: providerName,
		CloudID:  sandbox.SandboxID,
		Name:     sandbox.SandboxID,
		Status:   sandbox.State,
		Labels:   labels,
	}
	server.ServerType.Name = blank(sandbox.Alias, sandbox.TemplateID)
	if server.ServerType.Name == "" {
		server.ServerType.Name = "base"
	}
	return server
}

func cubesandboxStatusView(leaseID string, sandbox cubesandboxSandbox) statusView {
	server := cubesandboxSandboxToServer(sandbox)
	return statusView{
		ID:         leaseID,
		Slug:       cubesandboxSlug(leaseID, sandbox),
		Provider:   providerName,
		TargetOS:   targetLinux,
		State:      sandbox.State,
		ServerID:   sandbox.SandboxID,
		ServerType: server.ServerType.Name,
		Network:    NetworkPublic,
		Ready:      cubesandboxStatusReady(sandbox.State),
		Labels:     server.Labels,
	}
}

func cubesandboxStatusReady(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "running":
		return true
	default:
		return false
	}
}

func cubesandboxLeaseID(sandbox cubesandboxSandbox) string {
	if lease := strings.TrimSpace(sandbox.Metadata["lease"]); lease != "" {
		return lease
	}
	return "cubesandbox_" + sandbox.SandboxID
}

func cubesandboxSlug(leaseID string, sandbox cubesandboxSandbox) string {
	if slug := strings.TrimSpace(sandbox.Metadata["slug"]); slug != "" {
		return slug
	}
	return newLeaseSlug(leaseID)
}

func isCubeSandboxSyntheticID(id string) bool {
	return strings.HasPrefix(id, "cubesandbox_") && len(id) > len("cubesandbox_")
}

func isCrabboxCubeSandboxSandbox(sandbox cubesandboxSandbox) bool {
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

func cubesandboxWorkspacePath(cfg Config) string {
	workdir := strings.TrimSpace(cfg.CubeSandbox.Workdir)
	if workdir == "" {
		workdir = "crabbox"
	}
	if strings.HasPrefix(workdir, "/") {
		return path.Clean(workdir)
	}
	return path.Join(cubesandboxUserHome(cfg.CubeSandbox.User), workdir)
}

func cubesandboxUserHome(user string) string {
	user = cubesandboxWorkspaceUser(user)
	if user == "" {
		user = "user"
	}
	if user == "root" {
		return "/root"
	}
	return path.Join("/home", user)
}

func cubesandboxWorkspaceUser(user string) string {
	clean, err := cubesandboxProcessUser(user)
	if err != nil || clean == "" {
		return "root"
	}
	return clean
}

func validateCubeSandboxUser(user string) error {
	_, err := cubesandboxProcessUser(user)
	return err
}

func cubesandboxProcessUser(user string) (string, error) {
	clean := strings.TrimSpace(user)
	if clean == "" {
		return "root", nil
	}
	if clean == "." || clean == ".." || strings.ContainsAny(clean, `/\`) || strings.ContainsRune(clean, 0) {
		return "", exit(2, "invalid cubesandbox.user %q: use a login name, not a path", user)
	}
	return clean, nil
}

func cubesandboxCommandString(command []string, shellMode bool) string {
	if len(command) == 0 {
		return ""
	}
	if shellMode {
		return strings.Join(command, " ")
	}
	if shouldUseShell(command) || leadingEnvAssignment(command) {
		return shellScriptFromArgv(command)
	}
	return strings.Join(shellWords(command), " ")
}

func rejectCubeSandboxSyncOptions(req RunRequest) error {
	if req.ChecksumSync {
		return exit(2, "%s uses CubeSandbox archive sync; --checksum is not supported", providerName)
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

func (b *cubesandboxBackend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}
