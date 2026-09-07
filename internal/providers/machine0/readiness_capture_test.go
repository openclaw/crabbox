//go:build !windows

package machine0

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestResolveReadinessOwnsClaimUntilPublicationBeforeCaptureAdmission(t *testing.T) {
	repo := setupState(t)
	api := &fakeAPI{sizes: []machineSize{testSize()}, getSequence: []machine{readyMachine("203.0.113.10")}}
	b := testBackendWithAPI(api)
	lease, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repo}})
	if err != nil {
		t.Fatal(err)
	}
	original, exists, err := resolveClaim(lease.LeaseID)
	if err != nil || !exists {
		t.Fatalf("resolve original claim: exists=%t err=%v", exists, err)
	}
	stopped := api.machine
	stopped.Status, stopped.IP = "STOPPED", ""
	running := stopped
	running.Status, running.IP = "RUNNING", "203.0.113.77"
	api.getSequence = []machine{stopped, running}

	preparingSSH, releaseSSH := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseSSH) }) }
	b.waitSSH = func(context.Context, *SSHTarget, time.Duration) error {
		close(preparingSSH)
		<-releaseSSH
		return nil
	}
	resolved := make(chan error, 1)
	resolveDone := make(chan struct{})
	go func() {
		defer close(resolveDone)
		_, err := b.Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID, StatusOnly: true, ReadyProbe: true})
		resolved <- err
	}()
	t.Cleanup(func() {
		release()
		<-resolveDone
	})
	select {
	case <-preparingSSH:
	case err := <-resolved:
		t.Fatalf("readiness never reached SSH preparation: %v", err)
	}
	if len(api.started) != 1 {
		t.Errorf("readiness did not start exactly once: %v", api.started)
	}

	// Check the actual durable lock, not scheduler timing, while readiness is
	// suspended after Start and before endpoint publication.
	lockPath := filepath.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "claim-locks", lease.LeaseID+".json.lock")
	lock, err := os.OpenFile(lockPath, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	held := err == syscall.EWOULDBLOCK
	if err == nil {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	} else if !held {
		t.Fatal(err)
	}
	if !held {
		t.Error("readiness did not hold the canonical claim lock through SSH preparation")
	}
	admissionEntered := false
	admitOriginal := func() error {
		return core.WithLeaseClaimUnchanged(lease.LeaseID, original, func() error {
			admissionEntered = true
			return core.AuthorizeCheckpointRelease(original, "")
		})
	}
	admitted := make(chan error, 1)
	if held {
		startedAdmission := make(chan struct{})
		go func() {
			close(startedAdmission)
			admitted <- admitOriginal()
		}()
		<-startedAdmission
	} else {
		// The broken implementation admits capture before status publication.
		admitted <- admitOriginal()
	}
	release()
	if err := <-resolved; err != nil {
		t.Fatalf("readiness failed: %v", err)
	}
	if err := <-admitted; err == nil || !strings.Contains(err.Error(), "claim changed") || admissionEntered {
		t.Errorf("stale capture admission: err=%v entered=%t", err, admissionEntered)
	}
	current, exists, err := resolveClaim(lease.LeaseID)
	if err != nil || !exists || current.Revision == original.Revision || current.CloudID != original.CloudID || current.SSHHost != running.IP {
		t.Fatalf("readiness did not publish the new endpoint generation: current=%#v exists=%t err=%v", current, exists, err)
	}
	retried := false
	if err := core.WithLeaseClaimUnchanged(lease.LeaseID, current, func() error {
		retried = true
		return core.AuthorizeCheckpointRelease(current, "")
	}); err != nil || !retried {
		t.Fatalf("capture could not retry with the current claim: err=%v entered=%t", err, retried)
	}
}
