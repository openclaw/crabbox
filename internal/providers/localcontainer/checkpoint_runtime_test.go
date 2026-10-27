package localcontainer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestNativeCheckpointOCIRuntimeCommitFailure(t *testing.T) {
	dir := t.TempDir()
	runtimePath := filepath.Join(dir, "docker")
	script := `#!/bin/sh
if [ "$1" = --context ]; then shift 2; fi
case "$1" in
  context) printf '%s\n' 'unix:///synthetic/docker.sock' ;;
  info) printf '%s\n' 'synthetic-daemon' ;;
  exec) printf '%s\n' '/workspace/repo' ;;
  inspect) printf '%s\n' '[]' ;;
  commit) printf '%s\n' 'runtime does not support commit' >&2; exit 1 ;;
  *) exit 99 ;;
esac
`
	if err := os.WriteFile(runtimePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	labels := checkpointScopeMetadata(checkpointScope{
		Runtime: runtimePath, Context: "synthetic", Config: dir,
		Endpoint: "unix:///synthetic/docker.sock", DaemonID: "synthetic-daemon",
	})
	labels["oci_runtime"] = "runsc"
	result, err := createNativeCheckpoint(t.Context(), core.NativeCheckpointCreateRequest{
		Server:  core.Server{CloudID: "synthetic-container", Labels: labels},
		Workdir: "/workspace/repo",
	})
	if err == nil || !strings.Contains(err.Error(), "OCI runtime runsc") || !strings.Contains(err.Error(), "--mode archive") {
		t.Fatalf("missing actionable runtime failure: %v", err)
	}
	if result.CaptureEstablished || result.Image.ID != "" {
		t.Fatalf("failed commit published a checkpoint: %+v", result)
	}
}
