package shared

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestCloneLabels(t *testing.T) {
	fromNil := CloneLabels(nil)
	if fromNil == nil || len(fromNil) != 0 {
		t.Fatalf("CloneLabels(nil)=%#v, want writable empty map", fromNil)
	}
	fromNil["state"] = "ready"
	empty := map[string]string{}
	if got := CloneLabels(empty); got == nil || len(got) != 0 {
		t.Fatalf("CloneLabels(empty)=%#v", got)
	}
	original := map[string]string{"state": "ready"}
	clone := CloneLabels(original)
	clone["state"] = "active"
	if original["state"] != "ready" {
		t.Fatalf("clone aliases original: %#v", original)
	}
}

func TestValidateClaimBindingFields(t *testing.T) {
	claim := core.LeaseClaim{
		Provider:      "example",
		ProviderScope: "account:one",
		LeaseID:       "cbx_aaaaaaaaaaaa",
		Slug:          "alpha",
		CloudID:       "resource-1",
		Labels:        map[string]string{"provider": "example", "lease": "cbx_aaaaaaaaaaaa", "slug": "alpha", "empty": ""},
	}
	want := ClaimBinding{
		Provider:       claim.Provider,
		ProviderScope:  claim.ProviderScope,
		LeaseID:        claim.LeaseID,
		Slug:           claim.Slug,
		CloudID:        claim.CloudID,
		RequiredLabels: map[string]string{"empty": ""},
	}
	if err := ValidateClaimBinding(claim, want); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		field string
		edit  func(*core.LeaseClaim)
	}{
		{"provider", "provider", func(got *core.LeaseClaim) { got.Provider = "other" }},
		{"scope", "provider scope", func(got *core.LeaseClaim) { got.ProviderScope = "account:two" }},
		{"lease", "lease ID", func(got *core.LeaseClaim) { got.LeaseID = "cbx_bbbbbbbbbbbb" }},
		{"slug", "slug", func(got *core.LeaseClaim) { got.Slug = "beta" }},
		{"cloud", "cloud ID", func(got *core.LeaseClaim) { got.CloudID = "resource-2" }},
		{"label", "label lease", func(got *core.LeaseClaim) { got.Labels = CloneLabels(got.Labels); got.Labels["lease"] = "other" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := claim
			tt.edit(&got)
			if err := ValidateClaimBinding(got, want); err == nil || !strings.Contains(err.Error(), tt.field) {
				t.Fatalf("err=%v", err)
			}
		})
	}
	missing := claim
	missing.Labels = CloneLabels(claim.Labels)
	delete(missing.Labels, "empty")
	if err := ValidateClaimBinding(missing, want); err == nil || !strings.Contains(err.Error(), "label empty") {
		t.Fatalf("missing required empty label err=%v", err)
	}
}

func TestValidateClaimBindingCanRequireEmptyProviderScope(t *testing.T) {
	claim := core.LeaseClaim{Provider: "example", ProviderScope: "region:other", Labels: map[string]string{"provider": "example"}}
	want := ClaimBinding{Provider: "example", ExactProviderScope: true}
	if err := ValidateClaimBinding(claim, want); err == nil || !strings.Contains(err.Error(), "provider scope") {
		t.Fatalf("nonempty provider scope accepted for an explicitly empty scope: %v", err)
	}
	claim.ProviderScope = ""
	if err := ValidateClaimBinding(claim, want); err != nil {
		t.Fatal(err)
	}
}

func TestResolveProviderClaimStrict(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const (
		provider = "example"
		scope    = "account:one"
		leaseID  = "cbx_aaaaaaaaaaaa"
		slug     = "alpha"
	)
	labels := map[string]string{"lease": leaseID, "slug": slug, "provider": provider}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, provider, scope, "", t.TempDir(), time.Minute, false, core.Server{Provider: provider, CloudID: "resource-1", Labels: labels}, core.SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	exact, ok, err := ResolveProviderClaimStrict(leaseID, provider, scope)
	if err != nil || !ok || exact.LeaseID != leaseID || exact.Revision == "" {
		t.Fatalf("exact=%#v ok=%v err=%v", exact, ok, err)
	}
	bySlug, ok, err := ResolveProviderClaimStrict(slug, provider, scope)
	if err != nil || !ok || bySlug.Revision != exact.Revision {
		t.Fatalf("slug=%#v ok=%v err=%v", bySlug, ok, err)
	}
	if _, ok, err := ResolveProviderClaimStrict(leaseID, provider, "account:two"); ok || !errors.Is(err, ErrStrictClaimMismatch) {
		t.Fatalf("scope mismatch ok=%v err=%v", ok, err)
	}
	lookalikeID := "cbx_bbbbbbbbbbbb"
	lookalikeLabels := map[string]string{"lease": lookalikeID, "slug": leaseID, "provider": provider}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(lookalikeID, leaseID, provider, scope, "", t.TempDir(), time.Minute, false, core.Server{Provider: provider, CloudID: "resource-2", Labels: lookalikeLabels}, core.SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ResolveProviderClaimStrict("cbx_cccccccccccc", provider, scope); ok || !errors.Is(err, ErrStrictClaimMismatch) {
		t.Fatalf("missing canonical ok=%v err=%v", ok, err)
	}
	if _, ok, err := ResolveProviderClaimStrict("missing", provider, scope); ok || err != nil {
		t.Fatalf("missing slug ok=%v err=%v", ok, err)
	}
	if _, ok, err := ResolveProviderClaimStrict(leaseID, "other", scope); ok || !errors.Is(err, ErrStrictClaimMismatch) {
		t.Fatalf("provider mismatch ok=%v err=%v", ok, err)
	}
}

func TestExactClaimOwnershipRejectsMissingAndStaleBindings(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	want := ClaimBinding{
		Provider:      "example",
		ProviderScope: "account:one",
		LeaseID:       "cbx_aaaaaaaaaaaa",
		Slug:          "alpha",
		CloudID:       "resource-1",
	}
	if _, err := RequireExactClaim(want); err == nil || !strings.Contains(err.Error(), "no exact local ownership claim") {
		t.Fatalf("missing claim err=%v", err)
	}
	labels := map[string]string{"lease": want.LeaseID, "slug": want.Slug, "provider": want.Provider}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(want.LeaseID, want.Slug, want.Provider, want.ProviderScope, "", t.TempDir(), time.Minute, false, core.Server{Provider: want.Provider, CloudID: want.CloudID, Labels: labels}, core.SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	claim, err := RequireExactClaim(want)
	if err != nil {
		t.Fatal(err)
	}
	stale := want
	stale.CloudID = "resource-2"
	if _, err := RequireExactClaim(stale); err == nil || !strings.Contains(err.Error(), "stale exact local ownership claim") {
		t.Fatalf("stale claim err=%v", err)
	}
	called := false
	if err := RemoveExactClaimAfter(claim, want, func() error { called = true; return nil }); err != nil || !called {
		t.Fatalf("fenced deletion called=%v err=%v", called, err)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(want.LeaseID); err != nil || exists {
		t.Fatalf("claim exists=%v err=%v", exists, err)
	}
}

func TestResolveProviderClaimStrictRejectsMalformedExactClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stateDir, err := core.CrabboxStateDir()
	if err != nil {
		t.Fatal(err)
	}
	claimsDir := filepath.Join(stateDir, "claims")
	if err := os.MkdirAll(claimsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	leaseID := "cbx_aaaaaaaaaaaa"
	if err := os.WriteFile(filepath.Join(claimsDir, leaseID+".json"), []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ResolveProviderClaimStrict(leaseID, "example", ""); ok || err == nil {
		t.Fatalf("malformed exact ok=%v err=%v", ok, err)
	}
}

func TestRequireClaimSnapshot(t *testing.T) {
	claim := core.LeaseClaim{LeaseID: "cbx_aaaaaaaaaaaa", Provider: "example", Revision: "revision-1"}
	server := core.Server{Labels: map[string]string{"lease": claim.LeaseID}}
	if _, err := RequireClaimSnapshot(server, claim.Provider); err == nil || !strings.Contains(err.Error(), "snapshot is missing") {
		t.Fatalf("missing snapshot err=%v", err)
	}
	core.SetServerLeaseClaimSnapshot(&server, core.LeaseClaim{}, false)
	if _, err := RequireClaimSnapshot(server, claim.Provider); err == nil || !strings.Contains(err.Error(), "no exact claim") {
		t.Fatalf("absent snapshot err=%v", err)
	}
	core.SetServerLeaseClaimSnapshot(&server, claim, true)
	got, err := RequireClaimSnapshot(server, claim.Provider)
	if err != nil || got.Revision != claim.Revision {
		t.Fatalf("claim=%#v err=%v", got, err)
	}
	for _, test := range []struct {
		name string
		edit func(*core.Server, *core.LeaseClaim)
		want string
	}{
		{"provider", func(_ *core.Server, claim *core.LeaseClaim) { claim.Provider = "other" }, "provider mismatch"},
		{"lease", func(server *core.Server, _ *core.LeaseClaim) { server.Labels["lease"] = "cbx_bbbbbbbbbbbb" }, "lease mismatch"},
		{"revision", func(_ *core.Server, claim *core.LeaseClaim) { claim.Revision = "" }, "no revision"},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidateServer := core.Server{Labels: CloneLabels(server.Labels)}
			candidateClaim := claim
			test.edit(&candidateServer, &candidateClaim)
			core.SetServerLeaseClaimSnapshot(&candidateServer, candidateClaim, true)
			if _, err := RequireClaimSnapshot(candidateServer, claim.Provider); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v, want %q", err, test.want)
			}
		})
	}
}

func TestCommitClaimTouchOrdersAuthorizationAndPublication(t *testing.T) {
	for _, scenario := range []string{"snapshot missing", "snapshot absent", "authorization denied", "invalid override", "changed during preparation", "committed"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			const leaseID = "static_claim_touch"
			server := core.Server{Provider: "ssh", CloudID: "fixture-host", Labels: map[string]string{"lease": leaseID, "state": "ready"}}
			if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "fixture", "ssh", "fixture-scope", "", t.TempDir(), time.Minute, false, server, core.SSHTarget{}); err != nil {
				t.Fatal(err)
			}
			expected, err := core.ReadLeaseClaim(leaseID)
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "snapshot missing" {
				core.SetServerLeaseClaimSnapshot(&server, expected, scenario != "snapshot absent")
			}
			override := 95 * time.Second
			if scenario == "invalid override" {
				override = 0
			}
			req := core.TouchRequest{Lease: core.LeaseTarget{LeaseID: leaseID, Server: server}, IdleTimeoutOverride: &override}
			now := time.Now().UTC().Truncate(time.Second)
			var calls []string
			updated, touchErr := CommitClaimTouch(t.Context(), req, ClaimTouchPolicy{
				Provider: "static",
				Authorize: func(_ context.Context, lease core.LeaseTarget, claim core.LeaseClaim) error {
					calls = append(calls, "authorize")
					if lease.LeaseID != leaseID || !reflect.DeepEqual(claim, expected) {
						t.Fatal("authorization lost exact snapshot")
					}
					if scenario == "authorization denied" {
						return errors.New("identity mismatch")
					}
					return nil
				},
				Prepare: func(claim core.LeaseClaim) (map[string]string, time.Time) {
					if len(calls) != 1 || calls[0] != "authorize" {
						t.Fatal("preparation ran before authorization")
					}
					calls = append(calls, "prepare")
					if scenario == "changed during preparation" {
						if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(leaseID, claim, map[string]string{"state": "other-writer"}); err != nil {
							t.Fatal(err)
						}
					}
					return map[string]string{"state": "touched"}, now
				},
			})
			wantCalls := []string{"authorize"}
			switch scenario {
			case "snapshot missing", "snapshot absent":
				wantCalls = nil
			case "changed during preparation", "committed":
				wantCalls = []string{"authorize", "prepare"}
			}
			if !reflect.DeepEqual(calls, wantCalls) {
				t.Fatalf("calls=%v want=%v", calls, wantCalls)
			}
			actual, err := core.ReadLeaseClaim(leaseID)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "committed" {
				if touchErr != nil || updated.Revision == expected.Revision || !reflect.DeepEqual(actual, updated) || updated.Labels["state"] != "touched" || updated.LastUsedAt != now.Format(time.RFC3339) || updated.IdleTimeoutSeconds != 95 {
					t.Fatalf("touch=%#v persisted=%#v err=%v", updated, actual, touchErr)
				}
			} else {
				if touchErr == nil {
					t.Fatal("expected refusal")
				}
				if scenario == "changed during preparation" {
					if !strings.Contains(touchErr.Error(), "claim changed") || actual.Labels["state"] != "other-writer" {
						t.Fatalf("raced touch=%#v err=%v", actual, touchErr)
					}
				} else if !reflect.DeepEqual(actual, expected) {
					t.Fatal("refused touch changed durable claim")
				}
			}
		})
	}
}
