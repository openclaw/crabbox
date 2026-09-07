package modal

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	core "github.com/openclaw/crabbox/internal/cli"
)

func ownedModalFixture(t *testing.T) (*modalBackend, *fakeModalAPI, core.LeaseClaim, Repo) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeModalAPI{}
	withFakeModalAPI(t, fake)
	b := NewModalBackend(Provider{}.Spec(), newTestConfig(), testRuntime()).(*modalBackend)
	repo := Repo{Name: "repo", Root: t.TempDir()}
	claim, _, err := b.createSandbox(t.Context(), fake, repo, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	fake.verbs = nil
	return b, fake, claim, repo
}

func assertModalClaim(t *testing.T, want core.LeaseClaim) {
	t.Helper()
	got, exists, err := core.ReadLeaseClaimWithPresence(want.LeaseID)
	if err != nil || !exists || !reflect.DeepEqual(got, want) {
		t.Fatalf("claim changed: got=%#v exists=%t err=%v", got, exists, err)
	}
}

func assertModalFence(t *testing.T, leaseID string) {
	t.Helper()
	lock := flock.New(filepath.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "claim-locks", leaseID+".json.lock"))
	got, err := lock.TryLock()
	if got {
		_ = lock.Unlock()
	}
	if err != nil || got {
		t.Fatalf("provider action is not claim-fenced: acquired=%t err=%v", got, err)
	}
}

func TestModalStopRequiresExactClaimBeforeProviderAccess(t *testing.T) {
	for _, id := range []string{"sb-123", "cbx_0123456789ab", "orphan-crab"} {
		t.Run(id, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			fake := &fakeModalAPI{}
			withFakeModalAPI(t, fake)
			b := NewModalBackend(Provider{}.Spec(), newTestConfig(), testRuntime()).(*modalBackend)
			if err := b.Stop(t.Context(), StopRequest{ID: id}); err == nil || len(fake.verbs) != 0 {
				t.Fatalf("claimless stop err=%v verbs=%v", err, fake.verbs)
			}
		})
	}
}

func TestModalCreatedClaimAndStopBindExactResource(t *testing.T) {
	for _, idKind := range []string{"lease", "slug", "raw"} {
		t.Run(idKind, func(t *testing.T) {
			b, fake, claim, _ := ownedModalFixture(t)
			binding, _, err := modalClaimBinding(claim)
			if err != nil || binding.ID != fake.sandbox.ID || binding.Scope != fake.sandbox.Scope {
				t.Fatalf("binding=%#v err=%v", binding, err)
			}
			fake.onTerminate = func(got modalBinding) error {
				assertModalFence(t, claim.LeaseID)
				if got != binding {
					t.Errorf("termination retargeted: %#v", got)
				}
				return nil
			}
			id := map[string]string{"lease": claim.LeaseID, "slug": claim.Slug, "raw": claim.CloudID}[idKind]
			if err := b.Stop(t.Context(), StopRequest{ID: id}); err != nil {
				t.Fatal(err)
			}
			if fake.terminateRemaining < 90*time.Second || fake.terminateRemaining > modalCleanupTimeout {
				t.Fatalf("native terminal confirmation budget=%s", fake.terminateRemaining)
			}
			if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); err != nil || exists || fake.sandbox.Status != "finished" {
				t.Fatalf("cleanup exists=%t err=%v status=%s", exists, err, fake.sandbox.Status)
			}
		})
	}
}

func TestModalRawIDRejectsDuplicateClaims(t *testing.T) {
	b, fake, claim, _ := ownedModalFixture(t)
	labels := maps.Clone(claim.Labels)
	labels["lease"], labels["slug"] = "cbx_0123456789ab", "duplicate-crab"
	server := Server{Provider: providerName, CloudID: claim.CloudID, Labels: labels}
	if _, err := core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurableAfter(labels["lease"], labels["slug"], b.cfg, claim.ProviderScope, server, core.SSHTarget{}, t.TempDir(), 0, false, core.LeaseClaim{}, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := b.Stop(t.Context(), StopRequest{ID: claim.CloudID}); err == nil || !strings.Contains(err.Error(), "ambiguous") || len(fake.verbs) != 0 {
		t.Fatalf("duplicate sandbox claims accepted: err=%v verbs=%v", err, fake.verbs)
	}
	assertModalClaim(t, claim)
}

func TestModalStopRejectsIncompleteAndConflictingClaims(t *testing.T) {
	mutations := map[string]func(*core.LeaseClaim){
		"legacy scope":     func(c *core.LeaseClaim) { c.ProviderScope = "" },
		"sandbox ID":       func(c *core.LeaseClaim) { c.CloudID = "" },
		"foreign provider": func(c *core.LeaseClaim) { c.Provider = "other" },
		"slug":             func(c *core.LeaseClaim) { c.Slug = "" },
		"provider label":   func(c *core.LeaseClaim) { c.Labels["provider"] = "other" },
		"lease label":      func(c *core.LeaseClaim) { c.Labels["lease"] = "cbx_ffffffffffff" },
		"slug label":       func(c *core.LeaseClaim) { c.Labels["slug"] = "other-crab" },
		"app label":        func(c *core.LeaseClaim) { c.Labels["app"] = "other-app" },
		"crabbox tag":      func(c *core.LeaseClaim) { delete(c.Labels, "crabbox") },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			b, fake, claim, _ := ownedModalFixture(t)
			changed := claim
			changed.Labels = maps.Clone(claim.Labels)
			mutate(&changed)
			if err := core.ReplaceLeaseClaimIfUnchanged(claim.LeaseID, claim, changed); err != nil {
				t.Fatal(err)
			}
			changed, _, _ = core.ReadLeaseClaimWithPresence(claim.LeaseID)
			if err := b.Stop(t.Context(), StopRequest{ID: claim.LeaseID}); err == nil || len(fake.verbs) != 0 {
				t.Fatalf("invalid claim accepted: err=%v verbs=%v", err, fake.verbs)
			}
			assertModalClaim(t, changed)
		})
	}
}

func TestModalCleanupKeepsOriginalClaimSnapshot(t *testing.T) {
	_, fake, claim, _ := ownedModalFixture(t)
	changed := claim
	changed.CloudID = "sb-successor"
	if err := core.ReplaceLeaseClaimIfUnchanged(claim.LeaseID, claim, changed); err != nil {
		t.Fatal(err)
	}
	changed, _, _ = core.ReadLeaseClaimWithPresence(claim.LeaseID)
	if err := removeModalClaim(t.Context(), fake, claim); err == nil || len(fake.verbs) != 0 {
		t.Fatalf("stale cleanup err=%v verbs=%v", err, fake.verbs)
	}
	assertModalClaim(t, changed)
}

func TestModalTerminationFailureRetainsExactClaim(t *testing.T) {
	for _, failure := range []error{errors.New("inventory unavailable"), errors.New("scope changed"), errors.New("termination unconfirmed"), context.Canceled} {
		t.Run(failure.Error(), func(t *testing.T) {
			b, fake, claim, _ := ownedModalFixture(t)
			fake.terminateErr = failure
			if err := b.Stop(t.Context(), StopRequest{ID: claim.LeaseID}); err == nil {
				t.Fatal("uncertain termination accepted")
			}
			assertModalClaim(t, claim)
		})
	}
}

func TestModalReclaimChangesRepositoryWithoutRetargeting(t *testing.T) {
	b, fake, claim, _ := ownedModalFixture(t)
	newRepo := t.TempDir()
	if _, err := b.resolveOwnedSandbox(t.Context(), fake, claim.CloudID, newRepo, false); err == nil {
		t.Fatal("cross-repo reuse accepted without reclaim")
	}
	fake.onInspect = func(modalBinding) error { assertModalFence(t, claim.LeaseID); return nil }
	updated, err := b.resolveOwnedSandbox(t.Context(), fake, claim.CloudID, newRepo, true)
	if err != nil {
		t.Fatal(err)
	}
	if updated.RepoRoot != newRepo || updated.CloudID != claim.CloudID || updated.ProviderScope != claim.ProviderScope || updated.Provider != claim.Provider || updated.Slug != claim.Slug {
		t.Fatalf("reclaim retargeted ownership: %#v", updated)
	}
}

func TestModalRollbackPreservesAppearingClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeModalAPI{}
	withFakeModalAPI(t, fake)
	b := NewModalBackend(Provider{}.Spec(), newTestConfig(), testRuntime()).(*modalBackend)
	old := publishModalClaim
	var successor core.LeaseClaim
	publishModalClaim = func(_ *modalBackend, _ context.Context, _ modalAPI, binding modalBinding, _ modalSandbox, repo Repo, _ bool) (core.LeaseClaim, error) {
		if err := core.ClaimLeaseForRepoProvider(binding.LeaseID, binding.Slug, "other", repo.Root, 0, false); err != nil {
			t.Fatal(err)
		}
		successor, _, _ = core.ReadLeaseClaimWithPresence(binding.LeaseID)
		return core.LeaseClaim{}, errors.New("claim publication failed")
	}
	t.Cleanup(func() { publishModalClaim = old })
	_, _, err := b.createSandbox(t.Context(), fake, Repo{Name: "repo", Root: t.TempDir()}, true, false, "")
	if err == nil || containsVerb(fake.verbs, "terminate") {
		t.Fatalf("rollback destroyed a newly claimed resource: err=%v verbs=%v", err, fake.verbs)
	}
	assertModalClaim(t, successor)
}

func TestModalClaimPublicationAndAbsentRollbackAreFenced(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeModalAPI{}
	withFakeModalAPI(t, fake)
	b := NewModalBackend(Provider{}.Spec(), newTestConfig(), testRuntime()).(*modalBackend)
	fake.onInspect = func(binding modalBinding) error {
		assertModalFence(t, binding.LeaseID)
		return errors.New("inspection failed")
	}
	fake.onTerminate = func(binding modalBinding) error {
		assertModalFence(t, binding.LeaseID)
		return nil
	}
	_, _, err := b.createSandbox(t.Context(), fake, Repo{Name: "repo", Root: t.TempDir()}, false, false, "")
	if err == nil || !strings.Contains(err.Error(), "inspection failed") || !reflect.DeepEqual(fake.verbs, []string{"create", "inspect", "terminate"}) {
		t.Fatalf("rollback err=%v verbs=%v", err, fake.verbs)
	}
}
