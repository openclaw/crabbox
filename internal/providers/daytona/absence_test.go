package daytona

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/daytona/clients/api-client-go"
	core "github.com/openclaw/crabbox/internal/cli"
)

func TestDaytonaAbsenceRecoveryHTTP(t *testing.T) {
	for _, name := range []string{"absent", "force command", "ordinary stop", "legacy", "wrong account", "wrong endpoint", "auth error", "malformed not found", "duplicate not found", "wrong exact ID", "null inventory", "null items", "missing cursor", "malformed inventory", "duplicate fields", "empty partial page", "oversized page", "missing item ID", "foreign item", "list auth", "list transport", "second page failure", "repeated cursor", "page limit", "resource on later page", "complete pages"} {
		t.Run(name, func(t *testing.T) {
			f, b, repo := newDaytonaLifecycleFixture(t)
			const leaseID, resourceID = "cbx_123456abcdef", "sandbox-original"
			scope, _, _ := fixedDaytonaScope(f.server.URL, "org-test")
			switch name {
			case "legacy":
				scope = ""
			case "wrong account":
				scope, _, _ = fixedDaytonaScope(f.server.URL, "org-other")
			case "wrong endpoint":
				scope, _, _ = fixedDaytonaScope("https://other.example.test", "org-test")
			}
			if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "absence-fixture", daytonaProvider, scope, "", repo.Root, time.Minute, false,
				core.Server{Provider: daytonaProvider, CloudID: resourceID, ImmutableID: resourceID}, core.SSHTarget{}); err != nil {
				t.Fatal(err)
			}
			claim, _ := core.ReadLeaseClaim(leaseID)
			original := f.server.Config.Handler
			var mutations, lists atomic.Int32
			f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					mutations.Add(1)
					http.Error(w, "unexpected mutation", 500)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/sandbox/" + resourceID:
					if name == "wrong exact ID" {
						_ = json.NewEncoder(w).Encode(api.Sandbox{Id: "different", OrganizationId: "org-test"})
						return
					}
					if name == "auth error" {
						w.WriteHeader(http.StatusForbidden)
						fmt.Fprint(w, `{"message":"not found in account"}`)
						return
					}
					w.WriteHeader(http.StatusNotFound)
					if name == "malformed not found" {
						fmt.Fprint(w, `<html>not found</html>`)
					} else if name == "duplicate not found" {
						fmt.Fprint(w, `{"message":"Not found","statusCode":403,"statusCode":404}`)
					} else {
						_ = json.NewEncoder(w).Encode(daytonaNotFoundBody(r))
					}
				case "/sandbox":
					page := lists.Add(1)
					if name != "ordinary stop" && r.URL.Query().Has("labels") {
						t.Error("absence proof used a label-filtered inventory")
					}
					body := `{"items":[],"nextCursor":null}`
					switch name {
					case "null inventory":
						body = `null`
					case "null items":
						body = `{"items":null,"nextCursor":null}`
					case "missing cursor":
						body = `{"items":[]}`
					case "malformed inventory":
						body = `{"items":`
					case "duplicate fields":
						body = `{"items":[{}],"items":[],"nextCursor":null}`
					case "empty partial page":
						body = `{"items":[],"nextCursor":"next-page"}`
					case "oversized page":
						body += strings.Repeat(" ", 8<<20)
					case "list auth":
						w.WriteHeader(http.StatusUnauthorized)
					case "list transport":
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
						} else {
							_ = conn.Close()
						}
						return
					case "missing item ID", "foreign item", "second page failure", "repeated cursor", "page limit", "resource on later page", "complete pages":
						item := api.SandboxListItem{Id: fmt.Sprintf("unlabelled-%d", page), OrganizationId: "org-test", Labels: map[string]string{}}
						if name == "missing item ID" {
							item.Id = ""
						}
						if name == "foreign item" {
							item.OrganizationId = "other-org"
						}
						if name == "resource on later page" && page == 2 {
							item.Id = resourceID
						}
						var cursor any
						if page == 1 || name == "repeated cursor" {
							cursor = "page-two"
						}
						if name == "page limit" {
							cursor = fmt.Sprintf("page-%d", page+1)
						}
						if page == 2 && name == "second page failure" {
							w.WriteHeader(http.StatusServiceUnavailable)
						}
						encoded, _ := json.Marshal(map[string]any{"items": []api.SandboxListItem{item}, "nextCursor": cursor})
						body = string(encoded)
					}
					fmt.Fprint(w, body)
				default:
					original.ServeHTTP(w, r)
				}
			})
			forgotten, err := false, error(nil)
			if name == "force command" || name == "ordinary stop" {
				configPath := filepath.Join(t.TempDir(), "config.yaml")
				config := fmt.Sprintf("provider: daytona\ndaytona:\n  apiUrl: %s\n  apiKey: synthetic-key\n", f.server.URL)
				if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("CRABBOX_CONFIG", configPath)
				t.Setenv("CRABBOX_DAYTONA_API_KEY", "synthetic-key")
				for _, env := range []string{"CRABBOX_COORDINATOR", "CRABBOX_COORDINATOR_MODE", "CRABBOX_POND", "CRABBOX_TAILSCALE"} {
					t.Setenv(env, "")
				}
				args := []string{"stop", "--provider", daytonaProvider, "--id", leaseID}
				if name == "force command" {
					args = append(args, "--force")
				}
				var output bytes.Buffer
				err = (core.App{Stdout: &output, Stderr: &output}).Run(t.Context(), args)
				forgotten = strings.Contains(output.String(), "forgotten locally (resource absent)")
				if strings.Contains(output.String(), "released") {
					t.Fatalf("wrong outcome: %s", output.String())
				}
			} else {
				forgotten, err = core.ForgetAbsentLeaseClaim(t.Context(), b, claim)
			}
			want := name == "absent" || name == "force command" || name == "complete pages"
			if forgotten != want || (err == nil) != want {
				t.Fatalf("forgotten=%t err=%v want=%t", forgotten, err, want)
			}
			if name == "legacy" && !strings.Contains(err.Error(), "claim predates account binding; manual recovery per docs") {
				t.Fatalf("missing legacy recovery guidance: %v", err)
			}
			got, exists, readErr := core.ReadLeaseClaimWithPresence(leaseID)
			if readErr != nil || exists == want || exists && !reflect.DeepEqual(got, claim) || mutations.Load() != 0 {
				t.Fatalf("claim/effect mismatch: exists=%t err=%v mutations=%d", exists, readErr, mutations.Load())
			}
		})
	}
}

func TestDaytonaOrdinaryClaimAccountBinding(t *testing.T) {
	f, b, repo := newDaytonaLifecycleFixture(t)
	sandbox, id, slug, err := b.createDaytonaSandbox(t.Context(), repo, true, false, "bound-fixture")
	if err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(id)
	scope, _, _ := fixedDaytonaScope(f.server.URL, "org-test")
	if err != nil || claim.ProviderScope != scope || claim.CloudImmutableID != sandbox.GetId() || claim.FixedCreateIntent != nil {
		t.Fatalf("ordinary claim lacks account/resource binding: %+v err=%v", claim, err)
	}
	server := daytonaSandboxToServer(sandbox)
	if err := core.ClaimLeaseTargetForRepoConfig(id, slug, b.cfg, server, core.SSHTarget{}, repo.Root, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	refreshed, _ := core.ReadLeaseClaim(id)
	if refreshed.ProviderScope != scope || refreshed.CloudImmutableID != claim.CloudImmutableID {
		t.Fatal("ordinary refresh discarded the acquisition binding")
	}
	if err := b.AuthorizeStatusTouchClaim(t.Context(), core.LeaseTarget{LeaseID: id, Server: server}, refreshed); err != nil {
		t.Fatal(err)
	}
	server.CloudID, server.ImmutableID = "replacement", "replacement"
	if err := core.ClaimLeaseTargetForRepoConfig(id, slug, b.cfg, server, core.SSHTarget{}, repo.Root, time.Minute, true); err == nil {
		t.Fatal("reclaim retargeted an account-bound claim")
	}
}

func TestDaytonaOrdinaryAcquisitionRequiresAccountAttestation(t *testing.T) {
	f, b, repo := newDaytonaLifecycleFixture(t)
	f.currentKeyIdentity = func(identity map[string]any) { delete(identity, "organizationId") }
	if _, _, _, err := b.createDaytonaSandbox(t.Context(), repo, true, false, "fixture"); err == nil || f.sandboxCreates != 0 {
		t.Fatalf("acquired without account attestation: err=%v creates=%d", err, f.sandboxCreates)
	}
}
