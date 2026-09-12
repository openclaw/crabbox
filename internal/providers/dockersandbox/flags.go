package dockersandbox

import (
	"flag"
	"fmt"
	"math"
	"path"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
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

func (f *stringListFlag) Get() any {
	return append([]string{}, (*f)...)
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

func RegisterDockerSandboxProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
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

func ApplyDockerSandboxProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == providerName {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "use --docker-sandbox-cpus or --docker-sandbox-memory", "use --docker-sandbox-template"); err != nil {
			return err
		}
	}
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if core.FlagWasSet(fs, "docker-sandbox-cli") {
		cfg.DockerSandbox.CLIPath = *v.CLIPath
		core.RecordProviderFlagInputs(cfg, true, "docker-sandbox")
	}
	if core.FlagWasSet(fs, "docker-sandbox-agent") {
		cfg.DockerSandbox.Agent = *v.Agent
		core.RecordProviderFlagInputs(cfg, true, "docker-sandbox")
	}
	if core.FlagWasSet(fs, "docker-sandbox-template") {
		cfg.DockerSandbox.Template = *v.Template
		core.RecordProviderFlagInputs(cfg, true, "docker-sandbox")
	}
	if core.FlagWasSet(fs, "docker-sandbox-cpus") {
		cfg.DockerSandbox.CPUs = *v.CPUs
		core.RecordProviderFlagInputs(cfg, true, "docker-sandbox")
	}
	if core.FlagWasSet(fs, "docker-sandbox-memory") {
		cfg.DockerSandbox.Memory = *v.Memory
		core.RecordProviderFlagInputs(cfg, true, "docker-sandbox")
	}
	if core.FlagWasSet(fs, "docker-sandbox-clone") {
		cfg.DockerSandbox.Clone = *v.Clone
		core.RecordProviderFlagInputs(cfg, true, "docker-sandbox")
	}
	if core.FlagWasSet(fs, "docker-sandbox-workdir") {
		cfg.DockerSandbox.Workdir = *v.Workdir
		core.RecordProviderFlagInputs(cfg, true, "docker-sandbox")
	}
	if core.FlagWasSet(fs, "docker-sandbox-extra-workspace") {
		cfg.DockerSandbox.ExtraWorkspaces = append([]string(nil), (*v.ExtraWorkspaces)...)
		core.RecordProviderFlagInputs(cfg, true, "docker-sandbox")
	}
	if core.FlagWasSet(fs, "docker-sandbox-mcp") {
		cfg.DockerSandbox.MCP = append([]string(nil), (*v.MCP)...)
		core.RecordProviderFlagInputs(cfg, true, "docker-sandbox")
	}
	if core.FlagWasSet(fs, "docker-sandbox-kit") {
		cfg.DockerSandbox.Kit = append([]string(nil), (*v.Kit)...)
		core.RecordProviderFlagInputs(cfg, true, "docker-sandbox")
	}
	return validateConfig(*cfg)
}

func validateConfig(cfg core.Config) error {
	agent := strings.TrimSpace(cfg.DockerSandbox.Agent)
	if agent == "" {
		agent = defaultAgent
	}
	if agent != defaultAgent {
		return core.Exit(2, "docker-sandbox agent %q is not supported yet; v1 supports shell only", agent)
	}
	if math.IsNaN(cfg.DockerSandbox.CPUs) || math.IsInf(cfg.DockerSandbox.CPUs, 0) {
		return core.Exit(2, "docker-sandbox cpus must be finite")
	}
	if cfg.DockerSandbox.CPUs < 0 {
		return core.Exit(2, "docker-sandbox cpus must be greater than zero")
	}
	if cfg.DockerSandbox.CPUs != math.Trunc(cfg.DockerSandbox.CPUs) {
		return core.Exit(2, "docker-sandbox cpus must be a whole number")
	}
	if workdir := strings.TrimSpace(cfg.DockerSandbox.Workdir); workdir != "" {
		clean := path.Clean(workdir)
		if !strings.HasPrefix(clean, "/") {
			return core.Exit(2, "docker-sandbox workdir %q must be an absolute path", workdir)
		}
		if clean == "/" {
			return core.Exit(2, "docker-sandbox workdir %q is too broad; choose a dedicated workspace path", workdir)
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
