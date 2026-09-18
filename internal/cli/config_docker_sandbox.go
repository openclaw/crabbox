package cli

//go:generate go run ../../scripts/configgen -source config_docker_sandbox.go -output config_docker_sandbox_generated.go -type DockerSandboxConfig -provider docker-sandbox

// DockerSandboxConfig owns input bindings; final validation stays provider-owned.
type DockerSandboxConfig struct {
	CLIPath         string   `config:"cliPath" env:"CRABBOX_DOCKER_SANDBOX_CLI" flag:"docker-sandbox-cli" sources:"user,repo,env,flag" help:"path to the sbx CLI binary" default:"sbx" fileIgnoreEmpty:"true" fileStorage:"value"`
	Agent           string   `config:"agent" env:"CRABBOX_DOCKER_SANDBOX_AGENT" flag:"docker-sandbox-agent" sources:"user,repo,env,flag" help:"Docker Sandbox agent; v1 supports shell only" default:"shell" fileIgnoreEmpty:"true" fileStorage:"value"`
	Template        string   `config:"template" env:"CRABBOX_DOCKER_SANDBOX_TEMPLATE" flag:"docker-sandbox-template" sources:"user,repo,env,flag" help:"Docker Sandbox template"`
	CPUs            float64  `config:"cpus" env:"CRABBOX_DOCKER_SANDBOX_CPUS" flag:"docker-sandbox-cpus" sources:"user,repo,env,flag" help:"Docker Sandbox CPU count" envFloat:"checked" fileFloat:"nonnegative"`
	Memory          string   `config:"memory" env:"CRABBOX_DOCKER_SANDBOX_MEMORY" flag:"docker-sandbox-memory" sources:"user,repo,env,flag" help:"Docker Sandbox memory size"`
	Clone           bool     `config:"clone" env:"CRABBOX_DOCKER_SANDBOX_CLONE" flag:"docker-sandbox-clone" sources:"user,repo,env,flag" help:"use sbx create --clone for a Git repository workspace"`
	Workdir         string   `config:"workdir" env:"CRABBOX_DOCKER_SANDBOX_WORKDIR" flag:"docker-sandbox-workdir" sources:"user,repo,env,flag" help:"absolute working directory inside the Docker Sandbox"`
	ExtraWorkspaces []string `config:"extraWorkspaces" env:"CRABBOX_DOCKER_SANDBOX_EXTRA_WORKSPACES" flag:"docker-sandbox-extra-workspace" sources:"user,repo,env,flag" help:"additional host workspace path for Docker Sandbox; repeatable" fileList:"raw" envList:"presence" flagList:"append-trimmed-nonempty"`
	MCP             []string `config:"mcp" env:"CRABBOX_DOCKER_SANDBOX_MCP" flag:"docker-sandbox-mcp" sources:"user,repo,env,flag" help:"Docker Sandbox MCP server reference; repeatable" fileList:"raw" envList:"presence" flagList:"append-trimmed-nonempty"`
	Kit             []string `config:"kit" env:"CRABBOX_DOCKER_SANDBOX_KIT" flag:"docker-sandbox-kit" sources:"user,repo,env,flag" help:"Docker Sandbox kit reference to attach; repeatable" fileList:"raw" envList:"presence" flagList:"append-trimmed-nonempty"`
}
