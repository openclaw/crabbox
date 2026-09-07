package gcp

import (
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestNativeCheckpointRetirementPreservesBrokeredRoutes(t *testing.T) {
	for _, strategy := range []string{core.CheckpointStrategyDiskSnapshot, core.CheckpointStrategyImage} {
		t.Run(strategy, func(t *testing.T) {
			req := core.NativeCheckpointRequest{Config: core.Config{Coordinator: "https://coordinator.invalid", TargetOS: core.TargetLinux}, Server: core.Server{CloudID: "fixture-source"}, Strategy: strategy}
			capability, ok := (Provider{}).NativeCheckpointCapability(req)
			if !ok || !capability.RetireSource {
				t.Fatalf("brokered route lost retirement: %+v ok=%v", capability, ok)
			}
			req.Config.Coordinator = ""
			if capability, ok := (Provider{}).NativeCheckpointCapability(req); ok || capability.RetireSource {
				t.Fatalf("unsupported direct route enabled: %+v ok=%v", capability, ok)
			}
		})
	}
}
