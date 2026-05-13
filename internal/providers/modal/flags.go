package modal

import "flag"

type modalFlagValues struct {
	App     *string
	Image   *string
	Workdir *string
	Python  *string
}

func RegisterModalProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return modalFlagValues{
		App:     fs.String("modal-app", defaults.Modal.App, "Modal app name for Crabbox sandboxes"),
		Image:   fs.String("modal-image", defaults.Modal.Image, "Modal sandbox image, as a registry reference"),
		Workdir: fs.String("modal-workdir", defaults.Modal.Workdir, "Absolute working directory inside the Modal sandbox"),
		Python:  fs.String("modal-python", defaults.Modal.Python, "Python binary used to run the local Modal client"),
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
	return nil
}
