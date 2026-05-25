//go:build smoke

package wandb

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"
)

// TestSmokeVersionAndExec hits the live CoreWeave Sandboxes gateway.
// Requires WANDB_ENTITY_NAME plus CRABBOX_WANDB_API_KEY or WANDB_API_KEY.
// Sandbox lifetime capped at 60s.
//
//	go test -tags smoke -run TestSmokeVersionAndExec -v ./internal/providers/wandb/
func TestSmokeVersionAndExec(t *testing.T) {
	if os.Getenv("CRABBOX_WANDB_API_KEY") == "" && os.Getenv("WANDB_API_KEY") == "" {
		t.Skip("set CRABBOX_WANDB_API_KEY or WANDB_API_KEY to run smoke")
	}
	if os.Getenv("WANDB_ENTITY_NAME") == "" {
		t.Skip("set WANDB_ENTITY_NAME to run smoke")
	}
	if testing.Short() {
		t.Skip("live sandbox skipped in -short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	api, err := newWandbClient(Config{}, Runtime{})
	if err != nil {
		t.Fatalf("newWandbClient: %v", err)
	}

	v, err := api.Version(ctx)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	t.Logf("connected; api version = %s", v)

	sb, err := api.Acquire(ctx, wandbAcquireRequest{
		Image:           "ubuntu:24.04",
		MaxLifetimeSecs: 60,
		Tags:            []string{"crabbox-smoke"},
	})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Logf("acquired sandbox id=%s status=%s", sb.ID, sb.Status)

	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer stopCancel()
		if err := api.Stop(stopCtx, sb.ID, 5, true); err != nil {
			t.Logf("Stop returned: %v (ignored at teardown)", err)
		} else {
			t.Logf("stopped sandbox %s", sb.ID)
		}
	}()

	var out, errBuf bytes.Buffer
	code, err := api.Exec(ctx, wandbExecRequest{
		SandboxID: sb.ID,
		Command:   []string{"sh", "-c", "echo hello-from-crabbox && id"},
		Timeout:   30,
		Stdout:    &out,
		Stderr:    &errBuf,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	t.Logf("exec exit=%d stdout=%q stderr=%q", code, out.String(), errBuf.String())
	if code != 0 {
		t.Fatalf("expected exit 0, got %d (stderr=%q)", code, errBuf.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("hello-from-crabbox")) {
		t.Fatalf("stdout missing hello-from-crabbox: got %q", out.String())
	}
}
