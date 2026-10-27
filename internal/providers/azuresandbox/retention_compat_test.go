package azuresandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"testing/synctest"

	core "github.com/openclaw/crabbox/internal/cli"
)

// The fixtures are unmodified output from the v0.79.3 acquisition writer,
// running with its fake provider and synctest clock, not production user data.
func TestRetainedUpgradePreservesV0793EphemeralClaim(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fixture{}
		b, req := fixtureBackend(t, f)
		root := filepath.Join("testdata", "v0.79.3-ephemeral")
		read := func(name string) []byte {
			t.Helper()
			data, err := os.ReadFile(filepath.Join(root, name))
			if err != nil {
				t.Fatal(err)
			}
			return data
		}
		raw := read("claim.json")
		if err := json.Unmarshal(read("sandbox.json"), &f.box); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(read("settings.json"), &b.cfg.AzureSandbox); err != nil {
			t.Fatal(err)
		}
		state, err := core.CrabboxStateDir()
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(state, "claims")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, req.RequestedLeaseID+".json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
		original, err := b.claim(req.RequestedLeaseID)
		if err != nil {
			t.Fatal(err)
		}
		req.Repo.Root = original.RepoRoot
		if original.FixedCreateIntent.Retention != nil || original.FixedCreateIntent.Journal.Version != 1 {
			t.Fatal("legacy fixture is not ephemeral journal v1")
		}
		if _, err := b.acquire(t.Context(), req); err != nil {
			t.Fatalf("stable claim replay: %v", err)
		}
		current, err := b.claim(req.RequestedLeaseID)
		if err != nil {
			t.Fatal(err)
		}
		if current.FixedCreateIntent.Retention != nil || current.FixedCreateIntent.Journal.Version != 1 || current.FixedCreateIntent.Attempt["expires_at"] != original.FixedCreateIntent.Attempt["expires_at"] || current.CloudID != original.CloudID || f.creates != 0 {
			t.Fatal("upgrade changed allocation, policy, deadline or identity")
		}
		if _, err := b.executionDeadline(current); err != nil {
			t.Fatal(err)
		}
		view, err := b.Status(t.Context(), core.StatusRequest{ID: req.RequestedLeaseID})
		if err != nil || !view.Ready || view.Retention != nil {
			t.Fatalf("legacy status changed after upgrade: %+v %v", view, err)
		}
		if err := b.Stop(t.Context(), core.StopRequest{ID: req.RequestedLeaseID}); err != nil {
			t.Fatal(err)
		}
		terminal, err := b.claim(req.RequestedLeaseID)
		if err != nil {
			t.Fatal(err)
		}
		if terminal.FixedCreateIntent.State != "released" || terminal.FixedCreateIntent.Retention != nil || terminal.FixedCreateIntent.Journal.Version != 1 || f.deletes != 1 {
			t.Fatal("upgrade broke compatible ephemeral cleanup")
		}
		encoded, err := json.Marshal(b.cfg.AzureSandbox)
		if err != nil {
			t.Fatal(err)
		}
		var oldSettings, currentSettings any
		if err := json.Unmarshal(read("settings.json"), &oldSettings); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, &currentSettings); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(oldSettings, currentSettings) {
			t.Fatal("legacy Azure settings changed on upgrade")
		}
	})
}
