package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

type leaseCreateFlagValues struct {
	Provider      *string
	Profile       *string
	Class         *string
	Architecture  *string
	OSImage       *string
	ServerType    *string
	Market        *string
	Slug          *string
	Pond          *string
	Expose        *stringListFlag
	TTL           *time.Duration
	Idle          *time.Duration
	Desktop       *bool
	DesktopEnv    *string
	Browser       *bool
	Code          *bool
	ProviderFlags providerFlagValues
	Target        targetFlagValues
	Network       networkFlagValues
}

func registerLeaseCreateFlags(fs *flag.FlagSet, defaults Config) leaseCreateFlagValues {
	expose := stringListFlag{}
	fs.Var(&expose, "expose", "declare a TCP port this lease wants reachable over the SSH-mesh plane; repeatable")
	return leaseCreateFlagValues{
		Provider:      fs.String("provider", defaults.Provider, providerHelpAll()),
		Profile:       fs.String("profile", defaults.Profile, "profile"),
		Class:         fs.String("class", defaults.Class, "machine class"),
		Architecture:  fs.String("arch", defaults.Architecture, "CPU architecture: amd64 or arm64"),
		OSImage:       fs.String("os", defaults.OSImage, "portable Linux OS image selector, for example ubuntu:26.04"),
		ServerType:    fs.String("type", getenv("CRABBOX_SERVER_TYPE", ""), "provider server/instance type"),
		Market:        fs.String("market", defaults.Capacity.Market, "capacity market: spot or on-demand"),
		Slug:          fs.String("slug", "", "request a friendly slug for a new lease"),
		Pond:          fs.String("pond", defaults.Pond, "tag this lease with a pond name so peers can be selected with --pond"),
		Expose:        &expose,
		TTL:           fs.Duration("ttl", defaults.TTL, "maximum lease lifetime"),
		Idle:          fs.Duration("idle-timeout", defaults.IdleTimeout, "idle timeout"),
		Desktop:       fs.Bool("desktop", defaults.Desktop, "provision or require a visible desktop/VNC session"),
		DesktopEnv:    fs.String("desktop-env", defaults.DesktopEnv, "Linux desktop environment: xfce, wayland, or gnome"),
		Browser:       fs.Bool("browser", defaults.Browser, "provision or require a browser binary"),
		Code:          fs.Bool("code", defaults.Code, "provision or require web code-server capability"),
		ProviderFlags: registerProviderFlags(fs, defaults),
		Target:        registerTargetFlags(fs, defaults),
		Network:       registerNetworkFlags(fs, defaults),
	}
}

func applyLeaseCreateFlags(cfg *Config, fs *flag.FlagSet, values leaseCreateFlagValues) error {
	return applyLeaseCreateFlagsForLease(cfg, fs, values, "")
}

func applyLeaseCreateFlagsForLease(cfg *Config, fs *flag.FlagSet, values leaseCreateFlagValues, existingLeaseID string) error {
	cfg.Provider = *values.Provider
	cfg.Profile = *values.Profile
	cfg.Class = *values.Class
	if flagWasSet(fs, "arch") {
		arch, err := normalizeArchitecture(*values.Architecture)
		if err != nil {
			return err
		}
		cfg.Architecture = arch
		cfg.architectureExplicit = true
	}
	if flagWasSet(fs, "pond") {
		pond, err := requestedPondName(*values.Pond)
		if err != nil {
			return err
		}
		cfg.Pond = pond
	} else if cfg.Pond != "" {
		pond, err := requestedPondName(cfg.Pond)
		if err != nil {
			return err
		}
		cfg.Pond = pond
	}
	applyCapabilityFlags(cfg, *values.Desktop, *values.Browser, *values.Code)
	cfg.DesktopEnv = *values.DesktopEnv
	if err := applyTargetFlagOverrides(cfg, fs, values.Target); err != nil {
		return err
	}
	if err := autoRouteStaticLease(cfg, fs, existingLeaseID); err != nil {
		return err
	}
	if flagWasSet(fs, "os") {
		osImage, err := normalizeOSImage(*values.OSImage)
		if err != nil {
			return err
		}
		cfg.OSImage = osImage
		cfg.osImageExplicit = true
		applyOSImageProviderDefaults(cfg, false)
	}
	if err := applyNetworkFlagOverrides(cfg, fs, values.Network); err != nil {
		return err
	}
	if err := applyProviderRoutingFlags(cfg, fs, values.ProviderFlags); err != nil {
		return err
	}
	if existingLeaseID != "" && cfg.Provider == "aws" && cfg.TargetOS == targetMacOS && !flagWasSet(fs, "market") {
		cfg.Capacity.Market = "on-demand"
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
	if err := applyProviderConfigDefaults(cfg); err != nil {
		return err
	}
	if err := validateProviderTarget(*cfg); err != nil {
		return err
	}
	if err := validateRequestedCapabilities(*cfg); err != nil {
		return err
	}
	if values.Expose != nil && len(*values.Expose) > 0 {
		ports, err := requestedExposedPorts(*values.Expose)
		if err != nil {
			return err
		}
		cfg.ExposedPorts = ports
	}
	if err := validateLeaseDurations(*cfg); err != nil {
		return err
	}
	if cfg.Pond != "" {
		dynamicTailscaleTagAllowed := pondDynamicTailscaleTagAllowed(*cfg)
		appendPondTailscaleTag(cfg, dynamicTailscaleTagAllowed)
		// Reuse paths do not mutate ACL state.
		if existingLeaseID == "" && dynamicTailscaleTagAllowed {
			if err := maybeBootstrapPondACL(context.Background(), *cfg); err != nil {
				return err
			}
		}
	}
	return nil
}

const pondACLAutoBootstrapEnvVar = "CRABBOX_POND_ACL_BOOTSTRAP"

// maybeBootstrapPondACL self-bootstraps the pond tag's tagOwners + grants
// rows on the operator tailnet when explicitly enabled. TS_API_KEY alone is
// not consent to edit a tailnet policy; operators must also set
// CRABBOX_POND_ACL_BOOTSTRAP=1. When disabled, when the key is absent, when
// the provider lacks Tailscale, or when the row is already present, this is a
// silent no-op so doctor still owns the manual-snippet fallback path. Failures
// from the live API are surfaced so the lease is not created against a tailnet
// that cannot actually carry pond traffic.
func maybeBootstrapPondACL(ctx context.Context, cfg Config) error {
	if cfg.Pond == "" || !cfg.Tailscale.Enabled {
		return nil
	}
	if !pondDynamicTailscaleTagAllowed(cfg) {
		return nil
	}
	if !truthyEnv(os.Getenv(pondACLAutoBootstrapEnvVar)) {
		return nil
	}
	apiKey := strings.TrimSpace(os.Getenv("TS_API_KEY"))
	if apiKey == "" {
		return nil
	}
	// Don't mutate tailnet ACLs if no Tailscale auth key is configured
	// for the lease itself — provisioning will fail later, and we'd leave
	// a dangling policy mutation behind.
	if cfg.Tailscale.AuthKey == "" && os.Getenv("CRABBOX_TAILSCALE_AUTH_KEY") == "" {
		return nil
	}
	client := pondTailnetACLClientFactory(apiKey)
	if client == nil {
		return nil
	}
	tailnet := strings.TrimSpace(os.Getenv("TS_TAILNET"))
	owner := localCoordinatorOwner()
	err := pondACLEnsure(ctx, client, tailnet, owner, cfg.Pond)
	// A self-hosted control plane (e.g. Headscale) without a Tailscale-shaped
	// policy API must not block lease creation. Doctor surfaces the same
	// condition to the operator with the manual-snippet pointer.
	if errors.Is(err, ErrPondACLAutoBootstrapUnavailable) {
		return nil
	}
	return err
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

func truthyEnv(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

type leaseTargetConfigOptions struct {
	Desktop bool
	// LeaseID is the resolved lease id/slug from the command's --id flag (or
	// equivalent positional). When set, `static_<host>` ids auto-route to the
	// ssh provider so callers don't have to re-pass --provider / --static-host
	// that warmup already implied.
	LeaseID string
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
	if err := autoRouteStaticLease(&cfg, fs, opts.LeaseID); err != nil {
		return Config{}, err
	}
	if err := applyNetworkModeFlagOverride(&cfg, fs, networkFlags); err != nil {
		return Config{}, err
	}
	if err := routeConfiguredProvider(&cfg); err != nil {
		return Config{}, err
	}
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		return Config{}, err
	}
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
	_ = updateLeaseClaimEndpoint(leaseID, server, target)
	if printFallback && resolved.FallbackReason != "" {
		fmt.Fprintf(a.Stderr, "network fallback %s\n", resolved.FallbackReason)
	}
	return server, target, leaseID, nil
}

func (a App) claimAndTouchLeaseTarget(ctx context.Context, cfg Config, server Server, target SSHTarget, leaseID string, reclaim bool) error {
	repo, err := findRepo()
	if err != nil {
		return err
	}
	if err := claimLeaseTargetForRepoConfig(leaseID, serverSlug(server), cfg, server, target, repo.Root, cfg.IdleTimeout, reclaim); err != nil {
		return err
	}
	a.touchLeaseTargetBestEffort(ctx, cfg, LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, "")
	return nil
}
