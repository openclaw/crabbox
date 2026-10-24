//go:build windows

package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestControllerStateLockWindowsIsExclusiveAndReleasable(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state", "controller.json")
	first, err := acquireControllerStateLock(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireControllerStateLock(statePath); err == nil || !strings.Contains(err.Error(), "already locked") {
		t.Fatalf("second acquire error=%v", err)
	}
	if err := first.Unlock(); err != nil {
		t.Fatal(err)
	}
	second, err := acquireControllerStateLock(statePath)
	if err != nil {
		t.Fatalf("acquire after unlock: %v", err)
	}
	if err := second.Unlock(); err != nil {
		t.Fatal(err)
	}
}
