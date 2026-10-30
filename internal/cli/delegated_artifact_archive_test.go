package cli

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDelegatedArtifactArchiveBinaryRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX collector fixture")
	}
	root := t.TempDir()
	var pngData, zipData bytes.Buffer
	if err := png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(&zipData)
	f, err := zw.CreateHeader(&zip.FileHeader{Name: "trace.data", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	// A trace larger than the old delegated-download 64 KiB limit stays binary.
	_, _ = f.Write(bytes.Repeat([]byte("trace-event\x00\xff\n"), 8192))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	fixtures := map[string][]byte{"screen.png": pngData.Bytes(), "trace.zip": zipData.Bytes(), "result.json": []byte("{\"ok\":true}\n")}
	req := RunRequest{Repo: Repo{Root: t.TempDir()}, RunID: "binary-roundtrip", ArtifactGlobs: []string{"*"}, RequiredArtifactGlobs: []string{"*.png", "trace.zip", "result.json"}}
	for name, data := range fixtures {
		if err := os.WriteFile(filepath.Join(root, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
		req.Downloads = append(req.Downloads, name+"="+filepath.Join(t.TempDir(), name))
	}
	selected, err := DelegatedRunArtifactSelection(req)
	if err != nil {
		t.Fatal(err)
	}
	archiveDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(archiveDir, "output.tgz")
	cmd := exec.Command("bash", "-c", DelegatedRunArtifactFileScript(req.RequiredArtifactGlobs, selected, 0, 0), "collector", archive)
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("collector: %v %s", err, output)
	}
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := MaterializeDelegatedRunArtifactArchive(context.Background(), req, "lease", data)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 4 {
		t.Fatalf("artifacts=%+v", artifacts)
	}
	for _, artifact := range artifacts[:3] {
		got, err := os.ReadFile(artifact.Path)
		if err != nil || !bytes.Equal(got, fixtures[filepath.Base(artifact.Path)]) {
			t.Fatalf("binary mismatch: %s %v", artifact.Path, err)
		}
	}
	got, err := os.ReadFile(artifacts[3].Path)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("archive mismatch: %v", err)
	}
}

func delegatedArtifactTestArchive(t *testing.T, name string, kind byte, data []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	tw := tar.NewWriter(gz)
	header := &tar.Header{Name: name, Mode: 0o600, Typeflag: kind, Size: int64(len(data))}
	if kind == tar.TypeSymlink || kind == tar.TypeLink {
		header.Linkname, header.Size = "/outside/private", 0
		data = nil
	}
	if err := tw.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestDelegatedArtifactArchiveWithholdsInvalidOutputs(t *testing.T) {
	valid := delegatedArtifactTestArchive(t, "proof.json", tar.TypeReg, []byte("{}"))
	corrupt := bytes.Clone(valid)
	corrupt[len(corrupt)-5] ^= 1
	for name, archive := range map[string][]byte{
		"traversal":  delegatedArtifactTestArchive(t, "../proof.json", tar.TypeReg, nil),
		"absolute":   delegatedArtifactTestArchive(t, "/proof.json", tar.TypeReg, nil),
		"protected":  delegatedArtifactTestArchive(t, ".git/proof.json", tar.TypeReg, nil),
		"symlink":    delegatedArtifactTestArchive(t, "proof.json", tar.TypeSymlink, nil),
		"hardlink":   delegatedArtifactTestArchive(t, "proof.json", tar.TypeLink, nil),
		"unselected": delegatedArtifactTestArchive(t, "other.json", tar.TypeReg, nil),
		"truncated":  valid[:len(valid)-8],
		"corrupt":    corrupt,
		"trailing":   append(bytes.Clone(valid), valid...),
	} {
		t.Run(name, func(t *testing.T) {
			local := filepath.Join(t.TempDir(), "proof.json")
			if err := os.WriteFile(local, []byte("existing"), 0o600); err != nil {
				t.Fatal(err)
			}
			req := RunRequest{Downloads: []string{"proof.json=" + local}}
			if _, err := MaterializeDelegatedRunArtifactArchive(context.Background(), req, "lease", archive); err == nil {
				t.Fatal("invalid archive accepted")
			}
			got, _ := os.ReadFile(local)
			if string(got) != "existing" {
				t.Fatalf("destination changed: %q", got)
			}
		})
	}
}

func TestDelegatedArtifactArchiveMissingEmptyAndCancellation(t *testing.T) {
	archive := delegatedArtifactTestArchive(t, "proof.json", tar.TypeReg, []byte("{}"))
	if _, err := MaterializeDelegatedRunArtifactArchive(context.Background(), RunRequest{ArtifactGlobs: []string{"*.json"}, RequiredArtifactGlobs: []string{"missing.json"}}, "lease", archive); err == nil {
		t.Fatal("missing required accepted")
	}
	for _, remote := range []string{"../secret", "/secret", ".git/config", "https://example.test/proof", "link/*.json"} {
		if _, err := DelegatedRunArtifactSelection(RunRequest{Downloads: []string{remote + "=local"}}); err == nil {
			t.Fatalf("unsafe download accepted: %s", remote)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	local := filepath.Join(t.TempDir(), "proof.json")
	if _, err := MaterializeDelegatedRunArtifactArchive(ctx, RunRequest{Downloads: []string{"proof.json=" + local}}, "lease", archive); err != context.Canceled {
		t.Fatalf("cancellation=%v", err)
	}
	if _, err := os.Stat(local); !os.IsNotExist(err) {
		t.Fatalf("canceled output exists: %v", err)
	}
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	tw := tar.NewWriter(gz)
	_ = tw.Close()
	_ = gz.Close()
	root := t.TempDir()
	if _, err := MaterializeDelegatedRunArtifactArchive(context.Background(), RunRequest{Repo: Repo{Root: root}, ArtifactGlobs: []string{"missing/**"}}, "lease", output.Bytes()); err != nil {
		t.Fatalf("empty optional archive: %v", err)
	}
}

func TestDelegatedArtifactArchiveBounds(t *testing.T) {
	for _, tc := range []struct {
		name      string
		count     int
		size      int64
		duplicate bool
	}{
		{name: "file count", count: DelegatedRunArtifactDefaultMaxFiles + 1},
		{name: "expanded bytes", count: 1, size: DelegatedRunArtifactDefaultMaxBytes + 1},
		{name: "duplicates", count: 2, duplicate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			gz := gzip.NewWriter(&output)
			tw := tar.NewWriter(gz)
			for i := 0; i < tc.count; i++ {
				name := fmt.Sprintf("proof-%d.json", i)
				if tc.duplicate {
					name = "proof.json"
				}
				if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o600, Size: tc.size}); err != nil {
					t.Fatal(err)
				}
				if tc.size > 0 {
					if _, err := tw.Write(make([]byte, tc.size)); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := MaterializeDelegatedRunArtifactArchive(context.Background(), RunRequest{ArtifactGlobs: []string{"*.json"}}, "lease", output.Bytes()); err == nil {
				t.Fatal("archive boundary not enforced")
			}
		})
	}
}
