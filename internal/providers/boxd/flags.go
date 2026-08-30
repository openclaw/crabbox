package boxd

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

type flagValues struct {
	APIURL          *string
	Org             *string
	WorkRoot        *string
	DeleteOnRelease *bool
}

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return flagValues{
		APIURL:          fs.String("boxd-api-url", defaults.Boxd.APIURL, "boxd HTTPS console origin (default https://app.boxd.sh)"),
		Org:             fs.String("boxd-org", defaults.Boxd.Org, "boxd organization (empty = personal account)"),
		WorkRoot:        fs.String("boxd-work-root", defaults.Boxd.WorkRoot, "remote Crabbox work root"),
		DeleteOnRelease: fs.Bool("boxd-delete-on-release", defaults.Boxd.DeleteOnRelease, "destroy boxd machines on release instead of stopping them (default true)"),
	}
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if isProviderName(cfg.Provider) {
		if core.FlagWasSet(fs, "class") {
			return core.Exit(2, "--class is not supported for provider=%s; machine sizing follows the boxd account quota", providerName)
		}
		if core.FlagWasSet(fs, "type") {
			return core.Exit(2, "--type is not supported for provider=%s; machine sizing follows the boxd account quota", providerName)
		}
		if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
			return core.Exit(2, "provider=%s supports target=linux only", providerName)
		}
	}
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if core.FlagWasSet(fs, "boxd-api-url") {
		cfg.Boxd.APIURL = *v.APIURL
	}
	if core.FlagWasSet(fs, "boxd-org") {
		cfg.Boxd.Org = *v.Org
	}
	if core.FlagWasSet(fs, "boxd-work-root") {
		cfg.Boxd.WorkRoot = *v.WorkRoot
		core.MarkBoxdWorkRootExplicit(cfg)
	}
	if core.FlagWasSet(fs, "boxd-delete-on-release") {
		cfg.Boxd.DeleteOnRelease = *v.DeleteOnRelease
		core.MarkDeleteOnReleaseExplicit(cfg, providerName)
	}
	if isProviderName(cfg.Provider) {
		applyDefaults(cfg)
	}
	return nil
}

func isProviderName(provider string) bool {
	return strings.EqualFold(strings.TrimSpace(provider), providerName)
}
