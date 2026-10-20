package utm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

// The cross-platform actor models plutil's documented key-path contract; a
// separate macOS test below executes the real native editor on binary plists.
func (f *controlFixture) runPlutil(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	if f.nativePlists {
		cmd := exec.CommandContext(ctx, req.Name, req.Args...)
		cmd.Stdin = req.Stdin
		data, err := cmd.CombinedOutput()
		return core.LocalCommandResult{Stdout: string(data)}, err
	}
	path := req.Args[len(req.Args)-1]
	data, err := os.ReadFile(path)
	if err != nil {
		return core.LocalCommandResult{}, err
	}
	var document map[string]any
	if err = json.Unmarshal(data, &document); err != nil {
		return core.LocalCommandResult{}, err
	}
	parts := strings.Split(req.Args[1], ".")
	var current any = document
	for _, part := range parts[:len(parts)-1] {
		switch value := current.(type) {
		case map[string]any:
			current = value[part]
		case []any:
			index, _ := strconv.Atoi(part)
			current = value[index]
		default:
			return core.LocalCommandResult{}, fmt.Errorf("invalid plist path")
		}
	}
	last := parts[len(parts)-1]
	if req.Args[0] == "-extract" {
		var value any
		switch v := current.(type) {
		case map[string]any:
			value = v[last]
		case []any:
			i, _ := strconv.Atoi(last)
			value = v[i]
		}
		if req.Args[2] == "raw" {
			return core.LocalCommandResult{Stdout: fmt.Sprint(value)}, nil
		}
		encoded, _ := json.Marshal(value)
		return core.LocalCommandResult{Stdout: string(encoded)}, nil
	}
	if req.Args[0] != "-replace" {
		return core.LocalCommandResult{}, fmt.Errorf("unexpected plist action")
	}
	var replacement any = req.Args[3]
	if req.Args[2] == "-json" {
		if err = json.Unmarshal([]byte(req.Args[3]), &replacement); err != nil {
			return core.LocalCommandResult{}, err
		}
	}
	switch v := current.(type) {
	case map[string]any:
		v[last] = replacement
	case []any:
		i, _ := strconv.Atoi(last)
		v[i] = replacement
	}
	encoded, _ := json.Marshal(document)
	return core.LocalCommandResult{}, os.WriteFile(path, encoded, 0600)
}
func (f *controlFixture) bundlePath(id string) string {
	return filepath.Join(f.storageRoot, "physical-"+id+".utm", "config.plist")
}
func (f *controlFixture) createBundle(v vm) error {
	path := f.bundlePath(v.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	doc := map[string]any{"Backend": "QEMU", "ConfigurationVersion": 4, "Information": map[string]any{"UUID": v.ID, "Name": v.Name, "Notes": v.Configuration.Notes}, "System": map[string]any{"Architecture": v.Configuration.Architecture, "MemorySize": 2048, "CPUCount": 2}, "Network": []any{map[string]any{"Mode": "Emulated", "Hardware": "virtio-net-pci", "MacAddress": v.Configuration.Networks[0].Address, "PortForward": []any{}}}, "Drive": []any{map[string]any{"ImageName": "original-disk.qcow2", "ReadOnly": false}}, "QEMU": map[string]any{"UEFIBoot": true, "AdditionalArguments": []any{}}, "Sharing": map[string]any{"DirectoryShareMode": "None", "ClipboardSharing": false}}
	data, _ := json.Marshal(doc)
	if err := os.WriteFile(path, data, 0600); err != nil {
		return err
	}
	if f.nativePlists {
		for _, args := range [][]string{{"-convert", "binary1", path}, {"-insert", "OpaqueData", "-data", "AQIDBA==", path}, {"-insert", "OpaqueDate", "-date", "2020-01-01T00:00:00Z", path}} {
			if out, err := exec.Command("/usr/bin/plutil", args...).CombinedOutput(); err != nil {
				return fmt.Errorf("native fixture: %s: %w", out, err)
			}
		}
	}
	return nil
}
func (f *controlFixture) reloadBundle(ctx context.Context, v *vm) error {
	path := f.bundlePath(v.ID)
	info, err := f.runPlutil(ctx, core.LocalCommandRequest{Name: "/usr/bin/plutil", Args: []string{"-extract", "Information", "json", "-o", "-", path}})
	if err != nil {
		return err
	}
	var information bundleInformation
	if err = json.Unmarshal([]byte(info.Stdout), &information); err != nil {
		return err
	}
	netResult, err := f.runPlutil(ctx, core.LocalCommandRequest{Name: "/usr/bin/plutil", Args: []string{"-extract", "Network", "json", "-o", "-", path}})
	if err != nil {
		return err
	}
	var nets []struct {
		Mode        string
		MacAddress  string
		PortForward []forward
	}
	if err = json.Unmarshal([]byte(netResult.Stdout), &nets); err != nil {
		return err
	}
	v.Configuration.Notes = information.Notes
	v.Configuration.Networks = []network{{Index: 0, Mode: strings.ToLower(nets[0].Mode), Address: nets[0].MacAddress, Forwards: nets[0].PortForward}}
	return nil
}
func TestBundleEditPreservesOtherFieldsAndUsesObservedTCP(t *testing.T) {
	b, f := setup(t)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := b.lookup(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	expectedBundle, err := filepath.EvalSymlinks(filepath.Dir(f.bundlePath(r.VMID)))
	if err != nil {
		t.Fatal(err)
	}
	if r.BundlePath != expectedBundle {
		t.Fatal("exact UUID bundle path not persisted")
	}
	data, err := os.ReadFile(f.bundlePath(r.VMID))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err = json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg["Drive"], []any{map[string]any{"ImageName": "original-disk.qcow2", "ReadOnly": false}}) || !reflect.DeepEqual(cfg["System"], map[string]any{"Architecture": "aarch64", "MemorySize": float64(2048), "CPUCount": float64(2)}) {
		t.Fatalf("unrelated configuration changed: %#v", cfg)
	}
	if f.vms[2].Configuration.Networks[0].Forwards[0].Protocol != "TCP" {
		t.Fatal("native TCP representation lost")
	}
}
func TestNativeBinaryPlistEditPreservesDataDatesAndSource(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("native macOS plist editor")
	}
	b, f := setup(t)
	f.nativePlists = true
	if err := f.createBundle(preparedTemplate()); err != nil {
		t.Fatal(err)
	}
	sourceBefore, err := os.ReadFile(f.bundlePath(sourceUUID))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{})
	if err != nil {
		t.Fatal(err)
	}
	path := f.bundlePath(lease.Server.CloudID)
	for key, want := range map[string]string{"OpaqueData": "AQIDBA==", "OpaqueDate": "2020-01-01T00:00:00Z", "Drive.0.ImageName": "original-disk.qcow2", "System.MemorySize": "2048", "Network.0.PortForward.0.Protocol": "TCP"} {
		out, err := exec.Command("/usr/bin/plutil", "-extract", key, "raw", "-o", "-", path).CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != want {
			t.Fatalf("%s=%s err=%v", key, out, err)
		}
	}
	sourceAfter, err := os.ReadFile(f.bundlePath(sourceUUID))
	if err != nil || string(sourceAfter) != string(sourceBefore) {
		t.Fatal("source config changed")
	}
}
func TestBundleIdentityAndSymlinkRefusalsPreserveOriginal(t *testing.T) {
	for _, scenario := range []string{"wrong-name", "wrong-uuid", "duplicate-uuid", "symlink-config", "symlink-bundle", "started", "source-uuid"} {
		t.Run(scenario, func(t *testing.T) {
			b, f := setup(t)
			var original []byte
			var path string
			_, err := b.Acquire(context.Background(), core.AcquireRequest{OnAcquired: func(lease core.LeaseTarget) error {
				path = f.bundlePath(lease.Server.CloudID)
				original, _ = os.ReadFile(path)
				switch scenario {
				case "wrong-name", "wrong-uuid":
					var doc map[string]any
					json.Unmarshal(original, &doc)
					info := doc["Information"].(map[string]any)
					if scenario == "wrong-name" {
						info["Name"] = "unrelated"
					} else {
						info["UUID"] = sourceUUID
					}
					data, _ := json.Marshal(doc)
					os.WriteFile(path, data, 0600)
					original = data
				case "duplicate-uuid":
					other := filepath.Join(f.storageRoot, "duplicate.utm")
					os.MkdirAll(other, 0700)
					os.WriteFile(filepath.Join(other, "config.plist"), original, 0600)
				case "symlink-config":
					outside := filepath.Join(t.TempDir(), "outside.plist")
					os.WriteFile(outside, original, 0600)
					os.Remove(path)
					os.Symlink(outside, path)
				case "symlink-bundle":
					bundle := filepath.Dir(path)
					outside := filepath.Join(t.TempDir(), "outside.utm")
					os.Rename(bundle, outside)
					os.Symlink(outside, bundle)
				case "started":
					f.vms[2].Status = "started"
				case "source-uuid":
					r, e := b.lookup(lease.LeaseID)
					if e != nil {
						return e
					}
					r.VMID = sourceUUID
					if e = saveRecord(r); e == nil {
						return fmt.Errorf("unsafe source journal accepted")
					}
					f.vms[2].Status = "started"
				}
				return nil
			}})
			if err == nil {
				t.Fatal("unsafe bundle edit accepted")
			}
			after, e := os.ReadFile(path)
			if e != nil || string(after) != string(original) {
				t.Fatal("original configuration modified on refusal")
			}
		})
	}
}
func TestReloadFailureKeepsExactJournalAndDiskUpdateRetryable(t *testing.T) {
	b, f := setup(t)
	f.failReload = true
	_, err := b.Acquire(context.Background(), core.AcquireRequest{})
	if err == nil || !strings.Contains(err.Error(), "reload failed") {
		t.Fatalf("err=%v", err)
	}
	records, err := b.records()
	if err != nil || len(records) != 1 || records[0].VMID == "" || records[0].BundlePath == "" || records[0].Configured {
		t.Fatalf("recovery=%#v err=%v", records, err)
	}
	if err = b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
}
