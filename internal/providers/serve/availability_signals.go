package serve

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/localcontainer"
)

type hostSampler struct {
	exec        core.CommandRunner
	debug       io.Writer
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
	idle, e3 := strconv.ParseFloat(last[3], 64)
	if e1 != nil || e2 != nil || e3 != nil || user < 0 || user > 100 || system < 0 || system > 100 || idle < 0 || idle > 100 {
		return nil
	}
	// top can report user + system above 100% on saturated hosts. Derive
	// utilization from its interval idle percentage instead of rejecting it.
	value := (100 - idle) / 100
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

const (
	availabilitySampleInterval = 10 * time.Second
	availabilitySignalTimeout  = 45 * time.Second
	availabilitySampleMaxAge   = time.Minute
)

type containerInventory func(context.Context) ([]localcontainer.ServedContainer, error)

type hostSignals struct {
	cpu    func(context.Context) *float64
	memory func() string
	idle   func(context.Context) *float64
	power  func(context.Context) string
}

func (h *hostSampler) sample(ctx context.Context, inventory containerInventory, stats containerCPUStats, id string) availabilitySample {
	return h.sampleSignals(ctx, inventory, stats, id, hostSignals{
		cpu: h.hostCPU, memory: hostMemoryPressure, idle: h.userIdle,
		power: func(ctx context.Context) string {
			power, _, _ := samplePower(ctx, h.exec)
			return power
		},
	})
}

// Each signal gets its own deadline. In particular, slow runtime or top calls
// must not spend the deadline needed by an unrelated, healthy host signal.
func (h *hostSampler) sampleSignals(ctx context.Context, inventory containerInventory, stats containerCPUStats, id string, signals hostSignals) availabilitySample {
	s := availabilitySample{memory: "unknown", power: "unknown"}
	var cpu *float64
	var cores float64
	var wg sync.WaitGroup
	run := func(name string, read func(context.Context) bool) {
		wg.Go(func() {
			bounded, cancel := context.WithTimeout(ctx, availabilitySignalTimeout)
			defer cancel()
			started := time.Now()
			known := read(bounded)
			outcome := signalOutcome(bounded, nil)
			if outcome == "ok" && !known {
				outcome = "unknown"
			}
			debugAvailability(h.debug, name, outcome, started)
		})
	}
	run("host_cpu", func(ctx context.Context) bool { cpu = signals.cpu(ctx); return cpu != nil })
	run("memory_pressure", func(context.Context) bool { s.memory = signals.memory(); return s.memory != "unknown" })
	run("user_idle", func(ctx context.Context) bool { s.idle = signals.idle(ctx); return s.idle != nil })
	run("power", func(ctx context.Context) bool { s.power = signals.power(ctx); return s.power != "unknown" })
	run("container_cpu", func(ctx context.Context) bool {
		s.containerCPUUnknown = true
		started := time.Now()
		items, err := inventory(ctx)
		debugAvailability(h.debug, "container_inventory", signalOutcome(ctx, err), started)
		if err != nil || items == nil {
			return false
		}
		selected := []localcontainer.ServedContainer{}
		for _, c := range items {
			if owned(c, id) && c.Running {
				selected = append(selected, c)
			}
		}
		if len(selected) != 0 {
			if stats == nil {
				return false
			}
			started = time.Now()
			cores, err = stats.CPUCores(ctx, selected)
			debugAvailability(h.debug, "container_cpu_stats", signalOutcome(ctx, err), started)
			if err != nil || subtractOwnedCPU(0, cores, runtime.NumCPU()) == nil {
				return false
			}
		}
		s.containerCPUUnknown = false
		return true
	})
	wg.Wait()
	s.cpu = cpu
	if cpu != nil && !s.containerCPUUnknown {
		s.cpu = subtractOwnedCPU(*cpu, cores, runtime.NumCPU())
	}
	s.at = time.Now()
	return s
}

func (h *hostSampler) userIdle(ctx context.Context) *float64 {
	if runtime.GOOS != "darwin" {
		return nil
	}
	out, ok := signalCommand(ctx, h.exec, "ioreg", "-r", "-c", "IOHIDSystem", "-d", "1")
	if ok {
		if match := idlePattern.FindStringSubmatch(out); len(match) == 2 {
			if n, err := strconv.ParseUint(match[1], 10, 64); err == nil {
				v := float64(n) / 1e9
				return &v
			}
		}
	}
	return nil
}

func (h *hostSampler) hostCPU(ctx context.Context) *float64 {
	switch runtime.GOOS {
	case "darwin":
		return h.darwinCPU(ctx)
	case "linux":
		b, err := os.ReadFile("/proc/stat")
		if err != nil {
			return nil
		}
		fields := strings.Fields(strings.SplitN(string(b), "\n", 2)[0])
		if len(fields) < 5 {
			return nil
		}
		var total, idle uint64
		// guest and guest_nice are already counted in user and nice.
		for i := 1; i < len(fields) && i <= 8; i++ {
			n, err := strconv.ParseUint(fields[i], 10, 64)
			if err != nil {
				return nil
			}
			total += n
			if i == 4 || i == 5 {
				idle += n
			}
		}
		return h.cpuDelta(total, idle)
	}
	return nil
}

func knownInventory(items []localcontainer.ServedContainer) containerInventory {
	return func(context.Context) ([]localcontainer.ServedContainer, error) {
		if items == nil {
			return nil, fmt.Errorf("container inventory unavailable")
		}
		return items, nil
	}
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
