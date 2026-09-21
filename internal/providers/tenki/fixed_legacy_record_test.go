package tenki

import (
	"bytes"
	"encoding/json"
	core "github.com/openclaw/crabbox/internal/cli"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixture was written by the pre-engine implementation against synthetic
// provider infrastructure. Reading a spent ID must not upgrade or reopen it.
func TestFixedLeaseLegacyWriterReceipt(t *testing.T) {
	b, _, req := newFixedTenkiTest(t)
	data, err := os.ReadFile("testdata/fixed-lease-v1-released.json")
	if err != nil {
		t.Fatal(err)
	}
	var legacy core.LeaseClaim
	if err := json.Unmarshal(data, &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.FixedCreateIntent == nil || legacy.FixedCreateIntent.Journal != nil || legacy.FixedCreateIntent.State != "released" {
		t.Fatal("fixture is not a pre-engine receipt")
	}
	dir, err := core.CrabboxStateDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "claims", legacy.LeaseID+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	req.RequestedLeaseID, req.RequestedSlug, req.Repo.Root = legacy.LeaseID, legacy.Slug, legacy.RepoRoot
	if _, err := b.Acquire(t.Context(), req); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("legacy receipt did not fence acquisition: %v", err)
	}
	lease := core.LeaseTarget{LeaseID: legacy.LeaseID, Server: core.Server{CloudID: legacy.CloudID, ImmutableID: legacy.CloudImmutableID, Labels: legacy.Labels}}
	if retained, err := b.RetainLeaseClaimAfterReleaseWithClaim(lease, legacy); err != nil || !retained {
		t.Fatalf("legacy terminal proof rejected: retained=%v err=%v", retained, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, after) {
		t.Fatal("read-only legacy receipt verification rewrote the record")
	}
}
