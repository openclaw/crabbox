package hyperv

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

// Claim labels recording where a lease's files were created. Release and cleanup
// read them instead of current configuration, so changing hyperv.storage never
// strands an existing lease's disks.
const (
	storageVHDDirLabel = "vhd_dir"
	storageVMDirLabel  = "vm_dir"
)

// hypervStorage locates one lease's files: the root differencing disk,
// automatic checkpoint disks and checkpoint-fork disks under vhdDir, and the VM
// configuration under vmDir.
type hypervStorage struct {
	vhdDir string
	vmDir  string
}

// legacyHyperVStorage is where leases were created before storage was
// configurable or recorded.
func legacyHyperVStorage() hypervStorage {
	return hypervStorage{vhdDir: hypervVHDDir(), vmDir: hypervVMDir()}
}

func hypervStorageUnderRoot(root string) hypervStorage {
	return hypervStorage{
		vhdDir: filepath.Join(root, "Virtual Hard Disks"),
		vmDir:  filepath.Join(root, "Virtual Machines"),
	}
}

func hypervVHDDir() string {
	return filepath.Join(hypervProfileRoot(), "Hyper-V", "Virtual Hard Disks")
}

func hypervVMDir() string {
	return filepath.Join(hypervProfileRoot(), "Hyper-V", "Virtual Machines")
}

func hypervProfileRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return `C:\Users\Public`
	}
	return home
}

// storageForCreate resolves where a new lease's files are created.
func (b *backend) storageForCreate(_ context.Context, cfg core.Config) (hypervStorage, error) {
	root := strings.TrimSpace(cfg.HyperV.Storage)
	if root == "" {
		return legacyHyperVStorage(), nil
	}
	if !filepath.IsAbs(root) {
		return hypervStorage{}, core.Exit(2, "hyperv.storage must be an absolute host directory, got %q", cfg.HyperV.Storage)
	}
	return hypervStorageUnderRoot(filepath.Clean(root)), nil
}

func (s hypervStorage) recordLabels(labels map[string]string) {
	labels[storageVHDDirLabel] = s.vhdDir
	labels[storageVMDirLabel] = s.vmDir
}

// leaseStorage returns the storage recorded on a lease claim. A claim without
// storage labels predates them and used the profile directories.
func leaseStorage(labels map[string]string) (hypervStorage, error) {
	vhdDir, vmDir := labels[storageVHDDirLabel], labels[storageVMDirLabel]
	if vhdDir == "" && vmDir == "" {
		return legacyHyperVStorage(), nil
	}
	if !validRecordedStorageDir(vhdDir) || !validRecordedStorageDir(vmDir) {
		return hypervStorage{}, core.Exit(2, "hyperv lease records invalid storage directories %s=%q %s=%q; refusing to touch its files", storageVHDDirLabel, vhdDir, storageVMDirLabel, vmDir)
	}
	return hypervStorage{vhdDir: vhdDir, vmDir: vmDir}, nil
}

// Crabbox records clean absolute directories, so anything else is not its own.
func validRecordedStorageDir(dir string) bool {
	return dir != "" && filepath.IsAbs(dir) && dir == filepath.Clean(dir)
}
