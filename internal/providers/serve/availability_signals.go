package serve

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/localcontainer"
)

type hostSampler struct {
	exec        core.CommandRunner
	total, idle uint64
}

func signalCommand(ctx context.Context, exec core.CommandRunner, name string, args ...string) (string, bool) {
	if exec == nil {
		return "", false
	}
	r, err := exec.Run(ctx, core.LocalCommandRequest{Name: name, Args: args, MaxCapturedOutputBytes: 64 * 1024})
	return r.Stdout, err == nil && r.ExitCode == 0
}

var cpuUsagePattern = regexp.MustCompile(`CPU usage:\s*([0-9.]+)% user,\s*([0-9.]+)% sys,\s*([0-9.]+)% idle`)
var idlePattern = regexp.MustCompile(`"HIDIdleTime"\s*=\s*([0-9]+)`)

func parseHostCPU(output string) *float64 {
	matches := cpuUsagePattern.FindAllStringSubmatch(output, -1)
	// top's first sample is since boot, not an interval.
	if len(matches) < 2 {
		return nil
	}
	last := matches[len(matches)-1]
	user, e1 := strconv.ParseFloat(last[1], 64)
	system, e2 := strconv.ParseFloat(last[2], 64)
	if e1 != nil || e2 != nil {
		return nil
	}
	value := (user + system) / 100
	if value < 0 || value > 1 {
		return nil
	}
	return &value
}
func samplePower(ctx context.Context, exec core.CommandRunner) (string, bool, bool) {
	switch runtime.GOOS {
	case "darwin":
		out, ok := signalCommand(ctx, exec, "pmset", "-g", "batt")
		if !ok {
			return "unknown", true, false
		}
		laptop := strings.Contains(out, "InternalBattery")
		if strings.Contains(out, "'AC Power'") {
			return "ac", laptop, true
		}
		if strings.Contains(out, "'Battery Power'") {
			return "battery", true, true
		}
	case "linux":
		paths, err := filepath.Glob("/sys/class/power_supply/*/type")
		if err != nil {
			return "unknown", true, false
		}
		battery, ac, known := false, false, false
		for _, path := range paths {
			b, err := os.ReadFile(path)
			if err != nil {
				return "unknown", true, false
			}
			switch strings.TrimSpace(string(b)) {
			case "Battery":
				battery = true
			case "Mains", "USB", "USB_C":
				b, err = os.ReadFile(filepath.Join(filepath.Dir(path), "online"))
				if err == nil {
					known = true
					ac = ac || strings.TrimSpace(string(b)) == "1"
				}
			}
		}
		if !battery || ac {
			return "ac", battery, true
		}
		if known {
			return "battery", true, true
		}
	}
	return "unknown", true, false
}

type containerCPUStats interface {
	CPUCores(context.Context, []localcontainer.ServedContainer) (float64, error)
}

// A nil inventory is unknown; an empty non-nil inventory is a known idle host.
func (h *hostSampler) sample(ctx context.Context, inventory []localcontainer.ServedContainer, stats containerCPUStats, id string) availabilitySample {
	s := availabilitySample{memory: "unknown", power: "unknown"}
	s.power, _, _ = samplePower(ctx, h.exec)
	var cpu *float64
	switch runtime.GOOS {
	case "darwin":
		cpu = h.darwinCPU(ctx)
		s.memory = hostMemoryPressure()
		if out, ok := signalCommand(ctx, h.exec, "ioreg", "-r", "-c", "IOHIDSystem", "-d", "1"); ok {
			if match := idlePattern.FindStringSubmatch(out); len(match) == 2 {
				if n, err := strconv.ParseUint(match[1], 10, 64); err == nil {
					v := float64(n) / 1e9
					s.idle = &v
				}
			}
		}
	case "linux":
		if b, err := os.ReadFile("/proc/stat"); err == nil {
			fields := strings.Fields(strings.SplitN(string(b), "\n", 2)[0])
			var total, idle uint64
			good := len(fields) >= 5
			// guest and guest_nice are already counted in user and nice.
			for i := 1; i < len(fields) && i <= 8; i++ {
				n, err := strconv.ParseUint(fields[i], 10, 64)
				if err != nil {
					good = false
				}
				total += n
				if i == 4 || i == 5 {
					idle += n
				}
			}
			if good {
				cpu = h.cpuDelta(total, idle)
			}
		}
		s.memory = hostMemoryPressure()
	}
	if cpu != nil && inventory != nil {
		selected := []localcontainer.ServedContainer{}
		for _, c := range inventory {
			if owned(c, id) && c.Running {
				selected = append(selected, c)
			}
		}
		if len(selected) == 0 {
			s.cpu = cpu
		} else if stats != nil {
			if cores, err := stats.CPUCores(ctx, selected); err == nil {
				s.cpu = subtractOwnedCPU(*cpu, cores, runtime.NumCPU())
			}
		}
	}
	s.at = time.Now()
	return s
}

func memoryPressureFromAvailable(available, total uint64) string {
	if total == 0 || available > total {
		return "unknown"
	}
	fraction := float64(available) / float64(total)
	if math.IsNaN(fraction) {
		return "unknown"
	}
	if fraction < 0.05 {
		return "critical"
	}
	if fraction < 0.10 {
		return "warn"
	}
	return "normal"
}

func (h *hostSampler) cpuDelta(total, idle uint64) *float64 {
	previousTotal, previousIdle := h.total, h.idle
	h.total, h.idle = total, idle
	if previousTotal == 0 || total <= previousTotal || idle < previousIdle || idle-previousIdle > total-previousTotal {
		return nil
	}
	n := 1 - float64(idle-previousIdle)/float64(total-previousTotal)
	return &n
}
