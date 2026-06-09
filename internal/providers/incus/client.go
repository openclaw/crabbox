package incus

import (
	"encoding/json"
	"net/url"
	"os"
	"runtime"
	"strings"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/cliconfig"
	"github.com/zitadel/oidc/v3/pkg/oidc"

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

type doctorConnectionInfo struct {
	Mode     string
	Protocol string
	Endpoint string
	Project  string
	Remote   string
	Auth     string
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
	remote := configuredRemoteName(cfg, clientConfig)
	if remoteConfig, ok := clientConfig.Remotes[remote]; ok && strings.HasPrefix(configuredRemoteAddr(remoteConfig), "unix:") && runtime.GOOS != "linux" {
		return nil, core.Exit(2, "provider=%s: incus.remote, incus.address, or incus.socket not configured for a reachable Linux Incus daemon (remote %q resolves to local unix socket on %s)", providerName, remote, runtime.GOOS)
	}
	server, err := clientConfig.GetInstanceServer(remote)
	if err != nil {
		return nil, core.Exit(2, "connect incus remote %q: %v", remote, err)
	}
	return server, nil
}

func doctorConnectionInfoForConfig(cfg Config) (doctorConnectionInfo, error) {
	info := doctorConnectionInfo{
		Project: selectedProject(cfg, nil),
	}
	if socket := strings.TrimSpace(cfg.Incus.Socket); socket != "" {
		info.Mode = "socket"
		info.Protocol = "unix"
		info.Endpoint = socket
		info.Auth = "unix_socket"
		return info, nil
	}
	if address := strings.TrimSpace(cfg.Incus.Address); address != "" {
		auth, err := doctorAddressAuth(cfg)
		if err != nil {
			return doctorConnectionInfo{}, err
		}
		info.Mode = "address"
		info.Protocol = "incus"
		info.Endpoint = address
		info.Auth = auth
		return info, nil
	}
	clientConfig, err := cliconfig.LoadConfig("")
	if err != nil {
		return doctorConnectionInfo{}, core.Exit(2, "load incus client config: %v", err)
	}
	remoteName := configuredRemoteName(cfg, clientConfig)
	remote, ok := clientConfig.Remotes[remoteName]
	if !ok {
		return doctorConnectionInfo{}, core.Exit(2, "connect incus remote %q: remote not found", remoteName)
	}
	if strings.HasPrefix(configuredRemoteAddr(remote), "unix:") && runtime.GOOS != "linux" {
		return doctorConnectionInfo{}, core.Exit(2, "provider=%s: incus.remote, incus.address, or incus.socket not configured for a reachable Linux Incus daemon (remote %q resolves to local unix socket on %s)", providerName, remoteName, runtime.GOOS)
	}
	info.Mode = "remote"
	info.Protocol = core.Blank(strings.TrimSpace(remote.Protocol), "incus")
	info.Endpoint = configuredRemoteAddr(remote)
	info.Project = selectedProject(cfg, &remote)
	info.Remote = remoteName
	info.Auth = remoteAuthType(remote)
	return info, nil
}

func connectionArgsForAddress(cfg Config) (*incusclient.ConnectionArgs, error) {
	args := &incusclient.ConnectionArgs{
		InsecureSkipVerify: cfg.Incus.InsecureTLS,
	}
	if certPath := strings.TrimSpace(cfg.Incus.TLSServerCert); certPath != "" {
		content, err := os.ReadFile(certPath)
		if err != nil {
			return nil, core.Exit(2, "read incus TLS server cert %q: %v", certPath, err)
		}
		args.TLSServerCert = strings.TrimSpace(string(content))
	}
	clientConfig, err := cliconfig.LoadConfig("")
	if err != nil {
		return nil, core.Exit(2, "load incus client config for TLS credentials: %v", err)
	}
	remoteName := configuredRemoteName(cfg, clientConfig)
	if remoteName != "" {
		if remoteConfig, ok := clientConfig.Remotes[remoteName]; ok && addressMatchesRemote(strings.TrimSpace(cfg.Incus.Address), remoteConfig) {
			if args.TLSServerCert == "" {
				serverCert, certErr := loadRemoteServerCert(clientConfig, remoteName)
				if certErr != nil {
					return nil, core.Exit(2, "read incus server cert for remote %q: %v", remoteName, certErr)
				}
				args.TLSServerCert = serverCert
			}
			if remoteConfig.AuthType == api.AuthenticationMethodOIDC {
				args.AuthType = api.AuthenticationMethodOIDC
				tokens, tokenErr := loadRemoteOIDCTokens(clientConfig, remoteName)
				if tokenErr != nil {
					return nil, core.Exit(2, "read incus OIDC tokens for remote %q: %v", remoteName, tokenErr)
				}
				args.OIDCTokens = tokens
			} else {
				cert, key, ca, certErr := clientConfig.GetClientCertificate(remoteName)
				if certErr == nil {
					args.TLSClientCert = cert
					args.TLSClientKey = key
					args.TLSCA = ca
				}
			}
		}
	}
	if args.TLSServerCert == "" && args.TLSClientCert == "" && args.AuthType != api.AuthenticationMethodOIDC && !args.InsecureSkipVerify {
		return nil, core.Exit(2, "provider=%s address mode requires Incus TLS trust material or --incus-insecure-tls", providerName)
	}
	return args, nil
}

func doctorAddressAuth(cfg Config) (string, error) {
	args, err := connectionArgsForAddress(cfg)
	if err != nil {
		return "", err
	}
	switch {
	case args.AuthType == api.AuthenticationMethodOIDC:
		return "oidc", nil
	case args.InsecureSkipVerify:
		return "insecure_tls", nil
	case strings.TrimSpace(args.TLSClientCert) != "":
		return "tls_client_cert", nil
	case strings.TrimSpace(args.TLSServerCert) != "":
		return "tls_server_cert", nil
	default:
		return "configured", nil
	}
}

func addressMatchesRemote(address string, remote cliconfig.Remote) bool {
	target, ok := normalizeIncusAddress(address)
	if !ok {
		return false
	}
	candidates := []string{configuredRemoteAddr(remote)}
	candidates = append(candidates, remote.Addrs...)
	for _, candidate := range candidates {
		normalized, ok := normalizeIncusAddress(candidate)
		if ok && normalized == target {
			return true
		}
	}
	return false
}

func normalizeIncusAddress(value string) (string, bool) {
	candidate := strings.TrimSpace(value)
	if candidate == "" {
		return "", false
	}
	parsed, err := url.Parse(candidate)
	if err != nil || parsed.Host == "" {
		parsed, err = url.Parse("https://" + candidate)
		if err != nil || parsed.Host == "" {
			return "", false
		}
	}
	scheme := strings.ToLower(strings.TrimSpace(parsed.Scheme))
	if scheme == "" {
		scheme = "https"
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Host))
	path := strings.TrimRight(parsed.EscapedPath(), "/")
	if path == "" {
		return scheme + "://" + host, true
	}
	return scheme + "://" + host + path, true
}

func loadRemoteServerCert(clientConfig *cliconfig.Config, remoteName string) (string, error) {
	content, err := os.ReadFile(clientConfig.ServerCertPath(remoteName))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(content)), nil
}

func loadRemoteOIDCTokens(clientConfig *cliconfig.Config, remoteName string) (*oidc.Tokens[*oidc.IDTokenClaims], error) {
	content, err := os.ReadFile(clientConfig.OIDCTokenPath(remoteName))
	if err != nil {
		if os.IsNotExist(err) {
			return &oidc.Tokens[*oidc.IDTokenClaims]{}, nil
		}
		return nil, err
	}
	tokens := oidc.Tokens[*oidc.IDTokenClaims]{}
	if err := json.Unmarshal(content, &tokens); err != nil {
		return nil, err
	}
	return &tokens, nil
}

func configuredRemoteName(cfg Config, clientConfig *cliconfig.Config) string {
	remote := strings.TrimSpace(cfg.Incus.Remote)
	if remote == "" && clientConfig != nil {
		remote = strings.TrimSpace(clientConfig.DefaultRemote)
	}
	return remote
}

func configuredRemoteAddr(remote cliconfig.Remote) string {
	addr := strings.TrimSpace(remote.LastWorkingAddr)
	if addr == "" && len(remote.Addrs) > 0 {
		addr = strings.TrimSpace(remote.Addrs[0])
	}
	return addr
}

func selectedProject(cfg Config, remote *cliconfig.Remote) string {
	if project := strings.TrimSpace(cfg.Incus.Project); project != "" {
		return project
	}
	if remote != nil {
		if project := strings.TrimSpace(remote.Project); project != "" {
			return project
		}
	}
	return "default"
}

func remoteAuthType(remote cliconfig.Remote) string {
	switch {
	case remote.Public:
		return "public"
	case strings.TrimSpace(remote.AuthType) != "":
		return strings.TrimSpace(remote.AuthType)
	default:
		return "tls"
	}
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
	remoteName := configuredRemoteName(cfg, clientConfig)
	remote, ok := clientConfig.Remotes[remoteName]
	if !ok {
		return ""
	}
	addr := configuredRemoteAddr(remote)
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
