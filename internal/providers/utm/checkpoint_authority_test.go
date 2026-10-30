package utm

import (
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestUTMCheckpointCaptureRetainsAdmittedClaim(t *testing.T) {
	for _, scenario := range []string{"current", "reclaimed-repository", "same-owner-new-revision", "missing-snapshot", "absent-snapshot", "missing-current-claim", "empty-snapshot-repository"} {
		t.Run(scenario, func(t *testing.T) {
			b, f, lease, _ := checkpointSetup(t)
			original, err := core.ReadLeaseClaim(lease.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			carried, exists, set := core.ServerLeaseClaimSnapshot(lease.Server)
			if !set || !exists || !reflect.DeepEqual(carried, original) {
				t.Fatal("fixture did not carry its production acquisition authority")
			}
			expected, expectedExists := original, true
			switch scenario {
			case "reclaimed-repository":
				expected, err = b.claimTarget(lease, original.Slug, t.TempDir(), true, original, true)
			case "same-owner-new-revision":
				expected, err = core.ReplaceLeaseClaimIfUnchangedDurableReturning(lease.LeaseID, original, original)
			case "missing-snapshot":
				lease.Server = core.Server{Provider: lease.Server.Provider, CloudID: lease.Server.CloudID, Labels: lease.Server.Labels}
			case "absent-snapshot":
				core.SetServerLeaseClaimSnapshot(&lease.Server, core.LeaseClaim{}, false)
			case "missing-current-claim":
				err = core.RemoveLeaseClaimIfUnchanged(lease.LeaseID, original)
				expected, expectedExists = core.LeaseClaim{}, false
			case "empty-snapshot-repository":
				carried.RepoRoot = ""
				core.SetServerLeaseClaimSnapshot(&lease.Server, carried, true)
			}
			if err != nil {
				t.Fatal(err)
			}
			if (scenario == "reclaimed-repository" || scenario == "same-owner-new-revision") && expected.Revision == original.Revision {
				t.Fatal("fixture did not publish a different claim generation")
			}
			r, err := b.lookup(lease.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			path, err := recordPath(r.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			journal, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			before := append([]nativeSnapshot(nil), f.snapshots...)
			result, receipts, err := captureFixture(t, b, lease)
			if scenario == "current" {
				if err != nil || !result.CaptureEstablished || len(f.mutations) != 1 || f.mutations[0] != "create" {
					t.Fatalf("current admitted owner could not capture: result=%+v mutations=%v err=%v", result, f.mutations, err)
				}
				metadata, err := checkpointMetadataFrom(resourceFixture(result))
				if err != nil || metadata.RepoRoot != original.RepoRoot || metadata.SourceRevision != original.Revision {
					t.Fatalf("capture receipt lost admitted authority: metadata=%+v err=%v", metadata, err)
				}
				return
			}
			var notSubmitted core.NativeCheckpointNotSubmittedError
			if err == nil || !errors.As(err, &notSubmitted) || result.CaptureEstablished || len(receipts) != 0 || len(f.mutations) != 0 {
				t.Fatalf("invalid admitted authority reached capture: err=%v established=%t receipts=%d mutations=%v", err, result.CaptureEstablished, len(receipts), f.mutations)
			}
			if !reflect.DeepEqual(f.snapshots, before) {
				t.Fatal("rejected capture changed unrelated snapshots")
			}
			after, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			if err != nil || exists != expectedExists || !reflect.DeepEqual(after, expected) {
				t.Fatalf("rejected capture changed the current claim: exists=%t err=%v", exists, err)
			}
			currentJournal, err := os.ReadFile(path)
			if err != nil || string(currentJournal) != string(journal) {
				t.Fatal("rejected capture changed its ownership journal")
			}
		})
	}
}

type captureFenceRunner struct {
	next   core.CommandRunner
	probe  func(context.Context)
	probed bool
}

func (r *captureFenceRunner) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	if req.Name == "/usr/bin/osascript" && len(req.Args) == 1 && req.Args[0] == "-" {
		data, err := io.ReadAll(req.Stdin)
		if err != nil {
			return core.LocalCommandResult{}, err
		}
		req.Stdin = strings.NewReader(string(data))
		if strings.Contains(string(data), "create snapshot targetVM") {
			r.probed = true
			r.probe(ctx)
		}
	}
	return r.next.Run(ctx, req)
}

func TestUTMCheckpointCaptureFencesAdmittedClaimThroughSubmission(t *testing.T) {
	b, f, lease, _ := checkpointSetup(t)
	original, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	runner := &captureFenceRunner{next: f, probe: func(ctx context.Context) {
		attempt, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		replacement := original
		replacement.RepoRoot = t.TempDir()
		_, err := core.ReplaceLeaseClaimIfUnchangedDurableReturningContext(attempt, lease.LeaseID, original, replacement)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("native submission did not hold admitted claim fence: %v", err)
		}
	}}
	b.rt.Exec = runner
	result, _, err := captureFixture(t, b, lease)
	if err != nil || !result.CaptureEstablished || !runner.probed {
		t.Fatalf("capture/submission probe incomplete: established=%t probed=%t err=%v", result.CaptureEstablished, runner.probed, err)
	}
	after, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil || !reflect.DeepEqual(after, original) {
		t.Fatal("submission probe changed admitted claim")
	}
}
