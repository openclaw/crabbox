package anthropicsandboxruntime

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type backend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func newBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	return &backend{spec: spec, cfg: cfg, rt: rt}
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) Warmup(context.Context, WarmupRequest) error {
	return exit(2, "provider=anthropic-sandbox-runtime is one-shot; use crabbox run")
}

func (b *backend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if err := rejectRunOptions(b.spec, req); err != nil {
		return RunResult{}, err
	}
	started := time.Now()
	cli, err := newSRTCLI(b.cfg, b.rt)
	if err != nil {
		return RunResult{}, err
	}
	commandText, err := buildCommandText(req.Command, req.ShellMode)
	if err != nil {
		return RunResult{}, err
	}
	if req.EnvSummary || strings.TrimSpace(os.Getenv("CRABBOX_ENV_ALLOW")) != "" {
		printEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
	}
	fmt.Fprintf(b.rt.Stderr, "provider=%s cli=%s sync_delegated=true lifecycle=one-shot\n", providerName, cli.binary())
	commandStart := time.Now()
	exitCode, runErr := cli.runCommand(ctx, req.Repo.Root, commandText, req.Env, b.rt.Stdout, b.rt.Stderr)
	commandDuration := time.Since(commandStart)
	result := RunResult{
		ExitCode:      exitCode,
		Command:       commandDuration,
		Total:         time.Since(started),
		SyncDelegated: true,
		Provider:      providerName,
		CommandText:   commandText,
	}
	result = core.FinalizeRunResult(result, runErr)
	fmt.Fprintf(b.rt.Stderr, "anthropic-sandbox-runtime run summary sync_delegated=true command=%s total=%s exit=%d\n", commandDuration.Round(time.Millisecond), result.Total.Round(time.Millisecond), exitCode)
	if req.TimingJSON {
		report := timingReportWithRunResult(timingReport{
			Provider:      providerName,
			SyncDelegated: true,
			SyncSkipped:   true,
			CommandMs:     commandDuration.Milliseconds(),
			TotalMs:       result.Total.Milliseconds(),
			ExitCode:      exitCode,
			Label:         strings.TrimSpace(req.Label),
		}, result, runErr)
		if err := writeTimingJSON(b.rt.Stderr, report); err != nil {
			return result, err
		}
	}
	if runErr != nil {
		if exitCode != 0 {
			return result, exit(exitCode, "anthropic-sandbox-runtime run failed: %v", runErr)
		}
		return result, exit(1, "anthropic-sandbox-runtime run failed: %v", runErr)
	}
	return result, nil
}

func (b *backend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, nil
}

func (b *backend) Status(context.Context, StatusRequest) (StatusView, error) {
	return StatusView{}, exit(2, "provider=anthropic-sandbox-runtime is one-shot and does not support status")
}

func (b *backend) Stop(context.Context, StopRequest) error {
	return exit(2, "provider=anthropic-sandbox-runtime is one-shot and does not support stop")
}

func (b *backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	cli, err := newSRTCLI(b.cfg, b.rt)
	if err != nil {
		return DoctorResult{}, err
	}
	result := DoctorResult{Provider: providerName}
	help, helpErr := cli.help(ctx)
	helpDetails := map[string]string{
		"cli":      cli.binary(),
		"settings": blank(strings.TrimSpace(b.cfg.AnthropicSRT.Settings), "srt default"),
		"debug":    fmt.Sprint(b.cfg.AnthropicSRT.Debug),
		"mutation": "false",
	}
	result.Checks = append(result.Checks, doctorCheck("srt_help", helpErr, helpDetails))
	version, versionErr := cli.version(ctx)
	if versionErr == nil {
		result.Checks = append(result.Checks, DoctorCheck{
			Status:  "ok",
			Check:   "srt_version",
			Message: "version reported; compatibility is based on command-surface checks",
			Details: map[string]string{"version": version, "authoritative": "false"},
		})
	} else {
		result.Checks = append(result.Checks, DoctorCheck{
			Status:  "warn",
			Check:   "srt_version",
			Message: versionErr.Error(),
			Details: map[string]string{"authoritative": "false", "optional": "true"},
		})
	}
	if helpErr != nil {
		result.Status = "error"
		result.Message = "cli=blocked control_plane=local command_surface=blocked mutation=false"
		return result, helpErr
	}
	result.Status = "ok"
	result.Message = fmt.Sprintf("cli=ready control_plane=local command_surface=ready mutation=false help=%s", firstNonEmptyLine(help))
	return result, nil
}

func rejectRunOptions(spec ProviderSpec, req RunRequest) error {
	if err := core.RejectDelegatedSyncOptionsForSpec(spec, req); err != nil {
		return err
	}
	if req.ID != "" || req.Keep || req.KeepOnFailure || strings.TrimSpace(req.RequestedSlug) != "" {
		return exit(2, "provider=anthropic-sandbox-runtime is one-shot and does not support persistent lease ids")
	}
	if req.Options.Desktop || req.Options.Browser || req.Options.Code {
		return exit(2, "provider=anthropic-sandbox-runtime does not support desktop, browser, or code-server options")
	}
	if req.Options.Tailscale.Enabled {
		return exit(2, "provider=anthropic-sandbox-runtime is delegated-run only and does not support Tailscale options")
	}
	if !req.ApplyLocalPatch && strings.TrimSpace(req.Repo.Root) == "" {
		return exit(2, "provider=anthropic-sandbox-runtime requires a local workspace")
	}
	return nil
}

func buildCommandText(command []string, shellMode bool) (string, error) {
	if len(command) == 0 {
		return "", exit(2, "missing command")
	}
	if shellMode {
		return strings.Join(command, " "), nil
	}
	if len(command) == 1 && shouldUseShell(command) {
		return command[0], nil
	}
	if shouldUseShell(command) || leadingEnvAssignment(command) {
		return shellScriptFromArgv(command), nil
	}
	return shellScriptFromArgv(command), nil
}

func doctorCheck(name string, err error, details map[string]string) DoctorCheck {
	if err != nil {
		return DoctorCheck{Status: "error", Check: name, Message: err.Error(), Details: details}
	}
	return DoctorCheck{Status: "ok", Check: name, Message: "ready", Details: details}
}
