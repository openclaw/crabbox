package cli

import (
	"context"
	"flag"
	"fmt"
	"time"
)

type leaseCreateFlagValues struct {
	Provider      *string
	Profile       *string
	Class         *string
	ServerType    *string
	Market        *string
	TTL           *time.Duration
	Idle          *time.Duration
	Desktop       *bool
	Browser       *bool
	Code          *bool
	ProviderFlags providerFlagValues
	Target        targetFlagValues
	Network       networkFlagValues
}

func registerLeaseCreateFlags(fs *flag.FlagSet, defaults Config) leaseCreateFlagValues {
	return leaseCreateFlagValues{
		Provider:      fs.String("provider", defaults.Provider, providerHelpAll()),
		Profile:       fs.String("profile", defaults.Profile, "profile"),
		Class:         fs.String("class", defaults.Class, "machine class"),
		ServerType:    fs.String("type", getenv("CRABBOX_SERVER_TYPE", ""), "provider server/instance type"),
		Market:        fs.String("market", defaults.Capacity.Market, "capacity market: spot or on-demand"),
		TTL:           fs.Duration("ttl", defaults.TTL, "maximum lease lifetime"),
		Idle:          fs.Duration("idle-timeout", defaults.IdleTimeout, "idle timeout"),
		Desktop:       fs.Bool("desktop", defaults.Desktop, "provision or require a visible desktop/VNC session"),
		Browser:       fs.Bool("browser", defaults.Browser, "provision or require a browser binary"),
		Code:          fs.Bool("code", defaults.Code, "provision or require web code-server capability"),
		ProviderFlags: registerProviderFlags(fs, defaults),
		Target:        registerTargetFlags(fs, defaults),
		Network:       registerNetworkFlags(fs, defaults),
	}
}

func applyLeaseCreateFlags(cfg *Config, fs *flag.FlagSet, values leaseCreateFlagValues) error {
	cfg.Provider = *values.Provider
	cfg.Profile = *values.Profile
	cfg.Class = *values.Class
	applyCapabilityFlags(cfg, *values.Desktop, *values.Browser, *values.Code)
	if err := applyTargetFlagOverrides(cfg, fs, values.Target); err != nil {
		return err
	}
	if err := applyNetworkFlagOverrides(cfg, fs, values.Network); err != nil {
		return err
	}
	if err := applyCapacityMarketFlag(cfg, fs, *values.Market); err != nil {
		return err
	}
	applyServerTypeFlagOverrides(cfg, fs, *values.ServerType)
	if flagWasSet(fs, "ttl") {
		cfg.TTL = *values.TTL
	}
	if flagWasSet(fs, "idle-timeout") {
		cfg.IdleTimeout = *values.Idle
	}
	if err := applyProviderFlags(cfg, fs, values.ProviderFlags); err != nil {
		return err
	}
	applyProviderConfigDefaults(cfg)
	if err := validateProviderTarget(*cfg); err != nil {
		return err
	}
	if err := validateRequestedCapabilities(*cfg); err != nil {
		return err
	}
	return validateLeaseDurations(*cfg)
}

func validateLeaseDurations(cfg Config) error {
	if cfg.TTL <= 0 {
		return exit(2, "ttl must be positive")
	}
	if cfg.IdleTimeout <= 0 {
		return exit(2, "idle timeout must be positive")
	}
	return nil
}

type leaseTargetConfigOptions struct {
	Desktop bool
}

func loadLeaseTargetConfig(fs *flag.FlagSet, provider string, targetFlags targetFlagValues, networkFlags networkModeFlagValues, opts leaseTargetConfigOptions) (Config, error) {
	cfg, err := loadConfig()
	if err != nil {
		return Config{}, err
	}
	cfg.Provider = provider
	if opts.Desktop {
		cfg.Desktop = true
	}
	if err := applyTargetFlagOverrides(&cfg, fs, targetFlags); err != nil {
		return Config{}, err
	}
	if err := applyNetworkModeFlagOverride(&cfg, fs, networkFlags); err != nil {
		return Config{}, err
	}
	applyProviderConfigDefaults(&cfg)
	if !cfg.ServerTypeExplicit {
		cfg.ServerType = serverTypeForConfig(cfg)
	}
	return cfg, nil
}

func setIDFromFirstArg(fs *flag.FlagSet, id *string) {
	if *id == "" && fs.NArg() > 0 {
		*id = fs.Arg(0)
	}
}

func requireLeaseID(id, usage string, cfg Config) error {
	if id == "" && !isStaticProvider(cfg.Provider) {
		return exit(2, "usage: %s", usage)
	}
	return nil
}

func (a App) resolveNetworkLeaseTarget(ctx context.Context, cfg Config, id string, printFallback bool) (Server, SSHTarget, string, error) {
	server, target, leaseID, err := a.resolveLeaseTarget(ctx, cfg, id)
	if err != nil {
		return Server{}, SSHTarget{}, "", err
	}
	resolved, err := resolveNetworkTarget(ctx, cfg, server, target)
	if err != nil {
		return Server{}, SSHTarget{}, "", err
	}
	target = resolved.Target
	if target.Host != "" {
		_ = probeSSHTransport(ctx, &target, 4*time.Second)
	}
	if printFallback && resolved.FallbackReason != "" {
		fmt.Fprintf(a.Stderr, "network fallback %s\n", resolved.FallbackReason)
	}
	return server, target, leaseID, nil
}

func (a App) claimAndTouchLeaseTarget(ctx context.Context, cfg Config, server Server, leaseID string, reclaim bool) error {
	repo, err := findRepo()
	if err != nil {
		return err
	}
	if err := claimLeaseForRepoConfig(leaseID, serverSlug(server), cfg, repo.Root, cfg.IdleTimeout, reclaim); err != nil {
		return err
	}
	a.touchLeaseTargetBestEffort(ctx, cfg, LeaseTarget{Server: server, LeaseID: leaseID}, "")
	return nil
}
