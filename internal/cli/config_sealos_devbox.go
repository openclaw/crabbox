package cli

//go:generate go run ../../scripts/configgen -source config_sealos_devbox.go -output config_sealos_devbox_generated.go -type SealosDevboxConfig -provider sealos-devbox

// SealosDevboxConfig owns source bindings; local-path expansion and native
// validation remain with the configuration callers and provider.
type SealosDevboxConfig struct {
	Kubectl         string `config:"kubectl" env:"CRABBOX_SEALOS_DEVBOX_KUBECTL" flag:"sealos-devbox-kubectl" sources:"user,env,flag" help:"kubectl executable" default:"kubectl" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	Kubeconfig      string `config:"kubeconfig" env:"CRABBOX_SEALOS_DEVBOX_KUBECONFIG" flag:"sealos-devbox-kubeconfig" sources:"user,env,flag" help:"Kubernetes kubeconfig path" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	Context         string `config:"context" env:"CRABBOX_SEALOS_DEVBOX_CONTEXT" flag:"sealos-devbox-context" sources:"user,env,flag" help:"Kubernetes context" fileIgnoreEmpty:"true" fileStorage:"value"`
	Namespace       string `config:"namespace" env:"CRABBOX_SEALOS_DEVBOX_NAMESPACE" flag:"sealos-devbox-namespace" sources:"user,env,flag" help:"Kubernetes namespace" default:"default" fileIgnoreEmpty:"true" fileStorage:"value"`
	Image           string `config:"image" env:"CRABBOX_SEALOS_DEVBOX_IMAGE" flag:"sealos-devbox-image" sources:"user,env,flag" help:"Sealos DevBox image" fileIgnoreEmpty:"true" fileStorage:"value"`
	TemplateID      string `config:"templateID" env:"CRABBOX_SEALOS_DEVBOX_TEMPLATE_ID" flag:"sealos-devbox-template-id" sources:"user,env,flag" help:"Sealos DevBox template ID" fileIgnoreEmpty:"true" fileStorage:"value"`
	CPU             string `config:"cpu" env:"CRABBOX_SEALOS_DEVBOX_CPU" flag:"sealos-devbox-cpu" sources:"user,env,flag" help:"Sealos DevBox CPU request" default:"2" fileIgnoreEmpty:"true" fileStorage:"value"`
	Memory          string `config:"memory" env:"CRABBOX_SEALOS_DEVBOX_MEMORY" flag:"sealos-devbox-memory" sources:"user,env,flag" help:"Sealos DevBox memory request" default:"4Gi" fileIgnoreEmpty:"true" fileStorage:"value"`
	StorageLimit    string `config:"storageLimit" env:"CRABBOX_SEALOS_DEVBOX_STORAGE_LIMIT" flag:"sealos-devbox-storage-limit" sources:"user,env,flag" help:"Sealos DevBox storage limit" default:"20Gi" fileIgnoreEmpty:"true" fileStorage:"value"`
	Network         string `config:"network" env:"CRABBOX_SEALOS_DEVBOX_NETWORK" flag:"sealos-devbox-network" sources:"user,env,flag" help:"Sealos DevBox network mode: SSHGate or NodePort" default:"SSHGate" fileIgnoreEmpty:"true" fileStorage:"value"`
	SSHGatewayHost  string `config:"sshGatewayHost" env:"CRABBOX_SEALOS_DEVBOX_SSH_GATEWAY_HOST" flag:"sealos-devbox-ssh-gateway-host" sources:"user,env,flag" help:"Sealos SSHGate host" fileIgnoreEmpty:"true" fileStorage:"value"`
	SSHGatewayPort  string `config:"sshGatewayPort" env:"CRABBOX_SEALOS_DEVBOX_SSH_GATEWAY_PORT" flag:"sealos-devbox-ssh-gateway-port" sources:"user,env,flag" help:"Sealos SSHGate port" default:"2233" fileIgnoreEmpty:"true" fileStorage:"value"`
	SSHUser         string `config:"sshUser" env:"CRABBOX_SEALOS_DEVBOX_SSH_USER" flag:"sealos-devbox-ssh-user" sources:"user,env,flag" help:"DevBox SSH user" default:"devbox" fileIgnoreEmpty:"true" fileStorage:"value"`
	WorkRoot        string `config:"workRoot" env:"CRABBOX_SEALOS_DEVBOX_WORK_ROOT" flag:"sealos-devbox-work-root" sources:"user,env,flag" help:"DevBox Crabbox work root" default:"/home/devbox/project" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	NodeHost        string `config:"nodeHost" env:"CRABBOX_SEALOS_DEVBOX_NODE_HOST" flag:"sealos-devbox-node-host" sources:"user,env,flag" help:"Node host for NodePort mode" fileIgnoreEmpty:"true" fileStorage:"value"`
	DeleteOnRelease bool   `config:"deleteOnRelease" env:"CRABBOX_SEALOS_DEVBOX_DELETE_ON_RELEASE" flag:"sealos-devbox-delete-on-release" sources:"user,repo,env,flag" help:"delete the DevBox on release instead of retaining it" reportApplied:"true"`
}
