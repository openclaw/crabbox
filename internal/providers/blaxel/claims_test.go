package blaxel

import (
	"reflect"
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

func TestFinishResolvedLeaseAdmission(t *testing.T) {
	for _, mode := range []string{"observe", "wrong scope", "different repo", "reclaim"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			const id = "blx_fixture"
			const baseURL = "https://api.blaxel.ai"
			scope := blaxelEndpointWorkspaceScope(baseURL, "workspace") + "/ownership:fixture"
			if err := core.ClaimLeaseForRepoProviderScopePond(id, "fixture-slug", providerName, scope, "fixture-pond", t.TempDir(), 3*time.Minute, false); err != nil {
				t.Fatal(err)
			}
			before, err := core.ReadLeaseClaim(id)
			if err != nil {
				t.Fatal(err)
			}
			repoRoot := ""
			if mode != "observe" {
				repoRoot = t.TempDir()
			}
			workspace := "workspace"
			if mode == "wrong scope" {
				workspace = "other-workspace"
			}
			lease, resource, slug, err := finishResolvedLease(before, repoRoot, mode == "reclaim" || mode == "wrong scope", 9*time.Minute, baseURL, workspace)
			after, readErr := core.ReadLeaseClaim(id)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if mode == "wrong scope" || mode == "different repo" {
				if err == nil || lease != "" || resource != "" || slug != "" {
					t.Fatalf("rejected result=%q/%q/%q err=%v", lease, resource, slug, err)
				}
				if mode == "wrong scope" && !strings.Contains(err.Error(), "different API endpoint or workspace") {
					t.Fatalf("scope error lost: %v", err)
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatal("rejected admission changed claim")
				}
				return
			}
			if err != nil || lease != id || resource != "fixture" || slug != "fixture-slug" {
				t.Fatalf("result=%q/%q/%q err=%v", lease, resource, slug, err)
			}
			if mode == "observe" {
				if !reflect.DeepEqual(before, after) {
					t.Fatal("observation changed claim")
				}
			} else if after.RepoRoot != repoRoot || after.ProviderScope != scope || after.Pond != before.Pond || after.Slug != before.Slug || after.IdleTimeoutSeconds != 180 {
				t.Fatalf("reclaim changed preserved fields: %#v", after)
			}
		})
	}
}

func TestFinishResolvedLeaseNativeProjection(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const baseURL = "https://api.blaxel.ai"
	for _, tc := range []struct{ id, slug, resource string }{
		{"  blx_fixture  ", " kept verbatim ", "fixture"},
		{"blx_fixture", "", "fixture"},
		{"  blxr_fixture  ", " ", "blxr_fixture"},
	} {
		t.Run(tc.id+tc.slug, func(t *testing.T) {
			claim := core.LeaseClaim{LeaseID: tc.id, Slug: tc.slug, Provider: providerName, ProviderScope: blaxelEndpointWorkspaceScope(baseURL, "workspace") + "/ownership:fixture"}
			lease, resource, slug, err := finishResolvedLease(claim, "", false, 0, baseURL, "workspace")
			wantSlug := tc.slug
			if strings.TrimSpace(wantSlug) == "" {
				wantSlug = core.NewLeaseSlug(tc.id)
			}
			if err != nil || lease != tc.id || resource != tc.resource || slug != wantSlug {
				t.Fatalf("result=%q/%q/%q err=%v", lease, resource, slug, err)
			}
		})
	}
}

func TestFinishResolvedLeaseTimeoutFallback(t *testing.T) {
	for _, tc := range []struct {
		name     string
		proposed time.Duration
		want     int
	}{
		{"positive", 5 * time.Minute, 300},
		{"zero", 0, 120},
		{"negative", -time.Minute, 120},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			const baseURL = "https://api.blaxel.ai"
			claim := core.LeaseClaim{LeaseID: "blx_fixture", Provider: providerName, ProviderScope: blaxelEndpointWorkspaceScope(baseURL, "workspace") + "/ownership:fixture", IdleTimeoutSeconds: 120}
			_, _, _, err := finishResolvedLease(claim, t.TempDir(), false, tc.proposed, baseURL, "workspace")
			if err != nil {
				t.Fatal(err)
			}
			after, err := core.ReadLeaseClaim(claim.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			if after.IdleTimeoutSeconds != tc.want {
				t.Fatalf("timeout=%d want=%d", after.IdleTimeoutSeconds, tc.want)
			}
		})
	}
}
