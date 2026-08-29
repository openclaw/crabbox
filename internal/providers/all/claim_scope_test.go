package all

import (
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestConfigClaimScopeLegacyParity(t *testing.T) {
	tests := []struct {
		name, provider string
		cfg            core.Config
		want           string
	}{
		{"azure canonical account", "azure", core.Config{AzureSubscription: " SUB ", AzureResourceGroup: " Group "}, "subscription:sub|resource-group:group"},
		{"azure incomplete", "azure", core.Config{AzureSubscription: "sub"}, ""},
		{"gcp legacy whitespace", "google", core.Config{GCPProject: " project "}, "project: project "},
		{"gcp empty", "gcp", core.Config{}, ""},
		{"cube default port", "cubesandbox", core.Config{CubeSandbox: core.CubeSandboxConfig{APIURL: "HTTPS://user:pass@CUBE.EXAMPLE:443/root/?token=secret#fragment"}}, "endpoint:https://cube.example/root"},
		{"cube nondefault port", "cubesandbox", core.Config{CubeSandbox: core.CubeSandboxConfig{APIURL: "http://CUBE.EXAMPLE:8080/root/"}}, "endpoint:http://cube.example:8080/root"},
		{"cube schemeless legacy", "cubesandbox", core.Config{CubeSandbox: core.CubeSandboxConfig{APIURL: "CUBE.EXAMPLE/root///"}}, "endpoint:CUBE.EXAMPLE/root"},
		{"cube ipv6", "cubesandbox", core.Config{CubeSandbox: core.CubeSandboxConfig{APIURL: "https://[2001:DB8::1]:443/root/"}}, "endpoint:https://[2001:db8::1]/root"},
		{"e2b normalized", "e2b", core.Config{E2B: core.E2BConfig{APIURL: "HTTP://user:pass@E2B.EXAMPLE:80/root/?#f"}}, "endpoint:http://e2b.example/root"},
		{"e2b empty", "e2b", core.Config{}, ""},
		{"e2b query scheme legacy", "e2b", core.Config{E2B: core.E2BConfig{APIURL: "user:pass@api.example/root?view=https://docs.example"}}, "endpoint:user:pass@api.example/root?view=https://docs.example"},
		{"namespace schemeless", "namespace-compute", core.Config{NamespaceInstance: core.NamespaceInstanceConfig{Endpoint: "API.EXAMPLE/path/", Region: " eu ", Keychain: " ci "}}, "endpoint:api.example/path|region:eu|keychain:ci"},
		{"namespace preserves port", "namespace-instance", core.Config{NamespaceInstance: core.NamespaceInstanceConfig{Endpoint: "HTTPS://user:pass@API.EXAMPLE:443/?token=secret#f"}}, "endpoint:https://api.example:443"},
		{"namespace empty query legacy", "namespace-instance", core.Config{NamespaceInstance: core.NamespaceInstanceConfig{Endpoint: "https://API.EXAMPLE/?"}}, "endpoint:https://api.example?"},
		{"namespace query scheme legacy", "namespace-instance", core.Config{NamespaceInstance: core.NamespaceInstanceConfig{Endpoint: "user:pass@api.example/root?view=https://docs.example"}}, "endpoint:user:pass@api.example/root?view=https://docs.example"},
		{"namespace partial", "namespace-instance", core.Config{NamespaceInstance: core.NamespaceInstanceConfig{Region: " us "}}, "region:us"},
		{"phala alias", "dstack", core.Config{Phala: core.PhalaConfig{NodeID: " node-7 "}}, "node:node-7"},
		{"phala empty", "phala", core.Config{}, ""},
		{"proxmox api suffix", "proxmox", core.Config{Proxmox: core.ProxmoxConfig{APIURL: "HTTPS://user:pass@PVE.EXAMPLE:8006/api2/json/?token=secret#f", Node: " pve-1 "}}, "endpoint:https://pve.example:8006|node:pve-1"},
		{"proxmox schemeless legacy", "proxmox", core.Config{Proxmox: core.ProxmoxConfig{APIURL: "PVE.EXAMPLE/api2/json/", Node: "pve"}}, "endpoint:PVE.EXAMPLE|node:pve"},
		{"proxmox incomplete", "proxmox", core.Config{Proxmox: core.ProxmoxConfig{APIURL: "https://pve.example"}}, ""},
		{"railway legacy case and query", "rail", core.Config{Railway: core.RailwayConfig{APIURL: " https://user:pass@API.EXAMPLE/graphql/?view=1 ", ProjectID: " proj ", EnvironmentID: " env "}}, "endpoint:https://API.EXAMPLE/graphql/?view=1|project:proj|environment:env"},
		{"railway incomplete", "railway", core.Config{Railway: core.RailwayConfig{APIURL: "https://api.example", ProjectID: "proj"}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := core.ProviderClaimScope(tt.provider, tt.cfg); got != tt.want {
				t.Fatalf("scope=%q want %q", got, tt.want)
			}
		})
	}
}

func TestConfigClaimScopeSeparatesRoutes(t *testing.T) {
	tests := []struct {
		provider string
		cfg      core.Config
		change   func(*core.Config)
	}{
		{"azure", core.Config{AzureSubscription: "sub", AzureResourceGroup: "rg"}, func(c *core.Config) { c.AzureSubscription = "other" }},
		{"azure", core.Config{AzureSubscription: "sub", AzureResourceGroup: "rg"}, func(c *core.Config) { c.AzureResourceGroup = "other" }},
		{"gcp", core.Config{GCPProject: "one"}, func(c *core.Config) { c.GCPProject = "two" }},
		{"cubesandbox", core.Config{CubeSandbox: core.CubeSandboxConfig{APIURL: "https://one.example"}}, func(c *core.Config) { c.CubeSandbox.APIURL = "https://two.example" }},
		{"e2b", core.Config{E2B: core.E2BConfig{APIURL: "https://one.example"}}, func(c *core.Config) { c.E2B.APIURL = "https://two.example" }},
		{"namespace-instance", core.Config{NamespaceInstance: core.NamespaceInstanceConfig{Endpoint: "https://one.example", Region: "us", Keychain: "ci"}}, func(c *core.Config) { c.NamespaceInstance.Keychain = "other" }},
		{"namespace-instance", core.Config{NamespaceInstance: core.NamespaceInstanceConfig{Endpoint: "https://one.example", Region: "us"}}, func(c *core.Config) { c.NamespaceInstance.Region = "eu" }},
		{"phala", core.Config{Phala: core.PhalaConfig{NodeID: "one"}}, func(c *core.Config) { c.Phala.NodeID = "two" }},
		{"proxmox", core.Config{Proxmox: core.ProxmoxConfig{APIURL: "https://one.example", Node: "one"}}, func(c *core.Config) { c.Proxmox.Node = "two" }},
		{"railway", core.Config{Railway: core.RailwayConfig{APIURL: "https://api.example", ProjectID: "one", EnvironmentID: "one"}}, func(c *core.Config) { c.Railway.EnvironmentID = "two" }},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			before := core.ProviderClaimScope(tt.provider, tt.cfg)
			tt.change(&tt.cfg)
			after := core.ProviderClaimScope(tt.provider, tt.cfg)
			if before == "" || after == "" || before == after {
				t.Fatalf("scope not separated: %q / %q", before, after)
			}
		})
	}
}

func TestAzureProviderClaimScopeRequiresCompleteRoute(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.AzureSubscription = " TEST-SUB "
	cfg.AzureResourceGroup = " Production-RG "
	if got, want := core.ProviderClaimScope("azure", cfg), "subscription:test-sub|resource-group:production-rg"; got != want {
		t.Fatalf("core.ProviderClaimScope(azure)=%q, want %q", got, want)
	}
	cfg.AzureResourceGroup = ""
	if got := core.ProviderClaimScope("azure", cfg); got != "" {
		t.Fatalf("incomplete azure scope=%q, want empty", got)
	}
}

func TestRailwayProviderClaimScopeRequiresCompleteRoute(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Railway.APIURL = "https://railway.example.test/graphql/v2/"
	cfg.Railway.ProjectID = "proj-1"
	cfg.Railway.EnvironmentID = "env-1"
	want := "endpoint:https://railway.example.test/graphql/v2|project:proj-1|environment:env-1"
	if got := core.ProviderClaimScope("railway", cfg); got != want {
		t.Fatalf("core.ProviderClaimScope(railway)=%q, want %q", got, want)
	}
	cfg.Railway.EnvironmentID = ""
	if got := core.ProviderClaimScope("railway", cfg); got != "" {
		t.Fatalf("incomplete railway scope=%q, want empty", got)
	}
}

func TestCubeSandboxProviderClaimScopeBindsAPIEndpoint(t *testing.T) {
	cfg := core.Config{CubeSandbox: core.CubeSandboxConfig{APIURL: "HTTPS://CUBE.EXAMPLE.TEST:443/root/"}}
	if got, want := core.ProviderClaimScope("cubesandbox", cfg), "endpoint:https://cube.example.test/root"; got != want {
		t.Fatalf("core.ProviderClaimScope(cubesandbox)=%q, want %q", got, want)
	}
	cfg.CubeSandbox.APIURL = ""
	if got := core.ProviderClaimScope("cubesandbox", cfg); got != "" {
		t.Fatalf("core.ProviderClaimScope(cubesandbox)=%q, want empty", got)
	}
}

func TestE2BProviderClaimScopeBindsAPIEndpoint(t *testing.T) {
	cfg := core.Config{E2B: core.E2BConfig{APIURL: "HTTPS://API.E2B.APP:443/v1/"}}
	if got, want := core.ProviderClaimScope("e2b", cfg), "endpoint:https://api.e2b.app/v1"; got != want {
		t.Fatalf("core.ProviderClaimScope(e2b)=%q, want %q", got, want)
	}
	cfg.E2B.APIURL = ""
	if got := core.ProviderClaimScope("e2b", cfg); got != "" {
		t.Fatalf("core.ProviderClaimScope(e2b)=%q, want empty", got)
	}
}

func TestNamespaceInstanceClaimScope(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.NamespaceInstance.Endpoint = "HTTPS://API.EXAMPLE.TEST/"
	cfg.NamespaceInstance.Region = " eu "
	cfg.NamespaceInstance.Keychain = " ci "
	cfg.NamespaceInstance.TenantID = "tenant_test"
	if got, want := core.ProviderClaimScope("namespace-instance", cfg), "endpoint:https://api.example.test|region:eu|keychain:ci"; got != want {
		t.Fatalf("scope=%q want %q", got, want)
	}
	cfg.NamespaceInstance.Endpoint = "user:secret@api.example.test/path"
	if got, want := core.ProviderClaimScope("namespace-instance", cfg), "endpoint:api.example.test/path|region:eu|keychain:ci"; got != want {
		t.Fatalf("schemeless scope=%q want %q", got, want)
	}
}
func TestPhalaClaimScope(t *testing.T) {
	cfg := core.BaseConfig()
	// No pinned node yields an empty (global) claim scope.
	if got := core.ProviderClaimScope("phala", cfg); got != "" {
		t.Fatalf("scope without node=%q want empty", got)
	}
	cfg.Phala.NodeID = "  node-7  "
	if got, want := core.ProviderClaimScope("phala", cfg), "node:node-7"; got != want {
		t.Fatalf("scope=%q want %q", got, want)
	}
}
