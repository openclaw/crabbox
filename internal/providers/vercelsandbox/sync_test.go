package vercelsandbox

import (
	"bytes"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestVercelSandboxSyncPreflightIgnoresChangedBytesForPortableArchive(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Sync.FailBytes = 10
	manifest := SyncManifest{
		Bytes:        1,
		ChangedBytes: 2 << 20,
		Files:        []string{"a.txt"},
		Changed:      []string{"a.txt"},
	}
	var stderr bytes.Buffer
	if err := checkVercelSandboxSyncPreflight(manifest, cfg, false, &stderr); err != nil {
		t.Fatalf("preflight should use archive total bytes, not changed bytes: %v", err)
	}
}
