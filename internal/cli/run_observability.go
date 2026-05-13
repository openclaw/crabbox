package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func maybePrintEnvForwardingSummary(w io.Writer, provider, behavior string, allow []string, env map[string]string) {
	if strings.TrimSpace(os.Getenv("CRABBOX_ENV_ALLOW")) == "" {
		return
	}
	printEnvForwardingSummary(w, provider, behavior, allow, env)
}

func printEnvForwardingSummary(w io.Writer, provider, behavior string, allow []string, env map[string]string) {
	if w == nil {
		return
	}
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)
	entries := make([]string, 0, len(names))
	for _, name := range names {
		entries = append(entries, envMetadata(name, env[name]))
	}
	if len(entries) == 0 {
		fmt.Fprintf(w, "env forwarding provider=%s behavior=%s matched=none allow=%s\n", provider, behavior, strings.Join(allow, ","))
		return
	}
	fmt.Fprintf(w, "env forwarding provider=%s behavior=%s vars=%s\n", provider, behavior, strings.Join(entries, ","))
}

func PrintEnvForwardingSummary(w io.Writer, provider, behavior string, allow []string, env map[string]string) {
	printEnvForwardingSummary(w, provider, behavior, allow, env)
}

func envMetadata(name, value string) string {
	state := "set"
	if value == "" {
		state = "empty"
	}
	if envNameLooksSecret(name) {
		return fmt.Sprintf("%s=%s len=%d secret=true", name, state, len(value))
	}
	return fmt.Sprintf("%s=%s", name, state)
}

func envNameLooksSecret(name string) bool {
	upper := strings.ToUpper(name)
	for _, marker := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "PASS", "CREDENTIAL", "AUTH"} {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}

func printRunContextSummary(w io.Writer, coord *CoordinatorClient, cfg Config, server Server, target SSHTarget, leaseID, workdir string, hydrated bool, actionsURL string, recorder *runRecorder) {
	if w == nil {
		return
	}
	runID := ""
	if recorder != nil {
		runID = recorder.runID
	}
	workspace := "raw"
	if hydrated {
		workspace = "actions-hydrated"
	}
	fmt.Fprintln(w, "run context:")
	fmt.Fprintf(w, "  run=%s portal=%s logs=%s\n", blank(runID, "-"), runPortalURL(coord, runID), runLogsURL(coord, runID))
	fmt.Fprintf(w, "  lease=%s slug=%s provider=%s target=%s type=%s\n", leaseID, blank(serverSlug(server), "-"), cfg.Provider, blank(target.TargetOS, cfg.TargetOS), server.ServerType.Name)
	fmt.Fprintf(w, "  ssh=%s@%s:%s ip=%s\n", redactedSSHUser(cfg, server, target), target.Host, target.Port, blank(server.PublicNet.IPv4.IP, target.Host))
	fmt.Fprintf(w, "  workdir=%s workspace=%s actions=%s\n", workdir, workspace, blank(actionsURL, "-"))
}

func printKeepOnFailureSSHHint(w io.Writer, cfg Config, leaseID string, server Server, target SSHTarget) {
	if w == nil {
		return
	}
	id := firstNonBlank(serverSlug(server), leaseID)
	expires := blank(leaseLabelTimeDisplay(server.Labels["expires_at"]), server.Labels["expires_at"])
	if expires == "" {
		expires = "idle/ttl"
	}
	fmt.Fprintf(w, "keep-on-failure: kept lease=%s slug=%s expires=%s idle_timeout=%s ttl=%s\n", leaseID, blank(serverSlug(server), "-"), expires, cfg.IdleTimeout, cfg.TTL)
	fmt.Fprintf(w, "inspect: crabbox inspect --provider %s --id %s\n", displayShellArg(cfg.Provider), displayShellArg(id))
	fmt.Fprintf(w, "ssh: crabbox ssh --provider %s --id %s\n", displayShellArg(cfg.Provider), displayShellArg(id))
	if target.Host != "" && !target.AuthSecret {
		fmt.Fprintf(w, "ssh-direct: %s\n", sshCommandLine(target, false))
	}
	if target.AuthSecret {
		fmt.Fprintf(w, "ssh-direct: crabbox ssh --provider %s --id %s --show-secret\n", displayShellArg(cfg.Provider), displayShellArg(id))
	}
	fmt.Fprintf(w, "stop: crabbox stop --provider %s %s\n", displayShellArg(cfg.Provider), displayShellArg(id))
}

func printKeepOnFailureDelegatedHint(w io.Writer, provider, leaseID, slug string, idleTimeout, ttl time.Duration) {
	if w == nil {
		return
	}
	id := firstNonBlank(slug, leaseID)
	fmt.Fprintf(w, "keep-on-failure: kept lease=%s slug=%s expires=idle/ttl idle_timeout=%s ttl=%s\n", leaseID, blank(slug, "-"), idleTimeout, ttl)
	fmt.Fprintf(w, "rerun: crabbox run --provider %s --id %s -- <command>\n", displayShellArg(provider), displayShellArg(id))
	fmt.Fprintf(w, "stop: crabbox stop --provider %s %s\n", displayShellArg(provider), displayShellArg(id))
}

func PrintKeepOnFailureDelegatedHint(w io.Writer, provider, leaseID, slug string, idleTimeout, ttl time.Duration) {
	printKeepOnFailureDelegatedHint(w, provider, leaseID, slug, idleTimeout, ttl)
}

func HandleDelegatedRunFailure(w io.Writer, req RunRequest, provider, leaseID, slug string, idleTimeout, ttl time.Duration, acquired bool, shouldStop *bool) {
	if !req.KeepOnFailure {
		return
	}
	if acquired && !req.Keep && shouldStop != nil {
		*shouldStop = false
	}
	printKeepOnFailureDelegatedHint(w, provider, leaseID, slug, idleTimeout, ttl)
}

func displayShellArg(value string) string {
	words := readableShellWords([]string{value})
	if len(words) == 0 {
		return "''"
	}
	return words[0]
}

func runPortalURL(coord *CoordinatorClient, runID string) string {
	if coord == nil || coord.BaseURL == "" || runID == "" {
		return "-"
	}
	return strings.TrimRight(coord.BaseURL, "/") + "/portal/runs/" + url.PathEscape(runID)
}

func runLogsURL(coord *CoordinatorClient, runID string) string {
	if coord == nil || coord.BaseURL == "" || runID == "" {
		return "-"
	}
	return strings.TrimRight(coord.BaseURL, "/") + "/v1/runs/" + url.PathEscape(runID) + "/logs"
}

func printRemoteCapabilityPreflight(ctx context.Context, w io.Writer, cfg Config, target SSHTarget, leaseID, workdir string, envFiles []string, hydrated bool, actionsURL string, hydrateSupported bool, env map[string]string) {
	if w == nil {
		return
	}
	for _, line := range remotePreflightWorkspaceLines(cfg, target, leaseID, workdir, hydrated, actionsURL, hydrateSupported) {
		fmt.Fprintln(w, line)
	}
	tools := preflightToolsForTarget(target, cfg.Run.PreflightTools)
	if len(tools) == 0 {
		return
	}
	command := remoteCapabilityPreflightCommand(workdir, env, envFiles, tools)
	if isWindowsNativeTarget(target) {
		command = windowsRemoteCapabilityPreflightCommand(workdir, env, envFiles, tools)
	}
	out, err := runSSHCombinedOutput(ctx, target, command)
	if err != nil {
		fmt.Fprintf(w, "remote preflight failed: %v\n", err)
		if strings.TrimSpace(out) != "" {
			fmt.Fprintf(w, "remote preflight output: %s\n", strings.TrimSpace(out))
		}
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) != "" {
			fmt.Fprintf(w, "remote preflight %s\n", strings.TrimSpace(line))
		}
	}
}

func printDelegatedPreflightUnsupported(w io.Writer, provider string) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, "remote preflight provider=%s delegated unsupported; provider owns workspace and command transport\n", provider)
}

func remotePreflightWorkspaceLines(cfg Config, target SSHTarget, leaseID, workdir string, hydrated bool, actionsURL string, hydrateSupported bool) []string {
	workspace := "raw"
	if hydrated {
		workspace = "actions-hydrated"
	}
	lines := []string{fmt.Sprintf("remote preflight workspace=%s workdir=%s hydrate_supported=%t", workspace, workdir, hydrateSupported)}
	if actionsURL != "" {
		lines = append(lines, "remote preflight actions="+actionsURL)
	}
	if !hydrated && strings.TrimSpace(cfg.Actions.Workflow) != "" {
		lines = append(lines, "remote preflight hydrate_suggestion="+hydrateCommandSuggestion(cfg, target, leaseID, hydrateSupported))
	}
	return lines
}

func hydrateCommandSuggestion(cfg Config, target SSHTarget, leaseID string, supported bool) string {
	args := []string{"crabbox", "actions", "hydrate", "--id", leaseID}
	if cfg.Provider != "" {
		args = append(args, "--provider", cfg.Provider)
	}
	targetOS := firstNonBlank(target.TargetOS, cfg.TargetOS)
	if targetOS != "" {
		args = append(args, "--target", targetOS)
	}
	windowsMode := firstNonBlank(target.WindowsMode, cfg.WindowsMode)
	if targetOS == targetWindows && windowsMode != "" {
		args = append(args, "--windows-mode", windowsMode)
	}
	if cfg.Actions.Workflow != "" {
		args = append(args, "--workflow", cfg.Actions.Workflow)
	}
	if cfg.Actions.Job != "" {
		args = append(args, "--job", cfg.Actions.Job)
	}
	command := strings.Join(readableShellWords(args), " ")
	if !supported {
		command += " (unsupported for this provider/target)"
	}
	return command
}

func remoteCapabilityPreflightCommand(workdir string, env map[string]string, envFiles []string, tools []string) string {
	script := `printf 'user=%s\n' "$(id -un 2>/dev/null || whoami 2>/dev/null || printf unknown)"
printf 'cwd=%s\n' "$(pwd -P 2>/dev/null || pwd)"
preflight_cmd() {
  label="$1"; shift
  exe="$1"; shift
  if command -v "$exe" >/dev/null 2>&1; then
    out="$("$@" 2>&1 | sed -n '1p')"
    if [ -z "$out" ]; then out=present; fi
    printf '%s=%s\n' "$label" "$out"
  else
    printf '%s=missing\n' "$label"
  fi
}
`
	for _, tool := range tools {
		script += posixPreflightProbe(tool)
	}
	return remoteShellCommandWithEnvFiles(workdir, env, envFiles, script)
}

func windowsRemoteCapabilityPreflightCommand(workdir string, env map[string]string, envFiles []string, tools []string) string {
	var b bytes.Buffer
	writeWindowsRemotePrefix(&b, workdir, env, envFiles)
	b.WriteString(`function Test-Value($Label, $ScriptBlock) {
  try {
    $value = & $ScriptBlock
    if ($null -eq $value -or "$value" -eq "") { $value = "unknown" }
    Write-Output ($Label + "=" + (($value | Select-Object -First 1) -join ""))
  } catch {
    Write-Output ($Label + "=error:" + $_.Exception.Message)
  }
}
function Test-Tool($Label, $Exe, $Arguments) {
  $cmd = Get-Command $Exe -ErrorAction SilentlyContinue
  if (-not $cmd) {
    Write-Output ($Label + "=missing")
    return
  }
  try {
    $value = & $Exe @Arguments 2>&1 | Select-Object -First 1
    if ($null -eq $value -or "$value" -eq "") { $value = "present" }
    Write-Output ($Label + "=" + $value)
  } catch {
    Write-Output ($Label + "=error:" + $_.Exception.Message)
  }
}
Test-Value "user" { whoami }
Test-Value "cwd" { (Get-Location).Path }
`)
	for _, tool := range tools {
		b.WriteString(windowsPreflightProbe(tool))
	}
	return powershellCommand(b.String())
}

type preflightToolSpec struct {
	Posix   []string
	Windows []string
	OS      map[string]bool
}

var preflightToolRegistry = map[string]preflightToolSpec{
	"apt":              {Posix: []string{"apt-get", "--version"}, OS: map[string]bool{"linux": true}},
	"bubblewrap":       {Posix: []string{"bwrap", "--version"}, OS: map[string]bool{"linux": true}},
	"bun":              {Posix: []string{"bun", "--version"}, Windows: []string{"bun", "--version"}},
	"bwrap":            {Posix: []string{"bwrap", "--version"}, OS: map[string]bool{"linux": true}},
	"corepack":         {Posix: []string{"corepack", "--version"}, Windows: []string{"corepack", "--version"}},
	"docker":           {Posix: []string{"docker", "--version"}, Windows: []string{"docker", "--version"}},
	"execution_policy": {Windows: []string{"Get-ExecutionPolicy -Scope Process"}, OS: map[string]bool{"windows": true}},
	"git":              {Posix: []string{"git", "--version"}, Windows: []string{"git", "--version"}},
	"longpaths":        {Windows: []string{"git config --global --get core.longpaths"}, OS: map[string]bool{"windows": true}},
	"node":             {Posix: []string{"node", "--version"}, Windows: []string{"node", "--version"}},
	"npm":              {Posix: []string{"npm", "--version"}, Windows: []string{"npm", "--version"}},
	"pnpm":             {Posix: []string{"pnpm", "--version"}, Windows: []string{"pnpm", "--version"}},
	"powershell":       {Windows: []string{"$PSVersionTable.PSVersion.ToString()"}, OS: map[string]bool{"windows": true}},
	"pwsh":             {Windows: []string{"pwsh", "--version"}, OS: map[string]bool{"windows": true}},
	"sudo":             {OS: map[string]bool{"linux": true, "macos": true}},
	"tar":              {Posix: []string{"tar", "--version"}, Windows: []string{"tar", "--version"}},
	"temp":             {Windows: []string{"$env:TEMP"}, OS: map[string]bool{"windows": true}},
	"uv":               {Posix: []string{"uv", "--version"}, Windows: []string{"uv", "--version"}},
	"yarn":             {Posix: []string{"yarn", "--version"}, Windows: []string{"yarn", "--version"}},
}

var defaultPreflightToolNames = []string{"git", "tar", "node", "npm", "corepack", "pnpm", "yarn", "bun", "docker", "sudo", "apt", "bubblewrap", "powershell", "execution_policy", "longpaths", "temp", "pwsh"}

func normalizePreflightToolNames(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		for _, part := range splitCommaList(value) {
			name := strings.ToLower(strings.TrimSpace(part))
			if name == "" {
				continue
			}
			if name == "default" || name == "defaults" {
				out = appendUniqueStrings(out, defaultPreflightToolNames...)
				continue
			}
			out = appendUniqueStrings(out, name)
		}
	}
	return out
}

func validatePreflightTools(tools []string) error {
	for _, tool := range normalizePreflightToolNames(tools) {
		if tool == "none" {
			continue
		}
		if _, ok := preflightToolRegistry[tool]; !ok {
			return exit(2, "unknown preflight tool %q", tool)
		}
	}
	return nil
}

func preflightToolsForTarget(target SSHTarget, configured []string) []string {
	tools := normalizePreflightToolNames(configured)
	if len(tools) == 0 {
		tools = defaultPreflightToolNames
	}
	if len(tools) == 1 && tools[0] == "none" {
		return nil
	}
	kind := preflightOSKind(target)
	out := make([]string, 0, len(tools))
	for _, tool := range tools {
		if spec, ok := preflightToolRegistry[tool]; ok && spec.supports(kind) {
			out = append(out, tool)
		}
	}
	return out
}

func (spec preflightToolSpec) supports(kind string) bool {
	if len(spec.OS) > 0 && !spec.OS[kind] {
		return false
	}
	if kind == "windows" {
		return len(spec.Windows) > 0 || spec.OS[kind]
	}
	return len(spec.Posix) > 0 || spec.OS[kind]
}

func preflightOSKind(target SSHTarget) string {
	if isWindowsNativeTarget(target) {
		return "windows"
	}
	if target.TargetOS == targetMacOS {
		return "macos"
	}
	return "linux"
}

func posixPreflightProbe(tool string) string {
	switch tool {
	case "sudo":
		return `if command -v sudo >/dev/null 2>&1; then
  if sudo -n true >/dev/null 2>&1; then printf 'sudo=yes\n'; else printf 'sudo=no-password-required-failed\n'; fi
else
  printf 'sudo=missing\n'
fi
`
	case "apt":
		return "preflight_cmd apt apt-get apt-get --version\n"
	case "bubblewrap":
		return "preflight_cmd bubblewrap bwrap bwrap --version\n"
	}
	spec := preflightToolRegistry[tool]
	if len(spec.Posix) == 0 {
		return ""
	}
	return "preflight_cmd " + shellQuote(tool) + " " + shellQuote(spec.Posix[0]) + " " + strings.Join(readableShellWords(spec.Posix), " ") + "\n"
}

func windowsPreflightProbe(tool string) string {
	switch tool {
	case "execution_policy":
		return `Test-Value "execution_policy" { Get-ExecutionPolicy -Scope Process }` + "\n"
	case "longpaths":
		return `Test-Value "longpaths" { git config --global --get core.longpaths }` + "\n"
	case "powershell":
		return `Test-Value "powershell" { $PSVersionTable.PSVersion.ToString() }` + "\n"
	case "temp":
		return `Test-Value "temp" { $env:TEMP }` + "\n"
	}
	spec := preflightToolRegistry[tool]
	if len(spec.Windows) == 0 {
		return ""
	}
	return "Test-Tool " + psQuote(tool) + " " + psQuote(spec.Windows[0]) + " @(" + psArrayLiteral(spec.Windows[1:]) + ")\n"
}

func psArrayLiteral(values []string) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, psQuote(value))
	}
	return strings.Join(parts, ", ")
}

type FailureCaptureMetadata struct {
	Provider       string
	LeaseID        string
	Slug           string
	RunID          string
	Workdir        string
	ExitCode       int
	ActionsRunURL  string
	Timing         timingReport
	EnvAllow       []string
	Env            map[string]string
	Config         Config
	StdoutPath     string
	StderrPath     string
	CaptureFlagSet bool
}

func openFailureStreamBundleFile(label, explicitPath string) (*os.File, string, func(), error) {
	if explicitPath != "" {
		return nil, explicitPath, func() {}, nil
	}
	file, err := os.CreateTemp("", "crabbox-failure-*."+label+".log")
	if err != nil {
		return nil, "", func() {}, exit(2, "failure bundle %s temp: %v", label, err)
	}
	path := file.Name()
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(path)
	}
	return file, path, cleanup, nil
}

func captureFailureArtifacts(ctx context.Context, target SSHTarget, workdir, leaseID, runID string, meta FailureCaptureMetadata) (local string, bytes int, err error) {
	if isWindowsNativeTarget(target) {
		return "", 0, exit(2, "capture-on-fail is not supported for native Windows targets")
	}
	name := safeCaptureName(firstNonBlank(runID, leaseID, "run")) + "-" + time.Now().UTC().Format("20060102T150405Z") + ".tar.gz"
	remotePath := ".crabbox/" + name
	if out, err := runSSHCombinedOutput(ctx, target, remoteFailureCaptureCommand(workdir, remotePath)); err != nil {
		local, bytes, bundleErr := writeLocalFailureBundle(name, "", meta)
		if bundleErr != nil {
			return local, bytes, exit(7, "capture-on-fail prepare: %v: %s; local bundle: %v", err, strings.TrimSpace(out), bundleErr)
		}
		return local, bytes, exit(7, "capture-on-fail prepare: %v: %s", err, strings.TrimSpace(out))
	}
	defer func() {
		if out, cleanupErr := runSSHCombinedOutput(ctx, target, remoteRemoveFailureCaptureCommand(workdir, remotePath)); cleanupErr != nil && err == nil {
			err = exit(7, "capture-on-fail remote cleanup: %v: %s", cleanupErr, strings.TrimSpace(out))
		}
	}()
	remoteLocalPath := filepath.Join(os.TempDir(), safeCaptureName(firstNonBlank(runID, leaseID, "run"))+"-remote-"+name)
	_, remoteLocal, downloadErr := downloadRemoteFile(ctx, target, workdir, remotePath+"="+remoteLocalPath)
	if downloadErr != nil {
		local, bytes, bundleErr := writeLocalFailureBundle(name, "", meta)
		if bundleErr != nil {
			return local, bytes, exit(7, "capture-on-fail download: %v; local bundle: %v", downloadErr, bundleErr)
		}
		return local, bytes, downloadErr
	}
	defer os.Remove(remoteLocal)
	return writeLocalFailureBundle(name, remoteLocal, meta)
}

func CaptureLocalFailureBundle(nameSeed string, meta FailureCaptureMetadata) (string, int, error) {
	name := safeCaptureName(firstNonBlank(nameSeed, meta.RunID, meta.LeaseID, "run")) + "-" + time.Now().UTC().Format("20060102T150405Z") + ".tar.gz"
	return writeLocalFailureBundle(name, "", meta)
}

func captureFailureBundle(ctx context.Context, target SSHTarget, workdir, leaseID, runID string, meta FailureCaptureMetadata) (string, int, error) {
	if isWindowsNativeTarget(target) {
		return CaptureLocalFailureBundle(firstNonBlank(runID, leaseID, "run"), meta)
	}
	return captureFailureArtifacts(ctx, target, workdir, leaseID, runID, meta)
}

func writeLocalFailureBundle(name, remoteTarPath string, meta FailureCaptureMetadata) (string, int, error) {
	localPath := filepath.Join(".crabbox", "captures", name)
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return localPath, 0, exit(2, "failure bundle create %s: %v", filepath.Dir(localPath), err)
	}
	file, err := os.Create(localPath)
	if err != nil {
		return localPath, 0, exit(2, "failure bundle create %s: %v", localPath, err)
	}
	counting := &countingWriteCloser{WriteCloser: file}
	gzipWriter := gzip.NewWriter(counting)
	tarWriter := tar.NewWriter(gzipWriter)
	closeErr := func() error {
		if err := tarWriter.Close(); err != nil {
			_ = gzipWriter.Close()
			_ = counting.Close()
			return err
		}
		if err := gzipWriter.Close(); err != nil {
			_ = counting.Close()
			return err
		}
		return counting.Close()
	}
	if err := addFailureBundleMetadata(tarWriter, meta); err != nil {
		_ = closeErr()
		return localPath, int(counting.N), err
	}
	if err := addFailureBundleFile(tarWriter, "crabbox-artifacts/stdout.log", meta.StdoutPath); err != nil {
		_ = closeErr()
		return localPath, int(counting.N), err
	}
	if err := addFailureBundleFile(tarWriter, "crabbox-artifacts/stderr.log", meta.StderrPath); err != nil {
		_ = closeErr()
		return localPath, int(counting.N), err
	}
	if remoteTarPath != "" {
		if err := appendRemoteFailureTar(tarWriter, remoteTarPath, "crabbox-artifacts/remote/"); err != nil {
			_ = closeErr()
			return localPath, int(counting.N), err
		}
	}
	if err := closeErr(); err != nil {
		return localPath, int(counting.N), exit(2, "failure bundle close %s: %v", localPath, err)
	}
	return localPath, int(counting.N), nil
}

func addFailureBundleMetadata(tw *tar.Writer, meta FailureCaptureMetadata) error {
	run := map[string]any{
		"provider":          meta.Provider,
		"leaseId":           meta.LeaseID,
		"slug":              meta.Slug,
		"runId":             meta.RunID,
		"workdir":           meta.Workdir,
		"exitCode":          meta.ExitCode,
		"actionsRunUrl":     meta.ActionsRunURL,
		"captureOnFailFlag": meta.CaptureFlagSet,
	}
	runJSON, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return err
	}
	timingJSON, err := json.MarshalIndent(meta.Timing, "", "  ")
	if err != nil {
		return err
	}
	entries := map[string]string{
		"crabbox-artifacts/crabbox-run.json":    string(runJSON) + "\n",
		"crabbox-artifacts/timings.json":        string(timingJSON) + "\n",
		"crabbox-artifacts/env.redacted.txt":    failureEnvSummary(meta.EnvAllow, meta.Env),
		"crabbox-artifacts/config.redacted.txt": failureConfigSummary(meta.Config),
		"crabbox-artifacts/README.txt": "Failure bundle files are local-only and not fully redacted. " +
			"Review before sharing. Secret values are not intentionally written by Crabbox metadata.\n",
	}
	for name, content := range entries {
		if err := addFailureBundleBytes(tw, name, []byte(content)); err != nil {
			return err
		}
	}
	return nil
}

func addFailureBundleBytes(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: time.Now()}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

func addFailureBundleFile(tw *tar.Writer, name, path string) error {
	if strings.TrimSpace(path) == "" {
		return addFailureBundleBytes(tw, name, nil)
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return addFailureBundleBytes(tw, name, nil)
		}
		return exit(2, "failure bundle stat %s: %v", path, err)
	}
	if !info.Mode().IsRegular() {
		return exit(2, "failure bundle read %s: not a regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return exit(2, "failure bundle open %s: %v", path, err)
	}
	defer file.Close()
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: info.Size(), ModTime: info.ModTime()}); err != nil {
		return err
	}
	if _, err := io.Copy(tw, file); err != nil {
		return exit(2, "failure bundle stream %s: %v", path, err)
	}
	return nil
}

func appendRemoteFailureTar(tw *tar.Writer, remoteTarPath, prefix string) error {
	file, err := os.Open(remoteTarPath)
	if err != nil {
		return exit(2, "failure bundle open remote tar %s: %v", remoteTarPath, err)
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return exit(2, "failure bundle read remote tar %s: %v", remoteTarPath, err)
	}
	defer gzipReader.Close()
	tr := tar.NewReader(gzipReader)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return exit(2, "failure bundle read remote tar %s: %v", remoteTarPath, err)
		}
		cleanName := filepath.ToSlash(filepath.Clean(header.Name))
		if cleanName == "." || cleanName == ".." || strings.HasPrefix(cleanName, "/") || strings.HasPrefix(cleanName, "../") {
			continue
		}
		next := *header
		next.Name = prefix + cleanName
		if err := tw.WriteHeader(&next); err != nil {
			return err
		}
		if header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA {
			if _, err := io.Copy(tw, tr); err != nil {
				return err
			}
		}
	}
}

func failureEnvSummary(allowed []string, values map[string]string) string {
	if len(allowed) == 0 && len(values) == 0 {
		return "env_allow=empty\n"
	}
	names := append([]string(nil), allowed...)
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	names = compactSortedStrings(names)
	var b strings.Builder
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			continue
		}
		value, ok := values[name]
		if !ok {
			fmt.Fprintf(&b, "%s=missing\n", name)
			continue
		}
		if envNameLooksSecret(name) {
			fmt.Fprintf(&b, "%s=present len=%d secret=true\n", name, len(value))
		} else {
			fmt.Fprintf(&b, "%s=present\n", name)
		}
	}
	return b.String()
}

func compactSortedStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := values[:0]
	last := ""
	for _, value := range values {
		if value == "" || value == last {
			continue
		}
		out = append(out, value)
		last = value
	}
	return out
}

func failureConfigSummary(cfg Config) string {
	var b strings.Builder
	fmt.Fprintf(&b, "provider=%s\n", blank(cfg.Provider, "-"))
	fmt.Fprintf(&b, "target=%s\n", blank(cfg.TargetOS, "-"))
	fmt.Fprintf(&b, "windows_mode=%s\n", blank(cfg.WindowsMode, "-"))
	fmt.Fprintf(&b, "class=%s\n", blank(cfg.Class, "-"))
	fmt.Fprintf(&b, "server_type=%s\n", blank(cfg.ServerType, "-"))
	fmt.Fprintf(&b, "idle_timeout=%s\n", cfg.IdleTimeout)
	fmt.Fprintf(&b, "ttl=%s\n", cfg.TTL)
	fmt.Fprintf(&b, "work_root=%s\n", blank(cfg.WorkRoot, "-"))
	fmt.Fprintf(&b, "sync_base_ref=%s\n", blank(cfg.Sync.BaseRef, "-"))
	return b.String()
}

func safeCaptureName(value string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_' || r == '-':
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "run"
	}
	return b.String()
}

func remoteFailureCaptureCommand(workdir, remotePath string) string {
	var script bytes.Buffer
	script.WriteString("set -eu\n")
	script.WriteString("cd " + shellQuote(workdir) + "\n")
	script.WriteString("mkdir -p .crabbox\n")
	script.WriteString("out=" + shellQuote(remotePath) + "\n")
	script.WriteString(`manifest=.crabbox/capture-manifest.txt
files=.crabbox/capture-files.txt
{
  printf 'captured_at=%s\n' "$(date -Is 2>/dev/null || date)"
  printf 'host=%s\n' "$(hostname 2>/dev/null || printf unknown)"
  printf 'pwd=%s\n' "$(pwd -P 2>/dev/null || pwd)"
  printf 'note=%s\n' 'local-only failure capture; caller owns redaction before sharing'
} > "$manifest"
gateway_tail=.crabbox/gateway-log-tail.txt
rm -f "$gateway_tail"
for path in /tmp/crabbox-gateway.log /tmp/gateway.log gateway.log logs/gateway.log .crabbox/gateway.log; do
  if [ -f "$path" ]; then
    tail -n 400 "$path" > "$gateway_tail" 2>/dev/null || true
    break
  fi
done
: > "$files"
for path in test-results playwright-report coverage junit.xml results.xml .crabbox/scripts .crabbox/capture-manifest.txt .crabbox/gateway-log-tail.txt; do
  if [ -e "$path" ]; then printf '%s\n' "$path" >> "$files"; fi
done
find . -maxdepth 3 -type f \( -name '*.log' -o -name 'junit*.xml' -o -name 'TEST-*.xml' \) \
  ! -path './test-results/*' \
  ! -path './playwright-report/*' \
  ! -path './coverage/*' \
  -print 2>/dev/null | sed 's#^\./##' >> "$files" || true
sort -u "$files" > "$files.sorted"
tar -czf "$out" -T "$files.sorted" 2>/dev/null || tar -czf "$out" "$manifest"
printf '%s\n' "$out"
`)
	return "bash -lc " + shellQuote(script.String())
}

func remoteRemoveFailureCaptureCommand(workdir, remotePath string) string {
	script := "set -eu\ncd " + shellQuote(workdir) + "\nrm -f -- " + shellQuote(remotePath)
	return "bash -lc " + shellQuote(script)
}

func printFailureTail(w io.Writer, label string, tail *streamTailBuffer, capturedPath string) {
	if w == nil {
		return
	}
	if capturedPath != "" {
		fmt.Fprintf(w, "%s tail: captured at %s\n", label, capturedPath)
		return
	}
	lines := tail.Lines()
	if len(lines) == 0 {
		fmt.Fprintf(w, "%s tail: empty\n", label)
		return
	}
	fmt.Fprintf(w, "%s tail last %d lines:\n", label, len(lines))
	for _, line := range lines {
		fmt.Fprintf(w, "%s\n", line)
	}
}
