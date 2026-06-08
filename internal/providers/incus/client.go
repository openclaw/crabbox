package incus

import (
	"net/url"
	"strings"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/cliconfig"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type Runtime = core.Runtime
type Server = core.Server

type instanceClient interface {
	ListInstances() ([]api.Instance, error)
	GetInstance(name string) (*api.Instance, string, error)
	CreateInstance(req api.InstancesPost) error
	UpdateInstance(name string, put api.InstancePut, etag string) error
	SetInstanceState(name string, put api.InstanceStatePut, etag string) error
	GetInstanceState(name string) (*api.InstanceState, string, error)
	DeleteInstance(name string) error
}

type sdkClient struct {
	server incusclient.InstanceServer
}

func (c *sdkClient) ListInstances() ([]api.Instance, error) {
	return c.server.GetInstances(api.InstanceTypeAny)
}

func (c *sdkClient) GetInstance(name string) (*api.Instance, string, error) {
	return c.server.GetInstance(name)
}

func (c *sdkClient) CreateInstance(req api.InstancesPost) error {
	op, err := c.server.CreateInstance(req)
	if err != nil {
		return err
	}
	return op.Wait()
}

func (c *sdkClient) UpdateInstance(name string, put api.InstancePut, etag string) error {
	op, err := c.server.UpdateInstance(name, put, etag)
	if err != nil {
		return err
	}
	return op.Wait()
}

func (c *sdkClient) SetInstanceState(name string, put api.InstanceStatePut, etag string) error {
	op, err := c.server.UpdateInstanceState(name, put, etag)
	if err != nil {
		return err
	}
	return op.Wait()
}

func (c *sdkClient) GetInstanceState(name string) (*api.InstanceState, string, error) {
	return c.server.GetInstanceState(name)
}

func (c *sdkClient) DeleteInstance(name string) error {
	op, err := c.server.DeleteInstance(name)
	if err != nil {
		return err
	}
	return op.Wait()
}

var newClient = func(cfg Config) (instanceClient, error) {
	server, err := connectInstanceServer(cfg)
	if err != nil {
		return nil, err
	}
	return &sdkClient{server: server}, nil
}

func connectInstanceServer(cfg Config) (incusclient.InstanceServer, error) {
	if socket := strings.TrimSpace(cfg.Incus.Socket); socket != "" {
		server, err := incusclient.ConnectIncusUnix(socket, nil)
		if err != nil {
			return nil, core.Exit(2, "connect incus socket %q: %v", socket, err)
		}
		return useProject(server, cfg.Incus.Project), nil
	}
	if address := strings.TrimSpace(cfg.Incus.Address); address != "" {
		args, err := connectionArgsForAddress(cfg)
		if err != nil {
			return nil, err
		}
		server, err := incusclient.ConnectIncus(address, args)
		if err != nil {
			return nil, core.Exit(2, "connect incus address %q: %v", address, err)
		}
		return useProject(server, cfg.Incus.Project), nil
	}
	clientConfig, err := cliconfig.LoadConfig("")
	if err != nil {
		return nil, core.Exit(2, "load incus client config: %v", err)
	}
	if project := strings.TrimSpace(cfg.Incus.Project); project != "" {
		clientConfig.ProjectOverride = project
	}
	remote := strings.TrimSpace(cfg.Incus.Remote)
	if remote == "" {
		remote = clientConfig.DefaultRemote
	}
	server, err := clientConfig.GetInstanceServer(remote)
	if err != nil {
		return nil, core.Exit(2, "connect incus remote %q: %v", remote, err)
	}
	return server, nil
}

func connectionArgsForAddress(cfg Config) (*incusclient.ConnectionArgs, error) {
	args := &incusclient.ConnectionArgs{
		TLSServerCert:      strings.TrimSpace(cfg.Incus.TLSServerCert),
		InsecureSkipVerify: cfg.Incus.InsecureTLS,
	}
	clientConfig, err := cliconfig.LoadConfig("")
	if err != nil {
		return nil, core.Exit(2, "load incus client config for TLS credentials: %v", err)
	}
	remote := strings.TrimSpace(cfg.Incus.Remote)
	if remote == "" {
		remote = clientConfig.DefaultRemote
	}
	if remote != "" {
		cert, key, ca, certErr := clientConfig.GetClientCertificate(remote)
		if certErr == nil {
			args.TLSClientCert = cert
			args.TLSClientKey = key
			args.TLSCA = ca
		}
	}
	if args.TLSServerCert == "" && args.TLSClientCert == "" && !args.InsecureSkipVerify {
		return nil, core.Exit(2, "provider=%s address mode requires Incus TLS trust material or --incus-insecure-tls", providerName)
	}
	return args, nil
}

func useProject(server incusclient.InstanceServer, project string) incusclient.InstanceServer {
	project = strings.TrimSpace(project)
	if project == "" || project == "default" {
		return server
	}
	return server.UseProject(project)
}

func imageSourceForConfig(cfg Config) api.InstanceSource {
	image := strings.TrimSpace(cfg.Incus.Image)
	server := strings.TrimSpace(cfg.Incus.RemoteImageServer)
	source := api.InstanceSource{Type: "image"}
	if remoteName, aliasName, ok := splitImageAliasRemote(image); ok {
		if remoteURL, protocol, ok := resolveImageRemote(remoteName); ok {
			source.Server = remoteURL
			source.Protocol = protocol
			source.Alias = aliasName
			return source
		}
		if server != "" {
			source.Server = server
			source.Protocol = "simplestreams"
			source.Alias = aliasName
			return source
		}
	}
	if server != "" {
		source.Server = server
		source.Protocol = "simplestreams"
		source.Alias = image
		return source
	}
	source.Alias = image
	return source
}

func splitImageAliasRemote(image string) (string, string, bool) {
	remoteName, alias, ok := strings.Cut(strings.TrimSpace(image), ":")
	if !ok || remoteName == "" || alias == "" || strings.HasPrefix(alias, "//") {
		return "", "", false
	}
	return remoteName, alias, true
}

func resolveImageRemote(name string) (string, string, bool) {
	if clientConfig, err := cliconfig.LoadConfig(""); err == nil {
		if remoteURL, protocol, ok := imageRemoteFromConfig(clientConfig, name); ok {
			return remoteURL, protocol, true
		}
	}
	return imageRemoteFromConfig(cliconfig.DefaultConfig(), name)
}

func imageRemoteFromConfig(clientConfig *cliconfig.Config, name string) (string, string, bool) {
	if clientConfig == nil {
		return "", "", false
	}
	remote, ok := clientConfig.Remotes[name]
	if !ok || len(remote.Addrs) == 0 {
		return "", "", false
	}
	addr := strings.TrimSpace(remote.LastWorkingAddr)
	if addr == "" {
		addr = strings.TrimSpace(remote.Addrs[0])
	}
	if addr == "" || strings.HasPrefix(addr, "unix:") {
		return "", "", false
	}
	if !strings.Contains(addr, "://") {
		addr = "https://" + addr
	}
	protocol := strings.TrimSpace(remote.Protocol)
	if protocol == "" {
		protocol = "simplestreams"
	}
	return addr, protocol, true
}

func sshHostForConfig(cfg Config) string {
	if host := strings.TrimSpace(cfg.Incus.ProxyListenHost); host != "" && !isWildcardHost(host) {
		return host
	}
	if socket := strings.TrimSpace(cfg.Incus.Socket); socket != "" {
		return "127.0.0.1"
	}
	if address := strings.TrimSpace(cfg.Incus.Address); address != "" {
		return hostFromAddr(address)
	}
	clientConfig, err := cliconfig.LoadConfig("")
	if err != nil {
		return ""
	}
	remoteName := strings.TrimSpace(cfg.Incus.Remote)
	if remoteName == "" {
		remoteName = clientConfig.DefaultRemote
	}
	remote, ok := clientConfig.Remotes[remoteName]
	if !ok {
		return ""
	}
	addr := strings.TrimSpace(remote.LastWorkingAddr)
	if addr == "" && len(remote.Addrs) > 0 {
		addr = strings.TrimSpace(remote.Addrs[0])
	}
	if addr == "" {
		return ""
	}
	if strings.HasPrefix(addr, "unix:") {
		return "127.0.0.1"
	}
	return hostFromAddr(addr)
}

func hostFromAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	if !strings.Contains(addr, "://") {
		addr = "https://" + addr
	}
	parsed, err := url.Parse(addr)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

func isWildcardHost(host string) bool {
	switch strings.Trim(strings.TrimSpace(host), "[]") {
	case "", "0.0.0.0", "::":
		return true
	default:
		return false
	}
}
