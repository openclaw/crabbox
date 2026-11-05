package cli

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openclaw/crabbox/internal/runtimeartifact"
)

func TestNativeRuntimeCachePinsVerifiedInodes(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "cache")
	run := func(script string) (string, error) {
		t.Helper()
		script = strings.ReplaceAll(script, `cache="/tmp/crabbox-runtime-cache-$(id -u)"`, "cache="+shellQuote(cache))
		out, err := exec.Command("/bin/sh", "-c", script).CombinedOutput()
		return string(out), err
	}
	newRuntime := func(body string) *remoteNativeRuntime {
		t.Helper()
		nonce, err := randomHex(16)
		if err != nil {
			t.Fatal(err)
		}
		r := &remoteNativeRuntime{nonce: nonce, path: "/tmp/crabbox-runtime-" + nonce + "/crabbox"}
		t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(r.path)) })
		if body != "" {
			if err := os.Mkdir(filepath.Dir(r.path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(filepath.Dir(r.path), ".nonce"), []byte(nonce), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(r.path, []byte(body), 0500); err != nil {
				t.Fatal(err)
			}
			r.identity = runtimeartifact.Identity{SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(body)))}
		}
		return r
	}
	first := newRuntime("#!/bin/sh\nprintf first\n")
	if out, err := run(nativeRuntimeCachePublishScript(first)); err != nil {
		t.Fatalf("publish: %v: %s", err, out)
	}
	pin := newRuntime("")
	pin.identity = first.identity
	if out, err := run(nativeRuntimeCacheProbeScript(pin)); err != nil || out != "first" {
		t.Fatalf("hit: %v: %q", err, out)
	}
	second := newRuntime("#!/bin/sh\nprintf second\n")
	if out, err := run(nativeRuntimeCachePublishScript(second)); err != nil {
		t.Fatalf("replacement: %v: %s", err, out)
	}
	if out, err := exec.Command(pin.path).Output(); err != nil || string(out) != "first" {
		t.Fatalf("replacement changed active pin: %v: %s", err, out)
	}
	for _, invalid := range []string{"wrong digest", "partial", "symlink"} {
		t.Run(invalid, func(t *testing.T) {
			candidate := newRuntime("")
			candidate.identity = first.identity
			if invalid != "wrong digest" {
				if err := os.Remove(filepath.Join(cache, "artifact")); err != nil {
					t.Fatal(err)
				}
				var err error
				if invalid == "partial" {
					err = os.WriteFile(filepath.Join(cache, "artifact"), nil, 0500)
				} else {
					err = os.Symlink(first.path, filepath.Join(cache, "artifact"))
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if out, err := run(nativeRuntimeCacheProbeScript(candidate)); err != nil || out != "CBX-CACHE-MISS\n" {
				t.Fatalf("invalid cache executed: %v: %q", err, out)
			}
			if _, err := os.Lstat(filepath.Dir(candidate.path)); !os.IsNotExist(err) {
				t.Fatalf("invalid pin retained: %v", err)
			}
		})
	}
	if err := os.Chmod(cache, 0777); err != nil {
		t.Fatal(err)
	}
	if out, err := run(nativeRuntimeCacheProbeScript(newRuntime(""))); err == nil {
		t.Fatalf("unsafe cache directory accepted: %s", out)
	}
}
