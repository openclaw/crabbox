//go:build !windows

package cli

import (
	"archive/tar"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func runArtifactFileTest(t *testing.T, workdir, script string, args ...string) ([]byte, error) {
	t.Helper()
	bash := "bash"
	if runtime.GOOS == "darwin" {
		bash = "/bin/bash"
	}
	cmd := exec.CommandContext(t.Context(), bash, append([]string{"-c", script, "--"}, args...)...)
	cmd.Dir = workdir
	cmd.Env = []string{"PATH=/usr/bin:/bin", "TMPDIR=" + t.TempDir()}
	return cmd.CombinedOutput()
}

func artifactFileTestDestination(t *testing.T) (string, string) {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return directory, filepath.Join(directory, "final archive.tgz")
}

func TestDelegatedRunArtifactFileScriptFinalizesSharedMatches(t *testing.T) {
	workdir := t.TempDir()
	for _, name := range []string{"reports/proof.txt", "reports/nested/result.json", "reports/.git/private", "reports/.crabbox/private"} {
		writeFile(t, filepath.Join(workdir, name), "private fixture contents")
	}
	for name, target := range map[string]string{
		"leaf": "proof.txt", "dangling": "absent", "directory-link": "nested",
	} {
		if err := os.Symlink(target, filepath.Join(workdir, "reports", name)); err != nil {
			t.Fatal(err)
		}
	}
	required := []string{"reports/proof.txt", "reports/nested/*.json"}
	globs := []string{"reports/**", "reports/proof.txt", "reports/**"}
	directory, destination := artifactFileTestDestination(t)
	output, err := runArtifactFileTest(t, workdir, DelegatedRunArtifactFileScript(required, globs, 0, 0), destination)
	if err != nil {
		t.Fatalf("finalization failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "required artifact reports/proof.txt matched=1") || strings.Contains(string(output), DelegatedRunArtifactBeginMarker) || strings.Contains(string(output), "private fixture contents") {
		t.Fatalf("unexpected file collector output: %s", output)
	}
	info, err := os.Stat(destination)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("final archive is not retained privately: info=%v err=%v", info, err)
	}
	files, err := os.ReadDir(directory)
	if err != nil || len(files) != 1 || files[0].Name() != filepath.Base(destination) {
		t.Fatalf("partial archive retained after success: files=%v err=%v", files, err)
	}
	want := []string{"reports/leaf", "reports/nested/result.json", "reports/proof.txt"}
	names := tarGzNames(t, destination)
	slices.Sort(names)
	if !slices.Equal(names, want) {
		t.Fatalf("canonical matcher changed: got=%v want=%v", names, want)
	}
	headers := readTarGzHeaders(t, destination)
	if headers["reports/leaf"].Typeflag != tar.TypeSymlink || headers["reports/leaf"].Linkname != "proof.txt" {
		t.Fatalf("leaf symlink contract changed: %+v", headers["reports/leaf"])
	}
	if string(readTarGzContents(t, destination)["reports/proof.txt"]) != "private fixture contents" {
		t.Fatal("final archive lost selected contents")
	}
	inline, err := runArtifactFileTest(t, workdir, DelegatedRunArtifactScript(required, globs, 0, 0))
	if err != nil {
		t.Fatalf("original inline collector failed: %v\n%s", err, inline)
	}
	_, encoded, found := strings.Cut(string(inline), DelegatedRunArtifactBeginMarker+"\n")
	encoded, _, ended := strings.Cut(encoded, DelegatedRunArtifactEndMarker+"\n")
	if !found || !ended {
		t.Fatal("inline framing changed")
	}
	archive, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(encoded), ""))
	if err != nil {
		t.Fatal(err)
	}
	inlinePath := filepath.Join(t.TempDir(), "inline.tgz")
	if err := os.WriteFile(inlinePath, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	inlineNames := tarGzNames(t, inlinePath)
	slices.Sort(inlineNames)
	if !slices.Equal(inlineNames, want) {
		t.Fatalf("inline/file selection differs: inline=%v final=%v", inlineNames, names)
	}
}

func TestDelegatedRunArtifactFileScriptFailureDoesNotFinalize(t *testing.T) {
	for _, kind := range []string{"missing-required", "file-cap", "byte-cap", "tar-failure", "directory-link-required"} {
		t.Run(kind, func(t *testing.T) {
			workdir := t.TempDir()
			writeFile(t, filepath.Join(workdir, "a.txt"), "a")
			writeFile(t, filepath.Join(workdir, "b.txt"), "b")
			directory, destination := artifactFileTestDestination(t)
			required, globs, prefix := []string(nil), []string{"*.txt"}, ""
			maxFiles, maxBytes, wantCode := 256, int64(10*1024*1024), 9
			switch kind {
			case "missing-required":
				required, wantCode = []string{"absent.txt"}, 8
			case "file-cap":
				maxFiles = 1
			case "byte-cap":
				maxBytes = 1
			case "tar-failure":
				prefix, wantCode = "tar() { printf partial > \"$2\"; return 42; }\n", 42
			case "directory-link-required":
				if err := os.Symlink(workdir, filepath.Join(workdir, "linked")); err != nil {
					t.Fatal(err)
				}
				required, wantCode = []string{"linked/*.txt"}, 8
			}
			output, err := runArtifactFileTest(t, workdir, prefix+DelegatedRunArtifactFileScript(required, globs, maxFiles, maxBytes), destination)
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != wantCode {
				t.Fatalf("exit=%v want=%d output=%s", err, wantCode, output)
			}
			files, readErr := os.ReadDir(directory)
			if readErr != nil || len(files) != 0 {
				t.Fatalf("failed collector retained an archive: files=%v err=%v", files, readErr)
			}
		})
	}
}

func TestDelegatedRunArtifactFileScriptPreservesExistingDestination(t *testing.T) {
	for _, kind := range []string{"file", "directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			workdir := t.TempDir()
			writeFile(t, filepath.Join(workdir, "proof.txt"), "proof")
			_, destination := artifactFileTestDestination(t)
			if kind == "directory" {
				if err := os.Mkdir(destination, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if kind == "symlink" {
				if err := os.Symlink("unrelated", destination); err != nil {
					t.Fatal(err)
				}
			} else {
				writeFile(t, destination, "unrelated")
			}
			before, _ := os.Lstat(destination)
			output, err := runArtifactFileTest(t, workdir, DelegatedRunArtifactFileScript(nil, []string{"proof.txt"}, 0, 0), destination)
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
				t.Fatalf("existing destination admitted: %v output=%s", err, output)
			}
			after, readErr := os.Lstat(destination)
			if readErr != nil || !os.SameFile(before, after) {
				t.Fatalf("existing destination replaced: %v", readErr)
			}
			if kind == "file" {
				contents, _ := os.ReadFile(destination)
				if string(contents) != "unrelated" {
					t.Fatal("existing destination overwritten")
				}
			}
		})
	}
}

func TestDelegatedRunArtifactFileScriptKeepsReplacedPartial(t *testing.T) {
	workdir := t.TempDir()
	writeFile(t, filepath.Join(workdir, "proof.txt"), "proof")
	directory, destination := artifactFileTestDestination(t)
	prefix := "tar() { mv -- \"$2\" \"$2.original\"; printf unrelated > \"$2\"; return 42; }\n"
	output, err := runArtifactFileTest(t, workdir, prefix+DelegatedRunArtifactFileScript(nil, []string{"proof.txt"}, 0, 0), destination)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 42 {
		t.Fatalf("unexpected replaced-partial result: %v output=%s", err, output)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 2 {
		t.Fatalf("unknown partial identity was removed: %v %v", entries, err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".original") {
			contents, err := os.ReadFile(filepath.Join(directory, entry.Name()))
			if err != nil || string(contents) != "unrelated" {
				t.Fatalf("replacement removed or overwritten: %q %v", contents, err)
			}
		}
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed partial finalized: %v", err)
	}
}

func TestDelegatedRunArtifactFileScriptDestinationBoundary(t *testing.T) {
	workdir := t.TempDir()
	directory, destination := artifactFileTestDestination(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(directory, alias); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		nil, {"relative.tgz"}, {destination, "extra"}, {filepath.Join(alias, "archive.tgz")}, {directory + "/../archive.tgz"},
	} {
		output, err := runArtifactFileTest(t, workdir, DelegatedRunArtifactFileScript(nil, nil, 0, 0), args...)
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
			t.Fatalf("invalid destination admitted: args=%q error=%v output=%s", args, err, output)
		}
	}
	if files, err := os.ReadDir(directory); err != nil || len(files) != 0 {
		t.Fatalf("invalid destination left output: %v %v", files, err)
	}
}

func TestDelegatedRunArtifactFileScriptEmptyOptionalArchive(t *testing.T) {
	workdir := t.TempDir()
	_, destination := artifactFileTestDestination(t)
	output, err := runArtifactFileTest(t, workdir, DelegatedRunArtifactFileScript(nil, []string{"missing/*"}, 0, 0), destination)
	if err != nil || !strings.Contains(string(output), "warning: no artifact matches") {
		t.Fatalf("empty optional archive failed: %v output=%s", err, output)
	}
	if names := tarGzNames(t, destination); len(names) != 0 {
		t.Fatalf("empty archive contains files: %v", names)
	}
}
