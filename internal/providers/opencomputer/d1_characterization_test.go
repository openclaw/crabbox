package opencomputer

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestD1ReadCharacterization(t *testing.T) {
	for _, mode := range []string{"ready", "terminal", "wait terminal", "raw error", "canceled fetch", "deadline fetch", "missing labels", "foreign labels", "foreign scope", "unclaimed"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fake := newFakeAPIWithServer(t, testutil.NewPipeHTTPServer)
				b := newAPIBackend(t, fake)
				scope := testOCClaimScope(fake.server.URL)
				leaseID := leasePrefix + fake.sandboxID
				fake.listState = " RUNNING "
				if strings.Contains(mode, "terminal") {
					fake.listState = "stopped"
				}
				if mode == "missing labels" {
					fake.tags = nil
				}
				if mode == "foreign labels" {
					fake.tags[openComputerClaimTagKey] = "foreign"
				}
				if mode == "foreign scope" {
					scope = "foreign"
				}
				if mode != "unclaimed" {
					if err := core.ClaimLeaseForRepoProviderScopePond(leaseID, "d1-status", providerName, scope, "d1-pond", "/repo", time.Minute, false); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if mode == "canceled fetch" {
					cancel()
				}
				if mode == "raw error" {
					fake.getStatusCode = http.StatusInternalServerError
				}
				if mode == "deadline fetch" {
					fake.blockGet = true
				}
				view, err := b.Status(ctx, core.StatusRequest{ID: leaseID, Wait: mode == "wait terminal" || mode == "deadline fetch", WaitTimeout: 10 * time.Millisecond})
				switch mode {
				case "ready", "terminal":
					state := strings.ToLower(strings.TrimSpace(fake.listState))
					want := core.StatusView{ID: leaseID, Slug: "d1-status", Provider: providerName, TargetOS: "linux", ServerID: fake.sandboxID, State: state, Pond: "d1-pond", Network: "public", Ready: mode == "ready", Labels: map[string]string{"provider": providerName, "lease": leaseID, "pond": "d1-pond", "state": state}}
					if err != nil || !reflect.DeepEqual(view, want) {
						t.Fatalf("view=%#v err=%v want=%#v", view, err, want)
					}
				default:
					if err == nil || !reflect.DeepEqual(view, core.StatusView{}) {
						t.Fatalf("view=%#v err=%v", view, err)
					}
					switch mode {
					case "raw error":
						if !strings.Contains(err.Error(), "500") {
							t.Fatal(err)
						}
					case "canceled fetch":
						if !errors.Is(err, context.Canceled) {
							t.Fatal(err)
						}
					case "deadline fetch":
						if err.Error() != "timed out waiting for opencomputer sandbox "+fake.sandboxID+" to become ready" {
							t.Fatal(err)
						}
					case "wait terminal":
						if err.Error() != "opencomputer sandbox "+fake.sandboxID+" entered terminal state \"stopped\" before becoming ready" {
							t.Fatal(err)
						}
					default:
						if core.ExitCodeForError(err, 1) != 4 {
							t.Fatal(err)
						}
					}
				}
				if mode == "foreign scope" || mode == "unclaimed" {
					if fake.calls(http.MethodGet, "/api/sandboxes") != 0 {
						t.Fatal("fetched before local claim validation")
					}
				}
				if strings.Contains(mode, "fetch") {
					return
				}
				api, err := newOCAPIClient(b.cfg, b.rt)
				if err != nil {
					t.Fatal(err)
				}
				_, err = shared.VerifySandboxClaim(ctx, leaseID, fake.sandboxID, func(claim core.LeaseClaim) error { return validateOpenComputerClaimScope(claim, api.baseURL) }, api.getSandboxWithTags, validateOpenComputerSandboxOwnership)
				if mode == "ready" || mode == "terminal" || mode == "wait terminal" {
					if err != nil {
						t.Fatal(err)
					}
				} else if err == nil {
					t.Fatal("verification accepted invalid claim or failed fetch")
				}

			})
		})
	}
}
