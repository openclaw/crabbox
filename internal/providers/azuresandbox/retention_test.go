package azuresandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func retainedFixture(t *testing.T) (*backend, core.FixedWarmupRequest, *fixture, *int, *int) {
	t.Helper()
	f := &fixture{}
	b, req := fixtureBackend(t, f)
	b.cfg.AzureSandbox.Retention = "session-disk"
	b.cfg.AzureSandbox.OwnerBinding = strings.Repeat("a", 64)
	pauses, resumes := 0, 0
	b.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/stop"):
			pauses++
			f.box.State = "Stopped"
			return response(200, ""), nil
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/resume"):
			resumes++
			f.box.State = "Running"
			return response(200, ""), nil
		case strings.HasSuffix(r.URL.Path, "/executeShellCommand"):
			return response(200, `{"exitCode":0,"stdout":"retained","stderr":""}`), nil
		}
		out, err := f.request(t, r)
		if r.Method == "PUT" && f.box != nil {
			data, _ := json.Marshal(f.created.Lifecycle)
			if err := json.Unmarshal(data, &f.box.Lifecycle); err != nil {
				t.Fatal(err)
			}
			data, _ = json.Marshal(f.box)
			return response(200, string(data)), err
		}
		return out, err
	})
	return b, req, f, &pauses, &resumes
}

func retentionRequest(t *testing.T, b *backend, id, op string) core.RetentionLifecycleRequest {
	t.Helper()
	v, err := b.Status(t.Context(), core.StatusRequest{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	i := v.Retention.Identity
	return core.RetentionLifecycleRequest{OperationID: op, OwnerBinding: v.Retention.OwnerBinding, ExpectedClaimRevision: v.Retention.ClaimRevision, ExpectedProviderIdentity: core.ProviderIdentityExpectation{LeaseID: i.LeaseID, AttemptLeaseID: i.AttemptLeaseID, Slug: i.Slug, ResourceID: i.ResourceID}}
}

func TestRetainedDiskWindowsPauseResumeAndExactRetirement(t *testing.T) {
	b, req, f, pauses, resumes := retainedFixture(t)
	if _, err := b.acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	original, _ := b.claim(req.RequestedLeaseID)
	if original.FixedCreateIntent.Journal.Version != 2 {
		t.Fatal("old reader fence missing")
	}
	p := retentionRequest(t, b, req.RequestedLeaseID, "pause-one")
	if err := b.Pause(t.Context(), core.PauseRequest{ID: req.RequestedLeaseID, Lifecycle: p}); err != nil {
		t.Fatal(err)
	}
	if err := b.Pause(t.Context(), core.PauseRequest{ID: req.RequestedLeaseID, Lifecycle: p}); err != nil {
		t.Fatal(err)
	}
	if *pauses != 1 || f.deletes != 0 {
		t.Fatal("pause was repeated or deleted")
	}
	paused, _ := b.claim(req.RequestedLeaseID)
	if _, err := executionDeadline(paused); err == nil {
		t.Fatal("closed window admitted work")
	}
	r := retentionRequest(t, b, req.RequestedLeaseID, "resume-one")
	if err := b.Resume(t.Context(), core.ResumeRequest{ID: req.RequestedLeaseID, Lifecycle: r}); err != nil {
		t.Fatal(err)
	}
	resumed, _ := b.claim(req.RequestedLeaseID)
	if err := b.Resume(t.Context(), core.ResumeRequest{ID: req.RequestedLeaseID, Lifecycle: r}); err != nil {
		t.Fatal(err)
	}
	replayed, _ := b.claim(req.RequestedLeaseID)
	if *resumes != 1 || resumed.FixedCreateIntent.Journal.Retention.ComputeWindow != replayed.FixedCreateIntent.Journal.Retention.ComputeWindow {
		t.Fatal("resume replay changed window")
	}
	if original.FixedCreateIntent.Attempt["expires_at"] != replayed.FixedCreateIntent.Attempt["expires_at"] || original.FixedCreateIntent.CreatedAt != replayed.FixedCreateIntent.CreatedAt {
		t.Fatal("legacy original deadline edited")
	}
	// An old pause request cannot regain authority after a successor.
	if err := b.Pause(t.Context(), core.PauseRequest{ID: req.RequestedLeaseID, Lifecycle: p}); err == nil {
		t.Fatal("stale operation was readmitted")
	}
	identity := r.ExpectedProviderIdentity
	b.cfg.AzureSandbox.Retention = ""
	b.cfg.AzureSandbox.OwnerBinding = "" // rollback disables new adoption only.
	if err := b.StopFixed(t.Context(), core.FixedStopRequest{StopRequest: core.StopRequest{ID: req.RequestedLeaseID}, ExpectedProviderIdentity: identity}); err != nil {
		t.Fatal(err)
	}
	if err := b.StopFixed(t.Context(), core.FixedStopRequest{StopRequest: core.StopRequest{ID: req.RequestedLeaseID}, ExpectedProviderIdentity: identity}); err != nil {
		t.Fatal(err)
	}
	view, err := b.Status(t.Context(), core.StatusRequest{ID: req.RequestedLeaseID})
	if err != nil {
		t.Fatal(err)
	}
	if view.State != "deleted" || f.deletes != 1 || view.Retention.Identity.ResourceID != identity.ResourceID || view.Retention.DesiredState != "retired" {
		t.Fatalf("bad terminal receipt: %+v", view)
	}
}

func TestRetainedUncertainPauseReconcilesWithoutRepeating(t *testing.T) {
	b, req, f, pauses, _ := retainedFixture(t)
	if _, err := b.acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	transport := b.api.http.Transport
	b.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/stop") {
			*pauses++
			f.box.State = "Stopped"
			return nil, fmt.Errorf("lost reply: %w", io.EOF)
		}
		return transport.RoundTrip(r)
	})
	p := retentionRequest(t, b, req.RequestedLeaseID, "uncertain-pause")
	if err := b.Pause(t.Context(), core.PauseRequest{ID: req.RequestedLeaseID, Lifecycle: p}); err == nil {
		t.Fatal("lost reply was accepted")
	}
	v, err := b.Status(t.Context(), core.StatusRequest{ID: req.RequestedLeaseID})
	if err != nil {
		t.Fatal(err)
	}
	if v.Ready || v.Retention.Operation.Phase != "pending" {
		t.Fatal("pending pause admitted work")
	}
	if err := b.Pause(t.Context(), core.PauseRequest{ID: req.RequestedLeaseID, Lifecycle: p}); err != nil {
		t.Fatal(err)
	}
	if *pauses != 1 {
		t.Fatal("unknown pause was repeated")
	}
}

type retentionClock struct{ at time.Time }

func (c *retentionClock) Now() time.Time { return c.at }

func TestRetainedExpiryBlocksWorkButPreservesDiskAndCleanup(t *testing.T) {
	b, req, f, _, _ := retainedFixture(t)
	clock := &retentionClock{at: time.Now()}
	b.rt.Clock = clock
	if _, err := b.acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	original, _ := b.claim(req.RequestedLeaseID)
	clock.at = clock.at.Add(5 * time.Hour)
	claim, _ := b.claim(req.RequestedLeaseID)
	if _, err := b.executionDeadline(claim); err == nil {
		t.Fatal("expired window admitted execution")
	}
	if _, err := b.Heartbeat(t.Context(), core.LeaseHeartbeatRequest{ID: claim.LeaseID}); err == nil {
		t.Fatal("heartbeat renewed expired window")
	}
	v, err := b.Status(t.Context(), core.StatusRequest{ID: claim.LeaseID})
	if err != nil {
		t.Fatal(err)
	}
	if v.State != "compute-expired" || !v.Retention.StopPending || f.box == nil {
		t.Fatalf("expiry discarded retained ownership: %+v", v)
	}
	if _, err := b.acquire(t.Context(), req); err == nil || !strings.Contains(err.Error(), "explicit lifecycle recovery required") || !strings.Contains(err.Error(), "inspect the same lease") || !strings.Contains(err.Error(), "pause/resume") || strings.Contains(err.Error(), "release the lease") {
		t.Fatalf("expired retained replay must preserve lifecycle recovery guidance: %v", err)
	}
	p := retentionRequest(t, b, claim.LeaseID, "expiry-pause")
	if err := b.Pause(t.Context(), core.PauseRequest{ID: claim.LeaseID, Lifecycle: p}); err != nil {
		t.Fatal(err)
	}
	r := retentionRequest(t, b, claim.LeaseID, "expiry-resume")
	if err := b.Resume(t.Context(), core.ResumeRequest{ID: claim.LeaseID, Lifecycle: r}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.acquire(t.Context(), req); err != nil {
		t.Fatalf("new window could not replay original allocation beyond original TTL: %v", err)
	}
	current, _ := b.claim(claim.LeaseID)
	if original.FixedCreateIntent.CreatedAt != current.FixedCreateIntent.CreatedAt || original.FixedCreateIntent.Attempt["expires_at"] != current.FixedCreateIntent.Attempt["expires_at"] {
		t.Fatal("original allocation deadlines were edited")
	}
	if err := b.Stop(t.Context(), core.StopRequest{ID: claim.LeaseID}); err != nil {
		t.Fatal(err)
	}
}

func TestRetainedPolicyRequiresExplicitIndependentReadback(t *testing.T) {
	b, req, f, _, _ := retainedFixture(t)
	if _, err := b.acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	transport := b.api.http.Transport
	b.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" && f.box != nil && strings.HasSuffix(r.URL.Path, "/"+f.box.ID) {
			data, _ := json.Marshal(f.box)
			var raw map[string]any
			_ = json.Unmarshal(data, &raw)
			delete(raw["lifecycle"].(map[string]any), "autoDeletePolicy")
			data, _ = json.Marshal(raw)
			return response(200, string(data)), nil
		}
		return transport.RoundTrip(r)
	})
	v, err := b.Status(t.Context(), core.StatusRequest{ID: req.RequestedLeaseID})
	if err != nil {
		t.Fatal(err)
	}
	if v.Ready || v.Retention.PolicyVerified || v.State != "unknown" {
		t.Fatal("absent policy treated as disabled")
	}
	claim, _ := b.claim(req.RequestedLeaseID)
	if _, err := b.ensureRunning(t.Context(), claim); err == nil {
		t.Fatal("unverified policy admitted work")
	}
	if err := b.Stop(t.Context(), core.StopRequest{ID: claim.LeaseID}); err != nil {
		t.Fatal(err)
	}
}

func TestRetainedCLIExactBoundary(t *testing.T) {
	b, req, _, _, _ := retainedFixture(t)
	t.Chdir(req.Repo.Root)
	// A git root is necessary for normal warmup repository ownership.
	if out, err := exec.Command("git", "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %s %v", out, err)
	}
	old := newAPI
	newAPI = func(core.Config, core.Runtime) (*client, error) { return b.api, nil }
	t.Cleanup(func() { newAPI = old })
	var stdout bytes.Buffer
	app := core.App{Stdout: &stdout, Stderr: io.Discard}
	for key, value := range map[string]string{"CRABBOX_AZURE_SANDBOX_REGION": "westus3", "CRABBOX_AZURE_SANDBOX_SUBSCRIPTION_ID": "subscription", "CRABBOX_AZURE_SANDBOX_RESOURCE_GROUP": "workers", "CRABBOX_AZURE_SANDBOX_GROUP": "sandboxes"} {
		t.Setenv(key, value)
	}
	common := []string{"--provider", providerName}
	warm := append([]string{"warmup", "--network", "public", "--tailscale=false", "--keep", "--lease-id", req.RequestedLeaseID, "--ttl", "4h", "--idle-timeout", "15m", "--azure-sandbox-retention", "session-disk", "--azure-sandbox-owner-binding", b.cfg.AzureSandbox.OwnerBinding}, common...)
	t.Setenv("CRABBOX_CONTROLLER_PROCESS_TREE_OWNED", "1")
	if err := app.Run(t.Context(), warm); err == nil {
		t.Fatal("legacy controller adopted retained warmup")
	}
	t.Setenv("CRABBOX_CONTROLLER_PROCESS_TREE_OWNED", "")
	if err := app.Run(t.Context(), warm); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := app.Run(t.Context(), append([]string{"inspect", "--id", req.RequestedLeaseID, "--json"}, common...)); err != nil {
		t.Fatal(err)
	}
	var v core.StatusView
	if err := json.Unmarshal(stdout.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	r := v.Retention
	id := r.Identity
	if r.Schema != "crabbox.session-disk.v1" || !r.PolicyVerified || id.AttemptLeaseID == "" || !v.Ready {
		t.Fatalf("incomplete receipt: %s", stdout.String())
	}
	args := append([]string{"pause", "--id", id.LeaseID, "--operation-id", "cli-pause", "--owner-binding", r.OwnerBinding, "--expected-claim-revision", r.ClaimRevision, "--expected-provider-lease-id", id.LeaseID, "--expected-provider-attempt-lease-id", id.AttemptLeaseID, "--expected-provider-slug", id.Slug, "--expected-provider-resource-id", id.ResourceID, "--json"}, common...)
	stdout.Reset()
	if err := app.Run(t.Context(), args); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(stdout.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.State != "suspended" || v.Ready || v.Retention.ComputeWindow.State != "closed" || v.Retention.Operation.Phase != "observed" {
		t.Fatalf("pause receipt: %s", stdout.String())
	}
	stdout.Reset()
	err := app.Run(t.Context(), append([]string{"status", "--id", id.LeaseID, "--wait", "--wait-timeout", "1s", "--json"}, common...))
	if err == nil || !strings.Contains(err.Error(), "cannot become ready by waiting") || !strings.Contains(err.Error(), "suspended") {
		t.Fatalf("suspended readiness wait did not return its lifecycle result: %v", err)
	}
	if err := json.Unmarshal(stdout.Bytes(), &v); err != nil || v.State != "suspended" {
		t.Fatalf("suspended wait receipt: %s %v", stdout.String(), err)
	}
	stop := append([]string{"stop", "--id", id.LeaseID, "--expected-provider-lease-id", id.LeaseID, "--expected-provider-attempt-lease-id", id.AttemptLeaseID, "--expected-provider-slug", id.Slug, "--expected-provider-resource-id", id.ResourceID}, common...)
	t.Setenv("CRABBOX_CONTROLLER_PROCESS_TREE_OWNED", "1")
	if err := app.Run(t.Context(), stop); err == nil {
		t.Fatal("legacy controller deleted retained disk")
	}
	t.Setenv("CRABBOX_CONTROLLER_PROCESS_TREE_OWNED", "")
	if err := app.Run(t.Context(), stop); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := app.Run(t.Context(), append([]string{"inspect", "--id", id.LeaseID, "--json"}, common...)); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(stdout.Bytes(), &v); err != nil || v.State != "deleted" || v.Retention.Identity != id {
		t.Fatalf("terminal identity receipt changed: %s %v", stdout.String(), err)
	}
}

func TestRetainedResumeUnknownSurvivesBackendRestart(t *testing.T) {
	b, req, f, _, resumes := retainedFixture(t)
	if _, err := b.acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	p := retentionRequest(t, b, req.RequestedLeaseID, "before-resume")
	if err := b.Pause(t.Context(), core.PauseRequest{ID: req.RequestedLeaseID, Lifecycle: p}); err != nil {
		t.Fatal(err)
	}
	r := retentionRequest(t, b, req.RequestedLeaseID, "lost-resume")
	transport := b.api.http.Transport
	b.api.http.Transport = transportFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == "POST" && strings.HasSuffix(req.URL.Path, "/resume") {
			*resumes++
			f.box.State = "Running"
			return nil, io.EOF
		}
		return transport.RoundTrip(req)
	})
	if err := b.Resume(t.Context(), core.ResumeRequest{ID: req.RequestedLeaseID, Lifecycle: r}); err == nil {
		t.Fatal("unknown resume returned success")
	}
	pending, _ := b.claim(req.RequestedLeaseID)
	if _, err := b.executionDeadline(pending); err == nil {
		t.Fatal("pending resume admitted work")
	}
	restarted := &backend{cfg: b.cfg, rt: b.rt, api: b.api}
	if err := restarted.Resume(t.Context(), core.ResumeRequest{ID: req.RequestedLeaseID, Lifecycle: r}); err != nil {
		t.Fatal(err)
	}
	settled, _ := restarted.claim(req.RequestedLeaseID)
	if *resumes != 1 || pending.FixedCreateIntent.Journal.Retention.ComputeWindow != settled.FixedCreateIntent.Journal.Retention.ComputeWindow {
		t.Fatal("restart renewed or repeated uncertain resume")
	}
	// Natural idle suspension is never silently awakened by heartbeat/run.
	f.box.State = "Stopped"
	if _, err := restarted.Heartbeat(t.Context(), core.LeaseHeartbeatRequest{ID: req.RequestedLeaseID}); err == nil {
		t.Fatal("heartbeat implicitly resumed retained disk")
	}
	if *resumes != 1 {
		t.Fatal("heartbeat created activation")
	}
}

func TestRetainedDeleteUncertaintyKeepsExactCleanupJournal(t *testing.T) {
	b, req, f, _, _ := retainedFixture(t)
	if _, err := b.acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	identity := retentionRequest(t, b, req.RequestedLeaseID, "unused").ExpectedProviderIdentity
	transport := b.api.http.Transport
	b.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == "DELETE" {
			c, err := core.ReadLeaseClaim(req.RequestedLeaseID)
			if err != nil {
				t.Fatal(err)
			}
			if c.FixedCreateIntent.Journal.Phase != "deleting" || c.FixedCreateIntent.Journal.Retention.DesiredState != "retired" || c.FixedCreateIntent.Journal.Retention.ComputeWindow.State != "closed" {
				t.Fatal("delete effect preceded durable retirement")
			}
			f.deletes++
			f.box = nil
			return nil, io.EOF
		}
		return transport.RoundTrip(r)
	})
	stop := core.FixedStopRequest{StopRequest: core.StopRequest{ID: req.RequestedLeaseID}, ExpectedProviderIdentity: identity}
	if err := b.StopFixed(t.Context(), stop); err == nil {
		t.Fatal("unknown delete settled without readback")
	}
	claim, _ := b.claim(req.RequestedLeaseID)
	if claim.CloudID != identity.ResourceID || claim.FixedCreateIntent.Journal.Phase != "deleting" {
		t.Fatal("lost cleanup identity")
	}
	if err := b.StopFixed(t.Context(), stop); err != nil {
		t.Fatal(err)
	}
	if f.deletes != 1 {
		t.Fatal("delete repeated after exact missing readback")
	}
}

func TestRetainedWrongOwnerCannotMutateOrReclaim(t *testing.T) {
	b, req, _, pauses, resumes := retainedFixture(t)
	if _, err := b.acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	p := retentionRequest(t, b, req.RequestedLeaseID, "foreign")
	p.OwnerBinding = strings.Repeat("b", 64)
	if err := b.Pause(t.Context(), core.PauseRequest{ID: req.RequestedLeaseID, Lifecycle: p}); err == nil {
		t.Fatal("foreign owner paused disk")
	}
	if *pauses != 0 || *resumes != 0 {
		t.Fatal("wrong owner caused provider effect")
	}
	req.Reclaim = true
	if _, err := b.acquire(t.Context(), req); err == nil {
		t.Fatal("retained owner permitted reclaim")
	}
}

func TestRetainedCleanSnapshotInitializationKeepsCleanBindingSeparate(t *testing.T) {
	b, req, f, _, _ := retainedFixture(t)
	binding := pinSnapshotFixture(t, b, nil)
	snapshotTransport(t, b, f, binding, nil)
	transport := b.api.http.Transport
	policies := 0
	b.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/lifecycle") {
			policies++
			if err := json.NewDecoder(r.Body).Decode(&f.box.Lifecycle); err != nil {
				t.Fatal(err)
			}
			if f.box.Lifecycle.AutoDelete.Enabled {
				t.Fatal("retained clone kept finite auto-delete")
			}
			return response(200, ""), nil
		}
		return transport.RoundTrip(r)
	})
	if _, err := b.acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if policies != 1 || f.created.SourcesRef.Snapshot == nil || f.created.Lifecycle != nil {
		t.Fatal("clean initialization bypassed native restore contract")
	}
	if !binding.Lifecycle.AutoDelete.Enabled {
		t.Fatal("retention rewrote clean source binding")
	}
	v, err := b.Status(t.Context(), core.StatusRequest{ID: req.RequestedLeaseID})
	if err != nil || !v.Ready || !v.Retention.PolicyVerified {
		t.Fatalf("retained clone did not attest policy: %+v %v", v, err)
	}
	if err := b.Stop(t.Context(), core.StopRequest{ID: req.RequestedLeaseID}); err != nil {
		t.Fatal(err)
	}
}

func TestRetainedOutageExposesPendingStopWithoutLosingOwnership(t *testing.T) {
	b, req, _, _, _ := retainedFixture(t)
	if _, err := b.acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	p := retentionRequest(t, b, req.RequestedLeaseID, "outage-pause")
	transport := b.api.http.Transport
	outage := false
	b.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/stop") {
			outage = true
			return nil, io.EOF
		}
		if outage {
			return nil, io.EOF
		}
		return transport.RoundTrip(r)
	})
	if err := b.Pause(t.Context(), core.PauseRequest{ID: req.RequestedLeaseID, Lifecycle: p}); err == nil {
		t.Fatal("outage pause succeeded")
	}
	v, err := b.Status(t.Context(), core.StatusRequest{ID: req.RequestedLeaseID})
	if err != nil {
		t.Fatal(err)
	}
	if v.State != "unknown" || v.Ready || !v.Retention.StopPending || v.Retention.Operation.Phase != "pending" || v.Retention.Identity.ResourceID != p.ExpectedProviderIdentity.ResourceID {
		t.Fatal("outage lost pending stop/ownership receipt")
	}
}

func TestRetainedUnappliedEffectsReassertSameOperation(t *testing.T) {
	for _, action := range []string{"pause", "resume"} {
		t.Run(action, func(t *testing.T) {
			b, req, f, pauses, resumes := retainedFixture(t)
			if _, err := b.acquire(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			if action == "resume" {
				p := retentionRequest(t, b, req.RequestedLeaseID, "setup-pause")
				if err := b.Pause(t.Context(), core.PauseRequest{ID: req.RequestedLeaseID, Lifecycle: p}); err != nil {
					t.Fatal(err)
				}
			}
			request := retentionRequest(t, b, req.RequestedLeaseID, "unapplied-"+action)
			transport := b.api.http.Transport
			calls := 0
			b.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				path := "/stop"
				if action == "resume" {
					path = "/resume"
				}
				if r.Method == "POST" && strings.HasSuffix(r.URL.Path, path) {
					calls++
					if calls == 1 {
						return nil, io.EOF
					}
				}
				return transport.RoundTrip(r)
			})
			invoke := func() error {
				if action == "pause" {
					return b.Pause(t.Context(), core.PauseRequest{ID: req.RequestedLeaseID, Lifecycle: request})
				}
				return b.Resume(t.Context(), core.ResumeRequest{ID: req.RequestedLeaseID, Lifecycle: request})
			}
			if err := invoke(); err == nil {
				t.Fatal("unapplied loss was success")
			}
			pending, _ := b.claim(req.RequestedLeaseID)
			if err := invoke(); err != nil {
				t.Fatal(err)
			}
			settled, _ := b.claim(req.RequestedLeaseID)
			if calls != 2 || pending.FixedCreateIntent.Journal.Retention.ComputeWindow != settled.FixedCreateIntent.Journal.Retention.ComputeWindow || f.creates != 1 {
				t.Fatalf("reassert changed allocation or grant: %d %d/%d", calls, *pauses, *resumes)
			}
		})
	}
}

func TestRetainedExpiredPendingResumeSettlesSuspension(t *testing.T) {
	b, req, f, pauses, resumes := retainedFixture(t)
	clock := &retentionClock{at: time.Now()}
	b.rt.Clock = clock
	if _, err := b.acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	p := retentionRequest(t, b, req.RequestedLeaseID, "setup-expiry")
	if err := b.Pause(t.Context(), core.PauseRequest{ID: req.RequestedLeaseID, Lifecycle: p}); err != nil {
		t.Fatal(err)
	}
	r := retentionRequest(t, b, req.RequestedLeaseID, "expired-pending-resume")
	transport := b.api.http.Transport
	b.api.http.Transport = transportFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == "POST" && strings.HasSuffix(req.URL.Path, "/resume") {
			*resumes++
			f.box.State = "Running"
			return nil, io.EOF
		}
		return transport.RoundTrip(req)
	})
	if err := b.Resume(t.Context(), core.ResumeRequest{ID: req.RequestedLeaseID, Lifecycle: r}); err == nil {
		t.Fatal("lost resume was success")
	}
	pending, _ := b.claim(req.RequestedLeaseID)
	clock.at = clock.at.Add(5 * time.Hour)
	if err := b.Resume(t.Context(), core.ResumeRequest{ID: req.RequestedLeaseID, Lifecycle: r}); err == nil {
		t.Fatal("expired resume returned success")
	}
	v, err := b.Status(t.Context(), core.StatusRequest{ID: req.RequestedLeaseID})
	if err != nil {
		t.Fatal(err)
	}
	if v.State != "suspended" || v.Retention.ComputeWindow.State != "closed" || v.Retention.ComputeWindow.ExpiresAt != pending.FixedCreateIntent.Journal.Retention.ComputeWindow.ExpiresAt || *resumes != 1 || *pauses != 2 {
		t.Fatalf("expiry did not settle same-window stop: %+v", v.Retention)
	}
}

func TestRetainedCrashBeforeLifecycleSubmissionRecovers(t *testing.T) {
	b, req, _, pauses, _ := retainedFixture(t)
	if _, err := b.acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	claim, _ := b.claim(req.RequestedLeaseID)
	r := retentionRequest(t, b, req.RequestedLeaseID, "before-submit")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err := core.MutateFixedRetention(ctx, leaseKind, claim, r, "pause", b.now, func(context.Context, core.LeaseClaim) (string, error) { cancel(); return "running", nil }, func(context.Context, core.LeaseClaim, string) error {
		t.Fatal("submitted after cancellation")
		return nil
	})
	if err == nil {
		t.Fatal("canceled-before-submit was success")
	}
	pending, _ := b.claim(req.RequestedLeaseID)
	if pending.FixedCreateIntent.Journal.Retention.Operation.Phase != "pending" {
		t.Fatal("pending intent not durable")
	}
	if err := b.Pause(t.Context(), core.PauseRequest{ID: req.RequestedLeaseID, Lifecycle: r}); err != nil {
		t.Fatal(err)
	}
	if *pauses != 1 {
		t.Fatal("crash recovery did not submit exact pending stop")
	}
}

func TestRetainedPendingNonStableReadbackCannotReassert(t *testing.T) {
	for _, observed := range []string{"transitioning", "unknown", "missing", "mismatched"} {
		t.Run(observed, func(t *testing.T) {
			b, req, _, _, _ := retainedFixture(t)
			if _, err := b.acquire(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			r := retentionRequest(t, b, req.RequestedLeaseID, "held-pause")
			claim, _ := b.claim(req.RequestedLeaseID)
			// Crash before submission leaves a real persisted operation in the owner.
			ctx, cancel := context.WithCancel(t.Context())
			_ = core.MutateFixedRetention(ctx, leaseKind, claim, r, "pause", b.now, func(context.Context, core.LeaseClaim) (string, error) { cancel(); return "running", nil }, func(context.Context, core.LeaseClaim, string) error {
				t.Fatal("unexpected initial submission")
				return nil
			})
			pending, _ := b.claim(req.RequestedLeaseID)
			ctx, cancel = context.WithCancel(t.Context())
			defer cancel()
			observations := 0
			err := core.MutateFixedRetention(ctx, leaseKind, pending, r, "pause", b.now, func(context.Context, core.LeaseClaim) (string, error) {
				observations++
				cancel()
				if observed == "mismatched" {
					return "", errSandboxOwnership
				}
				return observed, nil
			}, func(context.Context, core.LeaseClaim, string) error {
				t.Fatal("nonstable readback authorized submission")
				return nil
			})
			if err == nil || observations != 1 {
				t.Fatal("uncertain observation did not hold lifecycle")
			}
			latest, _ := b.claim(req.RequestedLeaseID)
			if latest.FixedCreateIntent.Journal.Retention.Operation.Phase != "pending" || latest.FixedCreateIntent.Journal.Retention.ComputeWindow != pending.FixedCreateIntent.Journal.Retention.ComputeWindow {
				t.Fatal("uncertain readback changed window or settled operation")
			}
		})
	}
}
