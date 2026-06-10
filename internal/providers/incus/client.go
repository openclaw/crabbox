package incus

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/gofrs/flock"
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
	server           incusclient.InstanceServer
	reloadOIDCTokens func() error
	saveOIDCTokens   func() error
	oidcLock         *flock.Flock
	operationMu      sync.Mutex
}

func (c *sdkClient) ListInstances() ([]api.Instance, error) {
	if err := c.beginOperation(); err != nil {
		return nil, err
	}
	defer c.endOperation()
	instances, err := c.server.GetInstances(api.InstanceTypeAny)
	return instances, c.persistResult(err)
}

func (c *sdkClient) GetInstance(name string) (*api.Instance, string, error) {
	if err := c.beginOperation(); err != nil {
		return nil, "", err
	}
	defer c.endOperation()
	inst, etag, err := c.server.GetInstance(name)
	return inst, etag, c.persistResult(err)
}

func (c *sdkClient) CreateInstance(req api.InstancesPost) error {
	if err := c.beginOperation(); err != nil {
		return err
	}
	defer c.endOperation()
	if err := c.persistResult(nil); err != nil {
		return err
	}
	op, err := c.server.CreateInstance(req)
	if err != nil {
		return c.persistResult(err)
	}
	if err := op.Wait(); err != nil {
		return c.persistResult(err)
	}
	c.persistCommittedMutation()
	return nil
}

func (c *sdkClient) UpdateInstance(name string, put api.InstancePut, etag string) error {
	if err := c.beginOperation(); err != nil {
		return err
	}
	defer c.endOperation()
	if err := c.persistResult(nil); err != nil {
		return err
	}
	op, err := c.server.UpdateInstance(name, put, etag)
	if err != nil {
		return c.persistResult(err)
	}
	if err := op.Wait(); err != nil {
		return c.persistResult(err)
	}
	c.persistCommittedMutation()
	return nil
}

func (c *sdkClient) SetInstanceState(name string, put api.InstanceStatePut, etag string) error {
	if err := c.beginOperation(); err != nil {
		return err
	}
	defer c.endOperation()
	if err := c.persistResult(nil); err != nil {
		return err
	}
	op, err := c.server.UpdateInstanceState(name, put, etag)
	if err != nil {
		return c.persistResult(err)
	}
	if err := op.Wait(); err != nil {
		return c.persistResult(err)
	}
	c.persistCommittedMutation()
	return nil
}

func (c *sdkClient) GetInstanceState(name string) (*api.InstanceState, string, error) {
	if err := c.beginOperation(); err != nil {
		return nil, "", err
	}
	defer c.endOperation()
	state, etag, err := c.server.GetInstanceState(name)
	return state, etag, c.persistResult(err)
}

func (c *sdkClient) DeleteInstance(name string) error {
	if err := c.beginOperation(); err != nil {
		return err
	}
	defer c.endOperation()
	if err := c.persistResult(nil); err != nil {
		return err
	}
	op, err := c.server.DeleteInstance(name)
	if err != nil {
		return c.persistResult(err)
	}
	if err := op.Wait(); err != nil {
		return c.persistResult(err)
	}
	c.persistCommittedMutation()
	return nil
}

func (c *sdkClient) beginOperation() error {
	c.operationMu.Lock()
	if c.oidcLock == nil {
		return nil
	}
	if err := c.oidcLock.Lock(); err != nil {
		c.operationMu.Unlock()
		return fmt.Errorf("lock Incus OIDC tokens: %w", err)
	}
	if c.reloadOIDCTokens != nil {
		if err := c.reloadOIDCTokens(); err != nil {
			_ = c.oidcLock.Unlock()
			c.operationMu.Unlock()
			return fmt.Errorf("reload Incus OIDC tokens: %w", err)
		}
	}
	return nil
}

func (c *sdkClient) endOperation() {
	if c.oidcLock != nil {
		_ = c.oidcLock.Unlock()
	}
	c.operationMu.Unlock()
}

func (c *sdkClient) persistResult(operationErr error) error {
	if c.saveOIDCTokens == nil {
		return operationErr
	}
	saveErr := c.saveOIDCTokens()
	if saveErr == nil {
		return operationErr
	}
	if operationErr != nil {
		return fmt.Errorf("%w; persist Incus OIDC tokens: %v", operationErr, saveErr)
	}
	return fmt.Errorf("persist Incus OIDC tokens: %w", saveErr)
}

func (c *sdkClient) persistCommittedMutation() {
	if c.saveOIDCTokens != nil {
		_ = c.saveOIDCTokens()
	}
}

type instanceConnection struct {
	server           incusclient.InstanceServer
	reloadOIDCTokens func() error
	saveOIDCTokens   func() error
	oidcLock         *flock.Flock
}

var newClient = func(cfg Config) (instanceClient, error) {
	connection, err := connectInstanceConnection(cfg)
	if err != nil {
		return nil, err
	}
	return &sdkClient{
		server:           connection.server,
		reloadOIDCTokens: connection.reloadOIDCTokens,
		saveOIDCTokens:   connection.saveOIDCTokens,
		oidcLock:         connection.oidcLock,
	}, nil
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
	connection, err := connectInstanceConnection(cfg)
	if err != nil {
		return nil, err
	}
	return connection.server, nil
}

func connectInstanceConnection(cfg Config) (instanceConnection, error) {
	if socket := strings.TrimSpace(cfg.Incus.Socket); socket != "" {
		server, err := incusclient.ConnectIncusUnix(socket, nil)
		if err != nil {
			return instanceConnection{}, core.Exit(2, "connect incus socket %q: %v", socket, err)
		}
		return instanceConnection{server: useProject(server, cfg.Incus.Project)}, nil
	}
	if address := strings.TrimSpace(cfg.Incus.Address); address != "" {
		args, oidcTokenPath, err := connectionArgsForAddressWithTokenPath(cfg)
		if err != nil {
			return instanceConnection{}, err
		}
		tokenLock, err := lockOIDCTokens(oidcTokenPath)
		if err != nil {
			return instanceConnection{}, err
		}
		lockHeld := tokenLock != nil
		defer func() {
			if lockHeld {
				_ = tokenLock.Unlock()
			}
		}()
		if tokenLock != nil {
			tokens, err := loadOIDCTokens(oidcTokenPath)
			if err != nil {
				return instanceConnection{}, core.Exit(2, "read Incus OIDC tokens: %v", err)
			}
			args.OIDCTokens = tokens
		}
		server, err := incusclient.ConnectIncus(address, args)
		if err != nil {
			return instanceConnection{}, core.Exit(2, "connect incus address %q: %v", address, err)
		}
		server = useProject(server, cfg.Incus.Project)
		reloadOIDCTokens, saveOIDCTokens, err := oidcTokenCallbacks(server, oidcTokenPath)
		if err != nil {
			return instanceConnection{}, err
		}
		if saveOIDCTokens != nil {
			if err := saveOIDCTokens(); err != nil {
				return instanceConnection{}, fmt.Errorf("persist Incus OIDC tokens: %w", err)
			}
		}
		if tokenLock != nil {
			if err := tokenLock.Unlock(); err != nil {
				return instanceConnection{}, fmt.Errorf("unlock Incus OIDC tokens: %w", err)
			}
			lockHeld = false
		}
		return instanceConnection{
			server:           server,
			reloadOIDCTokens: reloadOIDCTokens,
			saveOIDCTokens:   saveOIDCTokens,
			oidcLock:         tokenLock,
		}, nil
	}
	clientConfig, err := cliconfig.LoadConfig("")
	if err != nil {
		return instanceConnection{}, core.Exit(2, "load incus client config: %v", err)
	}
	if project := strings.TrimSpace(cfg.Incus.Project); project != "" {
		clientConfig.ProjectOverride = project
	}
	remote := configuredRemoteName(cfg, clientConfig)
	if remoteConfig, ok := clientConfig.Remotes[remote]; ok && strings.HasPrefix(configuredRemoteAddr(remoteConfig), "unix:") && runtime.GOOS != "linux" {
		return instanceConnection{}, core.Exit(2, "provider=%s: incus.remote, incus.address, or incus.socket not configured for a reachable Linux Incus daemon (remote %q resolves to local unix socket on %s)", providerName, remote, runtime.GOOS)
	}
	disableOIDCKeepAlive(clientConfig, remote)
	oidcTokenPath := ""
	if remoteConfig, ok := clientConfig.Remotes[remote]; ok && remoteConfig.AuthType == api.AuthenticationMethodOIDC {
		oidcTokenPath = clientConfig.OIDCTokenPath(remote)
	}
	tokenLock, err := lockOIDCTokens(oidcTokenPath)
	if err != nil {
		return instanceConnection{}, err
	}
	lockHeld := tokenLock != nil
	defer func() {
		if lockHeld {
			_ = tokenLock.Unlock()
		}
	}()
	server, err := clientConfig.GetInstanceServer(remote)
	if err != nil {
		return instanceConnection{}, core.Exit(2, "connect incus remote %q: %v", remote, err)
	}
	connection := instanceConnection{server: server, oidcLock: tokenLock}
	connection.reloadOIDCTokens, connection.saveOIDCTokens, err = oidcTokenCallbacks(server, oidcTokenPath)
	if err != nil {
		return instanceConnection{}, err
	}
	if connection.saveOIDCTokens != nil {
		if err := connection.saveOIDCTokens(); err != nil {
			return instanceConnection{}, fmt.Errorf("persist Incus OIDC tokens: %w", err)
		}
	}
	if tokenLock != nil {
		if err := tokenLock.Unlock(); err != nil {
			return instanceConnection{}, fmt.Errorf("unlock Incus OIDC tokens: %w", err)
		}
		lockHeld = false
	}
	return connection, nil
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
	addr := configuredRemoteAddr(remote)
	if strings.HasPrefix(addr, "unix:") {
		info.Mode = "socket"
		info.Protocol = "unix"
		info.Auth = "unix_socket"
	} else {
		info.Protocol = core.Blank(strings.TrimSpace(remote.Protocol), "incus")
		info.Auth = remoteAuthType(remote)
	}
	info.Endpoint = addr
	info.Project = selectedProject(cfg, &remote)
	info.Remote = remoteName
	return info, nil
}

func connectionArgsForAddress(cfg Config) (*incusclient.ConnectionArgs, error) {
	args, _, err := connectionArgsForAddressWithTokenPath(cfg)
	return args, err
}

func connectionArgsForAddressWithTokenPath(cfg Config) (*incusclient.ConnectionArgs, string, error) {
	args := &incusclient.ConnectionArgs{
		InsecureSkipVerify: cfg.Incus.InsecureTLS,
	}
	if certPath := strings.TrimSpace(cfg.Incus.TLSServerCert); certPath != "" {
		content, err := os.ReadFile(certPath)
		if err != nil {
			return nil, "", core.Exit(2, "read incus TLS server cert %q: %v", certPath, err)
		}
		args.TLSServerCert = strings.TrimSpace(string(content))
	}
	clientConfig, configErr := cliconfig.LoadConfig("")
	if configErr != nil {
		if args.TLSServerCert != "" || args.InsecureSkipVerify {
			clientConfig = nil
		} else {
			return nil, "", core.Exit(2, "load incus client config for TLS credentials: %v", configErr)
		}
	}
	oidcTokenPath := ""
	if clientConfig != nil {
		address := strings.TrimSpace(cfg.Incus.Address)
		matchedRemote := ""
		remoteName := configuredRemoteName(cfg, clientConfig)
		if remoteName != "" {
			if remoteConfig, ok := clientConfig.Remotes[remoteName]; ok && addressMatchesRemote(address, remoteConfig) {
				matchedRemote = remoteName
			}
		}
		if matchedRemote == "" {
			for name, remoteConfig := range clientConfig.Remotes {
				if name == remoteName {
					continue
				}
				if addressMatchesRemote(address, remoteConfig) {
					matchedRemote = name
					break
				}
			}
		}
		if matchedRemote != "" {
			if remoteConfig, ok := clientConfig.Remotes[matchedRemote]; ok {
				if args.TLSServerCert == "" {
					serverCert, certErr := loadRemoteServerCert(clientConfig, matchedRemote)
					if certErr != nil {
						return nil, "", core.Exit(2, "read incus server cert for remote %q: %v", matchedRemote, certErr)
					}
					args.TLSServerCert = serverCert
				}
				if remoteConfig.AuthType == api.AuthenticationMethodOIDC && !args.InsecureSkipVerify {
					args.AuthType = api.AuthenticationMethodOIDC
					tokens, tokenErr := loadRemoteOIDCTokens(clientConfig, matchedRemote)
					if tokenErr != nil {
						return nil, "", core.Exit(2, "read incus OIDC tokens for remote %q: %v", matchedRemote, tokenErr)
					}
					args.OIDCTokens = tokens
					oidcTokenPath = clientConfig.OIDCTokenPath(matchedRemote)
				} else {
					cert, key, ca, certErr := clientConfig.GetClientCertificate(matchedRemote)
					if certErr == nil {
						args.TLSClientCert = cert
						args.TLSClientKey = key
						args.TLSCA = ca
					}
				}
			}
		}
	}
	hasTLSClientAuth := strings.TrimSpace(args.TLSClientCert) != "" && strings.TrimSpace(args.TLSClientKey) != ""
	if !hasTLSClientAuth && args.AuthType != api.AuthenticationMethodOIDC {
		return nil, "", core.Exit(2, "provider=%s address mode requires a matching authenticated Incus remote with TLS client credentials or OIDC; --incus-insecure-tls only disables server certificate verification", providerName)
	}
	return args, oidcTokenPath, nil
}

type oidcTokenSource interface {
	GetOIDCTokens() *oidc.Tokens[*oidc.IDTokenClaims]
}

func oidcTokenCallbacks(server any, path string) (func() error, func() error, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil, nil
	}
	source, ok := server.(oidcTokenSource)
	if !ok || source.GetOIDCTokens() == nil {
		return nil, nil, fmt.Errorf("Incus OIDC client does not expose refreshed tokens")
	}
	reload := func() error {
		tokens, err := loadOIDCTokens(path)
		if err != nil {
			return err
		}
		*source.GetOIDCTokens() = *tokens
		return nil
	}
	save := func() error {
		return writeOIDCTokens(path, source.GetOIDCTokens())
	}
	return reload, save, nil
}

func writeOIDCTokens(path string, tokens *oidc.Tokens[*oidc.IDTokenClaims]) error {
	if tokens == nil {
		return nil
	}
	data, err := json.Marshal(tokens)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func lockOIDCTokens(path string) (*flock.Flock, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}
	if runtime.GOOS == "windows" {
		return nil, core.Exit(2, "provider=%s OIDC remotes are not supported on Windows because refreshed token files cannot be replaced atomically; use TLS client certificate authentication", providerName)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	lock := flock.New(path+".lock", flock.SetPermissions(0o600))
	if err := lock.Lock(); err != nil {
		return nil, fmt.Errorf("lock Incus OIDC tokens: %w", err)
	}
	return lock, nil
}

func disableOIDCKeepAlive(clientConfig *cliconfig.Config, remoteName string) {
	remote, ok := clientConfig.Remotes[remoteName]
	if !ok || remote.AuthType != api.AuthenticationMethodOIDC || remote.KeepAlive == 0 {
		return
	}
	remote.KeepAlive = 0
	clientConfig.Remotes[remoteName] = remote
}

func doctorAddressAuth(cfg Config) (string, error) {
	args, err := connectionArgsForAddress(cfg)
	if err != nil {
		return "", err
	}
	switch {
	case args.AuthType == api.AuthenticationMethodOIDC:
		return "oidc", nil
	case strings.TrimSpace(args.TLSClientCert) != "" && args.InsecureSkipVerify:
		return "tls_client_cert_insecure_tls", nil
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
	return loadOIDCTokens(clientConfig.OIDCTokenPath(remoteName))
}

func loadOIDCTokens(path string) (*oidc.Tokens[*oidc.IDTokenClaims], error) {
	content, err := os.ReadFile(path)
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
		if isLocalDaemonRemote(remoteName) {
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

func isLocalDaemonRemote(name string) bool {
	clientConfig, err := cliconfig.LoadConfig("")
	if err != nil {
		clientConfig = cliconfig.DefaultConfig()
	}
	remote, ok := clientConfig.Remotes[name]
	if !ok {
		return false
	}
	addr := configuredRemoteAddr(remote)
	return strings.HasPrefix(addr, "unix:")
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
