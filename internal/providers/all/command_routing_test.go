package all

import (
	"flag"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"

	. "github.com/openclaw/crabbox/internal/cli"
)

func runStopCommand(cfg Config, id string) string {
	routing := CommandRoutingFor(cfg, id, CommandRoutingStop)
	return routing.ShellCommand(append(append([]string{"crabbox", "stop"}, routing.Args...), "--id", id))
}

func TestRunStopCommandIncludesRoutingFlags(t *testing.T) {
	got := runStopCommand(Config{
		Provider:    "ssh",
		TargetOS:    "windows",
		WindowsMode: "normal",
		Static: StaticConfig{
			Host:     "win dev.local",
			User:     "runner",
			Port:     "2022",
			WorkRoot: `C:\crabbox`,
		},
	}, "static_win-dev")
	for _, want := range []string{
		"--provider ssh",
		"--target windows",
		"--windows-mode normal",
		"--static-host 'win dev.local'",
		"--static-user runner",
		"--static-port 2022",
		"--static-work-root 'C:\\crabbox'",
		"static_win-dev",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stop command missing %q:\n%s", want, got)
		}
	}
}

func TestRunStopCommandIncludesProviderRoutingFlags(t *testing.T) {
	got := runStopCommand(Config{
		Provider: "proxmox",
		TargetOS: "linux",
		Proxmox: ProxmoxConfig{
			APIURL:      "https://pve.example.test:8006",
			Node:        "pve1",
			InsecureTLS: true,
		},
	}, "cbx_123")
	for _, want := range []string{
		"--provider proxmox",
		"--proxmox-api-url https://pve.example.test:8006",
		"--proxmox-node pve1",
		"--proxmox-insecure-tls",
		"cbx_123",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stop command missing %q:\n%s", want, got)
		}
	}
}

func TestRunStopCommandIncludesNamespaceInstanceRoutingFlags(t *testing.T) {
	got := runStopCommand(Config{
		Provider: "namespace-instance",
		TargetOS: "linux",
		NamespaceInstance: NamespaceInstanceConfig{
			CLIPath:  "/opt/Namespace CLI/nsc",
			Endpoint: "https://user:secret@api.example.test/path",
			Region:   "eu west",
			Keychain: "ci",
		},
	}, "cbx_123")
	for _, want := range []string{
		"--provider namespace-instance",
		"--namespace-instance-cli '/opt/Namespace CLI/nsc'",
		"--namespace-instance-endpoint https://api.example.test/path",
		"--namespace-instance-region 'eu west'",
		"--namespace-instance-keychain ci",
		"--id cbx_123",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stop command missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "secret") || strings.Contains(got, "user") {
		t.Fatalf("stop command leaked endpoint credentials:\n%s", got)
	}
}

func TestRunStopCommandIncludesXCPNgRoutingFlagsWithoutPassword(t *testing.T) {
	got := runStopCommand(Config{
		Provider: "xcp-ng",
		TargetOS: "linux",
		XCPNg: XCPNgConfig{
			APIURL:       "https://pool-user:pool-pass@xcp-ng.example.test/path?view=1",
			Username:     "root",
			Password:     "xcp-ng-secret",
			Template:     "ubuntu template",
			TemplateUUID: "tpl-0001",
			SR:           "default sr",
			SRUUID:       "sr-0001",
			Network:      "pool network",
			NetworkUUID:  "net-0001",
			Host:         "host-0001",
			User:         "runner",
			WorkRoot:     "/work/xcp-ng",
			InsecureTLS:  true,
		},
	}, "cbx_123")
	for _, want := range []string{
		"--provider xcp-ng",
		"--target linux",
		"--xcp-ng-api-url 'https://xcp-ng.example.test/path?view=1'",
		"--xcp-ng-username root",
		"--xcp-ng-template 'ubuntu template'",
		"--xcp-ng-template-uuid tpl-0001",
		"--xcp-ng-sr 'default sr'",
		"--xcp-ng-sr-uuid sr-0001",
		"--xcp-ng-network 'pool network'",
		"--xcp-ng-network-uuid net-0001",
		"--xcp-ng-host host-0001",
		"--xcp-ng-user runner",
		"--xcp-ng-work-root /work/xcp-ng",
		"--xcp-ng-insecure-tls",
		"cbx_123",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stop command missing %q:\n%s", want, got)
		}
	}
	for _, secret := range []string{"xcp-ng-secret", "pool-user", "pool-pass", "password"} {
		if strings.Contains(got, secret) {
			t.Fatalf("stop command leaked %q:\n%s", secret, got)
		}
	}
}

func TestRunStopCommandRedactsProviderURLUserinfo(t *testing.T) {
	const rawURL = "https://api-user:api-secret@provider.example.test/path?view=1"
	for _, test := range []struct {
		name     string
		cfg      Config
		wantFlag string
		omitFlag string
	}{
		{
			name:     "proxmox",
			cfg:      Config{Provider: "proxmox", Proxmox: ProxmoxConfig{APIURL: rawURL}},
			wantFlag: "--proxmox-api-url 'https://provider.example.test/path?view=1'",
		},
		{
			name:     "daytona",
			cfg:      Config{Provider: "daytona", Daytona: DaytonaConfig{APIURL: rawURL}},
			wantFlag: "--daytona-api-url 'https://provider.example.test/path?view=1'",
		},
		{
			name:     "sprites",
			cfg:      Config{Provider: "sprites", Sprites: SpritesConfig{APIURL: rawURL}},
			wantFlag: "--sprites-api-url 'https://provider.example.test/path?view=1'",
		},
		{
			name:     "morph",
			cfg:      Config{Provider: "morph", Morph: MorphConfig{APIURL: rawURL}},
			omitFlag: "--morph-api-url",
		},
		{
			name:     "hostinger",
			cfg:      Config{Provider: "hostinger", Hostinger: HostingerConfig{APIURL: rawURL}},
			wantFlag: "--hostinger-url 'https://provider.example.test/path?view=1'",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := runStopCommand(test.cfg, "cbx_123")
			if !strings.Contains(got, test.wantFlag) {
				t.Fatalf("stop command missing safe URL %q:\n%s", test.wantFlag, got)
			}
			if test.omitFlag != "" && strings.Contains(got, test.omitFlag) {
				t.Fatalf("stop command overrides legacy private scope: %s", got)
			}
			for _, secret := range []string{"api-user", "api-secret", "api-user:api-secret"} {
				if strings.Contains(got, secret) {
					t.Fatalf("stop command leaked %q:\n%s", secret, got)
				}
			}
		})
	}
}

func TestRunStopCommandIncludesSemaphoreRoutingFlags(t *testing.T) {
	got := runStopCommand(Config{
		Provider: "semaphore",
		TargetOS: "linux",
		Semaphore: SemaphoreConfig{
			Host: "example.semaphoreci.com",
		},
	}, "sem_123")
	for _, want := range []string{
		"--provider semaphore",
		"--semaphore-host example.semaphoreci.com",
		"sem_123",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stop command missing %q:\n%s", want, got)
		}
	}
}

func TestRunStopCommandIncludesCoderDeleteFlag(t *testing.T) {
	got := runStopCommand(Config{
		Provider: "coder",
		TargetOS: "linux",
		Coder: CoderConfig{
			DeleteOnRelease: true,
		},
	}, "cbx_123")
	for _, want := range []string{
		"--provider coder",
		"--coder-delete-on-release=true",
		"--id cbx_123",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stop command missing %q:\n%s", want, got)
		}
	}
}

func TestRunStopCommandIncludesExeDevRoutingFlags(t *testing.T) {
	got := runStopCommand(Config{
		Provider: "exe-dev",
		TargetOS: "linux",
		ExeDev: ExeDevConfig{
			ControlHost: "staging.exe.dev",
		},
	}, "cbx_123")
	for _, want := range []string{
		"--provider exe-dev",
		"--exe-dev-control-host staging.exe.dev",
		"cbx_123",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stop command missing %q:\n%s", want, got)
		}
	}
}

func TestRunStopCommandIncludesMorphRoutingFlags(t *testing.T) {
	cfg := Config{
		Provider: "morph",
		TargetOS: "linux",
		Morph: MorphConfig{
			APIKey:          "secret-morph-key",
			APIURL:          "https://morph.example.test",
			DeleteOnRelease: true,
		},
	}
	MarkDeleteOnReleaseExplicit(&cfg, "morph")
	got := runStopCommand(cfg, "cbx_123")
	for _, want := range []string{
		"--provider morph",
		"--morph-api-url https://morph.example.test",
		"--morph-delete-on-release=true",
		"--id cbx_123",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stop command missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "secret-morph-key") {
		t.Fatalf("stop command leaked Morph API key:\n%s", got)
	}
}

func TestRunStopCommandIncludesHostingerRoutingFlags(t *testing.T) {
	got := runStopCommand(Config{
		Provider: "hostinger",
		TargetOS: "linux",
		Hostinger: HostingerConfig{
			APIURL: "https://hostinger.example.test/api",
		},
	}, "1750645")
	for _, want := range []string{
		"--provider hostinger",
		"--hostinger-url https://hostinger.example.test/api",
		"--id 1750645",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stop command missing %q:\n%s", want, got)
		}
	}
}

func TestRunStopCommandIncludesVastRoutingWithoutAPIKey(t *testing.T) {
	cfg := Config{
		Provider: "vast-ai",
		TargetOS: "linux",
		Vast: VastConfig{
			APIKey:        "vast-secret-api-key",
			APIURL:        "https://vast.example.test/api/v0",
			ReleaseAction: "stop",
		},
	}
	MarkDeleteOnReleaseExplicit(&cfg, "vast")
	got := runStopCommand(cfg, "cbx_123")
	for _, want := range []string{
		"--provider vast",
		"--vast-api-url https://vast.example.test/api/v0",
		"--vast-release-action stop",
		"--id cbx_123",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stop command missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, cfg.Vast.APIKey) {
		t.Fatalf("stop command leaked Vast API key:\n%s", got)
	}
}

func TestRunStopCommandIncludesExplicitNvidiaBrevReleaseAction(t *testing.T) {
	cfg := Config{
		Provider: "nvidia-brev",
		TargetOS: "linux",
		NvidiaBrev: NvidiaBrevConfig{
			CLI:           "/opt/bin/brev",
			ReleaseAction: "stop",
			Target:        "host",
			User:          "ubuntu",
		},
	}
	MarkDeleteOnReleaseExplicit(&cfg, "nvidia-brev")
	got := runStopCommand(cfg, "cbx_123")
	for _, want := range []string{
		"--provider nvidia-brev",
		"--nvidia-brev-cli /opt/bin/brev",
		"--nvidia-brev-release-action stop",
		"--nvidia-brev-target host",
		"--nvidia-brev-user ubuntu",
		"--id cbx_123",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stop command missing %q:\n%s", want, got)
		}
	}
}

func TestRunStopCommandPreservesExplicitDefaultNvidiaBrevCLI(t *testing.T) {
	got := runStopCommand(Config{
		Provider: "nvidia-brev",
		NvidiaBrev: NvidiaBrevConfig{
			CLI: "brev",
		},
	}, "cbx_123")
	if !strings.Contains(got, "--nvidia-brev-cli brev") {
		t.Fatalf("stop command omitted effective Brev CLI:\n%s", got)
	}
}

func TestRunStopCommandIncludesKubeVirtRoutingFlags(t *testing.T) {
	cfg := Config{
		Provider: "kubevirt",
		TargetOS: "linux",
		KubeVirt: KubeVirtConfig{
			Kubectl:         "/opt/bin/kubectl",
			Virtctl:         "/opt/bin/virtctl",
			Kubeconfig:      "/tmp/kube config",
			Context:         "dev",
			Namespace:       "team-vms",
			Template:        "/tmp/vm template.yaml",
			DeleteOnRelease: false,
		},
	}
	MarkDeleteOnReleaseExplicit(&cfg, "kubevirt")
	got := runStopCommand(cfg, "cbx_123")
	for _, want := range []string{
		"--provider kubevirt",
		"--kubevirt-kubectl /opt/bin/kubectl",
		"--kubevirt-virtctl /opt/bin/virtctl",
		"--kubevirt-kubeconfig '/tmp/kube config'",
		"--kubevirt-context dev",
		"--kubevirt-namespace team-vms",
		"--kubevirt-template '/tmp/vm template.yaml'",
		"--kubevirt-delete-on-release=false",
		"--id cbx_123",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stop command missing %q:\n%s", want, got)
		}
	}
}

func TestRunStopCommandOmitsAmbientReleasePolicy(t *testing.T) {
	for _, cfg := range []Config{
		{Provider: "incus", Incus: IncusConfig{DeleteOnRelease: true}},
		{Provider: "kubevirt", KubeVirt: KubeVirtConfig{DeleteOnRelease: true}},
		{Provider: "sealos-devbox", SealosDevbox: SealosDevboxConfig{DeleteOnRelease: true}},
		{Provider: "morph", Morph: MorphConfig{DeleteOnRelease: true}},
		{Provider: "namespace-devbox", Namespace: NamespaceConfig{DeleteOnRelease: true}},
		{Provider: "nvidia-brev", NvidiaBrev: NvidiaBrevConfig{ReleaseAction: "delete"}},
		{Provider: "vast", Vast: VastConfig{ReleaseAction: "stop"}},
	} {
		if got := runStopCommand(cfg, "cbx_123"); strings.Contains(got, "delete-on-release") || strings.Contains(got, "release-action") {
			t.Fatalf("ambient release policy leaked into stop command:\n%s", got)
		}
	}
}

func TestRunStopCommandIncludesInheritedKubeconfigForKubeVirt(t *testing.T) {
	t.Setenv("KUBECONFIG", "/tmp/base.yaml:/tmp/cluster.yaml")
	got := runStopCommand(Config{
		Provider: "kubevirt",
		TargetOS: "linux",
		KubeVirt: KubeVirtConfig{
			Kubectl:   "kubectl",
			Virtctl:   "virtctl",
			Context:   "dev",
			Namespace: "team-vms",
		},
	}, "cbx_123")
	for _, want := range []string{
		"KUBECONFIG='/tmp/base.yaml:/tmp/cluster.yaml' crabbox stop",
		"--provider kubevirt",
		"--kubevirt-context dev",
		"--id cbx_123",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stop command missing %q:\n%s", want, got)
		}
	}
}

func TestRunStopCommandIncludesSealosRoutingFlags(t *testing.T) {
	cfg := Config{
		Provider: "sealos-devbox",
		TargetOS: "linux",
		SealosDevbox: SealosDevboxConfig{
			Kubectl:         "/opt/bin/kubectl",
			Kubeconfig:      "/tmp/kube config",
			Context:         "dev",
			Namespace:       "team-devboxes",
			Network:         "NodePort",
			NodeHost:        "node.example.test",
			SSHGatewayPort:  "2222",
			SSHUser:         "alice",
			WorkRoot:        "/home/alice/project",
			DeleteOnRelease: false,
		},
	}
	MarkDeleteOnReleaseExplicit(&cfg, "sealos-devbox")
	got := runStopCommand(cfg, "cbx_123")
	for _, want := range []string{
		"--provider sealos-devbox",
		"--sealos-devbox-kubectl /opt/bin/kubectl",
		"--sealos-devbox-kubeconfig '/tmp/kube config'",
		"--sealos-devbox-context dev",
		"--sealos-devbox-namespace team-devboxes",
		"--sealos-devbox-network NodePort",
		"--sealos-devbox-node-host node.example.test",
		"--sealos-devbox-delete-on-release=false",
		"--id cbx_123",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stop command missing %q:\n%s", want, got)
		}
	}
}

func TestRunStopCommandIncludesInheritedKubeconfigForSealos(t *testing.T) {
	t.Setenv("KUBECONFIG", "/tmp/base.yaml:/tmp/cluster.yaml")
	got := runStopCommand(Config{
		Provider: "sealos-devbox",
		TargetOS: "linux",
		SealosDevbox: SealosDevboxConfig{
			Kubectl:        "kubectl",
			Context:        "dev",
			Namespace:      "team-devboxes",
			Network:        "SSHGate",
			SSHGatewayHost: "ssh.example.test",
			SSHGatewayPort: "2222",
			SSHUser:        "devbox",
			WorkRoot:       "/home/devbox/project",
		},
	}, "cbx_123")
	for _, want := range []string{
		"KUBECONFIG='/tmp/base.yaml:/tmp/cluster.yaml' crabbox stop",
		"--provider sealos-devbox",
		"--sealos-devbox-context dev",
		"--id cbx_123",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stop command missing %q:\n%s", want, got)
		}
	}
}

func TestRunStopCommandUsesExplicitTopLevelWorkRootForSealos(t *testing.T) {
	cfg := Config{
		Provider: "sealos-devbox",
		TargetOS: "linux",
		WorkRoot: "/srv/crabbox",
		SealosDevbox: SealosDevboxConfig{
			Kubectl:        "kubectl",
			Context:        "dev",
			Namespace:      "team-devboxes",
			Network:        "SSHGate",
			SSHGatewayHost: "ssh.example.test",
			SSHGatewayPort: "2222",
			SSHUser:        "devbox",
			WorkRoot:       "/home/devbox/project",
		},
	}
	MarkWorkRootExplicit(&cfg)
	got := runStopCommand(cfg, "cbx_123")
	if !strings.Contains(got, "--sealos-devbox-work-root /srv/crabbox") {
		t.Fatalf("stop command lost explicit work root:\n%s", got)
	}
	if strings.Contains(got, "--sealos-devbox-work-root /home/devbox/project") {
		t.Fatalf("stop command retained stale provider work root:\n%s", got)
	}
}

func TestRunStopCommandIncludesExternalRoutingFlags(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	got := runStopCommand(Config{
		Provider: "external",
		TargetOS: "linux",
		External: ExternalConfig{
			Command:  "node",
			Args:     []string{"/tmp/provider script.mjs", "--token", "secret-arg"},
			Config:   map[string]any{"namespace": "team-vms", "token": "secret-config"},
			WorkRoot: "/home/dev/crabbox",
		},
	}, "cbx_123")
	for _, want := range []string{
		"--provider external",
		"--external-routing-file",
		"--id cbx_123",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stop command missing %q:\n%s", want, got)
		}
	}
	for _, secret := range []string{"provider script.mjs", "secret-arg", "secret-config"} {
		if strings.Contains(got, secret) {
			t.Fatalf("stop command leaked %q:\n%s", secret, got)
		}
	}
}

func TestCommandRoutingPurposesKeepKubernetesEnvironmentAndReleaseIntent(t *testing.T) {
	t.Setenv("KUBECONFIG", "/tmp/base config:/tmp/cluster")
	for _, provider := range []string{"kubevirt", "sealos-devbox"} {
		for _, purpose := range []CommandRoutingPurpose{CommandRoutingReconnect, CommandRoutingRescue, CommandRoutingRetry, CommandRoutingStop} {
			cfg := BaseConfig()
			cfg.Provider = provider
			cfg.KubeVirt.Context = "ctx=west"
			cfg.SealosDevbox.Context = "ctx=west"
			cfg.KubeVirt.Kubeconfig = ""
			cfg.SealosDevbox.Kubeconfig = ""
			cfg.KubeVirt.DeleteOnRelease = true
			cfg.SealosDevbox.DeleteOnRelease = true
			route := CommandRoutingFor(cfg, "cbx_123", purpose)
			if len(route.Env) != 1 || route.Env[0] != "KUBECONFIG=/tmp/base config:/tmp/cluster" {
				t.Fatalf("%s/%s environment=%v", provider, purpose, route.Env)
			}
			for _, arg := range route.Args {
				if strings.HasPrefix(arg, "KUBECONFIG=") {
					t.Fatalf("environment assignment in argv: %v", route.Args)
				}
			}
			pinned := provider == "kubevirt" && (purpose == CommandRoutingReconnect || purpose == CommandRoutingRescue)
			if strings.Contains(strings.Join(route.Args, " "), "delete-on-release") != pinned {
				t.Fatalf("%s/%s ambient policy=%v", provider, purpose, route.Args)
			}
			cfg.KubeVirt.DeleteOnRelease = false
			cfg.SealosDevbox.DeleteOnRelease = false
			MarkDeleteOnReleaseExplicit(&cfg, provider)
			route = CommandRoutingFor(cfg, "cbx_123", purpose)
			if !strings.Contains(strings.Join(route.Args, " "), "--"+provider+"-delete-on-release=false") {
				t.Fatalf("explicit false lost: %v", route.Args)
			}
			cfg.KubeVirt.Kubeconfig = "/operator/kubeconfig"
			cfg.SealosDevbox.Kubeconfig = "/operator/kubeconfig"
			if route = CommandRoutingFor(cfg, "cbx_123", purpose); len(route.Env) != 0 {
				t.Fatalf("explicit kubeconfig retained ambient override: %v", route.Env)
			}
		}
	}
}

func TestCommandRoutingCloudScopeUsesEnvironment(t *testing.T) {
	tests := []struct {
		cfg  Config
		want []string
	}{
		{Config{Provider: "google-cloud", GCPProject: "my-project", GCPZone: "zone-a"}, []string{"CRABBOX_GCP_PROJECT=my-project", "CRABBOX_GCP_ZONE=zone-a"}},
		{Config{Provider: "azure", AzureSubscription: "sub", AzureResourceGroup: "rg", AzureLocation: "west"}, []string{"CRABBOX_AZURE_SUBSCRIPTION_ID=sub", "CRABBOX_AZURE_RESOURCE_GROUP=rg", "CRABBOX_AZURE_LOCATION=west"}},
		{Config{Provider: "aws", AWSRegion: "us-east-2"}, []string{"CRABBOX_AWS_REGION=us-east-2"}},
	}
	for _, tt := range tests {
		for _, purpose := range []CommandRoutingPurpose{CommandRoutingReconnect, CommandRoutingRetry, CommandRoutingStop} {
			route := CommandRoutingFor(tt.cfg, "cbx_123", purpose)
			if strings.Join(route.Env, "\n") != strings.Join(tt.want, "\n") {
				t.Fatalf("%s/%s environment=%v", tt.cfg.Provider, purpose, route.Env)
			}
			if strings.Contains(strings.Join(route.Args, " "), "CRABBOX_") {
				t.Fatalf("environment in argv: %v", route.Args)
			}
			if tt.cfg.Provider == "google-cloud" && route.Args[1] != "gcp" {
				t.Fatalf("alias not canonical: %v", route.Args)
			}
		}
	}
}

func TestCommandRoutingURLSecretsAndFalseTLS(t *testing.T) {
	for _, provider := range []string{"proxmox", "xcp-ng"} {
		cfg := Config{Provider: provider}
		cfg.Proxmox.APIURL = "https://user:secret@api.example/root?view=1&api%5Fkey=secret&signature=secret#secret"
		cfg.XCPNg.APIURL = cfg.Proxmox.APIURL
		cfg.XCPNg.Password = "secret"
		route := CommandRoutingFor(cfg, "cbx_123", CommandRoutingStop)
		output := strings.Join(route.Args, " ")
		if strings.Contains(output, "secret") || strings.Contains(output, "user:") {
			t.Fatalf("route leaks auth: %s", output)
		}
		if !strings.Contains(output, "https://api.example/root?view=1") || !strings.Contains(output, "--"+provider+"-insecure-tls=false") {
			t.Fatalf("safe URL or false policy lost: %s", output)
		}
	}
}

func TestCommandRoutingSchemelessEndpointSecrets(t *testing.T) {
	const endpoint = "user:pass@api.example/root?view=1&api_key=secret#secret"
	for _, provider := range []string{"e2b", "cubesandbox", "railway", "incus", "namespace-instance", "proxmox", "xcp-ng", "daytona", "hostinger", "vast", "sprites", "morph"} {
		cfg := BaseConfig()
		cfg.Provider = provider
		cfg.E2B.APIURL, cfg.CubeSandbox.APIURL = endpoint, endpoint
		cfg.Railway.APIURL, cfg.Incus.Address = endpoint, endpoint
		cfg.NamespaceInstance.Endpoint, cfg.Proxmox.APIURL = endpoint, endpoint
		cfg.XCPNg.APIURL, cfg.Daytona.APIURL = endpoint, endpoint
		cfg.Hostinger.APIURL, cfg.Vast.APIURL = endpoint, endpoint
		cfg.Sprites.APIURL, cfg.Morph.APIURL = endpoint, endpoint
		for _, purpose := range []CommandRoutingPurpose{CommandRoutingReconnect, CommandRoutingRescue, CommandRoutingRetry, CommandRoutingStop} {
			route := CommandRoutingFor(cfg, "cbx_123", purpose)
			output := strings.Join(route.Args, " ")
			if strings.Contains(output, "user:") || strings.Contains(output, "pass") || strings.Contains(output, "secret") {
				t.Fatalf("%s/%s unsafe endpoint: %s", provider, purpose, output)
			}
			privateEndpoint := provider == "e2b" || provider == "cubesandbox" || provider == "railway" || provider == "proxmox"
			if strings.Contains(output, "api.example/root?view=1") == privateEndpoint {
				t.Fatalf("%s/%s endpoint scope changed: %s", provider, purpose, output)
			}
		}
	}
}

func TestCommandRoutingRetainsLegacyPrivateEndpointScope(t *testing.T) {
	for i, tt := range []struct{ provider, endpoint, flag string }{
		{"railway", "https://api.example/graphql#profile", "--railway-url"},
		{"railway", "https://api.example/graphql?api_key=secret", "--railway-url"},
		{"e2b", "api.example/root?api_key=secret", "--e2b-api-url"},
		{"e2b", "user:pass@api.example/root?view=https://docs.example", "--e2b-api-url"},
		{"namespace-instance", "user:pass@api.example/root?view=https://docs.example", "--namespace-instance-endpoint"},
		{"cubesandbox", "api.example/root#profile", "--cubesandbox-api-url"},
		{"proxmox", "api.example/api2/json?token=secret", "--proxmox-api-url"},
		{"morph", "https://api-user:api-secret@provider.example.test/path?view=1", "--morph-api-url"},
	} {
		t.Run(tt.provider+"/"+strconv.Itoa(i), func(t *testing.T) {
			cfg := BaseConfig()
			cfg.Provider = tt.provider
			cfg.Railway.APIURL, cfg.E2B.APIURL, cfg.CubeSandbox.APIURL, cfg.Proxmox.APIURL, cfg.Morph.APIURL = tt.endpoint, tt.endpoint, tt.endpoint, tt.endpoint, tt.endpoint
			cfg.NamespaceInstance.Endpoint = tt.endpoint
			cfg.Railway.ProjectID, cfg.Railway.EnvironmentID, cfg.Proxmox.Node = "project", "env", "node"
			scope := ProviderClaimScope(cfg.Provider, cfg)
			if scope == "" {
				t.Fatal("expected legacy scope")
			}
			provider, err := ProviderFor(cfg.Provider)
			if err != nil {
				t.Fatal(err)
			}
			for _, purpose := range []CommandRoutingPurpose{CommandRoutingReconnect, CommandRoutingRescue, CommandRoutingRetry, CommandRoutingStop} {
				route := CommandRoutingFor(cfg, "cbx_123", purpose)
				output := strings.Join(route.Args, " ")
				if strings.Contains(output, tt.flag) || strings.Contains(output, "secret") || strings.Contains(output, "user:") || strings.Contains(output, "#profile") {
					t.Fatalf("%s overrides or exposes private endpoint: %s", purpose, output)
				}
				// Follow-ups retain the original private config, just as they retain
				// auth config; flags must not replace its legacy ownership identity.
				restored := cfg
				fs := flag.NewFlagSet("follow-up", flag.ContinueOnError)
				fs.SetOutput(io.Discard)
				fs.String("provider", "", "")
				fs.String("target", "", "")
				values := provider.RegisterFlags(fs, restored)
				if err := fs.Parse(route.Args); err != nil {
					t.Fatal(err)
				}
				if err := provider.ApplyFlags(&restored, fs, values); err != nil {
					t.Fatal(err)
				}
				if got := ProviderClaimScope(cfg.Provider, restored); got != scope {
					t.Fatalf("legacy scope changed: got %q want %q", got, scope)
				}
				restored.Railway.APIURL, restored.E2B.APIURL, restored.CubeSandbox.APIURL, restored.Proxmox.APIURL, restored.Morph.APIURL = "https://other.example", "https://other.example", "https://other.example", "https://other.example", "https://other.example"
				restored.NamespaceInstance.Endpoint = "https://other.example"
				if ProviderClaimScope(cfg.Provider, restored) == scope {
					t.Fatal("foreign endpoint shares legacy scope")
				}
			}
		})
	}
}

func TestCommandRoutingPreservesOpaquePaths(t *testing.T) {
	const path = "/srv/cache/https://docs.example?view=1#snapshot"
	for _, tt := range []struct{ provider, flag string }{
		{"ssh", "--static-work-root"},
		{"kubevirt", "--kubevirt-kubeconfig"},
	} {
		cfg := BaseConfig()
		cfg.Provider = tt.provider
		cfg.Static.WorkRoot, cfg.KubeVirt.Kubeconfig = path, path
		for _, purpose := range []CommandRoutingPurpose{CommandRoutingReconnect, CommandRoutingRescue, CommandRoutingRetry, CommandRoutingStop} {
			route := CommandRoutingFor(cfg, "cbx_123", purpose)
			found := false
			for i, arg := range route.Args {
				if arg == tt.flag && i+1 < len(route.Args) {
					found = true
					if route.Args[i+1] != path {
						t.Fatalf("%s/%s path changed: %q", tt.provider, purpose, route.Args[i+1])
					}
				}
			}
			if !found {
				t.Fatalf("%s/%s missing path selector", tt.provider, purpose)
			}
		}
	}
}

func TestCommandRoutingReconstructsProviderConfig(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*Config)
		fields    func(Config) []string
	}{
		{"semaphore", func(c *Config) { c.Semaphore.Host = "ci.example.test"; c.Semaphore.Project = "my-app" }, func(c Config) []string { return []string{c.Semaphore.Host, c.Semaphore.Project} }},
		{"namespace-instance", func(c *Config) {
			c.NamespaceInstance.Endpoint = "https://api.example.test"
			c.NamespaceInstance.Region = "eu"
			c.NamespaceInstance.Keychain = "ci"
			c.NamespaceInstance.WorkRoot = "/custom/root"
		}, func(c Config) []string {
			return []string{c.NamespaceInstance.Endpoint, c.NamespaceInstance.Region, c.NamespaceInstance.Keychain, c.NamespaceInstance.WorkRoot}
		}},
		{"incus", func(c *Config) {
			c.Incus.Remote = "lab"
			c.Incus.Project = "my-app"
			c.Incus.Address = "https://incus.example.test:8443"
			c.Incus.User = "alice"
			c.Incus.WorkRoot = "/srv/app"
			c.Incus.InsecureTLS = false
		}, func(c Config) []string {
			return []string{c.Incus.Remote, c.Incus.Project, c.Incus.Address, c.Incus.User, c.Incus.WorkRoot, strconv.FormatBool(c.Incus.InsecureTLS)}
		}},
		{"e2b", func(c *Config) {
			c.E2B.APIURL = "https://e2b.example.test"
			c.E2B.Domain = "box.example.test"
			c.E2B.User = "alice"
			c.E2B.Workdir = "/srv/app"
		}, func(c Config) []string { return []string{c.E2B.APIURL, c.E2B.Domain, c.E2B.User, c.E2B.Workdir} }},
		{"cubesandbox", func(c *Config) {
			c.CubeSandbox.APIURL = "https://cube.example.test"
			c.CubeSandbox.Domain = "box.example.test"
			c.CubeSandbox.User = "alice"
			c.CubeSandbox.Workdir = "/srv/app"
		}, func(c Config) []string {
			return []string{c.CubeSandbox.APIURL, c.CubeSandbox.Domain, c.CubeSandbox.User, c.CubeSandbox.Workdir}
		}},
		{"railway", func(c *Config) {
			c.Railway.APIURL = "https://railway.example.test"
			c.Railway.ProjectID = "proj"
			c.Railway.EnvironmentID = "env"
		}, func(c Config) []string {
			return []string{c.Railway.APIURL, c.Railway.ProjectID, c.Railway.EnvironmentID}
		}},
		{"railway", func(c *Config) {
			c.Railway.APIURL = "HTTPS://API.EXAMPLE/graphql/?view=%2f"
			c.Railway.ProjectID = "proj"
			c.Railway.EnvironmentID = "env"
		}, func(c Config) []string { return []string{ProviderClaimScope(c.Provider, c)} }},
		{"namespace-instance", func(c *Config) {
			c.NamespaceInstance.Endpoint = "https://API.EXAMPLE/?"
		}, func(c Config) []string { return []string{ProviderClaimScope(c.Provider, c)} }},
		{"proxmox", func(c *Config) {
			c.Proxmox.APIURL = "https://pve.example.test:8006"
			c.Proxmox.Node = "pve"
			c.Proxmox.User = "alice"
			c.Proxmox.WorkRoot = "/srv/app"
			c.Proxmox.InsecureTLS = false
		}, func(c Config) []string {
			return []string{c.Proxmox.APIURL, c.Proxmox.Node, c.Proxmox.User, c.Proxmox.WorkRoot, strconv.FormatBool(c.Proxmox.InsecureTLS)}
		}},
	}
	for _, tt := range tests {
		for _, purpose := range []CommandRoutingPurpose{CommandRoutingReconnect, CommandRoutingRetry, CommandRoutingStop} {
			t.Run(tt.name+"/"+string(purpose), func(t *testing.T) {
				cfg := BaseConfig()
				cfg.Provider = tt.name
				tt.configure(&cfg)
				route := CommandRoutingFor(cfg, "cbx_123", purpose)
				provider, err := ProviderFor(tt.name)
				if err != nil {
					t.Fatal(err)
				}
				restored := BaseConfig()
				restored.Provider = tt.name
				restored.Incus.InsecureTLS = true
				restored.Proxmox.InsecureTLS = true
				fs := flag.NewFlagSet("follow-up", flag.ContinueOnError)
				fs.SetOutput(io.Discard)
				fs.String("provider", "", "")
				fs.String("target", "", "")
				fs.String("windows-mode", "", "")
				values := provider.RegisterFlags(fs, restored)
				if err := fs.Parse(route.Args); err != nil {
					t.Fatal(err)
				}
				if err := provider.ApplyFlags(&restored, fs, values); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(tt.fields(cfg), tt.fields(restored)) {
					t.Fatalf("round trip: got %v want %v", tt.fields(restored), tt.fields(cfg))
				}
			})
		}
	}
}

func TestRunFailureDigestIncludesXCPNgRoutingFlagsWithoutPassword(t *testing.T) {
	args := CommandRoutingFor(Config{
		Provider: "xcp-ng",
		TargetOS: "linux",
		XCPNg: XCPNgConfig{
			APIURL:       "pool-user:pool-pass@xcp-ng.example.test/path?view=1",
			Username:     "root",
			Password:     "xcp-ng-secret",
			Template:     "ubuntu template",
			TemplateUUID: "tpl-0001",
			SR:           "default sr",
			SRUUID:       "sr-0001",
			Network:      "pool network",
			NetworkUUID:  "net-0001",
			Host:         "host-0001",
			User:         "runner",
			WorkRoot:     "/work/xcp-ng",
			InsecureTLS:  true,
		},
	}, "cbx_123", CommandRoutingRetry)
	joined := strings.Join(args.Args, " ")
	for _, want := range []string{
		"--provider xcp-ng",
		"--target linux",
		"--xcp-ng-api-url xcp-ng.example.test/path?view=1",
		"--xcp-ng-username root",
		"--xcp-ng-template ubuntu template",
		"--xcp-ng-template-uuid tpl-0001",
		"--xcp-ng-sr default sr",
		"--xcp-ng-sr-uuid sr-0001",
		"--xcp-ng-network pool network",
		"--xcp-ng-network-uuid net-0001",
		"--xcp-ng-host host-0001",
		"--xcp-ng-user runner",
		"--xcp-ng-work-root /work/xcp-ng",
		"--xcp-ng-insecure-tls",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("failure digest routing missing %q:\n%v", want, args)
		}
	}
	for _, secret := range []string{"xcp-ng-secret", "pool-user", "pool-pass", "password"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("failure digest routing leaked %q: %v", secret, args)
		}
	}
}

func TestCommandRoutingStaticAdapterOwnsResolvedTargetFallback(t *testing.T) {
	cfg := Config{Provider: "static-ssh", Static: StaticConfig{Port: "2022"}}
	target := SSHTarget{Host: "runner.example.test", User: "alice", Port: "22"}
	route := CommandRoutingFor(cfg, "lease", CommandRoutingRescue, target)
	output := strings.Join(route.Args, " ")
	for _, want := range []string{"--provider ssh", "--static-host runner.example.test", "--static-user alice", "--static-port 2022"} {
		if !strings.Contains(output, want) {
			t.Fatalf("missing %q: %s", want, output)
		}
	}
	target.AuthSecret = true
	target.User = "synthetic-secret"
	route = CommandRoutingFor(cfg, "lease", CommandRoutingRescue, target)
	if strings.Contains(strings.Join(route.Args, " "), "synthetic-secret") {
		t.Fatalf("resolved secret username leaked: %v", route.Args)
	}
}
