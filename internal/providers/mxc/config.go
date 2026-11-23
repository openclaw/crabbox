package mxc

import (
	"encoding/json"
	"net/netip"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type mxcConfig struct {
	Version     string        `json:"version"`
	Containment string        `json:"containment"`
	Process     mxcProcess    `json:"process"`
	Filesystem  mxcFilesystem `json:"filesystem"`
	Network     mxcNetwork    `json:"network"`
	Fallback    mxcFallback   `json:"fallback"`
	UI          mxcUI         `json:"ui"`
}
type mxcProcess struct {
	CommandLine       string   `json:"commandLine"`
	CWD               string   `json:"cwd,omitempty"`
	Env               []string `json:"env"`
	InheritDefaultEnv bool     `json:"inheritDefaultEnv"`
	Timeout           int64    `json:"timeout,omitempty"`
}
type mxcFilesystem struct {
	ReadWritePaths []string `json:"readwritePaths,omitempty"`
	ReadOnlyPaths  []string `json:"readonlyPaths,omitempty"`
}
type mxcNetwork struct {
	Egress  mxcEgress  `json:"egress"`
	Ingress mxcIngress `json:"ingress"`
}
type mxcEgress struct {
	Default string           `json:"default"`
	Allow   []mxcNetworkRule `json:"allow,omitempty"`
	Deny    []mxcNetworkRule `json:"deny,omitempty"`
}
type mxcIngress struct {
	Default      string `json:"default"`
	HostLoopback string `json:"hostLoopback"`
}
type mxcNetworkRule struct {
	To []mxcNetworkPeer `json:"to"`
}
type mxcNetworkPeer struct {
	CIDR string `json:"cidr"`
}
type mxcFallback struct {
	AllowDACLMutation bool `json:"allowDaclMutation"`
}
type mxcUI struct {
	Disable bool `json:"disable"`
}

func buildConfig(cfg core.Config, req core.RunRequest) (mxcConfig, error) {
	if err := validateConfig(cfg.MXC); err != nil {
		return mxcConfig{}, err
	}
	if req.ShellMode && !cfg.MXC.AllowWindowsUI {
		return mxcConfig{}, core.Exit(2, "provider=mxc --shell uses Windows PowerShell; rerun with --mxc-allow-windows-ui")
	}
	commandLine, err := windowsCommandLine(req.Command, req.ShellMode)
	if err != nil {
		return mxcConfig{}, err
	}
	readwrite := append([]string(nil), cfg.MXC.ReadWritePaths...)
	if root := strings.TrimSpace(req.Repo.Root); root != "" {
		if isWindowsVolumeRoot(root) {
			return mxcConfig{}, core.Exit(2, "refusing to grant MXC read-write access to volume root %q", root)
		}
		readwrite = append(readwrite, root)
	}
	readonly := append(defaultReadOnlyPaths(), cfg.MXC.ReadOnlyPaths...)
	if !req.ShellMode && len(req.Command) > 0 {
		if resolved, err := exec.LookPath(req.Command[0]); err == nil {
			readonly = append(readonly, resolved)
		}
	}
	env := windowsProcessEnvironment(req.Env)
	timeout := req.Options.TTL
	if timeout <= 0 {
		timeout = cfg.TTL
	}
	// Stable MXC uses an unsigned 32-bit millisecond timeout; zero disables it.
	if timeout < 0 || timeout > time.Duration(4294967295)*time.Millisecond || (timeout > 0 && timeout < time.Millisecond) {
		return mxcConfig{}, core.Exit(2, "provider=mxc timeout must be zero or between 1ms and 4294967295ms; adjust --ttl")
	}
	network, err := buildNetwork(cfg.MXC)
	if err != nil {
		return mxcConfig{}, err
	}
	return mxcConfig{
		Version:     core.MXCConfigDefaultVersion,
		Containment: strings.ToLower(defaultString(cfg.MXC.Containment, core.MXCConfigDefaultContainment)),
		Process:     mxcProcess{CommandLine: commandLine, CWD: req.Repo.Root, Env: env, Timeout: timeout.Milliseconds()},
		Filesystem:  mxcFilesystem{ReadWritePaths: uniquePaths(readwrite), ReadOnlyPaths: uniquePaths(readonly)},
		Network:     network,
		Fallback:    mxcFallback{AllowDACLMutation: cfg.MXC.AllowDACLMutation},
		UI:          mxcUI{Disable: !cfg.MXC.AllowWindowsUI},
	}, nil
}

func validateConfig(cfg core.MXCConfig) error {
	if version := defaultString(cfg.Version, core.MXCConfigDefaultVersion); version != core.MXCConfigDefaultVersion {
		return core.Exit(2, "provider=mxc supports only schema 1.0.0; version %q is not supported; review the migration in docs/providers/mxc.md and set --mxc-version 1.0.0", version)
	}
	if containment := strings.ToLower(defaultString(cfg.Containment, core.MXCConfigDefaultContainment)); containment != "process" && containment != "processcontainer" {
		return core.Exit(2, "provider=mxc containment %q cannot preserve the native Windows policy; use --mxc-containment processcontainer (or process); --mxc-experimental does not override this restriction", containment)
	}
	if cfg.AllowDACLMutation {
		return core.Exit(2, "provider=mxc schema 1.0.0 has no host-DACL mutation fallback; remove --mxc-allow-dacl-mutation or set mxc.allowDaclMutation=false; a host with native PSEC support is required")
	}
	_, err := buildNetwork(cfg)
	return err
}

func buildNetwork(cfg core.MXCConfig) (mxcNetwork, error) {
	policy := defaultString(cfg.Network, core.MXCConfigDefaultNetwork)
	action := "deny"
	switch policy {
	case "block":
	case "allow":
		action = "allow"
	default:
		return mxcNetwork{}, core.Exit(2, "invalid mxc.network %q; use --mxc-network block or allow", policy)
	}
	allow, err := networkRules(cfg.AllowedHosts, "allowedHosts", "--mxc-allowed-hosts")
	if err != nil {
		return mxcNetwork{}, err
	}
	deny, err := networkRules(cfg.BlockedHosts, "blockedHosts", "--mxc-blocked-hosts")
	if err != nil {
		return mxcNetwork{}, err
	}
	// Outbound opt-ins never grant private-network ingress or host loopback.
	return mxcNetwork{Egress: mxcEgress{Default: action, Allow: allow, Deny: deny}, Ingress: mxcIngress{Default: "deny", HostLoopback: "deny"}}, nil
}

func networkRules(hosts []string, field, flag string) ([]mxcNetworkRule, error) {
	var rules []mxcNetworkRule
	for index, host := range hosts {
		value := strings.TrimSpace(host)
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			addr, addrErr := netip.ParseAddr(value)
			if addrErr != nil || addr.Zone() != "" || addr.Is4In6() {
				return nil, core.Exit(2, "mxc.%s entry %d is unsupported; %s accepts only numeric IPv4/IPv6 addresses or CIDRs, not DNS names, wildcards, URLs, ports, zones or IPv4-mapped IPv6; no DNS resolution is performed", field, index+1, flag)
			}
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		if prefix.Addr().Is4In6() || prefix != prefix.Masked() {
			return nil, core.Exit(2, "mxc.%s entry %d must be a canonical IPv4/IPv6 network CIDR without host bits or IPv4-mapped IPv6; adjust %s", field, index+1, flag)
		}
		rules = append(rules, mxcNetworkRule{To: []mxcNetworkPeer{{CIDR: prefix.String()}}})
	}
	return rules, nil
}

func isWindowsVolumeRoot(path string) bool {
	clean := strings.TrimRight(strings.ReplaceAll(strings.TrimSpace(path), "/", `\`), `\`)
	if isWindowsDriveRoot(clean) {
		return true
	}
	lower := strings.ToLower(clean)
	if strings.HasPrefix(lower, `\\?\unc\`) {
		return len(windowsPathParts(clean[len(`\\?\unc\`):])) == 2
	}
	if strings.HasPrefix(lower, `\\?\`) {
		return isWindowsDriveRoot(clean[len(`\\?\`):])
	}
	if strings.HasPrefix(clean, `\\`) {
		return len(windowsPathParts(clean[2:])) == 2
	}
	return false
}

func isWindowsDriveRoot(path string) bool {
	return len(path) == 2 && path[1] == ':' && ((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z'))
}

func windowsPathParts(path string) []string {
	parts := strings.Split(path, `\`)
	out := parts[:0]
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func windowsProcessEnvironment(forwarded map[string]string) []string {
	type entry struct {
		key   string
		value string
	}
	entries := make(map[string]entry, len(forwarded)+30)
	for key, value := range forwarded {
		entries[strings.ToLower(key)] = entry{key: key, value: value}
	}
	for _, key := range []string{
		"ALLUSERSPROFILE", "APPDATA", "CommonProgramFiles", "CommonProgramFiles(x86)", "CommonProgramW6432",
		"COMPUTERNAME", "ComSpec", "DriverData", "LOCALAPPDATA", "NUMBER_OF_PROCESSORS", "OS", "Path", "PATHEXT",
		"PROCESSOR_ARCHITECTURE", "PROCESSOR_IDENTIFIER", "PROCESSOR_LEVEL", "PROCESSOR_REVISION", "ProgramData",
		"ProgramFiles", "ProgramFiles(x86)", "ProgramW6432", "PROMPT", "PSModulePath", "PUBLIC", "SystemDrive",
		"SystemRoot", "USERPROFILE", "WINDIR",
	} {
		if value, ok := os.LookupEnv(key); ok && strings.TrimSpace(value) != "" {
			entries[strings.ToLower(key)] = entry{key: key, value: value}
		}
	}
	env := make([]string, 0, len(entries))
	for _, item := range entries {
		env = append(env, item.key+"="+item.value)
	}
	sort.Strings(env)
	return env
}

func buildIsolatedConfig(cfg core.Config, req core.RunRequest) (mxcConfig, string, func(), error) {
	tempDir, err := os.MkdirTemp("", "crabbox-mxc-run-*")
	if err != nil {
		return mxcConfig{}, "", nil, core.Exit(2, "create MXC temporary directory: %v", err)
	}
	cleanup := func() { _ = os.RemoveAll(tempDir) }
	if err := secureDirectory(tempDir); err != nil {
		cleanup()
		return mxcConfig{}, "", nil, core.Exit(2, "secure MXC temporary directory: %v", err)
	}
	cfg.MXC.ReadWritePaths = append(append([]string(nil), cfg.MXC.ReadWritePaths...), tempDir)
	req.Env = cloneEnv(req.Env)
	for key := range req.Env {
		if strings.EqualFold(key, "TEMP") || strings.EqualFold(key, "TMP") {
			delete(req.Env, key)
		}
	}
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
		return core.Exit(2, "icacls: %s", strings.TrimSpace(string(result)))
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
		return "", core.Exit(2, "provider=mxc requires a command")
	}
	if shellMode {
		return "powershell.exe -NoProfile -NonInteractive -Command " + quoteWindowsArg(strings.Join(command, " ")), nil
	}
	resolvedCommand := append([]string(nil), command...)
	if resolved, err := lookPath(command[0]); err == nil {
		switch strings.ToLower(filepath.Ext(resolved)) {
		case ".bat", ".cmd":
			return "", core.Exit(2, "command %q resolves to a Windows script shim; rerun with --shell or invoke an executable directly", command[0])
		}
		resolvedCommand[0] = resolved
	}
	parts := make([]string, len(resolvedCommand))
	for i, arg := range resolvedCommand {
		parts[i] = quoteWindowsArg(arg)
	}
	return strings.Join(parts, " "), nil
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
