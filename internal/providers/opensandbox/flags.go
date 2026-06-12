package opensandbox

import (
	"flag"
	"strings"
)

type openSandboxFlagValues struct {
	APIURL          *string
	Image           *string
	Workdir         *string
	CPU             *string
	Memory          *string
	TimeoutSecs     *int
	ExecTimeoutSecs *int
	PlatformOS      *string
	PlatformArch    *string
	SecureAccess    *bool
	UseServerProxy  *bool
	ForgetMissing   *bool
}

func RegisterOpenSandboxProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return openSandboxFlagValues{
		APIURL:          fs.String("opensandbox-api-url", defaults.OpenSandbox.APIURL, "Trusted OpenSandbox API base URL; not accepted from repository config"),
		Image:           fs.String("opensandbox-image", defaults.OpenSandbox.Image, "OpenSandbox container image URI"),
		Workdir:         fs.String("opensandbox-workdir", defaults.OpenSandbox.Workdir, "Absolute working directory inside the sandbox (also used as sync target)"),
		CPU:             fs.String("opensandbox-cpu", defaults.OpenSandbox.CPU, "OpenSandbox CPU resource limit string (empty = service default)"),
		Memory:          fs.String("opensandbox-memory", defaults.OpenSandbox.Memory, "OpenSandbox memory resource limit string (empty = service default)"),
		TimeoutSecs:     fs.Int("opensandbox-timeout-secs", defaults.OpenSandbox.TimeoutSecs, "OpenSandbox sandbox lifetime cap and readiness budget in seconds (0 = Crabbox TTL)"),
		ExecTimeoutSecs: fs.Int("opensandbox-exec-timeout-secs", defaults.OpenSandbox.ExecTimeoutSecs, "OpenSandbox command timeout in seconds (0 = Crabbox default 600)"),
		PlatformOS:      fs.String("opensandbox-platform-os", defaults.OpenSandbox.PlatformOS, "OpenSandbox platform OS constraint (set with --opensandbox-platform-arch; both empty = service default)"),
		PlatformArch:    fs.String("opensandbox-platform-arch", defaults.OpenSandbox.PlatformArch, "OpenSandbox platform architecture constraint (set with --opensandbox-platform-os; both empty = service default)"),
		SecureAccess:    fs.Bool("opensandbox-secure-access", defaults.OpenSandbox.SecureAccess, "request secured sandbox endpoints"),
		UseServerProxy:  fs.Bool("opensandbox-use-server-proxy", defaults.OpenSandbox.UseServerProxy, "route execd requests through the OpenSandbox server proxy"),
		ForgetMissing:   fs.Bool("opensandbox-forget-missing", defaults.OpenSandbox.ForgetMissing, "remove the local claim when stop gets 404 (explicit stale-claim cleanup)"),
	}
}

func ApplyOpenSandboxProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case providerName:
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=opensandbox; use --opensandbox-cpu and --opensandbox-memory")
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=opensandbox; use --opensandbox-cpu and --opensandbox-memory")
		}
	}
	v, ok := values.(openSandboxFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "opensandbox-api-url") {
		cfg.OpenSandbox.APIURL = *v.APIURL
	}
	if flagWasSet(fs, "opensandbox-image") {
		cfg.OpenSandbox.Image = *v.Image
	}
	if flagWasSet(fs, "opensandbox-workdir") {
		cfg.OpenSandbox.Workdir = *v.Workdir
	}
	if flagWasSet(fs, "opensandbox-cpu") {
		cfg.OpenSandbox.CPU = *v.CPU
	}
	if flagWasSet(fs, "opensandbox-memory") {
		cfg.OpenSandbox.Memory = *v.Memory
	}
	if flagWasSet(fs, "opensandbox-timeout-secs") {
		cfg.OpenSandbox.TimeoutSecs = *v.TimeoutSecs
	}
	if flagWasSet(fs, "opensandbox-exec-timeout-secs") {
		cfg.OpenSandbox.ExecTimeoutSecs = *v.ExecTimeoutSecs
	}
	if flagWasSet(fs, "opensandbox-platform-os") {
		cfg.OpenSandbox.PlatformOS = *v.PlatformOS
	}
	if flagWasSet(fs, "opensandbox-platform-arch") {
		cfg.OpenSandbox.PlatformArch = *v.PlatformArch
	}
	if flagWasSet(fs, "opensandbox-secure-access") {
		cfg.OpenSandbox.SecureAccess = *v.SecureAccess
	}
	if flagWasSet(fs, "opensandbox-use-server-proxy") {
		cfg.OpenSandbox.UseServerProxy = *v.UseServerProxy
	}
	if flagWasSet(fs, "opensandbox-forget-missing") {
		cfg.OpenSandbox.ForgetMissing = *v.ForgetMissing
	}
	return validateOpenSandboxConfig(*cfg)
}
