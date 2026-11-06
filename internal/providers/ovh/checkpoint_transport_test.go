package ovh

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

// recordingOVH is a stateful HTTP stand-in for the OVH API. Tests drive the
// production Client (signing, transport, JSON) against it, so a rejected
// operation is proven by the absence of mutating requests, not by a fake.
type recordingOVH struct {
	t         *testing.T
	mu        sync.Mutex
	instance  Instance
	snapshots map[string]Image
	log       []string
}

func newRecordingOVH(t *testing.T, instance Instance) (*recordingOVH, *httptest.Server) {
	r := &recordingOVH{t: t, instance: instance, snapshots: map[string]Image{}}
	server := httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(server.Close)
	return r, server
}

func (r *recordingOVH) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.log = append(r.log, req.Method+" "+req.URL.Path)
	base := "/cloud/project/project-test"
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case req.Method == http.MethodGet && req.URL.Path == base+"/instance":
		reply([]Instance{r.instance})
	case req.Method == http.MethodGet && req.URL.Path == base+"/instance/"+r.instance.ID:
		reply(r.instance)
	case req.Method == http.MethodPost && req.URL.Path == base+"/instance/"+r.instance.ID+"/snapshot":
		var body map[string]string
		_ = json.NewDecoder(req.Body).Decode(&body)
		r.snapshots["snap-1"] = Image{ID: "snap-1", Name: body["snapshotName"], Region: r.instance.Region, Status: "active", Type: "linux", Visibility: "private"}
		reply(map[string]string{"snapshotId": "snap-1"})
	case req.Method == http.MethodGet && req.URL.Path == base+"/snapshot":
		out := []Image{}
		for _, s := range r.snapshots {
			out = append(out, s)
		}
		reply(out)
	case strings.HasPrefix(req.URL.Path, base+"/snapshot/"):
		id := strings.TrimPrefix(req.URL.Path, base+"/snapshot/")
		snapshot, ok := r.snapshots[id]
		switch {
		case !ok:
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		case req.Method == http.MethodGet:
			reply(snapshot)
		case req.Method == http.MethodDelete:
			delete(r.snapshots, id)
			reply(snapshot)
		}
	case req.Method == http.MethodDelete && strings.HasPrefix(req.URL.Path, base+"/sshkey/"):
		w.WriteHeader(http.StatusNoContent)
	case req.Method == http.MethodDelete && req.URL.Path == base+"/instance/"+r.instance.ID:
		r.instance.Status = "DELETED"
		w.WriteHeader(http.StatusNoContent)
	default:
		r.t.Errorf("unexpected OVH request %s %s", req.Method, req.URL.Path)
		http.NotFound(w, req)
	}
}

// note records a guest-side or CLI-side effect in the same ordered trace as the
// provider requests, so a test can show what happened before what.
func (r *recordingOVH) note(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.log = append(r.log, "** "+event)
}

func (r *recordingOVH) mark() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.log)
}

// since returns the requests recorded after mark, newest last.
func (r *recordingOVH) since(mark int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.log[mark:]...)
}

func mutating(requests []string) []string {
	var out []string
	for _, request := range requests {
		if strings.HasPrefix(request, "POST ") || strings.HasPrefix(request, "DELETE ") {
			out = append(out, request)
		}
	}
	return out
}

// transportFixture is a claimed lease whose provider calls go through the real
// signed client to a recording server.
type transportFixture struct {
	*checkpointFixture
	ovh     *recordingOVH
	backend *Backend
}

func newTransportFixture(t *testing.T) *transportFixture {
	t.Helper()
	f := newCheckpointFixture(t)
	instance := f.fake.instances[0]
	instance.Region = "GRA11"
	ovh, server := newRecordingOVH(t, instance)
	client := newTestClient(t, server.URL)
	newSnapshotClient = func(core.Config, core.Runtime) (snapshotClient, error) { return client, nil }
	backend := testBackend(f.fake.fakeAPI)
	backend.clientFactory = func(core.Config, core.Runtime) (API, error) { return client, nil }
	return &transportFixture{checkpointFixture: f, ovh: ovh, backend: backend}
}

func TestProductionTransportCheckpointLifecycle(t *testing.T) {
	f := newTransportFixture(t)
	res := ownedResource(f.checkpointFixture)
	if res.Image.ID != "snap-1" || res.Image.State != "active" {
		t.Fatalf("image=%+v", res.Image)
	}
	log := strings.Join(f.ovh.since(0), "\n")
	for _, want := range []string{
		"GET /cloud/project/project-test/instance/inst-1",
		"POST /cloud/project/project-test/instance/inst-1/snapshot",
		"GET /cloud/project/project-test/snapshot/snap-1",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("missing %q in\n%s", want, log)
		}
	}
	if got := len(f.ovh.snapshots); got != 1 || f.ovh.snapshots["snap-1"].Name != "crabbox-chk_abc123" {
		t.Fatalf("snapshots=%+v", f.ovh.snapshots)
	}
	verify, err := (Provider{}).VerifyNativeCheckpoint(context.Background(), res)
	if err != nil || verify.NextAction != "fork_or_delete" {
		t.Fatalf("verify=%+v err=%v", verify, err)
	}
	if err := (Provider{}).DeleteNativeCheckpoint(context.Background(), res); err != nil {
		t.Fatal(err)
	}
	if len(f.ovh.snapshots) != 0 {
		t.Fatalf("snapshot survived delete: %+v", f.ovh.snapshots)
	}
}

func TestProductionTransportRejectsOwnershipBeforeProviderMutation(t *testing.T) {
	f := newTransportFixture(t)
	res := ownedResource(f.checkpointFixture)

	t.Run("foreign project record sends no request at all", func(t *testing.T) {
		foreign := res
		foreign.Metadata = map[string]string{}
		for k, v := range res.Metadata {
			foreign.Metadata[k] = v
		}
		foreign.Metadata["source_project"] = "someone-elses-project"
		mark := f.ovh.mark()
		if err := (Provider{}).DeleteNativeCheckpoint(context.Background(), foreign); err == nil {
			t.Fatal("expected refusal")
		}
		if _, err := (Provider{}).VerifyNativeCheckpoint(context.Background(), foreign); err == nil {
			t.Fatal("expected refusal")
		}
		if got := f.ovh.since(mark); len(got) != 0 {
			t.Fatalf("foreign-project record reached the provider: %v", got)
		}
	})

	t.Run("renamed snapshot is read but never deleted", func(t *testing.T) {
		f.ovh.mu.Lock()
		snapshot := f.ovh.snapshots["snap-1"]
		snapshot.Name = "crabbox-chk_other"
		f.ovh.snapshots["snap-1"] = snapshot
		f.ovh.mu.Unlock()
		mark := f.ovh.mark()
		if err := (Provider{}).DeleteNativeCheckpoint(context.Background(), res); err == nil {
			t.Fatal("expected refusal")
		}
		if got := mutating(f.ovh.since(mark)); len(got) != 0 {
			t.Fatalf("mutation after identity drift: %v", got)
		}
		f.ovh.mu.Lock()
		snapshot.Name = "crabbox-chk_abc123"
		f.ovh.snapshots["snap-1"] = snapshot
		f.ovh.mu.Unlock()
	})

	t.Run("revoked claim stops snapshot creation before the POST", func(t *testing.T) {
		core.RemoveLeaseClaim(f.lease.LeaseID)
		req := f.request()
		req.CheckpointID = "chk_revoked"
		mark := f.ovh.mark()
		if _, err := (Provider{}).CreateNativeCheckpoint(context.Background(), req); err == nil || !strings.Contains(err.Error(), "no exact local claim") {
			t.Fatalf("err=%v", err)
		}
		if got := mutating(f.ovh.since(mark)); len(got) != 0 {
			t.Fatalf("mutation without a claim: %v", got)
		}
	})
}

func TestProductionTransportReassignedSourceStopsCreateBeforePOST(t *testing.T) {
	f := newTransportFixture(t)
	// The live instance behind the claimed id now carries another identity.
	f.ovh.mu.Lock()
	f.ovh.instance.Name = "reassigned-by-someone-else"
	f.ovh.mu.Unlock()
	mark := f.ovh.mark()
	_, err := (Provider{}).CreateNativeCheckpoint(context.Background(), f.request())
	if err == nil || !strings.Contains(err.Error(), "source lease mismatch") {
		t.Fatalf("err=%v", err)
	}
	if got := mutating(f.ovh.since(mark)); len(got) != 0 || len(f.ovh.snapshots) != 0 {
		t.Fatalf("mutation for a reassigned source: %v snapshots=%v", got, f.ovh.snapshots)
	}
}

func TestProductionTransportReleaseOfHeldSourceNeverDeletesInstance(t *testing.T) {
	f := newTransportFixture(t)
	holdSource(t, f.lease.LeaseID)
	mark := f.ovh.mark()
	err := f.backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: f.lease})
	if err == nil || !strings.Contains(err.Error(), "unresolved checkpoint") {
		t.Fatalf("err=%v", err)
	}
	if got := mutating(f.ovh.since(mark)); len(got) != 0 {
		t.Fatalf("held source was mutated: %v", got)
	}
}

// traceSince returns the ordered effects since mark with the fixture project
// name masked, for logging as a redacted production-path trace.
func (f *transportFixture) traceSince(mark int) []string {
	out := f.ovh.since(mark)
	for i := range out {
		out[i] = strings.ReplaceAll(out[i], "/cloud/project/project-test", "/cloud/project/<project>")
	}
	return out
}

func logTrace(t *testing.T, title string, trace []string) {
	t.Helper()
	t.Logf("TRACE %s", title)
	for i, line := range trace {
		t.Logf("TRACE   %d. %s", i+1, line)
	}
	if len(trace) == 0 {
		t.Logf("TRACE   (no provider request and no guest effect)")
	}
}

func indexOf(trace []string, want string) int {
	for i, line := range trace {
		if strings.Contains(line, want) {
			return i
		}
	}
	return -1
}

// A reclaimed claim generation is refused before guest preparation or the
// snapshot POST; the authorized control shows the same path succeeding.
func TestProductionPathTraceReclaimedGenerationVersusControl(t *testing.T) {
	f := newTransportFixture(t)
	prepareCheckpointSource = func(context.Context, core.SSHTarget) error {
		f.ovh.note("guest: prepare source (cloud-init clean, sync)")
		return nil
	}
	stale, _, err := core.ReadLeaseClaimWithPresence(f.lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}

	control := f.request()
	core.SetServerLeaseClaimSnapshot(&control.Server, stale, true)
	control.CheckpointID = "chk_control"
	mark := f.ovh.mark()
	if _, err := (Provider{}).CreateNativeCheckpoint(context.Background(), control); err != nil {
		t.Fatal(err)
	}
	trace := f.traceSince(mark)
	logTrace(t, "authorized control: current claim generation", trace)
	prepared, posted := indexOf(trace, "** guest: prepare"), indexOf(trace, "POST /cloud/project/<project>/instance/inst-1/snapshot")
	if prepared < 0 || posted < 0 || prepared > posted {
		t.Fatalf("control must prepare the guest, then POST: %v", trace)
	}

	// Another checkout reclaims the lease after this request was admitted.
	reclaimed := map[string]string{}
	for k, v := range stale.Labels {
		reclaimed[k] = v
	}
	reclaimed["state"] = "reclaimed-elsewhere"
	if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(f.lease.LeaseID, stale, reclaimed); err != nil {
		t.Fatal(err)
	}
	refused := f.request()
	core.SetServerLeaseClaimSnapshot(&refused.Server, stale, true)
	refused.CheckpointID = "chk_reclaimed"
	mark = f.ovh.mark()
	_, err = (Provider{}).CreateNativeCheckpoint(context.Background(), refused)
	trace = f.traceSince(mark)
	logTrace(t, "reclaimed claim generation: refused ("+firstLine(err)+")", trace)
	if err == nil || !errors.Is(err, core.ErrReleaseLeaseOwnershipChanged) {
		t.Fatalf("err=%v", err)
	}
	if indexOf(trace, "** guest:") >= 0 || len(mutating(trace)) != 0 {
		t.Fatalf("reclaimed generation reached the guest or a provider mutation: %v", trace)
	}
}

// A held source is refused before remote logout or the instance DELETE; the
// authorized control shows the cleanup running first and then the DELETE.
func TestProductionPathTraceHeldSourceReleaseVersusControl(t *testing.T) {
	release := func(t *testing.T, hold bool) ([]string, error) {
		f := newTransportFixture(t)
		if hold {
			holdSource(t, f.lease.LeaseID)
		}
		mark := f.ovh.mark()
		err := f.backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{
			Lease: f.lease,
			GuardedRemoteCleanup: func(context.Context, core.LeaseTarget) {
				f.ovh.note("guest: remote connection cleanup (tailscale logout)")
			},
		})
		return f.traceSince(mark), err
	}

	trace, err := release(t, false)
	logTrace(t, "authorized control: no unresolved checkpoint", trace)
	cleaned, deleted := indexOf(trace, "** guest: remote connection cleanup"), indexOf(trace, "DELETE /cloud/project/<project>/instance/inst-1")
	if err != nil || cleaned < 0 || deleted < 0 || cleaned > deleted {
		t.Fatalf("control must clean up, then DELETE: err=%v %v", err, trace)
	}

	trace, err = release(t, true)
	logTrace(t, "held source: refused ("+firstLine(err)+")", trace)
	if err == nil || !strings.Contains(err.Error(), "unresolved checkpoint") {
		t.Fatalf("err=%v", err)
	}
	if indexOf(trace, "** guest:") >= 0 || len(mutating(trace)) != 0 {
		t.Fatalf("held source saw remote cleanup or a provider mutation: %v", trace)
	}
}

func firstLine(err error) string {
	if err == nil {
		return "no error"
	}
	line, _, _ := strings.Cut(err.Error(), "\n")
	return line
}

// A Tailscale-enrolled source is refused before guest preparation or the
// snapshot POST, because the snapshot would carry its tailnet identity.
func TestProductionPathTraceTailscaleEnrolledSourceRefused(t *testing.T) {
	f := newTransportFixture(t)
	prepareCheckpointSource = func(context.Context, core.SSHTarget) error {
		f.ovh.note("guest: prepare source (cloud-init clean, sync)")
		return nil
	}
	claim, _, err := core.ReadLeaseClaimWithPresence(f.lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{}
	for k, v := range claim.Labels {
		labels[k] = v
	}
	labels["tailscale"] = "true"
	labels["tailscale_state"] = "ready"
	if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(f.lease.LeaseID, claim, labels); err != nil {
		t.Fatal(err)
	}
	enrolled, _, err := core.ReadLeaseClaimWithPresence(f.lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	req := f.request()
	core.SetServerLeaseClaimSnapshot(&req.Server, enrolled, true)
	mark := f.ovh.mark()
	_, err = (Provider{}).CreateNativeCheckpoint(context.Background(), req)
	trace := f.traceSince(mark)
	logTrace(t, "tailscale-enrolled source: refused ("+firstLine(err)+")", trace)
	var notSubmitted core.NativeCheckpointNotSubmittedError
	if err == nil || !strings.Contains(err.Error(), "Tailscale-enrolled") || !errors.As(err, &notSubmitted) {
		t.Fatalf("err=%v", err)
	}
	if indexOf(trace, "** guest:") >= 0 || len(mutating(trace)) != 0 {
		t.Fatalf("enrolled source reached the guest or a provider mutation: %v", trace)
	}
}

func TestTailscaleEnrolledDetectsEveryEnrolmentSignal(t *testing.T) {
	for name, claim := range map[string]core.LeaseClaim{
		"label":      {Labels: map[string]string{"tailscale": "true"}},
		"state":      {Labels: map[string]string{"tailscale_state": "requested"}},
		"ipv4":       {TailscaleIPv4: "100.64.0.1"},
		"fqdn":       {TailscaleFQDN: "box.tail.example"},
		"label-case": {Labels: map[string]string{"tailscale": "TRUE"}},
	} {
		if !tailscaleEnrolled(claim) {
			t.Errorf("%s not detected", name)
		}
	}
	for name, claim := range map[string]core.LeaseClaim{
		"none":        {},
		"label-false": {Labels: map[string]string{"tailscale": "false"}},
	} {
		if tailscaleEnrolled(claim) {
			t.Errorf("%s falsely detected", name)
		}
	}
}
