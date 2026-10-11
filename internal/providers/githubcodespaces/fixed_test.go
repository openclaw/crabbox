package githubcodespaces

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// Exercise the real REST client, including create payloads and identity reads.
type fixedCodespacesFixture struct {
	stopPending, deletePending, listFails, dirtyAfterStop, identityAfterStop bool
	failInventoryAt, inventoryCalls                                          int
	mu                                                                       sync.Mutex
	b                                                                        *backend
	req                                                                      core.AcquireRequest
	user                                                                     githubUser
	items                                                                    map[string]map[string]any
	creates                                                                  int
	stops, deletes                                                           []string
	loseReply, hideCreated, duplicate                                        bool
	journaled                                                                bool
}

func newFixedCodespacesFixture(t *testing.T) *fixedCodespacesFixture {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fixedCodespacesFixture{user: fakeGitHubUser("alice"), items: map[string]map[string]any{}}
	f.req = core.AcquireRequest{Repo: core.Repo{Root: t.TempDir(), Name: "my-app"}, RequestedLeaseID: "cbx_0123456789ab", RequestedSlug: "fixed-box", Keep: true}
	s := httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(s.Close)
	f.b = newTestBackend(t, newFakeCodespacesClient(), &fakeGH{login: "alice", token: "test-placeholder"}).backend
	f.b.cfg.GitHubCodespaces.APIURL = s.URL
	f.b.clientFactory = func(token string) codespacesAPI {
		return newClient(f.b.cfg.GitHubCodespaces, core.Runtime{HTTP: s.Client()}, token)
	}
	f.b.readyTimeout = time.Second
	return f
}

func (f *fixedCodespacesFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case r.Method == "GET" && r.URL.Path == "/user":
		reply(f.user)
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/codespaces/machines"):
		reply(map[string]any{"machines": []map[string]string{{"name": "standardLinux32gb"}}})
	case r.Method == "GET" && r.URL.Path == "/user/codespaces":
		f.inventoryCalls++
		if f.listFails || (f.failInventoryAt != 0 && f.inventoryCalls == f.failInventoryAt) {
			w.WriteHeader(503)
			reply(map[string]string{"message": "inventory unavailable"})
			return
		}
		items := []map[string]any{}
		for _, item := range f.items {
			if !f.hideCreated {
				items = append(items, item)
			}
		}
		reply(map[string]any{"codespaces": items, "total_count": len(items)})
	case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/repos/") && strings.HasSuffix(r.URL.Path, "/codespaces"):
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.creates++
		claim, ok, err := core.ReadLeaseClaimWithPresence(f.req.RequestedLeaseID)
		f.journaled = err == nil && ok && claim.FixedCreateIntent != nil && claim.FixedCreateIntent.Journal != nil && claim.FixedCreateIntent.Journal.Phase == "submitting" && claim.Labels[labelDisplayName] == body["display_name"]
		item := map[string]any{"id": 101, "name": "cs-owned", "display_name": body["display_name"], "state": "Available", "environment_id": "env-owned", "retention_period_minutes": 1440, "owner": f.user,
			"repository": map[string]any{"id": 1001, "full_name": "example-org/my-app"}, "machine": map[string]any{"name": body["machine"]},
			"git_status": map[string]any{"ref": "main", "ahead": 0, "has_unpushed_changes": false, "has_uncommitted_changes": false}}
		f.items["cs-owned"] = item
		if f.duplicate {
			other := cloneCodespaceJSON(item)
			other["id"], other["name"], other["environment_id"] = 102, "cs-duplicate", "env-duplicate"
			f.items["cs-duplicate"] = other
		}
		if f.loseReply {
			w.WriteHeader(503)
			reply(map[string]string{"message": "response lost"})
			return
		}
		reply(item)
	default:
		suffix := strings.TrimPrefix(r.URL.Path, "/user/codespaces/")
		name := strings.Split(suffix, "/")[0]
		item, ok := f.items[name]
		if !ok {
			w.WriteHeader(404)
			reply(map[string]string{"message": "Not Found"})
			return
		}
		switch {
		case r.Method == "GET":
			reply(item)
		case r.Method == "POST" && strings.HasSuffix(suffix, "/stop"):
			f.stops = append(f.stops, name)
			item["state"] = "Shutdown"
			if f.stopPending {
				item["state"] = "ShuttingDown"
			}
			if f.dirtyAfterStop {
				item["git_status"].(map[string]any)["has_uncommitted_changes"] = true
			}
			if f.identityAfterStop {
				item["environment_id"] = "env-replaced"
			}
			reply(item)
		case r.Method == "POST" && strings.HasSuffix(suffix, "/start"):
			item["state"] = "Available"
			reply(item)
		case r.Method == "DELETE":
			f.deletes = append(f.deletes, name)
			if !f.deletePending {
				delete(f.items, name)
			}
			w.WriteHeader(202)
		default:
			w.WriteHeader(400)
			reply(map[string]string{"message": "unexpected route"})
		}
	}
}

func cloneCodespaceJSON(item map[string]any) map[string]any {
	data, _ := json.Marshal(item)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return out
}

func (f *fixedCodespacesFixture) claim(t *testing.T) core.LeaseClaim {
	t.Helper()
	claim, ok, err := core.ReadLeaseClaimWithPresence(f.req.RequestedLeaseID)
	if err != nil || !ok {
		t.Fatalf("claim present=%t: %v", ok, err)
	}
	return claim
}

func TestFixedCodespacesCreateAndReplay(t *testing.T) {
	f := newFixedCodespacesFixture(t)
	first, err := f.b.Acquire(t.Context(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if !f.journaled {
		t.Fatal("create ran without a durable submitting journal and marker")
	}
	replay, err := f.b.Acquire(t.Context(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if f.creates != 1 || first.Server.CloudID != replay.Server.CloudID {
		t.Fatalf("creates=%d first=%s replay=%s", f.creates, first.Server.CloudID, replay.Server.CloudID)
	}
	claim := f.claim(t)
	if claim.FixedCreateIntent.State != "acquired" || claim.CloudImmutableID != "101" {
		t.Fatalf("claim=%+v", claim)
	}
	marker := claim.Labels[labelDisplayName]
	if len(marker) > 48 || !strings.HasPrefix(marker, "cbx_") {
		t.Fatalf("invalid marker %q", marker)
	}
}

func TestFixedCodespacesLostResponseRecovery(t *testing.T) {
	f := newFixedCodespacesFixture(t)
	f.loseReply, f.hideCreated = true, true
	if _, err := f.b.Acquire(t.Context(), f.req); err == nil {
		t.Fatal("expected uncertain create")
	}
	for range 2 {
		if _, err := f.b.Acquire(t.Context(), f.req); err == nil {
			t.Fatal("empty inventory must retain custody")
		}
	}
	if f.creates != 1 {
		t.Fatalf("creates=%d", f.creates)
	}
	f.hideCreated = false
	lease, err := f.b.Acquire(t.Context(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if f.creates != 1 || lease.Server.CloudID != "cs-owned" {
		t.Fatalf("creates=%d lease=%+v", f.creates, lease)
	}
}

func TestFixedCodespacesDuplicateRecoveryFailsClosed(t *testing.T) {
	f := newFixedCodespacesFixture(t)
	f.loseReply, f.duplicate = true, true
	_, _ = f.b.Acquire(t.Context(), f.req)
	_, err := f.b.Acquire(t.Context(), f.req)
	if err == nil || !strings.Contains(err.Error(), "multiple") {
		t.Fatalf("expected duplicate conflict: %v", err)
	}
	if f.creates != 1 || f.claim(t).CloudID != "" || len(f.deletes)+len(f.stops) != 0 {
		t.Fatal("ambiguous ownership was mutated")
	}
}

func TestFixedCodespacesIntentMismatch(t *testing.T) {
	for _, field := range []string{"repo", "ref", "machine", "devcontainer", "account", "release", "work-root"} {
		t.Run(field, func(t *testing.T) {
			f := newFixedCodespacesFixture(t)
			if _, err := f.b.Acquire(t.Context(), f.req); err != nil {
				t.Fatal(err)
			}
			switch field {
			case "repo":
				f.b.cfg.GitHubCodespaces.Repo = "example-org/other"
			case "ref":
				f.b.cfg.GitHubCodespaces.Ref = "other"
			case "machine":
				f.b.cfg.GitHubCodespaces.Machine = "other"
			case "devcontainer":
				f.b.cfg.GitHubCodespaces.DevcontainerPath = ".devcontainer/other.json"
			case "account":
				f.user = githubUser{ID: 999, Login: "other"}
			case "release":
				f.b.cfg.GitHubCodespaces.DeleteOnRelease = false
			case "work-root":
				f.b.cfg.GitHubCodespaces.WorkRoot = "/other"
			}
			if _, err := f.b.Acquire(t.Context(), f.req); err == nil {
				t.Fatal("accepted changed intent")
			}
			if f.creates != 1 {
				t.Fatalf("creates=%d", f.creates)
			}
		})
	}
}

func TestFixedCodespacesForeignNeverAdopted(t *testing.T) {
	for _, field := range []string{"marker", "repo", "owner", "environment", "machine"} {
		t.Run(field, func(t *testing.T) {
			f := newFixedCodespacesFixture(t)
			f.loseReply, f.hideCreated = true, true
			_, _ = f.b.Acquire(t.Context(), f.req)
			item := f.items["cs-owned"]
			switch field {
			case "marker":
				item["display_name"] = fmt.Sprint(item["display_name"]) + "x"
			case "repo":
				item["repository"] = map[string]any{"id": 1002, "full_name": "example-org/other"}
			case "owner":
				item["owner"] = githubUser{ID: 999, Login: "other"}
			case "environment":
				item["environment_id"] = ""
			case "machine":
				item["machine"] = map[string]any{"name": "other"}
			}
			f.hideCreated = false
			if _, err := f.b.Acquire(t.Context(), f.req); err == nil {
				t.Fatal("adopted foreign or incomplete Codespace")
			}
			if err := f.b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: f.req.RequestedLeaseID}}); err == nil {
				t.Fatal("released foreign resource")
			}
			if f.creates != 1 || f.claim(t).CloudID != "" || len(f.deletes)+len(f.stops) != 0 {
				t.Fatal("foreign ownership mutated")
			}
		})
	}
}

func TestFixedCodespacesStopExactResource(t *testing.T) {
	for _, retain := range []bool{false, true} {
		t.Run(fmt.Sprint(retain), func(t *testing.T) {
			f := newFixedCodespacesFixture(t)
			f.b.cfg.GitHubCodespaces.DeleteOnRelease = !retain
			lease, err := f.b.Acquire(t.Context(), f.req)
			if err != nil {
				t.Fatal(err)
			}
			other := cloneCodespaceJSON(f.items["cs-owned"])
			other["name"], other["id"] = "cs-foreign", 999
			f.items["cs-foreign"] = other
			if err := f.b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
				t.Fatal(err)
			}
			if len(f.stops) != 1 || f.stops[0] != "cs-owned" || f.items["cs-foreign"] == nil {
				t.Fatal("stopped wrong resource")
			}
			if retain {
				if len(f.deletes) != 0 || f.items["cs-owned"]["state"] != "Shutdown" {
					t.Fatal("did not confirm retained stop")
				}
				if _, err := f.b.Acquire(t.Context(), f.req); err != nil {
					t.Fatal(err)
				}
			} else {
				if len(f.deletes) != 1 || f.deletes[0] != "cs-owned" || f.claim(t).FixedCreateIntent.State != "released" {
					t.Fatal("missing terminal receipt")
				}
				if _, err := f.b.Acquire(t.Context(), f.req); err == nil {
					t.Fatal("recreated released lease")
				}
			}
		})
	}
}

func TestFixedCodespacesConcurrentReplay(t *testing.T) {
	f := newFixedCodespacesFixture(t)
	results := make(chan error, 2)
	for range 2 {
		go func() { _, err := f.b.Acquire(t.Context(), f.req); results <- err }()
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if f.creates != 1 {
		t.Fatalf("creates=%d", f.creates)
	}
}

func TestFixedCodespacesReleaseRecoversLostResponse(t *testing.T) {
	f := newFixedCodespacesFixture(t)
	f.loseReply = true
	_, _ = f.b.Acquire(t.Context(), f.req)
	lease, err := f.b.Resolve(t.Context(), core.ResolveRequest{ID: f.req.RequestedLeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if f.claim(t).CloudID != "" {
		t.Fatal("release-only resolution wrote binding")
	}
	if err := f.b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if f.creates != 1 || len(f.deletes) != 1 || f.deletes[0] != "cs-owned" {
		t.Fatal("did not release recovered resource")
	}
	if f.claim(t).FixedCreateIntent.State != "released" {
		t.Fatal("missing receipt")
	}
	// Repeated stop observes the receipt, and list tolerates its retained identity.
	lease, err = f.b.Resolve(t.Context(), core.ResolveRequest{ID: f.req.RequestedLeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.b.List(t.Context(), core.ListRequest{}); err != nil {
		t.Fatal(err)
	}
}

func TestFixedCodespacesCleanupRetainsUncertainClaim(t *testing.T) {
	f := newFixedCodespacesFixture(t)
	f.b.cfg.TTL = time.Minute
	f.req.Keep = false
	f.loseReply, f.hideCreated = true, true
	_, _ = f.b.Acquire(t.Context(), f.req)
	f.b.now = func() time.Time { return time.Now().Add(24 * time.Hour) }
	if err := f.b.Cleanup(t.Context(), core.CleanupRequest{}); err == nil {
		t.Fatal("uncertain absence must fail closed")
	}
	if f.claim(t).FixedCreateIntent.State == "released" || len(f.deletes) != 0 {
		t.Fatal("lost uncertain custody")
	}
	f.hideCreated = false
	if err := f.b.Cleanup(t.Context(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(f.deletes) != 1 || f.claim(t).FixedCreateIntent.State != "released" {
		t.Fatal("cleanup did not recover exact expired resource")
	}
}

func TestFixedCodespacesReleaseRequiresCompletion(t *testing.T) {
	for _, retain := range []bool{false, true} {
		t.Run(fmt.Sprint(retain), func(t *testing.T) {
			f := newFixedCodespacesFixture(t)
			f.b.cfg.GitHubCodespaces.DeleteOnRelease = !retain
			lease, err := f.b.Acquire(t.Context(), f.req)
			if err != nil {
				t.Fatal(err)
			}
			f.stopPending, f.deletePending = retain, !retain
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			if err := f.b.ReleaseLease(ctx, core.ReleaseLeaseRequest{Lease: lease}); err == nil {
				t.Fatal("accepted asynchronous acknowledgement as completion")
			}
			if f.claim(t).FixedCreateIntent.State == "released" {
				t.Fatal("lost custody before completion")
			}
			f.stopPending, f.deletePending = false, false
			if err := f.b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFixedCodespacesReleaseRejectsChangedIdentity(t *testing.T) {
	for _, when := range []string{"before", "after-stop"} {
		t.Run(when, func(t *testing.T) {
			f := newFixedCodespacesFixture(t)
			lease, err := f.b.Acquire(t.Context(), f.req)
			if err != nil {
				t.Fatal(err)
			}
			if when == "before" {
				f.items["cs-owned"]["id"] = 999
			} else {
				f.identityAfterStop = true
			}
			if err := f.b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err == nil {
				t.Fatal("accepted changed identity")
			}
			if len(f.deletes) != 0 || f.claim(t).FixedCreateIntent.State == "released" {
				t.Fatal("changed resource deleted")
			}
		})
	}
}

func TestFixedCodespacesLegacyClaimCannotReplay(t *testing.T) {
	f := newFixedCodespacesFixture(t)
	lease, err := f.b.acquireOrdinary(t.Context(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.b.Acquire(t.Context(), f.req); err == nil {
		t.Fatal("upgraded legacy claim without intent")
	}
	if f.creates != 1 {
		t.Fatal("created a second resource")
	}
	if err := f.b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
}

func TestFixedCodespacesAcknowledgementFailureRetainsCustody(t *testing.T) {
	f := newFixedCodespacesFixture(t)
	f.req.OnAcquired = func(core.LeaseTarget) error { return errors.New("interrupted caller") }
	if _, err := f.b.Acquire(t.Context(), f.req); err == nil {
		t.Fatal("expected acknowledgement failure")
	}
	f.req.OnAcquired = nil
	if _, err := f.b.Acquire(t.Context(), f.req); err != nil {
		t.Fatal(err)
	}
	if f.creates != 1 {
		t.Fatal("recreated acknowledged resource")
	}
}

func TestFixedCodespacesResolveUsesPersistedWorkRoot(t *testing.T) {
	f := newFixedCodespacesFixture(t)
	if _, err := f.b.Acquire(t.Context(), f.req); err != nil {
		t.Fatal(err)
	}
	f.b.cfg.GitHubCodespaces.WorkRoot = "/changed"
	lease, err := f.b.Resolve(t.Context(), core.ResolveRequest{ID: f.req.RequestedLeaseID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lease.SSH.ReadyCheck, "'/workspaces/my-app'") {
		t.Fatalf("ready check=%q", lease.SSH.ReadyCheck)
	}
}

func TestFixedCodespacesDirtyCleanupRetryPreservesDeletion(t *testing.T) {
	f := newFixedCodespacesFixture(t)
	lease, err := f.b.Acquire(t.Context(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	f.dirtyAfterStop = true
	if err := f.b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err == nil {
		t.Fatal("expected dirty-after-stop refusal")
	}
	if f.claim(t).FixedCreateIntent.Journal.Phase != "deleting" {
		t.Fatal("missing cleanup custody")
	}
	if err := f.b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err == nil {
		t.Fatal("dirty retry must preserve deletion custody and report failure")
	}
	if f.claim(t).Labels[labelRelease] != releaseDelete {
		t.Fatal("dirty retry replaced delete policy")
	}
	f.dirtyAfterStop = false
	f.items["cs-owned"]["git_status"].(map[string]any)["has_uncommitted_changes"] = false
	if err := f.b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if f.claim(t).FixedCreateIntent.State != "released" || len(f.deletes) != 1 {
		t.Fatal("clean retry did not finalize deletion")
	}
}

func TestFixedCodespacesPrePlanFailureCanStopAfterExpiry(t *testing.T) {
	f := newFixedCodespacesFixture(t)
	f.b.cfg.TTL = time.Minute
	f.req.Keep = false
	f.failInventoryAt = 2 // Initial discovery succeeds; planning inventory fails.
	if _, err := f.b.Acquire(t.Context(), f.req); err == nil {
		t.Fatal("expected pre-plan failure")
	}
	claim := f.claim(t)
	if claim.FixedCreateIntent.Attempt != nil || f.creates != 0 {
		t.Fatal("unexpected native submission")
	}
	f.b.now = func() time.Time { return time.Now().Add(24 * time.Hour) }
	if err := f.b.Cleanup(t.Context(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if f.claim(t).FixedCreateIntent.State != "released" || len(f.deletes)+len(f.stops) != 0 {
		t.Fatal("pristine cleanup lost or mutated native custody")
	}
}

func TestFixedCodespacesRetainedLeaseExplicitDeletion(t *testing.T) {
	for _, dirty := range []bool{false, true} {
		t.Run(fmt.Sprint(dirty), func(t *testing.T) {
			f := newFixedCodespacesFixture(t)
			f.b.cfg.GitHubCodespaces.DeleteOnRelease = dirty
			lease, err := f.b.Acquire(t.Context(), f.req)
			if err != nil {
				t.Fatal(err)
			}
			f.items["cs-owned"]["git_status"].(map[string]any)["has_uncommitted_changes"] = dirty
			if err := f.b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
				t.Fatal(err)
			}
			if f.claim(t).Labels[labelRelease] != releaseStop {
				t.Fatal("expected retention")
			}
			f.items["cs-owned"]["git_status"].(map[string]any)["has_uncommitted_changes"] = false
			f.b.cfg.GitHubCodespaces.DeleteOnRelease = true
			markDeleteOnReleaseExplicit(&f.b.cfg)
			if err := f.b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
				t.Fatal(err)
			}
			if f.claim(t).FixedCreateIntent.State != "released" || len(f.deletes) != 1 {
				t.Fatal("explicit deletion override ignored")
			}
			if !strings.HasPrefix(f.b.ReleaseLeaseMessage(lease), "deleted ") {
				t.Fatal("deletion reported as retention")
			}
			result, err := f.b.Doctor(t.Context(), core.DoctorRequest{})
			if err != nil || !strings.Contains(result.Message, "stranded=0") {
				t.Fatalf("terminal receipt reported as stranded: %+v %v", result, err)
			}

		})
	}
}
