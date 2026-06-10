package incus

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

type flagValues struct {
	Remote            *string
	Project           *string
	Address           *string
	Socket            *string
	InstanceType      *string
	Image             *string
	Profile           *string
	User              *string
	WorkRoot          *string
	DeleteOnRelease   *bool
	StartTimeout      *string
	LaunchPort        *string
	ProxyListenHost   *string
	ProxyListenPort   *string
	ProxyDevice       *string
	TLSServerCert     *string
	InsecureTLS       *bool
	RemoteImageServer *string
}

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return flagValues{
		Remote:            fs.String("incus-remote", defaults.Incus.Remote, "Incus remote name from the local Incus client config"),
		Project:           fs.String("incus-project", defaults.Incus.Project, "Incus project name"),
		Address:           fs.String("incus-address", defaults.Incus.Address, "Incus API address override, for example https://host:8443"),
		Socket:            fs.String("incus-socket", defaults.Incus.Socket, "Incus Unix socket path override"),
		InstanceType:      fs.String("incus-instance-type", defaults.Incus.InstanceType, "Incus instance type: container or vm"),
		Image:             fs.String("incus-image", defaults.Incus.Image, "Incus image alias or fingerprint"),
		Profile:           fs.String("incus-profile", defaults.Incus.Profile, "optional Incus profile applied to Crabbox leases"),
		User:              fs.String("incus-user", defaults.Incus.User, "SSH user inside Incus leases"),
		WorkRoot:          fs.String("incus-work-root", defaults.Incus.WorkRoot, "remote Crabbox work root inside Incus leases"),
		DeleteOnRelease:   fs.Bool("incus-delete-on-release", defaults.Incus.DeleteOnRelease, "delete the Incus instance on release"),
		StartTimeout:      fs.String("incus-start-timeout", defaults.Incus.StartTimeout.String(), "Incus start timeout"),
		LaunchPort:        fs.String("incus-launch-port", defaults.Incus.LaunchPort, "guest SSH port exposed by Incus cloud-init/bootstrap"),
		ProxyListenHost:   fs.String("incus-proxy-listen-host", defaults.Incus.ProxyListenHost, "host address for optional Incus proxy device"),
		ProxyListenPort:   fs.String("incus-proxy-listen-port", defaults.Incus.ProxyListenPort, "host TCP port for optional Incus proxy device"),
		ProxyDevice:       fs.String("incus-proxy-device", defaults.Incus.ProxyDevice, "Incus proxy device name"),
		TLSServerCert:     fs.String("incus-tls-server-cert", defaults.Incus.TLSServerCert, "trusted Incus server certificate path"),
		InsecureTLS:       fs.Bool("incus-insecure-tls", defaults.Incus.InsecureTLS, "allow self-signed or untrusted Incus TLS certificates"),
		RemoteImageServer: fs.String("incus-remote-image-server", defaults.Incus.RemoteImageServer, "remote image server for alias-based image resolution"),
	}
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if core.FlagWasSet(fs, "incus-remote") {
		cfg.Incus.Remote = *v.Remote
	}
	if core.FlagWasSet(fs, "incus-project") {
		cfg.Incus.Project = *v.Project
	}
	if core.FlagWasSet(fs, "incus-address") {
		cfg.Incus.Address = *v.Address
	}
	if core.FlagWasSet(fs, "incus-socket") {
		cfg.Incus.Socket = core.ExpandUserPath(*v.Socket)
	}
	if core.FlagWasSet(fs, "incus-instance-type") {
		normalized := normalizeInstanceType(*v.InstanceType)
		if normalized == "" {
			return core.Exit(2, "provider=%s: unsupported incus-instance-type %q (use container or vm)", providerName, *v.InstanceType)
		}
		cfg.Incus.InstanceType = normalized
		cfg.ServerType = core.IncusServerTypeForConfig(*cfg)
	}
	if core.FlagWasSet(fs, "incus-image") {
		cfg.Incus.Image = strings.TrimSpace(*v.Image)
		cfg.ServerType = core.IncusServerTypeForConfig(*cfg)
	}
	if core.FlagWasSet(fs, "incus-profile") {
		cfg.Incus.Profile = *v.Profile
	}
	if core.FlagWasSet(fs, "incus-user") {
		cfg.Incus.User = *v.User
		cfg.SSHUser = *v.User
	}
	if core.FlagWasSet(fs, "incus-work-root") {
		cfg.Incus.WorkRoot = *v.WorkRoot
		cfg.WorkRoot = *v.WorkRoot
	}
	if core.FlagWasSet(fs, "incus-delete-on-release") {
		cfg.Incus.DeleteOnRelease = *v.DeleteOnRelease
	}
	if core.FlagWasSet(fs, "incus-start-timeout") {
		if err := core.ApplyLeaseDuration(&cfg.Incus.StartTimeout, *v.StartTimeout); err != nil {
			return err
		}
	}
	if core.FlagWasSet(fs, "incus-launch-port") {
		cfg.Incus.LaunchPort = *v.LaunchPort
	}
	if core.FlagWasSet(fs, "incus-proxy-listen-host") {
		cfg.Incus.ProxyListenHost = *v.ProxyListenHost
	}
	if core.FlagWasSet(fs, "incus-proxy-listen-port") {
		cfg.Incus.ProxyListenPort = *v.ProxyListenPort
		cfg.SSHPort = core.Blank(*v.ProxyListenPort, cfg.SSHPort)
	}
	if core.FlagWasSet(fs, "incus-proxy-device") {
		cfg.Incus.ProxyDevice = *v.ProxyDevice
	}
	if core.FlagWasSet(fs, "incus-tls-server-cert") {
		cfg.Incus.TLSServerCert = core.ExpandUserPath(*v.TLSServerCert)
	}
	if core.FlagWasSet(fs, "incus-insecure-tls") {
		cfg.Incus.InsecureTLS = *v.InsecureTLS
	}
	if core.FlagWasSet(fs, "incus-remote-image-server") {
		cfg.Incus.RemoteImageServer = *v.RemoteImageServer
	}
	if isIncusProviderName(cfg.Provider) {
		cfg.Provider = providerName
		core.NormalizeTargetConfig(cfg)
	}
	return nil
}

func isIncusProviderName(provider string) bool {
	return strings.EqualFold(strings.TrimSpace(provider), providerName)
}
