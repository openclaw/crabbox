package serve

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/localcontainer"
)

func floatPtr(v float64) *float64 { return &v }
func idleSample(at time.Time) availabilitySample {
	return availabilitySample{at: at, cpu: floatPtr(.1), memory: "normal", idle: floatPtr(3600), power: "ac"}
}
func TestAvailabilityPolicy(t *testing.T) {
	tests := []struct {
		name          string
		laptop        bool
		change        func(*availabilityPolicy, *availabilitySample)
		state, reason string
	}{
		{"desktop", false, func(_ *availabilityPolicy, s *availabilitySample) { s.idle = nil }, "available", ""},
		{"laptop", true, func(_ *availabilityPolicy, _ *availabilitySample) {}, "available", ""},
		{"cpu threshold inclusive", false, func(_ *availabilityPolicy, s *availabilitySample) { s.cpu = floatPtr(.5) }, "available", ""},
		{"cpu busy", false, func(_ *availabilityPolicy, s *availabilitySample) { s.cpu = floatPtr(.51) }, "busy", "host CPU"},
		{"cpu unknown", false, func(_ *availabilityPolicy, s *availabilitySample) { s.cpu = nil }, "unknown", "host CPU unknown"},
		{"battery", true, func(_ *availabilityPolicy, s *availabilitySample) { s.power = "battery" }, "busy", "battery power"},
		{"desktop ignores power", false, func(_ *availabilityPolicy, s *availabilitySample) { s.power = "unknown" }, "available", ""},
		{"laptop power unknown", true, func(_ *availabilityPolicy, s *availabilitySample) { s.power = "unknown" }, "unknown", "power unknown"},
		{"AC opt out", true, func(p *availabilityPolicy, s *availabilitySample) { p.requireACPower = false; s.power = "battery" }, "available", ""},
		{"user active", true, func(_ *availabilityPolicy, s *availabilitySample) { s.idle = floatPtr(599) }, "busy", "user active"},
		{"idle boundary", true, func(_ *availabilityPolicy, s *availabilitySample) { s.idle = floatPtr(600) }, "available", ""},
		{"idle unknown", true, func(_ *availabilityPolicy, s *availabilitySample) { s.idle = nil }, "unknown", "user idle unknown"},
		{"desktop idle policy", false, func(p *availabilityPolicy, s *availabilitySample) {
			p.requireUserIdle = time.Minute
			s.idle = floatPtr(0)
		}, "busy", "user active"},
		{"warn allowed", false, func(_ *availabilityPolicy, s *availabilitySample) { s.memory = "warn" }, "available", ""},
		{"warn threshold", false, func(p *availabilityPolicy, s *availabilitySample) { p.maxMemoryPressure = "warn"; s.memory = "warn" }, "busy", "memory pressure"},
		{"critical", false, func(_ *availabilityPolicy, s *availabilitySample) { s.memory = "critical" }, "busy", "memory pressure"},
		{"memory unknown", false, func(_ *availabilityPolicy, s *availabilitySample) { s.memory = "unknown" }, "unknown", "memory pressure unknown"},
		{"always", true, func(p *availabilityPolicy, s *availabilitySample) {
			p.mode = "always"
			s.cpu = nil
			s.idle = nil
			s.memory = "unknown"
			s.power = "unknown"
		}, "available", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := defaultAvailabilityPolicy(tc.laptop)
			var w availabilityWindow
			var result availability
			for seconds := 0; seconds <= 480; seconds += 10 {
				s := idleSample(time.Unix(int64(seconds), 0))
				tc.change(&p, &s)
				result = w.evaluate(p, s)
			}
			if result.State != tc.state || (tc.reason != "" && !strings.Contains(strings.Join(result.Reasons, ","), tc.reason)) {
				t.Fatalf("got %+v; want %s %s", result, tc.state, tc.reason)
			}
		})
	}
}
func TestAvailabilityHysteresis(t *testing.T) {
	p := defaultAvailabilityPolicy(true)
	var w availabilityWindow
	tick := func(seconds int, change func(*availabilitySample)) availability {
		s := idleSample(time.Unix(int64(seconds), 0))
		if change != nil {
			change(&s)
		}
		return w.evaluate(p, s)
	}
	for s := 0; s < 360; s += 10 {
		if a := tick(s, nil); a.State == "available" {
			t.Fatalf("available before CPU warmup + five clear minutes: %d", s)
		}
	}
	if a := tick(360, nil); a.State != "available" {
		t.Fatal(a)
	}
	for s := 370; s < 430; s += 10 {
		a := tick(s, func(s *availabilitySample) { s.memory = "critical" })
		if a.State != "available" {
			t.Fatalf("busy before 60s: %d %+v", s, a)
		}
	}
	if a := tick(430, func(s *availabilitySample) { s.memory = "critical" }); a.State != "busy" {
		t.Fatal(a)
	}
	for s := 440; s < 740; s += 10 {
		if a := tick(s, nil); a.State == "available" {
			t.Fatalf("available before recovery: %d", s)
		}
	}
	if a := tick(740, nil); a.State != "available" {
		t.Fatal(a)
	}
	if a := tick(750, func(s *availabilitySample) { s.idle = floatPtr(0) }); a.State != "busy" {
		t.Fatalf("activity not immediate: %+v", a)
	}
	if a := tick(760, func(s *availabilitySample) { s.cpu = nil }); a.State == "available" {
		t.Fatal(a)
	}
	// A missing interval restarts the clear window; it cannot count stale idle time.
	if a := tick(1100, nil); a.State == "available" {
		t.Fatal(a)
	}
}
func TestAvailabilityCPUWindowAndSubtraction(t *testing.T) {
	for _, tc := range []struct {
		total, own float64
		cores      int
		want       float64
	}{{.8, 8, 16, .3}, {.3, 8, 16, 0}, {1, 0, 8, 1}} {
		v := subtractOwnedCPU(tc.total, tc.own, tc.cores)
		if v == nil || math.Abs(*v-tc.want) > 1e-9 {
			t.Fatalf("%+v: %v", tc, v)
		}
	}
	if subtractOwnedCPU(.5, math.NaN(), 4) != nil {
		t.Fatal("accepted unknown stats")
	}
	var w availabilityWindow
	p := defaultAvailabilityPolicy(false)
	for s := 0; s <= 60; s += 10 {
		sample := idleSample(time.Unix(int64(s), 0))
		sample.cpu = floatPtr(.2)
		w.evaluate(p, sample)
	}
	sample := idleSample(time.Unix(70, 0))
	sample.cpu = floatPtr(.8)
	a := w.evaluate(p, sample)
	if a.HostCPU == nil || math.Abs(*a.HostCPU-.3) > 1e-9 {
		t.Fatalf("not a rolling mean: %+v", a)
	}
	raw := "CPU usage: 99.0% user, 1.0% sys, 0.0% idle\nCPU usage: 10.0% user, 5.0% sys, 85.0% idle"
	if v := parseHostCPU(raw); v == nil || *v != .15 {
		t.Fatalf("top interval: %v", v)
	}
	if parseHostCPU(strings.Split(raw, "\n")[0]) != nil {
		t.Fatal("accepted since-boot sample")
	}
}
func TestAvailabilityHeartbeatEncoding(t *testing.T) {
	a := availability{State: "busy", Reasons: []string{"host CPU"}, HostCPU: floatPtr(.8), MemoryPressure: "normal", Power: "ac"}
	data, err := json.Marshal(machine{Availability: &a})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Availability availability `json:"availability"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Availability.State != "busy" || decoded.Availability.HostCPU == nil || *decoded.Availability.HostCPU != .8 {
		t.Fatalf("%s", data)
	}
	if !strings.Contains(string(data), `"userIdleSeconds":null`) {
		t.Fatalf("unknown encoded as idle: %s", data)
	}
}
func TestAvailabilityFlags(t *testing.T) {
	t.Setenv("CRABBOX_SERVE_MAX_HOST_CPU", "0.6")
	t.Setenv("CRABBOX_SERVE_REQUIRE_AC_POWER", "false")
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	p, err := availabilityFlags(fs, core.ServeAvailabilityConfig{MaxHostCPU: floatPtr(.4)}, true)
	if err != nil {
		t.Fatal(err)
	}
	if p.maxHostCPU != .6 || p.requireACPower || p.requireUserIdle != 10*time.Minute {
		t.Fatal(p)
	}
	if err := fs.Parse([]string{"--max-host-cpu", "0.7", "--require-user-idle", "0"}); err != nil {
		t.Fatal(err)
	}
	if p.maxHostCPU != .7 || p.requireUserIdle != 0 {
		t.Fatal(p)
	}
	if err := p.validate(); err != nil {
		t.Fatal(err)
	}
	for _, v := range []float64{-1, 1.1, math.NaN(), math.Inf(1)} {
		p.maxHostCPU = v
		if p.validate() == nil {
			t.Fatalf("accepted %v", v)
		}
	}
}
func TestAvailabilityStatusProbeValidation(t *testing.T) {
	err := (Provider{}).RunAgent(context.Background(), core.Config{}, core.Runtime{}, []string{"status", "--probe", "--probe-duration", "0"})
	if err == nil || !strings.Contains(err.Error(), "probe-duration") {
		t.Fatal(err)
	}
}

func TestAvailabilityAlwaysBeforeFirstSample(t *testing.T) {
	a := agent{opts: options{availability: availabilityPolicy{mode: "always"}}}
	got := a.currentAvailability()
	if got.State != "available" || got.MemoryPressure != "unknown" || got.Power != "unknown" || got.Reasons == nil {
		t.Fatalf("invalid startup heartbeat: %+v", got)
	}
}

func TestAgentUnavailableStillReconcilesAndPrepares(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	l := assignment{ID: "cbx_123456abcdef", MachineID: "m", CPUs: 1, MemoryMB: 1024, Image: "configured", State: "running", ExpiresAt: time.Now().Add(time.Hour), IdleDeadline: time.Now().Add(time.Hour).UnixMilli()}
	var report machine
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m machine
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			t.Error(err)
		}
		if !m.Draining {
			report = m
		}
		_ = json.NewEncoder(w).Encode(heartbeat{Assignments: []assignment{l}})
	}))
	defer server.Close()
	coord, err := core.NewCoordinatorClient(core.Config{Coordinator: server.URL, CoordToken: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	containers := &fakeContainers{items: []localcontainer.ServedContainer{{ID: "owned", LeaseID: l.ID, Running: true, Labels: map[string]string{"crabbox.serve": "true", "crabbox.serve.machine": "m"}}}}
	prepared := false
	a := agent{coord: coord, containers: containers, opts: options{stateDir: t.TempDir(), interval: time.Second}, state: localState{Machine: machine{MachineID: "m", CPUs: 2, MemoryMB: 2048, Image: l.Image}, Assignments: map[string]assignment{l.ID: l}}, rt: core.Runtime{Stderr: io.Discard}, startPreparation: func(context.Context) { prepared = true; cancel() }}
	if err := a.run(ctx); err != nil {
		t.Fatal(err)
	}
	if report.Availability == nil || report.Availability.State != "unknown" || report.FreeMemoryMB != 0 {
		t.Fatalf("unavailable heartbeat: %+v", report)
	}
	if !prepared || containers.refreshed != 1 || len(containers.removed) != 0 || len(report.Leases) != 1 {
		t.Fatalf("prepared=%t refreshed=%d removed=%v leases=%v", prepared, containers.refreshed, containers.removed, report.Leases)
	}
}
