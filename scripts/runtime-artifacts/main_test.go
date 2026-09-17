package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}, {"prepare"}, {"verify", "--directory", "/missing"}, {"prepare", "--unknown"}} {
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), args, &stdout, &stderr); code != 2 || stdout.Len() != 0 || stderr.Len() == 0 {
			t.Fatalf("args=%q code=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"unknown", "--help"}, &stdout, &stderr); code != 0 || !strings.HasPrefix(stdout.String(), "Usage:") || stderr.Len() != 0 {
		t.Fatalf("help code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestPrepareAndVerifyActualRuntime(t *testing.T) {
	directory, controller := runtimeFixture(t)
	args := []string{"prepare", "--directory", directory, "--controller", controller}
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), args, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("prepare code=%d stderr=%q", code, stderr.String())
	}
	manifest := append([]byte(nil), stdout.Bytes()...)
	if !json.Valid(manifest) {
		t.Fatalf("prepare output is not JSON: %q", manifest)
	}
	manifestPath := filepath.Join(directory, "manifest.json")
	if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
		t.Fatalf("prepare wrote its own output: %v", err)
	}
	if err := os.WriteFile(manifestPath, manifest, 0600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	args[0] = "verify"
	if code := run(t.Context(), args, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("verify code=%d stderr=%q", code, stderr.String())
	}
	var result struct {
		Artifacts []struct{ OS, Arch, SHA256 string }
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || len(result.Artifacts) != 2 {
		t.Fatalf("verify identities=%q err=%v", stdout.String(), err)
	}
	for i, arch := range []string{"amd64", "arm64"} {
		if got := result.Artifacts[i]; got.OS != "linux" || got.Arch != arch || len(got.SHA256) != 64 {
			t.Fatalf("target %d=%+v", i, got)
		}
	}
	stdout.Reset()
	if code := run(t.Context(), args, failingWriter{}, &stderr); code != 1 {
		t.Fatalf("output error code=%d", code)
	}
	stderr.Reset()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if code := run(ctx, args, &stdout, &stderr); code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "context canceled") {
		t.Fatalf("cancel code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	other := filepath.Join(t.TempDir(), "other-controller")
	if err := os.WriteFile(other, []byte("different controller"), 0600); err != nil {
		t.Fatal(err)
	}
	args[len(args)-1] = other
	stderr.Reset()
	if code := run(t.Context(), args, &stdout, &stderr); code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "controller SHA-256 mismatch") {
		t.Fatalf("mismatch code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func runtimeFixture(t *testing.T) (string, string) {
	t.Helper()
	directory := t.TempDir()
	controller, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", filepath.Join(directory, "linux-"+arch), "../../cmd/crabbox-runtime")
		cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0", "GOFLAGS=", "GOWORK=off", "GOPROXY=off", "GOSUMDB=off")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", arch, err, output)
		}
	}
	return directory, controller
}

func TestReleaseArchiveLayouts(t *testing.T) {
	pack, controller := runtimeFixture(t)
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"prepare", "--directory", pack, "--controller", controller}, &stdout, &stderr); code != 0 {
		t.Fatal(stderr.String())
	}
	if err := os.WriteFile(filepath.Join(pack, "manifest.json"), stdout.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	for _, platform := range []string{"darwin", "linux", "windows"} {
		for _, arch := range []string{"amd64", "arm64"} {
			for _, mode := range []string{"none", "unsigned", "final"} {
				t.Run(platform+"/"+arch+"/"+mode, func(t *testing.T) {
					binary := "crabbox"
					if platform == "windows" {
						binary += ".exe"
					}
					files := map[string]string{binary: controller}
					if platform == "darwin" && arch == "arm64" {
						files["crabbox-apple-vm-helper"] = controller
					}
					if mode != "none" {
						for _, target := range []string{"linux-amd64", "linux-arm64"} {
							files["crabbox-runtime/"+target] = filepath.Join(pack, target)
						}
						if mode == "final" {
							files["crabbox-runtime/manifest.json"] = filepath.Join(pack, "manifest.json")
						}
					}
					archive := writeArchiveFixture(t, files, platform == "windows")
					destination := filepath.Join(t.TempDir(), "new")
					report, err := extractArchive(t.Context(), archive, destination, platform, arch, mode)
					if err != nil {
						t.Fatal(err)
					}
					identity, err := digestFile(t.Context(), archive)
					if err != nil || report.SHA256 != identity.SHA256 || report.Size != identity.Size {
						t.Fatalf("archive identity=%+v err=%v", report, err)
					}
					for name, original := range files {
						want, _ := os.ReadFile(original)
						got, err := os.ReadFile(filepath.Join(destination, name))
						if err != nil || !bytes.Equal(got, want) {
							t.Fatalf("member %s differs: %v", name, err)
						}
						info, err := os.Stat(filepath.Join(destination, name))
						if err != nil {
							t.Fatal(err)
						}
						permissions := os.FileMode(0755)
						if name == "crabbox-runtime/manifest.json" {
							permissions = 0644
						}
						if runtime.GOOS != "windows" && info.Mode().Perm() != permissions {
							t.Fatalf("member %s permissions=%o, want %o", name, info.Mode().Perm(), permissions)
						}
					}
					if mode == "none" {
						if report.RuntimePack != nil {
							t.Fatal("legacy archive acquired runtime metadata")
						}
					} else if report.RuntimePack == nil || len(report.RuntimePack.Artifacts) != 2 || (report.RuntimePack.Manifest != nil) != (mode == "final") {
						t.Fatalf("runtime report=%+v", report.RuntimePack)
					}
					if _, err := extractArchive(t.Context(), archive, destination, platform, arch, mode); err == nil {
						t.Fatal("extract accepted an existing destination")
					}
				})
			}
		}
	}
	archive := writeArchiveFixture(t, map[string]string{"crabbox": controller}, false)
	destination := filepath.Join(t.TempDir(), "incomplete")
	if _, err := extractArchive(t.Context(), archive, destination, "linux", "amd64", "final"); err == nil {
		t.Fatal("accepted a missing runtime pack")
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("failed extraction left its owned directory: %v", err)
	}
	t.Run("manifest follows controller finalization", func(t *testing.T) {
		// Simulate a signing transformation without credentials or candidate execution.
		finalized := filepath.Join(t.TempDir(), "crabbox")
		data, err := os.ReadFile(controller)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(finalized, append(data, []byte("fixture-finalization")...), 0755); err != nil {
			t.Fatal(err)
		}
		files := map[string]string{"crabbox": finalized}
		for _, name := range []string{"linux-amd64", "linux-arm64", "manifest.json"} {
			files["crabbox-runtime/"+name] = filepath.Join(pack, name)
		}
		stale := writeArchiveFixture(t, files, false)
		if _, err := extractArchive(t.Context(), stale, filepath.Join(t.TempDir(), "stale"), "linux", "amd64", "final"); err == nil || !strings.Contains(err.Error(), "controller SHA-256 mismatch") {
			t.Fatalf("stale controller binding: %v", err)
		}
		var manifest, diagnostics bytes.Buffer
		if code := run(t.Context(), []string{"prepare", "--directory", pack, "--controller", finalized}, &manifest, &diagnostics); code != 0 {
			t.Fatal(diagnostics.String())
		}
		if err := os.WriteFile(filepath.Join(pack, "manifest.json"), manifest.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		current := writeArchiveFixture(t, files, false)
		if _, err := extractArchive(t.Context(), current, filepath.Join(t.TempDir(), "final"), "linux", "amd64", "final"); err != nil {
			t.Fatalf("final controller binding: %v", err)
		}
	})
}

func writeArchiveFixture(t *testing.T, files map[string]string, windows bool) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "archive.tar.gz")
	if windows {
		file = filepath.Join(t.TempDir(), "archive.zip")
	}
	output, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	var archive io.Closer
	var entry func(string, int64) (io.Writer, error)
	if windows {
		writer := zip.NewWriter(output)
		archive = writer
		entry = func(name string, _ int64) (io.Writer, error) {
			header := &zip.FileHeader{Name: name, Method: zip.Deflate}
			header.SetMode(0755)
			return writer.CreateHeader(header)
		}
	} else {
		compressed := gzip.NewWriter(output)
		defer compressed.Close()
		writer := tar.NewWriter(compressed)
		archive = writer
		entry = func(name string, size int64) (io.Writer, error) {
			err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0755, Size: size, Typeflag: tar.TypeReg})
			return writer, err
		}
	}
	for name, source := range files {
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		writer, err := entry(name, int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return file
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("output unavailable") }
