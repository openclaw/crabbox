package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"
)

// Owner control must stay independent of workload sessions. Its own foreground
// master amortizes authentication without leaving a ControlPersist daemon behind.
func startWorkspaceOwnerControl(ctx context.Context, target SSHTarget) (SSHTarget, func() error, error) {
	noop := func() error { return nil }
	relay := usesServeRelay(target) && serveRelayControlsFromContext(ctx) != nil
	if runtime.GOOS == "windows" || target.TargetOS == targetWindows ||
		target.AuthSecret || target.AuthoritativeKnownHosts || target.SSHConfigProxy || target.SSHConfigFile != "" ||
		(!relay && (target.NoControlMaster || target.ProxyCommand != "")) {
		return target, noop, nil
	}
	if relay {
		var err error
		target, err = prepareServeRelayControl(ctx, target)
		if err != nil {
			return target, noop, err
		}
		// Renewals and release must remain independent of workload sessions.
		target.ownerControlPath = ""
	}
	return startSSHControlMaster(ctx, target, workspaceOwnerSSHConnectTimeoutOption, workspaceOwnerSSHConnectionAttemptsOption)
}

func startSSHControlMaster(ctx context.Context, target SSHTarget, connectTimeout, attempts string) (SSHTarget, func() error, error) {
	noop := func() error { return nil }
	ctx = context.WithValue(ctx, serveRelayControlBypassKey{}, true)
	if err := resolveSSHPortNoInput(ctx, &target, connectTimeout, attempts, io.Discard); err != nil {
		return target, noop, err
	}
	session, err := newSSHTransportSession(ctx, target, false)
	if err != nil {
		return target, noop, err
	}
	// Darwin's Unix socket path limit is shorter than its default temp directory.
	dir, err := os.MkdirTemp("/tmp", "cbx-owner-")
	if err != nil {
		return target, noop, errors.Join(err, session.Close())
	}
	controlPath := filepath.Join(dir, "control")
	masterCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	args := []string{"-o", "ControlMaster=yes", "-o", "ControlPersist=no", "-S", controlPath, "-N"}
	args = append(args, session.commandPrefixWithOptions(connectTimeout, attempts)...)
	args = append(args, session.host())
	master := pondMeshExecCommand(masterCtx, target, directSSHExecutable(), args...)
	diagnostic := newSynchronizedBuffer(4096)
	master.cmd.Stderr = &diagnostic
	if os.Getenv("CRABBOX_SERVE_RELAY_DEBUG") == "1" {
		master.cmd.Stderr = io.MultiWriter(master.cmd.Stderr, &serveRelayDiagnosticForwarder{out: os.Stderr})
	}
	if err := master.Start(); err != nil {
		cancel()
		return target, noop, errors.Join(err, session.Close(), os.RemoveAll(dir))
	}
	done := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = master.Wait()
		close(done)
	}()
	var once sync.Once
	var closeErr error
	var cleanupErr error
	closeMaster := func() error {
		once.Do(func() {
			cancel()
			<-done
			if !master.WasTerminatedByOurCancel() {
				closeErr = waitErr
			}
			cleanupErr = errors.Join(session.Close(), os.RemoveAll(dir))
			closeErr = errors.Join(closeErr, cleanupErr)
		})
		return closeErr
	}
	budget := workspaceOwnerTransportCallBudget(sshWorkspaceOwnerTransport{target: target})
	if seconds, err := strconv.Atoi(connectTimeout); err == nil {
		budget = max(budget, time.Duration(seconds)*time.Second)
	}
	readyCtx, readyCancel := context.WithTimeout(ctx, budget)
	defer readyCancel()
	for {
		select {
		case <-done:
			_ = closeMaster()
			if ctx.Err() == nil && cleanupErr == nil {
				// Some SSH wrappers support commands but cannot keep a master alive.
				// No owner request ran yet; use direct control only after joining cleanup.
				target.NoControlMaster = true
				return target, noop, nil
			}
			return target, noop, errors.Join(fmt.Errorf("workspace owner SSH control connection exited: %s", redactSSHTransportDiagnostic(target, diagnostic.String())), closeErr, ctx.Err())
		default:
		}
		// OpenSSH publishes its control socket only after authenticating. Clients
		// never create replacement masters if this foreground process later exits.
		if info, err := os.Lstat(controlPath); err == nil && info.Mode()&os.ModeSocket != 0 {
			target.ownerControlPath = controlPath
			return target, closeMaster, nil
		}
		if err := sleepContext(readyCtx, 10*time.Millisecond); err != nil {
			return target, noop, errors.Join(err, closeMaster())
		}
	}
}
