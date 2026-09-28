package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// Owner control must stay independent of workload sessions. Its own foreground
// master amortizes authentication without leaving a ControlPersist daemon behind.
func startWorkspaceOwnerControl(ctx context.Context, target SSHTarget) (SSHTarget, func() error, error) {
	noop := func() error { return nil }
	if runtime.GOOS == "windows" || target.TargetOS == targetWindows || target.NoControlMaster ||
		target.AuthSecret || target.AuthoritativeKnownHosts || target.SSHConfigProxy || target.SSHConfigFile != "" || target.ProxyCommand != "" {
		return target, noop, nil
	}
	if err := resolveSSHPortNoInput(ctx, &target, workspaceOwnerSSHConnectTimeoutOption, workspaceOwnerSSHConnectionAttemptsOption, io.Discard); err != nil {
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
	args := []string{"-o", "ControlMaster=yes", "-o", "ControlPersist=no", "-o", "ForkAfterAuthentication=no", "-S", controlPath, "-N"}
	args = append(args, session.commandPrefixWithOptions(workspaceOwnerSSHConnectTimeoutOption, workspaceOwnerSSHConnectionAttemptsOption)...)
	args = append(args, session.host())
	master := pondMeshExecCommand(masterCtx, target, directSSHExecutable(), args...)
	diagnostic := newSynchronizedBuffer(4096)
	master.cmd.Stderr = &diagnostic
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
	closeMaster := func() error {
		once.Do(func() {
			cancel()
			<-done
			if !master.WasTerminatedByOurCancel() {
				closeErr = waitErr
			}
			closeErr = errors.Join(closeErr, session.Close(), os.RemoveAll(dir))
		})
		return closeErr
	}
	readyCtx, readyCancel := context.WithTimeout(ctx, workspaceOwnerTransportCallBudget(sshWorkspaceOwnerTransport{target: target}))
	defer readyCancel()
	for {
		select {
		case <-done:
			return target, noop, errors.Join(fmt.Errorf("workspace owner SSH control connection exited: %s", redactSSHTransportDiagnostic(target, diagnostic.String())), closeMaster())
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
