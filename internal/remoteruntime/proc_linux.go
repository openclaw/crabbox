package remoteruntime

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// The guard must stay unreaped to reserve the group identity, so signal 0
// alone cannot tell us when the workload has finished responding to TERM.
func groupHasLiveMembers(root string, group int) (bool, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return false, err
	}
	guardSeen := false
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, entry.Name(), "stat"))
		if errors.Is(err, os.ErrNotExist) {
			continue // A process may exit between listing and reading it.
		}
		if err != nil {
			return false, err
		}
		// comm can contain spaces and parentheses; pgrp follows its last ')'.
		end := bytes.LastIndexByte(data, ')')
		if end < 0 {
			return false, fmt.Errorf("invalid process stat for %d", pid)
		}
		fields := bytes.Fields(data[end+1:])
		if len(fields) < 3 || len(fields[0]) != 1 {
			return false, fmt.Errorf("short process stat for %d", pid)
		}
		pgid, err := strconv.Atoi(string(fields[2]))
		if err != nil {
			return false, err
		}
		if pgid != group {
			continue
		}
		if pid == group {
			guardSeen = true
		} else if fields[0][0] != 'Z' && fields[0][0] != 'X' {
			return true, nil
		}
	}
	if !guardSeen {
		return false, errors.New("runtime guard not visible in process group")
	}
	return false, nil
}

func (g *commandGroup) awaitTermGrace(group int) {
	waitTermGrace(g.grace, func() bool {
		err := syscall.Kill(-group, 0)
		if errors.Is(err, syscall.ESRCH) {
			return false
		}
		if err == nil {
			if live, err := groupHasLiveMembers("/proc", group); err == nil && !live {
				return false
			}
		}
		// EPERM and incomplete /proc visibility cannot prove workload exit.
		return true
	}, time.Now, time.Sleep)
}

func waitTermGrace(grace time.Duration, live func() bool, now func() time.Time, pause func(time.Duration)) {
	deadline := now().Add(grace)
	for live() {
		remaining := deadline.Sub(now())
		if remaining <= 0 {
			return
		}
		pause(min(10*time.Millisecond, remaining))
	}
}
