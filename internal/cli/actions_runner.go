package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

const (
	defaultActionsWaitTimeout    = 20 * time.Minute
	actionsRunnerStagePrefix     = "crabbox-runner-stage="
	actionsRunnerDiagnosticLimit = 16 * 1024
	maxActionsDiagnosticReserve  = 30 * time.Second
)

type actionsRunnerSetupResult struct {
	LastStage   string
	Diagnostics string
}

type githubRunnerHydrationHooks struct {
	Register     func(context.Context) error
	ClearState   func(context.Context) error
	Inputs       func(context.Context) (map[string]bool, bool, error)
	Dispatch     func(context.Context, []string) error
	OnDispatched func()
	Wait         func(context.Context, string) (actionsHydrationState, error)
}

func runGitHubRunnerHydration(
	ctx context.Context,
	workflow, ref, configuredJob string,
	fields []string,
	stderr io.Writer,
	hooks githubRunnerHydrationHooks,
) (actionsHydrationState, error) {
	if err := hooks.Register(ctx); err != nil {
		return actionsHydrationState{}, err
	}
	if err := ctx.Err(); err != nil {
		return actionsHydrationState{}, err
	}
	if err := hooks.ClearState(ctx); err != nil {
		return actionsHydrationState{}, err
	}
	if err := ctx.Err(); err != nil {
		return actionsHydrationState{}, err
	}
	if inputs, ok, err := hooks.Inputs(ctx); err != nil {
		fmt.Fprintf(stderr, "warning: inspect workflow inputs failed: %v\n", err)
	} else if ok {
		filtered, dropped := filterWorkflowInputs(fields, inputs)
		for _, field := range dropped {
			fmt.Fprintf(stderr, "warning: workflow %s does not declare input %s; omitting it\n", workflow, fieldName(field))
		}
		fields = filtered
		for _, required := range []string{"crabbox_id", "crabbox_runner_label", "crabbox_keep_alive_minutes"} {
			if !inputs[required] {
				return actionsHydrationState{}, Exit(2, "workflow %s at %s does not declare required hydrate input %s", workflow, ref, required)
			}
		}
	}
	expectedJob := configuredJob
	if !workflowFieldsContain(fields, "crabbox_job") {
		expectedJob = ""
	}
	if err := ctx.Err(); err != nil {
		return actionsHydrationState{}, err
	}
	if err := hooks.Dispatch(ctx, fields); err != nil {
		if expectedJob != "" && strings.Contains(err.Error(), "Unexpected input") {
			if err := ctx.Err(); err != nil {
				return actionsHydrationState{}, err
			}
			fields = dropWorkflowField(fields, "crabbox_job")
			expectedJob = ""
			fmt.Fprintln(stderr, "warning: retrying workflow dispatch without crabbox_job for compatibility")
			if retryErr := hooks.Dispatch(ctx, fields); retryErr != nil {
				return actionsHydrationState{}, retryErr
			}
		} else {
			return actionsHydrationState{}, err
		}
	}
	if hooks.OnDispatched != nil {
		hooks.OnDispatched()
	}
	if err := ctx.Err(); err != nil {
		return actionsHydrationState{}, err
	}
	return hooks.Wait(ctx, expectedJob)
}

type actionsRunnerSetupProgress struct {
	mu          sync.Mutex
	output      io.Writer
	partial     string
	lastStage   string
	diagnostics string
}

func newActionsRunnerSetupProgress(output io.Writer) *actionsRunnerSetupProgress {
	return &actionsRunnerSetupProgress{
		output:    output,
		lastStage: "bootstrap",
	}
}

func (p *actionsRunnerSetupProgress) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.partial += string(data)
	for {
		index := strings.IndexByte(p.partial, '\n')
		if index < 0 {
			break
		}
		p.consumeLineLocked(p.partial[:index])
		p.partial = p.partial[index+1:]
	}
	if len(p.partial) > actionsRunnerDiagnosticLimit {
		p.appendDiagnosticLocked(p.partial)
		p.partial = ""
	}
	return len(data), nil
}

func (p *actionsRunnerSetupProgress) Flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.partial != "" {
		p.consumeLineLocked(p.partial)
		p.partial = ""
	}
}

func (p *actionsRunnerSetupProgress) Result() actionsRunnerSetupResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return actionsRunnerSetupResult{
		LastStage:   p.lastStage,
		Diagnostics: strings.TrimSpace(p.diagnostics),
	}
}

func (p *actionsRunnerSetupProgress) consumeLineLocked(line string) {
	line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
	if line == "" {
		return
	}
	if stage, ok := strings.CutPrefix(line, actionsRunnerStagePrefix); ok {
		stage = sanitizeActionsRunnerStage(stage)
		if stage == "" {
			stage = "unknown"
		}
		p.lastStage = stage
		fmt.Fprintf(p.output, "actions runner setup stage=%s\n", stage)
		return
	}
	p.appendDiagnosticLocked(line + "\n")
}

func (p *actionsRunnerSetupProgress) appendDiagnosticLocked(value string) {
	p.diagnostics += value
	if len(p.diagnostics) > actionsRunnerDiagnosticLimit {
		p.diagnostics = p.diagnostics[len(p.diagnostics)-actionsRunnerDiagnosticLimit:]
	}
}

func sanitizeActionsRunnerStage(value string) string {
	var stage strings.Builder
	for _, r := range strings.TrimSpace(value) {
		switch {
		case r >= 'a' && r <= 'z':
			stage.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			stage.WriteRune(r)
		case r >= '0' && r <= '9':
			stage.WriteRune(r)
		case r == '-', r == '_', r == '.':
			stage.WriteRune(r)
		default:
			stage.WriteByte('-')
		}
	}
	return strings.Trim(stage.String(), "-")
}

func runActionsRunnerSetup(ctx context.Context, output io.Writer, run func(stdout, stderr io.Writer) error) (actionsRunnerSetupResult, error) {
	progress := newActionsRunnerSetupProgress(output)
	fmt.Fprintln(output, "actions runner setup stage=bootstrap")
	err := run(progress, progress)
	progress.Flush()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return progress.Result(), err
}

type githubActionsRunnerState struct {
	Found  bool
	Status string
	Busy   bool
}

type githubActionsRunnerLookup func(context.Context) (githubActionsRunnerState, error)

func githubActionsRunnerStateForName(ctx context.Context, repo GitHubRepo, name string, childEnvDenylist []string) (githubActionsRunnerState, error) {
	out, err := ghOutputWithChildEnvironment(
		ctx,
		"",
		childEnvDenylist,
		"api",
		"--paginate",
		"repos/"+repo.Slug()+"/actions/runners?per_page=100",
		"--jq",
		`.runners[] | [.name, .status, (.busy | tostring)] | @tsv`,
	)
	if err != nil {
		return githubActionsRunnerState{}, err
	}
	return parseGitHubActionsRunnerState(out, name), nil
}

func parseGitHubActionsRunnerState(output, name string) githubActionsRunnerState {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) != 3 || fields[0] != name {
			continue
		}
		return githubActionsRunnerState{
			Found:  true,
			Status: fields[1],
			Busy:   strings.EqualFold(fields[2], "true"),
		}
	}
	return githubActionsRunnerState{}
}

func waitForGitHubActionsRunnerOnline(
	ctx context.Context,
	repo GitHubRepo,
	name string,
	interval time.Duration,
	lookup githubActionsRunnerLookup,
	stderr io.Writer,
) error {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return waitForGitHubActionsRunnerOnlineWithPoll(ctx, repo, name, lookup, stderr, func(ctx context.Context) error {
		return waitForActionsPoll(ctx, interval)
	})
}

func waitForActionsPoll(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func waitForGitHubActionsRunnerOnlineWithPoll(
	ctx context.Context,
	repo GitHubRepo,
	name string,
	lookup githubActionsRunnerLookup,
	stderr io.Writer,
	poll func(context.Context) error,
) error {
	var lastState githubActionsRunnerState
	var lastErr error
	for {
		state, err := lookup(ctx)
		if err == nil {
			lastState = state
			lastErr = nil
			if ctx.Err() == nil && state.Found && strings.EqualFold(state.Status, "online") && !state.Busy {
				fmt.Fprintf(stderr, "actions runner online repo=%s name=%s busy=%t\n", repo.Slug(), name, state.Busy)
				return nil
			}
		} else {
			lastErr = err
		}

		status := "missing"
		if lastState.Found {
			status = blank(lastState.Status, "unknown")
		}
		if lastErr != nil {
			fmt.Fprintf(stderr, "waiting for actions runner online repo=%s name=%s error=%q\n", repo.Slug(), name, lastErr.Error())
		} else {
			fmt.Fprintf(stderr, "waiting for actions runner online and idle repo=%s name=%s status=%s busy=%t\n", repo.Slug(), name, status, lastState.Busy)
		}

		pollErr := ctx.Err()
		if pollErr == nil {
			pollErr = poll(ctx)
		}
		if pollErr != nil {
			if errors.Is(pollErr, context.DeadlineExceeded) {
				if lastErr != nil {
					return Exit(5, "timed out waiting for GitHub Actions runner %s in %s to become online and idle: last lookup error: %v", name, repo.Slug(), lastErr)
				}
				return Exit(5, "timed out waiting for GitHub Actions runner %s in %s to become online and idle: last status=%s busy=%t", name, repo.Slug(), status, lastState.Busy)
			}
			return pollErr
		}
	}
}

func actionsRunnerBootstrapContext(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return context.WithCancel(ctx)
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return context.WithCancel(ctx)
	}
	reserve := remaining / 20
	if reserve > maxActionsDiagnosticReserve {
		reserve = maxActionsDiagnosticReserve
	}
	if reserve <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, deadline.Add(-reserve))
}

func readGitHubActionsRunnerDiagnosticsBestEffort(ctx context.Context, target SSHTarget, secrets ...string) string {
	if err := ctx.Err(); err != nil {
		return fmt.Sprintf("diagnostics skipped: timeout budget exhausted: %v", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := runSSHOutputWithRemoteWaitTimeout(ctx, target, remoteGitHubActionsRunnerDiagnostics(target), 15*time.Second, "2", "1")
	if ctx.Err() != nil {
		return fmt.Sprintf("diagnostics unavailable: %v", ctx.Err())
	}
	if err != nil {
		return fmt.Sprintf("diagnostics unavailable: %v", err)
	}
	out = redactActionsRunnerText(out, secrets...)
	if len(out) > actionsRunnerDiagnosticLimit {
		out = out[len(out)-actionsRunnerDiagnosticLimit:]
	}
	return strings.TrimSpace(out)
}

func redactActionsRunnerText(value string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	return value
}

func remoteGitHubActionsRunnerDiagnostics(target SSHTarget) string {
	if isWindowsNativeTarget(target) {
		return PowershellCommand(`$ErrorActionPreference = "Continue"
$runnerDir = Join-Path $HOME "actions-runner"
Get-ScheduledTask | Where-Object { $_.TaskName -like "crabbox-actions-runner-*" } | ForEach-Object {
  Write-Output ("task=" + $_.TaskName + " state=" + $_.State)
}
Get-CimInstance Win32_Process | Where-Object { $_.Name -in @("Runner.Listener.exe", "Runner.Worker.exe") } | ForEach-Object {
  Write-Output ("process=" + $_.Name + " pid=" + $_.ProcessId)
}
foreach ($path in @((Join-Path $runnerDir "crabbox-runner.err.log"), (Join-Path $runnerDir "crabbox-runner.log"), (Join-Path $runnerDir "crabbox-config.log"))) {
  if (Test-Path -LiteralPath $path) {
    Write-Output ("log=" + $path)
    Get-Content -LiteralPath $path -Tail 40
  }
}
Get-ChildItem -LiteralPath (Join-Path $runnerDir "_diag") -Filter "Runner_*.log" -ErrorAction SilentlyContinue |
  Sort-Object LastWriteTime -Descending |
  Select-Object -First 1 |
  ForEach-Object {
    Write-Output ("log=" + $_.FullName)
    Get-Content -LiteralPath $_.FullName -Tail 40
  }
exit 0
`)
	}
	return `set +e
runner_dir="$HOME/actions-runner"
printf 'service='
systemctl is-active crabbox-actions-runner.service 2>&1
systemctl status crabbox-actions-runner.service --no-pager --lines=20 2>&1
for path in "$runner_dir/crabbox-runner.err.log" "$runner_dir/crabbox-runner.log" "$runner_dir/crabbox-config.log"; do
  if [ -f "$path" ]; then
    echo "log=$path"
    tail -n 40 "$path"
  fi
done
latest="$(find "$runner_dir/_diag" -maxdepth 1 -type f -name 'Runner_*.log' -printf '%T@ %p\n' 2>/dev/null | sort -nr | head -n 1 | cut -d' ' -f2-)"
if [ -n "$latest" ]; then
  echo "log=$latest"
  tail -n 40 "$latest"
fi
exit 0`
}
