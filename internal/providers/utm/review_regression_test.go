package utm

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestResolveRejectsForeignRepositoryBeforeSSH(t *testing.T) {
	b, _ := setup(t)
	lease, err := b.Acquire(t.Context(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	readyCalls := 0
	waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
		readyCalls++
		return nil
	}
	if _, err = b.Resolve(t.Context(), core.ResolveRequest{ID: lease.LeaseID, Repo: core.Repo{Root: t.TempDir()}}); err == nil {
		t.Fatal("foreign repository was admitted")
	}
	if readyCalls != 0 {
		t.Fatalf("foreign repository executed %d SSH readiness probes before rejection", readyCalls)
	}
}

func TestResolveHoldsClaimFenceDuringSSH(t *testing.T) {
	b, _ := setup(t)
	repo := core.Repo{Root: t.TempDir()}
	lease, err := b.Acquire(t.Context(), core.AcquireRequest{Repo: repo, Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	probed := false
	waitForSSHReady = func(ctx context.Context, _ *core.SSHTarget, _ io.Writer, _ string, _ time.Duration) error {
		probed = true
		attempt, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		entered := false
		err := core.WithLeaseClaimUnchangedContext(attempt, lease.LeaseID, claim, func() error { entered = true; return nil })
		if entered || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("SSH readiness ran outside claim fence: entered=%t error=%v", entered, err)
		}
		return nil
	}
	if _, err = b.Resolve(t.Context(), core.ResolveRequest{ID: lease.LeaseID, Repo: repo}); err != nil {
		t.Fatal(err)
	}
	if !probed {
		t.Fatal("valid owner did not perform readiness")
	}
}

type wrongSnapshotRunner struct{ next core.CommandRunner }

func (r wrongSnapshotRunner) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	if req.Name == "/usr/bin/osascript" && len(req.Args) == 1 && req.Args[0] == "-" {
		data, err := io.ReadAll(req.Stdin)
		if err != nil {
			return core.LocalCommandResult{}, err
		}
		if strings.Contains(string(data), "create snapshot targetVM") {
			return core.LocalCommandResult{Stdout: unrelatedSnapshotUUID}, nil
		}
		req.Stdin = strings.NewReader(string(data))
	}
	return r.next.Run(ctx, req)
}

func TestCaptureRejectsExistingSnapshotReceipt(t *testing.T) {
	b, f, lease, _ := checkpointSetup(t)
	b.rt.Exec = wrongSnapshotRunner{next: f}
	result, receipts, err := captureFixture(t, b, lease)
	if err == nil {
		t.Fatal("capture accepted a pre-existing snapshot")
	}
	for _, receipt := range append(receipts, result) {
		if receipt.Image.ID == unrelatedSnapshotUUID {
			t.Fatal("capture persisted deletion authority for an unrelated snapshot")
		}
	}
	if len(f.snapshots) != 1 || f.snapshots[0].ID != unrelatedSnapshotUUID {
		t.Fatal("unrelated snapshot changed")
	}
}

func TestRunPreparationUsesAlreadyHeldClaimFence(t *testing.T) {
	b, _ := setup(t)
	repo := core.Repo{Root: t.TempDir()}
	lease, err := b.Acquire(t.Context(), core.AcquireRequest{Repo: repo, Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	resolver, ok := any(b).(core.RunLeaseClaimResolver)
	if !ok {
		t.Fatal("UTM reuse bypasses core run claim admission")
	}
	unlock, err := b.lock(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err = core.WithLeaseClaimUnchangedContext(ctx, lease.LeaseID, claim, func() error {
		resolved, err := resolver.ResolveRunLeaseUnderClaim(ctx, core.ResolveRequest{ID: lease.LeaseID, Repo: repo, Prepare: true}, claim)
		if err == nil && (resolved.LeaseID != lease.LeaseID || resolved.Server.CloudID != lease.Server.CloudID) {
			t.Error("run preparation changed the owned resource")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestControlRejectsUnsupportedHostBeforeCommands(t *testing.T) {
	for _, host := range []struct{ os, arch string }{{"linux", "arm64"}, {"windows", "amd64"}, {"darwin", "amd64"}} {
		t.Run(host.os+"/"+host.arch, func(t *testing.T) {
			b, f := setup(t)
			b.hostOS, b.hostArch = host.os, host.arch
			if _, err := b.command(t.Context(), "/usr/bin/osascript", []string{"-"}, nil); err == nil {
				t.Fatal("unsupported host accepted")
			}
			if len(f.calls) != 0 {
				t.Fatal("unsupported host executed a local command")
			}
		})
	}
}
