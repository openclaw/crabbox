package cli

//go:generate go run ../../scripts/configgen -source config_static.go -output config_static_generated.go -type StaticConfig -provider ssh

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
)

// StaticConfig holds configured inputs; target projection and SSH credentials remain separate.
type StaticConfig struct {
	ID       string `config:"id" env:"CRABBOX_STATIC_ID" sources:"user,repo,env" fileIgnoreEmpty:"true" fileStorage:"value"`
	Name     string `config:"name" env:"CRABBOX_STATIC_NAME" sources:"user,repo,env" fileIgnoreEmpty:"true" fileStorage:"value"`
	Host     string `config:"host" env:"CRABBOX_STATIC_HOST" flag:"static-host" help:"static SSH host" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	User     string `config:"user" env:"CRABBOX_STATIC_USER" flag:"static-user" help:"static SSH user" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value"`
	Port     string `config:"port" env:"CRABBOX_STATIC_PORT" flag:"static-port" help:"static SSH port" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value"`
	WorkRoot string `config:"workRoot" env:"CRABBOX_STATIC_WORK_ROOT" flag:"static-work-root" help:"static target work root" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value"`
	// StartCommand and StopCommand are local argv arrays. Their JSON env and
	// flag encodings are outside configgen's scalar/list codecs.
	StartCommand []string `sources:"runtime"`
	StopCommand  []string `sources:"runtime"`
}

type fileStaticSection struct {
	fileStaticConfig `yaml:",inline"`
	StartCommand     []string `yaml:"startCommand,omitempty"`
	StopCommand      []string `yaml:"stopCommand,omitempty"`
}

type staticCommandApproval struct {
	start string
	stop  string
}

const (
	staticStartCommandEnv  = "CRABBOX_STATIC_START_COMMAND"
	staticStopCommandEnv   = "CRABBOX_STATIC_STOP_COMMAND"
	staticStartCommandFlag = "static-start-command"
	staticStopCommandFlag  = "static-stop-command"
)

func applyStaticFileConfig(cfg *Config, file *fileStaticSection, source configInputSource, credentialSource credentialValueSource) error {
	if file == nil {
		_, err := cfg.Static.applyFile(nil)
		return err
	}
	applied, err := cfg.Static.applyFile(&file.fileStaticConfig)
	recordConfigInput(cfg, "ssh", source, applied.InputAccepted)
	if applied.Host {
		cfg.credentialProvenance.staticHost = credentialSource
	}
	if err != nil {
		return err
	}
	provenance := &cfg.credentialProvenance
	for _, command := range []struct {
		field    string
		value    []string
		target   *[]string
		source   *credentialValueSource
		approved *string
	}{
		{"static.startCommand", file.StartCommand, &cfg.Static.StartCommand, &provenance.staticStartCommand, &provenance.staticCommandApproval.start},
		{"static.stopCommand", file.StopCommand, &cfg.Static.StopCommand, &provenance.staticStopCommand, &provenance.staticCommandApproval.stop},
	} {
		if command.value == nil {
			continue
		}
		argv, err := validateStaticCommand(command.field, command.value)
		if err != nil {
			return err
		}
		*command.target = argv
		recordConfigInput(cfg, "ssh", source, true)
		key := staticCommandKey(argv)
		if credentialSource == credentialSourceTrustedFile {
			*command.approved = key
		}
		*command.source = credentialDestinationSource(key, *command.approved, credentialSource)
	}
	return nil
}

func applyStaticEnvironmentConfig(cfg *Config) error {
	applied, err := cfg.Static.applyEnv()
	recordConfigInput(cfg, "ssh", configInputEnvironment, applied.InputAccepted)
	if applied.Host {
		cfg.credentialProvenance.staticHost = credentialSourceEnvironment
	}
	if err != nil {
		return err
	}
	for _, command := range []struct {
		env    string
		target *[]string
		source *credentialValueSource
	}{
		{staticStartCommandEnv, &cfg.Static.StartCommand, &cfg.credentialProvenance.staticStartCommand},
		{staticStopCommandEnv, &cfg.Static.StopCommand, &cfg.credentialProvenance.staticStopCommand},
	} {
		raw := strings.TrimSpace(os.Getenv(command.env))
		if raw == "" {
			continue
		}
		argv, err := parseStaticCommandJSON(command.env, raw)
		if err != nil {
			return err
		}
		*command.target = argv
		*command.source = credentialSourceEnvironment
		recordConfigInput(cfg, "ssh", configInputEnvironment, true)
	}
	return nil
}

type staticCommandFlagValues struct {
	Start *string
	Stop  *string
}

func registerStaticCommandFlags(fs *flag.FlagSet) staticCommandFlagValues {
	return staticCommandFlagValues{
		Start: fs.String(staticStartCommandFlag, "", `local static host start command as a JSON argv array, e.g. ["/usr/local/bin/host-power","up"]; [] clears`),
		Stop:  fs.String(staticStopCommandFlag, "", `local static host stop command as a JSON argv array, e.g. ["/usr/local/bin/host-power","down"]; [] clears`),
	}
}

func applyStaticCommandFlags(cfg *Config, fs *flag.FlagSet, values staticCommandFlagValues) error {
	for _, command := range []struct {
		name   string
		value  *string
		target *[]string
		source *credentialValueSource
	}{
		{staticStartCommandFlag, values.Start, &cfg.Static.StartCommand, &cfg.credentialProvenance.staticStartCommand},
		{staticStopCommandFlag, values.Stop, &cfg.Static.StopCommand, &cfg.credentialProvenance.staticStopCommand},
	} {
		if command.value == nil || !flagWasSet(fs, command.name) {
			continue
		}
		argv, err := parseStaticCommandJSON("--"+command.name, strings.TrimSpace(*command.value))
		if err != nil {
			return err
		}
		*command.target = argv
		*command.source = credentialSourceFlag
		recordConfigInput(cfg, "ssh", configInputFlag, true)
	}
	return nil
}

func parseStaticCommandJSON(name, raw string) ([]string, error) {
	var argv []string
	if err := json.Unmarshal([]byte(raw), &argv); err != nil {
		return nil, Exit(2, "%s must be a JSON argv array: %v", name, err)
	}
	return validateStaticCommand(name, argv)
}

// validateStaticCommand returns nil for an empty argv so explicit inputs can
// clear a lower-precedence command.
func validateStaticCommand(name string, argv []string) ([]string, error) {
	if len(argv) == 0 {
		return nil, nil
	}
	if strings.TrimSpace(argv[0]) == "" {
		return nil, Exit(2, "%s must start with an executable", name)
	}
	// Relative paths and PATH lookups can resolve into a repository-controlled
	// directory; approval must name one operator-controlled file.
	if !filepath.IsAbs(argv[0]) {
		return nil, Exit(2, "%s executable %q must be an absolute path", name, argv[0])
	}
	for _, arg := range argv {
		if strings.ContainsRune(arg, 0) {
			return nil, Exit(2, "%s contains a NUL byte", name)
		}
	}
	return append([]string(nil), argv...), nil
}

func staticCommandKey(argv []string) string {
	return strings.Join(argv, "\x00")
}

func validateStaticCommandSources(cfg Config) error {
	provenance := cfg.credentialProvenance
	for _, command := range []struct {
		field  string
		env    string
		flag   string
		argv   []string
		source credentialValueSource
	}{
		{"static.startCommand", staticStartCommandEnv, staticStartCommandFlag, cfg.Static.StartCommand, provenance.staticStartCommand},
		{"static.stopCommand", staticStopCommandEnv, staticStopCommandFlag, cfg.Static.StopCommand, provenance.staticStopCommand},
	} {
		if len(command.argv) > 0 && command.source == credentialSourceRepository {
			return Exit(2, "provider=%s refuses repository-configured %s because it runs a local command; set the same %s in trusted user config, %s, or --%s to approve it", staticProvider, command.field, command.field, command.env, command.flag)
		}
	}
	return nil
}
