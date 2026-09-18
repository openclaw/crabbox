package runtimeartifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func buildFixture(t *testing.T, arch, pkg string, flags ...string) []byte {
	t.Helper()
	return buildTargetFixture(t, Target{"linux", arch}, pkg, flags...)
}

func buildTargetFixture(t *testing.T, target Target, pkg string, flags ...string) []byte {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), []byte("module github.com/openclaw/crabbox\n\ngo 1.26\n"))
	if err := os.MkdirAll(filepath.Join(dir, pkg), 0700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, pkg, "main.go"), []byte("package main\nfunc main() {}\n"))
	args := []string{"build", "-trimpath", "-o", filepath.Join(dir, "fixture")}
	args = append(args, flags...)
	args = append(args, "./"+pkg)
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS="+target.OS, "GOARCH="+target.Arch, "CGO_ENABLED=0", "GOTOOLCHAIN=local", "GOWORK=off", "GOFLAGS=", "GOPROXY=off", "GOSUMDB=off")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build owned fixture: %v\n%s", err, output)
	}
	data, err := os.ReadFile(filepath.Join(dir, "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeFile(t *testing.T, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestFilesystemArtifactSource(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		for _, arch := range []string{"amd64", "arm64"} {
			t.Run(goos+"_"+arch, func(t *testing.T) {
				target := Target{goos, arch}
				data := buildTargetFixture(t, target, "cmd/crabbox-runner")
				want := bytes.Clone(data)
				buildID := digest([]byte("owned development source"))
				first, err := NewFilesystemBytes(t.Context(), data, target, buildID, "1")
				if err != nil {
					t.Fatal(err)
				}
				defer first.Close()
				second, err := NewFilesystemBytes(t.Context(), data, target, buildID, "1")
				if err != nil {
					t.Fatal(err)
				}
				defer second.Close()
				data[0] ^= 0xff // The producer still owns its original buffer.
				if got := first.Identity(); got != (Identity{Target: target, ProtocolVersion: "1", Size: int64(len(want)), SHA256: digest(want), Capability: Filesystem, BuildID: buildID}) {
					t.Fatalf("filesystem identity: %+v", got)
				}
				got, err := io.ReadAll(first)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("first owned stream: %v", err)
				}
				if err := first.Close(); err != nil {
					t.Fatal(err)
				}
				got, err = io.ReadAll(second)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("independent stream: %v", err)
				}
			})
		}
	}
}

func TestFilesystemArtifactSourceCancellation(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := fmt.Errorf("operator canceled development build")
	cancel(cause)
	if artifact, err := NewFilesystemBytes(ctx, nil, Target{"linux", "amd64"}, "", ""); artifact != nil || !errors.Is(err, cause) {
		t.Fatalf("canceled admission: artifact=%v error=%v", artifact, err)
	}
}

func manifestBytes(t *testing.T, m manifest) []byte {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestOpenLocal(t *testing.T) {
	amd64 := buildFixture(t, "amd64", "cmd/crabbox-runtime")
	arm64 := buildFixture(t, "arm64", "cmd/crabbox-runtime")
	wrongPackage := buildFixture(t, "amd64", "cmd/other")
	fullCLI := buildFixture(t, "amd64", "cmd/crabbox")
	controller := []byte("owned controller fixture, display version dev")
	makePack := func(t *testing.T) (string, manifest) {
		t.Helper()
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "controller"), controller)
		writeFile(t, filepath.Join(dir, "amd64"), amd64)
		writeFile(t, filepath.Join(dir, "arm64"), arm64)
		m := manifest{1, "CBX-REMOTE-1", digest(controller), []entry{
			{"linux", "amd64", "amd64", int64(len(amd64)), digest(amd64)},
			{"linux", "arm64", "arm64", int64(len(arm64)), digest(arm64)},
		}}
		return dir, m
	}
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run("valid_"+arch, func(t *testing.T) {
			dir, m := makePack(t)
			writeFile(t, filepath.Join(dir, "runtime.json"), manifestBytes(t, m))
			a, err := OpenLocal(context.Background(), filepath.Join(dir, "runtime.json"), filepath.Join(dir, "controller"), Target{"linux", arch}, "CBX-REMOTE-1")
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			want := amd64
			if arch == "arm64" {
				want = arm64
			}
			if a.Identity() != (Identity{Target: Target{"linux", arch}, ProtocolVersion: "CBX-REMOTE-1", Size: int64(len(want)), SHA256: digest(want), Capability: Supervisor}) {
				t.Fatalf("unexpected identity: %+v", a.Identity())
			}
			got, err := io.ReadAll(a)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("stream mismatch: %v", err)
			}
			if _, err := a.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			got, err = io.ReadAll(a)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("replayed stream mismatch: %v", err)
			}
		})
	}
	cases := []struct {
		name, want string
		change     func(*testing.T, string, *manifest)
	}{
		{"controller_changed", "controller SHA-256 mismatch", func(t *testing.T, dir string, m *manifest) {
			writeFile(t, filepath.Join(dir, "controller"), []byte("different controller, same display version dev"))
		}},
		{"missing_target", "artifact missing", func(t *testing.T, dir string, m *manifest) { m.Artifacts = m.Artifacts[1:] }},
		{"duplicate_target", "duplicate artifact target", func(t *testing.T, dir string, m *manifest) { m.Artifacts = append(m.Artifacts, m.Artifacts[0]) }},
		{"unsupported_target", "unsupported artifact target", func(t *testing.T, dir string, m *manifest) { m.Artifacts[1].Arch = "386" }},
		{"schema", "unsupported runtime schema", func(t *testing.T, dir string, m *manifest) { m.SchemaVersion = 2 }},
		{"protocol", "runtime protocol mismatch", func(t *testing.T, dir string, m *manifest) { m.ProtocolVersion = "CBX-REMOTE-2" }},
		{"empty", "no artifacts", func(t *testing.T, dir string, m *manifest) { m.Artifacts = []entry{} }},
		{"controller_digest", "invalid controller SHA-256", func(t *testing.T, dir string, m *manifest) { m.ControllerSHA256 = strings.ToUpper(m.ControllerSHA256) }},
		{"size", "artifact size mismatch", func(t *testing.T, dir string, m *manifest) { m.Artifacts[0].Size++ }},
		{"digest", "artifact SHA-256 mismatch", func(t *testing.T, dir string, m *manifest) { m.Artifacts[0].SHA256 = strings.Repeat("0", 64) }},
		{"negative_size", "invalid artifact size", func(t *testing.T, dir string, m *manifest) { m.Artifacts[0].Size = -1 }},
		{"missing_file", "inspect artifact path", func(t *testing.T, dir string, m *manifest) { m.Artifacts[0].Path = "missing" }},
		{"wrong_arch", "ELF architecture", func(t *testing.T, dir string, m *manifest) {
			m.Artifacts[0] = entry{"linux", "amd64", "arm64", int64(len(arm64)), digest(arm64)}
		}},
		{"wrong_package", "Go package mismatch", func(t *testing.T, dir string, m *manifest) {
			writeFile(t, filepath.Join(dir, "amd64"), wrongPackage)
			m.Artifacts[0].Size = int64(len(wrongPackage))
			m.Artifacts[0].SHA256 = digest(wrongPackage)
		}},
		{"full_cli_package", "Go package mismatch", func(t *testing.T, dir string, m *manifest) {
			writeFile(t, filepath.Join(dir, "amd64"), fullCLI)
			m.Artifacts[0].Size = int64(len(fullCLI))
			m.Artifacts[0].SHA256 = digest(fullCLI)
		}},
		{"not_elf", "read ELF", func(t *testing.T, dir string, m *manifest) {
			writeFile(t, filepath.Join(dir, "amd64"), controller)
			m.Artifacts[0].Size = int64(len(controller))
			m.Artifacts[0].SHA256 = digest(controller)
		}},
		{"directory", "file must be regular", func(t *testing.T, dir string, m *manifest) {
			if err := os.Mkdir(filepath.Join(dir, "directory"), 0700); err != nil {
				t.Fatal(err)
			}
			m.Artifacts[0].Path = "directory"
		}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			dir, m := makePack(t)
			tt.change(t, dir, &m)
			writeFile(t, filepath.Join(dir, "runtime.json"), manifestBytes(t, m))
			a, err := OpenLocal(context.Background(), filepath.Join(dir, "runtime.json"), filepath.Join(dir, "controller"), Target{"linux", "amd64"}, "CBX-REMOTE-1")
			if a != nil {
				a.Close()
				t.Fatal("returned artifact for invalid pack")
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want %q", err, tt.want)
			}
		})
	}
	t.Run("relocated_pack", func(t *testing.T) {
		dir, m := makePack(t)
		writeFile(t, filepath.Join(dir, "runtime.json"), manifestBytes(t, m))
		moved := filepath.Join(t.TempDir(), "moved")
		if err := os.Rename(dir, moved); err != nil {
			t.Fatal(err)
		}
		a, err := OpenLocal(context.Background(), filepath.Join(moved, "runtime.json"), filepath.Join(moved, "controller"), Target{"linux", "amd64"}, "CBX-REMOTE-1")
		if err != nil {
			t.Fatal(err)
		}
		a.Close()
	})
	t.Run("snapshot_both_targets", func(t *testing.T) {
		dir, m := makePack(t)
		name := filepath.Join(dir, "runtime.json")
		writeFile(t, name, manifestBytes(t, m))
		set, err := OpenLocalSet(t.Context(), name, filepath.Join(dir, "controller"), "CBX-REMOTE-1")
		if err != nil {
			t.Fatal(err)
		}
		for _, arch := range []string{"amd64", "arm64"} {
			a, err := set.Open(t.Context(), Target{"linux", arch})
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(a)
			a.Close()
			want := amd64
			if arch == "arm64" {
				want = arm64
			}
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("snapshot %s stream: %v", arch, err)
			}
		}
	})
	t.Run("snapshot_ignores_manifest_rewrite", func(t *testing.T) {
		dir, m := makePack(t)
		name := filepath.Join(dir, "runtime.json")
		original := m
		original.Artifacts = m.Artifacts[:1]
		writeFile(t, name, manifestBytes(t, original))
		set, err := OpenLocalSet(t.Context(), name, filepath.Join(dir, "controller"), "CBX-REMOTE-1")
		if err != nil {
			t.Fatal(err)
		}
		// The new manifest adds arm64 and changes the existing amd64 mapping.
		m.Artifacts[0].Path = "new-amd64-location"
		writeFile(t, name, manifestBytes(t, m))
		a, err := set.Open(t.Context(), Target{"linux", "amd64"})
		if err != nil {
			t.Fatalf("snapshot followed rewritten path: %v", err)
		}
		a.Close()
		if a, err := set.Open(t.Context(), Target{"linux", "arm64"}); a != nil || err == nil || !strings.Contains(err.Error(), "artifact missing") {
			if a != nil {
				a.Close()
			}
			t.Fatalf("snapshot selected added target: %v", err)
		}
		fresh, err := OpenLocal(t.Context(), name, filepath.Join(dir, "controller"), Target{"linux", "arm64"}, "CBX-REMOTE-1")
		if err != nil {
			t.Fatalf("fresh open did not see new target: %v", err)
		}
		fresh.Close()
	})
	t.Run("snapshot_rechecks_artifact_bytes", func(t *testing.T) {
		dir, m := makePack(t)
		name := filepath.Join(dir, "runtime.json")
		writeFile(t, name, manifestBytes(t, m))
		set, err := OpenLocalSet(t.Context(), name, filepath.Join(dir, "controller"), "CBX-REMOTE-1")
		if err != nil {
			t.Fatal(err)
		}
		a, err := set.Open(t.Context(), Target{"linux", "amd64"})
		if err != nil {
			t.Fatal(err)
		}
		a.Close()
		changed := bytes.Clone(amd64)
		changed[len(changed)-1] ^= 1
		writeFile(t, filepath.Join(dir, "amd64"), changed)
		if a, err := set.Open(t.Context(), Target{"linux", "amd64"}); a != nil || err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
			if a != nil {
				a.Close()
			}
			t.Fatalf("snapshot did not recheck artifact: %v", err)
		}
	})
	t.Run("snapshot_binds_controller_once", func(t *testing.T) {
		dir, m := makePack(t)
		name, controllerName := filepath.Join(dir, "runtime.json"), filepath.Join(dir, "controller")
		writeFile(t, name, manifestBytes(t, m))
		set, err := OpenLocalSet(t.Context(), name, controllerName, "CBX-REMOTE-1")
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, controllerName, []byte("another finalized controller"))
		a, err := set.Open(t.Context(), Target{"linux", "amd64"})
		if err != nil {
			t.Fatalf("existing controller-bound operation changed: %v", err)
		}
		a.Close()
		if fresh, err := OpenLocalSet(t.Context(), name, controllerName, "CBX-REMOTE-1"); fresh != nil || err == nil || !strings.Contains(err.Error(), "controller SHA-256 mismatch") {
			t.Fatalf("new operation accepted wrong controller: %v", err)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := OpenLocal(ctx, "unused", "unused", Target{"linux", "amd64"}, "CBX-REMOTE-1"); err != context.Canceled {
			t.Fatalf("got %v", err)
		}
	})
}

func TestManifestValidation(t *testing.T) {
	base := `{"schemaVersion":1,"protocolVersion":"CBX-REMOTE-1","controllerSha256":"` + strings.Repeat("a", 64) + `","artifacts":[{"os":"linux","arch":"amd64","path":"bin/crabbox","size":12,"sha256":"` + strings.Repeat("b", 64) + `"}]}`
	cases := []struct{ name, data, want string }{
		{"unknown", strings.Replace(base, `"schemaVersion":1`, `"schemaVersion":1,"extra":1`, 1), "unknown field"},
		{"case", strings.Replace(base, `"schemaVersion"`, `"SchemaVersion"`, 1), "missing required field"},
		{"duplicate", strings.Replace(base, `"schemaVersion":1`, `"schemaVersion":1,"schemaVersion":1`, 1), "duplicate field"},
		{"nested_duplicate", strings.Replace(base, `"os":"linux"`, `"os":"linux","os":"linux"`, 1), "duplicate field"},
		{"nested_unknown", strings.Replace(base, `"os":"linux"`, `"os":"linux","extra":true`, 1), "unknown field"},
		{"trailing", base + ` {}`, "trailing"},
		{"missing", strings.Replace(base, `"schemaVersion":1,`, ``, 1), "missing required field"},
		{"null", strings.Replace(base, `"size":12`, `"size":null`, 1), "missing required field"},
		{"fraction", strings.Replace(base, `"size":12`, `"size":1.5`, 1), "cannot unmarshal"},
		{"large", strings.Repeat(" ", maxManifestSize) + base, "exceeds"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			name := filepath.Join(t.TempDir(), "manifest.json")
			writeFile(t, name, []byte(tt.data))
			if _, err := readManifest(name); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want %q", err, tt.want)
			}
		})
	}
}

func TestPackPaths(t *testing.T) {
	for _, p := range []string{"", ".", "../binary", "/binary", "bin/../binary", "bin//binary", "./binary", `bin\binary`, "C:/binary"} {
		if validPackPath(p) {
			t.Errorf("accepted invalid pack path %q", p)
		}
	}
	if !validPackPath("bin/linux-arm64/crabbox") {
		t.Fatal("rejected relative pack path")
	}
}

func TestExecutableMetadataFormats(t *testing.T) {
	for _, osName := range []string{"linux", "darwin", "windows"} {
		for _, arch := range []string{"amd64", "arm64"} {
			target := Target{osName, arch}
			t.Run(osName+"/"+arch, func(t *testing.T) {
				data := buildTargetFixture(t, target, "cmd/crabbox-runtime")
				if err := inspectExecutable(t.Context(), bytes.NewReader(data), target); err != nil {
					t.Fatal(err)
				}
				other := target
				if arch == "amd64" {
					other.Arch = "arm64"
				} else {
					other.Arch = "amd64"
				}
				if err := inspectExecutable(t.Context(), bytes.NewReader(data), other); err == nil || !strings.Contains(err.Error(), "architecture does not match") {
					t.Fatalf("architecture mismatch: %v", err)
				}
				other = Target{"linux", arch}
				if osName == "linux" {
					other.OS = "darwin"
				}
				if err := inspectExecutable(t.Context(), bytes.NewReader(data), other); err == nil || !strings.Contains(err.Error(), "read ") {
					t.Fatalf("format mismatch: %v", err)
				}
			})
		}
	}
}

func TestExecutableMetadataPackageMismatch(t *testing.T) {
	for _, osName := range []string{"darwin", "windows"} {
		t.Run(osName, func(t *testing.T) {
			target := Target{osName, "amd64"}
			data := buildTargetFixture(t, target, "cmd/other")
			if err := inspectExecutable(t.Context(), bytes.NewReader(data), target); err == nil || !strings.Contains(err.Error(), "Go package mismatch") {
				t.Fatalf("package mismatch: %v", err)
			}
		})
	}
}

func TestBuildInfoPolicy(t *testing.T) {
	valid := func() *debug.BuildInfo {
		return &debug.BuildInfo{Path: runtimePackage, Main: debug.Module{Version: "(devel)"}, Settings: []debug.BuildSetting{{Key: "GOOS", Value: "linux"}, {Key: "GOARCH", Value: "amd64"}, {Key: "CGO_ENABLED", Value: "0"}, {Key: "vcs.modified", Value: "true"}}}
	}
	if err := validateBuildInfo(valid(), Target{"linux", "amd64"}); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name   string
		change func(*debug.BuildInfo)
		want   string
	}{
		{"test_binary", func(b *debug.BuildInfo) { b.Path += ".test" }, "Go package mismatch"},
		{"cgo", func(b *debug.BuildInfo) { b.Settings[2].Value = "1" }, "CGO_ENABLED"},
		{"os", func(b *debug.BuildInfo) { b.Settings[0].Value = "freebsd" }, "GOOS"},
		{"arch", func(b *debug.BuildInfo) { b.Settings[1].Value = "arm64" }, "GOARCH"},
		{"missing", func(b *debug.BuildInfo) { b.Settings = nil }, "Go build setting"},
		{"duplicate", func(b *debug.BuildInfo) { b.Settings = append(b.Settings, b.Settings[0]) }, "duplicate Go build setting"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b := valid()
			tt.change(b)
			if err := validateBuildInfo(b, Target{"linux", "amd64"}); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want %q", err, tt.want)
			}
		})
	}
}

func TestMetadataReadLimit(t *testing.T) {
	r := &metadataReader{context.Background(), bytes.NewReader([]byte("fixture")), 3}
	if _, err := r.ReadAt(make([]byte, 4), 0); err == nil {
		t.Fatal("expected read limit error")
	}
}

func TestELFPolicy(t *testing.T) {
	valid := func() *elf.File {
		return &elf.File{FileHeader: elf.FileHeader{Class: elf.ELFCLASS64, Data: elf.ELFDATA2LSB, Machine: elf.EM_X86_64, Type: elf.ET_EXEC, OSABI: elf.ELFOSABI_NONE}}
	}
	for _, typ := range []elf.Type{elf.ET_EXEC, elf.ET_DYN} {
		f := valid()
		f.Type = typ
		if err := validateELF(f, Target{"linux", "amd64"}); err != nil {
			t.Fatalf("valid standalone ELF type %s: %v", typ, err)
		}
	}
	for _, tt := range []struct {
		name, want string
		change     func(*elf.File)
	}{
		{"interpreter", "external interpreter", func(f *elf.File) { f.Progs = []*elf.Prog{{ProgHeader: elf.ProgHeader{Type: elf.PT_INTERP}}} }},
		{"object", "not an executable", func(f *elf.File) { f.Type = elf.ET_REL }},
		{"architecture", "architecture", func(f *elf.File) { f.Machine = elf.EM_AARCH64 }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := valid()
			tt.change(f)
			if err := validateELF(f, Target{"linux", "amd64"}); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want %q", err, tt.want)
			}
		})
	}
}

func TestMarshalLocal(t *testing.T) {
	amd64 := buildFixture(t, "amd64", "cmd/crabbox-runtime")
	arm64 := buildFixture(t, "arm64", "cmd/crabbox-runtime")
	controller := []byte("finalized controller fixture")
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "controller"), controller)
	if err := os.Mkdir(filepath.Join(dir, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "bin", "amd64"), amd64)
	writeFile(t, filepath.Join(dir, "bin", "arm64"), arm64)
	inputs := []ArtifactInput{
		{Target{"linux", "amd64"}, "bin/amd64"},
		{Target{"linux", "arm64"}, "bin/arm64"},
	}
	data, err := MarshalLocal(context.Background(), dir, filepath.Join(dir, "controller"), inputs, "CBX-REMOTE-1")
	if err != nil {
		t.Fatal(err)
	}
	reversed := []ArtifactInput{inputs[1], inputs[0]}
	other, err := MarshalLocal(context.Background(), dir, filepath.Join(dir, "controller"), reversed, "CBX-REMOTE-1")
	if err != nil || !bytes.Equal(other, data) {
		t.Fatalf("manifest must be independent of input ordering: %v", err)
	}
	if reversed[0] != inputs[1] {
		t.Fatal("modified caller's input ordering")
	}
	writeFile(t, filepath.Join(dir, "runtime.json"), data)
	m, err := readManifest(filepath.Join(dir, "runtime.json"))
	if err != nil {
		t.Fatal(err)
	}
	if m.ControllerSHA256 != digest(controller) {
		t.Fatalf("controller digest: got %s", m.ControllerSHA256)
	}
	for _, input := range inputs {
		a, err := OpenLocal(context.Background(), filepath.Join(dir, "runtime.json"), filepath.Join(dir, "controller"), input.Target, "CBX-REMOTE-1")
		if err != nil {
			t.Fatal(err)
		}
		got, readErr := io.ReadAll(a)
		a.Close()
		want := amd64
		if input.Target.Arch == "arm64" {
			want = arm64
		}
		if readErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("produced manifest roundtrip for %s: %v", input.Target.Arch, readErr)
		}
	}
	for _, tt := range []struct {
		name, protocol, want string
		inputs               []ArtifactInput
	}{
		{"empty_protocol", "", "protocol must not be empty", inputs},
		{"empty_inputs", "CBX-REMOTE-1", "no artifacts", nil},
		{"duplicate_target", "CBX-REMOTE-1", "duplicate artifact target", []ArtifactInput{inputs[0], inputs[0]}},
		{"unsupported_target", "CBX-REMOTE-1", "unsupported artifact target", []ArtifactInput{{Target{"linux", "386"}, "bin/amd64"}}},
		{"absolute_path", "CBX-REMOTE-1", "invalid artifact path", []ArtifactInput{{Target{"linux", "amd64"}, filepath.Join(dir, "bin", "amd64")}}},
		{"missing_file", "CBX-REMOTE-1", "inspect artifact path", []ArtifactInput{{Target{"linux", "amd64"}, "missing"}}},
		{"wrong_architecture", "CBX-REMOTE-1", "ELF architecture", []ArtifactInput{{Target{"linux", "amd64"}, "bin/arm64"}}},
		{"not_executable", "CBX-REMOTE-1", "read ELF", []ArtifactInput{{Target{"linux", "amd64"}, "controller"}}},
		{"oversized_manifest", strings.Repeat("p", maxManifestSize), "manifest exceeds", inputs},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data, err := MarshalLocal(context.Background(), dir, filepath.Join(dir, "controller"), tt.inputs, tt.protocol)
			if data != nil || err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %d bytes and error %v, want %q", len(data), err, tt.want)
			}
		})
	}
	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := MarshalLocal(ctx, dir, filepath.Join(dir, "controller"), inputs, "CBX-REMOTE-1"); err != context.Canceled {
			t.Fatalf("got %v", err)
		}
	})
	moved := filepath.Join(t.TempDir(), "relocated")
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	a, err := OpenLocal(context.Background(), filepath.Join(moved, "runtime.json"), filepath.Join(moved, "controller"), inputs[0].Target, "CBX-REMOTE-1")
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
}

func TestCapabilityManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	controller := filepath.Join(dir, "controller")
	writeFile(t, controller, []byte("finalized controller"))
	buildID := digest([]byte("filesystem source"))
	required := Requirement{Filesystem, "1", buildID}
	var inputs []CapabilityInput
	for _, goos := range []string{"linux", "darwin", "windows"} {
		for _, arch := range []string{"amd64", "arm64"} {
			target := Target{goos, arch}
			name := goos + "-" + arch
			writeFile(t, filepath.Join(dir, name), buildTargetFixture(t, target, "cmd/crabbox-runtime"))
			claims := []CapabilityClaim{{Filesystem, "1", buildID}}
			if goos == "linux" {
				claims = append(claims, CapabilityClaim{Supervisor, "CBX-REMOTE-1", ""})
			}
			inputs = append(inputs, CapabilityInput{target, name, claims})
		}
	}
	data, err := MarshalCapabilities(t.Context(), dir, controller, inputs)
	if err != nil {
		t.Fatal(err)
	}
	again, err := MarshalCapabilities(t.Context(), dir, controller, inputs)
	if err != nil || !bytes.Equal(data, again) {
		t.Fatalf("deterministic manifest: %v", err)
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	writeFile(t, manifestPath, data)
	set, err := OpenCapabilitySet(t.Context(), manifestPath, controller, required)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range inputs {
		a, err := set.Open(t.Context(), in.Target)
		if err != nil {
			t.Fatal(err)
		}
		got := a.Identity()
		if got.Capability != Filesystem || got.BuildID != buildID || got.ProtocolVersion != "1" || got.Target != in.Target {
			t.Fatalf("identity: %+v", got)
		}
		contents, err := io.ReadAll(a)
		a.Close()
		if err != nil || digest(contents) != got.SHA256 {
			t.Fatalf("validated stream: %v", err)
		}
	}
	supervisor, err := OpenCapabilitySet(t.Context(), manifestPath, controller, Requirement{Supervisor, "CBX-REMOTE-1", ""})
	if err != nil {
		t.Fatal(err)
	}
	a, err := supervisor.Open(t.Context(), Target{"linux", "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	if _, err := supervisor.Open(t.Context(), Target{"darwin", "amd64"}); err == nil {
		t.Fatal("non-Linux supervisor admitted")
	}
	if _, err := OpenCapabilitySet(t.Context(), manifestPath, controller, Requirement{Filesystem, "1", digest([]byte("different source"))}); err == nil {
		t.Fatal("wrong fingerprint admitted")
	}
	if _, err := OpenLocalSet(t.Context(), manifestPath, controller, "1"); err == nil {
		t.Fatal("v2 admitted by legacy loader")
	}
	legacy, err := MarshalLocal(t.Context(), dir, controller, []ArtifactInput{{inputs[0].Target, inputs[0].Path}}, "CBX-REMOTE-1")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, manifestPath, legacy)
	legacySet, err := OpenCapabilitySet(t.Context(), manifestPath, controller, Requirement{Supervisor, "CBX-REMOTE-1", ""})
	if err != nil {
		t.Fatal(err)
	}
	a, err = legacySet.Open(t.Context(), inputs[0].Target)
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	if _, err := OpenCapabilitySet(t.Context(), manifestPath, controller, required); err == nil {
		t.Fatal("v1 inferred filesystem capability")
	}
	// A partial operator pack need not provide the official six-target inventory.
	partial, err := MarshalCapabilities(t.Context(), dir, controller, inputs[:1])
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, manifestPath, partial)
	partialSet, err := OpenCapabilitySet(t.Context(), manifestPath, controller, required)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := partialSet.Open(t.Context(), Target{"windows", "amd64"}); err == nil {
		t.Fatal("missing target admitted")
	}
	// The existing snapshot continues to select its original six entries.
	a, err = set.Open(t.Context(), Target{"windows", "arm64"})
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
}

func TestCapabilityManifestValidation(t *testing.T) {
	valid := capabilityManifest{2, digest([]byte("controller")), []capabilityEntry{{entry{"linux", "amd64", "runtime", 1, digest([]byte("runtime"))}, []CapabilityClaim{{Filesystem, "1", digest([]byte("source"))}}}}}
	cases := []struct {
		name string
		edit func(*capabilityManifest)
	}{
		{"unknown capability", func(m *capabilityManifest) { m.Artifacts[0].Capabilities[0].Name = "other" }},
		{"missing fingerprint", func(m *capabilityManifest) { m.Artifacts[0].Capabilities[0].BuildID = "" }},
		{"duplicate capability", func(m *capabilityManifest) {
			m.Artifacts[0].Capabilities = append(m.Artifacts[0].Capabilities, m.Artifacts[0].Capabilities[0])
		}},
		{"non Linux supervisor", func(m *capabilityManifest) {
			m.Artifacts[0].OS = "darwin"
			m.Artifacts[0].Capabilities = []CapabilityClaim{{Supervisor, "1", ""}}
		}},
		{"supervisor fingerprint", func(m *capabilityManifest) { m.Artifacts[0].Capabilities[0].Name = Supervisor }},
		{"no claims", func(m *capabilityManifest) { m.Artifacts[0].Capabilities = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, _ := json.Marshal(valid)
			var m capabilityManifest
			if err := json.Unmarshal(data, &m); err != nil {
				t.Fatal(err)
			}
			tc.edit(&m)
			if err := validateCapabilities(&m); err == nil {
				t.Fatal("invalid claim accepted")
			}
		})
	}
	dir := t.TempDir()
	name := filepath.Join(dir, "manifest.json")
	data, _ := json.Marshal(valid)
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"unknown field", bytes.Replace(data, []byte(`"name":"filesystem"`), []byte(`"name":"filesystem","extra":1`), 1)},
		{"null build ID", bytes.Replace(data, []byte(`"buildId":"`+digest([]byte("source"))+`"`), []byte(`"buildId":null`), 1)},
		{"duplicate field", bytes.Replace(data, []byte(`"schemaVersion":2`), []byte(`"schemaVersion":2,"schemaVersion":2`), 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeFile(t, name, tc.data)
			data, err := readManifestData(name)
			if err == nil {
				_, err = parseCapabilityManifest(data)
			}
			if err == nil {
				t.Fatal("invalid JSON accepted")
			}
		})
	}
}
