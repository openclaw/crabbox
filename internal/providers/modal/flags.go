package modal

import (
	"flag"
	"strings"
)

type modalFlagValues struct {
	App         *string
	Image       *string
	Workdir     *string
	Python      *string
	Environment *string
	Secrets     *modalSecretList
}

type modalSecretList struct {
	values []string
	set    bool
}

func (s *modalSecretList) String() string { return strings.Join(s.values, ",") }

func (s *modalSecretList) Set(value string) error {
	if !s.set {
		s.values = nil
		s.set = true
	}
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			s.values = append(s.values, item)
		}
	}
	return nil
}

func RegisterModalProviderFlags(fs *flag.FlagSet, defaults Config) any {
	secrets := modalSecretList{values: append([]string(nil), defaults.Modal.Secrets...)}
	fs.Var(&secrets, "modal-secret", "named Modal Secret to inject into the sandbox; repeatable or comma-separated")
	return modalFlagValues{
		App:         fs.String("modal-app", defaults.Modal.App, "Modal app name for Crabbox sandboxes"),
		Image:       fs.String("modal-image", defaults.Modal.Image, "Modal sandbox image, as a registry reference"),
		Workdir:     fs.String("modal-workdir", defaults.Modal.Workdir, "Absolute working directory inside the Modal sandbox"),
		Python:      fs.String("modal-python", defaults.Modal.Python, "Python binary used to run the local Modal client"),
		Environment: fs.String("modal-environment", defaults.Modal.Environment, "Modal environment for the sandbox and named Secrets"),
		Secrets:     &secrets,
	}
}

func ApplyModalProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == providerName {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=modal")
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=modal")
		}
	}
	v, ok := values.(modalFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "modal-app") {
		cfg.Modal.App = *v.App
	}
	if flagWasSet(fs, "modal-image") {
		cfg.Modal.Image = *v.Image
	}
	if flagWasSet(fs, "modal-workdir") {
		cfg.Modal.Workdir = *v.Workdir
	}
	if flagWasSet(fs, "modal-python") {
		cfg.Modal.Python = *v.Python
	}
	if flagWasSet(fs, "modal-environment") {
		cfg.Modal.Environment = *v.Environment
	}
	if flagWasSet(fs, "modal-secret") {
		cfg.Modal.Secrets = append([]string(nil), v.Secrets.values...)
	}
	return nil
}
