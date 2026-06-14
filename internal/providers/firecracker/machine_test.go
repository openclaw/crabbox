//go:build linux

package firecracker

import "testing"

func TestFirecrackerDrivesBootRootFSBeforeCloudInit(t *testing.T) {
	drives := firecrackerDrives("/leases/rootfs.ext4", "/leases/cidata.iso")
	if len(drives) != 2 {
		t.Fatalf("drives len=%d want 2", len(drives))
	}
	if drives[0].PathOnHost == nil || *drives[0].PathOnHost != "/leases/rootfs.ext4" {
		t.Fatalf("root drive path=%v", drives[0].PathOnHost)
	}
	if drives[0].IsRootDevice == nil || !*drives[0].IsRootDevice {
		t.Fatalf("root drive IsRootDevice=%v want true", drives[0].IsRootDevice)
	}
	if drives[0].IsReadOnly == nil || *drives[0].IsReadOnly {
		t.Fatalf("root drive IsReadOnly=%v want false", drives[0].IsReadOnly)
	}
	if drives[1].PathOnHost == nil || *drives[1].PathOnHost != "/leases/cidata.iso" {
		t.Fatalf("cloud-init drive path=%v", drives[1].PathOnHost)
	}
	if drives[1].DriveID == nil || *drives[1].DriveID != "cidata" {
		t.Fatalf("cloud-init drive id=%v want cidata", drives[1].DriveID)
	}
	if drives[1].IsRootDevice == nil || *drives[1].IsRootDevice {
		t.Fatalf("cloud-init IsRootDevice=%v want false", drives[1].IsRootDevice)
	}
}
