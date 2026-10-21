package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Frozen schema-1 envelope wrapping the signed v2 golden from
// worker/test/run-receipt.test.ts at 8f224b05bf677b0d8266af9eca52e309477266cf.
// The envelope is a test fixture, not a recovered historical run. This proves
// local stored-file compatibility, not coordinator database upgrade behavior.
func TestPendingFinishSchemaOneStoredCompatibility(t *testing.T) {
	setAttestTestHome(t)
	raw, err := os.ReadFile("testdata/pending-finish-schema1.json")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "original.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	read, err := readPendingFinishFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var pending pendingRunFinish
	if err := decodePendingFinishJSON(read, &pending); err != nil {
		t.Fatal(err)
	}
	body, receipt, err := validatePendingRunFinish(pending)
	if err != nil {
		t.Fatal(err)
	}
	if pending.SchemaVersion != 1 || receipt.SchemaVersion != 2 || body.ExitCode != 17 || receipt.Signer != "sha256:56475aa75463474c0285df5dbf2bcab73da651358839e9b77481b2eab107708c" || pending.BodySHA256 != "sha256:c7f7d644b7c15002cf75af8792b29021b3dbc89b2a8f0509c50d91a54d3cc5bb" {
		t.Fatal("stored schema or original signed fixture changed")
	}
	encoded, err := json.Marshal(pending)
	if err != nil {
		t.Fatal(err)
	}
	var reloaded pendingRunFinish
	if err := decodePendingFinishJSON(encoded, &reloaded); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reloaded.Body, pending.Body) {
		t.Fatal("outer envelope roundtrip rewrote original request bytes")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatalf("read mutated stored evidence: %v", err)
	}
	keyPath, err := attestKeyPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("loading old evidence created a signing key: %v", err)
	}
	pending.SchemaVersion = 2
	if _, _, err := validatePendingRunFinish(pending); err == nil {
		t.Fatal("unsupported future envelope accepted")
	}
	t.Log("stored schema=1 receipt=2 exact bytes retained; old signature verified without signing key; no coordinator/storage migration exercised")
}
