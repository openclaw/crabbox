package incus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/api"
	core "github.com/openclaw/crabbox/internal/cli"
)

func (f *fakeClient) Identity() (connectionIdentity, error) {
	if f.identity.Endpoint != "" {
		return f.identity, nil
	}
	return connectionIdentity{Endpoint: "unix:/test/incus.socket", Project: "default", Certificate: "test-daemon"}, nil
}
func (f *fakeClient) CreateSnapshot(ctx context.Context, name, snapshot string) error {
	if f.snapshotErr != nil {
		return f.snapshotErr
	}
	if f.snapshots == nil {
		f.snapshots = map[string]*api.InstanceSnapshot{}
	}
	key := name + "/" + snapshot
	if f.snapshots[key] != nil {
		return api.StatusErrorf(409, "snapshot exists")
	}
	f.snapshots[key] = &api.InstanceSnapshot{Name: snapshot, Config: maps.Clone(f.instances[name].Config)}
	return nil
}
func (f *fakeClient) GetSnapshot(name, snapshot string) (*api.InstanceSnapshot, error) {
	snap := f.snapshots[name+"/"+snapshot]
	if snap == nil || f.instances[name] == nil {
		return nil, api.StatusErrorf(404, "snapshot missing")
	}
	return snap, nil
}
func (f *fakeClient) DeleteSnapshot(ctx context.Context, name, snapshot string) error {
	if f.deleteSnapshotErr != nil {
		return f.deleteSnapshotErr
	}
	delete(f.snapshots, name+"/"+snapshot)
	return nil
}
func (f *fakeClient) PublishSnapshot(ctx context.Context, name, snapshot string, properties map[string]string) (string, error) {
	if f.publishErr != nil {
		return "", f.publishErr
	}
	if _, err := f.GetSnapshot(name, snapshot); err != nil {
		return "", err
	}
	if f.images == nil {
		f.images = map[string]*api.Image{}
	}
	if f.imageFiles == nil {
		f.imageFiles = map[string]map[string][]byte{}
	}
	id := strings.Repeat("a", 64)
	f.images[id] = &api.Image{Fingerprint: id, Type: "container", ImagePut: api.ImagePut{Properties: maps.Clone(properties)}}
	f.imageFiles[id] = cloneFiles(f.files[name])
	if f.lostPublishReply != nil {
		return "", f.lostPublishReply
	}
	return id, nil
}
func (f *fakeClient) ListImages() ([]api.Image, error) {
	images := []api.Image{}
	for _, image := range f.images {
		images = append(images, *image)
	}
	return images, nil
}
func (f *fakeClient) GetImage(id string) (*api.Image, error) {
	image := f.images[id]
	if image == nil {
		return nil, api.StatusErrorf(404, "image missing")
	}
	if f.afterImageRead != nil {
		f.afterImageRead()
	}
	return image, nil
}
func (f *fakeClient) DeleteImage(ctx context.Context, id string) error {
	if f.deleteImageErr != nil {
		return f.deleteImageErr
	}
	delete(f.images, id)
	return f.lostImageDeleteReply
}
func (f *fakeClient) ReadFile(name, path string) ([]byte, error) {
	if data, ok := f.files[name][path]; ok {
		return data, nil
	}
	return nil, api.StatusErrorf(404, "file missing")
}
func (f *fakeClient) WriteFile(name, path string, data []byte, mode int) error {
	if f.writeFileErr != nil {
		return f.writeFileErr
	}
	if f.instances[name].IsActive() {
		return errors.New("fixture refuses identity mutation on active instance")
	}
	f.files[name][path] = append([]byte(nil), data...)
	return nil
}
func cloneFiles(files map[string][]byte) map[string][]byte {
	result := map[string][]byte{}
	for k, v := range files {
		result[k] = append([]byte(nil), v...)
	}
	return result
}

func lifecycleFixture(t *testing.T) (*backend, *fakeClient, AcquireRequest) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	fake := &fakeClient{instances: map[string]*api.Instance{}, states: map[string]*api.InstanceState{}}
	oldClient, oldWait := newClient, waitForSSHReady
	newClient = func(Config) (instanceClient, error) { return fake, nil }
	waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	t.Cleanup(func() { newClient, waitForSSHReady = oldClient, oldWait })
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	return b, fake, AcquireRequest{RequestedLeaseID: "cbx_123456789abc", RequestedSlug: "quiet-lobster", Repo: core.Repo{Root: t.TempDir()}, Keep: true}
}

func TestFixedIncusLostResponseConcurrentReplayAndConflicts(t *testing.T) {
	b, fake, req := lifecycleFixture(t)
	fake.lostCreateReply = errors.New("reply lost after commit")
	if _, err := b.Acquire(context.Background(), req); err == nil {
		t.Fatal("lost response should remain uncertain")
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || claim.FixedCreateIntent == nil || claim.FixedCreateIntent.Attempt["uuid"] == "" {
		t.Fatalf("missing durable cleanup identity: %+v %v", claim, err)
	}
	fake.lostCreateReply = nil
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := b.Acquire(context.Background(), req)
			if err != nil || lease.LeaseID != req.RequestedLeaseID {
				t.Errorf("replay: lease=%s err=%v", lease.LeaseID, err)
			}
		}()
	}
	wg.Wait()
	if len(fake.created) != 1 {
		t.Fatalf("created %d instances", len(fake.created))
	}
	for _, change := range []struct {
		name  string
		apply func()
	}{
		{"image", func() { b.cfg.Incus.Image = "different" }},
		{"checkpoint", func() { req.RequestedCheckpointID = "chk_other" }},
		{"project", func() { fake.identity, _ = fake.Identity(); fake.identity.Project = "other" }},
		{"daemon", func() { fake.identity, _ = fake.Identity(); fake.identity.Certificate = "other" }},
		{"ownership", func() { fake.instances[fake.created[0].Name].Config["volatile.uuid"] = "replacement" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			oldCfg, oldReq, oldIdentity := b.cfg, req, fake.identity
			original := cloneMap(fake.instances[fake.created[0].Name].Config)
			change.apply()
			if _, err := b.Acquire(context.Background(), req); err == nil {
				t.Fatal("conflicting replay accepted")
			}
			b.cfg, req, fake.identity = oldCfg, oldReq, oldIdentity
			fake.instances[fake.created[0].Name].Config = original
		})
	}
}

func TestIncusCheckpointSurvivesSourceAndForkReplacesIdentity(t *testing.T) {
	b, fake, req := lifecycleFixture(t)
	source, err := b.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	sourceFiles := fake.files[source.Server.Name]
	sourceFiles["/work/crabbox/workspace/dependency"] = []byte("installed dependency")
	sourceFiles["/home/crabbox/.ssh/authorized_keys"] = []byte("source-authorized-key")
	sourceFiles["/etc/ssh/ssh_host_ed25519_key"] = []byte("source-host-key")
	sourceFiles["/etc/machine-id"] = []byte("source-machine-id")
	before := cloneFiles(sourceFiles)
	p := Provider{}
	checkpoint, err := p.CreateNativeCheckpoint(context.Background(), core.NativeCheckpointCreateRequest{Persist: (&checkpointRecorder{}).persist, Config: b.cfg, Server: source.Server, LeaseID: source.LeaseID, CheckpointID: "chk_lifecycle", NoReboot: true, Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	if !maps.EqualFunc(before, sourceFiles, bytes.Equal) || !fake.instances[source.Server.Name].IsActive() {
		t.Fatal("capture changed live source")
	}
	if len(fake.snapshots) != 0 {
		t.Fatal("temporary capture snapshot leaked")
	}
	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: source, Force: true}); err != nil {
		t.Fatal(err)
	}
	resource := core.NativeCheckpointResourceRequest{Persist: (&checkpointRecorder{}).persist, Image: checkpoint.Image, Metadata: checkpoint.Metadata}
	verify, err := p.VerifyNativeCheckpoint(context.Background(), resource)
	if err != nil || verify.ProviderState != "available" {
		t.Fatalf("verify after source deletion: %+v %v", verify, err)
	}
	forkCfg := b.cfg
	if err := p.ApplyNativeCheckpointForkConfig(core.NativeCheckpointForkRequest{Config: &forkCfg, Record: core.NativeCheckpointForkRecord{Kind: checkpoint.Image.Kind, ImageID: checkpoint.Image.ID, Metadata: checkpoint.Metadata}}); err != nil {
		t.Fatal(err)
	}
	forkBackend := newBackend(p.Spec(), forkCfg, b.rt).(*backend)
	forkReq := req
	forkReq.RequestedLeaseID, forkReq.RequestedSlug, forkReq.RequestedCheckpointID = "cbx_abcdef123456", "fresh-lobster", "chk_lifecycle"
	fork, err := forkBackend.Acquire(context.Background(), forkReq)
	if err != nil {
		t.Fatal(err)
	}
	if fork.SSH.Key == source.SSH.Key || fork.Server.ImmutableID == source.Server.ImmutableID {
		t.Fatal("fork reused source authority")
	}
	files := fake.files[fork.Server.Name]
	for _, file := range []string{"/home/crabbox/.ssh/authorized_keys", "/etc/ssh/ssh_host_ed25519_key", "/etc/machine-id"} {
		if bytes.Equal(files[file], before[file]) {
			t.Errorf("fork retained source identity in %s", file)
		}
	}
	if !bytes.Equal(files["/home/crabbox/.ssh/authorized_keys"], files["/etc/ssh/crabbox_authorized_keys"]) || !strings.Contains(string(files["/etc/ssh/sshd_config"]), "AuthorizedKeysFile /etc/ssh/crabbox_authorized_keys\n") {
		t.Fatal("fork SSH authority is not exclusively replaced")
	}
	if _, exists := files["/etc/cloud/cloud-init.disabled"]; !exists {
		t.Fatal("inherited cloud-init can restore source credentials")
	}
	if string(files["/work/crabbox/workspace/dependency"]) != "installed dependency" {
		t.Fatal("fork lost disk data")
	}
	if _, err := forkBackend.Acquire(context.Background(), forkReq); err != nil {
		t.Fatalf("fork replay: %v", err)
	}
	if len(fake.created) != 2 {
		t.Fatal("fork replay created another instance")
	}
	if err := p.DeleteNativeCheckpoint(context.Background(), resource); err != nil {
		t.Fatal(err)
	}
	if len(fake.images) != 0 || fake.instances[fork.Server.Name] == nil {
		t.Fatal("checkpoint deletion did not preserve fork")
	}
	if err := forkBackend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: fork, Force: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := forkBackend.Acquire(context.Background(), forkReq); err == nil {
		t.Fatal("released fixed ID was resurrected")
	}
}

func TestIncusCheckpointFailuresKeepOwnershipAndCleanupIdentity(t *testing.T) {
	for _, failure := range []string{"mounted-disk", "vm", "snapshot-response", "publish-response", "snapshot-delete", "image-owner", "image-delete", "fork-identity"} {
		t.Run(failure, func(t *testing.T) {
			b, fake, req := lifecycleFixture(t)
			source, err := b.Acquire(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			sentinel := errors.New("injected " + failure)
			switch failure {
			case "mounted-disk":
				fake.instances[source.Server.Name].ExpandedDevices = api.DevicesMap{"workspace": {"type": "disk", "path": "/work"}}
			case "vm":
				fake.instances[source.Server.Name].Type = "virtual-machine"
			case "snapshot-response":
				fake.snapshotErr = sentinel
			case "publish-response":
				fake.publishErr = sentinel
			case "snapshot-delete":
				fake.deleteSnapshotErr = sentinel
			}
			p := Provider{}
			checkpoint, captureErr := p.CreateNativeCheckpoint(context.Background(), core.NativeCheckpointCreateRequest{Persist: (&checkpointRecorder{}).persist, Config: b.cfg, Server: source.Server, LeaseID: source.LeaseID, CheckpointID: "chk_failure"})
			switch failure {
			case "mounted-disk", "vm":
				if captureErr == nil || len(fake.snapshots) != 0 || len(fake.images) != 0 {
					t.Fatal("unsupported source was captured")
				}
				return
			case "snapshot-response", "publish-response":
				if captureErr == nil || checkpoint.Image.ID == "" || checkpoint.Metadata[sourceProperty] == "" {
					t.Fatalf("lost uncertain capture identity: %+v %v", checkpoint, captureErr)
				}
				resource := core.NativeCheckpointResourceRequest{Persist: (&checkpointRecorder{}).persist, Image: checkpoint.Image, Metadata: checkpoint.Metadata}
				if err := p.DeleteNativeCheckpoint(context.Background(), resource); err == nil {
					t.Fatal("uncertain capture cleanup erased identity")
				}
				return
			case "snapshot-delete":
				if captureErr == nil || checkpoint.Image.State != "available" {
					t.Fatalf("lost available image after snapshot cleanup failed: %+v %v", checkpoint, captureErr)
				}
				fake.deleteSnapshotErr = nil
			default:
				if captureErr != nil {
					t.Fatal(captureErr)
				}
			}
			resource := core.NativeCheckpointResourceRequest{Persist: (&checkpointRecorder{}).persist, Image: checkpoint.Image, Metadata: checkpoint.Metadata}
			switch failure {
			case "image-owner":
				fake.images[checkpoint.Image.ID].Properties[sourceProperty] = "another-owner"
			case "image-delete":
				fake.deleteImageErr = sentinel
			case "fork-identity":
				cfg := b.cfg
				if err := p.ApplyNativeCheckpointForkConfig(core.NativeCheckpointForkRequest{Config: &cfg, Record: core.NativeCheckpointForkRecord{Kind: checkpoint.Image.Kind, ImageID: checkpoint.Image.ID, Metadata: checkpoint.Metadata}}); err != nil {
					t.Fatal(err)
				}
				fake.writeFileErr = sentinel
				forkReq := req
				forkReq.RequestedLeaseID, forkReq.RequestedSlug, forkReq.RequestedCheckpointID = "cbx_abcdef123456", "fresh-lobster", "chk_failure"
				fb := newBackend(p.Spec(), cfg, b.rt).(*backend)
				if _, err := fb.Acquire(context.Background(), forkReq); err == nil {
					t.Fatal("fork with failed identity replacement started")
				}
				claim, err := core.ReadLeaseClaim(forkReq.RequestedLeaseID)
				if err != nil || fake.instances[claim.CloudID] == nil || fake.instances[claim.CloudID].IsActive() {
					t.Fatalf("partial fork not retained stopped: %+v %v", claim, err)
				}
				for _, cliRun := range []bool{false, true} {
					starts := len(fake.stateUpdates)
					if cliRun {
						t.Setenv("CRABBOX_PROVIDER", "incus")
						t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
						t.Chdir(req.Repo.Root)
						err = (core.App{Stdout: io.Discard, Stderr: io.Discard}).Run(context.Background(), []string{"run", "--provider", "incus", "--id", forkReq.RequestedLeaseID, "--no-sync", "--", "true"})
					} else {
						_, err = fb.Resolve(context.Background(), ResolveRequest{ID: forkReq.RequestedLeaseID})
					}
					if err == nil || !strings.Contains(err.Error(), "incomplete") || len(fake.stateUpdates) != starts {
						t.Fatalf("incomplete fork operational reuse cli=%v: %v; state updates %v", cliRun, err, fake.stateUpdates)
					}
				}
				if _, err := fb.Resolve(context.Background(), ResolveRequest{ID: forkReq.RequestedLeaseID, StatusOnly: true}); err != nil {
					t.Fatalf("inspect incomplete fork: %v", err)
				}
				if _, err := fb.Resolve(context.Background(), ResolveRequest{ID: forkReq.RequestedLeaseID, ReleaseOnly: true}); err != nil {
					t.Fatalf("stop incomplete fork: %v", err)
				}
				fake.writeFileErr = nil
				if _, err := fb.Acquire(context.Background(), forkReq); err != nil {
					t.Fatalf("reconcile partial fork: %v", err)
				}
				return
			}
			deleteErr := p.DeleteNativeCheckpoint(context.Background(), resource)
			if failure == "snapshot-delete" {
				if deleteErr != nil || len(fake.images) != 0 {
					t.Fatalf("cleanup retry: %v", deleteErr)
				}
			} else if deleteErr == nil || len(fake.images) != 1 {
				t.Fatalf("unsafe deletion after %s: %v", failure, deleteErr)
			}
		})
	}
}

func TestIncusCopiedIdentityCannotReleaseOrCleanup(t *testing.T) {
	for _, changed := range []string{"daemon", "project", "uuid", "claim"} {
		t.Run(changed, func(t *testing.T) {
			b, fake, req := lifecycleFixture(t)
			lease, err := b.Acquire(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			switch changed {
			case "daemon":
				fake.identity, _ = fake.Identity()
				fake.identity.Certificate = "replacement"
			case "project":
				fake.identity, _ = fake.Identity()
				fake.identity.Project = "replacement"
			case "uuid":
				fake.instances[lease.Server.Name].Config["volatile.uuid"] = "replacement"
			case "claim":
				err := core.WithDurableLeaseClaimLock(lease.LeaseID, func(claim *core.LeaseClaim, _ bool, persist func() error) error {
					claim.CloudID = "another-resource"
					return persist()
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease, Force: true}); err == nil {
				t.Fatal("stale claim released another resource")
			}
			labels := fake.instances[lease.Server.Name].Config
			labels[labelKey("keep")] = "false"
			labels[labelKey("expires_at")] = "1"
			_ = b.Cleanup(context.Background(), CleanupRequest{})
			if len(fake.deleted) != 0 {
				t.Fatal("cleanup deleted a conflicting resource")
			}
		})
	}
}

func (f *fakeClient) Profile(name string) (*api.Profile, error) {
	return &api.Profile{Name: name, ProfilePut: api.ProfilePut{Config: maps.Clone(f.profileConfig)}}, nil
}
func (f *fakeClient) CanonicalPath(name, path string) (string, error) { return path, nil }

func TestIncusNativeCheckpointCLIUsesProviderLifecycle(t *testing.T) {
	b, fake, req := lifecycleFixture(t)
	t.Setenv("CRABBOX_PROVIDER", "incus")
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "config.yaml"))
	t.Chdir(req.Repo.Root)
	source, err := b.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	app := core.App{Stdout: &stdout, Stderr: &stderr}
	run := func(args ...string) string {
		t.Helper()
		stdout.Reset()
		stderr.Reset()
		if err := app.Run(context.Background(), args); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, stderr.String())
		}
		return stdout.String()
	}
	output := run("checkpoint", "create", "--provider", "incus", "--id", source.LeaseID, "--mode", "native", "--json")
	var record struct{ ID, Kind string }
	if err := json.Unmarshal([]byte(output), &record); err != nil || record.ID == "" || record.Kind != core.CheckpointKindIncus {
		t.Fatalf("native record: %s %v", output, err)
	}
	run("stop", "--provider", "incus", source.LeaseID)
	output = run("checkpoint", "inspect", record.ID, "--verify", "--json")
	if !strings.Contains(output, `"providerState": "available"`) && !strings.Contains(output, `"providerState":"available"`) {
		t.Fatalf("verify: %s", output)
	}
	fake.deleteImageErr = errors.New("injected image deletion failure")
	if err := app.Run(context.Background(), []string{"checkpoint", "delete", record.ID}); err == nil {
		t.Fatal("CLI accepted failed remote deletion")
	}
	run("checkpoint", "inspect", record.ID, "--verify", "--json")
	fake.deleteImageErr = nil
	run("checkpoint", "delete", record.ID)
	if len(fake.images) != 0 {
		t.Fatal("CLI did not delete provider image")
	}
}

func TestIncusRootDiskCaptureRejectsGuestMountedWorkspace(t *testing.T) {
	b, fake, req := lifecycleFixture(t)
	source, err := b.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	fake.files[source.Server.Name]["/proc/1/mountinfo"] = []byte("1 0 0:1 / / rw - ext4 /dev/root rw\n2 1 0:2 / /work rw - tmpfs tmpfs rw\n")
	_, err = (Provider{}).CreateNativeCheckpoint(context.Background(), core.NativeCheckpointCreateRequest{Persist: (&checkpointRecorder{}).persist, Config: b.cfg, Server: source.Server, LeaseID: source.LeaseID, CheckpointID: "chk_mounted", Workdir: "/work/project"})
	if err == nil || !strings.Contains(err.Error(), "guest mount") || len(fake.snapshots) != 0 {
		t.Fatalf("mounted workspace capture: %v", err)
	}
}

func (f *fakeClient) ClearTemplates(name string) error {
	if f.instances[name].IsActive() {
		return errors.New("cannot reset running fixture templates")
	}
	return nil
}

func TestIncusInterruptedIntentAndLostDeletionReconcile(t *testing.T) {
	b, fake, req := lifecycleFixture(t)
	fake.createErr = errors.New("request never reached server")
	if _, err := b.Acquire(context.Background(), req); err == nil {
		t.Fatal("expected ambiguous create error")
	}
	before, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	fake.createErr = nil
	lease, err := b.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.Name != before.FixedCreateIntent.Attempt["name"] || lease.Server.ImmutableID != before.FixedCreateIntent.Attempt["uuid"] {
		t.Fatal("resubmitted intent changed resource identity")
	}
	fake.profileConfig = map[string]string{"limits.cpu": "2"}
	if _, err := b.Acquire(context.Background(), req); err == nil {
		t.Fatal("changed profile contents accepted")
	}
	fake.profileConfig = nil
	// Simulate a committed delete whose response did not reach the client.
	if err := fake.DeleteInstance(lease.Server.Name); err != nil {
		t.Fatal(err)
	}
	target, err := b.Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatalf("reconcile lost delete: %v", err)
	}
	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: target, Force: true}); err != nil {
		t.Fatal(err)
	}
	after, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil || after.FixedCreateIntent.State != "released" {
		t.Fatalf("missing terminal claim: %+v %v", after, err)
	}
}

func TestIncusLostPublishResponseFindsIndependentImage(t *testing.T) {
	b, fake, req := lifecycleFixture(t)
	lease, err := b.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	fake.lostPublishReply = errors.New("publish reply lost")
	p := Provider{}
	checkpoint, err := p.CreateNativeCheckpoint(context.Background(), core.NativeCheckpointCreateRequest{Persist: (&checkpointRecorder{}).persist, Config: b.cfg, Server: lease.Server, LeaseID: lease.LeaseID, CheckpointID: "chk_lost_publish"})
	if err == nil || !strings.HasPrefix(checkpoint.Image.ID, "pending:") {
		t.Fatalf("lost publish not recorded: %+v %v", checkpoint, err)
	}
	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease, Force: true}); err != nil {
		t.Fatal(err)
	}
	resource := core.NativeCheckpointResourceRequest{Persist: (&checkpointRecorder{}).persist, Image: checkpoint.Image, Metadata: checkpoint.Metadata}
	verified, err := p.VerifyNativeCheckpoint(context.Background(), resource)
	if err != nil || verified.ProviderState != "available" {
		t.Fatalf("lost publish reconciliation: %+v %v", verified, err)
	}
	if err := p.DeleteNativeCheckpoint(context.Background(), resource); err != nil {
		t.Fatal(err)
	}
	if len(fake.images) != 0 {
		t.Fatal("independent image leaked after lost response")
	}
}

type checkpointRecorder struct {
	result core.NativeCheckpointCreateResult
}

func (r *checkpointRecorder) persist(result core.NativeCheckpointCreateResult) error {
	r.result = result
	r.result.Metadata = maps.Clone(result.Metadata)
	return nil
}
