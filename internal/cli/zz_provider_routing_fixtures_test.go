package cli

import (
	"flag"
	"os"
	"strings"
)

// Core tests have a fake registry to avoid an import cycle. These fixtures only
// supply the opaque routing values used by orchestration/claim tests; the real
// adapter contracts and historical scope formats are tested in providers/all.
func (testGCPProvider) ClaimScope(cfg Config) string {
	if cfg.GCPProject == "" {
		return ""
	}
	return "project:" + cfg.GCPProject
}
func (testProxmoxProvider) CommandRouting(cfg Config, _ CommandRoutingRequest) CommandRouting {
	return CommandRouting{Args: []string{"--proxmox-api-url", cfg.Proxmox.APIURL}}
}
func (testStaticSSHProvider) CommandRouting(cfg Config, request CommandRoutingRequest) CommandRouting {
	cfg.Static.Host = firstNonBlank(cfg.Static.Host, request.Target.Host)
	cfg.Static.User = firstNonBlank(cfg.Static.User, request.Target.User)
	cfg.Static.Port = firstNonBlank(cfg.Static.Port, request.Target.Port)
	var args []string
	for _, f := range []struct{ flag, value string }{
		{"--static-host", cfg.Static.Host}, {"--static-user", cfg.Static.User},
		{"--static-port", cfg.Static.Port}, {"--static-work-root", cfg.Static.WorkRoot},
	} {
		if f.value != "" {
			args = append(args, f.flag, f.value)
		}
	}
	return CommandRouting{Args: args}
}
func (testExternalProvider) CommandRouting(cfg Config, request CommandRoutingRequest) CommandRouting {
	path := cfg.External.RoutingFile
	if path == "" {
		path, _ = ExternalRoutingPath(request.LeaseID)
	}
	routing := cfg.External
	if ExternalRoutingDigest(routing) == "" {
		if stored, err := LoadExternalRouting(path); err == nil {
			routing = stored
		}
	}
	args := []string{"--external-routing-file", path, "--external-routing-digest", ExternalRoutingDigest(routing)}
	if cfg.External.Connection.Desktop.Username == "" {
		cfg.External.Connection.Desktop.Username = routing.Connection.Desktop.Username
	}
	if cfg.External.Connection.Desktop.PasswordEnv == "" {
		cfg.External.Connection.Desktop.PasswordEnv = routing.Connection.Desktop.PasswordEnv
	}
	return CommandRouting{Args: AppendExternalDesktopRoutingArgs(args, cfg)}
}

type routingFixtureProvider struct {
	name  string
	scope func(Config) string
	route func(Config) CommandRouting
}

func (p routingFixtureProvider) Name() string                                 { return p.name }
func (p routingFixtureProvider) Aliases() []string                            { return nil }
func (p routingFixtureProvider) Spec() ProviderSpec                           { return ProviderSpec{Name: p.name} }
func (p routingFixtureProvider) RegisterFlags(*flag.FlagSet, Config) any      { return nil }
func (p routingFixtureProvider) ApplyFlags(*Config, *flag.FlagSet, any) error { return nil }
func (p routingFixtureProvider) Configure(Config, Runtime) (Backend, error)   { return nil, nil }
func (p routingFixtureProvider) ClaimScope(cfg Config) string {
	if p.scope != nil {
		return p.scope(cfg)
	}
	return ""
}
func (p routingFixtureProvider) CommandRouting(cfg Config, _ CommandRoutingRequest) CommandRouting {
	if p.route != nil {
		return p.route(cfg)
	}
	return CommandRouting{}
}
func init() {
	RegisterProvider(routingFixtureProvider{name: "cubesandbox", scope: func(c Config) string { return "endpoint:" + c.CubeSandbox.APIURL }})
	RegisterProvider(routingFixtureProvider{name: "railway", scope: func(Config) string {
		return "endpoint:https://railway.example.test/graphql/v2|project:proj-1|environment:env-1"
	}})
	RegisterProvider(routingFixtureProvider{name: "kubevirt", route: func(c Config) CommandRouting {
		return CommandRouting{Args: []string{"--kubevirt-context", c.KubeVirt.Context}, Env: []string{"KUBECONFIG=" + strings.TrimSpace(os.Getenv("KUBECONFIG"))}}
	}})
	RegisterProvider(routingFixtureProvider{name: "sealos-devbox", route: func(c Config) CommandRouting {
		return CommandRouting{Args: []string{"--sealos-devbox-context", c.SealosDevbox.Context, "--sealos-devbox-ssh-gateway-host", c.SealosDevbox.SSHGatewayHost}, Env: []string{"KUBECONFIG=" + strings.TrimSpace(os.Getenv("KUBECONFIG"))}}
	}})
}
