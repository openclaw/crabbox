package runtimeartifact

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverRelocatedAndLinkedPack(t *testing.T) {
	dir := t.TempDir()
	controller := filepath.Join(dir, "crabbox")
	writeFile(t, controller, []byte("controller fixture"))
	pack := filepath.Join(dir, "crabbox-runtime")
	if err := os.Mkdir(pack, 0700); err != nil {
		t.Fatal(err)
	}
	m := manifest{SchemaVersion: 1, ProtocolVersion: "test", ControllerSHA256: digest([]byte("controller fixture")), Artifacts: []entry{{"linux", "amd64", "linux-amd64", 1, digest([]byte("a"))}}}
	writeFile(t, filepath.Join(pack, "manifest.json"), manifestBytes(t, m))
	moved := filepath.Join(t.TempDir(), "relocated")
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	moved, err := filepath.EvalSymlinks(moved)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "crabbox")
	if err := os.Symlink(filepath.Join(moved, "crabbox"), link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	for _, path := range []string{filepath.Join(moved, "crabbox"), link} {
		real, manifest, err := Discover(path, "")
		if err != nil {
			t.Fatal(err)
		}
		if real != filepath.Join(moved, "crabbox") || manifest != filepath.Join(moved, "crabbox-runtime", "manifest.json") {
			t.Fatalf("wrong discovery: %s %s", real, manifest)
		}
		if _, err := OpenLocalSet(context.Background(), manifest, real, "test"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDiscoverAbsentIncompleteAndOverride(t *testing.T) {
	dir := t.TempDir()
	controller := filepath.Join(dir, "crabbox")
	writeFile(t, controller, []byte("controller"))
	if _, m, err := Discover(controller, ""); err != nil || m != "" {
		t.Fatalf("CLI only: %s %v", m, err)
	}
	pack := filepath.Join(dir, "crabbox-runtime")
	if err := os.Mkdir(pack, 0700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Discover(controller, ""); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("incomplete pack: %v", err)
	}
	override := filepath.Join(t.TempDir(), "explicit.json")
	if _, m, err := Discover(controller, override); err != nil || m != override {
		t.Fatalf("override: %s %v", m, err)
	}
	writeFile(t, filepath.Join(pack, "manifest.json"), []byte("malformed"))
	real, m, err := Discover(controller, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLocalSet(t.Context(), m, real, "test"); err == nil {
		t.Fatal("malformed manifest accepted")
	}
}
