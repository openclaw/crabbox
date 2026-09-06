package modal

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
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

const (
	modalMaxSandboxTimeout = 24 * time.Hour
	modalCleanupTimeout    = 2 * time.Minute
)

func (b *modalBackend) Spec() ProviderSpec { return b.spec }

func (b *modalBackend) Warmup(ctx context.Context, req WarmupRequest) error {
	started := b.now()
	client, err := newModalAPI(b.cfg, b.rt)
	if err != nil {
		return err
	}
	claim, sandbox, err := b.createSandbox(ctx, client, req.Repo, req.Keep, req.Reclaim, req.RequestedSlug)
	if err != nil {
		return err
	}
	leaseID, slug := claim.LeaseID, claim.Slug
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
	workdir, workdirErr := cleanModalWorkdir(modalWorkdir(b.cfg))
	var client modalAPI
	var claim core.LeaseClaim
	var leaseID, sandboxID, slug string
	bind := func() error {
		binding, _, err := modalClaimBinding(claim)
		if err != nil {
			return err
		}
		leaseID, sandboxID, slug = claim.LeaseID, claim.CloudID, claim.Slug
		client = client.Bind(binding)
		return nil
	}
	fenced := func(action func() error) error {
		return core.WithLeaseClaimUnchanged(claim.LeaseID, claim, action)
	}
	handle := func() shared.DelegatedSandbox {
		return shared.DelegatedSandbox{LeaseID: leaseID, Slug: slug, CleanupCommand: modalCleanupCommand(leaseID)}
	}
	return shared.RunDelegatedSandbox(ctx, req, shared.DelegatedSandboxLifecycle{
		Provider: providerName, Runtime: b.rt, Workdir: workdir, IdleTimeout: b.cfg.IdleTimeout, TTL: b.cfg.TTL,
		CleanupTimeout: modalCleanupTimeout,
		Preflight: func(context.Context) error {
			if err := rejectModalSyncOptions(req); err != nil {
				return err
			}
			if workdirErr != nil {
				return workdirErr
			}
			var err error
			client, err = newModalAPI(b.cfg, b.rt)
			return err
		},
		PrepareArchive: func(ctx context.Context) (*core.PreparedArchive, error) {
			return core.PrepareDelegatedArchive(ctx, core.DelegatedArchivePreparationRequest{
				Config: b.cfg, Repo: req.Repo, ForceSyncLarge: req.ForceSyncLarge,
				TempPattern: "crabbox-modal-sync-*.tgz", Stderr: b.rt.Stderr, Now: b.now,
			})
		},
		Acquire: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			var err error
			claim, _, err = b.createSandbox(ctx, client, req.Repo, req.Keep, req.Reclaim, req.RequestedSlug)
			if err == nil {
				err = bind()
			}
			if err == nil {
				fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=modal sandbox=%s\n", leaseID, slug, sandboxID)
			}
			return handle(), err
		},
		Resolve: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			var err error
			claim, err = b.resolveOwnedSandbox(ctx, client, req.ID, req.Repo.Root, req.Reclaim)
			if err == nil {
				err = bind()
			}
			return handle(), err
		},
		Sync: func(ctx context.Context, prepared *core.PreparedArchive) ([]core.TimingPhase, time.Duration, error) {
			var phases []core.TimingPhase
			var elapsed time.Duration
			err := fenced(func() error {
				var err error
				phases, elapsed, err = b.syncWorkspace(ctx, client, sandboxID, req, workdir, prepared)
				return err
			})
			return phases, elapsed, err
		},
		NoSync: func(ctx context.Context) error {
			return fenced(func() error { return b.prepareWorkspace(ctx, client, sandboxID, workdir) })
		},
		Command: func(ctx context.Context) (shared.DelegatedSandboxCommand, error) {
			command, err := buildModalCommand(req.Command, req.ShellMode, workdir)
			if err != nil {
				return shared.DelegatedSandboxCommand{}, err
			}
			if req.EnvSummary {
				printEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
			}
			var cleanup func(context.Context)
			var closeCommand func(context.Context) error
			if len(req.Env) > 0 {
				var envPath string
				err = fenced(func() error {
					var err error
					envPath, cleanup, err = b.uploadEnvProfile(ctx, client, claim, req.Env)
					return err
				})
				if cleanup != nil {
					closeCommand = func(ctx context.Context) error {
						cleanup(ctx)
						return nil
					}
				}
				if err != nil {
					return shared.DelegatedSandboxCommand{Close: closeCommand}, err
				}
				command = shared.WrapCommandWithShellEnvProfile(command, envPath)
			}
			return shared.DelegatedSandboxCommand{Close: closeCommand, Run: func(ctx context.Context) (int, error) {
				var code int
				err := fenced(func() error {
					var err error
					code, err = client.Exec(ctx, modalExecRequest{
						SandboxID: sandboxID, Command: command,
						Timeout: durationSecondsCeil(modalTimeoutDuration(b.cfg.TTL)), Stdout: b.rt.Stdout, Stderr: b.rt.Stderr,
					})
					return err
				})
				return code, err
			}}, nil
		},
		Cleanup: func(ctx context.Context) error {
			return removeModalClaim(ctx, client, claim)
		},
	})
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
	leaseID, sandboxID, slug, err := b.resolveSandboxID(ctx, client, req.ID)
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
	claim, err := resolveModalClaim(req.ID)
	if err != nil {
		return err
	}
	client, err := newModalAPI(b.cfg, b.rt)
	if err != nil {
		return err
	}
	if err := removeModalClaim(ctx, client, claim); err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s\n", claim.LeaseID, claim.CloudID)
	return nil
}

func (b *modalBackend) createSandbox(ctx context.Context, client modalAPI, repo Repo, keep, reclaim bool, requestedSlug string) (core.LeaseClaim, modalSandbox, error) {
	if strings.TrimSpace(repo.Root) == "" {
		return core.LeaseClaim{}, modalSandbox{}, exit(2, "Modal acquisition requires a repository root for durable ownership")
	}
	workspace, err := cleanModalWorkdir(modalWorkdir(b.cfg))
	if err != nil {
		return core.LeaseClaim{}, modalSandbox{}, err
	}
	leaseID := newLeaseID()
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return core.LeaseClaim{}, modalSandbox{}, err
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
		Environment:    cfg.Modal.Environment,
		Secrets:        append([]string(nil), cfg.Modal.Secrets...),
	})
	if err != nil {
		return core.LeaseClaim{}, modalSandbox{}, fmt.Errorf("%w; inspect Modal app=%s lease=%s before native cleanup of an uncertain creation", modalError("create sandbox", err), modalApp(cfg), leaseID)
	}
	if sandbox.ID == "" {
		return core.LeaseClaim{}, modalSandbox{}, exit(5, "modal create sandbox returned no sandbox id; inspect Modal app=%s lease=%s", modalApp(cfg), leaseID)
	}
	binding := modalBinding{ID: sandbox.ID, LeaseID: leaseID, Slug: slug, Scope: sandbox.Scope}
	if err := binding.validate(sandbox); err != nil {
		return core.LeaseClaim{}, modalSandbox{}, fmt.Errorf("%w; retain and inspect Modal sandbox=%s lease=%s", err, sandbox.ID, leaseID)
	}
	claim, err := publishModalClaim(b, ctx, client, binding, sandbox, repo, reclaim)
	if err != nil {
		cleanupErr := core.CleanupLeaseClaimIfUnchangedAfter(leaseID, core.LeaseClaim{}, false, func() error {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), modalCleanupTimeout)
			defer cancel()
			return client.Terminate(cleanupCtx, binding)
		})
		if cleanupErr != nil {
			return core.LeaseClaim{}, modalSandbox{}, errors.Join(err, fmt.Errorf("retain and inspect Modal sandbox=%s lease=%s after claim failure; cleanup: %w", sandbox.ID, leaseID, cleanupErr))
		}
		return core.LeaseClaim{}, modalSandbox{}, err
	}
	return claim, sandbox, nil
}

func modalCleanupCommand(leaseID string) string {
	return fmt.Sprintf("crabbox stop --provider %s --id %s", providerName, shellQuote(leaseID))
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

func (b *modalBackend) resolveSandboxID(ctx context.Context, client modalAPI, id string) (string, string, string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", "", "", exit(2, "provider=modal requires a Crabbox lease id, slug, or Modal sandbox id")
	}
	if claim, ok, err := resolveLeaseClaim(id); err != nil {
		return "", "", "", err
	} else if ok && claim.Provider == providerName {
		if claim.CloudID != "" {
			return claim.LeaseID, claim.CloudID, claim.Slug, nil
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
		return id, sandbox.ID, slug, nil
	}
	sandbox, err := client.GetSandbox(ctx, id)
	if err == nil && isCrabboxModalSandbox(sandbox) {
		leaseID := modalLeaseID(sandbox)
		slug := modalSlug(leaseID, sandbox)
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
