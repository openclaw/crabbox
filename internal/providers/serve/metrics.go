package serve

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (a *agent) sampleCapacity(ctx context.Context) {
	used := 0
	for _, l := range a.state.Assignments {
		used += l.MemoryMB
	}
	a.state.Machine.FreeMemoryMB = max(0, a.state.Machine.MemoryMB-used)
	if runtime.GOOS == "linux" {
		if data, err := os.ReadFile("/proc/loadavg"); err == nil {
			fields := strings.Fields(string(data))
			if len(fields) > 0 {
				if load, err := strconv.ParseFloat(fields[0], 64); err == nil {
					a.state.Machine.Load = load
				}
			}
		}
		if data, err := os.ReadFile("/proc/meminfo"); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				fields := strings.Fields(line)
				if len(fields) > 1 && fields[0] == "MemAvailable:" {
					if kb, err := strconv.Atoi(fields[1]); err == nil {
						a.state.Machine.FreeMemoryMB = kb / 1024
					}
				}
			}
		}
		return
	}
	if runtime.GOOS != "darwin" {
		return
	}
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	result, err := a.rt.Exec.Run(bounded, core.LocalCommandRequest{Name: "sysctl", Args: []string{"-n", "vm.loadavg"}, MaxCapturedOutputBytes: 1024})
	if err == nil {
		fields := strings.Fields(result.Stdout)
		if len(fields) > 1 {
			if load, err := strconv.ParseFloat(fields[1], 64); err == nil {
				a.state.Machine.Load = load
			}
		}
	}
	result, err = a.rt.Exec.Run(bounded, core.LocalCommandRequest{Name: "vm_stat", MaxCapturedOutputBytes: 8192})
	if err != nil {
		return
	}
	pageSize, pages := 4096, 0
	for i, line := range strings.Split(result.Stdout, "\n") {
		if i == 0 {
			fields := strings.Fields(line)
			for j, f := range fields {
				if f == "of" && j+1 < len(fields) {
					if n, err := strconv.Atoi(fields[j+1]); err == nil {
						pageSize = n
					}
				}
			}
			continue
		}
		if strings.HasPrefix(line, "Pages free:") || strings.HasPrefix(line, "Pages inactive:") || strings.HasPrefix(line, "Pages speculative:") {
			_, v, _ := strings.Cut(line, ":")
			if n, err := strconv.Atoi(strings.Trim(strings.TrimSpace(v), ".")); err == nil {
				pages += n
			}
		}
	}
	a.state.Machine.FreeMemoryMB = pages * pageSize / (1024 * 1024)
}
