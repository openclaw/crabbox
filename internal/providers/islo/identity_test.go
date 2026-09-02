package islo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	gosdk "github.com/islo-labs/go-sdk"
	sdkcore "github.com/islo-labs/go-sdk/core"
	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	isloTestResourceID = "0195f3d2-5c1a-7c39-9c1e-6f0f2b7a41cd"
	isloTestKeyName    = "ci-key"
	// isloTestClaimScope is the claim scope the default control-plane endpoint
	// produces. Spelling it out pins the stored format, which core compares for
	// equality against Provider.ClaimScope.
	isloTestClaimScope = "endpoint:" + isloDefaultBaseURL
)

func claimIsloLeaseWithIdentity(t *testing.T, leaseID, slug, name, resourceID, scope string) core.LeaseClaim {
	t.Helper()
	identity := isloIdentity{ID: resourceID, Name: name, CreatedBy: isloTestKeyName, CreatedByEntity: "api_key"}
	_, err := core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurable(leaseID, slug, Config{Provider: isloProvider}, scope,
		Server{Provider: isloProvider, CloudID: resourceID, ImmutableID: resourceID, Name: name, Labels: identity.labels()}, core.SSHTarget{}, t.TempDir(), time.Hour, false, core.LeaseClaim{}, false)
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := resolveExactIsloLeaseClaim(leaseID)
	if err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, err)
	}
	return claim
}

func TestIsloCreateSandboxBindsImmutableProviderIdentity(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeIsloSyncClient{
		createName:      "crabbox-repo-abcdef",
		createID:        isloTestResourceID,
		createdBy:       isloTestKeyName,
		createdByEntity: "api_key",
	}
	restore := swapNewIsloClient(client)
	t.Cleanup(restore)
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{APIKey: "test", BaseURL: "https://api.islo.dev/"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}

	leaseID, name, _, _, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir(), Name: "repo"}, false, "")
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := resolveExactIsloLeaseClaim(leaseID)
	if err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, err)
	}
	if claim.CloudImmutableID != isloTestResourceID {
		t.Fatalf("claim immutable id=%q, want the provider resource id %q", claim.CloudImmutableID, isloTestResourceID)
	}
	// claim.CloudID carries the provider resource id, matching the value the
	// adapter reports as Server.CloudID; the sandbox name lives in a label.
	if claim.CloudID != isloTestResourceID {
		t.Fatalf("claim cloud id=%q, want the provider resource id %q", claim.CloudID, isloTestResourceID)
	}
	if claim.Labels[isloSandboxNameLabel] != name {
		t.Fatalf("claim name label=%q, want sandbox name %q", claim.Labels[isloSandboxNameLabel], name)
	}
	// The scope is written by the same core claim helper every provider uses,
	// from Provider.ClaimScope, so core's own scope comparisons see it too.
	if claim.ProviderScope != isloTestClaimScope || claim.ProviderScope != core.ProviderClaimScope(isloProvider, backend.cfg) {
		t.Fatalf("claim provider scope=%q, want the normalized API endpoint %q", claim.ProviderScope, isloTestClaimScope)
	}
	if claim.Labels[isloCreatedByLabel] != isloTestKeyName || claim.Labels[isloCreatedByEntityLabel] != "api_key" {
		t.Fatalf("claim creator labels=%#v", claim.Labels)
	}
}

func TestIsloAdoptionDoesNotReplaceAConcurrentClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	isolateIsloTestHome(t)
	client := &fakeIsloSyncClient{}
	client.registerSandbox(isloTeardownName, isloTestResourceID)
	var concurrent core.LeaseClaim
	client.getSandboxHook = func() {
		client.getSandboxHook = nil
		concurrent = claimIsloLeaseWithIdentity(t, isloTeardownLeaseID, "other", isloTeardownName,
			"0195f3d2-5c1a-7c39-9c1e-000000000000", "endpoint:https://other.example.test")
	}
	backend := newIsloTeardownBackend(t, client, io.Discard)

	if _, _, _, err := backend.resolveLeaseIDForRepo(context.Background(), client, isloTeardownLeaseID, t.TempDir(), true); err == nil || !strings.Contains(err.Error(), "claim changed") {
		t.Errorf("err=%v, want concurrent claim publication to prevent adoption", err)
	}
	got, ok, err := resolveExactIsloLeaseClaim(isloTeardownLeaseID)
	if err != nil || !ok || !reflect.DeepEqual(got, concurrent) {
		t.Fatalf("claim changed during adoption: got=%#v want=%#v ok=%t err=%v", got, concurrent, ok, err)
	}
}

func TestIsloCreateDoesNotDeleteAConcurrentClaim(t *testing.T) {
	for _, claimedID := range []string{isloTestResourceID, "0195f3d2-5c1a-7c39-9c1e-000000000000"} {
		t.Run(claimedID, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			isolateIsloTestHome(t)
			client := &fakeIsloSyncClient{createName: isloTeardownName, createID: isloTestResourceID}
			var concurrent core.LeaseClaim
			client.createSandboxHook = func() {
				concurrent = claimIsloLeaseWithIdentity(t, isloTeardownLeaseID, "other", isloTeardownName, claimedID, isloTestClaimScope)
			}
			backend := newIsloTeardownBackend(t, client, io.Discard)

			if _, _, _, _, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir(), Name: "repo"}, true, ""); err == nil {
				t.Error("concurrent claim publication must prevent creation from taking ownership")
			}
			if client.deleteCalls != 0 {
				t.Errorf("deleted names=%#v, want rollback refused for a competing claim", client.deletedNames)
			}
			got, ok, err := resolveExactIsloLeaseClaim(isloTeardownLeaseID)
			if err != nil || !ok || !reflect.DeepEqual(got, concurrent) {
				t.Fatalf("competing claim changed: got=%#v want=%#v ok=%t err=%v", got, concurrent, ok, err)
			}
		})
	}
}

func TestIsloCreateRollsBackAfterCanceledPublication(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	isolateIsloTestHome(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &fakeIsloSyncClient{createName: isloTeardownName, createID: isloTestResourceID, createSandboxHook: cancel}
	backend := newIsloTeardownBackend(t, client, io.Discard)

	if _, _, _, _, err := backend.createSandbox(ctx, client, Repo{Root: t.TempDir(), Name: "repo"}, false, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want the canceled publication reported", err)
	}
	if client.deleteCalls != 1 || !client.deletedIDs[isloTestResourceID] {
		t.Fatalf("rollback deletes=%d tombstoned=%t", client.deleteCalls, client.deletedIDs[isloTestResourceID])
	}
	requireIsloClaimDropped(t, isloTeardownLeaseID)
}

func TestIsloRunCleanupKeepsAcquiredOwnership(t *testing.T) {
	for _, change := range []string{"resource", "repository", "metadata"} {
		t.Run(change, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			isolateIsloTestHome(t)
			client := &fakeIsloSyncClient{
				createName: isloTeardownName, createID: isloTestResourceID,
				execErrOnCommand: errors.New("synthetic workload failure"), execErrOnCommandContains: "islo-cleanup-owner-probe",
			}
			var changed core.LeaseClaim
			client.execErrOnCommandHook = func() {
				acquired, ok, err := resolveExactIsloLeaseClaim(isloTeardownLeaseID)
				if err != nil || !ok {
					t.Fatalf("claim before workload: ok=%t err=%v", ok, err)
				}
				switch change {
				case "resource":
					replacement := acquired
					replacement.CloudID = "0195f3d2-5c1a-7c39-9c1e-000000000000"
					replacement.CloudImmutableID = replacement.CloudID
					if err := core.ReplaceLeaseClaimIfUnchanged(isloTeardownLeaseID, acquired, replacement); err != nil {
						t.Fatal(err)
					}
					client.registerSandbox(isloTeardownName, replacement.CloudID)
				case "repository":
					if err := core.ClaimLeaseForRepoProvider(isloTeardownLeaseID, acquired.Slug, isloProvider, t.TempDir(), time.Hour, true); err != nil {
						t.Fatal(err)
					}
				case "metadata":
					if err := updateLeaseClaimTailscale(isloTeardownLeaseID, "100.64.7.7", "lease.tailnet.example"); err != nil {
						t.Fatal(err)
					}
				}
				changed, ok, err = resolveExactIsloLeaseClaim(isloTeardownLeaseID)
				if err != nil || !ok {
					t.Fatalf("claim after change: ok=%t err=%v", ok, err)
				}
			}
			backend := newIsloTeardownBackend(t, client, io.Discard)

			_, err := backend.Run(context.Background(), RunRequest{
				Repo: Repo{Root: t.TempDir(), Name: "repo"}, NoSync: true, Command: []string{"printf", "islo-cleanup-owner-probe"},
			})
			if err == nil || !strings.Contains(err.Error(), "synthetic workload failure") {
				t.Fatalf("err=%v, want the synthetic workload failure", err)
			}
			if change == "metadata" {
				if client.deleteCalls != 1 {
					t.Fatalf("delete calls=%d, want cleanup after a legitimate metadata refresh", client.deleteCalls)
				}
				requireIsloClaimDropped(t, isloTeardownLeaseID)
				return
			}
			if client.deleteCalls != 0 {
				t.Errorf("deleted names=%#v, want the replacement owner's resource untouched", client.deletedNames)
			}
			current, ok, err := resolveExactIsloLeaseClaim(isloTeardownLeaseID)
			if err != nil || !ok || !reflect.DeepEqual(current, changed) {
				t.Fatalf("replacement claim changed: got=%#v want=%#v ok=%t err=%v", current, changed, ok, err)
			}
		})
	}
}

// TestIsloInspectJSONPinsAuthoritativeIdentity pins the `crabbox inspect --json`
// contract: the immutable provider resource id is reported alongside the lease
// id, provider, and readiness.
func TestIsloInspectJSONPinsAuthoritativeIdentity(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const leaseID = "isb_crabbox-repo-abcdef"
	claimIsloLeaseWithIdentity(t, leaseID, "web", "crabbox-repo-abcdef", isloTestResourceID, isloTestClaimScope)
	client := &fakeIsloSyncClient{byID: map[string]*gosdk.SandboxResponse{
		isloTestResourceID: {ID: isloTestResourceID, Name: "crabbox-repo-abcdef", Status: "running", Image: "ubuntu:24.04"},
	}}
	restore := swapNewIsloClient(client)
	t.Cleanup(restore)
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{APIKey: "test"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}

	view, err := backend.Status(context.Background(), StatusRequest{ID: leaseID})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]any{
		"id":                 leaseID,
		"provider":           isloProvider,
		"ready":              true,
		"providerResourceId": isloTestResourceID,
	} {
		got, ok := decoded[field]
		if !ok {
			t.Fatalf("inspect JSON is missing %q: %s", field, encoded)
		}
		if got != want {
			t.Fatalf("inspect JSON %q=%v, want %v", field, got, want)
		}
	}
	if decoded["serverId"] != "crabbox-repo-abcdef" {
		t.Fatalf("inspect JSON serverId=%v, want the sandbox name", decoded["serverId"])
	}
}

// TestIsloStatusResolvesThroughImmutableID proves the adapter addresses the
// resource by its immutable id rather than by name: the by-name lookup is wired
// to fail outright, so a status read that still succeeds can only have gone
// through `GET /sandboxes/-/by-id/{id}`.
func TestIsloStatusResolvesThroughImmutableID(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const leaseID = "isb_crabbox-repo-abcdef"
	claimIsloLeaseWithIdentity(t, leaseID, "web", "crabbox-repo-abcdef", isloTestResourceID, isloTestClaimScope)
	client := &fakeIsloSyncClient{
		getSandboxErr: errors.New("by-name lookup must not be used when the claim carries an immutable id"),
		byID: map[string]*gosdk.SandboxResponse{
			isloTestResourceID: {ID: isloTestResourceID, Name: "crabbox-repo-abcdef", Status: "running"},
		},
	}
	restore := swapNewIsloClient(client)
	t.Cleanup(restore)
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{APIKey: "test"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}

	view, err := backend.Status(context.Background(), StatusRequest{ID: leaseID})
	if err != nil {
		t.Fatal(err)
	}
	if view.ProviderResourceID != isloTestResourceID || view.ServerID != "crabbox-repo-abcdef" {
		t.Fatalf("resource id=%q server id=%q", view.ProviderResourceID, view.ServerID)
	}
	if len(client.byIDCalls) != 1 || client.byIDCalls[0] != isloTestResourceID {
		t.Fatalf("by-id calls=%#v, want exactly one lookup of the claimed resource", client.byIDCalls)
	}
}

func TestRequireIsloIdentityMatch(t *testing.T) {
	claim := core.LeaseClaim{
		LeaseID:          "isb_crabbox-repo-abcdef",
		Provider:         isloProvider,
		CloudID:          isloTestResourceID,
		CloudImmutableID: isloTestResourceID,
		Labels: map[string]string{
			isloSandboxNameLabel:     "crabbox-repo-abcdef",
			isloCreatedByLabel:       isloTestKeyName,
			isloCreatedByEntityLabel: "api_key",
		},
	}
	for name, tc := range map[string]struct {
		observed     isloIdentity
		wantErr      string
		wantAdvisory string
	}{
		"same resource": {
			observed: isloIdentity{ID: isloTestResourceID, Name: "crabbox-repo-abcdef", CreatedBy: isloTestKeyName},
		},
		"response without id or creator": {
			observed: isloIdentity{Name: "crabbox-repo-abcdef"},
		},
		"different resource under the claimed name": {
			observed: isloIdentity{ID: "0195f3d2-5c1a-7c39-9c1e-000000000000", Name: "crabbox-repo-abcdef"},
			wantErr:  "this lease does not own",
		},
		// created_by is the API key name. Attribution only corroborates
		// ownership, so a difference must never make a lease undeletable: it is
		// reported and the caller continues.
		"different api key name": {
			observed:     isloIdentity{ID: isloTestResourceID, Name: "crabbox-repo-abcdef", CreatedBy: "other-key"},
			wantAdvisory: `created_by is now "other-key"`,
		},
		"different creator entity": {
			observed:     isloIdentity{ID: isloTestResourceID, Name: "crabbox-repo-abcdef", CreatedByEntity: "user"},
			wantAdvisory: `created_by_entity is now "user"`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			advisory, err := requireIsloIdentityMatch(claim, tc.observed)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("err=%v, want nil", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err=%v, want %q", err, tc.wantErr)
			}
			if tc.wantAdvisory == "" {
				if advisory != "" {
					t.Fatalf("advisory=%q, want none", advisory)
				}
				return
			}
			if !strings.Contains(advisory, tc.wantAdvisory) || !strings.Contains(advisory, "advisory only") {
				t.Fatalf("advisory=%q, want %q reported as advisory", advisory, tc.wantAdvisory)
			}
		})
	}
}

func TestRequireIsloClaimScope(t *testing.T) {
	for name, tc := range map[string]struct {
		bound   string
		scope   string
		wantErr bool
	}{
		"same endpoint":                {bound: isloTestClaimScope, scope: isloTestClaimScope},
		"legacy claim without a scope": {bound: "", scope: isloTestClaimScope},
		"different endpoint":           {bound: isloTestClaimScope, scope: "endpoint:https://other.example", wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			claim := core.LeaseClaim{LeaseID: "isb_crabbox-repo-abcdef", Provider: isloProvider, ProviderScope: tc.bound}
			err := requireIsloClaimScope(claim, tc.scope)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err=%v, wantErr=%t", err, tc.wantErr)
			}
		})
	}
}

// TestIsloClaimScopeNormalizesBaseURL pins that the scope goes through the
// shared endpoint normalizer every sandbox provider uses. Without it two
// spellings of the same endpoint produce different scopes, and `crabbox stop`
// refuses to act on the lease it created.
func TestIsloClaimScopeNormalizesBaseURL(t *testing.T) {
	for name, want := range map[string]string{
		"":                          isloTestClaimScope,
		"https://api.islo.dev/":     isloTestClaimScope,
		" https://api.islo.dev  ":   isloTestClaimScope,
		"https://API.Islo.DEV":      isloTestClaimScope,
		"https://api.islo.dev:443":  isloTestClaimScope,
		"https://api.islo.dev/?x=1": isloTestClaimScope,
		"https://user@api.islo.dev": isloTestClaimScope,
		"https://other.example/":    "endpoint:https://other.example",
	} {
		if got := isloClaimScope(Config{Islo: IsloConfig{BaseURL: name}}); got != want {
			t.Fatalf("scope(%q)=%q, want %q", name, got, want)
		}
	}
}

// TestIsloStatusReportsADeletedSandboxFromItsTombstone pins the inspect contract
// for a stale lease. The by-id lookup keeps answering after a delete, so
// inspection reports state "deleted" and ready=false instead of failing with a
// bare not-found error.
func TestIsloStatusReportsADeletedSandboxFromItsTombstone(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	isolateIsloTestHome(t)
	const leaseID = "isb_crabbox-repo-abcdef"
	claimIsloLeaseWithIdentity(t, leaseID, "web", "crabbox-repo-abcdef", isloTestResourceID, isloTestClaimScope)
	client := &fakeIsloSyncClient{}
	client.registerSandbox("crabbox-repo-abcdef", isloTestResourceID)
	client.markDeleted("crabbox-repo-abcdef")
	restore := swapNewIsloClient(client)
	t.Cleanup(restore)
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{APIKey: "test"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}

	view, err := backend.Status(context.Background(), StatusRequest{ID: leaseID})
	if err != nil {
		t.Fatal(err)
	}
	if view.State != "deleted" || view.Ready {
		t.Fatalf("state=%q ready=%t, want the tombstone reported as deleted and not ready", view.State, view.Ready)
	}
	if view.ProviderResourceID != isloTestResourceID {
		t.Fatalf("resource id=%q, want the claimed resource id", view.ProviderResourceID)
	}
}

// TestIsloStatusFlagsAResourceIDMismatch covers the fallback read: when the
// claimed id is not addressable the lookup falls back to the name, and the
// resource that answers may not be the one the lease owns. That must be visible
// rather than published as providerResourceId in silence.
func TestIsloStatusFlagsAResourceIDMismatch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	isolateIsloTestHome(t)
	const leaseID = "isb_crabbox-repo-abcdef"
	const otherID = "0195f3d2-5c1a-7c39-9c1e-000000000000"
	claimIsloLeaseWithIdentity(t, leaseID, "web", "crabbox-repo-abcdef", isloTestResourceID, isloTestClaimScope)
	client := &fakeIsloSyncClient{byID: map[string]*gosdk.SandboxResponse{isloTestResourceID: nil}}
	client.registerSandbox("crabbox-repo-abcdef", otherID)
	if err := updateLeaseClaimTailscale(leaseID, "100.64.7.7", "lease.tailnet.example"); err != nil {
		t.Fatal(err)
	}
	restore := swapNewIsloClient(client)
	t.Cleanup(restore)
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{APIKey: "test"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}

	view, err := backend.Status(context.Background(), StatusRequest{ID: leaseID})
	if err != nil {
		t.Fatal(err)
	}
	if view.Labels["islo_resource_id_mismatch"] != "true" || view.Labels["islo_claimed_resource_id"] != isloTestResourceID {
		t.Fatalf("labels=%#v, want the mismatch and the claimed id surfaced", view.Labels)
	}
	// The documented contract is that automation may key off providerResourceId,
	// so a foreign resource's id must never be reported there. Withholding it is
	// what makes the mismatch labels the only channel for the detail.
	if view.ProviderResourceID != "" {
		t.Fatalf("providerResourceId=%q, want it withheld rather than naming a resource the lease does not own (other=%q claimed=%q)",
			view.ProviderResourceID, otherID, isloTestResourceID)
	}
	if view.Ready {
		t.Error("a different resource must not make this lease ready")
	}
	if len(client.execRequests) != 0 {
		t.Errorf("executed %d remote commands against a mismatched resource", len(client.execRequests))
	}
	if _, err := backend.Status(context.Background(), StatusRequest{ID: leaseID, Wait: true, WaitTimeout: time.Millisecond}); err == nil {
		t.Error("waiting for a mismatched resource must fail")
	}
}

func TestIsloStatusRejectsUnboundByIDResponse(t *testing.T) {
	for _, responseID := range []string{"", "0195f3d2-5c1a-7c39-9c1e-000000000000"} {
		t.Run(responseID, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			isolateIsloTestHome(t)
			claimIsloLeaseWithIdentity(t, isloTeardownLeaseID, "web", isloTeardownName, isloTestResourceID, isloTestClaimScope)
			client := &fakeIsloSyncClient{byID: map[string]*gosdk.SandboxResponse{
				isloTestResourceID: {ID: responseID, Name: isloTeardownName, Status: "running"},
			}}
			backend := newIsloTeardownBackend(t, client, io.Discard)

			if _, err := backend.Status(context.Background(), StatusRequest{ID: isloTeardownLeaseID}); err == nil || !strings.Contains(err.Error(), "by-id response") {
				t.Fatalf("err=%v, want an invalid by-id response to fail closed", err)
			}
		})
	}
}

func TestIsloStatusTailscaleUsesVerifiedCurrentName(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	isolateIsloTestHome(t)
	claimIsloLeaseWithIdentity(t, isloTeardownLeaseID, "web", isloTeardownName, isloTestResourceID, isloTestClaimScope)
	if err := updateLeaseClaimTailscale(isloTeardownLeaseID, "100.64.7.7", ""); err != nil {
		t.Fatal(err)
	}
	const currentName = "crabbox-repo-fedcba"
	client := &fakeIsloSyncClient{}
	client.registerSandbox(currentName, isloTestResourceID)
	client.registerSandbox(isloTeardownName, "0195f3d2-5c1a-7c39-9c1e-000000000000")
	backend := newIsloTeardownBackend(t, client, io.Discard)

	if _, err := backend.Status(context.Background(), StatusRequest{ID: isloTeardownLeaseID}); err != nil {
		t.Fatal(err)
	}
	if len(client.getSandboxNames) != 1 || client.getSandboxNames[0] != currentName {
		t.Fatalf("status checked names=%#v, want only the current verified name %s", client.getSandboxNames, currentName)
	}
}

func TestIsloStatusRejectsIdentityChangeBeforeTailscaleCheck(t *testing.T) {
	for _, responseID := range []string{"", "0195f3d2-5c1a-7c39-9c1e-000000000000"} {
		t.Run(responseID, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			isolateIsloTestHome(t)
			claimIsloLeaseWithIdentity(t, isloTeardownLeaseID, "web", isloTeardownName, isloTestResourceID, isloTestClaimScope)
			if err := updateLeaseClaimTailscale(isloTeardownLeaseID, "100.64.7.7", ""); err != nil {
				t.Fatal(err)
			}
			client := &fakeIsloSyncClient{
				byID:       map[string]*gosdk.SandboxResponse{isloTestResourceID: {ID: isloTestResourceID, Name: isloTeardownName, Status: "running"}},
				getSandbox: &gosdk.SandboxResponse{ID: responseID, Name: isloTeardownName, Status: "running"},
			}
			backend := newIsloTeardownBackend(t, client, io.Discard)

			if _, err := backend.Status(context.Background(), StatusRequest{ID: isloTeardownLeaseID}); err == nil {
				t.Error("status must refuse a changed identity before executing a Tailscale command")
			}
			if len(client.execRequests) != 0 {
				t.Errorf("executed %d commands after the sandbox identity changed", len(client.execRequests))
			}
		})
	}
}

func TestIsloNotFoundRecognizesBothErrorShapes(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"nil":                  {},
		"typed sdk not found":  {err: &gosdk.NotFoundError{APIError: sdkcore.NewAPIError(http.StatusNotFound, errors.New("missing"))}, want: true},
		"raw api error 404":    {err: sdkcore.NewAPIError(http.StatusNotFound, errors.New("missing")), want: true},
		"wrapped api error":    {err: fmt.Errorf("islo get sandbox: %w", sdkcore.NewAPIError(http.StatusNotFound, errors.New("missing"))), want: true},
		"raw api error 503":    {err: sdkcore.NewAPIError(http.StatusServiceUnavailable, errors.New("unavailable"))},
		"transport failure":    {err: errors.New("connection reset")},
		"conflict is not gone": {err: sdkcore.NewAPIError(http.StatusConflict, errors.New("conflict"))},
	} {
		t.Run(name, func(t *testing.T) {
			if got := isloNotFound(tc.err); got != tc.want {
				t.Fatalf("isloNotFound(%v)=%t, want %t", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsloSandboxDeletedRequiresTerminalState(t *testing.T) {
	for name, tc := range map[string]struct {
		sandbox *gosdk.SandboxResponse
		want    bool
	}{
		"nil response":                 {},
		"running":                      {sandbox: &gosdk.SandboxResponse{ID: isloTestResourceID, Status: "running"}},
		"status deleted":               {sandbox: &gosdk.SandboxResponse{ID: isloTestResourceID, Status: "deleted"}, want: true},
		"status deleted mixed case":    {sandbox: &gosdk.SandboxResponse{ID: isloTestResourceID, Status: "Deleted"}, want: true},
		"deleted_at without status":    {sandbox: &gosdk.SandboxResponse{ID: isloTestResourceID, Status: "stopping", DeletedAt: stringValue("2026-01-01T00:00:01Z")}},
		"blank deleted_at is no proof": {sandbox: &gosdk.SandboxResponse{ID: isloTestResourceID, Status: "stopping", DeletedAt: stringValue("   ")}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := isloSandboxDeleted(tc.sandbox); got != tc.want {
				t.Fatalf("isloSandboxDeleted=%t, want %t", got, tc.want)
			}
		})
	}
}
