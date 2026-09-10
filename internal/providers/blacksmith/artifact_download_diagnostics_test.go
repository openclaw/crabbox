//go:build darwin || linux

package blacksmith

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBlacksmithDownloadFailureDiagnostics(t *testing.T) {
	for _, kind := range []string{"native-failure", "cancellation", "stdout-file-collision", "stdout-link-collision", "stdout-dir-collision", "replaced-stage", "symlink-stage", "over-limit"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("CRABBOX_CONTROLLER_PROCESS_TREE_OWNED", "")
			toolsDir, tempParent, repo := t.TempDir(), t.TempDir(), t.TempDir()
			for _, name := range []string{"blacksmith", "scp"} {
				if err := os.WriteFile(filepath.Join(toolsDir, name), []byte("#!/bin/sh\nexit 99\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", toolsDir+":/usr/bin:/bin")
			t.Setenv("TMPDIR", tempParent)
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			cause := errors.New("synthetic cancellation")
			nativeFailure := errors.New("synthetic native process exit")
			streams := map[string]string{
				"stdout": "NATIVE_STDOUT_SENTINEL\x1e\x1f\n",
				"stderr": "NATIVE_STDERR_SENTINEL\n",
			}
			if kind == "over-limit" {
				streams["stdout"] = strings.Repeat("x", int(blacksmithArtifactDiagnosticCaptureBytes)+1)
			}
			var stage, originalStage string
			var console bytes.Buffer
			preserved := map[string]string{}
			calls := 0
			backend := newTestBlacksmithBackend(baseConfig(), ownershipRunner(func(runCtx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
				calls++
				if runCtx != ctx || req.Dir != repo || !req.RequireProcessGroupJoin || req.MaxCapturedOutputBytes != int(blacksmithArtifactDiagnosticCaptureBytes) || req.Stdout != nil || req.Stderr != nil {
					t.Fatal("download changed its context, closure, capture bounds or stream privacy")
				}
				stage = filepath.Dir(req.Args[len(req.Args)-1])
				collision := filepath.Join(stage, "native-download.stdout.log")
				switch kind {
				case "stdout-file-collision":
					preserved[collision] = "existing unrelated file"
				case "stdout-link-collision":
					outside := filepath.Join(tempParent, "unrelated")
					preserved[outside] = "unrelated link target"
					if err := os.Symlink(outside, collision); err != nil {
						t.Fatal(err)
					}
				case "stdout-dir-collision":
					if err := os.Mkdir(collision, 0o700); err != nil {
						t.Fatal(err)
					}
					preserved[filepath.Join(collision, "unrelated")] = "existing unrelated child"
				case "replaced-stage", "symlink-stage":
					originalStage = stage + ".original"
					if err := os.Rename(stage, originalStage); err != nil {
						t.Fatal(err)
					}
					if kind == "symlink-stage" {
						if err := os.Symlink(originalStage, stage); err != nil {
							t.Fatal(err)
						}
					} else if err := os.Mkdir(stage, 0o700); err != nil {
						t.Fatal(err)
					}
					preserved[filepath.Join(stage, "unrelated")] = "replacement directory content"
				}
				for path, text := range preserved {
					if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "cancellation" {
					cancel(cause)
				}
				return LocalCommandResult{ExitCode: 23, Stdout: streams["stdout"], Stderr: streams["stderr"]}, nativeFailure
			}))
			backend.rt.Stdout, backend.rt.Stderr = &console, &console
			remote := ".crabbox/blacksmith-artifact-" + strings.Repeat("a", 64) + "/archive.tgz"
			data, err := backend.downloadArtifact(ctx, repo, "tbx_diagnostics", "/synthetic/key", remote, 1, fmt.Sprintf("%x", sha256.Sum256([]byte("x"))))
			if data != nil || !errors.Is(err, nativeFailure) || !strings.Contains(err.Error(), "exit 23") || calls != 1 {
				t.Fatalf("diagnostics changed the primary failure or published bytes: calls=%d data=%v err=%v", calls, data, err)
			}
			if kind == "cancellation" && (!errors.Is(err, context.Canceled) || !errors.Is(err, cause)) {
				t.Errorf("diagnostic persistence lost cancellation: %v", err)
			}
			for path, want := range preserved {
				got, readErr := os.ReadFile(path)
				if readErr != nil || string(got) != want {
					t.Errorf("unrelated content changed at %s: %v", path, readErr)
				}
			}
			if originalStage != "" {
				if !strings.Contains(err.Error(), "identity changed") {
					t.Errorf("stage replacement was not diagnosed: %v", err)
				}
				for _, directory := range []string{stage, originalStage} {
					for stream := range streams {
						if _, statErr := os.Lstat(filepath.Join(directory, "native-download."+stream+".log")); !errors.Is(statErr, os.ErrNotExist) {
							t.Errorf("wrote through a changed stage: %s, %v", directory, statErr)
						}
					}
				}
				return
			}
			for stream, want := range streams {
				path := filepath.Join(stage, "native-download."+stream+".log")
				if stream == "stdout" && strings.Contains(kind, "collision") {
					if !errors.Is(err, os.ErrExist) {
						t.Errorf("diagnostic collision was not retained as secondary failure: %v", err)
					}
					continue
				}
				if stream == "stdout" && kind == "over-limit" {
					if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
						t.Errorf("oversized diagnostics reached disk: %v", statErr)
					}
					continue
				}
				got, readErr := os.ReadFile(path)
				info, statErr := os.Lstat(path)
				if readErr != nil || statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || string(got) != want {
					t.Errorf("native %s diagnostics were not privately retained: read=%v stat=%v", stream, readErr, statErr)
				}
				metadata := fmt.Sprintf("%s=%q bytes=%d sha256=%x", stream, path, len(want), sha256.Sum256([]byte(want)))
				if !strings.Contains(err.Error(), metadata) {
					t.Errorf("missing retained diagnostic metadata: %v", err)
				}
				if strings.Contains(err.Error()+console.String(), want) {
					t.Errorf("native %s diagnostic contents escaped into console/error", stream)
				}
			}
			assertNoBlacksmithArtifactPublication(t, repo, "tbx_diagnostics")
		})
	}
}
