//go:build darwin || linux

package blacksmith

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestBlacksmithArtifactRefusesControllerOwnedGroup(t *testing.T) {
	for _, boundary := range []string{"run-options", "artifact-supervisor"} {
		t.Run(boundary, func(t *testing.T) {
			isolateBlacksmithOwnership(t)
			t.Setenv("CRABBOX_CONTROLLER_PROCESS_TREE_OWNED", "1")
			calls := 0
			backend := newTestBlacksmithBackend(core.BaseConfig(), ownershipRunner(func(context.Context, core.LocalCommandRequest) (core.LocalCommandResult, error) {
				calls++
				return core.LocalCommandResult{}, errors.New("unexpected native call")
			}))
			req := core.RunRequest{Repo: core.Repo{Root: t.TempDir()}, Command: []string{"true"}, ArtifactGlobs: []string{"report"}}
			var err error
			if boundary == "run-options" {
				_, err = backend.Run(t.Context(), req)
			} else {
				_, _, _, err = backend.runArtifactTestbox(t.Context(), req, "tbx_unsupported", nil, nil, nil, time.Second)
			}
			if err == nil || !strings.Contains(err.Error(), "joined local command groups cannot run inside a controller-owned process group") || calls != 0 {
				t.Fatalf("unsupported group reached native work: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestBlacksmithDownloadCancellationRetainsClaimUntilClosure(t *testing.T) {
	isolateBlacksmithOwnership(t)
	t.Setenv("CRABBOX_CONTROLLER_PROCESS_TREE_OWNED", "")
	root := t.TempDir()
	toolsDir := filepath.Join(root, "bin")
	if err := os.Mkdir(toolsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", toolsDir+":/usr/bin:/bin")
	t.Setenv("TMPDIR", root)
	ready, writes := filepath.Join(root, "ready"), filepath.Join(root, "writes")
	// These are local fixtures, not native Blacksmith or scp. The finite child
	// writes at most sixteen bytes, including if cancellation regresses.
	fakeNative := "#!/bin/sh\nfor arg do destination=$arg; done\nscp \"$destination\"\n"
	fakeSCP := "#!/bin/sh\nprintf x > \"$1\"\n(\n" +
		"  i=0\n  while [ \"$i\" -lt 16 ]; do\n" +
		"    printf x >> " + core.ShellQuote(writes) + "\n" +
		"    if [ \"$i\" -eq 0 ]; then printf ready > " + core.ShellQuote(ready) + "; fi\n" +
		"    sleep 0.02\n    i=$((i + 1))\n  done\n) &\nwait\n"
	for name, source := range map[string]string{"blacksmith": fakeNative, "scp": fakeSCP} {
		if err := os.WriteFile(filepath.Join(toolsDir, name), []byte(source), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	const id = "tbx_download_owner"
	claim := testOwnedBlacksmithClaim(t, id, "jade-krill", root)
	route, _, _ := blacksmithClaimBinding(claim)
	realRunner := core.RuntimeForProviderOperation(io.Discard).Exec
	var downloads atomic.Int32
	backend := newTestBlacksmithBackend(core.BaseConfig(), ownershipRunner(func(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if req.Name == "blacksmith" && len(req.Args) >= 2 && req.Args[0] == "testbox" && req.Args[1] == "status" {
			return core.LocalCommandResult{Stdout: testBlacksmithStatus(id, "ready")}, nil
		}
		if req.Name != "/bin/sh" || req.Dir != root || !req.RequireProcessGroupJoin {
			return core.LocalCommandResult{ExitCode: 2}, errors.New("download did not require inline process-group closure")
		}
		downloads.Add(1)
		return realRunner.Run(ctx, req)
	}))
	backend.route, backend.claim = &route, &claim
	deadline, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	ctx, cancel := context.WithCancel(deadline)
	defer cancel()
	var helperReturned atomic.Bool
	done := make(chan error, 1)
	go func() {
		done <- backend.withOwnedTestbox(ctx, claim, func() error {
			digest := sha256.Sum256([]byte("x"))
			data, err := backend.downloadArtifact(ctx, root, id, "/synthetic/key", ".crabbox/blacksmith-artifact-"+strings.Repeat("a", 64)+"/archive.tgz", 1, hex.EncodeToString(digest[:]))
			helperReturned.Store(true)
			if err == nil || len(data) != 0 {
				return errors.New("canceled download returned artifact bytes")
			}
			return err
		})
	}()
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("download closed before its tiny writer started: %v", err)
		case <-deadline.Done():
			t.Fatal("tiny writer did not become ready")
		case <-time.After(5 * time.Millisecond):
		}
	}
	type writerOutcome struct {
		bytes int64
		err   error
	}
	writerStarted := make(chan struct{})
	writerDone := make(chan writerOutcome, 1)
	go func() {
		close(writerStarted)
		var size int64
		err := core.WithDurableLeaseClaimLockContext(deadline, id, func(current *core.LeaseClaim, exists bool, save func() error) error {
			if !helperReturned.Load() {
				return errors.New("claim writer entered before download owner returned")
			}
			info, err := os.Stat(writes)
			if err != nil {
				return err
			}
			size = info.Size()
			current.RepoRoot = filepath.Join(root, "replacement")
			return save()
		})
		writerDone <- writerOutcome{size, err}
	}()
	<-writerStarted
	select {
	case result := <-writerDone:
		t.Fatalf("claim writer crossed active download: %v", result.err)
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("download cancellation was lost: %v", err)
		}
	case <-deadline.Done():
		t.Fatal("download owner did not close")
	}
	var outcome writerOutcome
	select {
	case outcome = <-writerDone:
		if outcome.err != nil {
			t.Fatal(outcome.err)
		}
	case <-deadline.Done():
		t.Fatal("claim writer remained blocked after download closure")
	}
	time.Sleep(100 * time.Millisecond)
	info, err := os.Stat(writes)
	if err != nil || info.Size() != outcome.bytes || info.Size() < 1 || info.Size() > 16 || downloads.Load() != 1 {
		t.Fatalf("tiny writer continued after claim release: before=%d after=%v downloads=%d err=%v", outcome.bytes, info, downloads.Load(), err)
	}
}
