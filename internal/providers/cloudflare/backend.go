package cloudflare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

func NewCloudflareBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	if cfg.ServerType == "" {
		cfg.ServerType = cloudflareContainerInstanceTypeForClass(cfg.Class)
	}
	if normalized, ok := normalizeCloudflareContainerInstanceType(cfg.ServerType); ok {
		cfg.ServerType = normalized
	}
	return &cloudflareBackend{spec: spec, cfg: cfg, rt: rt}
}

type cloudflareBackend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

var cloudflareDoctorTimeout = 10 * time.Second

func (b *cloudflareBackend) Spec() ProviderSpec { return b.spec }

func (b *cloudflareBackend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	client, err := newCloudflareClient(b.cfg, b.rt)
	if err != nil {
		return DoctorResult{}, err
	}
	if cloudflareDoctorTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cloudflareDoctorTimeout)
		defer cancel()
	}
	if err := client.checkAuth(ctx); err != nil {
		return DoctorResult{}, err
	}
	return DoctorResult{
		Provider: providerName,
		Message:  fmt.Sprintf("auth=ready control_plane=ready api=readiness mutation=false runner=%s type=%s runtime=ready", client.baseURL, client.instanceType),
	}, nil
}

func (b *cloudflareBackend) Warmup(ctx context.Context, req WarmupRequest) error {
	if req.ActionsRunner {
		return exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	started := b.now()
	client, err := newCloudflareClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, sandbox, slug, err := b.createSandbox(ctx, client, req.Repo, req.Reclaim, req.RequestedSlug)
	if err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s sandbox=%s\n", leaseID, slug, providerName, sandbox.ID)
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: %s warmup keeps the container until explicit stop\n", providerName)
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

func (b *cloudflareBackend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if err := rejectCloudflareSyncOptions(req); err != nil {
		return RunResult{}, err
	}
	workdir, err := cloudflareWorkdir(b.cfg)
	if err != nil {
		return RunResult{}, err
	}
	started := b.now()
	client, err := newCloudflareClient(b.cfg, b.rt)
	if err != nil {
		return RunResult{}, err
	}
	leaseID, sandboxID, slug := "", "", ""
	acquired := false
	if req.ID == "" {
		var sandbox cloudflareContainer
		leaseID, sandbox, slug, err = b.createSandbox(ctx, client, req.Repo, req.Reclaim, req.RequestedSlug)
		if err != nil {
			return RunResult{}, err
		}
		sandboxID = sandbox.ID
		fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s sandbox=%s\n", leaseID, slug, providerName, sandboxID)
		acquired = true
	} else {
		var instanceType string
		leaseID, sandboxID, slug, instanceType, err = b.resolveSandboxID(req.ID, req.Repo.Root, req.Reclaim)
		if err != nil {
			return RunResult{}, err
		}
		client.useInstanceType(instanceType)
	}
	shouldStop := acquired && !req.Keep
	if shouldStop {
		defer func() {
			if !shouldStop {
				return
			}
			cleanupCtx, cancel := cloudflareCleanupContext()
			defer cancel()
			if err := client.destroySandbox(cleanupCtx, sandboxID); err != nil {
				fmt.Fprintf(b.rt.Stderr, "warning: %s destroy failed for %s: %v\n", providerName, sandboxID, err)
				return
			}
			removeLeaseClaim(leaseID)
		}()
	}
	session := &RunSessionHandle{
		Provider:       providerName,
		LeaseID:        leaseID,
		Slug:           slug,
		Reused:         !acquired,
		Kept:           !shouldStop,
		CleanupCommand: cloudflareCleanupCommand(leaseID),
	}

	syncDuration := time.Duration(0)
	syncPhases := []timingPhase{{Name: "sync", Skipped: true, Reason: "--no-sync"}}
	if !req.NoSync {
		syncPhases, syncDuration, err = b.syncWorkspace(ctx, client, sandboxID, req, workdir)
		if err != nil {
			return RunResult{Total: b.now().Sub(started), SyncDelegated: true, Session: session}, err
		}
		fmt.Fprintf(b.rt.Stderr, "sync complete in %s\n", syncDuration.Round(time.Millisecond))
	} else if err := b.prepareWorkspace(ctx, client, sandboxID, workdir, false); err != nil {
		return RunResult{Session: session}, err
	}
	if req.SyncOnly {
		result := RunResult{Total: b.now().Sub(started), SyncDelegated: true, Session: session}
		fmt.Fprintf(b.rt.Stdout, "synced %s\n", workdir)
		if req.TimingJSON {
			err := writeTimingJSON(b.rt.Stderr, timingReport{
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
			})
			return result, err
		}
		return result, nil
	}

	command, err := buildCloudflareCommand(req.Command, req.ShellMode)
	if err != nil {
		return RunResult{Session: session}, err
	}
	if req.EnvSummary {
		printEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
	}
	commandStarted := b.now()
	exitCode, commandErr := client.execStream(ctx, sandboxID, execStreamRequest{
		Command:   command,
		Cwd:       workdir,
		Env:       req.Env,
		TimeoutMS: durationMillisecondsCeil(b.cfg.TTL),
	}, b.rt.Stdout, b.rt.Stderr)
	if commandErr != nil && exitCode == 0 {
		exitCode = 1
	}
	commandDuration := b.now().Sub(commandStarted)
	result := RunResult{
		ExitCode:      exitCode,
		Command:       commandDuration,
		Total:         b.now().Sub(started),
		SyncDelegated: true,
		Session:       session,
	}
	if req.NoSync {
		fmt.Fprintf(b.rt.Stderr, "%s run summary sync_skipped=true command=%s total=%s exit=%d\n", providerName, result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), result.ExitCode)
	} else {
		fmt.Fprintf(b.rt.Stderr, "%s run summary sync=%s command=%s total=%s exit=%d\n", providerName, syncDuration.Round(time.Millisecond), result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), result.ExitCode)
	}
	if req.TimingJSON {
		if err := writeTimingJSON(b.rt.Stderr, timingReport{
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
		}); err != nil {
			return result, err
		}
	}
	if commandErr != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		session.Kept = !shouldStop
		return result, ExitError{Code: 1, Message: fmt.Sprintf("%s run failed: %v", providerName, commandErr)}
	}
	if result.ExitCode != 0 {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		session.Kept = !shouldStop
		return result, ExitError{Code: result.ExitCode, Message: fmt.Sprintf("%s run exited %d", providerName, result.ExitCode)}
	}
	return result, nil
}

func (b *cloudflareBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	claims, err := localCloudflareClaims()
	if err != nil {
		return nil, err
	}
	if req.Refresh {
		if len(claims) == 0 {
			return []LeaseView{}, nil
		}
		return b.listRefreshed(ctx, claims)
	}
	servers := make([]Server, 0, len(claims))
	for _, claim := range claims {
		servers = append(servers, claimToServer(claim, "unknown"))
	}
	return servers, nil
}

func (b *cloudflareBackend) listRefreshed(ctx context.Context, claims []localClaim) ([]LeaseView, error) {
	client, err := newCloudflareClient(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	servers := make([]Server, 0, len(claims))
	defaultInstanceType := client.instanceType
	for _, claim := range claims {
		client.instanceType = defaultInstanceType
		client.useInstanceType(claim.instanceType())
		sandbox, err := client.getSandbox(ctx, claim.LeaseID)
		if err != nil {
			if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "not found") {
				servers = append(servers, claimToServer(claim, "missing"))
				continue
			}
			fmt.Fprintf(b.rt.Stderr, "warning: %s status failed for %s: %v\n", providerName, claim.LeaseID, err)
			servers = append(servers, claimToServer(claim, "unknown"))
			continue
		}
		servers = append(servers, sandboxToServer(claim.LeaseID, claim.Slug, sandbox))
	}
	return servers, nil
}

func (b *cloudflareBackend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	client, err := newCloudflareClient(b.cfg, b.rt)
	if err != nil {
		return StatusView{}, err
	}
	leaseID, sandboxID, slug, instanceType, err := b.resolveSandboxID(req.ID, "", false)
	if err != nil {
		return StatusView{}, err
	}
	client.useInstanceType(instanceType)
	deadline := b.now().Add(req.WaitTimeout)
	if req.WaitTimeout <= 0 {
		deadline = b.now().Add(5 * time.Minute)
	}
	for {
		sandbox, err := client.getSandbox(ctx, sandboxID)
		if err != nil {
			return StatusView{}, err
		}
		view := sandboxStatusView(leaseID, slug, sandbox)
		if cloudflareTerminalState(view.State) {
			removeLeaseClaim(leaseID)
			return view, nil
		}
		if !req.Wait || view.Ready {
			return view, nil
		}
		if b.now().After(deadline) {
			return StatusView{}, exit(5, "timed out waiting for %s container %s to become ready", providerName, sandboxID)
		}
		select {
		case <-ctx.Done():
			return StatusView{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *cloudflareBackend) Stop(ctx context.Context, req StopRequest) error {
	client, err := newCloudflareClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, sandboxID, _, instanceType, err := b.resolveSandboxID(req.ID, "", false)
	if err != nil {
		return err
	}
	client.useInstanceType(instanceType)
	if err := client.destroySandbox(ctx, sandboxID); err != nil {
		if cloudflareNotFoundError(err) {
			removeLeaseClaim(leaseID)
			fmt.Fprintf(b.rt.Stdout, "removed stale %s claim %s reason=not-found\n", providerName, leaseID)
			return nil
		}
		return err
	}
	removeLeaseClaim(leaseID)
	fmt.Fprintf(b.rt.Stdout, "stopped %s provider=%s sandbox=%s\n", leaseID, providerName, sandboxID)
	return nil
}

func (b *cloudflareBackend) Cleanup(ctx context.Context, req CleanupRequest) error {
	client, err := newCloudflareClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	claims, err := localCloudflareClaims()
	if err != nil {
		return err
	}
	removed := 0
	defaultInstanceType := client.instanceType
	for _, claim := range claims {
		client.instanceType = defaultInstanceType
		client.useInstanceType(claim.instanceType())
		sandbox, err := client.getSandbox(ctx, claim.LeaseID)
		if err != nil {
			if cloudflareNotFoundError(err) {
				if req.DryRun {
					fmt.Fprintf(b.rt.Stdout, "would remove stale %s claim %s slug=%s reason=not-found\n", providerName, claim.LeaseID, blank(claim.Slug, "-"))
					continue
				}
				removeLeaseClaim(claim.LeaseID)
				removed++
				fmt.Fprintf(b.rt.Stdout, "removed stale %s claim %s slug=%s reason=not-found\n", providerName, claim.LeaseID, blank(claim.Slug, "-"))
				continue
			}
			fmt.Fprintf(b.rt.Stderr, "warning: %s status failed for %s: %v\n", providerName, claim.LeaseID, err)
			continue
		}
		if !cloudflareTerminalState(sandbox.State) {
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would remove stale %s claim %s slug=%s state=%s\n", providerName, claim.LeaseID, blank(claim.Slug, "-"), sandbox.State)
			continue
		}
		removeLeaseClaim(claim.LeaseID)
		removed++
		fmt.Fprintf(b.rt.Stdout, "removed stale %s claim %s slug=%s state=%s\n", providerName, claim.LeaseID, blank(claim.Slug, "-"), sandbox.State)
	}
	if !req.DryRun {
		fmt.Fprintf(b.rt.Stdout, "%s cleanup removed=%d checked=%d\n", providerName, removed, len(claims))
	}
	return nil
}

func cloudflareNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "404") || strings.Contains(text, "not found")
}

func cloudflareCleanupCommand(leaseID string) string {
	return fmt.Sprintf("crabbox stop --provider %s --id %s", providerName, shellQuote(leaseID))
}

func (b *cloudflareBackend) createSandbox(ctx context.Context, client *cloudflareClient, repo Repo, reclaim bool, requestedSlug string) (string, cloudflareContainer, string, error) {
	leaseID := newLeaseID()
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return "", cloudflareContainer{}, "", err
	}
	workdir, err := cloudflareWorkdir(b.cfg)
	if err != nil {
		return "", cloudflareContainer{}, "", err
	}
	labels := map[string]string{
		"crabbox":       "true",
		"provider":      providerName,
		"lease":         leaseID,
		"slug":          slug,
		"repo":          repo.Name,
		"instance_type": client.instanceType,
	}
	sandbox, err := client.createSandbox(ctx, createSandboxRequest{
		ID:                 leaseID,
		LeaseID:            leaseID,
		Slug:               slug,
		Repo:               repo.Name,
		Workdir:            workdir,
		InstanceType:       client.instanceType,
		TTLSeconds:         durationSecondsCeil(b.cfg.TTL),
		IdleTimeoutSeconds: durationSecondsCeil(b.cfg.IdleTimeout),
		Labels:             labels,
	})
	if err != nil {
		return "", cloudflareContainer{}, "", err
	}
	if err := claimLeaseForRepoProviderPondLabels(leaseID, slug, providerName, b.cfg.Pond, repo.Root, b.cfg.IdleTimeout, reclaim, labels); err != nil {
		cleanupCtx, cancel := cloudflareCleanupContext()
		cleanupErr := client.destroySandbox(cleanupCtx, sandbox.ID)
		cancel()
		if cleanupErr != nil {
			return "", cloudflareContainer{}, "", fmt.Errorf("%w; cleanup failed for %s sandbox %s: %v", err, providerName, sandbox.ID, cleanupErr)
		}
		return "", cloudflareContainer{}, "", err
	}
	return leaseID, sandbox, slug, nil
}

func (b *cloudflareBackend) resolveSandboxID(identifier, repoRoot string, reclaim bool) (string, string, string, string, error) {
	claim, ok, err := resolveLeaseClaimForProvider(identifier, providerName)
	if err != nil {
		return "", "", "", "", err
	}
	if ok {
		if repoRoot != "" {
			if err := claimLeaseForRepoProvider(claim.LeaseID, claim.Slug, providerName, repoRoot, time.Duration(claim.IdleTimeoutSeconds)*time.Second, reclaim); err != nil {
				return "", "", "", "", err
			}
		}
		return claim.LeaseID, claim.LeaseID, blank(claim.Slug, newLeaseSlug(claim.LeaseID)), cloudflareClaimInstanceType(claim), nil
	}
	value := strings.TrimSpace(identifier)
	if value == "" {
		return "", "", "", "", exit(2, "%s id is required", providerName)
	}
	return "", "", "", "", exit(2, "refusing to use %s sandbox %q without a local Crabbox claim", providerName, value)
}

func buildCloudflareCommand(command []string, shellMode bool) (string, error) {
	if len(command) == 0 {
		return "", errors.New("missing command")
	}
	if shellMode {
		return strings.Join(command, " "), nil
	}
	if shouldUseShell(command) || leadingEnvAssignment(command) {
		return shellScriptFromArgv(command), nil
	}
	return strings.Join(shellWords(command), " "), nil
}

func rejectCloudflareSyncOptions(req RunRequest) error {
	if req.ChecksumSync {
		return exit(2, "%s uses archive sync; --checksum is not supported", providerName)
	}
	return nil
}

func cloudflareWorkdir(cfg Config) (string, error) {
	workdir := blank(strings.TrimSpace(cfg.Cloudflare.Workdir), "/workspace/crabbox")
	clean := path.Clean(workdir)
	if !strings.HasPrefix(clean, "/") {
		return "", exit(2, "%s workdir %q must resolve to an absolute path", providerName, workdir)
	}
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var", "/workspace":
		return "", exit(2, "%s workdir %q is too broad; choose a dedicated subdirectory", providerName, clean)
	}
	return clean, nil
}

func sandboxStatusView(leaseID, slug string, sandbox cloudflareContainer) StatusView {
	server := sandboxToServer(leaseID, slug, sandbox)
	return StatusView{
		ID:         leaseID,
		Slug:       blank(slug, newLeaseSlug(leaseID)),
		Provider:   providerName,
		TargetOS:   targetLinux,
		State:      server.Status,
		ServerID:   sandbox.ID,
		ServerType: server.ServerType.Name,
		Network:    networkPublic,
		Ready:      cloudflareReady(server.Status),
		Labels:     server.Labels,
	}
}

func sandboxToServer(leaseID, slug string, sandbox cloudflareContainer) Server {
	labels := map[string]string{}
	for k, v := range sandbox.Labels {
		labels[k] = v
	}
	labels["provider"] = providerName
	labels["lease"] = leaseID
	labels["slug"] = blank(slug, newLeaseSlug(leaseID))
	labels["target"] = targetLinux
	state := blank(sandbox.State, "running")
	instanceType := blank(sandbox.InstanceType, providerName)
	labels["state"] = state
	labels["instance_type"] = instanceType
	server := Server{
		Provider: providerName,
		CloudID:  sandbox.ID,
		Name:     sandbox.ID,
		Status:   state,
		Labels:   labels,
	}
	server.ServerType.Name = instanceType
	return server
}

func claimToServer(claim localClaim, state string) Server {
	labels := map[string]string{
		"provider": providerName,
		"lease":    claim.LeaseID,
		"slug":     blank(claim.Slug, newLeaseSlug(claim.LeaseID)),
		"target":   targetLinux,
		"state":    state,
	}
	server := Server{
		Provider: providerName,
		CloudID:  claim.LeaseID,
		Name:     claim.LeaseID,
		Status:   state,
		Labels:   labels,
	}
	server.ServerType.Name = providerName
	return server
}

func cloudflareReady(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "ready", "started", "active", "healthy":
		return true
	default:
		return false
	}
}

func cloudflareTerminalState(status string) bool {
	state := strings.ToLower(strings.TrimSpace(status))
	return state == "expired" || state == "stopped" || state == "stopped_with_code"
}

func durationSecondsCeil(duration time.Duration) int {
	if duration <= 0 {
		return 0
	}
	return int((duration + time.Second - 1) / time.Second)
}

func durationMillisecondsCeil(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	return int64((duration + time.Millisecond - 1) / time.Millisecond)
}

func (b *cloudflareBackend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}

type localClaim struct {
	LeaseID      string            `json:"leaseID"`
	Slug         string            `json:"slug,omitempty"`
	Provider     string            `json:"provider,omitempty"`
	InstanceType string            `json:"instanceType,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
}

func (c localClaim) instanceType() string {
	if c.InstanceType != "" {
		return c.InstanceType
	}
	return c.Labels["instance_type"]
}

func cloudflareClaimInstanceType(claim LeaseClaim) string {
	return claim.Labels["instance_type"]
}

func localCloudflareClaims() ([]localClaim, error) {
	dir, err := localClaimsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, exit(2, "read claims directory: %v", err)
	}
	var claims []localClaim
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, exit(2, "read claim %s: %v", entry.Name(), err)
		}
		var claim localClaim
		if err := json.Unmarshal(data, &claim); err != nil {
			return nil, exit(2, "parse claim %s: %v", entry.Name(), err)
		}
		if claim.Provider == providerName {
			claims = append(claims, claim)
		}
	}
	return claims, nil
}

func localClaimsDir() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "crabbox", "claims"), nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", exit(2, "user state directory is unavailable")
	}
	return filepath.Join(dir, "crabbox", "state", "claims"), nil
}
