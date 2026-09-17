package runtimeartifact

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type unopenedDevelopmentSource struct{ opens int }

func (s *unopenedDevelopmentSource) Open(context.Context, Target) (*Artifact, error) {
	s.opens++
	return nil, errors.New("test source must remain unopened during selection")
}

func TestResolveSourcePackagingPolicy(t *testing.T) {
	fs := Requirement{Capability: Filesystem, ProtocolVersion: "1", BuildID: digest([]byte("selected helper source"))}
	supervisor := Requirement{Capability: Supervisor, ProtocolVersion: "CBX-REMOTE-1"}
	for _, test := range []struct {
		name, kind, pack string
		required         Requirement
		wantSource       bool
		wantError        bool
	}{
		{"development-filesystem", "development", "", fs, true, false},
		{"cli-only-supervisor", "development", "", supervisor, false, false},
		{"missing-release-filesystem", "complete", "", fs, false, true},
		{"missing-release-supervisor", "complete", "", supervisor, false, true},
		{"unknown-distribution", "unknown", "", fs, false, true},
		{"incomplete-development-pack", "development", "incomplete", fs, false, true},
		{"malformed-development-pack", "development", "malformed", fs, false, true},
		{"explicit-missing-pack", "development", "explicit", fs, false, true},
		{"legacy-supervisor-pack", "complete", "v1", supervisor, true, false},
		{"legacy-pack-no-filesystem", "development", "v1", fs, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			controller := filepath.Join(dir, "crabbox")
			controllerBytes := []byte("controller fixture")
			writeFile(t, controller, controllerBytes)
			explicit := ""
			if test.pack == "explicit" {
				explicit = filepath.Join(dir, "missing.json")
			} else if test.pack != "" {
				pack := filepath.Join(dir, "crabbox-runtime")
				if err := os.Mkdir(pack, 0700); err != nil {
					t.Fatal(err)
				}
				if test.pack == "malformed" {
					writeFile(t, filepath.Join(pack, "manifest.json"), []byte("incomplete manifest"))
				} else if test.pack == "v1" {
					m := manifest{SchemaVersion: 1, ProtocolVersion: supervisor.ProtocolVersion, ControllerSHA256: digest(controllerBytes), Artifacts: []entry{{"linux", "amd64", "linux-amd64", 1, digest([]byte("fixture"))}}}
					writeFile(t, filepath.Join(pack, "manifest.json"), manifestBytes(t, m))
				}
			}
			development := &unopenedDevelopmentSource{}
			selected, err := resolveSource(t.Context(), controller, explicit, test.kind, test.required, development)
			if (err != nil) != test.wantError || (selected != nil) != test.wantSource {
				t.Fatalf("selected=%T error=%v", selected, err)
			}
			if development.opens != 0 {
				t.Fatal("selection invoked the development compiler")
			}
		})
	}
}

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
