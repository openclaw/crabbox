//go:build linux

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func controllerListenerOwnershipSupported() bool { return true }

func controllerVerifyDaemonOwnedListener(port string, supervisorPID int) error {
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return fmt.Errorf("invalid local listener port")
	}
	inodes, err := controllerLinuxLoopbackListenerInodes(portNumber)
	if err != nil {
		return err
	}
	owners, err := controllerLinuxSocketOwners(inodes)
	if err != nil {
		return err
	}
	if len(owners) == 0 {
		return fmt.Errorf("no process owns the loopback listener")
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
