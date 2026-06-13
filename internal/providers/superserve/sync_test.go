package superserve

import (
	"bytes"
	"strings"
	"testing"
)

func TestCheckSuperserveSyncPreflightUsesFullArchiveCandidate(t *testing.T) {
	cfg := testConfig()
	cfg.Sync.FailBytes = 100
	manifest := SyncManifest{
		Files:        []string{"large.bin"},
		Changed:      []string{"small.txt"},
		Bytes:        200,
		ChangedBytes: 1,
	}
	var stderr bytes.Buffer

	err := checkSuperserveSyncPreflight(manifest, cfg, false, &stderr)
	if err == nil || !strings.Contains(err.Error(), "candidate too large") {
		t.Fatalf("err=%v, want full archive candidate guardrail", err)
	}
	if strings.Contains(stderr.String(), "dirty_delta") {
		t.Fatalf("stderr=%q, want full candidate preflight, not dirty delta", stderr.String())
	}
}
