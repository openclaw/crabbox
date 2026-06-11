//go:build darwin && arm64

package applevzhelper

import (
	"bytes"
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
