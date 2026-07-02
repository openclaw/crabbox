package aws

import (
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestNativeCheckpointCapabilitySupportsWindowsImages(t *testing.T) {
	req := core.NativeCheckpointRequest{
		Server:           core.Server{CloudID: "i-123"},
		Target:           core.SSHTarget{TargetOS: core.TargetWindows, WindowsMode: core.WindowsModeNormal},
		Strategy:         core.CheckpointStrategyImage,
		StrategyExplicit: true,
	}

	direct, ok := (Provider{}).NativeCheckpointCapability(req)
	if !ok || direct.Kind != core.CheckpointKindAWSAMI || !direct.Direct {
		t.Fatalf("direct capability=%#v ok=%v, want direct AWS AMI", direct, ok)
	}

	req.Config.Coordinator = "https://coordinator.example"
	brokered, ok := (Provider{}).NativeCheckpointCapability(req)
	if !ok || brokered.Kind != core.CheckpointKindAWSAMI || brokered.Direct {
		t.Fatalf("brokered capability=%#v ok=%v, want brokered AWS AMI", brokered, ok)
	}

	req.Strategy = core.CheckpointStrategyDiskSnapshot
	if capability, ok := (Provider{}).NativeCheckpointCapability(req); ok {
		t.Fatalf("disk snapshot capability=%#v, want unsupported", capability)
	}

	req.StrategyExplicit = false
	automatic, ok := (Provider{}).NativeCheckpointCapability(req)
	if !ok || automatic.Kind != core.CheckpointKindAWSAMI || automatic.Direct {
		t.Fatalf("automatic capability=%#v ok=%v, want brokered AWS AMI", automatic, ok)
	}
}
