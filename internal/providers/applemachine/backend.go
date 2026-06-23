package applemachine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
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
	result, err := b.rt.Exec.Run(ctx, LocalCommandRequest{Name: blank(b.cfg.AppleContainer.CLIPath, "container"), Args: []string{"--version"}})
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
	leaseID, slug, name, err := b.createLease(ctx, req.Repo, req.Reclaim, req.RequestedSlug)
	if err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s machine=%s\n", leaseID, slug, providerName, name)
	total := time.Since(started)
	fmt.Fprintf(b.rt.Stdout, "warmup complete total=%s\n", total.Round(time.Millisecond))
	if req.TimingJSON {
		return writeTimingJSON(b.rt.Stderr, timingReport{Provider: providerName, LeaseID: leaseID, Slug: slug, TotalMs: total.Milliseconds(), ExitCode: 0})
	}
	return nil
}

func (b *backend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
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
	leaseID, slug, name := "", "", ""
	acquired := false
	var err error
	if strings.TrimSpace(req.ID) == "" {
		leaseID, slug, name, err = b.createLease(ctx, req.Repo, req.Reclaim, req.RequestedSlug)
		acquired = err == nil
	} else {
		leaseID, slug, name, err = b.resolveLease(req.ID, req.Repo.Root, req.Reclaim)
	}
	if err != nil {
		return RunResult{}, err
	}
	session := &RunSessionHandle{
		Provider:       providerName,
		LeaseID:        leaseID,
		Slug:           slug,
		Reused:         !acquired,
		Kept:           !acquired || req.Keep,
		CleanupCommand: appleMachineCleanupCommand(leaseID),
	}
	failed := false
	if acquired && !req.Keep {
		defer func() {
			if failed && req.KeepOnFailure {
				fmt.Fprintf(b.rt.Stderr, "kept failed apple-machine lease=%s slug=%s\n", leaseID, slug)
				session.Kept = true
				return
			}
			if err := b.removeMachine(context.Background(), name); err != nil {
				fmt.Fprintf(b.rt.Stderr, "warning: %v\n", err)
				session.Kept = true
				return
			}
			removeLeaseClaim(leaseID)
			session.Kept = false
		}()
	}
	args := []string{"machine", "run", "--name", name}
	if root := strings.TrimSpace(req.Repo.Root); root != "" {
		args = append(args, "--cwd", root)
	}
	envFile, cleanup, err := writeEnvFile(req.Env, req.Options.EnvAllow)
	if err != nil {
		return RunResult{}, err
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
		return RunResult{}, exit(2, "provider=%s requires a command", providerName)
	}
	args = append(args, command...)
	commandStarted := time.Now()
	result, runErr := b.command(ctx, args, req.Repo.Root)
	commandDuration := time.Since(commandStarted)
	out := RunResult{ExitCode: result.ExitCode, Command: commandDuration, Total: time.Since(started), SyncDelegated: true, Provider: providerName, LeaseID: leaseID, Slug: slug, CommandText: strings.Join(req.Command, " "), Session: session}
	if req.TimingJSON {
		if err := writeTimingJSON(b.rt.Stderr, timingReportWithRunResult(timingReport{Provider: providerName, LeaseID: leaseID, Slug: slug, SyncDelegated: true, SyncSkipped: true, CommandMs: out.Command.Milliseconds(), TotalMs: out.Total.Milliseconds(), ExitCode: out.ExitCode, Label: strings.TrimSpace(req.Label)}, out, runErr)); err != nil {
			return out, err
		}
	}
	if runErr != nil {
		failed = true
		return out, exit(result.ExitCode, "apple-machine command failed: %s", failureDetail(result, runErr))
	}
	return out, nil
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
	byName := map[string]claimView{}
	for _, claim := range claims {
		if claim.Provider == providerName {
			byName[machineName(claim.LeaseID)] = claimView{leaseID: claim.LeaseID, slug: claim.Slug}
		}
	}
	views := make([]LeaseView, 0)
	for _, item := range machines {
		claim, ok := byName[item.ID]
		if !ok {
			continue
		}
		views = append(views, machineServer(item, claim.leaseID, claim.slug, b.cfg))
	}
	return views, nil
}

func (b *backend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	leaseID, slug, name, err := b.resolveLease(req.ID, "", false)
	if err != nil {
		return StatusView{}, err
	}
	item, err := b.inspectMachine(ctx, name)
	if err != nil {
		return StatusView{}, err
	}
	server := machineServer(item, leaseID, slug, b.cfg)
	return StatusView{ID: leaseID, Slug: slug, Provider: providerName, TargetOS: targetLinux, State: server.Status, ServerID: name, ServerType: server.ServerType.Name, Ready: machineReady(item.Status), Labels: server.Labels}, nil
}

func (b *backend) Stop(ctx context.Context, req StopRequest) error {
	leaseID, _, name, err := b.resolveLease(req.ID, "", false)
	if err != nil {
		return err
	}
	if err := b.removeMachine(ctx, name); err != nil {
		return err
	}
	removeLeaseClaim(leaseID)
	fmt.Fprintf(b.rt.Stderr, "released lease=%s machine=%s\n", leaseID, name)
	return nil
}

func (b *backend) createLease(ctx context.Context, repo Repo, reclaim bool, requestedSlug string) (string, string, string, error) {
	if err := requireHost(); err != nil {
		return "", "", "", err
	}
	if err := validateRepoMount(repo.Root); err != nil {
		return "", "", "", err
	}
	leaseID := newLeaseID()
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return "", "", "", err
	}
	name := machineName(leaseID)
	if err := b.createMachine(ctx, name); err != nil {
		return "", "", "", err
	}
	if err := b.waitMachineReady(ctx, name); err != nil {
		_ = b.removeMachine(context.Background(), name)
		return "", "", "", err
	}
	if err := claimLease(leaseID, slug, repo.Root, b.cfg.IdleTimeout, reclaim); err != nil {
		_ = b.removeMachine(context.Background(), name)
		return "", "", "", err
	}
	return leaseID, slug, name, nil
}

func (b *backend) waitMachineReady(ctx context.Context, name string) error {
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		result, err := b.rt.Exec.Run(ctx, LocalCommandRequest{
			Name: blank(strings.TrimSpace(b.cfg.AppleContainer.CLIPath), "container"),
			Args: []string{"machine", "run", "--name", name, ":"},
		})
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return exit(5, "Apple container machine %q did not become ready: %s", name, failureDetail(result, err))
		case <-ticker.C:
		}
	}
}

func (b *backend) resolveLease(identifier, repoRoot string, reclaim bool) (string, string, string, error) {
	claim, ok, err := resolveLeaseClaim(identifier)
	if err != nil {
		return "", "", "", err
	}
	if !ok {
		return "", "", "", exit(4, "apple-machine lease %q was not found", identifier)
	}
	if repoRoot != "" {
		if err := claimLease(claim.LeaseID, claim.Slug, repoRoot, time.Duration(claim.IdleTimeoutSeconds)*time.Second, reclaim); err != nil {
			return "", "", "", err
		}
	}
	return claim.LeaseID, claim.Slug, machineName(claim.LeaseID), nil
}

func machineName(leaseID string) string {
	return "crabbox-" + strings.TrimPrefix(leaseID, "cbx_")
}

func machineServer(item machine, leaseID, slug string, cfg Config) Server {
	labels := map[string]string{"crabbox": "true", "provider": providerName, "lease": leaseID, "slug": slug, "target": targetLinux}
	server := Server{Provider: providerName, CloudID: item.ID, Name: item.ID, Status: item.Status, Labels: labels}
	server.ServerType.Name = blank(cfg.AppleContainer.Image, "ubuntu:26.04")
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

type claimView struct{ leaseID, slug string }

func coreClaims() ([]core.LeaseClaim, error) { return core.ListLeaseClaims() }
