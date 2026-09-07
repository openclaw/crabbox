//go:build !windows

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const webVNCDaemonCleanupSupported = true
const webVNCDaemonCleanupFDEnv = "CRABBOX_INTERNAL_WEBVNC_CLEANUP_FD"

var errWebVNCDaemonCleanupUnconfirmed = errors.New("owned WebVNC SSH tunnel cleanup is unconfirmed; retaining daemon identity")

type webVNCDaemonCleanupKey struct{}
type webVNCTunnelCleanupKey struct{}

type webVNCTunnelCleanup struct {
	mu     sync.Mutex
	active int
	failed bool
}

func retainWebVNCDaemonTerminationSignals() func() {
	// main unregisters its first-signal handler on cancellation. Keep TERM and
	// INT registered until cleanup finishes so repeated signals cannot skip it.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	return func() { signal.Stop(signals) }
}

func superviseWebVNCDaemonCleanup(ctx context.Context, nonce, workspaceID string, run func(context.Context) error) error {
	// The local daemon name may be a slug while the child resolves a canonical
	// lease ID. Only the launcher's local name addresses the durable identity.
	_, path, err := webVNCDaemonPaths(workspaceID)
	if err != nil {
		return err
	}
	identity, err := readWebVNCDaemonIdentity(path)
	if err != nil {
		return err
	}
	started, err := webVNCDaemonProcessStartIdentity(os.Getpid())
	command, alive := webVNCDaemonProcessCommand(os.Getpid())
	if err != nil || !alive || identity.PID != os.Getpid() || identity.Nonce != nonce || !identity.CleanupTracked ||
		identity.WorkspaceID != workspaceID || !webVNCDaemonIdentityMatchesProcess(identity, command, started) {
		return fmt.Errorf("WebVNC supervisor cleanup identity does not match its launch")
	}
	err = run(context.WithValue(ctx, webVNCDaemonCleanupKey{}, true))
	if ctx.Err() == nil || errors.Is(err, errWebVNCDaemonCleanupUnconfirmed) {
		return err
	}
	// The launch gate pins this identity before any child can run. A receipt is
	// published only after every launched child acknowledged completed cleanup.
	return errors.Join(err, writeWebVNCDaemonIdentity(path+".cleanup", identity))
}

func prepareWebVNCDaemonChildCleanup(ctx context.Context, cmd *exec.Cmd) (func(bool) error, error) {
	cmd.Env = childEnvironmentWithout(cmd.Environ(), webVNCDaemonCleanupFDEnv)
	if ctx.Value(webVNCDaemonCleanupKey{}) != true {
		return func(bool) error { return nil }, nil
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(errWebVNCDaemonCleanupUnconfirmed, err)
	}
	fd := 3 + len(cmd.ExtraFiles)
	cmd.ExtraFiles = append(cmd.ExtraFiles, writer)
	cmd.Env = append(cmd.Env, webVNCDaemonCleanupFDEnv+"="+strconv.Itoa(fd))
	return func(started bool) error {
		defer reader.Close()
		_ = writer.Close()
		if !started {
			return nil
		}
		// The child writes before exit. Bound a leaked descriptor rather than
		// interpreting EOF, forced exit, or a stuck descendant as success.
		if err := reader.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
			return errors.Join(errWebVNCDaemonCleanupUnconfirmed, err)
		}
		var receipt [1]byte
		if _, err := io.ReadFull(reader, receipt[:]); err != nil || receipt[0] != 1 {
			return errWebVNCDaemonCleanupUnconfirmed
		}
		return nil
	}, nil
}

func beginSupervisedWebVNCCleanup(ctx context.Context) (context.Context, func(), error) {
	raw := os.Getenv(webVNCDaemonCleanupFDEnv)
	if raw == "" {
		return ctx, func() {}, nil
	}
	fd, err := strconv.Atoi(raw)
	if err != nil || fd < 3 {
		return ctx, nil, fmt.Errorf("invalid WebVNC cleanup descriptor")
	}
	file := os.NewFile(uintptr(fd), "webvnc-child-cleanup")
	releaseSignals := retainWebVNCDaemonTerminationSignals()
	tracker := &webVNCTunnelCleanup{}
	return context.WithValue(ctx, webVNCTunnelCleanupKey{}, tracker), func() {
		defer releaseSignals()
		defer file.Close()
		tracker.mu.Lock()
		defer tracker.mu.Unlock()
		if tracker.active == 0 && !tracker.failed {
			_, _ = file.Write([]byte{1})
		}
	}, nil
}

func trackSupervisedWebVNCTunnel(ctx context.Context) func(error) {
	tracker, _ := ctx.Value(webVNCTunnelCleanupKey{}).(*webVNCTunnelCleanup)
	if tracker == nil {
		return func(error) {}
	}
	tracker.mu.Lock()
	tracker.active++
	tracker.mu.Unlock()
	return func(err error) {
		tracker.mu.Lock()
		defer tracker.mu.Unlock()
		tracker.active--
		tracker.failed = tracker.failed || err != nil
	}
}

func webVNCDaemonCleanupConfirmed(identity webVNCDaemonIdentity, path string) bool {
	receipt, err := readWebVNCDaemonIdentity(path + ".cleanup")
	return err == nil && receipt == identity
}
