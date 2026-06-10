package mxc

import (
	"encoding/json"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

type mxcConfig struct {
	Version     string        `json:"version"`
	Containment string        `json:"containment"`
	Process     mxcProcess    `json:"process"`
	Filesystem  mxcFilesystem `json:"filesystem"`
	Network     mxcNetwork    `json:"network"`
	Fallback    mxcFallback   `json:"fallback"`
}
type mxcProcess struct {
	CommandLine string   `json:"commandLine"`
	CWD         string   `json:"cwd,omitempty"`
	Env         []string `json:"env,omitempty"`
	Timeout     int64    `json:"timeout,omitempty"`
}
type mxcFilesystem struct {
	ReadWritePaths []string `json:"readwritePaths,omitempty"`
	ReadOnlyPaths  []string `json:"readonlyPaths,omitempty"`
}
type mxcNetwork struct {
	DefaultPolicy   string   `json:"defaultPolicy"`
	EnforcementMode string   `json:"enforcementMode,omitempty"`
	AllowedHosts    []string `json:"allowedHosts,omitempty"`
	BlockedHosts    []string `json:"blockedHosts,omitempty"`
}
type mxcFallback struct {
	AllowDACLMutation bool `json:"allowDaclMutation"`
}

func buildConfig(cfg Config, req RunRequest) (mxcConfig, error) {
	commandLine, err := windowsCommandLine(req.Command, req.ShellMode)
	if err != nil {
		return mxcConfig{}, err
	}
	readwrite := append([]string(nil), cfg.MXC.ReadWritePaths...)
	if root := strings.TrimSpace(req.Repo.Root); root != "" {
		readwrite = append(readwrite, root)
	}
	readonly := append(defaultReadOnlyPaths(), cfg.MXC.ReadOnlyPaths...)
	if !req.ShellMode && len(req.Command) > 0 {
		if resolved, err := exec.LookPath(req.Command[0]); err == nil {
			readonly = append(readonly, resolved)
		}
	}
	env := make([]string, 0, len(req.Env))
	for key, value := range req.Env {
		env = append(env, key+"="+value)
	}
	sort.Strings(env)
	timeout := req.Options.TTL
	if timeout <= 0 {
		timeout = cfg.TTL
	}
	return mxcConfig{
		Version:     defaultString(cfg.MXC.Version, "0.6.0-alpha"),
		Containment: defaultString(cfg.MXC.Containment, "processcontainer"),
		Process:     mxcProcess{CommandLine: commandLine, CWD: req.Repo.Root, Env: env, Timeout: timeout.Milliseconds()},
		Filesystem:  mxcFilesystem{ReadWritePaths: uniquePaths(readwrite), ReadOnlyPaths: uniquePaths(readonly)},
		Network:     mxcNetwork{DefaultPolicy: defaultString(cfg.MXC.Network, "block"), EnforcementMode: "both", AllowedHosts: cfg.MXC.AllowedHosts, BlockedHosts: cfg.MXC.BlockedHosts},
		Fallback:    mxcFallback{AllowDACLMutation: false},
	}, nil
}

func buildIsolatedConfig(cfg Config, req RunRequest) (mxcConfig, string, func(), error) {
	tempDir, err := os.MkdirTemp("", "crabbox-mxc-run-*")
	if err != nil {
		return mxcConfig{}, "", nil, exit(2, "create MXC temporary directory: %v", err)
	}
	cleanup := func() { _ = os.RemoveAll(tempDir) }
	if err := secureDirectory(tempDir); err != nil {
		cleanup()
		return mxcConfig{}, "", nil, exit(2, "secure MXC temporary directory: %v", err)
	}
	cfg.MXC.ReadWritePaths = append(append([]string(nil), cfg.MXC.ReadWritePaths...), tempDir)
	req.Env = cloneEnv(req.Env)
	req.Env["TEMP"] = tempDir
	req.Env["TMP"] = tempDir
	config, err := buildConfig(cfg, req)
	if err != nil {
		cleanup()
		return mxcConfig{}, "", nil, err
	}
	return config, tempDir, cleanup, nil
}

func secureDirectory(path string) error {
	if runtime.GOOS != "windows" {
		return os.Chmod(path, 0o700)
	}
	current, err := user.Current()
	if err != nil {
		return err
	}
	result, err := exec.Command("icacls.exe", path, "/inheritance:r", "/grant:r", current.Username+`:(OI)(CI)F`).CombinedOutput()
	if err != nil {
		return exit(2, "icacls: %s", strings.TrimSpace(string(result)))
	}
	return nil
}

func cloneEnv(env map[string]string) map[string]string {
	cloned := make(map[string]string, len(env)+2)
	for key, value := range env {
		cloned[key] = value
	}
	return cloned
}

func writeConfigFile(dir string, config mxcConfig) (string, func(), error) {
	file, err := os.CreateTemp(dir, "config-*.json")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.Remove(file.Name()) }
	if runtime.GOOS != "windows" {
		if err := file.Chmod(0o600); err != nil {
			file.Close()
			cleanup()
			return "", nil, err
		}
	}
	enc := json.NewEncoder(file)
	enc.SetIndent("", "  ")
	if err := enc.Encode(config); err != nil {
		file.Close()
		cleanup()
		return "", nil, err
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return file.Name(), cleanup, nil
}

func defaultReadOnlyPaths() []string {
	values := []string{os.Getenv("SystemRoot"), os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)")}
	return uniquePaths(values)
}
func uniquePaths(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		clean := filepath.Clean(value)
		key := strings.ToLower(clean)
		if !seen[key] {
			seen[key] = true
			out = append(out, clean)
		}
	}
	sort.Strings(out)
	return out
}
func windowsCommandLine(command []string, shellMode bool) (string, error) {
	return windowsCommandLineWithLookPath(command, shellMode, exec.LookPath)
}

func windowsCommandLineWithLookPath(command []string, shellMode bool, lookPath func(string) (string, error)) (string, error) {
	if len(command) == 0 {
		return "", exit(2, "provider=mxc requires a command")
	}
	if shellMode {
		return "powershell.exe -NoProfile -NonInteractive -Command " + quoteWindowsArg(strings.Join(command, " ")), nil
	}
	parts := make([]string, len(command))
	for i, arg := range command {
		parts[i] = quoteWindowsArg(arg)
	}
	commandLine := strings.Join(parts, " ")
	if resolved, err := lookPath(command[0]); err == nil {
		switch strings.ToLower(filepath.Ext(resolved)) {
		case ".bat", ".cmd":
			return "", exit(2, "command %q resolves to a Windows script shim; rerun with --shell or invoke an executable directly", command[0])
		}
	}
	return commandLine, nil
}
func quoteWindowsArg(value string) string {
	if value != "" && !strings.ContainsAny(value, " \t\n\v\"") {
		return value
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for _, r := range value {
		if r == '\\' {
			slashes++
			continue
		}
		if r == '"' {
			b.WriteString(strings.Repeat("\\", slashes*2+1))
			b.WriteRune(r)
			slashes = 0
			continue
		}
		b.WriteString(strings.Repeat("\\", slashes))
		slashes = 0
		b.WriteRune(r)
	}
	b.WriteString(strings.Repeat("\\", slashes*2))
	b.WriteByte('"')
	return b.String()
}
func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}
