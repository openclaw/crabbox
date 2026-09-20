package xcpng

import (
	"flag"
	"net"
	"net/url"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) ClaimScope(cfg core.Config) string {
	endpoint, err := xapiEndpoint(cfg.XCPNg.APIURL)
	if err != nil {
		return ""
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if (parsed.Scheme == "https" && port == "443") || (parsed.Scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		parsed.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		parsed.Host = "[" + host + "]"
	} else {
		parsed.Host = host
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return "endpoint:" + parsed.String() + "|username:" + strings.TrimSpace(cfg.XCPNg.Username)
}

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Authentication:   core.DirectProviderAuthentication(core.ProviderAuthenticationUsernamePassword),
		Name:             "xcp-ng",
		Family:           "xcp-ng",
		Kind:             core.ProviderKindSSHLease,
		Targets:          []core.TargetSpec{{OS: core.TargetLinux}},
		Features:         core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup},
		Coordinator:      core.CoordinatorNever,
		ClassDisposition: core.ProviderClassDispositionUnmapped,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterXCPNgConfigFlags(fs, defaults.XCPNg)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(core.XCPNgConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := v.Apply(&cfg.XCPNg, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, "xcp-ng")
	if err != nil {
		return err
	}
	visited := core.XCPNgConfigFlagPresence(fs)
	// Unlike file/env pairs, a visited UUID wins even when empty and regardless of argv order.
	if visited.TemplateUUID {
		cfg.XCPNg.Template = ""
	} else if visited.Template {
		cfg.XCPNg.TemplateUUID = ""
	}
	if visited.Template || visited.TemplateUUID {
		cfg.ServerType = xcpNgServerTypeForConfig(*cfg)
	}
	if visited.SRUUID {
		cfg.XCPNg.SR = ""
	} else if visited.SR {
		cfg.XCPNg.SRUUID = ""
	}
	if visited.NetworkUUID {
		cfg.XCPNg.Network = ""
	} else if visited.Network {
		cfg.XCPNg.NetworkUUID = ""
	}
	if applied.User {
		cfg.SSHUser = cfg.XCPNg.User
	}
	if applied.WorkRoot {
		cfg.WorkRoot = cfg.XCPNg.WorkRoot
	}
	return nil
}

func (Provider) ServerTypeForConfig(cfg core.Config) string {
	return xcpNgServerTypeForConfig(cfg)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewLeaseBackend(p.Spec(), cfg, rt), nil
}
