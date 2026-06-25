package flue

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string      { return providerName }
func (Provider) Aliases() []string { return nil }

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:        providerName,
		Family:      providerName,
		Kind:        core.ProviderKindDelegatedRun,
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
		Features:    core.FeatureSet{core.FeatureArchiveSync},
		Coordinator: core.CoordinatorNever,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterFlueProviderFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyFlueProviderFlags(cfg, fs, values)
}

func (Provider) ValidateConfig(cfg core.Config) error {
	return ValidateFlueConfig(cfg)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if err := p.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	return &backend{spec: p.Spec(), cfg: cfg, rt: rt}, nil
}

type backend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) Warmup(context.Context, WarmupRequest) error {
	return exit(2, "provider=%s is one-shot; use crabbox run", providerName)
}

func (b *backend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if err := rejectFlueRunOptions(b.spec, b.cfg, req); err != nil {
		return RunResult{}, err
	}
	started := b.now()
	leaseID := newLeaseID()
	slug := newLeaseSlug(leaseID)
	if requested := strings.TrimSpace(req.RequestedSlug); requested != "" {
		slug = normalizeLeaseSlug(requested)
	}
	commandText, err := flueCommandText(req.Command, req.ShellMode)
	if err != nil {
		return RunResult{}, err
	}
	cli, err := newFlueCLI(b.cfg, b.rt)
	if err != nil {
		return RunResult{}, err
	}
	payload, err := buildFlueRunPayload(ctx, b.cfg, req, leaseID, slug, started, b.rt.Stderr, b.now)
	if err != nil {
		return RunResult{}, err
	}
	defer payload.Cleanup()
	if req.EnvSummary || len(req.Env) > 0 {
		printEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded in protocol request file", req.Options.EnvAllow, req.Env)
	}
	fmt.Fprintf(b.rt.Stderr, "provider=%s cli=%s target=node workflow=%s sync_delegated=true lifecycle=one-shot\n", providerName, blank(strings.TrimSpace(b.cfg.Flue.CLIPath), defaultCLIPath), workflowSelector(b.cfg.Flue.Workflow))
	commandStarted := b.now()
	run, runErr := cli.run(ctx, payload.RequestFile, flueEnvRedactions(req.Env))
	commandDuration := b.now().Sub(commandStarted)
	if run.Response.Timing.RunMs > 0 {
		commandDuration = time.Duration(run.Response.Timing.RunMs) * time.Millisecond
	}
	exitCode := run.Response.ExitCode
	result := RunResult{
		ExitCode:      exitCode,
		Command:       commandDuration,
		Total:         b.now().Sub(started),
		SyncDelegated: true,
		Provider:      providerName,
		LeaseID:       leaseID,
		Slug:          slug,
		CommandText:   commandText,
	}
	if runErr != nil {
		if exitCode == 0 {
			exitCode = flueProcessExitCode(run.Raw.ExitCode)
			result.ExitCode = exitCode
		}
		result = finalizeRunResult(result, runErr)
		_ = b.writeTiming(req, result, payload, commandDuration, runErr)
		return result, runErr
	}
	if run.Response.Stdout != "" {
		_, _ = io.WriteString(b.rt.Stdout, run.Response.Stdout)
	}
	if run.Response.Stderr != "" {
		_, _ = io.WriteString(b.rt.Stderr, run.Response.Stderr)
	}
	var resultErr error
	if strings.TrimSpace(run.Response.Error) != "" && exitCode == 0 {
		resultErr = exit(1, "flue delegated run failed: %s", redactFlueDetail(run.Response.Error, flueEnvRedactions(req.Env)))
		result.ExitCode = 1
	} else if exitCode != 0 {
		message := fmt.Sprintf("flue delegated command exited %d", exitCode)
		if strings.TrimSpace(run.Response.Error) != "" {
			message += ": " + redactFlueDetail(run.Response.Error, flueEnvRedactions(req.Env))
		}
		resultErr = exit(exitCode, "%s", message)
	}
	result = finalizeRunResult(result, resultErr)
	if err := b.writeTiming(req, result, payload, commandDuration, resultErr); err != nil {
		return result, err
	}
	fmt.Fprintf(b.rt.Stderr, "flue run summary sync=%s command=%s total=%s exit=%d\n", payload.SyncTotal.Round(time.Millisecond), commandDuration.Round(time.Millisecond), result.Total.Round(time.Millisecond), result.ExitCode)
	if resultErr != nil {
		return result, resultErr
	}
	return result, nil
}

func (b *backend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, nil
}

func (b *backend) Status(context.Context, StatusRequest) (StatusView, error) {
	return StatusView{}, exit(2, "provider=%s is one-shot and does not support status", providerName)
}

func (b *backend) Stop(context.Context, StopRequest) error {
	return exit(2, "provider=%s is one-shot and does not support stop", providerName)
}

func (b *backend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}

func (b *backend) writeTiming(req RunRequest, result RunResult, payload flueRunPayload, commandDuration time.Duration, runErr error) error {
	if !req.TimingJSON {
		return nil
	}
	return writeTimingJSON(b.rt.Stderr, timingReportWithRunResult(timingReport{
		Provider:      providerName,
		LeaseID:       result.LeaseID,
		Slug:          result.Slug,
		SyncDelegated: true,
		SyncMs:        durationMillis(payload.SyncTotal),
		SyncPhases:    payload.SyncPhases,
		CommandMs:     durationMillis(commandDuration),
		TotalMs:       durationMillis(result.Total),
		ExitCode:      result.ExitCode,
		Label:         strings.TrimSpace(req.Label),
		Workdir:       payload.Request.Workspace,
	}, result, runErr))
}

func rejectFlueRunOptions(spec ProviderSpec, cfg Config, req RunRequest) error {
	if err := ValidateFlueConfig(cfg); err != nil {
		return err
	}
	if err := rejectDelegatedSyncOptionsForSpec(spec, req); err != nil {
		return err
	}
	if len(req.Command) == 0 {
		return exit(2, "missing command")
	}
	if req.ID != "" || req.Keep || req.KeepOnFailure {
		return exit(2, "provider=%s is one-shot and does not support persistent lease ids", providerName)
	}
	if req.NoSync {
		return exit(2, "provider=%s requires archive sync; --no-sync is not supported", providerName)
	}
	if req.SyncOnly {
		return exit(2, "provider=%s requires a Flue workflow command; --sync-only is not supported", providerName)
	}
	if req.Options.Desktop || req.Options.Browser || req.Options.Code {
		return exit(2, "provider=%s does not support desktop, browser, or code-server options", providerName)
	}
	if req.Options.Tailscale.Enabled {
		return exit(2, "provider=%s is delegated-run only and does not support Tailscale options", providerName)
	}
	if req.ApplyLocalPatch {
		return exit(2, "provider=%s delegates sync; --apply-local-patch is not supported", providerName)
	}
	return nil
}
