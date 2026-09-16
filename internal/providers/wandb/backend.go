package wandb

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
	"google.golang.org/grpc/codes"
)

const (
	wandbStopTimeout        = 15 * time.Second
	wandbStatusPollInterval = 200 * time.Millisecond
	wandbStatusWaitTimeout  = 5 * time.Minute
)

func NewWandbBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	cfg.Provider = providerName
	applyWandbDefaults(&cfg)
	return &wandbBackend{spec: spec, cfg: cfg, rt: rt}
}

type wandbBackend struct {
	spec   core.ProviderSpec
	cfg    core.Config
	rt     core.Runtime
	client wandbAPI
}

func (b *wandbBackend) Spec() core.ProviderSpec { return b.spec }

func (b *wandbBackend) Warmup(ctx context.Context, req core.WarmupRequest) error {
	_ = ctx
	_ = req
	return core.Exit(2, "provider=%s does not support warmup; sandboxes are acquired per-run", providerName)
}

func (b *wandbBackend) Run(ctx context.Context, req core.RunRequest) (result core.RunResult, retErr error) {
	if err := rejectWandbRunOptions(req); err != nil {
		return core.RunResult{}, err
	}
	if len(req.Command) == 0 {
		return core.RunResult{}, core.Exit(2, "missing command")
	}
	// Credential resolution lives in the client (CRABBOX_WANDB_API_KEY →
	// cfg.Wandb.APIKey → WANDB_API_KEY plus required WANDB_ENTITY_NAME). The
	// old direct WANDB_API_KEY check here ignored the documented
	// CRABBOX_WANDB_API_KEY override.
	client, err := b.api()
	if err != nil {
		return core.RunResult{}, err
	}
	defer b.closeClientAfterOperation()
	providerScope, err := wandbProviderScope()
	if err != nil {
		return core.RunResult{}, err
	}
	started := core.ClockNow(b.rt.Clock)
	cfg := b.cfg
	image := core.Blank(strings.TrimSpace(cfg.Wandb.DefaultImage), core.WandbDefaultImageFallback)
	maxLifetime := wandbMaxLifetimeSeconds(cfg)

	sandboxID := strings.TrimSpace(req.ID)
	acquired := false
	var claim core.LeaseClaim
	if sandboxID == "" {
		fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s image=%s max_lifetime=%ds\n", providerName, image, maxLifetime)
		sb, err := client.Acquire(ctx, wandbAcquireRequest{
			Image:           image,
			MaxLifetimeSecs: maxLifetime,
			Tags:            []string{"crabbox"},
			EnvironmentVars: req.Env,
		})
		if err != nil {
			return core.RunResult{}, err
		}
		sandboxID = sb.ID
		acquired = true
		fmt.Fprintf(b.rt.Stderr, "provisioned sandbox=%s status=%s\n", sb.ID, sb.Status)
		claim, err = claimWandbSandbox(sandboxID, providerScope, cfg)
		if err != nil {
			stopCtx, cancel := context.WithTimeout(context.Background(), wandbStopTimeout)
			defer cancel()
			if stopErr := client.Stop(stopCtx, sandboxID, 10, true); stopErr != nil {
				return core.RunResult{}, fmt.Errorf("persist wandb sandbox %s ownership claim: %w (rollback stop also failed: %v)", sandboxID, err, stopErr)
			}
			return core.RunResult{}, fmt.Errorf("persist wandb sandbox %s ownership claim: %w", sandboxID, err)
		}
		if req.EnvSummary {
			core.PrintEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
		}
	} else {
		if len(req.Env) > 0 && !wandbExistingIDEnvCanBeOmitted(req) {
			// CoreWeave Sandboxes apply environment variables at Start time only;
			// the v1beta2 Exec RPC has no env field, so we can't honour
			// selected env on an already-running sandbox. Core-owned run metadata
			// may be omitted, as can the built-in implicit CI/NODE_OPTIONS defaults;
			// neither exception forwards new environment values through Exec.
			return core.RunResult{}, core.Exit(2, "provider=%s cannot forward env vars to an existing sandbox (--id); rerun without --id or omit --allow-env", providerName)
		}
		claim, sandboxID, err = requireWandbOwnership(ctx, client, sandboxID, providerScope)
		if err != nil {
			return core.RunResult{}, err
		}
	}

	// Stop semantics match the modal/e2b/islo/tensorlake sibling pattern:
	// we acquire+release per-run by default, but honour --keep (always
	// retain) and --keep-on-failure (retain only when the run fails) so
	// users can debug a sandbox after a bad command.
	shouldStop := acquired && !req.Keep
	result = core.RunResult{
		Session: &core.RunSessionHandle{
			Provider:       providerName,
			LeaseID:        sandboxID,
			Slug:           sandboxID,
			Reused:         !acquired,
			Kept:           true,
			CleanupCommand: wandbCleanupCommand(sandboxID),
		},
	}
	defer func() {
		// Finalize only after the primary command or provider failure is
		// classified; secondary diagnostics must not replace that outcome.
		result = core.FinalizeRunResult(result, retErr)
		if shouldStop {
			stopCtx, cancel := context.WithTimeout(context.Background(), wandbStopTimeout)
			cleanupErr := removeWandbClaimAfter(claim, func() error {
				return client.Stop(stopCtx, sandboxID, 10, true)
			})
			cancel()
			if cleanupErr != nil {
				result, retErr = shared.AppendDelegatedRunFailure(result, retErr, fmt.Errorf("wandb stop failed for %s: %w", sandboxID, cleanupErr), 1)
			} else {
				result.Session.Kept = false
			}
		}
		result.Total = core.ClockNow(b.rt.Clock).Sub(started)
		if req.TimingJSON {
			timingErr := core.WriteTimingJSON(b.rt.Stderr, core.TimingReportWithRunResult(core.TimingReport{
				Provider: providerName, Slug: sandboxID,
				CommandMs: result.Command.Milliseconds(), TotalMs: result.Total.Milliseconds(),
				ExitCode: result.ExitCode, Label: strings.TrimSpace(req.Label),
			}, result, retErr))
			result, retErr = shared.AppendDelegatedRunFailure(result, retErr, timingErr, core.ExitCodeForError(timingErr, 1))
		}
	}()

	commandStarted := core.ClockNow(b.rt.Clock)
	req.Observation.Phase(core.RunPhaseCommand)
	stdout, stderr := req.Observation.CommandWriters(b.rt.Stdout, b.rt.Stderr, core.RunOutputWorkload)
	var exitCode int
	var execErr error
	if err := verifyWandbClaim(claim); err != nil {
		execErr = err
	} else {
		exitCode, execErr = client.Exec(ctx, wandbExecRequest{
			SandboxID: sandboxID,
			Command:   req.Command,
			Stdout:    stdout,
			Stderr:    stderr,
		})
	}

	// Command measures just the user's exec; Total includes Acquire+poll.
	// Conflating them (the previous bug) made commandMs == totalMs on every
	// fresh-sandbox run, hiding provisioning time from --timing-json users.
	commandDuration := core.ClockNow(b.rt.Clock).Sub(commandStarted)
	result.ExitCode = exitCode
	result.Command = commandDuration

	if execErr != nil {
		result, execErr = shared.PinDelegatedRunFailure(result, execErr)
		core.HandleDelegatedRunFailure(b.rt.Stderr, req, providerName, sandboxID, sandboxID, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return result, execErr
	}
	result = core.FinalizeRunResult(result, nil)
	if result.ExitCode != 0 {
		core.HandleDelegatedRunFailure(b.rt.Stderr, req, providerName, sandboxID, sandboxID, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return result, core.ExitError{Code: result.ExitCode, Message: fmt.Sprintf("%s sandbox exit=%d", providerName, result.ExitCode)}
	}
	return result, nil
}

func wandbCleanupCommand(sandboxID string) string {
	return fmt.Sprintf("crabbox stop --provider %s --id %s", providerName, core.ShellQuote(sandboxID))
}

type wandbSandboxMissingError struct {
	sandboxID string
}

func (e *wandbSandboxMissingError) Error() string {
	return fmt.Sprintf("wandb sandbox %q is not tagged as Crabbox-managed or no longer exists", e.sandboxID)
}

func (e *wandbSandboxMissingError) As(target any) bool {
	if exitErr, ok := target.(*core.ExitError); ok {
		*exitErr = core.ExitError{Code: 4, Message: e.Error()}
		return true
	}
	return false
}

func requireWandbOwnership(ctx context.Context, client wandbAPI, identifier, providerScope string) (core.LeaseClaim, string, error) {
	claim, ok, err := resolveWandbClaim(identifier)
	if err != nil {
		return core.LeaseClaim{}, "", err
	}
	if !ok || claim.CloudID == "" {
		return core.LeaseClaim{}, "", core.Exit(4, "wandb sandbox %q has no matching local ownership claim", identifier)
	}
	if claim.ProviderScope == "" || claim.ProviderScope != providerScope {
		return core.LeaseClaim{}, "", core.Exit(4, "wandb sandbox %q ownership claim belongs to a different endpoint, entity, or project", identifier)
	}
	if err := requireWandbInventoryOwnership(ctx, client, claim.CloudID); err != nil {
		return claim, claim.CloudID, err
	}
	return claim, claim.CloudID, nil
}

func requireWandbInventoryOwnership(ctx context.Context, client wandbAPI, sandboxID string) error {
	sandboxes, err := client.List(ctx, []string{"crabbox"}, "all")
	if err != nil {
		return err
	}
	for _, sandbox := range sandboxes {
		if sandbox.ID == sandboxID {
			return nil
		}
	}
	if _, err := client.Status(ctx, sandboxID); err != nil {
		var apiErr *wandbAPIError
		if errors.As(err, &apiErr) && apiErr.Code == codes.NotFound {
			return &wandbSandboxMissingError{sandboxID: sandboxID}
		}
		return err
	}
	return core.Exit(4, "wandb sandbox %q still exists but is not tagged as Crabbox-managed", sandboxID)
}

func (b *wandbBackend) List(ctx context.Context, req core.ListRequest) ([]core.LeaseView, error) {
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
	views := make([]core.Server, 0, len(sandboxes))
	for _, sb := range sandboxes {
		views = append(views, core.Server{
			CloudID:  sb.ID,
			Provider: providerName,
			Name:     sb.ID,
			Status:   sb.Status,
			Labels:   map[string]string{"created_at": sb.CreatedAt},
		})
	}
	return views, nil
}

func (b *wandbBackend) Status(ctx context.Context, req core.StatusRequest) (core.StatusView, error) {
	sandboxID := strings.TrimSpace(req.ID)
	if sandboxID == "" {
		return core.StatusView{}, core.Exit(2, "provider=%s status requires --id <sandbox-id>", providerName)
	}
	client, err := b.api()
	if err != nil {
		return core.StatusView{}, err
	}
	defer b.closeClientAfterOperation()
	providerScope, err := wandbProviderScope()
	if err != nil {
		return core.StatusView{}, err
	}
	_, sandboxID, err = requireWandbOwnership(ctx, client, sandboxID, providerScope)
	if err != nil {
		return core.StatusView{}, err
	}
	pollCtx := ctx
	cancel := func() {}
	maxAttempts := 1
	if req.Wait {
		waitTimeout := req.WaitTimeout
		if waitTimeout <= 0 {
			waitTimeout = wandbStatusWaitTimeout
		}
		pollCtx, cancel = context.WithTimeout(ctx, waitTimeout)
		maxAttempts = 0
	}
	defer cancel()
	result, err := shared.Poll(pollCtx, maxAttempts, wandbStatusPollInterval, shared.SleepContext,
		func(ctx context.Context) (wandbSandbox, error) { return client.Status(ctx, sandboxID) },
		func(_ context.Context, sandbox wandbSandbox, fetchErr error) (bool, error) {
			if fetchErr != nil {
				return false, fetchErr
			}
			switch strings.ToLower(strings.TrimSpace(sandbox.Status)) {
			case "running", "stopped", "failed", "terminated":
				return true, nil
			default:
				return false, nil
			}
		}, nil)
	if err != nil {
		if req.Wait && errors.Is(pollCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return core.StatusView{}, core.Exit(5, "timed out waiting for wandb sandbox %s to become ready", sandboxID)
		}
		return core.StatusView{}, err
	}
	sb := result.Value
	state := strings.ToLower(strings.TrimSpace(sb.Status))
	ready := state == "running"
	return core.StatusView{
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

func (b *wandbBackend) Stop(ctx context.Context, req core.StopRequest) error {
	sandboxID := strings.TrimSpace(req.ID)
	if sandboxID == "" {
		return core.Exit(2, "provider=%s stop requires --id <sandbox-id>", providerName)
	}
	client, err := b.api()
	if err != nil {
		return err
	}
	defer b.closeClientAfterOperation()
	providerScope, err := wandbProviderScope()
	if err != nil {
		return err
	}
	claim, sandboxID, err := requireWandbOwnership(ctx, client, sandboxID, providerScope)
	if err != nil {
		var missing *wandbSandboxMissingError
		if errors.As(err, &missing) {
			return removeWandbClaimAfter(claim, func() error { return nil })
		}
		return err
	}
	return removeWandbClaimAfter(claim, func() error {
		return client.Stop(ctx, sandboxID, 10, false)
	})
}

// Doctor mirrors the modal/e2b/runpod pattern: dial, probe auth via a cheap
// authenticated RPC, list inventory, return an inventory-style result. The
// missing-credential and gRPC-unreachable cases bubble up through b.api() and
// client.Version() with their typed errors.
func (b *wandbBackend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	client, err := b.api()
	if err != nil {
		return core.DoctorResult{}, err
	}
	defer b.closeClientAfterOperation()
	if _, err := client.Version(ctx); err != nil {
		// Surface the typed *wandbAPIError as-is: errors.As() at the cli
		// boundary unwraps it into ExitError with the mapped sysexit code
		// (77 EX_NOPERM, 69 EX_UNAVAILABLE, 124 timeout, …). Wrapping with
		// exit(1, …) here would erase that code.
		return core.DoctorResult{}, err
	}
	views, err := b.List(ctx, core.ListRequest{})
	if err != nil {
		return core.DoctorResult{}, err
	}
	return core.InventoryDoctorResult(providerName, len(views)), nil
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

// applyWandbDefaults fills in interpreter / image / lifetime defaults without
// touching SSH or WorkRoot — delegated-run providers must not stomp on SSH
// config.
func applyWandbDefaults(cfg *core.Config) {
	cfg.Provider = providerName
	if cfg.TargetOS == "" {
		cfg.TargetOS = targetLinux
	}
	if cfg.Wandb.DefaultImage == "" {
		cfg.Wandb.DefaultImage = core.WandbDefaultImageFallback
	}
	if cfg.Wandb.MaxLifetimeSeconds <= 0 {
		cfg.Wandb.MaxLifetimeSeconds = core.WandbMaxLifetimeSecondsFallback
	}
}

func wandbMaxLifetimeSeconds(cfg core.Config) int {
	maxLifetime := cfg.Wandb.MaxLifetimeSeconds
	if maxLifetime <= 0 {
		maxLifetime = core.WandbMaxLifetimeSecondsFallback
	}
	if cfg.TTL > 0 {
		ttlSeconds := int((cfg.TTL + time.Second - 1) / time.Second)
		if ttlSeconds > 0 && ttlSeconds < maxLifetime {
			maxLifetime = ttlSeconds
		}
	}
	return maxLifetime
}

func rejectWandbRunOptions(req core.RunRequest) error {
	if req.Reclaim {
		return core.Exit(2, "provider=%s lifecycle is owned by W&B; --reclaim is not supported", providerName)
	}
	if !req.NoSync {
		return core.Exit(2, "provider=%s does not support workspace sync; pass --no-sync", providerName)
	}
	if req.SyncOnly {
		return core.Exit(2, "provider=%s does not support sync; --sync-only is rejected", providerName)
	}
	if req.ChecksumSync {
		return core.Exit(2, "provider=%s does not support sync; --checksum is rejected", providerName)
	}
	if req.ForceSyncLarge {
		return core.Exit(2, "provider=%s does not support sync; --force-sync-large is rejected", providerName)
	}
	if req.FullResync {
		return core.Exit(2, "provider=%s does not support sync; --full-resync is rejected", providerName)
	}
	if req.ShellMode {
		return core.Exit(2, "provider=%s does not support --shell", providerName)
	}
	// req.EnvSummary (set by --allow-env / env profiles / CRABBOX_ENV_ALLOW)
	// is intentionally NOT rejected — Run forwards the resolved req.Env into
	// the sandbox via Acquire.EnvironmentVars.
	return nil
}

func wandbExistingIDEnvCanBeOmitted(req core.RunRequest) bool {
	for name := range req.Env {
		if core.IsRunExecutionMetadataEnvName(name) {
			continue
		}
		if req.EnvSummary || (name != "CI" && name != "NODE_OPTIONS") {
			return false
		}
	}
	return true
}
