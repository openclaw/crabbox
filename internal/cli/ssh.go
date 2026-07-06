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
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
)

type SSHTarget struct {
	User                   string
	Host                   string
	Key                    string
	CertificateFile        string
	KnownHostsFile         string
	Port                   string
	FallbackPorts          []string
	TargetOS               string
	WindowsMode            string
	ReadyCheck             string
	AuthSecret             bool
	NoControlMaster        bool
	DisableHostKeyChecking bool
	NetworkKind            NetworkMode
	SSHConfigProxy         bool
	ProxyCommand           string
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
				return exit(5, "timed out waiting for SSH on %s during %s ports=%s; %s", target.Host, phase, lastPorts, sshWaitNextAction(phase))
			}
			return exit(5, "timed out waiting for SSH on %s during %s; %s", target.Host, phase, sshWaitNextAction(phase))
		}
		if target.SSHConfigProxy {
			if runSSHQuietWithOptionsResolvePort(ctx, target, sshReadyCommand(*target), "5", "1") == nil {
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
				probe.FallbackPorts = []string{}
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

func sshWaitNextAction(phase string) string {
	switch phase {
	case "before sync":
		return "next_action=retry with --full-resync, then use a fresh lease if SSH still fails"
	case "before command":
		return "next_action=retry the command, then stop or replace the lease if SSH still fails"
	default:
		return "next_action=check lease status, then stop or replace the lease if SSH stays unreachable"
	}
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
		return runSSHQuietWithOptionsResolvePort(ctx, target, sshReadyCommand(*target), "2", "1") == nil
	}
	for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
		probe := *target
		probe.Port = port
		probe.FallbackPorts = []string{}
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
		return runSSHQuietWithOptionsResolvePort(ctx, target, sshTransportProbeCommand(*target), "2", "1") == nil
	}
	for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
		probe := *target
		probe.Port = port
		probe.FallbackPorts = []string{}
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

func RunSSHQuiet(ctx context.Context, target SSHTarget, remote string) error {
	return runSSHQuiet(ctx, target, remote)
}

// RunSSHOutput runs remote on target and returns its trimmed stdout. It is the
// exported entry point for provider backends (e.g. the Phala TDX attestation
// fetch) that need to capture a remote command's stdout rather than merely
// asserting it succeeded.
func RunSSHOutput(ctx context.Context, target SSHTarget, remote string) (string, error) {
	return runSSHOutput(ctx, target, remote)
}

func runSSHQuietWithOptions(ctx context.Context, target SSHTarget, remote, connectTimeout, connectionAttempts string) error {
	return runSSHQuietWithOptionsResolvePort(ctx, &target, remote, connectTimeout, connectionAttempts)
}

func runSSHQuietWithOptionsResolvePort(ctx context.Context, target *SSHTarget, remote, connectTimeout, connectionAttempts string) error {
	remote = wrapRemoteForTarget(*target, remote)
	var lastErr error
	for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
		probe := *target
		probe.Port = port
		probe.FallbackPorts = []string{}
		cmd := exec.CommandContext(ctx, "ssh", sshArgsNoInputWithOptions(probe, remote, connectTimeout, connectionAttempts)...)
		err := runSSHCommand(cmd, io.Discard, io.Discard)
		if err == nil {
			target.Port = probe.Port
			return nil
		}
		lastErr = err
		if !shouldRetrySSHPort(err) {
			return err
		}
	}
	return lastErr
}

func runSSHQuietWithRemoteWaitTimeout(ctx context.Context, target SSHTarget, remote string, waitTimeout time.Duration, connectTimeout, connectionAttempts string) error {
	remote = wrapRemoteForTargetWithWaitTimeout(target, remote, waitTimeout)
	var lastErr error
	for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
		probe := target
		probe.Port = port
		probe.FallbackPorts = []string{}
		cmd := exec.CommandContext(ctx, "ssh", sshArgsNoInputWithOptions(probe, remote, connectTimeout, connectionAttempts)...)
		err := runSSHCommand(cmd, io.Discard, io.Discard)
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
		cmd := exec.CommandContext(ctx, "ssh", sshArgsNoInput(probe, remote)...)
		var out bytes.Buffer
		err := runSSHCommand(cmd, &out, io.Discard)
		if err == nil {
			return strings.TrimSpace(out.String()), nil
		}
		lastOut = out.Bytes()
		lastErr = err
		if !shouldRetrySSHPort(err) {
			return "", err
		}
	}
	return strings.TrimSpace(string(lastOut)), lastErr
}

func runSSHOutputWithRemoteWaitTimeout(ctx context.Context, target SSHTarget, remote string, waitTimeout time.Duration, connectTimeout, connectionAttempts string) (string, error) {
	remote = wrapRemoteForTargetWithWaitTimeout(target, remote, waitTimeout)
	var lastOut []byte
	var lastErr error
	for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
		probe := target
		probe.Port = port
		probe.FallbackPorts = []string{}
		cmd := exec.CommandContext(ctx, "ssh", sshArgsNoInputWithOptions(probe, remote, connectTimeout, connectionAttempts)...)
		var out bytes.Buffer
		err := runSSHCommand(cmd, &out, io.Discard)
		if err == nil {
			return strings.TrimSpace(out.String()), nil
		}
		lastOut = out.Bytes()
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
		// Crabbox's SSH helpers intentionally execute commands assembled by
		// typed remote-command builders. Callers must shell-quote user data
		// before it reaches this boundary; see remoteCommand/shellQuote tests.
		cmd := exec.CommandContext(ctx, "ssh", sshArgsNoInput(probe, remote)...)
		var out synchronizedBuffer
		err := runSSHCommand(cmd, &out, &out)
		if err == nil {
			return strings.TrimSpace(out.String()), nil
		}
		lastOut = out.Bytes()
		lastErr = err
		if !shouldRetrySSHPort(err) {
			return strings.TrimSpace(out.String()), err
		}
	}
	return strings.TrimSpace(string(lastOut)), lastErr
}

func runWSL2ControlScriptCombinedOutput(ctx context.Context, target SSHTarget, remote string, waitTimeout time.Duration, connectTimeout, connectionAttempts string) (string, error) {
	command := wsl2StdinScriptCommandWithWaitTimeout(waitTimeout)
	var lastOut []byte
	var lastErr error
	for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
		probe := target
		probe.Port = port
		probe.FallbackPorts = []string{}
		cmd := exec.CommandContext(ctx, "ssh", sshArgsWithOptions(probe, command, connectTimeout, connectionAttempts)...)
		cmd.Stdin = strings.NewReader(remote)
		var out synchronizedBuffer
		err := runSSHCommand(cmd, &out, &out)
		if err == nil {
			return strings.TrimSpace(out.String()), nil
		}
		lastOut = out.Bytes()
		lastErr = err
		if !shouldRetrySSHPort(err) {
			return strings.TrimSpace(string(lastOut)), err
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
		err := runSSHCommand(cmd, stdout, stderr)
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

func runSSHInputStream(ctx context.Context, target SSHTarget, remote string, input io.ReadSeeker, stdout, stderr io.Writer) error {
	remote = wrapRemoteForTarget(target, remote)
	if input == nil {
		input = strings.NewReader("")
	}
	var lastErr error
	for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
		if _, err := input.Seek(0, io.SeekStart); err != nil {
			return err
		}
		probe := target
		probe.Port = port
		cmd := exec.CommandContext(ctx, "ssh", sshArgs(probe, remote)...)
		cmd.Stdin = input
		err := runSSHCommand(cmd, stdout, stderr)
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
		cmd := exec.CommandContext(ctx, "ssh", sshArgsNoInput(probe, remote)...)
		err := runSSHCommand(cmd, stdout, stderr)
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

func runSSHCommand(cmd *exec.Cmd, stdout, stderr io.Writer) error {
	return runCommandWithPlatformStreams(cmd, stdout, stderr)
}

type synchronizedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *synchronizedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(data)
}

func (b *synchronizedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func isSSHCommandExitError(err error) bool {
	var exitErr *exec.ExitError
	return asExitError(err, &exitErr)
}

func sshArgs(target SSHTarget, remote string) []string {
	return sshArgsWithOptions(target, remote, "10", "3")
}

func sshArgsNoInput(target SSHTarget, remote string) []string {
	return sshArgsNoInputWithOptions(target, remote, "10", "3")
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

func sshArgsNoInputWithOptions(target SSHTarget, remote, connectTimeout, connectionAttempts string) []string {
	return append(sshBaseArgsWithOptions(target, connectTimeout, connectionAttempts),
		"-n",
		target.User+"@"+target.Host,
		remote,
	)
}

func sshBaseArgs(target SSHTarget) []string {
	return sshBaseArgsWithOptions(target, "10", "3")
}

func sshForwardingDenyArgs() []string {
	return []string{
		"-o", "ForwardAgent=no",
		"-o", "ForwardX11=no",
		"-o", "ForwardX11Trusted=no",
	}
}

func sshBaseArgsWithOptions(target SSHTarget, connectTimeout, connectionAttempts string) []string {
	args := append(sshForwardingDenyArgs(),
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout="+connectTimeout,
		"-o", "ConnectionAttempts="+connectionAttempts,
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=2",
		"-p", target.Port,
	)
	if target.DisableHostKeyChecking {
		args = append(args,
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			"-o", "LogLevel=ERROR",
		)
	} else {
		args = append(args,
			"-o", "StrictHostKeyChecking=accept-new",
			"-o", "UserKnownHostsFile="+sshConfigFileValue(knownHostsFile(target)),
		)
	}
	if target.AuthSecret || target.NoControlMaster {
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
	if target.CertificateFile != "" {
		args = append(args, "-o", "CertificateFile="+target.CertificateFile)
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
	if target.KnownHostsFile != "" {
		return target.KnownHostsFile
	}
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
	scope := strings.Join([]string{
		target.User,
		target.Key,
		target.CertificateFile,
		target.KnownHostsFile,
		target.ProxyCommand,
	}, "\x00")
	sum := sha1.Sum([]byte(scope))
	return filepath.Join("/tmp", "crabbox-ssh-"+hex.EncodeToString(sum[:4])+"-%C")
}

type rsyncOptions struct {
	Debug             bool
	Delete            bool
	Checksum          bool
	FullResync        bool
	UseFilesFrom      bool
	FilesFrom         []byte
	NoTimes           bool
	Timeout           time.Duration
	HeartbeatInterval time.Duration
}

func normalizeRsyncOptions(opts rsyncOptions) rsyncOptions {
	if opts.NoTimes {
		opts.Checksum = true
	}
	return opts
}

func rsync(ctx context.Context, target SSHTarget, src, dst string, excludes []string, stdout, stderr io.Writer, opts rsyncOptions) error {
	opts = normalizeRsyncOptions(opts)
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}
	args := []string{
		"-az",
		"-e", strings.Join(shellWords(append([]string{"ssh"}, sshBaseArgs(target)...)), " "),
	}
	if opts.NoTimes {
		args = append(args, "--no-times", "--omit-dir-times")
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
		return exit(6, "rsync timed out after %s; next_action=retry with --full-resync, then use a fresh lease if sync still stalls", opts.Timeout)
	}
	if opts.Debug {
		fmt.Fprintf(stderr, "rsync elapsed=%s checksum=%t delete=%t\n", time.Since(start).Round(time.Millisecond), opts.Checksum, opts.Delete)
	}
	return err
}

func wrapRemoteForTarget(target SSHTarget, remote string) string {
	return wrapRemoteForTargetWithWaitTimeout(target, remote, 0)
}

func wrapRemoteForTargetWithWaitTimeout(target SSHTarget, remote string, waitTimeout time.Duration) string {
	if isWindowsNativeTarget(target) {
		if strings.HasPrefix(remote, "powershell.exe ") || strings.HasPrefix(remote, "powershell ") {
			return remote
		}
		return powershellCommand(remote)
	}
	if isWindowsWSL2Target(target) {
		return wsl2CommandWithWaitTimeout(remote, waitTimeout)
	}
	return remote
}

func wsl2CommandWithWaitTimeout(remote string, waitTimeout time.Duration) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(remote))
	wait := `$process.WaitForExit()
  $code = $process.ExitCode`
	invoke := `& wsl.exe --exec bash $wslPath
  $code = $LASTEXITCODE`
	if waitTimeout > 0 {
		waitMS := int(waitTimeout / time.Millisecond)
		wait = fmt.Sprintf(`if (-not $process.WaitForExit(%d)) {
    try {
      $process.Kill($true)
    } catch {
      $process.Kill()
    }
    $process.WaitForExit(5000) | Out-Null
    throw "WSL2 command timed out after %s"
  }
  $code = $process.ExitCode`, waitMS, waitTimeout.Round(time.Second))
		invoke = `$psi = [System.Diagnostics.ProcessStartInfo]::new("wsl.exe")
  $psi.UseShellExecute = $false
  $psi.Arguments = "--exec bash " + $wslPath
  $process = [System.Diagnostics.Process]::Start($psi)
  ` + wait
	}
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
  ` + invoke + `
} finally {
  Remove-Item -LiteralPath $path -Force -ErrorAction SilentlyContinue
}
exit $code`)
}

func wsl2StdinScriptCommandWithWaitTimeout(waitTimeout time.Duration) string {
	wait := `$process.WaitForExit()
  $code = $process.ExitCode`
	if waitTimeout > 0 {
		waitMS := int(waitTimeout / time.Millisecond)
		wait = fmt.Sprintf(`if (-not $process.WaitForExit(%d)) {
    try {
      $process.Kill($true)
    } catch {
      $process.Kill()
    }
    $process.WaitForExit(5000) | Out-Null
    throw "WSL2 command timed out after %s"
  }
  $code = $process.ExitCode`, waitMS, waitTimeout.Round(time.Second))
	}
	return powershellCommand(`$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"
$dir = "C:\ProgramData\crabbox\commands"
New-Item -ItemType Directory -Force -Path $dir | Out-Null
$name = "cmd-" + [Guid]::NewGuid().ToString("N") + ".sh"
$path = Join-Path $dir $name
try {
  $script = [System.IO.File]::Open($path, [System.IO.FileMode]::Create, [System.IO.FileAccess]::Write, [System.IO.FileShare]::None)
  try {
    [Console]::OpenStandardInput().CopyTo($script)
  } finally {
    $script.Close()
  }
  $wslPath = "/mnt/c/ProgramData/crabbox/commands/" + $name
  $psi = [System.Diagnostics.ProcessStartInfo]::new("wsl.exe")
  $psi.UseShellExecute = $false
  $psi.Arguments = "--exec bash " + $wslPath
  $process = [System.Diagnostics.Process]::Start($psi)
  ` + wait + `
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
				elapsed := time.Since(start).Round(time.Second)
				if elapsed >= 4*time.Minute {
					fmt.Fprintf(stderr, "still syncing after %s... watchdog=sync-quiet next_action=wait, or cancel and retry with --full-resync/fresh lease if no progress\n", elapsed)
				} else {
					fmt.Fprintf(stderr, "still syncing after %s...\n", elapsed)
				}
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
	wslExe, err := exec.LookPath("wsl.exe")
	if err != nil {
		wslExe, err = exec.LookPath("wsl")
	}
	// Fall back to native rsync when WSL is absent or lacks native tools.
	if err != nil || !windowsWSLHasNativeRsyncSSH(ctx, wslExe) {
		return windowsNativeRsyncCommand(ctx, args)
	}
	mountRoot := windowsWSLMountRoot(ctx, wslExe)

	// Prepare WSL key: copy with correct permissions.
	wslKey := ""
	knownHostsPath := ""
	wslKnownHosts := ""
	if target.Key != "" {
		wslKey = "/tmp/crabbox-wsl-" + filepath.Base(filepath.Dir(target.Key))
		knownHostsPath = knownHostsFile(target)
		if keyData, err := os.ReadFile(windowsHostPath(target.Key)); err == nil {
			cpCmd := exec.CommandContext(ctx, wslExe, "sh", "-c",
				fmt.Sprintf("umask 077; cat > %s && chmod 600 %s",
					shellQuote(wslKey),
					shellQuote(wslKey)))
			cpCmd.Stdin = bytes.NewReader(keyData)
			if err := cpCmd.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "warning: copy SSH key into WSL for rsync failed: %v\n", err)
				return windowsNativeRsyncCommand(ctx, args)
			}
		} else {
			fmt.Fprintf(os.Stderr, "warning: read SSH key for WSL rsync failed: %s: %v\n", target.Key, err)
			return windowsNativeRsyncCommand(ctx, args)
		}
		if knownHostsData, err := os.ReadFile(windowsHostPath(knownHostsPath)); err == nil {
			wslKnownHosts = wslKey + "-known_hosts"
			cpKnownHostsCmd := exec.CommandContext(ctx, wslExe, "sh", "-c",
				fmt.Sprintf("cat > %s && chmod 600 %s",
					shellQuote(wslKnownHosts),
					shellQuote(wslKnownHosts)))
			cpKnownHostsCmd.Stdin = bytes.NewReader(knownHostsData)
			if err := cpKnownHostsCmd.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "warning: copy known_hosts into WSL for rsync failed: %v\n", err)
				wslKnownHosts = ""
			}
		}
	}

	// Convert all args: replace Windows paths inside strings (including
	// the -e "ssh ..." arg which embeds key and known_hosts paths).
	wslArgs := make([]string, len(args))
	for i, arg := range args {
		converted := windowsToWSLPathWithRoot(arg, mountRoot)
		// Replace key path with WSL temp copy inside -e string
		if wslKey != "" && target.Key != "" {
			keyWSL := windowsToWSLMountPathWithRoot(target.Key, mountRoot)
			converted = strings.ReplaceAll(converted, keyWSL, wslKey)
		}
		// Replace known_hosts path
		if knownHostsPath != "" && wslKnownHosts != "" {
			khWSL := windowsToWSLMountPathWithRoot(knownHostsPath, mountRoot)
			converted = strings.ReplaceAll(converted, khWSL, wslKnownHosts)
		}
		wslArgs[i] = converted
	}
	return exec.CommandContext(ctx, wslExe, append([]string{"rsync"}, wslArgs...)...)
}

func windowsNativeRsyncCommand(ctx context.Context, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "rsync", args...)
	cmd.Env = append(os.Environ(), "MSYS2_ARG_CONV_EXCL=*", "MSYS_NO_PATHCONV=1", "CYGWIN=nodosfilewarning")
	return cmd
}

func windowsHostPath(path string) string {
	path = strings.ReplaceAll(path, `\`, "/")
	if len(path) >= 3 && path[0] == '/' && path[2] == '/' &&
		(path[1] >= 'a' && path[1] <= 'z' || path[1] >= 'A' && path[1] <= 'Z') {
		return string(path[1]) + ":" + path[2:]
	}
	return path
}

func windowsWSLHasNativeRsyncSSH(ctx context.Context, wslExe string) bool {
	if strings.TrimSpace(wslExe) == "" {
		return false
	}
	out, err := exec.CommandContext(ctx, wslExe, "sh", "-c", "rsync_path=$(command -v rsync) || exit 1; ssh_path=$(command -v ssh) || exit 1; printf '%s\\n%s\\n' \"$rsync_path\" \"$ssh_path\"").Output()
	return err == nil && windowsWSLNativeToolPaths(string(out))
}

func windowsWSLNativeToolPaths(output string) bool {
	paths := 0
	for _, line := range strings.Split(output, "\n") {
		path := strings.TrimSpace(line)
		if path == "" {
			continue
		}
		paths++
		if !windowsWSLNativeToolPath(path) {
			return false
		}
	}
	return paths >= 2
}

func windowsWSLNativeToolPath(path string) bool {
	path = strings.ToLower(strings.TrimSpace(path))
	if path == "" {
		return false
	}
	if strings.HasSuffix(path, ".exe") {
		return false
	}
	if windowsWSLMountedDrivePath(path) {
		return false
	}
	return true
}

func windowsWSLMountedDrivePath(path string) bool {
	rest, ok := strings.CutPrefix(path, "/mnt/")
	if !ok {
		return false
	}
	rest = strings.TrimPrefix(rest, "host/")
	return len(rest) >= 1 && rest[0] >= 'a' && rest[0] <= 'z' && (len(rest) == 1 || rest[1] == '/')
}

func windowsToWSLMountPathWithRoot(path, mountRoot string) string {
	path = strings.ReplaceAll(path, `\`, "/")
	if len(path) >= 2 && path[1] == ':' {
		drive := strings.ToLower(string(path[0]))
		return strings.TrimRight(mountRoot, "/") + "/" + drive + path[2:]
	}
	if len(path) >= 3 && path[0] == '/' && path[2] == '/' && path[1] >= 'a' && path[1] <= 'z' {
		return strings.TrimRight(mountRoot, "/") + path
	}
	return path
}

// windowsToWSLPath converts Windows paths found anywhere in a string to
// WSL mount paths. Handles both C:\... and /c/... formats embedded in
// larger strings (like the -e "ssh -i C:\path\key ..." argument).
func windowsToWSLPath(s string) string {
	return windowsToWSLPathWithRoot(s, "/mnt")
}

func windowsToWSLPathWithRoot(s, mountRoot string) string {
	mountRoot = strings.TrimRight(mountRoot, "/")
	s = strings.ReplaceAll(s, `\`, "/")
	// Replace drive-letter paths: C:/... -> /mnt/c/... or /mnt/host/c/...
	for i := 0; i < len(s)-2; i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z') && s[i+1] == ':' && s[i+2] == '/' {
			// Only replace if at start of string or preceded by a non-path char
			if i == 0 || s[i-1] == ' ' || s[i-1] == '\'' || s[i-1] == '"' || s[i-1] == '=' {
				drive := strings.ToLower(string(c))
				replacement := mountRoot + "/" + drive + s[i+2:]
				s = s[:i] + replacement
				i += len(mountRoot) + 2 // skip past /mnt[/host]/X
			}
		}
	}
	// Also handle /c/... -> /mnt/c/... or /mnt/host/c/... (from rsyncLocalPath conversion)
	for i := 0; i < len(s)-2; i++ {
		if s[i] == '/' && s[i+1] >= 'a' && s[i+1] <= 'z' && s[i+2] == '/' {
			if i == 0 || s[i-1] == ' ' || s[i-1] == '\'' || s[i-1] == '"' || s[i-1] == '=' {
				// Avoid converting remote paths like crabbox@host:/work
				if i == 0 || s[i-1] != ':' {
					s = s[:i] + mountRoot + s[i:]
					i += len(mountRoot)
				}
			}
		}
	}
	return s
}

func windowsWSLMountRoot(ctx context.Context, wslExe string) string {
	if runtime.GOOS != "windows" {
		return "/mnt"
	}
	out, err := exec.CommandContext(ctx, wslExe, "sh", "-c", "if [ -d /mnt/host/c ]; then printf /mnt/host; else printf /mnt; fi").Output()
	if err == nil {
		if value := strings.TrimSpace(string(out)); value != "" {
			return value
		}
	}
	return "/mnt"
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
	return remoteCommandWithEnvFiles(workdir, env, singleEnvFile(envFile), command)
}

func remoteCommandWithEnvFiles(workdir string, env map[string]string, envFiles []string, command []string) string {
	var b strings.Builder
	writeRemoteCommandPrefix(&b, workdir, env, envFiles)
	b.WriteString("bash -lc ")
	b.WriteString(shellQuote(remoteBashLoginScript(workdir, `exec "$@"`)))
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
	return remoteShellCommandWithEnvFiles(workdir, env, singleEnvFile(envFile), script)
}

func remoteShellCommandWithEnvFiles(workdir string, env map[string]string, envFiles []string, script string) string {
	var b strings.Builder
	writeRemoteCommandPrefix(&b, workdir, env, envFiles)
	b.WriteString("bash -lc ")
	b.WriteString(shellQuote(remoteBashLoginScript(workdir, script)))
	return b.String()
}

func remoteBashLoginScript(workdir, script string) string {
	// Some sandbox images run bash startup files that cd back to $HOME for
	// login shells. Keep the outer cd for env-file loading, then restore cwd
	// inside bash -lc before the user command runs.
	return "cd " + shellQuote(workdir) + " && " + script
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

func writeRemoteCommandPrefix(b *strings.Builder, workdir string, env map[string]string, envFiles []string) {
	b.WriteString("cd ")
	b.WriteString(shellQuote(workdir))
	b.WriteString(" && ")
	for _, envFile := range envFiles {
		envFile = strings.TrimSpace(envFile)
		if envFile == "" {
			continue
		}
		b.WriteString("if [ -f ")
		b.WriteString(shellQuote(envFile))
		b.WriteString(" ]; then . ")
		b.WriteString(shellQuote(envFile))
		b.WriteString("; fi && ")
	}
	for k, v := range env {
		if !validEnvName(k) {
			continue
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(shellQuote(v))
		b.WriteByte(' ')
	}
}

func singleEnvFile(envFile string) []string {
	if strings.TrimSpace(envFile) == "" {
		return nil
	}
	return []string{envFile}
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

func remoteResetWorkdir(workdir string) string {
	parent := filepath.ToSlash(filepath.Dir(workdir))
	script := "set -eu\nmkdir -p " + shellQuote(parent) + "\nrm -rf -- " + shellQuote(workdir) + "\nmkdir -p " + shellQuote(workdir)
	return "bash -lc " + shellQuote(script)
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
	if gitRemoteURLHasCredentials(remoteURL) {
		return ""
	}
	if strings.HasPrefix(remoteURL, "git@github.com:") {
		return "https://github.com/" + strings.TrimSuffix(strings.TrimPrefix(remoteURL, "git@github.com:"), ".git") + ".git"
	}
	return remoteURL
}

func remoteReadSyncFingerprint(workdir string) string {
	script := "cd " + shellQuote(workdir) + " && " + remoteSyncMetaDirScript() + "cat \"$meta_dir/sync-fingerprint\" 2>/dev/null || true"
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

func remoteSyncInterpreterCommand(python, perl, args string) string {
	if args != "" && !strings.HasPrefix(args, " ") {
		args = " " + args
	}
	return "if command -v python3 >/dev/null 2>&1; then python3 -c " + shellQuote(python) + args +
		"; elif command -v python >/dev/null 2>&1; then python -c " + shellQuote(python) + args +
		"; elif command -v perl >/dev/null 2>&1; then perl -e " + shellQuote(perl) + args +
		"; else echo " + shellQuote("missing required sync interpreter: need python3, python, or perl") + " >&2; exit 127; fi"
}

func remoteWriteSyncManifestsNew(workdir string) string {
	script := "set -e\nmkdir -p " + shellQuote(workdir) + "\ncd " + shellQuote(workdir) + "\n" + remoteSyncMetaDirScript() + `mkdir -p "$meta_dir"
IFS= read -r manifest_len
case "$manifest_len" in
  ''|*[!0-9]*) echo "invalid sync manifest length" >&2; exit 1 ;;
esac
dd bs=1 count="$manifest_len" of="$meta_dir/sync-manifest.new" status=none
cat > "$meta_dir/sync-deleted.new"
`
	return "bash -lc " + shellQuote(script)
}

func syncManifestInputForTarget(target SSHTarget, manifestData, deletedData []byte) string {
	if isWindowsWSL2Target(target) {
		manifestEncoded := base64.StdEncoding.EncodeToString(manifestData)
		deletedEncoded := base64.StdEncoding.EncodeToString(deletedData)
		return fmt.Sprintf("%d\n%d\n", len(manifestEncoded), len(deletedEncoded)) + manifestEncoded + deletedEncoded
	}
	return fmt.Sprintf("%d\n", len(manifestData)) + string(manifestData) + string(deletedData)
}

func remoteWriteSyncManifestsNewForTarget(target SSHTarget, workdir string) string {
	if isWindowsWSL2Target(target) {
		return remoteWriteSyncManifestsNewPython(workdir)
	}
	return remoteWriteSyncManifestsNew(workdir)
}

func remoteWriteSyncManifestsNewPython(workdir string) string {
	python := `import base64
import sys

def read_len(name):
    line = sys.stdin.buffer.readline()
    try:
        return int(line.strip())
    except ValueError:
        raise SystemExit(f"invalid {name} length")

manifest_len = read_len("sync manifest")
deleted_len = read_len("sync deleted")
manifest_encoded = sys.stdin.buffer.read(manifest_len)
if len(manifest_encoded) != manifest_len:
    raise SystemExit(f"short sync manifest: got {len(manifest_encoded)} want {manifest_len}")
deleted_encoded = sys.stdin.buffer.read(deleted_len)
if len(deleted_encoded) != deleted_len:
    raise SystemExit(f"short sync deleted manifest: got {len(deleted_encoded)} want {deleted_len}")
manifest = base64.b64decode(manifest_encoded)
deleted = base64.b64decode(deleted_encoded)

with open(sys.argv[1], "wb") as handle:
    handle.write(manifest)
with open(sys.argv[2], "wb") as handle:
    handle.write(deleted)
`
	script := "set -e\nmkdir -p " + shellQuote(workdir) + "\ncd " + shellQuote(workdir) + "\n" + remoteSyncMetaDirScript() + "mkdir -p \"$meta_dir\"\n" +
		"python3 -c " + shellQuote(python) + " \"$meta_dir/sync-manifest.new\" \"$meta_dir/sync-deleted.new\"\n"
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
	python := `import sys

def read_manifest(path):
    with open(path, "rb") as handle:
        data = handle.read()
    return [entry for entry in data.split(b"\0") if entry]

old = read_manifest(sys.argv[1])
new = set(read_manifest(sys.argv[2]))
writer = getattr(sys.stdout, "buffer", sys.stdout)
writer.write(b"".join(entry + b"\0" for entry in old if entry not in new))
`
	perl := `use strict;
use warnings;

sub read_manifest {
    my ($path) = @_;
    open my $fh, "<", $path or die "open $path: $!\n";
    binmode $fh;
    local $/;
    my $data = <$fh>;
    $data = "" unless defined $data;
    close $fh or die "close $path: $!\n";
    return grep { length $_ } split /\0/, $data, -1;
}

my @old = read_manifest($ARGV[0]);
my %new = map { $_ => 1 } read_manifest($ARGV[1]);
binmode STDOUT;
print STDOUT map { $_ . "\0" } grep { !$new{$_} } @old;
`
	script := "set -e -o pipefail\ncd " + shellQuote(workdir) + `
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
  ` + remoteSyncInterpreterCommand(python, perl, "\"$old\" \"$new\"") + `
}
if [ -f "$deleted" ]; then delete_paths < "$deleted"; fi
if [ -f "$old" ] && [ -f "$new" ]; then manifest_removed_paths | delete_paths; fi
`
	return "bash -lc " + shellQuote(script)
}

func remotePruneSyncManifestForTarget(target SSHTarget, workdir string) string {
	if isWindowsWSL2Target(target) {
		return remotePruneSyncManifestCoreutils(workdir)
	}
	return remotePruneSyncManifest(workdir)
}

func remotePruneSyncManifestCoreutils(workdir string) string {
	script := "set -e -o pipefail\ncd " + shellQuote(workdir) + `
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
if [ -f "$deleted" ]; then delete_paths < "$deleted"; fi
if [ -f "$old" ] && [ -f "$new" ]; then
  old_sorted="$meta_dir/sync-manifest.old.sorted"
  new_sorted="$meta_dir/sync-manifest.new.sorted"
  LC_ALL=C sort -z "$old" > "$old_sorted"
  LC_ALL=C sort -z "$new" > "$new_sorted"
  comm -z -23 "$old_sorted" "$new_sorted" | delete_paths
  rm -f "$old_sorted" "$new_sorted"
fi
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
