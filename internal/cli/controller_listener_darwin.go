//go:build darwin

package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func controllerListenerOwnershipSupported() bool { return true }

func controllerVerifyDaemonOwnedListener(port string, supervisorPID int) error {
	owners, err := controllerDarwinLoopbackListenerOwnerPIDs(port)
	if err != nil {
		return err
	}
	for _, pid := range owners {
		owned, err := controllerDarwinProcessDescendsFrom(pid, supervisorPID)
		if err != nil {
			return fmt.Errorf("verify listener process %d ancestry: %w", pid, err)
		}
		if !owned {
			return fmt.Errorf("listener process %d is outside WebVNC daemon process tree %d", pid, supervisorPID)
		}
	}
	return nil
}

func localWebVNCListenerIdentity(port string) (localWebVNCSourceIdentity, error) {
	owners, err := controllerDarwinLoopbackListenerOwnerPIDs(port)
	if err != nil {
		return localWebVNCSourceIdentity{}, err
	}
	pid, err := exactLocalWebVNCListenerOwnerPID(owners)
	if err != nil {
		return localWebVNCSourceIdentity{}, err
	}
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return localWebVNCSourceIdentity{}, fmt.Errorf("inspect macOS listener process owner: %w", err)
	}
	if info.Proc.P_pid != int32(pid) || info.Eproc.Ucred.Uid != uint32(os.Geteuid()) {
		return localWebVNCSourceIdentity{}, fmt.Errorf("IPv4 loopback listener process %d is not owned by the current user", pid)
	}
	started, err := webVNCDaemonProcessStartIdentity(pid)
	if err != nil {
		return localWebVNCSourceIdentity{}, fmt.Errorf("inspect listener process %d start identity: %w", pid, err)
	}
	return localWebVNCSourceIdentity{PID: pid, ProcessStarted: started}, nil
}

func controllerDarwinLoopbackListenerOwnerPIDs(port string) ([]int, error) {
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return nil, fmt.Errorf("invalid local listener port")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	path := "/usr/sbin/lsof"
	output, err := exec.CommandContext(ctx, path, "-nP", "-a", "-iTCP:"+port, "-sTCP:LISTEN", "-Fpn").Output()
	if err != nil {
		return nil, fmt.Errorf("inspect macOS listener ownership: %w", err)
	}
	owners := controllerDarwinLoopbackListenerOwners(string(output), port)
	if len(owners) == 0 {
		return nil, fmt.Errorf("no process owns the IPv4 loopback listener")
	}
	return owners, nil
}

func controllerDarwinLoopbackListenerOwners(output, port string) []int {
	owners := map[int]struct{}{}
	currentPID := 0
	want := "127.0.0.1:" + port
	for _, line := range strings.Split(output, "\n") {
		if value, ok := strings.CutPrefix(line, "p"); ok {
			currentPID, _ = strconv.Atoi(value)
			continue
		}
		if value, ok := strings.CutPrefix(line, "n"); ok && value == want && currentPID > 0 {
			owners[currentPID] = struct{}{}
		}
	}
	out := make([]int, 0, len(owners))
	for pid := range owners {
		out = append(out, pid)
	}
	return out
}

func controllerDarwinProcessDescendsFrom(pid, ancestor int) (bool, error) {
	seen := map[int]struct{}{}
	for pid > 0 {
		if pid == ancestor {
			return true, nil
		}
		if _, ok := seen[pid]; ok {
			return false, fmt.Errorf("process ancestry cycle")
		}
		seen[pid] = struct{}{}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		output, err := exec.CommandContext(ctx, "/bin/ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
		cancel()
		if err != nil {
			return false, err
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(output)))
		if err != nil {
			return false, fmt.Errorf("parse parent pid: %w", err)
		}
	}
	return false, nil
}
