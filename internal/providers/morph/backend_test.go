package morph

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type fakeMorphAPI struct {
	bootSnapshot        func(context.Context, string, morphBootSnapshotRequest) (morphInstance, error)
	getSnapshot         func(context.Context, string) (morphSnapshot, error)
	getInstance         func(context.Context, string) (morphInstance, error)
	listInstances       func(context.Context, map[string]string) ([]morphInstance, error)
	getSSHKey           func(context.Context, string) (morphSSHKey, error)
	setInstanceMetadata func(context.Context, string, map[string]string) error
	updateInstanceTTL   func(context.Context, string, int, string) error
	updateInstanceWake  func(context.Context, string, *bool, *bool) error
	pauseInstance       func(context.Context, string) error
	resumeInstance      func(context.Context, string) error
	deleteInstance      func(context.Context, string) error
}

func (f *fakeMorphAPI) BootSnapshot(ctx context.Context, snapshotID string, req morphBootSnapshotRequest) (morphInstance, error) {
	if f.bootSnapshot == nil {
		return morphInstance{}, errors.New("unexpected BootSnapshot")
	}
	return f.bootSnapshot(ctx, snapshotID, req)
}

func (f *fakeMorphAPI) GetSnapshot(ctx context.Context, snapshotID string) (morphSnapshot, error) {
	if f.getSnapshot == nil {
		return morphSnapshot{}, errors.New("unexpected GetSnapshot")
	}
	return f.getSnapshot(ctx, snapshotID)
}

func (f *fakeMorphAPI) GetInstance(ctx context.Context, instanceID string) (morphInstance, error) {
	if f.getInstance == nil {
		return morphInstance{}, errors.New("unexpected GetInstance")
	}
	return f.getInstance(ctx, instanceID)
}

func (f *fakeMorphAPI) ListInstances(ctx context.Context, metadata map[string]string) ([]morphInstance, error) {
	if f.listInstances == nil {
		return nil, errors.New("unexpected ListInstances")
	}
	return f.listInstances(ctx, metadata)
}

func (f *fakeMorphAPI) GetSSHKey(ctx context.Context, instanceID string) (morphSSHKey, error) {
	if f.getSSHKey == nil {
		return morphSSHKey{}, errors.New("unexpected GetSSHKey")
	}
	return f.getSSHKey(ctx, instanceID)
}

func (f *fakeMorphAPI) SetInstanceMetadata(ctx context.Context, instanceID string, metadata map[string]string) error {
	if f.setInstanceMetadata == nil {
		return errors.New("unexpected SetInstanceMetadata")
	}
	return f.setInstanceMetadata(ctx, instanceID, metadata)
}

func (f *fakeMorphAPI) UpdateInstanceTTL(ctx context.Context, instanceID string, ttlSeconds int, ttlAction string) error {
	if f.updateInstanceTTL == nil {
		return errors.New("unexpected UpdateInstanceTTL")
	}
	return f.updateInstanceTTL(ctx, instanceID, ttlSeconds, ttlAction)
}

func (f *fakeMorphAPI) UpdateInstanceWakeOn(ctx context.Context, instanceID string, wakeOnSSH, wakeOnHTTP *bool) error {
	if f.updateInstanceWake == nil {
		return errors.New("unexpected UpdateInstanceWakeOn")
	}
	return f.updateInstanceWake(ctx, instanceID, wakeOnSSH, wakeOnHTTP)
}

func (f *fakeMorphAPI) PauseInstance(ctx context.Context, instanceID string) error {
	if f.pauseInstance == nil {
		return errors.New("unexpected PauseInstance")
	}
	return f.pauseInstance(ctx, instanceID)
}

func (f *fakeMorphAPI) ResumeInstance(ctx context.Context, instanceID string) error {
	if f.resumeInstance == nil {
		return errors.New("unexpected ResumeInstance")
	}
	return f.resumeInstance(ctx, instanceID)
}

func (f *fakeMorphAPI) DeleteInstance(ctx context.Context, instanceID string) error {
	if f.deleteInstance == nil {
		return errors.New("unexpected DeleteInstance")
	}
	return f.deleteInstance(ctx, instanceID)
}

func TestMorphAcquireStoresMetadataAndKey(t *testing.T) {
	configureMorphTestHome(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	cfg := testMorphConfig()

	var gotMetadata map[string]string
	var gotTTL int
	var gotTTLAction string
	var gotWakeOnSSH *bool
	waitCalled := false
	originalWait := waitForMorphSSHReady
	waitForMorphSSHReady = func(_ context.Context, target *SSHTarget, _ io.Writer, _ string, _ time.Duration) error {
		waitCalled = true
		if target.Host != "ssh.cloud.morph.so" || target.User != "inst_1" {
			t.Fatalf("unexpected target: %#v", target)
		}
		return nil
	}
	defer func() { waitForMorphSSHReady = originalWait }()

	fake := &fakeMorphAPI{
		getSnapshot: func(_ context.Context, snapshotID string) (morphSnapshot, error) {
			if snapshotID != "snapshot_123" {
				t.Fatalf("snapshotID=%q", snapshotID)
			}
			return morphSnapshot{ID: snapshotID}, nil
		},
		listInstances: func(_ context.Context, metadata map[string]string) ([]morphInstance, error) {
			if metadata["crabbox"] != "true" || metadata["provider"] != providerName {
				t.Fatalf("unexpected list filter: %#v", metadata)
			}
			return nil, nil
		},
		bootSnapshot: func(_ context.Context, snapshotID string, _ morphBootSnapshotRequest) (morphInstance, error) {
			return morphInstance{ID: "inst_1", Status: "starting", Refs: morphInstanceRefs{SnapshotID: snapshotID}}, nil
		},
		setInstanceMetadata: func(_ context.Context, instanceID string, metadata map[string]string) error {
			if instanceID != "inst_1" {
				t.Fatalf("instanceID=%q", instanceID)
			}
			gotMetadata = metadata
			return nil
		},
		updateInstanceTTL: func(_ context.Context, instanceID string, ttlSeconds int, ttlAction string) error {
			gotTTL = ttlSeconds
			gotTTLAction = ttlAction
			return nil
		},
		updateInstanceWake: func(_ context.Context, instanceID string, wakeOnSSH, wakeOnHTTP *bool) error {
			gotWakeOnSSH = wakeOnSSH
			if wakeOnHTTP != nil {
				t.Fatalf("wakeOnHTTP should be omitted")
			}
			return nil
		},
		getInstance: func(_ context.Context, instanceID string) (morphInstance, error) {
			return morphInstance{
				ID:       instanceID,
				Status:   "ready",
				Metadata: morphMetadata(gotMetadata),
				Refs:     morphInstanceRefs{SnapshotID: "snapshot_123"},
			}, nil
		},
		getSSHKey: func(_ context.Context, instanceID string) (morphSSHKey, error) {
			return morphSSHKey{PrivateKey: "PRIVATE KEY"}, nil
		},
	}

	backend := &morphLeaseBackend{
		spec:              Provider{}.Spec(),
		cfg:               cfg,
		rt:                Runtime{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}},
		client:            fake,
		now:               func() time.Time { return now },
		readyPollInterval: time.Millisecond,
		readyTimeout:      time.Second,
	}

	lease, err := backend.Acquire(context.Background(), AcquireRequest{RequestedSlug: "blue-lobster"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID == "" || lease.Server.CloudID != "inst_1" || lease.Server.Labels["slug"] != "blue-lobster" {
		t.Fatalf("unexpected lease: %#v", lease)
	}
	if gotMetadata["lease"] != lease.LeaseID || gotMetadata["provider"] != providerName || gotMetadata["instance_id"] != "inst_1" || gotMetadata["work_root"] != "/tmp/crabbox" || gotMetadata["snapshot_id"] != "snapshot_123" {
		t.Fatalf("unexpected metadata: %#v", gotMetadata)
	}
	if gotTTL != int((15*time.Minute).Seconds()) || gotTTLAction != "pause" {
		t.Fatalf("unexpected ttl update: ttl=%d action=%s", gotTTL, gotTTLAction)
	}
	if gotWakeOnSSH == nil || !*gotWakeOnSSH {
		t.Fatalf("wake-on-ssh not enabled: %#v", gotWakeOnSSH)
	}
	if !waitCalled {
		t.Fatal("waitForSSHReady was not called")
	}
	keyData, err := os.ReadFile(lease.SSH.Key)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(keyData)) != "PRIVATE KEY" {
		t.Fatalf("unexpected stored key: %q", string(keyData))
	}
	info, err := os.Stat(lease.SSH.Key)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key perms=%o want 600", info.Mode().Perm())
	}
}

func TestNewMorphBackendAllowsMissingSnapshotForExistingLeaseOps(t *testing.T) {
	cfg := testMorphConfig()
	cfg.Morph.Snapshot = ""
	cfg.ServerType = ""

	backend, err := NewMorphBackend(Provider{}.Spec(), cfg, Runtime{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
	if err != nil {
		t.Fatal(err)
	}
	if backend.Spec().Name != providerName {
		t.Fatalf("unexpected backend spec: %#v", backend.Spec())
	}
}

func TestConfigureDoctorReturnsMorphDoctorBackend(t *testing.T) {
	doctor, err := Provider{}.ConfigureDoctor(testMorphConfig(), Runtime{Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatalf("ConfigureDoctor: %v", err)
	}
	if doctor.Spec().Name != providerName {
		t.Fatalf("doctor.Spec().Name=%q want %q", doctor.Spec().Name, providerName)
	}
	if _, ok := doctor.(*morphLeaseBackend); !ok {
		t.Fatalf("doctor backend type=%T", doctor)
	}
}

func TestMorphAcquireRequiresSnapshot(t *testing.T) {
	cfg := testMorphConfig()
	cfg.Morph.Snapshot = ""

	backend := &morphLeaseBackend{
		spec:   Provider{}.Spec(),
		cfg:    cfg,
		rt:     Runtime{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}},
		client: &fakeMorphAPI{},
		now:    time.Now,
	}

	_, err := backend.Acquire(context.Background(), AcquireRequest{})
	if err == nil || !strings.Contains(err.Error(), "CRABBOX_MORPH_SNAPSHOT") {
		t.Fatalf("Acquire error=%v", err)
	}
}

func TestMorphResolveResumesPausedInstanceWithoutWakeOnSSH(t *testing.T) {
	configureMorphTestHome(t)
	cfg := testMorphConfig()
	cfg.Morph.WakeOnSSH = false

	resumeCalls := 0
	getInstanceCalls := 0
	waitCalls := 0
	originalWait := waitForMorphSSHReady
	waitForMorphSSHReady = func(_ context.Context, _ *SSHTarget, _ io.Writer, _ string, _ time.Duration) error {
		waitCalls++
		return nil
	}
	defer func() { waitForMorphSSHReady = originalWait }()

	fake := &fakeMorphAPI{
		getInstance: func(_ context.Context, instanceID string) (morphInstance, error) {
			getInstanceCalls++
			if getInstanceCalls == 1 {
				return morphInstance{ID: instanceID, Status: "paused"}, nil
			}
			return morphInstance{ID: instanceID, Status: "ready"}, nil
		},
		resumeInstance: func(_ context.Context, instanceID string) error {
			resumeCalls++
			return nil
		},
		getSSHKey: func(_ context.Context, instanceID string) (morphSSHKey, error) {
			return morphSSHKey{PrivateKey: "PRIVATE KEY"}, nil
		},
	}

	backend := &morphLeaseBackend{
		spec:              Provider{}.Spec(),
		cfg:               cfg,
		rt:                Runtime{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}},
		client:            fake,
		now:               time.Now,
		readyPollInterval: time.Millisecond,
		readyTimeout:      time.Second,
	}

	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "inst_2"})
	if err != nil {
		t.Fatal(err)
	}
	if resumeCalls != 1 || waitCalls != 1 {
		t.Fatalf("resumeCalls=%d waitCalls=%d", resumeCalls, waitCalls)
	}
	if lease.Server.Status != "ready" || lease.SSH.User != "inst_2" {
		t.Fatalf("unexpected resolved lease: %#v", lease)
	}
}

func TestMorphResolveStatusOnlyAndReleaseOnlySkipSSHPreparation(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  ResolveRequest
	}{
		{name: "status-only", req: ResolveRequest{ID: "inst_2", StatusOnly: true}},
		{name: "release-only", req: ResolveRequest{ID: "inst_2", ReleaseOnly: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configureMorphTestHome(t)
			cfg := testMorphConfig()
			cfg.Morph.WakeOnSSH = false

			resumeCalls := 0
			sshKeyCalls := 0
			fake := &fakeMorphAPI{
				getInstance: func(_ context.Context, instanceID string) (morphInstance, error) {
					return morphInstance{ID: instanceID, Status: "paused"}, nil
				},
				resumeInstance: func(_ context.Context, instanceID string) error {
					resumeCalls++
					return nil
				},
				getSSHKey: func(_ context.Context, instanceID string) (morphSSHKey, error) {
					sshKeyCalls++
					return morphSSHKey{PrivateKey: "PRIVATE KEY"}, nil
				},
			}

			backend := &morphLeaseBackend{
				spec:              Provider{}.Spec(),
				cfg:               cfg,
				rt:                Runtime{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}},
				client:            fake,
				now:               time.Now,
				readyPollInterval: time.Millisecond,
				readyTimeout:      time.Second,
			}

			lease, err := backend.Resolve(context.Background(), tc.req)
			if err != nil {
				t.Fatal(err)
			}
			if resumeCalls != 0 || sshKeyCalls != 0 {
				t.Fatalf("resumeCalls=%d sshKeyCalls=%d", resumeCalls, sshKeyCalls)
			}
			if lease.SSH.Host != "" || lease.SSH.User != "" {
				t.Fatalf("expected empty ssh target, got %#v", lease.SSH)
			}
			if lease.Server.Status != "paused" {
				t.Fatalf("unexpected server state: %#v", lease.Server)
			}
		})
	}
}

func TestMorphReleasePausesOrDeletesAndCleansKey(t *testing.T) {
	for _, tc := range []struct {
		name            string
		deleteOnRelease bool
		wantPauseCalls  int
		wantDeleteCalls int
	}{
		{name: "pause", deleteOnRelease: false, wantPauseCalls: 1},
		{name: "delete", deleteOnRelease: true, wantDeleteCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configureMorphTestHome(t)
			cfg := testMorphConfig()
			cfg.Morph.DeleteOnRelease = tc.deleteOnRelease
			keyPath, err := testboxKeyPath("cbx_release")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(keyPath, []byte("PRIVATE KEY"), 0o600); err != nil {
				t.Fatal(err)
			}

			pauseCalls := 0
			deleteCalls := 0
			fake := &fakeMorphAPI{
				getInstance: func(_ context.Context, instanceID string) (morphInstance, error) {
					return morphInstance{ID: instanceID, Status: "ready"}, nil
				},
				pauseInstance: func(_ context.Context, instanceID string) error {
					pauseCalls++
					return nil
				},
				deleteInstance: func(_ context.Context, instanceID string) error {
					deleteCalls++
					return nil
				},
			}

			backend := &morphLeaseBackend{
				spec:   Provider{}.Spec(),
				cfg:    cfg,
				rt:     Runtime{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}},
				client: fake,
				now:    time.Now,
			}
			err = backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{
				Lease: LeaseTarget{
					LeaseID: "cbx_release",
					Server:  Server{CloudID: "inst_release"},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if pauseCalls != tc.wantPauseCalls || deleteCalls != tc.wantDeleteCalls {
				t.Fatalf("pauseCalls=%d deleteCalls=%d", pauseCalls, deleteCalls)
			}
			if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("key file still exists: err=%v", err)
			}
		})
	}
}

func TestMorphTouchInitializesMissingMetadata(t *testing.T) {
	configureMorphTestHome(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	cfg := testMorphConfig()

	var gotMetadata map[string]string
	fake := &fakeMorphAPI{
		getInstance: func(_ context.Context, instanceID string) (morphInstance, error) {
			return morphInstance{
				ID:     instanceID,
				Status: "ready",
				Refs:   morphInstanceRefs{SnapshotID: "snapshot_123"},
			}, nil
		},
		setInstanceMetadata: func(_ context.Context, instanceID string, metadata map[string]string) error {
			gotMetadata = metadata
			return nil
		},
		updateInstanceTTL: func(_ context.Context, instanceID string, ttlSeconds int, ttlAction string) error {
			return nil
		},
		updateInstanceWake: func(_ context.Context, instanceID string, wakeOnSSH, wakeOnHTTP *bool) error {
			return nil
		},
	}

	backend := &morphLeaseBackend{
		spec:   Provider{}.Spec(),
		cfg:    cfg,
		rt:     Runtime{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}},
		client: fake,
		now:    func() time.Time { return now },
	}
	_, err := backend.Touch(context.Background(), TouchRequest{
		Lease: LeaseTarget{
			LeaseID: "cbx_nil",
			Server: Server{
				CloudID: "inst_nil",
				Labels:  map[string]string{"slug": "blue-lobster"},
			},
		},
		State:       "running",
		IdleTimeout: 30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotMetadata["lease"] != "cbx_nil" || gotMetadata["work_root"] != "/tmp/crabbox" || gotMetadata["idle_timeout_secs"] != "1800" {
		t.Fatalf("unexpected metadata: %#v", gotMetadata)
	}
}

func TestMorphTouchPreservesCustomWorkRootAndRefreshesTTL(t *testing.T) {
	configureMorphTestHome(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	cfg := testMorphConfig()
	cfg.Morph.WakeOnSSH = false
	instance := morphInstance{
		ID:     "inst_touch",
		Status: "ready",
		Refs:   morphInstanceRefs{SnapshotID: "snapshot_123"},
	}
	instance.Metadata = morphMetadata{
		"lease":      "cbx_touch",
		"slug":       "blue-lobster",
		"work_root":  "/workspace/custom",
		"provider":   providerName,
		"created_at": strconv.FormatInt(now.Unix(), 10),
	}

	var gotMetadata map[string]string
	var gotTTL int
	var gotWakeOnSSH *bool
	fake := &fakeMorphAPI{
		getInstance: func(_ context.Context, instanceID string) (morphInstance, error) {
			return instance, nil
		},
		setInstanceMetadata: func(_ context.Context, instanceID string, metadata map[string]string) error {
			gotMetadata = metadata
			return nil
		},
		updateInstanceTTL: func(_ context.Context, instanceID string, ttlSeconds int, ttlAction string) error {
			gotTTL = ttlSeconds
			if ttlAction != "pause" {
				t.Fatalf("ttlAction=%s", ttlAction)
			}
			return nil
		},
		updateInstanceWake: func(_ context.Context, instanceID string, wakeOnSSH, wakeOnHTTP *bool) error {
			gotWakeOnSSH = wakeOnSSH
			return nil
		},
	}

	backend := &morphLeaseBackend{
		spec:   Provider{}.Spec(),
		cfg:    cfg,
		rt:     Runtime{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}},
		client: fake,
		now:    func() time.Time { return now },
	}
	server, err := backend.Touch(context.Background(), TouchRequest{
		Lease: LeaseTarget{
			LeaseID: "cbx_touch",
			Server:  morphServer(instance, cfg, "cbx_touch", "blue-lobster"),
		},
		State:       "running",
		IdleTimeout: 30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotMetadata["work_root"] != "/workspace/custom" || gotMetadata["state"] != "running" || gotMetadata["instance_id"] != "inst_touch" {
		t.Fatalf("unexpected touched metadata: %#v", gotMetadata)
	}
	if gotTTL != int((30 * time.Minute).Seconds()) {
		t.Fatalf("ttl=%d want %d", gotTTL, int((30 * time.Minute).Seconds()))
	}
	if gotWakeOnSSH == nil || *gotWakeOnSSH {
		t.Fatalf("unexpected wakeOnSSH value: %#v", gotWakeOnSSH)
	}
	if server.Labels["work_root"] != "/workspace/custom" {
		t.Fatalf("server labels lost work_root: %#v", server.Labels)
	}
}

func testMorphConfig() Config {
	return Config{
		Provider:    providerName,
		TargetOS:    targetLinux,
		TTL:         2 * time.Hour,
		IdleTimeout: 15 * time.Minute,
		SSHPort:     "22",
		WorkRoot:    "/tmp/crabbox",
		Morph: MorphConfig{
			APIKey:         "token",
			APIURL:         "https://cloud.morph.so",
			Snapshot:       "snapshot_123",
			SSHGatewayHost: "ssh.cloud.morph.so",
			WorkRoot:       "/tmp/crabbox",
			WakeOnSSH:      true,
		},
	}
}

func configureMorphTestHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
}
