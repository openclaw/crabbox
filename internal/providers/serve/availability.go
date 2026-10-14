package serve

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/localcontainer"
)

// Nil measurements are unknown, never an idle/zero observation.
type availability struct {
	State           string   `json:"state"`
	Reasons         []string `json:"reasons"`
	HostCPU         *float64 `json:"hostCPU"`
	MemoryPressure  string   `json:"memoryPressure"`
	UserIdleSeconds *float64 `json:"userIdleSeconds"`
	Power           string   `json:"power"`
}
type availabilityPolicy struct {
	mode              string
	maxHostCPU        float64
	requireUserIdle   time.Duration
	requireACPower    bool
	maxMemoryPressure string
	laptop            bool
}
type availabilitySample struct {
	at                  time.Time
	cpu                 *float64
	memory              string
	idle                *float64
	power               string
	containerCPUUnknown bool
}
type availabilityWindow struct {
	samples     []availabilitySample
	state       string
	busySince   map[string]time.Time
	clearSince  time.Time
	lastReasons []string
}

func defaultAvailabilityPolicy(laptop bool) availabilityPolicy {
	p := availabilityPolicy{mode: "auto", maxHostCPU: 0.5, maxMemoryPressure: "critical", laptop: laptop}
	if laptop {
		p.requireUserIdle = 10 * time.Minute
		p.requireACPower = true
	}
	return p
}
func availabilityFlags(fs *flag.FlagSet, cfg core.ServeAvailabilityConfig, laptop bool) (*availabilityPolicy, error) {
	p := defaultAvailabilityPolicy(laptop)
	if cfg.Mode != "" {
		p.mode = cfg.Mode
	}
	if cfg.MaxHostCPU != nil {
		p.maxHostCPU = *cfg.MaxHostCPU
	}
	if cfg.RequireUserIdle != "" {
		v, err := time.ParseDuration(cfg.RequireUserIdle)
		if err != nil {
			return nil, fmt.Errorf("serve.availability.requireUserIdle: %w", err)
		}
		p.requireUserIdle = v
	}
	if cfg.RequireACPower != nil {
		p.requireACPower = *cfg.RequireACPower
	}
	if cfg.MaxMemoryPressure != "" {
		p.maxMemoryPressure = cfg.MaxMemoryPressure
	}
	envFlags := []struct{ name, flag string }{
		{"CRABBOX_SERVE_AVAILABILITY", "availability"}, {"CRABBOX_SERVE_MAX_HOST_CPU", "max-host-cpu"},
		{"CRABBOX_SERVE_REQUIRE_USER_IDLE", "require-user-idle"}, {"CRABBOX_SERVE_REQUIRE_AC_POWER", "require-ac-power"},
		{"CRABBOX_SERVE_MAX_MEMORY_PRESSURE", "max-memory-pressure"},
	}
	fs.StringVar(&p.mode, "availability", p.mode, "Availability gating: auto or always")
	fs.Float64Var(&p.maxHostCPU, "max-host-cpu", p.maxHostCPU, "Maximum non-serve host CPU fraction (0-1)")
	fs.DurationVar(&p.requireUserIdle, "require-user-idle", p.requireUserIdle, "Required console idle time; 0 disables")
	fs.BoolVar(&p.requireACPower, "require-ac-power", p.requireACPower, "Require AC power on laptops")
	fs.StringVar(&p.maxMemoryPressure, "max-memory-pressure", p.maxMemoryPressure, "Busy at memory pressure: warn or critical")
	for _, e := range envFlags {
		if value, ok := os.LookupEnv(e.name); ok {
			if err := fs.Set(e.flag, value); err != nil {
				return nil, fmt.Errorf("%s: %w", e.name, err)
			}
		}
	}
	return &p, nil
}
func (p availabilityPolicy) validate() error {
	if (p.mode != "auto" && p.mode != "always") || math.IsNaN(p.maxHostCPU) || math.IsInf(p.maxHostCPU, 0) || p.maxHostCPU < 0 || p.maxHostCPU > 1 || p.requireUserIdle < 0 || (p.maxMemoryPressure != "warn" && p.maxMemoryPressure != "critical") {
		return fmt.Errorf("invalid serve availability policy: use auto|always, CPU 0-1, nonnegative idle duration and warn|critical memory pressure")
	}
	return nil
}
func subtractOwnedCPU(total, ownedCores float64, cores int) *float64 {
	if cores <= 0 || math.IsNaN(total) || math.IsInf(total, 0) || math.IsNaN(ownedCores) || math.IsInf(ownedCores, 0) || total < 0 || total > 1 || ownedCores < 0 {
		return nil
	}
	n := math.Max(0, total-ownedCores/float64(cores))
	return &n
}

func (w *availabilityWindow) evaluate(p availabilityPolicy, s availabilitySample) (result availability) {
	result = availability{State: "unknown", Reasons: []string{}, MemoryPressure: s.memory, UserIdleSeconds: s.idle, Power: s.power}
	defer func() {
		if s.containerCPUUnknown {
			result.Reasons = append(result.Reasons, "serve container CPU unknown")
		}
	}()
	if result.MemoryPressure == "" {
		result.MemoryPressure = "unknown"
	}
	if result.Power == "" {
		result.Power = "unknown"
	}
	// Retain one sample before the boundary to integrate a time-weighted minute.
	w.samples = append(w.samples, s)
	cutoff := s.at.Add(-time.Minute)
	for len(w.samples) > 1 && !w.samples[1].at.After(cutoff) {
		w.samples = w.samples[1:]
	}
	cpuKnown := len(w.samples) > 1 && !w.samples[0].at.After(cutoff)
	var weighted float64
	var duration time.Duration
	for i := 1; i < len(w.samples); i++ {
		prev, cur := w.samples[i-1], w.samples[i]
		start := prev.at
		if start.Before(cutoff) {
			start = cutoff
		}
		dt := cur.at.Sub(start)
		// Long sampling gaps must not convert stale measurements into idle time.
		if cur.cpu == nil || cur.at.Sub(prev.at) > availabilitySampleMaxAge {
			cpuKnown = false
		} else if dt > 0 {
			weighted += *cur.cpu * dt.Seconds()
			duration += dt
		}
	}
	if s.cpu == nil {
		cpuKnown = false
	}
	if cpuKnown && duration >= time.Minute {
		n := math.Max(0, math.Min(1, weighted/duration.Seconds()))
		result.HostCPU = &n
	}
	if p.mode == "always" {
		result.State = "available"
		w.state = "available"
		w.lastReasons = nil
		return result
	}
	var unknown, busy []string
	if result.HostCPU == nil {
		unknown = append(unknown, "host CPU unknown")
	} else if *result.HostCPU > p.maxHostCPU {
		busy = append(busy, "host CPU")
	}
	switch s.memory {
	case "critical":
		busy = append(busy, "memory pressure")
	case "warn":
		if p.maxMemoryPressure == "warn" {
			busy = append(busy, "memory pressure")
		}
	case "normal":
	default:
		unknown = append(unknown, "memory pressure unknown")
	}
	active := false
	if p.requireUserIdle > 0 {
		if s.idle == nil {
			unknown = append(unknown, "user idle unknown")
		} else if *s.idle < p.requireUserIdle.Seconds() {
			busy = append(busy, "user active")
			active = true
		}
	}
	if p.laptop && p.requireACPower {
		switch s.power {
		case "battery":
			busy = append(busy, "battery power")
		case "ac":
		default:
			unknown = append(unknown, "power unknown")
		}
	}
	if w.busySince == nil {
		w.busySince = map[string]time.Time{}
	}
	nextBusy := map[string]time.Time{}
	mature := active
	for _, reason := range busy {
		since, ok := w.busySince[reason]
		if !ok {
			since = s.at
		}
		nextBusy[reason] = since
		if s.at.Sub(since) >= time.Minute {
			mature = true
		}
	}
	w.busySince = nextBusy
	if len(unknown) > 0 || len(busy) > 0 {
		w.clearSince = time.Time{}
		if mature || w.state == "busy" {
			w.state = "busy"
		} else if len(unknown) > 0 || w.state != "available" {
			w.state = "unknown"
		}
		result.State = w.state
		result.Reasons = append(busy, unknown...)
		w.lastReasons = append([]string(nil), result.Reasons...)
		return result
	}
	if w.state == "available" {
		result.State = "available"
		return result
	}
	if w.clearSince.IsZero() {
		w.clearSince = s.at
	}
	if s.at.Sub(w.clearSince) >= 5*time.Minute {
		w.state = "available"
		w.lastReasons = nil
		result.State = "available"
		return result
	}
	if w.state == "busy" {
		result.State = "busy"
		result.Reasons = append(result.Reasons, w.lastReasons...)
	}
	result.Reasons = append(result.Reasons, "waiting for 5m clear")
	return result
}

// Sampling runs independently of heartbeat/deadline enforcement.
func (a *agent) monitorAvailability(ctx context.Context) {
	var window availabilityWindow
	sampler := hostSampler{exec: availabilityRunner(a.rt), debug: a.rt.Stderr}
	stats, _ := a.containers.(containerCPUStats)
	id := a.state.Machine.MachineID
	for {
		started := time.Now()
		// Reuse reconciliation's immutable inventory instead of concurrently
		// querying its runtime. A stale inventory must not imply an idle host.
		a.availabilityMu.Lock()
		inventory := a.availabilityInventory
		if time.Since(a.availabilityInventoryAt) > 30*time.Second {
			inventory = nil
		}
		a.availabilityMu.Unlock()
		sample := sampler.sample(ctx, knownInventory(inventory), stats, id)
		result := window.evaluate(a.opts.availability, sample)
		a.availabilityMu.Lock()
		a.availabilityResult = result
		a.availabilityAt = sample.at
		a.availabilityMu.Unlock()
		timer := time.NewTimer(availabilitySampleDelay(started, time.Now()))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
func (a *agent) currentAvailability() availability {
	a.availabilityMu.Lock()
	defer a.availabilityMu.Unlock()
	if a.opts.availability.mode == "always" {
		r := a.availabilityResult
		r.State = "available"
		if r.MemoryPressure == "" {
			r.MemoryPressure = "unknown"
		}
		if r.Power == "" {
			r.Power = "unknown"
		}
		if r.Reasons == nil {
			r.Reasons = []string{}
		}
		return r
	}
	if a.availabilityAt.IsZero() || time.Since(a.availabilityAt) > availabilitySampleMaxAge {
		return availability{State: "unknown", Reasons: []string{"availability samples stale"}, MemoryPressure: "unknown", Power: "unknown"}
	}
	return a.availabilityResult
}
func availabilityText(a *availability) string {
	if a == nil {
		return "available"
	}
	if len(a.Reasons) == 0 {
		return a.State
	}
	return a.State + " (" + strings.Join(a.Reasons, ", ") + ")"
}
func probeAvailability(ctx context.Context, cfg core.Config, rt core.Runtime, o options, duration time.Duration, asJSON bool) error {
	if duration < time.Minute || duration > time.Hour {
		return fmt.Errorf("--probe-duration must be between 1m and 1h")
	}
	saved, err := readState(o.stateDir)
	if err != nil {
		return err
	}
	cfg.LocalContainer.Runtime = o.runtime
	rt.Exec = availabilityRunner(rt)
	sampler := hostSampler{exec: rt.Exec, debug: rt.Stderr}
	containers := localcontainer.NewServedRuntime(cfg, rt)
	var window availabilityWindow
	end := time.Now().Add(duration)
	for {
		started := time.Now()
		sample := sampler.sample(ctx, containers.List, containers, saved.Machine.MachineID)
		result := window.evaluate(o.availability, sample)
		if !time.Now().Before(end) {
			if asJSON {
				return json.NewEncoder(rt.Stdout).Encode(result)
			}
			cpu := "unknown"
			if result.HostCPU != nil {
				cpu = strconv.FormatFloat(*result.HostCPU, 'f', 3, 64)
			}
			idle := "unknown"
			if result.UserIdleSeconds != nil {
				idle = strconv.FormatFloat(*result.UserIdleSeconds, 'f', 0, 64) + "s"
			}
			_, err = fmt.Fprintf(rt.Stdout, "availability=%s hostCPU=%s memoryPressure=%s userIdle=%s power=%s\n", availabilityText(&result), cpu, result.MemoryPressure, idle, result.Power)
			return err
		}
		timer := time.NewTimer(min(availabilitySampleDelay(started, time.Now()), time.Until(end)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// Slow samples already consumed the interval; do not add another ten seconds
// and turn a valid measurement into a gap in the rolling CPU window.
func availabilitySampleDelay(started, now time.Time) time.Duration {
	return max(0, started.Add(availabilitySampleInterval).Sub(now))
}
