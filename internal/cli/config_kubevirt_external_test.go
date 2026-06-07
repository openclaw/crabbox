package cli

import (
	"os"
	"path/filepath"
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
