package agentsandbox

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"reflect"
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

func TestAgentSandboxFlagDurationAndPathEvents(t *testing.T) {
	for _, raw := range []string{"0s", "-1s", "250ms"} {
		t.Run(raw, func(t *testing.T) {
			cfg := core.BaseConfig()
			cfg.AgentSandbox.Context = "test"
			cfg.AgentSandbox.WarmPool = "test"
			cfg.AgentSandbox.Kubeconfig = "~/inherited"
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			values := registerFlags(fs, cfg)
			if err := fs.Parse([]string{"--agent-sandbox-sandbox-ready-timeout=" + raw, "--agent-sandbox-delete-on-release=false"}); err != nil {
				t.Fatal(err)
			}
			err := applyFlags(&cfg, fs, values)
			parsed, _ := time.ParseDuration(raw)
			if cfg.AgentSandbox.SandboxReadyTimeout != parsed || cfg.AgentSandbox.Kubeconfig != "~/inherited" {
				t.Fatalf("flag state=%#v", cfg.AgentSandbox)
			}
			if cfg.AgentSandbox.DeleteOnRelease || !core.DeleteOnReleaseExplicit(cfg, providerName) {
				t.Fatal("explicit false marker lost before validation")
			}
			if (err != nil) != (parsed < 0) {
				t.Fatalf("validation error=%v for %q", err, raw)
			}
		})
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

func TestConfigShowCompleteRawContract(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config core.AgentSandboxConfig
		fields []core.ProviderConfigShowField
	}{{name: "zero", config: core.AgentSandboxConfig{Kubectl: "", Kubeconfig: "", Context: "", Namespace: "", WarmPool: "", Container: "", Workdir: "", SandboxReadyTimeout: 0, PodReadyTimeout: 0, ExecTimeoutSecs: 0, DeleteOnRelease: false, ForgetMissing: false}, fields: []core.ProviderConfigShowField{{JSONName: "kubectl", JSONValue: "", TextName: "kubectl", TextValue: "-"}, {JSONName: "kubeconfig", JSONValue: "", TextName: "kubeconfig", TextValue: "-"}, {JSONName: "context", JSONValue: "", TextName: "context", TextValue: "-"}, {JSONName: "namespace", JSONValue: "", TextName: "namespace", TextValue: "-"}, {JSONName: "warmPool", JSONValue: "", TextName: "warm_pool", TextValue: "-"}, {JSONName: "container", JSONValue: "", TextName: "container", TextValue: "-"}, {JSONName: "workdir", JSONValue: "", TextName: "workdir", TextValue: "-"}, {JSONName: "sandboxReadyTimeout", JSONValue: "0s", TextName: "sandbox_ready_timeout", TextValue: "0s"}, {JSONName: "podReadyTimeout", JSONValue: "0s", TextName: "pod_ready_timeout", TextValue: "0s"}, {JSONName: "execTimeoutSecs", JSONValue: int(0), TextName: "exec_timeout_secs", TextValue: "0"}, {JSONName: "deleteOnRelease", JSONValue: false, TextName: "delete_on_release", TextValue: "false"}, {JSONName: "forgetMissing", JSONValue: false, TextName: "forget_missing", TextValue: "false"}}},
		{name: "raw", config: core.AgentSandboxConfig{Kubectl: " Kubectl reference ", Kubeconfig: " Kubeconfig reference ", Context: " Context reference ", Namespace: " Namespace reference ", WarmPool: " WarmPool reference ", Container: " Container reference ", Workdir: " Workdir reference ", SandboxReadyTimeout: -1500 * time.Millisecond, PodReadyTimeout: -1500 * time.Millisecond, ExecTimeoutSecs: -7, DeleteOnRelease: true, ForgetMissing: true}, fields: []core.ProviderConfigShowField{{JSONName: "kubectl", JSONValue: " Kubectl reference ", TextName: "kubectl", TextValue: " Kubectl reference "}, {JSONName: "kubeconfig", JSONValue: " Kubeconfig reference ", TextName: "kubeconfig", TextValue: " Kubeconfig reference "}, {JSONName: "context", JSONValue: " Context reference ", TextName: "context", TextValue: " Context reference "}, {JSONName: "namespace", JSONValue: " Namespace reference ", TextName: "namespace", TextValue: " Namespace reference "}, {JSONName: "warmPool", JSONValue: " WarmPool reference ", TextName: "warm_pool", TextValue: " WarmPool reference "}, {JSONName: "container", JSONValue: " Container reference ", TextName: "container", TextValue: " Container reference "}, {JSONName: "workdir", JSONValue: " Workdir reference ", TextName: "workdir", TextValue: " Workdir reference "}, {JSONName: "sandboxReadyTimeout", JSONValue: "-1.5s", TextName: "sandbox_ready_timeout", TextValue: "-1.5s"}, {JSONName: "podReadyTimeout", JSONValue: "-1.5s", TextName: "pod_ready_timeout", TextValue: "-1.5s"}, {JSONName: "execTimeoutSecs", JSONValue: int(-7), TextName: "exec_timeout_secs", TextValue: "-7"}, {JSONName: "deleteOnRelease", JSONValue: true, TextName: "delete_on_release", TextValue: "true"}, {JSONName: "forgetMissing", JSONValue: true, TextName: "forget_missing", TextValue: "true"}}},
		{name: "whitespace", config: core.AgentSandboxConfig{Kubectl: " \t ", Kubeconfig: " \t ", Context: " \t ", Namespace: " \t ", WarmPool: " \t ", Container: " \t ", Workdir: " \t ", SandboxReadyTimeout: -1500 * time.Millisecond, PodReadyTimeout: -1500 * time.Millisecond, ExecTimeoutSecs: -7, DeleteOnRelease: true, ForgetMissing: true}, fields: []core.ProviderConfigShowField{{JSONName: "kubectl", JSONValue: " \t ", TextName: "kubectl", TextValue: " \t "}, {JSONName: "kubeconfig", JSONValue: " \t ", TextName: "kubeconfig", TextValue: " \t "}, {JSONName: "context", JSONValue: " \t ", TextName: "context", TextValue: " \t "}, {JSONName: "namespace", JSONValue: " \t ", TextName: "namespace", TextValue: " \t "}, {JSONName: "warmPool", JSONValue: " \t ", TextName: "warm_pool", TextValue: " \t "}, {JSONName: "container", JSONValue: " \t ", TextName: "container", TextValue: " \t "}, {JSONName: "workdir", JSONValue: " \t ", TextName: "workdir", TextValue: " \t "}, {JSONName: "sandboxReadyTimeout", JSONValue: "-1.5s", TextName: "sandbox_ready_timeout", TextValue: "-1.5s"}, {JSONName: "podReadyTimeout", JSONValue: "-1.5s", TextName: "pod_ready_timeout", TextValue: "-1.5s"}, {JSONName: "execTimeoutSecs", JSONValue: int(-7), TextName: "exec_timeout_secs", TextValue: "-7"}, {JSONName: "deleteOnRelease", JSONValue: true, TextName: "delete_on_release", TextValue: "true"}, {JSONName: "forgetMissing", JSONValue: true, TextName: "forget_missing", TextValue: "true"}}}} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := core.Config{Provider: "unselected-display"}
			cfg.AgentSandbox = tc.config
			want := core.ProviderConfigShowSection{JSONKey: "agentSandbox", TextLabel: "agent_sandbox", Providers: []string{"agent-sandbox"}, Fields: tc.fields}
			for _, selection := range []string{"unselected-display", "agent-sandbox"} {
				cfg.Provider = selection
				before := cfg
				got := (Provider{}).ConfigShowSection(cfg)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("selection=%s section=%#v want%#v", selection, got, want)
				}
				if !reflect.DeepEqual(cfg, before) {
					t.Fatal("passive projector mutated config")
				}
			}
		})
	}
}

func TestConfigShowIncludesAgentSandboxRoute(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.AgentSandbox.Kubectl = "/opt/bin/kubectl"
	cfg.AgentSandbox.Kubeconfig = "/tmp/agent-kubeconfig"
	cfg.AgentSandbox.Context = "agent-context"
	cfg.AgentSandbox.Namespace = "sandboxes"
	cfg.AgentSandbox.WarmPool = "linux-pool"
	cfg.AgentSandbox.Container = "worker"
	cfg.AgentSandbox.Workdir = "/workspace/my-app"
	cfg.AgentSandbox.SandboxReadyTimeout = 2 * time.Minute
	cfg.AgentSandbox.PodReadyTimeout = 45 * time.Second
	cfg.AgentSandbox.ExecTimeoutSecs = 42
	cfg.AgentSandbox.DeleteOnRelease = false
	cfg.AgentSandbox.ForgetMissing = true

	section := (Provider{}).ConfigShowSection(cfg)
	values := map[string]any{}
	var projected strings.Builder
	projected.WriteString(section.TextLabel)
	for _, field := range section.Fields {
		values[field.JSONName] = field.JSONValue
		projected.WriteString(" " + field.TextName + "=" + field.TextValue)
	}
	projected.WriteByte('\n')
	view := map[string]any{section.JSONKey: values}
	agent, ok := view["agentSandbox"].(map[string]any)
	if !ok || agent["kubeconfig"] != "/tmp/agent-kubeconfig" || agent["warmPool"] != "linux-pool" ||
		agent["sandboxReadyTimeout"] != "2m0s" || agent["deleteOnRelease"] != false || agent["forgetMissing"] != true {
		t.Fatalf("agentSandbox view=%#v", agent)
	}
	var text bytes.Buffer
	text.WriteString(projected.String())
	for _, want := range []string{
		"agent_sandbox kubectl=/opt/bin/kubectl",
		"kubeconfig=/tmp/agent-kubeconfig",
		"context=agent-context",
		"namespace=sandboxes",
		"warm_pool=linux-pool",
		"container=worker",
		"workdir=/workspace/my-app",
		"sandbox_ready_timeout=2m0s",
		"pod_ready_timeout=45s",
		"exec_timeout_secs=42",
		"delete_on_release=false",
		"forget_missing=true",
	} {
		if !strings.Contains(text.String(), want) {
			t.Fatalf("config show missing %q: %q", want, text.String())
		}
	}
}
