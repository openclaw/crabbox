package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

func loadEnvProfiles(paths []string) (map[string]string, error) {
	out := map[string]string{}
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, exit(2, "read env profile %s: %v", path, err)
		}
		for key, value := range parseEnvProfile(data) {
			out[key] = value
		}
	}
	return out, nil
}

func allowedEnvFromProfiles(allow []string, profileEnv map[string]string) map[string]string {
	out := allowedEnv(allow)
	for key, value := range profileEnv {
		if validEnvName(key) && envAllowed(key, allow) {
			out[key] = value
		}
	}
	return out
}

func allowedProfileEnv(allow []string, profileEnv map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range profileEnv {
		if validEnvName(key) && envAllowed(key, allow) {
			out[key] = value
		}
	}
	return out
}

func allowedEnvWithoutProfileKeys(allow []string, profileEnv map[string]string) map[string]string {
	out := allowedEnv(allow)
	for key := range profileEnv {
		delete(out, key)
	}
	return out
}

type runEnvSelection struct {
	Profile          map[string]string
	Inline           map[string]string
	Effective        map[string]string
	SummaryRequested bool
}

func removeEnvironmentKeys(values map[string]string, denied ...string) {
	for key := range values {
		for _, name := range denied {
			if strings.EqualFold(key, strings.TrimSpace(name)) {
				delete(values, key)
				break
			}
		}
	}
}

func stripExternalDesktopPasswordFromRunEnv(cfg Config, selection *runEnvSelection) {
	if selection == nil {
		return
	}
	denied := externalDesktopChildEnvDenylist(cfg, cfg.TargetOS)
	removeEnvironmentKeys(selection.Profile, denied...)
	removeEnvironmentKeys(selection.Inline, denied...)
	removeEnvironmentKeys(selection.Effective, denied...)
}

func stripTargetCredentialsFromRunEnv(selection *runEnvSelection, target SSHTarget) {
	if selection == nil {
		return
	}
	removeEnvironmentKeys(selection.Profile, target.ChildEnvDenylist...)
	removeEnvironmentKeys(selection.Inline, target.ChildEnvDenylist...)
	removeEnvironmentKeys(selection.Effective, target.ChildEnvDenylist...)
}

func allowedRemoteEnv(cfg Config) map[string]string {
	values := allowedEnv(cfg.EnvAllow)
	removeEnvironmentKeys(values, externalDesktopChildEnvDenylist(cfg, cfg.TargetOS)...)
	return values
}

func allowedRemoteEnvForTarget(cfg Config, target SSHTarget) map[string]string {
	values := allowedRemoteEnv(cfg)
	removeEnvironmentKeys(values, target.ChildEnvDenylist...)
	return values
}

func selectRunEnv(allow []string, profilePaths []string, explicitAllow bool) (runEnvSelection, error) {
	profileEnv, err := loadEnvProfiles(profilePaths)
	if err != nil {
		return runEnvSelection{}, err
	}
	profile := allowedProfileEnv(allow, profileEnv)
	inline := allowedEnvWithoutProfileKeys(allow, profile)
	return runEnvSelection{
		Profile:          profile,
		Inline:           inline,
		Effective:        mergeEnv(inline, profile),
		SummaryRequested: explicitAllow || len(profilePaths) > 0 || strings.TrimSpace(os.Getenv("CRABBOX_ENV_ALLOW")) != "",
	}, nil
}

func remoteRunEnvFiles(paths ...string) []string {
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path != "" {
			out = append(out, path)
		}
	}
	return out
}

func parseEnvProfile(data []byte) map[string]string {
	out := map[string]string{}
	for _, raw := range bytes.Split(data, []byte{'\n'}) {
		line := strings.TrimSpace(stripEnvProfileComment(string(raw)))
		if line == "" {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		line = strings.TrimSpace(line)
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if !validEnvName(key) {
			continue
		}
		value = strings.TrimSpace(value)
		if strings.Contains(value, "$(") || strings.Contains(value, "`") {
			continue
		}
		parsed, ok := parseEnvProfileValue(value)
		if !ok {
			continue
		}
		out[key] = parsed
	}
	return out
}

func stripEnvProfileComment(line string) string {
	var quote rune
	escaped := false
	for i, r := range line {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && quote == '"' {
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			continue
		}
		if r == '#' && envProfileHashStartsComment(line, i) {
			return line[:i]
		}
	}
	return line
}

func envProfileHashStartsComment(line string, index int) bool {
	if index < 0 || index >= len(line) {
		return false
	}
	if index == 0 {
		return true
	}
	prev := line[index-1]
	return prev == ' ' || prev == '\t'
}

func parseEnvProfileValue(value string) (string, bool) {
	if value == "" {
		return "", true
	}
	if strings.HasPrefix(value, "'") {
		if !strings.HasSuffix(value, "'") || len(value) < 2 {
			return "", false
		}
		return value[1 : len(value)-1], true
	}
	if strings.HasPrefix(value, "\"") {
		if !strings.HasSuffix(value, "\"") || len(value) < 2 {
			return "", false
		}
		v := value[1 : len(value)-1]
		v = strings.ReplaceAll(v, `\"`, `"`)
		v = strings.ReplaceAll(v, `\\`, `\`)
		return v, true
	}
	fields := strings.Fields(value)
	if len(fields) != 1 {
		return "", false
	}
	return fields[0], true
}

func uploadRunEnvProfile(ctx context.Context, target SSHTarget, workdir, remotePath string, env map[string]string) error {
	if len(env) == 0 {
		return nil
	}
	remote := remoteUploadRunEnvProfileCommand(workdir, remotePath)
	input := formatShellEnvFile(env)
	if isWindowsNativeTarget(target) {
		remote = windowsRemoteUploadRunEnvProfileCommand(workdir, remotePath)
		input = formatPlainEnvFile(env)
	}
	var stdout, stderr bytes.Buffer
	if err := runSSHInput(ctx, target, remote, strings.NewReader(input), &stdout, &stderr); err != nil {
		detail := trimFailureDetail(strings.TrimSpace(stdout.String() + "\n" + stderr.String()))
		if detail != "" {
			return exit(7, "upload env profile %s: %v: %s", remotePath, err, detail)
		}
		return exit(7, "upload env profile %s: %v", remotePath, err)
	}
	return nil
}

func runEnvProfilePath(name string) string {
	return ".crabbox/env/" + safeCaptureName(name) + ".env"
}

func runEnvHelperPath(name string) string {
	return ".crabbox/env/" + safeCaptureName(name)
}

func safeEnvHelperName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", exit(2, "--env-helper requires a name")
	}
	if strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return "", exit(2, "--env-helper must be a simple name, not a path")
	}
	safe := safeCaptureName(name)
	if safe == "" || safe != name {
		return "", exit(2, "--env-helper must contain only letters, numbers, dash, or underscore")
	}
	return safe, nil
}

func probeRunEnvProfile(ctx context.Context, target SSHTarget, workdir, remotePath string, env map[string]string, stderr io.Writer) error {
	if len(env) == 0 {
		return nil
	}
	names := sortedEnvNames(env)
	remote := remoteProbeRunEnvProfileCommand(workdir, remotePath, names)
	if isWindowsNativeTarget(target) {
		remote = windowsRemoteProbeRunEnvProfileCommand(workdir, remotePath, names)
	}
	out, err := runSSHOutput(ctx, target, remote)
	if err != nil {
		return exit(7, "probe env profile %s: %v", remotePath, err)
	}
	entries := splitNonEmptyLines(out)
	if len(entries) == 0 {
		return exit(7, "probe env profile %s: empty probe output", remotePath)
	}
	fmt.Fprintf(stderr, "env profile remote=%s vars=%s\n", remotePath, strings.Join(entries, ","))
	return nil
}

func uploadRunEnvHelper(ctx context.Context, target SSHTarget, workdir, helperPath, profilePath string) error {
	if err := validateRunEnvHelperTarget(target, helperPath); err != nil {
		return err
	}
	remote := remoteUploadRunEnvHelperCommand(workdir, helperPath)
	input := formatRunEnvHelper(profilePath)
	var stdout, stderr bytes.Buffer
	if err := runSSHInput(ctx, target, remote, strings.NewReader(input), &stdout, &stderr); err != nil {
		detail := trimFailureDetail(strings.TrimSpace(stdout.String() + "\n" + stderr.String()))
		if detail != "" {
			return exit(7, "upload env helper %s: %v: %s", helperPath, err, detail)
		}
		return exit(7, "upload env helper %s: %v", helperPath, err)
	}
	return nil
}

func validateRunEnvHelperTarget(target SSHTarget, helperPath string) error {
	if strings.TrimSpace(helperPath) == "" {
		return nil
	}
	if isWindowsNativeTarget(target) {
		return exit(2, "--env-helper is not supported for native Windows targets yet")
	}
	return nil
}

func remoteProbeRunEnvProfileCommand(workdir, remotePath string, names []string) string {
	var b strings.Builder
	b.WriteString("set -eu\ncd ")
	b.WriteString(shellQuote(workdir))
	b.WriteByte('\n')
	b.WriteString("test -f ")
	b.WriteString(shellQuote(remotePath))
	b.WriteByte('\n')
	b.WriteString(". ")
	b.WriteString(shellQuote(remotePath))
	b.WriteByte('\n')
	for _, name := range names {
		if !validEnvName(name) {
			continue
		}
		secret := "false"
		if envNameLooksSecret(name) {
			secret = "true"
		}
		b.WriteString("if [ \"${")
		b.WriteString(name)
		b.WriteString("+set}\" = set ]; then v=${")
		b.WriteString(name)
		b.WriteString("}; if [ -z \"$v\" ]; then printf '%s=empty")
		if secret == "true" {
			b.WriteString(" len=0 secret=true")
		}
		b.WriteString("\\n' ")
		b.WriteString(shellQuote(name))
		b.WriteString("; else printf '%s=set")
		if secret == "true" {
			b.WriteString(" len=%s secret=true")
		}
		b.WriteString("\\n' ")
		b.WriteString(shellQuote(name))
		if secret == "true" {
			b.WriteString(" \"${#v}\"")
		}
		b.WriteString("; fi; else printf '%s=missing\\n' ")
		b.WriteString(shellQuote(name))
		b.WriteString("; fi\n")
	}
	return "bash -lc " + shellQuote(b.String())
}

func remoteUploadRunEnvHelperCommand(workdir, remotePath string) string {
	dir := shellDir(remotePath)
	script := "set -eu\n" +
		"cd " + shellQuote(workdir) + "\n" +
		"mkdir -p " + shellQuote(dir) + "\n" +
		"umask 077\n" +
		"cat > " + shellQuote(remotePath) + "\n" +
		"chmod 700 " + shellQuote(remotePath) + "\n"
	return "bash -lc " + shellQuote(script)
}

func formatRunEnvHelper(profilePath string) string {
	return "#!/usr/bin/env bash\n" +
		"set -e\n" +
		"cd \"$(dirname \"$0\")/../..\"\n" +
		"profile=" + shellQuote(profilePath) + "\n" +
		"if [ ! -f \"$profile\" ]; then echo \"crabbox env helper: missing $profile\" >&2; exit 127; fi\n" +
		". \"$profile\"\n" +
		"if [ \"$#\" -eq 0 ]; then echo \"usage: $0 <command> [args...]\" >&2; exit 64; fi\n" +
		"exec \"$@\"\n"
}

func remoteUploadRunEnvProfileCommand(workdir, remotePath string) string {
	dir := shellDir(remotePath)
	script := "set -eu\n" +
		"cd " + shellQuote(workdir) + "\n" +
		"mkdir -p " + shellQuote(dir) + "\n" +
		"umask 077\n" +
		"cat > " + shellQuote(remotePath) + "\n" +
		"chmod 600 " + shellQuote(remotePath) + "\n"
	return "bash -lc " + shellQuote(script)
}

func remoteRemoveRunEnvProfileCommand(workdir, remotePath string) string {
	script := "set -eu\ncd " + shellQuote(workdir) + "\nrm -f -- " + shellQuote(remotePath)
	return "bash -lc " + shellQuote(script)
}

func removeRunEnvProfileCommand(target SSHTarget, workdir, remotePath string) string {
	if isWindowsNativeTarget(target) {
		return windowsRemoteRemoveRunEnvProfileCommand(workdir, remotePath)
	}
	return remoteRemoveRunEnvProfileCommand(workdir, remotePath)
}

func formatShellEnvFile(env map[string]string) string {
	keys := sortedEnvNames(env)
	var b strings.Builder
	for _, key := range keys {
		if !validEnvName(key) {
			continue
		}
		fmt.Fprintf(&b, "export %s=%s\n", key, shellQuote(env[key]))
	}
	return b.String()
}

func formatPlainEnvFile(env map[string]string) string {
	keys := sortedEnvNames(env)
	var b strings.Builder
	for _, key := range keys {
		if !validEnvName(key) {
			continue
		}
		fmt.Fprintf(&b, "%s=%s\n", key, strings.NewReplacer("\r", "", "\n", "").Replace(env[key]))
	}
	return b.String()
}

func windowsRemoteUploadRunEnvProfileCommand(workdir, remotePath string) string {
	return windowsRemoteUploadUTF8BOMFileCommand(workdir, remotePath)
}

func windowsRemoteRemoveRunEnvProfileCommand(workdir, remotePath string) string {
	script := `$ErrorActionPreference = "Stop"
Set-Location -LiteralPath ` + psQuote(workdir) + `
Remove-Item -LiteralPath ` + psQuote(remotePath) + ` -Force -ErrorAction SilentlyContinue
`
	return powershellCommand(script)
}

func windowsRemoteProbeRunEnvProfileCommand(workdir, remotePath string, names []string) string {
	script := `$ErrorActionPreference = "Stop"
Set-Location -LiteralPath ` + psQuote(workdir) + `
$profilePath = ` + psQuote(remotePath) + `
if (-not (Test-Path -LiteralPath $profilePath)) { throw "missing env profile $profilePath" }
Get-Content -Encoding UTF8 -LiteralPath $profilePath | ForEach-Object {
  $line = $_
  if ($line -match '^([^=]+)=(.*)$') {
    [Environment]::SetEnvironmentVariable($matches[1], $matches[2], 'Process')
  }
}
`
	for _, name := range names {
		if !validEnvName(name) {
			continue
		}
		secret := envNameLooksSecret(name)
		script += `$value = [Environment]::GetEnvironmentVariable(` + psQuote(name) + `, 'Process')
if ($null -eq $value) {
  Write-Output ` + psQuote(name+"=missing") + `
} elseif ($value -eq "") {
  Write-Output ` + psQuote(name+"=empty"+secretSuffix(secret, 0)) + `
} else {
`
		if secret {
			script += `  Write-Output (` + psQuote(name+"=set len=") + ` + $value.Length + ` + psQuote(" secret=true") + `)
`
		} else {
			script += `  Write-Output ` + psQuote(name+"=set") + `
`
		}
		script += `}
`
	}
	return powershellCommand(script)
}

func secretSuffix(secret bool, length int) string {
	if !secret {
		return ""
	}
	return fmt.Sprintf(" len=%d secret=true", length)
}

func sortedEnvNames(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func splitNonEmptyLines(text string) []string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func shellDir(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "."
	}
	if idx := strings.LastIndexByte(path, '/'); idx >= 0 {
		if idx == 0 {
			return "/"
		}
		return path[:idx]
	}
	return "."
}

func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if i == 0 {
			if !((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r == '_') {
				return false
			}
			continue
		}
		if !((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_') {
			return false
		}
	}
	return true
}
