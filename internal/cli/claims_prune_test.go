package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

type pruneTestProvider struct{ Provider }

func (pruneTestProvider) Spec() ProviderSpec {
	return ProviderSpec{Name: "bounded-prune-test", ClaimExpiryBound: time.Hour}
}

func setupPruneTest(t *testing.T) (time.Time, leaseClaim) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	RegisterProvider(pruneTestProvider{})
	t.Cleanup(func() { delete(providerRegistry, "bounded-prune-test") })
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	return now, leaseClaim{LeaseID: "cbx_000000000001", Provider: "bounded-prune-test", ClaimedAt: now.Add(-10 * 24 * time.Hour).Format(time.RFC3339), LastUsedAt: now.Add(-8 * 24 * time.Hour).Format(time.RFC3339), IdleTimeoutSeconds: 1800}
}

func TestClaimsPruneEligibility(t *testing.T) {
	cases := []struct {
		name                 string
		edit                 func(*leaseClaim)
		raw                  string
		dry, pruned, problem bool
	}{
		{name: "old bounded", pruned: true},
		{name: "claimedAt fallback", edit: func(c *leaseClaim) { c.LastUsedAt = "" }, pruned: true},
		{name: "fresh", edit: func(c *leaseClaim) { c.LastUsedAt = "2026-10-03T11:59:00Z" }},
		{name: "threshold equality", edit: func(c *leaseClaim) { c.LastUsedAt = "2026-09-26T12:00:00Z" }},
		{name: "static", edit: func(c *leaseClaim) { c.StaticHost = "host.example" }},
		{name: "local", edit: func(c *leaseClaim) { c.Provider = "local-container" }},
		{name: "vm", edit: func(c *leaseClaim) { c.Provider = "parallels" }},
		{name: "ssh", edit: func(c *leaseClaim) { c.Provider = "ssh" }},
		{name: "unknown", edit: func(c *leaseClaim) { c.Provider = "unknown" }},
		{name: "cloud mode unknown", edit: func(c *leaseClaim) { c.Provider = "aws" }},
		{name: "testbox settlement unknown", edit: func(c *leaseClaim) { c.Provider = "blacksmith-testbox" }},
		{name: "fixed intent", edit: func(c *leaseClaim) { c.FixedCreateIntent = &FixedCreateIntent{State: "pending"} }},
		{name: "fixed terminal receipt", edit: func(c *leaseClaim) { c.FixedCreateIntent = &FixedCreateIntent{State: "released"} }},
		{name: "pending registration", edit: func(c *leaseClaim) { c.RuntimeAdapterPendingRegistrationID = "pending" }},
		{name: "registration", edit: func(c *leaseClaim) { c.RuntimeAdapterRegistrationID = "active" }},
		{name: "coordinator", edit: func(c *leaseClaim) { c.CoordinatorRegistrationURL = "https://broker.example" }},
		{name: "checkpoint", edit: func(c *leaseClaim) { c.CheckpointCapture = &CheckpointCaptureBinding{ID: "checkpoint"} }},
		{name: "invalid time", edit: func(c *leaseClaim) { c.LastUsedAt = "not a time" }, problem: true},
		{name: "missing time", edit: func(c *leaseClaim) { c.LastUsedAt = ""; c.ClaimedAt = "" }, problem: true},
		{name: "unbounded invalid time", edit: func(c *leaseClaim) { c.Provider = "aws"; c.LastUsedAt = "not a time" }},
		{name: "invalid json", raw: "{", problem: true},
		{name: "dry run", dry: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now, claim := setupPruneTest(t)
			if tc.edit != nil {
				tc.edit(&claim)
			}
			writeClaimsListFixture(t, claim.LeaseID+".json", claim)
			path, _ := leaseClaimPath(claim.LeaseID)
			if tc.raw != "" {
				if err := os.WriteFile(path, []byte(tc.raw), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			// Sentinels outside claims must survive even successful pruning.
			dir, _ := CrabboxStateDir()
			for _, name := range []string{"keys/retained", "claim-locks/retained.lock", "coordinator/retained"} {
				path := filepath.Join(dir, name)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("retain"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			out, err := pruneLeaseClaims(t.Context(), now, claimsPruneAge, tc.dry, "", readLeaseClaimSnapshotWithPresence)
			if err != nil {
				t.Fatal(err)
			}
			if (len(out.Pruned) == 1) != tc.pruned || (len(out.Problems) > 0) != tc.problem {
				t.Fatalf("output=%+v", out)
			}
			after, err := os.ReadFile(path)
			if tc.pruned {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("claim still exists: %v", err)
				}
			} else if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("retained claim changed: %v", err)
			}
			for _, name := range []string{"keys/retained", "claim-locks/retained.lock", "coordinator/retained"} {
				data, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil || string(data) != "retain" {
					t.Fatalf("%s changed: %v", name, err)
				}
			}
		})
	}
}

func TestClaimsPruneRespectsRemoteBoundEvenWithShortThreshold(t *testing.T) {
	now, c := setupPruneTest(t)
	c.LastUsedAt = now.Add(-30 * time.Minute).Format(time.RFC3339)
	ok, err := claimPruneEligible(c, now, time.Second)
	if err != nil || ok {
		t.Fatalf("remote timeout has not elapsed: %v %v", ok, err)
	}
}

func TestClaimsPruneConcurrentModificationWins(t *testing.T) {
	now, c := setupPruneTest(t)
	writeClaimsListFixture(t, c.LeaseID+".json", c)
	reader := func(path, id string, info os.FileInfo) (leaseClaim, bool, error) {
		claim, exists, err := readLeaseClaimSnapshotWithPresence(path, id, info)
		if err != nil {
			return claim, exists, err
		}
		if err := mutateLeaseClaim(id, func(current *leaseClaim) error { current.LastUsedAt = now.Format(time.RFC3339); return nil }); err != nil {
			t.Fatal(err)
		}
		return claim, exists, nil
	}
	out, err := pruneLeaseClaims(t.Context(), now, claimsPruneAge, false, "", reader)
	if err != nil || len(out.Pruned) != 0 || out.Kept != 1 {
		t.Fatalf("output=%+v err=%v", out, err)
	}
	current, err := ReadLeaseClaim(c.LeaseID)
	if err != nil || current.LastUsedAt != now.Format(time.RFC3339) {
		t.Fatalf("new claim lost: %+v %v", current, err)
	}
}

func TestClaimsPruneLockWaitHonorsBudget(t *testing.T) {
	now, c := setupPruneTest(t)
	writeClaimsListFixture(t, c.LeaseID+".json", c)
	path, _ := leaseClaimPath(c.LeaseID)
	lockPath, err := leaseClaimLockPath(path)
	if err != nil {
		t.Fatal(err)
	}
	lock := flock.New(lockPath)
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	out, err := pruneLeaseClaims(ctx, now, claimsPruneAge, false, "", readLeaseClaimSnapshotWithPresence)
	if !errors.Is(err, context.DeadlineExceeded) || len(out.Pruned) != 0 {
		t.Fatalf("output=%+v err=%v", out, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestClaimsAutoPruneRateLimitAndResume(t *testing.T) {
	now, c := setupPruneTest(t)
	first := c
	first.LastUsedAt = now.Format(time.RFC3339)
	second := c
	second.LeaseID = "cbx_000000000002"
	writeClaimsListFixture(t, first.LeaseID+".json", first)
	writeClaimsListFixture(t, second.LeaseID+".json", second)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reads := 0
	reader := func(path, id string, info os.FileInfo) (leaseClaim, bool, error) {
		reads++
		if reads == 2 {
			cancel()
		}
		return readLeaseClaimSnapshotWithPresence(path, id, info)
	}
	if err := autoPruneLeaseClaims(ctx, now, reader); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	dir, _ := CrabboxStateDir()
	stamp := filepath.Join(dir, "claims-prune.stamp")
	if _, err := os.Stat(stamp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete pass wrote stamp: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "claims-prune.cursor"))
	if err != nil || string(data) != first.LeaseID+".json" {
		t.Fatalf("cursor=%q err=%v", data, err)
	}
	reads = 0
	reader = func(path, id string, info os.FileInfo) (leaseClaim, bool, error) {
		reads++
		return readLeaseClaimSnapshotWithPresence(path, id, info)
	}
	if err := autoPruneLeaseClaims(t.Context(), now, reader); err != nil {
		t.Fatal(err)
	}
	if reads != 1 {
		t.Fatalf("resume reread retained prefix: %d", reads)
	}
	data, err = os.ReadFile(stamp)
	if err != nil || string(data) != now.Format(time.RFC3339Nano) {
		t.Fatalf("stamp=%q err=%v", data, err)
	}
	info, err := os.Stat(stamp)
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
		t.Fatalf("stamp permissions: %v %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "claims-prune.cursor")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cursor not cleared: %v", err)
	}
	reads = 0
	if err := autoPruneLeaseClaims(t.Context(), now.Add(23*time.Hour), reader); err != nil {
		t.Fatal(err)
	}
	if reads != 0 {
		t.Fatalf("daily limit read claims: %d", reads)
	}
	if err := autoPruneLeaseClaims(t.Context(), now.Add(24*time.Hour), reader); err != nil {
		t.Fatal(err)
	}
	if reads != 1 {
		t.Fatalf("next day's pass missing: %d", reads)
	}
}

func TestClaimsAutoPruneDisabledAndExpiredBudget(t *testing.T) {
	now, _ := setupPruneTest(t)
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	cfg := baseConfig()
	cfg.ClaimsAutoPrune = false
	app.autoPruneClaims(t.Context(), cfg)
	dir, _ := CrabboxStateDir()
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disabled maintenance touched state: %v", err)
	}
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	reads := 0
	err := autoPruneLeaseClaims(ctx, now, func(string, string, os.FileInfo) (leaseClaim, bool, error) { reads++; return leaseClaim{}, false, nil })
	if !errors.Is(err, context.DeadlineExceeded) || reads != 0 {
		t.Fatalf("reads=%d err=%v", reads, err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired budget touched state: %v", err)
	}
}

func TestClaimsAutoPruneConfigPrecedence(t *testing.T) {
	clearConfigEnv(t)
	if !baseConfig().ClaimsAutoPrune {
		t.Fatal("default must enable maintenance")
	}
	config := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(config, []byte("claims:\n  autoPrune: false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", config)
	cfg, err := loadConfig()
	if err != nil || cfg.ClaimsAutoPrune {
		t.Fatalf("file: enabled=%v err=%v", cfg.ClaimsAutoPrune, err)
	}
	t.Setenv("CRABBOX_CLAIMS_AUTO_PRUNE", "true")
	cfg, err = loadConfig()
	if err != nil || !cfg.ClaimsAutoPrune {
		t.Fatalf("env true: enabled=%v err=%v", cfg.ClaimsAutoPrune, err)
	}
	t.Setenv("CRABBOX_CLAIMS_AUTO_PRUNE", "false")
	cfg, err = loadConfig()
	if err != nil || cfg.ClaimsAutoPrune {
		t.Fatalf("env false: enabled=%v err=%v", cfg.ClaimsAutoPrune, err)
	}
}

func TestClaimsPruneCommandJSON(t *testing.T) {
	_, c := setupPruneTest(t)
	c.LastUsedAt = "2000-01-01T00:00:00Z"
	writeClaimsListFixture(t, c.LeaseID+".json", c)
	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	for _, args := range [][]string{{"claims", "prune", "--dry-run", "--json"}, {"claims", "prune", "--older-than", "168h", "--json"}} {
		stdout.Reset()
		if err := app.Run(t.Context(), args); err != nil {
			t.Fatalf("%v: %v %s", args, err, stderr.String())
		}
		var out claimsPruneOutput
		if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Version != 1 || !reflect.DeepEqual(out.Eligible, []string{c.LeaseID}) {
			t.Fatalf("output=%+v", out)
		}
		if out.DryRun && len(out.Pruned) != 0 || !out.DryRun && len(out.Pruned) != 1 {
			t.Fatalf("output=%+v", out)
		}
	}
	for _, age := range []string{"0", "-1h", "0d", "invalid"} {
		if err := app.Run(t.Context(), []string{"claims", "prune", "--older-than", age}); err == nil {
			t.Fatalf("accepted %q", age)
		}
	}
}

func TestClaimsPruneSkipsClaimRemovedDuringScan(t *testing.T) {
	now, claim := setupPruneTest(t)
	writeClaimsListFixture(t, claim.LeaseID+".json", claim)
	// A concurrent stop removes the claim between the directory listing and the read.
	vanish := func(path, leaseID string, expected os.FileInfo) (leaseClaim, bool, error) {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		return readLeaseClaimSnapshotWithPresence(path, leaseID, expected)
	}
	out, err := pruneLeaseClaims(t.Context(), now, claimsPruneAge, false, "", vanish)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Problems) != 0 || len(out.Pruned) != 0 || len(out.Eligible) != 0 {
		t.Fatalf("vanished claim: problems=%v pruned=%v eligible=%v", out.Problems, out.Pruned, out.Eligible)
	}
}
