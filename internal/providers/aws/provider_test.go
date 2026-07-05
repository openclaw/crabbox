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

func TestPrepareLeaseClaimEndpointPreservesCleanupIdentity(t *testing.T) {
	existing := core.LeaseClaim{
		LeaseID: "cbx_123456789abc",
		CloudID: "i-123",
		Slug:    "example",
		Labels: map[string]string{
			"lease":           "cbx_123456789abc",
			"slug":            "example",
			"aws_key_pair_id": "key-original",
			"aws_account_id":  "123456789012",
		},
	}
	server := core.Server{
		Provider: "aws",
		CloudID:  "i-123",
		Labels: map[string]string{
			"provider": "aws",
			"lease":    "cbx_123456789abc",
			"slug":     "example",
			"state":    "running",
		},
	}

	prepared, err := (Provider{}).PrepareLeaseClaimEndpoint(existing, "aws", "example", server, false)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Labels["aws_key_pair_id"] != "key-original" || prepared.Labels["aws_account_id"] != "123456789012" {
		t.Fatalf("labels=%v, want preserved cleanup identity", prepared.Labels)
	}
	server.Labels["aws_key_pair_id"] = "key-replacement"
	if _, err := (Provider{}).PrepareLeaseClaimEndpoint(existing, "aws", "example", server, false); err == nil {
		t.Fatal("expected mismatched immutable key identity rejection")
	}

	legacy := existing
	legacy.Labels = map[string]string{"lease": existing.LeaseID, "slug": existing.Slug}
	server.Labels["aws_key_pair_id"] = "key-cloud-tag"
	server.Labels["aws_account_id"] = "999999999999"
	prepared, err = (Provider{}).PrepareLeaseClaimEndpoint(legacy, "aws", "example", server, false)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Labels["aws_key_pair_id"] != "" || prepared.Labels["aws_account_id"] != "" {
		t.Fatalf("labels=%v, must not promote mutable cloud tags into cleanup authority", prepared.Labels)
	}
}
