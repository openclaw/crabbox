package external

import (
	"encoding/json"
	"flag"
	"path"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

type flagValues struct {
	Command     *string
	Args        *stringListFlag
	ConfigJSON  *string
	WorkRoot    *string
	RoutingFile *string
}

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	args := &stringListFlag{}
	fs.Var(args, "external-arg", "external provider argument; repeatable")
	return flagValues{
		Command:    fs.String("external-command", defaults.External.Command, "external provider executable"),
		Args:       args,
		ConfigJSON: fs.String("external-config-json", "{}", "external provider config as a JSON object"),
		WorkRoot:   fs.String("external-work-root", defaults.External.WorkRoot, "external provider Crabbox work root"),
		RoutingFile: fs.String(
			"external-routing-file",
			defaults.External.RoutingFile,
			"private external provider routing state file",
		),
	}
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if core.FlagWasSet(fs, "external-command") {
		cfg.External.Command = *v.Command
	}
	if core.FlagWasSet(fs, "external-arg") {
		cfg.External.Args = append([]string(nil), v.Args.values...)
	}
	if core.FlagWasSet(fs, "external-config-json") {
		config := map[string]any{}
		if err := json.Unmarshal([]byte(*v.ConfigJSON), &config); err != nil {
			return core.Exit(2, "external config JSON must be an object: %v", err)
		}
		cfg.External.Config = config
	}
	if core.FlagWasSet(fs, "external-work-root") {
		cfg.External.WorkRoot = *v.WorkRoot
		cfg.WorkRoot = *v.WorkRoot
	}
	if core.FlagWasSet(fs, "external-routing-file") {
		cfg.External.RoutingFile = *v.RoutingFile
		routing, err := core.LoadExternalRouting(cfg.External.RoutingFile)
		if err != nil {
			return core.Exit(2, "%v", err)
		}
		cfg.External = routing
		cfg.WorkRoot = externalWorkRoot(*cfg)
	}
	return validateConfig(*cfg)
}

type stringListFlag struct {
	values []string
	set    bool
}

func (f *stringListFlag) String() string {
	return strings.Join(f.values, ",")
}

func (f *stringListFlag) Set(value string) error {
	if !f.set {
		f.values = nil
		f.set = true
	}
	f.values = append(f.values, value)
	return nil
}

func validateConfig(cfg core.Config) error {
	if strings.TrimSpace(cfg.External.Command) == "" {
		return core.Exit(2, "external.command is required")
	}
	if strings.ContainsRune(cfg.External.Command, '\x00') {
		return core.Exit(2, "external.command contains a NUL byte")
	}
	for _, arg := range cfg.External.Args {
		if strings.ContainsRune(arg, '\x00') {
			return core.Exit(2, "external.args contains a NUL byte")
		}
	}
	clean := path.Clean(externalWorkRoot(cfg))
	if !strings.HasPrefix(clean, "/") {
		return core.Exit(2, "external.workRoot %q must resolve to an absolute path", cfg.External.WorkRoot)
	}
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var":
		return core.Exit(2, "external.workRoot %q is too broad; choose a dedicated subdirectory", clean)
	}
	return nil
}

func externalWorkRoot(cfg core.Config) string {
	return core.Blank(strings.TrimSpace(cfg.External.WorkRoot), "/workspaces/crabbox")
}
