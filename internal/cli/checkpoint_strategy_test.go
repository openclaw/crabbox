package cli

import "testing"

func TestCheckpointNativeModeAndRetirementStrategyParity(t *testing.T) {
	for _, tc := range []struct {
		name, provider, coordinator string
		implicit, image, disk       string
	}{
		{"direct AWS Linux", "aws", "", checkpointKindAWSAMI, checkpointKindAWSAMI, "unsupported"},
		{"broker AWS Linux", "aws", "https://coordinator.example", checkpointKindAWSEBS, checkpointKindAWSAMI, checkpointKindAWSEBS},
		{"broker Azure Linux", "azure", "https://coordinator.example", checkpointKindAzureOS, checkpointKindAzure, checkpointKindAzureOS},
		{"broker GCP Linux", "gcp", "https://coordinator.example", checkpointKindGCPDisk, checkpointKindGCP, checkpointKindGCPDisk},
		{"Docker", "local-container", "", checkpointKindDockerCommit, "unsupported", "unsupported"},
	} {
		for _, strategy := range []string{"", checkpointStrategyAuto, checkpointStrategyImage, checkpointStrategyDiskSnapshot} {
			t.Run(tc.name+"/"+blank(strategy, "default"), func(t *testing.T) {
				cfg := defaultConfig()
				cfg.Provider, cfg.TargetOS, cfg.Coordinator = tc.provider, targetLinux, tc.coordinator
				server := Server{Provider: tc.provider, CloudID: "fixture-source"}
				target := SSHTarget{TargetOS: targetLinux}
				want := tc.implicit
				switch strategy {
				case checkpointStrategyImage:
					want = tc.image
				case checkpointStrategyDiskSnapshot:
					want = tc.disk
				}
				if got := checkpointCreateMode("native", strategy, cfg, server, target, false); got != want {
					t.Fatalf("ordinary native kind=%s want=%s", got, want)
				}
				capability, ok := nativeModeCheckpointCapability(cfg, server, target, strategy)
				if want == "unsupported" {
					if ok {
						t.Fatalf("unsupported explicit strategy admitted: %+v", capability)
					}
					return
				}
				if !ok || capability.Kind != want {
					t.Fatalf("retirement selected=%+v ok=%v want=%s", capability, ok, want)
				}
				// Replay uses the persisted selection and the original explicitness,
				// including Docker's implicit image and AWS's automatic AMI.
				replay, ok := nativeCheckpointCapability(NativeCheckpointRequest{Config: cfg, Server: server, Target: target, Strategy: checkpointCreateStrategy("native", strategy, capability.Kind), StrategyExplicit: !isAutoCheckpointStrategy(strategy)})
				if !ok || replay != capability {
					t.Fatalf("replay changed prepared capability: selected=%+v replay=%+v ok=%v", capability, replay, ok)
				}
			})
		}
	}
}
