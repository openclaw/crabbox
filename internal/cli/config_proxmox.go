package cli

//go:generate go run ../../scripts/configgen -source config_proxmox.go -output config_proxmox_generated.go -type ProxmoxConfig -provider proxmox

// ProxmoxConfig owns mechanical bindings; provenance and generic connection
// projections remain in their existing policy wrappers.
type ProxmoxConfig struct {
	APIURL      string `config:"apiUrl" env:"CRABBOX_PROXMOX_API_URL" flag:"proxmox-api-url" sources:"user,repo,env,flag" help:"Proxmox VE API URL" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	TokenID     string `config:"tokenId" env:"CRABBOX_PROXMOX_TOKEN_ID" sources:"user,repo,env" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	TokenSecret string `config:"tokenSecret" env:"CRABBOX_PROXMOX_TOKEN_SECRET" sources:"user,repo,env" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	Node        string `config:"node" env:"CRABBOX_PROXMOX_NODE" flag:"proxmox-node" sources:"user,repo,env,flag" help:"Proxmox VE node name" fileIgnoreEmpty:"true" fileStorage:"value"`
	TemplateID  int    `config:"templateId" env:"CRABBOX_PROXMOX_TEMPLATE_ID" flag:"proxmox-template-id" sources:"user,repo,env,flag" help:"Proxmox QEMU template VMID" nonnegative:"true" fileInt:"positive" envInt:"fallback" fileStorage:"value"`
	Storage     string `config:"storage" env:"CRABBOX_PROXMOX_STORAGE" flag:"proxmox-storage" sources:"user,repo,env,flag" help:"Proxmox clone storage" fileIgnoreEmpty:"true" fileStorage:"value"`
	Pool        string `config:"pool" env:"CRABBOX_PROXMOX_POOL" flag:"proxmox-pool" sources:"user,repo,env,flag" help:"Proxmox pool for cloned VMs" fileIgnoreEmpty:"true" fileStorage:"value"`
	Bridge      string `config:"bridge" env:"CRABBOX_PROXMOX_BRIDGE" flag:"proxmox-bridge" sources:"user,repo,env,flag" help:"Proxmox bridge for net0 override" fileIgnoreEmpty:"true" fileStorage:"value"`
	User        string `config:"user" env:"CRABBOX_PROXMOX_USER" flag:"proxmox-user" sources:"user,repo,env,flag" help:"cloud-init SSH user for cloned VMs" default:"crabbox" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	WorkRoot    string `config:"workRoot" env:"CRABBOX_PROXMOX_WORK_ROOT" flag:"proxmox-work-root" sources:"user,repo,env,flag" help:"remote work root for Proxmox VMs" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	FullClone   bool   `config:"fullClone" env:"CRABBOX_PROXMOX_FULL_CLONE" flag:"proxmox-full-clone" sources:"user,repo,env,flag" help:"create full Proxmox clones" default:"true"`
	InsecureTLS bool   `config:"insecureTLS" env:"CRABBOX_PROXMOX_INSECURE_TLS" flag:"proxmox-insecure-tls" sources:"user,repo,env,flag" help:"allow self-signed Proxmox TLS certificates" reportApplied:"true"`
}

const ProxmoxConfigDefaultWorkRoot string = defaultPOSIXWorkRoot

func initialProxmoxConfig() ProxmoxConfig {
	cfg := defaultProxmoxConfig()
	cfg.WorkRoot = ProxmoxConfigDefaultWorkRoot
	return cfg
}

func applyProxmoxFileConfig(cfg *Config, file *fileProxmoxConfig, source configInputSource, credentialSource credentialValueSource) error {
	applied, err := cfg.Proxmox.applyFile(file)
	recordConfigInput(cfg, "proxmox", source, applied.InputAccepted)
	if applied.APIURL {
		cfg.credentialProvenance.proxmoxAPIURL = credentialSource
	}
	if applied.TokenID {
		cfg.credentialProvenance.proxmoxTokenID = credentialSource
	}
	if applied.TokenSecret {
		cfg.credentialProvenance.proxmoxTokenSecret = credentialSource
	}
	if applied.InsecureTLS {
		cfg.credentialProvenance.proxmoxInsecureTLS = credentialSource
	}
	return err
}

func applyProxmoxEnvironmentConfig(cfg *Config) error {
	applied, err := cfg.Proxmox.applyEnv()
	recordConfigInput(cfg, "proxmox", configInputEnvironment, applied.InputAccepted)
	if applied.APIURL {
		cfg.credentialProvenance.proxmoxAPIURL = credentialSourceEnvironment
	}
	if applied.TokenID {
		cfg.credentialProvenance.proxmoxTokenID = credentialSourceEnvironment
	}
	if applied.TokenSecret {
		cfg.credentialProvenance.proxmoxTokenSecret = credentialSourceEnvironment
	}
	if applied.InsecureTLS {
		cfg.credentialProvenance.proxmoxInsecureTLS = credentialSourceEnvironment
	}
	return err
}
