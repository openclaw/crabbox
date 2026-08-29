package azure

import (
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestNativeCheckpointRetirementPreservesBrokeredDiskRoute(t *testing.T) {
	for _, tc := range []struct {
		name, coordinator, target, strategy string
		retire                              bool
	}{
		{"brokered Linux disk", "https://coordinator.invalid", core.TargetLinux, core.CheckpointStrategyDiskSnapshot, true},
		{"unsupported managed image", "https://coordinator.invalid", core.TargetLinux, core.CheckpointStrategyImage, false},
		{"direct Windows restore cycle", "", core.TargetWindows, core.CheckpointStrategyDiskSnapshot, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capability, ok := (Provider{}).NativeCheckpointCapability(core.NativeCheckpointRequest{Config: core.Config{Coordinator: tc.coordinator, TargetOS: tc.target, WindowsMode: core.WindowsModeNormal}, Server: core.Server{CloudID: "fixture-source"}, Strategy: tc.strategy})
			if !ok || capability.RetireSource != tc.retire {
				t.Fatalf("capability=%+v ok=%v", capability, ok)
			}
		})
	}
}
