package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	osuser "os/user"
	"path/filepath"
	"strings"
	"time"
)

const sshTransportHostAlias = "crabbox-resolved-target"

// sshTransportSession materializes a resolved SSH target as a private,
// short-lived OpenSSH config. This keeps token usernames and provider proxy
// policy out of the argv and environment Crabbox gives rsync and OpenSSH.
type sshTransportSession struct {
	dir         string
	configPath  string
	destination string
	cleanup     func() error
}

func newSSHTransportSession(ctx context.Context, target SSHTarget, localForward bool) (*sshTransportSession, error) {
	dir, err := os.MkdirTemp("", "crabbox-ssh-transport-*")
	if err != nil {
		return nil, fmt.Errorf("create private SSH transport directory: %w", err)
	}
	if err := secureSSHTransportPath(dir, true); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("secure private SSH transport directory: %w", err)
	}
	fail := func(cause error) (*sshTransportSession, error) {
		_ = os.RemoveAll(dir)
		return nil, cause
	}
	route, err := resolveSSHTransportConfigRoute(ctx, target, localForward)
	if err != nil {
		return fail(err)
	}
	if route.proxyJump != "" {
		route.jumpConfigPath, err = writeSSHTransportJumpConfig(dir, route.userConfigPath)
		if err != nil {
			return fail(err)
		}
	}
	config, err := renderSSHTransportConfigWithRoute(target, localForward, route)
	if err != nil {
		return fail(err)
	}
	path := filepath.Join(dir, "ssh_config")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		return fail(fmt.Errorf("write private SSH transport config: %w", err))
	}
	if err := secureSSHTransportPath(path, false); err != nil {
		return fail(fmt.Errorf("secure private SSH transport config: %w", err))
	}
	return &sshTransportSession{dir: dir, configPath: path, destination: sshTransportDestination(target)}, nil
}

func (s *sshTransportSession) Close() error {
	if s == nil || s.dir == "" {
		return nil
	}
	dir := s.dir
	cleanup := s.cleanup
	s.dir = ""
	s.configPath = ""
	s.destination = ""
	s.cleanup = nil
	var cleanupErr error
	if cleanup != nil {
		cleanupErr = cleanup()
	}
	if err := os.RemoveAll(dir); err != nil {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove private SSH transport config: %w", err))
	}
	return cleanupErr
}

func newWSLSSHTransportSession(ctx context.Context, target SSHTarget, wslExe, mountRoot string) (*sshTransportSession, error) {
	// Config-backed routes must use the native OpenSSH client that owns the
	// user's config and authentication paths. newResolvedSSHCopySession keeps
	// those targets out of WSL; enforce the same boundary for direct callers.
	if target.SSHConfigProxy {
		return nil, exit(2, "SSH config proxy routes require native OpenSSH")
	}
	dir, err := os.MkdirTemp("", "crabbox-ssh-transport-*")
	if err != nil {
		return nil, fmt.Errorf("reserve private WSL SSH transport directory: %w", err)
	}
	if err := secureSSHTransportPath(dir, true); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("secure private WSL SSH transport directory: %w", err)
	}
	wslDir := ""
	wslDirOwned := false
	cleanup := func() error {
		if !wslDirOwned {
			return nil
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(cleanupCtx, wslExe, "rm", "-rf", "--", wslDir)
		applyTargetChildEnvironment(cmd, target)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("remove private WSL SSH transport config: %w", err)
		}
		return nil
	}
	fail := func(cause error) (*sshTransportSession, error) {
		_ = cleanup()
		_ = os.RemoveAll(dir)
		return nil, cause
	}
	mkdir := exec.CommandContext(ctx, wslExe, "sh", "-c", `umask 077; mktemp -d /tmp/crabbox-ssh-transport-XXXXXX`)
	applyTargetChildEnvironment(mkdir, target)
	var mkdirStdout bytes.Buffer
	var mkdirStderr bytes.Buffer
	mkdir.Stdout = &mkdirStdout
	mkdir.Stderr = &mkdirStderr
	err = mkdir.Run()
	if err != nil {
		return fail(fmt.Errorf("create private WSL SSH transport directory: %w: %s", err, strings.TrimSpace(mkdirStderr.String())))
	}
	wslDir = strings.TrimSpace(mkdirStdout.String())
	if !validWSLSSHTransportDirectory(wslDir) {
		return fail(fmt.Errorf("create private WSL SSH transport directory: mktemp returned an unsafe path"))
	}
	wslDirOwned = true

	wslTarget := wslSSHTransportTarget(target, wslDir, mountRoot)
	if target.Key != "" {
		key, err := os.ReadFile(windowsHostPath(target.Key))
		if err != nil {
			return fail(fmt.Errorf("read SSH key for WSL copy transport: %w", err))
		}
		if err := writeWSLSSHTransportFile(ctx, wslExe, wslTarget.Key, key, target); err != nil {
			return fail(err)
		}
	}
	if target.CertificateFile != "" {
		certificate, err := os.ReadFile(windowsHostPath(target.CertificateFile))
		if err != nil {
			return fail(fmt.Errorf("read SSH certificate for WSL copy transport: %w", err))
		}
		if err := writeWSLSSHTransportFile(ctx, wslExe, wslTarget.CertificateFile, certificate, target); err != nil {
			return fail(err)
		}
	}
	config, err := renderSSHTransportConfig(wslTarget, false)
	if err != nil {
		return fail(err)
	}
	configPath := wslDir + "/ssh_config"
	if err := writeWSLSSHTransportFile(ctx, wslExe, configPath, []byte(config), target); err != nil {
		return fail(err)
	}
	return &sshTransportSession{dir: dir, configPath: configPath, destination: sshTransportDestination(wslTarget), cleanup: cleanup}, nil
}

func validWSLSSHTransportDirectory(value string) bool {
	const prefix = "/tmp/crabbox-ssh-transport-"
	suffix := strings.TrimPrefix(value, prefix)
	return suffix != value && len(suffix) >= 6 && !strings.ContainsAny(suffix, "/\\\x00\r\n\t ")
}

func wslSSHTransportTarget(target SSHTarget, wslDir, mountRoot string) SSHTarget {
	wslTarget := target
	if target.Key != "" {
		wslTarget.Key = wslDir + "/identity"
	}
	if target.CertificateFile != "" {
		wslTarget.CertificateFile = wslDir + "/identity-cert.pub"
	}
	wslTarget.KnownHostsFile = windowsToWSLMountPathWithRoot(knownHostsFile(target), mountRoot)
	wslTarget.ProxyCommand = windowsToWSLPathWithRoot(target.ProxyCommand, mountRoot)
	return wslTarget
}

func writeWSLSSHTransportFile(ctx context.Context, wslExe, path string, data []byte, target SSHTarget) error {
	cmd := exec.CommandContext(ctx, wslExe, "sh", "-c", `umask 077; cat > "$1"; chmod 600 "$1"`, "crabbox", path)
	cmd.Stdin = bytes.NewReader(data)
	applyTargetChildEnvironment(cmd, target)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("write private WSL SSH transport file: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (s *sshTransportSession) commandPrefix() []string {
	return []string{"-F", s.configPath}
}

func (s *sshTransportSession) rsyncRemoteShell() string {
	return strings.Join(rsyncShellWords(append([]string{"ssh"}, s.commandPrefix()...)), " ")
}

func rsyncShellWords(words []string) []string {
	quoted := make([]string, len(words))
	for index, word := range words {
		quoted[index] = "'" + strings.ReplaceAll(word, "'", "''") + "'"
	}
	return quoted
}

func (s *sshTransportSession) host() string {
	if s.destination != "" {
		return s.destination
	}
	return sshTransportHostAlias
}

func sshTransportDestination(target SSHTarget) string {
	return sshTransportHostAlias
}

type sshTransportConfigRoute struct {
	hostName         string
	hostKeyAlias     string
	identityFiles    []string
	identitiesOnly   bool
	identityAgent    string
	certificateFiles []string
	proxyJump        string
	proxyCommand     string
	proxyUseFDPass   bool
	userConfigPath   string
	jumpConfigPath   string
}

type sshTransportRouteCapabilities struct {
	remoteCommand bool
	sessionType   bool
}

func resolveSSHTransportConfigRoute(ctx context.Context, target SSHTarget, localForward bool) (_ sshTransportConfigRoute, err error) {
	if !target.SSHConfigProxy {
		return sshTransportConfigRoute{}, nil
	}
	dir, err := os.MkdirTemp("", "crabbox-ssh-route-*")
	if err != nil {
		return sshTransportConfigRoute{}, fmt.Errorf("create private SSH route directory: %w", err)
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	if err := secureSSHTransportPath(dir, true); err != nil {
		return sshTransportConfigRoute{}, fmt.Errorf("secure private SSH route directory: %w", err)
	}
	seedPath := filepath.Join(dir, "ssh_config")
	userConfigPath := ""
	if home, err := os.UserHomeDir(); err == nil {
		candidate := filepath.Join(home, ".ssh", "config")
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			userConfigPath = candidate
		}
	}
	seed, err := renderSSHTransportRouteSeed(target, userConfigPath)
	if err != nil {
		return sshTransportConfigRoute{}, err
	}
	if err := os.WriteFile(seedPath, []byte(seed), 0o600); err != nil {
		return sshTransportConfigRoute{}, fmt.Errorf("write private SSH route config: %w", err)
	}
	if err := secureSSHTransportPath(seedPath, false); err != nil {
		return sshTransportConfigRoute{}, fmt.Errorf("secure private SSH route config: %w", err)
	}
	capabilityPath := filepath.Join(dir, "capability_config")
	if err := os.WriteFile(capabilityPath, nil, 0o600); err != nil {
		return sshTransportConfigRoute{}, fmt.Errorf("write private SSH capability config: %w", err)
	}
	if err := secureSSHTransportPath(capabilityPath, false); err != nil {
		return sshTransportConfigRoute{}, fmt.Errorf("secure private SSH capability config: %w", err)
	}
	capabilities := probeSSHTransportRouteCapabilities(ctx, target, capabilityPath)
	if cause := context.Cause(ctx); cause != nil {
		return sshTransportConfigRoute{}, cause
	}
	args := sshTransportRouteCommandArgs(seedPath, target, localForward, capabilities)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err = runOwnedSSHTransportCommand(ctx, target, args, &stdout, &stderr)
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return sshTransportConfigRoute{}, cause
		}
		diagnostic := strings.TrimSpace(redactSSHTransportDiagnostic(target, stderr.String()))
		return sshTransportConfigRoute{}, fmt.Errorf("resolve OpenSSH route for %s: %w: %s", target.Host, err, diagnostic)
	}
	route := parseSSHTransportConfigRoute(stdout.String(), userConfigPath)
	if target.Key == "" {
		route.identityFiles, err = resolveSSHTransportAuthenticationPaths(ctx, target, localForward, capabilities, seedPath, route.identityFiles)
		if err != nil {
			return sshTransportConfigRoute{}, err
		}
	} else {
		route.identityFiles = nil
	}
	if target.CertificateFile == "" {
		route.certificateFiles, err = resolveSSHTransportAuthenticationPaths(ctx, target, localForward, capabilities, seedPath, route.certificateFiles)
		if err != nil {
			return sshTransportConfigRoute{}, err
		}
	} else {
		route.certificateFiles = nil
	}
	route.identityAgent, err = resolveSSHTransportIdentityAgent(ctx, target, localForward, capabilities, seedPath, route.identityAgent)
	if err != nil {
		return sshTransportConfigRoute{}, err
	}
	return route, nil
}

func parseSSHTransportConfigRoute(output, userConfigPath string) sshTransportConfigRoute {
	route := sshTransportConfigRoute{userConfigPath: userConfigPath}
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.ToLower(key) {
		case "hostname":
			route.hostName = value
		case "identityfile":
			route.identityFiles = append(route.identityFiles, value)
		case "identitiesonly":
			route.identitiesOnly = strings.EqualFold(value, "yes")
		case "identityagent":
			route.identityAgent = value
		case "certificatefile":
			route.certificateFiles = append(route.certificateFiles, value)
		case "proxyjump":
			if !strings.EqualFold(value, "none") {
				route.proxyJump = value
			}
		case "proxycommand":
			if !strings.EqualFold(value, "none") {
				route.proxyCommand = value
			}
		case "proxyusefdpass":
			route.proxyUseFDPass = strings.EqualFold(value, "yes")
		case "hostkeyalias":
			route.hostKeyAlias = value
		}
	}
	return route
}

func resolveSSHTransportIdentityAgent(
	ctx context.Context,
	target SSHTarget,
	localForward bool,
	capabilities sshTransportRouteCapabilities,
	seedPath, value string,
) (string, error) {
	legacyEnvironmentReference := strings.HasPrefix(value, "$") && !strings.HasPrefix(value, "${")
	if value == "" || strings.EqualFold(value, "none") || strings.EqualFold(value, "SSH_AUTH_SOCK") || legacyEnvironmentReference {
		return value, nil
	}
	resolved, err := resolveSSHTransportAuthenticationPaths(ctx, target, localForward, capabilities, seedPath, []string{value})
	if err != nil {
		return "", err
	}
	return resolved[0], nil
}

func resolveSSHTransportAuthenticationPaths(
	ctx context.Context,
	target SSHTarget,
	localForward bool,
	capabilities sshTransportRouteCapabilities,
	seedPath string,
	paths []string,
) ([]string, error) {
	resolved := make([]string, 0, len(paths))
	home := ""
	for index, path := range paths {
		if err := validateSSHTransportRoutedAuthenticationPath("authentication file", path); err != nil {
			return nil, err
		}
		if strings.EqualFold(path, "none") {
			resolved = append(resolved, path)
			continue
		}
		if strings.Contains(path, "%d") {
			if home == "" {
				currentUser, err := osuser.Current()
				if err != nil {
					return nil, fmt.Errorf("resolve local home for SSH authentication path: %w", err)
				}
				home = currentUser.HomeDir
			}
			path = expandSSHTransportHomeToken(path, home)
		}
		probePath := filepath.Join(filepath.Dir(seedPath), fmt.Sprintf("authentication_path_config_%d", index))
		var probe strings.Builder
		// OpenSSH 7.x accepted %d for authentication files but not ControlPath,
		// so resolve that stable local value first. Asking the invoked client to
		// render the remaining shared tokens keeps version-specific semantics and
		// preserves the original Host alias.
		writeSSHTransportConfigValue(&probe, "ControlPath", path)
		writeSSHTransportLiteralConfigValue(&probe, "Include", seedPath)
		if err := os.WriteFile(probePath, []byte(probe.String()), 0o600); err != nil {
			return nil, fmt.Errorf("write private SSH authentication path config: %w", err)
		}
		if err := secureSSHTransportPath(probePath, false); err != nil {
			return nil, fmt.Errorf("secure private SSH authentication path config: %w", err)
		}

		args := sshTransportRouteCommandArgs(probePath, target, localForward, capabilities)
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		if err := runOwnedSSHTransportCommand(ctx, target, args, &stdout, &stderr); err != nil {
			if cause := context.Cause(ctx); cause != nil {
				return nil, cause
			}
			diagnostic := strings.TrimSpace(redactSSHTransportDiagnostic(target, stderr.String()))
			return nil, fmt.Errorf("resolve OpenSSH authentication path for %s: %w: %s", target.Host, err, diagnostic)
		}
		expanded, ok := parseSSHTransportControlPath(stdout.String())
		if !ok {
			return nil, fmt.Errorf("resolve OpenSSH authentication path for %s: OpenSSH omitted the expanded path", target.Host)
		}
		resolved = append(resolved, expanded)
	}
	return resolved, nil
}

func expandSSHTransportHomeToken(value, home string) string {
	var expanded strings.Builder
	for index := 0; index < len(value); index++ {
		if value[index] == '%' && index+1 < len(value) {
			switch value[index+1] {
			case '%':
				expanded.WriteString("%%")
				index++
				continue
			case 'd':
				expanded.WriteString(strings.ReplaceAll(home, "%", "%%"))
				index++
				continue
			}
		}
		expanded.WriteByte(value[index])
	}
	return expanded.String()
}

func parseSSHTransportControlPath(output string) (string, bool) {
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok && strings.EqualFold(key, "controlpath") {
			return strings.TrimSpace(value), true
		}
	}
	return "", false
}

func sshTransportRouteCommandArgs(seedPath string, target SSHTarget, localForward bool, capabilities sshTransportRouteCapabilities) []string {
	// Command-line options override config-file values. Neutralize interactive
	// session directives while preserving the route selected by the user's
	// Host and Match blocks.
	sessionType := "default"
	if localForward {
		sessionType = "none"
	}
	args := []string{"-G", "-F", seedPath}
	if capabilities.remoteCommand {
		args = append(args, "-o", "RemoteCommand=none")
	}
	if capabilities.sessionType {
		args = append(args, "-o", "SessionType="+sessionType)
	}
	if localForward {
		args = append(args, "-N")
	}
	args = append(args, "--", target.Host)
	if !localForward {
		command := "rsync --server"
		if isWindowsWSL2Target(target) {
			command = "wsl.exe rsync --server"
		}
		args = append(args, command)
	}
	return args
}

func probeSSHTransportRouteCapabilities(ctx context.Context, target SSHTarget, configPath string) sshTransportRouteCapabilities {
	supports := func(option string) bool {
		if context.Cause(ctx) != nil {
			return false
		}
		args := []string{"-G", "-F", configPath, "-o", option, "--", "crabbox-option-probe.invalid"}
		return runOwnedSSHTransportCommand(ctx, target, args, io.Discard, io.Discard) == nil
	}
	return sshTransportRouteCapabilities{
		remoteCommand: supports("RemoteCommand=none"),
		sessionType:   supports("SessionType=default"),
	}
}

func runOwnedSSHTransportCommand(ctx context.Context, target SSHTarget, args []string, stdout, stderr io.Writer) error {
	handle := pondMeshExecCommand(ctx, target.ChildEnvDenylist, "ssh", args...)
	if execHandle, ok := handle.(*pondMeshExecHandle); ok {
		execHandle.cmd.Stdout = stdout
		execHandle.cmd.Stderr = stderr
	}
	if err := handle.Start(); err != nil {
		return err
	}
	err := handle.Wait()
	if ctxErr := context.Cause(ctx); ctxErr != nil && handle.WasTerminatedByOurCancel() {
		return ctxErr
	}
	return err
}

func renderSSHTransportRouteSeed(target SSHTarget, userConfigPath string) (string, error) {
	if err := validateSSHTransportLiteralValue("config path", userConfigPath); err != nil {
		return "", err
	}
	for name, value := range map[string]string{"host": target.Host, "user": target.User, "port": target.Port} {
		if err := validateSSHCommandTokenValue(name, value); err != nil {
			return "", err
		}
	}
	var seed strings.Builder
	// The include is global, avoiding interpretation of the requested alias as
	// a Host pattern. User and port live in this protected config, keeping
	// token-style usernames out of process arguments; validation above protects
	// Match exec and ProxyCommand expansion in the included config.
	writeSSHTransportLiteralConfigValue(&seed, "User", target.User)
	writeSSHTransportLiteralConfigValue(&seed, "Port", target.Port)
	if userConfigPath != "" {
		writeSSHTransportLiteralConfigValue(&seed, "Include", userConfigPath)
	}
	return seed.String(), nil
}

func writeSSHTransportJumpConfig(dir, userConfigPath string) (string, error) {
	if err := validateSSHTransportLiteralValue("config path", userConfigPath); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "jump_config")
	var config strings.Builder
	config.WriteString("Host *\n")
	config.WriteString("  ClearAllForwardings yes\n")
	config.WriteString("  ForwardAgent no\n")
	config.WriteString("  ForwardX11 no\n")
	config.WriteString("  ForwardX11Trusted no\n")
	config.WriteString("  RequestTTY no\n")
	config.WriteString("  RemoteCommand none\n")
	config.WriteString("  PermitLocalCommand no\n")
	config.WriteString("  BatchMode yes\n")
	config.WriteString("  ControlMaster no\n")
	config.WriteString("  ControlPath none\n")
	config.WriteString("  ControlPersist no\n")
	if userConfigPath != "" {
		writeSSHTransportLiteralConfigValue(&config, "Include", userConfigPath)
	}
	if err := os.WriteFile(path, []byte(config.String()), 0o600); err != nil {
		return "", fmt.Errorf("write private SSH jump config: %w", err)
	}
	if err := secureSSHTransportPath(path, false); err != nil {
		return "", fmt.Errorf("secure private SSH jump config: %w", err)
	}
	return path, nil
}

func expandSSHProxyOriginalHost(command, host string) string {
	host = strings.ReplaceAll(host, "%", "%%")
	var b strings.Builder
	for index := 0; index < len(command); index++ {
		if command[index] == '%' && index+1 < len(command) {
			next := command[index+1]
			if next == 'n' {
				b.WriteString(host)
				index++
				continue
			}
			b.WriteByte(command[index])
			b.WriteByte(next)
			index++
			continue
		}
		b.WriteByte(command[index])
	}
	return b.String()
}

func expandSSHProxyJumpTokens(command, originalHost, host, user, port string) string {
	values := map[byte]string{
		'n': originalHost,
		'h': host,
		'r': user,
		'p': port,
	}
	var expanded strings.Builder
	for index := 0; index < len(command); index++ {
		if command[index] == '%' && index+1 < len(command) {
			next := command[index+1]
			if value, ok := values[next]; ok {
				expanded.WriteString(strings.ReplaceAll(value, "%", "%%"))
				index++
				continue
			}
			expanded.WriteByte(command[index])
			expanded.WriteByte(next)
			index++
			continue
		}
		expanded.WriteByte(command[index])
	}
	return expanded.String()
}

func proxyJumpCommand(route sshTransportConfigRoute) string {
	args := []string{"ssh"}
	if route.jumpConfigPath != "" {
		// This command is embedded in ProxyCommand. Protect path percent signs
		// from the outer OpenSSH client's token expansion.
		args = append(args, "-F", strings.ReplaceAll(route.jumpConfigPath, "%", "%%"))
	}
	args = append(args,
		"-o", "ClearAllForwardings=yes",
		"-o", "RequestTTY=no",
		"-o", "PermitLocalCommand=no",
		"-o", "BatchMode=yes",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		"-o", "ControlPersist=no",
	)
	hops := strings.Split(route.proxyJump, ",")
	if len(hops) > 1 {
		args = append(args, "-J", strings.Join(hops[:len(hops)-1], ","))
	}
	args = append(args, "-W", "[%h]:%p")
	args = append(args, proxyJumpHopArgs(hops[len(hops)-1])...)
	return strings.Join(sshProxyCommandWords(args), " ")
}

func proxyJumpHopArgs(hop string) []string {
	if len(hop) >= len("ssh://") && strings.EqualFold(hop[:len("ssh://")], "ssh://") {
		return []string{hop}
	}
	user := ""
	hostPort := hop
	if index := strings.LastIndexByte(hostPort, '@'); index >= 0 {
		user = hostPort[:index+1]
		hostPort = hostPort[index+1:]
	}
	host := hostPort
	port := ""
	if strings.HasPrefix(hostPort, "[") {
		if end := strings.IndexByte(hostPort, ']'); end > 0 {
			host = hostPort[1:end]
			if suffix := hostPort[end+1:]; strings.HasPrefix(suffix, ":") {
				port = strings.TrimPrefix(suffix, ":")
			}
		}
	} else if strings.Count(hostPort, ":") == 1 {
		if index := strings.LastIndexByte(hostPort, ':'); index > 0 {
			host = hostPort[:index]
			port = hostPort[index+1:]
		}
	}
	args := make([]string, 0, 3)
	if port != "" {
		args = append(args, "-p", port)
	}
	return append(args, user+host)
}

func renderSSHTransportConfig(target SSHTarget, localForward bool) (string, error) {
	return renderSSHTransportConfigWithRoute(target, localForward, sshTransportConfigRoute{})
}

func renderSSHTransportConfigWithRoute(target SSHTarget, localForward bool, route sshTransportConfigRoute) (string, error) {
	hostName := target.Host
	if route.hostName != "" {
		hostName = route.hostName
	}
	for _, host := range []string{target.Host, hostName} {
		if err := validateSSHCommandTokenValue("host", host); err != nil {
			return "", err
		}
	}
	hostKeyAlias := route.hostKeyAlias
	proxyCommand := target.ProxyCommand
	if proxyCommand == "" {
		proxyCommand = route.proxyCommand
	}
	proxyUseFDPass := route.proxyUseFDPass
	if proxyCommand != "" {
		for token, field := range map[string]struct {
			name  string
			value string
		}{
			"%n": {name: "host", value: target.Host},
			"%h": {name: "host", value: hostName},
			"%r": {name: "user", value: target.User},
			"%p": {name: "port", value: target.Port},
		} {
			if strings.Contains(proxyCommand, token) {
				if err := validateSSHCommandTokenValue(field.name, field.value); err != nil {
					return "", err
				}
			}
		}
		proxyCommand = expandSSHProxyOriginalHost(proxyCommand, target.Host)
	} else if route.proxyJump != "" {
		route.proxyJump = expandSSHProxyJumpTokens(route.proxyJump, target.Host, hostName, target.User, target.Port)
		proxyCommand = proxyJumpCommand(route)
		proxyUseFDPass = false
	}
	identityFiles := route.identityFiles
	if target.Key != "" {
		identityFiles = []string{target.Key}
	}
	certificateFiles := route.certificateFiles
	if target.CertificateFile != "" {
		certificateFiles = []string{target.CertificateFile}
	}
	for name, value := range map[string]string{
		"host":             hostName,
		"user":             target.User,
		"port":             target.Port,
		"known hosts file": knownHostsFile(target),
		"host key alias":   hostKeyAlias,
		"identity agent":   route.identityAgent,
		"proxy command":    proxyCommand,
	} {
		if strings.ContainsAny(value, "\x00\r\n") {
			return "", exit(2, "resolved SSH %s contains an unsupported control character", name)
		}
		if name != "proxy command" {
			if err := validateSSHTransportLiteralValue(name, value); err != nil {
				return "", err
			}
		}
	}
	for _, field := range []struct {
		name   string
		values []string
	}{
		{name: "identity file", values: identityFiles},
		{name: "certificate file", values: certificateFiles},
	} {
		for _, value := range field.values {
			if err := validateSSHTransportRoutedAuthenticationPath(field.name, value); err != nil {
				return "", err
			}
		}
	}
	if strings.TrimSpace(target.Host) == "" || strings.TrimSpace(target.User) == "" || strings.TrimSpace(target.Port) == "" {
		return "", exit(2, "resolved SSH transport requires host, user, and port")
	}

	var b strings.Builder
	b.WriteString("Host " + quoteSSHTransportConfigValue(sshTransportDestination(target)) + "\n")
	writeSSHTransportLiteralConfigValue(&b, "HostName", hostName)
	writeSSHTransportLiteralConfigValue(&b, "User", target.User)
	writeSSHTransportLiteralConfigValue(&b, "Port", target.Port)
	b.WriteString("  BatchMode yes\n")
	b.WriteString("  ForwardAgent no\n")
	b.WriteString("  ForwardX11 no\n")
	b.WriteString("  ForwardX11Trusted no\n")
	b.WriteString("  RequestTTY no\n")
	b.WriteString("  RemoteCommand none\n")
	b.WriteString("  ConnectTimeout 10\n")
	b.WriteString("  ConnectionAttempts 1\n")
	b.WriteString("  ServerAliveInterval 15\n")
	b.WriteString("  ServerAliveCountMax 2\n")
	if target.DisableHostKeyChecking {
		b.WriteString("  StrictHostKeyChecking no\n")
		writeSSHTransportLiteralConfigValue(&b, "UserKnownHostsFile", "/dev/null")
		b.WriteString("  LogLevel ERROR\n")
	} else {
		b.WriteString("  StrictHostKeyChecking accept-new\n")
		writeSSHTransportLiteralConfigValue(&b, "UserKnownHostsFile", knownHostsFile(target))
	}
	// A transport command owns one process tree. Never let a multiplexed master
	// retain a copy or forward after that process exits.
	b.WriteString("  ControlMaster no\n")
	b.WriteString("  ControlPath none\n")
	b.WriteString("  ControlPersist no\n")
	for _, identityFile := range identityFiles {
		writeSSHTransportLiteralConfigValue(&b, "IdentityFile", identityFile)
	}
	if target.Key != "" || route.identitiesOnly {
		b.WriteString("  IdentitiesOnly yes\n")
	}
	if route.identityAgent != "" {
		writeSSHTransportLiteralConfigValue(&b, "IdentityAgent", route.identityAgent)
	}
	for _, certificateFile := range certificateFiles {
		writeSSHTransportLiteralConfigValue(&b, "CertificateFile", certificateFile)
	}
	if hostKeyAlias != "" {
		writeSSHTransportLiteralConfigValue(&b, "HostKeyAlias", hostKeyAlias)
	}
	if proxyCommand != "" {
		// ProxyCommand consumes the rest of its config line as a shell command.
		// Quoting the whole value makes OpenSSH treat it as one executable name.
		b.WriteString("  ProxyCommand ")
		b.WriteString(proxyCommand)
		b.WriteByte('\n')
		if proxyUseFDPass {
			b.WriteString("  ProxyUseFdpass yes\n")
		}
	}
	if localForward {
		b.WriteString("  ExitOnForwardFailure yes\n")
		b.WriteString("  GatewayPorts no\n")
	}
	return b.String(), nil
}

func writeSSHTransportConfigValue(b *strings.Builder, name, value string) {
	b.WriteString("  ")
	b.WriteString(name)
	b.WriteByte(' ')
	b.WriteString(quoteSSHTransportConfigValue(value))
	b.WriteByte('\n')
}

func writeSSHTransportLiteralConfigValue(b *strings.Builder, name, value string) {
	if sshTransportDirectiveExpandsPercent(name) {
		value = strings.ReplaceAll(value, "%", "%%")
	}
	writeSSHTransportConfigValue(b, name, value)
}

func sshTransportDirectiveExpandsPercent(name string) bool {
	switch strings.ToLower(name) {
	case "hostname", "user", "identityfile", "identityagent", "certificatefile", "include", "userknownhostsfile":
		return true
	default:
		return false
	}
}

func validateSSHTransportLiteralValue(name, value string) error {
	if strings.ContainsAny(value, "\x00\r\n") {
		return exit(2, "resolved SSH %s contains an unsupported control character", name)
	}
	if strings.Contains(value, "${") {
		return exit(2, "resolved SSH %s contains unsupported OpenSSH environment expansion syntax", name)
	}
	if strings.Contains(value, `"`) {
		return exit(2, "resolved SSH %s contains an unsupported double quote", name)
	}
	return nil
}

func validateSSHTransportRoutedAuthenticationPath(name, value string) error {
	if strings.ContainsAny(value, "\x00\r\n") {
		return exit(2, "resolved SSH %s contains an unsupported control character", name)
	}
	if strings.Contains(value, `"`) {
		return exit(2, "resolved SSH %s contains an unsupported double quote", name)
	}
	return nil
}

func validateSSHCommandTokenValue(name, value string) error {
	if value == "" {
		return nil
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' {
			continue
		}
		switch char {
		case '.', '_', '-', ':', '@', '+', '=', ',', '/', '\\', '%':
			continue
		}
		return exit(2, "resolved SSH %s contains a character unsafe for ProxyCommand expansion", name)
	}
	return nil
}

func quoteSSHTransportConfigValue(value string) string {
	return `"` + value + `"`
}

// resolveSSHTransportLeaseTargetForRepo resolves the same managed SSH target
// as run/sync without invoking the legacy argv-rendered transport probe.
func (a App) resolveSSHTransportLeaseTargetForRepo(ctx context.Context, cfg *Config, id string, printFallback, reclaim bool) (LeaseTarget, error) {
	repo, err := findRepo()
	if err != nil {
		return LeaseTarget{}, err
	}
	lease, err := a.resolveSSHLeaseWithRequestConfig(ctx, cfg, ResolveRequest{Repo: repo, ID: id, Reclaim: reclaim}, false)
	if err != nil {
		return LeaseTarget{}, err
	}
	resolved, err := resolveSSHTargetNetwork(ctx, *cfg, lease.Server, lease.SSH, false)
	if err != nil {
		return LeaseTarget{}, err
	}
	lease.SSH = resolved.Target
	if printFallback && resolved.FallbackReason != "" {
		fmt.Fprintf(a.Stderr, "network fallback %s\n", resolved.FallbackReason)
	}
	return lease, nil
}

func (a App) probeSSHTransportLeaseAfterClaim(ctx context.Context, cfg Config, lease *LeaseTarget, reclaim bool) error {
	if lease == nil || lease.SSH.Host == "" {
		return nil
	}
	configuredPort := lease.SSH.Port
	_ = probePrivateSSHTransport(ctx, &lease.SSH, 4*time.Second)
	if lease.SSH.Port == configuredPort {
		return nil
	}
	// Refresh claim state after the first ownership-checked claim, then persist
	// the probed endpoint through another compare-and-swap ownership check.
	repo, err := findRepo()
	if err != nil {
		return err
	}
	server := lease.Server
	server.claimSnapshot = leaseClaim{}
	server.claimSnapshotExists = false
	server.claimSnapshotSet = false
	if err := a.claimLeaseTargetForRepoAndRegister(ctx, lease.LeaseID, serverSlug(server), cfg, server, lease.SSH, repo.Root, reclaim); err != nil {
		return err
	}
	a.touchLeaseTargetBestEffort(ctx, cfg, *lease, "")
	return nil
}

func probePrivateSSHTransport(ctx context.Context, target *SSHTarget, timeout time.Duration) bool {
	if target == nil || target.Host == "" {
		return false
	}
	for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
		if context.Cause(ctx) != nil {
			return false
		}
		probeCtx, cancel := context.WithTimeout(ctx, timeout)
		probe := *target
		probe.Port = port
		probe.FallbackPorts = nil
		session, err := newSSHTransportSession(probeCtx, probe, false)
		if err != nil {
			cancel()
			return false
		}
		args := append(session.commandPrefix(), "-n", session.host(), wrapRemoteForTarget(probe, sshTransportProbeCommand(probe)))
		runErr := runOwnedSSHTransportCommand(probeCtx, probe, args, io.Discard, io.Discard)
		closeErr := session.Close()
		cancel()
		if runErr == nil && closeErr == nil {
			target.Port = port
			return true
		}
		if context.Cause(ctx) != nil {
			return false
		}
	}
	return false
}
