package cli

import (
	"bytes"
	"context"
	"fmt"
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
		if envAllowed(key, allow) {
			out[key] = value
		}
	}
	return out
}

func allowedProfileEnv(allow []string, profileEnv map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range profileEnv {
		if envAllowed(key, allow) {
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
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
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
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
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
