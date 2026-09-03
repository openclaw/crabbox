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
	// Transport-only overrides can contain credentials; never serialize them.
	ChildEnv map[string]string `json:"-"`
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
	target := SSHTarget{
		User:             user,
		Host:             host,
		Key:              cfg.SSHKey,
		Port:             port,
		FallbackPorts:    cfg.SSHFallbackPorts,
		TargetOS:         cfg.TargetOS,
		WindowsMode:      cfg.WindowsMode,
		ChildEnvDenylist: externalDesktopChildEnvDenylist(cfg, cfg.TargetOS),
	}
	if provider, err := ProviderFor(cfg.Provider); err == nil {
		if configurer, ok := provider.(ProviderSSHTargetConfigurer); ok {
			configurer.ConfigureSSHTarget(&target, sshReadyCommand(target))
		}
	}
	return target
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
		if isWindowsWSL2Target(*target) {
			err := probeWSL2SSHReady(probeCtx, target, profile, stderr)
			if err == nil {
				return nil
			}
			if setupErr := workspaceOwnerReadinessError(err, phase); setupErr != nil {
				return setupErr
			}
			if IsWSLSFTPUnavailable(err) {
				return err
			}
			lastPorts = "wsl2"
			fmt.Fprintln(stderr, sshWaitProgressMessage(target, phase, "", "", lastPorts, time.Since(start), time.Until(deadline)))
		} else if target.SSHConfigProxy {
			err := probeProxySSHReady(probeCtx, target, profile)
			if err == nil {
				return nil
			}
			if setupErr := workspaceOwnerReadinessError(err, phase); setupErr != nil {
				return setupErr
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
				// Successful readiness also proves transport. Diagnose authentication
				// separately only when readiness fails, avoiding a healthy-path SSH call.
				err = runSSHQuietWithOptions(probeCtx, probe, sshReadyCommand(probe), profile.connectTimeout, profile.connectionAttempts)
				if err == nil {
					if target.Port != probe.Port {
						fmt.Fprintf(stderr, "using ssh port %s for %s (configured %s not ready)\n", probe.Port, target.Host, target.Port)
						target.Port = probe.Port
					}
					return nil
				}
				if setupErr := workspaceOwnerReadinessError(err, phase); setupErr != nil {
					return setupErr
				}
				if err := runSSHQuietWithOptions(probeCtx, probe, sshTransportProbeCommand(probe), profile.connectTimeout, profile.connectionAttempts); err != nil {
					if setupErr := workspaceOwnerReadinessError(err, phase); setupErr != nil {
						return setupErr
					}
					probes = append(probes, port+":tcp")
					continue
				}
				if transportPort == "" {
					transportPort = probe.Port
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
	if isWindowsWSL2Target(*target) {
		return probeWSL2SSHReady(ctx, target, profile, io.Discard) == nil
	}
	if target.SSHConfigProxy {
		return probeProxySSHReady(ctx, target, profile) == nil
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

func probeProxySSHReady(ctx context.Context, target *SSHTarget, profile sshReadinessProfile) error {
	var lastErr error
	for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
		candidate := *target
		candidate.Port, candidate.FallbackPorts = port, []string{}
		if lastErr = runSSHQuietWithOptions(ctx, candidate, sshReadyCommand(candidate), profile.connectTimeout, profile.connectionAttempts); lastErr != nil {
			var setupErr *workspaceOwnerSetupError
			if errors.As(lastErr, &setupErr) {
				return lastErr
			}
			continue
		}
		target.Port, target.FallbackPorts = port, []string{}
		return nil
	}
	return lastErr
}

func probeWSL2SSHReady(ctx context.Context, target *SSHTarget, profile sshReadinessProfile, stderr io.Writer) error {
	ports := sshPortCandidates(target.Port, target.FallbackPorts)
	type outcome struct {
		err         error
		missingSFTP bool
	}
	outcomes := make([]outcome, 0, len(ports))
	for _, port := range ports {
		probe := *target
		probe.Port, probe.FallbackPorts = port, []string{}
		run := func(remote string) error {
			args := sshArgsNoInputWithOptions(probe, wsl2ReadinessCommand(remote), profile.connectTimeout, profile.connectionAttempts)
			return runSSHCommand(sshCommandContext(ctx, probe, args...), io.Discard, io.Discard)
		}
		if err := run(sshTransportProbeCommand(probe)); err != nil {
			outcomes = append(outcomes, outcome{err: err})
			continue
		}
		if err := probeWSLSFTPSubsystem(ctx, probe, profile.connectTimeout, profile.connectionAttempts, stderr); err != nil {
			outcomes = append(outcomes, outcome{err: err, missingSFTP: IsWSLSFTPUnavailable(err)})
			continue
		}
		// Transport/SFTP probes stay lightweight. An owned readiness command
		// must pass the same staged witness setup as the subsequent workload.
		if workspaceOwnerFromContext(ctx) != nil {
			run = func(remote string) error {
				return runSSHQuietWithOptions(ctx, probe, remote, profile.connectTimeout, profile.connectionAttempts)
			}
		}
		if err := run(sshReadyCommand(probe)); err == nil {
			target.Port, target.FallbackPorts = port, []string{}
			return nil
		} else {
			var setupErr *workspaceOwnerSetupError
			if errors.As(err, &setupErr) {
				return err
			}
			outcomes = append(outcomes, outcome{err: err})
		}
	}
	allMissing := len(outcomes) == len(ports)
	var lastErr error
	for _, result := range outcomes {
		allMissing = allMissing && result.missingSFTP
		if !result.missingSFTP {
			lastErr = result.err
		}
	}
	if allMissing {
		return errWSLSFTPUnavailable
	}
	return lastErr
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

func wsl2ReadinessCommand(remote string) string {
	return powershellCommand(`$c=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('` + base64.StdEncoding.EncodeToString([]byte(remote)) + `'))
& wsl.exe --exec sh -lc $c
exit $LASTEXITCODE`)
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

// Port probes may retry before delivery; a delivered command must never replay.
func resolveSSHPortNoInput(ctx context.Context, target *SSHTarget, connectTimeout, connectionAttempts string, stderr io.Writer) error {
	ports := sshPortCandidates(target.Port, target.FallbackPorts)
	if len(ports) == 0 {
		ports = []string{"22"}
	}
	if len(ports) == 1 {
		target.Port, target.FallbackPorts = ports[0], []string{}
		return nil
	}
	probe := *target
	probe.FallbackPorts = []string{}
	var err error
	for index, port := range ports {
		probe.Port = port
		args := sshArgsNoInputWithOptions(probe, sshTransportProbeCommand(probe), connectTimeout, connectionAttempts)
		var diagnostic synchronizedBuffer
		err = runSSHCommand(sshCommandContext(ctx, probe, args...), io.Discard, &diagnostic)
		if err == nil {
			target.Port, target.FallbackPorts = port, []string{}
			return nil
		}
		if !shouldRetrySSHPort(err) || index == len(ports)-1 {
			if stderr != nil {
				_, _ = stderr.Write(diagnostic.Bytes())
			}
			return err
		}
	}
	return err
}

type sshTransportPreparation struct {
	command     string
	setupMarker string
	direct      io.ReadSeeker
	stage       *wslStageSpool
}

type sshCommandLimit struct {
	execution time.Duration
	control   bool
}

const sshMuxDescriptorFailure = "mux_client_request_session: send fds failed"

type sshMuxFailureDetector struct {
	log      []byte
	overflow bool
}

func (d *sshMuxFailureDetector) Write(data []byte) (int, error) {
	if len(d.log)+len(data) > 512 {
		d.overflow = true
	} else if !d.overflow {
		d.log = append(d.log, data...)
	}
	return len(data), nil
}

func (d *sshMuxFailureDetector) failed() bool {
	if d.overflow {
		return false
	}
	lines := strings.Split(strings.TrimSuffix(strings.ReplaceAll(string(d.log), "\r\n", "\n"), "\n"), "\n")
	// Even a local OpenSSH log can embed server-supplied disconnect text.
	// Require the complete two-line failure, with no surrounding log records.
	if len(lines) != 2 || lines[1] != sshMuxDescriptorFailure {
		return false
	}
	for fd := 0; fd < 3; fd++ {
		prefix := fmt.Sprintf("mm_send_fd: sendmsg(%d): ", fd)
		if strings.HasPrefix(lines[0], prefix) && len(lines[0]) > len(prefix) {
			return true
		}
	}
	return false
}

func prepareSSHTransport(target SSHTarget, command string, input io.ReadSeeker, size int64, limit sshCommandLimit) (sshTransportPreparation, error) {
	if size < 0 || limit.execution < 0 || limit.control && (int64(len(command))+size > sshControlMetadataLimit || limit.execution <= 0) {
		return sshTransportPreparation{}, errors.New("command exceeds finite transport limits")
	}
	if isWindowsWSL2Target(target) {
		spool, err := newWSLStageSpool(command, nil, input, size, limit)
		if err == nil && limit.control {
			if spool.size > sshControlMetadataLimit {
				_ = spool.close()
				return sshTransportPreparation{}, errors.New("control envelope exceeds its accounted transport budget")
			}
		}
		return sshTransportPreparation{stage: spool}, err
	}
	return sshTransportPreparation{command: wrapRemoteForTarget(target, command), direct: input}, nil
}

func (p *sshTransportPreparation) run(ctx context.Context, target *SSHTarget, connectTimeout, connectionAttempts string, stdout, stderr io.Writer) error {
	if p.stage != nil {
		p.stage.setupMarker = p.setupMarker
		return p.stage.run(ctx, target, connectTimeout, connectionAttempts, stdout, stderr)
	}
	if err := resolveSSHPortNoInput(ctx, target, connectTimeout, connectionAttempts, stderr); err != nil {
		return err
	}
	multiplexed := runtime.GOOS != "windows" && !target.AuthSecret && !target.NoControlMaster
	for attempt := 0; ; attempt++ {
		probe := *target
		if attempt == 2 {
			probe.NoControlMaster = true
		}
		muxFailure, err := p.runOnce(ctx, probe, connectTimeout, connectionAttempts, stdout, stderr, multiplexed && attempt < 2)
		if err == nil || ctx.Err() != nil || exitCode(err) != 255 || !muxFailure || attempt == 2 {
			return err
		}
	}
}

func (p *sshTransportPreparation) runOnce(ctx context.Context, target SSHTarget, connectTimeout, connectionAttempts string, stdout, stderr io.Writer, captureLocalDiagnostics bool) (muxFailure bool, err error) {
	args := sshArgsNoInputWithOptions(target, p.command, connectTimeout, connectionAttempts)
	if p.direct != nil {
		args = sshArgsWithOptions(target, p.command, connectTimeout, connectionAttempts)
	}
	cmd := sshCommandContext(ctx, target, args...)
	input, err := p.reset()
	if err != nil {
		return false, err
	}
	cmd.Stdin = input
	stdout, stderr, finish := workspaceOwnerSetupStreams(p.setupMarker, stdout, stderr)
	defer func() {
		err = finish(err)
		var setupErr *workspaceOwnerSetupError
		if errors.As(err, &setupErr) {
			muxFailure = false
		}
	}()
	if captureLocalDiagnostics {
		return runSSHCommandWithLocalDiagnostics(cmd, stdout, stderr)
	}
	return false, runSSHCommand(cmd, stdout, stderr)
}

func (p *sshTransportPreparation) reset() (io.Reader, error) {
	if p.direct != nil {
		if _, err := p.direct.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		return p.direct, nil
	}
	return nil, nil
}

func (p *sshTransportPreparation) close() error {
	var err error
	if p.stage != nil {
		err = errors.Join(err, p.stage.close())
	}
	return err
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

type sshPreparationError struct{ error }

func (e sshPreparationError) Unwrap() error { return e.error }

// executeSSH owns workspace preparation; executePreparedSSH is the lower,
// generic transport boundary and has no knowledge of workspace ownership.
func executeSSH(ctx context.Context, target *SSHTarget, remote string, input io.ReadSeeker, size int64, limit time.Duration, connectTimeout, attempts string, stdout, stderr io.Writer) (err error) {
	var inputSize *int64
	if input != nil {
		inputSize = &size
	}
	prepared, err := prepareWorkspaceOwnerRemote(ctx, *target, remote, inputSize)
	if err != nil {
		return sshPreparationError{err}
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, prepared.close(ctx, *target))
		}
	}()
	commandLimit := sshCommandLimit{execution: limit}
	transport, err := prepareSSHTransport(*target, prepared.command, input, size, commandLimit)
	if err != nil {
		return sshPreparationError{err}
	}
	transport.setupMarker = prepared.setupMarker
	return transport.execute(ctx, target, commandLimit, connectTimeout, attempts, stdout, stderr)
}

func executePreparedSSH(ctx context.Context, target *SSHTarget, command string, input io.ReadSeeker, size int64, limit sshCommandLimit, connectTimeout, attempts string, stdout, stderr io.Writer) (err error) {
	transport, err := prepareSSHTransport(*target, command, input, size, limit)
	if err != nil {
		return sshPreparationError{err}
	}
	return transport.execute(ctx, target, limit, connectTimeout, attempts, stdout, stderr)
}

func (transport *sshTransportPreparation) execute(ctx context.Context, target *SSHTarget, limit sshCommandLimit, connectTimeout, attempts string, stdout, stderr io.Writer) (err error) {
	defer func() { err = errors.Join(err, transport.close()) }()
	if limit.execution > 0 {
		// Only staged WSL transport has a size-dependent call budget.
		var bytes int64
		if transport.stage != nil {
			bytes = transport.stage.size
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, sshTransportCallBudget(*target, bytes, limit))
		defer cancel()
	}
	err = transport.run(ctx, target, connectTimeout, attempts, stdout, stderr)
	if err == nil {
		err = context.Cause(ctx)
	}
	return err
}

func runSSHQuietWithOptionsResolvePort(ctx context.Context, target *SSHTarget, remote, connectTimeout, connectionAttempts string) error {
	return executeSSH(ctx, target, remote, nil, 0, 0, connectTimeout, connectionAttempts, io.Discard, io.Discard)
}

func runSSHQuietWithRemoteWaitTimeout(ctx context.Context, target SSHTarget, remote string, waitTimeout time.Duration, connectTimeout, connectionAttempts string) error {
	return executeSSH(ctx, &target, remote, nil, 0, waitTimeout, connectTimeout, connectionAttempts, io.Discard, io.Discard)
}

func runSSHOutput(ctx context.Context, target SSHTarget, remote string) (string, error) {
	return runSSHOutputWithRemoteWaitTimeout(ctx, target, remote, 0, "10", "3")
}

func runSSHOutputWithRemoteWaitTimeout(ctx context.Context, target SSHTarget, remote string, waitTimeout time.Duration, connectTimeout, connectionAttempts string) (string, error) {
	var out bytes.Buffer
	err := executeSSH(ctx, &target, remote, nil, 0, waitTimeout, connectTimeout, connectionAttempts, &out, io.Discard)
	return strings.TrimSpace(out.String()), err
}

func runSSHCombinedOutput(ctx context.Context, target SSHTarget, remote string) (string, error) {
	return runSSHCombinedOutputLimit(ctx, target, remote, 0)
}

func runSSHCombinedOutputLimit(ctx context.Context, target SSHTarget, remote string, maxBytes int) (string, error) {
	out := synchronizedBuffer{limit: maxBytes}
	err := executeSSH(ctx, &target, remote, nil, 0, 0, "10", "3", &out, &out)
	return strings.TrimSpace(out.String()), err
}

var idempotentSSHRetryDelay = 2 * time.Second

func runIdempotentSSHCombinedOutput(ctx context.Context, target SSHTarget, remote string, retryDelay time.Duration) (string, error) {
	return runIdempotentSSHCombinedOutputLimit(ctx, target, remote, retryDelay, 0)
}

func runIdempotentSSHCombinedOutputLimit(ctx context.Context, target SSHTarget, remote string, retryDelay time.Duration, maxBytes int) (string, error) {
	var lastOut string
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		lastOut, lastErr = runSSHCombinedOutputLimit(ctx, target, remote, maxBytes)
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

func runWSL2ControlScriptCombinedOutput(ctx context.Context, target SSHTarget, remote string, waitTimeout time.Duration, connectTimeout, connectionAttempts string) (string, error) {
	var out synchronizedBuffer
	err := executeSSH(ctx, &target, remote, nil, 0, waitTimeout, connectTimeout, connectionAttempts, &out, &out)
	return strings.TrimSpace(out.String()), err
}

func runSSHInputQuiet(ctx context.Context, target SSHTarget, remote, input string) error {
	return runSSHInput(ctx, target, remote, strings.NewReader(input), io.Discard, io.Discard)
}

func runSSHInput(ctx context.Context, target SSHTarget, remote string, input io.Reader, stdout, stderr io.Writer) error {
	if input == nil {
		input = strings.NewReader("")
	}
	data, err := io.ReadAll(input)
	if err != nil {
		return err
	}
	return runSSHInputStream(ctx, target, remote, bytes.NewReader(data), stdout, stderr)
}

func runSSHInputStream(ctx context.Context, target SSHTarget, remote string, input io.ReadSeeker, stdout, stderr io.Writer) error {
	if input == nil {
		input = strings.NewReader("")
	}
	size, err := input.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return executeSSH(ctx, &target, remote, input, size, 0, "10", "3", stdout, stderr)
}

func runSSHStream(ctx context.Context, target SSHTarget, remote string, stdout, stderr io.Writer) int {
	code, _ := runSSHStreamResult(ctx, target, remote, stdout, stderr)
	return code
}

func runSSHStreamResult(ctx context.Context, target SSHTarget, remote string, stdout, stderr io.Writer) (code int, err error) {
	// Some streaming callers retain only the exit code. Report proven setup
	// failures here, once, without changing workload stderr or exit semantics.
	defer func() {
		var setupErr *workspaceOwnerSetupError
		if stderr == nil || !errors.As(err, &setupErr) {
			return
		}
		detail := setupErr.Error() + "\n"
		n, writeErr := io.WriteString(stderr, detail)
		if writeErr == nil && n < len(detail) {
			writeErr = io.ErrShortWrite
		}
		err = errors.Join(err, writeErr)
	}()
	err = executeSSH(ctx, &target, remote, nil, 0, 0, "10", "3", stdout, stderr)
	var preparation sshPreparationError
	if errors.As(err, &preparation) {
		return 7, err
	}
	return exitCode(err), err
}

func runSSHCommand(cmd *exec.Cmd, stdout, stderr io.Writer) error {
	return runCommandWithPlatformStreams(cmd, stdout, stderr)
}

func sameCommandStreamWriter(left, right io.Writer) bool {
	defer func() {
		_ = recover()
	}()
	return left == right
}

type synchronizedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *synchronizedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(data)
	if b.limit > 0 && len(data) > b.limit-b.buf.Len() {
		data = data[:b.limit-b.buf.Len()]
		b.truncated = true
	}
	_, _ = b.buf.Write(data)
	return n, nil
}

func (b *synchronizedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.truncated {
		return nil
	}
	return bytes.Clone(b.buf.Bytes())
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.truncated {
		return ""
	}
	return b.buf.String()
}

func (b *synchronizedBuffer) boundedString() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String(), b.truncated
}

type gitOriginDiagnosticsTruncatedError struct {
	err error
}

func (e *gitOriginDiagnosticsTruncatedError) Error() string {
	return e.err.Error()
}

func (e *gitOriginDiagnosticsTruncatedError) Unwrap() error {
	return e.err
}

func runIdempotentSSHGitOriginAttempt(ctx context.Context, target SSHTarget, remote string, retryDelay time.Duration) (string, error) {
	var (
		out       synchronizedBuffer
		lastErr   error
		truncated bool
	)
	for attempt := 0; attempt < 2; attempt++ {
		out = synchronizedBuffer{limit: gitSeedDiagnosticLimit}
		lastErr = executeSSH(ctx, &target, remote, nil, 0, 0, "10", "3", &out, &out)
		if lastErr == nil || !shouldRetrySSHPort(lastErr) || attempt == 1 {
			break
		}
		if err := sleepContext(ctx, retryDelay); err != nil {
			lastErr = err
			break
		}
	}
	output, truncated := out.boundedString()
	output = strings.TrimSpace(output)
	if truncated && lastErr != nil {
		lastErr = &gitOriginDiagnosticsTruncatedError{err: lastErr}
	}
	return output, lastErr
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
	var setupErr *workspaceOwnerSetupError
	if errors.As(err, &setupErr) {
		return false
	}
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

func rsync(ctx context.Context, target SSHTarget, src, dst string, excludes []string, stdout, stderr io.Writer, opts rsyncOptions) (err error) {
	opts = normalizeRsyncOptions(opts)
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}
	session, wslExe, mountRoot, err := newWorkspaceRsyncSession(ctx, target)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, session.Close()) }()
	args := []string{
		"-az",
		"-e", session.rsyncRemoteShellWithOptions("10", "3"),
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
	args = append(args, "--", ensureTrailingSlash(rsyncLocalPath(src)), session.host()+":"+dst+"/")
	handle, err := resolvedRsyncCommand(ctx, target, args, wslExe, mountRoot)
	if err != nil {
		return err
	}
	cmd := handle.cmd
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
		if err := owner.WaitForChild(rawCtx, owner.callTimeout()); err != nil {
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
	err = handle.Start()
	if err == nil {
		err = handle.Wait()
	}
	stopHeartbeat()
	if guardStarted {
		guardCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), owner.quiesceTimeout())
		rawGuardCtx := contextWithoutWorkspaceOwner(guardCtx)
		guardErr := runSSHQuiet(rawGuardCtx, target, owner.rsyncStopCommand())
		if guardErr == nil {
			guardErr = waitWorkspaceOwnerNoChild(rawGuardCtx, owner, owner.callTimeout())
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

// Win32-OpenSSH inherits an overlapped pipe. Use its asynchronous handle contract
// so a pending SSH write can complete before the server closes stdin at EOF.
func windowsPowerShellCopyExactInput(destination string, inputSize int64) string {
	// An empty frame must not bind stdin, which may already be at EOF.
	// Disable FileStream read-ahead so bytes after this frame remain on stdin.
	return `$remaining = [Int64]` + strconv.FormatInt(inputSize, 10) + `
if ($remaining -gt 0) {
	if (-not ("Cbx.SshStdin" -as [type])) {
		Add-Type -Name SshStdin -Namespace Cbx -MemberDefinition '[DllImport("kernel32.dll")]public static extern IntPtr GetStdHandle(int n);'
	}
	$stdinHandle = [Microsoft.Win32.SafeHandles.SafeFileHandle]::new([Cbx.SshStdin]::GetStdHandle(-10), $false)
	$stdin = $null
	try {
		$stdin = [IO.FileStream]::new($stdinHandle, [IO.FileAccess]::Read, 1, $true)
		$buffer = New-Object byte[] 65536
		while ($remaining -gt 0) {
			$readSize = [int][Math]::Min([Int64]$buffer.Length, $remaining)
			$read = $stdin.ReadAsync($buffer, 0, $readSize).GetAwaiter().GetResult()
			if ($read -le 0) { throw "SSH stdin ended before the framed payload" }
			` + destination + `.Write($buffer, 0, $read)
			$remaining -= $read
		}
	} finally {
		if ($null -ne $stdin) { $stdin.Dispose() }
		$stdinHandle.Dispose()
	}
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

// Login-shell logout hooks can overwrite a control script's exit status.
func remoteGitControlShellCommand(script string) string {
	return "/usr/bin/env BASH_ENV=/dev/null ENV=/dev/null /bin/bash --noprofile --norc -c " + shellQuote(script)
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
	return remoteGitControlShellCommand(script)
}

func remoteGitSeed(workdir string, plan gitCoherencePlan) string {
	if !plan.seedEnabled() {
		return "true"
	}
	parent := filepath.ToSlash(filepath.Dir(workdir))
	script := `set -e
printf 'crabbox-git-seed phase=prerequisite\n'
command -v git >/dev/null 2>&1 || exit 127
printf 'crabbox-git-seed phase=prepare\n'
workdir=` + shellQuote(workdir) + `
expected_origin=` + shellQuote(plan.RemoteURL) + `
expected_tree=` + shellQuote(plan.Tree) + `
` + remoteGitOriginTransportFunctions() + `
` + remoteGitWorkspaceFunctions() + `
if [ -d "$workdir" ]; then
  cd "$workdir"
  if usable_git_workspace; then
    printf 'crabbox-git-seed phase=origin\n'
    repair_origin
    exit 0
  fi
fi
mkdir -p ` + shellQuote(parent) + `
tmp="$(mktemp -d ` + shellQuote(parent+"/.seed.XXXXXX") + `)"
transport_error="$tmp.transport-error"
cleanup_seed() { rm -rf -- "$tmp"; rm -f -- "$transport_error"; }
trap cleanup_seed EXIT
printf 'crabbox-git-seed phase=clone\n'
if ! origin_git clone --quiet --filter=blob:none --no-checkout --single-branch --branch ` + shellQuote(plan.Branch) + ` "$expected_origin" "$tmp" >/dev/null 2>"$transport_error"; then
  cat "$transport_error" >&2
  exit ` + strconv.Itoa(gitOriginRuntimeFallbackExitCode) + `
fi
printf 'crabbox-git-seed phase=checkout\n'
git -C "$tmp" checkout --quiet --detach ` + shellQuote(plan.Target) + `
printf 'crabbox-git-seed phase=verify\n'
[ "$(git -C "$tmp" rev-parse --verify HEAD^{commit})" = ` + shellQuote(plan.Target) + ` ]
cd "$tmp"
usable_git_workspace
if [ -n "$expected_tree" ]; then
  [ "$(git write-tree)" = "$expected_tree" ]
fi
printf 'crabbox-git-seed phase=origin\n'
repair_origin
printf 'crabbox-git-seed phase=publish\n'
cd /
rm -rf -- "$workdir"
mv -- "$tmp" "$workdir"
rm -f -- "$transport_error"
trap - EXIT
`
	return remoteGitControlShellCommand(script)
}

func remoteGitOriginTransportFunctions() string {
	return `origin_git() {
  /usr/bin/env -i HOME=/nonexistent XDG_CONFIG_HOME=/nonexistent PATH=/usr/bin:/bin LANG=C LC_ALL=C \
    GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null GIT_TERMINAL_PROMPT=0 \
    GIT_ASKPASS=/bin/false SSH_ASKPASS=/bin/false GCM_INTERACTIVE=Never GIT_SSH_COMMAND=/bin/false \
    /usr/bin/git -c credential.helper= -c credential.interactive=never -c core.hooksPath=/dev/null \
      -c protocol.allow=never -c protocol.file.allow=always -c protocol.http.allow=always \
      -c protocol.https.allow=always -c protocol.ext.allow=never -c protocol.git.allow=never \
      -c protocol.ssh.allow=never "$@"
}
`
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
	return remoteGitControlShellCommand(script)
}

func remoteInvalidateSyncFingerprintForTarget(target SSHTarget, workdir string, plainManifest bool) string {
	if isWindowsNativeTarget(target) {
		return powershellCommand("exit 0")
	}
	metadataScript := remoteSyncMetaDirScript()
	shellCommand := func(script string) string { return "bash -lc " + shellQuote(script) }
	if plainManifest {
		metadataScript = remotePlainManifestGitFunction() + remotePlainManifestSyncMetaDirScript()
		shellCommand = remotePlainManifestShellCommand
	}
	script := `set -e
cd ` + shellQuote(workdir) + `
` + metadataScript + `
/bin/rm -f -- "$meta_dir/sync-fingerprint"`
	return shellCommand(script)
}

type remoteSyncFinalizeOptions struct {
	AllowMassDeletions bool
	HydrateGit         bool
	GitOverlay         bool
	PlainManifest      bool
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
	return remoteWriteSyncManifestsNewWithMetadataMode(workdir, finalizeToken, remoteSyncMetaDirScript(), false)
}

func remoteWriteSyncManifestsNewMode(workdir, finalizeToken string, plainManifest bool) string {
	if !plainManifest {
		return remoteWriteSyncManifestsNew(workdir, finalizeToken)
	}
	return remoteWriteSyncManifestsNewWithMetadataMode(
		workdir,
		finalizeToken,
		remotePlainManifestGitFunction()+remotePlainManifestSyncMetaDirScript(),
		true,
	)
}

func remoteWriteSyncManifestsNewWithMetadata(workdir, finalizeToken, metadataScript string) string {
	return remoteWriteSyncManifestsNewWithMetadataMode(workdir, finalizeToken, metadataScript, true)
}

func remoteWriteSyncManifestsNewWithMetadataMode(workdir, finalizeToken, metadataScript string, hermetic bool) string {
	manifestName := remoteSyncPendingManifestName(finalizeToken)
	deletedName := remoteSyncPendingDeletedName(finalizeToken)
	if hermetic {
		metadataScript = gitOverlayHermeticFunctions() + metadataScript
	}
	script := "set -e\nmkdir -p " + shellQuote(workdir) + "\ncd " + shellQuote(workdir) + "\n" + metadataScript + `mkdir -p "$meta_dir"
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
	if hermetic {
		return remoteGitOverlayShellCommand(script)
	}
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
	return remoteWriteSyncManifestsNewForTargetMode(target, workdir, finalizeToken, false)
}

func remoteWriteSyncManifestsNewForTargetMode(target SSHTarget, workdir, finalizeToken string, plainManifest bool) string {
	if isWindowsWSL2Target(target) {
		return remoteWriteSyncManifestsNewPythonMode(workdir, finalizeToken, plainManifest)
	}
	return remoteWriteSyncManifestsNewMode(workdir, finalizeToken, plainManifest)
}

func remoteDiscardSyncPendingMetadata(workdir, finalizeToken string, plainManifest bool) string {
	metadataScript := remoteSyncMetaDirScript()
	shellCommand := remoteGitControlShellCommand
	if plainManifest {
		metadataScript = remotePlainManifestGitFunction() + remotePlainManifestSyncMetaDirScript()
		shellCommand = remotePlainManifestShellCommand
	}
	script := `set -e
cd ` + shellQuote(workdir) + `
` + metadataScript + `
/bin/rm -f -- "$meta_dir/` + remoteSyncPendingManifestName(finalizeToken) + `" "$meta_dir/` + remoteSyncPendingDeletedName(finalizeToken) + `"
`
	return shellCommand(script)
}

func remoteWriteSyncManifestsNewPython(workdir, finalizeToken string) string {
	return remoteWriteSyncManifestsNewPythonMode(workdir, finalizeToken, false)
}

func remoteWriteSyncManifestsNewPythonMode(workdir, finalizeToken string, plainManifest bool) string {
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
	mkdir, pythonCommand := "mkdir -p ", "python3 -c "
	metadataScript, cleanup := remoteSyncMetaDirScript(), remoteSyncAbandonedMetadataCleanup()
	shellCommand := func(script string) string { return "bash -lc " + shellQuote(script) }
	if plainManifest {
		mkdir, pythonCommand = "/bin/mkdir -p -- ", "/usr/bin/python3 -c "
		metadataScript = remotePlainManifestGitFunction() + remotePlainManifestSyncMetaDirScript()
		cleanup = remotePlainSyncAbandonedMetadataCleanup()
		shellCommand = remotePlainManifestShellCommand
	}
	script := "set -e\n" + mkdir + shellQuote(workdir) + "\ncd " + shellQuote(workdir) + "\n" + metadataScript + mkdir + "\"$meta_dir\"\n" +
		cleanup + "\n" +
		pythonCommand + shellQuote(python) + " \"$meta_dir/" + manifestName + "\" \"$meta_dir/" + deletedName + "\"\n"
	return shellCommand(script)
}

func remotePlainSyncAbandonedMetadataCleanup() string {
	return `/usr/bin/find "$meta_dir" -type f \( -name 'sync-manifest.new' -o -name 'sync-deleted.new' -o -name 'sync-manifest.*.new' -o -name 'sync-deleted.*.new' -o -name 'sync-manifest.*.sorted' -o -name 'sync-finalize-token.tmp.*' -o -name 'sync-finalize-complete-token.tmp.*' -o -name 'sync-git-status.*' \) -mtime +7 -exec /bin/rm -f -- {} \; 2>/dev/null || true`
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
	return remoteGitControlShellCommand(script)
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
	return remotePruneSyncManifestForTargetMode(target, workdir, finalizeToken, false)
}

func remotePruneSyncManifestForTargetMode(target SSHTarget, workdir, finalizeToken string, plainManifest bool, allowMassDeletions ...bool) string {
	if plainManifest {
		return remotePruneSafeSyncManifest(
			workdir,
			finalizeToken,
			remotePlainManifestGitFunction()+remotePlainManifestSyncMetaDirScript(),
			allowMassDeletions...,
		)
	}
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
	plainManifestRecovery := opts.PlainManifest && opts.Coherence.enabled()
	manifestName := remoteSyncPendingManifestName(opts.Token)
	deletedName := remoteSyncPendingDeletedName(opts.Token)
	gitFunctions := remoteGitWorkspaceFunctions()
	metadataScript := remoteSyncMetaDirScript()
	if opts.GitOverlay || plainManifestRecovery {
		gitFunctions = gitOverlayHermeticFunctions() + gitFunctions
		metadataScript = remotePlainSyncMetaDirScript()
	} else if opts.PlainManifest {
		gitFunctions = remotePlainManifestGitFunction()
		metadataScript = remotePlainManifestSyncMetaDirScript()
	}
	script := `set -e
cd ` + shellQuote(workdir) + `
` + gitFunctions + `
` + metadataScript + `
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
	if opts.GitOverlay {
		script += remoteGitOverlayFinalizeScript(opts.Coherence, allowValue)
	} else if plainManifestRecovery {
		script += remoteGitOverlayRecoveryFinalizeScript(opts.Coherence, opts.BaseRef, opts.BaseSHA, allowValue)
	} else if opts.PlainManifest {
		script += `publish_fingerprint=
git_root=
if git_root="$(plain_git rev-parse --show-toplevel 2>/dev/null)" &&
   git_root="$(cd -P -- "$git_root" 2>/dev/null && pwd -P)" &&
   [ "$git_root" = "$(pwd -P)" ] &&
   plain_git status --porcelain=v1 --untracked-files=normal >"$git_status" 2>/dev/null; then
  deletions=$(awk '/^ D|^D / { n++ } END { print n+0 }' "$git_status")
  if [ ` + shellQuote(allowValue) + ` != '1' ] && [ "$deletions" -ge 200 ]; then
    echo "remote sync sanity failed: $deletions tracked deletions" >&2
    awk '/^ D|^D / { print "  " substr($0,4) }' "$git_status" | head -20 >&2
    exit 66
  fi
fi
rm -f "$meta_dir/git-hydrate-base"
`
	} else if opts.Coherence.enabled() {
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
	if !opts.GitOverlay && !opts.PlainManifest && opts.HydrateGit && opts.BaseRef != "" {
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
	if (!opts.PlainManifest || plainManifestRecovery) && opts.BaseRef != "" && opts.BaseSHA != "" {
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
	if opts.GitOverlay || plainManifestRecovery {
		return remoteGitOverlayShellCommand(script)
	}
	if opts.PlainManifest {
		return remotePlainManifestShellCommand(script)
	}
	return remoteGitControlShellCommand(script)
}

func runRemoteFinalizeSync(ctx context.Context, target SSHTarget, workdir string, opts remoteSyncFinalizeOptions) (string, error, string, bool) {
	out, err := runIdempotentSSHGitOriginAttempt(ctx, target, remoteFinalizeSync(workdir, opts), idempotentSSHRetryDelay)
	reason, fallback := gitOriginRuntimeFallbackResult(opts.Coherence.RemoteURL, out, err)
	if !fallback {
		return out, err, "", false
	}
	opts.HydrateGit, opts.GitOverlay, opts.PlainManifest = false, false, true
	opts.Fingerprint, opts.Coherence = "", gitCoherencePlan{}
	out, err = runIdempotentSSHGitOriginAttempt(ctx, target, remoteFinalizeSync(workdir, opts), idempotentSSHRetryDelay)
	return out, err, reason, true
}

func remoteGitOverlayFinalizeScript(plan gitCoherencePlan, allowMassDeletions string) string {
	return `
if ! overlay_workspace_safe "$PWD" || ! exact_git_root; then
  echo "remote overlay finalize failed: unsafe Git workspace" >&2
  exit 67
fi
if [ "$(git remote get-url origin 2>/dev/null || true)" != ` + shellQuote(plan.RemoteURL) + ` ] ||
   [ "$(git rev-parse --verify HEAD^{commit} 2>/dev/null || true)" != ` + shellQuote(plan.Target) + ` ] ||
   [ "$(git write-tree 2>/dev/null || true)" != ` + shellQuote(plan.Tree) + ` ]; then
  echo "remote overlay finalize failed: Git identity changed" >&2
  exit 67
fi
git diff-files --name-status >"$git_status" 2>/dev/null || {
  echo "remote overlay finalize failed: Git status inspection failed" >&2
  exit 67
}
deletions=$(awk '$1 == "D" { n++ } END { print n+0 }' "$git_status")
if [ ` + shellQuote(allowMassDeletions) + ` != '1' ] && [ "$deletions" -ge 200 ]; then
  echo "remote sync sanity failed: $deletions tracked deletions" >&2
  exit 66
fi
`
}

func remoteGitOverlayRecoveryFinalizeScript(plan gitCoherencePlan, baseRef, baseSHA, allowMassDeletions string) string {
	if !plan.enabled() {
		return `echo "Git overlay recovery requires the original coherence plan" >&2; exit 67
`
	}
	script := `publish_fingerprint=
if ! overlay_workspace_safe "$PWD" || ! overlay_runtime_state_safe "$PWD" || ! exact_git_root; then
  echo "Git overlay recovery requires an isolated safe Git workspace" >&2
  exit 67
fi
if [ "$(git remote get-url origin 2>/dev/null || true)" != ` + shellQuote(plan.RemoteURL) + ` ]; then
  echo "Git overlay recovery requires the original safe Git origin" >&2
  exit 67
fi
`
	if baseRef != "" && baseSHA != "" {
		script += `if [ "$(git rev-parse --verify ` + shellQuote("refs/remotes/origin/"+baseRef+"^{commit}") + ` 2>/dev/null || true)" != ` + shellQuote(baseSHA) + ` ] ||
   ! git merge-base ` + shellQuote(baseSHA) + ` ` + shellQuote(plan.Target) + ` >/dev/null 2>&1; then
  echo "Git overlay recovery requires the planned base ref and history" >&2
  exit 67
fi
`
	}
	return script + remoteGitCoherenceFinalizeScript(plan, allowMassDeletions)
}

func remoteGitCoherenceFinalizeScript(plan gitCoherencePlan, allowMassDeletions string) string {
	return `
coherence_committed=; coherence_mutated=; head_changed=; index_changed=
tmp_ref="refs/crabbox/sync-$expected_token"; advertised_branch=` + shellQuote(plan.Branch) + `; expected_origin=` + shellQuote(plan.RemoteURL) + `
` + remoteGitOriginTransportFunctions() + `
if ! exact_git_root; then
	publish_fingerprint=
elif ! repair_origin; then
	echo "remote sync finalize failed: Git origin repair failed" >&2; cleanup_finalize_lock; exit 67
elif transport_error="$meta_dir/sync-fetch-error.$expected_token.$$"; ! origin_git fetch --quiet --no-tags "$expected_origin" "+refs/heads/$advertised_branch:$tmp_ref" 2>"$transport_error"; then
	git update-ref -d "$tmp_ref" >/dev/null 2>&1 || true
	echo "remote sync finalize failed: Git coherence fetch failed" >&2
	cat "$transport_error" >&2
	cleanup_finalize_lock
	exit ` + strconv.Itoa(gitOriginRuntimeFallbackExitCode) + `
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
  if [ -n "${transport_error:-}" ]; then
    rm -f -- "$transport_error"
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
	return remoteSyncMetaDirScriptWithGit("git")
}

func remoteSyncMetaDirScriptWithGit(gitCommand string) string {
	return `meta_dir=$(git_root=; if git_root="$(` + gitCommand + ` rev-parse --show-toplevel 2>/dev/null)" &&
  git_root="$(cd -P -- "$git_root" 2>/dev/null && pwd -P)" &&
  [ "$git_root" = "$(pwd -P)" ]; then ` + gitCommand + ` rev-parse --git-path crabbox; else printf %s .crabbox; fi)
case "$meta_dir" in /*) ;; *) meta_dir="$PWD/$meta_dir" ;; esac
`
}

func remotePlainManifestSyncMetaDirScript() string {
	return remoteSyncMetaDirScriptWithGit("plain_git")
}

func remotePlainManifestShellCommand(script string) string {
	return "/usr/bin/env -i PATH=/usr/bin:/bin LANG=C LC_ALL=C BASH_ENV=/dev/null ENV=/dev/null /bin/bash --noprofile --norc -c " + shellQuote(script)
}

func remotePlainManifestGitFunction() string {
	return `plain_git() {
  /usr/bin/env -i HOME=/nonexistent XDG_CONFIG_HOME=/nonexistent PATH=/usr/bin:/bin LANG=C LC_ALL=C \
    GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null GIT_ATTR_NOSYSTEM=1 \
    GIT_OPTIONAL_LOCKS=0 GIT_TERMINAL_PROMPT=0 GIT_ASKPASS=/bin/false SSH_ASKPASS=/bin/false GCM_INTERACTIVE=Never \
    /usr/bin/git -c credential.helper= -c core.fsmonitor=false -c core.hooksPath=/dev/null \
      -c core.attributesFile=/dev/null -c protocol.allow=never "$@"
}
`
}

func remotePlainSyncMetaDirScript() string {
	return `if ! overlay_workspace_safe "$PWD" || ! overlay_metadata_safe "$PWD"; then
  echo "Git overlay requires isolated, nonsymlink workspace metadata" >&2
  exit 67
fi
meta_dir="$PWD/.git/crabbox"
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
