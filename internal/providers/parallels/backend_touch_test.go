package parallels

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	shared "github.com/openclaw/crabbox/internal/providers/shared"
)

// parallelsTouchRunner fails every command so the test proves Touch resolves the
// lease host from its label instead of probing candidate hosts with prlctl.
type parallelsTouchRunner struct {
	calls int
}

func (r *parallelsTouchRunner) Run(context.Context, core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.calls++
	return core.LocalCommandResult{}, errors.New("unexpected command")
}

func TestParallelsTouchPersistsIdlePolicyOverride(t *testing.T) {
	override := 45 * time.Minute
	for _, tc := range []struct {
		name      string
		storedKey string
		override  *time.Duration
		want      string
	}{
		{"preserve", "idle_timeout_secs", nil, "1800"},
		{"replace", "idle_timeout_secs", &override, "2700"},
		{"preserve legacy", "idle_timeout", nil, "1800"},
		{"replace legacy", "idle_timeout", &override, "2700"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", filepath.Join(root, "home"))
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
			t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
			leaseID := "cbx_touch_idle"
			server := core.Server{
				CloudID:  "vm-touch",
				Provider: "parallels",
				Name:     "crabbox-cbx-touch-idle-blue",
				Labels: map[string]string{
					"provider":   "parallels",
					"lease":      leaseID,
					"slug":       "blue",
					"host":       "local",
					tc.storedKey: "1800",
				},
			}
			before := maps.Clone(server.Labels)
			runner := &parallelsTouchRunner{}
			cfg := core.BaseConfig()
			cfg.Provider = "parallels"
			cfg.TargetOS = core.TargetLinux
			cfg.IdleTimeout = 5 * time.Minute
			backend := &leaseBackend{DirectSSHBackend: shared.DirectSSHBackend{Cfg: cfg, RT: core.Runtime{Exec: runner, Stderr: io.Discard}}}

			got, err := backend.Touch(context.Background(), core.TouchRequest{
				Lease:               core.LeaseTarget{LeaseID: leaseID, Server: server},
				State:               "ready",
				IdleTimeout:         cfg.IdleTimeout,
				IdleTimeoutOverride: tc.override,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got.Labels["idle_timeout_secs"] != tc.want || got.Labels["idle_timeout"] != tc.want {
				t.Fatalf("returned idle policy=%v, want both idle_timeout keys=%s", got.Labels, tc.want)
			}
			if !maps.Equal(persistedParallelsLeaseLabels(t, leaseID), got.Labels) {
				t.Fatalf("persisted=%v, want the returned labels %v", persistedParallelsLeaseLabels(t, leaseID), got.Labels)
			}
			if !maps.Equal(server.Labels, before) {
				t.Fatalf("touch mutated the input labels: %v, want %v", server.Labels, before)
			}
			if runner.calls != 0 {
				t.Fatalf("runner calls=%d, want 0 for a labeled host", runner.calls)
			}
		})
	}
}

func persistedParallelsLeaseLabels(t *testing.T, leaseID string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "parallels", "leases", leaseID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var labels map[string]string
	if err := json.Unmarshal(data, &labels); err != nil {
		t.Fatal(err)
	}
	return labels
}
