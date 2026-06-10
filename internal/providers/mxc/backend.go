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
)

var windowsBuildPattern = regexp.MustCompile(`(?m)CurrentBuildNumber\s+REG_SZ\s+(\d+)`)

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
func (b *backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	if err := requireSupportedWindows(); err != nil {
		return DoctorResult{}, err
	}
	path, err := exec.LookPath(defaultString(b.cfg.MXC.CLIPath, "wxc-exec.exe"))
	if err != nil {
		return DoctorResult{}, exit(3, "MXC executor not found: %v", err)
	}
	if err := b.smokeTest(ctx, path); err != nil {
		return DoctorResult{}, err
	}
	return DoctorResult{Provider: providerName, Message: fmt.Sprintf("cli=ready control_plane=local executor=%s containment=%s version=%s network=%s", path, b.cfg.MXC.Containment, b.cfg.MXC.Version, b.cfg.MXC.Network)}, nil
}

func (b *backend) smokeTest(ctx context.Context, path string) error {
	config, configDir, cleanupConfig, err := buildIsolatedConfig(b.cfg, RunRequest{Command: []string{"cmd.exe", "/d", "/c", "exit", "0"}, Options: LeaseOptions{TTL: 30 * time.Second}})
	if err != nil {
		return err
	}
	defer cleanupConfig()
	configPath, cleanup, err := writeConfigFile(configDir, config)
	if err != nil {
		return exit(3, "write MXC doctor config: %v", err)
	}
	defer cleanup()
	args := []string{}
	if b.cfg.MXC.Experimental {
		args = append(args, "--experimental")
	}
	args = append(args, configPath)
	result, runErr := b.rt.Exec.Run(ctx, LocalCommandRequest{Name: path, Args: args})
	if runErr != nil {
		return exit(3, "MXC runtime unavailable: %s", failureDetail(result, runErr))
	}
	return nil
}
func (b *backend) Warmup(context.Context, WarmupRequest) error {
	return exit(2, "provider=mxc is one-shot; use crabbox run")
}
func (b *backend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if err := requireSupportedWindows(); err != nil {
		return RunResult{}, err
	}
	if req.ID != "" || req.Keep || req.KeepOnFailure {
		return RunResult{}, exit(2, "provider=mxc is one-shot and does not support persistent lease ids")
	}
	if req.SyncOnly || req.ApplyLocalPatch || req.FreshPR.Number > 0 {
		return RunResult{}, exit(2, "provider=mxc executes the local checkout directly; explicit sync and patch preparation are not supported")
	}
	if req.EnvSummary || strings.TrimSpace(os.Getenv("CRABBOX_ENV_ALLOW")) != "" {
		printEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
	}
	started := time.Now()
	config, configDir, cleanupConfig, err := buildIsolatedConfig(b.cfg, req)
	if err != nil {
		return RunResult{}, err
	}
	defer cleanupConfig()
	path, cleanup, err := writeConfigFile(configDir, config)
	if err != nil {
		return RunResult{}, exit(2, "write MXC config: %v", err)
	}
	defer cleanup()
	args := []string{}
	if b.cfg.MXC.Experimental {
		args = append(args, "--experimental")
	}
	args = append(args, path)
	commandStarted := time.Now()
	result, runErr := b.rt.Exec.Run(ctx, LocalCommandRequest{Name: defaultString(b.cfg.MXC.CLIPath, "wxc-exec.exe"), Args: args, Dir: req.Repo.Root, Stdout: b.rt.Stdout, Stderr: b.rt.Stderr})
	out := RunResult{ExitCode: result.ExitCode, Command: time.Since(commandStarted), Total: time.Since(started), SyncDelegated: true, Provider: providerName, CommandText: strings.Join(req.Command, " ")}
	if req.TimingJSON {
		_ = writeTimingJSON(b.rt.Stderr, timingReport{Provider: providerName, CommandMs: out.Command.Milliseconds(), TotalMs: out.Total.Milliseconds(), ExitCode: out.ExitCode, SyncDelegated: true, SyncSkipped: true})
	}
	if runErr != nil {
		detail := strings.TrimSpace(result.Stderr)
		if detail == "" {
			detail = runErr.Error()
		}
		return out, exit(result.ExitCode, "MXC command failed: %s", detail)
	}
	return out, nil
}
func (b *backend) List(context.Context, ListRequest) ([]LeaseView, error) { return nil, nil }
func (b *backend) Status(context.Context, StatusRequest) (StatusView, error) {
	return StatusView{}, exit(2, "provider=mxc is one-shot and does not support status")
}
func (b *backend) Stop(context.Context, StopRequest) error {
	return exit(2, "provider=mxc is one-shot and does not support stop")
}
func requireSupportedWindows() error {
	if runtime.GOOS != "windows" {
		return exit(2, "provider=mxc requires a Windows host")
	}
	output, err := exec.Command("reg.exe", "query", `HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion`, "/v", "CurrentBuildNumber").CombinedOutput()
	if err != nil {
		return exit(3, "detect Windows build: %v", err)
	}
	build, err := parseWindowsBuild(string(output))
	if err != nil {
		return exit(3, "%v", err)
	}
	if build < 26100 {
		return exit(2, "provider=mxc requires Windows build 26100 or newer; detected build %d", build)
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

func failureDetail(result LocalCommandResult, err error) string {
	if detail := strings.TrimSpace(result.Stderr); detail != "" {
		return detail
	}
	if detail := strings.TrimSpace(result.Stdout); detail != "" {
		return detail
	}
	return err.Error()
}
