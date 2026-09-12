package mxc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

var windowsBuildPattern = regexp.MustCompile(`(?m)CurrentBuildNumber\s+REG_SZ\s+(\d+)`)

type backend struct {
	spec core.ProviderSpec
	cfg  core.Config
	rt   core.Runtime
}

func newBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	cfg.Provider = providerName
	return &backend{spec: spec, cfg: cfg, rt: rt}
}
func (b *backend) Spec() core.ProviderSpec { return b.spec }
func (b *backend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	if err := requireSupportedWindows(); err != nil {
		return core.DoctorResult{}, err
	}
	path, err := exec.LookPath(defaultString(b.cfg.MXC.CLIPath, "wxc-exec.exe"))
	if err != nil {
		return core.DoctorResult{}, core.Exit(3, "MXC executor not found: %v", err)
	}
	if err := b.smokeTest(ctx, path); err != nil {
		return core.DoctorResult{}, err
	}
	return core.DoctorResult{Provider: providerName, Message: fmt.Sprintf("cli=ready control_plane=local executor=%s containment=%s version=%s network=%s", path, b.cfg.MXC.Containment, b.cfg.MXC.Version, b.cfg.MXC.Network)}, nil
}

func (b *backend) smokeTest(ctx context.Context, path string) error {
	config, configDir, cleanupConfig, err := buildIsolatedConfig(b.cfg, core.RunRequest{Command: []string{"cmd.exe", "/d", "/c", "exit", "0"}, Options: core.LeaseOptions{TTL: 30 * time.Second}})
	if err != nil {
		return err
	}
	defer cleanupConfig()
	configPath, cleanup, err := writeConfigFile(configDir, config)
	if err != nil {
		return core.Exit(3, "write MXC doctor config: %v", err)
	}
	defer cleanup()
	args := []string{}
	if b.cfg.MXC.Experimental {
		args = append(args, "--experimental")
	}
	args = append(args, configPath)
	result, runErr := b.rt.Exec.Run(ctx, core.LocalCommandRequest{Name: path, Args: args})
	if runErr != nil {
		return core.Exit(3, "MXC runtime unavailable: %s", failureDetail(result, runErr))
	}
	return nil
}
func (b *backend) Warmup(context.Context, core.WarmupRequest) error {
	return core.Exit(2, "provider=mxc is one-shot; use crabbox run")
}
func (b *backend) Run(ctx context.Context, req core.RunRequest) (core.RunResult, error) {
	if err := requireSupportedWindows(); err != nil {
		return core.RunResult{}, err
	}
	if req.ID != "" || req.Keep || req.KeepOnFailure {
		return core.RunResult{}, core.Exit(2, "provider=mxc is one-shot and does not support persistent lease ids")
	}
	if req.SyncOnly || req.ApplyLocalPatch || req.FreshPR.Number > 0 {
		return core.RunResult{}, core.Exit(2, "provider=mxc executes the local checkout directly; explicit sync and patch preparation are not supported")
	}
	if req.EnvSummary || strings.TrimSpace(os.Getenv("CRABBOX_ENV_ALLOW")) != "" {
		core.PrintEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
	}
	started := time.Now()
	config, configDir, cleanupConfig, err := buildIsolatedConfig(b.cfg, req)
	if err != nil {
		return core.RunResult{}, err
	}
	defer cleanupConfig()
	path, cleanup, err := writeConfigFile(configDir, config)
	if err != nil {
		return core.RunResult{}, core.Exit(2, "write MXC config: %v", err)
	}
	defer cleanup()
	args := []string{}
	if b.cfg.MXC.Experimental {
		args = append(args, "--experimental")
	}
	args = append(args, path)
	commandStarted := time.Now()
	req.Observation.Phase(core.RunPhaseCommand)
	stdout, stderr := req.Observation.CommandWriters(b.rt.Stdout, b.rt.Stderr, core.RunOutputProvider)
	result, runErr := b.rt.Exec.Run(ctx, core.LocalCommandRequest{Name: defaultString(b.cfg.MXC.CLIPath, "wxc-exec.exe"), Args: args, Dir: req.Repo.Root, Stdout: stdout, Stderr: stderr})
	out := core.RunResult{ExitCode: result.ExitCode, Command: time.Since(commandStarted), Total: time.Since(started), SyncDelegated: true, Provider: providerName, CommandText: strings.Join(req.Command, " ")}
	if req.TimingJSON {
		_ = core.WriteTimingJSON(b.rt.Stderr, core.TimingReportWithRunResult(core.TimingReport{Provider: providerName, CommandMs: out.Command.Milliseconds(), TotalMs: out.Total.Milliseconds(), ExitCode: out.ExitCode, SyncDelegated: true, SyncSkipped: true}, out, runErr))
	}
	if runErr != nil {
		detail := strings.TrimSpace(result.Stderr)
		if detail == "" {
			detail = runErr.Error()
		}
		return out, core.Exit(result.ExitCode, "MXC command failed: %s", detail)
	}
	return out, nil
}
func (b *backend) List(context.Context, core.ListRequest) ([]core.LeaseView, error) { return nil, nil }
func (b *backend) Status(context.Context, core.StatusRequest) (core.StatusView, error) {
	return core.StatusView{}, core.Exit(2, "provider=mxc is one-shot and does not support status")
}
func (b *backend) Stop(context.Context, core.StopRequest) error {
	return core.Exit(2, "provider=mxc is one-shot and does not support stop")
}
func requireSupportedWindows() error {
	if runtime.GOOS != "windows" {
		return core.Exit(2, "provider=mxc requires a Windows host")
	}
	output, err := exec.Command("reg.exe", "query", `HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion`, "/v", "CurrentBuildNumber").CombinedOutput()
	if err != nil {
		return core.Exit(3, "detect Windows build: %v", err)
	}
	build, err := parseWindowsBuild(string(output))
	if err != nil {
		return core.Exit(3, "%v", err)
	}
	if build < 26100 {
		return core.Exit(2, "provider=mxc requires Windows build 26100 or newer; detected build %d", build)
	}
	return nil
}

func parseWindowsBuild(output string) (int, error) {
	match := windowsBuildPattern.FindStringSubmatch(output)
	if len(match) != 2 {
		return 0, fmt.Errorf("could not parse CurrentBuildNumber")
	}
	build, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, fmt.Errorf("parse CurrentBuildNumber: %w", err)
	}
	return build, nil
}

func failureDetail(result core.LocalCommandResult, err error) string {
	if detail := strings.TrimSpace(result.Stderr); detail != "" {
		return detail
	}
	if detail := strings.TrimSpace(result.Stdout); detail != "" {
		return detail
	}
	return err.Error()
}
