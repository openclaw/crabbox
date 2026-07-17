package cli

import (
	"context"
	"flag"
	"fmt"
	"strings"
)

func (a App) ssh(ctx context.Context, args []string) error {
	resolved, err := a.resolveSSHCommandTarget(ctx, "ssh", args, true)
	if err != nil {
		return err
	}
	target := resolved.Lease.SSH
	if target.AuthSecret && !resolved.ShowSecret {
		fmt.Fprintf(a.Stderr, "warning: ssh auth user is secret; rerun with --show-secret to print a pasteable command\n")
	}
	fmt.Fprintln(a.Stdout, sshCommandLine(target, target.AuthSecret && !resolved.ShowSecret))
	return nil
}

type resolvedSSHCommandTarget struct {
	Config     Config
	Lease      LeaseTarget
	ShowSecret bool
}

func (a App) resolveSSHCommandTarget(ctx context.Context, command string, args []string, allowShowSecret bool) (resolvedSSHCommandTarget, error) {
	return a.resolveSSHCommandTargetWithOptions(ctx, command, args, allowShowSecret, sshCommandResolveOptions{})
}

type sshCommandResolveOptions struct {
	registerFlags func(*flag.FlagSet)
	validateFlags func() error
}

func (a App) resolveSSHCommandTargetWithOptions(ctx context.Context, command string, args []string, allowShowSecret bool, opts sshCommandResolveOptions) (resolvedSSHCommandTarget, error) {
	defaults := defaultConfig()
	fs := newFlagSet(command, a.Stderr)
	provider := fs.String("provider", defaults.Provider, providerHelpSSH())
	id := fs.String("id", "", "lease id or slug")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	var showSecret *bool
	if allowShowSecret {
		showSecret = fs.Bool("show-secret", false, "print secret auth material for token-based SSH providers")
	}
	providerFlags := registerProviderFlags(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if opts.registerFlags != nil {
		opts.registerFlags(fs)
	}
	if err := parseInterspersedFlags(fs, args); err != nil {
		return resolvedSSHCommandTarget{}, err
	}
	if opts.validateFlags != nil {
		if err := opts.validateFlags(); err != nil {
			return resolvedSSHCommandTarget{}, err
		}
	}
	idFlagSet := flagWasSet(fs, "id")
	setIDFromFirstArg(fs, id)
	if fs.NArg() > 1 || (idFlagSet && fs.NArg() > 0) {
		return resolvedSSHCommandTarget{}, exit(2, "usage: crabbox %s [flags] <lease-id-or-slug>", command)
	}
	cfg, err := loadSSHCommandConfig(fs, *provider, providerFlags, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: *id})
	if err != nil {
		return resolvedSSHCommandTarget{}, err
	}
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return resolvedSSHCommandTarget{}, err
	}
	if err := requireLeaseID(*id, "crabbox "+command+" --id <lease-id-or-slug>", cfg); err != nil {
		return resolvedSSHCommandTarget{}, err
	}
	lease, err := a.resolveNetworkLoginLeaseTargetForRepo(ctx, &cfg, *id, false, *reclaim, command != "connect")
	if err != nil {
		return resolvedSSHCommandTarget{}, err
	}
	if err := a.claimAndTouchLeaseTarget(ctx, cfg, lease.Server, lease.SSH, lease.LeaseID, *reclaim); err != nil {
		return resolvedSSHCommandTarget{}, err
	}
	resolved := resolvedSSHCommandTarget{
		Config: cfg,
		Lease:  lease,
	}
	if showSecret != nil {
		resolved.ShowSecret = *showSecret
	}
	return resolved, nil
}

func loadSSHCommandConfig(fs *flag.FlagSet, provider string, providerFlags providerFlagValues, targetFlags targetFlagValues, networkFlags networkModeFlagValues, opts leaseTargetConfigOptions) (Config, error) {
	cfg, err := loadLeaseTargetConfig(fs, provider, targetFlags, networkFlags, opts)
	if err != nil {
		return Config{}, err
	}
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func sshCommandLine(target SSHTarget, redactSecret bool) string {
	renderTarget := target
	if redactSecret {
		renderTarget.User = "<token>"
	}
	args := append([]string{"ssh"}, sshBaseArgs(renderTarget)...)
	args = append(args, renderTarget.User+"@"+renderTarget.Host)
	return strings.Join(shellWords(args), " ")
}
