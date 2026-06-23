package freestyle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type ProviderSpec = core.ProviderSpec
type Runtime = core.Runtime
type Backend = core.Backend
type FreestyleConfig = core.FreestyleConfig
type SyncConfig = core.SyncConfig
type WarmupRequest = core.WarmupRequest
type RunRequest = core.RunRequest
type RunResult = core.RunResult
type ListRequest = core.ListRequest
type LeaseView = core.LeaseView
type StatusRequest = core.StatusRequest
type StatusView = core.StatusView
type StopRequest = core.StopRequest
type RunSessionHandle = core.RunSessionHandle
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

var freestyleCleanupTimeout = 30 * time.Second

type freestyleFlagValues struct {
	APIURL   *string
	Workdir  *string
	VCPUs    *int
	MemoryGB *int
}

func RegisterFreestyleProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return freestyleFlagValues{
		APIURL:   fs.String("freestyle-api-url", defaults.Freestyle.APIURL, "Freestyle API URL"),
		Workdir:  fs.String("freestyle-workdir", defaults.Freestyle.Workdir, "Freestyle sandbox workdir"),
		VCPUs:    fs.Int("freestyle-vcpus", defaults.Freestyle.VCPUs, "Freestyle sandbox vCPUs (power of two; omit for plan default)"),
		MemoryGB: fs.Int("freestyle-memory-gb", defaults.Freestyle.MemoryGB, "Freestyle sandbox memory in GiB (power of two; omit for plan default)"),
	}
}

func ApplyFreestyleProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(freestyleFlagValues)
	if !ok {
		return nil
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
	if flagWasSet(fs, "freestyle-memory-gb") {
		cfg.Freestyle.MemoryGB = *v.MemoryGB
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
	if req.ActionsRunner {
		return exit(2, "--actions-runner is not supported for provider=%s", freestyleProvider)
	}
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

func (b *freestyleBackend) Run(ctx context.Context, req RunRequest) (result RunResult, retErr error) {
	if err := delegatedSyncOptionsError(b.spec, req); err != nil {
		return RunResult{}, err
	}
	workspace, err := freestyleWorkspacePath(b.cfg)
	if err != nil {
		return RunResult{}, err
	}
	if !req.SyncOnly && (len(req.Command) == 0 || (len(req.Command) == 1 && strings.TrimSpace(req.Command[0]) == "")) {
		return RunResult{}, exit(2, "missing command")
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
		leaseID, name, err = b.resolveLeaseID(ctx, client, req.ID, req.Repo.Root, req.Reclaim)
		if err != nil {
			return RunResult{}, err
		}
		slug = freestyleClaimSlug(leaseID)
	}
	shouldStop := acquired && !req.Keep
	cleanedUp := false
	session := &RunSessionHandle{
		Provider:       freestyleProvider,
		LeaseID:        leaseID,
		Slug:           slug,
		Reused:         !acquired,
		Kept:           !shouldStop,
		CleanupCommand: freestyleCleanupCommand(leaseID),
	}
	finishResult := func(result RunResult) RunResult {
		if result.Provider == "" {
			result.Provider = freestyleProvider
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
	cleanupFreestyle := func() error {
		if !shouldStop {
			return nil
		}
		if err := deleteFreestyleVMForCleanup(client, name); err != nil {
			shouldStop = false
			return err
		}
		removeLeaseClaim(leaseID)
		cleanedUp = true
		shouldStop = false
		return nil
	}
	if shouldStop {
		defer func() {
			if err := cleanupFreestyle(); err != nil {
				fmt.Fprintf(b.rt.Stderr, "warning: freestyle stop failed for %s: %v\n", name, err)
			}
		}()
	}
	fmt.Fprintf(b.rt.Stderr, "provider=freestyle lease=%s sandbox=%s\n", leaseID, name)
	syncDuration := time.Duration(0)
	syncPhases := []timingPhase{{Name: "sync", Skipped: true, Reason: "--no-sync"}}
	if !req.NoSync {
		var err error
		syncPhases, syncDuration, err = b.syncWorkspace(ctx, client, name, req)
		if err != nil {
			handleDelegatedRunFailure(b.rt.Stderr, req, freestyleProvider, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
			return RunResult{}, err
		}
		fmt.Fprintf(b.rt.Stderr, "sync complete in %s\n", syncDuration.Round(time.Millisecond))
	} else if err := b.prepareWorkspace(ctx, client, name, workspace, false); err != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, freestyleProvider, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return RunResult{}, err
	}
	if req.SyncOnly {
		result := RunResult{
			Total:         b.now().Sub(started),
			SyncDelegated: true,
		}
		fmt.Fprintf(b.rt.Stdout, "synced %s\n", workspace)
		if req.TimingJSON {
			err := writeTimingJSON(b.rt.Stderr, timingReportWithRunResult(timingReport{
				Provider:      freestyleProvider,
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
	if req.EnvSummary {
		printEnvForwardingSummary(b.rt.Stderr, freestyleProvider, "forwarded", req.Options.EnvAllow, req.Env)
	}
	commandStart := b.now()
	exitCode, runErr := b.exec(ctx, client, name, workspace, req.Command, req.ShellMode, req.Env)
	commandDuration := b.now().Sub(commandStart)
	result = RunResult{
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
		if err := writeTimingJSON(b.rt.Stderr, timingReportWithRunResult(timingReport{
			Provider:      freestyleProvider,
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
		}, result, runErr)); err != nil {
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

func (b *freestyleBackend) List(ctx context.Context, _ ListRequest) ([]LeaseView, error) {
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
	leaseID, id, err := b.resolveLeaseID(ctx, client, req.ID, "", false)
	if err != nil {
		return statusView{}, err
	}
	deadline := b.now().Add(req.WaitTimeout)
	if req.WaitTimeout <= 0 {
		deadline = b.now().Add(5 * time.Minute)
	}
	for {
		vm, err := client.GetVM(ctx, id)
		if err != nil {
			return statusView{}, freestyleError("get vm", err)
		}
		view := freestyleStatusView(leaseID, vm)
		if !req.Wait || view.Ready {
			return view, nil
		}
		if freestyleStatusTerminal(view.State) {
			return statusView{}, exit(5, "freestyle vm %s entered terminal state %q before becoming ready", id, view.State)
		}
		if b.now().After(deadline) {
			return statusView{}, exit(5, "timed out waiting for vm %s to become ready", id)
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
	leaseID, id, err := b.resolveLeaseID(ctx, client, req.ID, "", false)
	if err != nil {
		return err
	}
	if err := client.DeleteVM(ctx, id); err != nil {
		return freestyleError("delete vm", err)
	}
	removeLeaseClaim(leaseID)
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s\n", leaseID, id)
	return nil
}

func (b *freestyleBackend) createSandbox(ctx context.Context, client freestyleAPI, repo Repo, reclaim bool, requestedSlug string) (string, string, string, error) {
	if _, err := freestyleRelativeWorkdir(b.cfg); err != nil {
		return "", "", "", err
	}
	name := newFreestyleSandboxName(repo)
	// Only send sizing when explicitly configured; otherwise omit so Freestyle
	// applies the plan defaults. Sending custom sizing on a plan that does not
	// allow it fails with CUSTOM_SIZING_NOT_ALLOWED.
	create := freestyleCreateVMRequest{
		Name:  name,
		Ports: []freestylePortMapping{},
	}
	if b.cfg.Freestyle.VCPUs > 0 || b.cfg.Freestyle.MemoryGB > 0 {
		create.Template = &freestyleCreateVMTemplate{
			VcpuCount: b.cfg.Freestyle.VCPUs,
			MemSizeGb: b.cfg.Freestyle.MemoryGB,
		}
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
		return "", "", "", b.rollbackCreatedVM(client, vm.ID, err)
	}
	if err := claimLeaseForRepoProviderPond(leaseID, slug, freestyleProvider, b.cfg.Pond, repo.Root, b.cfg.IdleTimeout, reclaim); err != nil {
		return "", "", "", b.rollbackCreatedVM(client, vm.ID, err)
	}
	return leaseID, vm.ID, slug, nil
}

func (b *freestyleBackend) rollbackCreatedVM(client freestyleAPI, vmID string, cause error) error {
	if cleanupErr := deleteFreestyleVMForCleanup(client, vmID); cleanupErr != nil {
		leakErr := fmt.Errorf("cleanup freestyle vm %s after acquire failure: %w; run `crabbox stop --provider freestyle --id %s%s` to retry cleanup", vmID, cleanupErr, freestyleLeasePrefix, vmID)
		if b.rt.Stderr != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: %v\n", leakErr)
		}
		return errors.Join(cause, leakErr)
	}
	return cause
}

func deleteFreestyleVMForCleanup(client freestyleAPI, vmID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), freestyleCleanupTimeout)
	defer cancel()
	return client.DeleteVM(ctx, vmID)
}

func freestyleCleanupCommand(leaseID string) string {
	return fmt.Sprintf("crabbox stop --provider %s --id %s", freestyleProvider, shellQuote(leaseID))
}

func (b *freestyleBackend) exec(ctx context.Context, client freestyleAPI, id, workdir string, command []string, shellMode bool, env map[string]string) (int, error) {
	execCommand := freestyleExecCommand(command, shellMode)
	parts := make([]string, 0, 3)
	if workdir != "" {
		parts = append(parts, "cd "+shellQuote(workdir))
	}
	if envCommand := freestyleEnvExportCommand(env); envCommand != "" {
		parts = append(parts, envCommand)
	}
	parts = append(parts, execCommand)
	fullCommand := strings.Join(parts, " && ")
	return client.Exec(ctx, id, "bash -lc "+shellQuote(fullCommand), b.rt.Stdout, b.rt.Stderr)
}

func freestyleEnvExportCommand(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	keys := make([]string, 0, len(env))
	for name := range env {
		if validFreestyleEnvName(name) {
			keys = append(keys, name)
		}
	}
	if len(keys) == 0 {
		return ""
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("export")
	for _, name := range keys {
		b.WriteByte(' ')
		b.WriteString(name)
		b.WriteByte('=')
		b.WriteString(shellQuote(env[name]))
	}
	return b.String()
}

var freestyleEnvNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validFreestyleEnvName(name string) bool {
	return freestyleEnvNameRE.MatchString(name)
}

func freestyleExecCommand(command []string, shellMode bool) string {
	if len(command) == 0 {
		return ""
	}
	if shellMode {
		return strings.Join(command, " ")
	}
	if len(command) == 1 && shouldUseShell(command) {
		return command[0]
	}
	if shouldUseShell(command) || leadingEnvAssignment(command) {
		return shellScriptFromArgv(command)
	}
	return strings.Join(shellWords(command), " ")
}

func (b *freestyleBackend) resolveLeaseID(ctx context.Context, client freestyleAPI, id, repoRoot string, reclaim bool) (string, string, error) {
	if id == "" {
		return "", "", exit(2, "provider=freestyle requires a Crabbox-created vm name, lease id, or slug")
	}
	if claim, ok, err := resolveLeaseClaim(id); err != nil {
		return "", "", err
	} else if ok && claim.Provider == freestyleProvider {
		if repoRoot != "" {
			if err := claimLeaseForRepoProviderPond(claim.LeaseID, claim.Slug, freestyleProvider, claim.Pond, repoRoot, time.Duration(claim.IdleTimeoutSeconds)*time.Second, reclaim); err != nil {
				return "", "", err
			}
		}
		return claim.LeaseID, strings.TrimPrefix(claim.LeaseID, freestyleLeasePrefix), nil
	}
	leaseID := id
	vmID := ""
	var vm freestyleVM
	if strings.HasPrefix(leaseID, freestyleLeasePrefix) {
		vmID = strings.TrimPrefix(leaseID, freestyleLeasePrefix)
		if vmID == "" {
			return "", "", exit(4, "freestyle vm %q is not claimed by Crabbox", id)
		}
		var err error
		vm, err = client.GetVM(ctx, vmID)
		if err != nil {
			return "", "", freestyleError("get vm", err)
		}
	} else {
		vms, err := client.ListVMs(ctx)
		if err != nil {
			return "", "", freestyleError("list vms", err)
		}
		normalizedID := normalizeLeaseSlug(id)
		for _, candidate := range vms {
			if !isCrabboxFreestyleSandboxName(candidate.Name) {
				continue
			}
			candidateLeaseID := freestyleLeasePrefix + candidate.ID
			if candidate.ID != id && candidate.Name != id && newLeaseSlug(candidateLeaseID) != normalizedID {
				continue
			}
			if vmID != "" {
				return "", "", exit(4, "freestyle identifier %q is ambiguous", id)
			}
			vm = candidate
			vmID = candidate.ID
			leaseID = candidateLeaseID
		}
		if vmID == "" {
			return "", "", exit(4, "freestyle vm %q is not claimed by Crabbox; use an id, name, slug, or claimed lease from `crabbox list --provider freestyle`", id)
		}
	}
	if !isCrabboxFreestyleSandboxName(vm.Name) {
		return "", "", exit(4, "freestyle vm %q is not claimed by Crabbox", id)
	}
	if repoRoot != "" {
		if err := claimLeaseForRepoProviderPond(leaseID, newLeaseSlug(leaseID), freestyleProvider, b.cfg.Pond, repoRoot, b.cfg.IdleTimeout, reclaim); err != nil {
			return "", "", err
		}
	}
	return leaseID, blank(vm.ID, vmID), nil
}

func freestyleVMToServer(vm freestyleVM) Server {
	leaseID := freestyleLeasePrefix + vm.ID
	labels := applyFreestyleClaimLabels(leaseID, vm)
	return Server{
		Provider: freestyleProvider,
		CloudID:  vm.ID,
		Name:     vm.Name,
		Status:   vm.State,
		Labels:   labels,
	}
}

func freestyleStatusView(leaseID string, vm freestyleVM) statusView {
	labels := map[string]string{
		"provider": freestyleProvider,
		"lease":    leaseID,
		"slug":     newLeaseSlug(leaseID),
		"state":    vm.State,
	}
	applyFreestyleClaimMetadata(labels, leaseID)
	return statusView{
		ID:         leaseID,
		Slug:       labels["slug"],
		Provider:   freestyleProvider,
		TargetOS:   targetLinux,
		State:      vm.State,
		ServerID:   vm.ID,
		ServerType: vm.Name,
		Network:    NetworkPublic,
		Ready:      freestyleStatusReady(vm.State),
		Labels:     labels,
	}
}

func applyFreestyleClaimLabels(leaseID string, vm freestyleVM) map[string]string {
	labels := map[string]string{
		"provider": freestyleProvider,
		"lease":    leaseID,
		"slug":     newLeaseSlug(leaseID),
		"target":   targetLinux,
		"state":    vm.State,
	}
	applyFreestyleClaimMetadata(labels, leaseID)
	return labels
}

func applyFreestyleClaimMetadata(labels map[string]string, leaseID string) {
	claim, ok, err := resolveLeaseClaim(leaseID)
	if err != nil || !ok || claim.Provider != freestyleProvider {
		return
	}
	if strings.TrimSpace(claim.Slug) != "" {
		labels["slug"] = normalizeLeaseSlug(claim.Slug)
	}
	if strings.TrimSpace(claim.Pond) != "" {
		labels["pond"] = claim.Pond
	}
}

func freestyleClaimSlug(leaseID string) string {
	if claim, ok, err := resolveLeaseClaim(leaseID); err == nil && ok && claim.Provider == freestyleProvider && strings.TrimSpace(claim.Slug) != "" {
		return claim.Slug
	}
	return newLeaseSlug(leaseID)
}

func freestyleStatusReady(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), "running")
}

func freestyleStatusTerminal(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "stopped", "lost":
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
	base = strings.TrimPrefix(base, freestyleNamePrefix)
	return freestyleNamePrefix + base + "-" + freestyleRandomSuffix()
}

func isCrabboxFreestyleSandboxName(name string) bool {
	return name == normalizeLeaseSlug(name) && strings.HasPrefix(name, freestyleNamePrefix)
}

func freestyleRandomSuffix() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())[:6]
	}
	return hex.EncodeToString(b[:])
}

func (b *freestyleBackend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}
