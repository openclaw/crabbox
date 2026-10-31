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

func TestRemoveVMStorageRemovesAttachedForkDisksButKeepsUnattachedFiles(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	const name = "crabbox-storage-9abc"
	leaseDir := filepath.Join(hypervVHDDir(), name)
	if err := os.MkdirAll(leaseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	attached := filepath.Join(leaseDir, "copied-system.vhdx")
	unattached := filepath.Join(leaseDir, "operator-data.vhdx")
	attachedOperator := filepath.Join(leaseDir, "operator-attached.vhdx")
	for _, path := range []string{attached, unattached, attachedOperator} {
		if err := os.WriteFile(path, []byte("disk"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := recordForkDisks(leaseDir, []string{attached}); err != nil {
		t.Fatal(err)
	}
	if err := testBackend(&recordingRunner{}).removeVMStorage(name, []string{attached, attachedOperator}); err != nil {
		t.Fatalf("removeVMStorage: %v", err)
	}
	if _, err := os.Stat(attached); !os.IsNotExist(err) {
		t.Fatalf("imported fork disk still present: %v", err)
	}
	for _, keep := range []string{unattached, attachedOperator} {
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("operator disk %s was removed: %v", keep, err)
		}
	}
}

func TestRemoveVMStorageDeletesManifestDisksWhenDiskQueryUnavailable(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	const name = "crabbox-storage-7aa1"
	leaseDir := filepath.Join(hypervVHDDir(), name)
	if err := os.MkdirAll(leaseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	imported := filepath.Join(leaseDir, "copied-system.vhdx")
	operator := filepath.Join(leaseDir, "operator-data.vhdx")
	for _, p := range []string{imported, operator} {
		if err := os.WriteFile(p, []byte("disk"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := recordForkDisks(leaseDir, []string{imported}); err != nil {
		t.Fatal(err)
	}
	if err := testBackend(&recordingRunner{}).removeVMStorage(name, nil); err != nil {
		t.Fatalf("removeVMStorage: %v", err)
	}
	if _, err := os.Stat(imported); !os.IsNotExist(err) {
		t.Fatalf("manifest disk leaked: %v", err)
	}
	if _, err := os.Stat(operator); err != nil {
		t.Fatalf("operator disk removed: %v", err)
	}
}

func TestRemoveVMStoragePreservesAttachedDiskWithoutForkMarker(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	const name = "crabbox-storage-5def"
	leaseDir := filepath.Join(hypervVHDDir(), name)
	if err := os.MkdirAll(leaseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(leaseDir, "operator-data.vhdx")
	if err := os.WriteFile(data, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := testBackend(&recordingRunner{}).removeVMStorage(name, []string{data}); err != nil {
		t.Fatalf("removeVMStorage: %v", err)
	}
	if _, err := os.Stat(data); err != nil {
		t.Fatalf("operator-attached disk in an ordinary lease dir was removed: %v", err)
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
