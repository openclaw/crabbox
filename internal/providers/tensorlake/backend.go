package tensorlake

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
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
	leaseID, sandboxID, name, slug, err := b.createSandbox(ctx, cli, req.Repo, req.Reclaim)
	if err != nil {
		return err
	}
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

func (b *tensorlakeBackend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if err := rejectIncompatibleSyncOptions(req); err != nil {
		return RunResult{}, err
	}
	workdir, err := tensorlakeWorkdir(b.cfg)
	if err != nil {
		return RunResult{}, err
	}
	started := b.now()
	cli, err := newTensorlakeCLI(b.cfg, b.rt)
	if err != nil {
		return RunResult{}, err
	}
	leaseID, sandboxID, slug := "", "", ""
	acquired := false
	if req.ID == "" {
		var name string
		leaseID, sandboxID, name, slug, err = b.createSandbox(ctx, cli, req.Repo, req.Reclaim)
		if err != nil {
			return RunResult{}, err
		}
		fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s sandbox=%s name=%s\n", leaseID, slug, providerName, sandboxID, name)
		acquired = true
	} else {
		leaseID, sandboxID, err = resolveLeaseID(req.ID, req.Repo.Root, req.Reclaim, b.cfg.IdleTimeout)
		if err != nil {
			return RunResult{}, err
		}
		slug = newLeaseSlug(leaseID)
	}
	shouldStop := acquired && !req.Keep
	if shouldStop {
		defer func() {
			if !shouldStop {
				return
			}
			if termErr := cli.terminate(context.Background(), sandboxID); termErr != nil {
				fmt.Fprintf(b.rt.Stderr, "warning: tensorlake terminate failed for %s: %v\n", sandboxID, termErr)
				return
			}
			removeLeaseClaim(leaseID)
		}()
	}
	fmt.Fprintf(b.rt.Stderr, "provider=%s lease=%s sandbox=%s workdir=%s\n", providerName, leaseID, sandboxID, workdir)

	syncDuration := time.Duration(0)
	syncPhases := []timingPhase{{Name: "sync", Skipped: true, Reason: "--no-sync"}}
	if !req.NoSync {
		var err error
		syncPhases, syncDuration, err = b.syncWorkspace(ctx, cli, sandboxID, req, workdir)
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
		envPath, cleanup, err := b.uploadEnvProfile(ctx, cli, sandboxID, req.Env)
		if err != nil {
			return RunResult{}, err
		}
		defer cleanup()
		command = wrapCommandWithEnvProfile(command, envPath)
	}
	commandStart := b.now()
	exitCode, runErr := cli.execStream(ctx, sandboxID, workdir, command, b.rt.Stdout, b.rt.Stderr)
	commandDuration := b.now().Sub(commandStart)
	result := RunResult{
		ExitCode:      exitCode,
		Command:       commandDuration,
		Total:         b.now().Sub(started),
		SyncDelegated: true,
	}
	if req.NoSync {
		fmt.Fprintf(b.rt.Stderr, "tensorlake run summary sync_skipped=true command=%s total=%s exit=%d\n",
			result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), exitCode)
	} else {
		fmt.Fprintf(b.rt.Stderr, "tensorlake run summary sync=%s command=%s total=%s exit=%d\n",
			syncDuration.Round(time.Millisecond), result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), exitCode)
	}
	if req.TimingJSON {
		if err := writeTimingJSON(b.rt.Stderr, timingReport{
			Provider:      providerName,
			LeaseID:       leaseID,
			SyncDelegated: true,
			SyncMs:        syncDuration.Milliseconds(),
			SyncPhases:    syncPhases,
			SyncSkipped:   req.NoSync,
			CommandMs:     result.Command.Milliseconds(),
			TotalMs:       result.Total.Milliseconds(),
			ExitCode:      exitCode,
		}); err != nil {
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
		claim, ok, err := resolveLeaseClaim(leaseID)
		if err != nil || !ok || claim.Provider != providerName {
			continue
		}
		servers = append(servers, Server{
			Provider: providerName,
			CloudID:  id,
			Name:     id,
			Status:   "running",
			Labels: map[string]string{
				"provider": providerName,
				"lease":    claim.LeaseID,
				"slug":     claim.Slug,
				"target":   targetLinux,
				"state":    "running",
			},
		})
	}
	return servers, nil
}

func (b *tensorlakeBackend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	cli, err := newTensorlakeCLI(b.cfg, b.rt)
	if err != nil {
		return StatusView{}, err
	}
	leaseID, sandboxID, err := resolveLeaseID(req.ID, "", false, 0)
	if err != nil {
		return StatusView{}, err
	}
	deadline := b.now().Add(req.WaitTimeout)
	if req.WaitTimeout <= 0 {
		deadline = b.now().Add(5 * time.Minute)
	}
	for {
		out, describeErr := cli.describe(ctx, sandboxID)
		state := parseDescribeState(out)
		ready := describeErr == nil && isReadyState(state)
		view := StatusView{
			ID:       leaseID,
			Slug:     newLeaseSlug(leaseID),
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
			return StatusView{}, exit(5, "timed out waiting for tensorlake sandbox %s to become ready", sandboxID)
		}
		select {
		case <-ctx.Done():
			return StatusView{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *tensorlakeBackend) Stop(ctx context.Context, req StopRequest) error {
	cli, err := newTensorlakeCLI(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, sandboxID, err := resolveLeaseID(req.ID, "", false, 0)
	if err != nil {
		return err
	}
	if err := cli.terminate(ctx, sandboxID); err != nil {
		return err
	}
	removeLeaseClaim(leaseID)
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s\n", leaseID, sandboxID)
	return nil
}

// createSandbox returns (leaseID, sandboxID, name, slug, err). The Tensorlake
// CLI returns the assigned sandbox ID on stdout; that ID is the canonical
// identifier we key the local claim by. The Crabbox-prefixed name is set on
// the Tensorlake side for human-readable `tensorlake sbx ls` output but is
// not used for subsequent API calls.
func (b *tensorlakeBackend) createSandbox(ctx context.Context, cli *tensorlakeCLI, repo Repo, reclaim bool) (string, string, string, string, error) {
	name := newSandboxName(repo)
	sandboxID, err := cli.createSandbox(ctx, name)
	if err != nil {
		return "", "", "", "", err
	}
	leaseID := leasePrefix + sandboxID
	slug := newLeaseSlug(leaseID)
	if err := claimLeaseForRepoProvider(leaseID, slug, providerName, repo.Root, b.cfg.IdleTimeout, reclaim); err != nil {
		_ = cli.terminate(context.Background(), sandboxID)
		return "", "", "", "", err
	}
	return leaseID, sandboxID, name, slug, nil
}

// resolveLeaseID resolves a user-supplied identifier (slug, lease ID, or
// raw Tensorlake sandbox ID) to a (leaseID, sandboxID) pair. Resolution is
// strict: only locally-claimed Crabbox sandboxes are accepted, mirroring
// islo. Raw IDs are accepted only when a matching `tlsbx_<id>` claim exists.
func resolveLeaseID(id, repoRoot string, reclaim bool, idleTimeout time.Duration) (string, string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", "", exit(2, "provider=tensorlake requires a Crabbox-created sandbox slug or lease id")
	}
	probes := []string{id}
	if !strings.HasPrefix(id, leasePrefix) {
		probes = append(probes, leasePrefix+id)
	}
	for _, probe := range probes {
		claim, ok, err := resolveLeaseClaim(probe)
		if err != nil {
			return "", "", err
		}
		if !ok || claim.Provider != providerName {
			continue
		}
		if repoRoot != "" {
			if err := claimLeaseForRepoProvider(claim.LeaseID, claim.Slug, providerName, repoRoot,
				timeoutOrDefault(idleTimeout, time.Duration(claim.IdleTimeoutSeconds)*time.Second), reclaim); err != nil {
				return "", "", err
			}
		}
		return claim.LeaseID, strings.TrimPrefix(claim.LeaseID, leasePrefix), nil
	}
	return "", "", exit(4, "tensorlake sandbox %q is not claimed by Crabbox; use a Crabbox slug or %s<sandbox-id>", id, leasePrefix)
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

// parseDescribeState extracts the Status field from `tensorlake sbx describe`
// stdout. The CLI prints key/value lines like "Status: running"; an empty
// string is returned when no Status line is present.
func parseDescribeState(out string) string {
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(key), "status") {
			return strings.ToLower(strings.TrimSpace(value))
		}
	}
	return ""
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
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())[:6]
	}
	return hex.EncodeToString(b[:])
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
	if !strings.HasPrefix(workdir, "/") {
		return "", exit(2, "tensorlake workdir %q must be an absolute path", workdir)
	}
	return workdir, nil
}

func (b *tensorlakeBackend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}
