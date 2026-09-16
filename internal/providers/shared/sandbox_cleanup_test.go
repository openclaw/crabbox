package shared

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestSandboxCleanupRetainsLockedAdmissionAndMutationOrder(t *testing.T) {
	for _, mode := range []string{"delete", "dry-run", "forget-missing", "retain-missing", "disappeared", "foreign-scope"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			const id = "cbx_aaaaaaaaaaaa"
			scope := "scope"
			if mode == "foreign-scope" {
				scope = "foreign"
			}
			server := core.Server{Provider: "example", CloudID: "sandbox", Labels: map[string]string{"lease": id, "slug": "alpha"}}
			if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(id, "alpha", "example", scope, "", t.TempDir(), time.Minute, false, server, core.SSHTarget{}); err != nil {
				t.Fatal(err)
			}
			stored, err := core.ReadLeaseClaim(id)
			if err != nil {
				t.Fatal(err)
			}
			listed := stored
			listed.ProviderScope = "scope"
			var stdout, stderr bytes.Buffer
			var events []string
			locked := false
			observe := func(event string) {
				if !locked {
					t.Errorf("%s escaped the operation lock", event)
				}
				events = append(events, event)
			}
			missing := errors.New("missing")
			cleanup := SandboxClaimCleanup[string]{
				Provider: "example", Runtime: core.Runtime{Stdout: &stdout, Stderr: &stderr},
				MatchesScope: func(claim core.LeaseClaim) bool { return claim.ProviderScope == "scope" },
				Lock: func(context.Context, string) (func(), error) {
					locked = true
					events = append(events, "lock")
					if mode == "disappeared" {
						if err := core.RemoveLeaseClaimIfUnchanged(id, stored); err != nil {
							return nil, err
						}
					}
					return func() { events = append(events, "unlock"); locked = false }, nil
				},
				SandboxID: func(claim core.LeaseClaim) string { return claim.CloudID },
				Get: func(context.Context, string) (string, error) {
					observe("get")
					if strings.HasSuffix(mode, "missing") {
						return "", missing
					}
					return "sandbox", nil
				},
				IsNotFound:    func(err error) bool { return err == missing },
				ForgetMissing: mode == "forget-missing", ForgetMissingHint: "example.forgetMissing",
				Due:      func(core.LeaseClaim, time.Time) (bool, string) { observe("due"); return true, "expired" },
				Validate: func(core.LeaseClaim, string) error { observe("validate"); return nil },
				Delete:   func(context.Context, string) error { observe("delete"); return nil },
			}
			if err := CleanupSandboxClaims(t.Context(), core.CleanupRequest{DryRun: mode == "dry-run"}, []core.LeaseClaim{listed}, cleanup); err != nil {
				t.Fatal(err)
			}
			want := []string{"lock", "get", "due", "validate", "delete", "unlock"}
			if mode == "dry-run" {
				want = []string{"lock", "get", "due", "validate", "unlock"}
			} else if strings.HasSuffix(mode, "missing") {
				want = []string{"lock", "get", "unlock"}
			} else if mode == "disappeared" || mode == "foreign-scope" {
				want = []string{"lock", "unlock"}
			}
			if !reflect.DeepEqual(events, want) || locked {
				t.Fatalf("events=%v want=%v locked=%v", events, want, locked)
			}
			current, err := core.ReadLeaseClaim(id)
			if err != nil {
				t.Fatal(err)
			}
			wantRemoved := mode == "delete" || mode == "forget-missing" || mode == "disappeared"
			if (current.LeaseID == "") != wantRemoved {
				t.Fatalf("claim removal=%v want=%v", current.LeaseID == "", wantRemoved)
			}
			if mode == "dry-run" && (!strings.Contains(stdout.String(), "would delete sandbox=sandbox") || strings.Contains(stdout.String(), "cleanup removed=")) {
				t.Fatalf("dry-run output=%q", stdout.String())
			}
		})
	}
}
