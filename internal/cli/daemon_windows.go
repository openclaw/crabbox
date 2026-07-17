//go:build windows

package cli

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func configureDaemonCommand(_ *exec.Cmd) {}

func stopDaemonProcess(process *os.Process, pid int) error {
	// taskkill /T terminates the full descendant tree; Process.Kill is the
	// portable fallback when taskkill is unavailable or the tree already moved.
	if err := windowsTaskkillCommand(pid).Run(); err == nil {
		return nil
	}
	return process.Kill()
}

func windowsTaskkillCommand(pid int) *exec.Cmd {
	return windowsTaskkillCommandWithEnvironment(pid, os.Environ())
}

func windowsTaskkillCommandWithEnvironment(pid int, environment []string) *exec.Cmd {
	cmd := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F")
	cmd.Env = windowsTaskkillEnvironment(environment)
	return cmd
}

func windowsTaskkillEnvironment(environment []string) []string {
	allowed := map[string]struct{}{
		"SYSTEMROOT": {},
		"WINDIR":     {},
		"COMSPEC":    {},
	}
	values := make(map[string]string, len(allowed))
	for _, entry := range environment {
		name, value, found := strings.Cut(entry, "=")
		name = strings.ToUpper(strings.TrimSpace(name))
		if !found || value == "" {
			continue
		}
		if _, ok := allowed[name]; ok {
			values[name] = value
		}
	}
	result := make([]string, 0, len(values))
	for _, name := range []string{"SYSTEMROOT", "WINDIR", "COMSPEC"} {
		if value, ok := values[name]; ok {
			result = append(result, name+"="+value)
		}
	}
	return result
}
