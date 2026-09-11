package cli

import "time"

//go:generate go run ../../scripts/configgen -source config_namespace_instance.go -output config_namespace_instance_generated.go -type NamespaceInstanceConfig -provider namespaceInstance

// NamespaceInstanceConfig owns input bindings; native instance defaults stay with the provider.
type NamespaceInstanceConfig struct {
	CLIPath     string        `config:"cli" env:"CRABBOX_NAMESPACE_INSTANCE_CLI" flag:"namespace-instance-cli" sources:"user,env,flag" help:"Namespace nsc CLI path" default:"nsc" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	MachineType string        `config:"machineType" env:"CRABBOX_NAMESPACE_INSTANCE_MACHINE_TYPE" flag:"namespace-instance-machine-type" sources:"user,repo,env,flag" help:"Namespace instance CPUxMemoryGB shape, for example 4x8" fileIgnoreEmpty:"true" fileStorage:"value"`
	Duration    time.Duration `config:"duration" env:"CRABBOX_NAMESPACE_INSTANCE_DURATION" flag:"namespace-instance-duration" sources:"user,repo,env,flag" help:"Namespace instance duration; defaults to --ttl" duration:"positive-overlay" fileStorage:"value" flagDuration:"raw-zero-reset"`
	Region      string        `config:"region" env:"CRABBOX_NAMESPACE_INSTANCE_REGION" flag:"namespace-instance-region" sources:"user,env,flag" help:"Namespace region" fileIgnoreEmpty:"true" fileStorage:"value"`
	Endpoint    string        `config:"endpoint" env:"CRABBOX_NAMESPACE_INSTANCE_ENDPOINT" flag:"namespace-instance-endpoint" sources:"user,env,flag" help:"Namespace API endpoint" fileIgnoreEmpty:"true" fileStorage:"value"`
	Keychain    string        `config:"keychain" env:"CRABBOX_NAMESPACE_INSTANCE_KEYCHAIN" flag:"namespace-instance-keychain" sources:"user,env,flag" help:"Namespace token keychain" fileIgnoreEmpty:"true" fileStorage:"value"`
	TenantID    string        `sources:"runtime"`
	Volumes     []string      `config:"volumes" env:"CRABBOX_NAMESPACE_INSTANCE_VOLUMES" flag:"namespace-instance-volume" sources:"user,env,flag" help:"Namespace volume kind:tag:mountpoint:size; repeatable" fileList:"raw" fileStorage:"value" envList:"presence" flagList:"append-trimmed"`
	WorkRoot    string        `config:"workRoot" env:"CRABBOX_NAMESPACE_INSTANCE_WORK_ROOT" flag:"namespace-instance-work-root" sources:"user,repo,env,flag" help:"remote Crabbox work root" default:"/work/crabbox" fileIgnoreEmpty:"true" fileStorage:"value"`
	Bare        bool          `config:"bare" env:"CRABBOX_NAMESPACE_INSTANCE_BARE" flag:"namespace-instance-bare" sources:"user,repo,env,flag" help:"disable Kubernetes for a smaller instance" default:"true"`
}

func applyNamespaceInstanceFileConfig(cfg *Config, file *fileNamespaceInstanceConfig, trusted bool) error {
	applied, err := cfg.NamespaceInstance.applyFile(file, trusted)
	// These independent overlays cannot fail or observe the accepted path before expansion.
	if applied.CLIPath {
		cfg.NamespaceInstance.CLIPath = expandUserPath(cfg.NamespaceInstance.CLIPath)
	}
	return err
}
