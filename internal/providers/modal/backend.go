package modal

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

func NewModalBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	return &modalBackend{spec: spec, cfg: cfg, rt: rt}
}

type modalBackend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

const modalMaxSandboxTimeout = 24 * time.Hour

func (b *modalBackend) Spec() ProviderSpec { return b.spec }

func (b *modalBackend) Warmup(ctx context.Context, req WarmupRequest) error {
	started := b.now()
	client, err := newModalAPI(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, sandbox, slug, err := b.createSandbox(ctx, client, req.Repo, req.Keep, req.Reclaim, req.RequestedSlug)
	if err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=modal sandbox=%s\n", leaseID, slug, sandbox.ID)
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: modal warmup keeps the sandbox until explicit stop\n")
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

func (b *modalBackend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if err := rejectModalSyncOptions(req); err != nil {
		return RunResult{}, err
	}
	workdir, err := cleanModalWorkdir(modalWorkdir(b.cfg))
	if err != nil {
		return RunResult{}, err
	}
	started := b.now()
	client, err := newModalAPI(b.cfg, b.rt)
	if err != nil {
		return RunResult{}, err
	}
	leaseID, sandboxID, slug := "", "", ""
	acquired := false
	if req.ID == "" {
		var sandbox modalSandbox
		leaseID, sandbox, slug, err = b.createSandbox(ctx, client, req.Repo, req.Keep, req.Reclaim, req.RequestedSlug)
		if err != nil {
			return RunResult{}, err
		}
		sandboxID = sandbox.ID
		fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=modal sandbox=%s\n", leaseID, slug, sandboxID)
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
			if err := client.Terminate(context.Background(), sandboxID); err != nil {
				fmt.Fprintf(b.rt.Stderr, "warning: modal terminate failed for %s: %v\n", sandboxID, err)
				return
			}
			removeLeaseClaim(leaseID)
		}()
	}

	syncDuration := time.Duration(0)
	syncPhases := []timingPhase{{Name: "sync", Skipped: true, Reason: "--no-sync"}}
	if !req.NoSync {
		syncPhases, syncDuration, err = b.syncWorkspace(ctx, client, sandboxID, req, workdir)
		if err != nil {
			return RunResult{Total: b.now().Sub(started), SyncDelegated: true}, err
		}
		fmt.Fprintf(b.rt.Stderr, "sync complete in %s\n", syncDuration.Round(time.Millisecond))
	} else if err := b.prepareWorkspace(ctx, client, sandboxID, workdir, false); err != nil {
		return RunResult{}, err
	}
	if req.SyncOnly {
		result := RunResult{Total: b.now().Sub(started), SyncDelegated: true}
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

	command, err := buildModalCommand(req.Command, req.ShellMode, workdir)
	if err != nil {
		return RunResult{}, err
	}
	if req.EnvSummary {
		printEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
	}
	if len(req.Env) > 0 {
		envPath, cleanup, err := b.uploadEnvProfile(ctx, client, sandboxID, req.Env)
		if err != nil {
			return RunResult{}, err
		}
		defer cleanup()
		command = wrapModalCommandWithEnvProfile(command, envPath)
	}
	commandStarted := b.now()
	exitCode, commandErr := client.Exec(ctx, modalExecRequest{
		SandboxID: sandboxID,
		Command:   command,
		Timeout:   durationSecondsCeil(modalTimeoutDuration(b.cfg.TTL)),
		Stdout:    b.rt.Stdout,
		Stderr:    b.rt.Stderr,
	})
	commandDuration := b.now().Sub(commandStarted)
	result := RunResult{
		ExitCode:      exitCode,
		Command:       commandDuration,
		Total:         b.now().Sub(started),
		SyncDelegated: true,
	}
	if req.NoSync {
		fmt.Fprintf(b.rt.Stderr, "modal run summary sync_skipped=true command=%s total=%s exit=%d\n", result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), result.ExitCode)
	} else {
		fmt.Fprintf(b.rt.Stderr, "modal run summary sync=%s command=%s total=%s exit=%d\n", syncDuration.Round(time.Millisecond), result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), result.ExitCode)
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
		return result, ExitError{Code: 1, Message: fmt.Sprintf("modal run failed: %v", commandErr)}
	}
	if result.ExitCode != 0 {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return result, ExitError{Code: result.ExitCode, Message: fmt.Sprintf("modal run exited %d", result.ExitCode)}
	}
	return result, nil
}

func (b *modalBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	client, err := newModalAPI(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	sandboxes, err := client.ListSandboxes(ctx, map[string]string{"crabbox": "true", "provider": providerName})
	if err != nil {
		return nil, modalError("list sandboxes", err)
	}
	servers := make([]Server, 0, len(sandboxes))
	for _, sandbox := range sandboxes {
		servers = append(servers, modalSandboxToServer(sandbox))
	}
	return servers, nil
}

func (b *modalBackend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	servers, err := b.List(ctx, ListRequest{})
	if err != nil {
		return DoctorResult{}, err
	}
	return inventoryDoctorResult(providerName, len(servers)), nil
}

func (b *modalBackend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	client, err := newModalAPI(b.cfg, b.rt)
	if err != nil {
		return StatusView{}, err
	}
	leaseID, sandboxID, slug, err := b.resolveSandboxID(ctx, client, req.ID, "", false)
	if err != nil {
		return StatusView{}, err
	}
	deadline := b.now().Add(req.WaitTimeout)
	if req.WaitTimeout <= 0 {
		deadline = b.now().Add(5 * time.Minute)
	}
	for {
		sandbox, err := client.GetSandbox(ctx, sandboxID)
		if err != nil {
			return StatusView{}, modalError("get sandbox", err)
		}
		view := modalStatusView(leaseID, slug, sandbox)
		if !req.Wait || view.Ready {
			return view, nil
		}
		if b.now().After(deadline) {
			return StatusView{}, exit(5, "timed out waiting for modal sandbox %s to become ready", sandboxID)
		}
		select {
		case <-ctx.Done():
			return StatusView{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *modalBackend) Stop(ctx context.Context, req StopRequest) error {
	client, err := newModalAPI(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, sandboxID, _, err := b.resolveSandboxID(ctx, client, req.ID, "", false)
	if err != nil {
		return err
	}
	if err := client.Terminate(ctx, sandboxID); err != nil {
		return modalError("terminate sandbox", err)
	}
	removeLeaseClaim(leaseID)
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s\n", leaseID, sandboxID)
	return nil
}

func (b *modalBackend) createSandbox(ctx context.Context, client modalAPI, repo Repo, keep, reclaim bool, requestedSlug string) (string, modalSandbox, string, error) {
	workspace, err := cleanModalWorkdir(modalWorkdir(b.cfg))
	if err != nil {
		return "", modalSandbox{}, "", err
	}
	leaseID := newLeaseID()
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return "", modalSandbox{}, "", err
	}
	cfg := b.cfg
	cfg.TTL = modalTimeoutDuration(cfg.TTL)
	cfg.ServerType = modalImage(cfg)
	labels := modalSandboxTags(cfg, leaseID, slug, repo.Name, keep, b.now().UTC())
	timeoutSeconds := durationSecondsCeil(cfg.TTL)
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=modal lease=%s slug=%s app=%s image=%s timeout=%ds\n", leaseID, slug, modalApp(cfg), modalImage(cfg), timeoutSeconds)
	sandbox, err := client.CreateSandbox(ctx, modalCreateSandboxRequest{
		App:            modalApp(cfg),
		Image:          modalImage(cfg),
		Workdir:        workspace,
		TimeoutSeconds: timeoutSeconds,
		Tags:           labels,
	})
	if err != nil {
		return "", modalSandbox{}, "", modalError("create sandbox", err)
	}
	if sandbox.ID == "" {
		return "", modalSandbox{}, "", exit(5, "modal create sandbox returned no sandbox id")
	}
	if err := claimLeaseForRepoProviderPond(leaseID, slug, providerName, cfg.Pond, repo.Root, cfg.IdleTimeout, reclaim); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if cleanupErr := client.Terminate(cleanupCtx, sandbox.ID); cleanupErr != nil {
			leakErr := fmt.Errorf("cleanup modal sandbox %s after claim failure: %w; run `crabbox stop --provider modal --id %s` to retry cleanup", sandbox.ID, cleanupErr, sandbox.ID)
			fmt.Fprintf(b.rt.Stderr, "warning: %v\n", leakErr)
			return "", modalSandbox{}, "", errors.Join(err, leakErr)
		}
		return "", modalSandbox{}, "", err
	}
	return leaseID, sandbox, slug, nil
}

func modalSandboxTags(cfg Config, leaseID, slug, repoName string, keep bool, now time.Time) map[string]string {
	base := directLeaseLabels(cfg, leaseID, slug, providerName, "", keep, now)
	tags := map[string]string{
		"crabbox":    "true",
		"provider":   providerName,
		"lease":      leaseID,
		"slug":       base["slug"],
		"state":      "ready",
		"keep":       base["keep"],
		"expires_at": base["expires_at"],
		"app":        modalApp(cfg),
		"image":      modalImage(cfg),
	}
	if strings.TrimSpace(repoName) != "" {
		tags["repo"] = repoName
	}
	return tags
}

func (b *modalBackend) resolveSandboxID(ctx context.Context, client modalAPI, id, repoRoot string, reclaim bool) (string, string, string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", "", "", exit(2, "provider=modal requires a Crabbox lease id, slug, or Modal sandbox id")
	}
	if claim, ok, err := resolveLeaseClaim(id); err != nil {
		return "", "", "", err
	} else if ok && claim.Provider == providerName {
		if repoRoot != "" {
			if err := claimLeaseForRepoProvider(claim.LeaseID, claim.Slug, providerName, repoRoot, time.Duration(claim.IdleTimeoutSeconds)*time.Second, reclaim); err != nil {
				return "", "", "", err
			}
		}
		sandbox, err := resolveModalSandboxByLease(ctx, client, claim.LeaseID)
		if err != nil {
			return "", "", "", err
		}
		return claim.LeaseID, sandbox.ID, claim.Slug, nil
	}
	if strings.HasPrefix(id, "cbx_") {
		sandbox, err := resolveModalSandboxByLease(ctx, client, id)
		if err != nil {
			return "", "", "", err
		}
		slug := modalSlug(id, sandbox)
		if repoRoot != "" {
			if err := claimLeaseForRepoProvider(id, slug, providerName, repoRoot, b.cfg.IdleTimeout, reclaim); err != nil {
				return "", "", "", err
			}
		}
		return id, sandbox.ID, slug, nil
	}
	sandbox, err := client.GetSandbox(ctx, id)
	if err == nil && isCrabboxModalSandbox(sandbox) {
		leaseID := modalLeaseID(sandbox)
		slug := modalSlug(leaseID, sandbox)
		if repoRoot != "" {
			if err := claimLeaseForRepoProvider(leaseID, slug, providerName, repoRoot, b.cfg.IdleTimeout, reclaim); err != nil {
				return "", "", "", err
			}
		}
		return leaseID, sandbox.ID, slug, nil
	}
	if err != nil && !isModalNotFoundError(err) {
		return "", "", "", modalError("get sandbox", err)
	}
	return "", "", "", exit(4, "modal sandbox or claim %q was not found", id)
}

func resolveModalSandboxByLease(ctx context.Context, client modalAPI, leaseID string) (modalSandbox, error) {
	sandboxes, err := client.ListSandboxes(ctx, map[string]string{"lease": leaseID, "provider": providerName})
	if err != nil {
		return modalSandbox{}, modalError("list sandboxes", err)
	}
	for _, sandbox := range sandboxes {
		if isCrabboxModalSandbox(sandbox) {
			return sandbox, nil
		}
	}
	return modalSandbox{}, exit(4, "modal lease %q was not found", leaseID)
}

func modalSandboxToServer(sandbox modalSandbox) Server {
	labels := map[string]string{}
	for k, v := range sandbox.Tags {
		labels[k] = v
	}
	labels["provider"] = providerName
	labels["lease"] = modalLeaseID(sandbox)
	if labels["slug"] == "" {
		labels["slug"] = newLeaseSlug(labels["lease"])
	}
	labels["target"] = targetLinux
	if labels["state"] == "" {
		labels["state"] = sandbox.Status
	}
	server := Server{
		Provider: providerName,
		CloudID:  sandbox.ID,
		Name:     blank(sandbox.Name, sandbox.ID),
		Status:   sandbox.Status,
		Labels:   labels,
	}
	server.ServerType.Name = blank(labels["image"], "python:3.13-slim")
	return server
}

func modalStatusView(leaseID, slug string, sandbox modalSandbox) StatusView {
	server := modalSandboxToServer(sandbox)
	return StatusView{
		ID:         leaseID,
		Slug:       blank(slug, modalSlug(leaseID, sandbox)),
		Provider:   providerName,
		TargetOS:   targetLinux,
		State:      sandbox.Status,
		ServerID:   sandbox.ID,
		ServerType: server.ServerType.Name,
		Network:    networkPublic,
		Ready:      modalStatusReady(sandbox.Status),
		Labels:     server.Labels,
	}
}

func modalStatusReady(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "running", "ready", "started", "active":
		return true
	default:
		return false
	}
}

func modalLeaseID(sandbox modalSandbox) string {
	if lease := strings.TrimSpace(sandbox.Tags["lease"]); lease != "" {
		return lease
	}
	return "modal_" + sandbox.ID
}

func modalSlug(leaseID string, sandbox modalSandbox) string {
	if slug := strings.TrimSpace(sandbox.Tags["slug"]); slug != "" {
		return slug
	}
	return newLeaseSlug(leaseID)
}

func isCrabboxModalSandbox(sandbox modalSandbox) bool {
	return sandbox.Tags["provider"] == providerName && sandbox.Tags["crabbox"] == "true"
}

func modalApp(cfg Config) string {
	return blank(strings.TrimSpace(cfg.Modal.App), "crabbox")
}

func modalImage(cfg Config) string {
	return blank(strings.TrimSpace(cfg.Modal.Image), "python:3.13-slim")
}

func modalWorkdir(cfg Config) string {
	return blank(strings.TrimSpace(cfg.Modal.Workdir), "/workspace/crabbox")
}

func cleanModalWorkdir(workdir string) (string, error) {
	trimmed := strings.TrimSpace(workdir)
	if trimmed == "" {
		return "", exit(2, "modal workdir is empty")
	}
	clean := path.Clean(trimmed)
	if !strings.HasPrefix(clean, "/") {
		return "", exit(2, "modal workdir %q must resolve to an absolute path", workdir)
	}
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var", "/workspace":
		return "", exit(2, "modal workdir %q is too broad; choose a dedicated subdirectory", clean)
	}
	return clean, nil
}

func modalTimeoutDuration(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return 5 * time.Minute
	}
	if ttl > modalMaxSandboxTimeout {
		return modalMaxSandboxTimeout
	}
	return ttl
}

func durationSecondsCeil(duration time.Duration) int {
	if duration <= 0 {
		return 0
	}
	return int((duration + time.Second - 1) / time.Second)
}

func buildModalCommand(command []string, shellMode bool, workdir string) ([]string, error) {
	if len(command) == 0 {
		return nil, errors.New("missing command")
	}
	var script string
	if shellMode {
		script = strings.Join(command, " ")
	} else if shouldUseShell(command) || leadingEnvAssignment(command) {
		script = shellScriptFromArgv(command)
	} else {
		script = "exec " + strings.Join(shellWords(command), " ")
	}
	if strings.TrimSpace(workdir) != "" {
		script = "cd " + shellQuote(workdir) + " && " + script
	}
	return []string{"bash", "-lc", script}, nil
}

func rejectModalSyncOptions(req RunRequest) error {
	if req.ChecksumSync {
		return exit(2, "%s uses Modal archive sync; --checksum is not supported", providerName)
	}
	return nil
}

func isModalNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "404")
}

func modalError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("modal %s: %w", action, err)
}

func (b *modalBackend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}
