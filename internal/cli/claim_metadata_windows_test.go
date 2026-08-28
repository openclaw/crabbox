//go:build windows

package cli

import (
	"io"
	"testing"
)

func TestClaimMetadataOpenReaderAllowsReplacementAndCleanup(t *testing.T) {
	isolateTestUserDirs(t)
	before := seedClaimContract(t)
	path, err := leaseClaimPath(before.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	// Hold the same OS opener used by claim reads across both namespace changes.
	oldFile, err := openArtifactReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer oldFile.Close()
	replacement := cloneLeaseClaim(before)
	replacement.RepoRoot = "/repo/replacement"
	updated, err := replaceLeaseClaimIfUnchangedDurableReturning(before.LeaseID, before, replacement)
	if err != nil {
		t.Fatalf("replace with old reader open: %v", err)
	}
	currentFile, err := openArtifactReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer currentFile.Close()
	if err := removeLeaseClaimIfUnchanged(updated.LeaseID, updated); err != nil {
		t.Fatalf("guarded cleanup with current reader open: %v", err)
	}
	if _, exists, err := readLeaseClaimWithPresence(updated.LeaseID); err != nil || exists {
		t.Fatalf("claim remains after cleanup: exists=%t err=%v", exists, err)
	}
	for _, snapshot := range []struct {
		reader io.Reader
		want   leaseClaim
	}{{oldFile, before}, {currentFile, updated}} {
		data, err := io.ReadAll(snapshot.reader)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decodeLeaseClaim(path, data)
		if err != nil || got.Revision != snapshot.want.Revision || got.RepoRoot != snapshot.want.RepoRoot {
			t.Fatalf("open reader lost published snapshot: got=%#v err=%v", got, err)
		}
	}
}
