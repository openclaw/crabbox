//go:build linux

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func controllerListenerOwnershipSupported() bool { return true }

func controllerVerifyDaemonOwnedListener(port string, supervisorPID int) error {
	owners, err := controllerLinuxLoopbackListenerOwnerPIDs(port)
	if err != nil {
		return err
	}
	for _, pid := range owners {
		owned, err := controllerLinuxProcessDescendsFrom(pid, supervisorPID)
		if err != nil {
			return fmt.Errorf("verify listener process %d ancestry: %w", pid, err)
		}
		if !owned {
			return fmt.Errorf("listener process %d is outside WebVNC daemon process tree %d", pid, supervisorPID)
		}
	}
	return nil
}

func controllerVerifyDaemonOwnedListenerWithEnvironment(port string, supervisorPID int, _ []string) error {
	return controllerVerifyDaemonOwnedListener(port, supervisorPID)
}

func localWebVNCListenerIdentity(port string) (localWebVNCSourceIdentity, error) {
	owners, err := controllerLinuxLoopbackListenerOwnerPIDs(port)
	if err != nil {
		return localWebVNCSourceIdentity{}, err
	}
	pid, err := exactLocalWebVNCListenerOwnerPID(owners)
	if err != nil {
		return localWebVNCSourceIdentity{}, err
	}
	var stat unix.Stat_t
	if err := unix.Stat(filepath.Join("/proc", strconv.Itoa(pid)), &stat); err != nil {
		return localWebVNCSourceIdentity{}, fmt.Errorf("inspect Linux listener process owner: %w", err)
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return localWebVNCSourceIdentity{}, fmt.Errorf("IPv4 loopback listener process %d is not owned by the current user", pid)
	}
	started, err := webVNCDaemonProcessStartIdentity(pid)
	if err != nil {
		return localWebVNCSourceIdentity{}, fmt.Errorf("inspect listener process %d start identity: %w", pid, err)
	}
	return localWebVNCSourceIdentity{PID: pid, ProcessStarted: started}, nil
}

func controllerLinuxLoopbackListenerOwnerPIDs(port string) ([]int, error) {
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return nil, fmt.Errorf("invalid local listener port")
	}
	inodes, err := controllerLinuxLoopbackListenerInodes(portNumber)
	if err != nil {
		return nil, err
	}
	owners, err := controllerLinuxSocketOwners(inodes)
	if err != nil {
		return nil, err
	}
	if len(owners) == 0 {
		return nil, fmt.Errorf("no process owns the loopback listener")
	}
	return owners, nil
}

func controllerLinuxLoopbackListenerInodes(port int) (map[string]struct{}, error) {
	data, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		return nil, fmt.Errorf("read Linux TCP inventory: %w", err)
	}
	wantPort := fmt.Sprintf("%04X", port)
	inodes := map[string]struct{}{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 || fields[3] != "0A" {
			continue
		}
		address, socketPort, ok := strings.Cut(fields[1], ":")
		if !ok || address != "0100007F" || !strings.EqualFold(socketPort, wantPort) {
			continue
		}
		inodes[fields[9]] = struct{}{}
	}
	if len(inodes) == 0 {
		return nil, fmt.Errorf("no IPv4 loopback listener found on port %d", port)
	}
	return inodes, nil
}

func controllerLinuxSocketOwners(inodes map[string]struct{}) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("read Linux process inventory: %w", err)
	}
	owners := map[int]struct{}{}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 || !entry.IsDir() {
			continue
		}
		fds, err := os.ReadDir(filepath.Join("/proc", entry.Name(), "fd"))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join("/proc", entry.Name(), "fd", fd.Name()))
			if err != nil || !strings.HasPrefix(target, "socket:[") || !strings.HasSuffix(target, "]") {
				continue
			}
			inode := strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")
			if _, ok := inodes[inode]; ok {
				owners[pid] = struct{}{}
			}
		}
	}
	out := make([]int, 0, len(owners))
	for pid := range owners {
		out = append(out, pid)
	}
	return out, nil
}

func controllerLinuxProcessDescendsFrom(pid, ancestor int) (bool, error) {
	seen := map[int]struct{}{}
	for pid > 0 {
		if pid == ancestor {
			return true, nil
		}
		if _, ok := seen[pid]; ok {
			return false, fmt.Errorf("process ancestry cycle")
		}
		seen[pid] = struct{}{}
		data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if err != nil {
			return false, err
		}
		closing := strings.LastIndexByte(string(data), ')')
		if closing < 0 {
			return false, fmt.Errorf("malformed process stat")
		}
		fields := strings.Fields(string(data[closing+1:]))
		if len(fields) < 2 {
			return false, fmt.Errorf("malformed process stat")
		}
		pid, err = strconv.Atoi(fields[1])
		if err != nil {
			return false, fmt.Errorf("parse parent pid: %w", err)
		}
	}
	return false, nil
}
