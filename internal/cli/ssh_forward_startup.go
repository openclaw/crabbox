package cli

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type sshForwardWait struct {
	done chan struct{}
	err  error // Published by closing done; read only after done.
}

func startSSHForwardWait(wait func() error) *sshForwardWait {
	w := &sshForwardWait{done: make(chan struct{})}
	go func() {
		w.err = wait()
		close(w.done)
	}()
	return w
}

type sshForwardRoot struct {
	pid   int
	ports []string
	wait  *sshForwardWait
}

func sshForwardStartupError(ctx context.Context, w *sshForwardWait) error {
	// Preserve a natural failure even if cancellation arrives at the same time.
	select {
	case <-w.done:
		if w.err != nil {
			return fmt.Errorf("SSH forward exited during startup: %w", w.err)
		}
		return errors.New("SSH forward exited during startup")
	default:
		return context.Cause(ctx)
	}
}

// OpenSSH creates local listeners after authentication. Only the nonforking,
// nonmultiplexed root can establish this config-consumption barrier; a proxy
// descendant's listener is insufficient. Every group is rechecked on success.
func waitSSHForwardRoots(ctx context.Context, roots []sshForwardRoot, timeout time.Duration) error {
	if !controllerListenerOwnershipSupported() {
		return errors.New("SSH tunnel listener ownership verification is unsupported on this platform")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var listenerErr error
	for {
		ready := true
		for _, root := range roots {
			if err := sshForwardStartupError(ctx, root.wait); err != nil {
				return errors.Join(err, listenerErr)
			}
			for _, port := range root.ports {
				if err := sshForwardStartupError(ctx, root.wait); err != nil {
					return errors.Join(err, listenerErr)
				}
				if err := sshForwardRootListenerReady(port, root.pid); err != nil {
					listenerErr = err
					ready = false
				}
			}
		}
		for _, root := range roots {
			if err := sshForwardStartupError(ctx, root.wait); err != nil {
				return errors.Join(err, listenerErr)
			}
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.Join(context.Cause(ctx), listenerErr)
		case <-ticker.C:
		}
	}
}

func verifySSHForwardRootOwners(owners []int, pid int) error {
	if pid <= 0 || len(owners) == 0 {
		return errors.New("no tracked IPv4 loopback listener")
	}
	for _, owner := range owners {
		if owner != pid {
			return fmt.Errorf("loopback listener is owned by pid %d, not tracked SSH pid %d", owner, pid)
		}
	}
	return nil
}
