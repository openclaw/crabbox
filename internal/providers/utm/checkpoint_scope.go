package utm

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type checkpointDisk struct{ ID, Image, Role string }
type checkpointScope struct {
	Included, Excluded []checkpointDisk
	HostSharesExcluded bool
}
type checkpointBundle struct {
	Backend     string
	Information bundleInformation
	Drive       []struct {
		Identifier, ImageName, ImageType, Interface string
		ReadOnly                                    *bool
	}
	QEMU   struct{ UEFIBoot bool }
	System struct {
		Architecture string
		Boot         struct{ EfiVariableStoragePath string }
		MacPlatform  struct{ AuxiliaryStoragePath string }
	}
	Sharing struct{ DirectoryShareMode string }
}

// Only the owned bundle is inspected. In particular, external image bookmarks,
// the template bundle and UTM's global registry supply no capture authority.
func (b *backend) checkpointConfig(ctx context.Context, r record, v vm) (cfg checkpointBundle, err error) {
	if r.BundlePath == "" {
		return cfg, fmt.Errorf("UTM checkpoint requires a recorded owned bundle path")
	}
	root, err := b.storageRoot()
	if err != nil {
		return cfg, err
	}
	if filepath.Dir(r.BundlePath) != root {
		return cfg, fmt.Errorf("UTM checkpoint bundle is outside its recorded storage root")
	}
	path, err := regularBundleConfig(root, filepath.Base(r.BundlePath))
	if err != nil {
		return cfg, err
	}
	if err = b.readCheckpointPlist(ctx, path, &cfg); err != nil {
		return cfg, err
	}

	if !strings.EqualFold(cfg.Information.UUID, r.VMID) || cfg.Information.Notes != r.marker() || cfg.Information.Name != v.Name || !strings.EqualFold(cfg.Backend, v.Backend) || cfg.System.Architecture != r.Architecture {
		return cfg, fmt.Errorf("UTM live and owned bundle identity/backend/architecture disagree")
	}
	if err = validateCheckpointBackendBundle(ctx, b, path, r, v); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// The Apple acquisition adapter owns its persisted guest-OS and shared-network
// contract. Keep that same validation at every checkpoint effect, rather than
// trusting live inventory while a different configuration waits on disk.
func validateCheckpointBackendBundle(ctx context.Context, adapter any, path string, r record, v vm) error {
	if v.Backend != "apple" {
		return nil
	}
	validator, ok := adapter.(interface {
		validateMacOSBundle(context.Context, string, record, vm) error
	})
	if !ok {
		return fmt.Errorf("underlying UTM provider cannot validate the persisted Apple checkpoint source")
	}
	return validator.validateMacOSBundle(ctx, path, r, v)
}

func (b *backend) checkpointDiskScope(ctx context.Context, r record, v vm) (scope checkpointScope, err error) {
	cfg, err := b.checkpointConfig(ctx, r, v)
	if err != nil {
		return scope, err
	}

	scope.HostSharesExcluded = true
	hasCustomBIOS := false
	for _, d := range cfg.Drive {
		if d.ImageType == "BIOS" || d.ImageType == "LinuxKernel" || (d.ImageType == "Disk" || d.ImageType == "CD") && d.Interface == "pflash" {
			hasCustomBIOS = true
		}
		external := d.ImageName == ""
		readonly := external
		if d.ReadOnly != nil {
			readonly = *d.ReadOnly
		}
		disk := checkpointDisk{ID: d.Identifier, Image: d.ImageName, Role: "disk"}
		if external || readonly || v.Backend == "qemu" && d.ImageType != "Disk" {
			disk.Role = "excluded external, read-only or non-disk drive"
			scope.Excluded = append(scope.Excluded, disk)
			continue
		}
		if d.Identifier == "" {
			return scope, fmt.Errorf("UTM included drive lacks identifier")
		}
		if err = validateCheckpointFile(r.BundlePath, d.ImageName, v.Backend == "qemu"); err != nil {
			return scope, err
		}
		scope.Included = append(scope.Included, disk)
	}
	if len(scope.Included) == 0 {
		return scope, fmt.Errorf("UTM VM has no eligible writable internal disks; attach an internal supported disk before capture")
	}
	// This matches the files UTM's backend includes without interpreting global
	// bookmarks or following an image path outside this VM's private Data folder.
	aux := []string{}
	if v.Backend == "qemu" && cfg.QEMU.UEFIBoot && !hasCustomBIOS {
		if _, err = os.Lstat(filepath.Join(r.BundlePath, "Data", "efi_vars.fd")); err == nil {
			aux = append(aux, "efi_vars.fd")
		}
	}
	if v.Backend == "apple" {
		aux = append(aux, cfg.System.Boot.EfiVariableStoragePath, cfg.System.MacPlatform.AuxiliaryStoragePath)
	}
	for _, name := range aux {
		if name == "" {
			continue
		}
		if err = validateCheckpointFile(r.BundlePath, name, v.Backend == "qemu"); err != nil {
			return scope, err
		}
		scope.Included = append(scope.Included, checkpointDisk{Image: name, Role: "firmware"})
	}
	return scope, nil
}
func validateCheckpointFile(bundle, name string, qcow bool) error {
	if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, "/\\\x00") {
		return fmt.Errorf("UTM snapshot image must be an internal bundle filename")
	}
	dataDir := filepath.Join(bundle, "Data")
	info, err := os.Lstat(dataDir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("UTM owned Data must be a real directory")
	}
	path := filepath.Join(dataDir, name)
	info, err = os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("UTM snapshot image %s must be an owned regular file", name)
	}
	if qcow {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		var magic [4]byte
		_, readErr := io.ReadFull(f, magic[:])
		closeErr := f.Close()
		if readErr != nil || string(magic[:]) != "QFI\xfb" {
			return fmt.Errorf("UTM stopped QEMU snapshots require qcow2 for every included image (%s)", name)
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}
