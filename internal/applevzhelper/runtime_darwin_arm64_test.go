//go:build darwin && arm64 && cgo

package applevzhelper

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Code-Hex/vz/v3"
)

func TestHandleVMStateErrorPersistsTerminalErrorAndRequestsStop(t *testing.T) {
	stateRoot := t.TempDir()
	name := "vm-error"
	inst := Instance{
		Name:      name,
		Status:    StatusRunning,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := os.MkdirAll(InstanceDir(stateRoot, name), 0o755); err != nil {
		t.Fatalf("create instance dir: %v", err)
	}

	result := handleVMState(vz.VirtualMachineStateError, &inst, stateRoot, name, &bytes.Buffer{})

	if !result.done {
		t.Fatal("expected error state to end serve loop")
	}
	if !result.requestStop {
		t.Fatal("expected error state to request VM stop")
	}
	if result.err == nil || !strings.Contains(result.err.Error(), "vm entered error state") {
		t.Fatalf("unexpected result error: %v", result.err)
	}
	if inst.Status != StatusError || inst.Error != "vm entered VirtualMachineStateError" {
		t.Fatalf("instance status=%q error=%q", inst.Status, inst.Error)
	}
	persisted, err := readMetadata(MetadataPath(stateRoot, name))
	if err != nil {
		t.Fatalf("read persisted metadata: %v", err)
	}
	if persisted.Status != StatusError || persisted.Error != "vm entered VirtualMachineStateError" {
		t.Fatalf("persisted status=%q error=%q", persisted.Status, persisted.Error)
	}
}

func TestResolveSourceImageRequiresChecksumForRemoteImages(t *testing.T) {
	_, err := resolveSourceImage(context.Background(), t.TempDir(), "https://example.test/image.img", "")
	if err == nil {
		t.Fatal("expected remote image without checksum to fail")
	}
	if !strings.Contains(err.Error(), "requires --image-sha256") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveSourceImageVerifiesRemoteImageChecksum(t *testing.T) {
	payload := []byte("fake image")
	sum := sha256.Sum256(payload)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	t.Cleanup(server.Close)

	path, err := resolveSourceImage(context.Background(), t.TempDir(), server.URL+"/image.img", hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("cached payload=%q", string(got))
	}

	_, err = resolveSourceImage(context.Background(), t.TempDir(), server.URL+"/image.img", strings.Repeat("0", 64))
	if err == nil {
		t.Fatal("expected checksum mismatch to fail")
	}
}

func TestResolveSourceImageVerifiesLocalImageWhenChecksumProvided(t *testing.T) {
	path := t.TempDir() + "/image.img"
	payload := []byte("local image")
	sum := sha256.Sum256(payload)
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	resolved, err := resolveSourceImage(context.Background(), t.TempDir(), path, "sha256:"+hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	if resolved != path {
		t.Fatalf("resolved=%q want %q", resolved, path)
	}

	_, err = resolveSourceImage(context.Background(), t.TempDir(), path, strings.Repeat("f", 64))
	if err == nil {
		t.Fatal("expected local checksum mismatch to fail")
	}
}
