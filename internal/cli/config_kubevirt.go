package cli

//go:generate go run ../../scripts/configgen -source config_kubevirt.go -output config_kubevirt_generated.go -type KubeVirtConfig -provider kubevirt

// KubeVirtConfig owns mechanical bindings and accepted local-path expansion.
// File admission remains in applyKubeVirtFileConfig.
type KubeVirtConfig struct {
	Kubectl    string `config:"kubectl" env:"CRABBOX_KUBEVIRT_KUBECTL" flag:"kubevirt-kubectl" sources:"user,repo,env,flag" help:"kubectl executable" default:"kubectl" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	Virtctl    string `config:"virtctl" env:"CRABBOX_KUBEVIRT_VIRTCTL" flag:"kubevirt-virtctl" sources:"user,repo,env,flag" help:"virtctl executable" default:"virtctl" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	Kubeconfig string `config:"kubeconfig" env:"CRABBOX_KUBEVIRT_KUBECONFIG" flag:"kubevirt-kubeconfig" sources:"user,repo,env,flag" help:"Kubernetes kubeconfig path" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	Context    string `config:"context" env:"CRABBOX_KUBEVIRT_CONTEXT" flag:"kubevirt-context" sources:"user,repo,env,flag" help:"Kubernetes context" fileIgnoreEmpty:"true" fileStorage:"value"`
	Namespace  string `config:"namespace" env:"CRABBOX_KUBEVIRT_NAMESPACE" flag:"kubevirt-namespace" sources:"user,repo,env,flag" help:"Kubernetes namespace" default:"default" fileIgnoreEmpty:"true" fileStorage:"value"`
	Template   string `config:"template" env:"CRABBOX_KUBEVIRT_TEMPLATE" flag:"kubevirt-template" sources:"user,repo,env,flag" help:"KubeVirt VirtualMachine manifest template" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	SSHUser    string `config:"sshUser" env:"CRABBOX_KUBEVIRT_SSH_USER" flag:"kubevirt-ssh-user" sources:"user,repo,env,flag" help:"guest SSH user" default:"crabbox" fileIgnoreEmpty:"true" fileStorage:"value"`
	SSHKey     string `config:"sshKey" env:"CRABBOX_KUBEVIRT_SSH_KEY" flag:"kubevirt-ssh-key" sources:"user,env,flag" help:"guest SSH private key" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	// File admission also requires the mandatory applyKubeVirtFileConfig wrapper.
	SSHPublicKey    string `config:"sshPublicKey" env:"CRABBOX_KUBEVIRT_SSH_PUBLIC_KEY" flag:"kubevirt-ssh-public-key" sources:"user,repo,env,flag" help:"guest SSH public key inserted into the template" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	SSHPort         string `config:"sshPort" env:"CRABBOX_KUBEVIRT_SSH_PORT" flag:"kubevirt-ssh-port" sources:"user,repo,env,flag" help:"guest SSH port" default:"22" fileIgnoreEmpty:"true" fileStorage:"value"`
	WorkRoot        string `config:"workRoot" env:"CRABBOX_KUBEVIRT_WORK_ROOT" flag:"kubevirt-work-root" sources:"user,repo,env,flag" help:"guest Crabbox work root" default:"/home/crabbox/crabbox" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	DeleteOnRelease bool   `config:"deleteOnRelease" env:"CRABBOX_KUBEVIRT_DELETE_ON_RELEASE" flag:"kubevirt-delete-on-release" sources:"user,repo,env,flag" help:"delete the VM on release instead of stopping it" default:"true" reportApplied:"true"`
}

// ExpandAppliedLocalPaths expands local paths accepted by a file or flag overlay.
func (cfg *KubeVirtConfig) ExpandAppliedLocalPaths(applied KubeVirtConfigApplied) {
	if applied.Kubectl {
		cfg.Kubectl = expandUserPath(cfg.Kubectl)
	}
	if applied.Virtctl {
		cfg.Virtctl = expandUserPath(cfg.Virtctl)
	}
	if applied.Kubeconfig {
		cfg.Kubeconfig = expandUserPath(cfg.Kubeconfig)
	}
	if applied.Template {
		cfg.Template = expandUserPath(cfg.Template)
	}
	if applied.SSHKey {
		cfg.SSHKey = expandUserPath(cfg.SSHKey)
	}
	if applied.SSHPublicKey {
		cfg.SSHPublicKey = expandUserPath(cfg.SSHPublicKey)
	}
}

// applyKubeVirtFileConfig is the sole file-policy entry to the generated overlay.
// Filter a snapshot so ignored input remains intact for configuration persistence.
func applyKubeVirtFileConfig(cfg *Config, file *fileKubeVirtConfig, trusted bool, source configInputSource) error {
	if file == nil {
		return nil
	}
	snapshot := *file
	if snapshot.SSHPublicKey != "" && !(trusted || inlineSSHPublicKey(snapshot.SSHPublicKey)) {
		snapshot.SSHPublicKey = ""
	}
	applied, err := cfg.KubeVirt.applyFile(&snapshot, trusted)
	recordConfigInput(cfg, "kubevirt", source, applied.InputAccepted)
	cfg.KubeVirt.ExpandAppliedLocalPaths(applied)
	if applied.DeleteOnRelease {
		MarkDeleteOnReleaseExplicit(cfg, "kubevirt")
	}
	return err
}
