package applevzhelper

import (
	"path/filepath"
	"testing"
)

func TestProtocolPathsAndStatusHelpers(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "tmp", "crabbox-apple-vz")
	name := "crabbox-cbx123-demo"

	if got := InstancesDir(root); got != filepath.Join(root, "instances") {
		t.Fatalf("InstancesDir=%q", got)
	}
	if got := CacheDir(root); got != filepath.Join(root, "cache") {
		t.Fatalf("CacheDir=%q", got)
	}
	if got := DownloadsDir(root); got != filepath.Join(root, "cache", "downloads") {
		t.Fatalf("DownloadsDir=%q", got)
	}
	if got := ImagesDir(root); got != filepath.Join(root, "cache", "images") {
		t.Fatalf("ImagesDir=%q", got)
	}
	if got := InstanceDir(root, name); got != filepath.Join(root, "instances", name) {
		t.Fatalf("InstanceDir=%q", got)
	}
	if got := MetadataPath(root, name); got != filepath.Join(root, "instances", name, MetadataFileName) {
		t.Fatalf("MetadataPath=%q", got)
	}
	for label, path := range map[string]string{
		"disk":    DiskPath(root, name),
		"seed":    SeedPath(root, name),
		"efi":     EFIPath(root, name),
		"console": ConsoleLogPath(root, name),
	} {
		if path == "" {
			t.Fatalf("%s path is empty", label)
		}
	}
	if !IsRunningStatus(StatusStarting) || !IsRunningStatus(StatusRunning) {
		t.Fatal("starting and running should be active states")
	}
	if IsRunningStatus(StatusStopped) || IsRunningStatus("") {
		t.Fatal("stopped and empty should not be active states")
	}
}
