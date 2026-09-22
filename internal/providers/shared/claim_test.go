package shared

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestPreserveClaimIdentityLabels(t *testing.T) {
	for _, tc := range []struct {
		name               string
		observed, recorded map[string]string
		want               map[string]string
		conflict           string
	}{
		{"nil", nil, nil, map[string]string{}, ""},
		{"omitted", map[string]string{"state": "running"}, map[string]string{"key": "original"}, map[string]string{"key": "original", "state": "running"}, ""},
		{"empty", map[string]string{"key": ""}, map[string]string{"key": "original"}, map[string]string{"key": "original"}, ""},
		{"confirmed", map[string]string{"key": "original"}, map[string]string{"key": "original"}, map[string]string{"key": "original"}, ""},
		{"legacy", map[string]string{"key": "observed", "state": "running"}, nil, map[string]string{"state": "running"}, ""},
		{"empty recorded", map[string]string{"key": "observed"}, map[string]string{"key": ""}, map[string]string{}, ""},
		{"conflict", map[string]string{"key": "replacement"}, map[string]string{"key": "original"}, nil, "key"},
		{"ordered conflict", map[string]string{"key": "replacement", "account": "other"}, map[string]string{"key": "original", "account": "owner"}, nil, "key"},
		{"later conflict", map[string]string{"account": "other"}, map[string]string{"key": "original", "account": "owner"}, nil, "account"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observed, recorded := maps.Clone(tc.observed), maps.Clone(tc.recorded)
			got, conflict := PreserveClaimIdentityLabels(tc.observed, tc.recorded, "key", "account")
			if conflict != tc.conflict || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("labels=%v conflict=%q, want %v %q", got, conflict, tc.want, tc.conflict)
			}
			if conflict == "" {
				got["state"] = "changed"
				got["key"] = "changed"
			}
			if !reflect.DeepEqual(tc.observed, observed) || !reflect.DeepEqual(tc.recorded, recorded) {
				t.Fatal("identity projection mutated or aliased an input map")
			}
		})
	}
}

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

func TestSandboxRepositoryMetadataScope(t *testing.T) {
	for _, tc := range []struct {
		name string
		repo core.Repo
		want string
	}{
		{"empty", core.Repo{}, "repo-sha256:e3b0c44298fc1c14"},
		{"whitespace empty", core.Repo{Root: " \t\n", Name: " \n"}, "repo-sha256:e3b0c44298fc1c14"},
		{"root before name and remote", core.Repo{Root: "abc", Name: "hello", RemoteURL: "https://example.com/repo.git"}, "repo-sha256:ba7816bf8f01cfea"},
		{"trim root", core.Repo{Root: " \tabc\n", Name: "hello"}, "repo-sha256:ba7816bf8f01cfea"},
		{"name fallback", core.Repo{Name: "hello"}, "repo-sha256:2cf24dba5fb0a30e"},
		{"trim name fallback", core.Repo{Root: "\t", Name: " hello\n"}, "repo-sha256:2cf24dba5fb0a30e"},
		{"remote alone ignored", core.Repo{RemoteURL: "https://example.com/repo.git"}, "repo-sha256:e3b0c44298fc1c14"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SandboxRepositoryMetadataScope(tc.repo); got != tc.want {
				t.Fatalf("scope=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestIndexProviderClaimsPreservesFilteringAndLastKeyWinner(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ids := []string{"cbx_000000000001", "cbx_000000000002", "cbx_000000000003", "cbx_000000000004"}
	for index, id := range ids {
		provider, native := "example", "same"
		if index == 2 {
			native = ""
		}
		if index == 3 {
			provider = "other"
		}
		server := core.Server{Provider: provider, CloudID: id, Labels: map[string]string{"native": native}}
		if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(id, "fixture", provider, "scope", "", t.TempDir(), time.Minute, false, server, core.SSHTarget{}); err != nil {
			t.Fatal(err)
		}
	}
	var visited []string
	indexed, err := IndexProviderClaims("example", func(claim core.LeaseClaim) string {
		if claim.Provider != "example" {
			t.Fatal("key projection ran for a foreign provider")
		}
		visited = append(visited, claim.LeaseID)
		return claim.Labels["native"]
	})
	if err != nil || len(indexed) != 1 || indexed["same"].LeaseID != ids[1] || !reflect.DeepEqual(visited, ids[:3]) {
		t.Fatalf("index=%v visited=%v error=%v", indexed, visited, err)
	}
	empty, err := IndexProviderClaims("absent", func(core.LeaseClaim) string {
		t.Fatal("key projection ran for an absent provider")
		return ""
	})
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty index=%v error=%v", empty, err)
	}
	empty["writable"] = core.LeaseClaim{}
}

func TestLabelsWithDefaultsPreservesStoredValuesAndCopies(t *testing.T) {
	stored := map[string]string{"provider": "stored", "state": "", "space": " ", "extra": "kept"}
	defaults := map[string]string{"provider": "fallback", "state": "running", "space": "fallback", "empty": ""}
	got := LabelsWithDefaults(stored, defaults)
	want := map[string]string{"provider": "stored", "state": "running", "space": " ", "extra": "kept", "empty": ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("labels=%#v, want %#v", got, want)
	}
	got["provider"] = "changed"
	if stored["provider"] != "stored" || stored["state"] != "" || defaults["provider"] != "fallback" {
		t.Fatal("label projection mutated its inputs")
	}
	fromNil := LabelsWithDefaults(nil, map[string]string{"lease": ""})
	if value, exists := fromNil["lease"]; !exists || value != "" {
		t.Fatalf("empty default not materialized: %#v", fromNil)
	}
	LabelsWithDefaults(nil, nil)["new"] = "writable"
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
	if claim, ok, err := ResolveProviderClaimStrict(leaseID, provider, scope); err != nil || !ok || claim.LeaseID != leaseID {
		t.Fatalf("lookalike slug displaced exact claim: claim=%#v ok=%v err=%v", claim, ok, err)
	}
	core.RemoveLeaseClaim(leaseID)
	if _, ok, err := ResolveProviderClaimStrict(leaseID, provider, scope); ok || !errors.Is(err, ErrStrictClaimMismatch) {
		t.Fatalf("missing canonical ID matched another claim's slug: ok=%v err=%v", ok, err)
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
	if err := RemoveExactClaimAfterContext(context.Background(), claim, want, func() error { called = true; return nil }); err != nil || !called {
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

func TestRemoveExactClaimAfterContext(t *testing.T) {
	for _, scenario := range []string{"canceled", "binding mismatch", "stale snapshot", "callback failure", "completed action"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			want := ClaimBinding{Provider: "example", ProviderScope: "account:one", LeaseID: "cbx_aaaaaaaaaaaa", Slug: "alpha", CloudID: "resource-1"}
			labels := map[string]string{"lease": want.LeaseID, "slug": want.Slug, "provider": want.Provider}
			if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(want.LeaseID, want.Slug, want.Provider, want.ProviderScope, "", t.TempDir(), time.Minute, false, core.Server{Provider: want.Provider, CloudID: want.CloudID, Labels: labels}, core.SSHTarget{}); err != nil {
				t.Fatal(err)
			}
			claim, err := RequireExactClaim(want)
			if err != nil {
				t.Fatal(err)
			}
			expected := claim
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch scenario {
			case "canceled":
				cancel()
			case "binding mismatch":
				want.CloudID = "resource-2"
			case "stale snapshot":
				expected, err = core.ReplaceLeaseClaimIfUnchangedDurableReturning(claim.LeaseID, claim, claim)
				if err != nil {
					t.Fatal(err)
				}
			}
			called := false
			failed := errors.New("provider cleanup failed")
			err = RemoveExactClaimAfterContext(ctx, claim, want, func() error {
				called = true
				if scenario == "callback failure" {
					return failed
				}
				cancel()
				return nil
			})
			if scenario == "completed action" {
				if err != nil || !called {
					t.Fatalf("confirmed action err=%v called=%t", err, called)
				}
				if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); exists || err != nil {
					t.Fatalf("confirmed cleanup retained claim: exists=%t err=%v", exists, err)
				}
				return
			}
			if err == nil || called != (scenario == "callback failure") {
				t.Fatalf("rejected cleanup err=%v called=%t", err, called)
			}
			if scenario == "canceled" && !errors.Is(err, context.Canceled) || scenario == "callback failure" && !errors.Is(err, failed) {
				t.Fatalf("cleanup cause lost: %v", err)
			}
			got, exists, readErr := core.ReadLeaseClaimWithPresence(claim.LeaseID)
			if readErr != nil || !exists || !reflect.DeepEqual(got, expected) {
				t.Fatalf("rejected cleanup changed ownership: claim=%#v err=%v", got, readErr)
			}
		})
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

func TestRefreshRetainedLeaseActivityMissingAndMalformed(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const id = "cbx_123456abcdef"
	if err := RefreshRetainedLeaseActivity(id, "example", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(id); err != nil || exists {
		t.Fatalf("missing refresh created claim: exists=%v err=%v", exists, err)
	}
	state, err := core.CrabboxStateDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(state, "claims", id+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	const malformed = "not claim JSON\n"
	if err := os.WriteFile(path, []byte(malformed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RefreshRetainedLeaseActivity(id, "example", time.Minute); err == nil {
		t.Fatal("malformed claim read error was swallowed")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != malformed {
		t.Fatalf("malformed claim changed: err=%v", err)
	}
}

func TestRefreshRetainedLeaseActivityPreservesOwnershipAndIdlePolicy(t *testing.T) {
	for _, tc := range []struct {
		name       string
		recorded   int
		configured time.Duration
		want       int
	}{
		{"recorded wins", 180, 9 * time.Minute, 180},
		{"zero falls back", 180, 0, 180},
		{"negative falls back", 180, -time.Minute, 180},
		{"initialize missing", 0, 9 * time.Minute, 540},
		{"both missing", 0, 0, 0},
		{"negative configuration without recorded timeout", 0, -time.Minute, 0},
		{"negative recorded timeout", -10, 0, -10},
		{"both negative", -10, -time.Minute, -10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			const id = "cbx_123456abcdef"
			server := core.Server{Provider: "example", CloudID: "sandbox-1", Labels: map[string]string{"lease": id, "slug": "alpha", "custom": "preserved"}}
			target := core.SSHTarget{Host: "192.0.2.1", User: "fixture", Port: "2222"}
			if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(id, "alpha", "example", "scope-one", "pond-one", t.TempDir(), time.Duration(tc.recorded)*time.Second, false, server, target); err != nil {
				t.Fatal(err)
			}
			before, err := core.ReadLeaseClaim(id)
			if err != nil {
				t.Fatal(err)
			}
			old := before
			old.LastUsedAt = "2000-01-01T00:00:00Z"
			old.IdleTimeoutSeconds = tc.recorded
			if err := core.ReplaceLeaseClaimIfUnchanged(id, before, old); err != nil {
				t.Fatal(err)
			}
			before, err = core.ReadLeaseClaim(id)
			if err != nil {
				t.Fatal(err)
			}
			started := time.Now().UTC().Truncate(time.Second)
			if err := RefreshRetainedLeaseActivity(id, "example", tc.configured); err != nil {
				t.Fatal(err)
			}
			after, err := core.ReadLeaseClaim(id)
			if err != nil {
				t.Fatal(err)
			}
			used, err := time.Parse(time.RFC3339, after.LastUsedAt)
			if err != nil || used.Before(started) || used.After(time.Now().UTC()) {
				t.Fatalf("last used=%q err=%v", after.LastUsedAt, err)
			}
			if after.IdleTimeoutSeconds != tc.want {
				t.Fatalf("idle=%d want=%d", after.IdleTimeoutSeconds, tc.want)
			}
			if after.Revision == before.Revision {
				t.Fatal("refresh did not publish a new revision")
			}
			// Core normalizes idle labels when a positive recorded policy exists.
			before.LastUsedAt, before.Revision, before.IdleTimeoutSeconds = after.LastUsedAt, after.Revision, tc.want
			if tc.recorded > 0 {
				before.Labels = CloneLabels(before.Labels)
				before.Labels["idle_timeout"] = "180"
				before.Labels["idle_timeout_secs"] = "180"
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("refresh changed ownership or endpoint metadata: before=%#v after=%#v", before, after)
			}
		})
	}
}

func TestValidateSandboxOwnershipMetadata(t *testing.T) {
	const provider = "example-sandbox"
	claim := core.LeaseClaim{LeaseID: "lease-1", ProviderScope: "scope-1"}
	matching := map[string]string{"crabbox.provider": provider, "crabbox.scope": claim.ProviderScope, "crabbox.claim": claim.LeaseID}
	for _, tc := range []struct {
		name, id string
		metadata map[string]string
		claim    core.LeaseClaim
		code     int
		message  string
	}{
		{name: "matching", id: "sandbox-1", metadata: matching, claim: claim},
		{name: "empty ID precedes metadata mismatch", metadata: nil, claim: claim, code: 5, message: "example-sandbox returned a sandbox without an id"},
		{name: "empty ID with matching metadata", metadata: matching, claim: claim, code: 5, message: "example-sandbox returned a sandbox without an id"},
		{name: "nil metadata", id: "sandbox-1", claim: claim, code: 4},
		{name: "missing provider", id: "sandbox-1", metadata: map[string]string{"crabbox.scope": "scope-1", "crabbox.claim": "lease-1"}, claim: claim, code: 4},
		{name: "wrong provider", id: "sandbox-1", metadata: map[string]string{"crabbox.provider": "other", "crabbox.scope": "scope-1", "crabbox.claim": "lease-1"}, claim: claim, code: 4},
		{name: "missing scope", id: "sandbox-1", metadata: map[string]string{"crabbox.provider": provider, "crabbox.claim": "lease-1"}, claim: claim, code: 4},
		{name: "scope compared exactly", id: "sandbox-1", metadata: map[string]string{"crabbox.provider": provider, "crabbox.scope": "scope-1 ", "crabbox.claim": "lease-1"}, claim: claim, code: 4},
		{name: "missing lease", id: "sandbox-1", metadata: map[string]string{"crabbox.provider": provider, "crabbox.scope": "scope-1"}, claim: claim, code: 4},
		{name: "lease compared exactly", id: "sandbox-1", metadata: map[string]string{"crabbox.provider": provider, "crabbox.scope": "scope-1", "crabbox.claim": " lease-1"}, claim: claim, code: 4},
		{name: "provider compared exactly", id: "sandbox-1", metadata: map[string]string{"crabbox.provider": provider + " ", "crabbox.scope": "scope-1", "crabbox.claim": "lease-1"}, claim: claim, code: 4},
		{name: "missing keys match empty expectations", id: "sandbox-1", metadata: map[string]string{"crabbox.provider": provider}},
		{name: "explicit empty expectations", id: "sandbox-1", metadata: map[string]string{"crabbox.provider": provider, "crabbox.scope": "", "crabbox.claim": ""}},
		{name: "empty expectations reject nonempty metadata", id: "sandbox-1", metadata: matching, code: 4},
		{name: "whitespace ID accepted verbatim", id: " \t", metadata: matching, claim: claim},
		{name: "whitespace ID diagnostic quoted", id: " \t", claim: claim, code: 4, message: "example-sandbox sandbox \" \\t\" ownership metadata does not match its local claim"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSandboxOwnershipMetadata(provider, tc.id, tc.metadata, tc.claim)
			if tc.code == 0 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var exitErr core.ExitError
			if !core.AsExitError(err, &exitErr) || exitErr.Code != tc.code {
				t.Fatalf("error=%v, want exit %d", err, tc.code)
			}
			want := tc.message
			if want == "" {
				want = "example-sandbox sandbox \"sandbox-1\" ownership metadata does not match its local claim"
			}
			if err.Error() != want {
				t.Fatalf("error=%q want=%q", err.Error(), want)
			}
		})
	}
}

func TestFinishScopedLeaseAdmissionAndProjection(t *testing.T) {
	for _, mode := range []string{"observe", "invalid", "different repo", "reclaim"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			const id = "fixture_resource"
			repo := t.TempDir()
			if err := core.ClaimLeaseForRepoProviderScopePond(id, "", "example", "scope", "pond", repo, 3*time.Minute, false); err != nil {
				t.Fatal(err)
			}
			before, err := core.ReadLeaseClaim(id)
			if err != nil {
				t.Fatal(err)
			}
			invalid := errors.New("adapter scope mismatch")
			calls := 0
			opts := ScopedLeaseFinishOptions{Provider: "example", LeasePrefix: "fixture_", IdleTimeout: 9 * time.Minute,
				ValidateClaim: func(got core.LeaseClaim) error {
					calls++
					if !reflect.DeepEqual(got, before) {
						t.Fatal("validation received different snapshot")
					}
					if mode == "invalid" {
						return invalid
					}
					return nil
				},
			}
			if mode != "observe" {
				opts.RepoRoot = t.TempDir()
			}
			opts.Reclaim = mode == "reclaim"
			lease, resource, slug, err := FinishScopedLease(before, opts)
			if calls != 1 {
				t.Fatalf("validation calls=%d", calls)
			}
			after, readErr := core.ReadLeaseClaim(id)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if mode == "invalid" || mode == "different repo" {
				if err == nil || lease != "" || resource != "" || slug != "" {
					t.Fatalf("rejected result=%q/%q/%q err=%v", lease, resource, slug, err)
				}
				if mode == "invalid" && !errors.Is(err, invalid) {
					t.Fatalf("validation error lost: %v", err)
				}
				if !reflect.DeepEqual(after, before) {
					t.Fatal("rejected admission changed claim")
				}
				return
			}
			if err != nil || lease != id || resource != "resource" || slug != core.NewLeaseSlug(id) {
				t.Fatalf("projection=%q/%q/%q err=%v", lease, resource, slug, err)
			}
			if mode == "observe" {
				if !reflect.DeepEqual(after, before) {
					t.Fatal("empty repository root published claim")
				}
			} else if after.RepoRoot != opts.RepoRoot || after.ProviderScope != before.ProviderScope || after.Pond != before.Pond || after.Slug != before.Slug || after.IdleTimeoutSeconds != 180 {
				t.Fatalf("reclaim changed preserved fields: %#v", after)
			}
		})
	}
}

func TestClaimLifecycleLabels(t *testing.T) {
	for _, tc := range []struct {
		name  string
		claim core.LeaseClaim
		want  map[string]string
	}{
		{"empty", core.LeaseClaim{}, map[string]string{}},
		{"persisted fallback", core.LeaseClaim{IdleTimeoutSeconds: 600, ClaimedAt: " 1970-01-01T00:01:40Z ", LastUsedAt: "1970-01-01T00:03:20.123Z", Labels: map[string]string{"idle_timeout_secs": "300", "private_metadata": "preserve: /exact/path"}}, map[string]string{"idle_timeout": "600", "idle_timeout_secs": "600", "created_at": "100", "last_touched_at": "200", "private_metadata": "preserve: /exact/path"}},
		{"labels retained", core.LeaseClaim{IdleTimeoutSeconds: 0, ClaimedAt: "invalid", LastUsedAt: "invalid", Labels: map[string]string{"created_at": "100", "last_touched_at": "200", "idle_timeout_secs": "300"}}, map[string]string{"created_at": "100", "last_touched_at": "200", "idle_timeout_secs": "300"}},
		{"invalid fallbacks", core.LeaseClaim{ClaimedAt: "invalid", LastUsedAt: "invalid", Labels: map[string]string{"created_at": ""}}, map[string]string{"created_at": ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ClaimLifecycleLabels(tc.claim)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v want %#v", got, tc.want)
			}
			got["fixture"] = "changed"
			if tc.claim.Labels["fixture"] != "" {
				t.Fatal("projection aliases persisted metadata")
			}
		})
	}
}

func TestClaimActivityHoldState(t *testing.T) {
	for _, state := range []string{"cleanup", "deleting", "expired", "released"} {
		claim := core.LeaseClaim{Labels: map[string]string{"state": " " + strings.ToUpper(state) + " "}}
		if got := ClaimActivityHoldState(claim); got != state {
			t.Errorf("hold %q projected as %q", state, got)
		}
		if err := AuthorizeClaimActivity(claim); err == nil {
			t.Errorf("hold %q authorized activity", state)
		}
	}
	for _, state := range []string{"", "ready", "busy", "running", "stopped", "failed", "loading"} {
		if got := ClaimActivityHoldState(core.LeaseClaim{Labels: map[string]string{"state": state}}); got != "" {
			t.Errorf("runtime/activity state %q became logical hold %q", state, got)
		}
	}
	if err := AuthorizeClaimActivity(core.LeaseClaim{Labels: map[string]string{"state": "busy"}}); err != nil {
		t.Fatalf("ordinary activity refused: %v", err)
	}
	if err := AuthorizeClaimActivity(core.LeaseClaim{CheckpointCapture: &core.CheckpointCaptureBinding{ID: "capture"}}); err == nil {
		t.Fatal("activity lost checkpoint exclusion")
	}
}

func TestObservedClaimActivityState(t *testing.T) {
	for _, tc := range []struct {
		name, claimState, recorded, observed, want string
		running, obsolete                          bool
	}{
		{"hold beats running", " RELEASED ", "busy", "running", "released", true, true},
		{"hold beats stopped", "deleting", "ready", "stopped", "deleting", false, false},
		{"running replaces obsolete", "", "provisioning", "ready", "ready", true, true},
		{"running preserves activity", "", "busy", "running", "busy", true, false},
		{"non-running wins", "", "busy", "loading", "loading", false, false},
		{"empty observation", "", "busy", "", "busy", false, true},
		{"unknown observation", "", "ready", "unknown", "ready", false, true},
		{"unknown is literal", "", "busy", " UNKNOWN ", " UNKNOWN ", false, false},
		{"raw recorded activity", "", " Busy ", "running", " Busy ", true, false},
		{"projected state is separate", "failed", "busy", "ready", "busy", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claim := core.LeaseClaim{Labels: map[string]string{"state": tc.claimState}}
			if got := ObservedClaimActivityState(claim, tc.recorded, tc.observed, tc.running, tc.obsolete); got != tc.want {
				t.Fatalf("observed state=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestLegacyLabelLifecycleLabels(t *testing.T) {
	for _, tc := range []struct {
		name, canonical, legacy string
		want                    int
		migrate                 bool
	}{
		{"released override", "7200", "7200", 7200, true},
		{"duration format", "90m", "300", 5400, true},
		{"canonical wins", "300", "7200", 300, false},
		{"legacy fallback", "invalid", "2h", 7200, true},
		{"invalid", "-1", "invalid", 300, false},
		{"subsecond", "0.1s", "300", 300, false},
		{"round seconds", "1.6s", "300", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claim := core.LeaseClaim{IdleTimeoutSeconds: 300, Labels: map[string]string{
				"idle_timeout_secs": tc.canonical, "idle_timeout": tc.legacy,
				"created_at": "100", "last_touched_at": "200", "ttl_secs": "3600", "keep": "true", "metadata": "preserved",
			}}
			override := LegacyLabelIdleTimeout(claim)
			if (override != nil) != tc.migrate || override != nil && *override != time.Duration(tc.want)*time.Second {
				t.Fatalf("override=%v want seconds=%d migrate=%v", override, tc.want, tc.migrate)
			}
			labels := LegacyLabelLifecycleLabels(claim)
			if labels["idle_timeout_secs"] != fmt.Sprint(tc.want) || labels["idle_timeout"] != fmt.Sprint(tc.want) {
				t.Fatalf("policy=%v", labels)
			}
			for _, key := range []string{"created_at", "last_touched_at", "ttl_secs", "keep", "metadata"} {
				if labels[key] != claim.Labels[key] {
					t.Fatalf("changed %s", key)
				}
			}
			labels["metadata"] = "changed"
			if claim.IdleTimeoutSeconds != 300 || claim.Labels["idle_timeout_secs"] != tc.canonical || claim.Labels["metadata"] != "preserved" {
				t.Fatal("projection changed original CAS snapshot")
			}
		})
	}
}
