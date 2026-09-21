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

func RegisterDockerSandboxProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterDockerSandboxConfigFlags(fs, defaults.DockerSandbox)
}

func ApplyDockerSandboxProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == providerName {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "use --docker-sandbox-cpus or --docker-sandbox-memory", "use --docker-sandbox-template"); err != nil {
			return err
		}
	}
	if ok, err := core.ApplyProviderConfigFlags[core.DockerSandboxConfigFlagValues](cfg, fs, values, &cfg.DockerSandbox, "docker-sandbox"); !ok || err != nil {
		return err
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
