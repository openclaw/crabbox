//go:build windows

package cli

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsWebVNCDaemonProcessIdentity struct {
	pid     int
	started string
}

var errWindowsWebVNCDaemonTreeEmpty = errors.New("tracked supervisor process tree is no longer running")

func terminateWebVNCDaemonProcessTree(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("pid must be positive")
	}
	tree, err := windowsWebVNCDaemonProcessTree(pid)
	if err != nil {
		if errors.Is(err, errWindowsWebVNCDaemonTreeEmpty) {
			return nil
		}
		return fmt.Errorf("inventory WebVNC daemon tree %d: %w", pid, err)
	}
	rootKillErr := windowsTaskkillCommand(pid).Run()
	// taskkill cannot address an already-exited root even though its orphaned
	// descendants remain identifiable by their recorded parent PID. Target each
	// exact-start survivor as a tree root before deciding cleanup succeeded.
	fallbackErrors := terminateWindowsWebVNCDaemonSurvivors(tree)
	if survivors := windowsWebVNCDaemonSurvivors(tree); len(survivors) == 0 {
		return nil
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		survivors := windowsWebVNCDaemonSurvivors(tree)
		if len(survivors) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			detail := ""
			if rootKillErr != nil {
				detail += fmt.Sprintf("; root taskkill: %v", rootKillErr)
			}
			if len(fallbackErrors) != 0 {
				detail += "; descendant taskkill: " + strings.Join(fallbackErrors, "; ")
			}
			return fmt.Errorf("WebVNC daemon tree %d survived taskkill (survivors: %s)%s", pid, formatWindowsWebVNCPIDs(survivors), detail)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func terminateWindowsWebVNCDaemonSurvivors(tree []windowsWebVNCDaemonProcessIdentity) []string {
	errors := []string{}
	for _, identity := range tree {
		if !windowsWebVNCDaemonIdentityStillMatches(identity) {
			continue
		}
		if err := windowsTaskkillCommand(identity.pid).Run(); err != nil && windowsWebVNCDaemonIdentityStillMatches(identity) {
			errors = append(errors, fmt.Sprintf("pid %d: %v", identity.pid, err))
		}
	}
	return errors
}

func windowsWebVNCDaemonProcessTree(rootPID int) ([]windowsWebVNCDaemonProcessIdentity, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snapshot)
	parentByPID := map[int]int{}
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return nil, err
	}
	for {
		parentByPID[int(entry.ProcessID)] = int(entry.ParentProcessID)
		entry.Size = uint32(unsafe.Sizeof(entry))
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				return nil, err
			}
			break
		}
	}
	pids := windowsWebVNCDaemonTreePIDs(parentByPID, rootPID)
	identities := make([]windowsWebVNCDaemonProcessIdentity, 0, len(pids))
	for _, processID := range pids {
		started, err := webVNCDaemonProcessStartIdentity(processID)
		if err != nil {
			if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
				continue
			}
			return nil, fmt.Errorf("identify process %d: %w", processID, err)
		}
		identities = append(identities, windowsWebVNCDaemonProcessIdentity{pid: processID, started: started})
	}
	if len(identities) == 0 {
		return nil, errWindowsWebVNCDaemonTreeEmpty
	}
	return identities, nil
}

func windowsWebVNCDaemonTreePIDs(parentByPID map[int]int, rootPID int) []int {
	selected := map[int]struct{}{rootPID: {}}
	for changed := true; changed; {
		changed = false
		for processID, parentPID := range parentByPID {
			if _, alreadySelected := selected[processID]; alreadySelected {
				continue
			}
			if _, parentSelected := selected[parentPID]; parentSelected {
				selected[processID] = struct{}{}
				changed = true
			}
		}
	}
	pids := make([]int, 0, len(selected))
	for processID := range selected {
		pids = append(pids, processID)
	}
	sort.Ints(pids)
	return pids
}

func windowsWebVNCDaemonSurvivors(tree []windowsWebVNCDaemonProcessIdentity) []int {
	survivors := make([]int, 0, len(tree))
	for _, identity := range tree {
		if windowsWebVNCDaemonIdentityStillMatches(identity) {
			survivors = append(survivors, identity.pid)
		}
	}
	return survivors
}

func windowsWebVNCDaemonIdentityStillMatches(identity windowsWebVNCDaemonProcessIdentity) bool {
	started, err := webVNCDaemonProcessStartIdentity(identity.pid)
	if err == nil {
		return started == identity.started
	}
	// Failure to prove absence is a live survivor for cleanup purposes.
	return !errors.Is(err, windows.ERROR_INVALID_PARAMETER)
}

func formatWindowsWebVNCPIDs(pids []int) string {
	values := make([]string, 0, len(pids))
	for _, pid := range pids {
		values = append(values, strconv.Itoa(pid))
	}
	return strings.Join(values, ",")
}

func webVNCDaemonProcessGroupAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	tree, err := windowsWebVNCDaemonProcessTree(pid)
	if errors.Is(err, errWindowsWebVNCDaemonTreeEmpty) {
		return false
	}
	return err != nil || len(tree) != 0
}
