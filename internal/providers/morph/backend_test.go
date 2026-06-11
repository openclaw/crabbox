package morph

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
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
	var gotBootRequest morphBootSnapshotRequest
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
		configDir, err := os.UserConfigDir()
		if err != nil {
			t.Fatal(err)
		}
		wantKnownHosts := filepath.Join(configDir, "crabbox", providerName, "known_hosts")
		if target.KnownHostsFile != wantKnownHosts {
			t.Fatalf("knownHostsFile=%q want %q", target.KnownHostsFile, wantKnownHosts)
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
		bootSnapshot: func(_ context.Context, snapshotID string, req morphBootSnapshotRequest) (morphInstance, error) {
			gotBootRequest = req
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

	nowCalls := 0
	backend := &morphLeaseBackend{
		spec:   Provider{}.Spec(),
		cfg:    cfg,
		rt:     Runtime{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}},
		client: fake,
		now: func() time.Time {
			nowCalls++
			if nowCalls == 1 {
				return now
			}
			return now.Add(time.Minute)
		},
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
	if gotBootRequest.Metadata["lease"] != lease.LeaseID || gotBootRequest.Metadata["provider"] != providerName || gotBootRequest.Metadata["snapshot_id"] != "snapshot_123" {
		t.Fatalf("unexpected boot metadata: %#v", gotBootRequest.Metadata)
	}
	if gotBootRequest.TTLSeconds == nil || *gotBootRequest.TTLSeconds != int((15*time.Minute).Seconds()) || gotBootRequest.TTLAction != "pause" {
		t.Fatalf("unexpected boot ttl: %#v", gotBootRequest)
	}
	if gotTTL != int((14*time.Minute).Seconds()) || gotTTLAction != "pause" {
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
	knownHostsDir := filepath.Dir(lease.SSH.KnownHostsFile)
	info, err = os.Stat(knownHostsDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("known_hosts dir perms=%o want 700", info.Mode().Perm())
	}
}

func TestStoreMorphSSHKeyDecryptsProtectedKey(t *testing.T) {
	configureMorphTestHome(t)
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const password = "morph-test-password"
	block, err := ssh.MarshalPrivateKeyWithPassphrase(privateKey, "", []byte(password))
	if err != nil {
		t.Fatal(err)
	}

	keyPath, err := storeMorphSSHKey("cbx_protected", morphSSHKey{
		PrivateKey: string(pem.EncodeToMemory(block)),
		Password:   password,
	})
	if err != nil {
		t.Fatal(err)
	}
	keyData, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ssh.ParseRawPrivateKey(keyData); err != nil {
		t.Fatalf("stored key is not usable without interaction: %v", err)
	}
	if bytes.Contains(keyData, []byte(password)) {
		t.Fatal("stored key contains Morph password")
	}
}

func TestStoreMorphSSHKeyRejectsWrongPassword(t *testing.T) {
	configureMorphTestHome(t)
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(privateKey, "", []byte("correct-password"))
	if err != nil {
		t.Fatal(err)
	}

	_, err = storeMorphSSHKey("cbx_wrong_password", morphSSHKey{
		PrivateKey: string(pem.EncodeToMemory(block)),
		Password:   "wrong-password",
	})
	if err == nil || !strings.Contains(err.Error(), "could not be decrypted") {
		t.Fatalf("storeMorphSSHKey error=%v", err)
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

func TestNewMorphBackendRejectsTailscaleNetwork(t *testing.T) {
	cfg := testMorphConfig()
	cfg.Network = networkTailscale

	_, err := NewMorphBackend(Provider{}.Spec(), cfg, Runtime{Stdout: io.Discard, Stderr: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "--network=tailscale is not supported") {
		t.Fatalf("NewMorphBackend error=%v", err)
	}
}

func TestApplyMorphProviderFlagsUpdatesServerType(t *testing.T) {
	cfg := testMorphConfig()
	cfg.Morph.Snapshot = "snapshot_old"
	cfg.ServerType = "snapshot_old"
	fs := flag.NewFlagSet("morph", flag.ContinueOnError)
	values := RegisterMorphProviderFlags(fs, cfg)
	if err := fs.Parse([]string{"--morph-snapshot", "snapshot_new"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyMorphProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Morph.Snapshot != "snapshot_new" || cfg.ServerType != "snapshot_new" {
		t.Fatalf("snapshot=%q serverType=%q", cfg.Morph.Snapshot, cfg.ServerType)
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

func TestMorphAcquireRollbackIsBounded(t *testing.T) {
	configureMorphTestHome(t)
	cfg := testMorphConfig()
	var stderr bytes.Buffer
	deleteFinished := make(chan struct{})
	fake := &fakeMorphAPI{
		getSnapshot: func(_ context.Context, snapshotID string) (morphSnapshot, error) {
			return morphSnapshot{ID: snapshotID}, nil
		},
		listInstances: func(_ context.Context, _ map[string]string) ([]morphInstance, error) {
			return nil, nil
		},
		bootSnapshot: func(_ context.Context, _ string, _ morphBootSnapshotRequest) (morphInstance, error) {
			return morphInstance{ID: "inst_rollback", Status: "starting"}, nil
		},
		setInstanceMetadata: func(_ context.Context, _ string, _ map[string]string) error {
			return errors.New("metadata failed")
		},
		deleteInstance: func(ctx context.Context, _ string) error {
			defer close(deleteFinished)
			<-ctx.Done()
			return ctx.Err()
		},
	}
	backend := &morphLeaseBackend{
		spec:            Provider{}.Spec(),
		cfg:             cfg,
		rt:              Runtime{Stdout: io.Discard, Stderr: &stderr},
		client:          fake,
		now:             time.Now,
		rollbackTimeout: 20 * time.Millisecond,
	}

	started := time.Now()
	_, err := backend.Acquire(context.Background(), AcquireRequest{})
	if err == nil || !strings.Contains(err.Error(), "metadata failed") {
		t.Fatalf("Acquire error=%v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Acquire rollback took %s, want bounded timeout", elapsed)
	}
	select {
	case <-deleteFinished:
	default:
		t.Fatal("rollback delete did not finish")
	}
	if !strings.Contains(stderr.String(), "context deadline exceeded") {
		t.Fatalf("stderr=%q, want rollback timeout warning", stderr.String())
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
				return morphInstance{
					ID:       instanceID,
					Status:   "paused",
					Metadata: morphMetadata{"crabbox": "true", "provider": providerName},
				}, nil
			}
			return morphInstance{
				ID:       instanceID,
				Status:   "ready",
				Metadata: morphMetadata{"crabbox": "true", "provider": providerName},
			}, nil
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

func TestMorphResolveEnablesProviderWakeOnSSHBeforeRelyingOnIt(t *testing.T) {
	configureMorphTestHome(t)
	cfg := testMorphConfig()
	cfg.Morph.WakeOnSSH = true

	getInstanceCalls := 0
	wakeOnCalls := 0
	wakeEnabled := false
	originalWait := waitForMorphSSHReady
	waitForMorphSSHReady = func(_ context.Context, _ *SSHTarget, _ io.Writer, _ string, _ time.Duration) error {
		if !wakeEnabled {
			t.Fatal("SSH readiness checked before wake-on-SSH was enabled")
		}
		return nil
	}
	defer func() { waitForMorphSSHReady = originalWait }()

	fake := &fakeMorphAPI{
		getInstance: func(_ context.Context, instanceID string) (morphInstance, error) {
			getInstanceCalls++
			status := "paused"
			if getInstanceCalls > 1 {
				status = "ready"
			}
			return morphInstance{
				ID:       instanceID,
				Status:   status,
				Metadata: morphMetadata{"crabbox": "true", "provider": providerName},
				WakeOn:   morphWakeOnSettings{WakeOnSSH: wakeEnabled},
			}, nil
		},
		updateInstanceWake: func(_ context.Context, _ string, wakeOnSSH, wakeOnHTTP *bool) error {
			wakeOnCalls++
			if wakeOnSSH == nil || !*wakeOnSSH || wakeOnHTTP != nil {
				t.Fatalf("unexpected wake-on update: ssh=%v http=%v", wakeOnSSH, wakeOnHTTP)
			}
			wakeEnabled = true
			return nil
		},
		getSSHKey: func(_ context.Context, _ string) (morphSSHKey, error) {
			return morphSSHKey{PrivateKey: "PRIVATE KEY"}, nil
		},
	}
	backend := &morphLeaseBackend{
		spec:              Provider{}.Spec(),
		cfg:               cfg,
		rt:                Runtime{Stdout: io.Discard, Stderr: io.Discard},
		client:            fake,
		now:               time.Now,
		readyPollInterval: time.Millisecond,
		readyTimeout:      time.Second,
	}

	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "inst_wake"})
	if err != nil {
		t.Fatal(err)
	}
	if wakeOnCalls != 1 || lease.Server.Status != "ready" {
		t.Fatalf("wakeOnCalls=%d lease=%#v", wakeOnCalls, lease)
	}
}

func TestMorphResolveResumesAfterSavingTransitionsToPaused(t *testing.T) {
	configureMorphTestHome(t)
	cfg := testMorphConfig()
	cfg.Morph.WakeOnSSH = false

	resumeCalls := 0
	getInstanceCalls := 0
	originalWait := waitForMorphSSHReady
	waitForMorphSSHReady = func(_ context.Context, _ *SSHTarget, _ io.Writer, _ string, _ time.Duration) error {
		return nil
	}
	defer func() { waitForMorphSSHReady = originalWait }()

	fake := &fakeMorphAPI{
		getInstance: func(_ context.Context, instanceID string) (morphInstance, error) {
			getInstanceCalls++
			status := "ready"
			switch getInstanceCalls {
			case 1:
				status = "saving"
			case 2:
				status = "paused"
			}
			return morphInstance{
				ID:       instanceID,
				Status:   status,
				Metadata: morphMetadata{"crabbox": "true", "provider": providerName},
			}, nil
		},
		resumeInstance: func(_ context.Context, _ string) error {
			resumeCalls++
			return nil
		},
		getSSHKey: func(_ context.Context, _ string) (morphSSHKey, error) {
			return morphSSHKey{PrivateKey: "PRIVATE KEY"}, nil
		},
	}
	backend := &morphLeaseBackend{
		spec:              Provider{}.Spec(),
		cfg:               cfg,
		rt:                Runtime{Stdout: io.Discard, Stderr: io.Discard},
		client:            fake,
		now:               time.Now,
		readyPollInterval: time.Millisecond,
		readyTimeout:      time.Second,
	}

	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "inst_saving"})
	if err != nil {
		t.Fatal(err)
	}
	if resumeCalls != 1 || lease.Server.Status != "ready" {
		t.Fatalf("resumeCalls=%d lease=%#v", resumeCalls, lease)
	}
}

func TestMorphReadyCheckIncludesSyncPrerequisites(t *testing.T) {
	for _, prerequisite := range []string{
		"command -v bash",
		"command -v git",
		"command -v rsync",
		"command -v tar",
		"command -v python3",
		"command -v python",
		"command -v perl",
	} {
		if !strings.Contains(morphReadyCheck, prerequisite) {
			t.Fatalf("morphReadyCheck missing %q: %s", prerequisite, morphReadyCheck)
		}
	}
}

func TestMorphServerReportsGatewayAndProviderNetworking(t *testing.T) {
	cfg := testMorphConfig()
	cfg.Morph.SSHGatewayHost = "gateway.morph.test"
	server := morphServer(morphInstance{
		ID:     "inst_network",
		Status: "ready",
		Networking: morphNetworking{
			Hostname:   "instance.morph.internal",
			ExternalIP: "203.0.113.10",
			InternalIP: "10.0.0.10",
		},
	}, cfg, "cbx_network", "network-test")

	if server.PublicNet.IPv4.IP != "gateway.morph.test" {
		t.Fatalf("server host=%q", server.PublicNet.IPv4.IP)
	}
	for key, want := range map[string]string{
		"morph_hostname":    "instance.morph.internal",
		"morph_external_ip": "203.0.113.10",
		"morph_internal_ip": "10.0.0.10",
	} {
		if server.Labels[key] != want {
			t.Fatalf("label %s=%q want %q", key, server.Labels[key], want)
		}
	}
}

func TestMorphResolveRejectsUnsafeMetadataLeaseID(t *testing.T) {
	home := t.TempDir()
	configDir := filepath.Join(home, ".config")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	fake := &fakeMorphAPI{
		getInstance: func(_ context.Context, instanceID string) (morphInstance, error) {
			return morphInstance{
				ID:       instanceID,
				Status:   "ready",
				Metadata: morphMetadata{"lease": "../escape", "provider": providerName, "crabbox": "true"},
			}, nil
		},
		getSSHKey: func(_ context.Context, _ string) (morphSSHKey, error) {
			return morphSSHKey{PrivateKey: "PRIVATE KEY"}, nil
		},
	}
	backend := &morphLeaseBackend{
		spec:   Provider{}.Spec(),
		cfg:    testMorphConfig(),
		rt:     Runtime{Stdout: io.Discard, Stderr: io.Discard},
		client: fake,
		now:    time.Now,
	}

	_, err := backend.Resolve(context.Background(), ResolveRequest{ID: "inst_unsafe"})
	if err == nil || !strings.Contains(err.Error(), "invalid lease claim id") {
		t.Fatalf("Resolve error=%v", err)
	}
	escapedPath := filepath.Join(configDir, "crabbox", "escape", "id_ed25519")
	if _, err := os.Stat(escapedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe key path exists: %s", escapedPath)
	}
}

func TestMorphResolveRejectsUnmanagedInstance(t *testing.T) {
	sshKeyCalls := 0
	fake := &fakeMorphAPI{
		getInstance: func(_ context.Context, instanceID string) (morphInstance, error) {
			return morphInstance{ID: instanceID, Status: "ready"}, nil
		},
		getSSHKey: func(_ context.Context, _ string) (morphSSHKey, error) {
			sshKeyCalls++
			return morphSSHKey{PrivateKey: "PRIVATE KEY"}, nil
		},
	}
	backend := &morphLeaseBackend{
		spec:   Provider{}.Spec(),
		cfg:    testMorphConfig(),
		rt:     Runtime{Stdout: io.Discard, Stderr: io.Discard},
		client: fake,
		now:    time.Now,
	}

	_, err := backend.Resolve(context.Background(), ResolveRequest{ID: "inst_unmanaged"})
	if err == nil || !strings.Contains(err.Error(), "not managed by Crabbox") {
		t.Fatalf("Resolve error=%v", err)
	}
	if sshKeyCalls != 0 {
		t.Fatalf("ssh key calls=%d, want 0", sshKeyCalls)
	}
}

func TestMorphReleaseRejectsUnmanagedInstance(t *testing.T) {
	deleteCalls := 0
	pauseCalls := 0
	fake := &fakeMorphAPI{
		getInstance: func(_ context.Context, instanceID string) (morphInstance, error) {
			return morphInstance{ID: instanceID, Status: "ready"}, nil
		},
		deleteInstance: func(_ context.Context, _ string) error {
			deleteCalls++
			return nil
		},
		pauseInstance: func(_ context.Context, _ string) error {
			pauseCalls++
			return nil
		},
	}
	cfg := testMorphConfig()
	cfg.Morph.DeleteOnRelease = true
	backend := &morphLeaseBackend{
		spec:   Provider{}.Spec(),
		cfg:    cfg,
		rt:     Runtime{Stdout: io.Discard, Stderr: io.Discard},
		client: fake,
		now:    time.Now,
	}

	err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{
		Lease: LeaseTarget{LeaseID: "cbx_unmanaged", Server: Server{CloudID: "inst_unmanaged"}},
	})
	if err == nil || !strings.Contains(err.Error(), "not managed by Crabbox") {
		t.Fatalf("ReleaseLease error=%v", err)
	}
	if deleteCalls != 0 || pauseCalls != 0 {
		t.Fatalf("deleteCalls=%d pauseCalls=%d, want 0", deleteCalls, pauseCalls)
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
					return morphInstance{
						ID:       instanceID,
						Status:   "paused",
						Metadata: morphMetadata{"crabbox": "true", "provider": providerName},
					}, nil
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
					return morphInstance{
						ID:       instanceID,
						Status:   "ready",
						Metadata: morphMetadata{"crabbox": "true", "provider": providerName},
					}, nil
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

func TestMorphReleaseLeaseMessage(t *testing.T) {
	lease := LeaseTarget{
		LeaseID: "cbx_release",
		Server:  Server{CloudID: "inst_release"},
	}

	pauseBackend := &morphLeaseBackend{cfg: testMorphConfig()}
	if got := pauseBackend.ReleaseLeaseMessage(lease); got != "paused lease=cbx_release instance=inst_release retained=true" {
		t.Fatalf("pause message=%q", got)
	}

	deleteCfg := testMorphConfig()
	deleteCfg.Morph.DeleteOnRelease = true
	deleteBackend := &morphLeaseBackend{cfg: deleteCfg}
	if got := deleteBackend.ReleaseLeaseMessage(lease); got != "deleted lease=cbx_release instance=inst_release" {
		t.Fatalf("delete message=%q", got)
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
				ID:       instanceID,
				Status:   "ready",
				Refs:     morphInstanceRefs{SnapshotID: "snapshot_123"},
				Metadata: morphMetadata{"crabbox": "true", "provider": providerName},
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

func TestMorphTouchPreservesCustomWorkRootAndProtectsActiveRun(t *testing.T) {
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
		"crabbox":    "true",
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
	if gotTTL != int((2 * time.Hour).Seconds()) {
		t.Fatalf("ttl=%d want %d", gotTTL, int((2 * time.Hour).Seconds()))
	}
	if gotWakeOnSSH == nil || *gotWakeOnSSH {
		t.Fatalf("unexpected wakeOnSSH value: %#v", gotWakeOnSSH)
	}
	if server.Status != "running" || server.Labels["state"] != "running" || server.Labels["work_root"] != "/workspace/custom" {
		t.Fatalf("unexpected touched server: %#v", server)
	}
}

func TestMorphTouchPreservesInstanceSnapshotIdentity(t *testing.T) {
	configureMorphTestHome(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	cfg := testMorphConfig()
	cfg.Morph.Snapshot = "snapshot_new"
	cfg.ServerType = "snapshot_new"
	instance := morphInstance{
		ID:     "inst_snapshot",
		Status: "ready",
		Refs:   morphInstanceRefs{SnapshotID: "snapshot_old"},
		Metadata: morphMetadata{
			"crabbox":     "true",
			"provider":    providerName,
			"snapshot_id": "snapshot_old",
			"server_type": "snapshot_new",
		},
	}

	var gotMetadata map[string]string
	fake := &fakeMorphAPI{
		getInstance: func(_ context.Context, _ string) (morphInstance, error) {
			return instance, nil
		},
		setInstanceMetadata: func(_ context.Context, _ string, metadata map[string]string) error {
			gotMetadata = metadata
			return nil
		},
		updateInstanceTTL: func(_ context.Context, _ string, _ int, _ string) error {
			return nil
		},
		updateInstanceWake: func(_ context.Context, _ string, _, _ *bool) error {
			return nil
		},
	}
	backend := &morphLeaseBackend{
		spec:   Provider{}.Spec(),
		cfg:    cfg,
		rt:     Runtime{Stdout: io.Discard, Stderr: io.Discard},
		client: fake,
		now:    func() time.Time { return now },
	}

	server, err := backend.Touch(context.Background(), TouchRequest{
		Lease:       LeaseTarget{LeaseID: "cbx_snapshot", Server: Server{CloudID: instance.ID}},
		State:       "ready",
		IdleTimeout: 30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotMetadata["snapshot_id"] != "snapshot_old" || gotMetadata["server_type"] != "snapshot_old" {
		t.Fatalf("snapshot identity changed: %#v", gotMetadata)
	}
	if server.ServerType.Name != "snapshot_old" {
		t.Fatalf("server type=%q", server.ServerType.Name)
	}
}

func TestMorphServerUsesProviderStateWhenInstanceIsNotReady(t *testing.T) {
	cfg := testMorphConfig()
	for _, status := range []string{"paused", "failed"} {
		instance := morphInstance{
			ID:       "inst_state",
			Status:   status,
			Metadata: morphMetadata{"state": "running"},
		}
		server := morphServer(instance, cfg, "cbx_state", "state-test")
		if server.Status != status || server.Labels["state"] != status {
			t.Fatalf("status=%q server=%#v", status, server)
		}
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
