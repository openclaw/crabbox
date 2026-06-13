//go:build windows

package cli

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveControllerFileRecoversDeterministicTombstone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.json")
	tombstone := path + ".deleted"
	if err := os.WriteFile(tombstone, []byte("stale sensitive state"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeControllerFile(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tombstone); !os.IsNotExist(err) {
		t.Fatalf("recovered tombstone still exists: %v", err)
	}
}

func TestRemoveControllerFileDeletesOriginalAndTombstone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.json")
	if err := os.WriteFile(path, []byte("state"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeControllerFile(path); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{path, path + ".deleted"} {
		if _, err := os.Stat(candidate); !os.IsNotExist(err) {
			t.Fatalf("removed path %s still exists: %v", candidate, err)
		}
	}
}

func TestConfirmedAbsentStateCleanupRecoversControllerTombstones(t *testing.T) {
	t.Run("external routing", func(t *testing.T) {
		setExternalRoutingTestHome(t)
		const leaseID = "cbx_123456789abc"
		path, err := ExternalRoutingPath(leaseID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".deleted", []byte("stale routing state"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := removeExternalRoutingIfUnchangedWithSync(leaseID, ExternalConfig{}, func(string) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path + ".deleted"); !os.IsNotExist(err) {
			t.Fatalf("routing tombstone remains: %v", err)
		}
	})

	t.Run("lease claim", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		const leaseID = "cbx_123456789abc"
		path, err := leaseClaimPath(leaseID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".deleted", []byte("stale claim state"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := cleanupLeaseClaimIfUnchangedAfterWithSync(leaseID, leaseClaim{}, false, nil, func(string) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path + ".deleted"); !os.IsNotExist(err) {
			t.Fatalf("claim tombstone remains: %v", err)
		}
	})

	t.Run("webvnc daemon identity", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		const leaseID = "cbx_123456789abc"
		_, path, err := webVNCDaemonPaths(leaseID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".deleted", []byte("stale WebVNC identity"), 0o600); err != nil {
			t.Fatal(err)
		}
		stopped, err := (App{Stdout: io.Discard, Stderr: io.Discard}).stopWebVNCDaemonIfRunning(leaseID)
		if err != nil {
			t.Fatal(err)
		}
		if stopped {
			t.Fatal("absent WebVNC identity reported a stopped daemon")
		}
		if _, err := os.Stat(path + ".deleted"); !os.IsNotExist(err) {
			t.Fatalf("WebVNC identity tombstone remains: %v", err)
		}
	})
}
