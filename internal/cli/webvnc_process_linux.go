//go:build linux

package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const linuxBootIDPath = "/proc/sys/kernel/random/boot_id"

func processBootIdentity() (string, error) {
	data, err := os.ReadFile(linuxBootIDPath)
	if err != nil {
		return "", err
	}
	bootID := strings.ToLower(strings.TrimSpace(string(data)))
	if !validLinuxBootID(bootID) {
		return "", fmt.Errorf("invalid Linux boot ID %q", bootID)
	}
	return bootID, nil
}

func processBootIdentityRequired() bool {
	return true
}

func validPersistedProcessBootIdentity(value string) bool {
	return validLinuxBootID(strings.ToLower(strings.TrimSpace(value)))
}

func validLinuxBootID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, char := range value {
		switch index {
		case 8, 13, 18, 23:
			if char != '-' {
				return false
			}
		default:
			if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
				return false
			}
		}
	}
	return true
}

func webVNCDaemonProcessStartIdentity(pid int) (string, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", err
	}
	// The command name is parenthesized and may itself contain spaces or ')'.
	// Fields after the final ')' begin at field 3; starttime is field 22.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return "", fmt.Errorf("parse /proc/%d/stat command", pid)
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) <= 19 {
		return "", fmt.Errorf("parse /proc/%d/stat start time", pid)
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return "", fmt.Errorf("parse /proc/%d/stat start time: %w", pid, err)
	}
	return fields[19], nil
}
