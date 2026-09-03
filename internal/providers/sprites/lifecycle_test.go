package sprites

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func spritesTestClaim(t *testing.T, cfg Config, name, id, org string) (LeaseTarget, LeaseClaim, string) {
	t.Helper()
	repo := t.TempDir()
	server := Server{Provider: spritesProvider, CloudID: name, Name: name, Labels: map[string]string{
		"provider": spritesProvider, "lease": "cbx_testidentity", "slug": "test-identity", "name": name,
		"sprites_resource_id": id, "sprites_organization": org,
	}}
	if err := core.ClaimLeaseTargetForRepoConfig("cbx_testidentity", "test-identity", cfg, server, SSHTarget{}, repo, 0, false); err != nil {
		t.Fatal(err)
	}
	claim, _, err := core.ReadLeaseClaimWithPresence("cbx_testidentity")
	if err != nil {
		t.Fatal(err)
	}
	return LeaseTarget{Server: server, LeaseID: "cbx_testidentity"}, claim, repo
}

func TestSpritesResolveValidatesClaimBeforeBootstrap(t *testing.T) {
	for _, scenario := range []string{"identity", "reclaim replacement", "organization", "endpoint", "name", "labels", "live lease", "repo", "expected identity"} {
		t.Run(scenario, func(t *testing.T) {
			testutil.IsolateUserDirs(t)
			cfg := Config{Provider: spritesProvider, Sprites: SpritesConfig{WorkRoot: "/home/sprite/crabbox"}}
			lease, before, repo := spritesTestClaim(t, cfg, "crabbox-test", "original-id", "test-org")
			sprite := spritesInfo{ID: "original-id", Name: "crabbox-test", Organization: "test-org", Labels: spritesAPILabels(lease.LeaseID, "test-identity")}
			req := ResolveRequest{ID: lease.LeaseID, Repo: core.Repo{Root: repo}}
			switch scenario {
			case "identity":
				sprite.ID = "replacement-id"
			case "reclaim replacement":
				sprite.ID, req.Reclaim = "replacement-id", true
			case "organization":
				sprite.Organization = "other-org"
			case "endpoint":
				cfg.Sprites.APIURL = "https://other.example"
			case "name":
				sprite.Name = "crabbox-other"
			case "labels":
				sprite.Labels = nil
			case "live lease":
				sprite.Labels = spritesAPILabels("cbx_other", "other")
			case "repo":
				req.Repo.Root = t.TempDir()
			case "expected identity":
				req.ExpectedProviderIdentity = core.ProviderIdentityExpectation{LeaseID: lease.LeaseID, ResourceID: "other-name"}
			}
			runner := &recordingRunner{}
			b := &spritesBackend{cfg: cfg, client: &fakeSpritesAPI{get: sprite}, rt: Runtime{Exec: runner, Stderr: io.Discard}}
			if _, err := b.Resolve(t.Context(), req); err == nil {
				t.Fatal("expected identity/ownership error")
			}
			if len(runner.calls) != 0 {
				t.Fatal("CLI invoked before identity/ownership validation")
			}
			after, _, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("claim changed: %v", err)
			}
		})
	}
}

func TestSpritesReadOnlyResolutionAndStatusDoNotBootstrap(t *testing.T) {
	for _, state := range []string{"cold", "warm", "running"} {
		t.Run(state, func(t *testing.T) {
			testutil.IsolateUserDirs(t)
			cfg := Config{Provider: spritesProvider, Sprites: SpritesConfig{WorkRoot: "/home/sprite/crabbox"}}
			lease, before, _ := spritesTestClaim(t, cfg, "crabbox-test", "original-id", "test-org")
			sprite := spritesInfo{ID: "original-id", Name: "crabbox-test", Organization: "test-org", Status: state, Labels: spritesAPILabels(lease.LeaseID, "test-identity")}
			b := &spritesBackend{cfg: cfg, client: &fakeSpritesAPI{get: sprite}}
			// No runner is installed: read-only resolution must not require a CLI.
			for _, req := range []ResolveRequest{{ID: lease.LeaseID, StatusOnly: true}, {ID: lease.LeaseID, NoLocalStateMutations: true}} {
				resolved, err := b.Resolve(t.Context(), req)
				if err != nil {
					t.Fatal(err)
				}
				if resolved.Server.Status != state || resolved.SSH.Key != "" {
					t.Fatalf("unexpected resolution: status=%s key=%s", resolved.Server.Status, resolved.SSH.Key)
				}
			}
			for _, wait := range []bool{false, true} {
				view, err := b.Status(t.Context(), core.StatusRequest{ID: lease.LeaseID, Wait: wait})
				if err != nil {
					t.Fatal(err)
				}
				if view.State != state || view.Ready || view.ProviderResourceID != "original-id" {
					t.Fatalf("view=%+v", view)
				}
			}
			after, _, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("claim changed: %v", err)
			}
			keyPath, err := core.TestboxKeyPath(lease.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
				t.Fatalf("unexpected key file: %v", err)
			}
		})
	}
}

func TestSpritesResolveAllowsVerifiedReuseAndExplicitAdoption(t *testing.T) {
	for _, scenario := range []string{"managed", "adopted", "reclaim"} {
		t.Run(scenario, func(t *testing.T) {
			testutil.IsolateUserDirs(t)
			cfg := Config{Provider: spritesProvider, Sprites: SpritesConfig{WorkRoot: "/home/sprite/crabbox"}}
			lease, _, repo := spritesTestClaim(t, cfg, "crabbox-test", "original-id", "test-org")
			sprite := spritesInfo{ID: "original-id", Name: "crabbox-test", Organization: "test-org", Labels: spritesAPILabels(lease.LeaseID, "test-identity")}
			if scenario != "managed" {
				sprite.Labels = nil
			}
			if scenario == "adopted" {
				lease.Server.Labels["sprites_ownership"] = "adopted"
				if err := core.ClaimLeaseTargetForRepoConfig(lease.LeaseID, "test-identity", cfg, lease.Server, SSHTarget{}, repo, 0, false); err != nil {
					t.Fatal(err)
				}
			}
			// Stop at bootstrap so this test never starts SSH or remote processes.
			runner := &recordingRunner{failContains: "sprite exec", err: errors.New("bootstrap reached")}
			b := &spritesBackend{cfg: cfg, client: &fakeSpritesAPI{get: sprite}, rt: Runtime{Exec: runner, Stderr: io.Discard}}
			_, err := b.Resolve(t.Context(), ResolveRequest{ID: lease.LeaseID, Repo: core.Repo{Root: repo}, Reclaim: scenario == "reclaim"})
			if err == nil || !strings.Contains(err.Error(), "bootstrap reached") || len(runner.calls) != 2 {
				t.Fatalf("verified reuse did not reach bootstrap: err=%v calls=%d", err, len(runner.calls))
			}
		})
	}
}

func TestSpritesReleaseAbsentResource(t *testing.T) {
	for _, scenario := range []string{"already deleted", "other organization", "organization unavailable", "missing organization", "legacy claim", "resource reappeared", "get forbidden", "get unavailable", "delete not found"} {
		t.Run(scenario, func(t *testing.T) {
			testutil.IsolateUserDirs(t)
			gets, deletes, orgs := 0, 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test-token" {
					t.Error("missing configured credential")
				}
				switch {
				case r.URL.Path == "/v1/organization":
					orgs++
					if scenario == "organization unavailable" {
						http.Error(w, "unavailable", 503)
						return
					}
					org := "test-org"
					if scenario == "other organization" {
						org = "other-org"
					}
					if scenario == "missing organization" {
						org = ""
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"organization": map[string]string{"slug": org}})
				case r.Method == http.MethodGet && r.URL.Path == "/v1/sprites/crabbox-test":
					gets++
					if scenario == "get forbidden" {
						http.Error(w, "forbidden", 403)
						return
					}
					if scenario == "get unavailable" {
						http.Error(w, "unavailable", 503)
						return
					}
					if scenario == "delete not found" || (scenario == "resource reappeared" && gets > 1) {
						_ = json.NewEncoder(w).Encode(spritesInfo{ID: "original-id", Name: "crabbox-test", Organization: "test-org", Labels: spritesAPILabels("cbx_testidentity", "test-identity")})
						return
					}
					http.NotFound(w, r)
				case r.Method == http.MethodDelete:
					deletes++
					http.NotFound(w, r)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			cfg := Config{Provider: spritesProvider, Sprites: SpritesConfig{Token: "test-token", APIURL: srv.URL, WorkRoot: "/home/sprite/crabbox"}}
			id, org := "original-id", "test-org"
			if scenario == "legacy claim" {
				id, org = "", ""
			}
			lease, before, _ := spritesTestClaim(t, cfg, "crabbox-test", id, org)
			keyPath, _, err := ensureTestboxKey(lease.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			client, err := newSpritesClient(cfg, Runtime{HTTP: srv.Client()})
			if err != nil {
				t.Fatal(err)
			}
			b := &spritesBackend{cfg: cfg, client: client, rt: Runtime{Stderr: io.Discard}}
			err = b.ReleaseLease(t.Context(), ReleaseLeaseRequest{Lease: lease})
			after, exists, readErr := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			_, keyErr := os.Stat(keyPath)
			wantSuccess := scenario == "already deleted" || scenario == "delete not found"
			if wantSuccess {
				if err != nil || exists || !os.IsNotExist(keyErr) {
					t.Fatalf("cleanup failed: err=%v exists=%t keyErr=%v", err, exists, keyErr)
				}
			} else if err == nil || !exists || keyErr != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("unsafe or failed cleanup changed local state: err=%v exists=%t keyErr=%v", err, exists, keyErr)
			}
			if scenario == "already deleted" && (gets != 2 || orgs != 1 || deletes != 0) {
				t.Fatalf("unexpected absence verification: gets=%d orgs=%d deletes=%d", gets, orgs, deletes)
			}
			if scenario != "delete not found" && deletes != 0 {
				t.Fatal("deletion attempted without live ownership")
			}
			if strings.HasPrefix(scenario, "get ") && orgs != 0 {
				t.Fatal("non-404 error treated as absence")
			}
		})
	}
}
