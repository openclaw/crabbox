package utm

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const snapshotUUID = "33333333-3333-4333-8333-333333333333"
const unrelatedSnapshotUUID = "44444444-4444-4444-8444-444444444444"

type snapshotFixture struct {
	control                              *controlFixture
	b                                    *backend
	snapshots                            []nativeSnapshot
	manifest                             snapshotManifest
	mutations                            []string
	marker                               string
	savedMarker                          string
	failCreate, failReadback, failDelete bool
	cancel                               func()
	hostVersion                          string
	eventStderr                          bool
}

func boolp(v bool) *bool { return &v }
func (f *snapshotFixture) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	out := func(v string) (core.LocalCommandResult, error) { return core.LocalCommandResult{Stdout: v}, nil }
	if req.Name == "/usr/bin/sw_vers" {
		return out(f.hostVersion)
	}
	if req.Name == "/usr/bin/plutil" {
		if req.Args[0] == "-extract" && req.Args[1] == "CFBundleShortVersionString" {
			return out("5.0.8")
		}
		if req.Args[0] == "-convert" {
			path := req.Args[len(req.Args)-1]
			data, err := os.ReadFile(path)
			if err != nil {
				return core.LocalCommandResult{}, err
			}
			var value any
			if err = json.Unmarshal(data, &value); err != nil {
				return core.LocalCommandResult{}, err
			}
			return out("<plist version=\"1.0\">" + fixturePlist(value) + "</plist>")
		}
	}
	if req.Name == "/usr/bin/osascript" {
		if len(req.Args) > 3 && req.Args[0] == "-l" && req.Args[3] == snapshotListScript {
			if f.failReadback && len(f.mutations) > 0 {
				return core.LocalCommandResult{}, errors.New("snapshot inventory unavailable")
			}
			data, _ := json.Marshal(f.snapshots)
			return out(string(data))
		}
		if len(req.Args) == 1 && req.Args[0] == "-" {
			script, _ := io.ReadAll(req.Stdin)
			text := string(script)
			if strings.Contains(text, "create snapshot targetVM") {
				f.mutations = append(f.mutations, "create")
				name := regexp.MustCompile(`named "([^"]+)"`).FindStringSubmatch(text)[1]
				f.snapshots = append(f.snapshots, nativeSnapshot{ID: snapshotUUID, Name: name, IncludesRunningState: boolp(false), DataMissing: boolp(false), Modified: "2020-01-01T00:00:00Z"})
				f.savedMarker = f.marker
				f.writeManifest(snapshotUUID, false, false)
				if f.cancel != nil {
					f.cancel()
				}
				if f.failCreate {
					return out("")
				}
				return out(snapshotUUID)
			}
			if strings.Contains(text, "restore targetSnapshot") || strings.Contains(text, "delete snapshot targetSnapshot") {
				// The actor independently rejects name targeting or a VM-wide command.
				if !strings.Contains(text, `set targetSnapshot to first snapshot of targetVM whose id is "`+snapshotUUID+`"`) {
					return core.LocalCommandResult{}, errors.New("mutation did not target exact snapshot UUID")
				}
				if strings.Contains(text, "restore targetSnapshot") {
					if !strings.Contains(text, "status of targetVM is not stopped") {
						return core.LocalCommandResult{}, errors.New("missing stopped guard")
					}
					if f.eventStderr {
						return core.LocalCommandResult{Stderr: "Apple event failed despite successful client exit"}, nil
					}
					f.mutations = append(f.mutations, "restore")
					f.marker = f.savedMarker
					f.manifest.CurrentParent = snapshotUUID
					f.writeManifest(snapshotUUID, false, false)
					return out("")
				}
				f.mutations = append(f.mutations, "delete")
				if f.failDelete {
					return core.LocalCommandResult{}, errors.New("snapshot delete failed")
				}
				f.snapshots = f.snapshots[:1]
				return out("")
			}
			req.Stdin = strings.NewReader(text)
		}
	}
	return f.control.Run(ctx, req)
}
func (f *snapshotFixture) writeManifest(id string, orphan, incomplete bool) {
	f.manifest.Version = 1
	f.manifest.Snapshots = nil
	entry := struct {
		Identifier, BackendIdentifier  string
		HasState, Orphaned, Incomplete bool
	}{Identifier: id, BackendIdentifier: "native-backend-state", Orphaned: orphan, Incomplete: incomplete}
	if len(f.snapshots) > 1 {
		entry.HasState = *f.snapshots[1].IncludesRunningState
	}
	f.manifest.Snapshots = append(f.manifest.Snapshots, entry)
	records, _ := f.b.records()
	if len(records) != 1 {
		panic("fixture journal missing")
	}
	data, _ := json.Marshal(f.manifest)
	if err := os.WriteFile(filepath.Join(records[0].BundlePath, "snapshots.plist"), data, 0600); err != nil {
		panic(err)
	}
}
func checkpointSetup(t *testing.T) (*backend, *snapshotFixture, core.LeaseTarget, core.Repo) {
	t.Helper()
	b, control := setup(t)
	repo := core.Repo{Root: t.TempDir(), Name: "my-app"}
	lease, err := b.Acquire(t.Context(), core.AcquireRequest{Repo: repo, Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	r, err := b.lookup(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	for i := range control.vms {
		if control.vms[i].ID == r.VMID {
			control.vms[i].Status = "stopped"
		}
	}
	path := filepath.Join(r.BundlePath, "config.plist")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err = json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["Drive"] = []any{map[string]any{"Identifier": "disk-1", "ImageName": "disk.qcow2", "ImageType": "Disk", "ReadOnly": false}, map[string]any{"Identifier": "cd-1", "ImageType": "CD", "ReadOnly": true}}
	cfg["QEMU"].(map[string]any)["UEFIBoot"] = false
	data, _ = json.Marshal(cfg)
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(r.BundlePath, "Data"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(r.BundlePath, "Data", "disk.qcow2"), []byte("QFI\xfbfixture"), 0600); err != nil {
		t.Fatal(err)
	}
	f := &snapshotFixture{control: control, b: b, marker: "before", hostVersion: "27.0", snapshots: []nativeSnapshot{{ID: unrelatedSnapshotUUID, Name: "unrelated", IncludesRunningState: boolp(true), DataMissing: boolp(false), Modified: "2020-01-01T00:00:00Z"}}}
	b.rt.Exec = f
	return b, f, lease, repo
}
func captureFixture(t *testing.T, b *backend, lease core.LeaseTarget) (core.NativeCheckpointCreateResult, []core.NativeCheckpointCreateResult, error) {
	t.Helper()
	var receipts []core.NativeCheckpointCreateResult
	result, err := b.captureCheckpoint(t.Context(), core.NativeCheckpointCreateRequest{Config: b.cfg, Server: lease.Server, LeaseID: lease.LeaseID, CheckpointID: "chk_0123456789abcdef", Persist: func(r core.NativeCheckpointCreateResult) error { receipts = append(receipts, r); return nil }})
	return result, receipts, err
}
func resourceFixture(r core.NativeCheckpointCreateResult) core.NativeCheckpointResourceRequest {
	return core.NativeCheckpointResourceRequest{Image: r.Image, Metadata: r.Metadata, CaptureEstablished: r.CaptureEstablished}
}
func restoreFixture(b *backend, lease core.LeaseTarget, repo core.Repo, result core.NativeCheckpointCreateResult) core.NativeCheckpointRestoreRequest {
	return core.NativeCheckpointRestoreRequest{Config: b.cfg, Repo: repo, Lease: lease, Resource: resourceFixture(result)}
}
func TestUTMCheckpointCaptureRestoreDeleteExactOwnedState(t *testing.T) {
	b, f, lease, repo := checkpointSetup(t)
	// Configured leases may be renamed. UUID + marker + current bundle name are authority.
	for i := range f.control.vms {
		if f.control.vms[i].ID == lease.Server.CloudID {
			f.control.vms[i].Name = "renamed retained VM"
		}
	}
	r, _ := b.lookup(lease.LeaseID)
	path := filepath.Join(r.BundlePath, "config.plist")
	data, _ := os.ReadFile(path)
	var cfg map[string]any
	_ = json.Unmarshal(data, &cfg)
	cfg["Information"].(map[string]any)["Name"] = "renamed retained VM"
	data, _ = json.Marshal(cfg)
	_ = os.WriteFile(path, data, 0600)
	result, receipts, err := captureFixture(t, b, lease)
	if err != nil {
		t.Fatal(err)
	}
	if !result.CaptureEstablished || result.Image.ID != snapshotUUID || len(receipts) < 3 || receipts[0].Image.State != "pending" || receipts[1].Image.ID != snapshotUUID {
		t.Fatalf("missing durable identity/attestation: %#v receipts=%#v", result, receipts)
	}
	m, err := checkpointMetadataFrom(resourceFixture(result))
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != 1 || m.CaptureState != "stopped" || m.IncludesRunningState || len(m.Scope.Included) != 1 || len(m.Scope.Excluded) != 1 || !m.Scope.HostSharesExcluded {
		t.Fatalf("incomplete capture scope: %+v", m)
	}
	audit, err := b.verifyCheckpoint(t.Context(), resourceFixture(result), m)
	if err != nil || audit.ProviderState != "available" {
		t.Fatalf("audit=%+v err=%v", audit, err)
	}
	f.marker = "after"
	req := restoreFixture(b, lease, repo, result)
	req.DryRun = true
	if _, err = b.restoreCheckpoint(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if f.marker != "after" || len(f.mutations) != 1 {
		t.Fatal("dry-run changed VM")
	}
	req.DryRun = false
	restored, err := b.restoreCheckpoint(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if f.marker != "before" || restored.Server.CloudID != lease.Server.CloudID || restored.SSH.Port != lease.SSH.Port || !restored.SSH.NoControlMaster {
		t.Fatalf("state/transport not restored: %+v marker=%s", restored, f.marker)
	}
	for i := range f.control.vms {
		if f.control.vms[i].ID == lease.Server.CloudID {
			f.control.vms[i].Status = "stopped"
		}
	}
	f.failDelete = true
	if err = b.deleteCheckpoint(t.Context(), resourceFixture(result), m); err == nil {
		t.Fatal("failed deletion reported success")
	}
	if len(f.snapshots) != 2 {
		t.Fatal("failed deletion lost identity")
	}
	f.failDelete = false
	if err = b.deleteCheckpoint(t.Context(), resourceFixture(result), m); err != nil {
		t.Fatal(err)
	}
	if len(f.snapshots) != 1 || f.snapshots[0].ID != unrelatedSnapshotUUID {
		t.Fatal("unrelated snapshot removed")
	}
	for _, id := range []string{sourceUUID, otherUUID, lease.Server.CloudID} {
		if _, ok := findVM(f.control.vms, id); !ok {
			t.Fatalf("VM removed: %s", id)
		}
	}
}
func TestUTMCheckpointCapturePreservesIDOnCancelAndReadbackFailure(t *testing.T) {
	for _, failure := range []string{"cancel", "readback", "missing-response"} {
		t.Run(failure, func(t *testing.T) {
			b, f, lease, _ := checkpointSetup(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if failure == "cancel" {
				f.cancel = cancel
			}
			if failure == "readback" {
				f.failReadback = true
			}
			if failure == "missing-response" {
				f.failCreate = true
			}
			var receipts []core.NativeCheckpointCreateResult
			result, err := b.captureCheckpoint(ctx, core.NativeCheckpointCreateRequest{Config: b.cfg, Server: lease.Server, LeaseID: lease.LeaseID, Persist: func(r core.NativeCheckpointCreateResult) error { receipts = append(receipts, r); return nil }})
			if err == nil || result.CaptureEstablished || result.Image.ID != snapshotUUID {
				t.Fatalf("lost failed capture identity: %+v %v", result, err)
			}
			var noSubmit core.NativeCheckpointNotSubmittedError
			if errors.As(err, &noSubmit) {
				t.Fatal("claimed non-submission after capture")
			}

			if len(receipts) < 2 || receipts[1].Image.ID != snapshotUUID {
				t.Fatal("returned ID not persisted before verification")
			}
		})
	}
}
func TestUTMCheckpointAdmissionBeforeMutation(t *testing.T) {
	for _, scenario := range []string{"running", "raw", "no-disk", "marker", "persist", "hold", "image", "retire"} {
		t.Run(scenario, func(t *testing.T) {
			b, f, lease, _ := checkpointSetup(t)
			req := core.NativeCheckpointCreateRequest{Config: b.cfg, Server: lease.Server, LeaseID: lease.LeaseID, Persist: func(core.NativeCheckpointCreateResult) error { return nil }}
			r, _ := b.lookup(lease.LeaseID)
			switch scenario {
			case "running":
				for i := range f.control.vms {
					if f.control.vms[i].ID == r.VMID {
						f.control.vms[i].Status = "started"
					}
				}
			case "raw":
				_ = os.WriteFile(filepath.Join(r.BundlePath, "Data", "disk.qcow2"), []byte("raw image"), 0600)
			case "no-disk":
				path := filepath.Join(r.BundlePath, "config.plist")
				data, _ := os.ReadFile(path)
				var v map[string]any
				_ = json.Unmarshal(data, &v)
				v["Drive"] = []any{}
				data, _ = json.Marshal(v)
				_ = os.WriteFile(path, data, 0600)
			case "marker":
				for i := range f.control.vms {
					if f.control.vms[i].ID == r.VMID {
						f.control.vms[i].Configuration.Notes = "changed"
					}
				}
			case "persist":
				req.Persist = func(core.NativeCheckpointCreateResult) error { return errors.New("disk full") }
			case "hold":
				_ = core.WithDurableLeaseClaimLock(lease.LeaseID, func(c *core.LeaseClaim, _ bool, persist func() error) error {
					c.CheckpointCapture = &core.CheckpointCaptureBinding{ID: "chk_other"}
					return persist()
				})
			case "image":
				req.Strategy = core.CheckpointStrategyImage
			case "retire":
				req.Capture = &core.NativeCheckpointCapture{SourceDisposition: "retire"}
			}
			_, err := b.captureCheckpoint(t.Context(), req)
			var noSubmit core.NativeCheckpointNotSubmittedError
			if err == nil || !errors.As(err, &noSubmit) || len(f.mutations) != 0 {
				t.Fatalf("unsafe admission scenario=%s mutations=%v err=%v", scenario, f.mutations, err)
			}
		})
	}
}
func TestUTMCheckpointRestoreRejectsPendingMissingChangedAndLive(t *testing.T) {
	for _, scenario := range []string{"pending", "unattested", "missing", "incomplete", "scope", "generation", "different-lease", "live", "modified", "overwritten"} {
		t.Run(scenario, func(t *testing.T) {
			b, f, lease, repo := checkpointSetup(t)
			result, _, err := captureFixture(t, b, lease)
			if err != nil {
				t.Fatal(err)
			}
			req := restoreFixture(b, lease, repo, result)
			switch scenario {
			case "pending":
				req.Resource.Image.State = "pending"
			case "unattested":
				req.Resource.CaptureEstablished = false
			case "missing":
				f.snapshots = f.snapshots[:1]
			case "incomplete":
				f.writeManifest(snapshotUUID, false, true)
			case "scope":
				r, _ := b.lookup(lease.LeaseID)
				path := filepath.Join(r.BundlePath, "config.plist")
				data, _ := os.ReadFile(path)
				var v map[string]any
				_ = json.Unmarshal(data, &v)
				v["Drive"].([]any)[0].(map[string]any)["Identifier"] = "changed-disk"
				data, _ = json.Marshal(v)
				_ = os.WriteFile(path, data, 0600)
			case "generation":
				_ = core.WithDurableLeaseClaimLock(lease.LeaseID, func(c *core.LeaseClaim, _ bool, persist func() error) error {
					c.ClaimedAt = "different-generation"
					return persist()
				})
			case "different-lease":
				req.Lease.LeaseID = "cbx_different"
			case "live":
				for i := range f.control.vms {
					if f.control.vms[i].ID == lease.Server.CloudID {
						f.control.vms[i].Status = "started"
					}
				}
			case "overwritten":
				f.manifest.Snapshots[0].BackendIdentifier = "different-native-state"
				records, _ := b.records()
				data, _ := json.Marshal(f.manifest)
				_ = os.WriteFile(filepath.Join(records[0].BundlePath, "snapshots.plist"), data, 0600)
			case "modified":
				f.snapshots[1].Modified = "2020-01-02T00:00:00Z"
			}
			_, err = b.restoreCheckpoint(t.Context(), req)
			if err == nil || len(f.mutations) != 1 {
				t.Fatalf("unsafe restore mutations=%v err=%v", f.mutations, err)
			}
		})
	}
}
func TestUTMCheckpointReconnectionFailureDoesNotClaimRestoreSuccess(t *testing.T) {
	b, f, lease, repo := checkpointSetup(t)
	result, _, err := captureFixture(t, b, lease)
	if err != nil {
		t.Fatal(err)
	}
	waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
		return errors.New("wrong guest identity")
	}
	_, err = b.restoreCheckpoint(t.Context(), restoreFixture(b, lease, repo, result))
	if err == nil || !strings.Contains(err.Error(), "verified SSH reconnection failed") || len(f.mutations) != 2 {
		t.Fatalf("missing reconnect proof: %v %v", err, f.mutations)
	}
}
func TestUTMCheckpointBackendHostGateAndNoFork(t *testing.T) {
	b, f, _, _ := checkpointSetup(t)
	f.hostVersion = "26.0"
	if _, _, err := b.checkpointHost(t.Context(), "apple"); err == nil || !strings.Contains(err.Error(), "macOS 27") {
		t.Fatalf("Apple host admitted: %v", err)
	}
	if _, _, err := b.checkpointHost(t.Context(), "qemu"); err != nil {
		t.Fatal(err)
	}
	cap, ok := (Provider{}).NativeCheckpointCapability(core.NativeCheckpointRequest{})
	if !ok || cap.RetireSource || cap.RetireUnsupported == "" {
		t.Fatal("lease-bound source retirement advertised")
	}
	if (Provider{}).Spec().Features.Has(core.FeatureFork) {
		t.Fatal("fork advertised")
	}
	if _, ok := any(Provider{}).(core.NativeCheckpointForkProvider); ok {
		t.Fatal("fork implemented")
	}
}
func TestUTMCheckpointManifestAndInternalPathValidation(t *testing.T) {
	b, f, lease, _ := checkpointSetup(t)
	result, _, err := captureFixture(t, b, lease)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := b.lookup(lease.LeaseID)
	f.manifest.Version = 2
	data, _ := json.Marshal(f.manifest)
	_ = os.WriteFile(filepath.Join(r.BundlePath, "snapshots.plist"), data, 0600)
	if _, err = b.snapshotManifest(t.Context(), r.BundlePath); err == nil {
		t.Fatal("unknown native manifest format accepted")
	}
	for _, name := range []string{"../disk", "/external", "", "dir/disk"} {
		if validateCheckpointFile(r.BundlePath, name, false) == nil {
			t.Fatalf("external name accepted: %q", name)
		}
	}
	path := filepath.Join(r.BundlePath, "Data", "link.qcow2")
	if err = os.Symlink(filepath.Join(r.BundlePath, "Data", "disk.qcow2"), path); err != nil {
		t.Fatal(err)
	}
	if validateCheckpointFile(r.BundlePath, "link.qcow2", true) == nil {
		t.Fatal("symlink image accepted")
	}
	result.Image.ID = unrelatedSnapshotUUID
	if _, err = checkpointMetadataFrom(resourceFixture(result)); err == nil {
		t.Fatal("snapshot receipt mismatch accepted")
	}
}

func fixturePlist(value any) string {
	switch v := value.(type) {
	case map[string]any:
		result := "<dict>"
		for k, item := range v {
			var key strings.Builder
			_ = xml.EscapeText(&key, []byte(k))
			result += "<key>" + key.String() + "</key>" + fixturePlist(item)
		}
		return result + "</dict>"
	case []any:
		result := "<array>"
		for _, item := range v {
			result += fixturePlist(item)
		}
		return result + "</array>"
	case bool:
		if v {
			return "<true/>"
		}
		return "<false/>"
	case float64:
		return "<real>" + fmt.Sprint(v) + "</real>"
	case string:
		var text strings.Builder
		_ = xml.EscapeText(&text, []byte(v))
		return "<string>" + text.String() + "</string>"
	default:
		panic("unsupported fixture plist value")
	}
}
func TestUTMCheckpointNativePlistDateDataAndUnknownFields(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("native macOS plist codec")
	}
	path := filepath.Join(t.TempDir(), "snapshots.plist")
	original := `<?xml version="1.0"?><plist version="1.0"><dict><key>Version</key><integer>1</integer><key>Created</key><date>2020-01-01T00:00:00Z</date><key>HardwareModel</key><data>AQIDBA==</data><key>Snapshots</key><array><dict><key>Identifier</key><string>` + snapshotUUID + `</string><key>BackendIdentifier</key><string>backend-state</string><key>HasState</key><true/><key>Created</key><date>2020-01-01T00:00:00Z</date></dict></array></dict></plist>`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("/usr/bin/plutil", "-convert", "binary1", path).CombinedOutput(); err != nil {
		t.Fatalf("binary fixture: %s %v", out, err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The production reader's conversion command is used against a native binary
	// fixture containing the data/date types actual UTM configs/manifests carry.
	out, err := exec.Command("/usr/bin/plutil", "-convert", "xml1", "-o", "-", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	var manifest snapshotManifest
	if err = decodeCheckpointPlist(string(out), &manifest); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) || manifest.Version != 1 || len(manifest.Snapshots) != 1 || manifest.Snapshots[0].Identifier != snapshotUUID || !manifest.Snapshots[0].HasState {
		t.Fatalf("plist changed or state lost: %+v", manifest)
	}
}

func TestUTMCheckpointStoppedSuspendedStateRemainsExplicit(t *testing.T) {
	b, f, lease, _ := checkpointSetup(t)
	original := f.b.rt.Exec
	f.b.rt.Exec = checkpointCaptureStateRunner{fixture: f, delegate: original}
	result, _, err := captureFixture(t, b, lease)
	if err != nil {
		t.Fatal(err)
	}
	m, err := checkpointMetadataFrom(resourceFixture(result))
	if err != nil {
		t.Fatal(err)
	}
	if m.CaptureState != "stopped" || !m.IncludesRunningState || !result.CaptureEstablished {
		t.Fatalf("lost actual suspended state: %+v", m)
	}
}

type checkpointCaptureStateRunner struct {
	fixture  *snapshotFixture
	delegate core.CommandRunner
}

func (r checkpointCaptureStateRunner) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	result, err := r.delegate.Run(ctx, req)
	if req.Name == "/usr/bin/osascript" && len(req.Args) > 3 && req.Args[3] == snapshotListScript && len(r.fixture.snapshots) > 1 {
		r.fixture.snapshots[1].IncludesRunningState = boolp(true)
		r.fixture.writeManifest(snapshotUUID, false, false)
		data, _ := json.Marshal(r.fixture.snapshots)
		result.Stdout = string(data)
	}
	return result, err
}

func TestUTMCheckpointZeroExitEventErrorCannotEstablishRestore(t *testing.T) {
	b, f, lease, repo := checkpointSetup(t)
	result, _, err := captureFixture(t, b, lease)
	if err != nil {
		t.Fatal(err)
	}
	f.marker = "after"
	f.eventStderr = true
	_, err = b.restoreCheckpoint(t.Context(), restoreFixture(b, lease, repo, result))
	if err == nil || !strings.Contains(err.Error(), "event reported an error") || f.marker != "after" {
		t.Fatalf("zero exit claimed restoration: %v marker=%s", err, f.marker)
	}
}

func TestUTMCheckpointDeleteMissingDataStillTargetsExactSnapshot(t *testing.T) {
	b, f, lease, _ := checkpointSetup(t)
	result, _, err := captureFixture(t, b, lease)
	if err != nil {
		t.Fatal(err)
	}
	m, err := checkpointMetadataFrom(resourceFixture(result))
	if err != nil {
		t.Fatal(err)
	}
	r, _ := b.lookup(lease.LeaseID)
	if err = os.Remove(filepath.Join(r.BundlePath, "Data", "disk.qcow2")); err != nil {
		t.Fatal(err)
	}
	f.snapshots[1].DataMissing = boolp(true)
	f.writeManifest(snapshotUUID, true, false)
	if err = b.deleteCheckpoint(t.Context(), resourceFixture(result), m); err != nil {
		t.Fatal(err)
	}
	if len(f.snapshots) != 1 || f.snapshots[0].ID != unrelatedSnapshotUUID {
		t.Fatal("orphan cleanup affected unrelated snapshot")
	}
}

type checkpointBundleValidationProbe struct {
	path   string
	record record
	vm     vm
	err    error
}

func (p *checkpointBundleValidationProbe) validateMacOSBundle(_ context.Context, path string, r record, v vm) error {
	p.path = path
	p.record = r
	p.vm = v
	return p.err
}
func TestUTMCheckpointRequiresUnderlyingPersistedAppleValidation(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "owned.utm", "config.plist")
	r := record{VMID: sourceUUID, Token: "owned-token", BundlePath: filepath.Dir(path)}
	v := vm{ID: sourceUUID, Backend: "apple", Name: "current display name"}
	if err := validateCheckpointBackendBundle(ctx, struct{}{}, path, r, v); err == nil {
		t.Fatal("Apple source admitted without underlying persisted OS/network validator")
	}
	failure := errors.New("persisted guest OS or shared NIC differs from owned lease")
	probe := &checkpointBundleValidationProbe{err: failure}
	if err := validateCheckpointBackendBundle(ctx, probe, path, r, v); !errors.Is(err, failure) {
		t.Fatalf("persisted validation failure ignored: %v", err)
	}
	if probe.path != path || probe.record.VMID != r.VMID || probe.record.Token != r.Token || probe.vm.Name != v.Name {
		t.Fatalf("validator lost exact owned path/identity: %+v", probe)
	}
	probe.err = nil
	if err := validateCheckpointBackendBundle(ctx, probe, path, r, v); err != nil {
		t.Fatal(err)
	}
	v.Backend = "qemu"
	if err := validateCheckpointBackendBundle(ctx, struct{}{}, path, r, v); err != nil {
		t.Fatalf("QEMU unexpectedly requires Apple adapter: %v", err)
	}
}
