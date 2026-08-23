package cli

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"

	xssh "golang.org/x/crypto/ssh"
)

type SSHTarget struct {
	User                   string
	Host                   string
	SSHHostKey             string
	Key                    string
	CertificateFile        string
	KnownHostsFile         string
	HostKeyAlias           string
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
	ChildEnvDenylist       []string
	ChildEnv               map[string]string
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
		User:             user,
		Host:             host,
		Key:              cfg.SSHKey,
		Port:             port,
		FallbackPorts:    cfg.SSHFallbackPorts,
		TargetOS:         cfg.TargetOS,
		WindowsMode:      cfg.WindowsMode,
		ChildEnvDenylist: externalDesktopChildEnvDenylist(cfg, cfg.TargetOS),
	}
}

// PreserveExternalDesktopChildEnvironmentBoundary records a valid, trusted or
// programmatic desktop password environment name before an override replaces
// it. Repository-selected and reserved names cannot influence a later child's
// environment. The retained names are intentionally monotonic for the lifetime
// of this Config value: an old operator secret must not become visible merely
// because a routed lease or explicit override selects a new name.
func PreserveExternalDesktopChildEnvironmentBoundary(cfg *Config) {
	if cfg == nil {
		return
	}
	name := strings.TrimSpace(cfg.External.Connection.Desktop.PasswordEnv)
	if name == "" || cfg.credentialProvenance.externalDesktopEnv == credentialSourceRepository {
		return
	}
	if ValidateExternalDesktopPasswordEnvironmentName(name) != nil {
		return
	}
	cfg.externalDesktopEnvDenylist = appendUniqueStrings(
		cfg.externalDesktopEnvDenylist,
		name,
	)
}

// ExternalDesktopChildEnvironmentDenylist returns retained historical names
// plus this External config's current effective name. The returned slice is a
// copy and is safe for callers to extend without mutating Config.
func ExternalDesktopChildEnvironmentDenylist(cfg Config) []string {
	return appendUniqueStrings(
		cfg.externalDesktopEnvDenylist,
		cfg.External.Connection.Desktop.PasswordEnv,
	)
}

func externalDesktopChildEnvDenylist(cfg Config, _ string) []string {
	// Standalone helpers may run before provider validation. Reuse the same
	// capture policy so repository-selected or reserved current names cannot
	// suppress unrelated child credentials such as GH_TOKEN or PATH. Trusted
	// External secret names remain denied even if repository config switches
	// the active provider.
	safe := cfg
	PreserveExternalDesktopChildEnvironmentBoundary(&safe)
	return appendUniqueStrings(nil, safe.externalDesktopEnvDenylist...)
}

// ApplyTargetChildEnvironmentBoundary prevents operator-owned desktop secrets
// from reaching target-facing local child processes such as ssh, ProxyCommand,
// rsync, scp, and viewer launchers.
func ApplyTargetChildEnvironmentBoundary(cfg Config, target *SSHTarget) {
	if target != nil {
		target.ChildEnvDenylist = appendUniqueStrings(
			target.ChildEnvDenylist,
			externalDesktopChildEnvDenylist(cfg, target.TargetOS)...,
		)
	}
}

func childEnvironmentWithout(env []string, denied ...string) []string {
	if len(denied) == 0 {
		return nil
	}
	blocked := make(map[string]struct{}, len(denied))
	for _, name := range denied {
		if name = strings.TrimSpace(name); name != "" {
			blocked[strings.ToUpper(name)] = struct{}{}
		}
	}
	result := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if _, remove := blocked[strings.ToUpper(name)]; !remove {
			result = append(result, entry)
		}
	}
	return result
}

func systemInspectionEnvironment() []string {
	return []string{"LC_ALL=C"}
}

func systemInspectionCommand(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.Env = systemInspectionEnvironment()
	return cmd
}

func applyTargetChildEnvironment(cmd *exec.Cmd, target SSHTarget) {
	if cmd == nil || (len(target.ChildEnvDenylist) == 0 && len(target.ChildEnv) == 0) {
		return
	}
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	overrideNames := make([]string, 0, len(target.ChildEnv))
	for name := range target.ChildEnv {
		if validEnvName(name) {
			overrideNames = append(overrideNames, name)
		}
	}
	sort.Strings(overrideNames)
	blocked := append(append([]string(nil), target.ChildEnvDenylist...), overrideNames...)
	env = childEnvironmentWithout(env, blocked...)
	for _, name := range overrideNames {
		env = append(env, name+"="+target.ChildEnv[name])
	}
	cmd.Env = env
}

func directSSHExecutable() string {
	return directSSHExecutableForGOOS(runtime.GOOS, os.Getenv, os.Stat)
}

func directSSHExecutableForGOOS(goos string, getenv func(string) string, stat func(string) (os.FileInfo, error)) string {
	if goos != "windows" {
		return "ssh"
	}
	windowsDir := strings.TrimSpace(getenv("WINDIR"))
	if windowsDir == "" {
		return "ssh"
	}
	candidate := filepath.Join(windowsDir, "System32", "OpenSSH", "ssh.exe")
	if info, err := stat(candidate); err == nil && info.Mode().IsRegular() {
		return candidate
	}
	return "ssh"
}

const sshCommandWaitDelay = 5 * time.Second

func sshCommandContext(ctx context.Context, target SSHTarget, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, directSSHExecutable(), args...)
	// A cancelled multiplexed SSH session can leave its ControlPersist master
	// holding inherited pipes after the session process exits. Bound Go's pipe
	// drain so cancellation cannot strand the caller in Cmd.Wait.
	cmd.WaitDelay = sshCommandWaitDelay
	applyTargetChildEnvironment(cmd, target)
	return cmd
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

type sshReadinessProfile struct {
	connectTimeout     string
	connectionAttempts string
}

func sshReadinessProfileForTarget(target SSHTarget) sshReadinessProfile {
	if target.TargetOS == targetWindows {
		return sshReadinessProfile{
			connectTimeout:     "10",
			connectionAttempts: "3",
		}
	}
	return sshReadinessProfile{
		connectTimeout:     "5",
		connectionAttempts: "1",
	}
}

func waitForSSHReady(ctx context.Context, target *SSHTarget, stderr io.Writer, phase string, timeout time.Duration) error {
	start := time.Now()
	deadline := time.Now().Add(timeout)
	profile := sshReadinessProfileForTarget(*target)
	probeCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	lastPorts := ""
	for {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if time.Until(deadline) <= 0 {
			if lastPorts != "" {
				return exit(5, "timed out waiting for SSH on %s during %s ports=%s; %s", target.Host, phase, lastPorts, sshWaitNextAction(phase))
			}
			return exit(5, "timed out waiting for SSH on %s during %s; %s", target.Host, phase, sshWaitNextAction(phase))
		}
		if target.SSHConfigProxy {
			if runSSHQuietWithOptionsResolvePort(probeCtx, target, sshReadyCommand(*target), profile.connectTimeout, profile.connectionAttempts) == nil {
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
				if runSSHQuietWithOptions(probeCtx, probe, sshTransportProbeCommand(probe), profile.connectTimeout, profile.connectionAttempts) != nil {
					probes = append(probes, port+":tcp")
					continue
				}
				if transportPort == "" {
					transportPort = probe.Port
				}
				if runSSHQuietWithOptions(probeCtx, probe, sshReadyCommand(probe), profile.connectTimeout, profile.connectionAttempts) == nil {
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
		if time.Until(deadline) <= 0 {
			continue
		}
		if err := sleepContext(ctx, 10*time.Second); err != nil {
			return context.Cause(ctx)
		}
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
	profile := sshReadinessProfileForTarget(*target)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if target.SSHConfigProxy {
		return runSSHQuietWithOptionsResolvePort(ctx, target, sshReadyCommand(*target), profile.connectTimeout, profile.connectionAttempts) == nil
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
		if runSSHQuietWithOptions(ctx, probe, sshReadyCommand(probe), profile.connectTimeout, profile.connectionAttempts) == nil {
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

type sshTransportPreparation struct {
	command string
	input   *replayableSSHInput
	direct  io.ReadSeeker
}

func prepareSSHTransport(target SSHTarget, remote workspaceOwnerRemotePreparation, payload []byte, direct io.ReadSeeker, payloadSize int64, hasInput bool, waitTimeout time.Duration) (sshTransportPreparation, error) {
	// Only owner-expanded WSL2 commands need stdin staging; every other target
	// keeps its established argv and input transport.
	if isWindowsWSL2Target(target) && remote.ownerExpanded {
		var replay *replayableSSHInput
		var err error
		if direct != nil {
			replay, err = newReplayableSSHInputStream([]byte(remote.command), direct, payloadSize)
		} else {
			replay, err = newReplayableSSHInput(append([]byte(remote.command), payload...))
		}
		if err != nil {
			return sshTransportPreparation{}, err
		}
		return sshTransportPreparation{
			command: wsl2StdinScriptCommandWithPayload(len(remote.command), int(payloadSize), waitTimeout),
			input:   replay,
		}, nil
	}

	prepared := sshTransportPreparation{
		command: wrapRemoteForTargetWithWaitTimeout(target, remote.command, waitTimeout),
	}
	if !hasInput {
		return prepared, nil
	}
	if direct != nil {
		prepared.direct = direct
		return prepared, nil
	}
	var err error
	prepared.input, err = newReplayableSSHInput(payload)
	return prepared, err
}

func (p *sshTransportPreparation) run(ctx context.Context, target SSHTarget, connectTimeout, connectionAttempts string, stdout, stderr io.Writer) error {
	args := sshArgsNoInputWithOptions(target, p.command, connectTimeout, connectionAttempts)
	if p.input != nil || p.direct != nil {
		args = sshArgsWithOptions(target, p.command, connectTimeout, connectionAttempts)
	}
	cmd := sshCommandContext(ctx, target, args...)
	input, err := p.reset()
	if err != nil {
		return err
	}
	cmd.Stdin = input
	return runSSHCommand(cmd, stdout, stderr)
}

func (p *sshTransportPreparation) reset() (io.Reader, error) {
	if p.input != nil {
		return p.input.reset()
	}
	if p.direct != nil {
		if _, err := p.direct.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		return p.direct, nil
	}
	return nil, nil
}

func (p *sshTransportPreparation) close() error {
	if p.input == nil {
		return nil
	}
	return p.input.close()
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

func runSSHQuietWithOptionsResolvePort(ctx context.Context, target *SSHTarget, remote, connectTimeout, connectionAttempts string) (err error) {
	prepared, err := prepareWorkspaceOwnerRemote(ctx, *target, remote, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, prepared.close(ctx, *target))
		}
	}()
	transport, err := prepareSSHTransport(*target, prepared, nil, nil, 0, false, 0)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, transport.close()) }()
	var lastErr error
	for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
		probe := *target
		probe.Port = port
		probe.FallbackPorts = []string{}
		err := transport.run(ctx, probe, connectTimeout, connectionAttempts, io.Discard, io.Discard)
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

func runSSHQuietWithRemoteWaitTimeout(ctx context.Context, target SSHTarget, remote string, waitTimeout time.Duration, connectTimeout, connectionAttempts string) (err error) {
	prepared, err := prepareWorkspaceOwnerRemote(ctx, target, remote, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, prepared.close(ctx, target))
		}
	}()
	transport, err := prepareSSHTransport(target, prepared, nil, nil, 0, false, waitTimeout)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, transport.close()) }()
	var lastErr error
	for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
		probe := target
		probe.Port = port
		probe.FallbackPorts = []string{}
		err := transport.run(ctx, probe, connectTimeout, connectionAttempts, io.Discard, io.Discard)
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

func runSSHOutput(ctx context.Context, target SSHTarget, remote string) (output string, err error) {
	prepared, err := prepareWorkspaceOwnerRemote(ctx, target, remote, nil)
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, prepared.close(ctx, target))
		}
	}()
	return runSSHOutputPrepared(ctx, target, prepared)
}

func runSSHOutputPrepared(ctx context.Context, target SSHTarget, prepared workspaceOwnerRemotePreparation) (output string, err error) {
	transport, err := prepareSSHTransport(target, prepared, nil, nil, 0, false, 0)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, transport.close()) }()
	var lastOut []byte
	var lastErr error
	for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
		probe := target
		probe.Port = port
		var out bytes.Buffer
		err := transport.run(ctx, probe, "10", "3", &out, io.Discard)
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

func runSSHOutputWithRemoteWaitTimeout(ctx context.Context, target SSHTarget, remote string, waitTimeout time.Duration, connectTimeout, connectionAttempts string) (output string, err error) {
	prepared, err := prepareWorkspaceOwnerRemote(ctx, target, remote, nil)
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, prepared.close(ctx, target))
		}
	}()
	transport, err := prepareSSHTransport(target, prepared, nil, nil, 0, false, waitTimeout)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, transport.close()) }()
	var lastOut []byte
	var lastErr error
	for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
		probe := target
		probe.Port = port
		probe.FallbackPorts = []string{}
		var out bytes.Buffer
		err := transport.run(ctx, probe, connectTimeout, connectionAttempts, &out, io.Discard)
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

func runSSHCombinedOutput(ctx context.Context, target SSHTarget, remote string) (output string, err error) {
	prepared, err := prepareWorkspaceOwnerRemote(ctx, target, remote, nil)
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, prepared.close(ctx, target))
		}
	}()
	transport, err := prepareSSHTransport(target, prepared, nil, nil, 0, false, 0)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, transport.close()) }()
	var lastOut []byte
	var lastErr error
	for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
		probe := target
		probe.Port = port
		// Crabbox's SSH helpers intentionally execute commands assembled by
		// typed remote-command builders. Callers must shell-quote user data
		// before it reaches this boundary; see remoteCommand/shellQuote tests.
		var out synchronizedBuffer
		err := transport.run(ctx, probe, "10", "3", &out, &out)
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

var idempotentSSHRetryDelay = 2 * time.Second

func runIdempotentSSHCombinedOutput(ctx context.Context, target SSHTarget, remote string, retryDelay time.Duration) (string, error) {
	var lastOut string
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		lastOut, lastErr = runSSHCombinedOutput(ctx, target, remote)
		if lastErr == nil || !shouldRetrySSHPort(lastErr) || attempt == 1 {
			return lastOut, lastErr
		}
		// Only callers whose entire remote command is safe to repeat may use
		// this helper. Transfer, hydration, and user commands stay outside it.
		if err := sleepContext(ctx, retryDelay); err != nil {
			return lastOut, err
		}
	}
	return lastOut, lastErr
}

func runWSL2ControlScriptCombinedOutput(ctx context.Context, target SSHTarget, remote string, waitTimeout time.Duration, connectTimeout, connectionAttempts string) (output string, err error) {
	prepared, err := prepareWorkspaceOwnerRemote(ctx, target, remote, nil)
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, prepared.close(ctx, target))
		}
	}()
	remote = prepared.command
	command := wsl2StdinScriptCommandWithWaitTimeout(len(remote), waitTimeout)
	input, err := newReplayableSSHInput([]byte(remote))
	if err != nil {
		return "", err
	}
	defer func() {
		err = errors.Join(err, input.close())
	}()
	var lastOut []byte
	var lastErr error
	for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
		probe := target
		probe.Port = port
		probe.FallbackPorts = []string{}
		cmd := sshCommandContext(ctx, probe, sshArgsWithOptions(probe, command, connectTimeout, connectionAttempts)...)
		cmd.Stdin, err = input.reset()
		if err != nil {
			return "", err
		}
		var out synchronizedBuffer
		err = runSSHCommand(cmd, &out, &out)
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

func runSSHInput(ctx context.Context, target SSHTarget, remote string, input io.Reader, stdout, stderr io.Writer) (err error) {
	if input == nil {
		input = strings.NewReader("")
	}
	data, err := io.ReadAll(input)
	if err != nil {
		return err
	}
	inputSize := int64(len(data))
	prepared, err := prepareWorkspaceOwnerRemote(ctx, target, remote, &inputSize)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, prepared.close(ctx, target))
		}
	}()
	transport, err := prepareSSHTransport(target, prepared, data, nil, inputSize, true, 0)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, transport.close()) }()
	var lastErr error
	for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
		probe := target
		probe.Port = port
		err = transport.run(ctx, probe, "10", "3", stdout, stderr)
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

func runSSHInputStream(ctx context.Context, target SSHTarget, remote string, input io.ReadSeeker, stdout, stderr io.Writer) (err error) {
	if input == nil {
		input = strings.NewReader("")
	}
	inputSize, err := input.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return err
	}
	prepared, err := prepareWorkspaceOwnerRemote(ctx, target, remote, &inputSize)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, prepared.close(ctx, target))
		}
	}()
	transport, err := prepareSSHTransport(target, prepared, nil, input, inputSize, true, 0)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, transport.close()) }()
	var lastErr error
	for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
		probe := target
		probe.Port = port
		err := transport.run(ctx, probe, "10", "3", stdout, stderr)
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

func runSSHStreamResult(ctx context.Context, target SSHTarget, remote string, stdout, stderr io.Writer) (code int, err error) {
	prepared, err := prepareWorkspaceOwnerRemote(ctx, target, remote, nil)
	if err != nil {
		return 7, err
	}
	transport, err := prepareSSHTransport(target, prepared, nil, nil, 0, false, 0)
	if err != nil {
		return 7, err
	}
	defer func() { err = errors.Join(err, transport.close()) }()
	lastCode := 7
	var lastErr error
	for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
		probe := target
		probe.Port = port
		runErr := transport.run(ctx, probe, "10", "3", stdout, stderr)
		if runErr == nil {
			return 0, nil
		}
		lastErr = runErr
		lastCode = exitCode(runErr)
		if !shouldRetrySSHPort(runErr) {
			return lastCode, errors.Join(runErr, prepared.close(ctx, target))
		}
	}
	return lastCode, errors.Join(lastErr, prepared.close(ctx, target))
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
	args = append(args, sshHostKeyVerificationArgs(target)...)
	if target.AuthSecret || target.NoControlMaster {
		args = append(args,
			"-o", "ControlMaster=no",
			"-o", "ControlPath=none",
			"-o", "ControlPersist=no",
		)
	} else if runtime.GOOS == "windows" {
		// Windows OpenSSH does not support Unix domain sockets for
		// connection multiplexing; ControlMaster causes
		// "getsockname failed: Not a socket" errors.
		args = append(args,
			"-o", "ControlMaster=no",
			"-o", "ControlPath=none",
			"-o", "ControlPersist=no",
		)
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

func sshHostKeyVerificationArgs(target SSHTarget) []string {
	if target.DisableHostKeyChecking {
		return []string{
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			"-o", "LogLevel=ERROR",
		}
	}
	strictHostKeyChecking := "accept-new"
	if target.HostKeyAlias != "" || strings.TrimSpace(target.SSHHostKey) != "" {
		strictHostKeyChecking = "yes"
	}
	args := []string{
		"-o", "StrictHostKeyChecking=" + strictHostKeyChecking,
		"-o", "UserKnownHostsFile=" + sshConfigFileValue(knownHostsFile(target)),
	}
	if strings.TrimSpace(target.SSHHostKey) != "" {
		args = append(args,
			"-o", "GlobalKnownHostsFile=none",
			"-o", "KnownHostsCommand=none",
			"-o", "VerifyHostKeyDNS=no",
			"-o", "UpdateHostKeys=no",
			"-o", "CheckHostIP=no",
		)
	}
	if target.HostKeyAlias != "" {
		args = append(args,
			"-o", "HostKeyAlias="+target.HostKeyAlias,
		)
		args = append(args, "-o", "HostKeyAlgorithms="+sshHostKeyAlgorithms(target))
	}
	return args
}

func sshHostKeyAlgorithms(target SSHTarget) string {
	algorithm := "ssh-ed25519"
	if fields := strings.Fields(target.SSHHostKey); len(fields) > 0 {
		algorithm = fields[0]
	}
	if algorithm == xssh.KeyAlgoRSA {
		return xssh.KeyAlgoRSASHA512 + "," + xssh.KeyAlgoRSASHA256
	}
	return algorithm
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
		target.HostKeyAlias,
		strings.TrimSpace(target.SSHHostKey),
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
	if !opts.UseFilesFrom {
		for _, exclude := range excludes {
			args = append(args, "--exclude", exclude)
		}
	}
	if opts.Debug {
		args = append(args, "--stats", "--itemize-changes", "--progress")
	}
	args = append(args, ensureTrailingSlash(rsyncLocalPath(src)), target.User+"@"+target.Host+":"+dst+"/")
	var cmd *exec.Cmd
	var err error
	if runtime.GOOS == "windows" {
		cmd, err = windowsRsyncCommand(ctx, target, args)
		if err != nil {
			return err
		}
	} else {
		cmd = exec.CommandContext(ctx, "rsync", args...)
	}
	applyTargetChildEnvironment(cmd, target)
	owner := workspaceOwnerFromContext(ctx)
	guardStarted := false
	if owner != nil && !isWindowsNativeTarget(target) {
		rawCtx := contextWithoutWorkspaceOwner(ctx)
		if err := runSSHQuiet(rawCtx, target, owner.rsyncPrepareCommand()); err != nil {
			return exit(7, "prepare rsync workspace witness: %v", err)
		}
		if _, err := runWorkspaceOwnerBackgroundOutput(rawCtx, target, owner, owner.rsyncGuardPayload(dst)); err != nil {
			return exit(7, "start rsync workspace witness: %v", err)
		}
		if err := owner.WaitForChild(rawCtx, 10*time.Second); err != nil {
			return err
		}
		guardStarted = true
	}
	start := time.Now()
	if opts.UseFilesFrom {
		cmd.Stdin = bytes.NewReader(opts.FilesFrom)
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	stopHeartbeat := startSyncHeartbeat(stderr, start, opts.HeartbeatInterval)
	err = cmd.Run()
	stopHeartbeat()
	if guardStarted {
		guardCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		rawGuardCtx := contextWithoutWorkspaceOwner(guardCtx)
		guardErr := runSSHQuiet(rawGuardCtx, target, owner.rsyncStopCommand())
		if guardErr == nil {
			guardErr = waitWorkspaceOwnerNoChild(rawGuardCtx, owner, 15*time.Second)
		}
		cleanupErr := runSSHQuiet(rawGuardCtx, target, owner.rsyncPrepareCommand())
		cancel()
		if guardErr == nil {
			guardErr = cleanupErr
		}
		if guardErr != nil && err == nil {
			err = exit(7, "finish rsync workspace witness: %v", guardErr)
		}
	}
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

func wsl2StdinScriptCommandWithWaitTimeout(inputSize int, waitTimeout time.Duration) string {
	return wsl2StdinScriptCommandWithPayload(inputSize, 0, waitTimeout)
}

func wsl2StdinScriptCommandWithPayload(scriptSize, payloadSize int, waitTimeout time.Duration) string {
	// Keep staging, execution, and cleanup in one WSL process so the timeout
	// covers the lifecycle. The frame accounts for .NET's stdin preamble, and
	// the marker lets post-exit cleanup preserve collisions.
	wait := `$process.WaitForExit()
  $code = $process.ExitCode`
	copyScript := `[Console]::OpenStandardInput().CopyTo($process.StandardInput.BaseStream)`
	watch := ""
	if waitTimeout > 0 {
		waitMS := int(waitTimeout / time.Millisecond)
		watch = `$watch = [System.Diagnostics.Stopwatch]::StartNew()
`
		wait = fmt.Sprintf(`$left = %d - [int]$watch.ElapsedMilliseconds
  if ($left -le 0 -or -not $process.WaitForExit($left)) {
    try {
      $process.Kill($true)
    } catch {
      $process.Kill()
    }
    $cleanupAllowed = $process.WaitForExit(5000)
    throw "WSL2 command timed out after %s"
  }
  $code = $process.ExitCode`, waitMS, waitTimeout.Round(time.Second))
		copyScript = fmt.Sprintf(`$copy = [Console]::OpenStandardInput().CopyToAsync($process.StandardInput.BaseStream)
    $left = %d - [int]$watch.ElapsedMilliseconds
    if ($left -le 0 -or -not $copy.Wait($left)) {
      try {
        $process.Kill($true)
      } catch {
        $process.Kill()
      }
      $cleanupAllowed = $process.WaitForExit(5000)
      throw "WSL2 command timed out after %s"
    }`, waitMS, waitTimeout.Round(time.Second))
	}
	return powershellCommand(`$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"
$dir = "/tmp/crabbox-command-" + [Guid]::NewGuid().ToString("N")
$failure = $null
$cleanupAllowed = $true
` + watch + `$preamble = [Console]::InputEncoding.GetPreamble().Length
$psi = [System.Diagnostics.ProcessStartInfo]::new("wsl.exe")
$psi.UseShellExecute = $false
$psi.RedirectStandardInput = $true
$psi.Arguments = '--exec sh -c "set -u;umask 077;dir=$1;script_expected=$2;payload_expected=$3;preamble=$4;mkdir -m 700 -- $dir||exit;: >$dir/.crabbox-owned;trap ''rm -rf -- $dir'' EXIT;cat >$dir/framed||exit;test $(wc -c <$dir/framed) = $((script_expected+payload_expected+preamble))||exit;dd if=$dir/framed of=$dir/script.sh bs=1 skip=$preamble count=$script_expected 2>/dev/null||exit;dd if=$dir/framed of=$dir/payload bs=1 skip=$((preamble+script_expected)) count=$payload_expected 2>/dev/null||exit;rm -f -- $dir/framed;code=0;bash $dir/script.sh <$dir/payload||code=$?;rm -rf -- $dir;cleanup=$?;trap - EXIT;if [ $cleanup -ne 0 ];then echo ''WSL2 command cleanup failed: exit ''$cleanup >&2;[ $code -ne 0 ]||code=$cleanup;fi;exit $code" sh ' + $dir + ' ` + strconv.Itoa(scriptSize) + ` ` + strconv.Itoa(payloadSize) + ` ' + $preamble
$process = [System.Diagnostics.Process]::Start($psi)
try {
  try {
    ` + copyScript + `
  } finally {
    $process.StandardInput.BaseStream.Close()
  }
  ` + wait + `
} catch {
  $failure = $_
}
if ($null -ne $failure) {
  if (-not $cleanupAllowed) {
    [Console]::Error.WriteLine("WSL2 command cleanup skipped: process still running")
    throw $failure
  }
  try {
    $cleanupInfo = [System.Diagnostics.ProcessStartInfo]::new("wsl.exe")
    $cleanupInfo.UseShellExecute = $false
    $cleanupInfo.Arguments = '--exec sh -c "test ! -f $1/.crabbox-owned||rm -rf -- $1" sh ' + $dir
    $cleanup = [System.Diagnostics.Process]::Start($cleanupInfo)
    if (-not $cleanup.WaitForExit(5000)) {
      $cleanup.Kill()
      $cleanup.WaitForExit(5000) | Out-Null
      [Console]::Error.WriteLine("WSL2 command cleanup failed: timed out")
    } elseif ($cleanup.ExitCode -ne 0) {
      [Console]::Error.WriteLine("WSL2 command cleanup failed: exit " + $cleanup.ExitCode)
    }
  } catch {
    [Console]::Error.WriteLine("WSL2 command cleanup failed: " + $_.Exception.Message)
  }
  throw $failure
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
func windowsRsyncCommand(ctx context.Context, target SSHTarget, args []string) (*exec.Cmd, error) {
	wslExe, ok := windowsRsyncWSLExecutable(ctx, target)
	if !ok {
		return windowsNativeRsyncCommand(ctx, args)
	}
	return windowsWSLRsyncCommand(ctx, target, args, wslExe)
}

func windowsRsyncWSLExecutable(ctx context.Context, target SSHTarget) (string, bool) {
	wslExe, err := exec.LookPath("wsl.exe")
	if err != nil {
		wslExe, err = exec.LookPath("wsl")
	}
	if err != nil || !windowsWSLHasNativeRsyncSSH(ctx, target, wslExe) {
		return "", false
	}
	return wslExe, true
}

func windowsWSLRsyncCommand(ctx context.Context, target SSHTarget, args []string, wslExe string) (*exec.Cmd, error) {
	mountRoot := windowsWSLMountRoot(ctx, target, wslExe)

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
			applyTargetChildEnvironment(cpCmd, target)
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
			applyTargetChildEnvironment(cpKnownHostsCmd, target)
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
	return exec.CommandContext(ctx, wslExe, append([]string{"rsync"}, wslArgs...)...), nil
}

func windowsNativeRsyncCommand(ctx context.Context, args []string) (*exec.Cmd, error) {
	name, pairedArgs, err := windowsNativeRsyncCommandSpec(args)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, name, pairedArgs...)
	applyWindowsNativeRsyncEnvironment(cmd)
	return cmd, nil
}

func windowsNativeRsyncCommandSpec(args []string) (string, []string, error) {
	rsyncPath, sshPath, err := resolveWindowsNativeRsyncPair(exec.LookPath, os.Stat)
	if err != nil {
		return "", nil, err
	}
	pairedArgs, err := bindWindowsNativeRsyncSSH(args, sshPath)
	if err != nil {
		return "", nil, err
	}
	return rsyncPath, pairedArgs, nil
}

func resolveWindowsNativeRsyncPair(lookPath func(string) (string, error), stat func(string) (os.FileInfo, error)) (string, string, error) {
	rsyncPath, err := lookPath("rsync.exe")
	if err != nil {
		return "", "", exit(2, "native Windows transfers require rsync.exe on PATH with a sibling ssh.exe in the same directory; install MSYS2 rsync and OpenSSH together, or use the supported WSL2 transport")
	}
	if absolute, absoluteErr := filepath.Abs(rsyncPath); absoluteErr == nil {
		rsyncPath = absolute
	}
	sshPath := filepath.Join(filepath.Dir(rsyncPath), "ssh.exe")
	info, err := stat(sshPath)
	if err != nil || !info.Mode().IsRegular() {
		return "", "", exit(2, "native Windows rsync requires its matching sibling OpenSSH at %s next to %s; install MSYS2 rsync and OpenSSH together, or use the supported WSL2 transport", sshPath, rsyncPath)
	}
	return rsyncPath, sshPath, nil
}

func bindWindowsNativeRsyncSSH(args []string, sshPath string) ([]string, error) {
	paired := append([]string(nil), args...)
	for index := 0; index < len(paired); index++ {
		if paired[index] != "-e" {
			continue
		}
		if index+1 >= len(paired) {
			return nil, exit(2, "native Windows rsync command is missing its owned -e remote shell value")
		}
		const ownedSSH = "'ssh'"
		remoteShell := paired[index+1]
		if !strings.HasPrefix(remoteShell, ownedSSH) || len(remoteShell) > len(ownedSSH) && remoteShell[len(ownedSSH)] != ' ' {
			return nil, exit(2, "native Windows rsync command has an unsupported -e remote shell; Crabbox must own the OpenSSH pairing")
		}
		paired[index+1] = rsyncShellWords([]string{sshPath})[0] + remoteShell[len(ownedSSH):]
		return paired, nil
	}
	return nil, exit(2, "native Windows rsync command is missing its owned -e remote shell")
}

func applyWindowsNativeRsyncEnvironment(cmd *exec.Cmd) {
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, "MSYS2_ARG_CONV_EXCL=*", "MSYS_NO_PATHCONV=1", "CYGWIN=nodosfilewarning")
}

func windowsHostPath(path string) string {
	path = strings.ReplaceAll(path, `\`, "/")
	if len(path) >= 3 && path[0] == '/' && path[2] == '/' &&
		(path[1] >= 'a' && path[1] <= 'z' || path[1] >= 'A' && path[1] <= 'Z') {
		return string(path[1]) + ":" + path[2:]
	}
	return path
}

func windowsWSLHasNativeRsyncSSH(ctx context.Context, target SSHTarget, wslExe string) bool {
	if strings.TrimSpace(wslExe) == "" {
		return false
	}
	cmd := windowsWSLNativeToolProbeCommand(ctx, wslExe)
	applyTargetChildEnvironment(cmd, target)
	out, err := cmd.Output()
	return err == nil && windowsWSLNativeToolPaths(string(out))
}

func windowsWSLNativeToolProbeCommand(ctx context.Context, wslExe string) *exec.Cmd {
	// Direct command output is intentional: assignment plus printf produced blank lines on real Windows/WSL systems.
	return exec.CommandContext(ctx, wslExe, "sh", "-c", "command -v rsync || exit 1; command -v ssh || exit 1")
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

func windowsWSLMountRoot(ctx context.Context, target SSHTarget, wslExe string) string {
	if runtime.GOOS != "windows" {
		return "/mnt"
	}
	cmd := exec.CommandContext(ctx, wslExe, "sh", "-c", "if [ -d /mnt/host/c ]; then printf /mnt/host; else printf /mnt; fi")
	applyTargetChildEnvironment(cmd, target)
	out, err := cmd.Output()
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

func windowsPowerShellCopyExactInput(destination string, inputSize int64) string {
	return `$stdin = [Console]::OpenStandardInput()
$remaining = [Int64]` + strconv.FormatInt(inputSize, 10) + `
$buffer = New-Object byte[] 65536
while ($remaining -gt 0) {
	$readSize = [int][Math]::Min([Int64]$buffer.Length, $remaining)
	$read = $stdin.Read($buffer, 0, $readSize)
	if ($read -le 0) { throw "SSH stdin ended before the framed payload" }
	` + destination + `.Write($buffer, 0, $read)
	$remaining -= $read
}
`
}

func windowsPowerShellStdinScriptCommand(inputSize int) string {
	return powershellCommand(`$ErrorActionPreference = "Stop"
$path = Join-Path $env:TEMP ("crabbox-stdin-command-" + [Guid]::NewGuid().ToString("N") + ".ps1")
try {
	$scriptFile = [IO.File]::Open($path, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
	try {
		$bom = [byte[]](0xEF, 0xBB, 0xBF)
		$scriptFile.Write($bom, 0, $bom.Length)
` + windowsPowerShellCopyExactInput("$scriptFile", int64(inputSize)) + `		$scriptFile.Flush($true)
	} finally {
		$scriptFile.Dispose()
	}
	& powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $path
	$code = $LASTEXITCODE
} finally {
	Remove-Item -Force -LiteralPath $path -ErrorAction SilentlyContinue
}
if ($null -eq $code) { $code = 0 }
exit $code
`)
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

func remoteGitWorkspaceFunctions() string {
	return `exact_git_root() {
  git_root="$(git rev-parse --show-toplevel 2>/dev/null)" || return 1
  git_root="$(cd -P -- "$git_root" 2>/dev/null && pwd -P)" || return 1
  [ "$git_root" = "$(pwd -P)" ]
}
usable_git_workspace() (
  exact_git_root || exit 1
  git rev-parse --verify HEAD^{commit} >/dev/null 2>&1 || exit 1
  workspace_index="$(git rev-parse --git-path index 2>/dev/null)" || exit 1
  case "$workspace_index" in /*) ;; *) workspace_index="$PWD/$workspace_index" ;; esac
  [ -f "$workspace_index" ] || exit 1
  workspace_tree="$(git write-tree 2>/dev/null)" || exit 1
  [ -n "$workspace_tree" ]
)
normalize_git_remote() {
  case "$1" in
    git@github.com:*) remote_path="${1#git@github.com:}"; remote_path="${remote_path%.git}"; printf 'https://github.com/%s.git' "$remote_path" ;;
    *) printf %s "$1" ;;
  esac
}
origin_matches() {
  actual_origin="$(git remote get-url origin 2>/dev/null)" || return 1
  [ "$(normalize_git_remote "$actual_origin")" = "$expected_origin" ]
}
repair_origin() {
  if git remote get-url origin >/dev/null 2>&1; then
    git remote set-url origin "$expected_origin" >/dev/null 2>&1
  else
    git remote add origin "$expected_origin" >/dev/null 2>&1
  fi && origin_matches
}
`
}

func remoteGitHydrateStatus(workdir, baseRef, expectedSHA string) string {
	if baseRef == "" || expectedSHA == "" {
		return "printf ''"
	}
	script := `cd ` + shellQuote(workdir) + ` && ` + remoteGitWorkspaceFunctions() + `
if ! exact_git_root; then
  exit 0
fi
` + remoteSyncMetaDirScript() + `
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

func remoteGitSeed(workdir string, plan gitCoherencePlan) string {
	if !plan.seedEnabled() {
		return "true"
	}
	parent := filepath.ToSlash(filepath.Dir(workdir))
	script := `set -e
workdir=` + shellQuote(workdir) + `
expected_origin=` + shellQuote(plan.RemoteURL) + `
expected_tree=` + shellQuote(plan.Tree) + `
` + remoteGitWorkspaceFunctions() + `
if [ -d "$workdir" ]; then
  cd "$workdir"
  if usable_git_workspace; then
    repair_origin
    exit 0
  fi
fi
mkdir -p ` + shellQuote(parent) + `
tmp="$(mktemp -d ` + shellQuote(parent+"/.seed.XXXXXX") + `)"
cleanup_seed() { rm -rf -- "$tmp"; }
trap cleanup_seed EXIT
git clone --quiet --filter=blob:none --no-checkout --single-branch --branch ` + shellQuote(plan.Branch) + ` "$expected_origin" "$tmp" >/dev/null 2>&1
git -C "$tmp" checkout --quiet --detach ` + shellQuote(plan.Target) + `
[ "$(git -C "$tmp" rev-parse --verify HEAD^{commit})" = ` + shellQuote(plan.Target) + ` ]
cd "$tmp"
usable_git_workspace
if [ -n "$expected_tree" ]; then
  [ "$(git write-tree)" = "$expected_tree" ]
fi
repair_origin
cd /
rm -rf -- "$workdir"
mv -- "$tmp" "$workdir"
trap - EXIT
`
	return "bash -lc " + shellQuote(script)
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

func remoteReadSyncFingerprint(workdir string, plan gitCoherencePlan) string {
	if !plan.enabled() {
		return "printf ''"
	}
	script := "cd " + shellQuote(workdir) + " && " + `expected_origin=` + shellQuote(plan.RemoteURL) + `
` + remoteGitWorkspaceFunctions() + `
if ! exact_git_root || ! origin_matches; then
  exit 0
fi
` + remoteSyncMetaDirScript() + `
committed="$meta_dir/sync-finalize-token"
complete="$meta_dir/sync-finalize-complete-token"
if [ -f "$committed" ] && [ -f "$complete" ] &&
   [ "$(cat "$committed")" = "$(cat "$complete")" ] &&
   [ "$(git rev-parse --verify HEAD^{commit} 2>/dev/null || true)" = ` + shellQuote(plan.Target) + ` ] &&
   [ "$(git write-tree 2>/dev/null || true)" = ` + shellQuote(plan.Tree) + ` ]; then
  cat "$meta_dir/sync-fingerprint" 2>/dev/null || true
fi`
	return "bash -lc " + shellQuote(script)
}

func remoteInvalidateSyncFingerprintForTarget(target SSHTarget, workdir string) string {
	if isWindowsNativeTarget(target) {
		return powershellCommand("exit 0")
	}
	script := `set -e
cd ` + shellQuote(workdir) + `
` + remoteSyncMetaDirScript() + `
rm -f "$meta_dir/sync-fingerprint"`
	return "bash -lc " + shellQuote(script)
}

type remoteSyncFinalizeOptions struct {
	AllowMassDeletions bool
	HydrateGit         bool
	BaseRef            string
	BaseSHA            string
	Fingerprint        string
	Token              string
	Coherence          gitCoherencePlan
}

func remoteSyncPendingManifestName(token string) string {
	return "sync-manifest." + token + ".new"
}

func remoteSyncPendingDeletedName(token string) string {
	return "sync-deleted." + token + ".new"
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

func remoteWriteSyncManifestsNew(workdir, finalizeToken string) string {
	manifestName := remoteSyncPendingManifestName(finalizeToken)
	deletedName := remoteSyncPendingDeletedName(finalizeToken)
	script := "set -e\nmkdir -p " + shellQuote(workdir) + "\ncd " + shellQuote(workdir) + "\n" + remoteSyncMetaDirScript() + `mkdir -p "$meta_dir"
` + remoteSyncAbandonedMetadataCleanup() + `
if ! IFS= read -r manifest_len; then
  echo "invalid sync manifest length" >&2
  exit 1
fi
case "$manifest_len" in
  ''|*[!0-9]*|0[0-9]*) echo "invalid sync manifest length" >&2; exit 1 ;;
esac
if ! IFS= read -r deleted_len; then
  echo "invalid sync deleted length" >&2
  exit 1
fi
case "$deleted_len" in
  ''|*[!0-9]*|0[0-9]*) echo "invalid sync deleted length" >&2; exit 1 ;;
esac
# Keep this to POSIX dd operands: minimal guests commonly provide BusyBox dd,
# which rejects GNU's progress-suppression extension. Check the output size too:
# portable dd can exit successfully after reading fewer records than requested.
dd bs=1 count="$manifest_len" of="$meta_dir/` + manifestName + `" 2>/dev/null
manifest_size=$(wc -c < "$meta_dir/` + manifestName + `" | tr -d '[:space:]')
if [ "$manifest_size" != "$manifest_len" ]; then
  echo "short sync manifest: got $manifest_size want $manifest_len" >&2
  exit 1
fi
dd bs=1 count="$deleted_len" of="$meta_dir/` + deletedName + `" 2>/dev/null
deleted_size=$(wc -c < "$meta_dir/` + deletedName + `" | tr -d '[:space:]')
if [ "$deleted_size" != "$deleted_len" ]; then
  echo "short sync deleted manifest: got $deleted_size want $deleted_len" >&2
  exit 1
fi
`
	return "bash -lc " + shellQuote(script)
}

func syncManifestInputForTarget(target SSHTarget, manifestData, deletedData []byte) string {
	if isWindowsWSL2Target(target) {
		manifestEncoded := base64.StdEncoding.EncodeToString(manifestData)
		deletedEncoded := base64.StdEncoding.EncodeToString(deletedData)
		return fmt.Sprintf("%d\n%d\n", len(manifestEncoded), len(deletedEncoded)) + manifestEncoded + deletedEncoded
	}
	return fmt.Sprintf("%d\n%d\n", len(manifestData), len(deletedData)) + string(manifestData) + string(deletedData)
}

func remoteWriteSyncManifestsNewForTarget(target SSHTarget, workdir, finalizeToken string) string {
	if isWindowsWSL2Target(target) {
		return remoteWriteSyncManifestsNewPython(workdir, finalizeToken)
	}
	return remoteWriteSyncManifestsNew(workdir, finalizeToken)
}

func remoteWriteSyncManifestsNewPython(workdir, finalizeToken string) string {
	manifestName := remoteSyncPendingManifestName(finalizeToken)
	deletedName := remoteSyncPendingDeletedName(finalizeToken)
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
		remoteSyncAbandonedMetadataCleanup() + "\n" +
		"python3 -c " + shellQuote(python) + " \"$meta_dir/" + manifestName + "\" \"$meta_dir/" + deletedName + "\"\n"
	return "bash -lc " + shellQuote(script)
}

func remoteSyncAbandonedMetadataCleanup() string {
	return `find "$meta_dir" -type f \( -name 'sync-manifest.new' -o -name 'sync-deleted.new' -o -name 'sync-manifest.*.new' -o -name 'sync-deleted.*.new' -o -name 'sync-manifest.*.sorted' -o -name 'sync-finalize-token.tmp.*' -o -name 'sync-finalize-complete-token.tmp.*' -o -name 'sync-git-status.*' \) -mtime +7 -exec rm -f -- {} \; 2>/dev/null || true`
}

func remoteSeedSyncManifestFromGit(workdir string) string {
	script := "set -e\ncd " + shellQuote(workdir) + `
` + remoteGitWorkspaceFunctions() + `
` + remoteSyncMetaDirScript() + `
old="$meta_dir/sync-manifest"
if [ ! -f "$old" ] && exact_git_root; then
  mkdir -p "$meta_dir"
  git ls-files -z > "$old"
fi
`
	return "bash -lc " + shellQuote(script)
}

func remotePruneSyncManifest(workdir, finalizeToken string) string {
	manifestName := remoteSyncPendingManifestName(finalizeToken)
	deletedName := remoteSyncPendingDeletedName(finalizeToken)
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
new="$meta_dir/` + manifestName + `"
deleted="$meta_dir/` + deletedName + `"
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

func remotePruneSyncManifestForTarget(target SSHTarget, workdir, finalizeToken string) string {
	if isWindowsWSL2Target(target) {
		return remotePruneSyncManifestCoreutils(workdir, finalizeToken)
	}
	return remotePruneSyncManifest(workdir, finalizeToken)
}

func remotePruneSyncManifestCoreutils(workdir, finalizeToken string) string {
	manifestName := remoteSyncPendingManifestName(finalizeToken)
	deletedName := remoteSyncPendingDeletedName(finalizeToken)
	script := "set -e -o pipefail\ncd " + shellQuote(workdir) + `
` + remoteSyncMetaDirScript() + `
old="$meta_dir/sync-manifest"
new="$meta_dir/` + manifestName + `"
deleted="$meta_dir/` + deletedName + `"
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
  old_sorted="$meta_dir/sync-manifest.` + finalizeToken + `.old.sorted"
  new_sorted="$meta_dir/sync-manifest.` + finalizeToken + `.new.sorted"
  cleanup_sorted_manifests() {
    rm -f "$old_sorted" "$new_sorted"
  }
  trap cleanup_sorted_manifests EXIT
  LC_ALL=C sort -z "$old" > "$old_sorted"
  LC_ALL=C sort -z "$new" > "$new_sorted"
  comm -z -23 "$old_sorted" "$new_sorted" | delete_paths
  cleanup_sorted_manifests
  trap - EXIT
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
	manifestName := remoteSyncPendingManifestName(opts.Token)
	deletedName := remoteSyncPendingDeletedName(opts.Token)
	script := `set -e
cd ` + shellQuote(workdir) + `
` + remoteGitWorkspaceFunctions() + `
` + remoteSyncMetaDirScript() + `
mkdir -p "$meta_dir"
new="$meta_dir/` + manifestName + `"
deleted="$meta_dir/` + deletedName + `"
manifest="$meta_dir/sync-manifest"
committed_token="$meta_dir/sync-finalize-token"
complete_token="$meta_dir/sync-finalize-complete-token"
expected_token=` + shellQuote(opts.Token) + `
publish_fingerprint=` + shellQuote(opts.Fingerprint) + `
case "$expected_token" in
  ''|*[!0-9a-f]*) echo "remote sync finalize failed: invalid sync token" >&2; exit 67 ;;
esac
` + remoteSyncFinalizeLockScript() + `
git_status="$meta_dir/sync-git-status.$expected_token.$$"
rm -f "$git_status"
if [ -f "$manifest" ] &&
   [ -f "$committed_token" ] && [ "$(cat "$committed_token")" = "$expected_token" ] &&
   [ -f "$complete_token" ] && [ "$(cat "$complete_token")" = "$expected_token" ]; then
  exit 0
fi
rm -f "$complete_token"
if [ -f "$new" ]; then
  committed_tmp="$committed_token.tmp.$$"
  printf %s "$expected_token" > "$committed_tmp"
  mv "$committed_tmp" "$committed_token"
  rm -f "$deleted"
  mv "$new" "$manifest"
elif [ ! -f "$manifest" ] || [ ! -f "$committed_token" ] || [ "$(cat "$committed_token")" != "$expected_token" ]; then
  echo "remote sync finalize failed: no committed manifest for this sync" >&2
  exit 67
fi
`
	if opts.Coherence.enabled() {
		script += remoteGitCoherenceFinalizeScript(opts.Coherence, allowValue)
	} else {
		script += `publish_fingerprint=
if exact_git_root && git status --short >"$git_status" 2>/dev/null; then
  deletions=$(awk '/^ D|^D / { n++ } END { print n+0 }' "$git_status")
  if [ ` + shellQuote(allowValue) + ` != '1' ] && [ "$deletions" -ge 200 ]; then
    echo "remote sync sanity failed: $deletions tracked deletions" >&2
    awk '/^ D|^D / { print "  " substr($0,4) }' "$git_status" | head -20 >&2
    exit 66
  fi
fi
`
	}
	if opts.HydrateGit && opts.BaseRef != "" {
		refspec := "+refs/heads/" + opts.BaseRef + ":refs/remotes/origin/" + opts.BaseRef
		script += `if exact_git_root && git remote get-url origin >/dev/null 2>&1; then
  if [ "$(git rev-parse --is-shallow-repository 2>/dev/null)" = true ]; then
    git fetch --quiet --unshallow origin ` + shellQuote(refspec) + ` || git fetch --quiet --depth=1000 origin ` + shellQuote(refspec) + ` || git fetch --quiet origin ` + shellQuote(refspec) + ` || git fetch --quiet origin ` + shellQuote(opts.BaseRef) + ` || true
  else
    git fetch --quiet origin ` + shellQuote(refspec) + ` || git fetch --quiet origin ` + shellQuote(opts.BaseRef) + ` || true
  fi
fi
`
	}
	if opts.BaseRef != "" && opts.BaseSHA != "" {
		script += `base_tmp="$meta_dir/git-hydrate-base.tmp.$$"
printf %s ` + shellQuote(opts.BaseRef+" "+opts.BaseSHA+"\n") + ` > "$base_tmp"
mv "$base_tmp" "$meta_dir/git-hydrate-base"
`
	}
	script += `if [ -n "$publish_fingerprint" ]; then
  fingerprint_tmp="$meta_dir/sync-fingerprint.tmp.$$"
  printf %s "$publish_fingerprint" > "$fingerprint_tmp"
  mv "$fingerprint_tmp" "$meta_dir/sync-fingerprint"
else
  rm -f "$meta_dir/sync-fingerprint"
fi
complete_tmp="$complete_token.tmp.$$"
printf %s "$expected_token" > "$complete_tmp"
mv "$complete_tmp" "$complete_token"
coherence_committed=1
`
	return "bash -lc " + shellQuote(script)
}

func remoteGitCoherenceFinalizeScript(plan gitCoherencePlan, allowMassDeletions string) string {
	return `
coherence_committed=; coherence_mutated=; head_changed=; index_changed=
tmp_ref="refs/crabbox/sync-$expected_token"; advertised_branch=` + shellQuote(plan.Branch) + `; expected_origin=` + shellQuote(plan.RemoteURL) + `
if ! exact_git_root; then
	publish_fingerprint=
elif ! repair_origin; then
	echo "remote sync finalize failed: Git origin repair failed" >&2; cleanup_finalize_lock; exit 67
elif ! git fetch --quiet --no-tags "$expected_origin" "+refs/heads/$advertised_branch:$tmp_ref"; then
	git update-ref -d "$tmp_ref" >/dev/null 2>&1 || true; echo "remote sync finalize failed: Git coherence fetch failed" >&2; cleanup_finalize_lock; exit 67
elif ! git merge-base --is-ancestor ` + shellQuote(plan.Target) + ` "$tmp_ref" >/dev/null 2>&1; then
	git update-ref -d "$tmp_ref" >/dev/null 2>&1 || true; echo "remote sync finalize failed: requested commit is not on advertised branch" >&2; cleanup_finalize_lock; exit 67
elif [ "$(git rev-parse --verify ` + shellQuote(plan.Target+"^{tree}") + ` 2>/dev/null || true)" != ` + shellQuote(plan.Tree) + ` ]; then
	git update-ref -d "$tmp_ref" >/dev/null 2>&1 || true; echo "remote sync finalize failed: requested Git tree verification failed" >&2; cleanup_finalize_lock; exit 67
else
  coherence_cleanup() {
    status=$?
    if [ -z "$coherence_committed" ] && [ -n "$coherence_mutated" ]; then
      if [ -n "$head_changed" ]; then
        if git update-ref --no-deref HEAD "$old_head" ` + shellQuote(plan.Target) + `; then
          if [ -n "$old_sym" ] && ! git symbolic-ref -q HEAD >/dev/null 2>&1 &&
             [ "$(git rev-parse --verify HEAD^{commit} 2>/dev/null || true)" = "$old_head" ] &&
             [ "$(git rev-parse --verify "$old_sym^{commit}" 2>/dev/null || true)" = "$old_head" ]; then git symbolic-ref HEAD "$old_sym" || status=67; fi
        else status=67; fi
      fi
      if [ -n "$index_changed" ] && [ "$(cat "$index_lock" 2>/dev/null || true)" = "$index_marker" ] && cmp -s "$index_path" "$index_verify"; then
        cp -p "$index_backup" "$index_restore" && mv "$index_restore" "$index_path" || status=67
      elif [ -n "$index_changed" ]; then status=67; fi
    fi
    git update-ref -d "$tmp_ref" "$fetched_head" >/dev/null 2>&1 || true; rm -f "${index_backup:-}" "${index_candidate:-}" "${index_verify:-}" "${index_restore:-}"
    if [ -n "${index_lock:-}" ] && [ "$(cat "$index_lock" 2>/dev/null || true)" = "${index_marker:-}" ]; then rm -f "$index_lock"; fi
    # Bash 5.2 can corrupt function context when a successful EXIT handler re-exits.
    cleanup_finalize_lock; trap - EXIT; if [ "$status" -ne 0 ]; then exit "$status"; fi
  }
  trap coherence_cleanup EXIT
  fetched_head="$(git rev-parse --verify "$tmp_ref^{commit}")"; old_head="$(git rev-parse --verify HEAD^{commit})"
  old_sym="$(git symbolic-ref -q HEAD 2>/dev/null || true)"
  index_path="$(git rev-parse --git-path index)"
  case "$index_path" in /*) ;; *) index_path="$PWD/$index_path" ;; esac
  index_lock="$index_path.lock"; index_marker="$expected_token.$$"
  index_backup="$index_path.crabbox.$$.backup"; index_candidate="$index_path.crabbox.$$.new"; index_verify="$index_path.crabbox.$$.verify"; index_restore="$index_path.crabbox.$$.restore"
  cp -p "$index_path" "$index_backup"; git read-tree --reset --index-output="$index_candidate" ` + shellQuote(plan.Target) + `
  [ "$(GIT_INDEX_FILE="$index_candidate" git write-tree)" = ` + shellQuote(plan.Tree) + ` ]
  GIT_INDEX_FILE="$index_candidate" git diff-files --name-status >"$git_status" 2>/dev/null ||
    { echo "remote sync sanity failed: candidate Git index inspection failed" >&2; exit 67; }
  deletions=$(awk '$1 == "D" { n++ } END { print n+0 }' "$git_status"); [ ` + shellQuote(allowMassDeletions) + ` = '1' ] || [ "$deletions" -lt 200 ]
  (set -C; printf %s "$index_marker" > "$index_lock") || { echo "remote sync finalize failed: Git index is busy" >&2; exit 67; }
  cmp -s "$index_path" "$index_backup" || { echo "remote sync finalize failed: Git index changed concurrently" >&2; exit 67; }
  cp -p "$index_candidate" "$index_verify"; mv "$index_candidate" "$index_path"; index_changed=1; coherence_mutated=1
  cmp -s "$index_path" "$index_verify" && [ "$(GIT_INDEX_FILE="$index_verify" git write-tree)" = ` + shellQuote(plan.Tree) + ` ] || { echo "remote sync finalize failed: installed Git index verification failed" >&2; exit 67; }
  git update-ref --no-deref HEAD ` + shellQuote(plan.Target) + ` "$old_head"; head_changed=1
  [ "$(git rev-parse --verify HEAD^{commit})" = ` + shellQuote(plan.Target) + ` ]
fi
`
}

func remoteSyncFinalizeLockScript() string {
	return `lock_path="$meta_dir/sync-finalize-lock"
lock_waits=0
while ! ln -s "$$" "$lock_path" 2>/dev/null; do
  lock_owner=$(readlink "$lock_path" 2>/dev/null || true)
  lock_owner_live=
  case "$lock_owner" in
    ''|*[!0-9]*) ;;
    *) if kill -0 "$lock_owner" 2>/dev/null; then lock_owner_live=1; fi ;;
  esac
  if [ -n "$lock_owner_live" ]; then
    sleep 1
  else
    sleep 1
    confirmed_owner=$(readlink "$lock_path" 2>/dev/null || true)
    if [ "$confirmed_owner" = "$lock_owner" ]; then
      owner_dead=
      case "$confirmed_owner" in
        ''|*[!0-9]*) owner_dead=1 ;;
        *) if ! kill -0 "$confirmed_owner" 2>/dev/null; then owner_dead=1; fi ;;
      esac
      if [ -n "$owner_dead" ]; then
        stale_lock="$lock_path.stale.$$"
        if mv "$lock_path" "$stale_lock" 2>/dev/null; then
          moved_owner=$(readlink "$stale_lock" 2>/dev/null || true)
          if [ "$moved_owner" != "$confirmed_owner" ]; then
            if [ ! -e "$lock_path" ] && [ ! -L "$lock_path" ]; then
              mv "$stale_lock" "$lock_path" 2>/dev/null || true
            fi
            echo "remote sync finalize failed: lock ownership changed during recovery" >&2
            exit 67
          fi
          rm -f "$stale_lock"
          continue
        fi
      fi
    fi
  fi
  lock_waits=$((lock_waits + 1))
  if [ "$lock_waits" -ge 120 ]; then
    echo "remote sync finalize failed: timed out waiting for active finalize" >&2
    exit 67
  fi
done
cleanup_finalize_lock() {
  if [ -n "${git_status:-}" ]; then
    rm -f -- "$git_status"
  fi
  if [ "$(readlink "$lock_path" 2>/dev/null || true)" = "$$" ]; then
    rm -f "$lock_path"
  fi
}
trap cleanup_finalize_lock EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
`
}

func remoteSyncMetaDirScript() string {
	return `meta_dir=$(git_root=; if git_root="$(git rev-parse --show-toplevel 2>/dev/null)" &&
  git_root="$(cd -P -- "$git_root" 2>/dev/null && pwd -P)" &&
  [ "$git_root" = "$(pwd -P)" ]; then git rev-parse --git-path crabbox; else printf %s .crabbox; fi)
case "$meta_dir" in /*) ;; *) meta_dir="$PWD/$meta_dir" ;; esac
`
}

func remoteSyncSanity(workdir string, allowMassDeletions bool) string {
	allowValue := ""
	if allowMassDeletions {
		allowValue = "1"
	}
	return "cd " + shellQuote(workdir) + " && " +
		"if test -d .git && git_status_output=$(git status --short 2>/dev/null); then " +
		"deletions=$(printf '%s\\n' \"$git_status_output\" | awk '/^ D|^D / { n++ } END { print n+0 }'); " +
		"if [ " + shellQuote(allowValue) + " != '1' ] && [ \"$deletions\" -ge 200 ]; then " +
		"echo \"remote sync sanity failed: $deletions tracked deletions\" >&2; " +
		"printf '%s\\n' \"$git_status_output\" | awk '/^ D|^D / { print \"  \" substr($0,4) }' | head -20 >&2; " +
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
