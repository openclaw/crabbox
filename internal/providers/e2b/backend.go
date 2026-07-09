package e2b

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"path"
	"strings"
	"time"
)

type e2bFlagValues struct {
	APIURL   *string
	Domain   *string
	Template *string
	Workdir  *string
	User     *string
}

const e2bCleanupTimeout = 30 * time.Second

func RegisterE2BProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return e2bFlagValues{
		APIURL:   fs.String("e2b-api-url", defaults.E2B.APIURL, "E2B API URL"),
		Domain:   fs.String("e2b-domain", defaults.E2B.Domain, "E2B sandbox domain"),
		Template: fs.String("e2b-template", defaults.E2B.Template, "E2B sandbox template ID"),
		Workdir:  fs.String("e2b-workdir", defaults.E2B.Workdir, "E2B sandbox working directory"),
		User:     fs.String("e2b-user", defaults.E2B.User, "E2B sandbox user for command and file ownership"),
	}
}

func ApplyE2BProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == e2bProvider {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=e2b")
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=e2b")
		}
	}
	v, ok := values.(e2bFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "e2b-api-url") {
		cfg.E2B.APIURL = *v.APIURL
	}
	if flagWasSet(fs, "e2b-domain") {
		cfg.E2B.Domain = *v.Domain
	}
	if flagWasSet(fs, "e2b-template") {
		cfg.E2B.Template = *v.Template
	}
	if flagWasSet(fs, "e2b-workdir") {
		cfg.E2B.Workdir = *v.Workdir
	}
	if flagWasSet(fs, "e2b-user") {
		cfg.E2B.User = *v.User
	}
	return nil
}

func NewE2BBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = e2bProvider
	return &e2bBackend{spec: spec, cfg: cfg, rt: rt}
}

type e2bBackend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

const e2bMaxSandboxTimeout = time.Hour

func (b *e2bBackend) Spec() ProviderSpec { return b.spec }

func (b *e2bBackend) Warmup(ctx context.Context, req WarmupRequest) error {
	if err := validateE2BUser(b.cfg.E2B.User); err != nil {
		return err
	}
	started := b.now()
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
	total := b.now().Sub(started)
	fmt.Fprintf(b.rt.Stdout, "warmup complete total=%s\n", total.Round(time.Millisecond))
	if req.TimingJSON {
		return writeTimingJSON(b.rt.Stderr, timingReport{
			Provider: e2bProvider,
			LeaseID:  leaseID,
			Slug:     slug,
			TotalMs:  total.Milliseconds(),
			ExitCode: 0,
		})
	}
	return nil
}

func (b *e2bBackend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if err := rejectE2BSyncOptions(req); err != nil {
		return RunResult{}, err
	}
	processUser, err := e2bProcessUser(b.cfg.E2B.User)
	if err != nil {
		return RunResult{}, err
	}
	started := b.now()
	client, err := newE2BClient(b.cfg, b.rt)
	if err != nil {
		return RunResult{}, err
	}
	leaseID, sandboxID, slug := "", "", ""
	acquired := false
	if req.ID == "" {
		var sandbox e2bSandbox
		leaseID, sandbox, slug, err = b.createSandbox(ctx, client, req.Repo, req.Keep, req.Reclaim, req.RequestedSlug)
		if err != nil {
			return RunResult{}, err
		}
		sandboxID = sandbox.SandboxID
		fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=e2b sandbox=%s\n", leaseID, slug, sandboxID)
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
			if err := b.deleteClaimedSandboxForCleanup(client, leaseID, sandboxID); err != nil {
				fmt.Fprintf(b.rt.Stderr, "warning: e2b stop failed for %s: %v\n", sandboxID, err)
			}
		}()
	}
	result := RunResult{
		SyncDelegated: true,
		Session: &RunSessionHandle{
			Provider:       e2bProvider,
			LeaseID:        leaseID,
			Slug:           slug,
			Reused:         !acquired,
			Kept:           !shouldStop,
			CleanupCommand: e2bCleanupCommand(leaseID),
		},
	}
	finishResult := func() RunResult {
		result.Total = b.now().Sub(started)
		result.Session.Kept = !shouldStop
		return result
	}

	session, err := client.ConnectSandbox(ctx, sandboxID, e2bTimeoutSeconds(b.cfg.TTL))
	if err != nil {
		return finishResult(), e2bError("connect sandbox", err)
	}
	workspace := e2bWorkspacePath(b.cfg)
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
				Provider:      e2bProvider,
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
	command := e2bCommandString(req.Command, req.ShellMode)
	if command == "" {
		return finishResult(), exit(2, "missing command")
	}
	commandStarted := b.now()
	fmt.Fprintf(b.rt.Stderr, "running on e2b %s\n", strings.Join(req.Command, " "))
	exitCode, commandErr := client.StartProcess(ctx, session, e2bProcessRequest{
		Command: command,
		CWD:     workspace,
		Env:     req.Env,
		User:    processUser,
		Timeout: e2bTimeoutDuration(b.cfg.TTL),
		Stdout:  b.rt.Stdout,
		Stderr:  b.rt.Stderr,
	})
	commandDuration := b.now().Sub(commandStarted)
	result.ExitCode = exitCode
	result.Command = commandDuration
	result.Total = b.now().Sub(started)
	result.Session.Kept = !shouldStop
	if req.NoSync {
		fmt.Fprintf(b.rt.Stderr, "e2b run summary sync_skipped=true command=%s total=%s exit=%d\n", result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), result.ExitCode)
	} else {
		fmt.Fprintf(b.rt.Stderr, "e2b run summary sync=%s command=%s total=%s exit=%d\n", syncDuration.Round(time.Millisecond), result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), result.ExitCode)
	}
	if req.TimingJSON {
		if err := writeTimingJSON(b.rt.Stderr, timingReportWithRunResult(timingReport{
			Provider:      e2bProvider,
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
		handleDelegatedRunFailure(b.rt.Stderr, req, e2bProvider, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return finishResult(), ExitError{Code: 1, Message: fmt.Sprintf("e2b run failed: %v", commandErr)}
	}
	if result.ExitCode != 0 {
		handleDelegatedRunFailure(b.rt.Stderr, req, e2bProvider, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return finishResult(), ExitError{Code: result.ExitCode, Message: fmt.Sprintf("e2b run exited %d", result.ExitCode)}
	}
	return finishResult(), nil
}

func (b *e2bBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	client, err := newE2BClient(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	sandboxes, err := client.ListSandboxes(ctx, map[string]string{"crabbox": "true", "provider": e2bProvider})
	if err != nil {
		return nil, e2bError("list sandboxes", err)
	}
	servers := make([]Server, 0, len(sandboxes))
	for _, sandbox := range sandboxes {
		servers = append(servers, e2bSandboxToServer(sandbox))
	}
	return servers, nil
}

func (b *e2bBackend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	servers, err := b.List(ctx, ListRequest{})
	if err != nil {
		return DoctorResult{}, err
	}
	return inventoryDoctorResult(e2bProvider, len(servers)), nil
}

func (b *e2bBackend) Status(ctx context.Context, req StatusRequest) (statusView, error) {
	client, err := newE2BClient(b.cfg, b.rt)
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
			return statusView{}, e2bError("get sandbox", err)
		}
		view := e2bStatusView(leaseID, sandbox)
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

func (b *e2bBackend) Stop(ctx context.Context, req StopRequest) error {
	client, err := newE2BClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	claim, sandbox, err := b.resolveStopTarget(ctx, client, req.ID)
	if err != nil {
		var missing *e2bClaimedSandboxMissingError
		if errors.As(err, &missing) {
			if removeErr := removeLeaseClaimIfUnchangedAfter(missing.claim.LeaseID, missing.claim, nil); removeErr != nil {
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

func (b *e2bBackend) ReclaimAndStop(ctx context.Context, req StopRequest) error {
	if req.ID == "" {
		return exit(2, "provider=e2b stop --reclaim requires an exact E2B sandbox id")
	}
	sandboxID := req.ID
	if isE2BSyntheticID(sandboxID) {
		sandboxID = strings.TrimPrefix(sandboxID, "e2b_")
	} else if strings.HasPrefix(sandboxID, "cbx_") {
		return exit(2, "provider=e2b stop --reclaim requires an exact E2B sandbox id, not lease %q", req.ID)
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
		return exit(4, "e2b sandbox lookup for %q returned a different sandbox %q", sandboxID, sandbox.SandboxID)
	}
	if !isCrabboxE2BSandbox(sandbox) {
		return exit(4, "e2b sandbox %q is not claimed by Crabbox", req.ID)
	}
	leaseID := strings.TrimSpace(sandbox.Metadata["lease"])
	if !isCanonicalLeaseID(leaseID) {
		return exit(4, "e2b sandbox %q lacks a canonical Crabbox lease id", req.ID)
	}
	slug := strings.TrimSpace(sandbox.Metadata["slug"])
	if slug == "" {
		return exit(4, "e2b sandbox %q lacks a canonical Crabbox slug", req.ID)
	}
	cfg := e2bClaimConfig(b.cfg)
	if existing, ok, err := resolveLeaseClaimForProviderCloudIDScope(sandbox.SandboxID, providerClaimScope(cfg)); err != nil {
		return err
	} else if ok && existing.LeaseID != leaseID {
		return exit(4, "e2b sandbox %q is already bound to lease %q", sandbox.SandboxID, existing.LeaseID)
	}
	previous, previousExists, err := readLeaseClaimWithPresence(leaseID)
	if err != nil {
		return err
	}
	if err := validateE2BReclaimCollision(leaseID, sandbox.SandboxID, previous, previousExists); err != nil {
		return err
	}
	var claim LeaseClaim
	if previousExists && previous.RepoRoot != "" {
		claim, err = claimLeaseTargetForRepoConfigIfUnchanged(
			leaseID,
			slug,
			cfg,
			e2bSandboxToServer(sandbox),
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
			slug,
			cfg,
			e2bSandboxToServer(sandbox),
			SSHTarget{},
			cfg.IdleTimeout,
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

func (b *e2bBackend) createSandbox(ctx context.Context, client e2bAPI, repo Repo, keep, reclaim bool, requestedSlug string) (string, e2bSandbox, string, error) {
	leaseID := newLeaseID()
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return "", e2bSandbox{}, "", err
	}
	template := blank(b.cfg.E2B.Template, "base")
	cfg := b.cfg
	workspace, err := cleanE2BWorkspacePath(e2bWorkspacePath(cfg))
	if err != nil {
		return "", e2bSandbox{}, "", err
	}
	cfg.TTL = e2bTimeoutDuration(cfg.TTL)
	cfg.ServerType = template
	labels := directLeaseLabels(cfg, leaseID, slug, e2bProvider, "", keep, b.now().UTC())
	labels["state"] = "ready"
	labels["workdir"] = workspace
	labels["template"] = template
	if repo.Name != "" {
		labels["repo"] = repo.Name
	}
	timeoutSeconds := e2bTimeoutSeconds(cfg.TTL)
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=e2b lease=%s slug=%s template=%s timeout=%ds\n", leaseID, slug, template, timeoutSeconds)
	sandbox, err := client.CreateSandbox(ctx, e2bCreateSandboxRequest{
		TemplateID:          template,
		TimeoutSeconds:      timeoutSeconds,
		Metadata:            labels,
		AllowInternetAccess: true,
	})
	if err != nil {
		return "", e2bSandbox{}, "", e2bError("create sandbox", err)
	}
	if sandbox.SandboxID == "" {
		return "", e2bSandbox{}, "", exit(5, "e2b create sandbox returned no sandbox id")
	}
	cfg = e2bClaimConfig(cfg)
	if err := claimLeaseTargetForRepoConfig(leaseID, slug, cfg, e2bSandboxToServer(sandbox), SSHTarget{}, repo.Root, cfg.IdleTimeout, reclaim); err != nil {
		if cleanupErr := b.deleteSandboxForCleanup(client, sandbox.SandboxID); cleanupErr != nil {
			leakErr := fmt.Errorf("cleanup e2b sandbox %s after claim failure: %w; run `crabbox stop --provider e2b --id %s --reclaim` to retry cleanup", sandbox.SandboxID, cleanupErr, sandbox.SandboxID)
			fmt.Fprintf(b.rt.Stderr, "warning: %v\n", leakErr)
			return "", e2bSandbox{}, "", errors.Join(err, leakErr)
		}
		return "", e2bSandbox{}, "", err
	}
	return leaseID, sandbox, slug, nil
}

func (b *e2bBackend) deleteSandboxForCleanup(client e2bAPI, sandboxID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), e2bCleanupTimeout)
	defer cancel()
	return client.DeleteSandbox(ctx, sandboxID)
}

func (b *e2bBackend) deleteClaimedSandboxForCleanup(client e2bAPI, leaseID, sandboxID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), e2bCleanupTimeout)
	defer cancel()
	return b.deleteClaimedSandbox(ctx, client, leaseID, sandboxID)
}

func e2bCleanupCommand(leaseID string) string {
	return fmt.Sprintf("crabbox stop --provider %s --id %s", e2bProvider, shellQuote(leaseID))
}

func (b *e2bBackend) resolveStopTarget(ctx context.Context, client e2bAPI, id string) (LeaseClaim, e2bSandbox, error) {
	if id == "" {
		return LeaseClaim{}, e2bSandbox{}, exit(2, "provider=e2b requires a Crabbox lease id, slug, or E2B sandbox id")
	}
	cfg := e2bClaimConfig(b.cfg)
	claim, ok, exact, err := resolveLeaseClaimForProviderScopeWithExact(id, providerClaimScope(cfg))
	if err != nil {
		return LeaseClaim{}, e2bSandbox{}, err
	}
	if exact && !ok {
		if claim.Provider != e2bProvider {
			return LeaseClaim{}, e2bSandbox{}, exit(4, "e2b identifier %q is claimed by a different provider", id)
		}
		return LeaseClaim{}, e2bSandbox{}, exit(4, "e2b identifier %q is claimed for a different API endpoint", id)
	}
	if !ok {
		if strings.HasPrefix(id, "cbx_") {
			return LeaseClaim{}, e2bSandbox{}, exit(4, "e2b lease %q has no exact local claim", id)
		}
		sandboxID := id
		if isE2BSyntheticID(id) {
			sandboxID = strings.TrimPrefix(id, "e2b_")
		}
		claim, ok, err = resolveLeaseClaimForProviderCloudIDScope(sandboxID, providerClaimScope(cfg))
		if err != nil {
			return LeaseClaim{}, e2bSandbox{}, err
		}
		if !ok {
			return LeaseClaim{}, e2bSandbox{}, exit(4, "e2b sandbox %q has no exact local claim; use --reclaim to adopt it explicitly", id)
		}
	}
	if claim.ProviderScope != providerClaimScope(cfg) {
		return LeaseClaim{}, e2bSandbox{}, exit(4, "e2b lease %q belongs to a different API endpoint; use --reclaim with the exact sandbox id to adopt it", claim.LeaseID)
	}
	if strings.TrimSpace(claim.CloudID) == "" {
		return LeaseClaim{}, e2bSandbox{}, exit(4, "e2b lease %q has a legacy claim not bound to an exact sandbox; use --reclaim with the exact sandbox id to adopt it", claim.LeaseID)
	}
	sandbox, err := client.GetSandbox(ctx, claim.CloudID)
	if err != nil {
		if isNotFoundError(err) {
			return LeaseClaim{}, e2bSandbox{}, &e2bClaimedSandboxMissingError{claim: claim}
		}
		return LeaseClaim{}, e2bSandbox{}, e2bError("get sandbox", err)
	}
	if err := validateE2BClaim(cfg, claim, sandbox); err != nil {
		return LeaseClaim{}, e2bSandbox{}, err
	}
	return claim, sandbox, nil
}

func (b *e2bBackend) deleteClaimedSandbox(ctx context.Context, client e2bAPI, leaseID, sandboxID string) error {
	cfg := e2bClaimConfig(b.cfg)
	claim, ok, exact, err := resolveLeaseClaimForProviderScopeWithExact(leaseID, providerClaimScope(cfg))
	if err != nil {
		return err
	}
	if !ok || !exact {
		return exit(4, "e2b lease %q has no exact local claim; refusing deletion", leaseID)
	}
	if claim.ProviderScope != providerClaimScope(cfg) || claim.CloudID != sandboxID {
		return exit(4, "e2b lease %q is not bound to sandbox %q on this API endpoint; refusing deletion", leaseID, sandboxID)
	}
	return removeLeaseClaimIfUnchangedAfter(leaseID, claim, func() error {
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

func validateE2BReclaimCollision(leaseID, sandboxID string, previous LeaseClaim, previousExists bool) error {
	if !previousExists {
		return nil
	}
	if previous.Provider != e2bProvider {
		return exit(4, "e2b lease %q is already claimed by provider %q; refusing reclaim", leaseID, previous.Provider)
	}
	if previous.CloudID != "" && previous.CloudID != sandboxID {
		return exit(4, "e2b lease %q is already bound to sandbox %q; refusing retarget to %q", leaseID, previous.CloudID, sandboxID)
	}
	return nil
}

type e2bClaimedSandboxMissingError struct {
	claim LeaseClaim
}

func (e *e2bClaimedSandboxMissingError) Error() string {
	return fmt.Sprintf("e2b sandbox %q for lease %q no longer exists", e.claim.CloudID, e.claim.LeaseID)
}

func validateE2BClaim(cfg Config, claim LeaseClaim, sandbox e2bSandbox) error {
	if claim.Provider != e2bProvider || claim.ProviderScope != providerClaimScope(e2bClaimConfig(cfg)) {
		return exit(4, "e2b lease %q belongs to a different provider or API endpoint", claim.LeaseID)
	}
	if claim.CloudID == "" || claim.CloudID != sandbox.SandboxID {
		return exit(4, "e2b lease %q is not bound to sandbox %q", claim.LeaseID, sandbox.SandboxID)
	}
	if !isCrabboxE2BSandbox(sandbox) || strings.TrimSpace(sandbox.Metadata["lease"]) != claim.LeaseID || strings.TrimSpace(sandbox.Metadata["slug"]) != claim.Slug {
		return exit(4, "e2b sandbox %q no longer has canonical ownership metadata for lease %q", sandbox.SandboxID, claim.LeaseID)
	}
	return nil
}

func e2bClaimConfig(cfg Config) Config {
	cfg.Provider = e2bProvider
	if strings.TrimSpace(cfg.E2B.APIURL) == "" {
		cfg.E2B.APIURL = "https://api.e2b.app"
	}
	return cfg
}

func (b *e2bBackend) resolveSandboxID(ctx context.Context, client e2bAPI, id, repoRoot string, reclaim bool) (string, string, string, error) {
	if id == "" {
		return "", "", "", exit(2, "provider=e2b requires a Crabbox lease id, slug, or E2B sandbox id")
	}
	if claim, ok, err := resolveLeaseClaim(id); err != nil {
		return "", "", "", err
	} else if ok && claim.Provider == e2bProvider {
		if claim.CloudID != "" {
			cfg := e2bClaimConfig(b.cfg)
			if claim.ProviderScope != providerClaimScope(cfg) {
				return "", "", "", exit(4, "e2b lease %q belongs to a different API endpoint", claim.LeaseID)
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
					e2bSandboxToServer(sandbox),
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
			return "", "", "", exit(4, "e2b sandbox %q is not claimed by Crabbox", id)
		}
		leaseID := e2bLeaseID(sandbox)
		return leaseID, sandbox.SandboxID, e2bSlug(leaseID, sandbox), nil
	}
	if strings.HasPrefix(id, "cbx_") {
		sandbox, err := resolveE2BSandboxByLease(ctx, client, id)
		if err != nil {
			return "", "", "", err
		}
		return id, sandbox.SandboxID, e2bSlug(id, sandbox), nil
	}
	sandbox, err := client.GetSandbox(ctx, id)
	if err == nil && isCrabboxE2BSandbox(sandbox) {
		leaseID := e2bLeaseID(sandbox)
		return leaseID, sandbox.SandboxID, e2bSlug(leaseID, sandbox), nil
	}
	if err != nil && !isNotFoundError(err) {
		return "", "", "", e2bError("get sandbox", err)
	}
	return "", "", "", exit(4, "e2b sandbox or claim %q was not found", id)
}

func resolveE2BSandboxByLease(ctx context.Context, client e2bAPI, leaseID string) (e2bSandbox, error) {
	sandboxes, err := client.ListSandboxes(ctx, map[string]string{"lease": leaseID, "provider": e2bProvider})
	if err != nil {
		return e2bSandbox{}, e2bError("list sandboxes", err)
	}
	for _, sandbox := range sandboxes {
		if isCrabboxE2BSandbox(sandbox) {
			return sandbox, nil
		}
	}
	return e2bSandbox{}, exit(4, "e2b lease %q was not found", leaseID)
}

func e2bSandboxToServer(sandbox e2bSandbox) Server {
	labels := map[string]string{}
	for k, v := range sandbox.Metadata {
		labels[k] = v
	}
	labels["provider"] = e2bProvider
	labels["lease"] = e2bLeaseID(sandbox)
	if labels["slug"] == "" {
		labels["slug"] = newLeaseSlug(labels["lease"])
	}
	labels["target"] = targetLinux
	if labels["state"] == "" {
		labels["state"] = sandbox.State
	}
	server := Server{
		Provider: e2bProvider,
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

func e2bStatusView(leaseID string, sandbox e2bSandbox) statusView {
	server := e2bSandboxToServer(sandbox)
	return statusView{
		ID:         leaseID,
		Slug:       e2bSlug(leaseID, sandbox),
		Provider:   e2bProvider,
		TargetOS:   targetLinux,
		State:      sandbox.State,
		ServerID:   sandbox.SandboxID,
		ServerType: server.ServerType.Name,
		Network:    NetworkPublic,
		Ready:      e2bStatusReady(sandbox.State),
		Labels:     server.Labels,
	}
}

func e2bStatusReady(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "running":
		return true
	default:
		return false
	}
}

func e2bLeaseID(sandbox e2bSandbox) string {
	if lease := strings.TrimSpace(sandbox.Metadata["lease"]); lease != "" {
		return lease
	}
	return "e2b_" + sandbox.SandboxID
}

func e2bSlug(leaseID string, sandbox e2bSandbox) string {
	if slug := strings.TrimSpace(sandbox.Metadata["slug"]); slug != "" {
		return slug
	}
	return newLeaseSlug(leaseID)
}

func isE2BSyntheticID(id string) bool {
	return strings.HasPrefix(id, "e2b_") && len(id) > len("e2b_")
}

func isCrabboxE2BSandbox(sandbox e2bSandbox) bool {
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

func e2bWorkspacePath(cfg Config) string {
	workdir := strings.TrimSpace(cfg.E2B.Workdir)
	if workdir == "" {
		workdir = "crabbox"
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
		return "", exit(2, "invalid e2b.user %q: use a login name, not a path", user)
	}
	return clean, nil
}

func e2bCommandString(command []string, shellMode bool) string {
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

func rejectE2BSyncOptions(req RunRequest) error {
	if req.ChecksumSync {
		return exit(2, "%s uses E2B archive sync; --checksum is not supported", e2bProvider)
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

func (b *e2bBackend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}
