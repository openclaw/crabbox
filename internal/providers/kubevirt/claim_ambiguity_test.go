package kubevirt

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
	t.Setenv("KUBECONFIG", "")
	b := &leaseBackend{cfg: core.BaseConfig()}
	scope := b.claimScope()
	dir, err := core.CrabboxStateDir()
	if err != nil {
		t.Fatal(err)
	}
	dir = filepath.Join(dir, "claims")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for index, id := range []string{"cbx_000000000001", "cbx_000000000002", "foreign_claim"} {
		claimScope := scope
		if index == 2 {
			claimScope = "foreign:" + scope
		}
		data, err := json.Marshal(core.LeaseClaim{LeaseID: id, Provider: providerName, ProviderScope: claimScope, Slug: "same-slug"})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, id+".json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	c, ok, err := b.resolveClaim("same-slug")
	if err == nil || !strings.Contains(err.Error(), "multiple") || ok || c.LeaseID != "" {
		t.Fatalf("ambiguous slug selected a claim: %+v %v %v", c, ok, err)
	}
	if c, ok, err := b.resolveClaim("cbx_000000000001"); err != nil || !ok || c.LeaseID != "cbx_000000000001" {
		t.Fatalf("exact ID lost: %+v %v %v", c, ok, err)
	}
	if err := os.Remove(filepath.Join(dir, "cbx_000000000002"+".json")); err != nil {
		t.Fatal(err)
	}
	if c, ok, err := b.resolveClaim("same-slug"); err != nil || !ok || c.LeaseID != "cbx_000000000001" {
		t.Fatalf("foreign scope interfered: %+v %v %v", c, ok, err)
	}

}
