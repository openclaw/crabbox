package asciibox

import (
	"flag"
	"fmt"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestNoEnvFlagSelection(t *testing.T) {
	for _, tc := range []struct {
		initial bool
		arg     string
		want    bool
	}{
		{false, "", false}, {true, "", true}, {false, "true", true}, {true, "false", false},
	} {
		t.Run(fmt.Sprintf("initial=%t/arg=%s", tc.initial, tc.arg), func(t *testing.T) {
			cfg := testConfig()
			cfg.AsciiBox.NoEnv = tc.initial
			fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
			values := RegisterAsciiBoxProviderFlags(fs, cfg)
			if tc.arg != "" {
				if err := fs.Set("ascii-box-no-env", tc.arg); err != nil {
					t.Fatal(err)
				}
			}
			if err := ApplyAsciiBoxProviderFlags(&cfg, fs, values); err != nil {
				t.Fatal(err)
			}
			if cfg.AsciiBox.NoEnv != tc.want {
				t.Fatalf("noEnv=%t want %t", cfg.AsciiBox.NoEnv, tc.want)
			}
		})
	}
}

func TestNoEnvReachesNativeCLI(t *testing.T) {
	home := t.TempDir()
	runner := &fakeCommandRunner{configPath: home + "/config.json"}
	c := &client{apiKey: "synthetic-key", apiURL: "https://ascii.dev", cliPath: "box", home: home, runner: runner}
	if _, err := c.CreateBox(t.Context(), createRequest{NoEnv: true}); err != nil {
		t.Fatal(err)
	}
	if !containsCommand(runner.commands, "box --no-update --json --org personal --api-url https://ascii.dev new --no-env") {
		t.Fatalf("native creation lost no-env: %v", runner.commands)
	}
}

func TestNoEnvOrdinaryReusePreservesInspectionAndCleanup(t *testing.T) {
	for _, tc := range []struct {
		receipt            string
		requested, allowed bool
	}{
		{"", false, true}, {"false", false, true}, {"true", true, true},
		{"", true, false}, {"false", true, false}, {"true", false, false},
		{"invalid", false, false}, {"invalid", true, false},
	} {
		t.Run(fmt.Sprintf("receipt=%s/requested=%t", tc.receipt, tc.requested), func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			f := &fakeAPI{box: testBox()}
			withFakeAPI(t, f)
			stubSSHWait(t)
			claim, err := publishBoxClaim(testConfig(), "cbx_123456789abc", "proof", t.TempDir(), f.box, true)
			if err != nil {
				t.Fatal(err)
			}
			if err := core.WithDurableLeaseClaimLock(claim.LeaseID, func(c *core.LeaseClaim, _ bool, persist func() error) error {
				delete(c.Labels, boxNoEnvLabel)
				if tc.receipt != "" {
					c.Labels[boxNoEnvLabel] = tc.receipt
				}
				return persist()
			}); err != nil {
				t.Fatal(err)
			}
			cfg := testConfig()
			cfg.AsciiBox.NoEnv = tc.requested
			b := NewBackend(Provider{}.Spec(), cfg, testRuntime()).(*backend)
			_, err = b.Resolve(t.Context(), core.ResolveRequest{ID: claim.LeaseID, Reclaim: true})
			if (err == nil) != tc.allowed {
				t.Fatalf("executable reuse: allowed=%t err=%v", tc.allowed, err)
			}
			if !tc.allowed && (len(f.prepareIDs) != 0 || !strings.Contains(err.Error(), "no-env")) {
				t.Fatalf("rejected reuse reached SSH: %v, %v", f.prepareIDs, err)
			}
			for _, readOnly := range []bool{false, true} {
				if _, err := b.Resolve(t.Context(), core.ResolveRequest{ID: claim.LeaseID, StatusOnly: true, NoLocalStateMutations: readOnly}); err != nil {
					t.Fatalf("status blocked: %v", err)
				}
			}
			if _, err := b.Status(t.Context(), core.StatusRequest{ID: claim.LeaseID}); err != nil {
				t.Fatalf("status blocked: %v", err)
			}
			lease, err := b.Resolve(t.Context(), core.ResolveRequest{ID: claim.LeaseID, ReleaseOnly: true})
			if err != nil {
				t.Fatalf("cleanup resolution blocked: %v", err)
			}
			if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
				t.Fatalf("cleanup blocked: %v", err)
			}
			if len(f.deletedIDs) != 1 {
				t.Fatalf("cleanup did not delete the owned box: %v", f.deletedIDs)
			}
		})
	}
}

func TestNoEnvFixedCreationAndReuse(t *testing.T) {
	b, f, req, _ := fixedBoxFixture(t)
	b.cfg.AsciiBox.NoEnv = true
	_, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || !f.createReq.NoEnv || claim.Labels[boxNoEnvLabel] != "true" {
		t.Fatalf("fixed creation lost policy: request=%+v claim=%+v err=%v", f.createReq, claim, err)
	}
	if _, err := b.Acquire(t.Context(), req); err != nil || len(f.keys) != 1 {
		t.Fatalf("matching fixed replay: %v calls=%d", err, len(f.keys))
	}
	b.cfg.AsciiBox.NoEnv = false
	f.prepareIDs = nil
	if _, err := b.Acquire(t.Context(), req); err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("fixed intent ignored noEnv: %v", err)
	}
	if _, err := b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID}); err == nil || len(f.prepareIDs) != 0 {
		t.Fatalf("fixed reuse reached SSH: %v, %v", f.prepareIDs, err)
	}
	if _, err := b.Status(t.Context(), core.StatusRequest{ID: req.RequestedLeaseID}); err != nil {
		t.Fatalf("fixed status blocked: %v", err)
	}
	if err := core.WithDurableLeaseClaimLock(claim.LeaseID, func(c *core.LeaseClaim, _ bool, persist func() error) error {
		delete(c.Labels, boxNoEnvLabel)
		return persist()
	}); err != nil {
		t.Fatal(err)
	}
	b.cfg.AsciiBox.NoEnv = true
	if _, err := b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID}); err == nil || len(f.prepareIDs) != 0 {
		t.Fatalf("unknown fixed receipt reached SSH: %v, %v", f.prepareIDs, err)
	}
	lease, err := b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatalf("fixed cleanup resolution blocked: %v", err)
	}
	if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatalf("fixed cleanup blocked: %v", err)
	}
	if len(f.deletedIDs) != 1 {
		t.Fatalf("fixed cleanup did not delete owned box: %v", f.deletedIDs)
	}
}
