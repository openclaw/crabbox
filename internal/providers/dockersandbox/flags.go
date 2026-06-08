package dockersandbox

import (
	"flag"
	"fmt"
	"math"
	"path"
	"strings"
)

type stringListFlag []string

func (f *stringListFlag) String() string {
	return strings.Join(*f, ",")
}

func (f *stringListFlag) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	*f = append(*f, value)
	return nil
}

type flagValues struct {
	CLIPath         *string
	Agent           *string
	Template        *string
	CPUs            *float64
	Memory          *string
	Clone           *bool
	Workdir         *string
	ExtraWorkspaces *stringListFlag
	MCP             *stringListFlag
	Kit             *stringListFlag
}

func RegisterDockerSandboxProviderFlags(fs *flag.FlagSet, defaults Config) any {
	extraWorkspaces := stringListFlag(append([]string(nil), defaults.DockerSandbox.ExtraWorkspaces...))
	mcp := stringListFlag(append([]string(nil), defaults.DockerSandbox.MCP...))
	kit := stringListFlag(append([]string(nil), defaults.DockerSandbox.Kit...))
	fs.Var(&extraWorkspaces, "docker-sandbox-extra-workspace", "additional host workspace path for Docker Sandbox; repeatable")
	fs.Var(&mcp, "docker-sandbox-mcp", "Docker Sandbox MCP server reference; repeatable")
	fs.Var(&kit, "docker-sandbox-kit", "Docker Sandbox kit reference to attach; repeatable")
	return flagValues{
		CLIPath:         fs.String("docker-sandbox-cli", defaults.DockerSandbox.CLIPath, "path to the sbx CLI binary"),
		Agent:           fs.String("docker-sandbox-agent", defaults.DockerSandbox.Agent, "Docker Sandbox agent; v1 supports shell only"),
		Template:        fs.String("docker-sandbox-template", defaults.DockerSandbox.Template, "Docker Sandbox template"),
		CPUs:            fs.Float64("docker-sandbox-cpus", defaults.DockerSandbox.CPUs, "Docker Sandbox CPU count"),
		Memory:          fs.String("docker-sandbox-memory", defaults.DockerSandbox.Memory, "Docker Sandbox memory size"),
		Clone:           fs.Bool("docker-sandbox-clone", defaults.DockerSandbox.Clone, "use sbx create --clone for a Git repository workspace"),
		Workdir:         fs.String("docker-sandbox-workdir", defaults.DockerSandbox.Workdir, "absolute working directory inside the Docker Sandbox"),
		ExtraWorkspaces: &extraWorkspaces,
		MCP:             &mcp,
		Kit:             &kit,
	}
}

func ApplyDockerSandboxProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == providerName {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=%s; use --docker-sandbox-cpus or --docker-sandbox-memory", providerName)
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=%s; use --docker-sandbox-template", providerName)
		}
	}
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "docker-sandbox-cli") {
		cfg.DockerSandbox.CLIPath = *v.CLIPath
	}
	if flagWasSet(fs, "docker-sandbox-agent") {
		cfg.DockerSandbox.Agent = *v.Agent
	}
	if flagWasSet(fs, "docker-sandbox-template") {
		cfg.DockerSandbox.Template = *v.Template
	}
	if flagWasSet(fs, "docker-sandbox-cpus") {
		cfg.DockerSandbox.CPUs = *v.CPUs
	}
	if flagWasSet(fs, "docker-sandbox-memory") {
		cfg.DockerSandbox.Memory = *v.Memory
	}
	if flagWasSet(fs, "docker-sandbox-clone") {
		cfg.DockerSandbox.Clone = *v.Clone
	}
	if flagWasSet(fs, "docker-sandbox-workdir") {
		cfg.DockerSandbox.Workdir = *v.Workdir
	}
	if flagWasSet(fs, "docker-sandbox-extra-workspace") {
		cfg.DockerSandbox.ExtraWorkspaces = append([]string(nil), (*v.ExtraWorkspaces)...)
	}
	if flagWasSet(fs, "docker-sandbox-mcp") {
		cfg.DockerSandbox.MCP = append([]string(nil), (*v.MCP)...)
	}
	if flagWasSet(fs, "docker-sandbox-kit") {
		cfg.DockerSandbox.Kit = append([]string(nil), (*v.Kit)...)
	}
	return validateConfig(*cfg)
}

func validateConfig(cfg Config) error {
	agent := strings.TrimSpace(cfg.DockerSandbox.Agent)
	if agent == "" {
		agent = defaultAgent
	}
	if agent != defaultAgent {
		return exit(2, "docker-sandbox agent %q is not supported yet; v1 supports shell only", agent)
	}
	if math.IsNaN(cfg.DockerSandbox.CPUs) || math.IsInf(cfg.DockerSandbox.CPUs, 0) {
		return exit(2, "docker-sandbox cpus must be finite")
	}
	if cfg.DockerSandbox.CPUs < 0 {
		return exit(2, "docker-sandbox cpus must be greater than zero")
	}
	if cfg.DockerSandbox.CPUs != math.Trunc(cfg.DockerSandbox.CPUs) {
		return exit(2, "docker-sandbox cpus must be a whole number")
	}
	if workdir := strings.TrimSpace(cfg.DockerSandbox.Workdir); workdir != "" {
		clean := path.Clean(workdir)
		if !strings.HasPrefix(clean, "/") {
			return exit(2, "docker-sandbox workdir %q must be an absolute path", workdir)
		}
		if clean == "/" {
			return exit(2, "docker-sandbox workdir %q is too broad; choose a dedicated workspace path", workdir)
		}
	}
	for _, field := range []struct {
		name   string
		values []string
	}{
		{name: "extra workspace", values: cfg.DockerSandbox.ExtraWorkspaces},
		{name: "mcp", values: cfg.DockerSandbox.MCP},
		{name: "kit", values: cfg.DockerSandbox.Kit},
	} {
		for _, value := range field.values {
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("docker-sandbox %s entries must not be empty", field.name)
			}
		}
	}
	return nil
}
