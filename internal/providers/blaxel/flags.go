package blaxel

import (
	"flag"
	"strings"
)

type flagValues struct {
	APIURL          *string
	Workspace       *string
	Region          *string
	Image           *string
	MemoryMB        *int
	TTL             *string
	IdleTTL         *string
	Workdir         *string
	ExecTimeoutSecs *int
	ForgetMissing   *bool
}

func RegisterBlaxelProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return flagValues{
		APIURL:          fs.String("blaxel-api-url", defaults.Blaxel.APIURL, "Trusted Blaxel API base URL; not accepted from repository config"),
		Workspace:       fs.String("blaxel-workspace", defaults.Blaxel.Workspace, "Blaxel workspace name or ID"),
		Region:          fs.String("blaxel-region", defaults.Blaxel.Region, "Blaxel deployment region (empty = service default/policy)"),
		Image:           fs.String("blaxel-image", defaults.Blaxel.Image, "Blaxel sandbox image"),
		MemoryMB:        fs.Int("blaxel-memory-mb", defaults.Blaxel.MemoryMB, "Blaxel sandbox memory in MB (0 = service default)"),
		TTL:             fs.String("blaxel-ttl", defaults.Blaxel.TTL, "Blaxel sandbox lifetime duration (empty = service default)"),
		IdleTTL:         fs.String("blaxel-idle-ttl", defaults.Blaxel.IdleTTL, "Blaxel sandbox idle timeout duration (empty = service default)"),
		Workdir:         fs.String("blaxel-workdir", defaults.Blaxel.Workdir, "absolute working directory inside the Blaxel sandbox"),
		ExecTimeoutSecs: fs.Int("blaxel-exec-timeout-secs", defaults.Blaxel.ExecTimeoutSecs, "Blaxel command timeout in seconds (0 = Crabbox default 600)"),
		ForgetMissing:   fs.Bool("blaxel-forget-missing", defaults.Blaxel.ForgetMissing, "remove the local claim when stop gets 404 (explicit stale-claim cleanup)"),
	}
}

func ApplyBlaxelProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if strings.EqualFold(strings.TrimSpace(cfg.Provider), providerName) {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=blaxel; use --blaxel-memory-mb")
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=blaxel; use --blaxel-image")
		}
	}
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "blaxel-api-url") {
		cfg.Blaxel.APIURL = *v.APIURL
	}
	if flagWasSet(fs, "blaxel-workspace") {
		cfg.Blaxel.Workspace = *v.Workspace
	}
	if flagWasSet(fs, "blaxel-region") {
		cfg.Blaxel.Region = *v.Region
	}
	if flagWasSet(fs, "blaxel-image") {
		cfg.Blaxel.Image = *v.Image
	}
	if flagWasSet(fs, "blaxel-memory-mb") {
		cfg.Blaxel.MemoryMB = *v.MemoryMB
	}
	if flagWasSet(fs, "blaxel-ttl") {
		cfg.Blaxel.TTL = *v.TTL
	}
	if flagWasSet(fs, "blaxel-idle-ttl") {
		cfg.Blaxel.IdleTTL = *v.IdleTTL
	}
	if flagWasSet(fs, "blaxel-workdir") {
		cfg.Blaxel.Workdir = *v.Workdir
	}
	if flagWasSet(fs, "blaxel-exec-timeout-secs") {
		cfg.Blaxel.ExecTimeoutSecs = *v.ExecTimeoutSecs
	}
	if flagWasSet(fs, "blaxel-forget-missing") {
		cfg.Blaxel.ForgetMissing = *v.ForgetMissing
	}
	return validateBlaxelConfig(*cfg)
}
