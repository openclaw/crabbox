package cli

import (
	"context"
	"testing"
)

func TestNetworkPublicIgnoresTailscaleMetadata(t *testing.T) {
	cfg := baseConfig()
	cfg.Network = NetworkPublic
	server := Server{Labels: map[string]string{
		"lease":          "cbx_abcdef123456",
		"tailscale":      "true",
		"tailscale_fqdn": "crabbox-blue.example.ts.net",
	}}
	target := SSHTarget{Host: "203.0.113.10", Port: "2222"}
	got, err := resolveNetworkTarget(context.Background(), cfg, server, target)
	if err != nil {
		t.Fatal(err)
	}
	if got.Network != NetworkPublic || got.Target.Host != "203.0.113.10" {
		t.Fatalf("resolve public = network=%s host=%s", got.Network, got.Target.Host)
	}
}

func TestNetworkTailscaleRequiresMetadata(t *testing.T) {
	cfg := baseConfig()
	cfg.Network = NetworkTailscale
	_, err := resolveNetworkTarget(context.Background(), cfg, Server{Labels: map[string]string{"lease": "cbx_abcdef123456"}}, SSHTarget{Host: "203.0.113.10"})
	if err == nil {
		t.Fatal("expected network=tailscale without metadata to fail")
	}
}

func TestRenderTailscaleHostname(t *testing.T) {
	got := renderTailscaleHostname("CBX-{slug}-{provider}-{id}", "cbx_abcdef123456", "Blue Lobster", "aws")
	if got != "cbx-blue-lobster-aws-cbx-abcdef123456" {
		t.Fatalf("renderTailscaleHostname=%q", got)
	}
}

func TestValidateNetworkConfigRejectsStaticProvisioning(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "ssh"
	cfg.Tailscale.Enabled = true
	if err := validateNetworkConfig(cfg); err == nil {
		t.Fatal("expected --tailscale static provider validation failure")
	}
}
