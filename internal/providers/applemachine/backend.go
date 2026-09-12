package applemachine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type backend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

var hostGOOS, hostGOARCH = runtime.GOOS, runtime.GOARCH

func newBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	return &backend{spec: spec, cfg: cfg, rt: rt}
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	if err := requireHost(); err != nil {
		return DoctorResult{}, err
	}
	result, err := b.rt.Exec.Run(ctx, LocalCommandRequest{Name: core.Blank(b.cfg.AppleContainer.CLIPath, "container"), Args: []string{"--version"}})
	if err != nil {
		return DoctorResult{}, exit(3, "Apple container CLI unavailable: %s", failureDetail(result, err))
	}
	machines, err := b.listMachines(ctx)
	if err != nil {
		return DoctorResult{}, err
	}
	return DoctorResult{Provider: providerName, Message: fmt.Sprintf("cli=ready control_plane=local inventory=ready leases=%d version=%s", len(machines), strings.TrimSpace(result.Stdout))}, nil
}

func (b *backend) Warmup(ctx context.Context, req WarmupRequest) error {
	started := time.Now()
	claim, err := b.createLease(ctx, req.Repo, req.Reclaim, req.RequestedSlug)
	if err != nil {
		return err
	}
	leaseID, slug, name := claim.LeaseID, claim.Slug, claim.CloudID
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s machine=%s\n", leaseID, slug, providerName, name)
	total := time.Since(started)
	return shared.CompleteWarmup(b.rt, req.TimingJSON, shared.WarmupCompletion{
		Provider: providerName,
		LeaseID:  leaseID,
		Slug:     slug,
		Total:    total,
	})
}

func (b *backend) Run(ctx context.Context, req RunRequest) (result RunResult, retErr error) {
	if err := requireHost(); err != nil {
		return RunResult{}, err
	}
	if req.SyncOnly || req.ApplyLocalPatch || req.FreshPR.Number > 0 {
		return RunResult{}, exit(2, "provider=%s uses the host home mount; sync-only, patch upload, and fresh-PR preparation are not supported", providerName)
	}
	if err := validateRepoMount(req.Repo.Root); err != nil {
		return RunResult{}, err
	}
	started := time.Now()
	var claim core.LeaseClaim
	acquired := false
	var err error
	if strings.TrimSpace(req.ID) == "" {
		claim, err = b.createLease(ctx, req.Repo, req.Reclaim, req.RequestedSlug)
		acquired = err == nil
	} else {
		claim, err = b.resolveLease(ctx, req.ID, req.Repo.Root, req.Reclaim)
	}
	if err != nil {
		return RunResult{}, err
	}
	leaseID, slug, name := claim.LeaseID, claim.Slug, claim.CloudID
	result = RunResult{
		Provider: providerName, LeaseID: leaseID, Slug: slug, SyncDelegated: true,
		Session: &RunSessionHandle{
			Provider: providerName, LeaseID: leaseID, Slug: slug, Reused: !acquired, Kept: true,
			CleanupCommand: appleMachineCleanupCommand(leaseID),
		},
	}
	defer func() {
		result, retErr = shared.PinDelegatedRunFailure(result, retErr)
		shouldStop := acquired && !req.Keep
		if shouldStop && retErr != nil && req.KeepOnFailure {
			shouldStop = false
			fmt.Fprintf(b.rt.Stderr, "kept failed apple-machine lease=%s slug=%s\n", leaseID, slug)
		}
		if shouldStop {
			if cleanupErr := b.removeBoundLease(context.Background(), claim); cleanupErr != nil {
				result, retErr = shared.AppendDelegatedRunFailure(result, retErr, fmt.Errorf("apple-machine cleanup failed: %w", cleanupErr), 1)
			} else {
				result.Session.Kept = false
			}
		}
		result.Total = time.Since(started)
		result = core.FinalizeRunResult(result, retErr)
		if req.TimingJSON {
			timingErr := writeTimingJSON(b.rt.Stderr, timingReportWithRunResult(timingReport{
				Provider: providerName, LeaseID: leaseID, Slug: slug, SyncDelegated: true, SyncSkipped: true,
				CommandMs: result.Command.Milliseconds(), TotalMs: result.Total.Milliseconds(),
				ExitCode: result.ExitCode, Label: strings.TrimSpace(req.Label),
			}, result, retErr))
			result, retErr = shared.AppendDelegatedRunFailure(result, retErr, timingErr, 1)
		}
	}()
	args := []string{"machine", "run", "--name", name}
	if root := strings.TrimSpace(req.Repo.Root); root != "" {
		args = append(args, "--cwd", root)
	}
	envFile, cleanup, err := writeEnvFile(req.Env, req.Options.EnvAllow)
	if err != nil {
		return result, err
	}
	if cleanup != nil {
		defer cleanup()
		args = append(args, "--env-file", envFile)
	}
	command := req.Command
	if req.ShellMode {
		command = []string{"/bin/sh", "-lc", shellScriptFromArgv(req.Command)}
	}
	if len(command) == 0 {
		return result, exit(2, "provider=%s requires a command", providerName)
	}
	args = append(args, command...)
	commandStarted := time.Now()
	req.Observation.Phase(core.RunPhaseCommand)
	commandBackend := *b
	commandBackend.rt.Stdout, commandBackend.rt.Stderr = req.Observation.CommandWriters(b.rt.Stdout, b.rt.Stderr, core.RunOutputProvider)
	native, runErr := commandBackend.command(ctx, args, req.Repo.Root)
	result.Command = time.Since(commandStarted)
	result.CommandText = strings.Join(req.Command, " ")
	classificationErr := runErr
	if core.IsPlainLocalCommandExit(native, runErr) {
		classificationErr = nil
	}
	outcome := shared.FinalizeDelegatedCommandOutcome(native.ExitCode, classificationErr)
	result.ExitCode, result.Status, result.ErrorKind = outcome.ExitCode, outcome.Status, outcome.ErrorKind
	if runErr != nil {
		return result, shared.ExitErrorWithCause(result.ExitCode, fmt.Sprintf("apple-machine command failed: %s", failureDetail(native, runErr)), runErr)
	}
	if result.ExitCode != 0 {
		return result, exit(result.ExitCode, "apple-machine command exited %d", result.ExitCode)
	}
	return result, nil
}

func appleMachineCleanupCommand(leaseID string) string {
	return "crabbox stop --provider " + providerName + " --id " + shellQuote(leaseID)
}

func writeEnvFile(env map[string]string, explicitlyAllowed []string) (string, func(), error) {
	explicit := map[string]bool{}
	for _, key := range explicitlyAllowed {
		explicit[strings.ToUpper(strings.TrimSpace(key))] = true
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		if machineOwnedEnv(key) {
			if explicit[strings.ToUpper(strings.TrimSpace(key))] {
				return "", nil, exit(2, "provider=%s cannot forward host-owned environment variable %s; set it inside the machine command instead", providerName, key)
			}
			continue
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return "", nil, nil
	}
	sort.Strings(keys)
	file, err := os.CreateTemp("", "crabbox-apple-machine-env-*.env")
	if err != nil {
		return "", nil, exit(2, "create apple-machine env file: %v", err)
	}
	cleanup := func() { _ = os.Remove(file.Name()) }
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		cleanup()
		return "", nil, exit(2, "secure apple-machine env file: %v", err)
	}
	for _, key := range keys {
		if strings.ContainsAny(key, "=\r\n") || strings.ContainsAny(env[key], "\r\n") {
			file.Close()
			cleanup()
			return "", nil, exit(2, "apple-machine environment values cannot contain newlines")
		}
		if _, err := fmt.Fprintf(file, "%s=%s\n", key, env[key]); err != nil {
			file.Close()
			cleanup()
			return "", nil, exit(2, "write apple-machine env file: %v", err)
		}
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", nil, exit(2, "close apple-machine env file: %v", err)
	}
	return file.Name(), cleanup, nil
}

func machineOwnedEnv(key string) bool {
	switch strings.ToUpper(strings.TrimSpace(key)) {
	case "HOME", "LOGNAME", "OLDPWD", "PATH", "PWD", "SHELL", "TMPDIR", "USER":
		return true
	default:
		return false
	}
}

func (b *backend) List(ctx context.Context, _ ListRequest) ([]LeaseView, error) {
	machines, err := b.listMachines(ctx)
	if err != nil {
		return nil, err
	}
	claims, err := coreClaims()
	if err != nil {
		return nil, err
	}
	byName := map[string]core.LeaseClaim{}
	for _, claim := range claims {
		if claim.Provider == providerName {
			byName[machineName(claim.LeaseID)] = claim
		}
	}
	views := make([]LeaseView, 0)
	for _, item := range machines {
		claim, ok := byName[item.ID]
		if !ok {
			continue
		}
		if err := core.WithLeaseClaimUnchangedContext(ctx, claim.LeaseID, claim, func() error {
			_, err := b.verifyMachineIdentity(ctx, claim)
			return err
		}); err != nil {
			return nil, err
		}
		views = append(views, machineServer(item, claim.LeaseID, claim.Slug, b.cfg))
	}
	return views, nil
}

func (b *backend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	claim, err := b.resolveLease(ctx, req.ID, "", false)
	if err != nil {
		return StatusView{}, err
	}
	var item machine
	err = core.WithLeaseClaimUnchangedContext(ctx, claim.LeaseID, claim, func() error {
		var err error
		item, err = b.verifyMachineIdentity(ctx, claim)
		return err
	})
	if err != nil {
		return StatusView{}, err
	}
	server := machineServer(item, claim.LeaseID, claim.Slug, b.cfg)
	return StatusView{ID: claim.LeaseID, Slug: claim.Slug, Provider: providerName, TargetOS: targetLinux, State: server.Status, ServerID: claim.CloudID, ServerType: server.ServerType.Name, Ready: machineReady(item.Status), Labels: server.Labels}, nil
}

func (b *backend) Stop(ctx context.Context, req StopRequest) error {
	claim, err := b.resolveLease(ctx, req.ID, "", false)
	if err != nil {
		return err
	}
	if err := b.removeBoundLease(ctx, claim); err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stderr, "released lease=%s machine=%s\n", claim.LeaseID, claim.CloudID)
	return nil
}

func (b *backend) createLease(ctx context.Context, repo Repo, reclaim bool, requestedSlug string) (core.LeaseClaim, error) {
	if err := requireHost(); err != nil {
		return core.LeaseClaim{}, err
	}
	if err := validateRepoMount(repo.Root); err != nil {
		return core.LeaseClaim{}, err
	}
	if strings.TrimSpace(repo.Root) == "" {
		return core.LeaseClaim{}, exit(2, "apple-machine acquisition requires a repository root for durable ownership")
	}
	leaseID := core.NewLeaseID()
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return core.LeaseClaim{}, err
	}
	root, err := b.storageRoot(ctx)
	if err != nil {
		return core.LeaseClaim{}, err
	}
	name := machineName(leaseID)
	if err := b.createMachine(ctx, name); err != nil {
		return core.LeaseClaim{}, fmt.Errorf("%w; creation may be incomplete: inspect container machine inspect %s before manual cleanup", err, shellQuote(name))
	}
	retained := func(err error) (core.LeaseClaim, error) {
		return core.LeaseClaim{}, fmt.Errorf("%w; retained machine=%s lease=%s: inspect container machine inspect %s before manual cleanup", err, name, leaseID, shellQuote(name))
	}
	retainedAfterRollback := func(primary, cleanup error) (core.LeaseClaim, error) {
		code := core.ExitCodeForError(primary, 1)
		_, combined := retained(errors.Join(primary, cleanup))
		return core.LeaseClaim{}, shared.ExitErrorWithCause(code, combined.Error(), combined)
	}
	currentRoot, err := b.storageRoot(ctx)
	if err != nil {
		return retained(err)
	}
	if currentRoot != root {
		return retained(fmt.Errorf("Apple container daemon storage changed during creation"))
	}
	if _, err := b.inspectMachine(ctx, name); err != nil {
		return retained(err)
	}
	identity, err := createMachineIdentity(root, name)
	if err != nil {
		return retained(err)
	}
	server := machineServer(machine{ID: name}, leaseID, slug, b.cfg)
	server.ImmutableID = identity
	server.Labels["apple_machine_storage"] = root
	binding := core.LeaseClaim{Provider: providerName, ProviderScope: root, LeaseID: leaseID, Slug: slug, CloudID: name, CloudImmutableID: identity, Labels: server.Labels}
	claim, err := core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurableAfterContext(ctx, leaseID, slug, b.cfg, root, server, core.SSHTarget{}, repo.Root, b.cfg.IdleTimeout, reclaim, core.LeaseClaim{}, false, func() error {
		_, err := b.verifyMachineIdentity(ctx, binding)
		return err
	})
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), machineCleanupTimeout)
		cleanupErr := core.CleanupLeaseClaimIfUnchangedAfterContext(cleanupCtx, leaseID, core.LeaseClaim{}, false, func() error {
			return b.deleteBoundMachine(cleanupCtx, binding)
		})
		cancel()
		if cleanupErr != nil {
			return retainedAfterRollback(err, cleanupErr)
		}
		return core.LeaseClaim{}, err
	}
	readyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err = core.WithLeaseClaimUnchangedContext(readyCtx, leaseID, claim, func() error {
		if _, err := b.verifyMachineIdentity(readyCtx, claim); err != nil {
			return err
		}
		return b.waitMachineReady(readyCtx, claim)
	})
	if err != nil {
		if cleanupErr := b.removeBoundLease(context.WithoutCancel(ctx), claim); cleanupErr != nil {
			return retainedAfterRollback(err, cleanupErr)
		}
		return core.LeaseClaim{}, err
	}
	return claim, nil
}

func (b *backend) waitMachineReady(ctx context.Context, claim core.LeaseClaim) error {
	type observation struct {
		err error
	}
	_, err := shared.Poll(ctx, 0, 500*time.Millisecond, shared.SleepContext,
		func(ctx context.Context) (observation, error) {
			if _, err := b.verifyMachineIdentity(ctx, claim); err != nil {
				return observation{}, err
			}
			_, err := b.control(ctx, []string{"machine", "run", "--name", claim.CloudID, ":"})
			return observation{err: err}, nil
		},
		func(_ context.Context, current observation, identityErr error) (bool, error) {
			if identityErr != nil {
				return false, identityErr
			}
			return current.err == nil, nil
		}, nil)
	if err != nil {
		return fmt.Errorf("Apple container machine %q did not become ready: %w", claim.CloudID, err)
	}
	return nil
}

func (b *backend) resolveLease(ctx context.Context, identifier, repoRoot string, reclaim bool) (core.LeaseClaim, error) {
	claim, ok, exact, err := core.ResolveLeaseClaimForProviderWithExact(identifier, providerName)
	if err != nil {
		return core.LeaseClaim{}, err
	}
	if (exact || core.IsCanonicalLeaseID(identifier)) && (!exact || !ok || claim.LeaseID != identifier) {
		return core.LeaseClaim{}, shared.ErrStrictClaimMismatch
	}
	if !ok {
		return core.LeaseClaim{}, exit(4, "apple-machine lease %q was not found", identifier)
	}
	if _, err := machineClaimBinding(claim); err != nil {
		return core.LeaseClaim{}, err
	}
	if repoRoot != "" {
		server := machineServer(machine{ID: claim.CloudID}, claim.LeaseID, claim.Slug, b.cfg)
		server.ImmutableID = claim.CloudImmutableID
		server.Labels = shared.CloneLabels(claim.Labels)
		return core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurableAfterContext(ctx, claim.LeaseID, claim.Slug, b.cfg, claim.ProviderScope, server, core.SSHTarget{}, repoRoot, time.Duration(claim.IdleTimeoutSeconds)*time.Second, reclaim, claim, true, func() error {
			_, err := b.verifyMachineIdentity(ctx, claim)
			return err
		})
	}
	return claim, nil
}

func machineName(leaseID string) string {
	return "crabbox-" + strings.TrimPrefix(leaseID, "cbx_")
}

func machineServer(item machine, leaseID, slug string, cfg Config) Server {
	labels := map[string]string{"crabbox": "true", "provider": providerName, "lease": leaseID, "slug": slug, "target": targetLinux}
	server := Server{Provider: providerName, CloudID: item.ID, Name: item.ID, Status: item.Status, Labels: labels}
	server.ServerType.Name = core.Blank(cfg.AppleContainer.Image, "ubuntu:26.04")
	return server
}

func machineReady(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "running", "ready":
		return true
	default:
		return false
	}
}

func requireHost() error {
	if hostGOOS != "darwin" || hostGOARCH != "arm64" {
		return exit(2, "provider=%s requires Apple silicon macOS", providerName)
	}
	return nil
}

func validateRepoMount(root string) error {
	if strings.TrimSpace(root) == "" {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return exit(2, "resolve home directory: %v", err)
	}
	rel, err := filepath.Rel(home, root)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return exit(2, "provider=%s requires the repository under %s because container machine shares the host home directory", providerName, home)
	}
	return nil
}

func coreClaims() ([]core.LeaseClaim, error) { return core.ListLeaseClaims() }
