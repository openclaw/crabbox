package freestyle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type ProviderSpec = core.ProviderSpec
type Runtime = core.Runtime
type Backend = core.Backend
type FreestyleConfig = core.FreestyleConfig
type WarmupRequest = core.WarmupRequest
type RunRequest = core.RunRequest
type RunResult = core.RunResult
type ListRequest = core.ListRequest
type LeaseView = core.LeaseView
type StatusRequest = core.StatusRequest
type StatusView = core.StatusView
type StopRequest = core.StopRequest
type Server = core.Server
type Repo = core.Repo
type ExitError = core.ExitError
type timingReport = core.TimingReport
type timingPhase = core.TimingPhase

const (
	targetLinux   = core.TargetLinux
	NetworkPublic = core.NetworkPublic
)

const (
	freestyleProvider    = "freestyle"
	freestyleLeasePrefix = "fsb_"
	freestyleNamePrefix  = "crabbox-"
)

type freestyleFlagValues struct {
	APIKey   *string
	APIURL   *string
	Workdir  *string
	VCPUs    *int
	MemoryMB *int
}

func RegisterFreestyleProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return freestyleFlagValues{
		APIKey:   fs.String("freestyle-api-key", defaults.Freestyle.APIKey, "Freestyle API key"),
		APIURL:   fs.String("freestyle-api-url", defaults.Freestyle.APIURL, "Freestyle API base URL"),
		Workdir:  fs.String("freestyle-workdir", defaults.Freestyle.Workdir, "Freestyle sandbox working directory under /workspace"),
		VCPUs:    fs.Int("freestyle-vcpus", defaults.Freestyle.VCPUs, "Freestyle sandbox vCPUs"),
		MemoryMB: fs.Int("freestyle-memory-mb", defaults.Freestyle.MemoryMB, "Freestyle sandbox memory in MB"),
	}
}

func ApplyFreestyleProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(freestyleFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "freestyle-api-key") {
		cfg.Freestyle.APIKey = *v.APIKey
	}
	if flagWasSet(fs, "freestyle-api-url") {
		cfg.Freestyle.APIURL = *v.APIURL
	}
	if flagWasSet(fs, "freestyle-workdir") {
		cfg.Freestyle.Workdir = *v.Workdir
	}
	if flagWasSet(fs, "freestyle-vcpus") {
		cfg.Freestyle.VCPUs = *v.VCPUs
	}
	if flagWasSet(fs, "freestyle-memory-mb") {
		cfg.Freestyle.MemoryMB = *v.MemoryMB
	}
	return nil
}

func NewFreestyleBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = freestyleProvider
	return &freestyleBackend{spec: spec, cfg: cfg, rt: rt}
}

type freestyleBackend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func (b *freestyleBackend) Spec() ProviderSpec { return b.spec }

func (b *freestyleBackend) Warmup(ctx context.Context, req WarmupRequest) error {
	started := b.now()
	client, err := newFreestyleClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, name, slug, err := b.createSandbox(ctx, client, req.Repo, req.Reclaim, req.RequestedSlug)
	if err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=freestyle sandbox=%s\n", leaseID, slug, name)
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: freestyle warmup keeps the sandbox until explicit stop\n")
	}
	total := b.now().Sub(started)
	fmt.Fprintf(b.rt.Stdout, "warmup complete total=%s\n", total.Round(time.Millisecond))
	if req.TimingJSON {
		return writeTimingJSON(b.rt.Stderr, timingReport{
			Provider: freestyleProvider,
			LeaseID:  leaseID,
			Slug:     slug,
			TotalMs:  total.Milliseconds(),
			ExitCode: 0,
		})
	}
	return nil
}

func (b *freestyleBackend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if err := rejectFreestyleSyncOptions(req); err != nil {
		return RunResult{}, err
	}
	workspace, err := freestyleWorkspacePath(b.cfg)
	if err != nil {
		return RunResult{}, err
	}
	started := b.now()
	client, err := newFreestyleClient(b.cfg, b.rt)
	if err != nil {
		return RunResult{}, err
	}
	leaseID, name, slug := "", "", ""
	acquired := false
	if req.ID == "" {
		leaseID, name, slug, err = b.createSandbox(ctx, client, req.Repo, req.Reclaim, req.RequestedSlug)
		if err != nil {
			return RunResult{}, err
		}
		fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=freestyle sandbox=%s\n", leaseID, slug, name)
		acquired = true
	} else {
		leaseID, name, err = resolveFreestyleLeaseID(req.ID, req.Repo.Root, req.Reclaim)
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
			if err := client.DeleteVM(context.Background(), name); err != nil {
				fmt.Fprintf(b.rt.Stderr, "warning: freestyle stop failed for %s: %v\n", name, err)
				return
			}
			removeLeaseClaim(leaseID)
		}()
	}
	fmt.Fprintf(b.rt.Stderr, "provider=freestyle lease=%s sandbox=%s\n", leaseID, name)
	syncDuration := time.Duration(0)
	syncPhases := []timingPhase{{Name: "sync", Skipped: true, Reason: "--no-sync"}}
	if !req.NoSync {
		var err error
		syncPhases, syncDuration, err = b.syncWorkspace(ctx, client, name, req)
		if err != nil {
			return RunResult{}, err
		}
		fmt.Fprintf(b.rt.Stderr, "sync complete in %s\n", syncDuration.Round(time.Millisecond))
	} else if err := b.prepareWorkspace(ctx, client, name, workspace); err != nil {
		return RunResult{}, err
	}
	commandStart := b.now()
	exitCode, runErr := b.exec(ctx, client, name, workspace, req.Command, req.ShellMode, req.Env)
	commandDuration := b.now().Sub(commandStart)
	result := RunResult{
		ExitCode:      exitCode,
		Command:       commandDuration,
		Total:         b.now().Sub(started),
		SyncDelegated: true,
	}
	if req.NoSync {
		fmt.Fprintf(b.rt.Stderr, "freestyle run summary sync_skipped=true command=%s total=%s exit=%d\n", result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), exitCode)
	} else {
		fmt.Fprintf(b.rt.Stderr, "freestyle run summary sync=%s command=%s total=%s exit=%d\n", syncDuration.Round(time.Millisecond), result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), exitCode)
	}
	if req.TimingJSON {
		if err := writeTimingJSON(b.rt.Stderr, timingReport{
			Provider:      freestyleProvider,
			LeaseID:       leaseID,
			SyncDelegated: true,
			SyncMs:        syncDuration.Milliseconds(),
			SyncPhases:    syncPhases,
			SyncSkipped:   req.NoSync,
			CommandMs:     result.Command.Milliseconds(),
			TotalMs:       result.Total.Milliseconds(),
			ExitCode:      exitCode,
			Label:         strings.TrimSpace(req.Label),
		}); err != nil {
			return result, err
		}
	}
	if runErr != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, freestyleProvider, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return result, ExitError{Code: 1, Message: fmt.Sprintf("freestyle run failed: %v", runErr)}
	}
	if exitCode != 0 {
		handleDelegatedRunFailure(b.rt.Stderr, req, freestyleProvider, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return result, ExitError{Code: exitCode, Message: fmt.Sprintf("freestyle run exited %d", exitCode)}
	}
	return result, nil
}

func (b *freestyleBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	client, err := newFreestyleClient(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	vms, err := client.ListVMs(ctx)
	if err != nil {
		return nil, freestyleError("list vms", err)
	}
	servers := make([]Server, 0, len(vms))
	for _, vm := range vms {
		if !isCrabboxFreestyleSandboxName(vm.Name) {
			continue
		}
		servers = append(servers, freestyleVMToServer(vm))
	}
	return servers, nil
}

func (b *freestyleBackend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	servers, err := b.List(ctx, ListRequest{})
	if err != nil {
		return core.DoctorResult{}, err
	}
	return core.InventoryDoctorResult(freestyleProvider, len(servers)), nil
}

func (b *freestyleBackend) Status(ctx context.Context, req StatusRequest) (statusView, error) {
	client, err := newFreestyleClient(b.cfg, b.rt)
	if err != nil {
		return statusView{}, err
	}
	leaseID, name, err := resolveFreestyleLeaseID(req.ID, "", false)
	if err != nil {
		return statusView{}, err
	}
	deadline := b.now().Add(req.WaitTimeout)
	if req.WaitTimeout <= 0 {
		deadline = b.now().Add(5 * time.Minute)
	}
	for {
		vm, err := client.GetVM(ctx, name)
		if err != nil {
			return statusView{}, freestyleError("get vm", err)
		}
		view := freestyleStatusView(leaseID, vm)
		if !req.Wait || view.Ready {
			return view, nil
		}
		if b.now().After(deadline) {
			return statusView{}, exit(5, "timed out waiting for sandbox %s to become ready", name)
		}
		select {
		case <-ctx.Done():
			return statusView{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *freestyleBackend) Stop(ctx context.Context, req StopRequest) error {
	client, err := newFreestyleClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, name, err := resolveFreestyleLeaseID(req.ID, "", false)
	if err != nil {
		return err
	}
	if err := client.DeleteVM(ctx, name); err != nil {
		return freestyleError("delete vm", err)
	}
	removeLeaseClaim(leaseID)
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s\n", leaseID, name)
	return nil
}

func (b *freestyleBackend) createSandbox(ctx context.Context, client freestyleAPI, repo Repo, reclaim bool, requestedSlug string) (string, string, string, error) {
	name := newFreestyleSandboxName(repo)
	create := freestyleCreateVMRequest{Name: name}
	if b.cfg.Freestyle.VCPUs > 0 {
		create.VcpuCount = b.cfg.Freestyle.VCPUs
	}
	if b.cfg.Freestyle.MemoryMB > 0 {
		create.MemSizeMb = b.cfg.Freestyle.MemoryMB
	}
	vm, err := client.CreateVM(ctx, create)
	if err != nil {
		return "", "", "", freestyleError("create vm", err)
	}
	if vm.ID == "" {
		return "", "", "", exit(5, "freestyle create vm returned no id")
	}
	leaseID := freestyleLeasePrefix + vm.ID
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		_ = client.DeleteVM(context.Background(), vm.ID)
		return "", "", "", err
	}
	if err := claimLeaseForRepoProviderWithPond(leaseID, slug, freestyleProvider, b.cfg.Pond, repo.Root, b.cfg.IdleTimeout, reclaim); err != nil {
		_ = client.DeleteVM(context.Background(), vm.ID)
		return "", "", "", err
	}
	return leaseID, vm.ID, slug, nil
}

func (b *freestyleBackend) exec(ctx context.Context, client freestyleAPI, name, workdir string, command []string, shellMode bool, env map[string]string) (int, error) {
	execCommand, err := freestyleExecCommand(command, shellMode)
	if err != nil {
		return 2, err
	}
	if workdir != "" {
		execCommand = "cd " + shellQuote(workdir) + " && " + execCommand
	}
	if len(env) > 0 {
		var prefix strings.Builder
		for k, v := range env {
			fmt.Fprintf(&prefix, "%s=%s ", k, shellQuote(v))
		}
		execCommand = prefix.String() + execCommand
	}
	return client.Exec(ctx, name, execCommand, b.rt.Stdout, b.rt.Stderr)
}

func freestyleExecCommand(command []string, shellMode bool) (string, error) {
	if len(command) == 0 {
		return "", errors.New("missing command")
	}
	joined := strings.Join(command, " ")
	if shellMode {
		return joined, nil
	}
	if shouldUseShell(command) || leadingEnvAssignment(command) {
		return joined, nil
	}
	return joined, nil
}

func resolveFreestyleLeaseID(id, repoRoot string, reclaim bool) (string, string, error) {
	if id == "" {
		return "", "", exit(2, "provider=freestyle requires a Crabbox lease id or slug")
	}
	if strings.HasPrefix(id, freestyleLeasePrefix) {
		name := strings.TrimPrefix(id, freestyleLeasePrefix)
		return id, name, nil
	}
	if claim, ok, err := resolveLeaseClaim(id); err != nil {
		return "", "", err
	} else if ok && claim.Provider == freestyleProvider {
		if repoRoot != "" {
			if err := claimLeaseForRepoProvider(claim.LeaseID, claim.Slug, freestyleProvider, repoRoot, time.Duration(claim.IdleTimeoutSeconds)*time.Second, reclaim); err != nil {
				return "", "", err
			}
		}
		return claim.LeaseID, strings.TrimPrefix(claim.LeaseID, freestyleLeasePrefix), nil
	}
	return "", "", exit(4, "freestyle: unknown lease or slug %q", id)
}

func freestyleVMToServer(vm freestyleVM) Server {
	leaseID := freestyleLeasePrefix + vm.ID
	status := vm.State
	labels := map[string]string{
		"provider": freestyleProvider,
		"lease":    leaseID,
		"slug":     newLeaseSlug(leaseID),
		"target":   targetLinux,
		"state":    status,
	}
	applyFreestyleClaimLabels(labels, leaseID)
	return Server{
		Provider: freestyleProvider,
		CloudID:  vm.ID,
		Name:     blank(vm.Name, vm.ID),
		Status:   status,
		Labels:   labels,
	}
}

func freestyleStatusView(leaseID string, vm freestyleVM) statusView {
	name := strings.TrimPrefix(leaseID, freestyleLeasePrefix)
	status := ""
	if vm.Name != "" {
		name = vm.Name
		status = vm.State
	}
	labels := map[string]string{
		"provider": freestyleProvider,
		"lease":    leaseID,
		"slug":     newLeaseSlug(leaseID),
		"state":    status,
	}
	applyFreestyleClaimLabels(labels, leaseID)
	return statusView{
		ID:       leaseID,
		Slug:     labels["slug"],
		Provider: freestyleProvider,
		TargetOS: targetLinux,
		State:    status,
		ServerID: name,
		Network:  NetworkPublic,
		Ready:    freestyleStatusReady(status),
		Labels:   labels,
	}
}

func applyFreestyleClaimLabels(labels map[string]string, leaseID string) {
	claim, ok, err := resolveLeaseClaim(leaseID)
	if err != nil || !ok {
		return
	}
	if claim.Slug != "" {
		labels["slug"] = normalizeLeaseSlug(claim.Slug)
	}
	if claim.Pond != "" {
		labels["pond"] = claim.Pond
	}
}

func freestyleStatusReady(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "ready", "running", "started", "active":
		return true
	default:
		return false
	}
}

func newFreestyleSandboxName(repo Repo) string {
	base := normalizeLeaseSlug(repo.Name)
	if base == "" {
		base = "crabbox"
	}
	base = strings.TrimPrefix(base, strings.TrimSuffix(freestyleNamePrefix, "-")+"-")
	return freestyleNamePrefix + base + "-" + freestyleRandomSuffix()
}

func isCrabboxFreestyleSandboxName(name string) bool {
	return strings.HasPrefix(normalizeLeaseSlug(name), freestyleNamePrefix)
}

func freestyleRandomSuffix() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())[:6]
	}
	return hex.EncodeToString(b[:])
}

func leadingEnvAssignment(command []string) bool {
	return len(command) > 1 && strings.Contains(command[0], "=") && !strings.HasPrefix(command[0], "-")
}

func freestyleError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("freestyle %s: %w", action, err)
}

func (b *freestyleBackend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}
