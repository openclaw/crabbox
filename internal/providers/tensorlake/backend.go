package tensorlake

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func NewTensorlakeBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	return &tensorlakeBackend{spec: spec, cfg: cfg, rt: rt}
}

type tensorlakeBackend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func (b *tensorlakeBackend) Spec() ProviderSpec { return b.spec }

func (b *tensorlakeBackend) Warmup(ctx context.Context, req WarmupRequest) error {
	started := b.now()
	cli, err := newTensorlakeCLI(b.cfg, b.rt)
	if err != nil {
		return err
	}
	claim, name, err := b.createSandbox(ctx, cli, req.Repo, req.Reclaim, req.RequestedSlug)
	if err != nil {
		return err
	}
	leaseID, sandboxID, slug := claim.LeaseID, claim.CloudID, claim.Slug
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s sandbox=%s name=%s\n", leaseID, slug, providerName, sandboxID, name)
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: tensorlake warmup keeps the sandbox until explicit stop\n")
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

func (b *tensorlakeBackend) Run(ctx context.Context, req RunRequest) (result RunResult, retErr error) {
	if err := rejectIncompatibleSyncOptions(req); err != nil {
		return RunResult{}, err
	}
	workdir, err := tensorlakeWorkdir(b.cfg)
	if err != nil {
		return RunResult{}, err
	}
	started := b.now()
	var prepared *core.PreparedArchive
	if !req.NoSync {
		prepared, err = b.prepareArchive(ctx, req)
		if err != nil {
			return RunResult{}, err
		}
		defer prepared.Close()
	}
	cli, err := newTensorlakeCLI(b.cfg, b.rt)
	if err != nil {
		return RunResult{}, err
	}
	var claim core.LeaseClaim
	acquired := false
	if req.ID == "" {
		var name string
		claim, name, err = b.createSandbox(ctx, cli, req.Repo, req.Reclaim, req.RequestedSlug)
		if err != nil {
			return RunResult{}, err
		}
		fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s sandbox=%s name=%s\n", claim.LeaseID, claim.Slug, providerName, claim.CloudID, name)
		acquired = true
	} else {
		claim, err = b.resolveLease(ctx, cli, req.ID, req.Repo.Root, req.Reclaim)
		if err != nil {
			return RunResult{}, err
		}
	}
	leaseID, sandboxID, slug := claim.LeaseID, claim.CloudID, claim.Slug
	shouldStop := acquired && !req.Keep
	cleanedUp := false
	session := &RunSessionHandle{
		Provider:       providerName,
		LeaseID:        leaseID,
		Slug:           slug,
		Reused:         !acquired,
		Kept:           !shouldStop,
		CleanupCommand: tensorlakeCleanupCommand(leaseID),
	}
	finishResult := func(result RunResult) RunResult {
		if result.Provider == "" {
			result.Provider = providerName
		}
		if result.LeaseID == "" {
			result.LeaseID = leaseID
		}
		if result.Slug == "" {
			result.Slug = slug
		}
		result.Session = session
		result.Session.Kept = !cleanedUp && !shouldStop
		return result
	}
	defer func() {
		result = finishResult(result)
	}()
	cleanupSandbox := func() error {
		if !shouldStop {
			return nil
		}
		if termErr := cli.removeBoundClaim(context.Background(), claim); termErr != nil {
			shouldStop = false
			return termErr
		}
		cleanedUp = true
		shouldStop = false
		return nil
	}
	if shouldStop {
		defer func() {
			if termErr := cleanupSandbox(); termErr != nil {
				fmt.Fprintf(b.rt.Stderr, "warning: tensorlake terminate failed for %s: %v\n", sandboxID, termErr)
			}
		}()
	}
	_, binding, err := bindingForClaim(claim)
	if err != nil {
		return RunResult{}, err
	}
	if err := core.WithLeaseClaimUnchanged(leaseID, claim, func() error {
		item, err := cli.verifyBinding(ctx, binding)
		if err == nil && item.State == "terminated" {
			return exit(2, "Tensorlake sandbox has terminated; create a new lease")
		}
		return err
	}); err != nil {
		return RunResult{}, err
	}
	fmt.Fprintf(b.rt.Stderr, "provider=%s lease=%s sandbox=%s workdir=%s\n", providerName, leaseID, sandboxID, workdir)

	syncDuration := time.Duration(0)
	syncPhases := []timingPhase{{Name: "sync", Skipped: true, Reason: "--no-sync"}}
	if !req.NoSync {
		var err error
		syncPhases, syncDuration, err = b.syncWorkspace(ctx, cli, sandboxID, req, workdir, prepared)
		if err != nil {
			return RunResult{Total: b.now().Sub(started), SyncDelegated: true}, err
		}
		fmt.Fprintf(b.rt.Stderr, "sync complete in %s\n", syncDuration.Round(time.Millisecond))
	} else if err := b.prepareWorkspace(ctx, cli, sandboxID, workdir); err != nil {
		return RunResult{}, err
	}

	command, err := buildCommand(req.Command, req.ShellMode)
	if err != nil {
		return RunResult{}, err
	}
	if req.EnvSummary || strings.TrimSpace(os.Getenv("CRABBOX_ENV_ALLOW")) != "" {
		printEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
	}
	if len(req.Env) > 0 {
		envPath, cleanup, err := b.uploadEnvProfile(ctx, cli, claim, req.Env)
		if cleanup != nil {
			defer func() {
				cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), envProfileCleanupTimeout)
				defer cancel()
				cleanup(cleanupCtx)
			}()
		}
		if err != nil {
			return RunResult{}, err
		}
		command = shared.WrapCommandWithShellEnvProfile(command, envPath)
	}
	commandStart := b.now()
	exitCode, runErr := cli.execStream(ctx, sandboxID, workdir, command, b.rt.Stdout, b.rt.Stderr)
	commandDuration := b.now().Sub(commandStart)
	result = RunResult{
		ExitCode:      exitCode,
		Command:       commandDuration,
		Total:         b.now().Sub(started),
		SyncDelegated: true,
	}
	result = finalizeRunResult(result, runErr)
	if req.NoSync {
		fmt.Fprintf(b.rt.Stderr, "tensorlake run summary sync_skipped=true command=%s total=%s exit=%d\n",
			result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), exitCode)
	} else {
		fmt.Fprintf(b.rt.Stderr, "tensorlake run summary sync=%s command=%s total=%s exit=%d\n",
			syncDuration.Round(time.Millisecond), result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), exitCode)
	}
	if req.TimingJSON {
		report := timingReportWithRunResult(timingReport{
			Provider:      providerName,
			LeaseID:       leaseID,
			Slug:          slug,
			SyncDelegated: true,
			SyncMs:        syncDuration.Milliseconds(),
			SyncPhases:    syncPhases,
			SyncSkipped:   req.NoSync,
			CommandMs:     result.Command.Milliseconds(),
			TotalMs:       result.Total.Milliseconds(),
			ExitCode:      exitCode,
			Label:         strings.TrimSpace(req.Label),
		}, result, runErr)
		if err := writeTimingJSON(b.rt.Stderr, report); err != nil {
			return result, err
		}
	}
	if runErr != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return result, ExitError{Code: 1, Message: fmt.Sprintf("tensorlake run failed: %v", runErr)}
	}
	if exitCode != 0 {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return result, ExitError{Code: exitCode, Message: fmt.Sprintf("tensorlake run exited %d", exitCode)}
	}
	return result, nil
}

func (b *tensorlakeBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	cli, err := newTensorlakeCLI(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	ids, err := cli.listIDs(ctx)
	if err != nil {
		return nil, err
	}
	servers := make([]Server, 0, len(ids))
	for _, id := range ids {
		leaseID := leasePrefix + id
		claim, ok, err := core.ReadLeaseClaimWithPresence(leaseID)
		if err != nil {
			return nil, err
		}
		if !ok || claim.Provider != providerName {
			continue
		}
		_, binding, err := bindingForClaim(claim)
		if err != nil {
			return nil, err
		}
		var item sandboxIdentity
		if err := core.WithLeaseClaimUnchanged(leaseID, claim, func() error {
			var err error
			item, err = cli.verifyBinding(ctx, binding)
			return err
		}); err != nil {
			return nil, err
		}

		servers = append(servers, Server{
			Provider: providerName,
			CloudID:  id,
			Name:     id,
			Status:   item.State,
			Labels: map[string]string{
				"provider": providerName,
				"lease":    claim.LeaseID,
				"slug":     claim.Slug,
				"target":   targetLinux,
				"state":    item.State,
			},
		})
	}
	return servers, nil
}

func (b *tensorlakeBackend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	servers, err := b.List(ctx, ListRequest{})
	if err != nil {
		return DoctorResult{}, err
	}
	return cliDoctorResult(providerName, len(servers), "unchecked"), nil
}

func (b *tensorlakeBackend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	cli, err := newTensorlakeCLI(b.cfg, b.rt)
	if err != nil {
		return StatusView{}, err
	}
	claim, err := b.resolveLease(ctx, cli, req.ID, "", false)
	if err != nil {
		return StatusView{}, err
	}
	leaseID, sandboxID, slug := claim.LeaseID, claim.CloudID, claim.Slug
	_, binding, err := bindingForClaim(claim)
	if err != nil {
		return StatusView{}, err
	}
	deadline := b.now().Add(req.WaitTimeout)
	if req.WaitTimeout <= 0 {
		deadline = b.now().Add(5 * time.Minute)
	}
	var lastDescribeErr error
	for {
		var item sandboxIdentity
		describeErr := core.WithLeaseClaimUnchanged(leaseID, claim, func() error {
			var err error
			item, err = cli.verifyBinding(ctx, binding)
			return err
		})
		state := item.State
		if describeErr != nil {
			if !req.Wait {
				return StatusView{}, describeErr
			}
			lastDescribeErr = describeErr
		} else {
			lastDescribeErr = nil
		}
		ready := describeErr == nil && isReadyState(state)
		view := StatusView{
			ID:       leaseID,
			Slug:     slug,
			Provider: providerName,
			TargetOS: targetLinux,
			State:    state,
			ServerID: sandboxID,
			Network:  NetworkPublic,
			Ready:    ready,
			Labels: map[string]string{
				"provider": providerName,
				"lease":    leaseID,
				"state":    state,
			},
		}
		if !req.Wait || view.Ready {
			return view, nil
		}
		if b.now().After(deadline) {
			err := exit(5, "timed out waiting for tensorlake sandbox %s to become ready", sandboxID)
			if lastDescribeErr != nil {
				return StatusView{}, errors.Join(err, fmt.Errorf("last tensorlake describe failed: %w", lastDescribeErr))
			}
			return StatusView{}, err
		}
		select {
		case <-ctx.Done():
			err := ctx.Err()
			if lastDescribeErr != nil {
				return StatusView{}, errors.Join(err, fmt.Errorf("last tensorlake describe failed: %w", lastDescribeErr))
			}
			return StatusView{}, err
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *tensorlakeBackend) Stop(ctx context.Context, req StopRequest) error {
	cli, err := newTensorlakeCLI(b.cfg, b.rt)
	if err != nil {
		return err
	}
	claim, err := b.resolveLease(ctx, cli, req.ID, "", false)
	if err != nil {
		return err
	}
	if err := cli.removeBoundClaim(ctx, claim); err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s\n", claim.LeaseID, claim.CloudID)
	return nil
}

func (b *tensorlakeBackend) createSandbox(ctx context.Context, cli *tensorlakeCLI, repo Repo, reclaim bool, requestedSlug string) (core.LeaseClaim, string, error) {
	if strings.TrimSpace(repo.Root) == "" {
		return core.LeaseClaim{}, "", exit(2, "Tensorlake acquisition requires a repository root for durable ownership")
	}
	scope, err := cli.observeScope(ctx)
	if err != nil {
		return core.LeaseClaim{}, "", err
	}
	name := newSandboxName(repo)
	id, err := cli.createSandbox(ctx, name)
	if err != nil {
		return core.LeaseClaim{}, "", fmt.Errorf("%w; inspect Tensorlake sandbox name=%s before manual cleanup of an uncertain creation", err, name)
	}
	leaseID := leasePrefix + id
	retained := func(err error) (core.LeaseClaim, string, error) {
		return core.LeaseClaim{}, "", fmt.Errorf("%w; retained Tensorlake sandbox=%s lease=%s for manual inspection", err, id, leaseID)
	}
	item, err := cli.inspectIdentity(ctx, id)
	if err != nil {
		return retained(err)
	}
	if item.Name != name || item.State == "terminated" {
		return retained(exit(2, "Tensorlake creation returned an unexpected sandbox identity or state"))
	}
	binding := sandboxBinding{id, item.Namespace, scope}
	rollback := func(cause error) (core.LeaseClaim, string, error) {
		cleanupErr := core.CleanupLeaseClaimIfUnchangedAfter(leaseID, core.LeaseClaim{}, false, func() error {
			return cli.terminateBound(context.Background(), binding)
		})
		if cleanupErr != nil {
			return retained(errors.Join(cause, cleanupErr))
		}
		return core.LeaseClaim{}, "", cause
	}
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return rollback(err)
	}
	server := Server{Provider: providerName, CloudID: id, Name: name, Status: item.State, Labels: map[string]string{"provider": providerName, "lease": leaseID, "slug": slug, "target": targetLinux, "tensorlake_namespace": item.Namespace}}
	claim, err := core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurableAfter(leaseID, slug, b.cfg, scope, server, core.SSHTarget{}, repo.Root, b.cfg.IdleTimeout, reclaim, core.LeaseClaim{}, false, func() error {
		item, err := cli.verifyBinding(ctx, binding)
		if err == nil && item.State == "terminated" {
			err = exit(2, "Tensorlake sandbox terminated before ownership publication")
		}
		return err
	})
	if err != nil {
		return rollback(err)
	}
	return claim, name, nil
}

func (b *tensorlakeBackend) resolveLease(ctx context.Context, cli *tensorlakeCLI, id, repoRoot string, reclaim bool) (core.LeaseClaim, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return core.LeaseClaim{}, exit(2, "provider=tensorlake requires a Crabbox-created sandbox slug or lease id")
	}
	strict := strings.HasPrefix(id, leasePrefix) || isLikelySandboxID(id)
	if isLikelySandboxID(id) {
		id = leasePrefix + id
	}
	claim, ok, exact, err := core.ResolveLeaseClaimForProviderWithExact(id, providerName)
	if err != nil {
		return core.LeaseClaim{}, err
	}
	if !ok || (strict || exact) && (!exact || claim.LeaseID != id) {
		return core.LeaseClaim{}, exit(4, "tensorlake sandbox %q is not exactly claimed by Crabbox", id)
	}
	_, binding, err := bindingForClaim(claim)
	if err != nil {
		return core.LeaseClaim{}, err
	}
	if repoRoot != "" {
		server := Server{Provider: providerName, CloudID: claim.CloudID, Labels: shared.CloneLabels(claim.Labels)}
		return core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurableAfter(claim.LeaseID, claim.Slug, b.cfg, claim.ProviderScope, server, core.SSHTarget{}, repoRoot, timeoutOrDefault(b.cfg.IdleTimeout, time.Duration(claim.IdleTimeoutSeconds)*time.Second), reclaim, claim, true, func() error {
			item, err := cli.verifyBinding(ctx, binding)
			if err == nil && item.State == "terminated" {
				err = exit(2, "Tensorlake sandbox has terminated; create a new lease")
			}
			return err
		})
	}
	return claim, nil
}

func timeoutOrDefault(primary, fallback time.Duration) time.Duration {
	if primary > 0 {
		return primary
	}
	return fallback
}

func newSandboxName(repo Repo) string {
	base := normalizeLeaseSlug(repo.Name)
	if base == "" {
		base = "crabbox"
	}
	base = strings.TrimPrefix(base, strings.TrimSuffix(namePrefix, "-")+"-")
	maxBase := maxSandboxNameLen - len(namePrefix) - 1 - sandboxNameSuffixLen
	if maxBase < 1 {
		maxBase = 1
	}
	if len(base) > maxBase {
		base = strings.Trim(base[:maxBase], "-")
	}
	if base == "" {
		base = "crabbox"
	}
	return namePrefix + base + "-" + randomSuffix()
}

func isReadyState(state string) bool {
	switch strings.TrimSpace(strings.ToLower(state)) {
	case "running", "ready", "started", "active":
		return true
	default:
		return false
	}
}

func randomSuffix() string {
	return shared.RandomSuffix()
}

func buildCommand(command []string, shellMode bool) ([]string, error) {
	if len(command) == 0 {
		return nil, errors.New("missing command")
	}
	if shellMode {
		return []string{"bash", "-lc", strings.Join(command, " ")}, nil
	}
	if shouldUseShell(command) || leadingEnvAssignment(command) {
		return []string{"bash", "-lc", shellScriptFromArgv(command)}, nil
	}
	return command, nil
}

func leadingEnvAssignment(command []string) bool {
	return len(command) > 1 && strings.Contains(command[0], "=") && !strings.HasPrefix(command[0], "-")
}

// tensorlakeWorkdir returns the configured absolute workspace path inside the
// sandbox, validating that it isn't relative or empty.
func tensorlakeWorkdir(cfg Config) (string, error) {
	workdir := strings.TrimSpace(cfg.Tensorlake.Workdir)
	if workdir == "" {
		workdir = "/workspace/crabbox"
	}
	clean := path.Clean(workdir)
	if !strings.HasPrefix(clean, "/") {
		return "", exit(2, "tensorlake workdir %q must be an absolute path", workdir)
	}
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var", "/workspace":
		return "", exit(2, "tensorlake workdir %q is too broad; choose a dedicated subdirectory", clean)
	}
	return clean, nil
}

func (b *tensorlakeBackend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}
