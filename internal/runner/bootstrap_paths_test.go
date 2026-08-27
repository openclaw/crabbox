package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPOSIXDigestIgnoresEscapedFilename(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX checksum command")
	}
	tool, err := exec.LookPath("sha256sum")
	if err != nil {
		tool, err = exec.LookPath("gsha256sum")
	}
	if err != nil {
		t.Skip("GNU sha256sum unavailable")
	}
	bin := t.TempDir()
	if err := os.Symlink(tool, filepath.Join(bin, "sha256sum")); err != nil {
		t.Fatal(err)
	}
	data := []byte("checksum fixture")
	name := filepath.Join(t.TempDir(), `file\name`)
	if err := os.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "sh", "-c", digestFunction+"\ndigest \"$1\"\n", "fixture", name)
	command.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	digest := sha256.Sum256(data)
	if err != nil || strings.TrimSpace(string(output)) != hex.EncodeToString(digest[:]) {
		t.Fatalf("digest=%q err=%v", output, err)
	}
}

func TestRemotePathPreservesPOSIXTrailingBackslash(t *testing.T) {
	for _, targetOS := range []string{"linux", "darwin"} {
		platform := Runtime{Target: Target{OS: targetOS, Arch: "amd64"}, Home: `/home/operator\`}
		artifact := Artifact{Identity: Identity{OS: targetOS, Arch: "amd64"}, SHA256: strings.Repeat("a", 64)}
		name, err := artifact.RemotePath(platform)
		if err != nil || name != platform.Home+"/.cache/crabbox/runners/"+artifact.SHA256 {
			t.Fatalf("home=%q path=%q err=%v", platform.Home, name, err)
		}
	}
}

func TestResultsMarkerUsesPhysicalWorkspace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX physical parent resolution")
	}
	for _, gitMetadata := range []bool{false, true} {
		t.Run(map[bool]string{false: "fallback", true: "git"}[gitMetadata], func(t *testing.T) {
			base, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			workspace := filepath.Join(base, "nested")
			if err := os.MkdirAll(filepath.Join(workspace, "child"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("nested/child", filepath.Join(base, "alias")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			marker := filepath.Join(workspace, ".crabbox", "results-start")
			if gitMetadata {
				git, err := exec.LookPath("git")
				if err != nil {
					t.Skip("git unavailable")
				}
				if output, err := exec.CommandContext(t.Context(), git, "-c", "init.templateDir=", "init", "--quiet", workspace).CombinedOutput(); err != nil {
					t.Fatalf("git init: %v: %s", err, output)
				}
				marker = filepath.Join(workspace, ".git", "crabbox", "results-start")
			}
			if err := os.MkdirAll(filepath.Dir(marker), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(marker, []byte("marker"), 0600); err != nil {
				t.Fatal(err)
			}
			want := time.Unix(1700000000, 0)
			if err := os.Chtimes(marker, want, want); err != nil {
				t.Fatal(err)
			}
			got, err := resultsMarkerTime(t.Context(), base+"/alias/..")
			if err != nil || !got.Equal(want) {
				t.Fatalf("marker=%v want=%v err=%v", got, want, err)
			}
		})
	}
}
