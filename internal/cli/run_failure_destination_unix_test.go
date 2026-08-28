//go:build !windows

package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestFailureBundleDestinationUnwritable(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"permission", privateRunOutputWriteError{unix.EACCES}, true},
		{"operation denied", privateRunOutputWriteError{unix.EPERM}, true},
		{"read-only filesystem", privateRunOutputWriteError{unix.EROFS}, true},
		{"wrapped path error", fmt.Errorf("create: %w", privateRunOutputWriteError{&os.PathError{Op: "mkdir", Path: ".crabbox", Err: unix.EROFS}}), true},
		{"privacy permission failure", unix.EACCES, false},
		{"privacy operation failure", unix.EPERM, false},
		{"unclassified read-only error", unix.EROFS, false},
		{"symlink", privateRunOutputWriteError{unix.ELOOP}, false},
		{"not directory", privateRunOutputWriteError{unix.ENOTDIR}, false},
		{"disk full", privateRunOutputWriteError{unix.ENOSPC}, false},
		{"io failure", privateRunOutputWriteError{unix.EIO}, false},
		{"foreign directory owner", validateFailureBundleDirectoryOwner(1001, 1000), false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := failureBundleDestinationUnwritable(tc.err); got != tc.want {
				t.Fatalf("unwritable=%t want=%t error=%v", got, tc.want, tc.err)
			}
		})
	}
}

func TestFailureBundleDirectoryRequiresCurrentOwner(t *testing.T) {
	for _, tc := range []struct{ owner, current uint32 }{{1000, 1000}, {1001, 1000}, {1001, 0}} {
		err := validateFailureBundleDirectoryOwner(tc.owner, tc.current)
		if (err == nil) != (tc.owner == tc.current) {
			t.Fatalf("owner=%d current=%d err=%v", tc.owner, tc.current, err)
		}
	}
}

func TestFailureBundleReadOnlyFilesystemSelection(t *testing.T) {
	for _, fallbackFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("fallbackFails=%t", fallbackFails), func(t *testing.T) {
			state := t.TempDir()
			local := filepath.Join(".crabbox", "captures", "bundle.tar.gz")
			fallback := filepath.Join(state, "captures", "bundle.tar.gz")
			calls := 0
			file, path, err := openFailureBundleDestination("bundle.tar.gz", func() (string, error) { return state, nil }, func(path string) (*os.File, error) {
				calls++
				if calls == 1 {
					if path != local {
						t.Fatalf("first destination=%q want=%q", path, local)
					}
					return nil, privateRunOutputWriteError{&os.PathError{Op: "mkdir", Path: filepath.Dir(path), Err: unix.EROFS}}
				}
				if calls != 2 || path != fallback {
					t.Fatalf("fallback call=%d path=%q want=%q", calls, path, fallback)
				}
				if fallbackFails {
					return nil, privateRunOutputWriteError{unix.EACCES}
				}
				return openFailureBundleFile(path)
			})
			if calls != 2 || path != fallback {
				t.Fatalf("calls=%d path=%q err=%v", calls, path, err)
			}
			if fallbackFails {
				if file != nil || err == nil || !strings.Contains(err.Error(), local) || !strings.Contains(err.Error(), fallback) {
					t.Fatalf("file=%v err=%v", file, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			assertRunOutputMode(t, path, 0o600)
			assertRunOutputMode(t, filepath.Dir(path), 0o700)
		})
	}
}

func makeFailureBundleDirUnwritable(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permission-denied fixture requires a non-root user; error classification is tested separately")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Error(err)
		}
	})
	probe, err := os.CreateTemp(dir, "write-probe-*")
	if err == nil {
		_ = probe.Close()
		_ = os.Remove(probe.Name())
		t.Fatal("fixture did not deny an actual filesystem write")
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("write probe: want permission denied, got %v", err)
	}
}

func TestFailureBundleUnwritableProjectFallsBack(t *testing.T) {
	for _, variant := range []string{"local", "remote archive", "native windows"} {
		for _, blocked := range []string{"cwd", ".crabbox", "captures"} {
			t.Run(variant+"/"+blocked, func(t *testing.T) {
				isolateTestUserDirs(t)
				if variant == "native windows" {
					t.Setenv("XDG_STATE_HOME", "") // Also cover the canonical config-directory default.
				}
				project := t.TempDir()
				t.Chdir(project)
				blockedPath := project
				if blocked == ".crabbox" {
					blockedPath = filepath.Join(project, ".crabbox")
				} else if blocked == "captures" {
					blockedPath = filepath.Join(project, ".crabbox", "captures")
				}
				if err := os.MkdirAll(blockedPath, 0o700); err != nil {
					t.Fatal(err)
				}
				stdout := filepath.Join(t.TempDir(), "stdout.log")
				if err := os.WriteFile(stdout, []byte("failed command output\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				meta := FailureCaptureMetadata{RunID: "run_readonly", ExitCode: 23, StdoutPath: stdout}
				remote := ""
				if variant == "remote archive" {
					remote = filepath.Join(t.TempDir(), "remote.tar.gz")
					file, err := os.Create(remote)
					if err != nil {
						t.Fatal(err)
					}
					gz := gzip.NewWriter(file)
					tw := tar.NewWriter(gz)
					if err := addFailureBundleMetadata(tw, meta); err != nil {
						t.Fatal(err)
					}
					if err := errors.Join(tw.Close(), gz.Close(), file.Close()); err != nil {
						t.Fatal(err)
					}
				}
				makeFailureBundleDirUnwritable(t, blockedPath)
				var path string
				var size int
				var err error
				switch variant {
				case "local":
					path, size, err = CaptureLocalFailureBundle("run_readonly", meta)
				case "remote archive":
					path, size, err = writeLocalFailureBundle("bundle.tar.gz", remote, meta)
				case "native windows":
					path, size, err = captureFailureBundle(context.Background(), SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}, "C:\\repo", "cbx_readonly", "run_readonly", meta)
				}
				if err != nil {
					t.Fatal(err)
				}
				state, err := crabboxStateDir()
				if err != nil {
					t.Fatal(err)
				}
				if filepath.Dir(path) != filepath.Join(state, "captures") || size <= 0 {
					t.Fatalf("path=%q size=%d state=%q", path, size, state)
				}
				contents := readTarGzContents(t, path)
				if string(contents["crabbox-artifacts/stdout.log"]) != "failed command output\n" {
					t.Fatal("missing stdout")
				}
				var run struct{ ExitCode int }
				if err := json.Unmarshal(contents["crabbox-artifacts/crabbox-run.json"], &run); err != nil || run.ExitCode != 23 {
					t.Fatalf("run=%+v err=%v", run, err)
				}
				if variant == "remote archive" && len(contents["crabbox-artifacts/remote/crabbox-artifacts/crabbox-run.json"]) == 0 {
					t.Fatal("missing remote archive contents")
				}
				assertRunOutputMode(t, path, 0o600)
				assertRunOutputMode(t, filepath.Dir(path), 0o700)
				assertRunOutputMode(t, blockedPath, 0o500)
			})
		}
	}
}

func TestFailureBundleFallbackErrorsNameBothDestinations(t *testing.T) {
	for _, failure := range []string{"unwritable", "symlink", "state unavailable"} {
		t.Run(failure, func(t *testing.T) {
			isolateTestUserDirs(t)
			project := t.TempDir()
			t.Chdir(project)
			makeFailureBundleDirUnwritable(t, project)
			state, err := crabboxStateDir()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(state, 0o700); err != nil {
				t.Fatal(err)
			}
			resolve := crabboxStateDir
			want := filepath.Join(state, "captures", "bundle.tar.gz")
			switch failure {
			case "unwritable":
				makeFailureBundleDirUnwritable(t, state)
			case "symlink":
				if err := os.Symlink(t.TempDir(), filepath.Join(state, "captures")); err != nil {
					t.Fatal(err)
				}
			case "state unavailable":
				resolve = func() (string, error) { return "", errors.New("user state directory is unavailable") }
				want = "user state directory is unavailable"
			}
			file, path, err := openFailureBundleDestination("bundle.tar.gz", resolve, openFailureBundleFile)
			if err == nil || file != nil || !strings.Contains(err.Error(), filepath.Join(".crabbox", "captures", "bundle.tar.gz")) || !strings.Contains(err.Error(), want) {
				t.Fatalf("path=%q file=%v err=%v", path, file, err)
			}
			if failure != "state unavailable" && path != want {
				t.Fatalf("path=%q want=%q", path, want)
			}
		})
	}
}

func TestFailureBundleUnreadableStreamDoesNotFallback(t *testing.T) {
	isolateTestUserDirs(t)
	t.Chdir(t.TempDir())
	locked := t.TempDir()
	stream := filepath.Join(locked, "stdout.log")
	if err := os.WriteFile(stream, []byte("output"), 0o600); err != nil {
		t.Fatal(err)
	}
	makeFailureBundleDirUnwritable(t, locked)
	if err := os.Chmod(stream, 0); err != nil {
		t.Fatal(err)
	}
	path, _, err := writeLocalFailureBundle("bundle.tar.gz", "", FailureCaptureMetadata{StdoutPath: stream})
	if err == nil || !strings.Contains(err.Error(), "permission denied") || path != filepath.Join(".crabbox", "captures", "bundle.tar.gz") {
		t.Fatalf("path=%q err=%v", path, err)
	}
	assertNoFailureBundleFallback(t)
}

func TestRunFailureBundleFallbackPreservesCommandExit(t *testing.T) {
	for _, fallbackFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("fallbackFails=%t", fallbackFails), func(t *testing.T) {
			clearConfigEnv(t)
			isolateTestUserDirs(t)
			dir := t.TempDir()
			installWorkspaceOwnerAwareSSH(t, filepath.Join(dir, "ssh"), `#!/bin/sh
case "$1" in
  *failure-destination-test*) printf 'command failed\n' >&2; exit 23 ;;
  *.crabbox/capture-manifest.txt*) printf 'remote capture unavailable\n' >&2; exit 7 ;;
esac
exit 0
`)
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "missing.yaml"))
			t.Setenv("CRABBOX_FAKE_SSH_PORT", "22")
			t.Setenv("CRABBOX_FAKE_SSH_PROXY", "1")
			project := t.TempDir()
			t.Chdir(project)
			makeFailureBundleDirUnwritable(t, project)
			state, err := crabboxStateDir()
			if err != nil {
				t.Fatal(err)
			}
			if fallbackFails {
				captureDir := filepath.Join(state, "captures")
				if err := os.MkdirAll(captureDir, 0o700); err != nil {
					t.Fatal(err)
				}
				makeFailureBundleDirUnwritable(t, captureDir)
			}
			var stdout, stderr bytes.Buffer
			err = (App{Stdout: &stdout, Stderr: &stderr}).runCommand(t.Context(), []string{
				"--provider", "run-env-profile-test", "--no-sync", "--no-hydrate", "--", "failure-destination-test",
			})
			var exitErr ExitError
			if !errors.As(err, &exitErr) || exitErr.Code != 23 {
				t.Fatalf("command error=%v\nstderr=%s", err, stderr.String())
			}
			if !strings.Contains(stderr.String(), "failure-bundle local="+filepath.Join(state, "captures")) {
				t.Fatalf("missing actual fallback path:\n%s", stderr.String())
			}
			if fallbackFails && (!strings.Contains(stderr.String(), "local bundle: failure bundle create .crabbox") || !strings.Contains(stderr.String(), "; fallback "+filepath.Join(state, "captures"))) {
				t.Fatalf("missing both failure destinations:\n%s", stderr.String())
			}
			if !fallbackFails {
				bundles, err := filepath.Glob(filepath.Join(state, "captures", "*.tar.gz"))
				if err != nil || len(bundles) != 1 {
					t.Fatalf("bundles=%v err=%v", bundles, err)
				}
				contents := readTarGzContents(t, bundles[0])
				if string(contents["crabbox-artifacts/stderr.log"]) != "command failed\n" {
					t.Fatal("fallback bundle lost command stderr")
				}
			}
		})
	}
}
