package wandb

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const wandbStopTimeout = 15 * time.Second

func NewWandbBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	applyWandbDefaults(&cfg)
	return &wandbBackend{spec: spec, cfg: cfg, rt: rt}
}

type wandbBackend struct {
	spec   ProviderSpec
	cfg    Config
	rt     Runtime
	client wandbAPI
}

func (b *wandbBackend) Spec() ProviderSpec { return b.spec }

func (b *wandbBackend) Warmup(ctx context.Context, req WarmupRequest) error {
	_ = ctx
	_ = req
	return exit(2, "provider=%s does not support warmup; sandboxes are acquired per-run", providerName)
}

func (b *wandbBackend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if err := rejectWandbRunOptions(req); err != nil {
		return RunResult{}, err
	}
	if len(req.Command) == 0 {
		return RunResult{}, exit(2, "missing command")
	}
	// Credential resolution lives in the client (CRABBOX_WANDB_API_KEY →
	// cfg.Wandb.APIKey → WANDB_API_KEY plus required WANDB_ENTITY_NAME). The
	// old direct WANDB_API_KEY check here ignored the documented
	// CRABBOX_WANDB_API_KEY override.
	client, err := b.api()
	if err != nil {
		return RunResult{}, err
	}
	defer b.closeClientAfterOperation()
	started := b.now()
	cfg := b.cfg
	image := blank(strings.TrimSpace(cfg.Wandb.DefaultImage), "ubuntu:24.04")
	maxLifetime := wandbMaxLifetimeSeconds(cfg)

	sandboxID := strings.TrimSpace(req.ID)
	acquired := false
	if sandboxID == "" {
		fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s image=%s max_lifetime=%ds\n", providerName, image, maxLifetime)
		sb, err := client.Acquire(ctx, wandbAcquireRequest{
			Image:           image,
			MaxLifetimeSecs: maxLifetime,
			Tags:            []string{"crabbox"},
			EnvironmentVars: req.Env,
		})
		if err != nil {
			return RunResult{}, err
		}
		sandboxID = sb.ID
		acquired = true
		fmt.Fprintf(b.rt.Stderr, "provisioned sandbox=%s status=%s\n", sb.ID, sb.Status)
		if req.EnvSummary {
			printEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
		}
	} else if len(req.Env) > 0 && !wandbExistingIDEnvIsImplicitDefault(req) {
		// CoreWeave Sandboxes apply environment variables at Start time only;
		// the v1beta2 Exec RPC has no env field, so we can't honour
		// selected env on an already-running sandbox. The only exception is
		// Crabbox's built-in implicit CI/NODE_OPTIONS allowlist, which older
		// configs may select without the user asking for env forwarding.
		return RunResult{}, exit(2, "provider=%s cannot forward env vars to an existing sandbox (--id); rerun without --id or omit --allow-env", providerName)
	}

	// Stop semantics match the modal/e2b/islo/tensorlake sibling pattern:
	// we acquire+release per-run by default, but honour --keep (always
	// retain) and --keep-on-failure (retain only when the run fails) so
	// users can debug a sandbox after a bad command.
	shouldStop := acquired && !req.Keep
	defer func() {
		if !shouldStop {
			return
		}
		stopCtx, cancel := context.WithTimeout(context.Background(), wandbStopTimeout)
		defer cancel()
		if err := client.Stop(stopCtx, sandboxID, 10, true); err != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: wandb stop failed for %s: %v\n", sandboxID, err)
		}
	}()
	result := RunResult{
		Session: &RunSessionHandle{
			Provider:       providerName,
			LeaseID:        sandboxID,
			Slug:           sandboxID,
			Reused:         !acquired,
			Kept:           !shouldStop,
			CleanupCommand: wandbCleanupCommand(sandboxID),
		},
	}
	finishResult := func() RunResult {
		result.Session.Kept = !shouldStop
		return result
	}

	commandStarted := b.now()
	exitCode, execErr := client.Exec(ctx, wandbExecRequest{
		SandboxID: sandboxID,
		Command:   req.Command,
		Stdout:    b.rt.Stdout,
		Stderr:    b.rt.Stderr,
	})
	if execErr != nil && exitCode == 0 {
		var ee ExitError
		if errors.As(execErr, &ee) && ee.Code != 0 {
			exitCode = ee.Code
		} else {
			exitCode = 1
		}
	}
	// Command measures just the user's exec; Total includes Acquire+poll.
	// Conflating them (the previous bug) made commandMs == totalMs on every
	// fresh-sandbox run, hiding provisioning time from --timing-json users.
	commandDuration := b.now().Sub(commandStarted)
	result.ExitCode = exitCode
	result.Command = commandDuration
	result.Total = b.now().Sub(started)

	// Emit timing JSON before any failure return so automation consuming
	// `--timing-json` still gets a report when the user's command exits
	// non-zero or the exec itself errors. Mirrors railway / modal / e2b.
	if req.TimingJSON {
		if err := writeTimingJSON(b.rt.Stderr, timingReport{
			Provider:  providerName,
			Slug:      sandboxID,
			CommandMs: commandDuration.Milliseconds(),
			TotalMs:   result.Total.Milliseconds(),
			ExitCode:  result.ExitCode,
			Label:     strings.TrimSpace(req.Label),
		}); err != nil {
			return finishResult(), err
		}
	}

	if execErr != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, sandboxID, sandboxID, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return finishResult(), execErr
	}
	if result.ExitCode != 0 {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, sandboxID, sandboxID, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return finishResult(), ExitError{Code: result.ExitCode, Message: fmt.Sprintf("%s sandbox exit=%d", providerName, result.ExitCode)}
	}
	return finishResult(), nil
}

func wandbCleanupCommand(sandboxID string) string {
	return fmt.Sprintf("crabbox stop --provider %s --id %s", providerName, shellQuote(sandboxID))
}

func (b *wandbBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	client, err := b.api()
	if err != nil {
		return nil, err
	}
	defer b.closeClientAfterOperation()
	status := ""
	if req.All {
		status = "all"
	}
	sandboxes, err := client.List(ctx, []string{"crabbox"}, status)
	if err != nil {
		return nil, err
	}
	views := make([]Server, 0, len(sandboxes))
	for _, sb := range sandboxes {
		views = append(views, Server{
			CloudID:  sb.ID,
			Provider: providerName,
			Name:     sb.ID,
			Status:   sb.Status,
			Labels:   map[string]string{"created_at": sb.CreatedAt},
		})
	}
	return views, nil
}

func (b *wandbBackend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	if req.ID == "" {
		return StatusView{}, exit(2, "provider=%s status requires --id <sandbox-id>", providerName)
	}
	client, err := b.api()
	if err != nil {
		return StatusView{}, err
	}
	defer b.closeClientAfterOperation()
	sb, err := client.Status(ctx, req.ID)
	if err != nil {
		return StatusView{}, err
	}
	state := strings.ToLower(strings.TrimSpace(sb.Status))
	ready := state == "running"
	return StatusView{
		ID:         sb.ID,
		Slug:       sb.ID,
		Provider:   providerName,
		TargetOS:   targetLinux,
		State:      state,
		ServerID:   sb.ID,
		ServerType: "wandb-sandbox",
		Network:    networkPublic,
		Ready:      ready,
		Labels:     map[string]string{"created_at": sb.CreatedAt},
	}, nil
}

func (b *wandbBackend) Stop(ctx context.Context, req StopRequest) error {
	if req.ID == "" {
		return exit(2, "provider=%s stop requires --id <sandbox-id>", providerName)
	}
	client, err := b.api()
	if err != nil {
		return err
	}
	defer b.closeClientAfterOperation()
	return client.Stop(ctx, req.ID, 10, false)
}

// Doctor mirrors the modal/e2b/runpod pattern: dial, probe auth via a cheap
// authenticated RPC, list inventory, return an inventory-style result. The
// missing-credential and gRPC-unreachable cases bubble up through b.api() and
// client.Version() with their typed errors.
func (b *wandbBackend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	client, err := b.api()
	if err != nil {
		return DoctorResult{}, err
	}
	defer b.closeClientAfterOperation()
	if _, err := client.Version(ctx); err != nil {
		// Surface the typed *wandbAPIError as-is: errors.As() at the cli
		// boundary unwraps it into ExitError with the mapped sysexit code
		// (77 EX_NOPERM, 69 EX_UNAVAILABLE, 124 timeout, …). Wrapping with
		// exit(1, …) here would erase that code.
		return DoctorResult{}, err
	}
	views, err := b.List(ctx, ListRequest{})
	if err != nil {
		return DoctorResult{}, err
	}
	return inventoryDoctorResult(providerName, len(views)), nil
}

func (b *wandbBackend) api() (wandbAPI, error) {
	if b.client != nil {
		return b.client, nil
	}
	c, err := newWandbClient(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	// Cache the client so multiple calls inside one backend operation reuse
	// one gRPC ClientConn. Operation entrypoints close it before returning.
	b.client = c
	return c, nil
}

func (b *wandbBackend) Close() error {
	if b.client == nil {
		return nil
	}
	client := b.client
	b.client = nil
	closer, ok := client.(interface{ Close() error })
	if !ok {
		return nil
	}
	return closer.Close()
}

func (b *wandbBackend) closeClientAfterOperation() {
	if err := b.Close(); err != nil {
		fmt.Fprintf(b.rt.Stderr, "warning: wandb client close failed: %v\n", err)
	}
}

func (b *wandbBackend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}

// applyWandbDefaults fills in interpreter / image / lifetime defaults without
// touching SSH or WorkRoot — delegated-run providers must not stomp on SSH
// config.
func applyWandbDefaults(cfg *Config) {
	cfg.Provider = providerName
	if cfg.TargetOS == "" {
		cfg.TargetOS = targetLinux
	}
	if cfg.Wandb.DefaultImage == "" {
		cfg.Wandb.DefaultImage = "ubuntu:24.04"
	}
	if cfg.Wandb.MaxLifetimeSeconds <= 0 {
		cfg.Wandb.MaxLifetimeSeconds = 1800
	}
}

func wandbMaxLifetimeSeconds(cfg Config) int {
	maxLifetime := cfg.Wandb.MaxLifetimeSeconds
	if maxLifetime <= 0 {
		maxLifetime = 1800
	}
	if cfg.TTL > 0 {
		ttlSeconds := int((cfg.TTL + time.Second - 1) / time.Second)
		if ttlSeconds > 0 && ttlSeconds < maxLifetime {
			maxLifetime = ttlSeconds
		}
	}
	return maxLifetime
}

func rejectWandbRunOptions(req RunRequest) error {
	if req.Reclaim {
		return exit(2, "provider=%s lifecycle is owned by W&B; --reclaim is not supported", providerName)
	}
	if !req.NoSync {
		return exit(2, "provider=%s does not support workspace sync; pass --no-sync", providerName)
	}
	if req.SyncOnly {
		return exit(2, "provider=%s does not support sync; --sync-only is rejected", providerName)
	}
	if req.ChecksumSync {
		return exit(2, "provider=%s does not support sync; --checksum is rejected", providerName)
	}
	if req.ForceSyncLarge {
		return exit(2, "provider=%s does not support sync; --force-sync-large is rejected", providerName)
	}
	if req.FullResync {
		return exit(2, "provider=%s does not support sync; --full-resync is rejected", providerName)
	}
	if req.ShellMode {
		return exit(2, "provider=%s does not support --shell", providerName)
	}
	// req.EnvSummary (set by --allow-env / env profiles / CRABBOX_ENV_ALLOW)
	// is intentionally NOT rejected — Run forwards the resolved req.Env into
	// the sandbox via Acquire.EnvironmentVars.
	return nil
}

func wandbExistingIDEnvIsImplicitDefault(req RunRequest) bool {
	if req.EnvSummary {
		return false
	}
	for name := range req.Env {
		if name != "CI" && name != "NODE_OPTIONS" {
			return false
		}
	}
	return true
}
