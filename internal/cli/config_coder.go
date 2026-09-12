package cli

import (
	"strings"

	"gopkg.in/yaml.v3"
)

//go:generate go run ../../scripts/configgen -source config_coder.go -output config_coder_generated.go -type CoderConfig -provider coder

// CoderConfig owns mechanical input bindings, not workspace or transport policy.
type CoderConfig struct {
	CLIPath              string   `config:"cliPath" env:"CRABBOX_CODER_CLI" flag:"coder-cli" sources:"user,repo,env,flag" help:"Coder CLI path" default:"coder" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	Template             string   `config:"template" env:"CRABBOX_CODER_TEMPLATE" flag:"coder-template" sources:"user,repo,env,flag" help:"Coder template for new workspaces" fileIgnoreEmpty:"true" fileStorage:"value"`
	Preset               string   `config:"preset" env:"CRABBOX_CODER_PRESET" flag:"coder-preset" sources:"user,repo,env,flag" help:"Coder template preset" fileIgnoreEmpty:"true" fileStorage:"value"`
	WorkspacePrefix      string   `config:"workspacePrefix" env:"CRABBOX_CODER_WORKSPACE_PREFIX" flag:"coder-workspace-prefix" sources:"user,repo,env,flag" help:"prefix for Crabbox-managed Coder workspace names" default:"crabbox-" fileIgnoreEmpty:"true" fileStorage:"value"`
	WorkRoot             string   `config:"workRoot" env:"CRABBOX_CODER_WORK_ROOT" flag:"coder-work-root" sources:"user,repo,env,flag" help:"Coder workspace Crabbox work root" default:"/home/coder/crabbox" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	DeleteOnRelease      bool     `config:"deleteOnRelease" env:"CRABBOX_CODER_DELETE_ON_RELEASE" flag:"coder-delete-on-release" sources:"user,repo,env,flag" help:"delete Coder workspace on release instead of stopping it"`
	Wait                 string   `config:"wait" env:"CRABBOX_CODER_WAIT" flag:"coder-wait" sources:"user,repo,env,flag" help:"Coder SSH startup wait mode: yes, no, or auto" default:"yes" fileIgnoreEmpty:"true" fileStorage:"value"`
	UseParameterDefaults bool     `config:"useParameterDefaults" env:"CRABBOX_CODER_USE_PARAMETER_DEFAULTS" flag:"coder-use-parameter-defaults" sources:"user,repo,env,flag" help:"pass --use-parameter-defaults to coder create"`
	Parameters           []string `config:"parameters" env:"CRABBOX_CODER_PARAMETERS" flag:"coder-parameter" sources:"user,repo,env,flag" help:"comma-separated Coder parameter values name=value" fileList:"nonempty-normalized" fileStorage:"value" envList:"trimmed-nonempty" flagList:"csv"`
	RichParameterFile    string   `config:"richParameterFile" env:"CRABBOX_CODER_RICH_PARAMETER_FILE" flag:"coder-rich-parameter-file" sources:"user,repo,env,flag" help:"Coder rich parameter file" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
}

func applyCoderFileConfig(cfg *Config, file *fileCoderConfig, source configInputSource) error {
	applied, err := cfg.Coder.applyFile(file)
	recordConfigInput(cfg, "coder", source, applied.InputAccepted)
	// Non-fallible, independent overlays do not observe these paths before expansion.
	if applied.CLIPath {
		cfg.Coder.CLIPath = expandUserPath(cfg.Coder.CLIPath)
	}
	if applied.RichParameterFile {
		cfg.Coder.RichParameterFile = expandUserPath(cfg.Coder.RichParameterFile)
	}
	return err
}

func (c *fileCoderConfig) UnmarshalYAML(node *yaml.Node) error {
	type plain fileCoderConfig
	var out plain
	if err := node.Decode(&out); err != nil {
		return err
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		value := node.Content[i+1]
		if key != "parameters" {
			continue
		}
		switch value.Kind {
		case yaml.SequenceNode:
			out.Parameters = out.Parameters[:0]
			for _, item := range value.Content {
				if strings.TrimSpace(item.Value) != "" {
					out.Parameters = append(out.Parameters, strings.TrimSpace(item.Value))
				}
			}
		case yaml.ScalarNode:
			out.Parameters = splitCommaList(value.Value)
		}
	}
	*c = fileCoderConfig(out)
	return nil
}
