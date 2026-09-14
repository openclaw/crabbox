package incus

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(core.IncusConfigFlagValues)
	if !ok {
		return nil
	}
	visited := core.IncusConfigFlagPresence(fs)
	if visited.Remote {
		cfg.Incus.Remote = *v.Remote
		core.RecordProviderFlagInputs(cfg, true, providerName)
	}
	if visited.Project {
		cfg.Incus.Project = *v.Project
		core.RecordProviderFlagInputs(cfg, true, providerName)
	}
	if visited.Address {
		cfg.Incus.Address = *v.Address
		core.RecordProviderFlagInputs(cfg, true, providerName)
	}
	if visited.Socket {
		cfg.Incus.Socket = core.ExpandUserPath(*v.Socket)
		core.RecordProviderFlagInputs(cfg, true, providerName)
	}
	if visited.InstanceType {
		normalized := normalizeInstanceType(*v.InstanceType)
		if normalized == "" {
			return core.Exit(2, "provider=%s: unsupported incus-instance-type %q (use container or vm)", providerName, *v.InstanceType)
		}
		cfg.Incus.InstanceType = normalized
		core.RecordProviderFlagInputs(cfg, true, providerName)
		cfg.ServerType = core.IncusServerTypeForConfig(*cfg)
	}
	if visited.Image {
		cfg.Incus.Image = strings.TrimSpace(*v.Image)
		core.RecordProviderFlagInputs(cfg, true, providerName)
		cfg.ServerType = core.IncusServerTypeForConfig(*cfg)
	}
	if visited.Profile {
		cfg.Incus.Profile = *v.Profile
		core.RecordProviderFlagInputs(cfg, true, providerName)
	}
	if visited.User {
		cfg.Incus.User = *v.User
		core.RecordProviderFlagInputs(cfg, true, providerName)
		cfg.SSHUser = *v.User
	}
	if visited.WorkRoot {
		cfg.Incus.WorkRoot = *v.WorkRoot
		core.RecordProviderFlagInputs(cfg, true, providerName)
		cfg.WorkRoot = *v.WorkRoot
	}
	if visited.DeleteOnRelease {
		cfg.Incus.DeleteOnRelease = *v.DeleteOnRelease
		core.RecordProviderFlagInputs(cfg, true, providerName)
		core.MarkDeleteOnReleaseExplicit(cfg, providerName)
	}
	if visited.StartTimeout {
		if err := core.ApplyLeaseDuration(&cfg.Incus.StartTimeout, *v.StartTimeout); err != nil {
			return err
		}
		core.RecordProviderFlagInputs(cfg, *v.StartTimeout != "", providerName)
	}
	if visited.LaunchPort {
		cfg.Incus.LaunchPort = *v.LaunchPort
		core.RecordProviderFlagInputs(cfg, true, providerName)
	}
	if visited.ProxyListenHost {
		cfg.Incus.ProxyListenHost = *v.ProxyListenHost
		core.RecordProviderFlagInputs(cfg, true, providerName)
	}
	if visited.ProxyListenPort {
		cfg.Incus.ProxyListenPort = *v.ProxyListenPort
		core.RecordProviderFlagInputs(cfg, true, providerName)
		cfg.SSHPort = core.Blank(*v.ProxyListenPort, cfg.SSHPort)
	}
	if visited.ProxyDevice {
		cfg.Incus.ProxyDevice = *v.ProxyDevice
		core.RecordProviderFlagInputs(cfg, true, providerName)
	}
	if visited.TLSServerCert {
		cfg.Incus.TLSServerCert = core.ExpandUserPath(*v.TLSServerCert)
		core.RecordProviderFlagInputs(cfg, true, providerName)
	}
	if visited.InsecureTLS {
		cfg.Incus.InsecureTLS = *v.InsecureTLS
		core.RecordProviderFlagInputs(cfg, true, providerName)
	}
	if visited.RemoteImageServer {
		cfg.Incus.RemoteImageServer = *v.RemoteImageServer
		core.RecordProviderFlagInputs(cfg, true, providerName)
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
