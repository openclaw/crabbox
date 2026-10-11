package crownest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestProviderClaimAmbiguity(t *testing.T) {
	testutil.IsolateUserDirs(t)
	const scope = "scope-a"
	dir, err := core.CrabboxStateDir()
	if err != nil {
		t.Fatal(err)
	}
	dir = filepath.Join(dir, "claims")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{leasePrefix + "alias_a", leasePrefix + "alias_b"} {
		data, err := json.Marshal(core.LeaseClaim{LeaseID: id, Provider: providerName, ProviderScope: scope, Slug: "same-slug"})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, id+".json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	id, _, _, err := resolveLeaseID("same-slug", "", false, 0, scope)
	if err == nil || !strings.Contains(err.Error(), "multiple") || id != "" {
		t.Fatalf("ambiguous slug selected %q: %v", id, err)
	}
}
