//go:build darwin || linux

package blacksmith

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestBlacksmithArtifactCapabilityProbeCancellation(t *testing.T) {
	for _, customCause := range []bool{false, true} {
		name := "caller-cancel"
		if customCause {
			name = "caller-cause"
		}
		t.Run(name, func(t *testing.T) {
			isolateBlacksmithOwnership(t)
			t.Setenv("CRABBOX_CONTROLLER_PROCESS_TREE_OWNED", "")
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			cause := error(context.Canceled)
			if customCause {
				cause = errors.New("synthetic caller cancellation cause")
			}
			calls := 0
			backend := newTestBlacksmithBackend(core.BaseConfig(), ownershipRunner(func(runCtx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				calls++
				if req.Name != "blacksmith" || !slices.Equal(req.Args, []string{"testbox", "download", "--help"}) {
					t.Errorf("unexpected request after capability cancellation: %s", req.Name)
					return core.LocalCommandResult{ExitCode: 2}, errors.New("unexpected request")
				}
				cancel(cause)
				return core.LocalCommandResult{ExitCode: -1}, runCtx.Err()
			}))
			_, ended, artifacts, err := backend.runArtifactTestbox(ctx, core.RunRequest{
				Repo: core.Repo{Root: t.TempDir()}, Command: []string{"true"}, ArtifactGlobs: []string{"report"},
			}, "tbx_capability_cancel", nil, nil, nil, time.Second)
			if !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
				t.Errorf("capability cancellation lost its classification or cause: %v", err)
			}
			if calls != 1 || !ended.IsZero() || len(artifacts) != 0 {
				t.Errorf("canceled probe continued: calls=%d ended=%v artifacts=%d", calls, ended, len(artifacts))
			}
			if err != nil && strings.Contains(err.Error(), "update the Blacksmith CLI") {
				t.Errorf("cancellation was relabeled as unsupported download: %v", err)
			}
		})
	}
}

type blacksmithPublicationCancellation struct {
	context.Context
	target string
	cancel context.CancelFunc
}

func (c blacksmithPublicationCancellation) Err() error {
	if _, err := os.Lstat(c.target); err == nil {
		c.cancel()
	}
	return c.Context.Err()
}

func TestBlacksmithArtifactPublicationCancellation(t *testing.T) {
	target := filepath.Join(t.TempDir(), "accepted.tgz")
	base, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := blacksmithPublicationCancellation{Context: base, target: target, cancel: cancel}
	err := writeBlacksmithRunArchive(ctx, target, []byte("tiny"), time.Now().Add(time.Second))
	// The cancellation trigger is the published filesystem entry. On the old
	// path it remains observable here because there was no postpublication check.
	if ctx.Err() != context.Canceled {
		t.Fatal("publication cancellation trigger was never observed")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("publication returned success despite cancellation at the target boundary: %v", err)
	}
	if _, statErr := os.Lstat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("canceled publication left an accepted target: %v", statErr)
	}
}

func TestBlacksmithArtifactPublicationPreservesExistingTarget(t *testing.T) {
	target := filepath.Join(t.TempDir(), "accepted.tgz")
	if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeBlacksmithRunArchive(t.Context(), target, []byte("replacement"), time.Now().Add(time.Second)); err == nil {
		t.Error("existing publication target was replaced")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "original" {
		t.Fatalf("existing target bytes changed: %q err=%v", data, err)
	}
}

func TestBlacksmithArtifactPublicationPreservesCompletedFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "accepted.tgz")
	if err := writeBlacksmithRunArchive(t.Context(), target, []byte("complete archive"), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "complete archive" {
		t.Fatalf("published archive bytes changed: data=%q error=%v", data, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "accepted.tgz" || !entries[0].Type().IsRegular() {
		t.Fatalf("publication did not retain exactly the completed file: entries=%v error=%v", entries, err)
	}
}

func TestBlacksmithArtifactPublicationExpiredDeadline(t *testing.T) {
	target := filepath.Join(t.TempDir(), "accepted.tgz")
	ctx := t.Context()
	err := writeBlacksmithRunArchive(ctx, target, []byte("tiny"), time.Now().Add(-time.Second))
	if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatalf("publication did not enforce its cutoff independently of cancellation: %v", err)
	}
	if _, statErr := os.Lstat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("expired publication left an accepted target: %v", statErr)
	}
}
