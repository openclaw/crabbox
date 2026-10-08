//go:build windows

package lume

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRelockLumeConfigAfterDirectoryRenameRejectsHeldLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	must(t, os.WriteFile(path, []byte(`{}`), 0o600))
	contender, err := openLumeConfigForLock(path)
	must(t, err)
	locked, err := tryExclusiveFileLock(contender)
	if err != nil || !locked {
		t.Fatalf("lock contender=%t err=%v", locked, err)
	}
	relocked, locked, err := relockLumeConfigAfterDirectoryRename(path)
	if err != nil {
		t.Fatal(err)
	}
	if locked || relocked != nil {
		t.Fatal("relocked config while another process held the identity lock")
	}
	must(t, unlockFile(contender))
	must(t, contender.Close())

	relocked, locked, err = relockLumeConfigAfterDirectoryRename(path)
	if err != nil || !locked || relocked == nil {
		t.Fatalf("relock after release=%t file=%v err=%v", locked, relocked, err)
	}
	must(t, unlockFile(relocked))
	must(t, relocked.Close())
}
