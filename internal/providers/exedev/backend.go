package exedev

import (
	"context"
	"fmt"
	"strings"
	"time"
)

func NewExeDevBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	return &exeDevBackend{spec: spec, cfg: cfg, rt: rt}
}

type exeDevBackend struct {
	spec   ProviderSpec
	cfg    Config
	rt     Runtime
	client exeDevAPI
}

func (b *exeDevBackend) Spec() ProviderSpec { return b.spec }

func (b *exeDevBackend) Warmup(ctx context.Context, req WarmupRequest) error {
	_ = ctx
	_ = req
	return exit(2, "provider=%s does not support warmup; exe.dev exec is stateless", providerName)
}

func (b *exeDevBackend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if err := rejectExeDevRunOptions(req); err != nil {
		return RunResult{}, err
	}
	if len(req.Command) == 0 {
		return RunResult{}, exit(2, "missing command")
	}
	client, err := b.api()
	if err != nil {
		return RunResult{}, err
	}
	command := exeDevCommandString(req.Command, req.ShellMode)
	if command == "" {
		return RunResult{}, exit(2, "missing command")
	}
	started := b.now()
	fmt.Fprintf(b.rt.Stderr, "running on %s %s\n", providerName, strings.Join(req.Command, " "))
	exitCode, execErr := client.Exec(ctx, command, b.rt.Stdout, b.rt.Stderr)
	commandDuration := b.now().Sub(started)
	result := RunResult{
		ExitCode: exitCode,
		Command:  commandDuration,
		Total:    commandDuration,
	}
	fmt.Fprintf(b.rt.Stderr, "%s run summary command=%s total=%s exit=%d\n", providerName, result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), result.ExitCode)
	if req.TimingJSON {
		if err := writeTimingJSON(b.rt.Stderr, timingReport{
			Provider:  providerName,
			CommandMs: commandDuration.Milliseconds(),
			TotalMs:   result.Total.Milliseconds(),
			ExitCode:  result.ExitCode,
		}); err != nil {
			return result, err
		}
	}
	if execErr != nil {
		return result, ExitError{Code: 1, Message: fmt.Sprintf("%s run failed: %v", providerName, execErr)}
	}
	if result.ExitCode != 0 {
		return result, ExitError{Code: result.ExitCode, Message: fmt.Sprintf("%s run exited %d", providerName, result.ExitCode)}
	}
	return result, nil
}

func (b *exeDevBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = ctx
	_ = req
	// exe.dev exec is stateless: there are no sandboxes to enumerate.
	return nil, nil
}

func (b *exeDevBackend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	_ = ctx
	_ = req
	return StatusView{}, exit(2, "provider=%s does not support status; exe.dev exec is stateless", providerName)
}

func (b *exeDevBackend) Stop(ctx context.Context, req StopRequest) error {
	_ = ctx
	_ = req
	return exit(2, "provider=%s does not support stop; exe.dev exec is stateless", providerName)
}

func (b *exeDevBackend) api() (exeDevAPI, error) {
	if b.client != nil {
		return b.client, nil
	}
	return newExeDevClient(b.cfg, b.rt)
}

func exeDevCommandString(command []string, shellMode bool) string {
	if len(command) == 0 {
		return ""
	}
	if shellMode {
		return strings.Join(command, " ")
	}
	if shouldUseShell(command) || leadingEnvAssignment(command) {
		return shellScriptFromArgv(command)
	}
	return strings.Join(shellWords(command), " ")
}

func rejectExeDevRunOptions(req RunRequest) error {
	if req.ID != "" {
		return exit(2, "provider=%s does not maintain sandboxes; --id is not supported", providerName)
	}
	if req.Keep {
		return exit(2, "provider=%s does not maintain sandboxes; --keep is not supported", providerName)
	}
	if req.Reclaim {
		return exit(2, "provider=%s does not maintain sandboxes; --reclaim is not supported", providerName)
	}
	if !req.NoSync {
		// exe.dev provides no sync surface (no upload endpoint documented).
		// Mirror other delegated-only providers that do not implement archive
		// sync by requiring --no-sync explicitly.
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
	return nil
}

func (b *exeDevBackend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}
