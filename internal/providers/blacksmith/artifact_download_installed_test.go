//go:build darwin || linux

package blacksmith

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type blacksmithInstalledHelperFixture struct {
	repo, temp, tools, scp string
}

func newBlacksmithInstalledHelperFixture(t *testing.T) blacksmithInstalledHelperFixture {
	t.Helper()
	f := blacksmithInstalledHelperFixture{repo: t.TempDir(), temp: t.TempDir(), tools: t.TempDir()}
	installed := filepath.Join(t.TempDir(), "installed helper's path")
	for _, name := range []string{filepath.Join(f.tools, "blacksmith"), installed} {
		if err := os.WriteFile(name, []byte("#!/bin/sh\nexit 99\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	f.scp, err = filepath.EvalSymlinks(installed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.scp, filepath.Join(f.tools, "scp")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", f.tools+":/usr/bin:/bin")
	t.Setenv("TMPDIR", f.temp)
	t.Setenv("CRABBOX_CONTROLLER_PROCESS_TREE_OWNED", "")
	return f
}

const installedHelperPayload = "installed helper fixture"

func TestBlacksmithDownloadInstalledHelperExecution(t *testing.T) {
	f := newBlacksmithInstalledHelperFixture(t)
	// Both executables are test-owned scripts. Only the final Go fixture reads
	// kernel limits and writes the tiny expected destination.
	native := "#!/bin/sh\nfor arg do destination=$arg; done\nexec scp 'one argument with spaces' '*' '' \"$destination\"\n"
	scp := "#!/bin/sh\nexec " + core.ShellQuote(os.Args[0]) + " -test.run=^TestBlacksmithInstalledHelperChild$ -- \"$0\" \"$@\"\n"
	for name, script := range map[string]string{filepath.Join(f.tools, "blacksmith"): native, f.scp: scp} {
		if err := os.WriteFile(name, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CRABBOX_TEST_INSTALLED_HELPER", "1")
	var before, after syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &before); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	realRunner := core.RuntimeForProviderOperation(io.Discard).Exec
	var nativeResult core.LocalCommandResult
	var stage string
	backend := newTestBlacksmithBackend(core.BaseConfig(), ownershipRunner(func(runCtx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if runCtx != ctx || !req.RequireProcessGroupJoin || req.MaxCapturedOutputBytes != int(blacksmithArtifactDiagnosticCaptureBytes) {
			t.Fatal("fixture lost its bounded original command owner")
		}
		stage = filepath.Dir(req.Args[len(req.Args)-1])
		var err error
		nativeResult, err = realRunner.Run(runCtx, req)
		return nativeResult, err
	}))
	remote := ".crabbox/blacksmith-artifact-" + strings.Repeat("a", 64) + "/archive.tgz"
	data, err := backend.downloadArtifact(ctx, f.repo, "tbx_installed_helper", "/synthetic/key", remote, int64(len(installedHelperPayload)), fmt.Sprintf("%x", sha256.Sum256([]byte(installedHelperPayload))))
	if err != nil || string(data) != installedHelperPayload || nativeResult.ExitCode != 0 || ctx.Err() != nil {
		t.Fatalf("test-owned helper did not close naturally with valid bytes: result=%+v err=%v", nativeResult, err)
	}
	var proof struct {
		Script, Cwd string
		Args        []string
		Soft, Hard  uint64
	}
	line, _, ok := strings.Cut(nativeResult.Stdout, "\n")
	if !ok || json.Unmarshal([]byte(line), &proof) != nil {
		t.Fatalf("missing helper behavior receipt: %q", nativeResult.Stdout)
	}
	physicalRepo, err := filepath.EvalSymlinks(f.repo)
	if err != nil {
		t.Fatal(err)
	}
	wantLimit := min(uint64(core.DelegatedRunArtifactDefaultMaxBytes), before.Max)
	if proof.Script != f.scp || proof.Cwd != physicalRepo || !slices.Equal(proof.Args, []string{"one argument with spaces", "*", "", filepath.Join(stage, "archive.tgz")}) || proof.Soft != wantLimit || proof.Hard != wantLimit {
		t.Fatalf("installed execution identity/argv/cwd/limits changed: %+v; installed=%s limit=%d", proof, f.scp, wantLimit)
	}
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &after); err != nil || before != after {
		t.Fatal("child limit changed the parent")
	}
	if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful joined fixture retained staging: %v", err)
	}
}

func TestBlacksmithInstalledHelperChild(t *testing.T) {
	if os.Getenv("CRABBOX_TEST_INSTALLED_HELPER") != "1" {
		return
	}
	index := slices.Index(os.Args, "--")
	if index < 0 || len(os.Args[index+1:]) != 5 {
		t.Fatal("unexpected fixture arguments")
	}
	args := os.Args[index+1:]
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(struct {
		Script, Cwd string
		Args        []string
		Soft, Hard  uint64
	}{args[0], cwd, args[1:], limit.Cur, limit.Max}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(args[4], []byte(installedHelperPayload), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBlacksmithDownloadInstalledHelperDrift(t *testing.T) {
	for _, kind := range []string{"inode", "content-restored-mtime", "mode", "missing", "symlink", "fifo", "setuid-before", "setgid-before", "inode-native-failure", "inode-cancellation"} {
		t.Run(kind, func(t *testing.T) {
			f := newBlacksmithInstalledHelperFixture(t)
			original, err := os.ReadFile(f.scp)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(f.scp)
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasSuffix(kind, "-before") {
				bit := os.ModeSetuid
				if kind == "setgid-before" {
					bit = os.ModeSetgid
					// A temp directory may inherit a group the caller cannot setgid.
					if err := os.Chown(f.scp, -1, os.Getegid()); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Chmod(f.scp, 0o700|bit); err != nil {
					t.Fatal(err)
				}
				changed, err := os.Lstat(f.scp)
				if err != nil {
					t.Fatal(err)
				}
				if changed.Mode()&bit == 0 {
					t.Fatalf("fixture did not retain requested special mode %v: got %v", bit, changed.Mode())
				}
			}
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			cause := errors.New("synthetic caller cancellation")
			nativeFailure := errors.New("synthetic native failure")
			var stage string
			calls := 0
			const stdout, stderr = "SYNTHETIC_NATIVE_STDOUT", "SYNTHETIC_NATIVE_STDERR"
			backend := newTestBlacksmithBackend(core.BaseConfig(), ownershipRunner(func(runCtx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				calls++
				destination := req.Args[len(req.Args)-1]
				stage = filepath.Dir(destination)
				if err := os.WriteFile(destination, []byte(installedHelperPayload), 0o600); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "inode", "inode-native-failure", "inode-cancellation", "missing", "symlink", "fifo":
					if err := os.Rename(f.scp, f.scp+".prior"); err != nil {
						t.Fatal(err)
					}
					if strings.HasPrefix(kind, "inode") {
						if err := os.WriteFile(f.scp, original, 0o700); err != nil {
							t.Fatal(err)
						}
						if err := os.Chtimes(f.scp, info.ModTime(), info.ModTime()); err != nil {
							t.Fatal(err)
						}
					} else if kind == "symlink" {
						if err := os.Symlink(f.scp+".prior", f.scp); err != nil {
							t.Fatal(err)
						}
					} else if kind == "fifo" {
						if err := syscall.Mkfifo(f.scp, 0o600); err != nil {
							t.Fatal(err)
						}
					}
				case "content-restored-mtime":
					if err := os.WriteFile(f.scp, []byte(strings.Replace(string(original), "99", "98", 1)), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.Chtimes(f.scp, info.ModTime(), info.ModTime()); err != nil {
						t.Fatal(err)
					}
					changed, err := os.Stat(f.scp)
					if err != nil || !os.SameFile(info, changed) || changed.Size() != info.Size() || changed.ModTime() != info.ModTime() {
						t.Fatal("fixture did not isolate same-inode/content-only drift")
					}
				case "mode":
					if err := os.Chmod(f.scp, 0o500); err != nil {
						t.Fatal(err)
					}
				}
				result := core.LocalCommandResult{Stdout: stdout, Stderr: stderr}
				if kind == "inode-native-failure" {
					result.ExitCode = 23
					return result, nativeFailure
				}
				if kind == "inode-cancellation" {
					cancel(cause)
					result.ExitCode = -1
					return result, runCtx.Err()
				}
				return result, nil
			}))
			remote := ".crabbox/blacksmith-artifact-" + strings.Repeat("a", 64) + "/archive.tgz"
			data, err := backend.downloadArtifact(ctx, f.repo, "tbx_helper_drift", "/synthetic/key", remote, int64(len(installedHelperPayload)), fmt.Sprintf("%x", sha256.Sum256([]byte(installedHelperPayload))))
			if err == nil || data != nil {
				t.Errorf("observed installed helper drift accepted correct archive bytes: data=%q err=%v", data, err)
			}
			if strings.HasSuffix(kind, "-before") {
				if calls != 0 {
					t.Errorf("privileged installed helper reached native runner: calls=%d", calls)
				}
				return
			}
			if calls != 1 {
				t.Fatalf("native attempts=%d; want one", calls)
			}
			if kind == "inode-native-failure" && !errors.Is(err, nativeFailure) {
				t.Errorf("helper drift replaced earlier native failure: %v", err)
			}
			if kind == "inode-cancellation" && (!errors.Is(err, context.Canceled) || !errors.Is(err, cause)) {
				t.Errorf("helper drift replaced cancellation: %v", err)
			}
			for stream, want := range map[string]string{"stdout": stdout, "stderr": stderr} {
				path := filepath.Join(stage, "native-download."+stream+".log")
				got, readErr := os.ReadFile(path)
				info, statErr := os.Lstat(path)
				if readErr != nil || statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || string(got) != want {
					t.Errorf("helper drift lost private %s diagnostics: read=%v stat=%v", stream, readErr, statErr)
				}
				if err != nil && strings.Contains(err.Error(), want) {
					t.Errorf("raw native diagnostics escaped into returned error: %v", err)
				}
			}
		})
	}
}
