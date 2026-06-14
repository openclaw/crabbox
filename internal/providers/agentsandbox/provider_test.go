package agentsandbox

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpecMatchesFoundationContract(t *testing.T) {
	provider := Provider{}
	if provider.Name() != providerName {
		t.Fatalf("Name=%q", provider.Name())
	}
	if aliases := provider.Aliases(); len(aliases) != 0 {
		t.Fatalf("aliases=%v, want none", aliases)
	}
	spec := provider.Spec()
	if spec.Kind != core.ProviderKindDelegatedRun {
		t.Fatalf("Kind=%q", spec.Kind)
	}
	if spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("Coordinator=%q", spec.Coordinator)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("Targets=%#v", spec.Targets)
	}
	if !spec.Features.Has(core.FeatureArchiveSync) || !spec.Features.Has(core.FeatureCleanup) {
		t.Fatalf("Features=%#v", spec.Features)
	}
}

func TestFlagsApplyAgentSandboxConfig(t *testing.T) {
	cfg := core.BaseConfig()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := registerFlags(fs, cfg)
	args := []string{
		"--agent-sandbox-kubeconfig", "~/.kube/as",
		"--agent-sandbox-context", "agent-context",
		"--agent-sandbox-namespace", "sandboxes",
		"--agent-sandbox-warm-pool", "linux-pool",
		"--agent-sandbox-container", "worker",
		"--agent-sandbox-workdir", "/workspace/my-app",
		"--agent-sandbox-sandbox-ready-timeout", "2m",
		"--agent-sandbox-pod-ready-timeout", "30s",
		"--agent-sandbox-exec-timeout-secs", "99",
		"--agent-sandbox-delete-on-release=false",
		"--agent-sandbox-forget-missing=true",
	}
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	if err := applyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(cfg.AgentSandbox.Kubeconfig, "/.kube/as") ||
		cfg.AgentSandbox.Context != "agent-context" ||
		cfg.AgentSandbox.Namespace != "sandboxes" ||
		cfg.AgentSandbox.WarmPool != "linux-pool" ||
		cfg.AgentSandbox.Container != "worker" ||
		cfg.AgentSandbox.Workdir != "/workspace/my-app" ||
		cfg.AgentSandbox.SandboxReadyTimeout != 2*time.Minute ||
		cfg.AgentSandbox.PodReadyTimeout != 30*time.Second ||
		cfg.AgentSandbox.ExecTimeoutSecs != 99 ||
		cfg.AgentSandbox.DeleteOnRelease ||
		!cfg.AgentSandbox.ForgetMissing {
		t.Fatalf("agent-sandbox flags not applied: %#v", cfg.AgentSandbox)
	}
	if !core.DeleteOnReleaseExplicit(cfg, providerName) {
		t.Fatal("delete-on-release flag not marked explicit")
	}
}

func TestValidateConfigRejectsUnsafeInputs(t *testing.T) {
	valid := core.BaseConfig()
	valid.AgentSandbox.Context = "agent-context"
	valid.AgentSandbox.WarmPool = "linux-pool"
	for name, mutate := range map[string]func(*core.Config){
		"missing context":          func(cfg *core.Config) { cfg.AgentSandbox.Context = "" },
		"missing warm pool":        func(cfg *core.Config) { cfg.AgentSandbox.WarmPool = "" },
		"relative kubectl path":    func(cfg *core.Config) { cfg.AgentSandbox.Kubectl = "./bin/kubectl" },
		"parent kubectl path":      func(cfg *core.Config) { cfg.AgentSandbox.Kubectl = "../kubectl" },
		"relative kubeconfig path": func(cfg *core.Config) { cfg.AgentSandbox.Kubeconfig = ".kube/config" },
		"bad namespace":            func(cfg *core.Config) { cfg.AgentSandbox.Namespace = "bad/ns" },
		"bad container":            func(cfg *core.Config) { cfg.AgentSandbox.Container = "bad container" },
		"relative workdir":         func(cfg *core.Config) { cfg.AgentSandbox.Workdir = "workspace" },
		"broad workdir":            func(cfg *core.Config) { cfg.AgentSandbox.Workdir = "/tmp" },
		"cleaned broad workdir":    func(cfg *core.Config) { cfg.AgentSandbox.Workdir = "/tmp/.." },
		"negative sandbox timeout": func(cfg *core.Config) { cfg.AgentSandbox.SandboxReadyTimeout = -time.Second },
		"negative pod timeout":     func(cfg *core.Config) { cfg.AgentSandbox.PodReadyTimeout = -time.Second },
		"negative exec timeout":    func(cfg *core.Config) { cfg.AgentSandbox.ExecTimeoutSecs = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			mutate(&cfg)
			if err := validateConfig(cfg); err == nil {
				t.Fatal("invalid config was accepted")
			}
		})
	}
}

func TestValidateConfigRejectsRelativeKubeconfigEnvironment(t *testing.T) {
	for _, tt := range []struct {
		name        string
		configured  string
		environment string
	}{
		{
			name:        "relative list entry",
			environment: filepath.Join(t.TempDir(), "config") + string(os.PathListSeparator) + ".kube/config",
		},
		{
			name:        "leading whitespace remains relative",
			environment: " " + filepath.Join(t.TempDir(), "config"),
		},
		{
			name:        "whitespace configured value falls through",
			configured:  " ",
			environment: ".kube/config",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := core.BaseConfig()
			cfg.AgentSandbox.Context = "agent-context"
			cfg.AgentSandbox.WarmPool = "linux-pool"
			cfg.AgentSandbox.Kubeconfig = tt.configured
			t.Setenv("KUBECONFIG", tt.environment)
			if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "must be absolute") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestValidateConfigAllowsEmptyKubeconfigListEntries(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.AgentSandbox.Context = "agent-context"
	cfg.AgentSandbox.WarmPool = "linux-pool"
	t.Setenv("KUBECONFIG", string(os.PathListSeparator)+filepath.Join(t.TempDir(), "config"))
	if err := validateConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestConfigureUsesLinuxDelegatedBackend(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.AgentSandbox.Context = "agent-context"
	cfg.AgentSandbox.WarmPool = "linux-pool"
	backend, err := Provider{}.Configure(cfg, core.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if backend.Spec().Name != providerName || backend.Spec().Kind != core.ProviderKindDelegatedRun {
		t.Fatalf("backend spec=%#v", backend.Spec())
	}
	if _, ok := backend.(core.DoctorBackend); !ok {
		t.Fatal("backend does not implement doctor")
	}
}
