package cli

import "time"

//go:generate go run ../../scripts/configgen -source config_incus.go -output config_incus_generated.go -type IncusConfig -provider incus

// IncusConfig owns source bindings; the provider retains ordered flag policy.
//
//configgen:flag-application manual
type IncusConfig struct {
	CheckpointMetadata map[string]string `sources:"runtime" yaml:"-" json:"-"`
	Remote             string            `config:"remote" env:"CRABBOX_INCUS_REMOTE" flag:"incus-remote" help:"Incus remote name from the local Incus client config" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value" default:"local"`
	Project            string            `config:"project" env:"CRABBOX_INCUS_PROJECT" flag:"incus-project" help:"Incus project name" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value"`
	Address            string            `config:"address" env:"CRABBOX_INCUS_ADDRESS" flag:"incus-address" help:"Incus API address override, for example https://host:8443" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value"`
	Socket             string            `config:"socket" env:"CRABBOX_INCUS_SOCKET" flag:"incus-socket" help:"Incus Unix socket path override" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	InstanceType       string            `config:"instanceType" env:"CRABBOX_INCUS_INSTANCE_TYPE" flag:"incus-instance-type" help:"Incus instance type: container or vm" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value" default:"container"`
	Image              string            `config:"image" env:"CRABBOX_INCUS_IMAGE" flag:"incus-image" help:"Incus image alias or fingerprint" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value" default:"images:ubuntu/24.04/cloud"`
	Profile            string            `config:"profile" env:"CRABBOX_INCUS_PROFILE" flag:"incus-profile" help:"optional Incus profile applied to Crabbox leases" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value"`
	User               string            `config:"user" env:"CRABBOX_INCUS_USER" flag:"incus-user" help:"SSH user inside Incus leases" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value" default:"crabbox"`
	WorkRoot           string            `config:"workRoot" env:"CRABBOX_INCUS_WORK_ROOT" flag:"incus-work-root" help:"remote Crabbox work root inside Incus leases" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value"`
	DeleteOnRelease    bool              `config:"deleteOnRelease" env:"CRABBOX_INCUS_DELETE_ON_RELEASE" flag:"incus-delete-on-release" help:"delete the Incus instance on release" sources:"user,repo,env,flag" default:"true" reportApplied:"true"`
	StartTimeout       time.Duration     `config:"startTimeout" env:"CRABBOX_INCUS_START_TIMEOUT" flag:"incus-start-timeout" help:"Incus start timeout" sources:"user,repo,env,flag" duration:"positive-overlay" fileStorage:"value" flagDuration:"raw-positive" default:"10m"`
	LaunchPort         string            `config:"launchPort" env:"CRABBOX_INCUS_LAUNCH_PORT" flag:"incus-launch-port" help:"guest SSH port exposed by Incus cloud-init/bootstrap" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value" default:"22"`
	ProxyListenHost    string            `config:"proxyListenHost" env:"CRABBOX_INCUS_PROXY_LISTEN_HOST" flag:"incus-proxy-listen-host" help:"host address for optional Incus proxy device" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value" default:"127.0.0.1"`
	ProxyListenPort    string            `config:"proxyListenPort" env:"CRABBOX_INCUS_PROXY_LISTEN_PORT" flag:"incus-proxy-listen-port" help:"host TCP port for optional Incus proxy device" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value"`
	ProxyDevice        string            `config:"proxyDevice" env:"CRABBOX_INCUS_PROXY_DEVICE" flag:"incus-proxy-device" help:"Incus proxy device name" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value" default:"crabbox-ssh"`
	TLSServerCert      string            `config:"tlsServerCert" env:"CRABBOX_INCUS_TLS_SERVER_CERT" flag:"incus-tls-server-cert" help:"trusted Incus server certificate path" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	InsecureTLS        bool              `config:"insecureTLS" env:"CRABBOX_INCUS_INSECURE_TLS" flag:"incus-insecure-tls" help:"allow self-signed or untrusted Incus TLS certificates" sources:"user,repo,env,flag"`
	RemoteImageServer  string            `config:"remoteImageServer" env:"CRABBOX_INCUS_REMOTE_IMAGE_SERVER" flag:"incus-remote-image-server" help:"remote image server for alias-based image resolution" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value"`
}

const IncusConfigDefaultWorkRoot string = defaultPOSIXWorkRoot

func initialIncusConfig() IncusConfig {
	cfg := defaultIncusConfig()
	cfg.WorkRoot = IncusConfigDefaultWorkRoot
	return cfg
}

func (cfg *IncusConfig) ExpandAppliedLocalPaths(applied IncusConfigApplied) {
	if applied.Socket {
		cfg.Socket = expandUserPath(cfg.Socket)
	}
	if applied.TLSServerCert {
		cfg.TLSServerCert = expandUserPath(cfg.TLSServerCert)
	}
}
