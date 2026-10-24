package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

const tailnetTestKey = "tskey-auth-not-a-real-enrollment-key"
const tailnetTestGeneration = "8be39656-32b0-4c47-b68c-0a9e1d3ef901"

func tailnetTestConfig() Config {
	cfg := baseConfig()
	cfg.Provider, cfg.TargetOS, cfg.Network = "proxmox", targetLinux, NetworkTailscale
	cfg.SSHPort = "22"
	cfg.Tailscale = TailscaleConfig{Enabled: true, Tags: []string{"tag:worker"}, HostnameTemplate: "cbx-{id}"}
	broker, _ := filepath.Abs(filepath.Join("trusted", "broker"))
	cfg.Proxmox = ProxmoxConfig{APIURL: "https://pve.example.test:8006", Node: "pve1", TemplateID: 9400, TailscaleBroker: broker}
	return cfg
}

func tailnetTestServer() Server {
	return Server{Provider: "proxmox", CloudID: "417", ID: 417, HostID: "pve1", ImmutableID: tailnetTestGeneration, Labels: map[string]string{
		"crabbox": "true", "provider": "proxmox", "lease": "cbx_123456abcdef", "fixed_intent_sha256": "bound-intent", "tailscale": "true", "tailscale_tags": "tag:worker", "tailscale_hostname": "cbx-test", "state": "leased",
	}}
}

func TestProxmoxTailnetTrustedBrokerConfigSources(t *testing.T) {
	var file fileProxmoxConfig
	if err := json.Unmarshal([]byte(`{"tailscaleBroker":"/untrusted/program"}`), &file); err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig()
	if err := applyProxmoxFileConfig(&cfg, &file, configInputRepo, credentialSourceForFile(false)); err != nil {
		t.Fatal(err)
	}
	if cfg.Proxmox.TailscaleBroker != "" {
		t.Fatal("repository can select executable")
	}
	if err := applyProxmoxFileConfig(&cfg, &file, configInputUser, credentialSourceTrustedFile); err != nil {
		t.Fatal(err)
	}
	if cfg.Proxmox.TailscaleBroker != "/untrusted/program" {
		t.Fatal("user opt-in missing")
	}
	t.Setenv("CRABBOX_PROXMOX_TAILSCALE_BROKER", "/trusted/from-env")
	if err := applyProxmoxEnvironmentConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Proxmox.TailscaleBroker != "/trusted/from-env" {
		t.Fatal("environment opt-in missing")
	}
}

func TestProxmoxTailnetSelectsOnlyTailscale0(t *testing.T) {
	for _, tc := range []struct{ name, interfaces, want string }{
		{"underlay first", `[{"name":"eth0","ip-addresses":[{"ip-address-type":"ipv4","ip-address":"10.77.1.32"}]},{"name":"tailscale0","ip-addresses":[{"ip-address-type":"ipv4","ip-address":"100.101.2.3"}]}]`, "100.101.2.3"},
		{"no interface", `[{"name":"eth0","ip-addresses":[{"ip-address-type":"ipv4","ip-address":"100.101.2.3"}]}]`, ""},
		{"wrong case", `[{"name":"Tailscale0","ip-addresses":[{"ip-address-type":"ipv4","ip-address":"100.101.2.3"}]}]`, ""},
		{"not cgnat", `[{"name":"tailscale0","ip-addresses":[{"ip-address-type":"ipv4","ip-address":"10.77.1.32"}]}]`, ""},
		{"below range", `[{"name":"tailscale0","ip-addresses":[{"ip-address-type":"ipv4","ip-address":"100.63.255.255"}]}]`, ""},
		{"above range", `[{"name":"tailscale0","ip-addresses":[{"ip-address-type":"ipv4","ip-address":"100.128.0.0"}]}]`, ""},
		{"ipv6", `[{"name":"tailscale0","ip-addresses":[{"ip-address-type":"ipv6","ip-address":"fd7a:115c:a1e0::1"}]}]`, ""},
		{"deterministic", `[{"name":"tailscale0","ip-addresses":[{"ip-address-type":"ipv4","ip-address":"100.70.0.2"},{"ip-address-type":"ipv4","ip-address":"100.70.0.1"}]}]`, "100.70.0.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var interfaces []proxmoxAgentInterface
			if err := json.Unmarshal([]byte(tc.interfaces), &interfaces); err != nil {
				t.Fatal(err)
			}
			got, err := proxmoxInterfaceIPv4(interfaces, true)
			if got != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("got=%s err=%v", got, err)
			}
		})
	}
}

func TestProxmoxTailnetRequiresProviderEnrollmentHooks(t *testing.T) {
	cfg := tailnetTestConfig()
	for _, client := range []*ProxmoxClient{{}, {GuestAdmission: func(Config, string, string, map[string]string) (map[string]string, error) { return nil, nil }}, {GuestEnrollment: func(context.Context, Config, Server) error { return nil }}} {
		_, err := client.CreateServerWithVMID(t.Context(), cfg, "fixture", "cbx_123456abcdef", "native", true, 417, map[string]string{"fixed_intent_sha256": "bound"}, func(Server) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "provider-owned") {
			t.Fatalf("missing hook admitted: %v", err)
		}
	}
}
