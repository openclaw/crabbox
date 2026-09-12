package freestyle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

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

func RegisterFreestyleProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterFreestyleConfigFlags(fs, defaults.Freestyle)
}

func ApplyFreestyleProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(core.FreestyleConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := v.Apply(&cfg.Freestyle, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, freestyleProvider)
	return err
}

func NewFreestyleBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	cfg.Provider = freestyleProvider
	return &freestyleBackend{spec: spec, cfg: cfg, rt: rt}
}

type freestyleBackend struct {
	spec core.ProviderSpec
	cfg  core.Config
	rt   core.Runtime
}

func (b *freestyleBackend) Spec() core.ProviderSpec { return b.spec }

func (b *freestyleBackend) Warmup(ctx context.Context, req core.WarmupRequest) error {
	if req.ActionsRunner {
		return core.Exit(2, "--actions-runner is not supported for provider=%s", freestyleProvider)
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
		return core.WriteTimingJSON(b.rt.Stderr, core.TimingReport{
			Provider: freestyleProvider,
			LeaseID:  leaseID,
			Slug:     slug,
			TotalMs:  total.Milliseconds(),
			ExitCode: 0,
		})
	}
	return nil
}

func (b *freestyleBackend) Run(ctx context.Context, req core.RunRequest) (core.RunResult, error) {
	workspace, workspaceErr := freestyleWorkspacePath(b.cfg)
	var client freestyleAPI
	var leaseID, name, slug string
	session := func() shared.DelegatedSandbox {
		return shared.DelegatedSandbox{LeaseID: leaseID, Slug: slug, CleanupCommand: freestyleCleanupCommand(leaseID)}
	}
	return shared.RunDelegatedSandbox(ctx, req, shared.DelegatedSandboxLifecycle{
		Provider: freestyleProvider, Runtime: b.rt, Workdir: workspace,
		IdleTimeout: b.cfg.IdleTimeout, TTL: b.cfg.TTL, CleanupTimeout: freestyleCleanupTimeout,
		Preflight: func(context.Context) error {
			if err := core.RejectDelegatedSyncOptionsForSpec(b.spec, req); err != nil {
				return err
			}
			if workspaceErr != nil {
				return workspaceErr
			}
			if !req.SyncOnly && (len(req.Command) == 0 || (len(req.Command) == 1 && strings.TrimSpace(req.Command[0]) == "")) {
				return core.Exit(2, "missing command")
			}
			var err error
			client, err = newFreestyleClient(b.cfg, b.rt)
			return err
		},
		PrepareArchive: func(ctx context.Context) (*core.PreparedArchive, error) { return b.prepareArchive(ctx, req) },
		Acquire: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			var err error
			leaseID, name, slug, err = b.createSandbox(ctx, client, req.Repo, req.Reclaim, req.RequestedSlug)
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=freestyle sandbox=%s\n", leaseID, slug, name)
			return session(), nil
		},
		Resolve: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			var err error
			leaseID, name, err = b.resolveLeaseID(ctx, client, req.ID, req.Repo.Root, req.Reclaim)
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			slug = freestyleClaimSlug(leaseID)
			return session(), nil
		},
		Setup: func(context.Context) error {
			fmt.Fprintf(b.rt.Stderr, "provider=freestyle lease=%s sandbox=%s\n", leaseID, name)
			return nil
		},
		Sync: func(ctx context.Context, archive *core.PreparedArchive) ([]core.TimingPhase, time.Duration, error) {
			return b.syncWorkspace(ctx, client, name, req, archive)
		},
		NoSync: func(ctx context.Context) error { return b.prepareWorkspace(ctx, client, name, workspace) },
		Command: func(context.Context) (shared.DelegatedSandboxCommand, error) {
			if req.EnvSummary {
				core.PrintEnvForwardingSummary(b.rt.Stderr, freestyleProvider, "forwarded", req.Options.EnvAllow, req.Env)
			}
			return shared.DelegatedSandboxCommand{Run: func(ctx context.Context, stdout, stderr io.Writer) (int, error) {
				return b.exec(ctx, client, name, workspace, req.Command, req.ShellMode, req.Env, stdout, stderr)
			}}, nil
		},
		Cleanup: func(ctx context.Context) error {
			if err := client.DeleteVM(ctx, name); err != nil {
				return err
			}
			core.RemoveLeaseClaim(leaseID)
			return nil
		},
	})
}

func (b *freestyleBackend) List(ctx context.Context, _ core.ListRequest) ([]core.LeaseView, error) {
	client, err := newFreestyleClient(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	vms, err := client.ListVMs(ctx)
	if err != nil {
		return nil, freestyleError("list vms", err)
	}
	servers := make([]core.Server, 0, len(vms))
	for _, vm := range vms {
		if !isCrabboxFreestyleSandboxName(vm.Name) {
			continue
		}
		servers = append(servers, freestyleVMToServer(vm))
	}
	return servers, nil
}

func (b *freestyleBackend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	servers, err := b.List(ctx, core.ListRequest{})
	if err != nil {
		return core.DoctorResult{}, err
	}
	return core.InventoryDoctorResult(freestyleProvider, len(servers)), nil
}

func (b *freestyleBackend) Status(ctx context.Context, req core.StatusRequest) (core.StatusView, error) {
	client, err := newFreestyleClient(b.cfg, b.rt)
	if err != nil {
		return core.StatusView{}, err
	}
	leaseID, id, err := b.resolveLeaseID(ctx, client, req.ID, "", false)
	if err != nil {
		return core.StatusView{}, err
	}
	deadline := b.now().Add(req.WaitTimeout)
	if req.WaitTimeout <= 0 {
		deadline = b.now().Add(5 * time.Minute)
	}
	for {
		vm, err := client.GetVM(ctx, id)
		if err != nil {
			return core.StatusView{}, freestyleError("get vm", err)
		}
		view := freestyleStatusView(leaseID, vm)
		if !req.Wait || view.Ready {
			return view, nil
		}
		if freestyleStatusTerminal(view.State) {
			return core.StatusView{}, core.Exit(5, "freestyle vm %s entered terminal state %q before becoming ready", id, view.State)
		}
		if b.now().After(deadline) {
			return core.StatusView{}, core.Exit(5, "timed out waiting for vm %s to become ready", id)
		}
		select {
		case <-ctx.Done():
			return core.StatusView{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *freestyleBackend) Stop(ctx context.Context, req core.StopRequest) error {
	client, err := newFreestyleClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, id, err := b.resolveLeaseID(ctx, client, req.ID, "", false)
	if err != nil {
		return err
	}
	if err := requireFreestyleLeaseClaim(leaseID); err != nil {
		return err
	}
	if err := client.DeleteVM(ctx, id); err != nil {
		return freestyleError("delete vm", err)
	}
	core.RemoveLeaseClaim(leaseID)
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s\n", leaseID, id)
	return nil
}

func (b *freestyleBackend) createSandbox(ctx context.Context, client freestyleAPI, repo core.Repo, reclaim bool, requestedSlug string) (string, string, string, error) {
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
		return "", "", "", core.Exit(5, "freestyle create vm returned no id")
	}
	leaseID := freestyleLeasePrefix + vm.ID
	slug, err := core.AllocateClaimLeaseSlug(leaseID, requestedSlug)
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
		leaseID := freestyleLeasePrefix + vmID
		leakErr := fmt.Errorf("cleanup freestyle vm %s after acquire failure: %w; first run `%s`, then run `%s` to retry cleanup", vmID, cleanupErr, freestyleAdoptCommand(leaseID), freestyleCleanupCommand(leaseID))
		if b.rt.Stderr != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: %v\n", leakErr)
		}
		return errors.Join(cause, leakErr)
	}
	return cause
}

func freestyleAdoptCommand(leaseID string) string {
	return fmt.Sprintf("crabbox run --provider %s --id %s --reclaim --no-sync -- true", freestyleProvider, core.ShellQuote(leaseID))
}

func deleteFreestyleVMForCleanup(client freestyleAPI, vmID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), freestyleCleanupTimeout)
	defer cancel()
	return client.DeleteVM(ctx, vmID)
}

func freestyleCleanupCommand(leaseID string) string {
	return fmt.Sprintf("crabbox stop --provider %s --id %s", freestyleProvider, core.ShellQuote(leaseID))
}

func (b *freestyleBackend) exec(ctx context.Context, client freestyleAPI, id, workdir string, command []string, shellMode bool, env map[string]string, stdout, stderr io.Writer) (int, error) {
	execCommand := freestyleExecCommand(command, shellMode)
	parts := make([]string, 0, 3)
	if workdir != "" {
		parts = append(parts, "cd "+core.ShellQuote(workdir))
	}
	if envCommand := freestyleEnvExportCommand(env); envCommand != "" {
		parts = append(parts, envCommand)
	}
	parts = append(parts, execCommand)
	fullCommand := strings.Join(parts, " && ")
	return client.Exec(ctx, id, "bash -lc "+core.ShellQuote(fullCommand), stdout, stderr)
}

func freestyleEnvExportCommand(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	keys := make([]string, 0, len(env))
	for name := range env {
		if core.ValidShellEnvName(name) {
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
		b.WriteString(core.ShellQuote(env[name]))
	}
	return b.String()
}

func freestyleExecCommand(command []string, shellMode bool) string {
	if len(command) == 0 {
		return ""
	}
	if shellMode {
		return strings.Join(command, " ")
	}
	if len(command) == 1 && core.ShouldUseShell(command) {
		return command[0]
	}
	if core.ShouldUseShell(command) || core.LeadingEnvAssignment(command) {
		return core.ShellScriptFromArgv(command)
	}
	return strings.Join(core.ShellWords(command), " ")
}

func (b *freestyleBackend) resolveLeaseID(ctx context.Context, client freestyleAPI, id, repoRoot string, reclaim bool) (string, string, error) {
	if id == "" {
		return "", "", core.Exit(2, "provider=freestyle requires a Crabbox-created vm name, lease id, or slug")
	}
	if claim, ok, err := core.ResolveLeaseClaim(id); err != nil {
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
			return "", "", core.Exit(4, "freestyle vm %q is not claimed by Crabbox", id)
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
		normalizedID := core.NormalizeLeaseSlug(id)
		for _, candidate := range vms {
			if !isCrabboxFreestyleSandboxName(candidate.Name) {
				continue
			}
			candidateLeaseID := freestyleLeasePrefix + candidate.ID
			if candidate.ID != id && candidate.Name != id && core.NewLeaseSlug(candidateLeaseID) != normalizedID {
				continue
			}
			if vmID != "" {
				return "", "", core.Exit(4, "freestyle identifier %q is ambiguous", id)
			}
			vm = candidate
			vmID = candidate.ID
			leaseID = candidateLeaseID
		}
		if vmID == "" {
			return "", "", core.Exit(4, "freestyle vm %q is not claimed by Crabbox; use an id, name, slug, or claimed lease from `crabbox list --provider freestyle`", id)
		}
	}
	if !isCrabboxFreestyleSandboxName(vm.Name) {
		return "", "", core.Exit(4, "freestyle vm %q is not claimed by Crabbox", id)
	}
	if repoRoot != "" {
		claim, ok, err := resolveExactFreestyleLeaseClaim(leaseID)
		if err != nil {
			return "", "", err
		}
		if ok {
			if err := claimLeaseForRepoProviderPond(claim.LeaseID, claim.Slug, freestyleProvider, claim.Pond, repoRoot, time.Duration(claim.IdleTimeoutSeconds)*time.Second, reclaim); err != nil {
				return "", "", err
			}
		} else if !reclaim {
			return "", "", core.Exit(4, "freestyle vm %q has no exact local claim; use --reclaim to adopt it before reuse", id)
		} else if err := claimLeaseForRepoProviderPond(leaseID, core.NewLeaseSlug(leaseID), freestyleProvider, b.cfg.Pond, repoRoot, b.cfg.IdleTimeout, true); err != nil {
			return "", "", err
		}
	}
	return leaseID, core.Blank(vm.ID, vmID), nil
}

func resolveExactFreestyleLeaseClaim(leaseID string) (core.LeaseClaim, bool, error) {
	claim, ok, err := core.ResolveLeaseClaim(leaseID)
	if err != nil {
		return claim, ok, err
	}
	if ok && claim.Provider == freestyleProvider && claim.LeaseID == leaseID {
		return claim, true, nil
	}
	return core.LeaseClaim{}, false, nil
}

func requireFreestyleLeaseClaim(leaseID string) error {
	if _, ok, err := resolveExactFreestyleLeaseClaim(leaseID); err != nil {
		return err
	} else if !ok {
		return core.Exit(4, "freestyle lease %q has no exact local claim; adopt it with an explicit --reclaim reuse before stop", leaseID)
	}
	return nil
}

func freestyleVMToServer(vm freestyleVM) core.Server {
	leaseID := freestyleLeasePrefix + vm.ID
	labels := applyFreestyleClaimLabels(leaseID, vm)
	return core.Server{
		Provider: freestyleProvider,
		CloudID:  vm.ID,
		Name:     vm.Name,
		Status:   vm.State,
		Labels:   labels,
	}
}

func freestyleStatusView(leaseID string, vm freestyleVM) core.StatusView {
	labels := map[string]string{
		"provider": freestyleProvider,
		"lease":    leaseID,
		"slug":     core.NewLeaseSlug(leaseID),
		"state":    vm.State,
	}
	applyFreestyleClaimMetadata(labels, leaseID)
	return core.StatusView{
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
		"slug":     core.NewLeaseSlug(leaseID),
		"target":   targetLinux,
		"state":    vm.State,
	}
	applyFreestyleClaimMetadata(labels, leaseID)
	return labels
}

func applyFreestyleClaimMetadata(labels map[string]string, leaseID string) {
	claim, ok, err := core.ResolveLeaseClaim(leaseID)
	if err != nil || !ok || claim.Provider != freestyleProvider {
		return
	}
	if strings.TrimSpace(claim.Slug) != "" {
		labels["slug"] = core.NormalizeLeaseSlug(claim.Slug)
	}
	if strings.TrimSpace(claim.Pond) != "" {
		labels["pond"] = claim.Pond
	}
}

func freestyleClaimSlug(leaseID string) string {
	if claim, ok, err := core.ResolveLeaseClaim(leaseID); err == nil && ok && claim.Provider == freestyleProvider && strings.TrimSpace(claim.Slug) != "" {
		return claim.Slug
	}
	return core.NewLeaseSlug(leaseID)
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

func newFreestyleSandboxName(repo core.Repo) string {
	base := core.NormalizeLeaseSlug(repo.Name)
	if base == "" {
		base = "crabbox"
	}
	base = strings.TrimPrefix(base, freestyleNamePrefix)
	return freestyleNamePrefix + base + "-" + freestyleRandomSuffix()
}

func isCrabboxFreestyleSandboxName(name string) bool {
	return name == core.NormalizeLeaseSlug(name) && strings.HasPrefix(name, freestyleNamePrefix)
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
