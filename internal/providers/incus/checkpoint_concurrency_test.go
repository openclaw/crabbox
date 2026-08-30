package incus

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestIncusCaptureCannotResurrectDeletedCheckpoint(t *testing.T) {
	backend, fake, req := lifecycleFixture(t)
	t.Setenv("CRABBOX_PROVIDER", "incus")
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "config.yaml"))
	t.Chdir(req.Repo.Root)
	source, err := backend.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	observed, proceed := make(chan struct{}), make(chan struct{})
	var paused atomic.Bool
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(proceed) }) }
	defer release()
	fake.afterImageRead = func() {
		if paused.CompareAndSwap(false, true) {
			close(observed)
			<-proceed
		}
	}
	app := core.App{Stdout: io.Discard, Stderr: io.Discard}
	done := make(chan error, 1)
	go func() {
		done <- app.Run(context.Background(), []string{"checkpoint", "create", "--provider", "incus", "--id", source.LeaseID, "--mode", "native", "--name", "concurrent-capture"})
	}()
	select {
	case <-observed:
	case err := <-done:
		t.Fatalf("capture ended before publication: %v", err)
	case <-time.After(10 * time.Second):
		release()
		<-done
		t.Fatal("capture did not reach its published-image boundary")
	}

	var output bytes.Buffer
	if err := (core.App{Stdout: &output, Stderr: io.Discard}).Run(context.Background(), []string{"checkpoint", "list", "--json"}); err != nil {
		release()
		<-done
		t.Fatal(err)
	}
	var records []struct{ ID, Name string }
	if err := json.Unmarshal(output.Bytes(), &records); err != nil || len(records) != 1 || records[0].Name != "concurrent-capture" {
		release()
		<-done
		t.Fatalf("pending capture record: %s (%v)", output.String(), err)
	}

	deleteErr := app.Run(context.Background(), []string{"checkpoint", "delete", records[0].ID})
	release()
	if err := <-done; err != nil {
		t.Fatalf("capture completion: %v", err)
	}
	if deleteErr == nil || !strings.Contains(deleteErr.Error(), "busy") {
		t.Fatalf("concurrent deletion must report the active capture, got %v", deleteErr)
	}
	if len(fake.images) != 1 {
		t.Fatal("concurrent deletion removed the active capture's image")
	}
	if err := app.Run(context.Background(), []string{"checkpoint", "delete", records[0].ID}); err != nil {
		t.Fatalf("delete completed capture: %v", err)
	}
	output.Reset()
	if err := (core.App{Stdout: &output, Stderr: io.Discard}).Run(context.Background(), []string{"checkpoint", "list", "--json"}); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(output.Bytes(), &records); err != nil || len(records) != 0 || len(fake.images) != 0 {
		t.Fatalf("completed deletion left a checkpoint or image: %s (%v)", output.String(), err)
	}
}
