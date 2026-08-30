package hetzner

import (
	"context"
	core "github.com/openclaw/crabbox/internal/cli"
	"strings"
	"testing"
)

func TestCheckpointRetirementRequiresProjectIdentity(t *testing.T) {
	old := newHetznerClient
	newHetznerClient = func() (hetznerClient, error) { t.Fatal("unscoped retirement called provider"); return nil, nil }
	t.Cleanup(func() { newHetznerClient = old })
	capability, ok := (Provider{}).NativeCheckpointCapability(core.NativeCheckpointRequest{Config: core.Config{TargetOS: core.TargetLinux}, Server: core.Server{CloudID: "123"}})
	if !ok || !capability.Direct || capability.RetireSource || capability.RetireUnsupported == "" || capability.CreateUnsupported != "" {
		t.Fatalf("ordinary snapshot/capture capability=%+v", capability)
	}
	b := &hetznerLeaseBackend{}
	absent, err := b.CheckpointSourceAbsent(context.Background(), core.CheckpointSourceRequest{Capture: core.NativeCheckpointCapture{SourceID: "123"}})
	if absent || err == nil || !strings.Contains(err.Error(), "project identity") {
		t.Fatalf("unscoped absence=%v %v", absent, err)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{CheckpointID: "chk_legacy"}); err == nil {
		t.Fatal("legacy retirement released without project identity")
	}
	if _, err := (Provider{}).CreateNativeCheckpoint(context.Background(), core.NativeCheckpointCreateRequest{Capture: &core.NativeCheckpointCapture{Phase: "prepared"}}); err == nil {
		t.Fatal("unscoped capture reached provider")
	}
}
