package cli

//go:generate go run ../../scripts/configgen -source config_cubesandbox.go -output config_cubesandbox_generated.go -type CubeSandboxConfig -provider cubesandbox

// CubeSandboxConfig describes mechanical bindings; endpoint provenance remains
// owned by the loader and credential destination policy.
type CubeSandboxConfig struct {
	APIKey        string `env:"CRABBOX_CUBESANDBOX_API_KEY" envAlias:"CUBE_API_KEY" envAlias2:"E2B_API_KEY" sources:"env"`
	APIURL        string `config:"apiUrl" env:"CRABBOX_CUBESANDBOX_API_URL" envAlias:"CUBE_API_URL" envAlias2:"E2B_API_URL" flag:"cubesandbox-api-url" sources:"user,repo,env,flag" help:"CubeSandbox API URL" default:"http://127.0.0.1:3000" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	Domain        string `config:"domain" env:"CRABBOX_CUBESANDBOX_DOMAIN" envAlias:"CUBE_SANDBOX_DOMAIN" flag:"cubesandbox-domain" sources:"user,repo,env,flag" help:"CubeSandbox sandbox domain" default:"cube.app" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	Template      string `config:"template" env:"CRABBOX_CUBESANDBOX_TEMPLATE" envAlias:"CUBE_TEMPLATE_ID" flag:"cubesandbox-template" sources:"user,repo,env,flag" help:"CubeSandbox sandbox template ID" fileIgnoreEmpty:"true" fileStorage:"value"`
	Workdir       string `config:"workdir" env:"CRABBOX_CUBESANDBOX_WORKDIR" flag:"cubesandbox-workdir" sources:"user,repo,env,flag" help:"CubeSandbox sandbox working directory" default:"crabbox" fileIgnoreEmpty:"true" fileStorage:"value"`
	User          string `config:"user" env:"CRABBOX_CUBESANDBOX_USER" flag:"cubesandbox-user" sources:"user,repo,env,flag" help:"CubeSandbox sandbox user for command and file ownership" fileIgnoreEmpty:"true" fileStorage:"value"`
	ProxyNodeIP   string `config:"proxyNodeIp" env:"CRABBOX_CUBESANDBOX_PROXY_NODE_IP" envAlias:"CUBE_PROXY_NODE_IP" flag:"cubesandbox-proxy-node-ip" sources:"user,repo,env,flag" help:"CubeSandbox CubeProxy node IP or host for data-plane requests" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	ProxyPortHTTP int    `config:"proxyPortHttp" env:"CRABBOX_CUBESANDBOX_PROXY_PORT_HTTP" envAlias:"CUBE_PROXY_PORT_HTTP" flag:"cubesandbox-proxy-port-http" sources:"user,repo,env,flag" help:"CubeSandbox CubeProxy HTTP/HTTPS port" default:"80" fileInt:"positive" fileStorage:"value" envInt:"checked-signed-alias" envIntErrorLabel:"cubesandbox proxy HTTP port" reportApplied:"true"`
	ProxyScheme   string `config:"proxyScheme" env:"CRABBOX_CUBESANDBOX_PROXY_SCHEME" envAlias:"CUBE_PROXY_SCHEME" flag:"cubesandbox-proxy-scheme" sources:"user,repo,env,flag" help:"CubeSandbox CubeProxy scheme (http or https)" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
}
