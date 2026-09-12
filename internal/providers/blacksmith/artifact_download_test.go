//go:build darwin || linux

package blacksmith

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestBlacksmithDownloadArtifactValidation(t *testing.T) {
	for _, kind := range []string{"valid", "relative-tmp", "hash", "size", "replacement", "symlink", "directory", "stage-replacement", "native-exit", "cancel", "entry-cause", "cancel-cause", "cleanup-cause", "invalid-remote", "invalid-size", "invalid-hash"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newBlacksmithInstalledHelperFixture(t)
			repoRoot, tempParent := fixture.repo, fixture.temp
			if kind == "valid" {
				t.Setenv("XDG_STATE_HOME", filepath.Join(repoRoot, "nested-state"))
				if err := validateBlacksmithNativeSyncScope(repoRoot); err == nil {
					t.Fatal("inbound fixture must overlap a rejected outgoing source scope")
				}
			}
			var physicalTempParent string
			if kind == "relative-tmp" {
				t.Chdir(t.TempDir())
				if err := os.Symlink(tempParent, "relative-tmp"); err != nil {
					t.Fatal(err)
				}
				t.Setenv("TMPDIR", "relative-tmp")
				var err error
				physicalTempParent, err = filepath.EvalSymlinks(tempParent)
				if err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("BLACKSMITH_DISABLE_AUTO_UPDATE", "0")
			base, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			cause := errors.New("synthetic download cancellation cause")
			var stage string
			var ctx context.Context = base
			if kind == "cleanup-cause" {
				ctx = &blacksmithDownloadCleanupCancellation{Context: base, stage: &stage, cancel: cancel, cause: cause}
			}
			if kind == "entry-cause" {
				cancel(cause)
			}
			const payload = "tiny artifact"
			digest := sha256.Sum256([]byte(payload))
			expectedBytes, expectedHash := int64(len(payload)), hex.EncodeToString(digest[:])
			remote := ".crabbox/blacksmith-artifact-" + strings.Repeat("a", 64) + "/archive.tgz"
			if kind == "invalid-remote" {
				remote += "/"
			}
			if kind == "invalid-size" {
				expectedBytes = core.DelegatedRunArtifactDefaultMaxBytes + 1
			}
			if kind == "invalid-hash" {
				expectedHash = "invalid"
			}
			calls := 0
			backend := newTestBlacksmithBackend(baseConfig(), ownershipRunner(func(runCtx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
				calls++
				destination := req.Args[len(req.Args)-1]
				stage = filepath.Dir(destination)
				if req.Dir != repoRoot || stage == repoRoot || !filepath.IsAbs(destination) {
					t.Fatal("download changed repository cwd or lost separate absolute staging")
				}
				if physicalTempParent != "" && filepath.Dir(stage) != physicalTempParent {
					t.Fatal("download stage did not use the physical temporary parent")
				}
				if runCtx != ctx || req.MaxCapturedOutputBytes <= 0 || req.DisableOutputCapture || !req.RequireProcessGroupJoin {
					t.Fatal("download lost caller context, bounded capture, or process-group ownership")
				}
				if testBlacksmithFlag(req.Args, "--id") != "tbx_download" || testBlacksmithFlag(req.Args, "--api-url") != "https://api.example.invalid" || testBlacksmithFlag(req.Args, "--org") != "example-org" || testBlacksmithFlag(req.Args, "--ssh-private-key") != "/synthetic/key" {
					t.Fatal("download lost captured native identity arguments")
				}
				if !containsString(req.Env, "BLACKSMITH_DISABLE_AUTO_UPDATE=1") || !containsString(req.Env, "POSIXLY_CORRECT=1") || !containsString(req.Env, "PATH="+filepath.Join(stage, "bin")+":/usr/bin:/bin") {
					t.Fatal("download helper resolution or updater boundary changed")
				}
				privateHelper, err := os.Lstat(filepath.Join(stage, "bin", "scp"))
				if err != nil || !privateHelper.Mode().IsRegular() || privateHelper.Mode().Perm() != 0o700 {
					t.Fatal("private helper entry was not exclusively executable")
				}
				if req.Args[len(req.Args)-2] != remote || destination != filepath.Join(stage, "archive.tgz") {
					t.Fatal("native download lost exact source/destination")
				}
				info, err := os.Lstat(destination)
				if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() != 0 {
					t.Fatal("download destination was not exclusively precreated")
				}
				if kind == "replacement" || kind == "symlink" || kind == "directory" {
					if err := os.Rename(destination, destination+".original"); err != nil {
						t.Fatal(err)
					}
				}
				contents := payload
				if kind == "hash" {
					contents = "evil artifact"
				}
				if kind == "size" {
					contents += "!"
				}
				switch kind {
				case "directory":
					err = os.Mkdir(destination, 0o700)
				case "symlink":
					err = os.Symlink(destination+".original", destination)
				default:
					err = os.WriteFile(destination, []byte(contents), 0o600)
				}
				if err != nil {
					t.Fatal(err)
				}
				if kind == "stage-replacement" {
					if err := os.Rename(stage, stage+".original"); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(stage, 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.Link(filepath.Join(stage+".original", "archive.tgz"), destination); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "cancel" {
					cancel(nil)
					return LocalCommandResult{ExitCode: -1}, runCtx.Err()
				}
				if kind == "cancel-cause" {
					cancel(cause)
					return LocalCommandResult{ExitCode: -1}, runCtx.Err()
				}
				if kind == "native-exit" {
					return LocalCommandResult{ExitCode: 23}, errors.New("synthetic native failure")
				}
				return LocalCommandResult{}, nil
			}))
			backend.route = &blacksmithRoute{API: "https://api.example.invalid", Org: "example-org"}
			data, err := backend.downloadArtifact(ctx, repoRoot, "tbx_download", "/synthetic/key", remote, expectedBytes, expectedHash)
			if strings.HasSuffix(kind, "-cause") {
				// Observe completed staging cleanup even on the old path, whose
				// return skipped the final cancellation normalization.
				if ctx.Err() != context.Canceled || !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
					t.Errorf("download lost cancellation classification or custom cause: %v", err)
				}
			}
			if kind == "valid" || kind == "relative-tmp" {
				if err != nil || string(data) != payload {
					t.Fatalf("valid download withheld: %v", err)
				}
			} else if err == nil || data != nil {
				t.Fatalf("invalid download accepted: %s", kind)
			}
			if strings.HasPrefix(kind, "invalid-") || kind == "entry-cause" {
				if calls != 0 {
					t.Fatal("invalid metadata reached native runner")
				}
				return
			}
			if calls != 1 {
				t.Fatalf("native attempts=%d, want one", calls)
			}
			_, statErr := os.Stat(stage)
			if kind == "native-exit" || kind == "cancel" || kind == "cancel-cause" || kind == "stage-replacement" {
				if statErr != nil || !strings.Contains(err.Error(), stage) {
					t.Fatal("uncertain writer staging was removed or not reported")
				}
			} else if !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("joined download staging was not removed")
			}
			if kind == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal("caller cancellation was lost")
			}
		})
	}
}

type blacksmithDownloadCleanupCancellation struct {
	context.Context
	stage  *string
	cancel context.CancelCauseFunc
	cause  error
}

func (c blacksmithDownloadCleanupCancellation) Err() error {
	if *c.stage != "" {
		if _, err := os.Lstat(*c.stage); errors.Is(err, os.ErrNotExist) {
			c.cancel(c.cause)
		}
	}
	return c.Context.Err()
}

func TestBlacksmithDownloadFileLimit(t *testing.T) {
	var before, after syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &before); err != nil {
		t.Fatal(err)
	}
	shells := []string{"/bin/sh"}
	for _, name := range []string{"bash", "dash"} {
		if shell, err := exec.LookPath(name); err == nil && !containsString(shells, shell) {
			shells = append(shells, shell)
		}
	}
	for _, shell := range shells {
		t.Run(shell, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "tiny")
			args, err := blacksmithDownloadLimitArgs(os.Args[0], []string{"-test.run=^TestBlacksmithDownloadLimitChild$", "--", file}, 1024)
			if err != nil {
				t.Fatal(err)
			}
			result, _ := core.RuntimeForProviderOperation(io.Discard).Exec.Run(t.Context(), LocalCommandRequest{
				Name: shell, Args: args, MaxCapturedOutputBytes: 1024,
				Env: []string{"PATH=/usr/bin:/bin", "POSIXLY_CORRECT=1", "CRABBOX_TEST_DOWNLOAD_LIMIT=1"},
			})
			if !strings.Contains(result.Stdout, "soft=1024 hard=1024") || result.ExitCode == 0 {
				t.Fatalf("child kernel limit differs: exit=%d output=%q", result.ExitCode, result.Stdout)
			}
			info, err := os.Stat(file)
			if err != nil || info.Size() != 1024 {
				t.Fatalf("tiny oversized write escaped file cap: info=%v err=%v", info, err)
			}
		})
	}
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &after); err != nil || before != after {
		t.Fatal("child file limit changed the parent")
	}
}

func TestBlacksmithDownloadLimitChild(t *testing.T) {
	if os.Getenv("CRABBOX_TEST_DOWNLOAD_LIMIT") != "1" {
		return
	}
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		os.Exit(2)
	}
	fmt.Printf("soft=%d hard=%d\n", limit.Cur, limit.Max)
	if err := os.WriteFile(os.Args[len(os.Args)-1], make([]byte, 2048), 0o600); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}
