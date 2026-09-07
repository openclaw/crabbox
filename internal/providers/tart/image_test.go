package tart

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func tartImageFixture(t *testing.T) (string, []byte, []byte) {
	t.Helper()
	dir := t.TempDir()
	config := []byte(`{"os":"darwin","arch":"arm64","cpuCount":4,"macAddress":"86:44:0f:12:49:26"}`)
	nvram := []byte("reviewed firmware bytes")
	chunks := [][]byte{[]byte("first disk chunk"), []byte("second disk chunk")}
	digest := func(data []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(data)) }
	manifest := tartImageManifest{Layers: []tartImageLayer{
		{MediaType: "application/vnd.cirruslabs.tart.config.v1", Size: int64(len(config)), Digest: digest(config)},
		{MediaType: "application/vnd.cirruslabs.tart.nvram.v1", Size: int64(len(nvram)), Digest: digest(nvram)},
	}}
	var disk []byte
	for _, chunk := range chunks {
		manifest.Layers = append(manifest.Layers, tartImageLayer{MediaType: "application/vnd.cirruslabs.tart.disk.v2", Annotations: map[string]string{
			"org.cirruslabs.tart.uncompressed-size": fmt.Sprint(len(chunk)), "org.cirruslabs.tart.uncompressed-content-digest": digest(chunk),
		}})
		disk = append(disk, chunk...)
	}
	manifest.Annotations = map[string]string{"org.cirruslabs.tart.uncompressed-disk-size": fmt.Sprint(len(disk))}
	for name, data := range map[string][]byte{"config.json": config, "nvram.bin": nvram, "disk.img": disk} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return dir, data, config
}

func TestVerifyTartImageFiles(t *testing.T) {
	for _, tc := range []struct {
		name, file string
		mutate     func([]byte) []byte
	}{
		{"matching image", "", nil},
		{"native regenerated MAC", "config.json", func(b []byte) []byte {
			return []byte(strings.ReplaceAll(string(b), "86:44:0f:12:49:26", "02:01:02:03:04:05"))
		}},
		{"changed hardware", "config.json", func(b []byte) []byte { return []byte(strings.ReplaceAll(string(b), `"cpuCount":4`, `"cpuCount":8`)) }},
		{"extra config field", "config.json", func(b []byte) []byte { return append([]byte(`{"extra":true,`), b[1:]...) }},
		{"trailing config", "config.json", func(b []byte) []byte { return append(b, []byte(`{}`)...) }},
		{"duplicate root key", "config.json", func(b []byte) []byte {
			return []byte(strings.ReplaceAll(string(b), `"cpuCount":4`, `"cpuCount":8,"cpuCount":4`))
		}},
		{"duplicate escaped key", "config.json", func(b []byte) []byte {
			return []byte(strings.ReplaceAll(string(b), `"cpuCount":4`, `"cpuCount":8,"cpu\u0043ount":4`))
		}},
		{"invalid MAC", "config.json", func(b []byte) []byte { return []byte(strings.ReplaceAll(string(b), "86:44:0f:12:49:26", "invalid")) }},
		{"multicast MAC", "config.json", func(b []byte) []byte {
			return []byte(strings.ReplaceAll(string(b), "86:44:0f:12:49:26", "ff:ff:ff:ff:ff:ff"))
		}},
		{"changed firmware", "nvram.bin", func(b []byte) []byte { b[0] ^= 1; return b }},
		{"changed first chunk", "disk.img", func(b []byte) []byte { b[0] ^= 1; return b }},
		{"changed last chunk", "disk.img", func(b []byte) []byte { b[len(b)-1] ^= 1; return b }},
		{"truncated disk", "disk.img", func(b []byte) []byte { return b[:len(b)-1] }},
		{"extended disk", "disk.img", func(b []byte) []byte { return append(b, 0) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, manifest, config := tartImageFixture(t)
			if tc.mutate != nil {
				path := filepath.Join(dir, tc.file)
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, tc.mutate(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err := verifyTartImageFiles(context.Background(), dir, manifest, config)
			wantOK := tc.name == "matching image" || tc.name == "native regenerated MAC"
			if (err == nil) != wantOK {
				t.Fatalf("verification error=%v; wantOK=%v", err, wantOK)
			}
		})
	}
}

func TestVerifyTartImageConfigRejectsNestedDuplicateKeys(t *testing.T) {
	for _, tc := range []struct{ name, actual, reviewed string }{
		{"nested object", `{"macAddress":"02:01:02:03:04:05","display":{"width":2048,"width":1024,"height":768}}`, `{"macAddress":"02:01:02:03:04:05","display":{"width":1024,"height":768}}`},
		{"object inside array", `{"macAddress":"02:01:02:03:04:05","devices":[{"name":"bad","name":"good"}]}`, `{"macAddress":"02:01:02:03:04:05","devices":[{"name":"good"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := verifyTartImageConfig([]byte(tc.actual), []byte(tc.reviewed)); err == nil {
				t.Fatal("accepted configuration with duplicate JSON keys")
			}
		})
	}
}

func TestVerifyTartImageConfigBoundsNesting(t *testing.T) {
	_, _, config := tartImageFixture(t)
	deep := []byte(`{"macAddress":"02:01:02:03:04:05","extra":` + strings.Repeat("[", 10001) + "0" + strings.Repeat("]", 10001) + "}")
	if err := verifyTartImageConfig(deep, config); err == nil {
		t.Fatal("accepted excessive JSON nesting")
	}
}

func TestVerifyTartImageFilesRejectUnsafeFiles(t *testing.T) {
	for _, mode := range []string{"missing disk", "symlink", "suspended", "canceled", "unbound metadata"} {
		t.Run(mode, func(t *testing.T) {
			dir, manifest, config := tartImageFixture(t)
			ctx := context.Background()
			switch mode {
			case "missing disk":
				if err := os.Remove(filepath.Join(dir, "disk.img")); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				path := filepath.Join(dir, "nvram.bin")
				if err := os.Rename(path, path+".real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".real", path); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			case "suspended":
				if err := os.WriteFile(filepath.Join(dir, "state.vzvmsave"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "unbound metadata":
				config = append(config, ' ')
			}
			if err := verifyTartImageFiles(ctx, dir, manifest, config); err == nil {
				t.Fatal("verification accepted unsafe image")
			} else if mode == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
		})
	}
}

func TestDefaultTartImageMetadata(t *testing.T) {
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(defaultTartManifest))
	if !strings.HasSuffix(core.DefaultTartImage, "@"+digest) {
		t.Fatal("default reference does not bind embedded manifest")
	}
	var manifest tartImageManifest
	if err := json.Unmarshal(defaultTartManifest, &manifest); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, layer := range manifest.Layers {
		if layer.MediaType == "application/vnd.cirruslabs.tart.config.v1" {
			found = true
			if layer.Digest != fmt.Sprintf("sha256:%x", sha256.Sum256(defaultTartConfig)) || layer.Size != int64(len(defaultTartConfig)) {
				t.Fatal("config not bound to manifest")
			}
		}
	}
	if !found {
		t.Fatal("missing config")
	}
	if core.BaseConfig().Tart.Image != core.DefaultTartImage {
		t.Fatal("core default differs from reviewed image")
	}
	if got, err := verifyDefaultTartImage(context.Background(), "my-custom-image:latest", "/does/not/exist", "custom"); err != nil || got != "" {
		t.Fatalf("custom image was checked: %q %v", got, err)
	}
}

func TestTartLegacyInventoryDoesNotInventImageProvenance(t *testing.T) {
	cfg := core.BaseConfig()
	b := newBackend((Provider{}).Spec(), cfg, core.Runtime{}).(*backend)
	server := b.serverFromInstance(tartInstance{Name: "legacy", State: "stopped", Source: "local"}, core.LeaseClaim{}, cfg)
	if server.Labels["image"] != "" || server.Labels["image_digest"] != "" {
		t.Fatalf("legacy VM received fabricated image provenance: %v", server.Labels)
	}
}

func TestAcquireRejectsUnverifiedDefaultBeforeConfigureOrBoot(t *testing.T) {
	testutil.IsolateUserDirs(t)
	t.Setenv("TART_HOME", t.TempDir())
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{"list": {Stdout: "[]"}}}
	runner.onRun = func(req core.LocalCommandRequest) {
		if len(req.Args) == 3 && req.Args[0] == "clone" {
			if req.Args[1] != core.DefaultTartImage {
				t.Fatalf("clone reference=%s", req.Args[1])
			}
			if err := os.MkdirAll(filepath.Join(os.Getenv("TART_HOME"), "vms", req.Args[2]), 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	cfg := core.BaseConfig()
	b := newBackend((Provider{}).Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	_, err := b.Acquire(context.Background(), core.AcquireRequest{})
	if err == nil || !strings.Contains(err.Error(), "verify built-in Tart image before boot") {
		t.Fatalf("Acquire error=%v", err)
	}
	var deleted bool
	for _, call := range runner.calls {
		switch call.Args[0] {
		case "set", "run", "exec":
			t.Fatalf("unverified VM executed %s", call.Args[0])
		case "delete":
			deleted = true
		}
	}
	if !deleted {
		t.Fatal("rejected owned clone was not deleted")
	}
}
