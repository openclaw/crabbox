package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadKubeVirtAndExternalConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := `provider: external
kubevirt:
  kubectl: /opt/bin/kubectl
  virtctl: /opt/bin/virtctl
  kubeconfig: /tmp/config
  context: dev
  namespace: test-vms
  template: /tmp/vm.yaml
  sshUser: alice
  sshKey: /tmp/id_ed25519
  sshPublicKey: ssh-ed25519 AAAA
  sshPort: "2222"
  workRoot: /home/alice/crabbox
  deleteOnRelease: false
external:
  command: node
  args: [/opt/provider.mjs, --profile, dev]
  config:
    namespace: test-vms
    sku: cpu32
  workRoot: /workspaces/project
  routingFile: /tmp/external-routing.json
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := readFileConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig()
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatal(err)
	}
	if cfg.KubeVirt.Kubectl != "/opt/bin/kubectl" || cfg.KubeVirt.Namespace != "test-vms" || cfg.KubeVirt.DeleteOnRelease {
		t.Fatalf("kubevirt=%#v", cfg.KubeVirt)
	}
	if cfg.External.Command != "node" || len(cfg.External.Args) != 3 || cfg.External.Config["sku"] != "cpu32" || cfg.External.WorkRoot != "/workspaces/project" || cfg.External.RoutingFile != "/tmp/external-routing.json" {
		t.Fatalf("external=%#v", cfg.External)
	}
}

func TestLoadKubeVirtConfigExpandsUserPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := `kubevirt:
  kubectl: ~/bin/kubectl
  virtctl: ~/bin/virtctl
  kubeconfig: ~/.kube/config
  template: ~/templates/vm.yaml
  sshKey: ~/.ssh/id_ed25519
  sshPublicKey: ~/.ssh/id_ed25519.pub
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := readFileConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig()
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatal(err)
	}
	for label, got := range map[string]string{
		"kubectl":      cfg.KubeVirt.Kubectl,
		"virtctl":      cfg.KubeVirt.Virtctl,
		"kubeconfig":   cfg.KubeVirt.Kubeconfig,
		"template":     cfg.KubeVirt.Template,
		"sshKey":       cfg.KubeVirt.SSHKey,
		"sshPublicKey": cfg.KubeVirt.SSHPublicKey,
	} {
		if !filepath.IsAbs(got) || !strings.HasPrefix(got, home+string(os.PathSeparator)) {
			t.Fatalf("%s path=%q home=%q", label, got, home)
		}
	}
}

func TestKubeVirtEnvExpandsUserPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CRABBOX_KUBEVIRT_KUBECTL", "~/bin/kubectl")
	t.Setenv("CRABBOX_KUBEVIRT_VIRTCTL", "~/bin/virtctl")
	t.Setenv("CRABBOX_KUBEVIRT_KUBECONFIG", "~/.kube/config")
	t.Setenv("CRABBOX_KUBEVIRT_TEMPLATE", "~/templates/vm.yaml")
	t.Setenv("CRABBOX_KUBEVIRT_SSH_KEY", "~/.ssh/id_ed25519")
	t.Setenv("CRABBOX_KUBEVIRT_SSH_PUBLIC_KEY", "~/.ssh/id_ed25519.pub")
	cfg := baseConfig()
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	for label, got := range map[string]string{
		"kubectl":      cfg.KubeVirt.Kubectl,
		"virtctl":      cfg.KubeVirt.Virtctl,
		"kubeconfig":   cfg.KubeVirt.Kubeconfig,
		"template":     cfg.KubeVirt.Template,
		"sshKey":       cfg.KubeVirt.SSHKey,
		"sshPublicKey": cfg.KubeVirt.SSHPublicKey,
	} {
		if !filepath.IsAbs(got) || !strings.HasPrefix(got, home+string(os.PathSeparator)) {
			t.Fatalf("%s path=%q home=%q", label, got, home)
		}
	}
}

func TestExternalArgEnvOverridesConfigArgs(t *testing.T) {
	t.Setenv("CRABBOX_EXTERNAL_ARG", "--quick-smoke")
	cfg := baseConfig()
	cfg.External.Args = []string{"--profile", "dev"}
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.External.Args) != 1 || cfg.External.Args[0] != "--quick-smoke" {
		t.Fatalf("external args=%#v", cfg.External.Args)
	}
}

func TestLoadDeclarativeExternalConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := `provider: external
external:
  lifecycle:
    acquire:
      argv: [devboxctl, new, "{{name}}", --size, "{{config.size}}"]
    list:
      argv: [devboxctl, list, --format, json]
      output: json-name-array
      namePrefix: "cbx-"
    release:
      argv: [devboxctl, rm, --yes, "{{name}}"]
  connection:
    resourceName: "{{leaseIdSlug}}"
    cloudId: devboxes/{{name}}
    serverType: "{{config.size}}"
    labels:
      backend: pod
    ssh:
      user: "{{env.DEVBOX_USER}}"
      host: "{{name}}"
      port: "22"
      sshConfigProxy: true
  config:
    size: cpu16
  workRoot: /home/developer/crabbox
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := readFileConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig()
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.External.Lifecycle.Acquire.Argv, "|"); got != "devboxctl|new|{{name}}|--size|{{config.size}}" {
		t.Fatalf("acquire argv=%q", got)
	}
	if cfg.External.Lifecycle.List.Output != "json-name-array" {
		t.Fatalf("list=%#v", cfg.External.Lifecycle.List)
	}
	if cfg.External.Lifecycle.List.NamePrefix != "cbx-" {
		t.Fatalf("list=%#v", cfg.External.Lifecycle.List)
	}
	if cfg.External.Connection.ResourceName != "{{leaseIdSlug}}" ||
		cfg.External.Connection.SSH.User != "{{env.DEVBOX_USER}}" ||
		!cfg.External.Connection.SSH.SSHConfigProxy {
		t.Fatalf("connection=%#v", cfg.External.Connection)
	}
	if cfg.External.Config["size"] != "cpu16" || cfg.External.WorkRoot != "/home/developer/crabbox" {
		t.Fatalf("external=%#v", cfg.External)
	}
}
