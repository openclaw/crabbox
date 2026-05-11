package cli

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
)

type SSHTarget struct {
	User           string
	Host           string
	Key            string
	Port           string
	FallbackPorts  []string
	TargetOS       string
	WindowsMode    string
	ReadyCheck     string
	AuthSecret     bool
	NetworkKind    NetworkMode
	SSHConfigProxy bool
	ProxyCommand   string
}

func isLocalMacTarget(target SSHTarget) bool {
	if runtime.GOOS != "darwin" || target.TargetOS != targetMacOS {
		return false
	}
	return isLocalHost(target.Host)
}

func isLocalHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" || host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}
	name, err := os.Hostname()
	if err != nil {
		return false
	}
	name = strings.ToLower(strings.TrimSpace(name))
	short, _, _ := strings.Cut(name, ".")
	return host == name || host == short
}

func sshTargetFromConfig(cfg Config, host string) SSHTarget {
	return sshTargetForLease(cfg, host, cfg.SSHUser, cfg.SSHPort)
}

func SSHTargetFromConfig(cfg Config, host string) SSHTarget {
	return sshTargetFromConfig(cfg, host)
}

func sshTargetForLease(cfg Config, host, user, port string) SSHTarget {
	if user == "" {
		user = cfg.SSHUser
	}
	if port == "" {
		port = cfg.SSHPort
	}
	return SSHTarget{
		User:          user,
		Host:          host,
		Key:           cfg.SSHKey,
		Port:          port,
		FallbackPorts: cfg.SSHFallbackPorts,
		TargetOS:      cfg.TargetOS,
		WindowsMode:   cfg.WindowsMode,
	}
}

func waitForSSH(ctx context.Context, target *SSHTarget, stderr io.Writer) error {
	return waitForSSHReady(ctx, target, stderr, "bootstrap", 20*time.Minute)
}

func WaitForSSH(ctx context.Context, target *SSHTarget, stderr io.Writer) error {
	return waitForSSH(ctx, target, stderr)
}

func bootstrapWaitTimeout(cfg Config) time.Duration {
	if cfg.Desktop || cfg.Browser {
		return 45 * time.Minute
	}
	return 20 * time.Minute
}

func BootstrapWaitTimeout(cfg Config) time.Duration {
	return bootstrapWaitTimeout(cfg)
}

func waitForSSHReady(ctx context.Context, target *SSHTarget, stderr io.Writer, phase string, timeout time.Duration) error {
	start := time.Now()
	deadline := time.Now().Add(timeout)
	lastPorts := ""
	for {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if time.Now().After(deadline) {
			if lastPorts != "" {
				return exit(5, "timed out waiting for SSH on %s during %s ports=%s", target.Host, phase, lastPorts)
			}
			return exit(5, "timed out waiting for SSH on %s during %s", target.Host, phase)
		}
		if target.SSHConfigProxy {
			if runSSHQuietWithOptions(ctx, *target, sshReadyCommand(*target), "5", "1") == nil {
				return nil
			}
			lastPorts = "proxy"
			fmt.Fprintln(stderr, sshWaitProgressMessage(target, phase, target.Port, "", lastPorts, time.Since(start), time.Until(deadline)))
		} else {
			reachablePort := ""
			transportPort := ""
			probes := make([]string, 0, len(sshPortCandidates(target.Port, target.FallbackPorts)))
			for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
				probe := *target
				probe.Port = port
				conn, err := net.DialTimeout("tcp", net.JoinHostPort(probe.Host, probe.Port), 5*time.Second)
				if err != nil {
					probes = append(probes, port+":closed")
					continue
				}
				_ = conn.Close()
				if reachablePort == "" {
					reachablePort = probe.Port
				}
				if runSSHQuietWithOptions(ctx, probe, sshTransportProbeCommand(probe), "5", "1") != nil {
					probes = append(probes, port+":tcp")
					continue
				}
				if transportPort == "" {
					transportPort = probe.Port
				}
				if runSSHQuietWithOptions(ctx, probe, sshReadyCommand(probe), "5", "1") == nil {
					if target.Port != probe.Port {
						fmt.Fprintf(stderr, "using ssh port %s for %s (configured %s not ready)\n", probe.Port, target.Host, target.Port)
						target.Port = probe.Port
					}
					return nil
				}
				probes = append(probes, port+":auth")
			}
			lastPorts = strings.Join(probes, ",")
			fmt.Fprintln(stderr, sshWaitProgressMessage(target, phase, reachablePort, transportPort, lastPorts, time.Since(start), time.Until(deadline)))
		}
		time.Sleep(10 * time.Second)
	}
}

func WaitForSSHReady(ctx context.Context, target *SSHTarget, stderr io.Writer, phase string, timeout time.Duration) error {
	return waitForSSHReady(ctx, target, stderr, phase, timeout)
}

func sshWaitProgressMessage(target *SSHTarget, phase, reachablePort, transportPort, portStatus string, elapsed, remaining time.Duration) string {
	if remaining < 0 {
		remaining = 0
	}
	elapsed = elapsed.Round(time.Second)
	remaining = remaining.Round(time.Second)
	suffix := ""
	if portStatus != "" {
		suffix = " ports=" + portStatus
	}
	if transportPort != "" {
		return fmt.Sprintf("waiting for %s:%s %s ready-check... elapsed=%s remaining=%s%s", target.Host, transportPort, phase, elapsed, remaining, suffix)
	}
	if reachablePort != "" {
		return fmt.Sprintf("waiting for %s:%s %s ssh-auth... elapsed=%s remaining=%s%s", target.Host, reachablePort, phase, elapsed, remaining, suffix)
	}
	return fmt.Sprintf("waiting for %s:%s %s... elapsed=%s remaining=%s%s", target.Host, target.Port, phase, elapsed, remaining, suffix)
}

func probeSSHReady(ctx context.Context, target *SSHTarget, timeout time.Duration) bool {
	if target.Host == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if target.SSHConfigProxy {
		return runSSHQuietWithOptions(ctx, *target, sshReadyCommand(*target), "2", "1") == nil
	}
	for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
		probe := *target
		probe.Port = port
		dialer := net.Dialer{Timeout: minDuration(timeout, 2*time.Second)}
		conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(probe.Host, probe.Port))
		if err != nil {
			continue
		}
		_ = conn.Close()
		if runSSHQuietWithOptions(ctx, probe, sshReadyCommand(probe), "2", "1") == nil {
			target.Port = probe.Port
			return true
		}
	}
	return false
}

func probeSSHTransport(ctx context.Context, target *SSHTarget, timeout time.Duration) bool {
	if target.Host == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if target.SSHConfigProxy {
		return runSSHQuietWithOptions(ctx, *target, sshTransportProbeCommand(*target), "2", "1") == nil
	}
	for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
		probe := *target
		probe.Port = port
		dialer := net.Dialer{Timeout: minDuration(timeout, 2*time.Second)}
		conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(probe.Host, probe.Port))
		if err != nil {
			continue
		}
		_ = conn.Close()
		if runSSHQuietWithOptions(ctx, probe, sshTransportProbeCommand(probe), "2", "1") == nil {
			target.Port = probe.Port
			return true
		}
	}
	return false
}

func sshTransportProbeCommand(SSHTarget) string {
	return "exit 0"
}

func sshReadyCommand(target SSHTarget) string {
	if target.ReadyCheck != "" {
		return target.ReadyCheck
	}
	if isWindowsNativeTarget(target) {
		return powershellCommand(`$ErrorActionPreference = "Stop"
git --version | Out-Null
tar --version | Out-Null
if (-not (Test-Path -LiteralPath ` + psQuote(targetWindowsReadyRoot(target)) + `)) { throw "work root missing" }`)
	}
	return "test -x /usr/local/bin/crabbox-ready && /usr/local/bin/crabbox-ready >/tmp/crabbox-ready.log 2>&1"
}

func targetWindowsReadyRoot(target SSHTarget) string {
	_ = target
	return `C:\`
}

func sshPortCandidates(port string, fallbackPorts []string) []string {
	if fallbackPorts == nil {
		fallbackPorts = []string{"22"}
	}
	return uniqueSSHPorts(append([]string{port}, fallbackPorts...))
}

func uniqueSSHPorts(ports []string) []string {
	seen := make(map[string]bool, len(ports))
	out := make([]string, 0, len(ports))
	for _, port := range ports {
		port = strings.TrimSpace(port)
		if port == "" || seen[port] {
			continue
		}
		seen[port] = true
		out = append(out, port)
	}
	return out
}

func runSSHQuiet(ctx context.Context, target SSHTarget, remote string) error {
	return runSSHQuietWithOptions(ctx, target, remote, "10", "3")
}

func runSSHQuietWithOptions(ctx context.Context, target SSHTarget, remote, connectTimeout, connectionAttempts string) error {
	remote = wrapRemoteForTarget(target, remote)
	var lastErr error
	for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
		probe := target
		probe.Port = port
		cmd := exec.CommandContext(ctx, "ssh", sshArgsWithOptions(probe, remote, connectTimeout, connectionAttempts)...)
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		err := cmd.Run()
		if err == nil {
			return nil
		}
		lastErr = err
		if !shouldRetrySSHPort(err) {
			return err
		}
	}
	return lastErr
}

func runSSHOutput(ctx context.Context, target SSHTarget, remote string) (string, error) {
	remote = wrapRemoteForTarget(target, remote)
	var lastOut []byte
	var lastErr error
	for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
		probe := target
		probe.Port = port
		cmd := exec.CommandContext(ctx, "ssh", sshArgs(probe, remote)...)
		out, err := cmd.Output()
		if err == nil {
			return strings.TrimSpace(string(out)), nil
		}
		lastOut = out
		lastErr = err
		if !shouldRetrySSHPort(err) {
			return "", err
		}
	}
	return strings.TrimSpace(string(lastOut)), lastErr
}

func runSSHCombinedOutput(ctx context.Context, target SSHTarget, remote string) (string, error) {
	remote = wrapRemoteForTarget(target, remote)
	var lastOut []byte
	var lastErr error
	for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
		probe := target
		probe.Port = port
		cmd := exec.CommandContext(ctx, "ssh", sshArgs(probe, remote)...)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return strings.TrimSpace(string(out)), nil
		}
		lastOut = out
		lastErr = err
		if !shouldRetrySSHPort(err) {
			return strings.TrimSpace(string(out)), err
		}
	}
	return strings.TrimSpace(string(lastOut)), lastErr
}

func runSSHInputQuiet(ctx context.Context, target SSHTarget, remote, input string) error {
	return runSSHInput(ctx, target, remote, strings.NewReader(input), io.Discard, io.Discard)
}

func runSSHInput(ctx context.Context, target SSHTarget, remote string, input io.Reader, stdout, stderr io.Writer) error {
	remote = wrapRemoteForTarget(target, remote)
	if input == nil {
		input = strings.NewReader("")
	}
	data, err := io.ReadAll(input)
	if err != nil {
		return err
	}
	var lastErr error
	for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
		probe := target
		probe.Port = port
		cmd := exec.CommandContext(ctx, "ssh", sshArgs(probe, remote)...)
		cmd.Stdin = bytes.NewReader(data)
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		err := cmd.Run()
		if err == nil {
			return nil
		}
		lastErr = err
		if !shouldRetrySSHPort(err) {
			return err
		}
	}
	return lastErr
}

func runSSHStream(ctx context.Context, target SSHTarget, remote string, stdout, stderr io.Writer) int {
	code, _ := runSSHStreamResult(ctx, target, remote, stdout, stderr)
	return code
}

func runSSHStreamResult(ctx context.Context, target SSHTarget, remote string, stdout, stderr io.Writer) (int, error) {
	remote = wrapRemoteForTarget(target, remote)
	lastCode := 7
	var lastErr error
	for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
		probe := target
		probe.Port = port
		cmd := exec.CommandContext(ctx, "ssh", sshArgs(probe, remote)...)
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		err := cmd.Run()
		if err == nil {
			return 0, nil
		}
		lastErr = err
		lastCode = exitCode(err)
		if !shouldRetrySSHPort(err) {
			return lastCode, err
		}
	}
	return lastCode, lastErr
}

func isSSHCommandExitError(err error) bool {
	var exitErr *exec.ExitError
	return asExitError(err, &exitErr)
}

func sshArgs(target SSHTarget, remote string) []string {
	return sshArgsWithOptions(target, remote, "10", "3")
}

func shouldRetrySSHPort(err error) bool {
	return exitCode(err) == 255
}

func sshArgsWithOptions(target SSHTarget, remote, connectTimeout, connectionAttempts string) []string {
	return append(sshBaseArgsWithOptions(target, connectTimeout, connectionAttempts),
		target.User+"@"+target.Host,
		remote,
	)
}

func sshBaseArgs(target SSHTarget) []string {
	return sshBaseArgsWithOptions(target, "10", "3")
}

func sshBaseArgsWithOptions(target SSHTarget, connectTimeout, connectionAttempts string) []string {
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + sshConfigFileValue(knownHostsFile(target)),
		"-o", "ConnectTimeout=" + connectTimeout,
		"-o", "ConnectionAttempts=" + connectionAttempts,
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=2",
		"-p", target.Port,
	}
	if target.AuthSecret {
		args = append(args, "-o", "ControlMaster=no")
	} else if runtime.GOOS == "windows" {
		// Windows OpenSSH does not support Unix domain sockets for
		// connection multiplexing; ControlMaster causes
		// "getsockname failed: Not a socket" errors.
		args = append(args, "-o", "ControlMaster=no")
	} else {
		args = append(args,
			"-o", "ControlMaster=auto",
			"-o", "ControlPersist=10m",
			"-o", "ControlPath="+sshControlPath(target),
		)
	}
	if target.Key != "" {
		args = append([]string{"-i", target.Key, "-o", "IdentitiesOnly=yes"}, args...)
	}
	if target.ProxyCommand != "" {
		args = append(args, "-o", "ProxyCommand="+target.ProxyCommand)
	}
	return args
}

func minDuration(left, right time.Duration) time.Duration {
	if left < right {
		return left
	}
	return right
}

func knownHostsFile(target SSHTarget) string {
	if target.Key != "" {
		return filepath.Join(filepath.Dir(target.Key), "known_hosts")
	}
	return filepath.Join(os.Getenv("HOME"), ".ssh", "known_hosts")
}

func sshConfigFileValue(path string) string {
	if strings.ContainsAny(path, " \t\"'") {
		return strconv.Quote(path)
	}
	return path
}

func sshControlPath(target SSHTarget) string {
	scope := target.Key
	if scope == "" {
		scope = target.User
	}
	sum := sha1.Sum([]byte(scope))
	return filepath.Join("/tmp", "crabbox-ssh-"+hex.EncodeToString(sum[:4])+"-%C")
}

type rsyncOptions struct {
	Debug             bool
	Delete            bool
	Checksum          bool
	UseFilesFrom      bool
	FilesFrom         []byte
	Timeout           time.Duration
	HeartbeatInterval time.Duration
}

func rsync(ctx context.Context, target SSHTarget, src, dst string, excludes []string, stdout, stderr io.Writer, opts rsyncOptions) error {
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}
	args := []string{
		"-az",
		"-e", strings.Join(shellWords(append([]string{"ssh"}, sshBaseArgs(target)...)), " "),
	}
	if opts.Delete && !opts.UseFilesFrom {
		args = append(args, "--delete")
	}
	if opts.Checksum {
		args = append(args, "--checksum")
	}
	if opts.UseFilesFrom {
		args = append(args, "--files-from=-", "--from0")
	}
	if isWindowsWSL2Target(target) {
		args = append(args, "--rsync-path", "wsl.exe rsync")
	}
	for _, exclude := range excludes {
		args = append(args, "--exclude", exclude)
	}
	if opts.Debug {
		args = append(args, "--stats", "--itemize-changes", "--progress")
	}
	args = append(args, ensureTrailingSlash(rsyncLocalPath(src)), target.User+"@"+target.Host+":"+dst+"/")
	start := time.Now()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = windowsRsyncCommand(ctx, target, args)
	} else {
		cmd = exec.CommandContext(ctx, "rsync", args...)
	}
	if opts.UseFilesFrom {
		cmd.Stdin = bytes.NewReader(opts.FilesFrom)
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	stopHeartbeat := startSyncHeartbeat(stderr, start, opts.HeartbeatInterval)
	err := cmd.Run()
	stopHeartbeat()
	if ctx.Err() == context.DeadlineExceeded {
		return exit(6, "rsync timed out after %s", opts.Timeout)
	}
	if opts.Debug {
		fmt.Fprintf(stderr, "rsync elapsed=%s checksum=%t delete=%t\n", time.Since(start).Round(time.Millisecond), opts.Checksum, opts.Delete)
	}
	return err
}

func wrapRemoteForTarget(target SSHTarget, remote string) string {
	if isWindowsNativeTarget(target) {
		if strings.HasPrefix(remote, "powershell.exe ") || strings.HasPrefix(remote, "powershell ") {
			return remote
		}
		return powershellCommand(remote)
	}
	if isWindowsWSL2Target(target) {
		return wsl2Command(remote)
	}
	return remote
}

func wsl2Command(remote string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(remote))
	return powershellCommand(`$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"
$dir = "C:\ProgramData\crabbox\commands"
New-Item -ItemType Directory -Force -Path $dir | Out-Null
$name = "cmd-" + [Guid]::NewGuid().ToString("N") + ".sh"
$path = Join-Path $dir $name
$scriptBytes = [Convert]::FromBase64String("` + encoded + `")
[System.IO.File]::WriteAllBytes($path, $scriptBytes)
$wslPath = "/mnt/c/ProgramData/crabbox/commands/" + $name
try {
  & wsl.exe --exec bash $wslPath
  $code = $LASTEXITCODE
} finally {
  Remove-Item -LiteralPath $path -Force -ErrorAction SilentlyContinue
}
exit $code`)
}

func startSyncHeartbeat(stderr io.Writer, start time.Time, interval time.Duration) func() {
	if interval <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				fmt.Fprintf(stderr, "still syncing after %s...\n", time.Since(start).Round(time.Second))
			}
		}
	}()
	return func() { close(done) }
}

func ensureTrailingSlash(path string) string {
	if strings.HasSuffix(path, "/") {
		return path
	}
	return path + "/"
}

// rsyncLocalPath converts a Windows drive path like C:/foo to /c/foo so that
// MSYS2/Cygwin rsync does not interpret the colon as a remote host separator.
func rsyncLocalPath(path string) string {
	return rsyncLocalPathForGOOS(runtime.GOOS, path)
}

func rsyncLocalPathForGOOS(goos, path string) string {
	if goos != "windows" {
		return path
	}
	path = strings.ReplaceAll(path, `\`, "/")
	if len(path) >= 2 && path[1] == ':' {
		drive := strings.ToLower(string(path[0]))
		return "/" + drive + path[2:]
	}
	return path
}

// windowsRsyncCommand builds an exec.Cmd for rsync on Windows.
// MSYS2/Cygwin rsync has broken signal handling with Windows SSH child
// processes, so we prefer WSL rsync when available. The SSH key is copied
// into WSL /tmp with correct permissions, and paths within args are
// converted to WSL mount paths.
func windowsRsyncCommand(ctx context.Context, target SSHTarget, args []string) *exec.Cmd {
	if _, err := exec.LookPath("wsl"); err != nil {
		// No WSL — fall back to native rsync with MSYS2 workarounds.
		cmd := exec.CommandContext(ctx, "rsync", args...)
		cmd.Env = append(os.Environ(), "MSYS2_ARG_CONV_EXCL=*", "MSYS_NO_PATHCONV=1", "CYGWIN=nodosfilewarning")
		return cmd
	}

	// Prepare WSL key: copy with correct permissions.
	wslKey := ""
	knownHostsPath := ""
	if target.Key != "" {
		wslKey = "/tmp/crabbox-wsl-" + filepath.Base(filepath.Dir(target.Key))
		knownHostsPath = filepath.Join(filepath.Dir(target.Key), "known_hosts")
		cpCmd := exec.Command("wsl", "bash", "-c",
			fmt.Sprintf("mkdir -p /tmp && cp %s %s 2>/dev/null; chmod 600 %s 2>/dev/null",
				shellQuote(windowsToWSLMountPath(target.Key)),
				shellQuote(wslKey),
				shellQuote(wslKey)))
		_ = cpCmd.Run()
	}

	// Convert all args: replace Windows paths inside strings (including
	// the -e "ssh ..." arg which embeds key and known_hosts paths).
	wslArgs := make([]string, len(args))
	for i, arg := range args {
		converted := windowsToWSLPath(arg)
		// Replace key path with WSL temp copy inside -e string
		if wslKey != "" && target.Key != "" {
			keyWSL := windowsToWSLMountPath(target.Key)
			converted = strings.ReplaceAll(converted, keyWSL, wslKey)
		}
		// Replace known_hosts path
		if knownHostsPath != "" {
			khWSL := windowsToWSLMountPath(knownHostsPath)
			wslKH := wslKey + "-known_hosts"
			converted = strings.ReplaceAll(converted, khWSL, wslKH)
		}
		wslArgs[i] = converted
	}
	return exec.CommandContext(ctx, "wsl", append([]string{"rsync"}, wslArgs...)...)
}

// windowsToWSLMountPath converts a single Windows path to WSL /mnt/ form.
func windowsToWSLMountPath(path string) string {
	path = strings.ReplaceAll(path, `\`, "/")
	if len(path) >= 2 && path[1] == ':' {
		drive := strings.ToLower(string(path[0]))
		return "/mnt/" + drive + path[2:]
	}
	if len(path) >= 3 && path[0] == '/' && path[2] == '/' && path[1] >= 'a' && path[1] <= 'z' {
		return "/mnt" + path
	}
	return path
}

// windowsToWSLPath converts Windows paths found anywhere in a string to
// WSL mount paths. Handles both C:\... and /c/... formats embedded in
// larger strings (like the -e "ssh -i C:\path\key ..." argument).
func windowsToWSLPath(s string) string {
	s = strings.ReplaceAll(s, `\`, "/")
	// Replace drive-letter paths: C:/... -> /mnt/c/...
	for i := 0; i < len(s)-2; i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z') && s[i+1] == ':' && s[i+2] == '/' {
			// Only replace if at start of string or preceded by a non-path char
			if i == 0 || s[i-1] == ' ' || s[i-1] == '\'' || s[i-1] == '"' || s[i-1] == '=' {
				drive := strings.ToLower(string(c))
				s = s[:i] + "/mnt/" + drive + s[i+2:]
				i += 4 // skip past /mnt/X
			}
		}
	}
	// Also handle /c/... -> /mnt/c/... (from rsyncLocalPath conversion)
	for i := 0; i < len(s)-2; i++ {
		if s[i] == '/' && s[i+1] >= 'a' && s[i+1] <= 'z' && s[i+2] == '/' {
			if i == 0 || s[i-1] == ' ' || s[i-1] == '\'' || s[i-1] == '"' || s[i-1] == '=' {
				// Avoid converting remote paths like crabbox@host:/work
				if i == 0 || s[i-1] != ':' {
					s = s[:i] + "/mnt" + s[i:]
					i += 4
				}
			}
		}
	}
	return s
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func ShellQuote(s string) string {
	return shellQuote(s)
}

func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func powershellCommand(script string) string {
	script = `$ProgressPreference = "SilentlyContinue"` + "\n" + script
	encoded := base64.StdEncoding.EncodeToString(utf16LE([]byte(script)))
	return "powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand " + encoded
}

func utf16LE(input []byte) []byte {
	encoded := utf16.Encode([]rune(string(input)))
	out := make([]byte, 0, len(encoded)*2)
	for _, unit := range encoded {
		out = append(out, byte(unit), byte(unit>>8))
	}
	return out
}

func remoteCommand(workdir string, env map[string]string, command []string) string {
	return remoteCommandWithEnvFile(workdir, env, "", command)
}

func remoteCommandWithEnvFile(workdir string, env map[string]string, envFile string, command []string) string {
	var b strings.Builder
	writeRemoteCommandPrefix(&b, workdir, env, envFile)
	b.WriteString("bash -lc ")
	b.WriteString(shellQuote(`exec "$@"`))
	b.WriteString(" bash")
	for _, word := range command {
		b.WriteByte(' ')
		b.WriteString(shellQuote(word))
	}
	return b.String()
}

func remoteShellCommand(workdir string, env map[string]string, script string) string {
	return remoteShellCommandWithEnvFile(workdir, env, "", script)
}

func remoteShellCommandWithEnvFile(workdir string, env map[string]string, envFile, script string) string {
	var b strings.Builder
	writeRemoteCommandPrefix(&b, workdir, env, envFile)
	b.WriteString("bash -lc ")
	b.WriteString(shellQuote(script))
	return b.String()
}

func shellScriptFromArgv(command []string) string {
	parts := make([]string, 0, len(command))
	seenCommand := false
	for _, word := range command {
		if isShellControlOperator(word) {
			parts = append(parts, word)
			if resetsShellCommandPosition(word) {
				seenCommand = false
			}
			continue
		}
		if !seenCommand && isShellEnvAssignment(word) {
			key, value, _ := strings.Cut(word, "=")
			parts = append(parts, key+"="+shellQuote(value))
			continue
		}
		seenCommand = true
		parts = append(parts, shellQuote(word))
	}
	return strings.Join(parts, " ")
}

func ShellScriptFromArgv(command []string) string {
	return shellScriptFromArgv(command)
}

func isShellEnvAssignment(word string) bool {
	if word == "" {
		return false
	}
	idx := strings.IndexByte(word, '=')
	if idx <= 0 {
		return false
	}
	for i, r := range word[:idx] {
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

func IsShellEnvAssignment(word string) bool {
	return isShellEnvAssignment(word)
}

func isShellControlOperator(word string) bool {
	switch word {
	case "&&", "||", ";", "|", ">", ">>", "<", "2>", "2>>":
		return true
	default:
		return false
	}
}

func resetsShellCommandPosition(word string) bool {
	switch word {
	case "&&", "||", ";", "|":
		return true
	default:
		return false
	}
}

func writeRemoteCommandPrefix(b *strings.Builder, workdir string, env map[string]string, envFile string) {
	b.WriteString("cd ")
	b.WriteString(shellQuote(workdir))
	b.WriteString(" && ")
	if envFile != "" {
		b.WriteString("if [ -f ")
		b.WriteString(shellQuote(envFile))
		b.WriteString(" ]; then . ")
		b.WriteString(shellQuote(envFile))
		b.WriteString("; fi && ")
	}
	for k, v := range env {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(shellQuote(v))
		b.WriteByte(' ')
	}
}

func shellWords(words []string) []string {
	out := make([]string, 0, len(words))
	for _, w := range words {
		out = append(out, shellQuote(w))
	}
	return out
}

func ShellWords(words []string) []string {
	return shellWords(words)
}

func remoteMkdir(workdir string) string {
	return "mkdir -p " + shellQuote(workdir)
}

func remoteGitHydrate(workdir, baseRef string) string {
	if baseRef == "" {
		return "true"
	}
	refspec := "+refs/heads/" + baseRef + ":refs/remotes/origin/" + baseRef
	return "cd " + shellQuote(workdir) + " && " +
		"if git rev-parse --is-inside-work-tree >/dev/null 2>&1 && git remote get-url origin >/dev/null 2>&1; then " +
		"git fetch --quiet --unshallow origin " + shellQuote(refspec) + " || git fetch --quiet --depth=1000 origin " + shellQuote(refspec) + " || git fetch --quiet origin " + shellQuote(refspec) + " || git fetch --quiet origin " + shellQuote(baseRef) + " || true; " +
		"fi"
}

func remoteGitHydrateStatus(workdir, baseRef, expectedSHA string) string {
	if baseRef == "" || expectedSHA == "" {
		return "printf ''"
	}
	script := `cd ` + shellQuote(workdir) + ` && ` + remoteSyncMetaDirScript() + `
if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  exit 0
fi
marker="$meta_dir/git-hydrate-base"
remote_sha="$(git rev-parse --verify ` + shellQuote("refs/remotes/origin/"+baseRef+"^{commit}") + ` 2>/dev/null || git rev-parse --verify ` + shellQuote("origin/"+baseRef+"^{commit}") + ` 2>/dev/null || true)"
if [ "$remote_sha" = ` + shellQuote(expectedSHA) + ` ]; then
  if [ -f "$marker" ] && grep -qx ` + shellQuote(baseRef+" "+expectedSHA) + ` "$marker"; then
    printf 'marker base current'
    exit 0
  fi
  printf 'remote base current'
  exit 0
fi
if [ -n "$remote_sha" ] && git merge-base --is-ancestor ` + shellQuote(expectedSHA) + ` "$remote_sha" >/dev/null 2>&1; then
  printf 'remote base contains local'
fi`
	return "bash -lc " + shellQuote(script)
}

func remoteWriteGitHydrateMarker(workdir, baseRef, expectedSHA string) string {
	if baseRef == "" || expectedSHA == "" {
		return "true"
	}
	script := "cd " + shellQuote(workdir) + " && " + remoteSyncMetaDirScript() + "mkdir -p \"$meta_dir\" && printf %s " + shellQuote(baseRef+" "+expectedSHA+"\n") + " > \"$meta_dir/git-hydrate-base\""
	return "bash -lc " + shellQuote(script)
}

func remoteGitSeed(workdir, remoteURL, head string) string {
	remoteURL = normalizeGitRemoteURL(remoteURL)
	if remoteURL == "" || head == "" {
		return "true"
	}
	parent := filepath.ToSlash(filepath.Dir(workdir))
	return "if [ ! -d " + shellQuote(workdir+"/.git") + " ]; then " +
		"mkdir -p " + shellQuote(parent) + "; " +
		"tmp=$(mktemp -d " + shellQuote(parent+"/.seed.XXXXXX") + "); " +
		"if git clone --quiet --filter=blob:none --no-checkout " + shellQuote(remoteURL) + " \"$tmp\" >/dev/null 2>&1; then " +
		"if (cd \"$tmp\" && (git fetch --quiet --depth=1 origin " + shellQuote(head) + " || true) && (git checkout --quiet " + shellQuote(head) + " || git checkout --quiet FETCH_HEAD)); then " +
		"rm -rf " + shellQuote(workdir) + " && mv \"$tmp\" " + shellQuote(workdir) + "; " +
		"else rm -rf \"$tmp\"; fi; " +
		"else rm -rf \"$tmp\"; fi; " +
		"fi"
}

func normalizeGitRemoteURL(remoteURL string) string {
	if strings.HasPrefix(remoteURL, "git@github.com:") {
		return "https://github.com/" + strings.TrimSuffix(strings.TrimPrefix(remoteURL, "git@github.com:"), ".git") + ".git"
	}
	return remoteURL
}

func remoteReadSyncFingerprint(workdir string) string {
	script := "cd " + shellQuote(workdir) + " && " + remoteSyncMetaDirScript() + "cat \"$meta_dir/sync-fingerprint\" 2>/dev/null || true"
	return "bash -lc " + shellQuote(script)
}

func remoteWriteSyncFingerprint(workdir, fingerprint string) string {
	script := "cd " + shellQuote(workdir) + " && " + remoteSyncMetaDirScript() + "mkdir -p \"$meta_dir\" && printf %s " + shellQuote(fingerprint) + " > \"$meta_dir/sync-fingerprint\""
	return "bash -lc " + shellQuote(script)
}

type remoteSyncFinalizeOptions struct {
	AllowMassDeletions bool
	HydrateGit         bool
	BaseRef            string
	BaseSHA            string
	Fingerprint        string
}

func remoteWriteSyncManifestNew(workdir string) string {
	script := "cd " + shellQuote(workdir) + " && " + remoteSyncMetaDirScript() + "mkdir -p \"$meta_dir\" && cat > \"$meta_dir/sync-manifest.new\""
	return "bash -lc " + shellQuote(script)
}

func remoteWriteSyncDeletedNew(workdir string) string {
	script := "cd " + shellQuote(workdir) + " && " + remoteSyncMetaDirScript() + "mkdir -p \"$meta_dir\" && cat > \"$meta_dir/sync-deleted.new\""
	return "bash -lc " + shellQuote(script)
}

func remoteWriteSyncManifestsNew(workdir string) string {
	python := `import pathlib
import sys

manifest_len = int(sys.stdin.buffer.readline())
manifest = sys.stdin.buffer.read(manifest_len)
deleted = sys.stdin.buffer.read()
pathlib.Path(sys.argv[1]).write_bytes(manifest)
pathlib.Path(sys.argv[2]).write_bytes(deleted)
`
	script := "mkdir -p " + shellQuote(workdir) + " && cd " + shellQuote(workdir) + " && " + remoteSyncMetaDirScript() + "mkdir -p \"$meta_dir\" && python3 -c " + shellQuote(python) + " \"$meta_dir/sync-manifest.new\" \"$meta_dir/sync-deleted.new\""
	return "bash -lc " + shellQuote(script)
}

func remoteSeedSyncManifestFromGit(workdir string) string {
	script := "set -e\ncd " + shellQuote(workdir) + `
` + remoteSyncMetaDirScript() + `
old="$meta_dir/sync-manifest"
if [ ! -f "$old" ] && git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  mkdir -p "$meta_dir"
  git ls-files -z > "$old"
fi
`
	return "bash -lc " + shellQuote(script)
}

func remotePruneSyncManifest(workdir string) string {
	script := "set -e\ncd " + shellQuote(workdir) + `
` + remoteSyncMetaDirScript() + `
old="$meta_dir/sync-manifest"
new="$meta_dir/sync-manifest.new"
deleted="$meta_dir/sync-deleted.new"
delete_paths() {
  while IFS= read -r -d '' rel; do
    case "$rel" in ''|/*|../*|*/../*) continue ;; esac
    rm -f -- "$rel"
    dir=$(dirname -- "$rel")
    while [ "$dir" != . ] && [ "$dir" != / ]; do
      rmdir -- "$dir" 2>/dev/null || break
      dir=$(dirname -- "$dir")
    done
  done
}
manifest_removed_paths() {
  python3 - "$old" "$new" <<'PY'
import pathlib
import sys

def read_manifest(path):
    try:
        data = pathlib.Path(path).read_bytes()
    except FileNotFoundError:
        return []
    return [entry for entry in data.split(b"\0") if entry]

old = read_manifest(sys.argv[1])
new = set(read_manifest(sys.argv[2]))
sys.stdout.buffer.write(b"".join(entry + b"\0" for entry in old if entry not in new))
PY
}
if [ -f "$deleted" ]; then delete_paths < "$deleted"; fi
if [ -f "$old" ] && [ -f "$new" ]; then manifest_removed_paths | delete_paths; fi
`
	return "bash -lc " + shellQuote(script)
}

func remoteApplySyncManifest(workdir string) string {
	script := "set -e; cd " + shellQuote(workdir) + "; " + remoteSyncMetaDirScript() + "mkdir -p \"$meta_dir\"; new=\"$meta_dir/sync-manifest.new\"; deleted=\"$meta_dir/sync-deleted.new\"; rm -f \"$deleted\"; mv \"$new\" \"$meta_dir/sync-manifest\""
	return "bash -lc " + shellQuote(script)
}

func remoteFinalizeSync(workdir string, opts remoteSyncFinalizeOptions) string {
	allowValue := ""
	if opts.AllowMassDeletions {
		allowValue = "1"
	}
	script := `set -e
cd ` + shellQuote(workdir) + `
` + remoteSyncMetaDirScript() + `
mkdir -p "$meta_dir"
new="$meta_dir/sync-manifest.new"
deleted="$meta_dir/sync-deleted.new"
rm -f "$deleted"
mv "$new" "$meta_dir/sync-manifest"
if test -d .git && git status --short >/tmp/crabbox-git-status 2>/dev/null; then
  deletions=$(awk '/^ D|^D / { n++ } END { print n+0 }' /tmp/crabbox-git-status)
  if [ ` + shellQuote(allowValue) + ` != '1' ] && [ "$deletions" -ge 200 ]; then
    echo "remote sync sanity failed: $deletions tracked deletions" >&2
    awk '/^ D|^D / { print "  " substr($0,4) }' /tmp/crabbox-git-status | head -20 >&2
    exit 66
  fi
fi
`
	if opts.HydrateGit && opts.BaseRef != "" {
		refspec := "+refs/heads/" + opts.BaseRef + ":refs/remotes/origin/" + opts.BaseRef
		script += `if git rev-parse --is-inside-work-tree >/dev/null 2>&1 && git remote get-url origin >/dev/null 2>&1; then
  git fetch --quiet --unshallow origin ` + shellQuote(refspec) + ` || git fetch --quiet --depth=1000 origin ` + shellQuote(refspec) + ` || git fetch --quiet origin ` + shellQuote(refspec) + ` || git fetch --quiet origin ` + shellQuote(opts.BaseRef) + ` || true
fi
`
	}
	if opts.BaseRef != "" && opts.BaseSHA != "" {
		script += `printf %s ` + shellQuote(opts.BaseRef+" "+opts.BaseSHA+"\n") + ` > "$meta_dir/git-hydrate-base" || true
`
	}
	if opts.Fingerprint != "" {
		script += `printf %s ` + shellQuote(opts.Fingerprint) + ` > "$meta_dir/sync-fingerprint" || true
`
	}
	return "bash -lc " + shellQuote(script)
}

func remoteSyncMetaDirScript() string {
	return "meta_dir=$(if [ -d .git ]; then printf %s .git/crabbox; else printf %s .crabbox; fi); "
}

func remoteSyncSanity(workdir string, allowMassDeletions bool) string {
	allowValue := ""
	if allowMassDeletions {
		allowValue = "1"
	}
	return "cd " + shellQuote(workdir) + " && " +
		"if test -d .git && git status --short >/tmp/crabbox-git-status 2>/dev/null; then " +
		"deletions=$(awk '/^ D|^D / { n++ } END { print n+0 }' /tmp/crabbox-git-status); " +
		"if [ " + shellQuote(allowValue) + " != '1' ] && [ \"$deletions\" -ge 200 ]; then " +
		"echo \"remote sync sanity failed: $deletions tracked deletions\" >&2; " +
		"awk '/^ D|^D / { print \"  \" substr($0,4) }' /tmp/crabbox-git-status | head -20 >&2; " +
		"exit 66; " +
		"fi; " +
		"fi"
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if asExitError(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			return status.ExitStatus()
		}
	}
	return 1
}

func parseServerID(s string) (int64, bool) {
	id, err := strconv.ParseInt(s, 10, 64)
	return id, err == nil
}

func ParseServerID(s string) (int64, bool) {
	return parseServerID(s)
}
