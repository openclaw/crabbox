//go:build smoke

package localcontainer

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// CRABBOX_LIVE=1 go test -tags smoke -run TestLiveLocalContainerOCIRuntimeRunc -v ./internal/providers/localcontainer
func TestLiveLocalContainerOCIRuntimeRunc(t *testing.T) {
	runtime := liveDockerSetup(t)
	if !isDockerRuntime(runtime) {
		t.Skip("OCI runtime selection currently supports Docker only")
	}
	cfg := liveDockerConfig(runtime)
	cfg.LocalContainer.OCIRuntime = "runc"
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{
		Exec: &liveDockerRunner{runtime: runtime}, Stdout: io.Discard, Stderr: io.Discard,
	}).(*backend)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	leaseID := core.NewLeaseID()
	id, dir, err := b.createContainer(ctx, cfg, core.LeaseProviderName(leaseID, "oci-smoke"), leaseID, "oci-smoke", "ssh-ed25519 fixture", false)
	if dir != "" {
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
	}
	if id != "" {
		t.Cleanup(func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cleanupCancel()
			if out, err := exec.CommandContext(cleanupCtx, runtime, "rm", "-f", id).CombinedOutput(); err != nil {
				t.Errorf("cleanup exact test container: %v: %s", err, out)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(ctx, runtime, "inspect", id, "--format", `{{json .HostConfig.Runtime}} {{json (index .Config.Labels "oci_runtime")}}`).CombinedOutput()
	if err != nil {
		t.Fatalf("inspect: %v: %s", err, out)
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		t.Fatalf("unexpected inspection: %s", out)
	}
	for _, field := range fields {
		var value string
		if err := json.Unmarshal([]byte(field), &value); err != nil || value != "runc" {
			t.Fatalf("runtime did not round-trip: %s", out)
		}
	}
}
