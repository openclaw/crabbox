package daytona

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestDaytonaStopCurrentRepositoryRejectsTransferredLease(t *testing.T) {
	f, b, req := newFixedDaytonaFixture(t)
	lease, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	claim, _, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	newRoot := t.TempDir()
	if _, err := b.reclaimFixed(t.Context(), claim, newRoot, true); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	config := fmt.Sprintf("provider: daytona\ntarget: linux\ndaytona:\n  apiUrl: %s\n  apiKey: test-credential\n", f.server.URL)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_DAYTONA_API_KEY", "test-credential")
	t.Chdir(req.Repo.Root)
	var out, diagnostic bytes.Buffer
	app := core.App{Stdout: &out, Stderr: &diagnostic}
	args := []string{"stop", "--current-repo", "--id", lease.LeaseID}
	before := len(f.paths)
	if err := app.Run(t.Context(), args); err == nil || !strings.Contains(err.Error(), "claimed by") {
		t.Fatalf("old repository cleanup=%v", err)
	}
	if len(f.paths) != before || f.deletes != 0 {
		t.Fatal("previous repository reached provider cleanup")
	}
	t.Chdir(newRoot)
	current, _, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	executionHeld, finishExecution, executionDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		executionDone <- core.WithLeaseClaimUnchangedShared(t.Context(), lease.LeaseID, current, func() error { close(executionHeld); <-finishExecution; return nil })
	}()
	<-executionHeld
	stopCtx, cancelStop := context.WithTimeout(t.Context(), 50*time.Millisecond)
	stopErr := app.Run(stopCtx, args)
	cancelStop()
	close(finishExecution)
	if err := <-executionDone; err != nil {
		t.Fatal(err)
	}
	if !errors.Is(stopErr, context.DeadlineExceeded) || len(f.paths) != before || f.deletes != 0 {
		t.Fatalf("scoped release did not wait for active execution: %v", stopErr)
	}
	if err := app.Run(t.Context(), args); err != nil {
		t.Fatalf("current repository cleanup=%v", err)
	}
	if f.deletes != 1 {
		t.Fatalf("native deletions=%d", f.deletes)
	}
	before = len(f.paths)
	t.Chdir(req.Repo.Root)
	if err := app.Run(t.Context(), args); err != nil {
		t.Fatalf("terminal replay=%v", err)
	}
	if len(f.paths) != before || f.deletes != 1 {
		t.Fatal("terminal replay performed provider operations")
	}
}

func TestDaytonaOrdinaryStopRemainsRepositoryIndependent(t *testing.T) {
	f, b, req := newFixedDaytonaFixture(t)
	lease, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	if err := b.Stop(t.Context(), core.StopRequest{ID: lease.LeaseID}); err != nil {
		t.Fatal(err)
	}
	if f.deletes != 1 {
		t.Fatal("ordinary administrative stop no longer releases the lease")
	}
}
