package utm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

// UTM 5.0.8 rejects nested port-forward records through update configuration.
// Its public reload command expressly supports external edits of stopped .utm
// bundles. Use the system plist editor so binary/data/date values and unrelated
// configuration survive without introducing a Python runtime or a plist codec.
func (b *backend) plistExtract(ctx context.Context, path, key, format string, out any) error {
	result, err := b.command(ctx, "/usr/bin/plutil", []string{"-extract", key, format, "-o", "-", path}, nil)
	if err != nil {
		return fmt.Errorf("read UTM bundle %s: %w", key, err)
	}
	if format == "raw" {
		p, ok := out.(*string)
		if !ok {
			return fmt.Errorf("raw plist destination must be a string")
		}
		*p = strings.TrimSpace(result.Stdout)
		return nil
	}
	if err = json.Unmarshal([]byte(result.Stdout), out); err != nil {
		return fmt.Errorf("decode UTM bundle %s: %w", key, err)
	}
	return nil
}
func (b *backend) storageRoot() (string, error) {
	if b.cfg.UTM.StorageRoot == "" {
		return "", fmt.Errorf("provider=utm requires utm.storageRoot or --utm-storage-root: the local directory where UTM creates cloned .utm bundles")
	}
	root, err := filepath.EvalSymlinks(b.cfg.UTM.StorageRoot)
	if err != nil {
		return "", fmt.Errorf("UTM storage root unavailable: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("utm.storageRoot must be an existing directory")
	}
	return root, nil
}
func regularBundleConfig(root, name string) (string, error) {
	if filepath.Base(name) != name || !strings.HasSuffix(name, ".utm") {
		return "", fmt.Errorf("invalid UTM bundle name")
	}
	bundle := filepath.Join(root, name)
	info, err := os.Lstat(bundle)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("UTM bundle must be a real directory, not a symlink: %s", name)
	}
	path := filepath.Join(bundle, "config.plist")
	info, err = os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("UTM bundle config must be a regular file: %s", name)
	}
	return path, nil
}
func (b *backend) cloneBundle(ctx context.Context, r record) (string, error) {
	if err := r.validate(); err != nil {
		return "", err
	}
	if r.VMID == "" {
		return "", fmt.Errorf("UTM clone UUID must be recorded before editing its bundle")
	}
	root, err := b.storageRoot()
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	var matches []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".utm") {
			continue
		}
		path, err := regularBundleConfig(root, e.Name())
		if err != nil {
			return "", err
		}
		var id string
		if err = b.plistExtract(ctx, path, "Information.UUID", "raw", &id); err != nil {
			return "", err
		}
		if strings.EqualFold(id, r.VMID) {
			matches = append(matches, path)
		}
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("UTM clone UUID %s requires exactly one bundle under utm.storageRoot; found %d", r.VMID, len(matches))
	}
	bundle := filepath.Dir(matches[0])
	if r.BundlePath != "" && r.BundlePath != bundle {
		return "", fmt.Errorf("UTM clone bundle location changed from recorded ownership path")
	}
	return matches[0], nil
}

type bundleInformation struct {
	UUID  string
	Name  string
	Notes string
}
type bundleNetwork struct {
	Mode        string
	MacAddress  string
	PortForward []map[string]any
}

func (b *backend) validateBundle(ctx context.Context, path string, r record, v vm) error {
	if r.isMacOS() {
		return b.validateMacOSBundle(ctx, path, r, v)
	}
	var info bundleInformation
	var networks []bundleNetwork
	var backend, architecture string
	for _, read := range []func() error{
		func() error { return b.plistExtract(ctx, path, "Information", "json", &info) },
		func() error { return b.plistExtract(ctx, path, "Backend", "raw", &backend) },
		func() error { return b.plistExtract(ctx, path, "System.Architecture", "raw", &architecture) },
		func() error { return b.plistExtract(ctx, path, "Network", "json", &networks) },
	} {
		if err := read(); err != nil {
			return err
		}
	}
	if !strings.EqualFold(info.UUID, r.VMID) || strings.EqualFold(info.UUID, r.SourceID) || info.Name != v.Name || info.Notes != v.Configuration.Notes || backend != "QEMU" || architecture != r.Architecture {
		return fmt.Errorf("UTM on-disk clone identity does not match its recorded UUID/name and live configuration")
	}
	if len(networks) != 1 || networks[0].Mode != "Emulated" || !strings.EqualFold(networks[0].MacAddress, v.Configuration.Networks[0].Address) {
		return fmt.Errorf("UTM on-disk clone NIC does not match the stopped emulated live configuration")
	}
	return nil
}
func (b *backend) stoppedClone(ctx context.Context, r record) (vm, error) {
	vms, err := b.inventory(ctx)
	if err != nil {
		return vm{}, err
	}
	v, ok := findVM(vms, r.VMID)
	if !ok || v.Status != "stopped" || v.Backend != "qemu" || v.Name != r.OwnedName || v.ID == r.SourceID {
		return vm{}, fmt.Errorf("UTM configuration requires the exact owned stopped QEMU clone")
	}
	if err = validateTemplate(v); err != nil {
		return vm{}, err
	}
	if v.Configuration.Architecture != r.Architecture {
		return vm{}, fmt.Errorf("UTM clone architecture changed")
	}
	return v, nil
}
func (b *backend) configureClone(ctx context.Context, r *record) error {
	before, err := b.stoppedClone(ctx, *r)
	if err != nil {
		return err
	}
	path, err := b.cloneBundle(ctx, *r)
	if err != nil {
		return err
	}
	originalInfo, err := os.Lstat(path)
	if err != nil {
		return err
	}
	original, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// Work only on a private same-directory copy. The original is untouched on
	// validation, plist-editor, cancellation or freshness failures.
	f, err := os.CreateTemp(filepath.Dir(path), ".crabbox-config-*.plist")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if _, err = f.Write(original); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = b.validateBundle(ctx, temp, *r, before); err != nil {
		return err
	}
	r.BundlePath = filepath.Dir(path)
	if err = saveRecord(*r); err != nil {
		return err
	}
	forwards, _ := json.Marshal([]map[string]any{{"Protocol": "TCP", "HostAddress": "127.0.0.1", "HostPort": r.Port, "GuestPort": 22}})
	for _, change := range [][]string{{"Information.Notes", "-string", r.marker()}, {"Network.0.MacAddress", "-string", r.MAC}, {"Network.0.PortForward", "-json", string(forwards)}} {
		args := append([]string{"-replace"}, change...)
		args = append(args, temp)
		if _, err = b.command(ctx, "/usr/bin/plutil", args, nil); err != nil {
			return fmt.Errorf("configure owned UTM bundle: %w", err)
		}
	}
	ready, err := os.OpenFile(temp, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = ready.Sync()
	closeErr := ready.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	current, err := b.stoppedClone(ctx, *r)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(before.Configuration, current.Configuration) {
		return fmt.Errorf("UTM clone configuration changed while preparing its network update")
	}
	checked, err := regularBundleConfig(filepath.Dir(filepath.Dir(path)), filepath.Base(filepath.Dir(path)))
	if err != nil {
		return fmt.Errorf("UTM clone bundle path changed before configuration commit: %w", err)
	}
	if checked != path {
		return fmt.Errorf("UTM clone bundle path changed before configuration commit")
	}
	currentInfo, err := os.Lstat(path)
	if err != nil {
		return err
	}
	currentData, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !os.SameFile(originalInfo, currentInfo) || !bytes.Equal(original, currentData) {
		return fmt.Errorf("UTM clone config.plist changed before network update; original preserved")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = os.Rename(temp, path); err != nil {
		return err
	}
	if err = syncDir(filepath.Dir(path)); err != nil {
		return err
	}
	if err = b.control(ctx, "reload", r.VMID); err != nil {
		return fmt.Errorf("owned UTM bundle was updated but reload failed; journal retained: %w", err)
	}
	vms, err := b.inventory(ctx)
	if err != nil {
		return err
	}
	after, ok := findVM(vms, r.VMID)
	if !ok {
		return fmt.Errorf("UTM clone disappeared after configuration reload")
	}
	return validateClone(after, *r)
}
