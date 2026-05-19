package wandb

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

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

const minCwsandboxVersion = "0.20.0"

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
	if strings.TrimSpace(os.Getenv("WANDB_API_KEY")) == "" {
		return RunResult{}, exit(2, "provider=%s requires WANDB_API_KEY", providerName)
	}
	client, err := b.api()
	if err != nil {
		return RunResult{}, err
	}
	started := b.now()
	cfg := b.cfg
	image := blank(strings.TrimSpace(cfg.Wandb.DefaultImage), "ubuntu:24.04")
	maxLifetime := cfg.Wandb.MaxLifetimeSeconds
	if maxLifetime <= 0 {
		maxLifetime = 1800
	}

	sandboxID := strings.TrimSpace(req.ID)
	acquired := false
	if sandboxID == "" {
		fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s image=%s max_lifetime=%ds\n", providerName, image, maxLifetime)
		sb, err := client.Acquire(ctx, wandbAcquireRequest{
			Image:           image,
			MaxLifetimeSecs: maxLifetime,
			Tags:            []string{"crabbox"},
		})
		if err != nil {
			return RunResult{}, err
		}
		sandboxID = sb.ID
		acquired = true
		fmt.Fprintf(b.rt.Stderr, "provisioned sandbox=%s status=%s\n", sb.ID, sb.Status)
	}

	exitCode, execErr := client.Exec(ctx, wandbExecRequest{
		SandboxID: sandboxID,
		Command:   req.Command,
		Stdout:    b.rt.Stdout,
		Stderr:    b.rt.Stderr,
	})
	commandDuration := b.now().Sub(started)

	if acquired && !req.Keep {
		if err := client.Stop(context.Background(), sandboxID, 10, true); err != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: wandb stop failed for %s: %v\n", sandboxID, err)
		}
	}

	result := RunResult{ExitCode: exitCode, Command: commandDuration, Total: commandDuration}
	if execErr != nil {
		return result, execErr
	}
	if result.ExitCode != 0 {
		return result, ExitError{Code: result.ExitCode, Message: fmt.Sprintf("%s sandbox exit=%d", providerName, result.ExitCode)}
	}
	return result, nil
}

func (b *wandbBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	client, err := b.api()
	if err != nil {
		return nil, err
	}
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
	return client.Stop(ctx, req.ID, 10, true)
}

func (b *wandbBackend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	client, err := b.api()
	if err != nil {
		return DoctorResult{}, err
	}
	version, err := client.Version(ctx)
	if err != nil {
		return DoctorResult{}, exit(2, "provider=%s shim not runnable: %v (install with `pip install 'wandb[sandbox]'`)", providerName, err)
	}
	if !versionMeetsMinimum(version, minCwsandboxVersion) {
		return DoctorResult{}, exit(2, "provider=%s cwsandbox %s is too old, need >= %s", providerName, version, minCwsandboxVersion)
	}
	if strings.TrimSpace(os.Getenv("WANDB_API_KEY")) == "" {
		return DoctorResult{}, exit(2, "provider=%s requires WANDB_API_KEY; set it before running `crabbox doctor`", providerName)
	}
	views, err := b.List(ctx, ListRequest{})
	if err != nil {
		return DoctorResult{}, err
	}
	return cliDoctorResult(providerName, len(views), "cwsandbox="+version), nil
}

func (b *wandbBackend) api() (wandbAPI, error) {
	if b.client != nil {
		return b.client, nil
	}
	return newWandbClient(b.cfg, b.rt)
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
	if cfg.Wandb.Python == "" {
		cfg.Wandb.Python = "python3"
	}
	if cfg.Wandb.DefaultImage == "" {
		cfg.Wandb.DefaultImage = "ubuntu:24.04"
	}
	if cfg.Wandb.MaxLifetimeSeconds <= 0 {
		cfg.Wandb.MaxLifetimeSeconds = 1800
	}
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
	if req.EnvSummary {
		return exit(2, "provider=%s cannot forward per-run environment variables", providerName)
	}
	return nil
}

// versionMeetsMinimum compares dotted version strings (e.g. "0.23.0" vs
// "0.20.0"). Non-numeric segments are treated as 0.
func versionMeetsMinimum(have, want string) bool {
	if strings.TrimSpace(have) == "" {
		return false
	}
	haveParts := splitVersion(have)
	wantParts := splitVersion(want)
	for i := 0; i < len(wantParts); i++ {
		h := 0
		if i < len(haveParts) {
			h = haveParts[i]
		}
		if h > wantParts[i] {
			return true
		}
		if h < wantParts[i] {
			return false
		}
	}
	return true
}

func splitVersion(v string) []int {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n := 0
		_ = json.Unmarshal([]byte(p), &n)
		out = append(out, n)
	}
	return out
}
