package ovh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// fakeSnapshotAPI extends the fake OVH API with the snapshot endpoints. No test
// here talks to a real OVH account.
type fakeSnapshotAPI struct {
	*fakeAPI
	snapshots    []Image
	createdNames []string
	deleted      []string
	// states are returned by successive GetSnapshot calls; the last one repeats.
	states      []string
	createErr   error
	hideCreated bool
	omitID      bool
	mismatch    bool
	// lostResponse accepts the snapshot but reports a transport error.
	lostResponse bool
	// getHook observes each GetSnapshot context; getBlocksUntilDone makes the
	// call return the context error once the hook unblocks.
	getHook            func(context.Context)
	getBlocksUntilDone bool
	duplicate          bool
	getErr             error
	deleteErr          error
	listCalls          int
}

func (f *fakeSnapshotAPI) CreateInstanceSnapshot(_ context.Context, _, _, name string) (string, error) {
	f.createdNames = append(f.createdNames, name)
	if f.createErr != nil {
		return "", f.createErr
	}
	if f.lostResponse {
		f.snapshots = append(f.snapshots, Image{ID: "snap-1", Name: name, Region: "GRA11", Status: "queued"})
		return "", errors.New("connection reset")
	}
	if f.mismatch {
		return "snap-1", nil
	}
	if !f.hideCreated {
		f.snapshots = append(f.snapshots, Image{ID: "snap-1", Name: name, Region: "GRA11", Status: "queued", Type: "linux", Visibility: "private"})
		if f.duplicate {
			f.snapshots = append(f.snapshots, Image{ID: "snap-2", Name: name, Region: "GRA11", Status: "queued"})
		}
	}
	if f.omitID || f.hideCreated {
		return "", nil
	}
	return "snap-1", nil
}

func (f *fakeSnapshotAPI) ListSnapshots(context.Context, string, string) ([]Image, error) {
	f.listCalls++
	return append([]Image(nil), f.snapshots...), nil
}

func (f *fakeSnapshotAPI) GetSnapshot(ctx context.Context, _, id string) (Image, error) {
	if f.getHook != nil {
		f.getHook(ctx)
	}
	if ctx.Err() != nil {
		return Image{}, ctx.Err()
	}
	if f.getErr != nil {
		return Image{}, f.getErr
	}
	for i := range f.snapshots {
		if f.snapshots[i].ID == id {
			if len(f.states) > 0 {
				f.snapshots[i].Status = f.states[0]
				if len(f.states) > 1 {
					f.states = f.states[1:]
				}
			}
			return f.snapshots[i], nil
		}
	}
	return Image{}, &APIError{Status: 404}
}

func (f *fakeSnapshotAPI) DeleteSnapshot(_ context.Context, _, id string) error {
	f.deleted = append(f.deleted, id)
	return f.deleteErr
}

type checkpointFixture struct {
	t       *testing.T
	fake    *fakeSnapshotAPI
	lease   core.LeaseTarget
	prepare int
}

func newCheckpointFixture(t *testing.T) *checkpointFixture {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	base := &fakeAPI{flavors: []Flavor{{ID: "flavor-id", Name: "b3-8"}}, images: []Image{{ID: "image-id", Name: "Ubuntu 24.04"}}}
	lease, err := testBackend(base).Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "blue-lobster"})
	if err != nil {
		t.Fatal(err)
	}
	f := &checkpointFixture{t: t, fake: &fakeSnapshotAPI{fakeAPI: base, states: []string{"active"}}, lease: lease}
	oldClient, oldPrepare, oldSleep, oldNow := newSnapshotClient, prepareCheckpointSource, checkpointSleep, checkpointNow
	newSnapshotClient = func(core.Config, core.Runtime) (snapshotClient, error) { return f.fake, nil }
	prepareCheckpointSource = func(context.Context, core.SSHTarget) error { f.prepare++; return nil }
	var clock time.Time = time.Unix(1_700_000_000, 0)
	checkpointNow = func() time.Time { return clock }
	checkpointSleep = func(_ context.Context, d time.Duration) error { clock = clock.Add(d); return nil }
	t.Cleanup(func() {
		newSnapshotClient, prepareCheckpointSource, checkpointSleep, checkpointNow = oldClient, oldPrepare, oldSleep, oldNow
	})
	return f
}

func (f *checkpointFixture) config() core.Config {
	return core.Config{Provider: providerName, TargetOS: core.TargetLinux, OVH: core.OVHConfig{ProjectID: "project-test", Region: "GRA11"}}
}

func (f *checkpointFixture) request() core.NativeCheckpointCreateRequest {
	return core.NativeCheckpointCreateRequest{
		Config:       f.config(),
		Runtime:      core.Runtime{Stderr: io.Discard},
		Server:       f.lease.Server,
		LeaseID:      f.lease.LeaseID,
		CheckpointID: "chk_abc123",
		Strategy:     core.CheckpointStrategyDiskSnapshot,
		Wait:         true,
		WaitTimeout:  time.Hour,
	}
}

func (f *checkpointFixture) resource(image core.NativeCheckpointImage, metadata map[string]string) core.NativeCheckpointResourceRequest {
	return core.NativeCheckpointResourceRequest{
		LoadConfig: func() (core.Config, error) { return f.config(), nil },
		Runtime:    core.Runtime{Stderr: io.Discard},
		Image:      image,
		Metadata:   metadata,
	}
}

func ownedResource(f *checkpointFixture) core.NativeCheckpointResourceRequest {
	f.t.Helper()
	req := f.request()
	f.fake.states = []string{"active"}
	result, err := (Provider{}).CreateNativeCheckpoint(context.Background(), req)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.resource(result.Image, result.Metadata)
}

func TestCreateNativeCheckpointSnapshotsClaimedLeaseAndWaits(t *testing.T) {
	f := newCheckpointFixture(t)
	f.fake.states = []string{"queued", "saving", "active"}
	req := f.request()
	var persisted []core.NativeCheckpointCreateResult
	req.Persist = func(r core.NativeCheckpointCreateResult) error { persisted = append(persisted, r); return nil }
	var stderr bytes.Buffer
	req.Stderr = &stderr
	result, err := (Provider{}).CreateNativeCheckpoint(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.fake.createdNames) != 1 || f.fake.createdNames[0] != "crabbox-chk_abc123" || f.prepare != 1 {
		t.Fatalf("names=%v prepare=%d", f.fake.createdNames, f.prepare)
	}
	image := result.Image
	if image.ID != "snap-1" || image.Kind != core.CheckpointKindOVH || image.Provider != providerName || !image.Direct || image.State != "active" || image.Region != "GRA11" || !result.CaptureEstablished {
		t.Fatalf("image=%+v established=%t", image, result.CaptureEstablished)
	}
	want := map[string]string{
		"checkpoint": "chk_abc123", "lease": f.lease.LeaseID, "source_instance": "inst-1",
		"source_project": "project-test", "source_region": "GRA11", "source_type": "instance",
	}
	for k, v := range want {
		if result.Metadata[k] != v {
			t.Fatalf("metadata[%s]=%q want %q (%v)", k, result.Metadata[k], v, result.Metadata)
		}
	}
	// The image identity is journaled before readiness polling begins.
	if len(persisted) != 2 || persisted[0].Image.ID != "snap-1" || persisted[1].Image.ID != "snap-1" || persisted[1].Image.State == "active" {
		t.Fatalf("persisted=%+v", persisted)
	}
	if !strings.Contains(stderr.String(), "waiting image=snap-1") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestCreateNativeCheckpointFallsBackToNameWhenIDOmitted(t *testing.T) {
	f := newCheckpointFixture(t)
	f.fake.omitID = true
	result, err := (Provider{}).CreateNativeCheckpoint(context.Background(), f.request())
	if err != nil || result.Image.ID != "snap-1" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestCreateNativeCheckpointWithoutWaitReturnsPendingSnapshot(t *testing.T) {
	f := newCheckpointFixture(t)
	f.fake.states = []string{"queued"}
	req := f.request()
	req.Wait = false
	result, err := (Provider{}).CreateNativeCheckpoint(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Image.ID != "snap-1" || result.Image.State != "queued" || result.CaptureEstablished {
		t.Fatalf("result=%+v", result)
	}
}

func TestCreateNativeCheckpointRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*checkpointFixture, *core.NativeCheckpointCreateRequest)
		want   string
		// plain marks guards that run before the not-submitted attestation.
		plain bool
	}{
		"image strategy": {mutate: func(_ *checkpointFixture, r *core.NativeCheckpointCreateRequest) {
			r.Strategy = core.CheckpointStrategyImage
		}, want: "--strategy image is unsupported"},
		"capture retirement": {mutate: func(_ *checkpointFixture, r *core.NativeCheckpointCreateRequest) {
			r.Capture = &core.NativeCheckpointCapture{}
		}, want: "retirement is unsupported", plain: true},
		"non linux": {mutate: func(_ *checkpointFixture, r *core.NativeCheckpointCreateRequest) {
			r.Config.TargetOS = core.TargetWindows
		}, want: "require a Linux lease"},
		"bad checkpoint id": {mutate: func(_ *checkpointFixture, r *core.NativeCheckpointCreateRequest) {
			r.CheckpointID = "../x"
		}, want: "canonical checkpoint ID"},
		"missing project": {mutate: func(_ *checkpointFixture, r *core.NativeCheckpointCreateRequest) {
			r.Config.OVH.ProjectID = ""
		}, want: "ovh.projectId is required"},
		"project mismatch": {mutate: func(_ *checkpointFixture, r *core.NativeCheckpointCreateRequest) {
			r.Config.OVH.ProjectID = "other-project"
		}, want: "mismatch"},
		"lease without claim": {mutate: func(_ *checkpointFixture, r *core.NativeCheckpointCreateRequest) {
			r.LeaseID = "cbx_aaaaaaaaaaaa"
		}, want: "no exact local claim"},
		"existing snapshot name": {mutate: func(f *checkpointFixture, _ *core.NativeCheckpointCreateRequest) {
			f.fake.snapshots = []Image{{ID: "snap-9", Name: "crabbox-chk_abc123", Region: "GRA11"}}
		}, want: "already exists"},
		"prepare failure": {mutate: func(f *checkpointFixture, _ *core.NativeCheckpointCreateRequest) {
			prepareCheckpointSource = func(context.Context, core.SSHTarget) error { return errors.New("prep failed") }
		}, want: "prep failed"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newCheckpointFixture(t)
			req := f.request()
			tc.mutate(f, &req)
			_, err := (Provider{}).CreateNativeCheckpoint(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want %q", err, tc.want)
			}
			var notSubmitted core.NativeCheckpointNotSubmittedError
			if !tc.plain && !errors.As(err, &notSubmitted) {
				t.Fatalf("pre-submission failure must be attested not-submitted: %T", err)
			}
			if len(f.fake.createdNames) != 0 {
				t.Fatalf("snapshot requested despite refusal: %v", f.fake.createdNames)
			}
		})
	}
}

func TestCreateNativeCheckpointRefusesRecreatedInstanceIdentity(t *testing.T) {
	f := newCheckpointFixture(t)
	f.fake.instances[0].Name = "someone-elses-box"
	_, err := (Provider{}).CreateNativeCheckpoint(context.Background(), f.request())
	if err == nil || !strings.Contains(err.Error(), "source lease mismatch") || len(f.fake.createdNames) != 0 {
		t.Fatalf("err=%v names=%v", err, f.fake.createdNames)
	}
}

func TestCreateNativeCheckpointKeepsCustodyAfterSubmission(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(*fakeSnapshotAPI)
		want  string
	}{
		"request error after send": {setup: func(f *fakeSnapshotAPI) { f.createErr = errors.New("connection reset") }, want: "connection reset"},
		"snapshot never listed":    {setup: func(f *fakeSnapshotAPI) { f.hideCreated = true }, want: "delete it by hand"},
		"duplicate names":          {setup: func(f *fakeSnapshotAPI) { f.duplicate, f.omitID = true, true }, want: "refusing to guess"},
		"returned id has foreign name": {setup: func(f *fakeSnapshotAPI) {
			f.hideCreated = false
			f.snapshots = []Image{{ID: "snap-1", Name: "other", Region: "GRA11"}}
			f.createErr = nil
			f.mismatch = true
		}, want: "does not match requested name"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newCheckpointFixture(t)
			tc.setup(f.fake)
			_, err := (Provider{}).CreateNativeCheckpoint(context.Background(), f.request())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want %q", err, tc.want)
			}
			var notSubmitted core.NativeCheckpointNotSubmittedError
			if errors.As(err, &notSubmitted) {
				t.Fatalf("post-submission failure must not claim not-submitted: %v", err)
			}
		})
	}
}

func TestCreateNativeCheckpointJournalsReturnedIDBeforeFollowUpRead(t *testing.T) {
	f := newCheckpointFixture(t)
	f.fake.getErr = &APIError{Status: 503}
	var persisted []core.NativeCheckpointCreateResult
	req := f.request()
	req.Persist = func(r core.NativeCheckpointCreateResult) error { persisted = append(persisted, r); return nil }
	result, err := (Provider{}).CreateNativeCheckpoint(context.Background(), req)
	if err == nil {
		t.Fatal("expected the follow-up read failure")
	}
	if len(persisted) != 1 || persisted[0].Image.ID != "snap-1" || result.Image.ID != "snap-1" {
		t.Fatalf("persisted=%+v result=%+v", persisted, result)
	}
}

func TestCreateNativeCheckpointDefinitiveRejectionDropsReservation(t *testing.T) {
	f := newCheckpointFixture(t)
	f.fake.createErr = &APIError{Status: 403}
	_, err := (Provider{}).CreateNativeCheckpoint(context.Background(), f.request())
	var notSubmitted core.NativeCheckpointNotSubmittedError
	if err == nil || !errors.As(err, &notSubmitted) {
		t.Fatalf("a 4xx refusal proves nothing was submitted: %T %v", err, err)
	}
	// A throttle or timeout is ambiguous and keeps custody.
	f2 := newCheckpointFixture(t)
	f2.fake.createErr = &APIError{Status: 429}
	if _, err := (Provider{}).CreateNativeCheckpoint(context.Background(), f2.request()); err == nil || errors.As(err, &notSubmitted) {
		t.Fatalf("429 is ambiguous: %v", err)
	}
}

func TestCreateNativeCheckpointRecoversSnapshotAfterLostResponse(t *testing.T) {
	f := newCheckpointFixture(t)
	f.fake.lostResponse = true
	var persisted []core.NativeCheckpointCreateResult
	req := f.request()
	req.Persist = func(r core.NativeCheckpointCreateResult) error { persisted = append(persisted, r); return nil }
	result, err := (Provider{}).CreateNativeCheckpoint(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("err=%v", err)
	}
	if result.Image.ID != "snap-1" || len(persisted) != 1 || persisted[0].Image.ID != "snap-1" {
		t.Fatalf("result=%+v persisted=%+v", result, persisted)
	}
}

func TestCreateNativeCheckpointWaitOutcomes(t *testing.T) {
	for name, tc := range map[string]struct {
		states []string
		want   string
	}{
		"failed":  {states: []string{"saving", "error"}, want: "failed; last state=error"},
		"timeout": {states: []string{"saving"}, want: "timed out waiting for OVH snapshot snap-1"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newCheckpointFixture(t)
			f.fake.states = tc.states
			req := f.request()
			req.WaitTimeout = time.Minute
			result, err := (Provider{}).CreateNativeCheckpoint(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want %q", err, tc.want)
			}
			// The snapshot identity stays in the result so the caller can delete it.
			if result.Image.ID != "snap-1" || result.CaptureEstablished {
				t.Fatalf("result=%+v", result)
			}
		})
	}
}

func TestVerifyNativeCheckpointReportsNextAction(t *testing.T) {
	f := newCheckpointFixture(t)
	res := ownedResource(f)
	for state, next := range map[string]string{"active": "fork_or_delete", "saving": "wait_or_delete", "error": "delete"} {
		f.fake.states = []string{state}
		got, err := (Provider{}).VerifyNativeCheckpoint(context.Background(), res)
		if err != nil || got.ProviderState != state || got.NextAction != next {
			t.Fatalf("state=%s got=%+v err=%v", state, got, err)
		}
	}
}

func TestDeleteNativeCheckpointRequiresExactOwnership(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*checkpointFixture, *core.NativeCheckpointResourceRequest)
		want   string
	}{
		"name drift": {mutate: func(f *checkpointFixture, _ *core.NativeCheckpointResourceRequest) {
			f.fake.snapshots[0].Name = "crabbox-chk_other"
		}, want: "mismatched name"},
		"foreign snapshot": {mutate: func(f *checkpointFixture, _ *core.NativeCheckpointResourceRequest) {
			f.fake.snapshots[0].Name = "my-own-backup"
		}, want: "mismatched name"},
		"region drift": {mutate: func(f *checkpointFixture, _ *core.NativeCheckpointResourceRequest) {
			f.fake.snapshots[0].Region = "BHS5"
		}, want: "mismatched region"},
		"record region drift": {mutate: func(_ *checkpointFixture, r *core.NativeCheckpointResourceRequest) {
			r.Image.Region = "BHS5"
		}, want: "mismatched region"},
		"different project": {mutate: func(_ *checkpointFixture, r *core.NativeCheckpointResourceRequest) {
			r.Metadata["source_project"] = "other-project"
		}, want: "different Public Cloud project"},
		"non direct": {mutate: func(_ *checkpointFixture, r *core.NativeCheckpointResourceRequest) {
			r.Image.Direct = false
		}, want: "non-direct"},
		"wrong kind": {mutate: func(_ *checkpointFixture, r *core.NativeCheckpointResourceRequest) {
			r.Image.Kind = core.CheckpointKindHetzner
		}, want: "non-direct"},
		"missing source binding": {mutate: func(_ *checkpointFixture, r *core.NativeCheckpointResourceRequest) {
			delete(r.Metadata, "source_instance")
		}, want: "missing ownership"},
		"no config loader": {mutate: func(_ *checkpointFixture, r *core.NativeCheckpointResourceRequest) {
			r.LoadConfig = nil
		}, want: "current CLI configuration"},
		"gone": {mutate: func(f *checkpointFixture, _ *core.NativeCheckpointResourceRequest) {
			f.fake.snapshots = nil
		}, want: "was not found"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newCheckpointFixture(t)
			res := ownedResource(f)
			tc.mutate(f, &res)
			err := (Provider{}).DeleteNativeCheckpoint(context.Background(), res)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want %q", err, tc.want)
			}
			if len(f.fake.deleted) != 0 {
				t.Fatalf("deleted despite refusal: %v", f.fake.deleted)
			}
		})
	}
}

func TestDeleteNativeCheckpointDeletesOwnedSnapshot(t *testing.T) {
	f := newCheckpointFixture(t)
	res := ownedResource(f)
	if err := (Provider{}).DeleteNativeCheckpoint(context.Background(), res); err != nil {
		t.Fatal(err)
	}
	if len(f.fake.deleted) != 1 || f.fake.deleted[0] != "snap-1" {
		t.Fatalf("deleted=%v", f.fake.deleted)
	}
	f.fake.deleteErr = &APIError{Status: 404}
	err := (Provider{}).DeleteNativeCheckpoint(context.Background(), res)
	if err == nil || !strings.Contains(err.Error(), "disappeared before deletion") {
		t.Fatalf("err=%v", err)
	}
	f.fake.deleteErr = errors.New("api down")
	if err := (Provider{}).DeleteNativeCheckpoint(context.Background(), res); err == nil || !strings.Contains(err.Error(), "api down") {
		t.Fatalf("err=%v", err)
	}
}

func TestApplyNativeCheckpointForkConfig(t *testing.T) {
	record := core.NativeCheckpointForkRecord{
		Kind: core.CheckpointKindOVH, ImageID: "snap-1", Region: "GRA11", Direct: true,
		Metadata: map[string]string{"source_type": "instance", "source_project": "project-test", "source_region": "GRA11"},
	}
	cfg := core.Config{OVH: core.OVHConfig{ProjectID: "project-test", Region: "BHS5", Image: "Ubuntu 24.04"}}
	if err := (Provider{}).ApplyNativeCheckpointForkConfig(core.NativeCheckpointForkRequest{Config: &cfg, Record: record}); err != nil {
		t.Fatal(err)
	}
	if cfg.OVH.Image != "snap-1" || cfg.OVH.Region != "GRA11" || !core.OVHImageWasExplicit(cfg) {
		t.Fatalf("cfg=%+v", cfg.OVH)
	}
	for name, mutate := range map[string]func(*core.NativeCheckpointForkRecord, *core.Config){
		"wrong kind":      func(r *core.NativeCheckpointForkRecord, _ *core.Config) { r.Kind = core.CheckpointKindHetzner },
		"not direct":      func(r *core.NativeCheckpointForkRecord, _ *core.Config) { r.Direct = false },
		"no image":        func(r *core.NativeCheckpointForkRecord, _ *core.Config) { r.ImageID = " " },
		"source type":     func(r *core.NativeCheckpointForkRecord, _ *core.Config) { r.Metadata["source_type"] = "volume" },
		"other project":   func(_ *core.NativeCheckpointForkRecord, c *core.Config) { c.OVH.ProjectID = "other" },
		"region mismatch": func(r *core.NativeCheckpointForkRecord, _ *core.Config) { r.Metadata["source_region"] = "BHS5" },
		"no region":       func(r *core.NativeCheckpointForkRecord, _ *core.Config) { r.Region = "" },
	} {
		t.Run(name, func(t *testing.T) {
			r := record
			r.Metadata = map[string]string{}
			for k, v := range record.Metadata {
				r.Metadata[k] = v
			}
			c := core.Config{OVH: core.OVHConfig{ProjectID: "project-test", Image: "Ubuntu 24.04"}}
			mutate(&r, &c)
			if err := (Provider{}).ApplyNativeCheckpointForkConfig(core.NativeCheckpointForkRequest{Config: &c, Record: r}); err == nil {
				t.Fatal("expected refusal")
			}
			if c.OVH.Image == "snap-1" {
				t.Fatal("config mutated despite refusal")
			}
		})
	}
}

func TestNativeCheckpointCapability(t *testing.T) {
	server := core.Server{CloudID: "inst-1"}
	got, ok := (Provider{}).NativeCheckpointCapability(core.NativeCheckpointRequest{Config: core.Config{TargetOS: core.TargetLinux}, Server: server, Strategy: core.CheckpointStrategyDiskSnapshot})
	if !ok || got.Kind != core.CheckpointKindOVH || !got.Direct || got.CreateUnsupported != "" || got.RetireUnsupported == "" {
		t.Fatalf("got=%+v ok=%t", got, ok)
	}
	got, ok = (Provider{}).NativeCheckpointCapability(core.NativeCheckpointRequest{Config: core.Config{TargetOS: core.TargetLinux}, Server: server, Strategy: core.CheckpointStrategyImage})
	if !ok || got.CreateUnsupported == "" {
		t.Fatalf("image strategy must be unsupported: %+v", got)
	}
	if _, ok := (Provider{}).NativeCheckpointCapability(core.NativeCheckpointRequest{Config: core.Config{TargetOS: core.TargetWindows}, Server: server}); ok {
		t.Fatal("non-Linux target must not advertise checkpoints")
	}
	if _, ok := (Provider{}).NativeCheckpointCapability(core.NativeCheckpointRequest{Config: core.Config{TargetOS: core.TargetLinux}}); ok {
		t.Fatal("serverless request must not advertise checkpoints")
	}
}

func TestNativeCheckpointWorkdir(t *testing.T) {
	cfg := core.Config{WorkRoot: "/work/crabbox"}
	if got := (Provider{}).NativeCheckpointWorkdir(core.NativeCheckpointWorkdirRequest{Config: cfg, Override: " /custom "}); got != "/custom" {
		t.Fatalf("override=%q", got)
	}
	got := (Provider{}).NativeCheckpointWorkdir(core.NativeCheckpointWorkdirRequest{Config: cfg, Server: core.Server{Labels: map[string]string{"work_root": "/labelled"}}, LeaseID: "cbx_abcdef123456", RepoName: "app"})
	if !strings.HasPrefix(got, "/labelled/") || !strings.HasSuffix(got, "app") {
		t.Fatalf("workdir=%q", got)
	}
}

func TestResolveImageBootsFromCheckpointSnapshot(t *testing.T) {
	ctx := context.Background()
	newAPI := func(snaps ...Image) *fakeSnapshotAPI {
		return &fakeSnapshotAPI{fakeAPI: &fakeAPI{images: []Image{{ID: "image-id", Name: "Ubuntu 24.04"}}}, snapshots: snaps}
	}
	active := Image{ID: "snap-1", Name: "crabbox-chk_abc123", Region: "GRA11", Status: "active"}

	got, err := resolveImage(ctx, newAPI(active), "project-test", "GRA11", "snap-1")
	if err != nil || got.ID != "snap-1" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	// Public images keep their normal resolution and never consult snapshots.
	api := newAPI(active)
	if got, err := resolveImage(ctx, api, "project-test", "GRA11", "Ubuntu 24.04"); err != nil || got.ID != "image-id" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	for name, tc := range map[string]struct {
		snap Image
		want string
	}{
		"foreign name":      {Image{ID: "snap-1", Name: "my-backup", Region: "GRA11", Status: "active"}, "was not returned"},
		"not yet active":    {Image{ID: "snap-1", Name: "crabbox-chk_abc123", Region: "GRA11", Status: "saving"}, "status=saving"},
		"other region":      {Image{ID: "snap-1", Name: "crabbox-chk_abc123", Region: "BHS5", Status: "active"}, "region=BHS5"},
		"name only, not id": {Image{ID: "snap-9", Name: "crabbox-chk_abc123", Region: "GRA11", Status: "active"}, "was not returned"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := resolveImage(ctx, newAPI(tc.snap), "project-test", "GRA11", "snap-1")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want %q", err, tc.want)
			}
		})
	}
	// A client without snapshot support keeps the original error.
	if _, err := resolveImage(ctx, &fakeAPI{images: []Image{{ID: "image-id", Name: "Ubuntu 24.04"}}}, "project-test", "GRA11", "snap-1"); err == nil || !strings.Contains(err.Error(), "was not returned") {
		t.Fatalf("err=%v", err)
	}
	// An id OVH rejects with a client error keeps the ordinary diagnostic.
	rejected := newAPI(active)
	rejected.getErr = &APIError{Status: 400}
	if _, err := resolveImage(ctx, rejected, "project-test", "GRA11", "snap-1"); err == nil || !strings.Contains(err.Error(), "was not returned") {
		t.Fatalf("err=%v", err)
	}
	// A lookup failure other than a client error is surfaced, not swallowed.
	broken := newAPI(active)
	broken.getErr = errors.New("api down")
	if _, err := resolveImage(ctx, broken, "project-test", "GRA11", "snap-1"); err == nil || !strings.Contains(err.Error(), "api down") {
		t.Fatalf("err=%v", err)
	}
}

// holdSource journals an unresolved native checkpoint for the lease, as a lost
// snapshot response would, directly in the local checkpoint store.
func holdSource(t *testing.T, leaseID string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "checkpoints", "chk_held")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	record := fmt.Sprintf(`{"id":"chk_held","kind":%q,"provider":"ovh","leaseId":%q,"createdAt":%q,"native":{"provider":"ovh","direct":true}}`,
		core.CheckpointKindOVH, leaseID, time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.json"), []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseAndCleanupRefuseSourceWithUnresolvedCheckpoint(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	fake := &fakeAPI{flavors: []Flavor{{ID: "flavor-id", Name: "b3-8"}}, images: []Image{{ID: "image-id", Name: "Ubuntu 24.04"}}}
	backend := testBackend(fake)
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "held-lobster"})
	if err != nil {
		t.Fatal(err)
	}
	holdSource(t, lease.LeaseID)

	err = backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease})
	if err == nil || !strings.Contains(err.Error(), "unresolved checkpoint chk_held") {
		t.Fatalf("release err=%v", err)
	}
	if len(fake.deletedInstances) != 0 || len(fake.deletedKeys) != 0 {
		t.Fatalf("release deleted a held source: instances=%v keys=%v", fake.deletedInstances, fake.deletedKeys)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID); err != nil || !exists {
		t.Fatalf("claim exists=%t err=%v", exists, err)
	}

	// Cleanup skips the held lease instead of aborting or deleting it.
	if err := markClaimReleased(lease.LeaseID); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	backend.RT.Stderr = &stderr
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deletedInstances) != 0 || !strings.Contains(stderr.String(), "checkpoint hold") {
		t.Fatalf("deleted=%v stderr=%q", fake.deletedInstances, stderr.String())
	}
}

func TestDoctorAcceptsCheckpointSnapshotImage(t *testing.T) {
	base := &fakeAPI{
		regions: []Region{{Name: "GRA11"}},
		flavors: []Flavor{{ID: "flavor-id", Name: "b3-8"}},
		images:  []Image{{ID: "image-id", Name: "Ubuntu 24.04"}},
	}
	api := &fakeSnapshotAPI{fakeAPI: base, snapshots: []Image{{ID: "snap-1", Name: "crabbox-chk_abc123", Region: "GRA11", Status: "active"}}}
	cfg := core.Config{OVH: core.OVHConfig{ProjectID: "project-test", Region: "GRA11", Image: "snap-1", Flavor: "b3-8"}}
	backend := NewBackend(Provider{}.Spec(), cfg, core.Runtime{})
	backend.clientFactory = func(core.Config, core.Runtime) (API, error) { return api, nil }
	result, err := backend.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil || result.Status == "failed" || !strings.Contains(result.Message, "inventory=ready") {
		t.Fatalf("snapshot image must pass the doctor: %+v err=%v", result, err)
	}
	for _, check := range result.Checks {
		if check.Status == "failed" {
			t.Fatalf("snapshot image failed a doctor check: %+v", check)
		}
	}
	// A snapshot that is not usable fails the image check, not the whole run.
	api.snapshots[0].Status = "saving"
	result, err = backend.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil || len(result.Checks) != 1 || result.Checks[0].Check != "image" || !strings.Contains(result.Checks[0].Message, "status=saving") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestDeleteNativeCheckpointNeverGuessesTargetAfterLostResponse(t *testing.T) {
	f := newCheckpointFixture(t)
	res := ownedResource(f)
	res.Image.ID = ""
	// A same-named snapshot exists, but a record without an id must not delete it.
	err := (Provider{}).DeleteNativeCheckpoint(context.Background(), res)
	if err == nil || !strings.Contains(err.Error(), "create response was lost") || !strings.Contains(err.Error(), "crabbox-chk_abc123") || strings.Contains(err.Error(), "--local-only") {
		t.Fatalf("err=%v", err)
	}
	if len(f.fake.deleted) != 0 {
		t.Fatalf("deleted by name: %v", f.fake.deleted)
	}
}

func TestAcquireForkValidatesSnapshotBindingBeforeAllocating(t *testing.T) {
	record := func() *core.NativeCheckpointForkRecord {
		return &core.NativeCheckpointForkRecord{
			Kind: core.CheckpointKindOVH, ImageID: "snap-1", Region: "GRA11", Direct: true,
			Metadata: map[string]string{
				"checkpoint": "chk_abc123", "lease": "cbx_abcdef123456", "source_instance": "inst-9",
				"source_project": "project-test", "source_region": "GRA11", "source_type": "instance",
			},
		}
	}
	active := Image{ID: "snap-1", Name: "crabbox-chk_abc123", Region: "GRA11", Status: "active"}
	for name, tc := range map[string]struct {
		snap   Image
		mutate func(*core.NativeCheckpointForkRecord)
		want   string
	}{
		"renamed snapshot":  {snap: Image{ID: "snap-1", Name: "crabbox-chk_other", Region: "GRA11", Status: "active"}, want: "mismatched name"},
		"moved region":      {snap: Image{ID: "snap-1", Name: "crabbox-chk_abc123", Region: "BHS5", Status: "active"}, want: "in region=BHS5"},
		"not active":        {snap: Image{ID: "snap-1", Name: "crabbox-chk_abc123", Region: "GRA11", Status: "saving"}, want: "status=saving"},
		"other project":     {snap: active, mutate: func(r *core.NativeCheckpointForkRecord) { r.Metadata["source_project"] = "other" }, want: "different Public Cloud project"},
		"record is not ovh": {snap: active, mutate: func(r *core.NativeCheckpointForkRecord) { r.Kind = core.CheckpointKindHetzner }, want: "does not support checkpoint kind"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			base := &fakeAPI{flavors: []Flavor{{ID: "flavor-id", Name: "b3-8"}}, images: []Image{{ID: "image-id", Name: "Ubuntu 24.04"}}}
			api := &fakeSnapshotAPI{fakeAPI: base, snapshots: []Image{tc.snap}}
			backend := testBackend(base)
			backend.Cfg.OVH.Image = "snap-1"
			backend.clientFactory = func(core.Config, core.Runtime) (API, error) { return api, nil }
			rec := record()
			if tc.mutate != nil {
				tc.mutate(rec)
			}
			_, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, CheckpointSource: rec})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want %q", err, tc.want)
			}
			if len(base.createInstances) != 0 || len(base.sshKeys) != 0 {
				t.Fatalf("allocated before validation: instances=%v keys=%v", base.createInstances, base.sshKeys)
			}
		})
	}

	// A record that matches the live snapshot provisions the child.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	base := &fakeAPI{flavors: []Flavor{{ID: "flavor-id", Name: "b3-8"}}, images: []Image{{ID: "image-id", Name: "Ubuntu 24.04"}}}
	api := &fakeSnapshotAPI{fakeAPI: base, snapshots: []Image{active}}
	backend := testBackend(base)
	backend.Cfg.OVH.Image = "snap-1"
	backend.clientFactory = func(core.Config, core.Runtime) (API, error) { return api, nil }
	if _, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, CheckpointSource: record()}); err != nil {
		t.Fatal(err)
	}
	if len(base.createInstances) != 1 || base.createInstances[0].ImageID != "snap-1" {
		t.Fatalf("createInstances=%+v", base.createInstances)
	}
}

func TestWaitForSnapshotBoundsEachReadByTheWaitTimeout(t *testing.T) {
	f := newCheckpointFixture(t)
	var deadlines []time.Duration
	f.fake.getHook = func(ctx context.Context) {
		// The post-create locate read runs on the caller context; only the
		// readiness polling is bounded by the wait budget.
		if d, ok := ctx.Deadline(); ok {
			deadlines = append(deadlines, time.Until(d))
		}
	}
	f.fake.states = []string{"saving", "saving", "active"}
	req := f.request()
	req.WaitTimeout = 10 * time.Minute
	if _, err := (Provider{}).CreateNativeCheckpoint(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(deadlines) == 0 {
		t.Fatal("no polling reads observed")
	}
	for _, d := range deadlines {
		if d <= 0 || d > 10*time.Minute {
			t.Fatalf("read deadline %v outside the wait budget", d)
		}
	}
}

func TestWaitForSnapshotTimesOutWhenReadOutlivesTheWait(t *testing.T) {
	f := newCheckpointFixture(t)
	reads := 0
	f.fake.getHook = func(ctx context.Context) {
		reads++
		if reads > 1 { // the first read is the unbounded post-create locate
			<-ctx.Done()
		}
	}
	req := f.request()
	req.WaitTimeout = 20 * time.Millisecond
	// Real time governs the request context; the fixture clock only governs polling.
	checkpointNow = time.Now
	result, err := (Provider{}).CreateNativeCheckpoint(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "timed out waiting for OVH snapshot snap-1") {
		t.Fatalf("err=%v", err)
	}
	if result.Image.ID != "snap-1" {
		t.Fatalf("result=%+v", result)
	}
}

func TestReleaseRunsGuardedCleanupOnlyAfterAuthorization(t *testing.T) {
	newBackend := func(t *testing.T) (*Backend, *fakeAPI, core.LeaseTarget) {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		fake := &fakeAPI{flavors: []Flavor{{ID: "flavor-id", Name: "b3-8"}}, images: []Image{{ID: "image-id", Name: "Ubuntu 24.04"}}}
		backend := testBackend(fake)
		lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "cleanup-lobster"})
		if err != nil {
			t.Fatal(err)
		}
		return backend, fake, lease
	}
	if backend, _, _ := newBackend(t); backend.ReleaseLeaseConnectionCleanupSafe() {
		t.Fatal("generic connection cleanup must be deferred to the guarded callback")
	}

	t.Run("held source never runs remote cleanup", func(t *testing.T) {
		backend, fake, lease := newBackend(t)
		holdSource(t, lease.LeaseID)
		cleaned := 0
		err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease, GuardedRemoteCleanup: func(context.Context, core.LeaseTarget) { cleaned++ }})
		if err == nil || cleaned != 0 || len(fake.deletedInstances) != 0 {
			t.Fatalf("err=%v cleaned=%d deleted=%v", err, cleaned, fake.deletedInstances)
		}
	})

	t.Run("authorized release cleans up before the delete", func(t *testing.T) {
		backend, fake, lease := newBackend(t)
		var order []string
		fake.onDeleteInstance = func() { order = append(order, "delete") }
		err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease, GuardedRemoteCleanup: func(context.Context, core.LeaseTarget) { order = append(order, "cleanup") }})
		if err != nil || strings.Join(order, ",") != "cleanup,delete" {
			t.Fatalf("err=%v order=%v", err, order)
		}
	})
}

func TestCreateNativeCheckpointRefusesReclaimedSourceGeneration(t *testing.T) {
	f := newCheckpointFixture(t)
	stale, exists, err := core.ReadLeaseClaimWithPresence(f.lease.LeaseID)
	if err != nil || !exists {
		t.Fatalf("claim exists=%t err=%v", exists, err)
	}
	// Another checkout reclaims the lease after this request was admitted.
	reclaimed := stale
	reclaimed.Labels = map[string]string{}
	for k, v := range stale.Labels {
		reclaimed.Labels[k] = v
	}
	reclaimed.Labels["state"] = "reclaimed-elsewhere"
	if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(f.lease.LeaseID, stale, reclaimed.Labels); err != nil {
		t.Fatal(err)
	}
	req := f.request()
	core.SetServerLeaseClaimSnapshot(&req.Server, stale, true)
	_, err = (Provider{}).CreateNativeCheckpoint(context.Background(), req)
	if err == nil || !errors.Is(err, core.ErrReleaseLeaseOwnershipChanged) {
		t.Fatalf("err=%v", err)
	}
	if len(f.fake.createdNames) != 0 || f.prepare != 0 {
		t.Fatalf("stale generation reached the guest or OVH: names=%v prepare=%d", f.fake.createdNames, f.prepare)
	}

	// The current generation is accepted.
	current, _, _ := core.ReadLeaseClaimWithPresence(f.lease.LeaseID)
	req = f.request()
	core.SetServerLeaseClaimSnapshot(&req.Server, current, true)
	if _, err := (Provider{}).CreateNativeCheckpoint(context.Background(), req); err != nil {
		t.Fatalf("current generation refused: %v", err)
	}
}
