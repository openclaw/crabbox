package blaxel

import (
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestNewSandboxNameFitsBlaxelLimit(t *testing.T) {
	name := newSandboxName(core.Repo{Name: "this-is-a-very-long-repository-name-that-would-exceed-the-blaxel-sandbox-name-limit"})
	if len(name) > sandboxNameMaxLen {
		t.Fatalf("name length=%d name=%q, want <= %d", len(name), name, sandboxNameMaxLen)
	}
	if !strings.HasPrefix(name, namePrefix) {
		t.Fatalf("name=%q missing prefix %q", name, namePrefix)
	}
}

func TestResolveBlaxelLeaseClaimSelection(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const baseURL = "https://api.example.com"
	const workspace = "example-workspace"
	scope := blaxelEndpointWorkspaceScope(baseURL, workspace) + "/ownership:0123456789abcdef0123456789abcdef"
	for _, claim := range []struct{ id, slug string }{
		{"blx_alpha", "blx-target"},
		{"blx_target", "selected-lease"},
	} {
		if err := core.ClaimLeaseForRepoProviderScopePond(claim.id, claim.slug, providerName, scope, "", t.TempDir(), time.Minute, false); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct{ name, identifier, want string }{
		{"exact id before normalized slug", "blx_target", "blx_target"},
		{"normalized slug", " Selected Lease ", "blx_target"},
		{"other slug", "blx-target", "blx_alpha"},
	} {
		t.Run(test.name, func(t *testing.T) {
			claim, found, err := resolveBlaxelLeaseClaim(test.identifier, baseURL, workspace)
			if err != nil || !found || claim.LeaseID != test.want || claim.ProviderScope != scope {
				t.Fatalf("resolved id=%q scope=%q found=%t err=%v, want id=%q scope=%q", claim.LeaseID, claim.ProviderScope, found, err, test.want, scope)
			}
		})
	}
	if _, found, err := resolveBlaxelLeaseClaim("blx_target", baseURL, "another-workspace"); err == nil || found || !strings.Contains(err.Error(), "different API endpoint or workspace") {
		t.Fatalf("nonmatching workspace: found=%t err=%v", found, err)
	}
}
