//go:build smoke

package orgo

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestSmokeCreateRunAndDelete hits the live Orgo REST API.
// Requires CRABBOX_ORGO_API_KEY or ORGO_API_KEY. When CRABBOX_ORGO_WORKSPACE_ID
// and ORGO_WORKSPACE_ID are unset, the test creates and deletes a temporary
// workspace.
//
//	go test -tags smoke -run TestSmokeCreateRunAndDelete -v ./internal/providers/orgo/
func TestSmokeCreateRunAndDelete(t *testing.T) {
	if os.Getenv("CRABBOX_ORGO_API_KEY") == "" && os.Getenv("ORGO_API_KEY") == "" {
		t.Skip("set CRABBOX_ORGO_API_KEY or ORGO_API_KEY to run smoke")
	}
	if testing.Short() {
		t.Skip("live Orgo smoke skipped in -short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	api, err := newOrgoClient(Config{}, Runtime{})
	if err != nil {
		t.Fatalf("newOrgoClient: %v", err)
	}

	workspaceID := strings.TrimSpace(os.Getenv("CRABBOX_ORGO_WORKSPACE_ID"))
	if workspaceID == "" {
		workspaceID = strings.TrimSpace(os.Getenv("ORGO_WORKSPACE_ID"))
	}
	createdWorkspace := ""
	if workspaceID == "" {
		workspace, err := api.CreateWorkspace(ctx, "crabbox-smoke-"+time.Now().UTC().Format("20060102150405"))
		if err != nil {
			t.Fatalf("CreateWorkspace: %v", err)
		}
		workspaceID = workspace.ID
		createdWorkspace = workspace.ID
		t.Logf("created workspace id=%s", workspace.ID)
	}
	if createdWorkspace != "" {
		defer func() {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer stopCancel()
			if err := api.DeleteWorkspace(stopCtx, createdWorkspace); err != nil {
				t.Logf("DeleteWorkspace returned: %v (ignored at teardown)", err)
			}
		}()
	}

	computer, err := api.CreateComputer(ctx, orgoCreateComputerRequest{
		WorkspaceID: workspaceID,
		Name:        "crabbox-smoke",
		OS:          "linux",
		RAMGB:       4,
		CPUs:        1,
		DiskGB:      8,
		Resolution:  "1280x720x24",
	})
	if err != nil {
		t.Fatalf("CreateComputer: %v", err)
	}
	t.Logf("created computer id=%s status=%s", computer.ID, computer.Status)
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer stopCancel()
		if err := api.DeleteComputer(stopCtx, computer.ID); err != nil {
			t.Logf("DeleteComputer returned: %v (ignored at teardown)", err)
		}
	}()

	var out, errBuf bytes.Buffer
	code, err := api.RunBash(ctx, computer.ID, "echo crabbox-orgo-ok && uname -a", &out, &errBuf)
	if err != nil {
		t.Fatalf("RunBash: %v", err)
	}
	t.Logf("bash exit=%d stdout=%q stderr=%q", code, out.String(), errBuf.String())
	if code != 0 {
		t.Fatalf("expected exit 0, got %d (stderr=%q)", code, errBuf.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("crabbox-orgo-ok")) {
		t.Fatalf("stdout missing crabbox-orgo-ok: got %q", out.String())
	}
}
