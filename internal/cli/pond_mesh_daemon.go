package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
)

func startPondMeshDaemons(ctx context.Context, opts pondConnectOptions, pond string, members []pondMember, summary pondMeshSummary) (err error) {
	groups, err := pondMeshForwardGroups(members, summary.Forwards)
	if err != nil {
		return err
	}
	var handles []pondMeshHandle
	var roots []sshForwardRoot
	var sessions []*sshTransportSession
	defer func() {
		if err == nil {
			return
		}
		stopDaemonHandles(handles)
		for i, root := range roots {
			<-root.wait.done
			// Wait only reaps the leader for daemon handles.
			if treeErr := terminateWebVNCDaemonProcessTree(root.pid); treeErr != nil {
				err = errors.Join(err, treeErr)
				continue // Retain configs if descendant teardown could not be proved.
			}
			err = errors.Join(err, sessions[i].Close())
		}
		for _, session := range sessions[len(roots):] {
			err = errors.Join(err, session.Close())
		}
	}()
	private := false
	for _, group := range groups {
		args, session, prepareErr := pondMeshForwardInvocation(ctx, group)
		if prepareErr != nil {
			return prepareErr
		}
		sessions = append(sessions, session)
		private = private || session != nil
		handle := pondMeshRunnerCommand(ctx, pondMeshDaemonRunner{}, group.Target, directSSHExecutable(), args...)
		if err := handle.Start(); err != nil {
			return fmt.Errorf("start ssh forwards for %s: %w", pondMeshForwardGroupLabel(group.Forwards), err)
		}
		// Register exactly one waiter before preparing the next member, including
		// when that member's session or Start subsequently fails.
		wait := startSSHForwardWait(handle.Wait)
		handles = append(handles, handle)
		root := sshForwardRoot{pid: handle.PID(), wait: wait}
		for _, fwd := range group.Forwards {
			root.ports = append(root.ports, strconv.Itoa(fwd.LocalPort))
			fmt.Fprintf(opts.Stderr, "  -L 127.0.0.1:%d -> %s:%d\n", fwd.LocalPort, fwd.Peer, fwd.RemotePort)
		}
		roots = append(roots, root)
	}
	if private {
		// Pond retains its three connection attempts, each with a 10s timeout.
		if err := waitSSHForwardRoots(ctx, roots, 35*time.Second); err != nil {
			return err
		}
	} else {
		// Preserve ordinary export startup behavior; private sessions require the
		// stronger authentication barrier above, including every grouped forward.
		timer := time.NewTimer(200 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-timer.C:
		}
	}
	for _, root := range roots {
		if err := sshForwardStartupError(ctx, root.wait); err != nil {
			return err
		}
	}
	for _, session := range sessions {
		if err := session.Close(); err != nil {
			return err
		}
	}
	for _, root := range roots {
		if err := sshForwardStartupError(ctx, root.wait); err != nil {
			return err
		}
	}
	return writePondMeshDaemonState(opts.HomeDir, pond, summary, groups, handles)
}
