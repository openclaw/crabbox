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
	hasCommand := strings.TrimSpace(cfg.External.Command) != ""
	hasLifecycle := lifecycleConfigured(cfg.External)
	if hasCommand == hasLifecycle {
		return core.Exit(2, "configure exactly one of external.command or external.lifecycle.acquire")
	}
	if hasCommand && strings.ContainsRune(cfg.External.Command, '\x00') {
		return core.Exit(2, "external.command contains a NUL byte")
	}
	for _, arg := range cfg.External.Args {
		if strings.ContainsRune(arg, '\x00') {
			return core.Exit(2, "external.args contains a NUL byte")
		}
	}
	if hasLifecycle {
		if len(cfg.External.Lifecycle.Release.Argv) == 0 {
			return core.Exit(2, "external.lifecycle.release.argv is required")
		}
		if len(cfg.External.Lifecycle.List.Argv) == 0 {
			return core.Exit(2, "external.lifecycle.list.argv is required")
		}
		for name, operation := range map[string]core.ExternalLifecycleOperation{
			"doctor":  cfg.External.Lifecycle.Doctor,
			"acquire": cfg.External.Lifecycle.Acquire,
			"resolve": cfg.External.Lifecycle.Resolve,
			"list":    cfg.External.Lifecycle.List,
			"release": cfg.External.Lifecycle.Release,
			"touch":   cfg.External.Lifecycle.Touch,
			"cleanup": cfg.External.Lifecycle.Cleanup,
		} {
			if err := validateLifecycleOperation(name, operation); err != nil {
				return err
			}
		}
		if strings.TrimSpace(cfg.External.Connection.SSH.User) == "" {
			return core.Exit(2, "external.connection.ssh.user is required")
		}
		if cfg.External.Lifecycle.List.Output != lifecycleOutputJSONNameArray && cfg.External.Lifecycle.List.Output != lifecycleOutputJSONLeaseArray {
			return core.Exit(2, "external.lifecycle.list.output must be %q or %q", lifecycleOutputJSONNameArray, lifecycleOutputJSONLeaseArray)
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

func validateLifecycleOperation(name string, operation core.ExternalLifecycleOperation) error {
	if name != "list" && operation.Output != lifecycleOutputNone {
		return core.Exit(2, "external.lifecycle.%s.output is only supported for list", name)
	}
	if name != "list" && operation.NamePrefix != "" {
		return core.Exit(2, "external.lifecycle.%s.namePrefix is only supported for list", name)
	}
	if operation.NamePrefix != "" && operation.Output != lifecycleOutputJSONNameArray {
		return core.Exit(2, "external.lifecycle.list.namePrefix requires output %q", lifecycleOutputJSONNameArray)
	}
	switch operation.Output {
	case lifecycleOutputNone, lifecycleOutputJSONNameArray, lifecycleOutputJSONLeaseArray:
	default:
		return core.Exit(2, "external.lifecycle.%s.output %q is unsupported", name, operation.Output)
	}
	for index, arg := range operation.Argv {
		if strings.ContainsRune(arg, '\x00') {
			return core.Exit(2, "external.lifecycle.%s.argv[%d] contains a NUL byte", name, index)
		}
	}
	if len(operation.Argv) > 0 && strings.TrimSpace(operation.Argv[0]) == "" {
		return core.Exit(2, "external.lifecycle.%s.argv executable is empty", name)
	}
	return nil
}

func externalWorkRoot(cfg core.Config) string {
	return core.Blank(strings.TrimSpace(cfg.External.WorkRoot), "/workspaces/crabbox")
}
