package islo

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	gosdk "github.com/islo-labs/go-sdk"
	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type ProviderSpec = core.ProviderSpec
type Runtime = core.Runtime
type Backend = core.Backend
type IsloConfig = core.IsloConfig
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
	isloProvider    = "islo"
	isloLeasePrefix = "isb_"
	isloNamePrefix  = "crabbox-"
)

type isloFlagValues struct {
	BaseURL        *string
	Image          *string
	Workdir        *string
	GatewayProfile *string
	SnapshotName   *string
	VCPUs          *int
	MemoryMB       *int
	DiskGB         *int
}

func RegisterIsloProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return isloFlagValues{
		BaseURL:        fs.String("islo-base-url", defaults.Islo.BaseURL, "Islo API base URL"),
		Image:          fs.String("islo-image", defaults.Islo.Image, "Islo sandbox image"),
		Workdir:        fs.String("islo-workdir", defaults.Islo.Workdir, "Islo sandbox working directory under /workspace"),
		GatewayProfile: fs.String("islo-gateway-profile", defaults.Islo.GatewayProfile, "Islo gateway profile name or id"),
		SnapshotName:   fs.String("islo-snapshot-name", defaults.Islo.SnapshotName, "Islo snapshot name"),
		VCPUs:          fs.Int("islo-vcpus", defaults.Islo.VCPUs, "Islo sandbox vCPUs"),
		MemoryMB:       fs.Int("islo-memory-mb", defaults.Islo.MemoryMB, "Islo sandbox memory in MB"),
		DiskGB:         fs.Int("islo-disk-gb", defaults.Islo.DiskGB, "Islo sandbox disk in GB"),
	}
}

func ApplyIsloProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(isloFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "islo-base-url") {
		cfg.Islo.BaseURL = *v.BaseURL
	}
	if flagWasSet(fs, "islo-image") {
		cfg.Islo.Image = *v.Image
		core.MarkIsloImageExplicit(cfg)
	}
	if flagWasSet(fs, "islo-workdir") {
		cfg.Islo.Workdir = *v.Workdir
	}
	if flagWasSet(fs, "islo-gateway-profile") {
		cfg.Islo.GatewayProfile = *v.GatewayProfile
	}
	if flagWasSet(fs, "islo-snapshot-name") {
		cfg.Islo.SnapshotName = *v.SnapshotName
	}
	if flagWasSet(fs, "islo-vcpus") {
		cfg.Islo.VCPUs = *v.VCPUs
	}
	if flagWasSet(fs, "islo-memory-mb") {
		cfg.Islo.MemoryMB = *v.MemoryMB
	}
	if flagWasSet(fs, "islo-disk-gb") {
		cfg.Islo.DiskGB = *v.DiskGB
	}
	return nil
}

func NewIsloBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = isloProvider
	return &isloBackend{spec: spec, cfg: cfg, rt: rt}
}

type isloBackend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func (b *isloBackend) Spec() ProviderSpec { return b.spec }

func (b *isloBackend) Warmup(ctx context.Context, req WarmupRequest) error {
	started := b.now()
	client, err := newIsloClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, name, slug, err := b.createSandbox(ctx, client, req.Repo, req.Reclaim, req.RequestedSlug)
	if err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=islo sandbox=%s\n", leaseID, slug, name)
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: islo warmup keeps the sandbox until explicit stop\n")
	}
	total := b.now().Sub(started)
	fmt.Fprintf(b.rt.Stdout, "warmup complete total=%s\n", total.Round(time.Millisecond))
	if req.TimingJSON {
		return writeTimingJSON(b.rt.Stderr, timingReport{
			Provider: isloProvider,
			LeaseID:  leaseID,
			Slug:     slug,
			TotalMs:  total.Milliseconds(),
			ExitCode: 0,
		})
	}
	return nil
}

func (b *isloBackend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if err := rejectIsloSyncOptions(req); err != nil {
		return RunResult{}, err
	}
	workspace, err := isloWorkspacePath(b.cfg)
	if err != nil {
		return RunResult{}, err
	}
	started := b.now()
	client, err := newIsloClient(b.cfg, b.rt)
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
		fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=islo sandbox=%s\n", leaseID, slug, name)
		acquired = true
	} else {
		leaseID, name, slug, err = resolveIsloLeaseID(req.ID, req.Repo.Root, req.Reclaim)
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
			if err := deleteIsloSandboxForCleanup(client, name); err != nil {
				fmt.Fprintf(b.rt.Stderr, "warning: islo stop failed for %s: %v\n", name, err)
				return
			}
			removeLeaseClaim(leaseID)
		}()
	}
	result := RunResult{
		SyncDelegated: true,
		Session: &core.RunSessionHandle{
			Provider:       isloProvider,
			LeaseID:        leaseID,
			Slug:           slug,
			Reused:         !acquired,
			Kept:           !shouldStop,
			CleanupCommand: isloCleanupCommand(leaseID),
		},
	}
	finishResult := func() RunResult {
		result.Total = b.now().Sub(started)
		result.Session.Kept = !shouldStop
		return result
	}
	fmt.Fprintf(b.rt.Stderr, "provider=islo lease=%s sandbox=%s\n", leaseID, name)
	syncDuration := time.Duration(0)
	syncPhases := []timingPhase{{Name: "sync", Skipped: true, Reason: "--no-sync"}}
	if !req.NoSync {
		var err error
		syncPhases, syncDuration, err = b.syncWorkspace(ctx, client, name, req)
		if err != nil {
			return finishResult(), err
		}
		fmt.Fprintf(b.rt.Stderr, "sync complete in %s\n", syncDuration.Round(time.Millisecond))
	} else if err := b.prepareWorkspace(ctx, client, name, workspace); err != nil {
		return finishResult(), err
	}
	commandStart := b.now()
	exitCode, runErr := b.exec(ctx, client, name, workspace, req.Command, req.ShellMode, req.Env)
	commandDuration := b.now().Sub(commandStart)
	result.ExitCode = exitCode
	result.Command = commandDuration
	result.Total = b.now().Sub(started)
	if req.NoSync {
		fmt.Fprintf(b.rt.Stderr, "islo run summary sync_skipped=true command=%s total=%s exit=%d\n", result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), exitCode)
	} else {
		fmt.Fprintf(b.rt.Stderr, "islo run summary sync=%s command=%s total=%s exit=%d\n", syncDuration.Round(time.Millisecond), result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), exitCode)
	}
	if req.TimingJSON {
		if err := writeTimingJSON(b.rt.Stderr, timingReport{
			Provider:      isloProvider,
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
		handleDelegatedRunFailure(b.rt.Stderr, req, isloProvider, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		result.Session.Kept = !shouldStop
		return result, ExitError{Code: 1, Message: fmt.Sprintf("islo run failed: %v", runErr)}
	}
	if exitCode != 0 {
		handleDelegatedRunFailure(b.rt.Stderr, req, isloProvider, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		result.Session.Kept = !shouldStop
		return result, ExitError{Code: exitCode, Message: fmt.Sprintf("islo run exited %d", exitCode)}
	}
	return result, nil
}

func (b *isloBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	client, err := newIsloClient(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	sandboxes, err := client.ListSandboxes(ctx)
	if err != nil {
		return nil, isloError("list sandboxes", err)
	}
	servers := make([]Server, 0, len(sandboxes))
	for _, sandbox := range sandboxes {
		if sandbox == nil || !isCrabboxIsloSandboxName(sandbox.GetName()) {
			continue
		}
		servers = append(servers, isloSandboxToServer(sandbox))
	}
	return servers, nil
}

func (b *isloBackend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	servers, err := b.List(ctx, ListRequest{})
	if err != nil {
		return core.DoctorResult{}, err
	}
	return core.InventoryDoctorResult(isloProvider, len(servers)), nil
}

func (b *isloBackend) Status(ctx context.Context, req StatusRequest) (statusView, error) {
	client, err := newIsloClient(b.cfg, b.rt)
	if err != nil {
		return statusView{}, err
	}
	leaseID, name, _, err := resolveIsloLeaseID(req.ID, "", false)
	if err != nil {
		return statusView{}, err
	}
	deadline := b.now().Add(req.WaitTimeout)
	if req.WaitTimeout <= 0 {
		deadline = b.now().Add(5 * time.Minute)
	}
	for {
		sandbox, err := client.GetSandbox(ctx, name)
		if err != nil {
			return statusView{}, isloError("get sandbox", err)
		}
		view := isloStatusView(leaseID, sandbox)
		if !req.Wait || view.Ready {
			return view, nil
		}
		if isloStatusTerminal(view.State) {
			return statusView{}, exit(5, "sandbox %s entered terminal state %q before becoming ready", name, view.State)
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

func (b *isloBackend) Stop(ctx context.Context, req StopRequest) error {
	client, err := newIsloClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, name, _, err := resolveIsloLeaseID(req.ID, "", false)
	if err != nil {
		return err
	}
	if err := client.DeleteSandbox(ctx, name); err != nil {
		return isloError("delete sandbox", err)
	}
	removeLeaseClaim(leaseID)
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s\n", leaseID, name)
	return nil
}

func (b *isloBackend) createSandbox(ctx context.Context, client isloAPI, repo Repo, reclaim bool, requestedSlug string) (string, string, string, error) {
	workdir, err := isloRelativeWorkdir(b.cfg)
	if err != nil {
		return "", "", "", err
	}
	name := newIsloSandboxName(repo)
	create := &gosdk.SandboxCreate{Name: stringValue(name)}
	if b.cfg.Islo.Image != "" {
		create.Image = stringValue(b.cfg.Islo.Image)
	}
	create.Workdir = stringValue(workdir)
	if b.cfg.Islo.GatewayProfile != "" {
		create.GatewayProfile = stringValue(b.cfg.Islo.GatewayProfile)
	}
	if b.cfg.Islo.SnapshotName != "" {
		create.SnapshotName = stringValue(b.cfg.Islo.SnapshotName)
	}
	if b.cfg.Islo.VCPUs > 0 {
		create.Vcpus = intValue(b.cfg.Islo.VCPUs)
	}
	if b.cfg.Islo.MemoryMB > 0 {
		create.MemoryMb = intValue(b.cfg.Islo.MemoryMB)
	}
	if b.cfg.Islo.DiskGB > 0 {
		create.DiskGb = intValue(b.cfg.Islo.DiskGB)
	}
	sandbox, err := client.CreateSandbox(ctx, create)
	if err != nil {
		return "", "", "", isloError("create sandbox", err)
	}
	if sandbox == nil || sandbox.GetName() == "" {
		return "", "", "", exit(5, "islo create sandbox returned no name")
	}
	leaseID := isloLeasePrefix + sandbox.GetName()
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		if cleanupErr := deleteIsloSandboxForCleanup(client, sandbox.GetName()); cleanupErr != nil {
			return "", "", "", fmt.Errorf("%w; cleanup failed for islo sandbox %s: %v", err, sandbox.GetName(), cleanupErr)
		}
		return "", "", "", err
	}
	if err := claimLeaseForRepoProviderWithPond(leaseID, slug, isloProvider, b.cfg.Pond, repo.Root, b.cfg.IdleTimeout, reclaim); err != nil {
		if cleanupErr := deleteIsloSandboxForCleanup(client, sandbox.GetName()); cleanupErr != nil {
			return "", "", "", fmt.Errorf("%w; cleanup failed for islo sandbox %s: %v", err, sandbox.GetName(), cleanupErr)
		}
		return "", "", "", err
	}
	return leaseID, sandbox.GetName(), slug, nil
}

func deleteIsloSandboxForCleanup(client isloAPI, name string) error {
	cleanupCtx, cancel := isloCleanupContext()
	defer cancel()
	return client.DeleteSandbox(cleanupCtx, name)
}

func (b *isloBackend) exec(ctx context.Context, client isloAPI, name, workdir string, command []string, shellMode bool, env map[string]string) (int, error) {
	execCommand, err := isloExecCommand(command, shellMode)
	if err != nil {
		return 2, err
	}
	req := &gosdk.ExecRequest{Command: execCommand}
	if workdir != "" {
		req.Workdir = stringValue(workdir)
	}
	if len(env) > 0 {
		req.Env = make(map[string]*string, len(env))
		for name, value := range env {
			value := value
			req.Env[name] = &value
		}
	}
	return client.ExecStream(ctx, name, req, b.rt.Stdout, b.rt.Stderr)
}

func isloExecCommand(command []string, shellMode bool) ([]string, error) {
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

func resolveIsloLeaseID(id, repoRoot string, reclaim bool) (string, string, string, error) {
	if id == "" {
		return "", "", "", exit(2, "provider=islo requires a Crabbox-created sandbox name, lease id, or slug")
	}
	if strings.HasPrefix(id, isloLeasePrefix) {
		name := strings.TrimPrefix(id, isloLeasePrefix)
		if !isCrabboxIsloSandboxName(name) {
			return "", "", "", exit(4, "islo lease %q is not a Crabbox-owned sandbox", id)
		}
		if claim, ok, err := resolveExactIsloLeaseClaim(id); err != nil {
			return "", "", "", err
		} else if ok {
			return claim.LeaseID, name, blank(claim.Slug, newLeaseSlug(claim.LeaseID)), nil
		}
		return id, name, newLeaseSlug(id), nil
	}
	if claim, ok, err := resolveIsloClaim(id); err != nil {
		return "", "", "", err
	} else if ok {
		if repoRoot != "" {
			if err := claimLeaseForRepoProvider(claim.LeaseID, claim.Slug, isloProvider, repoRoot, time.Duration(claim.IdleTimeoutSeconds)*time.Second, reclaim); err != nil {
				return "", "", "", err
			}
		}
		return claim.LeaseID, strings.TrimPrefix(claim.LeaseID, isloLeasePrefix), blank(claim.Slug, newLeaseSlug(claim.LeaseID)), nil
	}
	if !isCrabboxIsloSandboxName(id) {
		return "", "", "", exit(4, "islo sandbox %q is not claimed by Crabbox; use a Crabbox slug or %s<crabbox-sandbox-name>", id, isloLeasePrefix)
	}
	leaseID := isloLeasePrefix + id
	return leaseID, id, newLeaseSlug(leaseID), nil
}

func resolveExactIsloLeaseClaim(leaseID string) (core.LeaseClaim, bool, error) {
	claim, ok, err := resolveLeaseClaim(leaseID)
	if err != nil {
		return claim, ok, err
	}
	if ok && claim.Provider == isloProvider && claim.LeaseID == leaseID {
		return claim, true, nil
	}
	return core.LeaseClaim{}, false, nil
}

func isloCleanupCommand(leaseID string) string {
	return fmt.Sprintf("crabbox stop --provider %s %s", isloProvider, shellQuote(leaseID))
}

func resolveIsloClaim(id string) (core.LeaseClaim, bool, error) {
	claim, ok, err := resolveLeaseClaim(id)
	if err != nil || (ok && claim.Provider == isloProvider) {
		return claim, ok, err
	}
	if strings.HasPrefix(id, isloLeasePrefix) || !isCrabboxIsloSandboxName(id) {
		return core.LeaseClaim{}, false, nil
	}
	claim, ok, err = resolveLeaseClaim(isloLeasePrefix + id)
	if err != nil {
		return claim, ok, err
	}
	if ok && claim.Provider == isloProvider && claim.LeaseID == isloLeasePrefix+id {
		return claim, true, nil
	}
	return core.LeaseClaim{}, false, nil
}

func isloSandboxToServer(sandbox *gosdk.SandboxResponse) Server {
	if sandbox == nil {
		return Server{Provider: isloProvider, Labels: map[string]string{"provider": isloProvider}}
	}
	leaseID := isloLeasePrefix + sandbox.GetName()
	labels := map[string]string{
		"provider": isloProvider,
		"lease":    leaseID,
		"slug":     newLeaseSlug(leaseID),
		"target":   targetLinux,
		"state":    sandbox.GetStatus(),
	}
	applyIsloClaimLabels(labels, leaseID)
	return Server{
		Provider: isloProvider,
		CloudID:  sandbox.GetID(),
		Name:     sandbox.GetName(),
		Status:   sandbox.GetStatus(),
		Labels:   labels,
	}
}

func isloStatusView(leaseID string, sandbox *gosdk.SandboxResponse) statusView {
	name := strings.TrimPrefix(leaseID, isloLeasePrefix)
	status := ""
	image := ""
	if sandbox != nil {
		name = sandbox.GetName()
		status = sandbox.GetStatus()
		image = sandbox.GetImage()
	}
	labels := map[string]string{
		"provider": isloProvider,
		"lease":    leaseID,
		"slug":     newLeaseSlug(leaseID),
		"state":    status,
	}
	applyIsloClaimLabels(labels, leaseID)
	return statusView{
		ID:         leaseID,
		Slug:       labels["slug"],
		Provider:   isloProvider,
		TargetOS:   targetLinux,
		State:      status,
		ServerID:   name,
		ServerType: image,
		Network:    NetworkPublic,
		Ready:      isloStatusReady(status),
		Labels:     labels,
	}
}

func applyIsloClaimLabels(labels map[string]string, leaseID string) {
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

// isloStatusReady reports whether a sandbox is ready to accept commands.
//
// The Islo API reports exactly one ready state, "running"; the full set of
// statuses it returns is starting/running/paused/stopping/stopped/failed/
// deleted/unknown. The legacy "ready"/"started"/"active" values are no longer
// emitted (a "ready" boot state is normalized to "running" server-side), so
// matching them is unnecessary and misleading.
func isloStatusReady(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), "running")
}

// isloStatusTerminal reports whether a sandbox status is a terminal state that
// will never transition to ready, so callers can fail fast instead of polling
// until a deadline. Mirrors the terminal states the Islo API can report:
// "failed", "stopped", and "deleted" are terminal, and "stopping" is an
// in-progress teardown that will not recover.
func isloStatusTerminal(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "failed", "stopped", "stopping", "deleted":
		return true
	default:
		return false
	}
}

func newIsloSandboxName(repo Repo) string {
	base := normalizeLeaseSlug(repo.Name)
	if base == "" {
		base = "crabbox"
	}
	base = strings.TrimPrefix(base, strings.TrimSuffix(isloNamePrefix, "-")+"-")
	return isloNamePrefix + base + "-" + isloRandomSuffix()
}

func isCrabboxIsloSandboxName(name string) bool {
	return strings.HasPrefix(normalizeLeaseSlug(name), isloNamePrefix)
}

func isloRandomSuffix() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())[:6]
	}
	return hex.EncodeToString(b[:])
}

func leadingEnvAssignment(command []string) bool {
	return len(command) > 1 && strings.Contains(command[0], "=") && !strings.HasPrefix(command[0], "-")
}

func stringValue(v string) *string { return &v }
func intValue(v int) *int          { return &v }

func isloError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("islo %s: %w", action, err)
}

func (b *isloBackend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}
