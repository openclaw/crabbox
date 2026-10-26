package hyperv

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveVMStoragePreservesUnrelatedFilesInVMNamedDirectory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	vhdDir := hypervVHDDir()
	const name = "crabbox-storage-1234"
	leaseDir := filepath.Join(vhdDir, name)
	if err := os.MkdirAll(leaseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(leaseDir, "operator-data.vhdx")
	if err := os.WriteFile(data, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	ownedVHD := filepath.Join(vhdDir, name+".vhdx")
	if err := os.WriteFile(ownedVHD, []byte("lease"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := testBackend(&recordingRunner{}).removeVMStorage(name, nil); err != nil {
		t.Fatalf("removeVMStorage: %v", err)
	}
	if _, err := os.Stat(data); err != nil {
		t.Fatalf("unrelated file in VM-named directory was removed: %v", err)
	}
	if _, err := os.Stat(ownedVHD); !os.IsNotExist(err) {
		t.Fatalf("lease VHDX still present: %v", err)
	}
}

func TestRemoveVMStorageRemovesEmptyVMNamedDirectory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	const name = "crabbox-storage-5678"
	leaseDir := filepath.Join(hypervVHDDir(), name)
	if err := os.MkdirAll(leaseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := testBackend(&recordingRunner{}).removeVMStorage(name, nil); err != nil {
		t.Fatalf("removeVMStorage: %v", err)
	}
	if _, err := os.Stat(leaseDir); !os.IsNotExist(err) {
		t.Fatalf("empty lease directory still present: %v", err)
	}
}
