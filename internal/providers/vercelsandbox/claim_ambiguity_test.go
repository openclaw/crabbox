package vercelsandbox

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
	b := &backend{cfg: core.BaseConfig()}
	scope, err := b.newClaimScope()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := core.CrabboxStateDir()
	if err != nil {
		t.Fatal(err)
	}
	dir = filepath.Join(dir, "claims")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for index, id := range []string{leasePrefix + "alias_a", leasePrefix + "alias_b", leasePrefix + "alias_foreign"} {
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
	c, ok, err := b.resolveVercelSandboxLeaseClaim("same-slug")
	if err == nil || !strings.Contains(err.Error(), "multiple") || ok || c.LeaseID != "" {
		t.Fatalf("ambiguous slug selected a claim: %+v %v %v", c, ok, err)
	}
	if c, ok, err := b.resolveVercelSandboxLeaseClaim(leasePrefix + "alias_a"); err != nil || !ok || c.LeaseID != leasePrefix+"alias_a" {
		t.Fatalf("exact ID lost: %+v %v %v", c, ok, err)
	}
	if err := os.Remove(filepath.Join(dir, leasePrefix+"alias_b"+".json")); err != nil {
		t.Fatal(err)
	}
	if c, ok, err := b.resolveVercelSandboxLeaseClaim("same-slug"); err != nil || !ok || c.LeaseID != leasePrefix+"alias_a" {
		t.Fatalf("foreign scope interfered: %+v %v %v", c, ok, err)
	}

}
