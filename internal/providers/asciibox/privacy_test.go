package asciibox

import (
	"context"
	"flag"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestPrivacyFlags(t *testing.T) {
	cfg := testConfig()
	fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
	values := RegisterAsciiBoxProviderFlags(fs, cfg)
	if err := fs.Parse([]string{"--ascii-box-no-snapshots", "--ascii-box-visibility=private"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyAsciiBoxProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if !cfg.AsciiBox.NoSnapshots || cfg.AsciiBox.Visibility != "private" || cfg.AsciiBox.NoEnv {
		t.Fatalf("flags: %+v", cfg.AsciiBox)
	}
	for _, visibility := range []string{"", "private", "shared", "public"} {
		next := flag.NewFlagSet("fixture", flag.ContinueOnError)
		values := RegisterAsciiBoxProviderFlags(next, cfg)
		if err := next.Parse([]string{"--ascii-box-no-snapshots=false", "--ascii-box-visibility=" + visibility}); err != nil {
			t.Fatal(err)
		}
		err := ApplyAsciiBoxProviderFlags(&cfg, next, values)
		if (err != nil) != (visibility == "public") {
			t.Fatalf("visibility %q: %v", visibility, err)
		}
		if cfg.AsciiBox.NoSnapshots {
			t.Fatal("explicit false did not override config")
		}
	}
}

func TestPrivacyReachesNativeCLI(t *testing.T) {
	home := t.TempDir()
	runner := &fakeCommandRunner{configPath: home + "/config.json"}
	c := &client{apiKey: "synthetic-key", apiURL: "https://ascii.dev", cliPath: "box", org: "team_fixture", home: home, runner: runner}
	if _, err := c.CreateBox(t.Context(), createRequest{NoSnapshots: true, Visibility: "private"}); err != nil {
		t.Fatal(err)
	}
	if !containsCommand(runner.commands, "box --no-update --json --org team_fixture --api-url https://ascii.dev new --no-snapshots --visibility private") {
		t.Fatalf("native creation lost privacy: %v", runner.commands)
	}
}

func TestPrivacyRequiresLatestReadinessMetadata(t *testing.T) {
	home := t.TempDir()
	runner := &fakeCommandRunner{
		configPath:    home + "/config.json",
		newStdout:     `{"event":"created","id":"bx_2","state":"provisioning","snapshots":false,"visibility":"private"}`,
		infoResponses: []string{`{"box":{"id":"bx_2","state":"ready","sshEndpoint":"198.51.100.20:19036"}}`},
	}
	c := &client{apiKey: "synthetic-key", apiURL: "https://ascii.dev", cliPath: "box", home: home, runner: runner}
	box, err := c.CreateBox(context.Background(), createRequest{NoSnapshots: true, Visibility: "private"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.AsciiBox.NoSnapshots, cfg.AsciiBox.Visibility = true, "private"
	if err := validateBoxPrivacy(cfg, box); err == nil {
		t.Fatal("missing readiness metadata reused earlier creation privacy")
	}
}

func TestPrivacyAcquisitionAndReuse(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		for _, policy := range []string{"confirmed", "missing-snapshots", "snapshots-enabled", "missing-visibility", "visibility-mismatch"} {
			name := "ordinary/"
			if fixed {
				name = "fixed/"
			}
			t.Run(name+policy, func(t *testing.T) {
				b, f, req, _ := fixedBoxFixture(t)
				t.Setenv("BOX_ORG", "team_fixture")
				f.box = testBox()
				if !fixed {
					req.RequestedLeaseID = ""
					withFakeAPI(t, &f.fakeAPI)
				}
				b.cfg.AsciiBox.NoSnapshots = true
				b.cfg.AsciiBox.Visibility = "private"
				snapshots := policy == "snapshots-enabled"
				f.box.Snapshots = &snapshots
				if policy == "missing-snapshots" {
					f.box.Snapshots = nil
				}
				f.box.Visibility = "private"
				if policy == "missing-visibility" {
					f.box.Visibility = ""
				}
				if policy == "visibility-mismatch" {
					f.box.Visibility = "shared"
				}
				lease, err := b.Acquire(t.Context(), req)
				if policy != "confirmed" {
					if err == nil || !strings.Contains(err.Error(), "did not confirm") || len(f.prepareIDs) != 0 {
						t.Fatalf("unconfirmed privacy reached SSH: %v %v", f.prepareIDs, err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if !f.createReq.NoSnapshots || f.createReq.Visibility != "private" || f.createReq.NoEnv {
					t.Fatalf("request lost independent policy: %+v", f.createReq)
				}
				claim, err := core.ReadLeaseClaim(lease.LeaseID)
				if err != nil || claim.Labels[boxNoSnapshotsRequestLabel] != "true" || claim.Labels[boxVisibilityRequestLabel] != "private" {
					t.Fatalf("request receipt: %+v %v", claim.Labels, err)
				}
				f.prepareIDs = nil
				b.cfg.AsciiBox.Visibility = "shared"
				if fixed {
					if _, err := b.Acquire(t.Context(), req); err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
						t.Fatalf("fixed replay changed visibility: %v", err)
					}
				}
				if _, err := b.Resolve(t.Context(), core.ResolveRequest{ID: lease.LeaseID}); err == nil || !strings.Contains(err.Error(), "privacy creation policy") || len(f.prepareIDs) != 0 {
					t.Fatalf("reuse changed requested policy: %v %v", f.prepareIDs, err)
				}
				b.cfg.AsciiBox.Visibility = "private"
				f.box.Snapshots = nil
				if _, err := b.Resolve(t.Context(), core.ResolveRequest{ID: lease.LeaseID}); err == nil || len(f.prepareIDs) != 0 {
					t.Fatalf("reuse trusted old metadata: %v %v", f.prepareIDs, err)
				}
				if _, err := b.Status(t.Context(), core.StatusRequest{ID: lease.LeaseID}); err != nil {
					t.Fatal(err)
				}
				owned, err := b.Resolve(t.Context(), core.ResolveRequest{ID: lease.LeaseID, ReleaseOnly: true})
				if err != nil {
					t.Fatal(err)
				}
				if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: owned}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestVisibilityRejectsPersonalCreationBeforeAllocation(t *testing.T) {
	b, f, req, _ := fixedBoxFixture(t)
	b.cfg.AsciiBox.Visibility = "private"
	if _, err := b.Acquire(t.Context(), req); err == nil || !strings.Contains(err.Error(), "omit visibility") || len(f.keys) != 0 {
		t.Fatalf("personal visibility was not rejected before allocation: %v, creates=%d", err, len(f.keys))
	}
}
