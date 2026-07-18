package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	sshTunnelLoopbackHost = "127.0.0.1"
	sshTunnelReadyTimeout = 15 * time.Second
)

func (a App) tunnel(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("tunnel", a.Stderr)
	provider := fs.String("provider", defaults.Provider, providerHelpSSH())
	id := fs.String("id", "", "lease id or slug")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	localPort := fs.String("local-port", "", "local loopback port; omit or use 0 to choose an available port")
	providerFlags := registerProviderFlags(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	if strings.TrimSpace(*id) == "" || fs.NArg() != 1 {
		return exit(2, "usage: crabbox tunnel --id <lease-id-or-slug> [--local-port <port>] <remote-port>")
	}
	remotePort, err := parseTunnelPort(fs.Arg(0), "remote port", false)
	if err != nil {
		return err
	}
	requestedLocalPort, err := parseTunnelPort(*localPort, "local port", true)
	if err != nil {
		return err
	}
	cfg, err := loadSSHCommandConfig(fs, *provider, providerFlags, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: *id})
	if err != nil {
		return err
	}
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return err
	}
	lease, err := a.resolveSSHTransportLeaseTargetForRepo(ctx, &cfg, *id, false, *reclaim)
	if err != nil {
		return err
	}
	if err := a.claimAndTouchLeaseTarget(ctx, cfg, lease.Server, lease.SSH, lease.LeaseID, *reclaim); err != nil {
		return err
	}
	if err := a.probeSSHTransportLeaseAfterClaim(ctx, cfg, &lease, *reclaim); err != nil {
		return err
	}
	stopActivity := a.startInteractiveSSHLeaseActivity(ctx, cfg, lease)
	defer stopActivity()
	return runSSHLocalForward(ctx, lease.SSH, requestedLocalPort, remotePort, a.Stdout)
}

func parseTunnelPort(value, label string, allowAuto bool) (string, error) {
	value = strings.TrimSpace(value)
	if allowAuto && (value == "" || value == "0") {
		return "", nil
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return "", exit(2, "%s must be a TCP port in 1..65535", label)
	}
	return strconv.Itoa(port), nil
}

func runSSHLocalForward(ctx context.Context, target SSHTarget, requestedLocalPort, remotePort string, stdout anyWriter) (err error) {
	terminationCtx, stopTerminationSignals := pondMeshTerminationContext(ctx)
	defer stopTerminationSignals()
	ctx = terminationCtx

	reservation, err := reserveSSHLocalForwardPort(requestedLocalPort)
	if err != nil {
		return err
	}
	defer reservation.release()

	session, err := newSSHTransportSession(ctx, target, true)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, session.Close()) }()

	forwardCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	args := resolvedSSHTunnelArgs(session, reservation.port, remotePort)
	handle := pondMeshExecCommand(forwardCtx, target.ChildEnvDenylist, "ssh", args...)
	output := newSynchronizedTailBuffer(failureTailLines)
	if execHandle, ok := handle.(*pondMeshExecHandle); ok {
		execHandle.cmd.Stdout = output
		execHandle.cmd.Stderr = output
	}
	if err := handle.Start(); err != nil {
		return fmt.Errorf("start SSH local forward: %w", err)
	}
	type waitResult struct {
		err        error
		terminated bool
	}
	waited := make(chan waitResult, 1)
	go func() {
		waitErr := handle.Wait()
		waited <- waitResult{err: waitErr, terminated: handle.WasTerminatedByOurCancel()}
	}()

	stopAndWait := func() waitResult {
		cancel()
		return <-waited
	}
	deadline := time.NewTimer(sshTunnelReadyTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var readinessErr error
	for {
		select {
		case result := <-waited:
			return unexpectedSSHForwardExit(result, redactSSHTransportDiagnostic(target, output.String()))
		case <-terminationCtx.Done():
			result := stopAndWait()
			return cancelledSSHForwardResult(result)
		case <-deadline.C:
			result := stopAndWait()
			if cleanupErr := cancelledSSHForwardResult(result); cleanupErr != nil {
				return cleanupErr
			}
			detail := strings.TrimSpace(redactSSHTransportDiagnostic(target, output.String()))
			if detail != "" {
				return exit(5, "SSH tunnel did not become ready on %s:%s: %s", sshTunnelLoopbackHost, reservation.port, tailForError(detail))
			}
			return exit(5, "SSH tunnel did not become ready on %s:%s: %v", sshTunnelLoopbackHost, reservation.port, readinessErr)
		case <-ticker.C:
			ready, probeErr := sshLocalForwardReady(forwardCtx, reservation.port, handle.PID(), target.ChildEnvDenylist...)
			if !ready {
				readinessErr = probeErr
				continue
			}
			reservation.release()
			fmt.Fprintf(stdout, "http://%s:%s\n", sshTunnelLoopbackHost, reservation.port)
			goto ready
		}
	}

ready:
	select {
	case result := <-waited:
		return unexpectedSSHForwardExit(result, redactSSHTransportDiagnostic(target, output.String()))
	case <-terminationCtx.Done():
		result := stopAndWait()
		return cancelledSSHForwardResult(result)
	}
}

type synchronizedTailBuffer struct {
	mu   sync.Mutex
	tail *streamTailBuffer
}

func newSynchronizedTailBuffer(lines int) *synchronizedTailBuffer {
	return &synchronizedTailBuffer{tail: newStreamTailBuffer(lines)}
}

func (b *synchronizedTailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tail.Write(p)
}

func (b *synchronizedTailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Join(b.tail.Lines(), "\n")
}

func resolvedSSHTunnelArgs(session *sshTransportSession, localPort, remotePort string) []string {
	forward := net.JoinHostPort(sshTunnelLoopbackHost, localPort) + ":" + net.JoinHostPort(sshTunnelLoopbackHost, remotePort)
	args := append([]string{}, session.commandPrefix()...)
	return append(args, "-N", "-L", forward, session.host())
}

func sshLocalForwardReady(ctx context.Context, localPort string, processID int, denied ...string) (bool, error) {
	if ready, err := startedTunnelListenerReady(ctx, localPort, processID, denied...); !ready {
		return false, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(probeCtx, "tcp4", net.JoinHostPort(sshTunnelLoopbackHost, localPort))
	if err != nil {
		return false, fmt.Errorf("local listener is not accepting: %w", err)
	}
	_ = conn.Close()
	return true, nil
}

func unexpectedSSHForwardExit(result struct {
	err        error
	terminated bool
}, output string) error {
	if result.terminated {
		return nil
	}
	detail := strings.TrimSpace(output)
	if detail != "" {
		return exit(5, "SSH tunnel exited before cancellation: %s", tailForError(detail))
	}
	if result.err != nil {
		return fmt.Errorf("SSH tunnel exited before cancellation: %w", result.err)
	}
	return exit(5, "SSH tunnel exited before cancellation")
}

func cancelledSSHForwardResult(result struct {
	err        error
	terminated bool
}) error {
	if result.terminated {
		return nil
	}
	if result.err != nil {
		return result.err
	}
	return exit(5, "SSH tunnel exited unexpectedly during cancellation")
}

type sshLocalForwardPortReservation struct {
	port   string
	unlock func()
}

func reserveSSHLocalForwardPort(requested string) (*sshLocalForwardPortReservation, error) {
	if requested != "" {
		return reserveSpecificSSHLocalForwardPort(requested)
	}
	for attempt := 0; attempt < 32; attempt++ {
		listener, err := net.Listen("tcp4", net.JoinHostPort(sshTunnelLoopbackHost, "0"))
		if err != nil {
			return nil, fmt.Errorf("choose local SSH tunnel port: %w", err)
		}
		port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
		_ = listener.Close()
		reservation, err := reserveSpecificSSHLocalForwardPort(port)
		if err == nil {
			return reservation, nil
		}
	}
	return nil, exit(5, "no available IPv4 loopback port found for SSH tunnel")
}

func reserveSpecificSSHLocalForwardPort(port string) (*sshLocalForwardPortReservation, error) {
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return nil, exit(2, "local port must be a TCP port in 1..65535")
	}
	stateDir, err := crabboxStateDir()
	if err != nil {
		return nil, err
	}
	lockDir := filepath.Join(stateDir, "tunnel-ports")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, fmt.Errorf("create local tunnel port lock directory: %w", err)
	}
	if err := os.Chmod(lockDir, 0o700); err != nil {
		return nil, fmt.Errorf("secure local tunnel port lock directory: %w", err)
	}
	unlock, err := acquireDaemonFileLock(filepath.Join(lockDir, port+".lock"))
	if err != nil {
		return nil, fmt.Errorf("reserve local tunnel port %s: %w", port, err)
	}
	probe, err := net.Listen("tcp4", net.JoinHostPort(sshTunnelLoopbackHost, port))
	if err != nil {
		unlock()
		return nil, exit(5, "local tunnel port %s is already in use", port)
	}
	_ = probe.Close()
	return &sshLocalForwardPortReservation{port: strconv.Itoa(portNumber), unlock: unlock}, nil
}

func (r *sshLocalForwardPortReservation) release() {
	if r == nil || r.unlock == nil {
		return
	}
	unlock := r.unlock
	r.unlock = nil
	unlock()
}
